// sync_instances_test.go：build 收尾自动同步的编排语义——一致静默重置、
// 运行中实例先停后启、拒绝丢弃即终止且实例保持原样、锁随同步生灭。
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"meowsub/internal/config"
	"meowsub/internal/daemon"
	"meowsub/internal/runner"
)

// plainCp /tmp 的 tmpfs 不支持 reflink：Deploy 路径注入普通 cp 替身。
func plainCp(src, dst string) error {
	return exec.Command("cp", "-a", src, dst).Run()
}

type fakeCtl struct {
	runningSet map[string]bool
	stopped    []string
	started    []string
}

func (f *fakeCtl) running(run string) (bool, error) { return f.runningSet[run], nil }
func (f *fakeCtl) stop(run string) error {
	f.stopped = append(f.stopped, run)
	f.runningSet[run] = false
	return nil
}
func (f *fakeCtl) start(run string) error {
	f.started = append(f.started, run)
	f.runningSet[run] = true
	return nil
}

func putTreeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTreeFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// syncFixture 构造：成品层 build/tool=v2；实例 runs/tool=v1（旧一代）。
func syncFixture(t *testing.T) (*config.Config, string) {
	t.Helper()
	base := t.TempDir()
	putTreeFile(t, filepath.Join(base, "build", "tool"), "usr/bin/v", "v2")
	putTreeFile(t, filepath.Join(base, "runs", "tool"), "usr/bin/v", "v1")
	return &config.Config{
		BaseDir: base,
		Groups:  []*config.Group{{Name: "tool"}},
	}, base
}

func TestSyncInstancesAppliesAndRestarts(t *testing.T) {
	cfg, base := syncFixture(t)
	ctl := &fakeCtl{runningSet: map[string]bool{"tool": true}}
	var out strings.Builder

	restore := runner.CopyDirOverride
	runner.CopyDirOverride = plainCp
	defer func() { runner.CopyDirOverride = restore }()

	if err := syncInstances(cfg, nil, true, ctl, strings.NewReader(""), &out); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := readTreeFile(t, filepath.Join(base, "runs", "tool"), "usr/bin/v"); got != "v2" {
		t.Errorf("实例未同步到新成品: %s", got)
	}
	if got := readTreeFile(t, filepath.Join(base, "master", "tool"), "usr/bin/v"); got != "v2" {
		t.Errorf("基线未刷新: %s", got)
	}
	if len(ctl.stopped) != 1 || ctl.stopped[0] != "tool" ||
		len(ctl.started) != 1 || ctl.started[0] != "tool" {
		t.Errorf("运行中实例应先停后启: stopped=%v started=%v", ctl.stopped, ctl.started)
	}
	if daemon.InstanceLocked(base, "tool") {
		t.Error("同步结束后锁应解除")
	}
}

func TestSyncInstancesDeclineAbortsAndKeepsRun(t *testing.T) {
	cfg, base := syncFixture(t)
	ctl := &fakeCtl{runningSet: map[string]bool{"tool": false}}
	var out strings.Builder

	err := syncInstances(cfg, nil, false, ctl, strings.NewReader("n\n"), &out)
	if err == nil {
		t.Fatal("拒绝丢弃使用态改动应终止 build 同步")
	}
	if got := readTreeFile(t, filepath.Join(base, "runs", "tool"), "usr/bin/v"); got != "v1" {
		t.Errorf("实例应保持原样，实际: %s", got)
	}
	if len(ctl.stopped) != 0 || len(ctl.started) != 0 {
		t.Errorf("拒绝流程不应停启实例: %v %v", ctl.stopped, ctl.started)
	}
	if daemon.InstanceLocked(base, "tool") {
		t.Error("中止后锁应解除")
	}
}

func TestSyncInstancesDeclineCloseSkipsInstance(t *testing.T) {
	cfg, base := syncFixture(t)
	ctl := &fakeCtl{runningSet: map[string]bool{"tool": true}}
	in := strings.NewReader("n\n") // 关闭询问答否
	var out strings.Builder

	if err := syncInstances(cfg, nil, false, ctl, in, &out); err != nil {
		t.Fatalf("拒绝关闭应跳过而非中止: %v", err)
	}
	if got := readTreeFile(t, filepath.Join(base, "runs", "tool"), "usr/bin/v"); got != "v1" {
		t.Errorf("跳过的实例应保持原样，实际: %s", got)
	}
	if len(ctl.stopped) != 0 || len(ctl.started) != 0 {
		t.Errorf("跳过流程不应停启实例: %v %v", ctl.stopped, ctl.started)
	}
}

// TestSyncInstancesOnlyBuiltGroups 定向构建：未出炉组的旧成品层仍在盘上
// （usr 目录齐全），不得凭目录存在性误判为新成品触发其实例同步。
func TestSyncInstancesOnlyBuiltGroups(t *testing.T) {
	base := t.TempDir()
	// 两组的成品层与实例都在盘上；tool 本轮出炉，code 不在 built 集合。
	putTreeFile(t, filepath.Join(base, "build", "tool"), "usr/bin/v", "tool-v2")
	putTreeFile(t, filepath.Join(base, "runs", "tool"), "usr/bin/v", "tool-v1")
	putTreeFile(t, filepath.Join(base, "build", "code"), "usr/bin/v", "code-v2")
	putTreeFile(t, filepath.Join(base, "runs", "code"), "usr/bin/v", "code-v1")
	cfg := &config.Config{
		BaseDir: base,
		Groups:  []*config.Group{{Name: "tool"}, {Name: "code"}},
	}
	ctl := &fakeCtl{runningSet: map[string]bool{"tool": true, "code": true}}
	var out strings.Builder

	restore := runner.CopyDirOverride
	runner.CopyDirOverride = plainCp
	defer func() { runner.CopyDirOverride = restore }()

	if err := syncInstances(cfg, map[string]bool{"tool": true}, true, ctl,
		strings.NewReader(""), &out); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := readTreeFile(t, filepath.Join(base, "runs", "tool"), "usr/bin/v"); got != "tool-v2" {
		t.Errorf("出炉组的实例应同步: %s", got)
	}
	if got := readTreeFile(t, filepath.Join(base, "runs", "code"), "usr/bin/v"); got != "code-v1" {
		t.Errorf("未出炉组的实例不应被触碰: %s", got)
	}
	for _, c := range ctl.stopped {
		if c == "code" {
			t.Error("未出炉组的运行中实例不应被停止")
		}
	}
}
