// instancelock.go：实例同步锁。build 收尾自动同步在目录交换期间禁止任何
// 方式拉起/打开实例（shell/open/start 都经 daemon 的 boot 或 root 直连
// 路径，两处都查此锁）。锁文件记录持锁进程 pid：进程已死即视为陈锁自动
// 失效，build 崩溃不会永久堵死实例。
package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func lockPath(baseDir, run string) string {
	return filepath.Join(baseDir, "runs", ".lock-"+run)
}

// LockInstance 对实例加同步锁，返回幂等的解锁函数。已有存活锁时拒绝
// （返回持锁 pid），陈锁（持锁进程已死）直接接管。
func LockInstance(baseDir, run string) (func(), error) {
	p := lockPath(baseDir, run)
	if pid, ok := readLockPid(p); ok {
		if processAlive(pid) {
			return nil, fmt.Errorf("实例 %s 正被 pid %d 同步锁定，稍后重试", run, pid)
		}
		fmt.Printf("[meowsubd] 清理陈旧同步锁 %s（pid %d 已不存在）\n", p, pid)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, []byte(fmt.Sprintf("pid=%d\n", os.Getpid())), 0o644); err != nil {
		return nil, err
	}
	once := false
	return func() {
		if once {
			return
		}
		once = true
		os.Remove(p)
	}, nil
}

// InstanceLocked 报告实例当前是否处于存活同步锁之下。
func InstanceLocked(baseDir, run string) bool {
	pid, ok := readLockPid(lockPath(baseDir, run))
	return ok && processAlive(pid)
}

func readLockPid(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(data))
	s = strings.TrimPrefix(s, "pid=")
	pid, err := strconv.Atoi(s)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// processAlive 以 kill 0 探活：ESRCH 视为已死，EPERM 视为存活（异属主）。
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
