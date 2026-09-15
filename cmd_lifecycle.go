// cmd_lifecycle.go：使用态实例生命周期子命令（destroy-run/list-runs）。
// 与构建规划完全解耦：只依赖 runner 与成品层路径，不触碰规划流水线。
package main

import (
	"flag"
	"fmt"
	"os"

	"meowsub/internal/config"
	"meowsub/internal/daemon"
	"meowsub/internal/runner"
)

// runDestroyRun 删除使用态实例。
func runDestroyRun(args []string) error {
	fs := flag.NewFlagSet("destroy-run", flag.ExitOnError)
	p := fs.String("f", daemon.ConfigPath, "TOML 配置文件路径")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("参数错误: %w", err)
	}
	runName := fs.Arg(0)
	if runName == "" {
		return usage()
	}
	cfg, err := config.Load(*p)
	if err != nil {
		return err
	}
	rp := runner.Paths{BaseDir: cfg.BaseDir}
	return rp.Destroy(runName)
}

// runListRuns 列出使用态实例及其落盘路径；基线副本缺失时随行标注。
func runListRuns(args []string) error {
	fs := flag.NewFlagSet("list-runs", flag.ContinueOnError)
	p := fs.String("f", daemon.ConfigPath, "TOML 配置文件路径")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("参数错误: %w", err)
	}
	cfg, err := config.Load(*p)
	if err != nil {
		return err
	}
	rp := runner.Paths{BaseDir: cfg.BaseDir}
	runs, lerr := rp.List()
	if lerr != nil {
		return lerr
	}
	for _, name := range runs {
		line := fmt.Sprintf("%s -> %s", name, rp.Root(name))
		if _, serr := os.Stat(rp.Master(name)); serr != nil {
			line += "（无基线副本）"
		}
		fmt.Println(line)
	}
	return nil
}
