// server.go meowsubd 服务主体。
//
// 职责边界：负责「把机器跑起来、跑下去、按出身收回去」，以及在实例内
// 以调用者身份执行命令——非交互 -c 同步执行并回传输出；交互 shell 经
// machinectl 桥接容器内 pty，连接在握手后退化为终端字节流。
package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"meowsub/internal/config"
	"meowsub/internal/graphical"
	"meowsub/internal/runner"
)

// SocketPath 监听路径；用变量以便测试把服务与客户端一起重定向到临时 socket。
var SocketPath = "/run/meowsubd.sock"

// MachineName 实例对应的 systemd-machined 机器名：确定性命名支撑“有则复用”。
func MachineName(run string) string { return "ms-" + run }

// Server 服务状态。Exec 供测试注入命令执行；生产为 exec 直通。
type Server struct {
	// cfgRef 当前生效配置快照。cfgPath 非空时每次引导前当场重读并原子
	// 换入（改配置无需重启守护进程，下一拍引导即生效）；读取一律经
	// cfg()，与换入点（reloadCfg）保持竞争安全。
	cfgRef  atomic.Pointer[config.Config]
	cfgPath string
	// Exec 进程启动器：生产为 exec.Command；测试注入受控行为。
	Exec func(name string, args ...string) *exec.Cmd

	mu   sync.Mutex
	tb   *Table
	pros map[string]*exec.Cmd // run -> 引导进程（存在即机器在跑）
	// binds run -> (宿主路径 -> 引导/补挂时钉住的资源身份)。容器非本进程
	// 引导时无记录，首次请求按“未知”处理（清场补挂）。随容器死亡清空。
	binds map[string]map[string]graphical.Bind
}

// NewServer 构造服务并预登记 autostart 组的会话出身。cfgPath 可变参：
// 正式路径由 daemon.Run 传入，此后每次引导前当场重读；缺省（测试直构）
// 不重读，沿用构造时的配置。
func NewServer(cfg *config.Config, cfgPath ...string) *Server {
	s := &Server{tb: NewTable(), pros: map[string]*exec.Cmd{},
		binds: map[string]map[string]graphical.Bind{}, Exec: exec.Command}
	s.cfgRef.Store(cfg)
	if len(cfgPath) > 0 {
		s.cfgPath = cfgPath[0]
	}
	for _, g := range cfg.Groups {
		if g.Autostart {
			s.tb.Ensure(g.Name, g.Name, OriginAutostart)
		}
	}
	return s
}

// cfg 当前生效配置快照。
func (s *Server) cfg() *config.Config { return s.cfgRef.Load() }

// reloadCfg 引导前的配置重读：当场读盘并原子换入。cfgPath 未设置（测试
// 直构）时不做任何事；读盘或校验失败即报错拒绝引导——沿用旧快照静默
// 引导只会把“改了没生效”的困惑换一种形式重演。
func (s *Server) reloadCfg() error {
	if s.cfgPath == "" {
		return nil
	}
	cfg, err := config.Load(s.cfgPath)
	if err != nil {
		return fmt.Errorf("重读配置 %s 失败: %w", s.cfgPath, err)
	}
	s.cfgRef.Store(cfg)
	return nil
}

// groupFor 解析 run 对应的组配置；组不可考返回 nil。
func (s *Server) groupFor(run string) *config.Group {
	for _, g := range s.cfg().Groups {
		if run == g.Name || strings.HasSuffix(run, "@"+g.Name) {
			return g
		}
	}
	return nil
}

// sessionUIDFor 解析该组会话操作的目标 uid：caller 复用调用者身份，
// 固定值按组配置。
func (s *Server) sessionUIDFor(group string, callerUID int) int {
	if g := s.groupFor(group); g != nil {
		return g.SessionUID(callerUID)
	}
	return callerUID
}

// authorize 组级会话门禁：shell/open/run-desktop 前校验调用者身份
// （session_access：all | root | 特定 uid）。
func (s *Server) authorize(group string, callerUID int) error {
	g := s.groupFor(group)
	if g == nil || g.SessionAllowed(callerUID) {
		return nil
	}
	return fmt.Errorf("组 %s 的会话访问权限拒绝 uid %d（session_access=%s）",
		group, callerUID, g.SessionAccess)
}

// --- 磁盘与机器辅助 -----------------------------------------------------

func (s *Server) runsRoot() string { return filepath.Join(s.cfg().BaseDir, "runs") }

func (s *Server) buildLayer(g string) string {
	return filepath.Join(s.cfg().BaseDir, "build", g)
}

