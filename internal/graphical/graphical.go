// Package graphical 宿主图形会话资源到实例的通用直通，供所有运行期入口
// 统一使用。设计原则是搬运而非构造：绑挂层（Scan/NspawnArgs）枚举宿主
// 现存的会话目录整体搬入容器，不判断哪个是“正确”套接字；环境层
// （EnvArgs）把调用者透传的会话变量原样注入，有则透传、无则不注——
// 能否真正弹窗由实例内应用自证，这里不做硬性检查。
package graphical

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// 测试可整体重定向的宿主系统路径。
var (
	x11Dir  = "/tmp/.X11-unix"
	runUser = "/run/user"
)

// 会话资源种类。
const (
	// KindX11 共享 X11 套接字目录。
	KindX11 = "x11"
	// KindRuntime 用户运行时目录（Wayland 套接字、session bus 所在）。
	KindRuntime = "runtime"
)

// Bind 一项宿主会话资源：整体绑挂的目录路径与当前实例身份。目录被删除
// 重建（logout/login 周期会换 tmpfs 实例）则 Dev/Ino 变化，运行期据此
// 判定容器内旧 bind 已钉在死实例上，需清场补挂。
type Bind struct {
	Kind     string // KindX11 / KindRuntime
	Path     string
	Dev, Ino uint64
}

// X11Dir 共享 X11 套接字目录路径。
func X11Dir() string { return x11Dir }

// RuntimeDir uid 对应的运行时目录路径（Wayland 套接字、session bus 所在）。
func RuntimeDir(uid int) string { return runUser + "/" + strconv.Itoa(uid) }

// Scan 枚举当前宿主存在的会话资源：X11 目录整体一项，每个数字命名的
// /run/user/<uid> 目录一项，附身份。宿主缺资源时静默跳过。
func Scan() []Bind {
	var out []Bind
	collect := func(kind, path string) {
		fi, err := os.Stat(path)
		if err != nil || !fi.IsDir() {
			return
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return
		}
		out = append(out, Bind{Kind: kind, Path: path,
			Dev: uint64(st.Dev), Ino: st.Ino})
	}
	collect(KindX11, x11Dir)
	if es, err := os.ReadDir(runUser); err == nil {
		for _, e := range es {
			if e.IsDir() && isDigits(e.Name()) {
				collect(KindRuntime, runUser+"/"+e.Name())
			}
		}
	}
	return out
}

// NspawnArgs 把 Scan 结果转成 systemd-nspawn 的只读绑挂参数（引导期）。
func NspawnArgs(bs []Bind) []string {
	var args []string
	for _, b := range bs {
		args = append(args, "--bind-ro="+b.Path)
	}
	return args
}

// Entry 一项待直递的宿主会话资源。
type Entry struct {
	Host      string // 宿主绝对路径
	Container string // 容器内绝对路径（当前恒与宿主同路径）
	Kind      string // KindSocket / KindDir
}

// 套接字直递的条目种类。
const (
	KindSocket = "socket"
	KindDir    = "dir"
)

// Link 容器内待重建的符号链接（bind 不适用于链接，按原样重建；目标多为
// 同目录相对名，如 pulse → pipewire-0，重建后解析到已直递的套接字）。
type Link struct {
	Path   string // 容器内链接位置
	Target string // 链接目标原样
}

// excludeTop 容器自身体系占用或隐私路径：宿主侧不直递，容器内自建自管。
var excludeTop = map[string]bool{
	"systemd": true, // 容器 user manager 私有套接字
	"varlink": true, // 容器 io.systemd varlink 端点
	"dconf":   true, // 容器私有设置
}

// PlanDir 枚举宿主运行时目录 hostDir 中待直递的条目：顶层套接字逐个直
// 绑、子目录整目录直绑（活窗口，目录内新建套接字自动可见）、符号链接由
// 容器内重建；lock 文件、普通文件与排除项跳过。按容器路径排序，宿主目
// 录不存在时返回空。
func PlanDir(hostDir string) (entries []Entry, links []Link) {
	des, err := os.ReadDir(hostDir)
	if err != nil {
		return nil, nil
	}
	for _, de := range des {
		name := de.Name()
		if excludeTop[name] || strings.HasPrefix(name, ".") {
			continue
		}
		host := filepath.Join(hostDir, name)
		fi, err := os.Lstat(host)
		if err != nil {
			continue
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			if target, err := os.Readlink(host); err == nil {
				links = append(links, Link{Path: host, Target: target})
			}
		case fi.IsDir():
			entries = append(entries, Entry{Host: host, Container: host, Kind: KindDir})
		case fi.Mode()&os.ModeSocket != 0:
			entries = append(entries, Entry{Host: host, Container: host, Kind: KindSocket})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Container < entries[j].Container
	})
	return entries, links
}

// Binds 引导期便捷封装：扫描宿主并生成绑挂参数。
func Binds() []string { return NspawnArgs(Scan()) }

// EnvArgs 把调用者透传的会话环境组装为 machinectl shell 的 --setenv 注入
// 参数。只搬运不制造：环境里有什么注什么，缺失即缺席——容器内应用看到
// 与宿主一致的“无会话”事实并自然回退或报错，不猜套接字名、不填默认值
// （sudo/ssh 等无会话调用者与宿主上同命令行为一致）。唯一的推导是 dbus
// 地址：仅当 XDG_RUNTIME_DIR 已透传时按客户端标准回退拼
// unix:path=<runtime>/bus，只为照顾不实现该回退的客户端。
func EnvArgs(env map[string]string) []string {
	var args []string
	for _, k := range []string{"XDG_RUNTIME_DIR", "WAYLAND_DISPLAY", "DISPLAY"} {
		if v := env[k]; v != "" {
			args = append(args, "--setenv="+k+"="+v)
		}
	}
	if db := env["DBUS_SESSION_BUS_ADDRESS"]; db != "" {
		args = append(args, "--setenv=DBUS_SESSION_BUS_ADDRESS="+db)
	} else if rt := env["XDG_RUNTIME_DIR"]; rt != "" {
		args = append(args, "--setenv=DBUS_SESSION_BUS_ADDRESS=unix:path="+rt+"/bus")
	}
	return args
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
