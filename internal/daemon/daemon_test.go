package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"meowsub/internal/config"
	"meowsub/internal/graphical"
)

func cfgFixture(t *testing.T, base string, groups ...config.Group) *config.Config {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(base, "build", "code", "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{BaseDir: base}
	for _, g := range groups {
		cfg.Groups = append(cfg.Groups, &config.Group{Name: g.Name,
			Packages: g.Packages, Services: g.Services,
			Export: g.Export, Autostart: g.Autostart,
			SessionMode: g.SessionMode, SessionUser: g.SessionUser,
			SessionAccess: g.SessionAccess})
	}
	return cfg
}

// newExec 受控进程：ok=true 长驻（模拟引导），false 立即退出。
func newExec(ok bool) *exec.Cmd {
	if ok {
		return exec.Command("true")
	}
	return exec.Command("false")
}

// recordingExec 按命令名记录调用并返回受控进程：
// machinectl status → 失败（未运行）；nspawn 引导 → 长驻（sleep）；
// machinectl shell/poweroff → 成功。shell 非 nil 时 shell 命令改由它
// 产出受控进程（验证桥接回显与 -c 输出捕获）。
// probeOut 控制容器内视图探活的回显：空串 = 探测失败，"rc=1" = 视图
// 丢失，"rc=0" = 视图健在。预热保活会话开启后挂载点探针恒报 rc=0。
type recordingExec struct {
	mu       sync.Mutex
	calls    []string
	tb       *Table
	shell    func() *exec.Cmd
	booted   bool   // systemd-nspawn 引导后即视为已在 machined 登记
	probeOut string // 容器内视图探活回显
	warmed   bool   // 预热保活会话已开启（logind tmpfs 视为就位）
}

// callsSnapshot 竞争安全读取。
func (r *recordingExec) callsSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.calls...)
}

func (r *recordingExec) Exec(name string, args ...string) *exec.Cmd {
	r.mu.Lock()
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if name == "systemd-nspawn" {
		r.booted = true
	}
	booted := r.booted
	probeOut := r.probeOut
	warmed := r.warmed
	r.mu.Unlock()
	switch {
	case name == "machinectl" && len(args) > 0 && args[0] == "status":
		return newExec(booted) // 引导后 machined 才认识该机器
	case name == "machinectl" && slices.ContainsFunc(args,
		func(a string) bool { return strings.Contains(a, "[ -e ") }):
		// 模拟可见性探针协议（sockets 视图）：rc=$? 尾行
		if probeOut == "" {
			return newExec(false) // 探测失败
		}
		return exec.Command("/bin/echo", probeOut)
	case name == "machinectl" && slices.ContainsFunc(args,
		func(a string) bool { return strings.Contains(a, "/usr/bin/findmnt") }):
		// 模拟挂载点探针（rw 视图 / 预热等待）：保活会话开启后 tmpfs 就位
		if warmed {
			return exec.Command("/bin/echo", "rc=0")
		}
		if probeOut == "" {
			return newExec(false) // 探测失败
		}
		return exec.Command("/bin/echo", probeOut)
	case name == "machinectl" && len(args) > 0 && args[0] == "shell" &&
		slices.Contains(args, "/bin/sleep"):
		r.mu.Lock()
		r.warmed = true
		r.mu.Unlock()
		return exec.Command("sleep", "5") // 保活会话，由停止函数即刻回收
	case name == "machinectl" && len(args) > 0 && args[0] == "bind":
		return newExec(true) // 请求期补挂：视 machined 总能受理
	case name == "machinectl" && len(args) > 0 && args[0] == "shell" &&
		len(args) > 1 && args[len(args)-1] == "/bin/true":
		return newExec(true) // 就绪探针：视容器随时可受理会话
	case name == "systemd-nspawn":
		return exec.Command("sleep", "5") // 模拟长驻：即刻退出会触发死亡簿记
	case name == "machinectl" && len(args) > 0 && args[0] == "shell" &&
		r.shell != nil:
		return r.shell()
	default:
		return newExec(false)
	}
}

