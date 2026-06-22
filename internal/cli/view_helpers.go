// view_helpers.go 提供视图层的通用辅助函数。
// 包括：宽度校验、标题/副标题/元信息/提示的格式化渲染、路径和文本的智能截断、
// 文本预览（限制行数）、行宽保护、以及剩余宽度预算计算。
// 这些函数被多个视图组件共用，确保输出风格一致。
package cli

import (
	"fmt"
	"strings"
)

const defaultViewWidth = 80 // 默认视图宽度，当传入宽度无效时使用

// viewWidth 返回有效的视图宽度，无效值（<=0）时回退到默认宽度 80。
func viewWidth(width int) int {
	if width <= 0 {
		return defaultViewWidth
	}
	return width
}

// viewHeader 渲染带强调色的标题文本，支持 printf 格式化。
func viewHeader(format string, args ...any) string {
	return accent(fmt.Sprintf(format, args...))
}

// viewSubhead 渲染带缩进的暗色副标题文本。
func viewSubhead(s string) string {
	return dim("  " + s)
}

// viewMeta 渲染暗色元信息文本（如时间戳、文件大小等辅助信息）。
func viewMeta(s string) string {
	return dim(s)
}

// viewStatus 渲染带强调色的状态文本。
func viewStatus(s string) string {
	return accent(s)
}

// viewHint 渲染带缩进的暗色提示文本。
func viewHint(s string) string {
	return dim("  " + s)
}

// viewMore 渲染"还有更多"的提示行，如 "  +5 more items"。n<=0 时返回空字符串。
func viewMore(n int, noun string) string {
	if n <= 0 {
		return ""
	}
	return dim(fmt.Sprintf("  +%d more %s", n, noun))
}

// viewCompactPath 将路径压缩到指定宽度，优先保留首尾，截断中间部分（用 "..." 连接）。
func viewCompactPath(path string, width int) string {
	path = oneLineText(path)
	return compactMiddle(path, max(1, width))
}

// viewCompactText 将文本压缩到指定宽度，超出时截断尾部并添加 "..." 省略号。
func viewCompactText(s string, width int) string {
	s = oneLineText(s)
	return compactEnd(s, max(1, width))
}

// viewBodyPreview 截取文本的前 maxLines 行作为预览，返回预览文本和剩余行数。
// body 为空时返回空字符串和 0。
func viewBodyPreview(body string, maxLines int) (string, int) {
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return "", 0
	}
	lines := strings.Split(body, "\n")
	if maxLines <= 0 || len(lines) <= maxLines {
		return body, 0
	}
	return strings.Join(lines[:maxLines], "\n"), len(lines) - maxLines
}

// viewProtectLines 确保多行文本的每一行都不超过指定宽度，超出部分截断。
func viewProtectLines(s string, width int) string {
	width = viewWidth(width)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = compactEnd(line, width)
	}
	return strings.Join(lines, "\n")
}

// viewPadWidth 返回文本实际可见宽度与最小宽度中的较大值，用于确保对齐。
func viewPadWidth(s string, minWidth int) int {
	if w := visibleWidth(s); w > minWidth {
		return w
	}
	return minWidth
}

// viewBudget 计算剩余可用宽度：总宽度减去已占用宽度，最小为 1。
func viewBudget(width, used int) int {
	return max(1, viewWidth(width)-used)
}
