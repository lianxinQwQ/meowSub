package builder

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meowsub/internal/config"
	"meowsub/internal/execx"
	"meowsub/internal/planner"
	"meowsub/internal/reconcile"
	"meowsub/internal/staging"
	"meowsub/internal/state"
)

// ---- 脚手架 ----

func newNode(name, kind, parent string, groups, install []string) *planner.Node {
	return &planner.Node{
		Name: name, Kind: kind,
		Groups: groups, InstallSet: install,
	}
}

// 与 reconcile 测试同构的期望树
func docPlan(baseDir string) *planner.Plan {
	mid := newNode("code+dev-1a2b3c", planner.KindIntermediate, "", []string{"code", "dev"}, []string{"cargo", "rust"})
	code := newNode("code", planner.KindFinal, "", []string{"code"}, []string{"vscode"})
	dev := newNode("dev", planner.KindFinal, "", []string{"dev"}, []string{"libvterm", "vim"})
	tool := newNode("tool", planner.KindFinal, "", []string{"tool"}, []string{"libvterm", "neovim", "vim"})
	mid.Path = filepath.Join(baseDir, "sub", mid.Name)
	code.Parent, dev.Parent = mid, mid
	mid.Children = []*planner.Node{code, dev}
	tool.Path = filepath.Join(baseDir, tool.Name)
	p := &planner.Plan{
		BaseDir: baseDir,
		BasePkg: append(append([]string{}, planner.DefaultBasePackages...), "paru"),
		Roots:   []*planner.Node{mid, tool},
		Nodes:   []*planner.Node{mid, code, dev, tool},
		Closures: map[string]*planner.Closure{
			"code": {Pkgs: []string{"vscode", "cargo", "rust"}, Aur: map[string]bool{"vscode": true}},
			"dev":  {Pkgs: []string{"vim", "cargo", "rust", "libvterm"}, Aur: map[string]bool{}},
			"tool": {Pkgs: []string{"vim", "neovim", "libvterm"}, Aur: map[string]bool{}},
		},
	}
	// 成品位于 base_dir/<name>
	code.Path = filepath.Join(baseDir, "code")
	dev.Path = filepath.Join(baseDir, "dev")
	return p
}

func findCmd(t *testing.T, r *execx.RecordRunner, name string, substrings ...string) *execx.RecordedCmd {
	t.Helper()
	for _, c := range r.Cmds {
		if c.Name != name {
			continue
		}
		full := strings.Join(c.Args, "\x00")
		ok := true
		for _, s := range substrings {
			if !strings.Contains(full, s) {
				ok = false
				break
			}
		}
		if ok {
			return c
		}
	}
	t.Fatalf("command %s %v not found in %d cmds", name, substrings, len(r.Cmds))
	return nil
}

func cmdCount(r *execx.RecordRunner, name string) int {
	n := 0
	for _, c := range r.Cmds {
		if c.Name == name {
			n++
		}
	}
	return n
}

// TestNspawnTmpSizeArg nspawnBaseArgs 按 tmp 实参注入 /tmp 显式挂载：
// 值>0 追加 --tmpfs=/tmp:size=...（覆写 nspawn 自动 tmpfs，防容器内写
// /tmp ENOSPC），0 保持默认不注入。
func TestNspawnTmpSizeArg(t *testing.T) {
	b := New(&execx.RecordRunner{}, os.Stderr, t.TempDir())

	with := b.nspawnBaseArgs(4<<30, filepath.Join(b.BaseDir, "builder"), false, []string{"true"})
	if !strings.Contains(strings.Join(with, "\x00"), "--tmpfs=/tmp:size=4G") {
		t.Errorf("tmp>0 应注入 tmpfs 参数: %v", with)
	}

	without := b.nspawnBaseArgs(0, filepath.Join(b.BaseDir, "builder"), false, []string{"true"})
	if strings.Contains(strings.Join(without, "\x00"), "--tmpfs=") {
		t.Errorf("tmp=0 不应注入 tmpfs 参数: %v", without)
	}
}

// TestNspawnGroupTmpSize 组构建通道：/tmp 取成员组 build_tmp_size 最大值
// （共享中间节点服务多个组），与构建机器容器的 BuilderTmpSize 互不相干。
func TestNspawnGroupTmpSize(t *testing.T) {
	base := t.TempDir()
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	b.BuilderTmpSize = 4 << 30
	b.Groups = map[string]*config.Group{
		"code": {Name: "code", BuildTmpSize: 2 << 30},
		"dev":  {Name: "dev"},
	}

	mid := filepath.Join(base, "sub", "code+dev-x")
	if err := b.nspawnGroup(mid, []string{"code", "dev"}, "true"); err != nil {
		t.Fatalf("nspawnGroup: %v", err)
	}
	args := findCmd(t, r, "systemd-nspawn", "-D", mid).Args
	joined := strings.Join(args, "\x00")
	// 共享中间节点：取成员组最大值 2G，而非车间的 4G
	if !strings.Contains(joined, "--tmpfs=/tmp:size=2G") {
		t.Errorf("共享中间节点应取成员组最大 build_tmp_size: %v", args)
	}
	if !strings.Contains(joined, "pacman") {
		t.Errorf("容器命令应在 nspawn 参数之后: %v", args)
	}

	// 全员未声明：不注入，沿用 nspawn 默认
	if err := b.nspawnGroup(filepath.Join(base, "sub", "z"), []string{"dev"}, "true"); err != nil {
		t.Fatalf("nspawnGroup: %v", err)
	}
	plain := findCmd(t, r, "systemd-nspawn", "-D", filepath.Join(base, "sub", "z")).Args
	if strings.Contains(strings.Join(plain, "\x00"), "--tmpfs=") {
		t.Errorf("组均未声明 build_tmp_size 不应注入: %v", plain)
	}
}

// ---- ProbeReflink ----

func TestProbeReflinkOK(t *testing.T) {
	base := t.TempDir()
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	if err := b.ProbeReflink(); err != nil {
		t.Fatalf("ProbeReflink: %v", err)
	}
	c := findCmd(t, r, "cp", "--reflink=always")
	if !strings.HasPrefix(c.Args[len(c.Args)-1], base) ||
		!strings.HasPrefix(c.Args[len(c.Args)-2], base) {
		t.Errorf("probe files should live under base dir: %v", c.Args)
	}
	entries, _ := os.ReadDir(base)
	if len(entries) != 0 {
		t.Errorf("probe files not cleaned: %v", entries)
	}
}

func TestProbeReflinkFailsHard(t *testing.T) {
	base := t.TempDir()
	r := &execx.RecordRunner{FailOn: map[string]error{"cp": errors.New("Inappropriate file type for format")}}
	b := New(r, os.Stderr, base)
	err := b.ProbeReflink()
	if err == nil || !strings.Contains(err.Error(), "reflink") {
		t.Fatalf("want reflink error, got %v", err)
	}
}

// ---- ScanView ----

