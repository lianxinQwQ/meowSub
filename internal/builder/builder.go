// Package builder 负责把编排计划落成现实：初始系统保障、reflink 复制、容器内安装、元数据维护与计划渲染。
package builder

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"meowsub/internal/config"
	"meowsub/internal/execx"
	"meowsub/internal/fileindex"
	"meowsub/internal/planner"
	"meowsub/internal/reconcile"
	"meowsub/internal/staging"
	"meowsub/internal/state"
)

// HostResolver 用宿主机包数据库与 AUR RPC 解析软件组依赖闭包（只读查询，不安装）。
//
// 解析顺序（BFS 逐层展开 Depends）：
//  1. 官方仓库真名：一条 pacman -Si 全量转储建内存索引；
//  2. 官方 Provides 索引：sh、libssl.so=3-64 这类虚拟提供名映射到提供者；
//  3. AUR 真名：一次 RPC type=info 批量查询；
//  4. AUR Provides：RPC type=search&by=provides 按虚拟名搜索，多个命中取
//     字典序首个并提示；选中后经 type=info 取其依赖继续展开；
//  5. 全部落空才按虚拟提供名略去并警告（顶层目标包落空则直接报错）。
//
// 得到的是“相对空系统”的完整依赖集，用于聚类与交集规划；实际安装时容器内
// 的 paru --needed 仍会按子系统自身的已装状态精确解析。
type HostResolver struct {
	Runner execx.Runner
	// Fetcher 抓取网络资源（AUR 转储/RPC）；nil 时使用默认 HTTP 实现，测试可注入桩。
	Fetcher func(url string) ([]byte, error)
	// CacheDir AUR 转储缓存目录；空则用用户缓存目录下的 meowsub/。
	CacheDir string

	repoDB        map[string]*pkgInfo       // 官方仓库真名索引
	repoProviders map[string][]provideEntry // 官方仓库 Provides 索引
	aurProviders  map[string][]provideEntry // AUR Provides 索引
	aurByName     map[string]*pkgInfo       // AUR 真名缓存
	virtualDone   map[string]*pkgInfo       // 虚拟名 -> 解析结果/缺失占位（防重复查询）
	aurDumpTried  bool                      // 本次实例是否已尝试加载全量转储
	aurDumpOK     bool                      // 转储是否可用
}

type pkgInfo struct {
	Name     string
	Aur      bool
	missing  bool // 各索引均未命中（虚拟提供名）
	Version  string
	Depends  []string
	Provides []string
	// InstalledSize 安装后展开的精确字节数（pacman -Si "Installed Size"）。
	// 层尺寸预测的数据源：增量 = InstallSet 求和，继承部分是 reflink 共享。
	InstalledSize int64
}

// provideEntry 记录“谁提供了什么版本”。
type provideEntry struct {
	ver string // 提供条目的版本部分，可为空（如 sh）
	pkg *pkgInfo
}

const (
	aurRPCBase = "https://aur.archlinux.org/rpc/?v=5&"
	// aurDumpURL AUR 全量元数据转储（约 14MB gzip，11 万+包，含 Provides）。
	aurDumpURL = "https://aur.archlinux.org/packages-meta-ext-v1.json.gz"
	// aurDumpMaxAge 缓存有效期；过期后重新下载，下载失败时降级用过期缓存。
	aurDumpMaxAge = 6 * time.Hour
)

// Resolve 实现 planner.Resolver。
func (r *HostResolver) Resolve(packages []string) (*planner.Closure, error) {
	if len(packages) == 0 {
		return &planner.Closure{Aur: map[string]bool{}}, nil
	}
	if err := r.loadRepoDB(); err != nil {
		return nil, err
	}

	c := &planner.Closure{Pkgs: []string{}, Aur: map[string]bool{},
		AurVersion: map[string]string{}}
	members := map[string]bool{}   // 已进闭包的真实包名（去重）
	infos := map[string]*pkgInfo{} // base 名 -> 已解析/缺失占位
	var queue []string
	target := map[string]bool{}
	for _, p := range packages {
		name := stripConstraint(p)
		if name == "" || target[name] {
			continue
		}
		target[name] = true
		queue = append(queue, name)
	}

	// adopt 把解析到的包纳入闭包，其依赖继续入队。
	adopt := func(info *pkgInfo) {
		infos[info.Name] = info
		if !members[info.Name] {
			members[info.Name] = true
			c.Pkgs = append(c.Pkgs, info.Name)
			if info.Aur {
				c.Aur[info.Name] = true
				if info.Version != "" {
					c.AurVersion[info.Name] = info.Version
				}
			}
		}
		queue = append(queue, info.Depends...)
	}

	for {
		// 阶段一：仓库真名与两侧 Provides 索引消化队列，收集未命中 token
		var miss []string
		missSeen := map[string]bool{}
		for len(queue) > 0 {
			tok := queue[0]
			queue = queue[1:]
			base, _, _ := splitDep(tok)
			if base == "" || infos[base] != nil {
				continue
			}
			if info := r.repoDB[base]; info != nil {

				adopt(info)
				continue
			}
			if p := r.lookupProvider(tok); p != nil {
				infos[base] = p
				if p.Name != base {
					infos[p.Name] = p // 提供者真名也标记为已处理
				}
				adopt(p)
				continue
			}
			if !missSeen[base] {
				missSeen[base] = true
				miss = append(miss, tok)
			}
		}
		if len(miss) == 0 {
			break
		}
		// 阶段二：AUR RPC（真名批量 info + 虚拟名 provides 搜索）
		found := r.resolveAurBatch(miss)
		for _, tok := range miss {
			base, _, _ := splitDep(tok)
			if info := found[base]; info != nil {
				infos[base] = info
				adopt(info)
			} else {
				fmt.Fprintf(os.Stderr, "[警告] 官方仓库、Provides 索引与 AUR 均未命中 %q，已从依赖闭包中略去\n", tok)
				infos[base] = &pkgInfo{Name: base, missing: true} // 占位防重查
			}
		}
	}
	sort.Strings(c.Pkgs)

	// 顶层目标必须全部可解析，否则成品子系统会缺用户要装的软件
	var missingTargets []string
	for t := range target {
		if info := infos[t]; info == nil || info.missing {
			missingTargets = append(missingTargets, t)
		}
	}
	if len(missingTargets) > 0 {
		sort.Strings(missingTargets)
		return nil, fmt.Errorf("以下包在官方仓库和 AUR 中都不存在，请检查包名: %s",
			strings.Join(missingTargets, ", "))
	}
	return c, nil
}

// lookupProvider 在官方/AUR Provides 索引中查找虚拟提供名的提供者。
// 多个提供者时按确定性偏好排序：与虚拟名同名的 > 非 multilib 的 > 官方仓库的
// > 字典序，并给出提示。
func (r *HostResolver) lookupProvider(depTok string) *pkgInfo {
	base, _, ver := splitDep(depTok)
	for _, idx := range []map[string][]provideEntry{r.repoProviders, r.aurProviders} {
		cands := idx[base]
		if len(cands) == 0 {
			continue
		}
		var matched []*pkgInfo
		for _, e := range cands {
			// 版本约束按精确相等匹配；无版本要求则任意提供者皆可。
			if ver == "" || e.ver == ver {
				matched = append(matched, e.pkg)
			}
		}
		if len(matched) == 0 {
			continue
		}
		sort.Slice(matched, func(a, b int) bool {
			pa, pb := providerPenalty(matched[a], base), providerPenalty(matched[b], base)
			if pa != pb {
				return pa < pb
			}
			return matched[a].Name < matched[b].Name
		})
		if len(matched) > 1 {
			names := make([]string, len(matched))
			for i, m := range matched {
				names[i] = m.Name
			}
			fmt.Fprintf(os.Stderr, "[提示] %q 有多个提供者（%s），取 %s\n",
				depTok, strings.Join(names, " "), matched[0].Name)
		}
		return matched[0]
	}
	return nil
}

// providerPenalty 提供者选择的惩罚分，越小越优先：
// 真名即虚拟名的最可信；lib32-* 多库包对 x86_64 子系统几乎总是错误选择；
// AUR 提供者排在官方仓库之后。
func providerPenalty(p *pkgInfo, virtual string) int {
	score := 0
	if p.Name == virtual {
		score -= 100
	}
	if strings.HasPrefix(p.Name, "lib32-") {
		score += 100
	}
	if p.Aur {
		score += 10
	}
	return score
}

// resolveAurBatch 处理一轮未命中 token。优先查 AUR 全量元数据索引（一次下载
// 全局缓存）；转储不可用时回退逐名 RPC（真名批量 info + 虚拟名 provides 搜索，
// 多个提供者取字典序首个）。返回 base -> 包信息（仅含命中项）。
func (r *HostResolver) resolveAurBatch(tokens []string) map[string]*pkgInfo {
	out := map[string]*pkgInfo{}
	var rest []string // 转储不可用时的回退查询列表
	for _, tok := range tokens {
		base, _, _ := splitDep(tok)
		if cached := r.virtualDone[base]; cached != nil {
			out[base] = cached
			continue
		}
		if r.loadAurDump() {
			if info := r.aurByName[base]; info != nil {
				r.virtualDone[base] = info
				out[base] = info
				continue
			}
			// 转储刚就位，其 Provides 索引在 phase-1 还不存在，补查一次
			if p := r.lookupProvider(base); p != nil {
				r.virtualDone[base] = p
				out[base] = p
				continue
			}
			// 全量索引都没有：既非官方包也非 AUR 包，确属虚拟缺失
			continue
		}
		rest = append(rest, base)
	}
	if len(rest) == 0 {
		return out
	}

	// ---- RPC 回退路径 ----
	batch := r.rpcInfo(rest)
	for base, info := range batch {
		r.virtualDone[base] = info
		out[base] = info
	}
	var searchList []string
	searchSeen := map[string]bool{}
	for _, base := range rest {
		if out[base] != nil || searchSeen[base] {
			continue
		}
		searchSeen[base] = true
		searchList = append(searchList, base)
	}
	var chosen []string
	chosenFor := map[string]string{} // 虚拟名 -> 选中的提供者真名
	for _, v := range searchList {
		names := r.rpcSearchProvides(v)
		if len(names) == 0 {
			continue
		}
		sort.Strings(names)
		chosenFor[v] = names[0]
		chosen = append(chosen, names[0])
	}
	for base, info := range r.rpcInfo(uniqueStrings(chosen)) {
		out[base] = info
	}
	for v, pkgName := range chosenFor {
		if info := r.aurByName[pkgName]; info != nil && out[v] == nil {
			r.virtualDone[v] = info
			out[v] = info
		}
	}
	return out
}

// loadAurDump 下载（或读缓存）AUR 全量元数据并建立真名与 Provides 索引。
// 返回是否可用；失败时调用方回退 RPC。存在过期缓存且重新下载失败时降级使用过期数据。
func (r *HostResolver) loadAurDump() bool {
	if r.aurDumpTried {
		return r.aurDumpOK
	}
	r.aurDumpTried = true
	cachePath := filepath.Join(r.ensureCacheDir(), "aur-packages-meta-ext-v1.json.gz")

	useCache := func(stale bool) bool {
		data, err := os.ReadFile(cachePath)
		if err != nil {
			return false
		}
		if err := r.ingestAurDump(data); err != nil {
			return false
		}
		if stale {
			fmt.Fprintf(os.Stderr, "[提示] AUR 元数据转储下载失败，使用本地缓存（可能略旧）\n")
		} else {
			fmt.Fprintf(os.Stderr, "[提示] 已从缓存加载 AUR 全量元数据（%d 个包）\n", len(r.aurByName))
		}
		return true
	}

	// 新鲜缓存直接用
	if data, err := os.ReadFile(cachePath); err == nil && fileAge(cachePath) < aurDumpMaxAge {
		if err := r.ingestAurDump(data); err == nil {
			r.aurDumpOK = true
			fmt.Fprintf(os.Stderr, "[提示] 已从缓存加载 AUR 全量元数据（%d 个包）\n", len(r.aurByName))
			return true
		}
	}
	// 下载新转储
	data, err := r.fetch(aurDumpURL)
	if err == nil {
		if err := os.MkdirAll(r.CacheDir, 0o755); err == nil {
			_ = os.WriteFile(cachePath, data, 0o644)
		}
		if err := r.ingestAurDump(data); err == nil {
			r.aurDumpOK = true
			fmt.Fprintf(os.Stderr, "[提示] 已下载 AUR 全量元数据（%d 个包），缓存在 %s\n", len(r.aurByName), cachePath)
			return true
		}
		err = fmt.Errorf("解析失败: %w", err)
	}
	// 下载或解析失败 → 尝试过期缓存 → 彻底放弃则走 RPC 回退
	if useCache(true) {
		r.aurDumpOK = true
		return true
	}
	fmt.Fprintf(os.Stderr, "[警告] AUR 全量元数据不可用（%v），回退逐名 RPC 查询\n", err)
	return false
}

