package staging

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meowsub/internal/execx"
)

func writeTree(t *testing.T, root string, files map[string]int64) {
	t.Helper()
	for rel, size := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEstimateBytesSumsLogicalSize(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sys")
	writeTree(t, root, map[string]int64{
		"usr/bin/a":      100,
		"usr/lib/b.so":   250,
		"etc/conf/c.cfg": 50,
	})
	got, err := EstimateBytes(root)
	if err != nil {
		t.Fatalf("EstimateBytes: %v", err)
	}
	if got != 400 {
		t.Errorf("EstimateBytes = %d, want 400", got)
	}
}

func TestTmpfsSizeFormula(t *testing.T) {
	cases := []struct {
		logical, extra int64
		want           string
	}{
		{0, 0, "4G"},               // 下限 4GiB
		{1000, 0, "4G"},            // 下限 4GiB
		{10 << 30, 0, "10G"},       // 复制基数 1:1 计入
		{10 << 30, 3 << 30, "13G"}, // 基数 + 安装增量
		{20 << 30, 1 << 30, "21G"},
	}
	for _, c := range cases {
		if got := TmpfsSize(c.logical, c.extra); got != c.want {
			t.Errorf("TmpfsSize(%d, %d) = %q, want %q", c.logical, c.extra, got, c.want)
		}
	}
}

func newTestStager(t *testing.T) (*Stager, *execx.RecordRunner, string) {
	t.Helper()
	r := &execx.RecordRunner{}
	base := t.TempDir()
	s := &Stager{Runner: r, Base: base}
	return s, r, base
}

func TestStageHappyPathCommands(t *testing.T) {
	s, r, base := newTestStager(t)
	src := t.TempDir()
	writeTree(t, src, map[string]int64{"usr/bin/tool": 1024})

	mem, cleanup, err := s.Stage("go", src, 0)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer cleanup()

	wantMem := filepath.Join(base, "full-build-go")
	if mem != wantMem {
		t.Errorf("mem = %q, want %q", mem, wantMem)
	}
	mount := findCmd(t, r.Cmds, "mount", "-t", "tmpfs")
	if mount == nil {
		t.Fatal("应挂载 tmpfs")
	}
	// 极小的树触底 4GiB 下限：容量写法应确定
	if !strings.Contains(strings.Join(mount.Args, " "), "size=4G") {
		t.Errorf("mount 参数应含 size=4G：%v", mount.Args)
	}
	cp := findCmd(t, r.Cmds, "cp", "--reflink=auto")
	if cp == nil {
		t.Fatal("应用 reflink=auto 整份复制")
	}
	joined := strings.Join(cp.Args, " ")
	if !strings.HasPrefix(joined, "-a ") || !strings.HasSuffix(joined, src+"/. "+wantMem+"/") {
		t.Errorf("cp 参数不对：%v（期望 %s/. -> %s/）", cp.Args, src, wantMem)
	}

	cleanup() // 幂等：第二次无副作用
	n := len(r.Cmds)
	cleanup()
	if len(r.Cmds) != n {
		t.Errorf("cleanup 应幂等，命令数 %d -> %d", n, len(r.Cmds))
	}
	var umounted bool
	for _, c := range r.Cmds {
		if c.Name == "umount" && len(c.Args) > 0 && c.Args[0] == wantMem {
			umounted = true
		}
	}
	if !umounted {
		t.Error("cleanup 应卸载 tmpfs")
	}
}

func TestStageMountFailureCleansUpAndErrors(t *testing.T) {
	s, _, base := newTestStager(t)
	s.Runner = &execx.RecordRunner{FailOn: map[string]error{"mount": errors.New("boom")}}
	src := t.TempDir()
	_, cleanup, err := s.Stage("go", src, 0)
	cleanup()
	if err == nil {
		t.Fatal("mount 失败应返回错误")
	}
	if _, serr := os.Stat(base); serr == nil {
		if ents, _ := os.ReadDir(base); len(ents) > 0 {
			t.Errorf("失败后工作目录应清空，残留 %d 项", len(ents))
		}
	}
}

func TestStageCopyFailureUmountsAndErrors(t *testing.T) {
	s, r, base := newTestStager(t)
	s.Runner.(*execx.RecordRunner).FailOn = map[string]error{"cp": errors.New("EXDEV")}
	_, cleanup, err := s.Stage("go", t.TempDir(), 0)
	cleanup()
	if err == nil {
		t.Fatal("cp 失败应返回错误")
	}
	var umounted bool
	for _, c := range r.Cmds {
		if c.Name == "umount" {
			umounted = true
		}
	}
	if !umounted {
		t.Error("复制失败也应卸载 tmpfs")
	}
	if ents, _ := os.ReadDir(base); len(ents) > 0 {
		t.Errorf("失败后工作目录应清空，残留 %d 项", len(ents))
	}
}

// MinSize 只放大不收窄：配置下限顶破自动估算时按配置挂载。
func TestStageHonorsMinSize(t *testing.T) {
	s, r, _ := newTestStager(t)
	s.MinSize = 32 << 30
	src := t.TempDir()
	writeTree(t, src, map[string]int64{"usr/bin/tool": 8})

	if _, cleanup, err := s.Stage("go", src, 0); err != nil {
		t.Fatalf("Stage: %v", err)
	} else {
		defer cleanup()
	}
	mount := findCmd(t, r.Cmds, "mount", "-t", "tmpfs")
	if mount == nil {
		t.Fatal("应挂载 tmpfs")
	}
	if !strings.Contains(strings.Join(mount.Args, " "), "size=32G") {
		t.Errorf("MinSize 应放大挂载容量：%v", mount.Args)
	}
}

// checkFree：零增量跳过；小增量任何宿主文件系统都放得下；天文数字增量
// 应报"余量不足"并提示调大上限。
func TestCheckFreeBlocksWhenShort(t *testing.T) {
	dir := t.TempDir()
	if err := checkFree(dir, 0); err != nil {
		t.Errorf("零增量不应检查: %v", err)
	}
	if err := checkFree(dir, 1<<20); err != nil {
		t.Errorf("1MiB 增量不应报错: %v", err)
	}
	err := checkFree(dir, 1<<50)
	if err == nil {
		t.Fatal("增量远超剩余空间应报错")
	}
	if !strings.Contains(err.Error(), "tmpfs 余量不足") ||
		!strings.Contains(err.Error(), "mem_tmpfs_size") {
		t.Errorf("报错应说明余量并提示调大上限: %v", err)
	}
}

// findCmd 在记录的命令里找名字与参数子串都匹配的第一条。
func findCmd(t *testing.T, cmds []*execx.RecordedCmd, name string, substrings ...string) *execx.RecordedCmd {
	t.Helper()
	for _, c := range cmds {
		if c.Name != name {
			continue
		}
		ok := true
		for _, sub := range substrings {
			found := false
			for _, a := range c.Args {
				if strings.Contains(a, sub) {
					found = true
					break
				}
			}
			if !found {
				ok = false
				break
			}
		}
		if ok {
			return c
		}
	}
	return nil
}
