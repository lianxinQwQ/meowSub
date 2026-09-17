package builder

import (
	"reflect"
	"testing"

	"meowsub/internal/execx"
)

func TestParseRepoDesc(t *testing.T) {
	desc := "\n%NAME%\nvisual-studio-code-bin\n%VERSION%\n1.102.3-1\n%BASE%\nvscode\n%DESC%\nEditor\n"
	name, ver := parseRepoDesc(desc)
	if name != "visual-studio-code-bin" || ver != "1.102.3-1" {
		t.Fatalf("解析结果异常: name=%q version=%q", name, ver)
	}
	if n, v := parseRepoDesc(""); n != "" || v != "" {
		t.Fatalf("空内容应返回空值: %q %q", n, v)
	}
}

func TestFilterBuiltTargets(t *testing.T) {
	have := poolVersions{
		"code":  {"1.0-1"},
		"multi": {"2.0-1", "2.1-4"}, // repo-add 允许同名多版本
	}
	pkgs := []string{"code", "rust", "multi", "noversion"}
	wanted := map[string]string{
		"code":      "1.0-1", // 池内已有同版本 → 跳过
		"rust":      "9.9-1", // 版本变化 → 构建
		"multi":     "2.1-4", // 命中多版本之一 → 跳过
		"noversion": "",      // 上游版本未知 → 保守构建
	}
	todo, skipped := filterBuiltTargets(pkgs, wanted, have)
	if !reflect.DeepEqual(todo, []string{"rust", "noversion"}) {
		t.Fatalf("应构建的目标不符: %v", todo)
	}
	if skipped != 2 {
		t.Fatalf("跳过数应为 2，得到 %d", skipped)
	}
}

func TestFilterBuiltTargetsGitVCS(t *testing.T) {
	have := poolVersions{
		"codex-desktop-linux": {"26.707.31428.r1301.g5fc3961d-1"},
	}
	pkgs := []string{"codex-desktop-linux", "bumped", "relnumber", "unrelated", "noversion"}
	wanted := map[string]string{
		// 解析期静态版本 vs 池内 git 构建版本：静态部分与 pkgrel 一致 → 跳过
		"codex-desktop-linux": "26.707.31428-1",
		"bumped":              "26.708.0-1",     // 上游静态版本升级 → 构建
		"relnumber":           "26.707.31428-2", // 仅 pkgrel 变化（打包修正）→ 构建
		"unrelated":           "26.707.31429-1", // 静态版本不同 → 构建
		"noversion":           "",               // 版本未知 → 保守构建
	}
	todo, skipped := filterBuiltTargets(pkgs, wanted, have)
	if !reflect.DeepEqual(todo, []string{"bumped", "relnumber", "unrelated", "noversion"}) {
		t.Fatalf("应构建的目标不符: %v", todo)
	}
	if skipped != 1 {
		t.Fatalf("跳过数应为 1，得到 %d", skipped)
	}
}

func TestIsGitBuildOf(t *testing.T) {
	cases := []struct {
		built, want string
		ok          bool
	}{
		{"26.707.31428.r1301.g5fc3961d-1", "26.707.31428-1", true},  // 日志中的真实形态
		{"1.2.3.r1.g0abcdef-2", "1.2.3-2", true},                    // 哈希含数字前缀
		{"4:1.0.r3.gabc-1", "4:1.0-1", true},                        // 带 epoch
		{"26.707.31428.r1301.g5fc3961d-1", "26.707.31428-2", false}, // pkgrel 不一致
		{"26.708.0.r1.gabc-1", "26.707.31428-1", false},             // 静态版本升级
		{"26.707.31428-1", "26.707.31428-1", false},                 // 精确相等不走此路径
		{"20250825", "20250825", false},                             // 无 pkgrel 段
		{"1.0.rx.gabc-1", "1.0-1", false},                           // 提交数非数字
		{"1.0.r5-1", "1.0-1", false},                                // 缺 .g<哈希>
		{"1.0.r5.g-1", "1.0-1", false},                              // 空哈希
		{"1.0.r5.gABC-1", "1.0-1", false},                           // 哈希非小写十六进制
		{"1.0.r5.gabc-1", "1.0.r5.gabc-1", false},                   // want 不合约定时保守
	}
	for _, c := range cases {
		if got := isGitBuildOf(c.built, c.want); got != c.ok {
			t.Errorf("isGitBuildOf(%q, %q) = %v, 期望 %v", c.built, c.want, got, c.ok)
		}
	}
}

func TestPoolVersionAtLeastUsesArchVersionOrdering(t *testing.T) {
	const built = "26.908.70816.r2122.g5f7310d7-1"
	const want = "26.908.40834.r2114.g249cd4b6-1"
	key := "vercmp " + built + " " + want
	r := &execx.RecordRunner{OutputFor: map[string]execx.OutputEntry{
		key: {Out: "1\n"},
	}}

	if !poolVersionAtLeast(r, built, want) {
		t.Fatalf("池内更高版本应满足 AUR 版本: built=%q want=%q", built, want)
	}

	low := &execx.RecordRunner{OutputFor: map[string]execx.OutputEntry{
		key: {Out: "-1\n"},
	}}
	if poolVersionAtLeast(low, built, want) {
		t.Fatalf("低版本不应满足 AUR 版本: built=%q want=%q", built, want)
	}
}

func TestFilterBuiltTargetsAcceptsNewerVCSArtifact(t *testing.T) {
	const built = "26.908.70816.r2122.g5f7310d7-1"
	const want = "26.908.40834.r2114.g249cd4b6-1"
	r := &execx.RecordRunner{OutputFor: map[string]execx.OutputEntry{
		"vercmp " + built + " " + want: {Out: "1\n"},
	}}
	b := New(r, nil, t.TempDir())
	todo, skipped := filterBuiltTargetsBy(
		[]string{"codex-desktop-git"},
		map[string]string{"codex-desktop-git": want},
		poolVersions{"codex-desktop-git": {built}},
		b.hasPoolVersion,
	)
	if len(todo) != 0 || skipped != 1 {
		t.Fatalf("已有更高 VCS 产物却未跳过: todo=%v skipped=%d", todo, skipped)
	}
}

func TestPromoteStale(t *testing.T) {
	wanted := map[string]string{"code": "1.134.0-1", "rust-analyzer": "20250825"}
	skipped := []string{"code", "rust-analyzer"}

	// 上游实时版本与本地一致：不提升
	live := map[string]string{"code": "1.134.0-1", "rust-analyzer": "20250825"}
	if got := promoteStale(skipped, wanted, live); len(got) != 0 {
		t.Fatalf("版本一致不应提升: %v", got)
	}

	// code 上游已发新版（模拟转储缓存滞后）：仅提升它
	live["code"] = "1.135.0-1"
	got := promoteStale(skipped, wanted, live)
	if !reflect.DeepEqual(got, []string{"code"}) {
		t.Fatalf("应仅提升滞后目标: %v", got)
	}

	// 上游查不到的名字（如已删除）：保守不动
	if got := promoteStale(skipped, wanted, map[string]string{}); len(got) != 0 {
		t.Fatalf("复核空结果应维持跳过: %v", got)
	}
}
