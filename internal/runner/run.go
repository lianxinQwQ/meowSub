// Package runner 管理使用态子系统（run）的生命周期。
//
// 三区布局（见 internal/layout）：
//
//	base/build    构建区（本包只读引用其池与成品层）
//	base/master   使用态原始副本区：deploy 时从构建区建立的基线
//	base/runs     使用态区：脱离更新体系的可变实例
//
//	deploy     实例不存在时按 构建成品 → 基线 → 实例 双 reflink 建立；
//	           实例已存在时先与基线全量比对，有改动须交互确认「丢弃
//	           使用态内容」才继续，防止误覆盖运行期间的修改
//	destroy    删除实例并同步清除其基线副本
package runner

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"meowsub/internal/layout"
)

const (
	poolMount      = "/mnt/pkgpool"
	hostCacheMount = "/mnt/host-pkgcache"
)

// nspawnArgs 组装 systemd-nspawn 公共参数：rootfs 目录、池/宿主缓存只读
// 绑定、以及从进程环境与 /etc/environment 收集的代理变量（K=V 原样，
// 大小写保真）。容器事务要能访问镜像与本地池。
func (p Paths) nspawnArgs(root string, cmdArgs []string) []string {
	// 顺序关键：bind/setenv 是 systemd-nspawn 的参数，必须位于容器命令之前，
	// 否则会被当成容器内程序的选项（真实事故：pacman 收到 --bind-ro）。
	out := []string{"-D", root}
	pool := filepath.Join(layout.Build(p.BaseDir), "pool")
	if _, err := os.Stat(pool); err == nil {
		out = append(out, "--bind-ro="+pool+":"+poolMount)
	}
	if hc := hostCacheDir(); hc != "" {
		out = append(out, "--bind-ro="+hc+":"+hostCacheMount)
	}
	for _, kv := range proxyEnv() {
		out = append(out, "--setenv="+kv)
	}
	return append(out, cmdArgs...)
}

// hostCacheDir 读宿主机 /etc/pacman.conf 的第一个 CacheDir（不存在则默认值）。
func hostCacheDir() string {
	data, err := os.ReadFile("/etc/pacman.conf")
	if err != nil {
		return "/var/cache/pacman/pkg"
	}
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
		vals := strings.Fields(line[i+1:])
		if key == "CacheDir" && len(vals) > 0 {
			if fi, serr := os.Stat(vals[0]); serr == nil && fi.IsDir() {
				return vals[0]
			}
		}
	}
	return "/var/cache/pacman/pkg"
}

// proxyEnv 大小写保真地收集 http(s)_proxy/all_proxy/no_proxy 及大写变体；
// 进程环境优先于 /etc/environment，仅按精确键补缺。
func proxyEnv() []string {
	keys := []string{"http_proxy", "https_proxy", "all_proxy", "no_proxy"}
	variants := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		variants = append(variants, k, strings.ToUpper(k))
	}
	var out []string
	taken := map[string]bool{}
	for _, k := range variants {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			taken[k] = true
			out = append(out, k+"="+v)
		}
	}
	if data, err := os.ReadFile("/etc/environment"); err == nil {
		fileVal := map[string]string{}
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
			val := strings.Trim(strings.TrimSpace(line[i+1:]), "\"")
			isKey := false
			for _, k := range keys {
				if k == key || strings.ToUpper(k) == key {
					isKey = true
					break
				}
			}
			if !isKey || val == "" {
				continue
			}
			if _, dup := fileVal[key]; !dup {
				fileVal[key] = val
			}
		}
		for _, k := range variants {
			if taken[k] || fileVal[k] == "" {
				continue
			}
			out = append(out, k+"="+fileVal[k])
		}
	}
	return out
}

// Paths 使用态系统的存放布局（三区，见 internal/layout）。
type Paths struct {
	BaseDir string // 总根 base_dir；构建区、master、runs 均由其派生
	// In/Out 确认交互的输入输出源；零值用进程 stdin/stdout（测试注入）。
	In  io.Reader
	Out io.Writer
}

