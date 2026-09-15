package reconcile

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"meowsub/internal/planner"
	"meowsub/internal/state"
)

// ---- 测试脚手架 ----

func closure(pkgs ...string) *planner.Closure {
	return &planner.Closure{Pkgs: pkgs, Aur: map[string]bool{}}
}

type nodeSpec struct {
	name       string
	kind       string
	parent     string // "arch" 表示根级
	groups     []string
	installSet []string
}

func buildPlan(specs []nodeSpec) *planner.Plan {
	p := &planner.Plan{
		BaseDir:  "/srv/subs",
		BasePkg:  planner.DefaultBasePackages,
		Closures: map[string]*planner.Closure{},
	}
	byName := map[string]*planner.Node{}
	for _, s := range specs {
		n := &planner.Node{
			Name: s.name, Kind: s.kind,
			InstallSet: s.installSet, Groups: s.groups,
		}
		if s.parent != "arch" && s.parent != "" {
			n.Parent = byName[s.parent]
			n.Parent.Children = append(n.Parent.Children, n)
		}
		byName[s.name] = n
	}
	for _, s := range specs {
		n := byName[s.name]
		n.Path = planPath(p, n)
		if n.Parent == nil {
			p.Roots = append(p.Roots, n)
		}
		p.Nodes = append(p.Nodes, n)
	}
	return p
}

func planPath(p *planner.Plan, n *planner.Node) string {
	if n.Kind == planner.KindIntermediate || n.Parent != nil {
		return p.BaseDir + "/sub/" + n.Name
	}
	return p.BaseDir + "/" + n.Name
}

func record(s nodeSpec) *state.NodeRecord {
	return &state.NodeRecord{
		Name: s.name, Kind: s.kind, Parent: parentKey(s),
		Groups: s.groups, InstallSet: s.installSet,
		Path: "/srv/subs/" + pathOf(s),
	}
}

func parentKey(s nodeSpec) string {
	if s.parent == "arch" {
		return "arch"
	}
	return s.parent
}

func pathOf(s nodeSpec) string {
	if s.parent == "arch" {
		return s.name
	}
	return "sub/" + s.name
}

func baseMarkedView() *FsView {
	return &FsView{
		BaseExists: true, BaseMarked: true,
		UnmarkedAt: map[string]string{},
		Existing:   map[string]*state.NodeRecord{},
		Small:      map[string]*state.NodeRecord{},
	}
}

func emptyView() *FsView {
	return &FsView{
		BaseExists: false, BaseMarked: false,
		UnmarkedAt: map[string]string{},
		Existing:   map[string]*state.NodeRecord{},
		Small:      map[string]*state.NodeRecord{},
	}
}

func actionSummary(acts []*Action) []string {
	var out []string
	for _, a := range acts {
		out = append(out, fmt.Sprintf("%s:%s", a.Type, a.Name))
	}
	return out
}

func wantActions(t *testing.T, got []*Action, want []string) {
	t.Helper()
	gs := actionSummary(got)
	if !reflect.DeepEqual(gs, want) {
		t.Fatalf("actions = %v\nwant        %v", gs, want)
	}
}

// ---- 场景 ----

var docSpecs = []nodeSpec{
	{name: "tool", kind: planner.KindFinal, parent: "arch", groups: []string{"tool"}, installSet: []string{"libvterm", "neovim", "vim"}},
	{name: "code+dev-1a2b3c", kind: planner.KindIntermediate, parent: "arch", groups: []string{"code", "dev"}, installSet: []string{"cargo", "rust"}},
	{name: "code", kind: planner.KindFinal, parent: "code+dev-1a2b3c", groups: []string{"code"}, installSet: []string{"vscode"}},
	{name: "dev", kind: planner.KindFinal, parent: "code+dev-1a2b3c", groups: []string{"dev"}, installSet: []string{"libvterm", "vim"}},
}

func TestFreshEnvironment(t *testing.T) {
	plan := buildPlan(docSpecs)
	acts := Reconcile(plan, emptyView(), Options{})
	wantActions(t, acts, []string{
		"recreate_base:arch",
		"sync_pool:pool",
		"create_node:tool",
		"create_node:code+dev-1a2b3c",
		"create_node:code",
		"create_node:dev",
		"delete_intermediates:sub", // 中间层收割即弃
	})
}

