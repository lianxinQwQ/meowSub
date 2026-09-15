package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meowsub/internal/state"
)

func mkLayer(t *testing.T) string {
	t.Helper()
	l := t.TempDir()
	for _, d := range []string{"etc", "usr/bin", filepath.Join("usr/share/applications")} {
		if err := os.MkdirAll(filepath.Join(l, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func seedHostUsers(m map[string]*HostUser, err error) func() {
	old := lookupHost
	lookupHost = func([]string) (map[string]*HostUser, error) { return m, err }
	return func() { lookupHost = old }
}

func TestBakeIdentityUpsertMirrorsAndPreserves(t *testing.T) {
	layer := mkLayer(t)
	writeFile(t, filepath.Join(layer, "etc", "passwd"),
		"root:x:0:0:root:/root:/bin/bash\nlianxin:x:999:999:stale-profile:/home/old:/bin/sh\n")
	writeFile(t, filepath.Join(layer, "etc", "group"),
		"root:x:0::\nwheel:x:10:\n")

	restore := seedHostUsers(map[string]*HostUser{
		"lianxin": {Name: "lianxin", UID: 1000, GID: 1001,
			GECOS: "Lian Xin", Home: "/home/lianxin", Shell: "/bin/zsh",
			Group: "lianxin"},
	}, nil)
	defer restore()

	done, err := bakeIdentity(layer, []string{"lianxin"})
	if err != nil || len(done) != 1 || done[0] != "lianxin" {
		t.Fatalf("bakeIdentity = %v, %v", done, err)
	}

	pw := readFile(t, filepath.Join(layer, "etc", "passwd"))
	if !strings.Contains(pw, "lianxin:x:1000:1001:Lian Xin:/home/lianxin:/bin/zsh\n") {
		t.Fatalf("passwd 镜像缺失:\n%s", pw)
	}
	if strings.Contains(pw, "stale-profile") {
		t.Fatalf("同名旧条目应被整行替换:\n%s", pw)
	}
	if !strings.Contains(pw, "root:x:0:0") {
		t.Fatalf("无关条目不应被动:\n%s", pw)
	}

	gr := readFile(t, filepath.Join(layer, "etc", "group"))
	if !strings.Contains(gr, "lianxin:x:1001:") {
		t.Fatalf("group 缺镜像行:\n%s", gr)
	}
	if !strings.Contains(gr, "wheel:x:10:") {
		t.Fatalf("组库既有内容丢失:\n%s", gr)
	}

	sh := readFile(t, filepath.Join(layer, "etc", "shadow"))
	if !strings.HasPrefix(sh, "root:") && strings.Contains(sh, "lianxin:!:0:::::") {
		// shadow 可能只有新写入的一行，允许；关键是锁定格式
	} else if !strings.Contains(sh, "lianxin:!:") {
		t.Fatalf("shadow 格式异常:\n%s", sh)
	}

	// 幂等：二次执行后文本完全一致
	if err := os.WriteFile(filepath.Join(layer, "etc", "passwd"), []byte(pw), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := bakeIdentity(layer, []string{"lianxin"}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(layer, "etc", "passwd")); got != pw {
		t.Fatalf("重复烘焙应幂等:\n--- got ---\n%s\n--- want ---\n%s", got, pw)
	}
}

func TestBakeIdentityMissingUserFails(t *testing.T) {
	layer := mkLayer(t)
	restore := seedHostUsers(nil, nil) // 空宿主
	defer restore()
	if _, err := bakeIdentity(layer, []string{"ghost"}); err == nil ||
		!strings.Contains(err.Error(), "ghost") {
		t.Fatalf("缺失用户应报错并指名: %v", err)
	}
}

// seedPkgDB 在层的本地库中植入一个包的 desc 与 files 记录。
func seedPkgDB(t *testing.T, layer, name string, files []string) {
	t.Helper()
	dbDir := filepath.Join(layer, "var", "lib", "pacman", "local", name+"-9.9")
	desc := "%NAME%\n" + name + "\n\n%VERSION%\n9.9-1\n"
	writeFile(t, filepath.Join(dbDir, "desc"), desc)
	var b strings.Builder
	b.WriteString("%FILES%\n")
	for _, f := range files {
		b.WriteString(f + "\n")
	}
	writeFile(t, filepath.Join(dbDir, "files"), b.String())
}

func TestReadLayerPkgFilesDescAndSeparation(t *testing.T) {
	layer := mkLayer(t)
	seedPkgDB(t, layer, "vim", []string{"etc/vimrc", "usr/", "usr/bin/", "usr/bin/vim"})
	// 坏条目：缺 desc；不应影响其余包入索引
	writeFile(t, filepath.Join(layer,
		"var/lib/pacman/local/broken-no-desc/files"), "%FILES%\nusr/bin/x\n")

	db, err := readLayerPkgFiles(layer)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := db["vim"]
	if !ok || len(got) != 4 || got[0] != "etc/vimrc" || got[3] != "usr/bin/vim" {
		t.Fatalf("vim 清单 = %v, %v", got, ok)
	}
	if _, ok := db["broken-no-desc"]; ok {
		t.Fatalf("缺 desc 的条目应被跳过")
	}
}

func TestBakePackageExportsBinsAppsIcons(t *testing.T) {
	layer := mkLayer(t)
	seedPkgDB(t, layer, "vim", []string{
		"etc/vimrc",
		"usr/bin/",
		"usr/bin/vim",
		"usr/share/applications/",
		"usr/share/applications/vim.desktop",
	})
	writeFile(t, filepath.Join(layer, "usr/bin/vim"), "#!/bin/sh\n")
	writeFile(t, filepath.Join(layer, "usr/share/applications/vim.desktop"),
		"[Other]\nIcon=WRONG\n[Desktop Entry]\nName=Vim\nIcon=vim\nExec=vim\n")
	writeFile(t, filepath.Join(layer, "usr/share/icons/hicolor/48x48/apps/vim.png"), "s")
	writeFile(t, filepath.Join(layer, "usr/share/icons/hicolor/256x256/apps/vim.png"), "big")

	// 纯应用包：两个桌面入口、无 PATH 二进制（按 DesktopID 排序）
	seedPkgDB(t, layer, "code-suite", []string{
		"usr/share/applications/zzz-helper.desktop",
		"usr/share/applications/com.visualstudio.code.desktop",
	})
	writeFile(t, filepath.Join(layer,
		"usr/share/applications/zzz-helper.desktop"),
		"[Desktop Entry]\nExec=zzz\nTerminal=true\n")
	writeFile(t, filepath.Join(layer,
		"usr/share/applications/com.visualstudio.code.desktop"),
		"[Desktop Entry]\nIcon=com.visualstudio.code\nExec=code\n")
	writeFile(t, filepath.Join(layer,
		"usr/share/pixmaps/com.visualstudio.code.png"), "i")

	entries, err := bakePackageExports(layer, []string{"vim", "code-suite"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Package != "vim" ||
		entries[1].Package != "code-suite" {
		t.Fatalf("entries = %+v", entries)
	}
	vim := entries[0]
	if len(vim.Bins) != 1 || vim.Bins[0] != "/usr/bin/vim" {
		t.Fatalf("vim.Bins = %v", vim.Bins)
	}
	if len(vim.Apps) != 1 || vim.Apps[0].DesktopID != "vim" ||
		vim.Apps[0].Name != "Vim" {
		t.Fatalf("vim.Apps = %+v", vim.Apps)
	}
	if vim.Apps[0].IconRel != filepath.Join("usr", "share", "icons",
		"hicolor", "256x256", "apps", "vim.png") {
		t.Fatalf("应选最大尺寸图标: %+v", vim.Apps[0])
	}
	suite := entries[1]
	if len(suite.Bins) != 0 {
		t.Fatalf("纯应用包不应有 Bins: %v", suite.Bins)
	}
	if len(suite.Apps) != 2 ||
		suite.Apps[0].DesktopID != "com.visualstudio.code" ||
		suite.Apps[1].DesktopID != "zzz-helper" {
		t.Fatalf("Apps 应按 ID 排序: %+v", suite.Apps)
	}
	if suite.Apps[0].IconRel == "" {
		t.Fatalf("pixmaps 回退图标未解析到: %+v", suite.Apps[0])
	}
	if suite.Apps[1].IconRel != "" {
		t.Fatalf("无图标条目应为空: %+v", suite.Apps[1])
	}
}

func TestBakePackageExportsMissReportsAll(t *testing.T) {
	layer := mkLayer(t)
	// ghost 完全不存在；empty 存在但只有文档，无任何可导出物
	seedPkgDB(t, layer, "empty", []string{"usr/share/doc/empty/readme"})
	seedPkgDB(t, layer, "good", []string{"bin/good"})
	writeFile(t, filepath.Join(layer, "bin/good"), "#!/bin/sh\n")

	entries, err := bakePackageExports(layer, []string{"empty", "good", "ghost"})
	if err == nil || !strings.Contains(err.Error(), "empty") ||
		!strings.Contains(err.Error(), "ghost") {
		t.Fatalf("应一次性报出全部零命中包名: %v", err)
	}
	if strings.Contains(err.Error(), "good") {
		t.Fatalf("命中包不应出现在错误里: %v", err)
	}
	if len(entries) != 1 || entries[0].Package != "good" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestBakeNodeNilGroupNoop(t *testing.T) {
	b := New(nilRunner{}, &strings.Builder{}, t.TempDir())
	restore := seedDetectHumans(nil, nil)
	defer restore()
	u, e, err := b.bakeNode(t.TempDir(), nil)
	if u != nil || e != nil || err != nil {
		t.Fatalf("nil 组应全空: %v %v %v", u, e, err)
	}
}

// seedDetectHumans 替身宿主人类账户扫描。
func seedDetectHumans(names []string, err error) func() {
	old := detectHumans
	detectHumans = func() ([]string, error) { return names, err }
	return func() { detectHumans = old }
}

func TestBakeNodeAutoIdentityMirrorsScanned(t *testing.T) {
	layer := mkLayer(t)
	writeFile(t, filepath.Join(layer, "etc", "passwd"),
		"root:x:0:0:root:/root:/bin/bash\nbuilder:x:1000:1000:build:/home/builder:/bin/bash\n")
	writeFile(t, filepath.Join(layer, "etc", "group"),
		"root:x:0::\nbuilder:x:1000::\n")
	b := New(nilRunner{}, &strings.Builder{}, t.TempDir())
	restore := seedDetectHumans([]string{"lianxin"}, nil)
	defer restore()
	restoreHost := seedHostUsers(map[string]*HostUser{
		"lianxin": {Name: "lianxin", UID: 1000, GID: 1000,
			Home: "/home/lianxin", Shell: "/bin/bash", Group: "lianxin"},
	}, nil)
	defer restoreHost()

	users, _, err := b.bakeNode(layer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0] != "lianxin" {
		t.Fatalf("users = %v", users)
	}
	pw := readFile(t, filepath.Join(layer, "etc", "passwd"))
	if !strings.Contains(pw, "lianxin:x:1000:") {
		t.Fatalf("未声明组也应镜像扫描到的账户:\n%s", pw)
	}
	if strings.Contains(pw, "builder:") {
		t.Fatalf("构建期 builder 账户应从成品层移除:\n%s", pw)
	}
	if gr := readFile(t, filepath.Join(layer, "etc", "group")); strings.Contains(gr, "builder:") {
		t.Fatalf("组库中的 builder 应一并移除:\n%s", gr)
	}
}

func TestRemoveAccountFiltersAllThreeDBs(t *testing.T) {
	layer := mkLayer(t)
	writeFile(t, filepath.Join(layer, "etc", "passwd"),
		"root:x:0:0:root:/root:/bin/bash\n"+
			"builder:x:61000:61000:build:/home/builder:/bin/bash\n"+
			"li:x:1000:1000::/home/li:/bin/zsh\n")
	writeFile(t, filepath.Join(layer, "etc", "group"),
		"root:x:0::\nbuilder:x:61000::\nwheel:x:10:\n")
	writeFile(t, filepath.Join(layer, "etc", "shadow"),
		"root:!:19000::::\nbuilder:!:19000::::\n")

	if err := removeAccount(layer, "builder"); err != nil {
		t.Fatal(err)
	}
	pw := readFile(t, filepath.Join(layer, "etc", "passwd"))
	gr := readFile(t, filepath.Join(layer, "etc", "group"))
	sh := readFile(t, filepath.Join(layer, "etc", "shadow"))
	for _, blob := range []string{pw, gr, sh} {
		if strings.Contains(blob, "builder") {
			t.Fatalf("builder 条目应被删除:\n%s", blob)
		}
	}
	if !strings.Contains(pw, "root:x:0:0") ||
		!strings.Contains(pw, "li:x:1000:1000") {
		t.Fatalf("其余条目应原样保留:\n%s", pw)
	}
	if !strings.Contains(gr, "wheel:x:10:") {
		t.Fatalf("无关组丢失:\n%s", gr)
	}
}

func TestFilterHumanUsersTable(t *testing.T) {
	db := colonDB{
		"root":    {"root", "x", "0", "0", "root", "/root", "/bin/bash"},
		"http":    {"http", "x", "33", "33", "", "/", "/usr/bin/nologin"},
		"svc999":  {"svc999", "x", "999", "999", "", "/srv", "/bin/false"},
		"lianxin": {"lianxin", "x", "1000", "1000", "", "/home/lianxin", "/bin/zsh"},
		"noshell": {"noshell", "x", "1001", "1001", "", "/home/noshell", "/sbin/nologin"},
		"nohome":  {"nohome", "x", "1002", "1002", "", "/home/absent", "/bin/bash"},
		"baduid":  {"baduid", "x", "NaN", "0", "", "/home/baduid", "/bin/bash"},
		"user_b":  {"user_b", "x", "60000", "1005", "", "/home/user_b", "/usr/bin/bash"},
		"beyond":  {"beyond", "x", "60001", "1006", "", "/home/beyond", "/bin/bash"},
	}
	exists := func(p string) bool { return p != "/home/absent" }
	got := filterHumanUsers(db, 1000, 60000, exists)
	want := []string{"lianxin", "user_b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("filterHumanUsers = %v, want %v", got, want)
	}
}

func TestParseLoginDefsUIDRange(t *testing.T) {
	min, max := parseLoginDefs("SYS_UID_MIN   500\n# 注释\nUID_MIN\t\t 1000\nUID_MAX 60000\n垃圾行\n")
	if min != 1000 || max != 60000 {
		t.Fatalf("min=%d max=%d", min, max)
	}
	min, max = parseLoginDefs("完全无关的内容")
	if min != 1000 || max != 60000 {
		t.Fatalf("缺省回退应为 1000..60000: min=%d max=%d", min, max)
	}
}

type nilRunner struct{}

func (nilRunner) Run(name string, _ ...string) error          { return nil }
func (nilRunner) RunOutput(string, ...string) (string, error) { return "", nil }

func TestBakeServicesCapturesFailureContext(t *testing.T) {
	b := New(failRunner{}, &strings.Builder{}, t.TempDir())
	err := b.bakeServices(t.TempDir(), []string{"nope.service"})
	if err == nil || !strings.Contains(err.Error(), "nope.service") {
		t.Fatalf("服务启用失败应带上下文: %v", err)
	}
}

type failRunner struct{}

func (failRunner) Run(string, ...string) error { return os.ErrInvalid }
func (failRunner) RunOutput(string, ...string) (string, error) {
	return "", os.ErrInvalid
}

// readFile 小工具。
func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

var _ = state.ExportEntry{}
