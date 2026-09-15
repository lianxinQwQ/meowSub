// Package tomlpatch 提供保注释的 TOML 定向修改。
//
// 不做完整的 TOML 反序列化——那会把注释、空行与键顺序全部抹平；
// 这里只做行级手术：定位目标表头，替换既有键的整行，或在表内追加
// 新键，文本其余部分一字不动。适用于机器维护少量声明键的场景
// （meowsub 的 service enable/disable、autostart 开关等意图回写）。
package tomlpatch

import (
	"fmt"
	"sort"
	"strings"
)

// SetGroupKeys 在 src 中定位 [group] 表，将 updates 各键置为给定值，
// 返回修补后的完整 TOML 文本（恒以单个换行结尾）。
//
// 值支持三种类型：bool 渲染为 true/false；[]string 渲染为内联数组；
// string 渲染为带引号字符串（Go 转义）。组不存在时在文末追加新表；
// 键已存在（未被注释）则整行替换为新渲染；键不存在则在表内末尾插入。
func SetGroupKeys(src []byte, group string, updates map[string]interface{}) ([]byte, error) {
	if strings.ContainsAny(group, "[]\n") || group == "" {
		return nil, fmt.Errorf("非法表名 %q", group)
	}
	lines := strings.Split(strings.TrimRight(string(src), "\n"), "\n")

	rendered := make(map[string]string, len(updates))
	for k, v := range updates {
		r, err := render(k, v)
		if err != nil {
			return nil, err
		}
		rendered[k] = r
	}

	start, end := findSection(lines, group)
	switch {
	case start < 0: // 无此表：文末追加
		lines = append(lines, "", "["+group+"]")
		for _, r := range rendered {
			lines = append(lines, r)
		}
	default: // 有此表：先原位替换命中的键，未命中的统一插到表尾
		for i := start + 1; i < end; i++ {
			k, isKV := splitKey(lines[i])
			if isKV {
				if r, ok := rendered[k]; ok {
					lines[i] = r
					delete(rendered, k)
				}
			}
		}
		pending := sortedValues(rendered)
		if len(pending) > 0 {
			rest := append([]string{}, lines[end:]...)
			lines = append(lines[:end:end], append(pending, rest...)...)
		}
	}

	out := strings.Join(lines, "\n")
	return []byte(out + "\n"), nil
}

// findSection 返回 [group] 表头行下标与表体结束下标（下一个顶级表头或
// 文件尾）；无该表时返回 (-1, len(lines))。允许表头携带行内注释。
func findSection(lines []string, group string) (int, int) {
	for i, ln := range lines {
		if name, ok := headerName(ln); ok && name == group {
			for j := i + 1; j < len(lines); j++ {
				if _, isHdr := headerName(lines[j]); isHdr {
					return i, j
				}
			}
			return i, len(lines)
		}
	}
	return -1, len(lines)
}

// headerName 判断一行是否为顶级表头（[name] 或 [[array]] 视为非目标），
// 返回去掉行内注释与引号后的表名。
func headerName(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "[") || strings.HasPrefix(t, "#") {
		return "", false
	}
	if i := strings.Index(t, "#"); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	if !strings.HasSuffix(t, "]") {
		return "", false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(t, "["), "]")
	if strings.HasPrefix(inner, "[") { // [[array-of-tables]] 不匹配单表
		return "", false
	}
	name := strings.Trim(inner, " \t\"")
	return name, name != ""
}

// splitKey 从未被注释的 "key = value" 行取键名；其余行返回 ("",false)。
func splitKey(trimmed string) (string, bool) {
	if trimmed == "" || strings.HasPrefix(trimmed, "#") ||
		strings.HasPrefix(trimmed, "[") {
		return "", false
	}
	eq := strings.Index(trimmed, "=")
	if eq <= 0 {
		return "", false
	}
	k := strings.TrimSpace(trimmed[:eq])
	valid := k != ""
	for _, c := range k {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '_' || c == '-'
		if !ok {
			return "", false
		}
	}
	return k, valid
}

// sortedValues 以键名字典序输出剩余键值渲染行，保证追加顺序稳定。
func sortedValues(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

func render(key string, v interface{}) (string, error) {
	switch t := v.(type) {
	case bool:
		return fmt.Sprintf("%s = %v", key, t), nil
	case string:
		return fmt.Sprintf("%s = %q", key, t), nil
	case []string:
		parts := make([]string, len(t))
		for i, s := range t {
			parts[i] = fmt.Sprintf("%q", s)
		}
		return key + " = [" + strings.Join(parts, ", ") + "]", nil
	default:
		return "", fmt.Errorf("不支持写入的值类型 %T（键 %s）", v, key)
	}
}
