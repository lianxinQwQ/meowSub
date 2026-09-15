package graphical

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

// withRoots 把包级系统路径重定向到临时目录。
func withRoots(t *testing.T) (x11, runUserDir string) {
	t.Helper()
	x11 = filepath.Join(t.TempDir(), ".X11-unix")
	runUserDir = filepath.Join(t.TempDir(), "user")
	oldX, oldU := x11Dir, runUser
	x11Dir, runUser = x11, runUserDir
	t.Cleanup(func() { x11Dir, runUser = oldX, oldU })
	return x11, runUserDir
}

// scanFind 取 Scan 结果中指定路径的项。
func scanFind(t *testing.T, path string) Bind {
	t.Helper()
	for _, b := range Scan() {
		if b.Path == path {
			return b
		}
	}
	t.Fatalf("Scan 未扫出 %s：%v", path, Scan())
	return Bind{}
}

// assertIdentity 校验记录的身份与当前实际 stat 一致。
func assertIdentity(t *testing.T, b Bind) {
	t.Helper()
	fi, err := os.Stat(b.Path)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if b.Dev != uint64(st.Dev) || b.Ino != st.Ino {
		t.Fatalf("%s 身份不符：记录 (%d,%d)，实际 (%d,%d)",
			b.Path, b.Dev, b.Ino, st.Dev, st.Ino)
	}
}

func TestScanAndBinds(t *testing.T) {
	x11, users := withRoots(t)

	if got := Scan(); len(got) != 0 {
		t.Fatalf("宿主无图形资源时应得空结果，got %v", got)
	}
	if err := os.MkdirAll(x11, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"1000", "42"} {
		if err := os.MkdirAll(filepath.Join(users, uid), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 非数字与非目录一律忽略
	if err := os.MkdirAll(filepath.Join(users, "ignore"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(users, "777"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	scan := Scan()
	if len(scan) != 3 {
		t.Fatalf("应扫出 X11 目录与两个运行时目录，got %v", scan)
	}
	if scan[0].Kind != "x11" || scan[0].Path != x11 {
		t.Fatalf("X11 目录应排首且 Kind=x11，got %+v", scan[0])
	}
	assertIdentity(t, scan[0])
	for _, b := range scan[1:] {
		if b.Kind != "runtime" {
			t.Fatalf("运行时目录 Kind 应为 runtime：%+v", b)
		}
		assertIdentity(t, b)
	}
}

// TestPlanDirClassifies 直递口径：顶层套接字逐绑、含套接字的子目录整绑、
// 符号链接重建；systemd/varlink/dconf 排除，lock 与普通文件跳过。
func TestPlanDirClassifies(t *testing.T) {
	_, users := withRoots(t)
	rt := filepath.Join(users, "1000")
	// Go 的 unix listener 在 Close 时会摘除 socket 文件，故持有到测试结束
	var listeners []net.Listener
	t.Cleanup(func() {
		for _, li := range listeners {
			li.Close()
		}
	})
	mkSocket := func(p string) {
		t.Helper()
		li, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, li)
	}
	for _, d := range []string{"keyring", "at-spi", "systemd", "dconf"} {
		if err := os.MkdirAll(filepath.Join(rt, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mkSocket(filepath.Join(rt, "wayland-1"))
	mkSocket(filepath.Join(rt, "bus"))
	mkSocket(filepath.Join(rt, "keyring", "control"))
	if err := os.Symlink("pipewire-0", filepath.Join(rt, "pulse")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rt, "wayland-1.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rt, "notes.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	entries, links := PlanDir(rt)

	wantEntries := []Entry{
		{Host: filepath.Join(rt, "at-spi"), Container: filepath.Join(rt, "at-spi"), Kind: KindDir},
		{Host: filepath.Join(rt, "bus"), Container: filepath.Join(rt, "bus"), Kind: KindSocket},
		{Host: filepath.Join(rt, "keyring"), Container: filepath.Join(rt, "keyring"), Kind: KindDir},
		{Host: filepath.Join(rt, "wayland-1"), Container: filepath.Join(rt, "wayland-1"), Kind: KindSocket},
	}
	if !slices.Equal(entries, wantEntries) {
		t.Fatalf("entries 不符:\n got  %v\n want %v", entries, wantEntries)
	}
	wantLinks := []Link{{Path: filepath.Join(rt, "pulse"), Target: "pipewire-0"}}
	if !slices.Equal(links, wantLinks) {
		t.Fatalf("links 不符: got %v want %v", links, wantLinks)
	}

	// 不存在的目录：空结果不报错
	entries, links = PlanDir(filepath.Join(users, "404"))
	if entries != nil || links != nil {
		t.Fatalf("缺失目录应得空结果: %v %v", entries, links)
	}
}

// 目录被删除重建（logout/login 周期换 tmpfs 实例）后身份必须变化——
// 这是运行期判定旧 bind 钉在死实例上的依据。
func TestScanIdentityChangesOnRecreate(t *testing.T) {
	_, users := withRoots(t)
	dir := filepath.Join(users, "1000")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	first := scanFind(t, dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	second := scanFind(t, dir)
	if first.Dev == second.Dev && first.Ino == second.Ino {
		t.Fatalf("目录重建后实例身份应变化：(%d,%d) 不变",
			first.Dev, first.Ino)
	}
}

func TestX11DirAndRuntimeDir(t *testing.T) {
	x11, users := withRoots(t)
	if X11Dir() != x11 {
		t.Fatalf("X11Dir 未随测试重定向: %s", X11Dir())
	}
	if RuntimeDir(1000) != filepath.Join(users, "1000") {
		t.Fatalf("RuntimeDir 未随测试重定向: %s", RuntimeDir(1000))
	}
}

func TestEnvArgsForwardOnly(t *testing.T) {
	if got := EnvArgs(nil); len(got) != 0 {
		t.Fatalf("空环境应不注入任何变量，got %v", got)
	}
	got := EnvArgs(map[string]string{
		"XDG_RUNTIME_DIR":          "/run/user/1000",
		"WAYLAND_DISPLAY":          "wayland-1",
		"DISPLAY":                  ":0",
		"DBUS_SESSION_BUS_ADDRESS": "unix:path=/custom/bus",
	})
	want := []string{
		"--setenv=XDG_RUNTIME_DIR=/run/user/1000",
		"--setenv=WAYLAND_DISPLAY=wayland-1",
		"--setenv=DISPLAY=:0",
		"--setenv=DBUS_SESSION_BUS_ADDRESS=unix:path=/custom/bus",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// 缺失即缺席：不猜 wayland-0/:0，不构造 XDG_RUNTIME_DIR；dbus 仅在运行时
// 目录已透传时按客户端标准回退推导。
func TestEnvArgsNoFallbackGuesses(t *testing.T) {
	got := EnvArgs(map[string]string{"XDG_RUNTIME_DIR": "/run/user/7"})
	want := []string{
		"--setenv=XDG_RUNTIME_DIR=/run/user/7",
		"--setenv=DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/7/bus",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := EnvArgs(map[string]string{"DISPLAY": ":1"}); !slices.Equal(got,
		[]string{"--setenv=DISPLAY=:1"}) {
		t.Fatalf("无运行时目录时不应推导 dbus，got %v", got)
	}
}
