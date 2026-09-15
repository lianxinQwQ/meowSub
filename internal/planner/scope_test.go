// scope_test.go：Plan.Scope 的定向裁剪语义——保留目标组与途经祖先链、
// 剔除兄弟与无关节点、Closures 随目标收窄、小组目标走 SmallNodes、
// 未知组报错并列出可选组名、多目标共享祖先不重复。
package planner

import (
	"sort"
	"strings"
	"testing"
)

// scopeFixture 手工构造一棵计划树：
//
//	mid（顶层中间层）
//	├── midA（中间层）
//	│   ├── alpha（成品）
//	│   └── beta（成品）
//	└── gamma（成品）
//	small（小组，内存构建路径）
func scopeFixture() *Plan {
	mid := &Node{Kind: KindIntermediate, Name: "mid", Groups: []string{"alpha", "beta", "gamma"}}
	midA := &Node{Kind: KindIntermediate, Name: "midA", Groups: []string{"alpha", "beta"}, Parent: mid}
	alpha := &Node{Kind: KindFinal, Name: "alpha", Parent: midA}
	beta := &Node{Kind: KindFinal, Name: "beta", Parent: midA}
	gamma := &Node{Kind: KindFinal, Name: "gamma", Parent: mid}
	mid.Children = []*Node{midA, gamma}
	midA.Children = []*Node{alpha, beta}
	small := &Node{Kind: KindFinal, Name: "small"}
	return &Plan{
		BaseDir:    "/var/lib/meowsub/build",
		Roots:      []*Node{mid},
		Nodes:      []*Node{mid, midA, alpha, beta, gamma},
		SmallNodes: []*Node{small},
		Closures: map[string]*Closure{
			"alpha": {Pkgs: []string{"a"}},
			"beta":  {Pkgs: []string{"b"}},
			"gamma": {Pkgs: []string{"g"}},
			"small": {Pkgs: []string{"s"}},
		},
	}
}

func namesOf(ns []*Node) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.Name
	}
	return out
}

func assertNames(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

func closureKeys(m map[string]*Closure) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestScopeKeepsTargetAndAncestors(t *testing.T) {
	sp, err := scopeFixture().Scope([]string{"alpha"})
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}
	assertNames(t, "Nodes", namesOf(sp.Nodes), []string{"mid", "midA", "alpha"})
	assertNames(t, "Roots", namesOf(sp.Roots), []string{"mid"})
	if len(sp.SmallNodes) != 0 {
		t.Errorf("无关小组不应保留: %v", namesOf(sp.SmallNodes))
	}
	mid := sp.Nodes[0]
	if len(mid.Children) != 1 || mid.Children[0].Name != "midA" {
		t.Errorf("中间层 Children 应收窄到 midA: %v", namesOf(mid.Children))
	}
	if mid.Children[0].Parent != mid {
		t.Error("保留节点的 Parent 指针不应改变")
	}
	assertNames(t, "Closures keys", closureKeys(sp.Closures), []string{"alpha"})
	if sp.BaseDir != "/var/lib/meowsub/build" {
		t.Errorf("BaseDir 应原样保留: %q", sp.BaseDir)
	}
}

func TestScopeSmallTarget(t *testing.T) {
	sp, err := scopeFixture().Scope([]string{"small"})
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}
	assertNames(t, "SmallNodes", namesOf(sp.SmallNodes), []string{"small"})
	if len(sp.Nodes) != 0 || len(sp.Roots) != 0 {
		t.Errorf("小组目标不应带树上节点: nodes=%v roots=%v", namesOf(sp.Nodes), namesOf(sp.Roots))
	}
	assertNames(t, "Closures keys", closureKeys(sp.Closures), []string{"small"})
}

func TestScopeMultipleTargetsShareAncestors(t *testing.T) {
	sp, err := scopeFixture().Scope([]string{"beta", "gamma"})
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}
	assertNames(t, "Nodes", namesOf(sp.Nodes), []string{"mid", "midA", "beta", "gamma"})
	mid := sp.Nodes[0]
	assertNames(t, "mid.Children", namesOf(mid.Children), []string{"midA", "gamma"})
	midA := sp.Nodes[1]
	assertNames(t, "midA.Children", namesOf(midA.Children), []string{"beta"})
	assertNames(t, "Closures keys", closureKeys(sp.Closures), []string{"beta", "gamma"})
}

func TestScopeUnknownGroupErrors(t *testing.T) {
	_, err := scopeFixture().Scope([]string{"nope"})
	if err == nil {
		t.Fatal("未知组应报错")
	}
	if !strings.Contains(err.Error(), `"nope"`) || !strings.Contains(err.Error(), "alpha") {
		t.Errorf("错误应含组名与可选列表: %v", err)
	}
}
