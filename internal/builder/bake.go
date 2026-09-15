// bake.go 构建期烘焙：把身份与组级声明物化进成品层。
//
// 三件事，全部发生在包安装完成之后、标记落盘之前：
//
//	users    扫描宿主人类账户（login.defs 人类区间 + 非服务 shell +
//	         home 存在），按同名同 uid/gid 镜像进层的 etc 账户文件
//	services systemctl --root enable 单元（宿主侧离线操作）
//	export   从层内 pacman 库解析导出包元数据（PATH 可执行 / 应用 / 图标）入标记
//
// 身份镜像属硬约束：任一名字在宿主不存在即整体失败——半镜像的层比
// 构建失败更难排查。
package builder

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"meowsub/internal/config"
	"meowsub/internal/state"
)

// HostUser 是从宿主账户文件解析出的一个用户视图。
type HostUser struct {
	Name  string
	UID   int
	GID   int
	GECOS string
	Home  string
	Shell string
	// Group 主组名；宿主缺该组时按用户名就地合成。
	Group string
}

const (
	passwdFile    = "/etc/passwd"
	groupFile     = "/etc/group"
	loginDefsFile = "/etc/login.defs" // UID_MIN/UID_MAX 人类区间口径
)

// 服务账户判定：shell 为下列任一变体即非人类账户。
var serviceShells = map[string]bool{
	"/usr/sbin/nologin": true,
	"/sbin/nologin":     true,
	"/usr/bin/nologin":  true,
	"/bin/nologin":      true,
	"/bin/false":        true,
}

const (
	defaultUIDMin = 1000
	defaultUIDMax = 60000
)

type colonDB map[string][]string

func readColonDB(path string) (colonDB, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", path, err)
	}
	defer f.Close()
	db := colonDB{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 3 || parts[0] == "" {
			continue
		}
		db[parts[0]] = parts
	}
	return db, sc.Err()
}

// parseLoginDefs 提取 login.defs 的 UID_MIN/UID_MAX；缺项或非法值回退
// 到 1000..60000 常规口径。SYS_UID_* 等前缀相近的键不命中。
func parseLoginDefs(content string) (uidMin, uidMax int) {
	uidMin, uidMax = defaultUIDMin, defaultUIDMax
	for _, ln := range strings.Split(content, "\n") {
		fields := strings.Fields(ln)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "UID_MIN":
			if n, err := strconv.Atoi(fields[1]); err == nil && n > 0 {
				uidMin = n
			}
		case "UID_MAX":
			if n, err := strconv.Atoi(fields[1]); err == nil && n > 0 {
				uidMax = n
			}
		}
	}
	return
}

