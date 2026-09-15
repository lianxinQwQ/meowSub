// scope.go：计划的定向裁剪——build --only 时把全量期望树收窄到指定软件组
// 及其途经祖先，其余节点不进调和，产出动作即不触碰其余子系统。
//
// 裁剪只发生在调和之前：磁盘现状扫描（builder.ScanView 的孤儿检测）必须
// 始终对着全量期望树做，否则未选组的现存成品层会被误判为孤儿。
package planner

import (
	"fmt"
	"sort"
)

// Scope 返回只覆盖 groups（成品软件组名）的计划副本：保留各目标节点与
// 沿 Parent 的全部祖先中间层，Closures 仅保留目标组。节点指针与父子关系
// 原样共享（执行侧依赖 n.Parent.Path），仅 Children 与各列表按保留集收窄，
// Nodes 保持原拓扑序（父在前）。groups 内的未知名字返回错误（列出计划中
// 存在的成品组名）。
func (p *Plan) Scope(groups []string) (*Plan, error) {
	finals := map[string]*Node{}
	for _, n := range p.Nodes {
		if n.Kind == KindFinal {
			finals[n.Name] = n
		}
	}
	for _, n := range p.SmallNodes {
		finals[n.Name] = n
	}
	known := make([]string, 0, len(finals))
	for name := range finals {
		known = append(known, name)
	}
	sort.Strings(known)

	kept := map[*Node]bool{}
	var targets []string
	for _, g := range groups {
		n, ok := finals[g]
		if !ok {
			return nil, fmt.Errorf("计划中不存在成品软件组 %q（可选：%v）", g, known)
		}
		targets = append(targets, g)
		for ; n != nil; n = n.Parent {
			kept[n] = true
		}
	}

	out := &Plan{
		BaseDir:  p.BaseDir,
		BasePkg:  p.BasePkg,
		Closures: map[string]*Closure{},
	}
	for _, g := range targets {
		if c, ok := p.Closures[g]; ok {
			out.Closures[g] = c
		}
	}
	for _, r := range p.Roots {
		if kept[r] {
			out.Roots = append(out.Roots, r)
		}
	}
	for _, n := range p.Nodes {
		if !kept[n] {
			continue
		}
		n.Children = filterChildren(n.Children, kept)
		out.Nodes = append(out.Nodes, n)
	}
	for _, n := range p.SmallNodes {
		if kept[n] {
			out.SmallNodes = append(out.SmallNodes, n)
		}
	}
	return out, nil
}

func filterChildren(children []*Node, kept map[*Node]bool) []*Node {
	out := children[:0:0]
	for _, c := range children {
		if kept[c] {
			out = append(out, c)
		}
	}
	return out
}
