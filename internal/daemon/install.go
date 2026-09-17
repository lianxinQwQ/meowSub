// install.go 守护进程的安装、启动与 root 直连路径。
package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"meowsub/internal/config"
	"meowsub/internal/graphical"
	"meowsub/internal/runner"
	"meowsub/internal/state"
)

func readMarkerSafe(runRoot string) (*state.Marker, error) {
	return state.ReadMarker(runRoot)
}

// UnitPath 宿主单元文件位置。
const UnitPath = "/etc/systemd/system/meowsubd.service"

// ConfigPath 配置唯一正式位置（同时是 CLI -f 的默认值）。
const ConfigPath = "/etc/meowsub/meowsub.toml"

const unitTemplate = `# 由 meowsub install-daemon 幂等生成，请勿手改
[Unit]
Description=meowsubd privilege broker
After=systemd-machined.service multi-user.target

[Service]
Type=simple
ExecStart=%s daemon-run -f %s
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
`

// Install 以 root 幂等完成三件事：规范正式配置权限、写入 systemd 单元、
// enable 该单元。单元 ExecStart 直接引用 selfBin——调用者经 os.Executable()
// 自我定位的运行时真实路径，不落在任何固定目录；已完成且内容一致时静默跳过。
func Install(cfgPath, selfBin string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("install-daemon 需要 root")
	}
	if selfBin == "" {
		return fmt.Errorf("无法定位 meowsub 自身可执行文件路径")
	}
	// 正式配置由管理员放置（模板常经 sudo cp 携带 0600），这里只规范目录与
	// 文件权限保证普通用户可读：内容无机密，访问门禁由守护进程基于
	// SO_PEERCRED 强制，与文件可读性无关。
	dir := filepath.Dir(ConfigPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if st, serr := os.Stat(dir); serr == nil && st.Mode().Perm() != 0o755 {
		if cerr := os.Chmod(dir, 0o755); cerr != nil {
			return cerr
		}
	}
	if st, serr := os.Stat(ConfigPath); serr == nil {
		if st.Mode().Perm() != 0o644 {
			if cerr := os.Chmod(ConfigPath, 0o644); cerr != nil {
				return cerr
			}
			fmt.Println("[install] 已将", ConfigPath, "权限调整为 0644")
		}
	} else {
		fmt.Println("[install] 提示：尚未放置正式配置，可 sudo cp sample.toml",
			ConfigPath, "后重跑安装")
	}
	unit := fmt.Sprintf(unitTemplate, quoteUnitArg(selfBin), ConfigPath)
	if old, err := os.ReadFile(UnitPath); err != nil || string(old) != unit {
		if err := os.WriteFile(UnitPath, []byte(unit), 0o644); err != nil {
			return err
		}
		fmt.Println("[install] 已写入", UnitPath)
	}
	for _, a := range [][]string{{"daemon-reload"}, {"enable", "meowsubd.service"}} {
		c := exec.Command("systemctl", a...)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("systemctl %v: %w", a, err)
		}
	}
	fmt.Println(`[install] 完成：下次开机自动随宿主启动；
立即使用可执行 systemctl start meowsubd，或以任意 meowsub start/open 触达`)
	fmt.Println("[install] 单元 ExecStart 引用", selfBin,
		"——移动或删除该文件后服务失效，重跑 install-daemon 即可刷新")
	return nil
}

