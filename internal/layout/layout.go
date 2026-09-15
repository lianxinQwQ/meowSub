// Package layout 定义 base_dir 下三区目录结构与全部派生路径。
//
//	base_dir/
//	├── build/            构建区：初始系统、编译车间、软件包池、中间层与成品层
//	├── master/<名>/      使用态原始副本区：deploy 时从构建区 reflink 出的基线
//	├── runs/<名>/        使用态区：脱离更新体系的可变实例
//	└── .meowsub/         聚合状态（state.json）
//
// 基线副本让重部署前能校验使用态侧是否被改动，也让构建区的删除重建
// 不再是唯一事实来源。
package layout

import "path/filepath"

const (
	// BuildDirName 构建区目录名。
	BuildDirName = "build"
	// MasterDirName 使用态原始副本区目录名。
	MasterDirName = "master"
	// RunsDirName 使用态区目录名。
	RunsDirName = "runs"
)

// Build 构建区根：<base>/build。规划器、调和器与执行器的一切树内路径
// （arch、builder、pool、sub、成品层）都以它为基准派生。
func Build(base string) string { return filepath.Join(base, BuildDirName) }

// MasterDir 使用态原始副本区根：<base>/master。
func MasterDir(base string) string { return filepath.Join(base, MasterDirName) }

// RunsDir 使用态区根：<base>/runs。
func RunsDir(base string) string { return filepath.Join(base, RunsDirName) }

// Master 名为 name 的基线副本落盘路径。
func Master(base, name string) string { return filepath.Join(MasterDir(base), name) }

// Run 名为 name 的使用态实例落盘路径。
func Run(base, name string) string { return filepath.Join(RunsDir(base), name) }
