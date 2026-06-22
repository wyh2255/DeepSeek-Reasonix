//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

// theme_osc_unix.go 实现了 Unix 平台上的终端背景色查询。
// 通过 OSC 11 终端控制序列向终端发送背景色查询请求，
// 读取终端的响应并解析出 RGB 值，用于主题的 "auto" 模式自动检测。
package cli

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const (
	// terminalBGQueryTimeout 是等待终端响应 OSC 11 查询的超时时间。
	// 80ms 足够覆盖本地终端；过长会感觉卡顿。
	terminalBGQueryTimeout  = 80 * time.Millisecond
	// terminalBGQueryMaxBytes 是读取终端响应的最大字节数，防止异常终端发送大量数据。
	terminalBGQueryMaxBytes = 256
)

// queryTerminalBackground 通过 OSC 11 协议查询当前终端的背景色。
// 过程：将 stdin 设为原始模式 -> 发送 OSC 11 查询序列 -> 非阻塞读取响应 -> 解析 RGB。
// 若颜色未启用、非终端环境或超时，均返回 false。
func queryTerminalBackground() (terminalRGB, bool) {
	if !colorEnabled {
		return terminalRGB{}, false
	}
	inFd := int(os.Stdin.Fd())
	outFd := int(os.Stdout.Fd())
	if !term.IsTerminal(inFd) || !term.IsTerminal(outFd) {
		return terminalRGB{}, false
	}

	oldState, err := term.MakeRaw(inFd)
	if err != nil {
		return terminalRGB{}, false
	}
	defer term.Restore(inFd, oldState)

	flags, err := unix.FcntlInt(uintptr(inFd), unix.F_GETFL, 0)
	if err != nil {
		return terminalRGB{}, false
	}
	if err := unix.SetNonblock(inFd, true); err != nil {
		return terminalRGB{}, false
	}
	defer func() { _, _ = unix.FcntlInt(uintptr(inFd), unix.F_SETFL, flags) }()

	// 发送 OSC 11 查询序列：ESC ] 11 ; ? BEL
	if _, err := os.Stdout.Write([]byte("\x1b]11;?\x07")); err != nil {
		return terminalRGB{}, false
	}

	// 非阻塞循环读取终端响应，直到超时或解析成功
	deadline := time.Now().Add(terminalBGQueryTimeout)
	buf := make([]byte, 64)
	var response []byte
	for time.Now().Before(deadline) && len(response) < terminalBGQueryMaxBytes {
		n, err := unix.Read(inFd, buf)
		if n > 0 {
			response = append(response, buf[:n]...)
			// 每次读取后尝试解析，成功则立即返回
			if rgb, ok := parseOSC11Response(string(response)); ok {
				return rgb, true
			}
			continue
		}
		// 无数据或被信号中断，继续等待
		if err == nil || errors.Is(err, unix.EINTR) {
			continue
		}
		// 非阻塞模式下无数据可读，短暂休眠后重试
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		return terminalRGB{}, false
	}
	return parseOSC11Response(string(response))
}
