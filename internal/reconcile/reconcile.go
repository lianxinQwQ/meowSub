// Package reconcile 比较期望树与已有子系统，产出动作序列。
//
// 更新策略（写时复制层级约束）：
//
//	reflink 的写时复制是“向下分叉”的——底层 arch 滚动更新产生的新数据块
//	只属于 arch 本身，高层子系统仍共享旧块；若各层各自 -Syu，每层都会与
//	父级解离，反复更新后空间共享率持续衰减。
//
//	因此除基础系统滚动更新外，其余中间/成品子系统在每次 build 时一律删除，
//	从刚更新过的父级重新 reflink 复制并安装增量：保证每一代的共享都对着
//	相同内容建立，父级更新天然“传递”到全部高层。
package reconcile

import (
	"sort"

	"meowsub/internal/planner"
	"meowsub/internal/state"
)

// ActionType 动作类型。
type ActionType string

const (
	// ActRecreateBase 初始系统不存在或无标记：删除旧目录并全新 pacstrap。
	ActRecreateBase ActionType = "recreate_base"
	// ActUpdateBase 初始系统已存在且有标记：滚动更新（唯一保留的就地更新，
	// 它是整棵树重建的源头）。
	ActUpdateBase ActionType = "update_base"
	// ActRemoveStale 目标位置存在无标记的陌生目录，先清除。
	ActRemoveStale ActionType = "remove_stale"
	// ActCreateNode 新建中间/成品子系统（reflink 复制 + 安装增量）。
	ActCreateNode ActionType = "create_node"
	// ActRecreateNode 已有子系统随基础系统更新整体重建：
	// 删除 → 从最新父级 reflink 复制 → 安装增量。
	ActRecreateNode ActionType = "recreate_node"
	// ActPruneNode 孤儿子系统，--prune 时删除。
	ActPruneNode ActionType = "prune_node"
	// ActOrphan 孤儿子系统，未开 --prune 时仅警告。
	ActOrphan ActionType = "orphan"

	// ---- 内存构建路径（低于空间阈值的小组）----
	// ActDeleteIntermediates 本计划生成的全部中间层在成品收割完毕后统一删除
	// （中间层只是构建期脚手架；下轮需要时自动重建）。
	ActDeleteIntermediates ActionType = "delete_intermediates"
	// ActBuildFinalSmall 小组从未构建为成品：基础系统整份复制进内存临时区，
	// 装齐目标集后经目录树索引去重落盘为新成品。
	ActBuildFinalSmall ActionType = "build_final_small"
	// ActUpdateFinalSmall 小组已有成品：成品整份复制进内存临时区做就地同步
	//（装缺删余），差异经去重落盘回原目录。
	ActUpdateFinalSmall ActionType = "update_final_small"

	// ---- 预备层（准备与装配分离）----
	// ActSyncPool 宿主机把全部闭包并集一次性下载进本地软件包池，不安装。
	ActSyncPool ActionType = "sync_pool"
	// ActEnsureBuilder 常驻编译车间：arch 的 reflink 分身，专司 AUR 构建。
	ActEnsureBuilder ActionType = "ensure_builder"
	// ActBuildAur 在车间内预构建全部 AUR 目标入池。
	ActBuildAur ActionType = "build_aur"
)

// Action 是一个待执行动作。
type Action struct {
	Type   ActionType
	Name   string            // base 或节点名
	Path   string            // 涉及的绝对路径
	Node   *planner.Node     // 期望侧节点（可为 nil）
	Record *state.NodeRecord // 已有侧记录（可为 nil）
	Reason string
	// Pkgs 预备层专用清单：sync_pool 为全部闭包并集；build_aur 为 AUR 目标集。
	Pkgs []string
	// Versions build_aur 专用：AUR 真名 -> 解析期取得的上游版本。
	// 缺席表示版本未知，执行侧按“需要构建”处理。
	Versions map[string]string
	// Paths 批量路径专用清单：delete_intermediates 携带本计划生成的全部中间层。
	Paths []string
}

