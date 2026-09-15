// cmd_build.go：build 子命令 —— 唯一写入型流程，要求 root。
//
// parsePlanFlags 与 preparePlan 为 plan/build 共用（cmd_plan.go 也引用）：
// 先经「解析依赖闭包 → 聚类规划 → 调和」得到动作序列，build 再落成现实。
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"meowsub/internal/builder"
	"meowsub/internal/config"
	"meowsub/internal/daemon"
	"meowsub/internal/execx"
	"meowsub/internal/layout"
	"meowsub/internal/planner"
	"meowsub/internal/reconcile"
	"meowsub/internal/runner"
	"meowsub/internal/state"
)

// planFlags 是 plan/build 共用的参数集。
type planFlags struct {
	cfgPath        string
	prune          bool
	minShared      int
	aurWeight      float64
	verify         bool
	forceReinstall bool
	yes            bool
	memTmpfsSize   string
	only           string
}

// parsePlanFlags 解析 plan/build 共用参数。解析失败返回错误，由入口统一
// 转成 usage()。verify 仅 build 有意义：小组内存构建落盘后全树比对暂存与
// 实体，不一致即中止收尾。force-reinstall 使小组成品跳过滚更判定，固定
// 从基础 arch 整体重装。yes 仅 build 有意义：收尾自动同步跳过一切询问。
// mem-tmpfs-size 仅 build 有意义：内存构建 tmpfs 容量下限（如 16G），
// 覆盖配置 mem_tmpfs_size。only 仅 build 有意义：逗号分隔的成品组名，
// 只重建这些组及其途经中间层，其余子系统原地不动。
func parsePlanFlags(args []string) (planFlags, error) {
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	p := fs.String("f", daemon.ConfigPath, "TOML 配置文件路径")
	pb := fs.Bool("prune", false, "删除期望树不再引用的孤儿子系统")
	pm := fs.Int("min-shared", 2, "中间子系统的最小共享包数，低于则不创建")
	pa := fs.Float64("aur-weight", 2, "聚类相似度中 AUR 包的权重")
	pv := fs.Bool("verify", false, "落盘后校验内存/实体一致（全树比对，较慢；仅 build）")
	pf := fs.Bool("force-reinstall", false, "小组成品跳过 -Syu 滚更，固定整装重装")
	py := fs.Bool("yes", false, "收尾自动同步跳过询问：一律丢弃使用态改动、关闭运行中的实例（仅 build）")
	pmt := fs.String("mem-tmpfs-size", "", "内存构建 tmpfs 容量下限，如 16G；覆盖配置 mem_tmpfs_size（仅 build）")
	po := fs.String("only", "", "只构建指定成品组（逗号分隔，如 tool,code），其余子系统原地不动（仅 build）")
	if err := fs.Parse(args[1:]); err != nil {
		return planFlags{}, err
	}
	return planFlags{cfgPath: *p, prune: *pb, minShared: *pm, aurWeight: *pa,
		verify: *pv, forceReinstall: *pf, yes: *py, memTmpfsSize: *pmt, only: *po}, nil
}

