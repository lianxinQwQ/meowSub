// exports.go 导出体系：宿主侧入口三件套的生成与按组调和清扫。
//
// 生成物（全部带 meowSub 归属标注）：
//
//	<base>/bin/<名>                      包装脚本，exec 转 meowsub open
//	/usr/share/applications/meowsub-<组>-<ID>.desktop   Exec 组装 run-desktop
//	/usr/share/icons/.../apps/meowsub-<组>-<ID>.<ext>
//
// 调和纪律：上一轮登记的全部路径视作待删集合；本轮逐文件「内容一致跳过、
// 不一致原子替换」后从集合移除；剩余项（不再导出/改名的孤儿）删除。
package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"meowsub/internal/config"
	"meowsub/internal/state"
)

// 宿主侧系统路径（测试可整体重定向到临时目录）。
var (
	desktopRoot   = "/usr/share/applications"
	iconHicolRoot = "/usr/share/icons/hicolor"
	iconPixmapDir = "/usr/share/pixmaps"
)

// managedTag 写入所有生成文件首部注释，标记归属以便人工辨识与未来清扫。
func managedTag(group string) string {
	return "# Managed by meowSub (group=" + group + ") -- do not edit\n"
}

// desktopStub 是写进 /usr/share/applications 的最小条目。Exec 直接组装
// run-desktop 命令：给定组与实例内 .desktop 路径即可在容器内启动应用。
// selfBin 是生成时刻 meowsub 本体的自定位路径——Name 后缀带上组名，
// 多个子系统导出同名软件包时在启动器里可分辨。
func desktopStub(group, id, selfBin string, withIcon bool) string {
	target := "/usr/share/applications/" + id + ".desktop"
	var b strings.Builder
	b.WriteString(managedTag(group))
	b.WriteString("[Desktop Entry]\nType=Application\n")
	b.WriteString("Name=" + id + " (meowsub-" + group + ")\n")
	b.WriteString("Exec=" + quoteDesktopExecPath(selfBin) +
		" run-desktop --detach " + group + " " + target + " %U\n")
	// TryExec 按规范是单一路径值，不参与参数切分，原样写出。
	b.WriteString("TryExec=" + selfBin + "\n")
	b.WriteString("Terminal=false\n")
	if withIcon {
		b.WriteString("Icon=meowsub-" + group + "-" + id + "\n")
	}
	return b.String()
}

// shimScript 是 PATH 上的转接脚本本体。
func shimScript(group, name, selfBin string) string {
	return managedTag(group) +
		"exec " + quoteShellArg(selfBin) + " open " + group + " " + name + ` "$@"
`
}

// execWordSafe 判定路径是否只含 desktop Exec、POSIX shell 与 systemd
// ExecStart 三种语法都无需引号转义的字符。
func execWordSafe(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("/_.-:@+", r):
		default:
			return false
		}
	}
	return true
}

