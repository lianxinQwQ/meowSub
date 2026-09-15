// Package daemon 实现特权助手 meowsubd：所有需要 root 的容器操作经它执行，
// 用户侧命令只与本进程对话，不再逐次 sudo。
//
// 通信：unix socket（默认 /run/meowsubd.sock）上行 JSON、下行 JSON，
// 每连接恰好一个请求。例外是 shell 交互请求（Interactive）：应答一行握手
// JSON 后，连接退化为纯终端字节流，双向桥接至容器内会话结束。
// 身份用内核 SO_PEERCRED 取对端 uid，实例内进程镜像该身份。
//
// 实例生命周期三出身（决定占用归零时是否回收）：
//
//	autostart  配置声明开机自启——随守护进程启动引导，永不自动停
//	explicit   meowsub start 显式拉起——常驻到显式 stop
//	demand     入口/shell 首次触达隐式创建——占用表清零即优雅停机
package daemon

import "fmt"

// Op 请求操作类型。
type Op string

const (
	OpPing        Op = "ping"
	OpStart       Op = "start"        // 显式拉起（或复用已活）
	OpStop        Op = "stop"         // 显式停止
	OpPs          Op = "ps"           // 全部实例概览
	OpRunning     Op = "running"      // 单实例运行状态查询（build 收尾同步用）
	OpShell       Op = "shell"        // 进入实例 shell：交互桥接或非交互执行 Cmd
	OpLaunchEntry Op = "launch-entry" // 在实例内跑一条命令（GUI 入口路径）
	OpRunDesktop  Op = "run-desktop"  // 按实例内 .desktop 启动应用
)

// InstanceOrigin 实例出身。
type InstanceOrigin string

const (
	OriginAutostart InstanceOrigin = "autostart"
	OriginExplicit  InstanceOrigin = "explicit"
	OriginDemand    InstanceOrigin = "demand"
)

// Request 是客户端发送的单一请求体。
// CallerUID 由连接层以 SO_PEERCRED 填充（不经网络），用于把实例内进程
// 映射为调用者身份。
type Request struct {
	Op        Op       `json:"op"`
	CallerUID int      `json:"-"`                   // 由连接层以 SO_PEERCRED 填充，不经网络
	Group     string   `json:"group,omitempty"`     // 目标软件组
	Name      string   `json:"name,omitempty"`      // 目标实例名；空则回退组名
	Cmd       string   `json:"cmd,omitempty"`       // launch/shell 的容器内命令
	Args      []string `json:"args,omitempty"`      // 命令参数透传
	Graphical bool     `json:"graphical,omitempty"` // 注入图形直通预设

	// Wait 仅对 launch/run-desktop 生效：前台阻塞运行——响应随应用退出
	// 返回，客户端断开（Ctrl-C/关终端）即被守护进程感知并连锁终结实例内
	// 应用。缺省提交即忘，供宿主桌面启动器（.desktop）等不等待的场景。
	Wait bool `json:"wait,omitempty"`

	// 会话环境透传：CLI 从自身环境取固定四项（XDG_RUNTIME_DIR/
	// WAYLAND_DISPLAY/DISPLAY/DBUS_SESSION_BUS_ADDRESS，见 ForwardEnv）。
	// 图形会话的事实只来自调用者——daemon 是 root 系统服务，自身环境里
	// 没有这些值，因此只信请求、不读自身环境；缺席即缺席，不猜默认值。
	Env map[string]string `json:"env,omitempty"`

	// 以下仅 shell 交互桥接使用：握手后连接转字节流，daemon 据此在
	// 容器内重建调用者的终端观感。
	Interactive bool   `json:"interactive,omitempty"` // 请求桥接而非单次执行
	Term        string `json:"term,omitempty"`        // 客户端终端类型（$TERM）
	Rows        int    `json:"rows,omitempty"`        // 客户端初始窗口行数
	Cols        int    `json:"cols,omitempty"`        // 客户端初始窗口列数
}

// Response 统一响应；Data 的具体形状随 Op 而定（Ps 返回 []InstanceRow 等）。
type Response struct {
	OK    bool        `json:"ok"`
	Error string      `json:"error,omitempty"`
	Data  interface{} `json:"data,omitempty"`
}

// InstanceRow 单实例状态行。
type InstanceRow struct {
	Name    string         `json:"name"`
	Group   string         `json:"group"`
	Running bool           `json:"running"`
	Origin  InstanceOrigin `json:"origin,omitempty"`
	Holders int            `json:"holders,omitempty"`
}

func errf(format string, a ...any) Response {
	return Response{OK: false, Error: fmt.Sprintf(format, a...)}
}