// runBuild 真实构建/更新构建区子系统：渲染计划、reflink 探针、执行动作
// 序列、宿主侧物化导出、状态落盘。带 --only 时只滚动基础系统并重建指定
// 成品组及其途经中间层，其余子系统原地不动。
func runBuild(args []string) error {
	f, err := parsePlanFlags(args)
	if err != nil {
		return usage()
	}
	// 内存 tmpfs 容量下限：命令行参数优先于配置文件；合法性在动手前校验。
	memTmpfs := int64(0)
	if f.memTmpfsSize != "" {
		if memTmpfs, err = config.ParseSize(f.memTmpfsSize); err != nil {
			return fmt.Errorf("mem-tmpfs-size: %w", err)
		}
	}
	cfg, err := config.Load(f.cfgPath)
	if err != nil {
		return err
	}
	// --only 目标组前置校验：未知组名在动手前报错（闭包解析之前，无需 root）。
	var targets []string
	if f.only != "" {
		seen := map[string]bool{}
		for _, name := range strings.Split(f.only, ",") {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			targets = append(targets, name)
		}
		if len(targets) > 0 {
			names := make([]string, 0, len(cfg.Groups))
			for _, g := range cfg.Groups {
				names = append(names, g.Name)
			}
			known := groupIndex(cfg.Groups)
			for _, t := range targets {
				if _, ok := known[t]; !ok {
					return fmt.Errorf("未知软件组 %q（配置中的组：%s）", t, strings.Join(names, ", "))
				}
			}
		}
	}
	// 配置复制表前置校验：来源缺失即中止（磁盘未动，配置即契约）。
	if err := builder.ValidateCopySources(cfg); err != nil {
		return err
	}
	hash, err := fileHash(f.cfgPath)
	if err != nil {
		return err
	}
	plan, acts, b, st, err := preparePlan(cfg, targets, f.prune, f.minShared, f.aurWeight, f.forceReinstall)
	if err != nil {
		return err
	}
	b.Verify = f.verify
	b.MemTmpfsSize = memTmpfs
	if memTmpfs == 0 {
		b.MemTmpfsSize = cfg.MemTmpfsSize
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("build 需要 root 权限运行：sudo meowsub build ...")
	}
	fmt.Print(builder.RenderPlan(plan, acts))
	if err := b.ProbeReflink(); err != nil {
		return err
	}
	st, err = b.Execute(acts, st, hash)
	if err != nil {
		return err
	}
	// 收尾自动同步：本轮出炉的成品层推给已部署实例（详见 syncInstances）。
	built := map[string]bool{}
	for _, n := range plan.Nodes {
		if n.Kind == planner.KindFinal {
			built[n.Name] = true
		}
	}
	for _, n := range plan.SmallNodes {
		built[n.Name] = true
	}
	if err := syncInstances(cfg, built, f.yes, daemonCtl{}, os.Stdin, os.Stdout); err != nil {
		return err
	}
	// 宿主侧物化：按烘焙标记生成 bin shim / 桌面条目并刷新登记表。
	// 导出物引用 build 本体生成时刻的自定位路径，而非固定安装位置。
	selfBin, err := selfPath()
	if err != nil {
		return fmt.Errorf("定位 meowsub 自身失败: %w", err)
	}
	if err := daemon.SyncExports(cfg, st, selfBin); err != nil {
		return err
	}
	if err := st.Save(cfg.BaseDir); err != nil {
		return err
	}
	fmt.Println("\n构建完成。成品子系统（软件入口已导出至宿主，自启由 meowsubd 承接）：")
	for _, n := range plan.Nodes {
		if n.Kind == planner.KindFinal {
			fmt.Println(" ", n.Path)
		}
	}
	for _, n := range plan.SmallNodes {
		fmt.Println(" ", n.Path, "（内存构建）")
	}
	return nil
}

// preparePlan 执行 plan/build 共享的规划流水线：逐组解析依赖闭包（相同包
// 列表只解析一次）、聚类建树并填落盘路径、扫描磁盘现状、调和出动作序列。
// targets 非空时（build --only）把调和范围收窄到这些成品组及其途经祖先；
// 磁盘现状扫描始终对着全量期望树做，孤儿判定不受定向影响。返回期望计划
// （定向时为裁剪副本）、有序动作序列、装配执行器与既有聚合状态。
func preparePlan(cfg *config.Config, targets []string, prune bool, minShared int,
	aurWeight float64, forceReinstall bool) (
	*planner.Plan, []*reconcile.Action, *builder.Builder, *state.State, error) {
	buildRoot := layout.Build(cfg.BaseDir)
	resolver := &builder.HostResolver{Runner: execx.ExecRunner{}}
	groups := make([]string, 0, len(cfg.Groups))
	closures := map[string]*planner.Closure{}
	cache := map[string]*planner.Closure{}
	for _, g := range cfg.Groups {
		groups = append(groups, g.Name)
		key := hashKey(g.Packages)
		c, ok := cache[key]
		if !ok {
			fmt.Fprintf(os.Stderr, "解析软件组 [%s]: %v\n", g.Name, g.Packages)
			var err error
			if c, err = resolver.Resolve(g.Packages); err != nil {
				return nil, nil, nil, nil, fmt.Errorf("软件组 [%s]: %w", g.Name, err)
			}
			fmt.Fprintf(os.Stderr, "  -> %d 个包（AUR %d 个）\n", len(c.Pkgs), countAur(c))
			cache[key] = c
		}
		closures[g.Name] = c
	}
	sizes := resolver.SizeTable()
	basePkg := append(append([]string{}, planner.DefaultBasePackages...), "paru")
	plan := planner.BuildPlan(groups, closures, basePkg, planner.Options{
		MinShared: minShared, AurWeight: aurWeight,
		Sizes: sizes, TreeSizeFloor: cfg.MinTreeGroupSize,
	})
	plan.SetPaths(buildRoot)
	full := plan
	if len(targets) > 0 {
		scoped, serr := plan.Scope(targets)
		if serr != nil {
			return nil, nil, nil, nil, serr
		}
		plan = scoped
	}
	builder.SetRenderSizes(sizes)

	b := builder.New(execx.ExecRunner{}, os.Stdout, buildRoot)
	b.MetaBase = cfg.BaseDir
	b.Groups = groupIndex(cfg.Groups)
	b.ExtraRepos = toRepos(cfg.Repositories)
	b.BuildEnvKeys = append([]string{}, cfg.BuildEnv...)
	b.BuilderTmpSize = cfg.BuilderTmpSize
	st, err := state.Load(cfg.BaseDir)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	view, err := b.ScanView(full)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	acts := reconcile.Reconcile(plan, view, reconcile.Options{Prune: prune,
		ForceReinstall: forceReinstall})
	return plan, acts, b, st, nil
}

