// box.go 提供终端文本宽度计算和圆角边框绘制的工具函数。
// 使用 charmbracelet/x/ansi 库处理 ANSI 转义序列和 CJK 宽字符。
// 主要用于 CLI 界面中绘制美化的内容边框（如问题卡片、提示框等）。

package cli

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// visibleWidth returns the printable column width of s: ANSI SGR codes are
// ignored and wide / grapheme-cluster characters (CJK, emoji ZWJ sequences,
// keycaps, flags) are each counted as the cells they occupy. Thin wrapper over
// x/ansi (already in the dep tree via bubbletea/lipgloss) so call sites read
// intent rather than re-deriving the strip-and-measure dance.
func visibleWidth(s string) int {
	return ansi.StringWidth(s)
}

// padRight returns s padded with spaces on the right until it occupies w
// terminal columns (visible width, not bytes). Strings already at or beyond
// width are returned unchanged. Use this instead of fmt's %-Ns when content
// may contain CJK or ANSI SGR codes.
func padRight(s string, w int) string {
	pad := w - visibleWidth(s)
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(" ", pad)
}

// boxed wraps content in a rounded box drawn with the brand accent. Width
// auto-fits the longest line plus one column of padding on each side. The
// result always ends with a trailing newline so callers can Print it directly.
// boxed 将多行文本包裹在圆角边框中，使用品牌强调色绘制边框。
// 宽度自动适配最长行加上两侧各一个字符的内边距。
// 返回的字符串末尾始终带有换行符，调用方可直接打印。
// 使用 Unicode 制表符（╭╮╰╯─│）绘制圆角边框。
func boxed(lines []string) string {
	inner := 0
	for _, l := range lines {
		if w := visibleWidth(l); w > inner {
			inner = w
		}
	}
	inner += 2 // one space of padding on each side
	bar := strings.Repeat("─", inner)

	var b strings.Builder
	b.WriteString(accent("╭" + bar + "╮"))
	b.WriteByte('\n')
	for _, l := range lines {
		gap := inner - visibleWidth(l) - 2
		if gap < 0 {
			gap = 0
		}
		b.WriteString(accent("│"))
		b.WriteByte(' ')
		b.WriteString(l)
		b.WriteString(strings.Repeat(" ", gap))
		b.WriteByte(' ')
		b.WriteString(accent("│"))
		b.WriteByte('\n')
	}
	b.WriteString(accent("╰" + bar + "╯"))
	b.WriteByte('\n')
	return b.String()
}
