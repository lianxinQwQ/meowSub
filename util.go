// util.go：main 包内的纯工具函数（无子命令语义，供各 cmd_*.go 复用）。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"meowsub/internal/config"
	"meowsub/internal/planner"
)

// fsArg 取位置参数（跳过 -f 与其值后的第一个裸参数）。
func fsArg(args []string) string {
	skipNext := false
	for _, a := range args[1:] {
		if skipNext {
			skipNext = false
			continue
		}
		if strings.HasPrefix(a, "-") {
			if a == "-f" {
				skipNext = true
			}
			continue
		}
		return a
	}
	return ""
}

// groupIndex 组名 -> 组声明的索引（烘焙层 O(1) 查 users/services/export）。
func groupIndex(gs []*config.Group) map[string]*config.Group {
	m := make(map[string]*config.Group, len(gs))
	for _, g := range gs {
		m[g.Name] = g
	}
	return m
}

// selfPath 当前可执行文件的绝对路径，供生成 systemd 单元与宿主导出物
// （.desktop Exec/TryExec、bin shim）时引用自身；失败即报错，绝不落入
// 固定路径或空路径。
func selfPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	// 进程启动后自身二进制被原子替换时，readlink /proc/self/exe 的结果
	// 会带内核附加的该标记；剥掉后恰是替换后新文件的位置。
	return strings.TrimSuffix(p, " (deleted)"), nil
}

// countAur 统计闭包内 AUR 包个数（日志展示用）。
func countAur(c *planner.Closure) int {
	n := 0
	for _, p := range c.Pkgs {
		if c.Aur[p] {
			n++
		}
	}
	return n
}

// hashKey 包列表的缓存键：序列化后取 SHA-256（相同包列表只解析一次闭包）。
func hashKey(pkgs []string) string {
	h := sha256.Sum256([]byte(fmt.Sprint(pkgs)))
	return hex.EncodeToString(h[:])
}

// fileHash 整个文件的内容指纹（配置哈希，写入 state.json）。
func fileHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), nil
}

// toRepos 把配置仓库指针切片展平为值切片（builder 只关心 Name/Servers）。
func toRepos(rs []*config.Repository) []config.Repository {
	out := make([]config.Repository, 0, len(rs))
	for _, r := range rs {
		out = append(out, config.Repository{Name: r.Name, Servers: r.Servers})
	}
	return out
}