// ingestAurDump 流式解析 packages-meta-ext-v1.json.gz 的解压字节流并建索引。
func (r *HostResolver) ingestAurDump(data []byte) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer gz.Close()
	dec := json.NewDecoder(gz)
	t, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := t.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("顶层不是 JSON 数组")
	}
	for dec.More() {
		var p struct {
			Name     string   `json:"Name"`
			Version  string   `json:"Version"`
			Depends  []string `json:"Depends"`
			Provides []string `json:"Provides"`
		}
		if err := dec.Decode(&p); err != nil {
			return err
		}
		if p.Name == "" {
			continue
		}
		info := &pkgInfo{Name: p.Name, Aur: true, Version: p.Version,
			Depends: p.Depends, Provides: p.Provides}
		r.aurByName[info.Name] = info
		r.registerProvides(r.aurProviders, info)
	}
	return nil
}

func (r *HostResolver) ensureCacheDir() string {
	if r.CacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			base = os.TempDir()
		}
		r.CacheDir = filepath.Join(base, "meowsub")
	}
	return r.CacheDir
}

func fileAge(path string) time.Duration {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Duration(1 << 62) // 极大值表示“非常旧”
	}
	return time.Since(fi.ModTime())
}

// rpcInfo 一次 RPC type=info 批量获取 AUR 包元数据，注册进各类缓存。
func (r *HostResolver) rpcInfo(names []string) map[string]*pkgInfo {
	out := map[string]*pkgInfo{}
	var query []string
	for _, n := range names {
		if n == "" {
			continue
		}
		if cached := r.aurByName[n]; cached != nil && !cached.missing {
			out[n] = cached
			continue
		}
		query = append(query, "arg[]="+url.QueryEscape(n))
	}
	if len(query) == 0 {
		return out
	}
	data, err := r.fetch(aurRPCBase + "type=info&" + strings.Join(query, "&"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[警告] AUR RPC info 查询失败：%v\n", err)
		return out
	}
	var resp struct {
		ResultCount int `json:"resultcount"`
		Results     []struct {
			Name     string   `json:"Name"`
			Version  string   `json:"Version"`
			Depends  []string `json:"Depends"`
			Provides []string `json:"Provides"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "[警告] AUR RPC info 响应解析失败：%v\n", err)
		return out
	}
	for _, res := range resp.Results {
		info := &pkgInfo{Name: res.Name, Aur: true, Version: res.Version,
			Depends: res.Depends, Provides: res.Provides}
		r.aurByName[res.Name] = info
		r.registerProvides(r.aurProviders, info)
		out[res.Name] = info
	}
	return out
}

// rpcSearchProvides 用 RPC type=search&by=provides 搜索提供某虚拟名的 AUR 包。
func (r *HostResolver) rpcSearchProvides(virtual string) []string {
	if cached := r.virtualDone[virtual]; cached != nil {
		return nil // 已定论（成功或失败）不再搜索
	}
	u := aurRPCBase + "type=search&by=provides&arg[]=" + url.QueryEscape(virtual)
	data, err := r.fetch(u)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[警告] AUR RPC provides 查询失败（%q）：%v\n", virtual, err)
		return nil
	}
	var resp struct {
		ResultCount int `json:"resultcount"`
		Results     []struct {
			Name string `json:"Name"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "[警告] AUR RPC provides 响应解析失败（%q）：%v\n", virtual, err)
		return nil
	}
	names := make([]string, 0, len(resp.Results))
	for _, res := range resp.Results {
		names = append(names, res.Name)
	}
	return names
}

func (r *HostResolver) fetch(u string) ([]byte, error) {
	if r.Fetcher != nil {
		return r.Fetcher(u)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// registerProvides 把包的 Provides 条目登记进索引。
func (r *HostResolver) registerProvides(idx map[string][]provideEntry, info *pkgInfo) {
	for _, pv := range info.Provides {
		base, ver, _ := splitDep(pv)
		idx[base] = append(idx[base], provideEntry{ver: ver, pkg: info})
	}
}

// splitDep 拆分依赖/提供条目："libssl.so=3-64" -> ("libssl.so","=","3-64")；
// "gtk3>=3.24" -> ("gtk3",">=","3.24")；"sh" -> ("sh","","")。
func splitDep(tok string) (name, op, ver string) {
	i := strings.IndexAny(tok, "<>=")
	if i < 0 {
		return tok, "", ""
	}
	name = tok[:i]
	j := i
	for j < len(tok) && (tok[j] == '<' || tok[j] == '>' || tok[j] == '=') {
		j++
	}
	return name, tok[i:j], tok[j:]
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// loadRepoDB 用一条 pacman -Si 全量命令建立官方仓库真名与 Provides 索引。
func (r *HostResolver) loadRepoDB() error {
	if r.repoDB != nil {
		return nil
	}
	out, err := r.Runner.RunOutput("pacman", "-Si")
	if err != nil {
		return fmt.Errorf("读取官方仓库元数据失败（尝试运行 pacman -Sy 同步数据库后重试）: %w", err)
	}
	r.repoDB = map[string]*pkgInfo{}
	r.repoProviders = map[string][]provideEntry{}
	r.aurProviders = map[string][]provideEntry{}
	r.aurByName = map[string]*pkgInfo{}
	r.virtualDone = map[string]*pkgInfo{}
	for _, block := range splitBlocks(out) {
		info, err := parsePkgBlock(block, false)
		if err != nil || info.Name == "" {
			continue
		}
		r.repoDB[info.Name] = info
		r.registerProvides(r.repoProviders, info)
	}
	return nil
}

// splitBlocks 按空行切分 pacman -Si 的多包输出。
func splitBlocks(out string) []string {
	var blocks []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, strings.Join(cur, "\n"))
			cur = cur[:0]
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return blocks
}

// parsePkgBlock 解析单个包的 pacman -Si 输出（含缩进续行），
// 提取 Name、Depends On 与 Provides；Make Deps 不计入运行时闭包。
func parsePkgBlock(block string, aur bool) (*pkgInfo, error) {
	fields := strings.Fields
	info := &pkgInfo{Aur: aur}
	lastKey := ""
	deps := ""
	provs := ""
	for _, line := range strings.Split(block, "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") && strings.Contains(line, " : ") {
			parts := strings.SplitN(line, " : ", 2)
			lastKey = strings.TrimSpace(parts[0])
			switch lastKey {
			case "Name":
				if f := fields(parts[1]); len(f) > 0 {
					info.Name = f[0]
				} else {
					return nil, fmt.Errorf("Name 字段为空")
				}
			case "Depends On":
				deps += " " + parts[1]
			case "Provides":
				provs += " " + parts[1]
			case "Installed Size":
				if isz, ok := parseInstalledSize(strings.TrimSpace(parts[1])); ok {
					info.InstalledSize = isz
				}
			default:
				lastKey = ""
			}
			continue
		}
		switch lastKey {
		case "Depends On":
			deps += " " + line
		case "Provides":
			provs += " " + line
		}
	}
	for _, d := range fields(deps) {
		if d != "" && d != "None" {
			info.Depends = append(info.Depends, d) // 保留约束，供提供者精确匹配
		}
	}
	// Provides 保留版本约束（libssl.so=3-64 的版本参与匹配）
	for _, p := range fields(provs) {
		if p != "" && p != "None" {
			info.Provides = append(info.Provides, p)
		}
	}
	if info.Name == "" {
		return nil, fmt.Errorf("输出中没有 Name 字段")
	}
	return info, nil
}

// parseInstalledSize 解析 pacman -Si 的 "Installed Size" 值为字节。
// 形如 "9819.83 KiB" / "305.62 MiB" / "None"。单位表按 pacman 实际输出设计。
func parseInstalledSize(s string) (int64, bool) {
	f := strings.Fields(s)
	if len(f) != 2 || f[0] == "None" {
		return 0, false
	}
	val, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, false
	}
	var mult float64
	switch f[1] {
	case "B":
		mult = 1
	case "KiB":
		mult = 1024
	case "MiB":
		mult = 1024 * 1024
	case "GiB":
		mult = 1024 * 1024 * 1024
	default:
		return 0, false
	}
	return int64(val * mult), true
}

// installSetSize 汇总一个安装集的预测尺寸；未知包跳过（AUR 包在
// 收割进池之前无法从 sync 库获得尺寸）。
func installSetSize(set []string, sizes map[string]int64) int64 {
	var total int64
	for _, p := range set {
		total += sizes[p]
	}
	return total
}

// memInstallExtra 估算内存构建安装阶段将写入 tmpfs 的字节增量：
// 尺寸表命中直接取 pacman -Si 的 InstalledSize；池/AUR 包带不出 isize，
// 按池内同名 .pkg 压缩体积 ×3 折算（解包膨胀 + 下载缓存）。合计乘 1.2
// 事务余量（块取整、数据库、落进容器根的包缓存目录）。返回增量与无任何
// 尺寸依据的包数（供日志提示估算口径）。
func (b *Builder) memInstallExtra(installSet []string) (int64, int) {
	var total int64
	blind := 0
	for _, p := range installSet {
		if sz, ok := renderSizes[p]; ok && sz > 0 {
			total += sz
			continue
		}
		matched := false
		for _, pat := range []string{"-*.pkg.tar.zst", "-*.pkg.tar.xz"} {
			hits, err := filepath.Glob(filepath.Join(b.poolDir(), p+pat))
			if err != nil {
				continue
			}
			for _, f := range hits {
				if fi, ferr := os.Stat(f); ferr == nil && fi.Mode().IsRegular() {
					total += fi.Size() * 3
					matched = true
				}
			}
		}
		if !matched {
			blind++
		}
	}
	return total + total/5, blind
}

// SizeTable 从仓库索引导出 "包名 -> 安装后字节" 的查询表。
func (r *HostResolver) SizeTable() map[string]int64 {
	t := make(map[string]int64, len(r.repoDB))
	for name, info := range r.repoDB {
		if info.InstalledSize > 0 {
			t[name] = info.InstalledSize
		}
	}
	// AUR 转储同样带不出 isize（字段不含尺寸），留空由池侧 .MTREE 补
	return t
}

// stripConstraint 去掉版本约束："gtk3>=3.24" -> "gtk3"。
func stripConstraint(s string) string {
	if i := strings.IndexAny(s, "<>="); i >= 0 {
		return s[:i]
	}
	return s
}

// Builder 编排执行器。build 模式要求整体以 root 运行。
type Builder struct {
	Runner execx.Runner
	// BaseDir 构建区根目录（base_dir/build，见 internal/layout）：
	// arch、pool、builder、sub 与全部节点路径均在其下。
	BaseDir string
	// MetaBase 聚合状态 .meowsub 所在总根（base_dir）；零值回退 BaseDir
	// （测试直接以临时目录作构建区时，状态文件仍落在同根下）。
	MetaBase string
	Out      io.Writer
	// HostCacheDir 宿主机包缓存目录列表；nil 表示尚未解析（懒解析一次），
	// 空切片表示解析过但无可共享目录。
	HostCacheDir []string
	// ExtraRepos 配置声明的额外 pacman 仓库（multilib、archlinuxcn 等）。
	// 属于基础镜像属性：合成进 pacstrap 的配置，随 reflink 传导全部子系统。
	ExtraRepos []config.Repository
	// Groups 组级声明缓存（键=组名）：烘焙阶段取 users/services/export。
	// nil 视作无任何新键声明，烘焙整体退化为空操作。
	Groups map[string]*config.Group
	// ProxyEnv 透传给容器的代理变量（K=V）；nil 表示未采集。
	ProxyEnv []string
	// BuilderTmpSize 构建机器容器（编译车间 build/builder 与基础镜像
	// build/arch）/tmp 的 tmpfs 显式大小（字节，配置顶层 builder_tmp_size）：
	// 覆写 nspawn 自动分配的 tmpfs，防内存吃紧时容器内写 /tmp 直接 ENOSPC
	//（典型：AUR 预构建把数 GB 安装负载解进 /tmp）。0 = 不干预。
	// 组构建容器走 nspawnGroup，按成员组的 build_tmp_size 另行解析。
	BuilderTmpSize int64

	// MemTmpfsSize 内存构建（小组）工作区 tmpfs 的容量下限（字节）：来自
	// 配置 mem_tmpfs_size，命令行 --mem-tmpfs-size 可覆盖。实际挂载取
	// max(自动估算, 此值)；tmpfs 按写入占页，调大无预分配代价。0 = 纯估算。
	MemTmpfsSize int64

	// Verify 落盘一致性校验开关（build --verify）：小组内存构建落盘完成后
	// 逐条比对暂存树与实体树，不一致即中止本轮收尾（标记不写，下轮重建）。
	Verify bool

	// stage 内存构建的 tmpfs 工作区管理器（默认与 Runner 同源）。
	stage *staging.Stager
	// fileIx 已处理系统目录树的内存索引（树成品落定后建立，小组增量并入）。
	fileIx *fileindex.Index
	// afterStage 测试缝隙：staging 返回内存副本后、容器改动前回调，
	// 用于向副本注入“安装结果”；生产路径恒为 nil。
	afterStage func(memRoot string)
}

// New 创建 Builder。
func New(runner execx.Runner, out io.Writer, baseDir string) *Builder {
	return &Builder{Runner: runner, BaseDir: baseDir, Out: out}
}

// metaBase 聚合状态根：生产中为 base_dir；未显式指定（测试）时与构建区同根。
func (b *Builder) metaBase() string {
	if b.MetaBase != "" {
		return b.MetaBase
	}
	return b.BaseDir
}

func (b *Builder) stager() *staging.Stager {
	if b.stage == nil {
		b.stage = &staging.Stager{Runner: b.Runner, MinSize: b.MemTmpfsSize}
	}
	return b.stage
}

func (b *Builder) archDir() string { return filepath.Join(b.BaseDir, "arch") }

// poolDir 本地软件包池：仓库包下载 + AUR 构建产物 + repo-add 的本地仓库。
func (b *Builder) poolDir() string { return filepath.Join(b.BaseDir, "pool") }

// builderRoot 常驻编译车间根目录。
func (b *Builder) builderRoot() string { return filepath.Join(b.BaseDir, "builder") }

const (
	// poolMount 软件包池在容器内的挂载点（builder 读写、节点只读）。
	poolMount = "/mnt/pkgpool"
	// poolRepoName 池对应的 pacman 仓库名。
	poolRepoName = "meowsub-pool"
	// poolDBName 池的仓库数据库文件名。
	poolDBName = "meowsub-pool.db.tar.zst"
)

func (b *Builder) poolDBPath() string { return filepath.Join(b.poolDir(), poolDBName) }

// syntheticPacmanConf 读宿主机 /etc/pacman.conf，追加 [[repositories]] 段，
// 供 pacstrap -C 使用：额外仓库作为基础镜像属性进入首次构建。
// 段固定加 SigLevel = PackageRequired（信任体系仍来自宿主机钥匙环 +
// 官方源的 archlinuxcn-keyring 引导），服务器按声明顺序天然形成优先级。
func (b *Builder) syntheticPacmanConf() string {
	host, err := os.ReadFile(hostPacmanConf)
	base := string(host)
	if err != nil {
		base = ""
	}
	var sb strings.Builder
	sb.WriteString(strings.TrimRight(base, "\n"))
	for _, r := range b.ExtraRepos {
		sb.WriteString("\n\n[" + r.Name + "]\n")
		if len(r.Servers) == 0 { // 无 servers：视作“仅需登记的特殊仓库”
			continue
		}
		sb.WriteString("SigLevel = PackageRequired\n")
		for _, s := range r.Servers {
			sb.WriteString("Server = " + s + "\n")
		}
	}
	return sb.String()
}

// applyExtraRepos 把配置声明的额外仓库幂等写入已有基础系统的 pacman.conf。
// pacstrap 阶段的合成配置只服务首次构建；此后每轮 update_base 都会再次
// 对齐，使配置里的镜像增删能传导进现存环境。
func (b *Builder) applyExtraRepos(root string) {
	for _, r := range b.ExtraRepos {
		confPath := filepath.Join(root, "etc", "pacman.conf")
		data, err := os.ReadFile(confPath)
		if err != nil && !os.IsNotExist(err) {
			continue
		}
		updated := ensureRepoBlock(string(data), r)
		if updated == string(data) {
			continue
		}
		_ = os.WriteFile(confPath, []byte(updated), 0o644)
	}
}

// syncHostKeyring 把宿主机当前的 pacman 钥匙环整体同步进目标系统。
// 原则：子系统的信任视图以宿主机为准（宿主机的信任即管理员意志）。
// 这同时解决额外仓库（如 archlinuxcn）的引导自举：密钥不经由需验证的
// keyring 包传递，而由宿主机直接供给；keyring 包随后仅作常规升级项。
func (b *Builder) syncHostKeyring(root string) {
	ring := filepath.Join(root, "etc", "pacman.d", "gnupg")
	if _, err := os.Stat(ring); err == nil {
		if err := b.Runner.Run("rm", "-rf", ring); err != nil {
			fmt.Fprintf(b.Out, "[警告] 清理目标钥匙环失败：%v\n", err)
			return
		}
	}
	if err := b.Runner.Run("cp", "-a", "/etc/pacman.d/gnupg", ring); err != nil {
		fmt.Fprintf(b.Out, "[警告] 同步宿主机钥匙环失败：%v\n", err)
	}
}

// ensureRepoBlock 在 pacman.conf 末尾追加仓库段；已存在同名段则原样返回。
func ensureRepoBlock(conf string, r config.Repository) string {
	header := "[" + r.Name + "]"
	for _, l := range strings.Split(conf, "\n") {
		if strings.TrimSpace(l) == header {
			return conf
		}
	}
	var sb strings.Builder
	sb.WriteString(strings.TrimRight(conf, "\n") + "\n\n" + header + "\n")
	sb.WriteString("SigLevel = PackageRequired\n")
	for _, s := range r.Servers {
		sb.WriteString("Server = " + s + "\n")
	}
	return sb.String()
}

// extraRepoPackages 仓库名同时是“要装的东西”的场景：
// 如 [[repositories]] name="archlinuxcn-keyring"（无 servers，仅引导信任）。
func extraRepoPackages(repos []config.Repository) []string {
	var out []string
	for _, r := range repos {
		if len(r.Servers) == 0 && isLegalPkgName(r.Name) {
			out = append(out, r.Name)
		}
	}
	return out
}

func isLegalPkgName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '.' || c == '_' || c >= 'A' && c <= 'Z'
		if !ok {
			return false
		}
	}
	return true
}

