// Package planner 依据各软件组的依赖闭包，聚类生成单继承子系统树与每个节点的安装集。
//
// 算法（对应《中间子系统设计》）：
//  1. 每个软件组视为叶节点，以加权 Jaccard 相似度（AUR 包权重更高）做平均链接
//     层次凝聚聚类，得到一棵二叉树；
//  2. 自顶向下为每个内部节点求所有后代叶闭包的交集，减去祖先已装集合即为其
//     增量安装集；增量少于 MinShared 的内部节点不落盘（虚节点），其子树挂到
//     最近的可落盘祖先；
//  3. 叶节点（成品子系统）永远落盘，只装增量。
package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Closure 是一个软件组解析后的完整依赖闭包。
type Closure struct {
	Pkgs []string        // 闭包内全部包名
	Aur  map[string]bool // 包名 -> 是否来自 AUR
	// AurVersion AUR 包真名 -> 上游版本（pkgver-pkgrel）。查不到版本的
	// 包缺席；build_aur 用它与池内已收录版本比对决定是否重编。
	AurVersion map[string]string
}

// Resolver 为软件组包列表解析依赖闭包（真实实现走宿主机 paru --print）。
type Resolver interface {
	Resolve(packages []string) (*Closure, error)
}

const (
	// KindIntermediate 中间节点。
	KindIntermediate = "intermediate"
	// KindFinal 成品节点（叶）。
	KindFinal = "final"
)

// Node 是子系统树上的一个可落盘节点。
type Node struct {
	Name       string   // 成品：组名；中间：派生名
	Kind       string   // KindIntermediate / KindFinal
	Parent     *Node    // 最近的可落盘祖先；根级为 nil
	Children   []*Node  // 直接子节点
	Groups     []string // 本节点子树覆盖的全部组名（排序）
	InstallSet []string // 本节点需要安装的增量包（排序）
	CommonSet  []string // 本节点子树叶闭包交集（未减继承，排序）
	Path       string   // 落盘绝对路径（由 SetPaths 填充）
}

// Options 控制聚类与建树行为。
type Options struct {
	MinShared   int     // 中间节点最小增量包数，低于则不落盘；默认 2
	AurWeight   float64 // AUR 包在相似度中的权重；默认 2
	MergeThresh float64 // 合并相似度下限，低于则停止合并；默认 0（合到根）
	// Sizes 包名 -> 安装后字节（来自 pacman -Si）。与 TreeSizeFloor 同给才启用分流。
	Sizes map[string]int64
	// TreeSizeFloor 软件组参与树聚类的空间阈值：完整闭包（含基础包）的安装
	// 尺寸和须严格大于该值；否则该组归入内存构建路径（SmallNodes）。
	// 0 = 不分流，全部组进树（旧行为）。
	TreeSizeFloor int64
}

// Plan 是一次完整编排的结果。
type Plan struct {
	BaseDir string
	BasePkg []string // 初始系统显式包集合（用于从闭包中扣除）
	Roots   []*Node  // 挂在初始系统下的顶层节点
	Nodes   []*Node  // 全部可落盘节点，拓扑序（父在前）
	// SmallNodes 低于空间阈值的小组成品：不进树、不走聚类，由 build 经
	// 内存构建路径落盘。字段均为根级 final 节点（Parent=nil，Path 同成品）。
	SmallNodes []*Node
	Closures   map[string]*Closure
}

// ExtraBasePackages 基础镜像的补充设施包：cn 源信任引导与 AUR 前端。
// 依赖 [[repositories]] 配置了 archlinuxcn 后随 pacstrap 一并装入。
var ExtraBasePackages = []string{"archlinuxcn-keyring", "paru"}

// DefaultBasePackages 初始系统 pacstrap 的显式安装包，也是存量基础系统
// 每轮 updateBase 的补全清单（--needed 幂等）：清单新增项不做一次性
// 补装，随下一轮 build 自动传导。xdg-dbus-proxy 是 native 会话模式
// 过滤宿主 session bus 的必需基础设施。
var DefaultBasePackages = []string{"base", "base-devel", "sudo", "git", "xdg-dbus-proxy"}

// ---- 内部结构 ----

type hacTree struct {
	groupIdx int        // >=0 表示叶
	children []*hacTree // 内部节点的孩子
	members  []int      // 子树覆盖的组索引
}