func TestAurClosureProducesBuildStep(t *testing.T) {
	// 闭包含 AUR 包时才需要编译车间与预构建步骤
	plan := buildPlan(docSpecs)
	plan.Closures["code"] = &planner.Closure{
		Pkgs: []string{"vscode", "cargo", "rust"},
		Aur:  map[string]bool{"vscode": true},
	}
	acts := Reconcile(plan, emptyView(), Options{})
	wantActions(t, acts, []string{
		"recreate_base:arch",
		"sync_pool:pool",
		"ensure_builder:builder",
		"build_aur:aur",
		"create_node:tool",
		"create_node:code+dev-1a2b3c",
		"create_node:code",
		"create_node:dev",
		"delete_intermediates:sub",
	})
	var bact *Action
	for _, a := range acts {
		if a.Type == ActBuildAur {
			bact = a
		}
	}
	if bact == nil {
		t.Fatal("build_aur 动作缺失")
	}
	if strings.Join(bact.Pkgs, ",") != "vscode" {
		t.Errorf("build_aur 应携带 AUR 目标清单, got %v", bact.Pkgs)
	}
}

func TestSyncPoolExcludesAurTargets(t *testing.T) {
	// 宿主机 pacman -Sw 不认 AUR 名：包池清单必须剔除 AUR 目标
	plan := buildPlan(docSpecs)
	plan.Closures["code"] = &planner.Closure{
		Pkgs: []string{"vscode", "cargo", "rust"},
		Aur:  map[string]bool{"vscode": true},
	}
	var pool *Action
	for _, a := range Reconcile(plan, emptyView(), Options{}) {
		if a.Type == ActSyncPool {
			pool = a
		}
	}
	if pool == nil {
		t.Fatal("sync_pool 动作缺失")
	}
	for _, p := range pool.Pkgs {
		if plan.Closures["code"].Aur[p] {
			t.Errorf("AUR 包 %s 混入了包池下载清单: %v", p, pool.Pkgs)
		}
	}
	foundRust := false
	for _, p := range pool.Pkgs {
		if p == "rust" {
			foundRust = true
		}
	}
	if !foundRust {
		t.Errorf("官方包 rust 应在清单中: %v", pool.Pkgs)
	}
}

func TestNoBuilderWithoutAur(t *testing.T) {
	plan := buildPlan(docSpecs) // fixture 全部 Aur 为空
	for _, cl := range plan.Closures {
		if len(cl.Aur) != 0 {
			t.Fatalf("fixture 应无 AUR 闭包")
		}
	}
	for _, a := range Reconcile(plan, emptyView(), Options{}) {
		if a.Type == ActEnsureBuilder || a.Type == ActBuildAur {
			t.Errorf("无 AUR 不应出现 %s", a.Type)
		}
	}
}

func TestExistingNodesAlwaysRebuilt(t *testing.T) {
	// 写时复制不透传底层更新：已有节点一律随基础系统整体重建
	plan := buildPlan(docSpecs)
	view := baseMarkedView()
	for _, s := range docSpecs {
		r := record(s)
		view.Existing[s.name] = r
	}
	acts := Reconcile(plan, view, Options{})
	wantActions(t, acts, []string{
		"update_base:arch",
		"sync_pool:pool",
		"recreate_node:tool",
		"recreate_node:code+dev-1a2b3c",
		"recreate_node:code",
		"recreate_node:dev",
		"delete_intermediates:sub",
	})
	for _, a := range acts {
		if a.Type == ActRecreateNode && a.Reason == "" {
			t.Errorf("recreate 应携带原因")
		}
	}
}

func TestReparentForcesRecreate(t *testing.T) {
	specs := []nodeSpec{
		{name: "m-abc123", kind: planner.KindIntermediate, parent: "arch", groups: []string{"x", "y"}, installSet: []string{"p"}},
		{name: "x", kind: planner.KindFinal, parent: "m-abc123", groups: []string{"x"}, installSet: []string{"px"}},
	}
	plan := buildPlan(specs)
	view := baseMarkedView()
	stale := record(nodeSpec{name: "x", kind: planner.KindFinal, parent: "arch", groups: []string{"x"}, installSet: []string{"px"}})
	view.Existing["x"] = stale
	view.Existing["m-abc123"] = record(specs[0])

	acts := Reconcile(plan, view, Options{})
	found := false
	for _, a := range acts {
		if a.Name == "x" && a.Type == ActRecreateNode {
			found = true
			if a.Reason == "" {
				t.Errorf("recreate should carry reason")
			}
		}
	}
	if !found {
		t.Fatalf("want recreate for x, got %v", actionSummary(acts))
	}
}

