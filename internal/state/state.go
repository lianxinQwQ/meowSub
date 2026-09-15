// Package state 负责 meowSub 元数据（state.json 与各子系统内的标记文件）的持久化。
//
// 事实来源是各子系统根目录下的 .meowsub.json 标记：有标记即为本程序所生，
// 无标记的目标目录一律视为陌生目录清除重建。state.json 是聚合视图，用于
// 记录时间戳与配置哈希。
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// KindBase 初始子系统。
	KindBase = "base"
	// KindIntermediate 中间子系统。
	KindIntermediate = "intermediate"
	// KindFinal 成品子系统。
	KindFinal = "final"
	// KindBuilder 常驻编译车间（arch 的 reflink 分身，专司 AUR 构建）。
	KindBuilder = "builder"

	// MetaDirName 元数据目录名（位于 base_dir 下）。
	MetaDirName = ".meowsub"
	// StateFileName 聚合状态文件名。
	StateFileName = "state.json"
)

// MarkerFileName 子系统内标记文件名。
const MarkerFileName = ".meowsub.json"

// BaseRecord 描述初始子系统的元数据。
type BaseRecord struct {
	Path      string    `json:"path"`
	Packages  []string  `json:"packages"` // pacstrap + paru 的显式包列表
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NodeRecord 描述一个中间/成品子系统的元数据。
type NodeRecord struct {
	Name       string   `json:"name"`
	Path       string   `json:"path"` // 绝对路径
	Kind       string   `json:"kind"`
	Parent     string   `json:"parent,omitempty"` // "arch" 或中间子系统名
	Groups     []string `json:"groups,omitempty"`
	InstallSet []string `json:"install_set"`
	// 烘焙镜像（与 Marker 同义字段，供聚合视图消费）
	Users     []string      `json:"users,omitempty"`
	Services  []string      `json:"services,omitempty"`
	Export    []ExportEntry `json:"export,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

// GroupExports 记录某软件组当前被 meowSub 托管的宿主侧导出文件绝对路径。
// 每轮构建以此为「待删集合」的起点：重新生成命中即移除，剩余项清扫。
type GroupExports struct {
	Group string   `json:"group"`
	Paths []string `json:"paths"`
}

// State 是 base_dir/.meowsub/state.json 的内存表示。
type State struct {
	Version    int           `json:"version"`
	ConfigHash string        `json:"config_hash,omitempty"`
	Base       *BaseRecord   `json:"base,omitempty"`
	Nodes      []*NodeRecord `json:"nodes,omitempty"`
	// Builder 常驻编译车间记录；独立于 Nodes，避免被当成孤儿/参与调和。
	Builder *NodeRecord `json:"builder,omitempty"`
	// Exports 宿主侧导出文件归属登记（bin/desktop/icon 全集）。
	Exports []*GroupExports `json:"exports,omitempty"`
}

// ExportApp 记录包内一个桌面条目的元数据。
type ExportApp struct {
	// DesktopID usr/share/applications/<ID>.desktop 的基名（去后缀）。
	DesktopID string `json:"desktop_id"`
	// Name 原始 Desktop Entry 的人类可读名称；缺失时由导出侧回退到 DesktopID。
	Name string `json:"name,omitempty"`
	// IconRel 层内最大尺寸图标相对路径；未找到为空。
	IconRel string `json:"icon_rel,omitempty"`
}

// ExportEntry 记录构建期为某组解析出的一个导出软件包的元数据。
type ExportEntry struct {
	// Package 配置中声明的软件包名。
	Package string `json:"package"`
	// Bins 该包安装的 PATH 目录下可执行文件（容器内绝对路径）。
	Bins []string `json:"bins,omitempty"`
	// Apps 该包拥有的 .desktop 桌面条目。
	Apps []ExportApp `json:"apps,omitempty"`
}

// Marker 是写在每个子系统根目录下的标记文件内容（.meowsub.json）。
type Marker struct {
	Kind       string   `json:"kind"`
	Name       string   `json:"name"`
	Parent     string   `json:"parent,omitempty"`
	Groups     []string `json:"groups,omitempty"`
	InstallSet []string `json:"install_set,omitempty"`
	// Users 构建期镜像进层的宿主身份名。
	Users []string `json:"users,omitempty"`
	// Services 构建期 systemctl --root enable 的单元名。
	Services []string `json:"services,omitempty"`
	// Export 导出入口元数据。
	Export []ExportEntry `json:"export,omitempty"`
}

// NewEmpty 返回带版本号的空状态。
func NewEmpty() *State {
	return &State{Version: 1}
}

func metaDir(baseDir string) string { return filepath.Join(baseDir, MetaDirName) }

// Load 从 baseDir/.meowsub/state.json 读取状态；文件不存在时返回空状态而非错误。
func Load(baseDir string) (*State, error) {
	p := filepath.Join(metaDir(baseDir), StateFileName)
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return NewEmpty(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", p, err)
	}
	st := NewEmpty()
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", p, err)
	}
	return st, nil
}

// Save 将状态写回 baseDir/.meowsub/state.json。
func (s *State) Save(baseDir string) error {
	if err := os.MkdirAll(metaDir(baseDir), 0o755); err != nil {
		return fmt.Errorf("创建元数据目录: %w", err)
	}
	p := filepath.Join(metaDir(baseDir), StateFileName)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// WriteMarker 向 dir 写入标记文件。
func WriteMarker(dir string, m *Marker) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, MarkerFileName), append(data, '\n'), 0o644)
}

// ReadMarker 读取 dir 下的标记文件；不存在返回 (nil, nil)。
func ReadMarker(dir string) (*Marker, error) {
	data, err := os.ReadFile(filepath.Join(dir, MarkerFileName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("解析标记 %s/%s: %w", dir, MarkerFileName, err)
	}
	return &m, nil
}

// RemoveMarker 删除 dir 下的标记文件（重建前清理，避免残留误判）。
func RemoveMarker(dir string) error {
	err := os.Remove(filepath.Join(dir, MarkerFileName))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
