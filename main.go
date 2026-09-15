// 命令 meowsub：基于 TOML 配置与 Btrfs reflink 编排 Arch Linux 子系统群。
//
// 三区布局（base_dir 固定为 /var/lib/meowsub，见 internal/layout）：
//
//	base/build   构建区：初始系统、编译车间、软件包池、中间层与成品层
//	base/master  使用态原始副本区：build 同步流程建立的基线副本
//	base/runs    使用态区：脱离更新体系的可变实例
//
// 命令面：
//
//	plan/build          构建编排（build 为唯一写入型流程，需 sudo）
//	destroy-run/list-runs   实例生命周期（sudo）
//	start/stop/ps       经 meowsubd 的运行态管理（免密）
//	shell/open          进入实例或启动其中应用（经守护进程，调用者身份）
//	install-daemon      安装/刷新 meowsubd 宿主单元（sudo，一次性）
//
// 本文件只做三件事：进程入口、用法文本、子命令路由。各子命令的实现
// 见同目录 cmd_*.go（构建编排、实例生命周期、守护进程、交互入口）与
// util.go（纯工具函数）。
package main

import (
	"fmt"
	"os"

	"meowsub/internal/daemon"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "meowsub:", err)
		os.Exit(1)
	}
}

func usage() error {
	return fmt.Errorf(`用法:
  meowsub plan  [-f 配置.toml] [--min-shared N] [--aur-weight W] [--prune]
      [--force-reinstall]
      干跑：解析依赖、聚类并打印将要做的事，不落盘、不需要 root。
  meowsub build [-f 配置.toml] [--min-shared N] [--aur-weight W] [--prune] [--verify]
      [--force-reinstall] [--yes] [--mem-tmpfs-size 16G] [--only 组名[,组名...]]
      真实构建/更新构建区子系统（需 sudo）；--only 只滚动基础系统并重建
      指定成品组及其途经中间层，其余子系统原地不动，收尾也仅同步这些组的
      实例；--verify 在内存构建落盘后
      全树比对暂存与实体，不一致即中止该组建档，下轮按未完成重建；
      --force-reinstall 使小组成品跳过 -Syu 滚更判定，固定整装重装；
      --mem-tmpfs-size 抬高内存构建 tmpfs 容量下限，覆盖配置
      mem_tmpfs_size（tmpfs 按需占页，调大没有预分配代价）。
      构建完成后自动同步已部署实例（成品 → 基线 → 实例）：与基线一致静默
      重置；有使用态改动会询问是否丢弃（拒绝即终止 build）；运行中的实例
      询问关闭并在同步后重启，同步期间实例锁定、shell/open 暂不可拉起；
      --yes 跳过全部询问（一律丢弃并关闭）。
  meowsub destroy-run -f 配置.toml <实例名>
  meowsub list-runs [-f 配置.toml]
  meowsub install-daemon -f 配置.toml     # 安装/刷新 meowsubd 单元（sudo）
  meowsub start|stop <组名>               # 经守护进程启停实例（需 sudo）
  meowsub ps                              # 守护进程视角的实例登记（需 sudo）
  meowsub shell <组名> [-c '命令']        # 进入实例 shell（当前用户身份，
      # 免 sudo、无登录环节）；-c 同步执行并回显输出
  meowsub open <组名> <入口名> [参数...]  # 在实例内启动导出的软件入口
      # 前台阻塞：CLI 退出/被杀即连锁终结实例内应用；--detach 提交即忘
  meowsub run-desktop <组名> <实例内.desktop路径|ID> [参数...]
      # 解析实例内 .desktop 的 Exec 行并在实例内启动应用（前台阻塞，
      # 语义同 open 的 --detach）
目录结构（固定）：
  /var/lib/meowsub/{build,master,runs,bin}
  /etc/meowsub/meowsub.toml`)
}

// run 是子命令路由表：switch 的每个 case 对应 cmd_*.go 里的一个入口函数。
func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "plan":
		return runPlan(args)
	case "build":
		return runBuild(args)
	case "destroy-run":
		return runDestroyRun(args)
	case "list-runs":
		return runListRuns(args)
	case "start", "stop":
		return runStartStop(args)
	case "ps":
		return daemon.Ps()
	case "install-daemon":
		return runInstallDaemon(args)
	case "daemon-run":
		return runDaemonRun(args)
	case "shell":
		return shellCmd(args[1:])
	case "open":
		return openCmd(args[1:])
	case "run-desktop":
		return runDesktopCmd(args[1:])
	default:
		return usage()
	}
}