func (t *hacTree) leafList() []int { return t.members }

// BuildPlan 依据配置组、各组闭包与基础包集合，构建子系统树。
// groups 必须已排序；closures 需覆盖每个组（缺失按空闭包处理）。
func BuildPlan(groups []string, closures map[string]*Closure, baseSet []string, opts Options) *Plan {
	if opts.MinShared <= 0 {
		opts.MinShared = 2
	}
	if opts.AurWeight <= 0 {
		opts.AurWeight = 2
	}

	p := &Plan{
		BasePkg:  append([]string{}, baseSet...),
		Closures: map[string]*Closure{},
	}
	for g, c := range closures {
		p.Closures[g] = c
	}
	if len(groups) == 0 {
		return p
	}
	get := func(g string) *Closure {
		if c, ok := closures[g]; ok {
			return c
		}
		return &Closure{Aur: map[string]bool{}}
	}

	// ---- 0. 尺寸分流：大于阈值的组才聚类进树，其余走内存构建路径 ----
	tree := make([]string, 0, len(groups))
	smallSet := map[string]bool{}
	if opts.TreeSizeFloor > 0 && opts.Sizes != nil {
		for _, g := range groups {
			if isSmallGroup(get(g), opts.Sizes, opts.TreeSizeFloor) {
				smallSet[g] = true
			} else {
				tree = append(tree, g)
			}
		}
	} else {
		tree = append(tree, groups...)
	}
	for _, g := range groups { // 入参已排序：小组保持字典序
		if !smallSet[g] {
			continue
		}
		c := get(g)
		baseOnly := map[string]bool{}
		for _, b := range baseSet {
			baseOnly[b] = true
		}
		set := map[string]bool{}
		for _, pkg := range c.Pkgs {
			if !baseOnly[pkg] {
				set[pkg] = true
			}
		}
		p.SmallNodes = append(p.SmallNodes, &Node{
			Kind: KindFinal, Name: g, Groups: []string{g},
			InstallSet: sortedKeys(set),
		})
	}

	// ---- 1. 层次凝聚聚类（平均链接 + 加权 Jaccard）----
	n := len(tree)
	trees := make([]*hacTree, n)
	for i := range tree {
		trees[i] = &hacTree{groupIdx: i, members: []int{i}}
	}
	wj := func(a, b int) float64 {
		return weightedJaccard(get(tree[a]), get(tree[b]), opts.AurWeight)
	}
	for len(trees) > 1 {
		bestI, bestJ, bestS := -1, -1, opts.MergeThresh
		for i := 0; i < len(trees); i++ {
			for j := i + 1; j < len(trees); j++ {
				total := 0.0
				for _, x := range trees[i].members {
					for _, y := range trees[j].members {
						total += wj(x, y)
					}
				}
				s := total / float64(len(trees[i].members)*len(trees[j].members))
				if s > bestS+1e-12 {
					bestS, bestI, bestJ = s, i, j
				}
			}
		}
		if bestI < 0 {
			break // 最高相似度不足阈值，停止合并
		}
		i, j := bestI, bestJ
		trees[i] = &hacTree{
			groupIdx: -1,
			children: []*hacTree{trees[i], trees[j]},
			members:  append(append([]int{}, trees[i].members...), trees[j].members...),
		}
		trees = append(trees[:j], trees[j+1:]...)
	}

	// ---- 2. 自顶向下分配交集 ----
	base := map[string]bool{}
	for _, b := range baseSet {
		base[b] = true
	}
	leafSets := make([]map[string]bool, n)
	for i, g := range tree {
		set := map[string]bool{}
		for _, pkg := range get(g).Pkgs {
			if !base[pkg] {
				set[pkg] = true
			}
		}
		leafSets[i] = set
	}

	var nodes []*Node
	var walk func(t *hacTree, inherited map[string]bool) *Node
	walk = func(t *hacTree, inherited map[string]bool) *Node {
		common := intersectAll(leafSets, t.leafList())
		if t.groupIdx >= 0 || len(common) >= opts.MinShared {
			install := subtract(common, inherited)
			childInherited := unionSets(inherited, install)
			node := &Node{CommonSet: sortedKeys(common), InstallSet: sortedKeys(install)}
			if t.groupIdx >= 0 {
				g := tree[t.groupIdx]
				node.Kind = KindFinal
				node.Name = g
				node.Groups = []string{g}
				node.CommonSet = sortedKeys(leafSets[t.groupIdx])
			} else {
				gs := make([]string, 0, len(t.members))
				for _, l := range t.members {
					gs = append(gs, tree[l])
				}
				sort.Strings(gs)
				node.Kind = KindIntermediate
				node.Groups = gs
				node.Name = intermediateName(gs, node.InstallSet)
			}
			nodes = append(nodes, node)
			for _, c := range t.children {
				if child := walk(c, childInherited); child != nil {
					child.Parent = node
					node.Children = append(node.Children, child)
				}
			}
			return node
		}
		// 虚节点：不落盘，子树直接以当前继承集继续向下
		var first *Node
		for _, c := range t.children {
			if child := walk(c, inherited); child != nil && first == nil {
				first = child
			}
		}
		return first
	}

	for _, t := range trees {
		if root := walk(t, base); root != nil {
			p.Roots = append(p.Roots, root)
		}
	}
	p.Nodes = nodes
	return p
}

