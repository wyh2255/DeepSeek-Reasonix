// style.go 提供终端文本样式化的基础功能，包括颜色检测、ANSI 转义序列封装
// 和常用样式函数（加粗、暗淡、绿色、红色、黄色、强调色、反色等）。
// 颜色在启动时检测一次：仅在写入真实终端且用户未通过 NO_COLOR 或 TERM=dumb 禁用时启用。
// 管道/重定向输出和 CI 环境保持纯文本，以免破坏脚本。
package cli

import (
	"os"

	"golang.org/x/term"
)

// colorEnabled 在启动时决定是否启用颜色：仅在写入真实终端且用户未通过
// NO_COLOR (https://no-color.org) 或 TERM=dumb 禁用时为 true。
var colorEnabled = detectColor()

// detectColor 检测当前终端是否支持颜色输出。
func detectColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// ANSI 转义序列常量，用于终端文本样式化。
const (
	ansiReset   = "\033[0m"       // 重置所有样式
	ansiBold    = "\033[1m"       // 加粗
	ansiDim     = "\033[2m"       // 暗淡
	ansiGreen   = "\033[32m"      // 绿色
	ansiRed     = "\033[31m"      // 红色
	ansiYellow  = "\033[33m"      // 黄色
	ansiBlue    = "\033[38;5;39m" // 亮蓝色（256色）
	ansiCyan    = "\033[38;5;44m" // 青色（256色）
	ansiMagenta = "\033[38;5;176m" // 品红色（256色）
	ansiReverse = "\033[7m"       // 反色
	// ansiAccent 是暗色主题下 Reasonix 品牌铜色的回退值。
	// accent() 使用当前活跃的 CLI 主题，但测试和旧代码仍可引用此具体转义序列。
	ansiAccent = "\033[38;5;173m" // 铜色（256色）
)

// sgr 用指定的 SGR 转义码包裹字符串，颜色禁用时原样返回。
func sgr(code, s string) string {
	if !colorEnabled {
		return s
	}
	return code + s + ansiReset
}

// 以下为常用的文本样式函数，使用当前活跃 CLI 主题的颜色。
func bold(s string) string    { return sgr(ansiBold, s) }                        // 加粗
func dim(s string) string     { return themeFg(activeCLITheme.faint, s) }         // 暗淡（主题 faint 色）
func green(s string) string   { return themeFg(activeCLITheme.success, s) }       // 绿色（主题成功色）
func red(s string) string     { return themeFg(activeCLITheme.err, s) }           // 红色（主题错误色）
func yellow(s string) string  { return themeFg(activeCLITheme.warn, s) }          // 黄色（主题警告色）
func accent(s string) string  { return themeFg(activeCLITheme.accent, s) }        // 强调色（主题强调色）
func reverse(s string) string { return sgr(ansiReverse, s) }                      // 反色
