package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meowsub/internal/config"
	"meowsub/internal/execx"
)

// TestApplyCopyTable 复制表施加：目标旧内容先清除（复制表对目标位置拥有
// 最终决定权），深层目标目录自动建立，逐条经 cp -a --reflink=always 整份
// 复制（文件与目录同权）。
func TestApplyCopyTable(t *testing.T) {
	base := t.TempDir()
	srcDir := filepath.Join(base, "srccfg")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "a.conf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(base, "single.conf")
	if err := os.WriteFile(srcFile, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(base, "node")
	if err := os.MkdirAll(filepath.Join(node, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 预置旧目标：施加后必须被清除（哪怕 RecordRunner 不真正复制）
	if err := os.MkdirAll(filepath.Join(node, "etc", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(node, "etc", "deep", "single.conf"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	copies := []config.Copy{
		{Source: srcFile, Dest: "/etc/deep/single.conf"},
		{Source: srcDir, Dest: "/etc/cfgdir"},
	}
	if err := b.applyCopyTable(node, copies); err != nil {
		t.Fatalf("applyCopyTable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(node, "etc", "deep", "single.conf")); !os.IsNotExist(err) {
		t.Errorf("旧目标文件应已被清除")
	}
	findCmd(t, r, "cp", "--reflink=always",
		srcFile, filepath.Join(node, "etc/deep/single.conf"))
	findCmd(t, r, "cp", "--reflink=always",
		srcDir, filepath.Join(node, "etc/cfgdir"))
	if n := cmdCount(r, "cp"); n != 2 {
		t.Errorf("cp 次数 = %d, want 2", n)
	}
}

// TestApplyCopyTableEmpty 空复制表是干净的空操作。
func TestApplyCopyTableEmpty(t *testing.T) {
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, t.TempDir())
	if err := b.applyCopyTable(t.TempDir(), nil); err != nil {
		t.Fatalf("applyCopyTable: %v", err)
	}
	if len(r.Cmds) != 0 {
		t.Errorf("空表不应执行命令: %v", r.Cmds)
	}
}

// TestApplyCopyTableMissingSource 来源缺失即报错（防御性：正常流程由
// ValidateCopySources 前置拦截）。
func TestApplyCopyTableMissingSource(t *testing.T) {
	base := t.TempDir()
	node := filepath.Join(base, "node")
	if err := os.MkdirAll(filepath.Join(node, "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	copies := []config.Copy{{Source: filepath.Join(base, "ghost"), Dest: "/etc/x"}}
	if err := b.applyCopyTable(node, copies); err == nil {
		t.Fatal("来源缺失应报错")
	}
}

// TestApplyCopyTableSymlinkSource 来源为符号链接：存在性按链接目标解析
// （stat 语义），断链报错；有效链接以原路径交由 cp 处理。
func TestApplyCopyTableSymlinkSource(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "real.conf")
	if err := os.WriteFile(target, []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link.conf")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(base, "node")
	if err := os.MkdirAll(filepath.Join(node, "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	if err := b.applyCopyTable(node, []config.Copy{
		{Source: link, Dest: "/etc/link.conf"}}); err != nil {
		t.Fatalf("有效符号链接应放行: %v", err)
	}
	findCmd(t, r, "cp", "--reflink=always", link, filepath.Join(node, "etc/link.conf"))

	broken := filepath.Join(base, "broken.conf")
	if err := os.Symlink(filepath.Join(base, "ghost"), broken); err != nil {
		t.Fatal(err)
	}
	if err := b.applyCopyTable(node, []config.Copy{
		{Source: broken, Dest: "/etc/x"}}); err == nil {
		t.Fatal("断链来源应报错")
	}
}

// TestValidateCopySources 构建前置校验：任一组的任一来源不存在即报错并
// 指明组与路径；全部存在时放行。
func TestValidateCopySources(t *testing.T) {
	base := t.TempDir()
	ok := filepath.Join(base, "ok.conf")
	if err := os.WriteFile(ok, []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(base, "missing")
	cfg := &config.Config{Groups: []*config.Group{
		{Name: "tool", Copies: []config.Copy{{Source: ok, Dest: "/etc/x"}}},
		{Name: "bad", Copies: []config.Copy{{Source: missing, Dest: "/etc/y"}}},
	}}
	err := ValidateCopySources(cfg)
	if err == nil {
		t.Fatal("来源缺失应报错")
	}
	if !strings.Contains(err.Error(), "bad") || !strings.Contains(err.Error(), missing) {
		t.Errorf("错误应指明组名与来源路径: %v", err)
	}
	cfg.Groups = cfg.Groups[:1]
	if err := ValidateCopySources(cfg); err != nil {
		t.Fatalf("全部存在应放行: %v", err)
	}
}
