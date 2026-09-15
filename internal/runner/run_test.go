package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree 以 map[相对路径]内容 在 root 下构造最小可辨识的系统树。
// 特意造出 usr/ 目录，使目录被识别为完整系统根。
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// treeFiles 采集目录下全部常规文件：相对路径 -> 内容。
func treeFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		out[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertTree(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("文件数不符: got %v want %v", keys(got), keys(want))
	}
	for rel, wantC := range want {
		gotC, ok := got[rel]
		if !ok || gotC != wantC {
			t.Fatalf("文件 %q 不符: got %q want %q", rel, gotC, wantC)
		}
	}
}

// gnupg 钥匙环按普通树内容参与 diff：内容漂移进 newFiles、实体侧多出的
// 常规残留进 removed；真实 socket 等特殊文件由类型过滤天然不参与。
func TestDiffTreeCoversGnupg(t *testing.T) {
	staged := t.TempDir()
	lower := t.TempDir()
	writeTree(t, staged, map[string]string{
		"usr/bin/tool":                   "v2",
		"etc/pacman.d/gnupg/trustdb.gpg": "a",
	})
	writeTree(t, lower, map[string]string{
		"usr/bin/tool":                   "v1",
		"etc/pacman.d/gnupg/trustdb.gpg": "b",
		"etc/pacman.d/gnupg/S.dirmngr":   "stale-residue",
	})
	newFiles, removed := diffTree(staged, lower)
	wantNew := []string{"etc/pacman.d/gnupg/trustdb.gpg", "usr/bin/tool"}
	if len(newFiles) != len(wantNew) || newFiles[0] != wantNew[0] || newFiles[1] != wantNew[1] {
		t.Errorf("newFiles = %v, want %v", newFiles, wantNew)
	}
	if len(removed) != 1 || removed[0] != "etc/pacman.d/gnupg/S.dirmngr" {
		t.Errorf("removed = %v, want [etc/pacman.d/gnupg/S.dirmngr]", removed)
	}
}

