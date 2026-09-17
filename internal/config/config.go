// Package config 负责解析并校验 meowSub 的 TOML 配置文件。
//
// 配置格式：
//
//	base_dir = "/srv/subs"
//
//	build_env = ["http_proxy", "https_proxy"]
//
//	[[repositories]]          # 可选：额外仓库（multilib、archlinuxcn 等）
//	name = "archlinuxcn"
//	servers = [               # 按声明顺序作为镜像优先级
//	  "https://mirrors.ustc.edu.cn/archlinuxcn/$arch",
//	  "https://mirrors.aliyun.com/archlinuxcn/$arch",
//	]
//
//	[tool]
//	packages = ["vim", "neovim"]
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	toml "github.com/BurntSushi/toml"
)

// Group 表示配置文件中的一个软件组（对应一个成品子系统）。
type Group struct {
	Name     string
	Packages []string
	// Services 构建期 systemctl --root enable 的单元名。
	Services []string
	// Export 需要向外导出的软件包名清单：构建期从层的 pacman 本地库反查
	// 该包的 PATH 可执行与 .desktop 桌面条目。
	Export []string
	// Mounts 组级挂载声明（"宿主:容器[:ro]"），覆盖/追加全局同名容器路径。
	Mounts []Mount
	// Copies 组级配置复制表（"来源:子系统内绝对目标"）：构建期把宿主
	// 文件/目录 reflink 进成品子系统对应位置，目标已存在则覆盖。
	Copies []Copy
	// Autostart 开机自启标记：唯一持久化档位。true 时守护进程随宿主
	// 引导该组实例并豁免零占用回收。
	Autostart bool
	// SessionMode 宿主会话直通方案（/run/user/<uid>）：
	// sockets 兼容模式：套接字/目录直递，容器内写入落私有目录；
	// native 保留宿主 session bus（输入法等桌面服务可用），但关闭
	// portal/GIO 远程文件选择路径，优先使用实例内原生 chooser；
	// isolated 图形套接字直递但不直递宿主 session bus，应用由实例内
	// dbus-run-session 建立私有用户总线；
	// rw 整个运行时目录读写直挂（写入直达宿主，自担渗漏）；
	// off 不处理。
	SessionMode string
	// SessionUser 登录目标用户："caller"（默认，复用调用者 uid）或十进制
	// uid 字符串（固定身份，所有请求均以该 uid 运行）。
	SessionUser string
	// SessionAccess 访问门禁：谁可对本组使用 shell/open/run-desktop：
	// "all"（默认）| "root" | 十进制 uid 字符串（特定用户；root 恒通行）。
	SessionAccess string
	// BuildTmpSize 构建本组子系统时容器 /tmp 的 tmpfs 显式大小（字节）；
	// 共享中间节点取成员组的最大值。0 = 沿用 nspawn 默认。
	BuildTmpSize int64
	// RunTmpSize 本组运行实例 /tmp 的 tmpfs 显式大小（字节）。
	// 0 = 沿用 nspawn 默认。
	RunTmpSize int64
}

// 会话直通方案取值。
const (
	SessionModeSockets  = "sockets"
	SessionModeNative   = "native"
	SessionModeIsolated = "isolated"
	SessionModeRW       = "rw"
	SessionModeOff      = "off"
)

// 会话直通默认值（缺省键时由 Load 补齐）。native 保留宿主输入法等
// 必需桌面服务，同时避免实例默认调用宿主 portal。
const (
	DefaultSessionMode   = SessionModeNative
	DefaultSessionUser   = "caller"
	DefaultSessionAccess = "all"
)

// SessionUID 解析登录目标 uid：caller 返回 fallback（调用者 uid），固定
// 值直接使用（Load 已校验格式）。
func (g *Group) SessionUID(fallback int) int {
	if g.SessionUser == DefaultSessionUser {
		return fallback
	}
	n, err := strconv.Atoi(g.SessionUser)
	if err != nil {
		return fallback
	}
	return n
}

// SessionAllowed 访问门禁：all 恒通行；root 仅 root；特定 uid 仅该 uid
// （root 恒通行，便于救援）。
func (g *Group) SessionAllowed(callerUID int) bool {
	switch g.SessionAccess {
	case DefaultSessionAccess:
		return true
	case "root":
		return callerUID == 0
	default:
		n, err := strconv.Atoi(g.SessionAccess)
		return err == nil && (callerUID == n || callerUID == 0)
	}
}

// Repository 表示一个额外 pacman 仓库（名称 + 按优先级排列的镜像）。
type Repository struct {
	Name    string
	Servers []string
}

