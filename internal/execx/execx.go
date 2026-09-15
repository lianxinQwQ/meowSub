// Package execx 封装外部命令执行：真实执行与记录式执行（供测试断言）。
package execx

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Runner 是命令执行抽象。
type Runner interface {
	// Run 执行命令并透传输出到当前进程的 stdout/stderr。
	Run(name string, args ...string) error
	// RunOutput 执行命令并捕获 stdout 返回。
	RunOutput(name string, args ...string) (string, error)
}

// cEnv 强制 C locale，保证 pacman/paru 输出字段名（Name、Depends On 等）
// 可被稳定解析，不受宿主机语言影响。
func cEnv() []string {
	return append(os.Environ(), "LC_ALL=C")
}

// ExecRunner 真实执行器。
type ExecRunner struct{}

// Run 实现 Runner。
func (ExecRunner) Run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = cEnv()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("执行 %s: %w", display(name, args), err)
	}
	return nil
}

// RunOutput 实现 Runner。注意：命令以非零码退出时仍返回已捕获的 stdout，
// 便于解析 pacman/paru 这类“部分成功”的批量查询。
func (ExecRunner) RunOutput(name string, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	cmd.Env = cEnv()
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("执行 %s: %w", display(name, args), err)
	}
	return out.String(), nil
}

func display(name string, args []string) string {
	return strings.TrimSpace(name + " " + strings.Join(args, " "))
}

// RecordedCmd 记录一条已“执行”的命令。
type RecordedCmd struct {
	Name string
	Args []string
}

// OutputEntry 一次 RunOutput 的脚本化结果。
type OutputEntry struct {
	Out string
	Err error
}

// RecordRunner 测试用执行器：记录调用并可脚本化返回值与失败。
type RecordRunner struct {
	Cmds        []*RecordedCmd
	OutputQueue []OutputEntry          // 依序弹出
	OutputFor   map[string]OutputEntry // 按 “name arg1 arg2” 整串匹配，优先于队列
	FailOn      map[string]error       // 按命令名匹配
}

// Run 实现 Runner。
func (r *RecordRunner) Run(name string, args ...string) error {
	r.Cmds = append(r.Cmds, &RecordedCmd{Name: name, Args: append([]string{}, args...)})
	if r.FailOn != nil {
		if err, ok := r.FailOn[name]; ok {
			return err
		}
	}
	return nil
}

// RunOutput 实现 Runner：先按完整命令串查 OutputFor，再依序弹出队列，耗尽后返回空串。
func (r *RecordRunner) RunOutput(name string, args ...string) (string, error) {
	r.Cmds = append(r.Cmds, &RecordedCmd{Name: name, Args: append([]string{}, args...)})
	if r.OutputFor != nil {
		if e, ok := r.OutputFor[display(name, args)]; ok {
			return e.Out, e.Err
		}
	}
	if len(r.OutputQueue) > 0 {
		e := r.OutputQueue[0]
		r.OutputQueue = r.OutputQueue[1:]
		return e.Out, e.Err
	}
	return "", nil
}
