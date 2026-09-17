package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTmp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "meowsub.toml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	p := writeTmp(t, `
base_dir = "/srv/subs"

[tool]
packages = ["vim", "neovim"]

[code]
packages = ["visual-studio-code-bin", "cargo"]
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BaseDir != "/srv/subs" {
		t.Errorf("BaseDir = %q, want /srv/subs", cfg.BaseDir)
	}
	if len(cfg.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(cfg.Groups))
	}
	g0 := cfg.Groups[0]
	if g0.Name != "code" || len(g0.Packages) != 2 || g0.Packages[1] != "cargo" {
		// 表在 TOML 中按声明顺序出现；code 在前
		t.Errorf("group0 = %+v", g0)
	}
}

func TestLoadBuildEnv(t *testing.T) {
	p := writeTmp(t, `
base_dir = "/srv/subs"
build_env = ["HTTP_PROXY", "CUSTOM_FLAG", "http_proxy"]

[tool]
packages = ["vim"]
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := strings.Join(cfg.BuildEnv, ","), "HTTP_PROXY,CUSTOM_FLAG,http_proxy"; got != want {
		t.Fatalf("BuildEnv = %q, want %q", got, want)
	}
}

func TestLoadRejectsBadBuildEnv(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"非数组", `"CUSTOM_FLAG"`},
		{"空变量名", `["CUSTOM_FLAG", ""]`},
		{"非法变量名", `["BAD-NAME"]`},
		{"变量名不能以数字开头", `["1BAD"]`},
		{"重复变量名", `["CUSTOM_FLAG", "CUSTOM_FLAG"]`},
	}
	for _, tc := range cases {
		p := writeTmp(t, fmt.Sprintf(`
base_dir = "/srv/subs"
build_env = %s

[tool]
packages = ["vim"]
`, tc.value))
		if _, err := Load(p); err == nil {
			t.Errorf("%s: 非法 build_env 应报错", tc.name)
		}
	}
}

func TestLoadTmpSizes(t *testing.T) {
	p := writeTmp(t, `
base_dir = "/srv/subs"
builder_tmp_size = "4G"
mem_tmpfs_size = "16G"

[tool]
packages = ["vim"]
build_tmp_size = "2G"
run_tmp_size = "8G"

[code]
packages = ["git"]
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BuilderTmpSize != 4<<30 {
		t.Errorf("BuilderTmpSize = %d, want 4G", cfg.BuilderTmpSize)
	}
	if cfg.MemTmpfsSize != 16<<30 {
		t.Errorf("MemTmpfsSize = %d, want 16G", cfg.MemTmpfsSize)
	}
	var tool, code *Group
	for _, g := range cfg.Groups {
		switch g.Name {
		case "tool":
			tool = g
		case "code":
			code = g
		}
	}
	if tool == nil || code == nil {
		t.Fatalf("软件组缺失: %+v", cfg.Groups)
	}
	if tool.BuildTmpSize != 2<<30 || tool.RunTmpSize != 8<<30 {
		t.Errorf("tool tmp = build %d / run %d, want 2G / 8G",
			tool.BuildTmpSize, tool.RunTmpSize)
	}
	if code.BuildTmpSize != 0 || code.RunTmpSize != 0 {
		t.Errorf("未声明组的 tmp 应为零值: build %d / run %d",
			code.BuildTmpSize, code.RunTmpSize)
	}
}

func TestLoadTmpSizeInvalid(t *testing.T) {
	p := writeTmp(t, `
base_dir = "/srv/subs"
builder_tmp_size = "abc"

[tool]
packages = ["vim"]
`)
	if _, err := Load(p); err == nil {
		t.Fatal("非法 builder_tmp_size 应报错")
	}
	p2 := writeTmp(t, `
base_dir = "/srv/subs"

[tool]
packages = ["vim"]
run_tmp_size = "xyz"
`)
	if _, err := Load(p2); err == nil {
		t.Fatal("非法 run_tmp_size 应报错")
	}
	p3 := writeTmp(t, `
base_dir = "/srv/subs"
mem_tmpfs_size = "abc"

[tool]
packages = ["vim"]
`)
	if _, err := Load(p3); err == nil {
		t.Fatal("非法 mem_tmpfs_size 应报错")
	}
}

func TestParseSize(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"524288", 524288},
		{"800M", 800 << 20},
		{"2g", 2 << 30},
		{"512K", 512 << 10},
	} {
		got, err := ParseSize(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "abc", "3.5G", "1TB", "-4G"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) 应报错", bad)
		}
	}
}

