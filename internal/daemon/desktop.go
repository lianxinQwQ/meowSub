// desktop.go Desktop Entry 规范的最小实现：解析 Exec/Terminal/Icon，
// 以及字段码展开。meowsub open 对应用 ID 的入口时由此换算成容器内 argv。
package daemon

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func relPath(root, path string) (string, error) {
	return filepath.Rel(root, path)
}

// DesktopEntry 只承载 open 动词关心的字段。
type DesktopEntry struct {
	Exec     string
	Terminal bool
	Icon     string
}

// ParseDesktop 解析 [Desktop Entry] 段；段缺失或无 Exec 视为错误。
func ParseDesktop(data []byte) (*DesktopEntry, error) {
	e := &DesktopEntry{}
	sec := false
	found := false
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			sec = line == "[Desktop Entry]"
			continue
		}
		if !sec {
			continue
		}
		i := strings.Index(line, "=")
		if i <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		switch key {
		case "Exec":
			e.Exec = val
			found = true
		case "Terminal":
			e.Terminal = strings.EqualFold(val, "true")
		case "Icon":
			e.Icon = val
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !found || e.Exec == "" {
		return nil, fmt.Errorf("desktop 条目缺少有效 Exec")
	}
	return e, nil
}

// ExpandExec 按 Desktop Entry 规范把 Exec 字符串展开为 argv：
//
//	%f/%u  消费一个文件/URL 参数
//	%F/%U  追加全部剩余参数
//	%%     字面百分号
//	%d/%D/%n/%N/%i/%c/%k  已废弃或对启动器有特殊含义，一律丢弃
//
// 其余 token 原样保留；规范规定未出现字段码时参数追加在末尾。
func ExpandExec(exec string, args []string) []string {
	var argv []string
	sawCode := false
	toks := tokenize(exec)
	for _, tok := range toks {
		out := ""
		for i := 0; i < len(tok); i++ {
			c := tok[i]
			if c != '%' || i+1 >= len(tok) {
				out += string(c)
				continue
			}
			i++
			switch code := tok[i]; code {
			case 'f', 'u':
				sawCode = true
				if a := pop(&args); a != "" {
					out += a
				}
			case 'F', 'U':
				sawCode = true
				for _, a := range args {
					argv = append(argv, a)
				}
				args = nil
			case '%':
				out += "%"
			default:
				// %d %D %n %N %i %c %k 等弃用/专用码：丢弃
			}
		}
		if out != "" {
			argv = append(argv, out)
		}
	}
	if !sawCode {
		argv = append(argv, args...)
	} else if len(args) > 0 { // 未被字段码消化的剩余参数仍递送
		argv = append(argv, args...)
	}
	return argv
}

// tokenize 按规范拆分 Exec：双引号串内空格不分割，反斜杠转义引号与反斜杠。
func tokenize(s string) []string {
	var toks []string
	var cur strings.Builder
	inQuote := false
	esc := false
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\':
			esc = true
		case r == '"':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return toks
}

func pop(sp *[]string) string {
	a := *sp
	if len(a) == 0 {
		return ""
	}
	v := a[0]
	*sp = a[1:]
	return v
}

// --- open 路径的整合：应用 ID → argv -----------------------------------

// ResolveOpen 把“二进制名 或 应用ID”统一转成容器内 argv。
// 二进制优先于桌面条目；层内文件系统是判定依据。
func ResolveOpen(layerRoot, target string, args []string) ([]string, error) {
	candidates := []string{
		filepath.Join(layerRoot, "usr", "bin", target),
		filepath.Join(layerRoot, "usr", "local", "bin", target),
		filepath.Join(layerRoot, "bin", target),
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			if rel, err := relPath(layerRoot, c); err == nil {
				return append([]string{"/" + rel}, args...), nil
			}
		}
	}
	df := filepath.Join(layerRoot, "usr", "share", "applications", target+".desktop")
	data, err := os.ReadFile(df)
	if err != nil {
		return nil, fmt.Errorf("open 目标 %q 既非层内可执行也无对应 .desktop", target)
	}
	entry, err := ParseDesktop(data)
	if err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", df, err)
	}
	return wrapShell(ExpandExec(entry.Exec, unwrapFileURIs(args))), nil
}

// --- run-desktop 路径的整合：desktop 目标 → argv -------------------------

// LocateDesktop 把「实例内 .desktop 路径或应用 ID」定位到层内文件，返回
// 容器内绝对路径：带分隔符按实例内路径处理（须存在）；否则视为
// usr/share/applications 下的 ID 并自动补全后缀。
func LocateDesktop(layerRoot, target string) (string, error) {
	rel := strings.TrimPrefix(filepath.ToSlash(target), "/")
	if !strings.Contains(rel, "/") { // 裸 ID：默认桌面条目目录
		if !strings.HasSuffix(rel, ".desktop") {
			rel += ".desktop"
		}
		rel = "usr/share/applications/" + rel
	}
	if _, err := os.Stat(filepath.Join(layerRoot,
		filepath.FromSlash(rel))); err != nil {
		return "", fmt.Errorf("实例内找不到 desktop 文件 %q", target)
	}
	return "/" + rel, nil
}

// ResolveDesktopFile 把「实例内 .desktop 路径或应用 ID」折算为容器内启动
// argv。Terminal 条目暂按普通进程执行——machinectl 提交的会话不保证有
// 终端，后续可按层内可用模拟器包装。
func ResolveDesktopFile(layerRoot, target string, args []string) ([]string, error) {
	path, err := LocateDesktop(layerRoot, target)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(layerRoot,
		filepath.FromSlash(strings.TrimPrefix(path, "/"))))
	if err != nil {
		return nil, err
	}
	entry, err := ParseDesktop(data)
	if err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	if entry.Terminal {
		fmt.Printf("[run-desktop] Terminal 条目暂按普通进程执行: %s\n", path)
	}
	return wrapShell(ExpandExec(entry.Exec, unwrapFileURIs(args))), nil
}

// wrapShell 把 argv 拼成单串包进 /bin/sh -c：借 shell 的 PATH 查找消化
// Exec 里的裸命令名（Chromium 系 Exec 以裸 env 开头，machinectl 只认绝对
// 路径 argv[0]，直接提交必败）。应用经包装链前台驻留，machinectl 会话活
// 到应用退出，demand 实例的占用清零回收不被误触。不委托 gio/dex：两者
// 都是 spawn 后即退（实测 gio 0.4s、dex 0.175s），会话先于应用结束，
// 恰恰踩中这条生命周期。
func wrapShell(argv []string) []string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shQuote(a)
	}
	return []string{"/bin/sh", "-c", strings.Join(q, " ")}
}

// unwrapFileURIs 把桌面传入的本地 file:// URI 还原为路径，其余原样透传。
// 同构发行版布局下宿主路径即容器内路径；跨界文件需另行挂载，属已知限制。
func unwrapFileURIs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if u, err := url.Parse(a); err == nil && u.Scheme == "file" &&
			u.Host == "" && u.Path != "" && u.RawQuery == "" {
			out[i] = u.Path
			continue
		}
		out[i] = a
	}
	return out
}