// ProbeReflink 在构建区内做一次强制 reflink 试拷贝，失败即报错（强制写时复制）。
func (b *Builder) ProbeReflink() error {
	if err := os.MkdirAll(b.BaseDir, 0o755); err != nil {
		return err
	}
	probe := filepath.Join(b.BaseDir, ".meowsub-reflink-probe")
	cow := probe + ".cow"
	defer os.Remove(probe)
	defer os.Remove(cow)
	if err := os.WriteFile(probe, []byte("meowsub reflink probe"), 0o644); err != nil {
		return err
	}
	if err := b.Runner.Run("cp", "-a", "--reflink=always", probe, cow); err != nil {
		return fmt.Errorf("强制 reflink 复制失败：%s 所在文件系统不支持写时复制；"+
			"meowSub 要求 Btrfs/XFS 等支持 reflink 的文件系统来共享空间 (%w)", b.BaseDir, err)
	}
	return nil
}

// ScanView 扫描磁盘现状（各子系统内的标记文件），生成 reconcile.FsView。
// 标记是唯一事实来源：有标记即本程序所生；期望位置上无标记的目录视为陌生目录。
func (b *Builder) ScanView(pl *planner.Plan) (*reconcile.FsView, error) {
	view := &reconcile.FsView{
		UnmarkedAt: map[string]string{},
		Existing:   map[string]*state.NodeRecord{},
		Small:      map[string]*state.NodeRecord{},
	}

	arch := b.archDir()
	if fi, err := os.Stat(arch); err == nil && fi.IsDir() {
		view.BaseExists = true
		m, err := state.ReadMarker(arch)
		if err != nil {
			return nil, err
		}
		view.BaseMarked = m != nil && m.Kind == state.KindBase
	}

	desiredNames := map[string]bool{"arch": true, "builder": true}
	scanNode := func(n *planner.Node, small bool) error {
		desiredNames[n.Name] = true
		if fi, err := os.Stat(n.Path); err == nil && fi.IsDir() {
			m, err := state.ReadMarker(n.Path)
			if err != nil {
				return err
			}
			if m == nil || m.Name != n.Name {
				view.UnmarkedAt[n.Path] = "存在同名目录但缺少 meowSub 标记"
				return nil
			}
			rec := markerToRecord(m, n.Path)
			view.Existing[n.Name] = rec
			if small {
				view.Small[n.Name] = rec
			}
		}
		return nil
	}
	for _, n := range pl.Nodes {
		if err := scanNode(n, false); err != nil {
			return nil, err
		}
	}
	for _, n := range pl.SmallNodes {
		if err := scanNode(n, true); err != nil {
			return nil, err
		}
	}

	// 孤儿扫描：顶层目录与 sub/* 中带标记但不被期望树引用的子系统
	scanOrphans := func(dir string) error {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.IsDir() || e.Name() == state.MetaDirName || desiredNames[e.Name()] {
				continue
			}
			full := filepath.Join(dir, e.Name())
			m, err := state.ReadMarker(full)
			if err != nil {
				return err
			}
			if m != nil {
				view.Orphans = append(view.Orphans, markerToRecord(m, full))
			}
		}
		return nil
	}
	if err := scanOrphans(b.BaseDir); err != nil {
		return nil, err
	}
	if err := scanOrphans(filepath.Join(b.BaseDir, "sub")); err != nil {
		return nil, err
	}
	sort.Slice(view.Orphans, func(i, j int) bool { return view.Orphans[i].Name < view.Orphans[j].Name })
	return view, nil
}

func markerToRecord(m *state.Marker, path string) *state.NodeRecord {
	return &state.NodeRecord{
		Name: m.Name, Path: path, Kind: m.Kind, Parent: m.Parent,
		Groups: m.Groups, InstallSet: m.InstallSet,
	}
}

// logindDropInCommands 幂等写入运行时目录保留策略：容器内 logind 会在
// 最后一个会话关闭后拆除 /run/user/<uid>（user-runtime-dir），而那里挂着
// 宿主会话的叠加视图——按会话拆除会连带拆掉视图（真实事故：Firefox 的
// dconf 只读告警只是表象，视图被拆后 GUI 应用彻底失明）。清扫边界上移到
// 容器：daemon 占用清零即停机，tmpfs 随容器整体消失，无泄漏。初始系统
// 准备（baseSetupScript）与滚动更新（updateBase）都执行本段，存量 base
// 在下一轮 build 自动补齐。
const logindDropInCommands = `install -d /etc/systemd/logind.conf.d
cat > /etc/systemd/logind.conf.d/meowsub.conf <<'EOF'
# meowSub：/run/user/<uid> 挂载宿主会话叠加视图，清理以容器生命周期为准
[Login]
UserStopDelaySec=infinity
EOF`

const baseSetupScript = `set -e
useradd -m builder
echo 'builder ALL=(ALL:ALL) NOPASSWD: ALL' > /etc/sudoers.d/10-meowsub-builder
chmod 440 /etc/sudoers.d/10-meowsub-builder
` + logindDropInCommands

// ensureParuHealthy 探测容器内 paru 可用性。paru 由基础镜像的仓库
// （archlinuxcn 等）供给、随 base 滚动更新保持链接新鲜；探针失败时
// 尝试用仓库版本强装（自愈），仓库也没有就只能报错交由用户处置。
func (b *Builder) ensureParuHealthy(root string) error {
	if err := b.nspawn(root, "runuser", "-u", "builder", "--",
		"paru", "--version"); err == nil {
		return nil
	}
	fmt.Fprintf(b.Out, "[builder] paru 链接失效，尝试从已配置仓库恢复…\n")
	if err := b.nspawn(root, "pacman", "-Sy", "--noconfirm", "paru"); err != nil {
		return fmt.Errorf("paru 无法运行且仓库无法提供新版：%w（请检查 "+
			"[[repositories]] 是否配置了 archlinuxcn 等提供新 paru 的源）", err)
	}
	if err := b.nspawn(root, "runuser", "-u", "builder", "--",
		"paru", "--version"); err != nil {
		return fmt.Errorf("仓库恢复后 paru 仍不可运行: %w", err)
	}
	fmt.Fprintf(b.Out, "[builder] paru 已从仓库恢复\n")
	return nil
}

// nspawn 在构建机器容器（编译车间/基础镜像）内执行命令；自动只读挂载
// 宿主机包缓存池，供容器内 pacman/paru 命中已下载的包（写入仍走容器
// 自身缓存），并在池存在时只读挂载本地软件包仓库。
func (b *Builder) nspawn(root string, args ...string) error {
	return b.nspawnWith(root, false, args...)
}

// nspawnRWPool 在构建机器容器内执行命令，且把软件包池以读写方式挂载
// （仅供编译车间：makepkg 的 PKGDEST 产物直落池内）。
func (b *Builder) nspawnRWPool(root string, args ...string) error {
	return b.nspawnWith(root, true, args...)
}

// nspawnGroup 在组构建容器内执行命令：/tmp 按成员组的 build_tmp_size
// 解析（共享中间节点服务多个组，取最大需求封顶），与构建机器容器
// 的 builder_tmp_size 互不相干。
func (b *Builder) nspawnGroup(root string, groups []string, args ...string) error {
	return b.nspawnTmp(b.buildTmpSize(groups), root, false, args...)
}

func (b *Builder) nspawnWith(root string, rwPool bool, args ...string) error {
	return b.nspawnTmp(b.BuilderTmpSize, root, rwPool, args...)
}