func TestTmpSizeArg(t *testing.T) {
	if got := TmpSizeArg(0); got != "" {
		t.Errorf(`TmpSizeArg(0) = %q, want ""`, got)
	}
	if got := TmpSizeArg(4 << 30); got != "--tmpfs=/tmp:size=4G" {
		t.Errorf("TmpSizeArg(4G) = %q", got)
	}
	if got := TmpSizeArg(512 << 20); got != "--tmpfs=/tmp:size=512M" {
		t.Errorf("TmpSizeArg(512M) = %q", got)
	}
	if got := TmpSizeArg(3<<20 + 5); got != "--tmpfs=/tmp:size=3145733" {
		t.Errorf("TmpSizeArg(非整除字节数) = %q", got)
	}
}

func TestLoadRepositories(t *testing.T) {
	p := writeTmp(t, `
base_dir = "/srv/subs"

[[repositories]]
name = "multilib"
servers = ["https://mirrors.ustc.edu.cn/archlinux/$repo/os/$arch"]

[[repositories]]
name = "archlinuxcn"
servers = [
  "https://mirrors.ustc.edu.cn/archlinuxcn/$arch",
  "https://mirrors.aliyun.com/archlinuxcn/$arch",
]

[tool]
packages = ["vim"]
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Repositories) != 2 {
		t.Fatalf("repos = %d, want 2", len(cfg.Repositories))
	}
	m := cfg.Repositories[0]
	if m.Name != "multilib" || len(m.Servers) != 1 {
		t.Errorf("repo0 = %+v", m)
	}
	cn := cfg.Repositories[1]
	if cn.Name != "archlinuxcn" || len(cn.Servers) != 2 ||
		cn.Servers[0] == "" || cn.Servers[1] == "" {
		t.Errorf("servers 必须保序且非空: %+v", cn)
	}
}

func TestLoadRejectsBadRepository(t *testing.T) {
	cases := []struct{ name, toml string }{
		{"缺 name", `base_dir="/s"
[[repositories]]
servers = ["https://x"]`},
		{"空 servers", `base_dir="/s"
[[repositories]]
name = "cn"
servers = []`},
		{"未知键", `base_dir="/s"
[[repositories]]
name = "cn"
servers = ["https://x"]
wat = 1`},
	}
	for _, c := range cases {
		if _, err := Load(writeTmp(t, c.toml)); err == nil {
			t.Errorf("%s: 应拒绝", c.name)
		}
	}
}

func TestLoadMissingBaseDir(t *testing.T) {
	p := writeTmp(t, `
[tool]
packages = ["vim"]
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "base_dir") {
		t.Fatalf("want base_dir error, got %v", err)
	}
}

func TestLoadNoGroups(t *testing.T) {
	p := writeTmp(t, `base_dir = "/srv/subs"`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "软件组") {
		t.Fatalf("want 软件组 error, got %v", err)
	}
}

func TestLoadEmptyPackages(t *testing.T) {
	p := writeTmp(t, `
base_dir = "/srv/subs"

[tool]
packages = []
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "tool") {
		t.Fatalf("want empty packages error for tool, got %v", err)
	}
}

func TestLoadBadPackageName(t *testing.T) {
	p := writeTmp(t, `
base_dir = "/srv/subs"

[tool]
packages = ["vim", "  "]
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("want error for blank package name")
	}
}

func TestLoadFileNotExists(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err == nil {
		t.Fatal("want error for missing file")
	}
}