func TestOrphanWarnAndPrune(t *testing.T) {
	plan := buildPlan(docSpecs[:2]) // 只期望 tool 与中间层（code/dev 不再需要）
	view := baseMarkedView()
	for _, s := range docSpecs {
		view.Existing[s.name] = record(s)
	}
	// code/dev 在盘上有标记但期望树不再引用
	orphans := []*state.NodeRecord{record(docSpecs[2]), record(docSpecs[3])}
	view.Orphans = orphans

	acts := Reconcile(plan, view, Options{Prune: false})
	last := acts[len(acts)-2:]
	wantActions(t, last, []string{"orphan:code", "orphan:dev"})

	acts = Reconcile(plan, view, Options{Prune: true})
	last = acts[len(acts)-2:]
	wantActions(t, last, []string{"prune_node:code", "prune_node:dev"})
}

func TestUnmarkedBaseTriggersRecreate(t *testing.T) {
	plan := buildPlan(docSpecs)
	view := emptyView()
	view.BaseExists = true
	view.BaseMarked = false
	acts := Reconcile(plan, view, Options{})
	if acts[0].Type != ActRecreateBase {
		t.Fatalf("first action = %s, want recreate_base", acts[0].Type)
	}
	if acts[0].Reason == "" {
		t.Errorf("reason should mention 无标记")
	}
}

func TestStaleUnmarkedDirRemovedBeforeCreate(t *testing.T) {
	plan := buildPlan(docSpecs)
	view := emptyView()
	view.BaseExists = true
	view.BaseMarked = true
	// tool 位置上有个无标记的陌生目录
	view.UnmarkedAt["/srv/subs/tool"] = "陌生目录"
	acts := Reconcile(plan, view, Options{})
	wantActions(t, acts, []string{
		"update_base:arch",
		"sync_pool:pool",
		"remove_stale:tool",
		"create_node:tool",
		"create_node:code+dev-1a2b3c",
		"create_node:code",
		"create_node:dev",
		"delete_intermediates:sub",
	})
}

func TestStateButMissingOnDiskRecreates(t *testing.T) {
	plan := buildPlan(docSpecs)
	view := baseMarkedView()
	for _, s := range docSpecs {
		view.Existing[s.name] = record(s)
	}
	delete(view.Existing, "dev") // 盘上扫描不到 dev（被误删），应新建
	acts := Reconcile(plan, view, Options{})
	wantActions(t, acts, []string{
		"update_base:arch",
		"sync_pool:pool",
		"recreate_node:tool",
		"recreate_node:code+dev-1a2b3c",
		"recreate_node:code",
		"create_node:dev",
		"delete_intermediates:sub",
	})
}

// 强制重装：包集合无差异的小组成品也跳过滚更，固定整装重建。
func TestForceReinstallSkipsSmallSync(t *testing.T) {
	plan := buildPlan(docSpecs)
	addSmall(plan, "go", "gdep")
	view := baseMarkedView()
	view.Small["go"] = record(nodeSpec{name: "go", kind: planner.KindFinal,
		parent: "arch", groups: []string{"go"}, installSet: []string{"gdep"}})

	acts := Reconcile(plan, view, Options{})
	wantActions(t, acts, []string{
		"update_base:arch",
		"sync_pool:pool",
		"create_node:tool",
		"create_node:code+dev-1a2b3c",
		"create_node:code",
		"create_node:dev",
		"delete_intermediates:sub",
		"update_final_small:go",
	})

	acts = Reconcile(plan, view, Options{ForceReinstall: true})
	wantActions(t, acts, []string{
		"update_base:arch",
		"sync_pool:pool",
		"create_node:tool",
		"create_node:code+dev-1a2b3c",
		"create_node:code",
		"create_node:dev",
		"delete_intermediates:sub",
		"build_final_small:go",
	})
}

func TestParentsOrderedBeforeChildren(t *testing.T) {
	plan := buildPlan(docSpecs)
	acts := Reconcile(plan, emptyView(), Options{})
	idx := map[string]int{}
	for i, a := range acts {
		idx[a.Name] = i
	}
	if idx["code+dev-1a2b3c"] >= idx["code"] || idx["code+dev-1a2b3c"] >= idx["dev"] {
		t.Errorf("parent must precede children: %v", actionSummary(acts))
	}
}

// ---- 小组与中间层删除 ----