// SetPaths 按“成品在 <构建区>/<组名>、中间在 <构建区>/sub/<名>”填充路径。
// 入参为构建区根（base_dir/build，见 internal/layout），Plan.BaseDir 即该根。
func (p *Plan) SetPaths(buildRoot string) {
	p.BaseDir = buildRoot
	for _, nd := range p.Nodes {
		if nd.Kind == KindIntermediate {
			nd.Path = buildRoot + "/sub/" + nd.Name
		} else {
			nd.Path = buildRoot + "/" + nd.Name
		}
	}
	for _, nd := range p.SmallNodes {
		nd.Path = buildRoot + "/" + nd.Name
	}
}

// isSmallGroup 判定一个软件组是否走内存构建路径：闭包内至少有一个包查得到
// 安装尺寸（否则尺寸整体未知，保守按大树处理），且完整闭包（含基础包）的
// 尺寸和不超过阈值（严格大于阈值才进树）。
func isSmallGroup(c *Closure, sizes map[string]int64, floor int64) bool {
	var total int64
	known := false
	for _, p := range c.Pkgs {
		if s := sizes[p]; s > 0 {
			known = true
			total += s
		}
	}
	if !known {
		return false
	}
	return total <= floor
}

// weightedJaccard 加权 Jaccard：AUR 包计 w 权重，其余计 1。
func weightedJaccard(a, b *Closure, w float64) float64 {
	setB := map[string]bool{}
	for _, x := range b.Pkgs {
		setB[x] = true
	}
	inter, wa, wb := 0.0, 0.0, 0.0
	for _, x := range a.Pkgs {
		wt := weight(x, a, w)
		wa += wt
		if setB[x] {
			inter += wt
		}
	}
	for _, x := range b.Pkgs {
		wb += weight(x, b, w)
	}
	if wa+wb-inter == 0 {
		return 0
	}
	return inter / (wa + wb - inter)
}

func weight(pkg string, c *Closure, w float64) float64 {
	if c != nil && c.Aur != nil && c.Aur[pkg] {
		return w
	}
	return 1
}

func intersectAll(sets []map[string]bool, idx []int) map[string]bool {
	out := map[string]bool{}
	if len(idx) == 0 {
		return out
	}
	first := sets[idx[0]]
	for k := range first {
		inAll := true
		for _, i := range idx[1:] {
			if !sets[i][k] {
				inAll = false
				break
			}
		}
		if inAll {
			out[k] = true
		}
	}
	return out
}

func subtract(set, minus map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range set {
		if !minus[k] {
			out[k] = true
		}
	}
	return out
}

func unionSets(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// intermediateName 由后代组名与安装集派生稳定可读的中间子系统名：
// 组名以 + 连接（截断），后接安装集签名的 6 位十六进制散列。
func intermediateName(groups, installSet []string) string {
	base := sanitizeName(strings.Join(groups, "+"))
	if len(base) > 24 {
		base = base[:24]
	}
	h := sha256.Sum256([]byte(base + "|" + strings.Join(installSet, ",")))
	return base + "-" + hex.EncodeToString(h[:])[:6]
}

func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '+', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