func TestLoadMinTreeGroupSize(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want int64
	}{
		{"缺省为零", `base_dir="/s"
[tool]
packages=["vim"]`, 0},
		{"纯字节数", `base_dir="/s"
min_tree_group_size = 524288
[tool]
packages=["vim"]`, 524288},
		{"G 单位", `base_dir="/s"
min_tree_group_size = "2G"
[tool]
packages=["vim"]`, 2 << 30},
		{"M 单位小写", `base_dir="/s"
min_tree_group_size = "800m"
[tool]
packages=["vim"]`, 800 << 20},
		{"K 单位", `base_dir="/s"
min_tree_group_size = "512K"
[tool]
packages=["vim"]`, 512 << 10},
	}
	for _, c := range cases {
		cfg, err := Load(writeTmp(t, c.toml))
		if err != nil {
			t.Errorf("%s: Load: %v", c.name, err)
			continue
		}
		if cfg.MinTreeGroupSize != c.want {
			t.Errorf("%s: MinTreeGroupSize = %d, want %d", c.name, cfg.MinTreeGroupSize, c.want)
		}
	}
}

func TestLoadRejectsBadMinTreeGroupSize(t *testing.T) {
	for _, v := range []string{`"2X"`, `"G"`, `"-1"`, `"-1"`, `"3.5G"`, `"1TB"`} {
		toml := `base_dir="/s"
min_tree_group_size = ` + v + `
[tool]
packages=["vim"]`
		if _, err := Load(writeTmp(t, toml)); err == nil {
			t.Errorf("值 %s 应被拒绝", v)
		}
	}
}