func TestScanView(t *testing.T) {
	base := t.TempDir()
	plan := docPlan(base)
	// ghost：期望中的组，但磁盘目录无标记
	ghost := newNode("ghost", planner.KindFinal, "", []string{"ghost"}, []string{"g-pkg"})
	ghost.Path = filepath.Join(base, "ghost")
	plan.Nodes = append(plan.Nodes, ghost)
	plan.Roots = append(plan.Roots, ghost)

	// 磁盘现状
	mk := func(rel string, m *state.Marker) {
		t.Helper()
		d := filepath.Join(base, rel)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if m != nil {
			if err := state.WriteMarker(d, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("arch", &state.Marker{Kind: state.KindBase, Name: "arch"})
	mk("tool", &state.Marker{Kind: state.KindFinal, Name: "tool", Parent: "arch",
		InstallSet: []string{"libvterm", "neovim", "vim"}})
	mk(filepath.Join("sub", "code+dev-1a2b3c"), &state.Marker{Kind: state.KindIntermediate,
		Name: "code+dev-1a2b3c", Parent: "arch", Groups: []string{"code", "dev"},
		InstallSet: []string{"cargo", "rust"}})
	mk("orphan-old-9f8e7d", &state.Marker{Kind: state.KindIntermediate, Name: "orphan-old-9f8e7d"})
	mk("ghost", nil) // 期望中的组但无标记

	b := New(&execx.RecordRunner{}, os.Stderr, base)
	view, err := b.ScanView(plan)
	if err != nil {
		t.Fatalf("ScanView: %v", err)
	}
	if !view.BaseExists || !view.BaseMarked {
		t.Errorf("base = exists:%v marked:%v", view.BaseExists, view.BaseMarked)
	}
	if _, ok := view.Existing["tool"]; !ok {
		t.Errorf("tool missing from Existing: %v", view.Existing)
	}
	if rec := view.Existing["code+dev-1a2b3c"]; rec == nil || rec.Parent != "arch" {
		t.Errorf("mid record = %+v", rec)
	}
	foundOrphan := false
	for _, o := range view.Orphans {
		if o.Name == "orphan-old-9f8e7d" {
			foundOrphan = true
		}
	}
	if !foundOrphan {
		t.Errorf("orphan not detected: %+v", view.Orphans)
	}
	wantGhost := filepath.Join(base, "ghost")
	if _, ok := view.UnmarkedAt[wantGhost]; !ok {
		t.Errorf("unmarked ghost not detected at %s: %v", wantGhost, view.UnmarkedAt)
	}
}

// ---- Execute：全新构建 ----

func TestExecuteFreshBuild(t *testing.T) {
	base := t.TempDir()
	plan := docPlan(base)
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)

	fresh := reconcile.Reconcile(plan, &reconcile.FsView{
		BaseExists: false, BaseMarked: false,
		UnmarkedAt: map[string]string{}, Existing: map[string]*state.NodeRecord{},
	}, reconcile.Options{})

	st, err := b.Execute(fresh, state.NewEmpty(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// 基础系统：先清场再 pacstrap，再容器内配置构建用户与 paru
	findCmd(t, r, "rm", "-rf")
	findCmd(t, r, "pacstrap", "-K", filepath.Join(base, "arch"),
		"base", "base-devel", "sudo", "git")
	// paru 与 keyring 由额外仓库供给，属于基础镜像
	if len(planner.ExtraBasePackages) < 2 {
		t.Fatalf("ExtraBasePackages 应包含 archlinuxcn-keyring/paru: %v",
			planner.ExtraBasePackages)
	}
	setup := findCmd(t, r, "systemd-nspawn", "-D", filepath.Join(base, "arch"), "bash")
	script := strings.Join(setup.Args, " ")
	for _, want := range []string{"useradd", "builder", "sudoers.d"} {
		if !strings.Contains(script, want) {
			t.Errorf("setup script missing %q:\n%s", want, script)
		}
	}

	// 成品/中间系统：cp --reflink=always 从父复制；父先于子
	findCmd(t, r, "cp", "--reflink=always", "-a")
	parentIdx, midIdx := -1, -1
	for i, c := range r.Cmds {
		if c.Name != "cp" {
			continue
		}
		dst := c.Args[len(c.Args)-1]
		if strings.HasSuffix(dst, "tool") && parentIdx < 0 {
			parentIdx = i
		}
		if strings.Contains(dst, "code+dev-1a2b3c") {
			midIdx = i
		}
	}
	if parentIdx < 0 || midIdx < 0 {
		t.Fatalf("copies missing: tool@%d mid@%d", parentIdx, midIdx)
	}
	// 中间层的安装命令（装配期统一走 pacman，AUR 来自池仓库）
	findCmd(t, r, "systemd-nspawn", "-D",
		filepath.Join(base, "sub", "code+dev-1a2b3c"),
		"pacman", "-Sy", "--needed", "--noconfirm", "cargo", "rust")

	// 标记文件全部落盘
	for _, rel := range []string{"arch", "tool", filepath.Join("sub", "code+dev-1a2b3c")} {
		m, err := state.ReadMarker(filepath.Join(base, rel))
		if err != nil || m == nil {
			t.Errorf("marker missing at %s: %v", rel, err)
		}
	}

	// 状态写盘且内容正确
	st2, err := state.Load(base)
	if err != nil {
		t.Fatalf("state load: %v", err)
	}
	if st2.Base == nil || len(st2.Base.Packages) != 5 {
		t.Errorf("base record = %+v", st2.Base)
	}
	if len(st2.Nodes) != 3 {
		t.Errorf("state nodes = %d, want 3（中间层收割即弃，状态只留成品）", len(st2.Nodes))
	}
	for _, rec := range st2.Nodes {
		if rec.Kind == state.KindIntermediate {
			t.Errorf("中间层不应留档：%+v", rec)
		}
	}
	_ = st
}

// ---- Execute：更新与清理 ----

func TestExecuteUpdateBaseAndPrune(t *testing.T) {
	base := t.TempDir()
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)

	acts := []*reconcile.Action{
		{Type: reconcile.ActUpdateBase, Name: "arch", Path: filepath.Join(base, "arch")},
		{Type: reconcile.ActPruneNode, Name: "old-xyz-112233",
			Path: filepath.Join(base, "sub", "old-xyz-112233")},
	}
	pre := state.NewEmpty()
	pre.Nodes = append(pre.Nodes, &state.NodeRecord{
		Name: "old-xyz-112233", Path: filepath.Join(base, "sub", "old-xyz-112233"),
		Kind: state.KindIntermediate,
	})
	if _, err := b.Execute(acts, pre, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	findCmd(t, r, "systemd-nspawn", "-D", filepath.Join(base, "arch"),
		"pacman", "-Syu", "--noconfirm")
	findCmd(t, r, "rm", "-rf", filepath.Join(base, "sub", "old-xyz-112233"))

	st, err := state.Load(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Nodes) != 0 {
		t.Errorf("pruned node still in state: %v", st.Nodes)
	}
}

func TestExecuteRecreateNodeReplacesInPlace(t *testing.T) {
	// 重建策略：已有子系统先删旧目录，再从父级 reflink 复制并安装增量
	base := t.TempDir()
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	dest := filepath.Join(base, "dev")
	acts := []*reconcile.Action{{
		Type: reconcile.ActRecreateNode, Name: "dev", Path: dest,
		Node: &planner.Node{Name: "dev", Kind: planner.KindFinal,
			InstallSet: []string{"vim"}},
	}}
	if _, err := b.Execute(acts, state.NewEmpty(), ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	findCmd(t, r, "rm", "-rf", dest)
	findCmd(t, r, "cp", "-a", "--reflink=always",
		filepath.Join(base, "arch"), dest)
	findCmd(t, r, "systemd-nspawn", "-D", dest,
		"pacman", "-Sy", "--needed", "--noconfirm", "vim")
}

// ---- 宿主机包缓存共享（只读挂载） ----

func TestParseCacheDirs(t *testing.T) {
	cases := []struct {
		name, conf string
		want       []string
	}{
		{"无 CacheDir 回退默认", "[options]\nHoldPkg = pacman\n",
			[]string{defaultPacmanCache}},
		{"单条自定义", "[options]\nCacheDir = /home/x/pkgs\n",
			[]string{"/home/x/pkgs"}},
		{"多条与一行多项", "#CacheDir = /ignored\n[options]\nCacheDir = /a /b\nCacheDir=/c\n",
			[]string{"/a", "/b", "/c"}},
	}
	for _, c := range cases {
		got := parseCacheDirs(c.conf)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestResolveHostCacheDirsSkipsMissing(t *testing.T) {
	dir := t.TempDir()
	conf := "CacheDir = " + dir + "\nCacheDir = /nonexistent-meowsub\n"
	got := resolveHostCacheDirsFrom(conf)
	if len(got) != 1 || got[0] != dir {
		t.Errorf("got %v, want 仅保留存在的 %s", got, dir)
	}
}

func TestEnsureExtraCacheDir(t *testing.T) {
	mnt := hostCacheMount
	base := "[options]\nHoldPkg = pacman glibc\n"
	once := ensureExtraCacheDir(base, mnt)
	if !strings.Contains(once, "CacheDir = "+mnt) {
		t.Fatalf("未插入缓存目录行:\n%s", once)
	}
	if iOpt, iAdd := strings.Index(once, "[options]"), strings.Index(once, "CacheDir = "+mnt); iOpt > iAdd {
		t.Errorf("应插在 [options] 段内:\n%s", once)
	}
	if twice := ensureExtraCacheDir(once, mnt); twice != once {
		t.Errorf("非幂等:\n一次:\n%s\n两次:\n%s", once, twice)
	}
	withExisting := "[options]\nCacheDir = /a\nCacheDir = /b\nRepo = x\n"
	got := ensureExtraCacheDir(withExisting, mnt)
	if !(strings.Index(got, "CacheDir = /b") < strings.Index(got, "CacheDir = "+mnt)) ||
		strings.Index(got, "Repo = x") < strings.Index(got, "CacheDir = "+mnt) {
		t.Errorf("应追加在最后一条 CacheDir 之后、段结束之前:\n%s", got)
	}
	if tail := ensureExtraCacheDir("foo bar\n", mnt); !strings.HasSuffix(tail, "CacheDir = "+mnt+"\n") {
		t.Errorf("无 [options] 时应追加到尾部:\n%s", tail)
	}
}

func TestEnsureOwnCacheDirFirst(t *testing.T) {
	own := defaultPacmanCache
	base := "#CacheDir = /ignored\n[options]\nHoldPkg = pacman glibc\n"
	got := ensureOwnCacheDir(base, own)
	if iOpt, iAdd := strings.Index(got, "[options]"), strings.Index(got, "CacheDir = "+own); iOpt > iAdd {
		t.Errorf("无 CacheDir 时应插在 [options] 之后:\n%s", got)
	}
	withExisting := "[options]\nCacheDir = /a /b\nCacheDir=/c\n"
	got = ensureOwnCacheDir(withExisting, own)
	if strings.Index(got, "CacheDir = "+own) > strings.Index(got, "CacheDir = /a") {
		t.Errorf("应插在第一条 CacheDir 之前:\n%s", got)
	}
	if twice := ensureOwnCacheDir(got, own); twice != got {
		t.Errorf("非幂等:\n一次:\n%s\n两次:\n%s", got, twice)
	}
	if head := ensureOwnCacheDir("foo bar\n", own); !strings.HasPrefix(head, "CacheDir = "+own+"\n") {
		t.Errorf("无 [options] 时应加在首部:\n%s", head)
	}
}

// ---- 宿主机代理透传 ----

// envStub 构造注入式环境查询，替代真实进程环境以保证测试密闭。
func envStub(kv map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := kv[k]
		return v, ok
	}
}

func TestCollectBuildEnvFromProcess(t *testing.T) {
	f := filepath.Join(t.TempDir(), "environment") // 不存在的文件：仅进程环境
	env := map[string]string{
		"http_proxy":  "http://127.0.0.1:7890",
		"HTTP_PROXY":  "http://must-not-pass:9999",
		"HTTPS_PROXY": "socks5://127.0.0.1:7891",
		"no_proxy":    "localhost,127.0.0.1",
		"ALL_PROXY":   "", // 空值视同未设置
	}
	got := collectBuildEnvFrom([]string{"http_proxy", "HTTPS_PROXY", "no_proxy"}, envStub(env), f)
	if want := "http_proxy=http://127.0.0.1:7890\nHTTPS_PROXY=socks5://127.0.0.1:7891\nno_proxy=localhost,127.0.0.1"; strings.Join(got, "\n") != want {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestCollectBuildEnvKeepsConfiguredOrderAndCase(t *testing.T) {
	got := collectBuildEnvFrom([]string{"HTTP_PROXY", "http_proxy"}, envStub(map[string]string{
		"http_proxy": "http://lower:1",
		"HTTP_PROXY": "http://upper:2",
	}), filepath.Join(t.TempDir(), "environment"))
	if strings.Join(got, "\n") != "HTTP_PROXY=http://upper:2\nhttp_proxy=http://lower:1" {
		t.Errorf("应按配置顺序并保持大小写输出，got %v", got)
	}
}

func TestCollectBuildEnvFallsBackToEnvironmentFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "environment")
	os.WriteFile(f, []byte(`# 注释
HTTP_PROXY=http://proxy.example.net:3128
LANG=zh_CN.UTF-8
`), 0o644)

	// 文件兜底保真：大写键保持大写
	got := collectBuildEnvFrom([]string{"HTTP_PROXY"}, envStub(map[string]string{}), f)
	if strings.Join(got, ",") != "HTTP_PROXY=http://proxy.example.net:3128" {
		t.Errorf("got %v", got)
	}

	// 进程环境优先；文件只补精确缺失的键（小写已有、大写缺 → 大写从文件补）
	got = collectBuildEnvFrom([]string{"http_proxy", "HTTP_PROXY"}, envStub(map[string]string{"http_proxy": "http://proc:1"}), f)
	if strings.Join(got, "\n") != "http_proxy=http://proc:1\nHTTP_PROXY=http://proxy.example.net:3128" {
		t.Errorf("got %v", got)
	}

	// 进程环境的精确键不被文件覆盖（大小写都是精确匹配）
	got = collectBuildEnvFrom([]string{"HTTP_PROXY"}, envStub(map[string]string{"HTTP_PROXY": "http://proc-up:4"}), f)
	if strings.Join(got, ",") != "HTTP_PROXY=http://proc-up:4" {
		t.Errorf("got %v", got)
	}
}

func TestNspawnInjectsConfiguredBuildEnv(t *testing.T) {
	base := t.TempDir()
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	b.BuildEnv = []string{"http_proxy=http://user:pw@127.0.0.1:7890"}
	acts := []*reconcile.Action{
		{Type: reconcile.ActUpdateBase, Name: "arch", Path: filepath.Join(base, "arch")},
	}
	if _, err := b.Execute(acts, state.NewEmpty(), ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	findCmd(t, r, "systemd-nspawn", "--setenv=http_proxy=http://user:pw@127.0.0.1:7890",
		"pacman", "-Syu", "--noconfirm")
}

func TestUpdateBaseTrustChainOrder(t *testing.T) {
	// 真实事故回归：cn 包在钥匙环就位前被校验 → unknown trust 拒收。
	// 顺序必须：仓库段/钥匙环同步 → 卸 paru-bin → -Syu → keyring 包 → paru 包。
	base := t.TempDir()
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	b.HostCacheDir = []string{}
	b.ExtraRepos = []config.Repository{
		{Name: "archlinuxcn", Servers: []string{"https://cn.example/$arch"}},
	}
	acts := []*reconcile.Action{
		{Type: reconcile.ActUpdateBase, Name: "arch", Path: filepath.Join(base, "arch")},
	}
	if _, err := b.Execute(acts, state.NewEmpty(), ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	idx := func(match func(*execx.RecordedCmd) bool) int {
		for i, c := range r.Cmds {
			if match(c) {
				return i
			}
		}
		return -1
	}
	nsIdx := func(subs ...string) int {
		return idx(func(c *execx.RecordedCmd) bool {
			j := strings.Join(c.Args, "\x00")
			if c.Name != "systemd-nspawn" {
				return false
			}
			for _, s := range subs {
				if !strings.Contains(j, s) {
					return false
				}
			}
			return true
		})
	}
	keyringSync := idx(func(c *execx.RecordedCmd) bool {
		return c.Name == "cp" && strings.Contains(strings.Join(c.Args, "\x00"), "/etc/pacman.d/gnupg")
	})
	removeParuBin := nsIdx("paru-bin")
	syu := nsIdx("-Syu")
	kbPkg := nsIdx("archlinuxcn-keyring")
	paruPkg := nsIdx("pacman -S --noconfirm paru")
	for name, i := range map[string]int{"keyring同步": keyringSync,
		"卸paru-bin": removeParuBin, "-Syu": syu, "keyring包": kbPkg, "paru包": paruPkg} {
		if i < 0 {
			t.Fatalf("%s 步骤缺失", name)
		}
	}
	if !(keyringSync < removeParuBin && removeParuBin < syu &&
		syu < kbPkg && kbPkg < paruPkg) {
		t.Fatalf("信任链顺序错误: 同步=%d 卸=%d Syu=%d kb=%d paru=%d",
			keyringSync, removeParuBin, syu, kbPkg, paruPkg)
	}
}

func TestNspawnBindsHostCacheReadOnly(t *testing.T) {
	base := t.TempDir()
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	b.HostCacheDir = []string{"/host-pkgs"}
	acts := []*reconcile.Action{
		{Type: reconcile.ActUpdateBase, Name: "arch", Path: filepath.Join(base, "arch")},
	}
	if _, err := b.Execute(acts, state.NewEmpty(), ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	findCmd(t, r, "systemd-nspawn", "--bind-ro=/host-pkgs:"+hostCacheMount,
		"pacman", "-Syu", "--noconfirm")
}

func TestNspawnNoBindWhenNoCache(t *testing.T) {
	base := t.TempDir()
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	b.HostCacheDir = []string{}
	acts := []*reconcile.Action{
		{Type: reconcile.ActUpdateBase, Name: "arch", Path: filepath.Join(base, "arch")},
	}
	if _, err := b.Execute(acts, state.NewEmpty(), ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, c := range r.Cmds {
		if c.Name == "systemd-nspawn" && strings.Contains(strings.Join(c.Args, "\x00"), "bind-ro") {
			t.Fatalf("无可用缓存目录时不应有 bind-ro: %v", c.Args)
		}
	}
}

func TestRecreateBaseRegistersCacheInContainerConf(t *testing.T) {
	// 全新基础系统应在容器内 pacman.conf 登记宿主机只读缓存目录
	base := t.TempDir()
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	root := filepath.Join(base, "arch")
	acts := []*reconcile.Action{{Type: reconcile.ActRecreateBase, Name: "arch", Path: root}}
	st := state.NewEmpty()
	st.Base = &state.BaseRecord{}
	if _, err := b.Execute(acts, st, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "etc", "pacman.conf"))
	if err != nil {
		t.Fatalf("容器 pacman.conf 未生成: %v", err)
	}
	if !strings.Contains(string(data), "CacheDir = "+hostCacheMount) {
		t.Fatalf("容器 pacman.conf 未登记宿主机缓存:\n%s", data)
	}
}

// ---- 预备层：包池同步 / 编译车间 / AUR 预构建 ----

// ---- 层尺寸预测 ----

func TestParsePkgSize(t *testing.T) {
	// LC_ALL=C 的 pacman -Si 输出："Installed Size : 9819.83 KiB"
	block := "Repository    : core\nName            : bash\n" +
		"Installed Size  : 9819.83 KiB\n"
	info, err := parsePkgBlock(block, false)
	if err != nil {
		t.Fatal(err)
	}
	isz, ok := parseInstalledSize("9819.83 KiB")
	if !ok || isz < 10055000 || isz > 10056000 { // ≈10_055_573
		t.Errorf("parseInstalledSize KiB = %d", isz)
	}
	isz2, ok2 := parseInstalledSize("305.62 MiB")
	if !ok2 || isz2 < 320465797-1024 || isz2 > 320465797+1024 {
		t.Errorf("parseInstalledSize MiB = %d, want ≈320465797", isz2)
	}
	if _, ok := parseInstalledSize("None"); ok {
		t.Errorf("None 不应可解析")
	}
	_ = info
}

func TestClosureSizeSum(t *testing.T) {
	// 每层增量 = InstallSet 的 isize 和（继承部分是 reflink 共享不占新空间）
	sizes := map[string]int64{
		"vim":  40 << 20,
		"rust": 300 << 20,
		"gtk3": 53 << 20,
	}
	got := installSetSize([]string{"vim", "rust", "gtk3", "unknown-pkg"}, sizes)
	want := int64(40+300+53) << 20
	if got != want {
		t.Errorf("installSetSize = %d, want %d（未知包不计入）", got, want)
	}
}

func TestSyncPoolDownloadsIntoPool(t *testing.T) {
	base := t.TempDir()
	r := &execx.RecordRunner{
		OutputFor: map[string]execx.OutputEntry{
			// -Spdd 列本地归档路径：file:// 命中收割，远端 URL 跳过
			"pacman -Spdd --noconfirm vim rust": {
				Out: "file:///var/cache/pacman/pkg/vim-9.2-x86_64.pkg.tar.zst\n" +
					"https://mirror.example/rust-1:1.97.1-1-x86_64.pkg.tar.zst\n",
			},
		},
	}
	b := New(r, os.Stderr, base)
	pool := filepath.Join(base, "pool")
	acts := []*reconcile.Action{{Type: reconcile.ActSyncPool, Name: "pool",
		Path: pool, Pkgs: []string{"vim", "rust"}}}
	if _, err := b.Execute(acts, state.NewEmpty(), ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// 下载必须走默认缓存（不带 --cachedir：避开 pacman 沙箱下载器限制）
	if c := findCmd(t, r, "pacman", "-Swdd", "--noconfirm", "vim", "rust"); true {
		if strings.Contains(strings.Join(c.Args, "\x00"), "--cachedir") {
			t.Errorf("-Swdd 不应再指定自定义缓存目录: %v", c.Args)
		}
	}
	findCmd(t, r, "pacman", "-Spdd", "--noconfirm", "vim", "rust")
	// 收割：仅 file:// 命中的进池，强制 reflink（失败即报错，不许降级拷贝）
	findCmd(t, r, "cp", "-a", "--reflink=always",
		"/var/cache/pacman/pkg/vim-9.2-x86_64.pkg.tar.zst",
		filepath.Join(pool, "vim-9.2-x86_64.pkg.tar.zst"))
	for _, c := range r.Cmds {
		if c.Name == "cp" && strings.Contains(strings.Join(c.Args, "\x00"), "rust") {
			t.Errorf("远端 URL 不应触发复制: %v", c.Args)
		}
	}
	// 空仓库库初始化仍在
	findCmd(t, r, "repo-add", filepath.Join(pool, poolDBName))
}

func TestEnsureBuilderLifecycle(t *testing.T) {
	base := t.TempDir()
	r1 := &execx.RecordRunner{}
	b := New(r1, os.Stderr, base)
	builderRoot := filepath.Join(base, "builder")

	first := []*reconcile.Action{{Type: reconcile.ActEnsureBuilder,
		Name: "builder", Path: builderRoot}}
	if _, err := b.Execute(first, state.NewEmpty(), ""); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	findCmd(t, r1, "rm", "-rf", builderRoot)
	findCmd(t, r1, "cp", "-a", "--reflink=always",
		filepath.Join(base, "arch"), builderRoot)
	// 池仓库登记应落盘（PKGDEST 改经 build_aur 的环境注入，无文件片段）
	confData, _ := os.ReadFile(filepath.Join(builderRoot, "etc", "pacman.conf"))
	if !strings.Contains(string(confData), "["+poolRepoName+"]") ||
		!strings.Contains(string(confData), "Server = file://"+poolMount) {
		t.Errorf("池仓库未登记:\n%s", confData)
	}
	m, err := state.ReadMarker(builderRoot)
	if err != nil || m == nil || m.Kind != state.KindBuilder {
		t.Fatalf("builder 标记缺失或类型错误: %v %+v", err, m)
	}

	// 第二轮：有标记 → 就地滚动更新，不再复制
	r2 := &execx.RecordRunner{}
	b2 := New(r2, os.Stderr, base)
	if _, err := b2.Execute(first, state.NewEmpty(), ""); err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	for _, c := range r2.Cmds {
		if c.Name == "cp" {
			t.Fatalf("已有车间不应再复制: %v", c.Args)
		}
	}
	// 已有车间：滚更后必须先探针 paru 健康（本桩健康，不触发重编）
	findCmd(t, r2, "systemd-nspawn", "-D", builderRoot,
		"runuser", "-u", "builder", "--", "paru", "--version")
	for _, c := range r2.Cmds {
		if c.Name == "systemd-nspawn" && strings.Contains(strings.Join(c.Args, "\x00"), "paru-bin") {
			t.Fatalf("健康车间不应触发 paru 重编: %v", c.Args)
		}
	}
	findCmd(t, r2, "systemd-nspawn", "-D", builderRoot,
		"pacman", "-Syu", "--noconfirm")
}

func TestEnsureBuilderRebuildsBrokenParu(t *testing.T) {
	// 车间里 paru 断链（libalpm 升版）：应自动重编后再继续
	base := t.TempDir()
	r1 := &execx.RecordRunner{}
	b := New(r1, os.Stderr, base)
	builderRoot := filepath.Join(base, "builder")
	if _, err := b.Execute([]*reconcile.Action{{Type: reconcile.ActEnsureBuilder,
		Name: "builder", Path: builderRoot}}, state.NewEmpty(), ""); err != nil {
		t.Fatalf("first Execute: %v", err)
	}

	broken := &failProbeRunner{inner: &execx.RecordRunner{}, root: builderRoot, armed: true}
	st := state.NewEmpty()
	st.Builder = &state.NodeRecord{Name: "builder", Path: builderRoot,
		Kind: state.KindBuilder, Parent: "arch"}
	b3 := New(broken, os.Stderr, base)
	if _, err := b3.Execute([]*reconcile.Action{{Type: reconcile.ActEnsureBuilder,
		Name: "builder", Path: builderRoot}}, st, ""); err != nil {
		t.Fatalf("broken-paru Execute: %v", err)
	}
	// 自愈路径：从已配置仓库强装仓库版 paru（不再容器内现编）
	findCmd(t, broken.inner, "systemd-nspawn", "-D", builderRoot,
		"pacman", "-S", "--noconfirm", "paru")
	// 重编后必须复检通过才放行构建
	findCmd(t, broken.inner, "systemd-nspawn", "-D", builderRoot,
		"runuser", "-u", "builder", "--", "paru", "--version")
}

// failProbeRunner 仅让“车间内 paru 健康探针”失败一次，其余透传记录。
type failProbeRunner struct {
	inner *execx.RecordRunner
	root  string
	armed bool // 未触发过失败则为真
}

func isParuProbe(args []string, root string) bool {
	// nspawn 会注入 bind/setenv 等前缀，按“目标根 + 尾部子序列”判定
	if len(args) < 8 || args[0] != "-D" || args[1] != root {
		return false
	}
	tail := args[len(args)-6:]
	for i, w := range []string{"runuser", "-u", "builder", "--", "paru", "--version"} {
		if tail[i] != w {
			return false
		}
	}
	return true
}

func (f *failProbeRunner) Run(name string, args ...string) error {
	if name == "systemd-nspawn" && isParuProbe(args, f.root) && f.armed {
		f.armed = false
		f.inner.Cmds = append(f.inner.Cmds,
			&execx.RecordedCmd{Name: name, Args: append([]string{}, args...)})
		return errors.New("exit status 127")
	}
	return f.inner.Run(name, args...)
}

func (f *failProbeRunner) RunOutput(name string, args ...string) (string, error) {
	return f.inner.RunOutput(name, args...)
}

func TestBuildAurRunsInBuilderAndRegistersRepo(t *testing.T) {
	base := t.TempDir()
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	pool := b.poolDir()
	if err := os.MkdirAll(pool, 0o755); err != nil {
		t.Fatal(err)
	}
	acts := []*reconcile.Action{{Type: reconcile.ActBuildAur, Name: "aur",
		Path: pool, Pkgs: []string{"visual-studio-code-bin"}}}
	if _, err := b.Execute(acts, state.NewEmpty(), ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	findCmd(t, r, "systemd-nspawn",
		"--setenv=PKGDEST="+poolMount, // 产物直落池：env 覆盖 makepkg.conf
		"--bind="+pool+":"+poolMount,  // 车间内对池是读写
		"runuser", "-u", "builder", "--", "paru", "-S",
		"--rebuild", "--needed", "--noconfirm", "visual-studio-code-bin")
	findCmd(t, r, "repo-add", b.poolDBPath())
	// 叶子清理：构建后从车间卸载目标，保持每轮可复现
	findCmd(t, r, "systemd-nspawn",
		"pacman", "-Rns", "--noconfirm", "visual-studio-code-bin")
}

// ---- HostResolver ----

// gzBytes 构造 AUR 转储桩所需的 gzip 字节流。
func gzBytes(t *testing.T, payload string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// emptyAUR 返回“转储不可用 + RPC 全部落空”的抓取桩，用于隔离官方仓库路径。
func emptyAUR() func(string) ([]byte, error) {
	return func(u string) ([]byte, error) {
		if u == aurDumpURL {
			return nil, fmt.Errorf("offline")
		}
		return []byte(`{"resultcount":0,"results":[]}`), nil
	}
}

func TestHostResolver(t *testing.T) {
	// 官方仓库经一条 pacman -Si 全量转储建索引；AUR 包与 Provides 走全量元数据转储。
	// 虚拟提供名应被解析为真实提供者：sh->bash，libssl.so=3-64->openssl。
	repoDump := strings.Join([]string{
		"Repository    : extra\nName            : vim\nDepends On    : glibc   ncurses\n                   libacl  sh  libssl.so=3-64\n",
		"Repository    : core\nName            : glibc\nDepends On    : linux-api-headers\n",
		"Repository    : core\nName            : linux-api-headers\n",
		"Repository    : core\nName            : ncurses\n",
		"Repository    : core\nName            : libacl\n",
		"Repository    : core\nName            : bash\nProvides       : sh\n",
		"Repository    : extra\nName            : openssl\nProvides       : libssl.so=3-64  libcrypto.so=3-64\n",
		"Repository    : extra\nName            : alsa-lib\n",
		"Repository    : extra\nName            : libxkbfile\n",
	}, "\n")
	aurDump := `[{"Name":"visual-studio-code-bin","Depends":["alsa-lib>=1.1.8","libxkbfile"],"Provides":["code","vscode"]}]`
	r := &execx.RecordRunner{OutputFor: map[string]execx.OutputEntry{
		"pacman -Si": {Out: repoDump},
	}}
	res := &HostResolver{
		Runner:   r,
		CacheDir: t.TempDir(),
		Fetcher: func(u string) ([]byte, error) {
			if u == aurDumpURL {
				return gzBytes(t, aurDump), nil
			}
			return nil, fmt.Errorf("unexpected url %s", u)
		},
	}
	c, err := res.Resolve([]string{"vim", "visual-studio-code-bin"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := "alsa-lib,bash,glibc,libacl,libxkbfile,linux-api-headers,ncurses,openssl,vim,visual-studio-code-bin"
	if strings.Join(c.Pkgs, ",") != want {
		t.Errorf("pkgs = %v\n want = %v", c.Pkgs, want)
	}
	if !c.Aur["visual-studio-code-bin"] || len(c.Aur) != 1 {
		t.Errorf("aur 标记错误: %v", c.Aur)
	}
	// 虚拟提供名本身不应进入闭包
	for _, p := range c.Pkgs {
		if p == "sh" || strings.HasPrefix(p, "libssl.so") {
			t.Errorf("虚拟提供名 %s 不应出现在闭包中", p)
		}
	}
}

func TestAurDumpResolvesVirtualProvider(t *testing.T) {
	// 官方仓库没有该虚拟名的提供者，AUR 转储中有：应解析到 AUR 提供者
	repoDump := strings.Join([]string{
		"Repository    : extra\nName            : app\nDepends On    : mystic-theme-data\n",
		"Repository    : extra\nName            : gtk3\n",
	}, "\n")
	aurDump := `[{"Name":"mystic-theme","Depends":["gtk3"],"Provides":["mystic-theme-data","mystic-theme-other"]}]`
	res := &HostResolver{
		Runner: &execx.RecordRunner{OutputFor: map[string]execx.OutputEntry{
			"pacman -Si": {Out: repoDump},
		}},
		CacheDir: t.TempDir(),
		Fetcher: func(u string) ([]byte, error) {
			if u == aurDumpURL {
				return gzBytes(t, aurDump), nil
			}
			return nil, fmt.Errorf("unexpected url %s", u)
		},
	}
	c, err := res.Resolve([]string{"app"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if strings.Join(c.Pkgs, ",") != "app,gtk3,mystic-theme" {
		t.Errorf("pkgs = %v, want [app gtk3 mystic-theme]", c.Pkgs)
	}
	if !c.Aur["mystic-theme"] {
		t.Errorf("aur 标记错误: %v", c.Aur)
	}
}

func TestAurExactNameWinsRegardlessOfResolverOrder(t *testing.T) {
	// AUR 真名没有 Provides，但另有多个包提供同名虚拟依赖；无论转储是否
	// 已被前一个软件组加载，都必须先选 AUR 真名 linuxqq。
	aurDump := `[
  {"Name":"seed"},
  {"Name":"linuxqq"},
  {"Name":"linuxqq-appimage","Provides":["linuxqq"]},
  {"Name":"linuxqq-nt","Provides":["linuxqq"]},
  {"Name":"linuxqq-nt-bwrap","Provides":["linuxqq"]}
]`
	newResolver := func() *HostResolver {
		return &HostResolver{
			Runner:   &execx.RecordRunner{OutputFor: map[string]execx.OutputEntry{"pacman -Si": {Out: ""}}},
			CacheDir: t.TempDir(),
			Fetcher: func(u string) ([]byte, error) {
				if u == aurDumpURL {
					return gzBytes(t, aurDump), nil
				}
				return nil, fmt.Errorf("unexpected url %s", u)
			},
		}
	}
	assertLinuxQQ := func(label string, c *planner.Closure, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s Resolve: %v", label, err)
		}
		if strings.Join(c.Pkgs, ",") != "linuxqq" {
			t.Errorf("%s pkgs = %v, want [linuxqq]", label, c.Pkgs)
		}
		if !c.Aur["linuxqq"] || len(c.Aur) != 1 {
			t.Errorf("%s aur = %v, want only linuxqq", label, c.Aur)
		}
	}

	// 冷启动顺序：直接解析目标。
	fresh := newResolver()
	freshClosure, freshErr := fresh.Resolve([]string{"linuxqq"})
	assertLinuxQQ("fresh", freshClosure, freshErr)

	// 预热顺序：先解析其他 AUR 软件组，使 aurProviders 已建立，再解析目标。
	warm := newResolver()
	if _, err := warm.Resolve([]string{"seed"}); err != nil {
		t.Fatalf("warmup Resolve: %v", err)
	}
	if got := len(warm.aurProviders["linuxqq"]); got != 3 {
		t.Fatalf("aurProviders[linuxqq] = %d, want 3", got)
	}
	warmClosure, warmErr := warm.Resolve([]string{"linuxqq"})
	assertLinuxQQ("warmed", warmClosure, warmErr)
}

func TestRepoProviderPrecedesAurCandidates(t *testing.T) {
	// 官方仓库提供虚拟依赖时，AUR 的同名包和 AUR Provider 都不应参与选择。
	repoDump := strings.Join([]string{
		"Repository    : extra\nName            : app\nDepends On    : tool\n",
		"Repository    : extra\nName            : repo-tool\nProvides       : tool\n",
	}, "\n")
	aurDump := `[{"Name":"tool"},{"Name":"aur-tool","Provides":["tool"]}]`
	res := &HostResolver{
		Runner: &execx.RecordRunner{OutputFor: map[string]execx.OutputEntry{
			"pacman -Si": {Out: repoDump},
		}},
		CacheDir: t.TempDir(),
		Fetcher: func(u string) ([]byte, error) {
			if u == aurDumpURL {
				return gzBytes(t, aurDump), nil
			}
			return nil, fmt.Errorf("unexpected url %s", u)
		},
	}
	c, err := res.Resolve([]string{"app"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if strings.Join(c.Pkgs, ",") != "app,repo-tool" {
		t.Errorf("pkgs = %v, want [app repo-tool]", c.Pkgs)
	}
	if len(c.Aur) != 0 {
		t.Errorf("aur = %v, want no AUR package", c.Aur)
	}
}

func TestAurDumpCacheReuse(t *testing.T) {
	// 第二次运行即使网络不可用，也应能从缓存目录加载转储正常解析
	repoDump := "Repository    : extra\nName            : app\nDepends On    : aurpkg\n"
	cache := t.TempDir()
	fetchOK := func(u string) ([]byte, error) {
		if u == aurDumpURL {
			return gzBytes(t, `[{"Name":"aurpkg"}]`), nil
		}
		return nil, fmt.Errorf("unexpected url %s", u)
	}
	first := &HostResolver{
		Runner: &execx.RecordRunner{OutputFor: map[string]execx.OutputEntry{
			"pacman -Si": {Out: repoDump},
		}},
		CacheDir: cache, Fetcher: fetchOK,
	}
	if _, err := first.Resolve([]string{"app"}); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	second := &HostResolver{
		Runner: &execx.RecordRunner{OutputFor: map[string]execx.OutputEntry{
			"pacman -Si": {Out: repoDump},
		}},
		CacheDir: cache,
		Fetcher: func(u string) ([]byte, error) {
			return nil, fmt.Errorf("network down")
		},
	}
	c, err := second.Resolve([]string{"app"})
	if err != nil {
		t.Fatalf("second Resolve (cache): %v", err)
	}
	if strings.Join(c.Pkgs, ",") != "app,aurpkg" {
		t.Errorf("pkgs = %v, want 缓存复用后仍完整", c.Pkgs)
	}
}

func TestProviderResolutionAmbiguous(t *testing.T) {
	// 两个包都提供 gl：确定性选择字典序第一个
	dump := strings.Join([]string{
		"Repository    : extra\nName            : app\nDepends On    : gl\n",
		"Repository    : extra\nName            : aaa-gl\nProvides       : gl\n",
		"Repository    : extra\nName            : zzz-gl\nProvides       : gl\n",
	}, "\n")
	r := &execx.RecordRunner{OutputFor: map[string]execx.OutputEntry{"pacman -Si": {Out: dump}}}
	res := &HostResolver{Runner: r, CacheDir: t.TempDir(), Fetcher: emptyAUR()}
	c, err := res.Resolve([]string{"app"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if strings.Join(c.Pkgs, ",") != "aaa-gl,app" {
		t.Errorf("pkgs = %v, want 歧义时取字典序首个 aaa-gl", c.Pkgs)
	}
}

func TestProviderPrefersNativeOverMultilib(t *testing.T) {
	// lib32-zlib 与 zlib 都提供 libz.so：必须选原生的 zlib
	dump := strings.Join([]string{
		"Repository    : extra\nName            : app\nDepends On    : libz.so\n",
		"Repository    : extra\nName            : lib32-zlib\nProvides       : libz.so\n",
		"Repository    : core\nName            : zlib\nProvides       : libz.so\n",
	}, "\n")
	r := &execx.RecordRunner{OutputFor: map[string]execx.OutputEntry{"pacman -Si": {Out: dump}}}
	res := &HostResolver{Runner: r, CacheDir: t.TempDir(), Fetcher: emptyAUR()}
	c, err := res.Resolve([]string{"app"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if strings.Join(c.Pkgs, ",") != "app,zlib" {
		t.Errorf("pkgs = %v, want [app zlib]", c.Pkgs)
	}
}

func TestProviderPrefersExactName(t *testing.T) {
	// 提供者中有一个与虚拟名同名：最可信，直接选它
	dump := strings.Join([]string{
		"Repository    : extra\nName            : app\nDepends On    : go\n",
		"Repository    : extra\nName            : gcc-go\nProvides       : go\n",
		"Repository    : extra\nName            : go\n",
	}, "\n")
	r := &execx.RecordRunner{OutputFor: map[string]execx.OutputEntry{"pacman -Si": {Out: dump}}}
	res := &HostResolver{Runner: r, CacheDir: t.TempDir(), Fetcher: emptyAUR()}
	c, err := res.Resolve([]string{"app"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if strings.Join(c.Pkgs, ",") != "app,go" {
		t.Errorf("pkgs = %v, want 真名提供者 go", c.Pkgs)
	}
}

func TestProviderResolutionVersionMismatch(t *testing.T) {
	// 需要 libfoo.so=9-99 但只有 =3-64 的提供者：视为未解析并跳过
	dump := strings.Join([]string{
		"Repository    : extra\nName            : app\nDepends On    : libfoo.so=9-99\n",
		"Repository    : extra\nName            : libfoo\nProvides       : libfoo.so=3-64\n",
	}, "\n")
	r := &execx.RecordRunner{OutputFor: map[string]execx.OutputEntry{"pacman -Si": {Out: dump}}}
	res := &HostResolver{Runner: r, CacheDir: t.TempDir(), Fetcher: emptyAUR()}
	c, err := res.Resolve([]string{"app"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if strings.Join(c.Pkgs, ",") != "app" {
		t.Errorf("pkgs = %v, want 仅 app（版本不匹配的提供者不采用）", c.Pkgs)
	}
}

func TestHostResolverRejectsMissingTarget(t *testing.T) {
	r := &execx.RecordRunner{OutputFor: map[string]execx.OutputEntry{
		"pacman -Si": {Out: "Repository : extra\nName : vim\n"},
	}}
	res := &HostResolver{Runner: r, CacheDir: t.TempDir(), Fetcher: emptyAUR()}
	_, err := res.Resolve([]string{"vim", "cargo"})
	if err == nil || !strings.Contains(err.Error(), "cargo") {
		t.Fatalf("顶层包不存在时必须报错，got %v", err)
	}
}

// ---- RenderPlan ----

func TestRenderPlanFresh(t *testing.T) {
	base := t.TempDir()
	plan := docPlan(base)
	acts := reconcile.Reconcile(plan, &reconcile.FsView{
		BaseExists: false,
		UnmarkedAt: map[string]string{}, Existing: map[string]*state.NodeRecord{},
	}, reconcile.Options{})
	out := RenderPlan(plan, acts)
	for _, want := range []string{"[CREATE]", "[CREATE-BASE]", "tool", "code+dev-1a2b3c"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "[CREATE]"); n != 4 {
		t.Errorf("[CREATE] x%d, want 4\n%s", n, out)
	}
}

// ---- 中间层收割即弃 ----

func TestExecuteDeletesIntermediatesAndDropsRecords(t *testing.T) {
	base := t.TempDir()
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	st := state.NewEmpty()
	st.Nodes = []*state.NodeRecord{
		{Name: "old-mid", Kind: state.KindIntermediate,
			Path: filepath.Join(base, "sub", "old-mid"), UpdatedAt: time.Now()},
		{Name: "code", Kind: state.KindFinal, Path: filepath.Join(base, "code")},
	}
	acts := []*reconcile.Action{{
		Type: reconcile.ActDeleteIntermediates, Name: "sub",
		Path:  filepath.Join(base, "sub"),
		Paths: []string{filepath.Join(base, "sub", "old-mid")},
	}}
	if _, err := b.Execute(acts, st, "hash"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	findCmd(t, r, "rm", "-rf", filepath.Join(base, "sub", "old-mid"))
	for _, rec := range st.Nodes {
		if rec.Kind == state.KindIntermediate {
			t.Errorf("中间层记录应从状态中摘除：%+v", st.Nodes)
		}
	}
	var keptCode bool
	for _, rec := range st.Nodes {
		if rec.Name == "code" {
			keptCode = true
		}
	}
	if !keptCode {
		t.Error("成品记录应保留")
	}
}

// ---- ScanView 与小组 ----

func smallNode(base string) *planner.Node {
	n := newNode("go", planner.KindFinal, "", []string{"go"}, []string{"gdep"})
	n.Path = filepath.Join(base, "go")
	return n
}

func writeMarkerFile(t *testing.T, dir string, m *state.Marker) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteMarker(dir, m); err != nil {
		t.Fatal(err)
	}
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanViewFillsSmallMapsAndKeepsOrphansDistinct(t *testing.T) {
	base := t.TempDir()
	plan := docPlan(base)
	small := smallNode(base)
	plan.SmallNodes = append(plan.SmallNodes, small)

	writeMarkerFile(t, small.Path, &state.Marker{
		Kind: state.KindFinal, Name: "go", Parent: "arch",
		Groups: []string{"go"}, InstallSet: []string{"gdep"},
	})
	writeMarkerFile(t, filepath.Join(base, "foreign"),
		&state.Marker{Kind: state.KindFinal, Name: "foreign"})

	b := New(&execx.RecordRunner{}, os.Stderr, base)
	view, err := b.ScanView(plan)
	if err != nil {
		t.Fatalf("ScanView: %v", err)
	}
	rec := view.Small["go"]
	if rec == nil || rec.Parent != "arch" {
		t.Fatalf("Small[go] 应解析出记录，got %+v", view.Small)
	}
	if view.Existing["go"] == nil {
		t.Error("Existing 也应登记小组，保持孤儿判定一致")
	}
	var orphanNames []string
	for _, o := range view.Orphans {
		orphanNames = append(orphanNames, o.Name)
	}
	for _, n := range orphanNames {
		if n == "go" {
			t.Errorf("小组不应被判为孤儿：%v", orphanNames)
		}
	}
	foreignFound := false
	for _, n := range orphanNames {
		if n == "foreign" {
			foreignFound = true
		}
	}
	if !foreignFound {
		t.Errorf("陌生成品 foreign 应是孤儿：%v", orphanNames)
	}
}

// ---- 内存构建路径 ----

// 新建小组：整份复制 arch 进内存 → 装包 → 差异经索引去重落盘。
func TestBuildFinalSmallNewFlowEndToEnd(t *testing.T) {
	base := t.TempDir()
	stageBase := t.TempDir()
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	b.stage = &staging.Stager{Runner: r, Base: stageBase}
	writeFileT(t, filepath.Join(base, "arch", "usr", "bin", "tool"), "arch-tool-content")
	writeFileT(t, filepath.Join(base, "arch", "etc", "pacman.conf"), "[options]\nHoldPkg=base\n")

	small := smallNode(base) // InstallSet=[gdep]
	treeFinal := newNode("tool", planner.KindFinal, "", []string{"tool"}, []string{"t-pkg"})
	treeFinal.Path = filepath.Join(base, "tool")
	acts := []*reconcile.Action{
		{Type: reconcile.ActCreateNode, Name: "tool", Node: treeFinal, Path: treeFinal.Path},
		{Type: reconcile.ActBuildFinalSmall, Name: small.Name, Path: small.Path, Node: small},
	}
	memSeen := ""
	b.afterStage = func(mem string) {
		memSeen = mem
		// 与 arch 同内容（应命中索引 reflink 自 arch）+ 独有新文件（真拷贝）
		writeFileT(t, filepath.Join(mem, "usr", "bin", "tool"), "arch-tool-content")
		writeFileT(t, filepath.Join(mem, "usr", "bin", "gonly"), "go-exclusive-bytes")
	}

	st, err := b.Execute(acts, state.NewEmpty(), "hash")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	mount := findCmd(t, r, "mount", "-t", "tmpfs")
	if !strings.Contains(strings.Join(mount.Args, "\x00"), stageBase) {
		t.Errorf("tmpfs 应挂在测试工作区：%v", mount.Args)
	}
	findCmd(t, r, "cp", "--reflink=auto", filepath.Join(base, "arch")+"/.")
	if memSeen == "" {
		t.Fatal("afterStage 未被调用，内存构建链路断裂")
	}
	findCmd(t, r, "systemd-nspawn", "-D", memSeen, "pacman", "--needed", "gdep")
	if conf, cerr := os.ReadFile(filepath.Join(memSeen, "etc", "pacman.conf")); cerr == nil {
		if !strings.Contains(string(conf), poolRepoName) {
			t.Error("内存副本应登记本地软件包池仓库")
		}
	}
	hit := findLandingCp(t, r,
		filepath.Join(base, "arch", "usr", "bin", "tool"),
		filepath.Join(base, "go", "usr", "bin", "tool"))
	if hit == nil {
		t.Fatal("共享文件应强制 reflink 自 arch")
	}
	got, gerr := os.ReadFile(filepath.Join(base, "go", "usr", "bin", "gonly"))
	if gerr != nil || string(got) != "go-exclusive-bytes" {
		t.Errorf("独占文件应实际落盘：%v %q", gerr, got)
	}
	mk, merr := state.ReadMarker(filepath.Join(base, "go"))
	if merr != nil || mk == nil || mk.Kind != state.KindFinal ||
		mk.Name != "go" || mk.Parent != "arch" ||
		strings.Join(mk.InstallSet, ",") != "gdep" {
		t.Errorf("小组成品标记不符：%v %+v", merr, mk)
	}
	if findRecord(st.Nodes, "go") == nil {
		t.Error("状态应登记小组成品")
	}
}

// memInstallExtra：尺寸表命中取 isize；池包按压缩体积 ×3；.sig 不误匹配；
// 毫无依据的包盲计并提示。
func TestMemInstallExtraSources(t *testing.T) {
	old := renderSizes
	renderSizes = map[string]int64{"gdep": 100}
	t.Cleanup(func() { renderSizes = old })

	base := t.TempDir()
	pool := filepath.Join(base, "pool")
	if err := os.MkdirAll(pool, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(pool, "aurp-1.0-1-x86_64.pkg.tar.zst"), "0123456789")
	writeFileT(t, filepath.Join(pool, "aurp-1.0-1-x86_64.pkg.tar.zst.sig"), "sig-bytes")
	b := &Builder{BaseDir: base}

	extra, blind := b.memInstallExtra([]string{"gdep", "aurp", "ghost"})
	// gdep=100 精确；aurp=10*3=30 池包折算；ghost 无依据 → 盲计。
	// 合计 ×1.2：(130) + 130/5 = 156。
	if want := int64(156); extra != want {
		t.Errorf("extra = %d, want %d", extra, want)
	}
	if blind != 1 {
		t.Errorf("blind = %d, want 1", blind)
	}
}

func TestStagerCarriesMemTmpfsSize(t *testing.T) {
	b := &Builder{MemTmpfsSize: 32 << 30}
	if got := b.stager().MinSize; got != 32<<30 {
		t.Errorf("stager.MinSize = %d, want %d", got, int64(32<<30))
	}
}

// 安装增量计入 tmpfs 容量：尺寸表注入大包后挂载 size= 应顶破 4G 下限，
// 而不是重演"装下了树、装包撞死空间检查"的事故。
func TestMemBuildSizesTmpfsByInstallSet(t *testing.T) {
	old := renderSizes
	renderSizes = map[string]int64{"gdep": 6 << 30}
	t.Cleanup(func() { renderSizes = old })

	base := t.TempDir()
	stageBase := t.TempDir()
	r := &execx.RecordRunner{}
	var buf bytes.Buffer
	b := New(r, &buf, base)
	b.stage = &staging.Stager{Runner: r, Base: stageBase, SkipFreeCheck: true}
	writeFileT(t, filepath.Join(base, "arch", "usr", "bin", "tool"), "arch-tool-content")

	small := smallNode(base)
	acts := []*reconcile.Action{
		{Type: reconcile.ActBuildFinalSmall, Name: small.Name, Path: small.Path, Node: small},
	}
	b.afterStage = func(mem string) {
		writeFileT(t, filepath.Join(mem, "usr", "bin", "tool"), "arch-tool-content")
		writeFileT(t, filepath.Join(mem, "usr", "bin", "gonly"), "go-exclusive-bytes")
	}
	if _, err := b.Execute(acts, state.NewEmpty(), "hash"); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	mount := findCmd(t, r, "mount", "-t", "tmpfs")
	if mount == nil {
		t.Fatal("应挂载 tmpfs")
	}
	// extra = 6GiB × 1.2 = 7.2GiB，加微小树向上取整 → 8G
	if !strings.Contains(strings.Join(mount.Args, "\x00"), "size=8G") {
		t.Errorf("挂载容量应计入安装增量：%v", mount.Args)
	}
	if !strings.Contains(buf.String(), "tmpfs 安装增量预估") {
		t.Errorf("应输出增量预估日志：%s", buf.String())
	}
}

// 就地滚更：标记 InstallSet 与规划相等（仅版本漂移）→ 副本内 -Syu，
// 不做任何装缺删余；差异落盘仍走 diff（副本中消失的文件从盘上移除）。
func TestUpdateFinalSmallRollsForwardInPlace(t *testing.T) {
	base := t.TempDir()
	stageBase := t.TempDir()
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	b.stage = &staging.Stager{Runner: r, Base: stageBase}
	writeFileT(t, filepath.Join(base, "arch", "usr", "bin", "tool"), "arch-tool-content")

	small := smallNode(base) // InstallSet = ["gdep"]
	writeFileT(t, filepath.Join(small.Path, "usr", "bin", "tool"), "arch-tool-content")
	writeFileT(t, filepath.Join(small.Path, "etc", "stale-conf"), "leftover-from-old-package")
	writeMarkerFile(t, small.Path, &state.Marker{
		Kind: state.KindFinal, Name: "go", Parent: "arch",
		Groups: []string{"go"}, InstallSet: []string{"gdep"},
	})
	oldRec := &state.NodeRecord{Name: "go", Kind: state.KindFinal, Parent: "arch",
		Path: small.Path, InstallSet: []string{"gdep"}}

	acts := []*reconcile.Action{{
		Type: reconcile.ActUpdateFinalSmall, Name: "go", Path: small.Path,
		Node: small, Record: oldRec,
	}}
	b.afterStage = func(mem string) {
		// 模拟容器内 -Syu 结果：tool 升级
		writeFileT(t, filepath.Join(mem, "usr", "bin", "tool"), "upgraded-content")
	}
	if _, err := b.Execute(acts, state.NewEmpty(), "hash"); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if cmdIndexOf(r, "systemd-nspawn", "pacman", "-Syu") < 0 {
		t.Fatal("滚更路径应执行 pacman -Syu")
	}
	for _, c := range r.Cmds {
		s := strings.Join(c.Args, "\x00")
		if strings.Contains(s, "--needed") || strings.Contains(s, "-Rs") ||
			strings.Contains(s, "--asdeps") {
			t.Fatalf("滚更路径不应有装缺/删余命令: %v", c.Args)
		}
	}
	got, ferr := os.ReadFile(filepath.Join(small.Path, "usr", "bin", "tool"))
	if ferr != nil || string(got) != "upgraded-content" {
		t.Errorf("升级文件应落盘: %v %q", ferr, got)
	}
	if _, serr := os.Stat(filepath.Join(small.Path, "etc", "stale-conf")); !os.IsNotExist(serr) {
		t.Error("副本中消失的文件应从盘上移除")
	}
	mk, _ := state.ReadMarker(small.Path)
	if mk == nil || strings.Join(mk.InstallSet, ",") != "gdep" {
		t.Errorf("标记的安装集应刷新：%+v", mk)
	}
}

// 小组重建：标记 InstallSet 与规划不等 → 从基础 arch 整份重来，全量安装，
// 落盘 diff 以旧成品为 lower（消失文件移除、未变文件不重写）。
func TestSmallRebuildOnSetDifference(t *testing.T) {
	base := t.TempDir()
	stageBase := t.TempDir()
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	b.stage = &staging.Stager{Runner: r, Base: stageBase}
	writeFileT(t, filepath.Join(base, "arch", "usr", "bin", "tool"), "arch-tool-content")

	small := smallNode(base)
	small.InstallSet = []string{"gdep-new"}
	writeFileT(t, filepath.Join(small.Path, "usr", "bin", "tool"), "arch-tool-content")
	writeFileT(t, filepath.Join(small.Path, "etc", "stale-conf"), "leftover-from-old-package")
	writeMarkerFile(t, small.Path, &state.Marker{
		Kind: state.KindFinal, Name: "go", Parent: "arch",
		Groups: []string{"go"}, InstallSet: []string{"gdep-old"},
	})
	oldRec := &state.NodeRecord{Name: "go", Kind: state.KindFinal, Parent: "arch",
		Path: small.Path, InstallSet: []string{"gdep-old"}}

	acts := []*reconcile.Action{{
		Type: reconcile.ActBuildFinalSmall, Name: "go", Path: small.Path,
		Node: small, Record: oldRec,
	}}
	b.afterStage = func(mem string) {
		writeFileT(t, filepath.Join(mem, "usr", "bin", "tool"), "arch-tool-content")
		writeFileT(t, filepath.Join(mem, "usr", "bin", "fresh"), "fresh-bytes")
	}
	if _, err := b.Execute(acts, state.NewEmpty(), "hash"); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	cpStage := findCmd(t, r, "cp", "--reflink=auto", filepath.Join(base, "arch")+"/.")
	if cpStage == nil || !strings.Contains(strings.Join(cpStage.Args, "\x00"), "full-build-go") {
		t.Errorf("重建应整份复制基础 arch 进内存：%v", cpStage)
	}
	if cmdIndexOf(r, "systemd-nspawn", "pacman", "--needed", "gdep-new") < 0 {
		t.Fatal("重建应全量安装新 InstallSet")
	}
	for _, c := range r.Cmds {
		s := strings.Join(c.Args, "\x00")
		if strings.Contains(s, "-Rs") || strings.Contains(s, "--asdeps") || strings.Contains(s, "-Syu") {
			t.Fatalf("重建路径不应有滚更/删余命令: %v", c.Args)
		}
	}
	fresh, ferr := os.ReadFile(filepath.Join(small.Path, "usr", "bin", "fresh"))
	if ferr != nil || string(fresh) != "fresh-bytes" {
		t.Errorf("新装文件应落盘：%v %q", ferr, fresh)
	}
	if _, serr := os.Stat(filepath.Join(small.Path, "etc", "stale-conf")); !os.IsNotExist(serr) {
		t.Error("旧成品独有的残留文件应从盘上移除")
	}
	mk, _ := state.ReadMarker(small.Path)
	if mk == nil || strings.Join(mk.InstallSet, ",") != "gdep-new" {
		t.Errorf("标记的安装集应刷新：%+v", mk)
	}
}

// findLandingCp 在记录中找 src->dst 的强制 reflink 命令（区分于建树期的
// 整层复制）。
func findLandingCp(t *testing.T, r *execx.RecordRunner, src, dst string) *execx.RecordedCmd {
	t.Helper()
	for _, c := range r.Cmds {
		if c.Name != "cp" || len(c.Args) < 2 {
			continue
		}
		full := strings.Join(c.Args, "\x00")
		if strings.Contains(full, "--reflink=always") &&
			c.Args[len(c.Args)-2] == src && c.Args[len(c.Args)-1] == dst {
			return c
		}
	}
	return nil
}

func cmdIndexOf(r *execx.RecordRunner, name string, substrings ...string) int {
	for i, c := range r.Cmds {
		if c.Name != name {
			continue
		}
		full := strings.Join(c.Args, "\x00")
		ok := true
		for _, s := range substrings {
			if !strings.Contains(full, s) {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// 先落盘小组的独特文件须被后处理小组复用（子树记录传递）。
func TestSecondSmallReusesFirstCommittedFile(t *testing.T) {
	base := t.TempDir()
	stageBase := t.TempDir()
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	b.stage = &staging.Stager{Runner: r, Base: stageBase}
	writeFileT(t, filepath.Join(base, "arch", "usr", "bin", "tool"), "arch-tool-content")

	goN := smallNode(base) // InstallSet=[gdep]
	zeta := newNode("zeta", planner.KindFinal, "", []string{"zeta"}, []string{"zdep"})
	zeta.Path = filepath.Join(base, "zeta")

	secret := "first-group-secret"
	b.afterStage = func(mem string) {
		switch {
		case strings.HasSuffix(mem, "full-build-go"):
			writeFileT(t, filepath.Join(mem, "opt", "unique.bin"), secret)
		case strings.HasSuffix(mem, "full-build-zeta"):
			writeFileT(t, filepath.Join(mem, "opt", "unique.bin"), secret)
		}
	}
	acts := []*reconcile.Action{
		{Type: reconcile.ActBuildFinalSmall, Name: goN.Name, Path: goN.Path, Node: goN},
		{Type: reconcile.ActBuildFinalSmall, Name: zeta.Name, Path: zeta.Path, Node: zeta},
	}
	if _, err := b.Execute(acts, state.NewEmpty(), "hash"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	reused := false
	for _, c := range r.Cmds {
		if c.Name != "cp" || len(c.Args) < 2 {
			continue
		}
		full := strings.Join(c.Args, "\x00")
		if strings.Contains(full, "--reflink=always") &&
			c.Args[len(c.Args)-2] == filepath.Join(base, "go", "opt", "unique.bin") &&
			c.Args[len(c.Args)-1] == filepath.Join(base, "zeta", "opt", "unique.bin") {
			reused = true
		}
	}
	if !reused {
		t.Error("后处理小组应 reflink 复用先落盘小组刚写入的独特文件")
	}
	// 假命令环境下 cp 不产生真实字节：zeta 目录应保持无内容落盘（reflink
	// 由真实执行器完成），但 go 侧的独占文件必须真实存在以供复用。
	data, err := os.ReadFile(filepath.Join(base, "go", "opt", "unique.bin"))
	if err != nil || string(data) != secret {
		t.Errorf("先落盘小组的独占文件应真实存在：%v %q", err, data)
	}
}

func TestRenderPlanSmallSectionAndDeleteSummary(t *testing.T) {
	base := t.TempDir()
	plan := docPlan(base)
	goN := newNode("go", planner.KindFinal, "", []string{"go"}, []string{"gdep"})
	goN.Path = filepath.Join(base, "go")
	edN := newNode("ed", planner.KindFinal, "", []string{"ed"}, []string{"edep"})
	edN.Path = filepath.Join(base, "ed")
	plan.SmallNodes = []*planner.Node{edN, goN}

	view := &reconcile.FsView{BaseExists: true, BaseMarked: true,
		UnmarkedAt: map[string]string{},
		Existing:   map[string]*state.NodeRecord{},
		Small: map[string]*state.NodeRecord{
			"ed": {Name: "ed", Kind: state.KindFinal, Parent: "arch",
				Path: filepath.Join(base, "ed"), InstallSet: []string{"edep"}},
		}}
	acts := reconcile.Reconcile(plan, view, reconcile.Options{})
	out := RenderPlan(plan, acts)
	for _, want := range []string{"[BUILD-MEM] go", "[SYNC] ed", "内存构建", "中间层清理"} {
		if !strings.Contains(out, want) {
			t.Errorf("render 缺少 %q：\n%s", want, out)
		}
	}
}

// ---- 第二轮巡检 ----

// 就地同步动作缺记录时应报错而非 panic（防御非法手工构造的动作表）。
func TestUpdateFinalSmallWithoutRecordErrors(t *testing.T) {
	base := t.TempDir()
	r := &execx.RecordRunner{}
	b := New(r, os.Stderr, base)
	b.stage = &staging.Stager{Runner: r, Base: t.TempDir()}
	n := smallNode(base)
	writeFileT(t, filepath.Join(n.Path, "usr", "bin", "tool"), "content")
	writeMarkerFile(t, n.Path, &state.Marker{Kind: state.KindFinal, Name: "go"})
	acts := []*reconcile.Action{{
		Type: reconcile.ActUpdateFinalSmall, Name: "go", Path: n.Path,
		Node: n, Record: nil,
	}}
	if _, err := b.Execute(acts, state.NewEmpty(), ""); err == nil {
		t.Fatal("缺 Record 的就地同步应报错")
	}
}

// 常规文件被升级为 symlink：既不算移除也不算常规新增，应记为符号链接
// 落盘，由落盘阶段替换旧文件。
func TestDiffForLandingLandsSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	staged := filepath.Join(root, "staged")
	lower := filepath.Join(root, "lower")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(staged, "usr", "bin", "shim")), 0o755); err != nil {
		t.Fatal(err)
	}
	// staged 侧是 symlink；lower 侧是同名常规文件
	if err := os.Symlink("/bin/busybox", filepath.Join(staged, "usr", "bin", "shim")); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(lower, "usr", "bin", "shim"), "was-regular")
	newFiles, removed, newLinks := diffForLanding(staged, lower)
	if len(newFiles) != 0 || len(removed) != 0 {
		t.Errorf("类型替换不应进常规增删清单：new=%v removed=%v", newFiles, removed)
	}
	if len(newLinks) != 1 || newLinks[0] != "usr/bin/shim" {
		t.Errorf("symlink 替换常规文件应记为链接落盘：links=%v", newLinks)
	}
}

// TestLogindDropInFixed 运行时目录保留策略必须随镜像走：容器内 logind
// 在最后一个会话关闭后拆除 /run/user/<uid>（user-runtime-dir），而那里
// 挂着宿主会话的叠加视图，按会话拆除会连带拆掉视图（真实事故）。初始
// 系统准备与幂等补写两处脚本都必须固定 UserStopDelaySec=infinity。
func TestLogindDropInFixed(t *testing.T) {
	for name, script := range map[string]string{
		"baseSetupScript":      baseSetupScript,
		"logindDropInCommands": logindDropInCommands,
	} {
		if !strings.Contains(script, "/etc/systemd/logind.conf.d/meowsub.conf") {
			t.Errorf("%s 缺少 drop-in 落点 /etc/systemd/logind.conf.d/meowsub.conf", name)
		}
		if !strings.Contains(script, "UserStopDelaySec=infinity") {
			t.Errorf("%s 缺少 UserStopDelaySec=infinity", name)
		}
	}
}