// ---- build 收尾自动同步 ----
//
// 成品层出炉后推给全部已部署实例，保证“build 导出的软件入口指向的实例
// 就是刚构建的那一份”（shim 经 meowsub open 在调用时解析到 runs 实例，
// 实例陈旧则入口服务旧软件）。推送本体即 Deploy（成品 → 基线 → 实例）：
// 实例与旧基线一致时静默重置；有使用态改动时由安全闸询问丢弃，拒绝即
// 终止 build。运行中的实例先询问关闭、同步后重启；同步全程持实例锁，
// daemon 的 boot 与 root 直连路径都据此拒绝拉起，防目录交换竞态。

// instanceCtl 抽象 daemon 的停/启/查（测试注入替身；生产走守护进程套接字）。
type instanceCtl interface {
	running(run string) (bool, error)
	stop(run string) error
	start(run string) error
}

type daemonCtl struct{}

func (daemonCtl) running(run string) (bool, error) {
	resp, err := daemon.TryCall(daemon.Request{Op: daemon.OpRunning, Group: run})
	if err != nil {
		return false, err
	}
	m, ok := resp.Data.(map[string]any)
	if !ok {
		return false, fmt.Errorf("running 响应格式异常")
	}
	v, _ := m["running"].(bool)
	return v, nil
}

func (daemonCtl) stop(run string) error {
	_, err := daemon.TryCall(daemon.Request{Op: daemon.OpStop, Group: run})
	return err
}

func (daemonCtl) start(run string) error {
	_, err := daemon.TryCall(daemon.Request{Op: daemon.OpStart, Group: run})
	return err
}

// syncInstances 对本轮出炉组的全部已部署实例执行自动同步。built 为本轮
// 出炉的成品组名集合（nil 不限制）；无实例的组跳过（shim 经 meowsub open
// 按需自动部署）。定向构建下未出炉组的旧成品层仍在盘上，不可凭 usr 目录
// 误判为新成品，故以显式集合为准。
func syncInstances(cfg *config.Config, built map[string]bool, yes bool,
	ctl instanceCtl, in io.Reader, out io.Writer) error {
	p := runner.Paths{BaseDir: cfg.BaseDir, In: in, Out: out}
	runs, err := p.List()
	if err != nil {
		return err
	}
	buildRoot := layout.Build(cfg.BaseDir)
	for _, g := range cfg.Groups {
		if built != nil && !built[g.Name] {
			continue
		}
		layer := filepath.Join(buildRoot, g.Name)
		if fi, serr := os.Stat(filepath.Join(layer, "usr")); serr != nil || !fi.IsDir() {
			continue // 本轮没有该组的新成品
		}
		for _, run := range runs {
			if run != g.Name && !strings.HasSuffix(run, "@"+g.Name) {
				continue
			}
			if err := syncOne(p, ctl, layer, run, yes, in, out); err != nil {
				return fmt.Errorf("实例同步中止（成品层已更新；处理好实例数据后带 --yes 重跑 build 即可完成剩余同步）：%w", err)
			}
		}
	}
	return nil
}

// syncOne 单实例同步：加锁 → 按需询问并停止 → Deploy（含丢弃闸口）→
// 解锁 → 按需重启。关闭被拒绝时跳过该实例（不终止 build）。
func syncOne(p runner.Paths, ctl instanceCtl, layer, run string, yes bool,
	in io.Reader, out io.Writer) error {
	unlock, err := daemon.LockInstance(p.BaseDir, run)
	if err != nil {
		return err
	}
	defer unlock()
	wasRunning, rerr := ctl.running(run)
	if rerr != nil {
		fmt.Fprintf(out, "[sync] %s 运行状态不可知（%v），按未运行处理\n", run, rerr)
	}
	if wasRunning && !yes {
		if !confirmIn(in, out, fmt.Sprintf("实例 %s 正在运行：关闭并在同步后重新开启？", run)) {
			fmt.Fprintf(out, "[sync] 跳过 %s（保持旧树运行；稍后可显式 deploy）\n", run)
			return nil
		}
	}
	if wasRunning {
		if err := stopAndWait(ctl, run); err != nil {
			return err
		}
	}
	err = p.Deploy(layer, run, yes)
	if err == nil && wasRunning {
		if serr := ctl.start(run); serr != nil {
			fmt.Fprintf(out, "[sync] 警告：%s 已同步但重启失败（start 可手动补）：%v\n", run, serr)
		}
	}
	return err
}

// stopAndWait 停止实例并轮询确认断电（machinectl poweroff 异步）。
func stopAndWait(ctl instanceCtl, run string) error {
	if err := ctl.stop(run); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		r, err := ctl.running(run)
		if err != nil || !r {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待实例 %s 停止超时", run)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// confirmIn 打印问题并读取 y/n；无输入可用（EOF）一律视为拒绝。
func confirmIn(in io.Reader, out io.Writer, question string) bool {
	fmt.Fprintf(out, "%s [y/N] ", question)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