func (s *Server) listRuns() []string {
	es, err := os.ReadDir(s.runsRoot())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range es {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// pickRun 返回该组应使用的实例名：优先同名实例，否则组名本身。
func (s *Server) pickRun(group string) string {
	for _, r := range s.listRuns() {
		if r == group || strings.HasSuffix(r, "@"+group) {
			return r
		}
	}
	return group
}

func (s *Server) layerOK(group string) bool {
	fi, err := os.Stat(filepath.Join(s.buildLayer(group), "usr"))
	return err == nil && fi.IsDir()
}

func (s *Server) runRoot(run string) string { return filepath.Join(s.runsRoot(), run) }

// running 查询机器活性：先看 daemon 自持引导进程，再问 machinectl。
func (s *Server) running(run string) bool {
	s.mu.Lock()
	p := s.pros[run]
	s.mu.Unlock()
	if p != nil {
		return true // 仍在引导/运行中
	}
	return s.Exec("machinectl", "status", MachineName(run)).Run() == nil
}

func (s *Server) poweroff(run string) error {
	s.mu.Lock()
	delete(s.pros, run)
	delete(s.binds, run)
	s.mu.Unlock()
	if err := s.Exec("machinectl", "poweroff", MachineName(run)).Run(); err != nil {
		return s.Exec("machinectl", "terminate", MachineName(run)).Run()
	}
	return nil
}

// boot 引导一个已部署实例；引导进程为 daemon 子进程并登记回收。
// 同步锁在此把关：build 收尾交换实例目录期间拒绝任何拉起，防止新容器
// 抱着正在被替换的旧树。会话直通参数按组配置的 session_mode 组装。
func (s *Server) boot(run string) error {
	cfg := s.cfg()
	if InstanceLocked(cfg.BaseDir, run) {
		return fmt.Errorf("实例 %s 同步锁定中（build 收尾），稍后重试", run)
	}
	// 同一次扫描既生成绑挂参数又登记身份，保证缓存与 nspawn 实际所挂一致。
	scan := graphical.Scan()
	args := sessionBootArgs(cfg, run, scan)
	args = append(args, tmpfsBootArgs(cfg, run)...)
	args = append(args, mountBootArgs(cfg, run)...)
	cmd := s.Exec("systemd-nspawn", append([]string{"--boot",
		"-D", s.runRoot(run), "--machine=" + MachineName(run)}, args...)...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	seed := make(map[string]graphical.Bind, len(scan))
	for _, b := range scan {
		seed[b.Path] = b
	}
	s.mu.Lock()
	s.pros[run] = cmd
	s.binds[run] = seed
	s.mu.Unlock()
	go func() { // 僵尸收割；外部死亡同步簿记
		_ = cmd.Wait()
		s.mu.Lock()
		cur, ok := s.pros[run]
		if ok && cur == cmd {
			delete(s.pros, run)
			delete(s.binds, run)
		}
		s.mu.Unlock()
	}()
	return nil
}

// ensureSession 请求期会话资源保障。宿主侧身份（Dev/Ino）负责捕获重登录
// 轮换；容器内视图可见性探活负责捕获 logind 私有 tmpfs 遮蔽与拆除。方案
// 按组配置的 session_mode 分流（off 直接跳过；X11 目录恒直通）。
func (s *Server) ensureSession(run string, uid int) {
	mode := sessionModeFor(s.cfg(), run)
	if mode == config.SessionModeOff {
		return
	}
	rt := graphical.RuntimeDir(uid)
	for _, b := range graphical.Scan() {
		if b.Kind != graphical.KindX11 && b.Path != rt {
			continue
		}
		s.mu.Lock()
		have, ok := s.binds[run][b.Path]
		s.mu.Unlock()
		lost := !ok || have.Dev != b.Dev || have.Ino != b.Ino
		if ok && !lost {
			if b.Kind != graphical.KindRuntime {
				continue // 引导期已挂且实例未变
			}
			// 宿主侧身份未变 ≠ 容器内视图可见：容器 logind 首个会话即给
			// 运行时目录盖上私有 tmpfs，被遮蔽的条目在 mountinfo 里仍在。
			entries, _ := sessionPlanDir(b.Path, mode)
			var err error
			lost, err = s.sessionViewLost(run, mode, b.Path, entries)
			if err != nil {
				fmt.Printf("[meowsubd] %s 探测 %s 失败（继续执行）: %v\n",
					run, b.Path, err)
				continue
			}
			if !lost {
				continue
			}
			fmt.Printf("[meowsubd] %s 容器内 %s 视图丢失，重新直递\n",
				run, b.Path)
		}
		if err := s.redeliverSession(run, b, mode, uid); err != nil {
			fmt.Printf("[meowsubd] %s 直递 %s 失败（继续执行）: %v\n",
				run, b.Path, err)
			continue
		}
		s.mu.Lock()
		if s.binds[run] == nil {
			s.binds[run] = map[string]graphical.Bind{}
		}
		s.binds[run][b.Path] = b
		s.mu.Unlock()
	}
}

// probeRC 经 machinectl shell 在容器内执行单条命令，回传 rc= 尾行字面量。
// machinectl shell 不透传容器内退出码（会话正常关闭即返回 0），故由
// echo rc=$? 尾行显式携带。
func (s *Server) probeRC(machine, cmdline string) (string, error) {
	c := s.Exec("machinectl", "shell", "-q", machine, "--",
		"/bin/sh", "-c", cmdline+"; echo rc=$?")
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = io.Discard
	if err := c.Run(); err != nil {
		return "", err
	}
	fields := strings.Fields(strings.TrimSpace(out.String()))
	if len(fields) == 0 || !strings.HasPrefix(fields[len(fields)-1], "rc=") {
		return "", fmt.Errorf("探针输出异常: %q", out.String())
	}
	return fields[len(fields)-1], nil
}

// dirMounted 探测容器内 dir 是否挂载点。
func (s *Server) dirMounted(machine, dir string) (bool, error) {
	out, err := s.probeRC(machine,
		"/usr/bin/findmnt -rn "+shQuote(dir)+" >/dev/null")
	if err != nil {
		return false, err
	}
	switch out {
	case "rc=0":
		return true, nil
	case "rc=1":
		return false, nil
	default:
		return false, fmt.Errorf("探针失败: %q", out)
	}
}

// sessionViewLost 探测容器内 dir 的会话视图是否可见。rw 方案目录本体是
// 挂载点即健在；sockets 方案要求目录是挂载点（容器 logind 私有 tmpfs 已
// 就位——裸目录上直递的条目虽当下可见，终将被首个会话的 tmpfs 盖住，同
// 样视为丢失）且条目逐个存在：mountinfo（findmnt 全表）连被遮蔽的旧挂
// 载也列出来，不能作可见性依据。返回值语义：
//
//	(true, nil)  容器可达但视图不可见（被遮蔽/未挂载/被拆除），应重递；
//	(false, nil) 视图健在；
//	(false, err) 容器/探针本身不可用，调用方留痕并跳过本次重递。
func (s *Server) sessionViewLost(run, mode, dir string,
	entries []graphical.Entry) (bool, error) {
	if mode == config.SessionModeRW {
		mounted, err := s.dirMounted(MachineName(run), dir)
		return !mounted, err
	}
	var sb strings.Builder
	sb.WriteString("/usr/bin/findmnt -rn " + shQuote(dir) + " >/dev/null")
	for _, e := range entries {
		sb.WriteString(" && [ -e " + shQuote(e.Container) + " ]")
	}
	out, err := s.probeRC(MachineName(run), sb.String())
	if err != nil {
		return false, err
	}
	switch out {
	case "rc=0":
		return false, nil
	case "rc=1":
		return true, nil
	default:
		return false, fmt.Errorf("探针失败: %q", out)
	}
}

// warmSession 以 uid 开一个保活会话（后台 sleep）：触发容器内 logind 的
// user-runtime-dir@<uid> 给运行时目录挂上私有 tmpfs，并在直递期间保活。
// 这是 sockets 直递唯一能落地的位置——先递必被盖；无活会话时拆除定时器
// （logind UserStopDelaySec）也可能抢在直递完成前动手。返回停止函数：
// 直递完毕即回收保活会话，此后由真实会话接管 runtime 目录的存续。
func (s *Server) warmSession(machine string, uid int) func() {
	c := s.Exec("machinectl", shellArgv(uid, nil, machine,
		[]string{"-q"}, []string{"/bin/sleep", "10"})...)
	c.Stdout, c.Stderr = io.Discard, io.Discard
	if err := c.Start(); err != nil {
		fmt.Printf("[meowsubd] %s 预热 uid %d 会话失败（尽力直递）: %v\n",
			machine, uid, err)
		return func() {}
	}
	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	return func() {
		_ = c.Process.Kill()
		<-done
	}
}

// awaitRuntimeMount 轮询等待容器内 dir 成为挂载点（logind 私有 tmpfs 就
// 位），超时返回 false。
func (s *Server) awaitRuntimeMount(machine, dir string) bool {
	deadline := time.Now().Add(10 * time.Second)
	for {
		mounted, err := s.dirMounted(machine, dir)
		if err == nil && mounted {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// umountInContainer 对给定容器内路径惰性卸载（同路径叠层需反复摘），单次
// shell 往返完成。失败按“不是挂载点/路径不存在”理解，即清场完成。
func (s *Server) umountInContainer(machine string, paths []string) {
	if len(paths) == 0 {
		return
	}
	var sb strings.Builder
	for _, p := range paths {
		sb.WriteString("for i in 1 2 3 4; do /usr/bin/umount -l " +
			shQuote(p) + " || break; done; ")
	}
	c := s.Exec("machinectl", "shell", "-q", machine, "--",
		"/bin/sh", "-ec", sb.String())
	c.Stdout, c.Stderr = io.Discard, io.Discard
	_ = c.Run()
}

// redeliverSession 把宿主会话资源重新直递进容器。X11 与 rw 整目录直挂；
// sockets 模式的运行时目录必须落在容器 logind 私有 tmpfs 之上（
// user-runtime-dir 对非挂载点的 /run/user/<uid> 无条件盖自己的 tmpfs，
// 先递后盖永远无效）：趁旧挂载尚可达先清场，再预热保活会话并等 tmpfs
// 就位，然后逐条目 machinectl bind，最后重建符号链接（bind 不适用于链
// 接）。尽力而为：单个条目失败留痕不阻塞其余。
func (s *Server) redeliverSession(run string, b graphical.Bind,
	mode string, uid int) error {
	machine := MachineName(run)
	if b.Kind == graphical.KindX11 || mode == config.SessionModeRW {
		s.umountInContainer(machine, []string{b.Path})
		c := s.Exec("machinectl", "bind", "--mkdir", machine, b.Path)
		c.Stdout, c.Stderr = io.Discard, io.Discard
		return c.Run()
	}

	entries, links := sessionPlanDir(b.Path, mode)
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.Container
	}
	s.umountInContainer(machine, paths)

	stop := s.warmSession(machine, uid)
	defer stop()
	if !s.awaitRuntimeMount(machine, b.Path) {
		fmt.Printf("[meowsubd] %s 等待容器内 %s 挂载超时（仍然直递）\n",
			run, b.Path)
	}

	var firstErr error
	for _, e := range entries {
		c := s.Exec("machinectl", "bind", "--mkdir", machine, e.Host, e.Container)
		c.Stdout, c.Stderr = io.Discard, io.Discard
		if err := c.Run(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("直递 %s: %w", e.Container, err)
		}
	}
	if len(links) > 0 {
		var sb strings.Builder
		sb.WriteString("/usr/bin/true")
		for _, l := range links {
			sb.WriteString("; /usr/bin/ln -sfn " +
				shQuote(l.Target) + " " + shQuote(l.Path))
		}
		c := s.Exec("machinectl", "shell", "-q", machine, "--",
			"/bin/sh", "-ec", sb.String())
		c.Stdout, c.Stderr = io.Discard, io.Discard
		if err := c.Run(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("重建符号链接: %w", err)
		}
	}
	return firstErr
}

// shQuote 单引号包裹，内嵌单引号按 shell 惯例转义。
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func autostartOn(cfg *config.Config, group string) bool {
	for _, g := range cfg.Groups {
		if g.Name == group {
			return g.Autostart
		}
	}
	return false
}

func originFor(cfg *config.Config, group string) InstanceOrigin {
	if autostartOn(cfg, group) {
		return OriginAutostart
	}
	return OriginDemand
}

// --- 部署与实例保障 -----------------------------------------------------

// ensureInstance 保证该组存在可运行实例：复用任一该组实例；否则以组名
// 隐式部署（构建成品 → 基线 → 实例）。差异确认非交互放行但摘要打印留痕。
// 此处是所有引导路径（autostart 预引导、start、应用类操作的隐式拉起）的
// 公共必经点：引导前在此当场重读配置，daemon 启动后的配置改动下一拍
// 引导即生效，无需重启守护进程。
func (s *Server) ensureInstance(group string) (string, bool, error) {
	if err := s.reloadCfg(); err != nil {
		return "", false, err
	}
	for _, r := range s.listRuns() {
		if r == group || strings.HasSuffix(r, "@"+group) {
			return r, false, nil
		}
	}
	if !s.layerOK(group) {
		return "", false, fmt.Errorf("构建区缺少成品层 %s（先 build）",
			s.buildLayer(group))
	}
	fmt.Printf("[meowsubd] 组 %s 无既有实例，按三区链隐式创建\n", group)
	p := runner.Paths{BaseDir: s.cfg().BaseDir,
		In: strings.NewReader("y\n"), Out: os.Stdout}
	if err := p.Deploy(s.buildLayer(group), group, true); err != nil {
		return "", false, err
	}
	return group, true, nil
}

// ensureRunning 保证该组有实例且已引导（无则隐式部署），返回实例名。
// 应用执行类操作（shell/launch/run-desktop）的公共前置段。
func (s *Server) ensureRunning(group string) (string, error) {
	run := s.pickRun(group)
	if !s.running(run) {
		n, _, err := s.ensureInstance(group)
		if err != nil {
			return "", err
		}
		run = n
		s.tb.Ensure(run, group, originFor(s.cfg(), group))
	} else if _, ok := s.tb.Get(run); !ok {
		s.tb.Ensure(run, group, originFor(s.cfg(), group))
	}
	if !s.running(run) { // 刚部署完，需引导
		if err := s.boot(run); err != nil {
			return "", fmt.Errorf("引导 %s 失败: %w", run, err)
		}
		if err := s.awaitMachine(run); err != nil {
			return "", err
		}
	}
	return run, nil
}

// awaitMachine 阻塞至 ms-<run> 可受理 shell 会话。就绪有两道门槛：
// machined 登记（nspawn 拉起即异步进行）与容器内 systemd 的系统总线
// （dbus.service 起来才开得了 OpenMachineShell 会话），任一未就绪
// machinectl 都会报错。探测走真实路径——/bin/true 探针调 machinectl
// shell，成过一次则后续真实会话必然成功；已就绪时单次探测仅一次快速
// D-Bus 往返。
func (s *Server) awaitMachine(run string) error {
	probe := shellArgv(0, nil, MachineName(run), []string{"-q"}, []string{"/bin/true"})
	deadline := time.Now().Add(60 * time.Second)
	for {
		c := s.Exec("machinectl", probe...)
		c.Stdout, c.Stderr = io.Discard, io.Discard
		if c.Run() == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待 %s 就绪超时（machined 登记与容器内系统总线）",
				MachineName(run))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// --- 占用与回收 ---------------------------------------------------------

func (s *Server) hold(run string) { s.tb.Hold(run) }

// releaseAndReap 归还持有者；隐式按需实例清零即优雅停机。
func (s *Server) releaseAndReap(run string) {
	s.tb.Release(run)
	sn, ok := s.tb.Get(run)
	if !ok || !sn.ShouldAutoStop() {
		return
	}
	fmt.Printf("[meowsubd] %s 占用清零，自动停止\n", run)
	if err := s.poweroff(run); err != nil {
		fmt.Printf("[meowsubd] 回收 %s 出错: %v\n", run, err)
	}
	s.tb.Drop(run)
}

// --- 请求处理 ------------------------------------------------------------

// Handle 请求分发。管理面动作（start/stop/ps）仅 root；
// 应用执行类（shell -c / launch）本机任意用户可发起，实例内以
// 调用者镜像身份运行。ctx 在客户端连接断开时取消，供前台阻塞的
// launch/run-desktop 连锁终结实例内应用。
func (s *Server) Handle(ctx context.Context, r Request) Response {
	root := func() bool { return r.CallerUID == 0 }

	switch r.Op {
	case OpPing:
		return Response{OK: true, Data: "meowsubd"}

	case OpPs:
		if !root() {
			return errf("该操作需要 root：%s", sudoHint(r))
		}
		type row struct {
			Name, Group, Origin string
			Holders             int
		}
		snap := append([]session{}, s.tb.Snapshot()...)
		sort.Slice(snap, func(i, j int) bool {
			if snap[i].Group != snap[j].Group {
				return snap[i].Group < snap[j].Group
			}
			return snap[i].Origin < snap[j].Origin
		})
		var rows []row
		for _, sn := range snap {
			rows = append(rows, row{Name: sn.Group,
				Group: sn.Group, Origin: string(sn.Origin), Holders: sn.Holders})
		}
		return Response{OK: true, Data: rows}

	case OpRunning:
		if !root() {
			return errf("该操作需要 root：%s", sudoHint(r))
		}
		run := s.pickRun(resolveGroup(r))
		return Response{OK: true,
			Data: map[string]any{"run": run, "running": s.running(run)}}

	case OpStart, OpStop:
		if !root() {
			return errf("该操作需要 root：%s", sudoHint(r))
		}
		group := resolveGroup(r)
		run := s.pickRun(group)
		if r.Op == OpStart {
			origin := OriginExplicit
			if autostartOn(s.cfg(), group) && run == group {
				origin = OriginAutostart
			}
			s.tb.Ensure(run, group, origin)
			if s.running(run) {
				return Response{OK: true, Data: run + " 已在运行"}
			}
			if _, _, err := s.ensureInstance(group); err != nil {
				return errf("%v", err)
			}
			if err := s.boot(run); err != nil {
				return errf("引导 %s 失败: %v", run, err)
			}
			return Response{OK: true, Data: "已发起启动 " + run}
		}
		s.tb.Drop(run)
		if !s.running(run) {
			return Response{OK: true, Data: run + " 本就未运行"}
		}
		if err := s.poweroff(run); err != nil {
			return errf("停止 %s 失败: %v", run, err)
		}
		return Response{OK: true, Data: "已停止 " + run}

	case OpShell:
		if r.Interactive { // 交互请求由连接层分流至 bridgeShell
			return errf("交互 shell 请求应走桥接通道，不经 Handle")
		}
		if r.Cmd == "" {
			return Response{OK: false, Error: "非交互执行需要 -c 命令；" +
				"进入交互 shell 直接运行 meowsub shell <组名>"}
		}
	case OpRunDesktop:
		if r.Cmd == "" {
			return Response{OK: false, Error: "run-desktop 缺少实例内 .desktop 目标"}
		}
	case OpLaunchEntry:
		if r.Cmd == "" {
			return Response{OK: false, Error: "launch 缺少目标命令或应用 ID"}
		}
	default:
		return errf("未知操作 %q", r.Op)
	}

	group := resolveGroup(r)
	if group == "" {
		return errf("缺少目标软件组")
	}
	if err := s.authorize(group, r.CallerUID); err != nil {
		return errf("%v", err)
	}
	run, err := s.ensureRunning(group)
	if err != nil {
		return errf("%v", err)
	}
	s.hold(run)

	if r.Op == OpShell { // 非交互 -c：同步执行，输出随响应回传。
		// Cmd 是容器内命令（CLI 侧为 /bin/bash -lc），非 open 入口，
		// 不经 .desktop 解析——那会把绝对路径拼进 usr/bin/bin 一类的错位候选。
		defer s.releaseAndReap(run)
		if err := s.awaitMachine(run); err != nil {
			return errf("%v", err)
		}
		s.ensureSession(run, s.sessionUIDFor(group, r.CallerUID))
		argv := append([]string{r.Cmd}, r.Args...)
		var out bytes.Buffer
		c := s.Exec("machinectl",
			sessionShellArgv(s.cfg(), run, r.CallerUID, r.Env,
				MachineName(run), nil, argv)...)
		c.Stdout = &out
		c.Stderr = &out
		rerr := c.Run()
		data := strings.TrimRight(out.String(), "\n")
		if rerr != nil {
			var ee *exec.ExitError
			if !errors.As(rerr, &ee) { // 非退出码类失败（启动 machinectl 失败等）
				return errf("在 %s 内执行失败: %v", run, rerr)
			}
			if data != "" {
				data += "\n"
			}
			data += fmt.Sprintf("[退出码 %d]", ee.ExitCode())
		}
		return Response{OK: true, Data: data}
	}

	if r.Wait { // 前台阻塞：响应随应用退出返回；客户端断开即连锁终结
		defer s.releaseAndReap(run)
		layer := filepath.Join(s.cfg().BaseDir, "runs", run)
		argv, rerr := resolveLaunch(layer, r)
		if rerr != nil {
			return errf("%v", rerr)
		}
		s.ensureSession(run, s.sessionUIDFor(group, r.CallerUID))
		var ebuf bytes.Buffer // machinectl 的报错只在 stderr，吞掉就成无声失败
		c := s.Exec("machinectl",
			sessionShellArgv(s.cfg(), run, s.sessionUIDFor(group, r.CallerUID), r.Env,
				MachineName(run), nil, argv)...)
		c.Stdout = io.Discard
		c.Stderr = &ebuf
		c.Stdin = nil
		ended := armConnKill(c, ctx)
		rerr = c.Run()
		ended()
		if rerr != nil {
			var ee *exec.ExitError
			if !errors.As(rerr, &ee) { // 非退出码类失败（machinectl 起不来等）
				return errf("%v", rerr)
			}
			data := fmt.Sprintf("实例 %s 内应用已退出[退出码 %d]",
				run, ee.ExitCode())
			if d := strings.TrimSpace(ebuf.String()); d != "" {
				data += "：" + d
			}
			return Response{OK: true, Data: data}
		}
		return Response{OK: true, Data: "实例 " + run + " 内应用已正常退出"}
	}

	go func() { // launch / run-desktop：提交即忘（应用启动，无输出可回传）
		layer := filepath.Join(s.cfg().BaseDir, "runs", run)
		argv, rerr := resolveLaunch(layer, r)
		if rerr != nil {
			fmt.Printf("[meowsubd] %v\n", rerr)
			s.releaseAndReap(run)
			return
		}
		s.ensureSession(run, s.sessionUIDFor(group, r.CallerUID))
		var ebuf bytes.Buffer // machinectl 的报错只在 stderr，吞掉就成无声失败
		c := s.Exec("machinectl",
			sessionShellArgv(s.cfg(), run, s.sessionUIDFor(group, r.CallerUID), r.Env,
				MachineName(run), nil, argv)...)
		c.Stdout = io.Discard
		c.Stderr = &ebuf
		c.Stdin = nil
		if err := c.Run(); err != nil {
			msg := fmt.Sprintf("[meowsubd] 在 %s 内执行失败: %v", run, err)
			if d := strings.TrimSpace(ebuf.String()); d != "" {
				msg += "：" + d
			}
			fmt.Println(msg)
		}
		s.releaseAndReap(run)
	}()
	return Response{OK: true, Data: "已提交到 " + run}
}

// resolveLaunch 把 launch/run-desktop 的目标折算为容器内 argv。
func resolveLaunch(layer string, r Request) ([]string, error) {
	if r.Op == OpRunDesktop { // 目标是实例内 .desktop 路径或 ID
		return ResolveDesktopFile(layer, r.Cmd, r.Args)
	}
	return ResolveOpen(layer, r.Cmd, r.Args)
}

// armConnKill 在 ctx 取消（客户端断开）时杀掉 machinectl：其 pty 桥接随
// 之关闭，容器内前台进程组收到 SIGHUP，连锁终结应用。返回的 stop 由调用
// 方在 c.Run() 返回后调用（幂等），以收起监听协程。
func armConnKill(c *exec.Cmd, ctx context.Context) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	stop = func() { once.Do(func() { close(done) }) }
	exit := ctx.Done()
	if exit == nil {
		return stop
	}
	go func() {
		select {
		case <-exit:
			if c.Process != nil {
				_ = c.Process.Kill()
			}
		case <-done:
		}
	}()
	return stop
}

// shellArgv 组装 machinectl shell 调用。身份镜像（非 root 调用者映射为其
// uid，uid=0 即默认 root）与会话环境注入（调用者透传的 env，见
// graphical.EnvArgs——daemon 自身环境不含会话事实，一律不读）是各 shell
// 形态的公共部分；opts 为附加全局选项（如 -q、--setenv=TERM），argv 为
// 容器内命令。
func shellArgv(uid int, env map[string]string, machine string,
	opts, argv []string) []string {
	return shellArgvWithBus(uid, env, machine, opts, argv, false, false, false)
}

// sessionShellArgv 是应用/交互 shell 的统一入口。isolated 模式只把图形
// 会话套接字带入实例并启动私有 D-Bus；native 模式保留宿主 D-Bus，
// 但两者都关闭 portal，避免文件选择请求落到宿主桌面。
func sessionShellArgv(cfg *config.Config, run string, uid int,
	env map[string]string, machine string, opts, argv []string) []string {
	mode := sessionModeFor(cfg, run)
	privateBus := mode == config.SessionModeIsolated
	disablePortal := mode == config.SessionModeNative || privateBus
	filterHostBus := mode == config.SessionModeNative
	return shellArgvWithBus(uid, env, machine, opts, argv,
		privateBus, disablePortal, filterHostBus)
}

func shellArgvWithBus(uid int, env map[string]string, machine string,
	opts, argv []string, privateBus, disablePortal, filterHostBus bool) []string {
	mach := append([]string{"shell"}, opts...)
	if uid > 0 {
		mach = append(mach, "--uid="+strconv.Itoa(uid))
	}
	if privateBus {
		// dbus-run-session 会创建并注入新的 session bus；不要把调用者的
		// 宿主地址传给它，避免应用启动早期仍误连宿主 bus。
		mach = append(mach, graphical.EnvArgsWithoutDBus(env)...)
	} else {
		mach = append(mach, graphical.EnvArgs(env)...)
	}
	if disablePortal {
		// GTK/Electron 在检测到 portal 后可能主动把文件选择请求交给
		// 宿主桌面；native/isolated 都优先让实例内进程绘制 chooser。
		mach = append(mach,
			"--setenv=GTK_USE_PORTAL=0",
			"--setenv=GIO_USE_VFS=local")
	}
	mach = append(mach, machine)
	if privateBus {
		argv = append([]string{"/usr/bin/dbus-run-session", "--"}, argv...)
	} else if filterHostBus {
		argv = append([]string{"/bin/sh", "-c", nativeBusProxyScript,
			"meowsub-dbus-proxy"}, argv...)
	}
	return append(mach, argv...)
}

// nativeBusProxyScript 为 native 模式建立宿主 session bus 的受限视图：
// 输入法服务可用，但 portal、FileManager1 与其它宿主桌面服务不可见。
// 代理生命周期跟随应用/交互 shell，应用退出时一并清理。
const nativeBusProxyScript = `set -eu
proxy="$XDG_RUNTIME_DIR/.meowsub-bus-proxy-$$"
cleanup() {
  if [ -n "${proxy_pid:-}" ]; then
    kill "$proxy_pid" 2>/dev/null || true
  fi
  rm -f "$proxy"
}
trap cleanup EXIT
rm -f "$proxy"
if [ ! -x /usr/bin/xdg-dbus-proxy ]; then
  echo "meowsub: native 模式缺少 /usr/bin/xdg-dbus-proxy，暂时直连宿主 bus" >&2
  exec "$@"
fi
/usr/bin/xdg-dbus-proxy "$DBUS_SESSION_BUS_ADDRESS" "$proxy" \
  --filter \
  --talk=org.fcitx.Fcitx5 \
  --talk=org.fcitx.Fcitx \
  --talk=org.freedesktop.IBus \
  --talk=org.freedesktop.IBus.Panel \
  --talk=org.freedesktop.portal.Fcitx \
  --talk=org.freedesktop.portal.IBus &
proxy_pid=$!
for i in $(/usr/bin/seq 1 100); do
  [ -S "$proxy" ] && break
  kill -0 "$proxy_pid" 2>/dev/null || exit 1
  /usr/bin/sleep 0.01
done
[ -S "$proxy" ] || exit 1
export DBUS_SESSION_BUS_ADDRESS="unix:path=$proxy"
exec "$@"
`

// bridgeShell 交互 shell：以调用者身份在实例内拉起 bash，连接退化为本机
// 终端与 machined 所配 pty 之间的字节管道。无登录环节——root 起 --uid
// 会话无需认证，身份即调用者。握手行在子进程起跑后写出，此后连接上不再
// 有 JSON；本函数阻塞至会话结束（承载它的连接 goroutine 即会话期）。
// 返回值仅启动失败时作为普通错误响应写回。
func (s *Server) bridgeShell(c *net.UnixConn, r Request) Response {
	group := resolveGroup(r)
	if group == "" {
		return errf("缺少目标软件组")
	}
	if err := s.authorize(group, r.CallerUID); err != nil {
		return errf("%v", err)
	}
	run, err := s.ensureRunning(group)
	if err != nil {
		return errf("%v", err)
	}
	s.hold(run)
	defer s.releaseAndReap(run)
	if err := s.awaitMachine(run); err != nil { // 覆盖 start 后即刻 shell 的窗口
		return errf("%v", err)
	}

	inner := []string{"/bin/bash", "-l"}
	if r.Rows > 0 && r.Cols > 0 { // 客户端窗口尺寸经容器内 stty 落到 pty
		inner = []string{"/bin/bash", "-c", fmt.Sprintf(
			"stty rows %d cols %d 2>/dev/null; exec /bin/bash -l",
			r.Rows, r.Cols)}
	}
	opts := []string{"-q"} // 静默 machinectl 自身的连接提示，避免混入字节流
	if r.Term != "" {
		opts = append(opts, "--setenv=TERM="+r.Term)
	}

	f, err := c.File() // 复制 fd 直连子进程：不经内存拷贝协程，无收尾死锁
	if err != nil {
		return errf("连接不可桥接: %v", err)
	}
	defer f.Close()
	s.ensureSession(run, s.sessionUIDFor(group, r.CallerUID))
	cmd := s.Exec("machinectl",
		sessionShellArgv(s.cfg(), run, s.sessionUIDFor(group, r.CallerUID), r.Env,
			MachineName(run), opts, inner)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = f, f, f
	if err := cmd.Start(); err != nil {
		return errf("在 %s 内开启 shell 失败: %v", run, err)
	}
	if err := json.NewEncoder(c).Encode(
		Response{OK: true, Data: "已连接 " + run}); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return errf("握手写回失败: %v", err)
	}
	if err := cmd.Wait(); err != nil { // 非零退出（含容器内 shell 报错）留痕
		fmt.Printf("[meowsubd] %s 内 shell 会话异常结束: %v\n", run, err)
	}
	return Response{} // 不写回：连接此后已是字节流
}

// sudoHint 生成对应用户可直接复制的提示。
func sudoHint(r Request) string {
	name := resolveGroup(r)
	switch r.Op {
	case OpStart:
		return fmt.Sprintf("sudo meowsub start %s", name)
	case OpStop:
		return fmt.Sprintf("sudo meowsub stop %s", name)
	default:
		return "sudo meowsub ps"
	}
}

func resolveGroup(r Request) string {
	if r.Group != "" {
		return r.Group
	}
	return r.Name
}

// --- 连接层 -------------------------------------------------------------

// ServeUnix 监听 socket 服务至 Listener 关闭。策略：本机任意用户皆为合法
// 使用者（socket 全可写）；具体身份由每请求 SO_PEERCRED 决定，并在实例内
// 以该 uid 运行——这正是“非 sudo 场景直接以运行者身份执行”的落点。
// handle 处理一问一答式请求；bridge 处理 shell 交互桥接（连接转字节流）。
func ServeUnix(sock string, handle func(context.Context, Request) Response,
	bridge func(*net.UnixConn, Request) Response) error {
	if fi, err := os.Stat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(sock)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	if err := os.Chmod(sock, 0o666); err != nil { // 全本机可连
		ln.Close()
		return err
	}
	defer ln.Close()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go serveConn(conn, handle, bridge)
	}
}

func serveConn(c net.Conn, handle func(context.Context, Request) Response,
	bridge func(*net.UnixConn, Request) Response) {
	defer c.Close()
	tc, ok := c.(*net.UnixConn)
	if !ok {
		return
	}
	raw, credErr := tc.SyscallConn()
	var uid int
	if credErr == nil {
		credErr = raw.Control(func(fd uintptr) {
			ucred, e := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET,
				syscall.SO_PEERCRED)
			if e != nil {
				return
			}
			uid = int(ucred.Uid)
		})
	}
	writeResp := func(resp Response) { _ = json.NewEncoder(c).Encode(resp) }
	if credErr != nil {
		writeResp(Response{OK: false, Error: "无法核对对端身份"})
		return
	}
	line, rerr := bufio.NewReader(io.LimitReader(c, 1<<20)).ReadString('\n')
	if rerr != nil && line == "" {
		return
	}
	var req Request
	if json.Unmarshal([]byte(line), &req) != nil {
		writeResp(Response{OK: false, Error: "请求解析失败"})
		return
	}
	req.CallerUID = uid // 身份绑定：后续以该 uid 在实例内运行
	if req.Op == OpShell && req.Interactive {
		_ = bridge(tc, req) // 自管握手行与会话期，此后不再写回
		return
	}
	// 客户端断开感知：请求已收全，此后任何读返回 EOF/错误都意味着对端
	// 消失（正常等待响应的客户端会一直持有连接），取消 ctx 供前台阻塞
	// 的 launch/run-desktop 连锁终结实例内应用。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		var sink [1]byte
		for {
			if _, err := c.Read(sink[:]); err != nil {
				cancel()
				return
			}
		}
	}()
	writeResp(handle(ctx, req))
}