func (b *Builder) nspawnTmp(tmp int64, root string, rwPool bool, args ...string) error {
	return b.Runner.Run("systemd-nspawn", b.nspawnBaseArgs(tmp, root, rwPool, args)...)
}

func (b *Builder) nspawnBaseArgs(tmp int64, root string, rwPool bool, args []string) []string {
	base := []string{"-D", root}
	for _, d := range b.hostCacheDirs() {
		base = append(base, "--bind-ro="+d+":"+hostCacheMount)
	}
	if _, err := os.Stat(b.poolDir()); err == nil {
		mode := "--bind-ro="
		if rwPool {
			mode = "--bind="
		}
		base = append(base, mode+b.poolDir()+":"+poolMount)
	}
	for _, kv := range b.proxyEnv() {
		base = append(base, "--setenv="+kv)
	}
	if arg := config.TmpSizeArg(tmp); arg != "" {
		base = append(base, arg)
	}
	return append(base, args...)
}

// buildTmpSize 组构建容器的 /tmp 大小：成员组 build_tmp_size 的最大值
// （组未声明或不存在时按 0 计，全 0 = 沿用 nspawn 默认）。
func (b *Builder) buildTmpSize(groups []string) int64 {
	var max int64
	for _, name := range groups {
		if g, ok := b.Groups[name]; ok && g.BuildTmpSize > max {
			max = g.BuildTmpSize
		}
	}
	return max
}

// proxyEnv 返回需要透传给容器的代理环境变量（懒解析一次）。
// 网络拓扑说明：当前 nspawn 不隔离 network 命名空间，容器内的 127.0.0.1
// 就是宿主机回环本身，代理地址原样透传即可；将来若启用私有网络
// （--network-veth 等），只需在此处把 lo 地址改写为网关地址。
func (b *Builder) proxyEnv() []string {
	if b.ProxyEnv != nil {
		return b.ProxyEnv
	}
	b.ProxyEnv = collectProxyEnv()
	if len(b.ProxyEnv) > 0 {
		masked := make([]string, len(b.ProxyEnv))
		for i, kv := range b.ProxyEnv {
			k, v, _ := strings.Cut(kv, "=")
			masked[i] = k + "=" + maskUserinfo(v)
		}
		fmt.Fprintf(b.Out, "[proxy] 透传宿主机代理配置到容器：%s\n",
			strings.Join(masked, " "))
	}
	return b.ProxyEnv
}

var hostEnvironmentFile = "/etc/environment"

// proxyKeys 采集目标键（小写形式；大写变体按需派生）。
var proxyKeys = []string{"http_proxy", "https_proxy", "all_proxy", "no_proxy"}

func isProxyKey(key string) bool {
	for _, k := range proxyKeys {
		if k == key || strings.ToUpper(k) == key {
			return true
		}
	}
	return false
}

// collectProxyEnv 采集宿主机代理配置（真实来源：进程环境 + /etc/environment）。
func collectProxyEnv() []string {
	return collectProxyEnvFrom(os.LookupEnv, hostEnvironmentFile)
}

// collectProxyEnvFrom 键的大小写保真透传：大写对大写、小写对小写，
// 同一语义键的两个变体各自独立、互不派生——工具链对大小写的读取规则
// 本就不同（如 curl 刻意不读大写 HTTP_PROXY），保真才能在容器内复刻
// 宿主机的实际行为。lookup 优先于 environmentFile；文件仅按“精确键
// （含大小写）”补缺，不覆盖已有值。输出顺序稳定：小写键在前、大写紧随。
func collectProxyEnvFrom(lookup func(string) (string, bool), environmentFile string) []string {
	variantOrder := make([]string, 0, len(proxyKeys)*2)
	for _, k := range proxyKeys {
		variantOrder = append(variantOrder, k, strings.ToUpper(k))
	}

	out := make([]string, 0, len(variantOrder))
	taken := map[string]bool{}
	for _, k := range variantOrder { // 进程环境
		if v, ok := lookup(k); ok && v != "" {
			taken[k] = true
			out = append(out, k+"="+v)
		}
	}
	data, err := os.ReadFile(environmentFile)
	if err != nil {
		return out
	}
	fileVal := map[string]string{} // 同名以后出现的行不覆盖先出现的
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, "=")
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.Trim(strings.TrimSpace(line[i+1:]), `"'`)
		if val == "" || !isProxyKey(key) {
			continue
		}
		if _, dup := fileVal[key]; !dup {
			fileVal[key] = val
		}
	}
	for _, k := range variantOrder { // 文件只补精确缺失项
		if taken[k] || fileVal[k] == "" {
			continue
		}
		out = append(out, k+"="+fileVal[k])
	}
	return out
}

// maskUserinfo 掩去代理 URL 中的用户名密码段用于日志输出。
func maskUserinfo(u string) string {
	schemeIdx := strings.Index(u, "://")
	if schemeIdx < 0 {
		return u // 无 scheme 则假定无 userinfo
	}
	rest := u[schemeIdx+3:]
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return u
	}
	return u[:schemeIdx+3] + "***" + rest[at:]
}

// hostCacheDirs 返回宿主机包缓存目录列表（懒解析一次）。
func (b *Builder) hostCacheDirs() []string {
	if b.HostCacheDir != nil {
		return b.HostCacheDir
	}
	conf, err := os.ReadFile(hostPacmanConf)
	if err != nil {
		b.HostCacheDir = []string{} // 无配置文件：无共享，行为同旧版
		return b.HostCacheDir
	}
	b.HostCacheDir = resolveHostCacheDirsFrom(string(conf))
	return b.HostCacheDir
}

// registerHostCache 准备容器的包缓存：树自身缓存目录（可写，下载落点）
// 建目录并登记为 pacman.conf 第一条 CacheDir，宿主机只读缓存兜后作只读
// 来源——pacman 的多 CacheDir 语义是“第一个可写的用于写”。幂等；子节点
// 经 reflink 复制自动继承该配置。
func (b *Builder) registerHostCache(root string) {
	if err := os.MkdirAll(filepath.Join(root, defaultPacmanCache), 0o755); err != nil {
		fmt.Fprintf(b.Out, "[警告] 创建树内包缓存目录失败：%v\n", err)
	}
	if len(b.hostCacheDirs()) == 0 {
		return
	}
	confPath := filepath.Join(root, "etc", "pacman.conf")
	data, err := os.ReadFile(confPath)
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(b.Out, "[警告] 读取 %s 失败：%v\n", confPath, err)
		return
	}
	updated := ensureExtraCacheDir(ensureOwnCacheDir(string(data), defaultPacmanCache), hostCacheMount)
	if updated == string(data) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(confPath), 0o755); err != nil {
		fmt.Fprintf(b.Out, "[警告] 创建 %s 目录失败：%v\n", filepath.Dir(confPath), err)
		return
	}
	if err := os.WriteFile(confPath, []byte(updated), 0o644); err != nil {
		fmt.Fprintf(b.Out, "[警告] 写入 %s 失败：%v\n", confPath, err)
	}
}

const (
	hostPacmanConf     = "/etc/pacman.conf"
	hostCacheMount     = "/mnt/host-pkgcache"
	defaultPacmanCache = "/var/cache/pacman/pkg"
)

// parseCacheDirs 解析 pacman.conf 内容里的 CacheDir 指令；
// 支持一条多项与多条指令，忽略注释行。均未配置时回退默认路径。
func parseCacheDirs(conf string) []string {
	var out []string
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, "=")
		if i < 0 || strings.TrimSpace(line[:i]) != "CacheDir" {
			continue
		}
		out = append(out, strings.Fields(line[i+1:])...)
	}
	if len(out) == 0 {
		return []string{defaultPacmanCache}
	}
	return out
}

// resolveHostCacheDirsFrom 解析配置并剔除磁盘上不存在的目录。
func resolveHostCacheDirsFrom(conf string) []string {
	var out []string
	for _, d := range parseCacheDirs(conf) {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// pacmanConfKey 取 pacman.conf 一行的键值对；无 "=" 的行返回空键。
func pacmanConfKey(line string) (string, string) {
	t := strings.TrimSpace(line)
	i := strings.Index(t, "=")
	if i < 0 {
		return "", ""
	}
	return strings.TrimSpace(t[:i]), strings.TrimSpace(t[i+1:])
}

// ensureExtraCacheDir 把宿主机缓存目录登记进容器 pacman.conf。
// 幂等；插入位置优先级：最后一条 CacheDir 之后 > [options] 行之后 > 文件尾。
func ensureExtraCacheDir(conf, extra string) string {
	for _, l := range strings.Split(conf, "\n") {
		if k, v := pacmanConfKey(l); k == "CacheDir" && v == extra {
			return conf // 已登记
		}
	}
	add := "CacheDir = " + extra
	lines := strings.Split(conf, "\n")
	insert := func(at int) string {
		out := make([]string, 0, len(lines)+1)
		out = append(out, lines[:at]...)
		out = append(out, add)
		out = append(out, lines[at:]...)
		return strings.Join(out, "\n")
	}
	at := -1
	for i, l := range lines {
		if k, _ := pacmanConfKey(l); k == "CacheDir" {
			at = i + 1
		}
	}
	if at >= 0 {
		return insert(at)
	}
	for i, l := range lines {
		if strings.TrimSpace(l) == "[options]" {
			return insert(i + 1)
		}
	}
	return conf + "\n" + add + "\n"
}

// ensureOwnCacheDir 把树自身可写缓存目录登记为 pacman.conf 的第一条
// CacheDir，让下载落进树内缓存（第一个可写者用于写）。幂等；插入位置
// 优先级：第一条 CacheDir 之前 > [options] 行之后 > 文件首。
func ensureOwnCacheDir(conf, dir string) string {
	for _, l := range strings.Split(conf, "\n") {
		if k, v := pacmanConfKey(l); k == "CacheDir" {
			for _, d := range strings.Fields(v) {
				if d == dir {
					return conf // 已登记
				}
			}
		}
	}
	add := "CacheDir = " + dir
	lines := strings.Split(conf, "\n")
	insert := func(at int) string {
		out := make([]string, 0, len(lines)+1)
		out = append(out, lines[:at]...)
		out = append(out, add)
		out = append(out, lines[at:]...)
		return strings.Join(out, "\n")
	}
	for i, l := range lines {
		if k, _ := pacmanConfKey(l); k == "CacheDir" {
			return insert(i)
		}
	}
	for i, l := range lines {
		if strings.TrimSpace(l) == "[options]" {
			return insert(i + 1)
		}
	}
	return add + "\n" + conf
}

// installPkgs 在子系统内增量安装。装配阶段不再访问 AUR：官方包来自镜像、
// AUR 包来自本地软件包池仓库（预备层已预构建），统一是普通 pacman 事务。
// groups 用于解析组构建容器的 /tmp 大小（见 nspawnGroup）。
func (b *Builder) installPkgs(root string, pkgs []string, groups ...string) error {
	if len(pkgs) == 0 {
		return nil
	}
	// 原则：容器内事务永远 -Sy <pkgs>，依赖解析基于此刻的最新数据库，
	// 杜绝“旧库解出新镜像没有的版本”与部分升级两类经典陷阱。
	args := append([]string{"pacman", "-Sy", "--needed", "--noconfirm"}, pkgs...)
	return b.nspawnGroup(root, groups, args...)
}

// syncPool 宿主机侧一次性下载全部闭包并集进池（只下载不安装），
// 并初始化池的空仓库数据库供后续 repo-add 追加。
func (b *Builder) syncPool(a *reconcile.Action) error {
	if len(a.Pkgs) == 0 {
		return nil
	}
	fmt.Fprintf(b.Out, "[pool] 同步软件包池 %s（%d 个包，只下载不安装）\n",
		a.Path, len(a.Pkgs))
	if err := b.ensurePoolWritable(a.Path); err != nil {
		return err
	}
	// 下载走默认缓存（pacman 的沙箱化下载器只保证自家目录可写，
	// 自定义 --cachedir 会吃到 nobody 身份的 EACCES），随后收割进池。
	args := append([]string{"pacman", "-Swdd", "--noconfirm"}, a.Pkgs...)
	if err := b.Runner.Run(args[0], args[1:]...); err != nil {
		return fmt.Errorf("下载软件包失败: %w", err)
	}
	if err := b.harvestToPool(a.Pkgs); err != nil {
		return err
	}
	// 空仓库库先行建好：即便本轮没有 AUR 构建，节点登记的池仓库也能正常解析
	if err := b.Runner.Run("repo-add", b.poolDBPath()); err != nil {
		fmt.Fprintf(b.Out, "[警告] 初始化池仓库数据库失败：%v\n", err)
	}
	return nil
}

// harvestToPool 用 pacman -Sp 列出各包的本地归档路径（file:// 命中缓存），
// 逐个 reflink 复制进池。同盘复制共享数据块，池不额外占空间。
// 指向远端镜像的条目说明默认缓存意外缺货，警告并跳过（下轮 -Sw 补齐）。
func (b *Builder) harvestToPool(pkgs []string) error {
	spArgs := append([]string{"pacman", "-Spdd", "--noconfirm"}, pkgs...)
	out, err := b.Runner.RunOutput(spArgs[0], spArgs[1:]...)
	if err != nil {
		return fmt.Errorf("查询包归档路径失败: %w", err)
	}
	skip := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, " ") {
			continue // 空行或表格头等杂项
		}
		if !strings.HasPrefix(line, "file://") {
			skip++
			continue
		}
		src, uerr := url.PathUnescape(strings.TrimPrefix(line, "file://"))
		if uerr != nil || !strings.Contains(src, ".pkg.tar.") {
			continue
		}
		dst := filepath.Join(b.poolDir(), filepath.Base(src))
		if err := b.Runner.Run("cp", "-a", "--reflink=always", src, dst); err != nil {
			return fmt.Errorf("收割 %s 进池失败（强制 reflink：池须与宿主缓存同在 btrfs）: %w",
				filepath.Base(src), err)
		}
	}
	if skip > 0 {
		fmt.Fprintf(b.Out, "[提示] %d 个包未在本地缓存命中（已跳过），下轮 sync_pool 会随 -Sw 补齐\n", skip)
	}
	return nil
}