// FsView 是 builder 对磁盘现状的扫描结果，保持本包纯函数化以便测试。
type FsView struct {
	BaseExists bool                         // base_dir/arch 目录存在
	BaseMarked bool                         // 且带本程序标记
	UnmarkedAt map[string]string            // 期望路径 -> 无标记目录的说明
	Existing   map[string]*state.NodeRecord // 名字 -> 磁盘标记解析出的记录
	Small      map[string]*state.NodeRecord // 小组成品名 -> 磁盘标记解析出的记录
	Orphans    []*state.NodeRecord          // 盘上有标记但期望树不再引用的节点
}

// Options 调和选项。
type Options struct {
	Prune bool // 删除孤儿子系统
	// ForceReinstall 强制重装（build --force-reinstall）：小组成品跳过
	// 滚更判定，一律从基础 arch 整体重装。
	ForceReinstall bool
}

// Reconcile 输出按执行顺序排列的动作列表：基础系统最先，随后按拓扑序处理
// 各节点（已有的一律重建、缺失才新建），孤儿放最后。
func Reconcile(plan *planner.Plan, view *FsView, opts Options) []*Action {
	var acts []*Action

	// ---- 1. 基础系统 ----
	basePath := plan.BaseDir + "/arch"
	switch {
	case !view.BaseExists:
		acts = append(acts, &Action{Type: ActRecreateBase, Name: "arch", Path: basePath,
			Reason: "初始子系统不存在"})
	case !view.BaseMarked:
		acts = append(acts, &Action{Type: ActRecreateBase, Name: "arch", Path: basePath,
			Reason: "初始子系统存在但无 meowSub 标记，将删除重建"})
	default:
		acts = append(acts, &Action{Type: ActUpdateBase, Name: "arch", Path: basePath})
	}

	// ---- 1.5 预备层：准备与装配分离 ----
	// 包池同步只收官方仓库包（pacman -Sw 不认 AUR 名）；AUR 目标归车间预构建。
	poolPath := plan.BaseDir + "/pool"
	var union []string
	unionSet := map[string]bool{}
	hasAur := false
	for _, cl := range plan.Closures {
		if len(cl.Aur) > 0 {
			hasAur = true
		}
		for _, p := range cl.Pkgs {
			if cl.Aur[p] {
				continue // AUR 包由 build_aur 在编译车间产出，宿主机不下载
			}
			if !unionSet[p] {
				unionSet[p] = true
				union = append(union, p)
			}
		}
	}
	sort.Strings(union)
	acts = append(acts, &Action{Type: ActSyncPool, Name: "pool", Path: poolPath,
		Pkgs: union, Reason: "全部仓库包一次性下载进池；节点装配阶段零联网"})
	if hasAur {
		builderPath := plan.BaseDir + "/builder"
		acts = append(acts, &Action{Type: ActEnsureBuilder, Name: "builder", Path: builderPath,
			Reason: "常驻编译车间：补齐 makedeps、驱动 makepkg、产物经 PKGDEST 入池"})
		var aurTargets []string
		for _, cl := range plan.Closures {
			for name := range cl.Aur {
				if !unionSet["\x00aur"+name] { // 复用集合做跨组去重
					unionSet["\x00aur"+name] = true
					aurTargets = append(aurTargets, name)
				}
			}
		}
		aurVersions := map[string]string{}
		for _, cl := range plan.Closures {
			for name, ver := range cl.AurVersion {
				if ver != "" && aurVersions[name] == "" {
					aurVersions[name] = ver
				}
			}
		}
		sort.Strings(aurTargets)
		acts = append(acts, &Action{Type: ActBuildAur, Name: "aur", Path: poolPath,
			Versions: aurVersions,
			Pkgs:     aurTargets, Reason: "在车间内预构建入池；已收录且版本一致的跳过，仅按需重编"})
	}

	// ---- 2. 期望节点（拓扑序）：已有即整体重建，缺失才新建 ----
	const rebuildReason = "写时复制不透传底层更新：随基础系统滚动更新整体重建以恢复 reflink 共享"
	for _, n := range plan.Nodes {
		if _, dirty := view.UnmarkedAt[n.Path]; dirty {
			acts = append(acts, &Action{Type: ActRemoveStale, Name: n.Name, Path: n.Path,
				Reason: "目标位置存在无标记的陌生目录，视为不存在"})
		}
		if rec := view.Existing[n.Name]; rec != nil {
			acts = append(acts, &Action{Type: ActRecreateNode, Name: n.Name, Path: n.Path,
				Node: n, Record: rec, Reason: rebuildReason})
			continue
		}
		acts = append(acts, &Action{Type: ActCreateNode, Name: n.Name, Path: n.Path, Node: n})
	}

	// ---- 2.5 中间层收割即弃：成品全部落定后删除本计划生成的全部中间层 ----
	var interPaths []string
	for _, n := range plan.Nodes {
		if n.Kind == planner.KindIntermediate {
			interPaths = append(interPaths, n.Path)
		}
	}
	if len(interPaths) > 0 {
		sort.Strings(interPaths)
		acts = append(acts, &Action{Type: ActDeleteIntermediates, Name: "sub",
			Path: plan.BaseDir + "/sub", Paths: interPaths,
			Reason: "中间层只是构建期脚手架：成品收割完毕统一删除，下轮按需重建"})
	}

	// ---- 2.6 小组（低于阈值走内存构建路径，名字序逐个处理）----
	// 三态：无记录=全新构建；InstallSet 与规划相等（仅版本漂移）=副本内滚更；
	// 集合不等=从基础 arch 整体重建。不做任何包级增量增删。
	smallSyncReason := "包集合无差异仅版本漂移：副本内 pacman -Syu 滚更，差异去重落盘"
	smallRebuildReason := "包集合有差异：从基础 arch 整体重装，差异经去重落盘"
	forceReinstallReason := "强制重装：跳过滚更判定，从基础 arch 整体重装，差异经去重落盘"
	for _, n := range plan.SmallNodes {
		if _, dirty := view.UnmarkedAt[n.Path]; dirty {
			acts = append(acts, &Action{Type: ActRemoveStale, Name: n.Name, Path: n.Path,
				Reason: "目标位置存在无标记的陌生目录，视为不存在"})
		}
		if rec := view.Small[n.Name]; rec != nil {
			if !opts.ForceReinstall && equalStrings(rec.InstallSet, n.InstallSet) {
				acts = append(acts, &Action{Type: ActUpdateFinalSmall, Name: n.Name,
					Path: n.Path, Node: n, Record: rec, Reason: smallSyncReason})
				continue
			}
			reason := smallRebuildReason
			if opts.ForceReinstall {
				reason = forceReinstallReason
			}
			acts = append(acts, &Action{Type: ActBuildFinalSmall, Name: n.Name,
				Path: n.Path, Node: n, Record: rec, Reason: reason})
			continue
		}
		acts = append(acts, &Action{Type: ActBuildFinalSmall, Name: n.Name, Path: n.Path,
			Node: n, Reason: "从未构建：基础系统复制进内存装齐后经目录树索引去重落盘"})
	}

	// ---- 3. 孤儿 ----
	orphans := append([]*state.NodeRecord{}, view.Orphans...)
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].Name < orphans[j].Name })
	for _, o := range orphans {
		if opts.Prune {
			acts = append(acts, &Action{Type: ActPruneNode, Name: o.Name, Path: o.Path,
				Record: o, Reason: "期望树不再引用（--prune 生效）"})
		} else {
			acts = append(acts, &Action{Type: ActOrphan, Name: o.Name, Path: o.Path,
				Record: o, Reason: "期望树不再引用；使用 --prune 删除"})
		}
	}
	return acts
}

// equalStrings 逐项比较两个有序切片（InstallSet 构造时即排序）。
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