// addSmall 向计划追加一个根级小组成品（内存构建路径），并填好路径。
func addSmall(p *planner.Plan, name string, installSet ...string) *planner.Node {
	n := &planner.Node{
		Name: name, Kind: planner.KindFinal,
		Groups: []string{name}, InstallSet: installSet,
		Path: p.BaseDir + "/" + name,
	}
	p.SmallNodes = append(p.SmallNodes, n)
	return n
}

func TestSmallFreshFallsAfterTreeWithIntermediatesDeleted(t *testing.T) {
	plan := buildPlan(docSpecs)
	addSmall(plan, "go", "gdep")
	acts := Reconcile(plan, baseMarkedView(), Options{})
	wantActions(t, acts, []string{
		"update_base:arch",
		"sync_pool:pool",
		"create_node:tool",
		"create_node:code+dev-1a2b3c",
		"create_node:code",
		"create_node:dev",
		"delete_intermediates:sub",
		"build_final_small:go",
	})
}

func TestDeleteIntermediatesPositionAndPayload(t *testing.T) {
	plan := buildPlan(docSpecs)
	addSmall(plan, "go", "gdep")
	acts := Reconcile(plan, baseMarkedView(), Options{})
	var delIdx, lastNodeIdx, smallIdx int
	delIdx, lastNodeIdx, smallIdx = -1, -1, -1
	for i, a := range acts {
		switch {
		case a.Type == ActDeleteIntermediates:
			delIdx = i
		case a.Type == ActBuildFinalSmall || a.Type == ActUpdateFinalSmall:
			smallIdx = i
		case a.Type == ActCreateNode || a.Type == ActRecreateNode:
			lastNodeIdx = i
		}
	}
	if delIdx < 0 || smallIdx < 0 {
		t.Fatalf("缺删除或小组动作：%v", actionSummary(acts))
	}
	if !(delIdx > lastNodeIdx && delIdx < smallIdx) {
		t.Errorf("删中间层应在节点完成后、小组开始前：%v", actionSummary(acts))
	}
	var del *Action
	for _, a := range acts {
		if a.Type == ActDeleteIntermediates {
			del = a
		}
	}
	want := []string{"/srv/subs/sub/code+dev-1a2b3c"}
	if !reflect.DeepEqual(del.Paths, want) {
		t.Errorf("删中间层应携带本计划全部中间层路径 %v，got %v", want, del.Paths)
	}
}

func TestNoIntermediateNoDeleteAction(t *testing.T) {
	leafOnly := []nodeSpec{docSpecs[0]} // 只有 tool 一个叶
	plan := buildPlan(leafOnly)
	addSmall(plan, "go", "gdep")
	acts := Reconcile(plan, baseMarkedView(), Options{})
	for _, a := range acts {
		if a.Type == ActDeleteIntermediates {
			t.Fatalf("无中间层不应出现删除动作：%v", actionSummary(acts))
		}
	}
}

func TestSmallActionThreeStates(t *testing.T) {
	cases := []struct {
		name       string
		oldSet     []string
		want       ActionType
		wantRecord bool
	}{
		{"无记录应全新构建", nil, ActBuildFinalSmall, false},
		{"集合相等应滚更", []string{"gdep-new"}, ActUpdateFinalSmall, true},
		{"集合不等应重建", []string{"gdep-old"}, ActBuildFinalSmall, true},
	}
	for _, c := range cases {
		plan := buildPlan(docSpecs)
		goN := addSmall(plan, "go", "gdep-new")
		view := baseMarkedView()
		if c.oldSet != nil {
			view.Small["go"] = &state.NodeRecord{
				Name: "go", Kind: state.KindFinal, Parent: "arch",
				Path: "/srv/subs/go", InstallSet: c.oldSet,
			}
		}
		acts := Reconcile(plan, view, Options{})
		var got *Action
		for _, a := range acts {
			if a.Name == "go" && (a.Type == ActBuildFinalSmall || a.Type == ActUpdateFinalSmall) {
				got = a
			}
		}
		if got == nil || got.Type != c.want {
			t.Errorf("%s: want %v got %v", c.name, c.want, actionSummary(acts))
			continue
		}
		if (got.Record != nil) != c.wantRecord {
			t.Errorf("%s: Record 携带不符（want %v）", c.name, c.wantRecord)
		}
		if got.Node != goN {
			t.Errorf("%s: 应携带期望节点", c.name)
		}
		if got.Reason == "" {
			t.Errorf("%s: 应说明语义", c.name)
		}
	}
}

