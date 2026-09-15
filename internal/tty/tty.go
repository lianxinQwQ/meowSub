// Package tty 终端控制的最小封装：客户端与守护进程桥接容器内 pty 前，
// 把本地终端切 raw（字节直通、信号远端化），会话结束恢复。只用 stdlib
// syscall，不引第三方依赖。
package tty

import (
	"syscall"
	"unsafe"
)

// State 记录终端原始属性，供 Restore 恢复。
type State struct {
	termios syscall.Termios
}

func ioctl(fd, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// IsTerminal 判断 fd 是否为终端。
func IsTerminal(fd uintptr) bool {
	var st State
	return ioctl(fd, syscall.TCGETS, unsafe.Pointer(&st.termios)) == nil
}

// MakeRaw 将终端置 raw 模式（关行缓冲、回显与信号生成），返回原状态；
// 此后 Ctrl-C 等作为字节穿透到对端 pty，由远端行规程转成信号。
func MakeRaw(fd uintptr) (*State, error) {
	var st State
	if err := ioctl(fd, syscall.TCGETS, unsafe.Pointer(&st.termios)); err != nil {
		return nil, err
	}
	raw := st.termios
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK |
		syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON |
		syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := ioctl(fd, syscall.TCSETS, unsafe.Pointer(&raw)); err != nil {
		return nil, err
	}
	return &st, nil
}

// Restore 恢复 MakeRaw 之前的终端属性。
func Restore(fd uintptr, st *State) error {
	if st == nil {
		return nil
	}
	return ioctl(fd, syscall.TCSETS, unsafe.Pointer(&st.termios))
}

type winsize struct{ Row, Col, X, Y uint16 }

// Winsize 取终端当前窗口尺寸（行、列）。
func Winsize(fd uintptr) (rows, cols int, err error) {
	var ws winsize
	if err = ioctl(fd, syscall.TIOCGWINSZ, unsafe.Pointer(&ws)); err != nil {
		return 0, 0, err
	}
	return int(ws.Row), int(ws.Col), nil
}