// Mount 表示一条工作时挂载声明："宿主:容器[:ro]"。宿主侧相对路径在
// 解析期按配置文件所在目录折算为绝对路径；容器侧必须绝对。
type Mount struct {
	Host      string
	Container string
	// ReadOnly 声明第三段 "ro" 时只读直挂（--bind-ro）。
	ReadOnly bool
}

// Copy 表示一条配置复制声明："来源:子系统内绝对目标"。来源相对路径在
// 解析期按配置文件所在目录折算为绝对路径；目标必须绝对且非根。
type Copy struct {
	Source string
	Dest   string
}

// Config 是整个配置文件的解析结果。
type Config struct {
	BaseDir string
	// BuildEnv 构建容器允许从宿主机透传的环境变量名。值由构建进程环境
	// 提供；若进程环境中没有，则尝试从 /etc/environment 补齐。
	BuildEnv []string
	// MinTreeGroupSize 软件组参与子系统树聚类的空间阈值（字节）：
	// 完整闭包安装尺寸严格大于该值的组才进树，其余走内存构建路径。0 = 不分流。
	MinTreeGroupSize int64
	// BuilderTmpSize 编译车间等构建机器容器（build/builder 与 build/arch）
	// /tmp 的 tmpfs 显式大小（字节）：AUR 预构建会把数 GB 安装负载解进
	// /tmp，nspawn 自动分配的 tmpfs 无兜底，内存吃紧时写满即 ENOSPC。
	// 显式 size= 收紧；0 = 不干预，沿用 nspawn 默认。
	BuilderTmpSize int64
	// MemTmpfsSize 内存构建（小组）工作区 tmpfs 的容量下限（字节）：
	// 实际挂载取 max(自动估算, 此值)。tmpfs 按写入逐页占用，是上限而非
	// 预分配，调大没有代价；估小才会装包中途 ENOSPC。命令行
	// --mem-tmpfs-size 优先于此值。0 = 纯自动估算。
	MemTmpfsSize int64
	Groups       []*Group      // 按组名字典序
	Repositories []*Repository // 按配置文件声明顺序（决定镜像优先级）
	// Mounts 全局工作时挂载；组级 Mounts 覆盖/追加同名容器路径。
	Mounts []Mount
}

