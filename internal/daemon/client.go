// client.go 用户侧与 meowsubd 对话的最小客户端。
package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"
)

// ForwardEnv 从当前进程环境提取需透传给 daemon 的会话变量（固定四项，
// 不整包转发，避免把调用者全量环境漏进守护进程）。图形直通的“事实”
// 只来自调用者会话：请求里有什么 daemon 注什么，缺席即缺席。
func ForwardEnv() map[string]string {
	env := map[string]string{}
	for _, k := range []string{"XDG_RUNTIME_DIR", "WAYLAND_DISPLAY",
		"DISPLAY", "DBUS_SESSION_BUS_ADDRESS"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return env
}

// Call 打开到 socket 的连接，发送单请求并取回响应。
func Dial(sock string, req Request, timeout time.Duration) (Response, error) {
	conn, err := net.DialTimeout("unix", sock, timeout)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	if timeout > 0 { // timeout<=0：不设整轮 deadline，供前台阻塞的 Wait 请求
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, err
	}
	resp, rerr := bufio.NewReader(conn).ReadString('\n')
	if rerr != nil && resp == "" {
		return Response{}, rerr
	}
	var out Response
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		return Response{}, err
	}
	if !out.OK {
		return out, fmt.Errorf("%s", out.Error)
	}
	return out, nil
}

// ErrNoDaemon 允许调用方判断“服务未安装/未启动”而降级。
type ErrNoDaemon struct{ inner error }

func (e *ErrNoDaemon) Error() string { return "meowsubd 未运行：" + e.inner.Error() }

// noDaemon 包装连接类失败（socket 不存在/不可达）。
func noDaemon(err error) error {
	switch err.(type) {
	case *net.OpError, *os.PathError:
		return &ErrNoDaemon{inner: err}
	}
	return err
}

// TryCall 统一封装：默认 15s 整轮往返超时。
func TryCall(req Request) (Response, error) {
	return CallTimeout(req, 15*time.Second)
}

// CallTimeout 发送单请求并取回响应；timeout 覆盖从拨号到读完响应的整轮。
// 同步执行的 shell -c 可能远超 15s，需放宽。
func CallTimeout(req Request, timeout time.Duration) (Response, error) {
	resp, err := Dial(SocketPath, req, timeout)
	if err != nil {
		return resp, noDaemon(err)
	}
	return resp, err
}

// OpenInteractive 发起 shell 交互桥接：发出请求并读完一行握手 JSON 后，
// 连接即退化为与 meowsubd 之间的纯终端字节流，调用方负责双向搬运与最终
// Close。守护进程可能在握手前完成隐式部署与引导，握手超时放宽到 2 分钟；
// 启动前的失败（无 daemon/部署/引导）以错误返回，含提示文案。
func OpenInteractive(req Request) (net.Conn, Response, error) {
	conn, err := net.DialTimeout("unix", SocketPath, 15*time.Second)
	if err != nil {
		return nil, Response{}, noDaemon(err)
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		conn.Close()
		return nil, Response{}, noDaemon(err)
	}
	resp, rerr := bufio.NewReader(conn).ReadString('\n')
	if rerr != nil && resp == "" {
		conn.Close()
		return nil, Response{}, rerr
	}
	var out Response
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		conn.Close()
		return nil, Response{}, err
	}
	if !out.OK {
		conn.Close()
		return nil, out, fmt.Errorf("%s", out.Error)
	}
	_ = conn.SetDeadline(time.Time{}) // 桥接期不限时，会话结束以 EOF 为准
	return conn, out, nil
}
