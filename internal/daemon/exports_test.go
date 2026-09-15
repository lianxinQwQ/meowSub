package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"meowsub/internal/config"
	"meowsub/internal/state"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- Desktop 解析与字段码 ------------------------------------------------

func TestParseDesktopBasic(t *testing.T) {
	data := []byte("[Other]\nExec=WRONG\n[Desktop Entry]\nTerminal=true\nIcon=i\nExec=foo %f --bar\n")
	e, err := ParseDesktop(data)
	if err != nil {
		t.Fatal(err)
	}
	if e.Exec != "foo %f --bar" || !e.Terminal || e.Icon != "i" {
		t.Fatalf("entry = %+v", e)
	}
}

func TestExpandExecFieldCodes(t *testing.T) {
	cases := []struct {
		name, exec string
		args       []string
		want       []string
	}{
		{"无字段码参数追加", "code", []string{"-n"}, []string{"code", "-n"}},
		{"单文件消费", "vim %f", []string{"/a/b.txt", "-R"},
			[]string{"vim", "/a/b.txt", "-R"}},
		{"多文件全量", "viewer %F --x", []string{"1.txt", "2.png"},
			[]string{"viewer", "1.txt", "2.png", "--x"}},
		{"URL 单个", "browser -u %u home", nil, []string{"browser", "-u", "home"}},
		{"百分号转义", "prog 100%%", nil, []string{"prog", "100%"}},
		{"弃用码丢弃", "app %d %c tail", nil, []string{"app", "tail"}},
		{"引号保留整体", `msg "hello world"`, []string{},
			[]string{"msg", "hello world"}},
	}
	for _, c := range cases {
		got := ExpandExec(c.exec, c.args)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestResolveOpenPrefersBinaryThenDesktop(t *testing.T) {
	layer := t.TempDir()
	for _, d := range []string{
		filepath.Join("usr/share/applications"),
		filepath.Join("usr/local/bin"),
	} {
		os.MkdirAll(filepath.Join(layer, d), 0o755)
	}
	writeFile(t, filepath.Join(layer, "usr/local/bin/tool"), "#!/bin/sh\n")
	writeFile(t, filepath.Join(layer, "usr/share/applications/gui-app.desktop"),
		"[Desktop Entry]\nExec=gui-bin %U --flag\n")

	argv, err := ResolveOpen(layer, "tool", []string{"-s"})
	if err != nil || argv[0] != "/usr/local/bin/tool" || argv[1] != "-s" {
		t.Fatalf("二进制路径优先失败: %v %v", argv, err)
	}
	argv, err = ResolveOpen(layer, "gui-app", []string{"http://x/y"})
	if err != nil || !reflect.DeepEqual(argv, []string{
		"/bin/sh", "-c", `'gui-bin' 'http://x/y' '--flag'`}) {
		t.Fatalf("desktop 展开应包进 shell 兜底: %v %v", argv, err)
	}
	if _, err := ResolveOpen(layer, "ghost", nil); err == nil {
		t.Fatal("未知目标应报错")
	}
}

// --- SyncExports 调和行为 ------------------------------------------------

// testSelfBin 生成物断言用的哨兵自定位路径：证明内容来自参数而非硬编码位置。
const testSelfBin = "/opt/meow/bin/meowsub"

func syncFixture(t *testing.T) (string, *state.State, string) {
	base := t.TempDir()
	layer := filepath.Join(base, "runs", "code")
	m := &state.Marker{Name: "code", Export: []state.ExportEntry{
		{Package: "vim", Bins: []string{"/usr/bin/vim", "/usr/bin/view"},
			Apps: []state.ExportApp{{DesktopID: "vim",
				IconRel: "usr/share/icons/hicolor/48x48/apps/vim.png"}}},
		{Package: "musescore", Apps: []state.ExportApp{
			{DesktopID: "mscore"}}}}}
	writeFile(t, filepath.Join(layer,
		"usr/share/icons/hicolor/48x48/apps/vim.png"), "icon")
	if err := state.WriteMarker(layer, m); err != nil {
		t.Fatal(err)
	}
	st := &state.State{}
	return base, st, layer
}

func TestSyncExportsGeneratesRegistersAndSweeps(t *testing.T) {
	base, st, _ := syncFixture(t)

	redirectSystemRoots(t)

	prevDesktop := filepath.Join(desktopRoot, "meowsub-code-old.desktop")
	cfg := &config.Config{BaseDir: base, Groups: []*config.Group{
		{Name: "code", Export: []string{"vim", "musescore"}}}}

	// 预置一轮“上轮登记”但指向已不存在的陈旧导出
	st.Exports = []*state.GroupExports{{Group: "old",
		Paths: []string{prevDesktop}}}
	if err := os.WriteFile(prevDesktop, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SyncExports(cfg, st, testSelfBin); err != nil {
		t.Fatal(err)
	}

	// 包内每个 PATH 可执行各得一个 open shim
	for _, name := range []string{"vim", "view"} {
		content, err := os.ReadFile(filepath.Join(base, "bin", name))
		if err != nil ||
			!strings.Contains(string(content),
				"exec "+testSelfBin+" open code "+name) {
			t.Fatalf("shim %s 内容异常: %q %v", name, content, err)
		}
	}

	// 桌面条目组装 run-desktop 命令；Name 后缀带组名以区分子系统
	df := filepath.Join(desktopRoot, "meowsub-code-vim.desktop")
	got, err := os.ReadFile(df)
	if err != nil {
		t.Fatalf("desktop 未生成: %v", err)
	}
	if !strings.Contains(string(got), "Name=vim (meowsub-code)\n") {
		t.Fatalf("Name 应带组名后缀:\n%s", got)
	}
	wantExec := "Exec=" + testSelfBin + " run-desktop --detach code " +
		"/usr/share/applications/vim.desktop %U\n"
	if !strings.Contains(string(got), wantExec) {
		t.Fatalf("Exec 应组装 run-desktop:\n%s", got)
	}
	if !strings.Contains(string(got), "TryExec="+testSelfBin+"\n") {
		t.Fatalf("TryExec 异常:\n%s", got)
	}
	if !strings.Contains(string(got), "Icon=meowsub-code-vim\n") {
		t.Fatalf("有图标条目应含 Icon 行:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(iconHicolRoot, "48x48/apps",
		"meowsub-code-vim.png")); err != nil {
		t.Fatalf("图标未复制到 hicolor: %v", err)
	}
	if _, err := os.Stat(filepath.Join(desktopRoot,
		"meowsub-code-mscore.desktop")); err != nil {
		t.Fatalf("第二包桌面条目缺失: %v", err)
	}
	ms, err := os.ReadFile(filepath.Join(desktopRoot,
		"meowsub-code-mscore.desktop"))
	if err != nil || strings.Contains(string(ms), "Icon=") {
		t.Fatalf("无图标条目不应含 Icon 行: %q %v", ms, err)
	}
	if _, err := os.Stat(prevDesktop); !os.IsNotExist(err) {
		t.Fatal("孤儿导出未被清扫")
	}
	// 登记表精确覆盖：2 shim + 2 desktop + 1 图标，且不含旧组
	if len(st.Exports) != 1 || st.Exports[0].Group != "code" {
		t.Fatalf("exports 登记 = %+v", st.Exports)
	}
	if len(st.Exports[0].Paths) != 5 {
		t.Fatalf("paths = %+v", st.Exports[0].Paths)
	}
}

// --- 构建区回退 ----------------------------------------------------------

func TestSyncExportsFallsBackToBuildLayer(t *testing.T) {
	redirectSystemRoots(t)
	base := t.TempDir()
	layer := filepath.Join(base, "build", "code") // 只有构建区成品，无 runs 实例
	m := &state.Marker{Name: "code", Export: []state.ExportEntry{
		{Package: "vim", Bins: []string{"/usr/bin/vim"},
			Apps: []state.ExportApp{{DesktopID: "vim",
				IconRel: "usr/share/icons/hicolor/48x48/apps/vim.png"}}}}}
	writeFile(t, filepath.Join(layer,
		"usr/share/icons/hicolor/48x48/apps/vim.png"), "icon")
	if err := state.WriteMarker(layer, m); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{BaseDir: base, Groups: []*config.Group{
		{Name: "code", Export: []string{"vim"}}}}
	st := &state.State{}

	if err := SyncExports(cfg, st, testSelfBin); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(base, "bin", "vim"))
	if err != nil || !strings.Contains(string(content),
		"exec "+testSelfBin+" open code vim") {
		t.Fatalf("构建区回退 shim 未生成: %q %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(iconHicolRoot, "48x48/apps",
		"meowsub-code-vim.png")); err != nil {
		t.Fatalf("构建区回退图标未复制: %v", err)
	}
	if len(st.Exports) != 1 || st.Exports[0].Group != "code" ||
		len(st.Exports[0].Paths) != 3 {
		t.Fatalf("exports 登记 = %+v", st.Exports)
	}
}

// --- run-desktop 目标解析 ------------------------------------------------

func TestResolveDesktopFileForms(t *testing.T) {
	layer := t.TempDir()
	writeFile(t, filepath.Join(layer, "usr/share/applications/app.desktop"),
		"[Desktop Entry]\nExec=/opt/bin/app %U\n")
	writeFile(t, filepath.Join(layer, "usr/share/applications/noid.desktop"),
		"[Desktop Entry]\nExec=noid-bin %f --x\n")
	writeFile(t, filepath.Join(layer, "etc/xdg/custom.desktop"),
		"[Desktop Entry]\nExec=custom-bin %f\n")

	cases := []struct {
		name   string
		target string
		want   []string
	}{
		{"实例内绝对路径", "/etc/xdg/custom.desktop",
			[]string{"/bin/sh", "-c", `'custom-bin' 'a.txt'`}},
		{"相对路径（无前导斜杠）", "usr/share/applications/noid.desktop",
			[]string{"/bin/sh", "-c", `'noid-bin' 'a.txt' '--x'`}},
		{"裸ID自动补后缀", "app", nil},
		{"ID带后缀", "app.desktop", nil},
	}
	for _, c := range cases {
		args := []string{"a.txt"}
		if c.want == nil { // ID 形态顺便验证 file:// URI 还原
			args = []string{"file:///home/u/a%20b.png"}
			c.want = []string{"/bin/sh", "-c",
				`'/opt/bin/app' '/home/u/a b.png'`}
		}
		got, err := ResolveDesktopFile(layer, c.target, args)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v (%v) want %v", c.name, got, err, c.want)
		}
	}
	if _, err := ResolveDesktopFile(layer, "ghost", nil); err == nil {
		t.Error("未知目标应报错")
	}
}

func redirectSystemRoots(t *testing.T) {
	t.Helper()
	sys := t.TempDir()
	os.MkdirAll(filepath.Join(sys, "applications"), 0o755)
	oldD, oldH, oldP := desktopRoot, iconHicolRoot, iconPixmapDir
	desktopRoot = filepath.Join(sys, "applications")
	iconHicolRoot = filepath.Join(sys, "icons/hicolor")
	iconPixmapDir = filepath.Join(sys, "pixmaps")
	t.Cleanup(func() { desktopRoot, iconHicolRoot, iconPixmapDir = oldD, oldH, oldP })
}

func TestSyncExportsSkipsIdenticalAndReplacesChanged(t *testing.T) {
	redirectSystemRoots(t)
	base, st, _ := syncFixture(t)
	cfg := &config.Config{BaseDir: base, Groups: []*config.Group{
		{Name: "code", Export: []string{"vim"}}}}
	if err := SyncExports(cfg, st, testSelfBin); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(base, "bin", "vim")
	before, _ := os.ReadFile(shim)
	fiBefore, _ := os.Stat(shim)

	// 内容一致：重复执行不应改动 mtime
	if err := SyncExports(cfg, st, testSelfBin); err != nil {
		t.Fatal(err)
	}
	fiAfter, _ := os.Stat(shim)
	if !fiBefore.ModTime().Equal(fiAfter.ModTime()) {
		t.Fatal("内容一致时应跳过写入（mtime 变化）")
	}

	// 外部被改：下一轮调和应替换回托管内容
	os.WriteFile(shim, []byte("tampered"), 0o755)
	if err := SyncExports(cfg, st, testSelfBin); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(shim)
	if string(after) == "tampered" {
		t.Fatal("被篡改的托管控件未恢复")
	}
	_ = before
}

// --- 自定位守卫与生成语法引用 --------------------------------------------

func TestSyncExportsRejectsEmptySelfBin(t *testing.T) {
	base, st, _ := syncFixture(t)
	redirectSystemRoots(t)
	cfg := &config.Config{BaseDir: base, Groups: []*config.Group{
		{Name: "code", Export: []string{"vim"}}}}
	if err := SyncExports(cfg, st, ""); err == nil {
		t.Fatal("空自定位路径应报错而非落盘")
	}
}

func TestQuoteDesktopExecPath(t *testing.T) {
	if got := quoteDesktopExecPath("/usr/local/bin/meowsub"); got != "/usr/local/bin/meowsub" {
		t.Fatalf("安全路径应原样: %q", got)
	}
	got := quoteDesktopExecPath(`/opt/My% App"bin\meowsub`)
	want := `"/opt/My%% App\"bin\\meowsub"`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestQuoteShellArg(t *testing.T) {
	if got := quoteShellArg("/opt/x/meowsub"); got != "/opt/x/meowsub" {
		t.Fatalf("安全路径应原样: %q", got)
	}
	if got, want := quoteShellArg("/opt/my'app/meowsub"), `'/opt/my'\''app/meowsub'`; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestQuoteUnitArg(t *testing.T) {
	if got := quoteUnitArg("/opt/x/meowsub"); got != "/opt/x/meowsub" {
		t.Fatalf("安全路径应原样: %q", got)
	}
	if got, want := quoteUnitArg("/opt/My App$%x"), `"/opt/My App$$%%x"`; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
