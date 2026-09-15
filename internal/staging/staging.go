// Package staging 为"内存构建"准备 tmpfs 工作区：把一个系统根目录整份复制
// 进按需挂载的内存盘，容器在其副本上做包装/卸包，宿主盘在落盘前零写入。
//
// 只有这一条路（整体复制进 tmpfs），不走 overlay 写分离——此前实践已验证
// overlay 路线在本场景不可靠。tmpfs 的 size= 是按写入逐页占用的上限而非
// 预分配：调大没有代价，估小了容器才会在装包中途 ENOSPC（实例事故：4G
// 上限只装下了基础树副本，pacman 随后约 3G 的安装增量直接撞死空间检查）。
package staging

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"meowsub/internal/execx"
)

const (
	// DefaultBase 内存工作区的宿主目录。
	DefaultBase = "/run/meowsub"
	// defaultTmpfsFloor tmpfs 容量下限：过小的组也保证升级余量。
	defaultTmpfsFloor = int64(4) << 30
)

// Stager 创建并回收内存工作区。Runner 可注入（测试用记录式执行器）；
// Base 默认 /run/meowsub，测试覆盖为临时目录。
//
// MinSize 为容量下限（字节，配置 mem_tmpfs_size / 命令行 --mem-tmpfs-size）：
// 实际挂载取 max(自动估算, MinSize)，只放大不收窄——配置是放宽上限的旋钮，
// 不是掐死估算的闸（tmpfs 按需占页，放宽没有代价；收窄只会复现 ENOSPC）。
// SkipFreeCheck 为测试缝隙：RecordRunner 下挂载是假的，对真实文件系统做
// 余量探测与假象不符，测试用它关掉 checkFree。
type Stager struct {
	Runner  execx.Runner
	Base    string
	MinSize int64

	SkipFreeCheck bool
}

func (s *Stager) runner() execx.Runner {
	if s.Runner == nil {
		return execx.ExecRunner{}
	}
	return s.Runner
}

func (s *Stager) base() string {
	if s.Base == "" {
		return DefaultBase
	}
	return s.Base
}

// EstimateBytes 遍历 root 统计常规文件的逻辑字节总量（symlink 不计）。
func EstimateBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if fi, ferr := d.Info(); ferr == nil && fi.Mode().IsRegular() {
			total += fi.Size()
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// TmpfsSize 由复制基数与安装增量估算 tmpfs 容量（GiB 取整写法）。
// tmpfs 上没有 reflink：跨设备复制必然退化为整拷，进来的每个文件都按
// 整页独立占位，故复制基数按 1:1 计入、不做折算；extra 为复制完成后
// 容器预期写入的字节增量（pacman 装包 + 下载缓存）。下限 4GiB。
func TmpfsSize(logical, extra int64) string {
	return gibCeil(tmpfsBytes(logical, extra))
}

// tmpfsBytes 估算挂载容量（字节）：复制基数 + 安装增量，套 4GiB 下限。
func tmpfsBytes(logical, extra int64) int64 {
	est := logical + extra
	if est < defaultTmpfsFloor {
		est = defaultTmpfsFloor
	}
	return est
}

// gibCeil 字节数向上取整到 GiB 的紧凑写法。
func gibCeil(n int64) string {
	return fmt.Sprintf("%dG", (n+(1<<30)-1)>>30)
}

// Stage 把 src 系统整份复制进新建的 tmpfs 工作区 full-build-<name>
// （前缀与 runner 的使用态暂存区区分，避免撞名）。extra 为复制完成后
// 容器预期写入的字节增量（安装集等），既计入容量也在复制后实测兜底。
// 返回内存副本根路径与幂等的清理函数（卸载 + 删除）；失败时清理已创建的
// 资源，且仍返回可安全调用（空操作）的清理函数。
func (s *Stager) Stage(name, src string, extra int64) (string, func(), error) {
	run := s.runner()
	dir := filepath.Join(s.base(), "full-build-"+name)

	if err := os.MkdirAll(s.base(), 0o755); err != nil {
		return "", func() {}, fmt.Errorf("创建 %s: %w", s.base(), err)
	}
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", func() {}, err
	}
	cleanupOnce := false
	cleanup := func() {
		if cleanupOnce {
			return
		}
		cleanupOnce = true
		_ = run.Run("umount", dir)
		os.RemoveAll(dir)
	}

	logical, err := EstimateBytes(src)
	if err != nil {
		cleanup()
		return "", cleanup, fmt.Errorf("估算 %s 容量: %w", src, err)
	}
	est := tmpfsBytes(logical, extra)
	if s.MinSize > est {
		est = s.MinSize
	}
	size := gibCeil(est)
	if err := run.Run("mount", "-t", "tmpfs", "-o", "size="+size,
		"meowsub-full-"+name, dir); err != nil {
		cleanup()
		return "", cleanup, fmt.Errorf("tmpfs 挂载失败（预估需 %s）: %w", size, err)
	}
	// 跨设备（btrfs→tmpfs）reflink 物理不可行：EXDEV 属预期；auto 让
	// 复制走快速路径、落入内存后成为独立页——这正是“整系统进内存”的本意。
	if err := run.Run("cp", "-a", "--reflink=auto", src+"/.", dir+"/"); err != nil {
		cleanup()
		return "", cleanup, fmt.Errorf("系统镜像到 tmpfs 失败: %w", err)
	}
	// 估算失准时（文件膨胀、hardlink 摊平、增量漏算）在此提前暴露，
	// 而不是等 pacman 解包到一半才 ENOSPC。
	if !s.SkipFreeCheck {
		if err := checkFree(dir, extra); err != nil {
			cleanup()
			return "", cleanup, err
		}
	}
	return dir, cleanup, nil
}

// checkFree 实测 dir 所在文件系统的剩余空间，覆盖不了安装增量（留 10%
// 容差：估算只求“够用”，边际误差交给取整余量）即报错。探测本身失败
// 不阻塞：余量校验只是兜底，后续事务自会证伪。
func checkFree(dir string, extra int64) error {
	if extra <= 0 {
		return nil
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return nil
	}
	free := int64(st.Bavail) * st.Bsize
	need := extra - extra/10
	if free < need {
		return fmt.Errorf("tmpfs 余量不足：安装增量约需 %s，实测仅剩 %dG；"+
			"调大 mem_tmpfs_size（或 --mem-tmpfs-size）后重试",
			gibCeil(need), free>>30)
	}
	return nil
}