func TestSmallUnmarkedDirRemovedBeforeBuild(t *testing.T) {
	plan := buildPlan([]nodeSpec{})
	addSmall(plan, "go", "gdep")
	view := baseMarkedView()
	view.UnmarkedAt["/srv/subs/go"] = "陌生目录"
	acts := Reconcile(plan, view, Options{})
	wantActions(t, acts, []string{
		"update_base:arch",
		"sync_pool:pool",
		"remove_stale:go",
		"build_final_small:go",
	})
}

func TestSmallClosuresJoinPoolUnion(t *testing.T) {
	plan := buildPlan([]nodeSpec{})
	addSmall(plan, "go", "cnrust")
	plan.Closures["go"] = &planner.Closure{
		Pkgs: []string{"cnrust", "vscode-bin"},
		Aur:  map[string]bool{"vscode-bin": true},
	}
	acts := Reconcile(plan, baseMarkedView(), Options{})
	var pool, aur *Action
	for _, a := range acts {
		switch a.Type {
		case ActSyncPool:
			pool = a
		case ActBuildAur:
			aur = a
		}
	}
	if pool == nil {
		t.Fatal("sync_pool 缺失")
	}
	has := map[string]bool{}
	for _, p := range pool.Pkgs {
		has[p] = true
	}
	if !has["cnrust"] {
		t.Errorf("小组闭包官方包应入池清单：%v", pool.Pkgs)
	}
	if has["vscode-bin"] {
		t.Errorf("AUR 包不应混入池下载清单：%v", pool.Pkgs)
	}
	if aur == nil || !contains(aur.Pkgs, "vscode-bin") {
		t.Errorf("小组 AUR 包也应进车间预构建：%v", actionSummary(acts))
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// 定向构建（build --only）的核心语义：调和消费裁剪后的计划 + 全量磁盘视图
// ——动作只触及目标组及其途经祖先，预备层只覆盖目标闭包，孤儿判定仍按
// 全量期望树生效（未选组绝不会被误判为孤儿）。
func TestScopedPlanReconcileTouchesOnlyTargets(t *testing.T) {
	plan := buildPlan(docSpecs)
	addSmall(plan, "go", "gdep")
	plan.Closures["tool"] = closure("vim", "libvterm")
	plan.Closures["code"] = closure("vscode", "cargo", "rust")
	plan.Closures["dev"] = closure("rustup", "gdb")
	plan.Closures["go"] = closure("gdep")

	scoped, err := plan.Scope([]string{"code"})
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}
	// 全量扫描视角：各节点都在盘上（已有即重建），go 同样有记录
	view := baseMarkedView()
	for _, s := range docSpecs {
		view.Existing[s.name] = record(s)
	}
	view.Small["go"] = record(nodeSpec{name: "go", kind: planner.KindFinal,
		parent: "arch", groups: []string{"go"}, installSet: []string{"gdep"}})

	acts := Reconcile(scoped, view, Options{})
	wantActions(t, acts, []string{
		"update_base:arch",
		"sync_pool:pool",
		"recreate_node:code+dev-1a2b3c",
		"recreate_node:code",
		"delete_intermediates:sub",
	})
	// 预备层只覆盖目标组闭包：tool/dev/go 独有包不得混入
	var pool *Action
	for _, a := range acts {
		if a.Type == ActSyncPool {
			pool = a
		}
	}
	if pool == nil {
		t.Fatal("sync_pool 缺失")
	}
	for _, p := range pool.Pkgs {
		if p == "vim" || p == "libvterm" || p == "rustup" || p == "gdb" || p == "gdep" {
			t.Errorf("未选组的包 %s 混入了包池清单：%v", p, pool.Pkgs)
		}
	}
	if !contains(pool.Pkgs, "vscode") || !contains(pool.Pkgs, "rust") {
		t.Errorf("目标组闭包应完整入池：%v", pool.Pkgs)
	}
	// 中间层收割只携带保留的中间层
	for _, a := range acts {
		if a.Type == ActDeleteIntermediates {
			want := []string{"/srv/subs/sub/code+dev-1a2b3c"}
			if !reflect.DeepEqual(a.Paths, want) {
				t.Errorf("收割路径应随计划收窄 %v，got %v", want, a.Paths)
			}
		}
	}
	// 全量视图里 tool/dev/go 都有记录，但绝不产生针对它们的动作
	for _, a := range acts {
		if a.Name == "tool" || a.Name == "dev" || a.Name == "go" {
			t.Errorf("未选组 %s 不应产生动作：%v", a.Name, actionSummary(acts))
		}
	}
}
