// cmd_daemon.go：meowsubd 守护进程相关子命令。
// install-daemon/daemon-run 是单元安装与守护进程本体入口；
// start/stop 经守护进程启停实例；ps 查看守护进程视角的实例登记。
package main

import (
	"flag"
	"fmt"

	"meowsub/internal/config"
	"meowsub/internal/daemon"
)

// runInstallDaemon 安装/刷新 meowsubd 宿主单元（sudo，一次性）。
func runInstallDaemon(args []string) error {
	fs := flag.NewFlagSet("install-daemon", flag.ExitOnError)
	f := fs.String("f", daemon.ConfigPath, "将安装为正式配置的 TOML 路径")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("参数错误: %w", err)
	}
	selfBin, err := selfPath()
	if err != nil {
		return fmt.Errorf("定位 meowsub 自身失败: %w", err)
	}
	return daemon.Install(*f, selfBin)
}

// runDaemonRun 守护进程本体入口（systemd 单元 ExecStart 调用）。
func runDaemonRun(args []string) error {
	fs := flag.NewFlagSet("daemon-run", flag.ExitOnError)
	f := fs.String("f", daemon.ConfigPath, "TOML 配置文件路径")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("参数错误: %w", err)
	}
	// 启动时校验一次确保快速失败；此后守护进程每次引导前当场重读该路径。
	return daemon.Run(*f)
}

// runStartStop 经守护进程启停指定组的使用态实例。
func runStartStop(args []string) error {
	cmd := args[0] // "start" 或 "stop"
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	f := fs.String("f", daemon.ConfigPath, "TOML 配置文件路径")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("参数错误: %w", err)
	}
	name := fsArg(args)
	if name == "" {
		return usage()
	}
	return daemon.StartStop(func() (*config.Config, error) { return config.Load(*f) },
		cmd == "start", name)
}