// TestSessionBootArgsModes 引导参数按 session_mode 分流：sockets 逐条目
// 只读直递（符号链接不出现在 bind 参数里）、rw 整目录读写直挂、off 仅
// X11。X11 恒为只读直绑。
func TestSessionBootArgsModes(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "keyring"), 0o700); err != nil {
		t.Fatal(err)
	}
	li, err := net.Listen("unix", filepath.Join(dir, "wayland-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer li.Close()
	if err := os.Symlink("pipewire-0", filepath.Join(dir, "pulse")); err != nil {
		t.Fatal(err)
	}
	scan := []graphical.Bind{
		{Kind: "x11", Path: "/tmp/.X11-unix"},
		{Kind: "runtime", Path: dir},
	}
	mk := func(mode string) *config.Config {
		return &config.Config{Groups: []*config.Group{
			{Name: "code", SessionMode: mode}}}
	}

	sockets := sessionBootArgs(mk("sockets"), "code", scan)
	wantSockets := []string{
		"--bind-ro=/tmp/.X11-unix",
		"--bind-ro=" + filepath.Join(dir, "keyring") + ":" + filepath.Join(dir, "keyring"),
		"--bind-ro=" + filepath.Join(dir, "wayland-1") + ":" + filepath.Join(dir, "wayland-1"),
	}
	if !slices.Equal(sockets, wantSockets) {
		t.Fatalf("sockets 不符:\n got  %v\n want %v", sockets, wantSockets)
	}

	rw := sessionBootArgs(mk("rw"), "code", scan)
	wantRW := []string{"--bind-ro=/tmp/.X11-unix", "--bind=" + dir}
	if !slices.Equal(rw, wantRW) {
		t.Fatalf("rw 不符:\n got  %v\n want %v", rw, wantRW)
	}

	off := sessionBootArgs(mk("off"), "code", scan)
	if !slices.Equal(off, []string{"--bind-ro=/tmp/.X11-unix"}) {
		t.Fatalf("off 不符: %v", off)
	}
}

// TestTmpfsBootArgs run_tmp_size 解析：组级独立生效（<实例>@<组名> 同样
// 命中）；组不可考或未声明不注入（无全局回退，0 = 沿用 nspawn 默认）。
func TestTmpfsBootArgs(t *testing.T) {
	cfg := &config.Config{
		Groups: []*config.Group{
			{Name: "code", RunTmpSize: 8 << 30},
			{Name: "dev", BuildTmpSize: 2 << 30}, // 构建期值不影响运行时
		},
	}
	if got := tmpfsBootArgs(cfg, "code"); !slices.Equal(got, []string{"--tmpfs=/tmp:size=8G"}) {
		t.Errorf("组级 run_tmp_size 不符: %v", got)
	}
	if got := tmpfsBootArgs(cfg, "mydev@code"); !slices.Equal(got, []string{"--tmpfs=/tmp:size=8G"}) {
		t.Errorf("实例@组名 应命中组配置: %v", got)
	}
	if got := tmpfsBootArgs(cfg, "dev"); got != nil {
		t.Errorf("只声明 build_tmp_size 的组运行期不注入: %v", got)
	}
	if got := tmpfsBootArgs(cfg, "ghost"); got != nil {
		t.Errorf("组不可考不注入: %v", got)
	}
	if got := tmpfsBootArgs(nil, "code"); got != nil {
		t.Errorf("nil 配置不注入: %v", got)
	}
}

// TestSessionAccessGate session_access 组级门禁：all 恒通行、root 仅
// root、特定 uid 仅该 uid（root 恒放行）。
func TestSessionAccessGate(t *testing.T) {
	cfg := cfgFixture(t, t.TempDir(), config.Group{Name: "code"})
	cfg.Groups[0].SessionAccess = "root"
	s := NewServer(cfg)
	if err := s.authorize("code", 1000); err == nil {
		t.Fatal("root 门禁应拒绝普通用户")
	}
	if err := s.authorize("code", 0); err != nil {
		t.Fatalf("root 门禁应放行 root: %v", err)
	}

	cfg.Groups[0].SessionAccess = "1000"
	if err := s.authorize("code", 1000); err != nil {
		t.Fatalf("特定 uid 门禁应放行本人: %v", err)
	}
	if err := s.authorize("code", 1001); err == nil {
		t.Fatal("特定 uid 门禁应拒绝他人")
	}
	if err := s.authorize("code", 0); err != nil {
		t.Fatalf("特定 uid 门禁应放行 root: %v", err)
	}
}

// TestSessionUIDFor session_user 组级配置：caller 复用调用者，固定值覆盖。
func TestSessionUIDFor(t *testing.T) {
	cfg := cfgFixture(t, t.TempDir(), config.Group{Name: "code"})
	cfg.Groups[0].SessionUser = "1000"
	s := NewServer(cfg)
	if uid := s.sessionUIDFor("code", 4242); uid != 1000 {
		t.Fatalf("固定 uid 应覆盖调用者: %d", uid)
	}
	if uid := s.sessionUIDFor("ghost", 4242); uid != 4242 {
		t.Fatalf("未知组应回退调用者: %d", uid)
	}
}

// countCalls 统计匹配谓词的调用数。
func countCalls(calls []string, match func(string) bool) int {
	n := 0
	for _, c := range calls {
		if match(c) {
			n++
		}
	}
	return n
}

// assertSocketsRedeliver 验证一次 sockets 重递的形态：有条目 bind、有
// 预热保活会话、绝不惰性卸载运行时目录本体（那是容器 logind 的私有
// tmpfs，摘除会打断容器内会话）。
func assertSocketsRedeliver(t *testing.T, calls []string) {
	t.Helper()
	if countCalls(calls, func(c string) bool {
		return strings.HasPrefix(c, "machinectl bind")
	}) == 0 {
		t.Fatalf("重递应产生条目 bind: %v", calls)
	}
	if countCalls(calls, func(c string) bool {
		return strings.HasPrefix(c, "machinectl shell") &&
			slices.Contains(strings.Fields(c), "/bin/sleep")
	}) == 0 {
		t.Fatalf("重递前应预热保活会话: %v", calls)
	}
	for _, c := range calls {
		if strings.Contains(c, "umount -l '/run/user/1000'") {
			t.Fatalf("sockets 重递不得卸载运行时目录本体: %s", c)
		}
	}
}

// TestEnsureSessionRebindsWhenContainerViewLost 身份已登记但容器内视图
// 不可见（被 logind 私有 tmpfs 遮蔽或拆除）时，探活应触发重新直递；视图
// 健在时不得重复直递；探测失败按不可用处理，留痕跳过。
func TestEnsureSessionRebindsWhenContainerViewLost(t *testing.T) {
	if _, err := os.Stat("/run/user/1000"); err != nil {
		t.Skip("宿主无 /run/user/1000，无可探活的运行时目录")
	}
	cfg := cfgFixture(t, t.TempDir())
	rec := &recordingExec{booted: true}
	s := NewServer(cfg)
	s.Exec = rec.Exec

	if resp := s.Handle(context.Background(), Request{Op: OpStart, Group: "code", CallerUID: 0}); !resp.OK {
		t.Fatalf("start 失败: %+v", resp)
	}
	req := Request{Op: OpShell, Group: "code", CallerUID: 1000,
		Cmd: "/bin/bash", Args: []string{"-lc", "echo hi"}}
	if resp := s.Handle(context.Background(), req); !resp.OK {
		t.Fatalf("首次 shell 应成功: %+v", resp)
	}
	countBinds := func() int {
		return countCalls(rec.callsSnapshot(), func(c string) bool {
			return strings.HasPrefix(c, "machinectl bind")
		})
	}
	first := countBinds()
	if first == 0 {
		t.Fatalf("无登记时应清场补挂: %v", rec.callsSnapshot())
	}
	assertSocketsRedeliver(t, rec.callsSnapshot())

	// 视图不可见：探活 rc=1 → 重新直递（一次直递 = 多条 bind）
	rec.probeOut = "rc=1"
	if resp := s.Handle(context.Background(), req); !resp.OK {
		t.Fatalf("视图丢失后 shell 应成功: %+v", resp)
	}
	after := countBinds()
	if after <= first {
		t.Fatalf("视图丢失应触发重新直递: %d → %d", first, after)
	}

	// 视图健在：不再直递
	rec.probeOut = "rc=0"
	if resp := s.Handle(context.Background(), req); !resp.OK {
		t.Fatalf("视图健在时 shell 应成功: %+v", resp)
	}
	if again := countBinds(); again != after {
		t.Fatalf("视图健在不应重复直递: %d → %d", after, again)
	}
}

// TestEnsureSessionSeededProbeSkipsWhenAlive 引导登记身份且容器内视图健
// 在时，请求期只探活不直递；视图丢失才重递。
func TestEnsureSessionSeededProbeSkipsWhenAlive(t *testing.T) {
	if _, err := os.Stat("/run/user/1000"); err != nil {
		t.Skip("宿主无 /run/user/1000，无可探活的运行时目录")
	}
	cfg := cfgFixture(t, t.TempDir())
	rec := &recordingExec{} // booted=false：start 走真实引导路径并登记身份
	s := NewServer(cfg)
	s.Exec = rec.Exec
	if resp := s.Handle(context.Background(), Request{Op: OpStart, Group: "code", CallerUID: 0}); !resp.OK {
		t.Fatalf("start 失败: %+v", resp)
	}
	s.mu.Lock()
	seeded := len(s.binds["code"]) > 0
	s.mu.Unlock()
	if !seeded {
		t.Fatalf("引导应登记会话身份: %v", rec.callsSnapshot())
	}
	req := Request{Op: OpShell, Group: "code", CallerUID: 1000,
		Cmd: "/bin/bash", Args: []string{"-lc", "echo hi"}}

	// 视图健在：零 bind
	rec.probeOut = "rc=0"
	if resp := s.Handle(context.Background(), req); !resp.OK {
		t.Fatalf("视图健在时 shell 应成功: %+v", resp)
	}
	if n := countCalls(rec.callsSnapshot(), func(c string) bool {
		return strings.HasPrefix(c, "machinectl bind")
	}); n != 0 {
		t.Fatalf("视图健在不应激活直递: %d 次 bind", n)
	}

	// 视图丢失：重递且形态合规
	rec.probeOut = "rc=1"
	if resp := s.Handle(context.Background(), req); !resp.OK {
		t.Fatalf("视图丢失后 shell 应成功: %+v", resp)
	}
	assertSocketsRedeliver(t, rec.callsSnapshot())
}

// TestEnsureSessionRWWholeDirRedeliver rw 方案按整目录直挂补挂，不预热、
// 不逐条目：目录不在位（探活 rc=1）时重挂，健在时不重复。
func TestEnsureSessionRWWholeDirRedeliver(t *testing.T) {
	if _, err := os.Stat("/run/user/1000"); err != nil {
		t.Skip("宿主无 /run/user/1000，无可探活的运行时目录")
	}
	cfg := cfgFixture(t, t.TempDir(), config.Group{Name: "code",
		SessionAccess: "all"})
	cfg.Groups[0].SessionMode = "rw"
	rec := &recordingExec{booted: true}
	s := NewServer(cfg)
	s.Exec = rec.Exec
	if resp := s.Handle(context.Background(), Request{Op: OpStart, Group: "code", CallerUID: 0}); !resp.OK {
		t.Fatalf("start 失败: %+v", resp)
	}
	req := Request{Op: OpShell, Group: "code", CallerUID: 1000,
		Cmd: "/bin/bash", Args: []string{"-lc", "echo hi"}}
	if resp := s.Handle(context.Background(), req); !resp.OK {
		t.Fatalf("首次 shell 应成功: %+v", resp)
	}
	runtimeBinds := func() int {
		return countCalls(rec.callsSnapshot(), func(c string) bool {
			return strings.HasPrefix(c, "machinectl bind") &&
				strings.Contains(c, " /run/user/1000")
		})
	}
	first := runtimeBinds()
	if first != 1 {
		t.Fatalf("rw 无登记时应整目录直挂一次: %d", first)
	}
	for _, c := range rec.callsSnapshot() {
		if slices.Contains(strings.Fields(c), "/bin/sleep") {
			t.Fatalf("rw 方案不应预热保活会话: %s", c)
		}
	}

	rec.probeOut = "rc=0"
	if resp := s.Handle(context.Background(), req); !resp.OK {
		t.Fatalf("视图健在时 shell 应成功: %+v", resp)
	}
	if n := runtimeBinds(); n != first {
		t.Fatalf("rw 视图健在不应重复直挂: %d → %d", first, n)
	}

	rec.probeOut = "rc=1"
	if resp := s.Handle(context.Background(), req); !resp.OK {
		t.Fatalf("视图丢失后 shell 应成功: %+v", resp)
	}
	if n := runtimeBinds(); n != first+1 {
		t.Fatalf("rw 视图丢失应整目录重挂一次: %d → %d", first, n)
	}
}

func TestOccupancyMatrix(t *testing.T) {
	tbl := NewTable()
	s := tbl.Ensure("mydev", "code", OriginDemand)

	tbl.Hold("mydev")
	if s.ShouldAutoStop() {
		t.Fatal("有持有者不应回收")
	}
	tbl.Release("mydev")
	if !s.ShouldAutoStop() {
		t.Fatal("隐式实例清零应回收")
	}

	e := tbl.Ensure("sv", "tool", OriginExplicit)
	tbl.Release("sv")
	if e.ShouldAutoStop() {
		t.Fatal("显式出身不因清零回收")
	}
	a := tbl.Ensure("auto", "dev", OriginAutostart)
	tbl.Release("auto")
	if a.ShouldAutoStop() {
		t.Fatal("autostart 出身永不自动停")
	}
}

func TestLifecycleRequiresRootCaller(t *testing.T) {
	cfg := cfgFixture(t, t.TempDir())
	s := NewServer(cfg)
	for _, op := range []Op{OpStart, OpStop, OpPs} {
		resp := s.Handle(context.Background(), Request{Op: op, Group: "code", CallerUID: 1000})
		if resp.OK || !strings.Contains(resp.Error, "root") {
			t.Fatalf("%s 非 root 应拒绝: %+v", op, resp)
		}
	}
}

func TestHandleOpStartThenEntryReusesMachine(t *testing.T) {
	base := t.TempDir()
	cfg := cfgFixture(t, base)
	rec := &recordingExec{}
	s := NewServer(cfg)
	s.Exec = rec.Exec

	// start：触发隐式部署 + 引导
	resp := s.Handle(context.Background(), Request{Op: OpStart, Group: "code", CallerUID: 0})
	if !resp.OK {
		t.Fatalf("start 失败: %+v", resp)
	}
	sn, ok := s.tb.Get("code")
	if !ok || sn.Origin != OriginExplicit {
		t.Fatalf("出身应为 explicit: %+v", sn)
	}
	foundBoot := false
	for _, c := range rec.callsSnapshot() {
		if strings.HasPrefix(c, "systemd-nspawn --boot") &&
			strings.Contains(c, "ms-code") {
			foundBoot = true
		}
	}
	if !foundBoot {
		t.Fatalf("未见引导调用: %v", rec.calls)
	}
}

// TestLaunchWaitRunsSync Wait 请求同步执行：machinectl 的退出码随响应
// 回传（前台阻塞语义），而非提交即忘。
func TestLaunchWaitRunsSync(t *testing.T) {
	base := t.TempDir()
	cfg := cfgFixture(t, base)
	rec := &recordingExec{}
	s := NewServer(cfg)
	s.Exec = rec.Exec

	bin := filepath.Join(base, "runs", "code", "usr", "bin", "hello")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	resp := s.Handle(context.Background(), Request{Op: OpLaunchEntry,
		Group: "code", Cmd: "hello", CallerUID: 1000, Wait: true})
	if !resp.OK {
		t.Fatalf("wait launch 失败: %+v", resp)
	}
	if d, _ := resp.Data.(string); !strings.Contains(d, "退出码") {
		t.Fatalf("响应应回传退出码: %+v", resp)
	}
	found := false
	for _, c := range rec.callsSnapshot() {
		if strings.HasPrefix(c, "machinectl shell") &&
			strings.Contains(c, "hello") {
			found = true
		}
	}
	if !found {
		t.Fatalf("未见 machinectl shell 调用: %v", rec.calls)
	}
}

func TestDemandInstanceAutoStopsOnZeroHolders(t *testing.T) {
	base := t.TempDir()
	cfg := cfgFixture(t, base)
	rec := &recordingExec{}
	s := NewServer(cfg)
	s.Exec = rec.Exec

	resp := s.Handle(context.Background(), Request{Op: OpLaunchEntry, Group: "code",
		Cmd: "vim", Args: []string{"-v"}})
	if !resp.OK {
		t.Fatalf("launch 失败: %+v", resp)
	}
	foundStop := func() bool {
		for _, c := range rec.callsSnapshot() {
			if strings.HasPrefix(c, "machinectl poweroff ms-code") {
				return true
			}
		}
		return false
	}
	// 回收发生在异步协程：轮询至登记表清空且 poweroff 出现
	deadline := time.Now().Add(time.Second)
	for {
		_, still := s.tb.Get("code")
		if !still && foundStop() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("未见归零回收: 表=%v 调用=%v", s.tb.Snapshot(), rec.callsSnapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPsEmptyAndAuthDenied(t *testing.T) {
	s := NewServer(cfgFixture(t, t.TempDir()))
	resp := s.Handle(context.Background(), Request{Op: OpPs})
	if !resp.OK {
		t.Fatal("ps 应成功")
	}
	if _, err := Dial("/nonexistent.sock", Request{}, 0); err == nil {
		t.Log("无 socket 的连接错误形态：", err)
	} else if !errors.Is(err, os.ErrNotExist) && os.IsPermission(err) {
		t.Fatal("意外权限错误")
	}
}

func TestShellValidation(t *testing.T) {
	s := NewServer(cfgFixture(t, t.TempDir()))
	resp := s.Handle(context.Background(), Request{Op: OpShell, Group: "code"})
	if resp.OK || !strings.Contains(resp.Error, "-c") {
		t.Fatalf("非交互空 Cmd 应被拒绝: %+v", resp)
	}
	resp = s.Handle(context.Background(), Request{Op: OpShell, Group: "code", Interactive: true})
	if resp.OK || !strings.Contains(resp.Error, "桥接") {
		t.Fatalf("Handle 不应受理交互请求（由连接层分流）: %+v", resp)
	}
}

func TestShellNonInteractiveReturnsOutput(t *testing.T) {
	cfg := cfgFixture(t, t.TempDir())
	rec := &recordingExec{shell: func() *exec.Cmd {
		return exec.Command("echo", "hello-from-instance")
	}}
	s := NewServer(cfg)
	s.Exec = rec.Exec

	resp := s.Handle(context.Background(), Request{Op: OpShell, Group: "code",
		Cmd: "/bin/bash", Args: []string{"-lc", "echo hi"}})
	if !resp.OK {
		t.Fatalf("shell -c 应成功: %+v", resp)
	}
	if data, _ := resp.Data.(string); !strings.Contains(data, "hello-from-instance") {
		t.Fatalf("输出未随响应回传: %+v", resp)
	}
	// demand 实例在同步路径上立即回收
	if _, still := s.tb.Get("code"); still {
		t.Fatal("同步执行应释放占用")
	}
	for _, c := range rec.callsSnapshot() {
		if strings.HasPrefix(c, "machinectl poweroff ms-code") {
			return
		}
	}
	t.Fatalf("未见归零回收: %v", rec.callsSnapshot())
}

func TestShellNonInteractiveReportsExitCode(t *testing.T) {
	cfg := cfgFixture(t, t.TempDir())
	rec := &recordingExec{shell: func() *exec.Cmd {
		return exec.Command("false")
	}}
	s := NewServer(cfg)
	s.Exec = rec.Exec

	resp := s.Handle(context.Background(), Request{Op: OpShell, Group: "code",
		Cmd: "/bin/bash", Args: []string{"-lc", "false"}})
	if !resp.OK {
		t.Fatalf("非零退出不应整体失败: %+v", resp)
	}
	if data, _ := resp.Data.(string); !strings.Contains(data, "[退出码 1]") {
		t.Fatalf("退出码未随输出返回: %+v", resp)
	}
}

// TestEnsureSessionLateGraphics 覆盖容器非本 daemon 引导（外部拉起或
// daemon 重启）时的请求期补挂：无身份记录 → 清场并 machinectl bind，
// 且仅首个请求触发，身份登记后后续请求不再重复。
func TestEnsureSessionLateGraphics(t *testing.T) {
	if _, err := os.Stat("/tmp/.X11-unix"); err != nil {
		t.Skip("宿主无 X11 套接字目录，无资源可补挂")
	}
	cfg := cfgFixture(t, t.TempDir())
	rec := &recordingExec{booted: true} // 机器已在 machined 登记，非本 daemon 引导
	s := NewServer(cfg)
	s.Exec = rec.Exec

	// 先以 root 登记显式出身（机器已在跑，直接复用不引导），避免 demand
	// 实例归零回收干扰两次请求之间的缓存比对。
	if resp := s.Handle(context.Background(), Request{Op: OpStart, Group: "code", CallerUID: 0}); !resp.OK {
		t.Fatalf("start 失败: %+v", resp)
	}
	req := Request{Op: OpShell, Group: "code", CallerUID: 1000,
		Cmd: "/bin/bash", Args: []string{"-lc", "echo hi"},
		Env: map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000",
			"WAYLAND_DISPLAY": "wayland-9"}}
	if resp := s.Handle(context.Background(), req); !resp.OK {
		t.Fatalf("shell -c 应成功: %+v", resp)
	}
	countBinds := func() int {
		n := 0
		for _, c := range rec.callsSnapshot() {
			if strings.HasPrefix(c, "machinectl bind") {
				n++
			}
		}
		return n
	}
	first := countBinds()
	if first == 0 {
		t.Fatalf("外部引导的实例应触发请求期补挂: %v", rec.callsSnapshot())
	}
	// 透传的会话变量应出现在 shell 调用里，且不出现兜底猜测（请求未带
	// DISPLAY/WAYLAND_DISPLAY 时 daemon 不得自行构造）
	var sawEnv bool
	for _, c := range rec.callsSnapshot() {
		if strings.HasPrefix(c, "machinectl shell") &&
			strings.Contains(c, "--setenv=WAYLAND_DISPLAY=wayland-9") {
			sawEnv = true
		}
		if strings.Contains(c, "wayland-0") ||
			strings.Contains(c, "--setenv=DISPLAY=") {
			t.Fatalf("不应出现兜底猜测: %s", c)
		}
	}
	if !sawEnv {
		t.Fatalf("透传变量未注入: %v", rec.callsSnapshot())
	}

	// 第二次请求前声明容器内视图健在（探活 rc=0）：
	// 身份已登记 + 视图健在 => 不应重复补挂。
	rec.probeOut = "rc=0"
	if resp := s.Handle(context.Background(), req); !resp.OK {
		t.Fatalf("第二次 shell -c 应成功: %+v", resp)
	}
	if again := countBinds(); again != first {
		t.Fatalf("身份已登记后不应重复补挂: %d → %d", first, again)
	}
}

// TestInteractiveBridge 走真实 unix socket：以 /bin/cat 充当 machinectl
// shell，验证握手 JSON 行、双向字节透传与会话结束后的 demand 回收。
func TestInteractiveBridge(t *testing.T) {
	base := t.TempDir()
	cfg := cfgFixture(t, base)
	rec := &recordingExec{shell: func() *exec.Cmd {
		return exec.Command("cat")
	}}
	s := NewServer(cfg)
	s.Exec = rec.Exec

	sock := filepath.Join(base, "test.sock")
	old := SocketPath
	SocketPath = sock // 服务与客户端统一指向临时 socket，勿触真实守护进程
	defer func() { SocketPath = old }()
	go func() { _ = ServeUnix(sock, s.Handle, s.bridgeShell) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket 未就绪")
		}
		time.Sleep(2 * time.Millisecond)
	}

	conn, hs, err := OpenInteractive(Request{Op: OpShell, Group: "code",
		Interactive: true, Term: "xterm-256color", Rows: 40, Cols: 120,
		Env: map[string]string{"WAYLAND_DISPLAY": "wayland-2"}})
	if err != nil {
		t.Fatalf("交互握手失败: %v", err)
	}
	defer conn.Close()
	if s, _ := hs.Data.(string); !strings.Contains(s, "code") {
		t.Fatalf("握手信息缺失: %+v", hs)
	}

	// machinectl shell 参数：-q 静默、TERM 注入、透传会话变量、容器内
	// stty 尺寸、目标机器
	var sawShell bool
	for _, c := range rec.callsSnapshot() {
		if strings.HasPrefix(c, "machinectl shell") &&
			strings.Contains(c, " -q ") &&
			strings.Contains(c, "--setenv=TERM=xterm-256color") &&
			strings.Contains(c, "--setenv=WAYLAND_DISPLAY=wayland-2") &&
			strings.Contains(c, "stty rows 40 cols 120") &&
			strings.Contains(c, "ms-code") {
			sawShell = true
		}
	}
	if !sawShell {
		t.Fatalf("machinectl shell 参数不符: %v", rec.callsSnapshot())
	}

	// 字节透传：写入什么便回显什么
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("读取回显失败: %v", err)
	}
	if string(buf) != "ping\n" {
		t.Fatalf("回显不符: %q", buf)
	}

	// 客户端断开 → shell 退出 → demand 实例清零回收
	conn.Close()
	foundStop := func() bool {
		for _, c := range rec.callsSnapshot() {
			if strings.HasPrefix(c, "machinectl poweroff ms-code") {
				return true
			}
		}
		return false
	}
	dl := time.Now().Add(2 * time.Second)
	for {
		if _, still := s.tb.Get("code"); !still && foundStop() {
			return
		}
		if time.Now().After(dl) {
			t.Fatalf("未见会话结束回收: 表=%v 调用=%v",
				s.tb.Snapshot(), rec.callsSnapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestMountBootArgs 引导期挂载参数组装：按实例名（组名或 实例@组名）
// 解析组，组级与全局按容器路径合并；ro 条目渲染 --bind-ro，其余
// --bind；未知实例回退全局表；nil 配置返回空。
func TestMountBootArgs(t *testing.T) {
	cfg := &config.Config{
		Mounts: []config.Mount{
			{Host: "/srv/data", Container: "/data"},
			{Host: "/g2", Container: "/d2", ReadOnly: true},
		},
		Groups: []*config.Group{
			{Name: "code", Packages: []string{"git"}},
			{Name: "tool", Packages: []string{"vim"}, Mounts: []config.Mount{
				{Host: "/elsewhere/work", Container: "/data"},
				{Host: "/etc/hosts", Container: "/etc/hosts", ReadOnly: true},
			}},
		},
	}
	want := func(args ...string) []string { return args }
	// 组级 /data 条目按全局原顺序就地顶替，其余全局项保留、组级项追加
	if got := mountBootArgs(cfg, "tool"); !slices.Equal(got, want(
		"--bind=/elsewhere/work:/data", "--bind-ro=/g2:/d2",
		"--bind-ro=/etc/hosts:/etc/hosts")) {
		t.Errorf("tool = %v", got)
	}
	if got := mountBootArgs(cfg, "session@tool"); !slices.Equal(got, want(
		"--bind=/elsewhere/work:/data", "--bind-ro=/g2:/d2",
		"--bind-ro=/etc/hosts:/etc/hosts")) {
		t.Errorf("session@tool = %v", got)
	}
	if got := mountBootArgs(cfg, "code"); !slices.Equal(got, want(
		"--bind=/srv/data:/data", "--bind-ro=/g2:/d2")) {
		t.Errorf("code（未声明组应继承全局）= %v", got)
	}
	if got := mountBootArgs(cfg, "ghost"); !slices.Equal(got, want(
		"--bind=/srv/data:/data", "--bind-ro=/g2:/d2")) {
		t.Errorf("ghost（未知实例应回退全局）= %v", got)
	}
	if got := mountBootArgs(nil, "x"); got != nil {
		t.Errorf("nil 配置应返回空: %v", got)
	}
}

// TestBootPicksUpConfigChange 引导前当场重读配置：daemon 存续期间改写
// 正式配置（新增 mounts），下一次 start 引导的 nspawn 参数即含新挂载
// ——改配置无需重启守护进程。断言锚定具体挂载而非泛 --bind：会话直通
// 的 --bind-ro 条目（X11 等）两拍都在且与本题无关。
func TestBootPicksUpConfigChange(t *testing.T) {
	base := t.TempDir()
	cfgPath := filepath.Join(base, "meowsub.toml")
	writeCfg := func(mounts string) {
		body := "base_dir = \"" + base + "\"\n" + mounts +
			"\n[code]\npackages = [\"git\"]\n"
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(base, "build", "code", "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCfg("") // 初始配置：无 mounts
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingExec{}
	s := NewServer(cfg, cfgPath)
	s.Exec = rec.Exec

	resp := s.Handle(context.Background(),
		Request{Op: OpStart, Group: "code", CallerUID: 0})
	if !resp.OK {
		t.Fatalf("首次 start 失败: %+v", resp)
	}
	for _, c := range rec.callsSnapshot() {
		if strings.HasPrefix(c, "systemd-nspawn") &&
			strings.Contains(c, "--bind=/srv/share") {
			t.Fatalf("初始配置无该挂载，引导不应携带: %v", c)
		}
	}

	// 不重启守护进程，直接改写配置；换新 exec 簿记并按停机清簿记，
	// 让下一次 start 走真实引导路径。
	writeCfg("mounts = [\"/srv/share:/share\"]")
	rec2 := &recordingExec{}
	s.Exec = rec2.Exec
	_ = s.poweroff("code")
	resp = s.Handle(context.Background(),
		Request{Op: OpStart, Group: "code", CallerUID: 0})
	if !resp.OK {
		t.Fatalf("二次 start 失败: %+v", resp)
	}
	found := false
	for _, c := range rec2.callsSnapshot() {
		if strings.HasPrefix(c, "systemd-nspawn") &&
			strings.Contains(c, "--bind=/srv/share:/share") {
			found = true
		}
	}
	if !found {
		t.Fatalf("新引导应携带改后配置的挂载: %v", rec2.calls)
	}
}

// TestBootRejectsBrokenConfig 配置被改坏后引导明确报错（含路径与原因），
// 而非沿用旧快照静默引导。
func TestBootRejectsBrokenConfig(t *testing.T) {
	base := t.TempDir()
	cfgPath := filepath.Join(base, "meowsub.toml")
	body := "base_dir = \"" + base + "\"\n\n[code]\npackages = [\"git\"]\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg, cfgPath)
	s.Exec = (&recordingExec{}).Exec
	if err := os.WriteFile(cfgPath, []byte("base_dir = 42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp := s.Handle(context.Background(),
		Request{Op: OpStart, Group: "code", CallerUID: 0})
	if resp.OK || !strings.Contains(resp.Error, "重读配置") {
		t.Fatalf("坏配置应拒绝引导并留原因: %+v", resp)
	}
}
