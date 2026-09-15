// cmd_shell.go：进入实例或启动实例内应用的交互入口（shell/open/run-desktop）。
// shell 全轨走守护进程：交互桥接容器内 pty（以调用者身份，免 sudo、无登录），
// -c 同步执行回显输出；open/run-desktop 维持非 root 经守护进程、root 直连兜底。
// 非 root 一律不读配置：正式配置在 /etc 下仅 root 可读，转发请求由守护进程
// 自行加载并做会话门禁；配置仅在 root 直连兜底时加载。
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"meowsub/internal/config"
	"meowsub/internal/daemon"
	"meowsub/internal/tty"
)

// shellCmd 处理 shell <组名> [-c '命令']：全部经守护进程。交互请求桥接
// 容器内 pty，身份为调用者（SO_PEERCRED 实测 uid，root 即 root）；-c 在
// 实例内同步执行并把输出随响应回传。
func shellCmd(rest []string) error {
	var cmdline, group string
	var extra []string
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "-c":
			if i+1 < len(rest) {
				i++
				cmdline = rest[i]
			}
		default:
			extra = append(extra, rest[i])
		}
	}
	if len(extra) > 0 {
		group, extra = extra[0], extra[1:]
	} else {
		return usage()
	}
	if cmdline == "" {
		return shellInteractive(group)
	}
	// 同步执行无时限保证，放宽默认 15s 超时。
	resp, err := daemon.CallTimeout(daemon.Request{Op: daemon.OpShell,
		Group: group, Cmd: "/bin/bash", Args: []string{"-lc", cmdline},
		Env: daemon.ForwardEnv()},
		10*time.Minute)
	if err != nil {
		return daemon.MaybeRootHint(err, os.Geteuid() == 0,
			"先 sudo meowsub install-daemon 安装并启动服务")
	}
	fmt.Println(resp.Data)
	return nil
}

// shellInteractive 请求守护进程桥接交互 shell 并搬运终端字节流：本地终端
// 切 raw，按键原样穿透到容器内 pty（同 ssh 原理），会话结束恢复。
func shellInteractive(group string) error {
	req := daemon.Request{Op: daemon.OpShell, Group: group,
		Interactive: true, Term: os.Getenv("TERM"), Env: daemon.ForwardEnv()}
	if tty.IsTerminal(os.Stdin.Fd()) {
		req.Rows, req.Cols, _ = tty.Winsize(os.Stdin.Fd())
	}
	conn, resp, err := daemon.OpenInteractive(req)
	if err != nil {
		return daemon.MaybeRootHint(err, os.Geteuid() == 0,
			"先 sudo meowsub install-daemon 安装并启动服务")
	}
	defer conn.Close()
	if s, _ := resp.Data.(string); s != "" {
		fmt.Println(s)
	}

	restore := func() {}
	if st, terr := tty.MakeRaw(os.Stdin.Fd()); terr == nil {
		restore = func() { _ = tty.Restore(os.Stdin.Fd(), st) }
	} // 非 tty（如管道）无法 raw：纯字节搬运依旧可用
	defer restore()

	go func() { // 本地输入 → 守护进程；输入耗尽即断开以终结会话
		_, _ = io.Copy(conn, os.Stdin)
		conn.Close()
	}()
	_, _ = io.Copy(os.Stdout, conn) // 守护进程 → 终端，至远端会话结束
	return nil
}

// detachFlag 前台阻塞开关的公共定义：缺省阻塞（CLI 退出/被杀即连锁终结
// 实例内应用），--detach 提交即忘，供宿主桌面启动器与脚本使用。
func detachFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("detach", false,
		"提交即忘：不等待应用退出。缺省前台阻塞，Ctrl-C 连锁终结实例内应用")
}

// callLaunch 发起 launch/run-desktop 请求：Wait 请求不设整轮超时（应用
// 可能运行任意久，断开由连接层感知并连锁终结）。
func callLaunch(req daemon.Request) (daemon.Response, error) {
	timeout := time.Duration(0)
	if !req.Wait {
		timeout = 15 * time.Second
	}
	return daemon.CallTimeout(req, timeout)
}

// openCmd 在实例内启动导出的软件入口：非 root 走守护进程，root 直连。
// args 已由 run() 切去子命令名，这里直接解析（args[1:] 会吞掉组名）。
func openCmd(args []string) error {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	f := fs.String("f", daemon.ConfigPath, "TOML 配置文件路径")
	detach := detachFlag(fs)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("参数错误: %w", err)
	}
	pos := fs.Args()
	if len(pos) == 0 {
		return usage()
	}
	group, target := pos[0], pos[1]
	rest := pos[2:]
	if os.Geteuid() != 0 {
		resp, err := callLaunch(daemon.Request{Op: daemon.OpLaunchEntry,
			Group: group, Cmd: target, Args: rest,
			Env: daemon.ForwardEnv(), Wait: !*detach})
		if err != nil {
			return daemon.MaybeRootHint(err, false, "")
		}
		fmt.Println(resp.Data)
		return nil
	}
	cfg, err := config.Load(*f)
	if err != nil {
		return err
	}
	if *detach {
		go func() { _ = daemon.LaunchAsRoot(cfg.BaseDir, group, target, rest) }()
		fmt.Println("已提交到 " + group)
		return nil
	}
	return daemon.LaunchAsRoot(cfg.BaseDir, group, target, rest)
}

// runDesktopCmd 解析实例内 .desktop 的 Exec 行并在实例内启动应用。
// args 已由 run() 切去子命令名，这里直接解析（args[1:] 会吞掉组名）。
func runDesktopCmd(args []string) error {
	fs := flag.NewFlagSet("run-desktop", flag.ExitOnError)
	f := fs.String("f", daemon.ConfigPath, "TOML 配置文件路径")
	detach := detachFlag(fs)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("参数错误: %w", err)
	}
	pos := fs.Args()
	if len(pos) < 2 {
		return usage()
	}
	group, target := pos[0], pos[1]
	rest := pos[2:]
	if os.Geteuid() != 0 {
		resp, err := callLaunch(daemon.Request{Op: daemon.OpRunDesktop,
			Group: group, Cmd: target, Args: rest,
			Env: daemon.ForwardEnv(), Wait: !*detach})
		if err != nil {
			return daemon.MaybeRootHint(err, false, "")
		}
		fmt.Println(resp.Data)
		return nil
	}
	cfg, err := config.Load(*f)
	if err != nil {
		return err
	}
	if *detach {
		go func() {
			_ = daemon.LaunchDesktopAsRoot(cfg.BaseDir, group, target, rest)
		}()
		fmt.Println("已提交到 " + group)
		return nil
	}
	return daemon.LaunchDesktopAsRoot(cfg.BaseDir, group, target, rest)
}
