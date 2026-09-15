// bootbind.go 引导期会话直通绑挂参数组装，按组配置的 session_mode 分流：
//
//	sockets（默认）：X11 目录只读直绑 + 运行时目录按 PlanDir 口径逐条目
//	                 只读直递（套接字逐个、含套接字的子目录整目录）；
//	rw：             X11 只读直绑 + 运行时目录整目录读写直挂（写入直达
//	                 宿主，自担渗漏，见 sample.toml 说明）；
//	off：            仅 X11。
//
// server.boot 与 install.go 的无 daemon 兜底引导共用本组装。
package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"meowsub/internal/config"
	"meowsub/internal/graphical"
)

// sessionModeFor 解析 run 对应组的会话直通方案；组不可考时按默认
// sockets（run 命名规则同 pickRun：组名或 <实例>@<组名>）。
func sessionModeFor(cfg *config.Config, run string) string {
	if cfg == nil {
		return config.DefaultSessionMode
	}
	for _, g := range cfg.Groups {
		if run == g.Name || strings.HasSuffix(run, "@"+g.Name) {
			return g.SessionMode
		}
	}
	return config.DefaultSessionMode
}

// tmpSizeFor 解析 run 对应组实例 /tmp 的显式大小（组级 run_tmp_size；
// run 命名规则同 pickRun：组名或 <实例>@<组名>。组不可考或未声明返回
// 0 = 沿用 nspawn 默认）。
func tmpSizeFor(cfg *config.Config, run string) int64 {
	if cfg == nil {
		return 0
	}
	for _, g := range cfg.Groups {
		if run == g.Name || strings.HasSuffix(run, "@"+g.Name) {
			return g.RunTmpSize
		}
	}
	return 0
}

// tmpfsBootArgs 引导期 /tmp 挂载参数：显式 size= 覆写 nspawn 自动分配的
// tmpfs（自动值无兜底，内存吃紧时可能小到容器内写 /tmp 直接 ENOSPC）。
func tmpfsBootArgs(cfg *config.Config, run string) []string {
	if sz := tmpSizeFor(cfg, run); sz > 0 {
		return []string{config.TmpSizeArg(sz)}
	}
	return nil
}

// mountsFor 解析 run 对应组的工作时挂载表：组级与全局按容器路径合并
// （组级同名覆盖、其余追加）；组不可考时仅全局表。
func mountsFor(cfg *config.Config, run string) []config.Mount {
	if cfg == nil {
		return nil
	}
	for _, g := range cfg.Groups {
		if run == g.Name || strings.HasSuffix(run, "@"+g.Name) {
			return g.EffectiveMounts(cfg.Mounts)
		}
	}
	return cfg.Mounts
}

// mountBootArgs 引导期工作时挂载参数：合并后的挂载表逐条渲染为
// --bind=宿主:容器，声明 ro 的条目只读直挂（--bind-ro）。挂载随引导
// 注入、实例整个生命周期可见；已运行实例不热挂载，改配置后重启实例生效。
func mountBootArgs(cfg *config.Config, run string) []string {
	var args []string
	for _, m := range mountsFor(cfg, run) {
		if m.ReadOnly {
			args = append(args, "--bind-ro="+m.Host+":"+m.Container)
		} else {
			args = append(args, "--bind="+m.Host+":"+m.Container)
		}
	}
	return args
}

// sessionBootArgs 组装引导期绑挂参数。运行时目录按方案分流；X11 目录
// 恒为只读直绑（X11 套接字无容器侧写入需求，且 /tmp/.X11-unix 不受
// 运行时目录生命周期影响）。
func sessionBootArgs(cfg *config.Config, run string, scan []graphical.Bind) []string {
	mode := sessionModeFor(cfg, run)
	var args []string
	for _, b := range scan {
		switch {
		case b.Kind == graphical.KindX11:
			args = append(args, "--bind-ro="+b.Path)
		case mode == config.SessionModeOff:
			// 不处理运行时目录
		case mode == config.SessionModeRW:
			args = append(args, "--bind="+b.Path)
		default: // sockets
			entries, _ := graphical.PlanDir(b.Path)
			for _, e := range entries {
				args = append(args, "--bind-ro="+e.Host+":"+e.Container)
			}
		}
	}
	return args
}

// cleanupLegacyOverlays 一次性清退叠加层方案（已由套接字直递取代）在
// /run/meowsub/rt 的宿主侧残留：惰性卸载其下全部挂载后移除目录树。
// 守护进程启动时调用一次，尽力而为。
func cleanupLegacyOverlays() {
	const root = "/run/meowsub/rt"
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return
	}
	var targets []string
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && (f[1] == root || strings.HasPrefix(f[1], root+"/")) {
			targets = append(targets, f[1])
		}
	}
	// 路径长的先摘（子挂载在前）
	sort.Sort(sort.Reverse(sort.StringSlice(targets)))
	for _, t := range targets {
		_ = exec.Command("umount", "-l", t).Run()
	}
	_ = os.RemoveAll(filepath.Clean(root))
}