// Load 读取 path 指向的 TOML 配置并校验。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}

	var top map[string]interface{}
	if err := toml.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("解析 TOML %s: %w", path, err)
	}

	cfg := &Config{}
	// 相对来源的解析基准：配置文件所在目录（正式位置即 /etc/meowsub）。
	base := configDirOf(path)
	for key, val := range top {
		if key == "base_dir" {
			s, ok := val.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, fmt.Errorf("base_dir 必须是非空字符串")
			}
			cfg.BaseDir = strings.TrimSpace(s)
			continue
		}
		if key == "build_env" {
			list, ok := val.([]interface{})
			if !ok {
				return nil, fmt.Errorf("build_env 必须是字符串数组")
			}
			seen := map[string]bool{}
			for i, raw := range list {
				name, ok := raw.(string)
				if !ok || strings.TrimSpace(name) == "" {
					return nil, fmt.Errorf("build_env[%d] 必须是非空环境变量名", i)
				}
				name = strings.TrimSpace(name)
				if !validEnvName(name) {
					return nil, fmt.Errorf("build_env[%d] 含非法环境变量名 %q", i, name)
				}
				if seen[name] {
					return nil, fmt.Errorf("build_env 重复项 %q", name)
				}
				seen[name] = true
				cfg.BuildEnv = append(cfg.BuildEnv, name)
			}
			continue
		}
		if key == "mounts" {
			list, ok := val.([]interface{})
			if !ok {
				return nil, fmt.Errorf("mounts 必须是字符串数组")
			}
			for i, raw := range list {
				m, err := parseMount(raw, base, fmt.Sprintf("mounts[%d]", i))
				if err != nil {
					return nil, err
				}
				cfg.Mounts = append(cfg.Mounts, *m)
			}
			continue
		}
		if key == "repositories" {
			list, ok := val.([]map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("repositories 必须是 [[repositories]] 表数组")
			}
			for i, raw := range list {
				name, _ := raw["name"].(string)
				if strings.TrimSpace(name) == "" {
					return nil, fmt.Errorf("repositories[%d] 缺少 name", i)
				}
				r := &Repository{Name: strings.TrimSpace(name)}
				if rawSv, has := raw["servers"]; has {
					sv, ok := rawSv.([]interface{})
					if !ok || len(sv) == 0 {
						return nil, fmt.Errorf("仓库 %q 的 servers 必须是非空字符串数组", r.Name)
					}
					for _, s := range sv {
						u, ok := s.(string)
						if !ok || strings.TrimSpace(u) == "" {
							return nil, fmt.Errorf("仓库 %q 含空 Server", r.Name)
						}
						r.Servers = append(r.Servers, strings.TrimSpace(u))
					}
				}
				for k := range raw {
					if k != "name" && k != "servers" {
						return nil, fmt.Errorf("仓库 %q 含未知键 %q", r.Name, k)
					}
				}
				cfg.Repositories = append(cfg.Repositories, r)
			}
			continue
		}
		if key == "min_tree_group_size" {
			n, err := parseSizeValue(val)
			if err != nil {
				return nil, fmt.Errorf("min_tree_group_size: %w", err)
			}
			cfg.MinTreeGroupSize = n
			continue
		}
		if key == "builder_tmp_size" {
			n, err := parseSizeValue(val)
			if err != nil {
				return nil, fmt.Errorf("builder_tmp_size: %w", err)
			}
			cfg.BuilderTmpSize = n
			continue
		}
		if key == "mem_tmpfs_size" {
			n, err := parseSizeValue(val)
			if err != nil {
				return nil, fmt.Errorf("mem_tmpfs_size: %w", err)
			}
			cfg.MemTmpfsSize = n
			continue
		}
		table, ok := val.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("顶层键 %q 应写作软件组表 [%s]", key, key)
		}
		g := &Group{Name: key,
			SessionMode:   DefaultSessionMode,
			SessionUser:   DefaultSessionUser,
			SessionAccess: DefaultSessionAccess}
		strLists := map[string]*[]string{
			"services": &g.Services,
			"export":   &g.Export,
		}
		rawPkgs, has := table["packages"]
		if !has {
			return nil, fmt.Errorf("软件组 [%s] 缺少 packages 数组", key)
		}
		pkgList, ok := rawPkgs.([]interface{})
		if !ok {
			return nil, fmt.Errorf("软件组 [%s] 的 packages 必须是字符串数组", key)
		}
		seen := map[string]bool{}
		for _, p := range pkgList {
			name, ok := p.(string)
			if !ok || strings.TrimSpace(name) == "" {
				return nil, fmt.Errorf("软件组 [%s] 含空包名", key)
			}
			name = strings.TrimSpace(name)
			if seen[name] {
				return nil, fmt.Errorf("软件组 [%s] 重复包 %q", key, name)
			}
			seen[name] = true
			g.Packages = append(g.Packages, name)
		}
		if len(g.Packages) == 0 {
			return nil, fmt.Errorf("软件组 [%s] 的 packages 为空", key)
		}
		for _, k := range []string{"services", "export"} {
			raw, has := table[k]
			if !has {
				continue
			}
			list, ok := raw.([]interface{})
			if !ok {
				return nil, fmt.Errorf("软件组 [%s] 的 %s 必须是字符串数组", key, k)
			}
			seenK := map[string]bool{}
			for i, it := range list {
				s, ok := it.(string)
				if !ok || strings.TrimSpace(s) == "" {
					return nil, fmt.Errorf("软件组 [%s] 的 %s[%d] 必须是非空字符串", key, k, i)
				}
				s = strings.TrimSpace(s)
				if seenK[s] {
					return nil, fmt.Errorf("软件组 [%s] 的 %s 重复项 %q", key, k, s)
				}
				seenK[s] = true
				*strLists[k] = append(*strLists[k], s)
			}
		}
		if raw, has := table["mounts"]; has {
			list, ok := raw.([]interface{})
			if !ok {
				return nil, fmt.Errorf("软件组 [%s] 的 mounts 必须是字符串数组", key)
			}
			for i, m := range list {
				mt, err := parseMount(m, base, fmt.Sprintf("[%s].mounts[%d]", key, i))
				if err != nil {
					return nil, err
				}
				g.Mounts = append(g.Mounts, *mt)
			}
		}
		if raw, has := table["copies"]; has {
			list, ok := raw.([]interface{})
			if !ok {
				return nil, fmt.Errorf("软件组 [%s] 的 copies 必须是字符串数组", key)
			}
			seenC := map[Copy]bool{}
			for i, it := range list {
				c, err := parseCopyEntry(it, base,
					fmt.Sprintf("[%s].copies[%d]", key, i))
				if err != nil {
					return nil, err
				}
				if seenC[c] {
					return nil, fmt.Errorf("软件组 [%s] 的 copies 重复项 %q", key, it)
				}
				seenC[c] = true
				g.Copies = append(g.Copies, c)
			}
		}
		if raw, has := table["autostart"]; has {
			b, ok := raw.(bool)
			if !ok {
				return nil, fmt.Errorf("软件组 [%s] 的 autostart 必须是布尔值", key)
			}
			g.Autostart = b
		}
		if raw, has := table["session_mode"]; has {
			s, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("软件组 [%s] 的 session_mode 必须是字符串", key)
			}
			switch s {
			case SessionModeSockets, SessionModeNative, SessionModeIsolated, SessionModeRW, SessionModeOff:
				g.SessionMode = s
			default:
				return nil, fmt.Errorf("软件组 [%s] 的 session_mode %q 非法"+
					"（sockets | native | isolated | rw | off）", key, s)
			}
		}
		if raw, has := table["session_user"]; has {
			s, err := parseUserSpec(raw, fmt.Sprintf("软件组 [%s] 的 session_user", key))
			if err != nil {
				return nil, err
			}
			g.SessionUser = s
		}
		if raw, has := table["session_access"]; has {
			s, err := parseUserSpec(raw, fmt.Sprintf("软件组 [%s] 的 session_access", key))
			if err != nil {
				return nil, err
			}
			g.SessionAccess = s
		}
		if raw, has := table["build_tmp_size"]; has {
			n, err := parseSizeValue(raw)
			if err != nil {
				return nil, fmt.Errorf("软件组 [%s] 的 build_tmp_size: %w", key, err)
			}
			g.BuildTmpSize = n
		}
		if raw, has := table["run_tmp_size"]; has {
			n, err := parseSizeValue(raw)
			if err != nil {
				return nil, fmt.Errorf("软件组 [%s] 的 run_tmp_size: %w", key, err)
			}
			g.RunTmpSize = n
		}
		for k := range table {
			switch k {
			case "packages", "services", "export", "mounts", "copies", "autostart",
				"session_mode", "session_user", "session_access",
				"build_tmp_size", "run_tmp_size":
				continue
			default:
				return nil, fmt.Errorf("软件组 [%s] 含未知键 %q", key, k)
			}
		}
		cfg.Groups = append(cfg.Groups, g)
	}

	if cfg.BaseDir == "" {
		return nil, fmt.Errorf("配置缺少顶层 base_dir")
	}
	if len(cfg.Groups) == 0 {
		return nil, fmt.Errorf("配置中没有任何软件组（至少需要一个 [组名] 表）")
	}
	sort.Slice(cfg.Groups, func(i, j int) bool { return cfg.Groups[i].Name < cfg.Groups[j].Name })
	return cfg, nil
}

