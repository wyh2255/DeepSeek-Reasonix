// think.go 实现了 ThinkSplitter，用于从 content 流中提取 MiniMax-M3 的内联思维链。
//
// MiniMax-M3 不像 DeepSeek 那样使用 reasoning_content 字段，而是在 content 中
// 用 <think>...</think> 标签包裹思维链内容。ThinkSplitter 将这些标签内容提取为
// ChunkReasoning 事件，剩余部分作为 ChunkText 事件。
//
// 状态机设计:
//   - thinkProbe: 初始探测状态，等待首个字符判断是否以 <think> 开头
//   - thinkInside: 在思维链内部，扫描 </think> 关闭标签
//   - thinkPassthrough: 思维链已结束或不存在，直接透传为文本
//
// 关键特性:
//   - 仅在回合最开头的 <think> 被识别（防止误劫持提及此标签的回答）
//   - 支持跨增量的标签边界检测（markerSuffixLen）
//   - 流结束时 flush 缓冲区中未完成的内容
package openai

import "strings"

const (
	thinkOpen  = "<think>"   // 思维链开始标签
	thinkClose = "</think>" // 思维链结束标签
)

// thinkState 是 ThinkSplitter 的状态机状态。
type thinkState int

const (
	thinkProbe       thinkState = iota // 初始探测：等待判断是否以 <think> 开头
	thinkInside                        // 在思维链内部：扫描 </think> 关闭标签
	thinkPassthrough                   // 透传模式：直接输出为文本
)

// thinkSplitter 从 content 流中提取 <think>...</think> 块作为推理文本。
// MiniMax-M3 以内联方式嵌入思维链（而非填充 reasoning_content），
// 此结构在回合最开头的 <think> 处激活，因此不会劫持仅仅提及此标签的回答。
type thinkSplitter struct {
	state thinkState // 当前状态
	buf   string     // 跨增量的缓冲区
}

// push 将增量字符串推入状态机，返回 (reasoning, text)。
// reasoning 非空表示思维链内容，text 非空表示可见文本。
// 两个返回值可能同时为空（等待更多数据）。
func (t *thinkSplitter) push(s string) (reasoning, text string) {
	switch t.state {
	case thinkPassthrough:
		return "", s
	case thinkInside:
		return t.scanClose(s)
	}

	t.buf += s
	trimmed := strings.TrimLeft(t.buf, " \t\r\n")
	if len(trimmed) < len(thinkOpen) {
		if strings.HasPrefix(thinkOpen, trimmed) {
			return "", "" // still could become <think> once more arrives
		}
		return "", t.drainPassthrough()
	}
	if strings.HasPrefix(trimmed, thinkOpen) {
		t.state = thinkInside
		t.buf = ""
		return t.scanClose(trimmed[len(thinkOpen):])
	}
	return "", t.drainPassthrough()
}

// scanClose 在思维链内部扫描 </think> 关闭标签。
// 找到标签时返回标签前的思维内容和标签后的文本；未找到时保留可能的标签前缀。
func (t *thinkSplitter) scanClose(s string) (reasoning, text string) {
	t.buf += s
	if idx := strings.Index(t.buf, thinkClose); idx >= 0 {
		r := t.buf[:idx]
		rest := strings.TrimLeft(t.buf[idx+len(thinkClose):], " \t\r\n")
		t.buf = ""
		t.state = thinkPassthrough
		return r, rest
	}
	keep := markerSuffixLen(t.buf, thinkClose)
	r := t.buf[:len(t.buf)-keep]
	t.buf = t.buf[len(t.buf)-keep:]
	return r, ""
}

// flush 在流结束时输出缓冲区中剩余的内容。
// 未闭合的 <think> 块作为推理内容输出；其他内容作为文本输出。
func (t *thinkSplitter) flush() (reasoning, text string) {
	if t.buf == "" {
		return "", ""
	}
	out := t.buf
	t.buf = ""
	if t.state == thinkInside {
		return out, ""
	}
	return "", out
}

// drainPassthrough 将缓冲区内容作为文本输出并切换到透传模式。
func (t *thinkSplitter) drainPassthrough() string {
	t.state = thinkPassthrough
	out := t.buf
	t.buf = ""
	return out
}

// markerSuffixLen 返回 s 的最长后缀长度，该后缀是 marker 的前缀。
// 用于跨增量边界检测：当缓冲区末尾可能是标签的前几个字符时，
// 保留这些字符等待下一个增量补全。
//
// 例如: s="abc <think>", marker="</think>" → 返回 7（"<think>" 是 "</think>" 的前缀）
func markerSuffixLen(s, marker string) int {
	max := len(marker) - 1
	if max > len(s) {
		max = len(s)
	}
	for n := max; n > 0; n-- {
		if strings.HasPrefix(marker, s[len(s)-n:]) {
			return n
		}
	}
	return 0
}
