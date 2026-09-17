package planner

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func cl(aur []string, pkgs ...string) *Closure {
	c := &Closure{Pkgs: append([]string{}, pkgs...), Aur: map[string]bool{}}
	for _, a := range aur {
		c.Aur[a] = true
	}
	sort.Strings(c.Pkgs)
	return c
}

var testBase = []string{"base", "base-devel", "sudo", "git"}

func TestDefaultBasePackagesIncludeDBusProxy(t *testing.T) {
	for _, pkg := range DefaultBasePackages {
		if pkg == "xdg-dbus-proxy" {
			return
		}
	}
	t.Fatalf("DefaultBasePackages = %v, want xdg-dbus-proxy", DefaultBasePackages)
}

// docFixture 复刻设计文档场景：tool/code/dev 三组。
func docFixture() ([]string, map[string]*Closure) {
	groups := []string{"code", "dev", "tool"} // 已排序
	closures := map[string]*Closure{
		"code": cl([]string{"vscode"}, "vscode", "cargo", "rust"),
		"dev":  cl(nil, "vim", "cargo", "rust", "libvterm"),
		"tool": cl(nil, "vim", "neovim", "libvterm"),
	}
	return groups, closures
}

func findNode(t *testing.T, p *Plan, name string) *Node {
	t.Helper()
	for _, n := range p.Nodes {
		if n.Name == name || strings.HasPrefix(n.Name, name+"-") {
			return n
		}
	}
	t.Fatalf("node %q not found in %+v", name, nodeNames(p))
	return nil
}

func nodeNames(p *Plan) []string {
	var out []string
	for _, n := range p.Nodes {
		out = append(out, n.Name)
	}
	return out
}

// 加权后相似度：sim(dev,tool)=2/5=0.4（共享 vim+libvterm），
// sim(code,dev)=2/6≈0.33（vscode 的双倍权重抬高分母）→ 先合并 dev+tool。
func TestBuildPlanDocScenario(t *testing.T) {
	groups, closures := docFixture()
	p := BuildPlan(groups, closures, testBase, Options{MinShared: 2, AurWeight: 2})

	if len(p.Nodes) != 4 {
		t.Fatalf("nodes = %v, want 4 (code, dev+tool 中间层, dev, tool)", nodeNames(p))
	}

	mid := findNode(t, p, "dev+tool")
	if mid.Kind != KindIntermediate {
		t.Errorf("mid kind = %q", mid.Kind)
	}
	if !reflect.DeepEqual(mid.InstallSet, []string{"libvterm", "vim"}) {
		t.Errorf("mid install set = %v, want [libvterm vim]", mid.InstallSet)
	}
	if !reflect.DeepEqual(mid.CommonSet, []string{"libvterm", "vim"}) {
		t.Errorf("mid common set = %v", mid.CommonSet)
	}
	if !reflect.DeepEqual(mid.Groups, []string{"dev", "tool"}) {
		t.Errorf("mid groups = %v", mid.Groups)
	}

	code := findNode(t, p, "code")
	if code.Parent != nil {
		t.Errorf("code 应挂根")
	}
	if !reflect.DeepEqual(code.InstallSet, []string{"cargo", "rust", "vscode"}) {
		t.Errorf("code install set = %v, want [cargo rust vscode]", code.InstallSet)
	}
	dev := findNode(t, p, "dev")
	if dev.Parent != mid {
		t.Errorf("dev parent should be mid")
	}
	if !reflect.DeepEqual(dev.InstallSet, []string{"cargo", "rust"}) {
		t.Errorf("dev install set = %v, want [cargo rust]", dev.InstallSet)
	}
	tool := findNode(t, p, "tool")
	if tool.Parent != mid {
		t.Errorf("tool parent should be mid")
	}
	if !reflect.DeepEqual(tool.InstallSet, []string{"neovim"}) {
		t.Errorf("tool install set = %v, want [neovim]", tool.InstallSet)
	}

	// 拓扑序：父在子前
	seen := map[string]bool{}
	for _, n := range p.Nodes {
		if n.Parent != nil && !seen[n.Parent.Name] {
			t.Fatalf("topo violation: %s before parent %s", n.Name, n.Parent.Name)
		}
		seen[n.Name] = true
	}
}