// validEnvName 判断 POSIX 环境变量名。配置只接受变量名，不接受 NAME=value，
// 变量值统一从宿主机环境读取，避免把凭据直接写进配置文件。
func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_' ||
			(i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

// parseUserSpec 解析用户类取值：字符串 "caller"/"all"/"root" 或十进制
// uid（TOML 整数与字符串均可），统一归一为字符串存储。
func parseUserSpec(v interface{}, where string) (string, error) {
	switch n := v.(type) {
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return "", fmt.Errorf("%s 不能为空字符串", where)
		}
		switch s {
		case "caller", "all", "root":
			return s, nil
		}
		if uid, err := strconv.Atoi(s); err == nil && uid >= 0 {
			return strconv.Itoa(uid), nil
		}
		return "", fmt.Errorf("%s 非法（caller | all | root | 十进制 uid）: %q",
			where, n)
	case int64:
		if n < 0 {
			return "", fmt.Errorf("%s 不能为负 uid (%d)", where, n)
		}
		return strconv.FormatInt(n, 10), nil
	default:
		return "", fmt.Errorf("%s 必须是字符串或整数 uid，得到 %T", where, v)
	}
}

// configDirOf 配置文件所在目录（绝对路径）：两张表相对来源的解析基准。
func configDirOf(path string) string {
	dir := filepath.Dir(path)
	if !filepath.IsAbs(dir) {
		if abs, err := filepath.Abs(dir); err == nil {
			return abs
		}
	}
	return dir
}

