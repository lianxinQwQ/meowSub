// landing_dirs_test.go：目录落盘试验——用真实 landFromMemory / verifyLanding
// 对含空目录的迷你系统树做增量与全新两种落盘，验证目录是否参与事务。
// 正确行为：内存树里的目录（含空目录）应原样落盘（含权限位）；盘上已随
// 事务变空、内存侧又消失的目录应被剪除；--verify 应双向核对目录存在性。
package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meowsub/internal/execx"
	"meowsub/internal/fileindex"
	"meowsub/internal/reconcile"
	"meowsub/internal/staging"
	"meowsub/internal/state"
)

func putDir(t *testing.T, root, rel string, perm os.FileMode) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, perm); err != nil {
		t.Fatal(err)
	}
}

// wantDir 核对目录存在性与权限位；kind 期望 "dir"/"absent"，perm 为 0 表示不校验权限。
func wantDir(t *testing.T, root, rel, kind string, perm os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(filepath.Join(root, rel))
	if kind == "absent" {
		if err == nil {
			t.Errorf("落盘后 %-24s 期望不存在，实际仍在（%v）", rel, fi.Mode())
		}
		return
	}
	if err != nil || !fi.IsDir() {
		t.Errorf("落盘后 %-24s 期望目录，实际 %v", rel, err)
		return
	}
	if perm != 0 && fi.Mode().Perm() != perm {
		t.Errorf("落盘后 %-24s 权限 %v，期望 %v", rel, fi.Mode().Perm(), perm)
	}
}

// TestLandFromMemoryFreshEmptyDirs 全新落盘：内存树里的空目录（含嵌套与
// 特殊权限位）必须原样落地，不得只在有文件时才顺带建父目录。
func TestLandFromMemoryFreshEmptyDirs(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	mem := filepath.Join(base, "mem")
	putFile(t, mem, "usr/bin/tool", "x")
	putDir(t, mem, "srv/empty", 0o755)        // 双层皆空
	putDir(t, mem, "var/cache/pacman", 0o755) // 中段目录仅作前缀
	putDir(t, mem, "etc/templates", 0o700)    // 权限位需保真

	b := New(&execx.RecordRunner{}, os.Stderr, base)
	if _, err := b.landFromMemory(mem, "", dest, fileindex.New()); err != nil {
		t.Fatalf("landFromMemory: %v", err)
	}
	wantDir(t, dest, "srv", "dir", 0o755)
	wantDir(t, dest, "srv/empty", "dir", 0o755)
	wantDir(t, dest, "var/cache/pacman", "dir", 0o755)
	wantDir(t, dest, "etc/templates", "dir", 0o700)
	want(t, dest, "usr/bin/tool", "reg", "")
}

// TestLandFromMemoryIncrementalEmptyDirs 增量落盘：新增空目录落地；盘上已
// 变空且内存侧消失的目录剪除；两侧都在的目录原样保留。
func TestLandFromMemoryIncrementalEmptyDirs(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	mem := filepath.Join(base, "mem")

	// 盘上旧一代：oldtool 将被移除；srv/gone 整目录随事务消失；srv/stay 保留
	putFile(t, dest, "usr/bin/oldtool", "old")
	putFile(t, dest, "srv/gone/f.txt", "gone")
	putFile(t, dest, "srv/stay/f.txt", "same")
	putDir(t, dest, "var/lib/keeper", 0o755)

	// 内存新一代
	putFile(t, mem, "usr/bin/newtool", "new")
	putFile(t, mem, "srv/stay/f.txt", "same")
	putDir(t, mem, "var/lib/keeper", 0o755) // 未变化的空目录
	putDir(t, mem, "opt/newdir", 0o755)     // 新增空目录

	b := New(&execx.RecordRunner{}, os.Stderr, base)
	if _, err := b.landFromMemory(mem, dest, dest, fileindex.New()); err != nil {
		t.Fatalf("landFromMemory: %v", err)
	}
	want(t, dest, "usr/bin/oldtool", "absent", "")
	want(t, dest, "usr/bin/newtool", "reg", "")
	wantDir(t, dest, "opt/newdir", "dir", 0o755)     // 新增空目录应落地
	wantDir(t, dest, "var/lib/keeper", "dir", 0o755) // 未变目录应保留
	wantDir(t, dest, "srv/gone", "absent", 0)        // 已空的消失目录应剪除
	wantDir(t, dest, "srv/stay", "dir", 0o755)       // 有内容的目录不动
}

// TestVerifyLandingCoversDirs 目录参与全树比对：实体缺目录、多目录都要点名。
func TestVerifyLandingCoversDirs(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	mem := filepath.Join(base, "mem")
	putFile(t, mem, "usr/bin/tool", "x")
	putDir(t, mem, "srv/templates", 0o755)
	putFile(t, dest, "usr/bin/tool", "x")

	b := New(&execx.RecordRunner{}, os.Stderr, base)
	err := b.verifyLanding(mem, dest)
	if err == nil || !strings.Contains(err.Error(), "srv/templates") {
		t.Fatalf("实体缺目录应判不一致并点名 srv/templates：%v", err)
	}

	if err := os.MkdirAll(filepath.Join(dest, "srv/templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := b.verifyLanding(mem, dest); err != nil {
		t.Fatalf("补齐目录后应判一致: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dest, "srv/extra"), 0o755); err != nil {
		t.Fatal(err)
	}
	err = b.verifyLanding(mem, dest)
	if err == nil || !strings.Contains(err.Error(), "srv/extra") {
		t.Fatalf("实体多出目录应判不一致并点名 srv/extra：%v", err)
	}
}

// TestBuildFinalSmallLandsEmptyDirs 小组成品内存构建端到端：pacman 装包
// 自带的空目录（含权限位）必须落进成品层，不得静默丢弃。
func TestBuildFinalSmallLandsEmptyDirs(t *testing.T) {
	base := t.TempDir()
	stageBase := t.TempDir()
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	b.stage = &staging.Stager{Runner: r, Base: stageBase}
	writeFileT(t, filepath.Join(base, "arch", "usr", "bin", "tool"), "arch-tool-content")
	writeFileT(t, filepath.Join(base, "arch", "etc", "pacman.conf"), "[options]\nHoldPkg=base\n")

	small := smallNode(base)
	acts := []*reconcile.Action{
		{Type: reconcile.ActBuildFinalSmall, Name: small.Name, Path: small.Path, Node: small},
	}
	b.afterStage = func(mem string) {
		// pacman 装完包后的内存树形态：常规文件 + 包自带空目录
		writeFileT(t, filepath.Join(mem, "usr", "bin", "tool"), "arch-tool-content")
		writeFileT(t, filepath.Join(mem, "usr", "bin", "gonly"), "go-exclusive-bytes")
		putDir(t, mem, "srv/empty", 0o755)
		putDir(t, mem, "var/lib/privileged", 0o700)
	}
	if _, err := b.Execute(acts, state.NewEmpty(), "hash"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	wantDir(t, filepath.Join(base, "go"), "srv/empty", "dir", 0o755)
	wantDir(t, filepath.Join(base, "go"), "var/lib/privileged", "dir", 0o700)
}