func TestLoadNewGroupKeys(t *testing.T) {
	p := writeTmp(t, `
base_dir = "/srv/subs"
mounts = ["/srv/data:/srv/data", "/home/lianxin/projects:/root/projects"]

[tool]
packages  = ["vim"]
services  = ["sshd.service", "docker.socket"]
export    = ["vim"]
autostart = true
mounts    = ["/elsewhere/work:/root/projects"]   # 同容器路径 → 覆写全局

[code]
packages = ["code"]
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Mounts) != 2 || cfg.Mounts[0].Host != "/srv/data" ||
		cfg.Mounts[1].Container != "/root/projects" {
		t.Fatalf("global mounts = %+v", cfg.Mounts)
	}
	tool := cfg.Groups[len(cfg.Groups)-1] // 声明序最后一个
	if tool.Name != "tool" {
		t.Fatalf("group = %s", tool.Name)
	}
	if !tool.Autostart {
		t.Error("autostart 应为 true")
	}
	if len(tool.Services) != 2 || tool.Services[1] != "docker.socket" {
		t.Errorf("services = %v", tool.Services)
	}

	// 全局 + 组级覆写：同名容器路径被顶替，其余追加
	got := tool.EffectiveMounts(cfg.Mounts)
	want := []Mount{
		{Host: "/srv/data", Container: "/srv/data"},
		{Host: "/elsewhere/work", Container: "/root/projects"}, // 覆写全局
	}
	if len(got) != len(want) || got != nil && (got[0] != want[0] || got[1] != want[1]) {
		t.Fatalf("EffectiveMounts = %+v, want %+v", got, want)
	}
	code := cfg.Groups[0]
	if cm := code.EffectiveMounts(cfg.Mounts); len(cm) != 2 {
		t.Fatalf("未声明组应原样继承全局: %+v", cm)
	}
}

func TestLoadNewKeyErrors(t *testing.T) {
	cases := []struct{ name, toml string }{
		{"挂载缺容器侧", `base_dir="/s"
[code]
packages=["c"]
	mounts = ["/only/host"]`},
		{"autostart 非布尔", `base_dir="/s"
[code]
packages=["c"]
autostart = "yes"`},
		{"users 键已移除", `base_dir="/s"
[code]
packages=["c"]
users = ["lianxin"]`},
		{"services 重复", `base_dir="/s"
[code]
packages=["c"]
services = ["a", "a"]`},
		{"export 非数组", `base_dir="/s"
[code]
packages=["c"]
export = "vim"`},
		{"未知键仍拒绝", `base_dir="/s"
[code]
packages=["c"]
mode = "resident"`},
	}
	for _, c := range cases {
		if _, err := Load(writeTmp(t, c.toml)); err == nil {
			t.Errorf("%s: 应拒绝", c.name)
		}
	}
}

// TestSessionConfigDefaults 缺省键时三取值补齐默认，门禁与目标 uid 解析
// 按默认语义工作。
func TestSessionConfigDefaults(t *testing.T) {
	cfg, err := Load(writeTmp(t, "base_dir=\"/s\"\n[tool]\npackages=[\"vim\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Groups[0]
	if g.SessionMode != SessionModeNative || g.SessionUser != "caller" || g.SessionAccess != "all" {
		t.Fatalf("默认值不符: %+v", g)
	}
	if uid := g.SessionUID(4242); uid != 4242 {
		t.Fatalf("caller 应复用调用者 uid: %d", uid)
	}
	if !g.SessionAllowed(1000) || !g.SessionAllowed(0) {
		t.Fatal("all 应放行任意调用者")
	}
}

// TestSessionConfigOverrides 组级覆盖会话方案；uid 接受 TOML 整数与字符串。
func TestSessionConfigOverrides(t *testing.T) {
	cfg, err := Load(writeTmp(t, `
base_dir = "/s"
[tool]
packages = ["vim"]
session_mode = "rw"
session_user = 1000
session_access = "root"

[code]
packages = ["vim"]
session_mode = "off"
session_user = "1000"
session_access = "1000"
`))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*Group{}
	for _, g := range cfg.Groups {
		byName[g.Name] = g
	}
	tool, code := byName["tool"], byName["code"]
	if tool.SessionMode != "rw" || tool.SessionUser != "1000" || tool.SessionAccess != "root" {
		t.Fatalf("tool 覆盖不符: %+v", tool)
	}
	if code.SessionMode != "off" || code.SessionUser != "1000" || code.SessionAccess != "1000" {
		t.Fatalf("code 覆盖不符: %+v", code)
	}

	isolatedCfg, err := Load(writeTmp(t, `
base_dir = "/s"
[gui]
packages = ["gtk3"]
session_mode = "isolated"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := isolatedCfg.Groups[0].SessionMode; got != SessionModeIsolated {
		t.Fatalf("isolated 模式未解析: %q", got)
	}
	nativeCfg, err := Load(writeTmp(t, `
base_dir = "/s"
[gui]
packages = ["gtk3"]
session_mode = "native"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := nativeCfg.Groups[0].SessionMode; got != SessionModeNative {
		t.Fatalf("native 模式未解析: %q", got)
	}
	if uid := tool.SessionUID(0); uid != 1000 {
		t.Fatalf("固定 uid 应覆盖 caller: %d", uid)
	}
	// access=root：普通用户拒绝、root 放行
	if tool.SessionAllowed(1000) {
		t.Fatal("root 门禁应拒绝普通用户")
	}
	if !tool.SessionAllowed(0) {
		t.Fatal("root 门禁应放行 root")
	}
	// access=特定 uid：该 uid 与 root 放行，他人拒绝
	if !code.SessionAllowed(1000) || !code.SessionAllowed(0) || code.SessionAllowed(1001) {
		t.Fatal("特定 uid 门禁不符")
	}
}

// TestSessionConfigInvalid 非法取值一律在 Load 阶段拒绝。
func TestSessionConfigInvalid(t *testing.T) {
	cases := []struct{ name, toml string }{
		{"未知方案", `base_dir="/s"
[tool]
packages=["v"]
session_mode = "overlay"`},
		{"未知用户名", `base_dir="/s"
[tool]
packages=["v"]
session_user = "nobody"`},
		{"负 uid", `base_dir="/s"
[tool]
packages=["v"]
session_access = -1`},
		{"非字符串模式", `base_dir="/s"
[tool]
packages=["v"]
session_mode = 3`},
	}
	for _, c := range cases {
		if _, err := Load(writeTmp(t, c.toml)); err == nil {
			t.Errorf("%s: 应拒绝", c.name)
		}
	}
}

// TestLoadRelativeSourcesResolve 挂载与复制两表的来源（条目第一段）为
// 相对路径时按配置文件所在目录解析为绝对路径；绝对来源原样保留。
// 目标（第二段）必须绝对，这一约束在 TestLoadRejectsRelativeTargets。
func TestLoadRelativeSourcesResolve(t *testing.T) {
	p := writeTmp(t, `
base_dir = "/s"
mounts = ["tmux.conf:/mnt/tmux", "configs:/mnt/configs", "/etc/hosts:/mnt/hosts", "rodir:/mnt/ro:ro"]

[tool]
packages = ["vim"]
copies = ["tmux.conf:/etc/tmux.conf", "configs:/srv/configs", "/etc/hosts:/etc/hosts"]
`)
	dir := filepath.Dir(p)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	join := func(rel string) string { return filepath.Join(dir, rel) }
	ms := cfg.Mounts
	if len(ms) != 4 {
		t.Fatalf("mounts = %+v", ms)
	}
	if ms[0].Host != join("tmux.conf") || ms[0].Container != "/mnt/tmux" || ms[0].ReadOnly {
		t.Errorf("mounts[0] = %+v", ms[0])
	}
	if ms[1].Host != join("configs") || ms[1].Container != "/mnt/configs" {
		t.Errorf("mounts[1] = %+v", ms[1])
	}
	if ms[2].Host != "/etc/hosts" || ms[2].Container != "/mnt/hosts" || ms[2].ReadOnly {
		t.Errorf("mounts[2] 绝对来源应原样保留: %+v", ms[2])
	}
	if ms[3].Host != join("rodir") || !ms[3].ReadOnly {
		t.Errorf("mounts[3] ro 声明不符: %+v", ms[3])
	}

	if len(cfg.Groups) != 1 || cfg.Groups[0].Name != "tool" {
		t.Fatalf("groups = %+v", cfg.Groups)
	}
	cs := cfg.Groups[0].Copies
	if len(cs) != 3 {
		t.Fatalf("copies = %+v", cs)
	}
	if cs[0].Source != join("tmux.conf") || cs[0].Dest != "/etc/tmux.conf" {
		t.Errorf("copies[0] = %+v", cs[0])
	}
	if cs[1].Source != join("configs") || cs[1].Dest != "/srv/configs" {
		t.Errorf("copies[1] = %+v", cs[1])
	}
	if cs[2].Source != "/etc/hosts" || cs[2].Dest != "/etc/hosts" {
		t.Errorf("copies[2] = %+v", cs[2])
	}
}

// TestLoadRejectsRelativeTargets 两表目标（条目第二段）必须是绝对路径：
// 挂载的容器侧与复制的子系统内侧一律不做相对解析，加载即拒绝。
func TestLoadRejectsRelativeTargets(t *testing.T) {
	cases := []struct{ name, toml string }{
		{"挂载容器路径相对", `base_dir="/s"
[code]
packages=["c"]
mounts = ["/h:rel"]`},
		{"复制目标相对", `base_dir="/s"
[code]
packages=["c"]
copies = ["/h:rel"]`},
		{"复制目标为根", `base_dir="/s"
[code]
packages=["c"]
copies = ["/h:/"]`},
		{"复制单段", `base_dir="/s"
[code]
packages=["c"]
copies = ["/only/source"]`},
		{"复制条目重复", `base_dir="/s"
[code]
packages=["c"]
copies = ["/a:/d", "/a:/d"]`},
		{"挂载第三段非法", `base_dir="/s"
[code]
packages=["c"]
mounts = ["/h:/c:rw"]`},
		{"copies 非数组", `base_dir="/s"
[code]
packages=["c"]
copies = "/etc/x"`},
	}
	for _, c := range cases {
		if _, err := Load(writeTmp(t, c.toml)); err == nil {
			t.Errorf("%s: 应拒绝", c.name)
		}
	}
}