// 不变量：任一成品组沿根到叶路径的安装集并集 == 闭包 - 基础集合，且路径上无重复包。
func TestInstallPathInvariant(t *testing.T) {
	groups, closures := docFixture()
	p := BuildPlan(groups, closures, testBase, Options{MinShared: 2, AurWeight: 2})
	baseSet := map[string]bool{}
	for _, b := range testBase {
		baseSet[b] = true
	}
	for _, g := range groups {
		leaf := findNode(t, p, g)
		got := map[string]bool{}
		for n := leaf; n != nil; n = n.Parent {
			for _, pkg := range n.InstallSet {
				if got[pkg] {
					t.Fatalf("%s: package %s installed twice on path", g, pkg)
				}
				got[pkg] = true
			}
		}
		want := map[string]bool{}
		for _, pkg := range closures[g].Pkgs {
			if !baseSet[pkg] {
				want[pkg] = true
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s path coverage = %v, want %v", g, keys(got), keys(want))
		}
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestMinSharedSkipsSmallIntermediate(t *testing.T) {
	groups, closures := docFixture()
	p := BuildPlan(groups, closures, testBase, Options{MinShared: 3, AurWeight: 2})
	if len(p.Nodes) != 3 {
		t.Fatalf("nodes = %v, want 3 (无中间层)", nodeNames(p))
	}
	dev := findNode(t, p, "dev")
	if dev.Parent != nil {
		t.Errorf("dev should hang from root")
	}
	if !reflect.DeepEqual(dev.InstallSet, []string{"cargo", "libvterm", "rust", "vim"}) {
		t.Errorf("dev install set = %v", dev.InstallSet)
	}
}

func TestAurWeightChangesClustering(t *testing.T) {
	mk := func() ([]string, map[string]*Closure) {
		// b、c 共享 AUR 包 s；a 与 b 共享两个普通包
		g := []string{"a", "b", "c"}
		c := map[string]*Closure{
			"a": cl(nil, "p", "q"),
			"b": cl([]string{"s"}, "p", "q", "s"),
			"c": cl([]string{"s"}, "p", "r", "s"),
		}
		return g, c
	}

	// 高 AUR 权重：b、c 因共享 AUR 包 s 聚在一起
	g, c := mk()
	pHigh := BuildPlan(g, c, nil, Options{MinShared: 2, AurWeight: 10})
	var foundBC, foundAB bool
	for _, n := range pHigh.Nodes {
		if reflect.DeepEqual(n.Groups, []string{"b", "c"}) {
			foundBC = true
		}
		if reflect.DeepEqual(n.Groups, []string{"a", "b"}) {
			foundAB = true
		}
	}
	if !foundBC || foundAB {
		t.Fatalf("weight=10: got %v, want b+c 中间层且无 a+b", nodeNames(pHigh))
	}

	// 权重为 1：普通包占优，a、b 聚在一起
	g, c = mk()
	pLow := BuildPlan(g, c, nil, Options{MinShared: 2, AurWeight: 1})
	foundBC, foundAB = false, false
	for _, n := range pLow.Nodes {
		if reflect.DeepEqual(n.Groups, []string{"b", "c"}) {
			foundBC = true
		}
		if reflect.DeepEqual(n.Groups, []string{"a", "b"}) {
			foundAB = true
		}
	}
	if !foundAB || foundBC {
		t.Fatalf("weight=1: got %v, want a+b 中间层且无 b+c", nodeNames(pLow))
	}
}

func TestIntermediateNameStableAndShaped(t *testing.T) {
	groups, closures := docFixture()
	p1 := BuildPlan(groups, closures, testBase, Options{MinShared: 2, AurWeight: 2})
	p2 := BuildPlan(groups, closures, testBase, Options{MinShared: 2, AurWeight: 2})
	n1 := findNode(t, p1, "dev+tool").Name
	n2 := findNode(t, p2, "dev+tool").Name
	if n1 != n2 {
		t.Fatalf("name unstable: %q vs %q", n1, n2)
	}
	re := regexp.MustCompile(`^dev\+tool-[0-9a-f]{6}$`)
	if !re.MatchString(n1) {
		t.Errorf("name shape = %q", n1)
	}
}

// 基础集合扣除：git 已在基础系统中，交集只剩 git 时中间层退化为虚节点，
// 各成品只装自己的独有包。
func TestBasePackagesSubtracted(t *testing.T) {
	g := []string{"x", "y"}
	c := map[string]*Closure{
		"x": cl(nil, "git", "htop"), // git 在基础集合中
		"y": cl(nil, "git", "bat"),
	}
	baseWithParu := append(append([]string{}, testBase...), "paru")
	p := BuildPlan(g, c, baseWithParu, Options{MinShared: 2})
	if len(p.Nodes) != 2 {
		t.Fatalf("nodes = %v, want 2 leaves（交集被基础集扣空）", nodeNames(p))
	}
	x := findNode(t, p, "x")
	if !reflect.DeepEqual(x.InstallSet, []string{"htop"}) {
		t.Errorf("x install set = %v, want [htop]", x.InstallSet)
	}
	y := findNode(t, p, "y")
	if !reflect.DeepEqual(y.InstallSet, []string{"bat"}) {
		t.Errorf("y install set = %v, want [bat]", y.InstallSet)
	}
}

func TestDisjointGroupsNoIntermediate(t *testing.T) {
	g := []string{"x", "y"}
	c := map[string]*Closure{
		"x": cl(nil, "aaa"),
		"y": cl(nil, "bbb"),
	}
	p := BuildPlan(g, c, nil, Options{MinShared: 2})
	if len(p.Nodes) != 2 {
		t.Fatalf("nodes = %v, want 2 leaves", nodeNames(p))
	}
	for _, n := range p.Nodes {
		if n.Kind != KindFinal {
			t.Errorf("unexpected non-final node %s", n.Name)
		}
	}
}

func TestSingleGroup(t *testing.T) {
	p := BuildPlan([]string{"solo"}, map[string]*Closure{"solo": cl(nil, "vim")}, nil, Options{})
	if len(p.Nodes) != 1 || p.Nodes[0].Kind != KindFinal || p.Nodes[0].Parent != nil {
		t.Fatalf("want single root leaf, got %+v", p.Nodes)
	}
}

// ---- 尺寸分流 ----

// sizesFixture 按包名给尺寸（pacman -Si 口径），使各闭包总和形成排序：
// sum(code)=1100（进树）、sum(dev)=1000（恰在阈值）、sum(tool)=650（小组）。
func sizesFixture() map[string]int64 {
	return map[string]int64{
		"vscode":   600,
		"cargo":    250,
		"rust":     250,
		"vim":      300,
		"libvterm": 200,
		"neovim":   150,
	}
}

const testFloor = int64(1000)

func TestSizeThresholdSplitsSmallGroups(t *testing.T) {
	groups, closures := docFixture()
	p := BuildPlan(groups, closures, testBase, Options{
		MinShared: 2, AurWeight: 2,
		Sizes: sizesFixture(), TreeSizeFloor: testFloor,
	})
	names := nodeNames(p)
	if len(names) != 1 || !strings.HasPrefix(names[0], "code") {
		t.Fatalf("树上只应有 code 一条成品链，got %v", names)
	}
	var small []string
	for _, n := range p.SmallNodes {
		small = append(small, n.Name)
	}
	if want := []string{"dev", "tool"}; !reflect.DeepEqual(small, want) {
		t.Fatalf("SmallNodes = %v, want %v（按名排序）", small, want)
	}
	// 小组的安装集 = 闭包 − 基础集
	dev := p.SmallNodes[0]
	baseSet := map[string]bool{}
	for _, b := range testBase {
		baseSet[b] = true
	}
	got := []string{}
	for _, pkg := range closures["dev"].Pkgs {
		if !baseSet[pkg] {
			got = append(got, pkg)
		}
	}
	sort.Strings(got)
	if len(got) == 0 || !equalStrings(dev.InstallSet, got) {
		t.Errorf("dev.InstallSet = %v, want %v", dev.InstallSet, got)
	}
	if dev.Kind != KindFinal || dev.Parent != nil {
		t.Errorf("小组应为根级 final 节点: %+v", dev)
	}
}

func TestSizeThresholdStrictBoundary(t *testing.T) {
	c := map[string]*Closure{"solo": cl(nil, "pkg-a")}
	sizes := map[string]int64{"pkg-a": 1000}
	p := BuildPlan([]string{"solo"}, c, nil, Options{Sizes: sizes, TreeSizeFloor: 1000})
	if len(p.Nodes) != 0 || len(p.SmallNodes) != 1 {
		t.Fatalf("等于阈值应归小组：nodes=%v smalls=%v", nodeNames(p), len(p.SmallNodes))
	}
	sizes["pkg-a"] = 1001
	p = BuildPlan([]string{"solo"}, c, nil, Options{Sizes: sizes, TreeSizeFloor: 1000})
	if len(p.Nodes) != 1 || len(p.SmallNodes) != 0 {
		t.Fatalf("大于阈值应进树：nodes=%v smalls=%v", nodeNames(p), len(p.SmallNodes))
	}
}

func TestUnknownSizesStayInTree(t *testing.T) {
	// 纯 AUR 组闭包在 sync 库查不到尺寸（sizes 缺失）：无法判定大小，
	// 保守按大树处理，维持既有聚类行为。
	groups := []string{"aurgrp"}
	closures := map[string]*Closure{"aurgrp": cl([]string{"yay"}, "yay")}
	sizes := map[string]int64{} // 没有任何已知尺寸
	p := BuildPlan(groups, closures, testBase, Options{Sizes: sizes, TreeSizeFloor: 500000000})
	if len(p.Nodes) != 1 || len(p.SmallNodes) != 0 {
		t.Fatalf("尺寸未知组应留在树上：nodes=%v smalls=%v", nodeNames(p), len(p.SmallNodes))
	}
}

func TestZeroThresholdKeepsLegacyBehaviour(t *testing.T) {
	groups, closures := docFixture()
	legacy := BuildPlan(groups, closures, testBase, Options{MinShared: 2, AurWeight: 2})
	withSizes := BuildPlan(groups, closures, testBase, Options{
		MinShared: 2, AurWeight: 2, Sizes: sizesFixture(), TreeSizeFloor: 0,
	})
	if !reflect.DeepEqual(nodeNames(legacy), nodeNames(withSizes)) {
		t.Errorf("阈值为 0 应与旧行为全等：%v vs %v",
			nodeNames(legacy), nodeNames(withSizes))
	}
	if len(withSizes.SmallNodes) != 0 {
		t.Errorf("阈值为 0 不应产生小组")
	}
}

func TestSetPathsCoversSmallNodes(t *testing.T) {
	groups, closures := docFixture()
	p := BuildPlan(groups, closures, testBase, Options{
		Sizes: sizesFixture(), TreeSizeFloor: 1000,
	})
	p.SetPaths("/srv/subs")
	if p.BaseDir != "/srv/subs" {
		t.Fatal("BaseDir 未填")
	}
	for _, n := range p.SmallNodes {
		if n.Path != "/srv/subs/"+n.Name {
			t.Errorf("small %s path = %q", n.Name, n.Path)
		}
	}
	for _, n := range p.Nodes {
		if n.Kind == KindIntermediate && n.Path != "/srv/subs/sub/"+n.Name {
			t.Errorf("intermediate %s path = %q", n.Name, n.Path)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