// diffTree 应看见符号链接：新增/目标漂移/缺失各归其位。
func TestDiffTreeSeesSymlinks(t *testing.T) {
	staged, lower := t.TempDir(), t.TempDir()
	writeTree(t, staged, map[string]string{"usr/bin/tool": "v"})
	writeTree(t, lower, map[string]string{"usr/bin/tool": "v"})
	mustLink := func(root, rel, target string) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, full); err != nil {
			t.Fatal(err)
		}
	}
	mustLink(staged, "usr/lib/a.so", "real") // staged 新增
	mustLink(lower, "usr/lib/b.so", "gone")  // lower 独有 → removed
	mustLink(staged, "usr/lib/c.so", "t1")   // 目标漂移 → changed
	mustLink(lower, "usr/lib/c.so", "t2")
	newFiles, removed := diffTree(staged, lower)
	wantNew := []string{"usr/lib/a.so", "usr/lib/c.so"}
	if len(newFiles) != 2 || newFiles[0] != wantNew[0] || newFiles[1] != wantNew[1] {
		t.Errorf("new = %v, want %v", newFiles, wantNew)
	}
	if len(removed) != 1 || removed[0] != "usr/lib/b.so" {
		t.Errorf("removed = %v, want [usr/lib/b.so]", removed)
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func testPaths(t *testing.T) (Paths, string) { // 构建区成品层 code/
	t.Helper()
	base := t.TempDir()
	writeTree(t, filepath.Join(base, "build", "code"), map[string]string{
		"usr/bin/code":       "code-v1",
		"etc/config/default": "base",
	})
	return Paths{BaseDir: base, In: strings.NewReader(""), Out: &strings.Builder{}}, base
}

// 无 reflink 的测试替身：宿主 fs（tmpfs/ext4）不支持 reflink 时 Deploy 仍可测。
func plainCopy() func(string, string) error {
	return func(src, dst string) error {
		return sh("cp", "-a", src, dst)
	}
}

func TestDeployFreshCreatesMasterAndRun(t *testing.T) {
	p, base := testPaths(t)
	old := copyDir
	copyDir = plainCopy()
	defer func() { copyDir = old }()

	src := filepath.Join(base, "build", "code")
	if err := p.Deploy(src, "mydev", true); err != nil {
		t.Fatal(err)
	}
	assertTree(t, treeFiles(t, p.Master("mydev")), treeFiles(t, src))
	assertTree(t, treeFiles(t, p.Root("mydev")), treeFiles(t, src))
	if _, err := os.Stat(p.Root(".incoming-mydev")); !os.IsNotExist(err) {
		t.Fatalf("暂存目录残留: %v", err)
	}
}

func TestRedeployIdenticalResetsSilently(t *testing.T) {
	p, base := testPaths(t)
	old := copyDir
	copyDir = plainCopy()
	defer func() { copyDir = old }()

	src := filepath.Join(base, "build", "code")
	if err := p.Deploy(src, "mydev", true); err != nil {
		t.Fatal(err)
	}
	// 实例未动，重部署应静默通过（In 为空 reader，若触发交互会因 EOF 拒绝而失败）
	if err := p.Deploy(src, "mydev", false); err != nil {
		t.Fatalf("一致实例不应要求确认: %v", err)
	}
	assertTree(t, treeFiles(t, p.Root("mydev")), treeFiles(t, src))
}

func TestRedeployDivergedRequiresConfirmation(t *testing.T) {
	p, base := testPaths(t)
	old := copyDir
	copyDir = plainCopy()
	defer func() { copyDir = old }()

	src := filepath.Join(base, "build", "code")
	if err := p.Deploy(src, "mydev", true); err != nil {
		t.Fatal(err)
	}
	// 使用态侧出现改动与新增
	writeTree(t, p.Root("mydev"), map[string]string{
		"etc/config/default": "user-edited",
		"home/me/notes.md":   "precious",
	})
	before := treeFiles(t, p.Root("mydev"))

	// 交互输入 n → 中止且什么都没改
	p.In = strings.NewReader("n\n")
	if err := p.Deploy(src, "mydev", false); err == nil {
		t.Fatal("拒绝确认后应报错中止")
	}
	assertTree(t, treeFiles(t, p.Root("mydev")), before)

	// 非交互无确认（EOF）→ 同样中止
	p.In = strings.NewReader("")
	if err := p.Deploy(src, "mydev", false); err == nil {
		t.Fatal("无法获得确认时应中止")
	}
	assertTree(t, treeFiles(t, p.Root("mydev")), before)

	// 确认后 → 双副本整体重置为最新构建成品
	p.In = strings.NewReader("y\n")
	if err := p.Deploy(src, "mydev", false); err != nil {
		t.Fatal(err)
	}
	assertTree(t, treeFiles(t, p.Root("mydev")), treeFiles(t, src))
	assertTree(t, treeFiles(t, p.Master("mydev")), treeFiles(t, src))
}

func TestRedeployDivergedYesFlagSkipsPrompt(t *testing.T) {
	p, base := testPaths(t)
	old := copyDir
	copyDir = plainCopy()
	defer func() { copyDir = old }()

	src := filepath.Join(base, "build", "code")
	if err := p.Deploy(src, "mydev", true); err != nil {
		t.Fatal(err)
	}
	writeTree(t, p.Root("mydev"), map[string]string{"extra.txt": "x"})
	if err := p.Deploy(src, "mydev", true); err != nil {
		t.Fatal(err)
	}
	assertTree(t, treeFiles(t, p.Root("mydev")), treeFiles(t, src))
}

func TestRedeployWithoutBaselineNeedsExplicitConfirm(t *testing.T) {
	p, base := testPaths(t)
	old := copyDir
	copyDir = plainCopy()
	defer func() { copyDir = old }()

	src := filepath.Join(base, "build", "code")
	// 模拟迁移前旧数据：直接放入实例目录而无基线
	writeTree(t, p.Root("legacy"), map[string]string{"old/file": "keep?"})

	p.In = strings.NewReader("")
	if err := p.Deploy(src, "legacy", false); err == nil {
		t.Fatal("缺基线时必须显式确认")
	}
	if _, err := os.Stat(filepath.Join(p.Root("legacy"), "old", "file")); err != nil {
		t.Fatal("中止后原实例不应被改动")
	}
	p.In = strings.NewReader("yes\n")
	if err := p.Deploy(src, "legacy", false); err != nil {
		t.Fatal(err)
	}
	assertTree(t, treeFiles(t, p.Root("legacy")), treeFiles(t, src))
}

func TestDestroyRemovesRunAndMaster(t *testing.T) {
	p, base := testPaths(t)
	old := copyDir
	copyDir = plainCopy()
	defer func() { copyDir = old }()

	if err := p.Deploy(filepath.Join(base, "build", "code"), "mydev", true); err != nil {
		t.Fatal(err)
	}
	if err := p.Destroy("mydev"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.Root("mydev")); !os.IsNotExist(err) {
		t.Fatal("实例未删除")
	}
	if _, err := os.Stat(p.Master("mydev")); !os.IsNotExist(err) {
		t.Fatal("基线未删除")
	}
	if err := p.Destroy("mydev"); err == nil {
		t.Fatal("销毁不存在的 run 应报错")
	}
}

func TestListIgnoresHiddenDirsAndReportsBaseline(t *testing.T) {
	p, base := testPaths(t)
	old := copyDir
	copyDir = plainCopy()
	defer func() { copyDir = old }()

	if err := p.Deploy(filepath.Join(base, "build", "code"), "b", true); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p.runsDir(), ".incoming-x"), 0o755); err != nil {
		t.Fatal(err)
	}
	runs, err := p.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0] != "b" {
		t.Fatalf("List 结果异常: %v", runs)
	}
}