// resolveSource 表条目来源折算：绝对路径原样 Clean；相对路径按配置文件
// 所在目录解析为绝对路径（正式位置即 /etc/meowsub）。
func resolveSource(p, base string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "/") {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

// parseMount 解析一条 "宿主:容器[:ro]" 挂载声明。宿主侧相对路径按配置
// 目录折算；容器侧必须绝对（相对目标不做任何自动解析）。
func parseMount(v interface{}, base, where string) (*Mount, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("%s 必须是字符串", where)
	}
	parts := strings.SplitN(strings.TrimSpace(s), ":", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("%s 格式非法，应为 \"/宿主:/容器[:ro]\": %q", where, s)
	}
	m := &Mount{Host: resolveSource(parts[0], base), Container: parts[1]}
	if len(parts) == 3 {
		if parts[2] != "ro" {
			return nil, fmt.Errorf("%s 第三段仅支持 \"ro\"（只读直挂）: %q", where, s)
		}
		m.ReadOnly = true
	}
	if !strings.HasPrefix(m.Container, "/") {
		return nil, fmt.Errorf("%s 容器侧必须是绝对路径: %q", where, s)
	}
	return m, nil
}

// parseCopyEntry 解析一条 "来源:子系统内绝对目标" 复制声明。来源相对
// 路径按配置目录折算；目标必须绝对且非根（防止施加时清空子系统根）。
func parseCopyEntry(v interface{}, base, where string) (Copy, error) {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return Copy{}, fmt.Errorf("%s 必须是非空字符串", where)
	}
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return Copy{}, fmt.Errorf("%s 格式非法，应为 \"来源:/子系统内绝对目标\": %q",
			where, s)
	}
	dest := filepath.Clean(parts[1])
	if !strings.HasPrefix(dest, "/") {
		return Copy{}, fmt.Errorf("%s 目标必须是子系统内绝对路径: %q", where, s)
	}
	if dest == "/" {
		return Copy{}, fmt.Errorf("%s 目标不能是根目录: %q", where, s)
	}
	return Copy{Source: resolveSource(parts[0], base), Dest: dest}, nil
}

// EffectiveMounts 全局挂载 + 组级覆盖/追加：组级中容器路径相同的条目
// 顶替全局对应项，其余依次追加。返回顺序稳定。
func (g *Group) EffectiveMounts(global []Mount) []Mount {
	out := make([]Mount, len(global), len(global)+len(g.Mounts))
	copy(out, global)
	for _, gm := range g.Mounts {
		replaced := false
		for i := range out {
			if out[i].Container == gm.Container {
				out[i] = gm
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, gm)
		}
	}
	return out
}

// TmpSizeArg 渲染 systemd-nspawn 的 /tmp 显式挂载参数
// "--tmpfs=/tmp:size=..."（覆写 nspawn 对容器 /tmp 的默认 tmpfs）；
// size 非 0 时才有意义，0 返回空串由调用方跳过。
func TmpSizeArg(size int64) string {
	if size <= 0 {
		return ""
	}
	return "--tmpfs=/tmp:size=" + formatSize(size)
}

// ParseSize 解析命令行传入的空间尺寸：纯整数字节或带 K/M/G 后缀的字符串
// （1024 进制），规则与配置文件的空间键一致。用于 --mem-tmpfs-size 等
// 运行时参数。
func ParseSize(s string) (int64, error) {
	return parseSizeValue(s)
}

// formatSize 字节数转紧凑写法：整除 G/M/K 时带后缀，其余保留字节数。
func formatSize(size int64) string {
	for _, u := range []struct {
		n int64
		s string
	}{{1 << 30, "G"}, {1 << 20, "M"}, {1 << 10, "K"}} {
		if size >= u.n && size%u.n == 0 {
			return strconv.FormatInt(size/u.n, 10) + u.s
		}
	}
	return strconv.FormatInt(size, 10)
}

// parseSizeValue 解析空间阈值：纯整数为字节；字符串支持 K/M/G（大小写均可，
// 1024 进制）后缀。非法类型、负数或无法识别的后缀一律报错。
func parseSizeValue(v interface{}) (int64, error) {
	switch n := v.(type) {
	case int64:
		if n < 0 {
			return 0, fmt.Errorf("不能为负数 (%d)", n)
		}
		return n, nil
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, fmt.Errorf("不能为空字符串")
		}
		unit := int64(1)
		switch last := s[len(s)-1]; {
		case last == 'k' || last == 'K':
			unit, s = 1<<10, s[:len(s)-1]
		case last == 'm' || last == 'M':
			unit, s = 1<<20, s[:len(s)-1]
		case last == 'g' || last == 'G':
			unit, s = 1<<30, s[:len(s)-1]
		}
		num, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("无法识别的尺寸 %q（示例：524288、\"800M\"、\"2G\"）", n)
		}
		if num < 0 {
			return 0, fmt.Errorf("不能为负数 (%s)", n)
		}
		return num * unit, nil
	default:
		return 0, fmt.Errorf("必须是整数（字节）或带 K/M/G 后缀的字符串，得到 %T", v)
	}
}