// filterHumanUsers 按人类区间、非服务 shell、home 目录存在三条过滤出
// 宿主人类账户名（排序返回）。
func filterHumanUsers(db colonDB, uidMin, uidMax int,
	homeExists func(string) bool) []string {
	var out []string
	for name, f := range db {
		if len(f) < 7 {
			continue
		}
		uid, err := strconv.Atoi(f[2])
		if err != nil || uid < uidMin || uid > uidMax {
			continue
		}
		if serviceShells[f[6]] {
			continue
		}
		if !homeExists(f[5]) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// DetectHostHumans 扫描宿主人类账户，供烘焙期身份镜像使用。
func DetectHostHumans() ([]string, error) {
	min, max := defaultUIDMin, defaultUIDMax
	data, err := os.ReadFile(loginDefsFile)
	if err == nil {
		min, max = parseLoginDefs(string(data))
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("读取 %s: %w", loginDefsFile, err)
	}
	db, lerr := readColonDB(passwdFile)
	if lerr != nil {
		return nil, lerr
	}
	return filterHumanUsers(db, min, max, func(home string) bool {
		fi, serr := os.Stat(home)
		return serr == nil && fi.IsDir()
	}), nil
}

// detectHumans 测试替身注入点。
var detectHumans = DetectHostHumans

// hostUsers 从宿主 /etc/passwd 与 /etc/group 解析指定名单；
// 任一用户缺失即报错（附全部缺失名）。主组查不到时以用户名合成。
func hostUsers(names []string) (map[string]*HostUser, error) {
	pw, err := readColonDB(passwdFile)
	if err != nil {
		return nil, err
	}
	gr, err := readColonDB(groupFile)
	if err != nil {
		return nil, err
	}
	gidByName := func(name string) int {
		if p, ok := gr[name]; ok && len(p) > 2 {
			n, _ := strconv.Atoi(p[2])
			return n
		}
		return -1
	}
	out := make(map[string]*HostUser, len(names))
	var missing []string
	for _, n := range names {
		f, ok := pw[n]
		if !ok {
			missing = append(missing, n)
			continue
		}
		u := &HostUser{Name: f[0]}
		num := func(i int) int { v, _ := strconv.Atoi(f[i]); return v }
		u.UID, u.GID = num(2), num(3)
		if len(f) > 4 {
			u.GECOS = f[4]
		}
		if len(f) > 5 {
			u.Home = f[5]
		}
		if len(f) > 6 {
			u.Shell = f[6]
		}
		u.Group = ""
		for gname, gp := range gr { // 少量行，线性反查即可
			if len(gp) > 2 && gp[2] == strconv.Itoa(u.GID) {
				u.Group = gname
				break
			}
		}
		if u.Group == "" {
			u.Group = n // 就地合成主组：宿主无对应 group 行
			_ = gidByName(n)
		}
		out[n] = u
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("宿主不存在以下用户：%v", missing)
	}
	return out, nil
}

// lookupHost 账户解析入口：生产直读 /etc；测试可整体替身。
var lookupHost = hostUsers

// bakeIdentity 把扫描名单确定性地镜像进层内 etc/{passwd,group,shadow}：
// 同名条目整行替换为镜像值，其余行与顺序保持不动，文件保证换行结尾。
// 返回实际镜像的名字（排序）。
func bakeIdentity(layer string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	host, err := lookupHost(names)
	if err != nil {
		return nil, err
	}
	sorted := append([]string{}, names...)
	sort.Strings(sorted)
	var done []string
	for _, n := range sorted {
		u, ok := host[n]
		if !ok {
			return done, fmt.Errorf("宿主不存在用户 %s", n)
		}
		uid, gid := strconv.Itoa(u.UID), strconv.Itoa(u.GID)
		// shadow 必须是完整 9 字段格式：lastchg=当天天数（0 表示密码立即
		// 过期会令 PAM 会话建立失败），min/max/warn 沿 useradd 默认值；
		// 密码位「!」= 已锁定（uid 会话不经口令认证，只需账户信息可用）。
		// 畸形行会让容器内 unix_chkpwd 报 could not obtain user info，
		// machinectl shell 的非 root 会话一律起不来。
		lastchg := strconv.Itoa(int(time.Now().Unix() / 86400))
		writes := [][3]string{ // 文件, 键, 整行
			{"passwd", n, strings.Join([]string{n, "x", uid, gid,
				u.GECOS, u.Home, u.Shell}, ":")},
			{"group", u.Group,
				strings.Join([]string{u.Group, "x", gid, ""}, ":")},
			{"shadow", n, strings.Join([]string{n, "!", lastchg,
				"0", "99999", "7", "", "", ""}, ":")},
		}
		for _, w := range writes {
			if err := upsertColon(filepath.Join(layer, "etc", w[0]), w[1], w[2]); err != nil {
				return done, fmt.Errorf("镜像 %s 到 %s/%s 失败: %w", n, layer, w[0], err)
			}
		}
		// 家目录：bash -l 与容器内 systemd --user 都假定其存在；空目录即可
		//（数据仍走全局挂载）。已存在则不动。属主修正仅 root（生产构建）
		// 可为，测试进程非 root 时跳过。
		if u.Home != "" && u.Home != "/" {
			hp := filepath.Join(layer, u.Home)
			if _, err := os.Stat(hp); os.IsNotExist(err) {
				if err := os.MkdirAll(hp, 0o700); err != nil {
					return done, fmt.Errorf("创建家目录 %s 失败: %w", hp, err)
				}
			}
			if os.Geteuid() == 0 {
				if err := os.Chown(hp, u.UID, u.GID); err != nil {
					return done, fmt.Errorf("家目录属主 %s 失败: %w", hp, err)
				}
			}
		}
		done = append(done, n)
	}
	return done, nil
}

// upsertColon 行级 upsert etc 账户库：键相同则整行替换（重复键清理至一处），
// 否则追加末尾；其余内容零改动。文件不存在时创建。
func upsertColon(path, key, line string) error {
	old, err := os.ReadFile(path)
	notExist := os.IsNotExist(err)
	if err != nil && !notExist {
		return err
	}
	lines := []string{}
	if !notExist {
		lines = strings.Split(strings.TrimRight(string(old), "\n"), "\n")
		if len(lines) == 1 && lines[0] == "" {
			lines = nil
		}
	}
	replacedAt := -1
	for i, ln := range lines {
		fields := strings.Split(ln, ":")
		if len(fields) > 0 && fields[0] == key {
			replacedAt = i
			break
		}
	}
	if replacedAt >= 0 {
		lines[replacedAt] = line
		for i := replacedAt + 1; i < len(lines); { // 清理残余重复键
			if f := strings.Split(lines[i], ":"); len(f) > 0 && f[0] == key {
				lines = append(lines[:i], lines[i+1:]...)
				continue
			}
			i++
		}
	} else {
		lines = append(lines, line)
	}
	if lines == nil {
		lines = []string{line}
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// bakeServices 在层上离线启用单元。硬失败：任意 enable 出错即刻中止构建
// ——漏启一个服务远比一次重建昂贵。
func (b *Builder) bakeServices(layer string, services []string) error {
	for _, s := range services {
		if err := b.Runner.Run("systemctl", "--root="+layer, "enable", s); err != nil {
			return fmt.Errorf("enable %s 失败（层 %s）：%w（单元名是否正确？）", s, layer, err)
		}
	}
	return nil
}

// readLayerPkgFiles 遍历 <layer>/var/lib/pacman/local/* 组建「包名 → 文件
// 清单」索引：desc 的 %NAME% 段作键，files 的 %FILES% 段作值（目录条目带
// 尾斜杠）。缺任一文件的子目录视为坏条目静默跳过；本地库缺失时报错。
func readLayerPkgFiles(layer string) (map[string][]string, error) {
	local := filepath.Join(layer, "var", "lib", "pacman", "local")
	subdirs, err := os.ReadDir(local)
	if err != nil {
		return nil, fmt.Errorf("读取 pacman 本地库 %s: %w", local, err)
	}
	db := make(map[string][]string, len(subdirs))
	for _, d := range subdirs {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(local, d.Name())
		names, nerr := dbSection(filepath.Join(dir, "desc"), "NAME")
		files, ferr := dbSection(filepath.Join(dir, "files"), "FILES")
		if nerr != nil || ferr != nil || len(names) == 0 {
			continue
		}
		db[names[0]] = files
	}
	return db, nil
}

// dbSection 提取 desc/files 这类 %KEY% 分节文本中指定节的非空行。
func dbSection(path, key string) ([]string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	in := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "%") && strings.HasSuffix(line, "%") {
			in = line[1:len(line)-1] == key
			continue
		}
		if in {
			out = append(out, line)
		}
	}
	return out, nil
}

// exportBinDirs 导出认定的 PATH 目录（usrmerge 后 sbin 即 bin，无需单列）。
var exportBinDirs = map[string]bool{
	"usr/bin":       true,
	"usr/local/bin": true,
	"bin":           true,
}

// bakePackageExports 为每个导出包名从层的 pacman 本地库解析导出元数据；
// 查无此包或包内既无 PATH 可执行也无 .desktop 均计入 missed，聚合一次报
// 错中止构建——拼写错误的包名必须当场暴露而非静默丢失入口。
func bakePackageExports(layer string, pkgs []string) ([]state.ExportEntry, error) {
	var entries []state.ExportEntry
	var missed []string
	if len(pkgs) > 0 {
		db, err := readLayerPkgFiles(layer)
		if err != nil {
			return nil, err
		}
		for _, p := range pkgs {
			files, ok := db[p]
			if !ok {
				missed = append(missed, p)
				continue
			}
			e, hit := classifyPkgFiles(layer, p, files)
			if !hit {
				missed = append(missed, p)
				continue
			}
			entries = append(entries, e)
		}
	}
	if len(missed) > 0 {
		sort.Strings(missed)
		return entries, fmt.Errorf(
			"export 解析失败，层内找不到可导出的软件包：%v", missed)
	}
	return entries, nil
}

// classifyPkgFiles 把包文件清单拆为 PATH 可执行与桌面条目；两者皆空返回
// hit=false。图标沿用 desktop Icon= 字段在 hicolor 择最大尺寸的定位规则。
func classifyPkgFiles(layer, pkg string, files []string) (state.ExportEntry, bool) {
	e := state.ExportEntry{Package: pkg}
	for _, f := range files {
		f = filepath.ToSlash(strings.TrimSpace(f))
		if f == "" || strings.HasSuffix(f, "/") { // 纯目录条目
			continue
		}
		dir, base := filepath.Split(f)
		dir = strings.TrimSuffix(dir, "/")
		switch {
		case exportBinDirs[dir]:
			e.Bins = append(e.Bins, "/"+f)
		case dir == "usr/share/applications" && strings.HasSuffix(base, ".desktop"):
			data, rerr := os.ReadFile(filepath.Join(layer,
				filepath.FromSlash(f)))
			if rerr != nil {
				continue
			}
			app := state.ExportApp{
				DesktopID: strings.TrimSuffix(base, ".desktop"),
				Name:      desktopNameField(data),
			}
			_, app.IconRel = findLayerIcon(layer, desktopIconField(data))
			e.Apps = append(e.Apps, app)
		}
	}
	sort.Strings(e.Bins)
	sort.Slice(e.Apps, func(i, j int) bool {
		return e.Apps[i].DesktopID < e.Apps[j].DesktopID
	})
	return e, len(e.Bins) > 0 || len(e.Apps) > 0
}

// desktopEntryField 从 .desktop 文本提取 [Desktop Entry] 段的指定字段。
func desktopEntryField(data []byte, key string) string {
	sec := false
	prefix := key + "="
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			sec = line == "[Desktop Entry]"
			continue
		}
		if sec && strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

// desktopNameField 从 .desktop 文本提取 [Desktop Entry] 段的 Name= 值。
func desktopNameField(data []byte) string {
	return desktopEntryField(data, "Name")
}

// desktopIconField 从 .desktop 文本提取 [Desktop Entry] 段的 Icon= 值。
func desktopIconField(data []byte) string {
	return desktopEntryField(data, "Icon")
}

// findLayerIcon 在层内定位图标相对路径：
// 绝对/含路径值直接校验存在；名称值在 hicolor 各数字尺寸下择最大者，
// 找不到时退回 usr/share/pixmaps 平铺目录。
func findLayerIcon(layer, icon string) (string, string) {
	if icon == "" {
		return "", ""
	}
	if strings.Contains(icon, "/") {
		p := filepath.Join(layer, strings.TrimPrefix(icon, "/"))
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			rel, _ := filepath.Rel(layer, p)
			return icon, rel
		}
	}
	exts := []string{".png", ".svg", ".xpm"}
	if sub, err := os.ReadDir(filepath.Join(layer, "usr", "share",
		"icons", "hicolor")); err == nil {
		bestSize, bestRel := -1, ""
		for _, sizeDir := range sub {
			if !sizeDir.IsDir() {
				continue
			}
			// 目录名形如 256x256 / 48x48 / scalable
			name := sizeDir.Name()
			numStr := name
			if i := strings.IndexByte(name, 'x'); i > 0 {
				numStr = name[:i]
			}
			size, err := strconv.Atoi(numStr)
			if err != nil || size <= bestSize {
				continue
			}
			for _, ext := range exts {
				cand := filepath.Join("usr", "share", "icons", "hicolor",
					sizeDir.Name(), "apps", icon+ext)
				if fi, err := os.Stat(filepath.Join(layer, cand)); err == nil && !fi.IsDir() {
					bestSize, bestRel = size, cand
					break
				}
			}
		}
		if bestRel != "" {
			return icon, bestRel
		}
	}
	for _, ext := range exts {
		rel := filepath.Join("usr", "share", "pixmaps", icon+ext)
		if _, err := os.Stat(filepath.Join(layer, rel)); err == nil {
			return icon, rel
		}
	}
	return "", ""
}

// groupOpts / groupServices：按最终组名取声明；中间层与未声明组得到 nil/空。
func (b *Builder) groupOpts(groupName string) *config.Group {
	if b.Groups == nil {
		return nil
	}
	return b.Groups[groupName]
}

func (b *Builder) groupServices(groupName string) []string {
	g := b.groupOpts(groupName)
	if g == nil {
		return nil
	}
	return g.Services
}

func (b *Builder) groupCopies(groupName string) []config.Copy {
	g := b.groupOpts(groupName)
	if g == nil {
		return nil
	}
	return g.Copies
}

// imageBuilderAccount 基础镜像内置的编译账户：包安装完成后在成品层再无
// 用处，烘焙期删除以免其 uid 与宿主人类区间重叠造成 getpwuid 歧义。
const imageBuilderAccount = "builder"

// removeAccount 从层的 etc/{passwd,group,shadow} 中删除指定账户的全部
// 条目（按行首字段匹配），其余行与顺序保持不动；文件不存在则跳过。
func removeAccount(layer, name string) error {
	for _, f := range []string{"passwd", "group", "shadow"} {
		p := filepath.Join(layer, "etc", f)
		data, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		var kept []string
		for _, ln := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if ln == "" {
				continue
			}
			if strings.HasPrefix(ln, name+":") {
				continue
			}
			kept = append(kept, ln)
		}
		out := ""
		if len(kept) > 0 {
			out = strings.Join(kept, "\n") + "\n"
		}
		if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// bakeNode 是两类成品节点共用的烘焙编排入口。grp 可为 nil（未配置新键的
// 旧声明），此时仅做账户清理与身份镜像。
func (b *Builder) bakeNode(layer string, grp *config.Group) (
	users []string, export []state.ExportEntry, err error) {
	if err = removeAccount(layer, imageBuilderAccount); err != nil {
		return nil, export, err
	}
	names, derr := detectHumans()
	if derr != nil {
		return nil, export, derr
	}
	if users, err = bakeIdentity(layer, names); err != nil {
		return users, export, err
	}
	if grp == nil {
		return users, nil, nil
	}
	if err = b.bakeServices(layer, grp.Services); err != nil {
		return users, export, err
	}
	if export, err = bakePackageExports(layer, grp.Export); err != nil {
		fmt.Fprintf(b.Out, "[bake] 组 [%s]:%v\n", grp.Name, err)
		return users, export, err
	}
	return users, export, nil
}