// ensurePoolWritable 创建并放权软件包池目录。
// 池被三种身份写：宿主机 pacman -Sw（通常 root）、车间内 builder 用户
// （PKGDEST 读写挂载）、以及运行在用户命名空间里的上述进程（uid 经映射
// 后在宿主侧可能呈现为 65534/nobody）。目录权限放宽到 0777 让所有映射
// 视角都可落盘；池内容全是可公开读取的包产物，无越权风险。同时做一次
// 试写入预检，把“磁盘/配额/挂载属性”类环境故障提前暴露成明确诊断。
func (b *Builder) ensurePoolWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		fmt.Fprintf(b.Out, "[警告] 放开 %s 目录权限失败：%v\n", dir, err)
	}
	probe, err := os.CreateTemp(dir, ".meowsub-probe-*")
	if err != nil {
		return fmt.Errorf(
			"软件包池不可写（%v）。自检：目录模式见 ls -ld %s；"+
				"请检查 btrfs 子卷配额是否打满、父分区挂载属性、"+
				"以及执行本命令的会话是否存在用户命名空间映射（uid 显示为 nobody）",
			err, dir)
	}
	probe.Close()
	os.Remove(probe.Name())
	return nil
}

// ensureBuilder 常驻编译车间生命周期：
// 首次 = arch 的 reflink 分身 + PKGDEST 注入 + 池仓库登记；
// 之后每次 = 滚动更新，与 arch 同节奏。
func (b *Builder) ensureBuilder(a *reconcile.Action, st *state.State, now time.Time) error {
	root := a.Path
	if fi, err := os.Stat(root); err == nil && fi.IsDir() {
		if m, mErr := state.ReadMarker(root); mErr == nil && m != nil &&
			m.Kind == state.KindBuilder && m.Name == "builder" {
			fmt.Fprintf(b.Out, "[builder] 滚动更新编译车间 %s\n", root)
			b.registerHostCache(root)
			b.registerPoolRepo(root)
			// -Syu 刷新所有已登记仓库（含池）并滚动升级，一步到位
			if err := b.nspawn(root, "pacman", "-Syu", "--noconfirm"); err != nil {
				return err
			}
			// 滚动更新可能 bump libalpm soname，先确保 paru 可用；
			// 不再容器内现编——paru 归仓库供给。
			if err := b.ensureParuHealthy(root); err != nil {
				return err
			}
			b.touchBuilderRecord(st, now)
			return state.WriteMarker(root, &state.Marker{Kind: state.KindBuilder,
				Name: "builder", Parent: "arch"})
		}
	}
	// 重建：无论原目录缺失还是无效标记，一律先清场再从 arch 分身复制
	fmt.Fprintf(b.Out, "[builder] 从 %s reflink 复制编译车间\n", b.archDir())
	if err := b.Runner.Run("rm", "-rf", root); err != nil {
		return err
	}
	if err := b.Runner.Run("cp", "-a", "--reflink=always", b.archDir(), root); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil { // 记录式 Runner 兜底
		return err
	}
	if err := state.RemoveMarker(root); err != nil {
		return err
	}
	b.registerHostCache(root)
	b.registerPoolRepo(root)
	// 分身继承的 paru 可能因 arch 后续滚更而断链，构建前必须健康
	if err := b.ensureParuHealthy(root); err != nil {
		return err
	}
	rec := &state.NodeRecord{Name: "builder", Path: root,
		Kind: state.KindBuilder, Parent: "arch"}
	if st.Builder != nil {
		rec.CreatedAt = st.Builder.CreatedAt
	} else {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	st.Builder = rec
	return state.WriteMarker(root, &state.Marker{Kind: state.KindBuilder,
		Name: "builder", Parent: "arch"})
}

func (b *Builder) touchBuilderRecord(st *state.State, now time.Time) {
	if st.Builder == nil {
		st.Builder = &state.NodeRecord{Name: "builder",
			Path: b.builderRoot(), Kind: state.KindBuilder, Parent: "arch",
			CreatedAt: now}
	}
	st.Builder.UpdatedAt = now
}

// poolVersions 包名 -> 池内已收录的全部版本（repo-add 允许同名多版本并存）。
type poolVersions map[string][]string

// filterBuiltTargets 按池内已收录版本过滤 AUR 构建目标：
// 版本已知且池内有同版本产物 → 跳过；版本未知（解析期未取得上游版本）
// 一律保守构建。返回仍需构建的目标与跳过数。
func filterBuiltTargets(pkgs []string, wanted map[string]string, have poolVersions) (todo []string, skipped int) {
	for _, n := range pkgs {
		want := wanted[n]
		if want != "" && hasPoolVersion(have[n], want) {
			skipped++
			continue
		}
		todo = append(todo, n)
	}
	return todo, skipped
}

// hasPoolVersion 判断池内是否已有 want 的产物：精确相等，或构建版本是
// 同静态版本的 git 构建（见 isGitBuildOf）。
func hasPoolVersion(built []string, want string) bool {
	for _, v := range built {
		if v == want || isGitBuildOf(v, want) {
			return true
		}
	}
	return false
}

// isGitBuildOf 判断 built 是否为 want 的 git 构建版本。VCS 型 PKGBUILD
// 的 pkgver() 在构建时给静态版本追加 .r<提交数>.g<哈希>（makepkg 统一
// 约定），AUR 元数据永远不含该段，精确比较会令此类包每轮重编；故
// pkgrel 相等且 built 的 pkgver 以 want 的 pkgver 开头、余下恰为摘要段
// 即命中。不合约定的版本一律返回 false，保守重编。
func isGitBuildOf(built, want string) bool {
	bPkg, bRel, ok := cutVersion(built)
	if !ok {
		return false
	}
	wPkg, wRel, ok := cutVersion(want)
	if !ok || bRel != wRel {
		return false
	}
	summary, ok := strings.CutPrefix(bPkg, wPkg)
	if !ok {
		return false
	}
	return isGitSummary(summary)
}

// cutVersion 按最后一个 '-' 拆出版本号的 pkgver 与 pkgrel。
func cutVersion(v string) (pkg, rel string, ok bool) {
	i := strings.LastIndex(v, "-")
	if i < 0 {
		return "", "", false
	}
	return v[:i], v[i+1:], true
}

// isGitSummary 校验 makepkg git 版本的摘要段 ".r<提交数>.g<小写十六进制>"。
func isGitSummary(s string) bool {
	rest, ok := strings.CutPrefix(s, ".r")
	if !ok {
		return false
	}
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	if i == 0 {
		return false
	}
	hash, ok := strings.CutPrefix(rest[i:], ".g")
	if !ok || hash == "" {
		return false
	}
	for _, c := range hash {
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' {
			continue
		}
		return false
	}
	return true
}

func hasStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func minusStrings(all, keep []string) (out []string) {
	kset := map[string]bool{}
	for _, k := range keep {
		kset[k] = true
	}
	for _, v := range all {
		if !kset[v] {
			out = append(out, v)
		}
	}
	return out
}

// parseRepoDesc 从 pacman 仓库数据库条目的 desc 文本中取包名与版本。
func parseRepoDesc(content string) (name, version string) {
	var field string
	for _, ln := range strings.Split(content, "\n") {
		switch strings.TrimSpace(ln) {
		case "%NAME%":
			field = "name"
			continue
		case "%VERSION%":
			field = "version"
			continue
		}
		if field == "" || strings.TrimSpace(ln) == "" {
			field = ""
			continue
		}
		val := strings.TrimSpace(ln)
		if field == "name" && name == "" {
			name = val
		} else if field == "version" && version == "" {
			version = val
		}
		field = ""
	}
	return name, version
}

// poolRepoVersions 解析包池的本地仓库数据库。数据库不存在或读取失败时
// 返回空表（所有目标按需构建，随后 repo-add 自然收录修正）。
func (b *Builder) poolRepoVersions() poolVersions {
	out := poolVersions{}
	db := b.poolDBPath()
	listing, err := b.Runner.RunOutput("tar", "-tf", db)
	if err != nil {
		if _, statErr := os.Stat(db); statErr == nil {
			fmt.Fprintf(b.Out, "[警告] 读取包池仓库数据库失败（%v），本轮 AUR 目标将全部按需构建\n", err)
		}
		return out
	}
	for _, entry := range strings.Split(listing, "\n") {
		entry = strings.TrimSpace(entry)
		if !strings.HasSuffix(entry, "/desc") {
			continue
		}
		content, err := b.Runner.RunOutput("tar", "-xOf", db, entry)
		if err != nil {
			continue
		}
		name, ver := parseRepoDesc(content)
		if name == "" || ver == "" {
			continue
		}
		if !hasStr(out[name], ver) {
			out[name] = append(out[name], ver)
		}
	}
	return out
}

// aurRPCInfoURL AUR RPC v5 info 查询入口（宿主机直连，一次批量复核）。
const aurRPCInfoURL = "https://aur.archlinux.org/rpc/v5/info?"

