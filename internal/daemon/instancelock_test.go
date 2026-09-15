// instancelock_test.go：同步锁的生命周期——存活锁拒绝、解锁放行、陈锁
// （持锁进程已死）自动接管。
package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestInstanceLock(t *testing.T) {
	base := t.TempDir()

	if InstanceLocked(base, "tool") {
		t.Fatal("无锁文件不应报告锁定")
	}

	unlock, err := LockInstance(base, "tool")
	if err != nil {
		t.Fatalf("加锁: %v", err)
	}
	if !InstanceLocked(base, "tool") {
		t.Fatal("加锁后应报告锁定")
	}
	if _, err := LockInstance(base, "tool"); err == nil {
		t.Fatal("存活锁下二次加锁应被拒绝")
	}
	unlock()
	if InstanceLocked(base, "tool") {
		t.Fatal("解锁后不应报告锁定")
	}
	if _, err := os.Stat(filepath.Join(base, "runs", ".lock-tool")); !os.IsNotExist(err) {
		t.Fatal("解锁应移除锁文件")
	}
}

func TestInstanceLockStaleTakeover(t *testing.T) {
	base := t.TempDir()
	dead := filepath.Join(base, "runs")
	if err := os.MkdirAll(dead, 0o755); err != nil {
		t.Fatal(err)
	}
	// pid 1 必然存活：应拒绝；pid 取一个几乎不可能存在的极大值：应接管。
	if err := os.WriteFile(filepath.Join(dead, ".lock-tool"),
		[]byte("pid="+strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LockInstance(base, "tool"); err == nil {
		t.Fatal("存活 pid 的锁应被拒绝")
	}
	if err := os.WriteFile(filepath.Join(dead, ".lock-tool"),
		[]byte("pid=2147483647"), 0o644); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockInstance(base, "tool")
	if err != nil {
		t.Fatalf("陈锁应被接管: %v", err)
	}
	unlock()
}
