//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

// theme_osc_other.go 为非 Unix 平台（如 Windows）提供终端背景色查询的存根实现。
// 这些平台不支持通过 OSC 11 协议查询终端背景色，因此直接返回失败，
// 让调用方回退到 COLORFGBG 环境变量等其他检测方式。
package cli

// queryTerminalBackground 在非 Unix 平台上始终返回 false，表示无法通过
// OSC 11 协议查询终端背景色。
func queryTerminalBackground() (terminalRGB, bool) {
	return terminalRGB{}, false
}
