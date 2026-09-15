package fileindex

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sysA(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "sysA")
	writeFile(t, root, "usr/bin/tool", "shared-tool-bytes")
	writeFile(t, root, "etc/pacman.conf", "[options]\n")
	return root
}

func sysB(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "sysB")
	writeFile(t, root, "usr/bin/tool", "shared-tool-bytes") // 同路径同内容
	writeFile(t, root, "etc/conf", "b-unique")
	return root
}

func TestScanSystemIndexesRegularFiles(t *testing.T) {
	ix := New()
	a := sysA(t)
	if err := ix.ScanSystem(a); err != nil {
		t.Fatalf("ScanSystem: %v", err)
	}
	e, ok := ix.entryOf(a, "usr/bin/tool")
	if !ok {
		t.Fatal("usr/bin/tool 应入索引")
	}
	if e.Size != int64(len("shared-tool-bytes")) {
		t.Errorf("size = %d", e.Size)
	}
	if _, ok := ix.entryOf(a, "no/such"); ok {
		t.Error("不存在的路径不应有索引")
	}
}

func TestCandidatesFiltersBySizeAndSorts(t *testing.T) {
	ix := New()
	a, b := sysA(t), sysB(t)
	if err := ix.ScanSystem(a); err != nil {
		t.Fatal(err)
	}
	if err := ix.ScanSystem(b); err != nil {
		t.Fatal(err)
	}
	got := ix.Candidates("usr/bin/tool", int64(len("shared-tool-bytes")))
	want := []string{a, b}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Candidates = %v, want %v（按根路径排序去歧义）", got, want)
	}
	if got := ix.Candidates("usr/bin/tool", 999999); len(got) != 0 {
		t.Errorf("尺寸不符应无候选，got %v", got)
	}
	if got := ix.Candidates("missing/path", 1); len(got) != 0 {
		t.Errorf("未知路径应无候选，got %v", got)
	}
}

func TestAbsorbOnlyTouchListedPaths(t *testing.T) {
	ix := New()
	a, b := sysA(t), sysB(t)
	_ = ix.ScanSystem(a)
	_ = ix.ScanSystem(b)
	before, _ := ix.entryOf(a, "usr/bin/tool")

	c := t.TempDir()
	writeFile(t, c, "etc/newfile", "brand-new")
	// 只声明 etc/newfile：不得影响其它系统的既有条目
	ix.Absorb(c, []string{"etc/newfile"})

	if _, ok := ix.entryOf(c, "etc/newfile"); !ok {
		t.Fatal("新文件应并入索引")
	}
	after, _ := ix.entryOf(a, "usr/bin/tool")
	if after.MtimeSec != before.MtimeSec || after.Size != before.Size {
		t.Error("Absorb 不应改动未列出的既有条目")
	}
	if got := ix.Candidates("etc/newfile", int64(9)); !reflect.DeepEqual(got, []string{c}) {
		t.Errorf("新条目候选 = %v", got)
	}
}

func TestRescanRefreshesStaleEntries(t *testing.T) {
	ix := New()
	a := sysA(t)
	_ = ix.ScanSystem(a)
	p := filepath.Join(a, "etc", "pacman.conf")
	bigger := "[options]\nSigLevel=Never\n# extra line to change size\n"
	if err := os.WriteFile(p, []byte(bigger), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, changed := ix.RefreshIfChanged(a, "etc/pacman.conf"); !changed {
		t.Fatal("磁盘已变，Refresh 应检出变化")
	}
	if got := ix.Candidates("etc/pacman.conf", int64(len(bigger))); len(got) == 0 {
		t.Error("刷新后应按新尺寸命中候选")
	}
	unchanged, changed := ix.RefreshIfChanged(a, "usr/bin/tool")
	if changed {
		t.Error("未变化的文件不应报告变化")
	}
	if unchanged.Size != int64(len("shared-tool-bytes")) {
		t.Errorf("未变化文件的条目应保留原值: %+v", unchanged)
	}
}

func TestSHA256File(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(f, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("hello"))
	got, err := SHA256File(f)
	if err != nil {
		t.Fatalf("SHA256File: %v", err)
	}
	if got != hex.EncodeToString(sum[:]) {
		t.Errorf("hash = %s", got)
	}
	if _, err := SHA256File(filepath.Join(t.TempDir(), "gone")); err == nil {
		t.Error("缺失文件应报错")
	}
}
