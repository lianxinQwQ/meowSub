package tomlpatch

import (
	"strings"
	"testing"
)

const sample = `# 顶部注释必须保留
base_dir = "/srv/subs"

[[repositories]]
name = "cn"
servers = ["https://cn"]

[tool]              # 行内注释应保留
packages = ["vim", "neovim"]
services = ["old.service"]
# services 下方这条手工备注应原样留在原位

[code]
packages = ["code"]
`

func mustPatch(t *testing.T, src []byte, group string,
	upd map[string]interface{}) string {
	t.Helper()
	out, err := SetGroupKeys(src, group, upd)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestReplaceExistingKeyInPlace(t *testing.T) {
	got := mustPatch(t, []byte(sample), "tool",
		map[string]interface{}{"services": []string{"sshd.service"}})
	for _, want := range []string{
		"# 顶部注释必须保留",
		`base_dir = "/srv/subs"`,
		"[[repositories]]",
		`servers = ["https://cn"]`,
		"[tool]              # 行内注释应保留",
		`packages = ["vim", "neovim"]`, // 前一个键不受影响
		`services = ["sshd.service"]`,  // 目标键整行替换
		"# services 下方这条手工备注应原样留在原位",
		"[code]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺少 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"old.service"`) {
		t.Fatalf("旧值未被替换:\n%s", got)
	}
}

func TestAppendNewKeyInsideExistingGroup(t *testing.T) {
	got := mustPatch(t, []byte("[tool]\npackages = [\"vim\"]\n"), "tool",
		map[string]interface{}{"autostart": true})
	if !strings.Contains(got, `packages = ["vim"]`) || !strings.Contains(got, "autostart = true") {
		t.Fatalf("结果异常:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("文件应以换行结尾: %q", got[len(got)-3:])
	}
}

func TestCreateMissingGroupAppends(t *testing.T) {
	got := mustPatch(t, []byte(sample), "dev", map[string]interface{}{
		"packages":  []string{"rust"},
		"autostart": false,
	})
	for _, want := range []string{"[dev]", `packages = ["rust"]`, "autostart = false"} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺少 %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "[tool]              # 行内注释应保留") {
		t.Fatal("既有内容不应被触碰")
	}
}

func TestIdempotentStableOutput(t *testing.T) {
	in := []byte(sample)
	upd := map[string]interface{}{"autostart": true, "services": []string{"a", "b"}}
	one := mustPatch(t, in, "tool", upd)
	two := mustPatch(t, []byte(one), "tool", upd)
	if one != two {
		t.Fatalf("重复修补应幂等稳定:\n--- first ---\n%s\n--- second ---\n%s", one, two)
	}
}

func TestStringEscapingAndInvalidName(t *testing.T) {
	got := mustPatch(t, []byte("[a]\n"), "a", map[string]interface{}{
		"path": `/tmp/x"y`,
	})
	if !strings.Contains(got, `path = "/tmp/x\"y"`) {
		t.Fatalf("转义缺失: %s", got)
	}
	if _, err := SetGroupKeys([]byte(""), "x[y]", nil); err == nil {
		t.Fatal("应拒绝非法表名")
	}
}
