// cmd_plan.go：plan 子命令 —— 构建编排的干跑入口：解析依赖、聚类规划、
// 调和并打印将要做的事，不落盘、不需要 root。
package main

import (
	"fmt"

	"meowsub/internal/builder"
	"meowsub/internal/config"
)

// runPlan 与 build 共享 preparePlan 的规划流水线，渲染计划后即止。
func runPlan(args []string) error {
	f, err := parsePlanFlags(args)
	if err != nil {
		return usage()
	}
	if f.only != "" {
		return fmt.Errorf("--only 仅 build 支持")
	}
	cfg, err := config.Load(f.cfgPath)
	if err != nil {
		return err
	}
	plan, acts, _, _, err := preparePlan(cfg, nil, f.prune, f.minShared, f.aurWeight, f.forceReinstall)
	if err != nil {
		return err
	}
	fmt.Print(builder.RenderPlan(plan, acts))
	fmt.Println("（干跑结束：以上为将要执行的动作，未改动磁盘）")
	return nil
}
