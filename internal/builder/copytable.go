// copytable.go 配置复制表：构建期把宿主文件/目录 reflink 进成品子系统。
//
// 来源在配置解析期已折算为宿主绝对路径（相对路径按配置文件所在目录解析）；
// 目标为子系统内绝对路径。施加时先清除目标旧内容再整份复制（文件与目录
// 统一 cp -a --reflink=always），保证每轮构建结果确定：成品每轮删旧重建、
// 小组滚更路径同样重新施加，来源变化随下一轮 build 自动传导。
package builder

import (
	"fmt"
	"os"
	"path/filepath"

	"meowsub/internal/config"
)

// ValidateCopySources 构建前置校验：全部组的复制来源必须存在，缺失即
// 报错（配置即契约，报错时磁盘未动）。来源已是绝对路径，仅做存在性
// 检查；来源为符号链接时按链接目标解析存在性（stat 语义）。
func ValidateCopySources(cfg *config.Config) error {
	for _, g := range cfg.Groups {
		for _, c := range g.Copies {
			if _, err := os.Stat(c.Source); err != nil {
				return fmt.Errorf("软件组 [%s] 的复制来源不存在 %s: %w",
					g.Name, c.Source, err)
			}
		}
	}
	return nil
}

// applyCopyTable 对成品子系统 root 施加组复制表。目标旧内容一律先清除
// （复制表对目标位置拥有最终决定权，覆盖包管理器安装的默认配置），深层
// 目标目录自动建立。
func (b *Builder) applyCopyTable(root string, copies []config.Copy) error {
	for _, c := range copies {
		// 动手前确认来源在：否则先删目标再复制失败会两头落空。
		if _, err := os.Stat(c.Source); err != nil {
			return fmt.Errorf("复制来源不可读 %s: %w", c.Source, err)
		}
		dst := filepath.Join(root, filepath.Clean(c.Dest))
		if err := os.RemoveAll(dst); err != nil {
			return fmt.Errorf("清除复制目标 %s: %w", dst, err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("建立复制目标目录 %s: %w", filepath.Dir(dst), err)
		}
		fmt.Fprintf(b.Out, "[copy] %s → %s\n", c.Source, c.Dest)
		if err := b.Runner.Run("cp", "-a", "--reflink=always", c.Source, dst); err != nil {
			return fmt.Errorf("复制 %s → %s（强制 reflink）: %w", c.Source, c.Dest, err)
		}
	}
	return nil
}