// aurLiveVersions 批量查询上游实时版本（pkgver-pkgrel）。网络或解析失败时
// 返回空表与错误——调用方应保守维持既有跳过判定。
func aurLiveVersions(names []string) (map[string]string, error) {
	out := map[string]string{}
	if len(names) == 0 {
		return out, nil
	}
	q := url.Values{}
	for _, n := range names {
		q.Add("arg[]", n)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(aurRPCInfoURL + q.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Results []struct {
			Name    string `json:"Name"`
			Version string `json:"Version"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	for _, r := range body.Results {
		if r.Name != "" && r.Version != "" {
			out[r.Name] = r.Version
		}
	}
	return out, nil
}

// promoteStale 对“池内同版本、准备跳过”的目标做实时 RPC 复核：
// 上游版本与本地元数据不一致（转储缓存最长 6 小时滞后的窗口）则把目标
// 提升回本轮构建。返回被提升的名。live 缺失某名（上游查不到）不动作。
func promoteStale(skipped []string, wanted map[string]string, live map[string]string) []string {
	var promoted []string
	for _, n := range skipped {
		lv := live[n]
		if lv != "" && lv != wanted[n] {
			promoted = append(promoted, n)
		}
	}
	return promoted
}

// buildAurPackages 在车间内以 builder 用户跑 paru 预构建全部 AUR 目标。
// --rebuild 强制重编（不受“已装即跳过”影响），makepkg 的 PKGDEST 把产物
// 直接送入池；随后宿主机 repo-add 收录，并清掉车间里的叶子包保证下轮
// 仍走同一条可复现路径。
// 版本闸门：解析期拿到上游版本且池内已收录同版本产物的目标跳过；
// 跳过前再用实时 RPC 复核一遍上游版本，杜绝元数据滞后导致的漏追版；
// 版本变化（或未知版本）的目标先从池仓库摘除登记、再进车间重编，
// 保证它们始终以 AUR 分类走真实的 makepkg 流程。
func (b *Builder) buildAurPackages(a *reconcile.Action) error {
	if len(a.Pkgs) == 0 {
		return nil
	}
	have := b.poolRepoVersions()
	fmt.Fprintf(b.Out, "[aur] 池内已收录 %d 个包的版本索引\n", len(have))
	todo, _ := filterBuiltTargets(a.Pkgs, a.Versions, have)
	skippedNames := minusStrings(a.Pkgs, todo)
	if len(skippedNames) > 0 {
		if live, err := aurLiveVersions(skippedNames); err != nil {
			fmt.Fprintf(b.Out, "[aur] 实时复核上游版本失败（%v），维持既有跳过判定\n", err)
		} else if promoted := promoteStale(skippedNames, a.Versions, live); len(promoted) > 0 {
			fmt.Fprintf(b.Out, "[aur] 复核发现 %d 个目标上游有新版（本地元数据滞后），加入本轮：%v\n",
				len(promoted), promoted)
			todo = append(todo, promoted...)
			sort.Strings(todo)
			skippedNames = minusStrings(a.Pkgs, todo)
		}
		fmt.Fprintf(b.Out, "[aur] %d 个目标池内已是同版本，跳过重编：%v\n",
			len(skippedNames), skippedNames)
	}
	if len(todo) == 0 {
		fmt.Fprintf(b.Out, "[aur] 全部 %d 个 AUR 目标均为最新，无需打包\n", len(a.Pkgs))
		return nil
	}
	a.Pkgs = todo
	root := b.builderRoot()
	// 摘除重建目标的池仓库登记：目标一旦被 repo-add 收录，paru 就把它当
	// 普通同步仓库包——未安装时直接 file:// 装旧版、已安装时 --needed 判
	// “已是最新”空转退出 0，两种路都绕过 makepkg 且使 --rebuild 形同虚设
	// （它只作用于 AUR 分类的目标）。摘除后目标必然回归 AUR 分类走真构建，
	// 产出后由下方 repo-add 重新登记。首建目标尚不在库中，报错属预期。
	fmt.Fprintf(b.Out, "[aur] 从池仓库暂摘重建目标：%v\n", a.Pkgs)
	removeArgs := append([]string{"repo-remove", b.poolDBPath()}, a.Pkgs...)
	if err := b.Runner.Run(removeArgs[0], removeArgs[1:]...); err != nil {
		fmt.Fprintf(b.Out, "[aur] 目标此前不在池仓库中（继续）\n")
	}
	// 宿主侧刚改动过池库：容器内必须重新 -Sy 才能看到摘除后的状态。
	if err := b.nspawn(root, "pacman", "-Sy", "--noconfirm"); err != nil {
		return fmt.Errorf("刷新池仓库数据库失败: %w", err)
	}
	fmt.Fprintf(b.Out, "[pool] 在编译车间预构建 AUR 目标：%v\n", a.Pkgs)
	// PKGDEST 经环境变量注入（makepkg 无 drop-in 配置机制，/etc/makepkg.d
	// 是不存在的特性；env 可覆盖 makepkg.conf 且穿透 runuser），
	// AUR 构建产物直落读写挂载的池。
	// 残留叶子包清障：只要目标已在车间内安装且版本一致，paru/pacman 的
	// --needed 就会判定“up to date -- skipping / nothing to do”直接跳过
	// 重建，且以退出码 0 收场——导致闸门判了要重建、实际却空转并把旧包
	// 反复收录。故构建前无条件尝试卸载；目标本就未安装时 pacman 报错属预期。
	fmt.Fprintf(b.Out, "[aur] 清理车间残留叶子包（未安装则忽略）：%v\n", a.Pkgs)
	purge := append([]string{"pacman", "-Rns", "--noconfirm"}, a.Pkgs...)
	if err := b.nspawn(root, purge...); err != nil {
		fmt.Fprintf(b.Out, "[aur] 目标此前未安装（继续）\n")
	}
	args := append([]string{
		"--setenv=PKGDEST=" + poolMount,
		"runuser", "-u", "builder", "--", "paru", "-S",
		"--rebuild", "--needed", "--noconfirm",
	}, a.Pkgs...)
	if err := b.nspawnRWPool(root, args...); err != nil {
		return fmt.Errorf("AUR 预构建失败: %w", err)
	}
	glob := filepath.Join(b.poolDir(), "*.pkg.tar.zst")
	matches, _ := filepath.Glob(glob)
	repoArgs := append([]string{b.poolDBPath()}, matches...)
	if err := b.Runner.Run("repo-add", repoArgs...); err != nil {
		fmt.Fprintf(b.Out, "[警告] 池仓库收录失败：%v\n", err)
	}
	cleanup := append([]string{"pacman", "-Rns", "--noconfirm"}, a.Pkgs...)
	if err := b.nspawn(root, cleanup...); err != nil {
		fmt.Fprintf(b.Out, "[警告] 清理车间内 AUR 叶子包失败（不影响构建）：%v\n", err)
	}
	return nil
}

// registerPoolRepo 在容器 pacman.conf 登记本地池仓库（幂等）。
// 节点通过只读挂载直接 file:// 引用它；AUR 包对装配期而言只是普通仓库包。
func (b *Builder) registerPoolRepo(root string) {
	confPath := filepath.Join(root, "etc", "pacman.conf")
	data, err := os.ReadFile(confPath)
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(b.Out, "[警告] 读取 %s 失败：%v\n", confPath, err)
		return
	}
	updated := ensureRepoSection(string(data), poolRepoName, poolMount)
	if updated == string(data) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(confPath), 0o755); err != nil {
		fmt.Fprintf(b.Out, "[警告] 创建目录失败：%v\n", err)
		return
	}
	if err := os.WriteFile(confPath, []byte(updated), 0o644); err != nil {
		fmt.Fprintf(b.Out, "[警告] 写入 %s 失败：%v\n", confPath, err)
	}
}

// ensureRepoSection 幂等地向 pacman.conf 追加池仓库段。
// 插入位置：最后一个仓库段之前不会破坏 [options]——因此固定追加文件尾，
// pacman 对段顺序不敏感（签名策略沿用全局 SigLevel 设置）。
func ensureRepoSection(conf, name, mount string) string {
	header := "[" + name + "]"
	for _, l := range strings.Split(conf, "\n") {
		if strings.TrimSpace(l) == header {
			return conf
		}
	}
	block := "\n# meowSub 本地软件包池（file:// 只读挂载）\n" +
		header + "\n" +
		"Server = file://" + mount + "\n" +
		"SigLevel = Never\n"
	conf = strings.TrimRight(conf, "\n") + "\n"
	return conf + block
}

// Execute 按序执行动作并维护状态与标记文件；结束后写回 state.json。
func (b *Builder) Execute(acts []*reconcile.Action, st *state.State, cfgHash string) (*state.State, error) {
	now := time.Now()
	// 内存目录树懒建立：首个小组动作前，树成品已全部落定，此时扫描
	// arch 与各树成品各一遍，此后落盘查重全走内存（小组的改动再增量并入）。
	var idx *fileindex.Index
	buildIdx := func() (*fileindex.Index, error) {
		if idx != nil {
			return idx, nil
		}
		var err error
		idx, err = b.buildFileIndex(acts)
		return idx, err
	}
	for _, a := range acts {
		var err error
		switch a.Type {
		case reconcile.ActRecreateBase:
			err = b.recreateBase(a, st, now)
		case reconcile.ActUpdateBase:
			err = b.updateBase(a, st, now)
		case reconcile.ActRemoveStale:
			err = b.Runner.Run("rm", "-rf", a.Path)
		case reconcile.ActSyncPool:
			err = b.syncPool(a)
		case reconcile.ActEnsureBuilder:
			err = b.ensureBuilder(a, st, now)
		case reconcile.ActBuildAur:
			err = b.buildAurPackages(a)
		case reconcile.ActCreateNode:
			err = b.createNode(a, st, now, false)
		case reconcile.ActRecreateNode:
			err = b.createNode(a, st, now, true)
		case reconcile.ActDeleteIntermediates:
			err = b.deleteIntermediates(a, st)
		case reconcile.ActBuildFinalSmall, reconcile.ActUpdateFinalSmall:
			var ix *fileindex.Index
			if ix, err = buildIdx(); err == nil {
				err = b.materializeSmall(a, st, now, ix,
					a.Type == reconcile.ActUpdateFinalSmall)
			}
		case reconcile.ActPruneNode:
			fmt.Fprintf(b.Out, "[prune] 删除孤儿子系统 %s\n", a.Path)
			err = b.Runner.Run("rm", "-rf", a.Path)
			st.Nodes = removeRecord(st.Nodes, a.Name)
		case reconcile.ActOrphan:
			fmt.Fprintf(b.Out, "[警告] 孤儿子系统 %s 未处理：%s\n", a.Path, a.Reason)
		}
		if err != nil {
			return st, fmt.Errorf("执行 %s(%s): %w", a.Type, a.Name, err)
		}
	}
	st.ConfigHash = cfgHash
	st.Nodes = dropRecordsByKind(st.Nodes, state.KindIntermediate) // 中间层即用即弃后不留残档
	if err := st.Save(b.metaBase()); err != nil {
		return st, err
	}
	return st, nil
}

// buildFileIndex 扫描基础系统与本轮创建/重建的全部树成品，建立内存目录树。
// 只认有 usr/ 的完整系统根；池、车间与 runs 不参与共享来源。
func (b *Builder) buildFileIndex(acts []*reconcile.Action) (*fileindex.Index, error) {
	ix := fileindex.New()
	add := func(root string) {
		if _, err := os.Stat(filepath.Join(root, "usr")); err != nil {
			return
		}
		if err := ix.ScanSystem(root); err != nil {
			fmt.Fprintf(b.Out, "[索引] 跳过 %s：%v\n", root, err)
			return
		}
	}
	add(b.archDir())
	seen := map[string]bool{}
	for _, a := range acts {
		if a.Node == nil || a.Node.Kind != planner.KindFinal {
			continue
		}
		switch a.Type {
		case reconcile.ActCreateNode, reconcile.ActRecreateNode:
			if !seen[a.Path] {
				seen[a.Path] = true
				add(a.Path)
			}
		}
	}
	fmt.Fprintf(b.Out, "[索引] 目录树已读入内存：%d 个系统\n", len(ix.Systems()))
	return ix, nil
}

// deleteIntermediates 删除本计划生成的全部中间层并同步摘除状态记录。
// 中间层是普通 reflink 目录（非 btrfs 子卷），递归删除即可。
func (b *Builder) deleteIntermediates(a *reconcile.Action, st *state.State) error {
	names := map[string]bool{}
	for _, p := range a.Paths {
		names[filepath.Base(p)] = true
		fmt.Fprintf(b.Out, "[收割] 删除中间层 %s\n", p)
		if err := b.Runner.Run("rm", "-rf", p); err != nil {
			return err
		}
	}
	kept := st.Nodes[:0]
	for _, r := range st.Nodes {
		if r.Kind == state.KindIntermediate && names[r.Name] {
			continue
		}
		kept = append(kept, r)
	}
	st.Nodes = kept
	return nil
}

// materializeSmall 小组的内存构建路径：滚更（包集合无差异）时把盘上成品
// 复制进 tmpfs 做 -Syu；否则把基础 arch 整份复制进 tmpfs 全量安装目标集。
// 差异经目录树索引去重落盘，lower 取盘上成品（存在即用）：消失的旧文件
// 会被移除、未变的文件不重写。
func (b *Builder) materializeSmall(a *reconcile.Action, st *state.State,
	now time.Time, ix *fileindex.Index, update bool) error {
	n := a.Node
	if n == nil {
		return fmt.Errorf("动作缺少期望节点信息")
	}
	dest := a.Path
	src := b.archDir()
	mode := "重建"
	if update {
		if a.Record == nil {
			return fmt.Errorf("滚更动作缺少已有记录")
		}
		src = dest
		mode = "滚更"
	}
	fmt.Fprintf(b.Out, "[mem] %s小组成品 %s（根=%s）\n", mode, dest, src)
	// 安装增量计入 tmpfs 容量：4G 事故正是"装下了树、装包撞死"。
	extra, blind := b.memInstallExtra(n.InstallSet)
	if blind > 0 {
		fmt.Fprintf(b.Out, "[mem] tmpfs 安装增量预估 %s（%d 个包，%d 个缺尺寸记录按池包体积折算）\n",
			humanSize(extra), len(n.InstallSet), blind)
	} else {
		fmt.Fprintf(b.Out, "[mem] tmpfs 安装增量预估 %s（%d 个包）\n",
			humanSize(extra), len(n.InstallSet))
	}
	mem, cleanup, err := b.stager().Stage(n.Name, src, extra)
	if err != nil {
		return err
	}
	defer cleanup()
	if b.afterStage != nil {
		b.afterStage(mem)
	}
	b.registerHostCache(mem)
	b.registerPoolRepo(mem)

	if update {
		if err := b.nspawnGroup(mem, n.Groups, "pacman", "-Syu", "--noconfirm"); err != nil {
			return fmt.Errorf("副本内滚更失败: %w", err)
		}
	} else if err := b.installPkgs(mem, n.InstallSet, n.Groups...); err != nil {
		return err
	}

	// 落盘基准：盘上成品存在即作 lower（不限于滚更路径——重装场景同样
	// 需要移除消失的旧文件、跳过未变文件）。
	lower := ""
	if _, serr := os.Stat(filepath.Join(dest, "usr")); serr == nil {
		lower = dest
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	newFiles, err := b.landFromMemory(mem, lower, dest, ix)
	if err != nil {
		return err
	}
	if b.Verify {
		if err := b.verifyLanding(mem, dest); err != nil {
			return fmt.Errorf("落盘一致性校验失败（本轮标记不写，下轮调和按未完成重建）: %w", err)
		}
	}
	parentKey := "arch"
	kind := state.KindFinal
	m := &state.Marker{Kind: kind, Name: n.Name, Parent: parentKey,
		Groups: n.Groups, InstallSet: n.InstallSet}
	if m.Users, m.Export, err = b.bakeNode(dest, b.groupOpts(n.Name)); err != nil {
		return err
	}
	m.Services = b.groupServices(n.Name)
	if err := b.applyCopyTable(dest, b.groupCopies(n.Name)); err != nil {
		return err
	}
	if err := state.WriteMarker(dest, m); err != nil {
		return err
	}
	old := findRecord(st.Nodes, n.Name)
	rec := &state.NodeRecord{Name: n.Name, Path: dest, Kind: kind,
		Parent: parentKey, Groups: n.Groups, InstallSet: n.InstallSet}
	rec.Users, rec.Services, rec.Export = m.Users, m.Services, m.Export
	if old != nil {
		rec.CreatedAt = old.CreatedAt
		*old = *rec
	} else {
		rec.CreatedAt = now
		st.Nodes = append(st.Nodes, rec)
	}
	rec.UpdatedAt = now
	fmt.Fprintf(b.Out, "[mem] %s 完成：落盘 %d 个差异文件\n", dest, len(newFiles))
	return nil
}

// landFromMemory 把内存副本相对盘上基准的差异逐文件落盘：
// 常规文件先对暂存文件算一次校验和，再到索引按同路径同尺寸筛候选，读候选
// 核验，命中则 reflink 共享对方数据块，全不命中才真正写新数据；符号链接
// 按“路径→目标”整体落盘（无数据块，不进去重索引）。返回本轮落盘的相对
// 路径清单（常规文件 + 链接，调用方据其做增量并入索引）。
func (b *Builder) landFromMemory(mem, lower, dest string, ix *fileindex.Index) ([]string, error) {
	newFiles, removed, newLinks := diffForLanding(mem, lower)
	relinked, kept, linked := 0, 0, 0
	for _, rel := range newFiles {
		srcFile := filepath.Join(mem, rel)
		fi, err := os.Stat(srcFile)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		sum, err := fileindex.SHA256File(srcFile)
		if err != nil { // 读取失败的极端情况：直接落盘
			if lerr := b.placePlain(srcFile, filepath.Join(dest, rel)); lerr != nil {
				return nil, lerr
			}
			kept++
			continue
		}
		hit := b.chooseSource(ix, rel, fi.Size(), sum, dest, lower)
		dstPath := filepath.Join(dest, rel)
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return nil, err
		}
		os.Remove(dstPath)
		if hit != "" {
			if err := b.Runner.Run("cp", "-a", "--reflink=always", hit, dstPath); err != nil {
				return nil, fmt.Errorf("dedup reflink %s 失败: %w", rel, err)
			}
			relinked++
			continue
		}
		if err := b.placePlain(srcFile, dstPath); err != nil {
			return nil, fmt.Errorf("落盘新文件 %s 失败: %w", rel, err)
		}
		kept++
	}
	for _, rel := range newLinks {
		target, err := os.Readlink(filepath.Join(mem, rel))
		if err != nil {
			return nil, fmt.Errorf("读取暂存链接 %s: %w", rel, err)
		}
		dstPath := filepath.Join(dest, rel)
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return nil, err
		}
		os.Remove(dstPath) // 目标位置可能是旧链接，或已变成普通/特殊文件
		if err := os.Symlink(target, dstPath); err != nil {
			return nil, fmt.Errorf("落盘链接 %s: %w", rel, err)
		}
		linked++
	}
	for _, rel := range removed {
		os.Remove(filepath.Join(dest, rel))
	}
	ix.Absorb(dest, newFiles)
	fmt.Fprintf(b.Out, "[mem] 落盘完成：reflink 回共享 %d，新增独占 %d，链接 %d，移除 %d\n",
		relinked, kept, linked, len(removed))
	return append(newFiles, newLinks...), nil
}

// chooseSource 在索引里找同路径同尺寸且内容一致的来源系统文件。
// 自身目录（dest/lower）不作为候选——reflink 自己等于没省；命中候选需
// 读盘做 SHA256 核验，不一致换下一个（摘要只是初筛）。找不到返回空串。
func (b *Builder) chooseSource(ix *fileindex.Index, rel string, size int64,
	sumHex, dest, lower string) string {
	for _, root := range ix.Candidates(rel, size) {
		if root == dest || root == lower {
			continue
		}
		candSum, err := fileindex.SHA256File(filepath.Join(root, rel))
		if err == nil && candSum == sumHex {
			return filepath.Join(root, rel)
		}
	}
	return ""
}

func (b *Builder) placePlain(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	fi, _ := in.Stat()
	out.Chmod(fi.Mode().Perm())
	_, err = io.Copy(out, in)
	return err
}

// verifyLanding 落盘一致性校验（--verify）：逐条比对暂存树（内存系统）与
// 落盘树（实体系统）——常规文件比尺寸与 SHA256，符号链接比目标串；反向
// 要求实体侧不出现暂存侧没有的条目。
// 不一致即报错并附前若干条明细，由调用方中止本轮收尾。
func (b *Builder) verifyLanding(mem, dest string) error {
	const maxShow = 10
	var bad []string
	memSeen := map[string]bool{}
	count := 0
	filepath.WalkDir(mem, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(mem, path)
		if rerr != nil {
			return nil
		}
		dp := filepath.Join(dest, rel)
		switch {
		case d.Type()&os.ModeSymlink != 0:
			count++
			memSeen[rel] = true
			src, _ := os.Readlink(path)
			dst, derr := os.Readlink(dp)
			if derr != nil || src != dst {
				bad = append(bad, fmt.Sprintf("%s: 链接不一致（暂存→%s，实体 %v）", rel, src, derr))
			}
		case d.Type().IsRegular():
			count++
			memSeen[rel] = true
			si, serr := d.Info()
			di, derr := os.Lstat(dp)
			if serr != nil || derr != nil || !di.Mode().IsRegular() {
				bad = append(bad, fmt.Sprintf("%s: 常规文件缺失或类型漂移（%v）", rel, derr))
				return nil
			}
			if si.Size() != di.Size() {
				bad = append(bad, fmt.Sprintf("%s: 尺寸不一致（%d ≠ %d）", rel, si.Size(), di.Size()))
				return nil
			}
			ss, se := fileindex.SHA256File(path)
			ds, de := fileindex.SHA256File(dp)
			if se != nil || de != nil || ss != ds {
				bad = append(bad, fmt.Sprintf("%s: 内容不一致", rel))
			}
		}
		return nil
	})
	filepath.WalkDir(dest, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink == 0 && !d.Type().IsRegular() {
			return nil // socket/fifo 等特殊文件不在比对口径内
		}
		rel, rerr := filepath.Rel(dest, path)
		if rerr != nil {
			return nil
		}
		if !memSeen[rel] {
			bad = append(bad, fmt.Sprintf("%s: 实体侧多出条目", rel))
		}
		return nil
	})
	if len(bad) > 0 {
		show := bad
		if len(show) > maxShow {
			show = append(show[:maxShow:maxShow], fmt.Sprintf("…共 %d 处不一致", len(bad)))
		}
		return fmt.Errorf("内存/实体不一致 %d 处：\n  %s", len(bad), strings.Join(show, "\n  "))
	}
	fmt.Fprintf(b.Out, "[校验] %s：内存/实体一致（条目 %d）\n", dest, count)
	return nil
}

// diffForLanding 比较"内存副本 vs 盘上基准"（lower 为空表示全新落盘，
// 一切皆新增）：返回需落盘的常规文件、符号链接与需从盘上移除的路径。
// 暂存侧常规文件每文件只哈希一次；基准侧先以尺寸粗筛，尺寸相同才读内容
// 比对。符号链接按“路径→目标”整体比对，不读内容；暂存侧缺失的同名条目
// 无论原类型一律移除（链接与文件同权参与事务增删）。
func diffForLanding(staged, lower string) (newFiles, removed, newLinks []string) {
	sums := map[string]string{}  // rel -> 暂存侧常规文件 sha256
	sizes := map[string]int64{}  // rel -> 暂存侧常规文件尺寸
	links := map[string]string{} // rel -> 暂存侧符号链接目标
	filepath.WalkDir(staged, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(staged, path)
		if rerr != nil {
			return nil
		}
		switch {
		case d.Type().IsRegular():
			fi, ferr := d.Info()
			if ferr != nil || !fi.Mode().IsRegular() {
				return nil
			}
			sum, herr := fileindex.SHA256File(path)
			if herr != nil {
				return nil
			}
			sums[rel] = sum
			sizes[rel] = fi.Size()
		case d.Type()&os.ModeSymlink != 0:
			target, lerr := os.Readlink(path)
			if lerr != nil {
				return nil
			}
			links[rel] = target
		}
		return nil
	})

	collectNew := func() {
		for rel := range sums {
			newFiles = append(newFiles, rel)
		}
		for rel := range links {
			newLinks = append(newLinks, rel)
		}
		sort.Strings(newFiles)
		sort.Strings(newLinks)
	}
	if lower == "" {
		collectNew()
		sort.Strings(removed)
		return newFiles, removed, newLinks
	}
	filepath.WalkDir(lower, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(lower, path)
		if rerr != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			// 盘上链接：暂存侧同为目标一致的链接 → 未变化；目标漂移或
			// 暂存侧已换成常规文件 → 重写（落盘前先移除）；暂存侧没有 →
			// 随事务消失。
			if target, ok := links[rel]; ok {
				if disk, lerr := os.Readlink(path); lerr == nil && disk == target {
					delete(links, rel)
				}
				return nil
			}
			if _, isFile := sums[rel]; !isFile {
				removed = append(removed, rel)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		sum, stagedFile := sums[rel]
		if !stagedFile {
			if _, isLink := links[rel]; !isLink {
				removed = append(removed, rel) // 盘上有而暂存区没有：随事务消失
			}
			return nil
		}
		sameContent := false
		if fi, ferr := d.Info(); ferr == nil && fi.Mode().IsRegular() && fi.Size() == sizes[rel] {
			h, herr := fileindex.SHA256File(path)
			sameContent = herr == nil && h == sum
		}
		if sameContent {
			delete(sums, rel) // 未变化
		}
		return nil
	})
	collectNew()
	sort.Strings(removed)
	return newFiles, removed, newLinks
}

func subtractStrings(have, want []string) []string {
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}
	var out []string
	for _, h := range have {
		if !wantSet[h] {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

func dropRecordsByKind(nodes []*state.NodeRecord, kind string) []*state.NodeRecord {
	out := nodes[:0]
	for _, r := range nodes {
		if r.Kind == kind {
			continue
		}
		out = append(out, r)
	}
	return out
}

func (b *Builder) recreateBase(a *reconcile.Action, st *state.State, now time.Time) error {
	fmt.Fprintf(b.Out, "[base] 初始化基础系统 %s（%s）\n", a.Path, a.Reason)
	if err := b.Runner.Run("rm", "-rf", a.Path); err != nil {
		return err
	}
	if err := os.MkdirAll(a.Path, 0o755); err != nil {
		return err
	}
	pacstrapPkgs := append([]string{}, planner.DefaultBasePackages...)
	if len(b.ExtraRepos) > 0 {
		// 仓库是基础镜像属性：合成配置喂给 pacstrap，keyring/paru 等补充
		// 设施由额外仓库在首次构建时一并供给。
		confPath := a.Path + "-pacman.conf.tmp"
		if err := os.WriteFile(confPath, []byte(b.syntheticPacmanConf()), 0o644); err != nil {
			return err
		}
		defer os.Remove(confPath)
		args := append([]string{"-K", "-C", confPath, a.Path}, pacstrapPkgs...)
		args = append(args, extraRepoPackages(b.ExtraRepos)...)
		args = append(args, planner.ExtraBasePackages...)
		if err := b.Runner.Run("pacstrap", args...); err != nil {
			return err
		}
	} else if err := b.Runner.Run("pacstrap", append([]string{"-K", a.Path},
		pacstrapPkgs...)...); err != nil {
		return err
	}
	b.registerHostCache(a.Path)
	b.syncHostKeyring(a.Path) // 信任视图对齐宿主机（额外仓库的密钥由此传导）
	fmt.Fprintf(b.Out, "[base] 配置构建用户\n")
	if err := b.nspawn(a.Path, "bash", "-ec", baseSetupScript); err != nil {
		return err
	}
	allPkgs := append(pacstrapPkgs, "paru")
	if err := state.WriteMarker(a.Path, &state.Marker{Kind: state.KindBase, Name: "arch"}); err != nil {
		return err
	}
	baseState := &state.BaseRecord{Path: a.Path, Packages: allPkgs, UpdatedAt: now}
	if st.Base != nil {
		baseState.CreatedAt = st.Base.CreatedAt
	} else {
		baseState.CreatedAt = now
	}
	st.Base = baseState
	return nil
}

func (b *Builder) updateBase(a *reconcile.Action, st *state.State, now time.Time) error {
	fmt.Fprintf(b.Out, "[base] 滚动更新基础系统 %s\n", a.Path)
	b.registerHostCache(a.Path) // 自愈：上游 pacman.conf 变动（如 .pacnew）后补登记
	b.applyExtraRepos(a.Path)
	// 信任先行：任何新源包在校验前必须已有对应密钥（视图对齐宿主机），
	// 否则 cn 包会以 unknown trust 拒收（真实事故：paru 经宿主缓存命中）。
	b.syncHostKeyring(a.Path)
	// 迁移守卫：旧环境容器内现编的 paru-bin 会与仓库版 paru 冲突，先迁移
	if err := b.nspawn(a.Path, "bash", "-ec",
		"pacman -Qq paru-bin &>/dev/null && pacman -Rdd --noconfirm paru-bin || true"); err != nil {
		fmt.Fprintf(b.Out, "[警告] 迁移 paru-bin 失败：%v（继续常规更新）\n", err)
	}
	// 滚动更新并补全基础包：DefaultBasePackages 是基础镜像的恒久属性，
	// --needed 让存量 base 缺哪个补哪个——清单新增项随下一轮 build 自动
	// 传导，不为单个包写用完即死的一次性补装。
	if err := b.nspawn(a.Path, append([]string{"pacman", "-Syu", "--needed",
		"--noconfirm"}, planner.DefaultBasePackages...)...); err != nil {
		return err
	}
	// 运行时目录保留策略（幂等）：存量 base 由此补齐，见 logindDropInCommands
	if err := b.nspawn(a.Path, "bash", "-ec", logindDropInCommands); err != nil {
		return err
	}
	// keyring 包常规化（此时密钥已在环上，校验必过；缺失也非致命）
	if err := b.nspawn(a.Path, "bash", "-ec",
		"pacman -Qq archlinuxcn-keyring &>/dev/null || pacman -S --noconfirm archlinuxcn-keyring"); err != nil {
		fmt.Fprintf(b.Out, "[提示] archlinuxcn-keyring 未随仓库提供：%v\n", err)
	}
	// cn 源就绪后确保 paru 存在（旧镜像可能只有裸 builder）
	if err := b.nspawn(a.Path, "bash", "-ec",
		"pacman -Qq paru &>/dev/null || pacman -S --noconfirm paru"); err != nil {
		return fmt.Errorf("安装/恢复 paru 失败: %w", err)
	}
	if st.Base == nil {
		pkgs := append(append([]string{}, planner.DefaultBasePackages...), "paru")
		st.Base = &state.BaseRecord{Path: a.Path, Packages: pkgs, CreatedAt: now}
	}
	st.Base.UpdatedAt = now
	state.WriteMarker(a.Path, &state.Marker{Kind: state.KindBase, Name: "arch"})
	return nil
}

func (b *Builder) createNode(a *reconcile.Action, st *state.State, now time.Time, recreate bool) error {
	n := a.Node
	if n == nil {
		return fmt.Errorf("动作缺少期望节点信息")
	}
	src := b.archDir()
	if n.Parent != nil {
		src = n.Parent.Path
	}
	if recreate {
		fmt.Fprintf(b.Out, "[重建] %s（%s）\n", a.Path, a.Reason)
		if err := b.Runner.Run("rm", "-rf", a.Path); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(b.Out, "[新建] %s ← reflink %s\n", a.Path, src)
	}
	if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
		return err
	}
	if err := b.Runner.Run("cp", "-a", "--reflink=always", src, a.Path); err != nil {
		return err
	}
	// 记录式 Runner 不真正复制，真实场景下目录已由 cp 创建；此处兜底建目录
	if err := os.MkdirAll(a.Path, 0o755); err != nil {
		return err
	}
	// 清掉从父级复制来的旧标记，装完后写新标记
	if err := state.RemoveMarker(a.Path); err != nil {
		return err
	}
	b.registerHostCache(a.Path)
	b.registerPoolRepo(a.Path) // 装配期把池仓库当作普通仓库引用
	if err := b.installPkgs(a.Path, n.InstallSet, n.Groups...); err != nil {
		return err
	}
	parentKey := "arch"
	if n.Parent != nil {
		parentKey = n.Parent.Name
	}
	kind := state.KindFinal
	if n.Kind == planner.KindIntermediate {
		kind = state.KindIntermediate
	}
	m := &state.Marker{Kind: kind, Name: n.Name, Parent: parentKey,
		Groups: n.Groups, InstallSet: n.InstallSet}
	bakedUsers, bakedExport, berr := b.bakeNode(a.Path, b.groupOpts(n.Name))
	if berr != nil {
		return berr
	}
	m.Users, m.Export = bakedUsers, bakedExport
	m.Services = b.groupServices(n.Name)
	// 复制表归属最终组：中间层被多组共享，无法归属单一组声明，不施加。
	if kind == state.KindFinal {
		if err := b.applyCopyTable(a.Path, b.groupCopies(n.Name)); err != nil {
			return err
		}
	}
	if err := state.WriteMarker(a.Path, m); err != nil {
		return err
	}
	old := findRecord(st.Nodes, n.Name)
	rec := &state.NodeRecord{Name: n.Name, Path: a.Path, Kind: kind,
		Parent: parentKey, Groups: n.Groups, InstallSet: n.InstallSet}
	rec.Users, rec.Services, rec.Export = m.Users, m.Services, m.Export
	if old != nil {
		rec.CreatedAt = old.CreatedAt
		*old = *rec
	} else {
		rec.CreatedAt = now
		st.Nodes = append(st.Nodes, rec)
	}
	rec.UpdatedAt = now
	return nil
}

func findRecord(nodes []*state.NodeRecord, name string) *state.NodeRecord {
	for _, r := range nodes {
		if r.Name == name {
			return r
		}
	}
	return nil
}

func removeRecord(nodes []*state.NodeRecord, name string) []*state.NodeRecord {
	out := nodes[:0]
	for _, r := range nodes {
		if r.Name != name {
			out = append(out, r)
		}
	}
	return out
}

// renderSizes plan 渲染用的包尺寸表（nil 则不显示尺寸列）。
var renderSizes map[string]int64

// SetRenderSizes 注入包尺寸表供 plan 渲染预测列。
func SetRenderSizes(m map[string]int64) { renderSizes = m }

// RenderPlan 将期望树与动作渲染为人读文本。
//
// 尺寸列是"安装占用"口径的保守纯预测：本层 InstallSet 的 isize 求和，
// 数据全部来自仓库 sync 库（AUR 包收割前不计入）。它不含任何更新期
// 写入的预测——更新写集只能由 overlay upper 实测得出。
func RenderPlan(pl *planner.Plan, acts []*reconcile.Action) string {
	tag := map[string]string{}
	reason := map[string]string{}
	for _, a := range acts {
		switch a.Type {
		case reconcile.ActRecreateBase:
			tag["__base__"] = "[CREATE-BASE]"
			reason["__base__"] = a.Reason
		case reconcile.ActUpdateBase:
			tag["__base__"] = "[UPDATE-BASE]"
		case reconcile.ActCreateNode:
			tag[a.Name] = "[CREATE]"
		case reconcile.ActRecreateNode:
			tag[a.Name] = "[RECREATE]"
		}
		if a.Reason != "" {
			reason[a.Name] = a.Reason
		}
	}
	nodeTag := func(n *planner.Node) string {
		if t, ok := tag[n.Name]; ok {
			return t
		}
		return "[RECREATE]"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "初始系统 %s %s", filepath.Join(pl.BaseDir, "arch"), tag["__base__"])
	if r := reason["__base__"]; r != "" {
		fmt.Fprintf(&sb, " （%s）", r)
	}
	sb.WriteString("\n")

	var walk func(n *planner.Node, prefix string, last bool)
	walk = func(n *planner.Node, prefix string, last bool) {
		connector, childPrefix := "├── ", "│   "
		if last {
			connector, childPrefix = "└── ", "    "
		}
		fmt.Fprintf(&sb, "%s%s%s %s", prefix, connector, nodeTag(n), n.Name)
		fmt.Fprintf(&sb, "  <%s>", displayPath(pl.BaseDir, n))
		if len(n.InstallSet) > 0 {
			parts := make([]string, len(n.InstallSet))
			for i, p := range n.InstallSet {
				parts[i] = "+" + p
			}
			fmt.Fprintf(&sb, "  %s", strings.Join(parts, " "))
			if renderSizes != nil {
				var total int64
				for _, p := range n.InstallSet {
					total += renderSizes[p]
				}
				fmt.Fprintf(&sb, "  [isize %s]", humanSize(total))
			}
		}
		if r := reason[n.Name]; r != "" {
			fmt.Fprintf(&sb, "  （%s）", r)
		}
		sb.WriteString("\n")
		for i, c := range n.Children {
			walk(c, prefix+childPrefix, i == len(n.Children)-1)
		}
	}
	for i, r := range pl.Roots {
		walk(r, "", i == len(pl.Roots)-1)
	}

	// 小组段落：内存构建路径的成品（低于空间阈值、不进树）
	smallTag := map[string]string{}
	for _, a := range acts {
		switch a.Type {
		case reconcile.ActBuildFinalSmall:
			smallTag[a.Name] = "[BUILD-MEM]"
		case reconcile.ActUpdateFinalSmall:
			smallTag[a.Name] = "[SYNC]"
		case reconcile.ActRemoveStale:
			if _, isSmall := smallByPath(pl, a.Path); !isSmall {
				break
			}
			if _, seen := smallTag[a.Name]; !seen {
				smallTag[a.Name] = "[CLEAN-STALE]"
			}
		}
	}
	if len(pl.SmallNodes) > 0 {
		fmt.Fprintf(&sb, "\n内存构建（低于阈值小组）\n")
		for _, n := range pl.SmallNodes {
			tag := smallTag[n.Name]
			if tag == "" {
				tag = "[BUILD-MEM]"
			}
			fmt.Fprintf(&sb, "  %s %s  <%s>", tag, n.Name, n.Path)
			if len(n.InstallSet) > 0 {
				parts := make([]string, len(n.InstallSet))
				for i, p := range n.InstallSet {
					parts[i] = "+" + p
				}
				fmt.Fprintf(&sb, "  %s", strings.Join(parts, " "))
				if renderSizes != nil {
					fmt.Fprintf(&sb, "  [isize %s]",
						humanSize(installSetSize(n.InstallSet, renderSizes)))
				}
			}
			sb.WriteString("\n")
		}
	}

	counts := map[string]int{}
	poolPkgs, aurPkgs, midDeleted := 0, 0, 0
	for _, a := range acts {
		counts[string(a.Type)]++
		switch a.Type {
		case reconcile.ActSyncPool:
			poolPkgs = len(a.Pkgs)
		case reconcile.ActBuildAur:
			aurPkgs = len(a.Pkgs)
		case reconcile.ActDeleteIntermediates:
			midDeleted = len(a.Paths)
		}
	}
	if poolPkgs > 0 || counts[string(reconcile.ActEnsureBuilder)] > 0 {
		fmt.Fprintf(&sb, "\n预备层：包池同步（%d 个仓库包）", poolPkgs)
		if counts[string(reconcile.ActEnsureBuilder)] > 0 {
			fmt.Fprintf(&sb, " · 编译车间保温")
			if aurPkgs > 0 {
				fmt.Fprintf(&sb, " · AUR 预构建（%d 个；池内同版本自动跳过）", aurPkgs)
			}
		}
		sb.WriteString("\n")
	}
	if counts[string(reconcile.ActDeleteIntermediates)] > 0 {
		fmt.Fprintf(&sb, "中间层清理：收割完毕删除 %d 个中间层\n", midDeleted)
	}
	if len(pl.SmallNodes) > 0 {
		fmt.Fprintf(&sb, "内存构建：%d 个小组（先树后组、名字序逐个落盘，索引去重共享）\n",
			len(pl.SmallNodes))
	}
	fmt.Fprintf(&sb, "\n动作统计：新建 %d / 重建 %d / 孤儿 %d\n",
		counts[string(reconcile.ActCreateNode)], counts[string(reconcile.ActRecreateNode)],
		counts[string(reconcile.ActOrphan)]+counts[string(reconcile.ActPruneNode)])
	return sb.String()
}

// smallByPath 判断路径是否属于某个小组成品。
func smallByPath(pl *planner.Plan, path string) (*planner.Node, bool) {
	for _, n := range pl.SmallNodes {
		if n.Path == path {
			return n, true
		}
	}
	return nil, false
}

// actionTagOf 找到节点的动作类型（尺寸标注区分 CREATE/RECREATE 口径）。
func actionTagOf(acts []*reconcile.Action, name string) reconcile.ActionType {
	for _, a := range acts {
		if a.Name == name {
			return a.Type
		}
	}
	return ""
}

// humanSize 字节的人读形式（KiB/MiB/GiB）。
func humanSize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2fGiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func displayPath(baseDir string, n *planner.Node) string {
	if n.Path != "" {
		return n.Path
	}
	if n.Kind == planner.KindIntermediate {
		return baseDir + "/sub/" + n.Name
	}
	return baseDir + "/" + n.Name
}