// quoteDesktopExecPath 按 Desktop Entry 规范引用 Exec 行中的参数：安全字符
// 原样；否则双引号包裹并转义反斜杠、双引号；`%` 双写——字段码在引号内
// 同样会被启动器展开。
func quoteDesktopExecPath(p string) string {
	if execWordSafe(p) {
		return p
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`)
	return `"` + r.Replace(p) + `"`
}

// quoteShellArg 按 POSIX shell 规则引用 shim 脚本中的词：安全字符原样；
// 否则单引号包裹，内嵌单引号以反斜杠转义后续接。
func quoteShellArg(s string) string {
	if execWordSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeIfChanged 内容一致则跳过；返回是否实际写入。
func writeIfChanged(path, content string, perm os.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		if fi, serr := os.Stat(path); serr == nil && fi.Mode().Perm() == perm {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		return false, err
	}
	return true, nil
}

// SyncExports 遍历配置组，依据实例层标记重算导出物并执行待删集合调和。
// selfBin 是生成时刻 meowsub 本体的自定位路径，写进全部 shim 与桌面条目。
// st 由调用方载入并在成功路径上 Save。仅 root（build 尾部）可调用。
func SyncExports(cfg *config.Config, st *state.State, selfBin string) error {
	if selfBin == "" {
		return fmt.Errorf("无法定位 meowsub 自身可执行文件路径")
	}
	root := cfg.BaseDir

	prev := map[string][]string{}
	for _, g := range st.Exports {
		sort.Strings(g.Paths)
		prev[g.Group] = g.Paths
	}

	desired := map[string][]string{}
	for _, g := range cfg.Groups {
		if len(g.Export) == 0 {
			continue
		}
		// 导出源：已部署实例优先（run 与基线一致由 deploy 门闩保证）；
		// 尚无实例时回退构建区成品层——shim 经 meowsub open 按需自动部署。
		layer := filepath.Join(root, "runs", g.Name)
		m, _ := readMarkerSafe(layer)
		if m == nil {
			layer = filepath.Join(root, "build", g.Name)
			m, _ = readMarkerSafe(layer)
		}
		if m == nil {
			fmt.Printf("[export] 组 [%s] 无可用标记，跳过（先 build）\n", g.Name)
			continue
		}
		entryByPkg := map[string]state.ExportEntry{}
		for _, e := range m.Export {
			entryByPkg[e.Package] = e
		}
		binDir := filepath.Join(root, "bin")
		var paths []string
		shims := map[string]bool{}
		desktops := map[string]bool{}
		for _, pkg := range g.Export {
			e, ok := entryByPkg[pkg]
			if !ok {
				fmt.Printf("[export] 组 [%s] 实例标记缺包 %s 的导出元数据（重新 build 刷新）\n",
					g.Name, pkg)
				continue
			}
			for _, bin := range e.Bins { // 多包同名可执行：shim 内容恒同，登记一次
				name := filepath.Base(bin)
				if shims[name] {
					continue
				}
				shim := filepath.Join(binDir, name)
				if _, err := writeIfChanged(shim,
					shimScript(g.Name, name, selfBin), 0o755); err != nil {
					return fmt.Errorf("写入 %s: %w", shim, err)
				}
				shims[name] = true
				paths = append(paths, shim)
			}
			for _, app := range e.Apps {
				withIcon, iconPath := false, ""
				if app.IconRel != "" {
					src := filepath.Join(layer, app.IconRel)
					ext := strings.ToLower(filepath.Ext(src))
					dstBase := fmt.Sprintf("meowsub-%s-%s%s",
						g.Name, app.DesktopID, ext)
					sizeDir := iconSizeDir(app.IconRel)
					var dst string
					if sizeDir != "" {
						dst = filepath.Join(iconHicolRoot, sizeDir,
							"apps", dstBase)
					} else {
						dst = filepath.Join(iconPixmapDir, dstBase)
					}
					data, rerr := os.ReadFile(src)
					if rerr == nil {
						if werr := writeFileAtomic(dst, data, 0o644); werr != nil {
							return werr
						}
						withIcon, iconPath = true, dst
					}
				}
				if desktops[app.DesktopID] {
					continue
				}
				df := filepath.Join(desktopRoot,
					fmt.Sprintf("meowsub-%s-%s.desktop",
						g.Name, app.DesktopID))
				content := desktopStub(g.Name, app.DesktopID, selfBin, withIcon)
				if _, err := writeIfChanged(df, content, 0o644); err != nil {
					return err
				}
				desktops[app.DesktopID] = true
				paths = append(paths, df)
				if iconPath != "" {
					paths = append(paths, iconPath)
				}
			}
		}
		sort.Strings(paths)
		desired[g.Name] = paths
	}

	// ---- 待删集合清扫 ----
	generated := map[string]bool{}
	groupsOrder := make([]string, 0, len(desired))
	for g, ps := range desired {
		groupsOrder = append(groupsOrder, g)
		for _, p := range ps {
			generated[p] = true
		}
	}
	sort.Strings(groupsOrder)

	killed := map[string]int{}
	oldGroups := make([]string, 0, len(prev))
	for g := range prev {
		oldGroups = append(oldGroups, g)
	}
	sort.Strings(oldGroups)
	for _, g := range oldGroups {
		for _, p := range prev[g] {
			if !generated[p] {
				if err := os.Remove(p); err == nil {
					killed[g]++
				} else if !os.IsNotExist(err) {
					return fmt.Errorf("清扫 %s 失败: %w", p, err)
				}
			}
		}
	}

	st.Exports = st.Exports[:0]
	for _, g := range groupsOrder {
		st.Exports = append(st.Exports, &state.GroupExports{
			Group: g, Paths: desired[g]})
	}
	for g, n := range killed {
		fmt.Printf("[export] 清扫组 [%s] 陈旧导出 %d 个\n", g, n)
	}
	total := 0
	for _, ps := range desired {
		total += len(ps)
	}
	fmt.Printf("[export] 托管导出 %d 个文件（%d 个软件组）\n", total, len(groupsOrder))
	return nil
}

// iconSizeDir 从层内图标相对路径提取 hicolor 尺寸段（如 256x256）；无则空串。
func iconSizeDir(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, seg := range parts {
		if i > 0 && parts[i-1] == "hicolor" && isSizeSeg(seg) {
			return seg
		}
	}
	return ""
}

func isSizeSeg(s string) bool {
	i := strings.IndexByte(s, 'x')
	if i <= 0 {
		return false
	}
	for _, c := range s {
		if c != 'x' && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// writeFileAtomic 先落临时再换名，避免半截图标被桌面读到。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".incoming"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
