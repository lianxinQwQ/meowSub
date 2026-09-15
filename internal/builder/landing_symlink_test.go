// landing_symlink_test.go：伪落盘试验——用真实 landFromMemory 对含符号链接的
// 迷你系统树做增量与全新两种落盘，验证符号链接是否存活。
// 正确行为：tmpfs 内的符号链接应当原样落盘；盘上普通文件若在暂存侧变成
// 符号链接，应当以符号链接形态更新而非被删除。
package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meowsub/internal/execx"
	"meowsub/internal/fileindex"
)

func putFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func putLink(t *testing.T, root, rel, target string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

// want 逐一核对落盘结果：kind 期望 "reg"/"symlink"/"absent"。
func want(t *testing.T, dest, rel, kind, linkTarget string) {
	t.Helper()
	p := filepath.Join(dest, rel)
	fi, err := os.Lstat(p)
	got := "absent"
	actualTarget := ""
	if err == nil {
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			got = "symlink"
			actualTarget, _ = os.Readlink(p)
		case fi.Mode().IsRegular():
			got = "reg"
		default:
			got = "other"
		}
	}
	if got != kind || (kind == "symlink" && linkTarget != "" && actualTarget != linkTarget) {
		t.Errorf("落盘后 %-28s 期望 %-8s %s 实际 %-8s %s", rel, kind, linkTarget, got, actualTarget)
	}
}

// 安装后的暂存树（等价 tmpfs 内 pacman 装完的样子）：
//   - libdemo.so.2.0.0 内容 v1→v2（常规更新）
//   - libdemo.so.2 链接保持不变
//   - oldtool 由普通文件变成链接（上游打包常规操作）
//   - 新装包 libnew：.1 是链接、.1.0.0 是实体、realtool 是实体
func stageInstalled(t *testing.T, mem string) {
	t.Helper()
	putFile(t, mem, "usr/lib/libdemo.so.2.0.0", "v2")
	putLink(t, mem, "usr/lib/libdemo.so.2", "libdemo.so.2.0.0")
	putLink(t, mem, "usr/bin/oldtool", "realtool")
	putLink(t, mem, "usr/lib/libnew.so.1", "libnew.so.1.0.0")
	putFile(t, mem, "usr/lib/libnew.so.1.0.0", "new")
	putFile(t, mem, "usr/bin/realtool", "real")
}

// TestLandFromMemoryIncrementalSymlinks 增量落盘：盘上成品作 lower。
func TestLandFromMemoryIncrementalSymlinks(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	mem := filepath.Join(base, "mem")

	// 盘上成品（旧一代）
	putFile(t, dest, "usr/lib/libdemo.so.2.0.0", "v1")
	putLink(t, dest, "usr/lib/libdemo.so.2", "libdemo.so.2.0.0")
	putFile(t, dest, "usr/bin/oldtool", "old")
	putLink(t, dest, "usr/lib/libgone.so", "nowhere") // 上游已删除的陈旧链接
	putLink(t, dest, "usr/lib/libretarget.so", "oldtarget")
	stageInstalled(t, mem)
	putLink(t, mem, "usr/lib/libretarget.so", "newtarget") // 目标漂移

	b := New(&execx.RecordRunner{}, os.Stderr, base)
	if _, err := b.landFromMemory(mem, dest, dest, fileindex.New()); err != nil {
		t.Fatalf("landFromMemory: %v", err)
	}
	want(t, dest, "usr/lib/libdemo.so.2.0.0", "reg", "")
	want(t, dest, "usr/lib/libdemo.so.2", "symlink", "libdemo.so.2.0.0")
	want(t, dest, "usr/bin/oldtool", "symlink", "realtool")
	want(t, dest, "usr/lib/libnew.so.1", "symlink", "libnew.so.1.0.0")
	want(t, dest, "usr/lib/libnew.so.1.0.0", "reg", "")
	want(t, dest, "usr/bin/realtool", "reg", "")
	want(t, dest, "usr/lib/libgone.so", "absent", "")
	want(t, dest, "usr/lib/libretarget.so", "symlink", "newtarget")
}