// quoteUnitArg 按 systemd ExecStart 语法引用参数：安全字符原样；否则双引号
// 包裹并转义说明符——`%` 双写（systemd 规格），`$` 双写（环境变量展开）。
func quoteUnitArg(p string) string {
	if execWordSafe(p) {
		return p
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`)
	return `"` + r.Replace(p) + `"`
}

// Run 是 daemon-run 动词主体：预引导 autostart 组后阻塞服务 socket。
// 仅接受 root。cfgPath 是正式配置路径：启动即载入校验（失败随单元重启
// 暴露），此后每次引导前经 Server.reloadCfg 当场重读——daemon 存续期间
// 改配置，下一拍引导即生效，无需重启守护进程。
func Run(cfgPath string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("daemon-run 需要 root")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if st, serr := os.Stat(cfg.BaseDir); serr == nil {
		if sysAny := st.Sys(); sysAny != nil {
			fmt.Printf("[meowsubd] base_dir=%s（uid 鉴权以 root 为基线）\n", cfg.BaseDir)
		}
	}
	s := NewServer(cfg, cfgPath)
	cleanupLegacyOverlays() // 叠加层方案残留的一次性清退
	go func() {             // autostart 组随服务启动依次引导
		for _, g := range cfg.Groups {
			if !g.Autostart {
				continue
			}
			run, _, err := s.ensureInstance(g.Name)
			if err != nil {
				fmt.Printf("[meowsubd] autostart 组 %s 就绪失败: %v\n", g.Name, err)
				continue
			}
			if !s.running(run) {
				if berr := s.boot(run); berr != nil {
					fmt.Printf("[meowsubd] autostart 组 %s 引导失败: %v\n", g.Name, berr)
				} else {
					fmt.Printf("[meowsubd] autostart 组 %s 已引导为 %s\n",
						g.Name, MachineName(run))
				}
			}
		}
	}()
	return ServeUnix(SocketPath, s.Handle, s.bridgeShell)
}

// StartStop 处理 start/stop 动词：优先走守护进程；无服务且身为 root 时降级
// 直接执行等价操作。配置经 loadCfg 惰性加载——非 root 转发路径不读配置
// （正式配置仅 root 可读），仅 root 兜底分支需要 cfg.BaseDir。
func StartStop(loadCfg func() (*config.Config, error), start bool, name string) error {
	if name == "" {
		return fmt.Errorf("用法: meowsub start|stop <组名>")
	}
	op := OpStop
	if start {
		op = OpStart
	}
	resp, err := TryCall(Request{Op: op, Group: name})
	if err == nil {
		fmt.Println(resp.Data)
		return nil
	}
	if !asNoDaemon(err) || os.Geteuid() != 0 {
		return err
	}
	cfg, err := loadCfg()
	if err != nil {
		return err
	}
	p := pathsFor(cfg.BaseDir)
	group := name
	run := pickRunLike(p, group)
	if start {
		if _, ok := exists(p.Root(run)); !ok {
			src := filepath.Join(cfg.BaseDir, "build", group)
			if err := p.Deploy(src, run, true); err != nil {
				return err
			}
		}
		args := sessionBootArgs(cfg, run, graphical.Scan())
		args = append(args, tmpfsBootArgs(cfg, run)...)
		args = append(args, mountBootArgs(cfg, run)...)
		c := exec.Command("systemd-nspawn",
			append([]string{"--boot", "-D", p.Root(run),
				"--machine=" + MachineName(run)}, args...)...)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		return c.Run()
	}
	machine := MachineName(run)
	if exec.Command("machinectl", "status", machine).Run() != nil {
		fmt.Println(name, "本就未运行")
		return nil
	}
	pc := exec.Command("machinectl", "poweroff", machine)
	pc.Stdout, pc.Stderr = os.Stdout, os.Stderr
	return pc.Run()
}

// MaybeRootHint 统一“无守护进程”类错误的出口提示。
func MaybeRootHint(err error, isRoot bool, rootHint string) error {
	if !asNoDaemon(err) {
		return err
	}
	if isRoot && rootHint != "" {
		return fmt.Errorf("%w；已是 root：%s", err, rootHint)
	}
	if !isRoot {
		return fmt.Errorf("%w；请先 sudo meowsub install-daemon 安装并启动服务", err)
	}
	return err
}

// Ps 列出 daemon 视角的实例登记。
func Ps() error {
	resp, err := TryCall(Request{Op: OpPs})
	if err != nil {
		if asNoDaemon(err) {
			return fmt.Errorf("ps 需要 meowsubd：%w", err)
		}
		return err
	}
	rows, _ := resp.Data.([]interface{})
	if len(rows) == 0 {
		fmt.Println("（尚无登记实例）")
		return nil
	}
	for _, r := range rows {
		fmt.Println(r)
	}
	return nil
}

// LaunchAsRoot 无 daemon 时的 root 直连入口执行：确保实例与机器，解析目标
// （二进制名优先、其次 .desktop 展开），随后 machinectl 前台提交。
func LaunchAsRoot(cfg *config.Config, group, target string, args []string) error {
	argv, err := launchTarget(cfg.BaseDir, group, target, args)
	if err != nil {
		return err
	}
	run := argv[0]
	av := sessionShellArgv(cfg, run, 0, ForwardEnv(),
		MachineName(run), []string{"-q"}, argv[1:])
	c := exec.Command("machinectl", av...)
	c.Stdout, c.Stderr, c.Stdin = os.Stdout, os.Stderr, os.Stdin
	return c.Run()
}

// LaunchDesktopAsRoot 无 daemon 时 run-desktop 的 root 直连：确保实例，
// 按实例内路径或 ID 解析 .desktop 为 argv 后前台提交。
func LaunchDesktopAsRoot(cfg *config.Config, group, target string, args []string) error {
	p := pathsFor(cfg.BaseDir)
	run := pickRunLike(p, group)
	if InstanceLocked(cfg.BaseDir, run) {
		return fmt.Errorf("实例 %s 同步锁定中（build 收尾），稍后重试", run)
	}
	if _, ok := exists(p.Root(run)); !ok {
		if err := p.Deploy(filepath.Join(cfg.BaseDir, "build", group), run, true); err != nil {
			return err
		}
	}
	argv, err := ResolveDesktopFile(p.Root(run), target, args)
	if err != nil {
		return err
	}
	av := sessionShellArgv(cfg, run, 0, ForwardEnv(),
		MachineName(run), []string{"-q"}, argv)
	c := exec.Command("machinectl", av...)
	c.Stdout, c.Stderr, c.Stdin = os.Stdout, os.Stderr, os.Stdin
	return c.Run()
}

// launchTarget 确保 group 存在可用实例并返回 [run, 容器内argv...]。
func launchTarget(baseDir, group, target string, args []string) ([]string, error) {
	p := pathsFor(baseDir)
	run := pickRunLike(p, group)
	if InstanceLocked(baseDir, run) {
		return nil, fmt.Errorf("实例 %s 同步锁定中（build 收尾），稍后重试", run)
	}
	if _, ok := exists(p.Root(run)); !ok {
		if err := p.Deploy(filepath.Join(baseDir, "build", group), run, true); err != nil {
			return nil, err
		}
	}
	argv, err := ResolveOpen(p.Root(run), target, args)
	if err != nil {
		return nil, err
	}
	return append([]string{run}, argv...), nil
}

// --- 小工具 ---// --- 小工具 -------------------------------------------------------------

func pathsFor(base string) runner.Paths { return runner.Paths{BaseDir: base} }

func exists(path string) (os.FileInfo, bool) {
	fi, err := os.Stat(path)
	return fi, err == nil && fi.IsDir()
}

func pickRunLike(p runner.Paths, group string) string {
	runs, _ := p.List()
	for _, r := range runs {
		if r == group || strings.HasSuffix(r, "@"+group) {
			return r
		}
	}
	return group
}

func asNoDaemon(err error) bool {
	_, ok := err.(*ErrNoDaemon)
	return ok
}