func (p Paths) out() io.Writer {
	if p.Out != nil {
		return p.Out
	}
	return os.Stdout
}

func (p Paths) in() io.Reader {
	if p.In != nil {
		return p.In
	}
	return os.Stdin
}

func (p Paths) runsDir() string { return layout.RunsDir(p.BaseDir) }

func (p Paths) masterDir() string { return layout.MasterDir(p.BaseDir) }

// Root 返回某个 run 的最终落盘目录。
func (p Paths) Root(name string) string { return filepath.Join(p.runsDir(), name) }

// Master 返回某个 run 的基线副本目录。
func (p Paths) Master(name string) string { return layout.Master(p.BaseDir, name) }

// List 列出全部 run 名（字典序）。
func (p Paths) List() ([]string, error) {
	entries, err := os.ReadDir(p.runsDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// sh 执行外部命令并把输出直连当前进程（容器交互需要 tty 可见性）。
func sh(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// CopyDirOverride 非空时替代默认复制（仅测试注入：无 reflink 的文件系统
// 上 Deploy 路径无法直接跑通，见 plainCopy 模式）。
var CopyDirOverride func(src, dst string) error

// copyDir 目录树复制：生产路径强制 reflink（同盘 btrfs 上任何降级都视为
// 环境错误）；独立成变量供测试在无 reflink 的文件系统上替身。
var copyDir = func(src, dst string) error {
	if CopyDirOverride != nil {
		return CopyDirOverride(src, dst)
	}
	return sh("cp", "-a", "--reflink=always", src, dst)
}

// swapCopy 把目录树从 src 整份 reflink 到 dst：先落到同盘暂存名再换名，
// cp 失败时旧 dst 原封不动。dst 存在则被整体替换。
func swapCopy(src, dst, incoming string) error {
	os.RemoveAll(incoming)
	defer os.RemoveAll(incoming)
	if err := copyDir(src, incoming); err != nil {
		return err
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return os.Rename(incoming, dst)
}

// Deploy 把构建区成品层 src 部署为使用态实例 name，建立基线→实例双副本：
//
//	构建区成品 → reflink → master 基线 → reflink → runs 实例
//
// 实例已存在时先与自身基线全量比对（sha256 逐文件）：一致说明实例未被
// 改动，静默重置；不一致（含基线缺失无法比对）则打印差异摘要并要求
// 交互确认「丢弃使用态内容」，yes 非交互参数可跳过确认。
// 强制 reflink：源与目标同盘 btrfs，任何降级都视为环境错误。
// 两级替换均走 .incoming 换名交换，中断不会留下半份目录。
func (p Paths) Deploy(src, name string, yes bool) error {
	if name == "" || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("非法 run 名 %q", name)
	}
	dst := p.Root(name)
	if _, err := os.Stat(filepath.Join(src, "usr")); err != nil {
		return fmt.Errorf("来源成品层 %s 缺少有效 rootfs: %w", src, err)
	}
	if _, err := os.Stat(dst); err == nil {
		if err := p.gateRedeploy(name, dst, yes); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := os.MkdirAll(p.masterDir(), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(p.runsDir(), 0o755); err != nil {
		return err
	}
	fmt.Printf("[run] 刷新基线副本 %s ← %s\n", p.Master(name), src)
	if err := swapCopy(src, p.Master(name), filepath.Join(p.masterDir(), ".incoming-"+name)); err != nil {
		return fmt.Errorf("deploy 失败（强制 reflink，需同盘 btrfs）: %w", err)
	}
	fmt.Printf("[run] 已部署 %s → %s（脱离更新体系；基线 %s）\n",
		name, dst, p.Master(name))
	if err := swapCopy(p.Master(name), dst, filepath.Join(p.runsDir(), ".incoming-"+name)); err != nil {
		return fmt.Errorf("deploy 失败（强制 reflink，需同盘 btrfs）: %w", err)
	}
	fmt.Printf("[run] 已部署 %s → %s（脱离更新体系；基线 %s）\n",
		name, dst, p.Master(name))
	return nil
}

// gateRedeploy 重部署前的安全闸：比对该实例与其基线副本，有差异或基线
// 缺失时要求用户显式确认丢弃实例内容。
func (p Paths) gateRedeploy(name, runRoot string, yes bool) error {
	master := p.Master(name)
	if _, err := os.Stat(master); os.IsNotExist(err) {
		if p.confirm(fmt.Sprintf(
			"run %q 已存在但缺少基线副本，无法校验将被丢弃的内容；确认重置？", name), yes) {
			return nil
		}
		return fmt.Errorf("已中止：%s 未被改动", runRoot)
	}
	changed, removed := diffTree(runRoot, master)
	if len(changed)+len(removed) == 0 {
		fmt.Printf("[run] 实例与基线完全一致，直接重置\n")
		return nil
	}
	fmt.Printf("[run] 使用态相对基线有改动：变更/新增 %d 个文件、缺失 %d 个（将随重置丢弃）\n",
		len(changed), len(removed))
	for _, rel := range changed[:min(len(changed), 10)] {
		fmt.Printf("[run]   M %s\n", rel)
	}
	for _, rel := range removed[:min(len(removed), 10)] {
		fmt.Printf("[run]   D %s\n", rel)
	}
	if p.confirm(fmt.Sprintf("确认丢弃上述改动并重置 run %q？", name), yes) {
		return nil
	}
	return fmt.Errorf("已中止：%s 未被改动", runRoot)
}

// confirm 打印问题并读取 y/n；yes 为 true 时直接放行（脚本非交互模式）。
func (p Paths) confirm(question string, yes bool) bool {
	if yes {
		return true
	}
	fmt.Fprintf(p.out(), "%s [y/N] ", question)
	line, err := bufio.NewReader(p.in()).ReadString('\n')
	if err != nil && line == "" { // 无输入可用（EOF）：一律视为拒绝
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// Destroy 删除一个 run 的落盘目录及其基线副本。
func (p Paths) Destroy(name string) error {
	dst := p.Root(name)
	if _, err := os.Stat(dst); err != nil {
		return fmt.Errorf("run %q 不存在", name)
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if _, err := os.Stat(p.Master(name)); err == nil {
		if err := os.RemoveAll(p.Master(name)); err != nil {
			return err
		}
		fmt.Printf("[run] 已同步清除基线副本 %s\n", p.Master(name))
	}
	return nil
}

// diffTree 比较 staged 与 lower，返回 staged 相对更新的差异：
// newFiles=新增或内容变化的相对路径（排序），removed=lower 有而 staged 无。
// 常规文件按内容 SHA256，符号链接按“link\x00目标”整体比对；目录、socket
// 等特殊文件不参与。
func diffTree(staged, lower string) (newFiles []string, removed []string) {
	digest := func(root string) map[string]string {
		out := map[string]string{}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return nil
			}
			switch {
			case d.Type().IsRegular():
				h, herr := fileSHA256(path)
				if herr != nil {
					return nil
				}
				out[rel] = h
			case d.Type()&os.ModeSymlink != 0:
				t, lerr := os.Readlink(path)
				if lerr != nil {
					return nil
				}
				out[rel] = "link\x00" + t
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[run] 遍历 %s: %v\n", root, err)
		}
		return out
	}
	stagedAll := digest(staged)
	lowerAll := digest(lower)
	for rel, h := range stagedAll {
		if lh, ok := lowerAll[rel]; !ok || lh != h {
			newFiles = append(newFiles, rel)
		}
	}
	for rel := range lowerAll {
		if _, ok := stagedAll[rel]; !ok {
			removed = append(removed, rel)
		}
	}
	sort.Strings(newFiles)
	sort.Strings(removed)
	return newFiles, removed
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