// TestLandFromMemoryFreshSymlinks 全新落盘：dest 为空（等价“目标位置视为
// 不存在”后的重建），一切只能来自暂存树。
func TestLandFromMemoryFreshSymlinks(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	mem := filepath.Join(base, "mem")
	stageInstalled(t, mem)
	putLink(t, mem, "usr/lib/libretarget.so", "newtarget")

	b := New(&execx.RecordRunner{}, os.Stderr, base)
	if _, err := b.landFromMemory(mem, "", dest, fileindex.New()); err != nil {
		t.Fatalf("landFromMemory: %v", err)
	}
	want(t, dest, "usr/lib/libdemo.so.2.0.0", "reg", "")
	want(t, dest, "usr/lib/libdemo.so.2", "symlink", "libdemo.so.2.0.0")
	want(t, dest, "usr/bin/oldtool", "symlink", "realtool")
	want(t, dest, "usr/lib/libnew.so.1", "symlink", "libnew.so.1.0.0")
	want(t, dest, "usr/lib/libnew.so.1.0.0", "reg", "")
	want(t, dest, "usr/bin/realtool", "reg", "")
	want(t, dest, "usr/lib/libretarget.so", "symlink", "newtarget")
}

// TestVerifyLanding 一致树应通过；篡改实体侧（改内容/删链接/多出条目）应
// 报不一致且明细点名各条目。
func TestVerifyLanding(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	mem := filepath.Join(base, "mem")
	putFile(t, mem, "usr/lib/liba.so.1.0", "abc")
	putLink(t, mem, "usr/lib/liba.so.1", "liba.so.1.0")
	putFile(t, mem, "usr/bin/tool", "bin")
	putFile(t, dest, "usr/lib/liba.so.1.0", "abc")
	putLink(t, dest, "usr/lib/liba.so.1", "liba.so.1.0")
	putFile(t, dest, "usr/bin/tool", "bin")

	b := New(&execx.RecordRunner{}, os.Stderr, base)
	if err := b.verifyLanding(mem, dest); err != nil {
		t.Fatalf("一致树误报: %v", err)
	}

	putFile(t, dest, "usr/lib/liba.so.1.0", "XYZ")
	if err := os.Remove(filepath.Join(dest, "usr/lib/liba.so.1")); err != nil {
		t.Fatal(err)
	}
	putFile(t, dest, "usr/bin/extra", "x")
	err := b.verifyLanding(mem, dest)
	if err == nil {
		t.Fatal("篡改后应检出不一致")
	}
	msg := err.Error()
	for _, s := range []string{"usr/lib/liba.so.1.0", "usr/lib/liba.so.1", "usr/bin/extra"} {
		if !strings.Contains(msg, s) {
			t.Errorf("校验明细缺 %s：\n%s", s, msg)
		}
	}
}

// TestVerifyLandingCoversGnupg 钥匙环是普通树内容：一致时通过，内容漂移
// 或实体侧多出条目应判不一致并点名。
func TestVerifyLandingCoversGnupg(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	mem := filepath.Join(base, "mem")
	putFile(t, mem, "usr/bin/tool", "v1")
	putFile(t, mem, "etc/pacman.d/gnupg/trustdb.gpg", "staged")
	putFile(t, dest, "usr/bin/tool", "v1")
	putFile(t, dest, "etc/pacman.d/gnupg/trustdb.gpg", "staged")

	b := New(&execx.RecordRunner{}, os.Stderr, base)
	if err := b.verifyLanding(mem, dest); err != nil {
		t.Fatalf("一致树误报: %v", err)
	}

	putFile(t, dest, "etc/pacman.d/gnupg/trustdb.gpg", "drifted") // 内容漂移
	putFile(t, dest, "etc/pacman.d/gnupg/S.dirmngr", "stale")     // 实体侧多出
	err := b.verifyLanding(mem, dest)
	if err == nil {
		t.Fatal("钥匙环差异应判不一致")
	}
	msg := err.Error()
	for _, s := range []string{"etc/pacman.d/gnupg/trustdb.gpg", "etc/pacman.d/gnupg/S.dirmngr"} {
		if !strings.Contains(msg, s) {
			t.Errorf("校验明细缺 %s：\n%s", s, msg)
		}
	}
}
