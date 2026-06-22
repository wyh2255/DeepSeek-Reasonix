// md.go 实现了 Markdown 到 ANSI 终端文本的渲染器。
// 该文件负责：
//   - 使用 Goldmark 解析 Markdown AST，支持 GFM 表格扩展和数学公式扩展
//   - 将标题、段落、列表、代码块、引用块、表格等渲染为带 ANSI 颜色的终端文本
//   - 处理行内元素：加粗、斜体、代码片段、链接、数学公式
//   - 实现 CJK 宽度感知的自动换行，正确处理中文/日文/韩文字符宽度
//   - 修复 Goldmark 对 CJK 标点符号的 emphasis 边界判断问题
//   - 支持 GFM 表格的自动列宽计算和单元格换行
package cli

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// mdRenderer 是 Markdown 终端渲染器，将模型的 Markdown 回答转换为带 ANSI 样式的终端文本。
// 仅实现聊天模型常用 Markdown 构造的渲染（标题、段落、列表、代码块、引用、强调、链接、
// 分隔线、表格），其他内容降级为纯文本。自动换行尊重 CJK 字符宽度并跳过 ANSI SGR 转义码。
type mdRenderer struct {
	md    goldmark.Markdown
	width int
}

// newMarkdownRenderer 创建一个新的 Markdown 渲染器。
// width 为终端列宽，用于控制自动换行和表格列宽分配。
// 启用 GFM 表格扩展和自定义数学公式解析器。
func newMarkdownRenderer(width int) *mdRenderer {
	if width <= 0 {
		width = 80
	}
	// Enable the GFM table extension so | header | rows | get parsed into
	// a Table node rather than falling through as a literal text block.
	return &mdRenderer{
		md: goldmark.New(
			goldmark.WithExtensions(extension.Table),
			goldmark.WithParserOptions(
				parser.WithInlineParsers(util.Prioritized(&mathParser{}, 150)),
			),
		),
		width: width,
	}
}

// italic 将文本包装为 ANSI 斜体样式。当颜色被禁用时返回原始文本。
func italic(s string) string {
	if !colorEnabled {
		return s
	}
	return "\033[3m" + s + "\033[0m"
}

// Render 将 Markdown 文本解析并渲染为带 ANSI 样式的终端输出，末尾附加换行符。
// 空输入返回空字符串，调用方可据此区分"无需绘制"和"绘制空行"。
// 渲染前会先标准化数学分隔符和修复 CJK emphasis 问题。
func (r *mdRenderer) Render(input string) string {
	if strings.TrimSpace(input) == "" {
		return ""
	}
	input = fixCJKEmphasis(normalizeMath(input))
	src := []byte(input)
	doc := r.md.Parser().Parse(text.NewReader(src))
	var buf strings.Builder
	r.renderBlocks(&buf, doc, src, 0)
	out := strings.TrimRight(buf.String(), "\n")
	if out == "" {
		return ""
	}
	return out + "\n"
}

// fixCJKEmphasis works around goldmark's CommonMark parser not recognising
// CJK punctuation as Unicode punctuation: a closing ** is only right-flanking
// when the char before it is punctuation, so **X，**Y (， = U+FF0C) is not bold.
// Inserting a space after such a closer fixes the flanking. The space must go
// only on a *closer* — putting it after an opener (，**X** → ，** X**) would
// instead break the left-flanking — so emphasis open/close is tracked by a
// running toggle. Inline code spans and fenced blocks are passed through so
// literal ** inside code is never touched.
func fixCJKEmphasis(s string) string {
	runes := []rune(s)
	n := len(runes)
	var b strings.Builder
	b.Grow(len(s) + 16)

	inFenced := false   // inside ``` fenced code block
	inCode := false     // inside ` inline code span
	inEmphasis := false // between an opening ** and its closer

	for i := 0; i < n; i++ {
		r := runes[i]

		// Fenced code block: ``` toggles in/out.
		if r == '`' && i+2 < n && runes[i+1] == '`' && runes[i+2] == '`' {
			inFenced = !inFenced
			b.WriteString("```")
			i += 2
			continue
		}
		// Inline code span: ` toggles in/out (but not inside fenced blocks).
		if r == '`' && !inFenced {
			inCode = !inCode
			b.WriteRune(r)
			continue
		}
		// Inside code — pass through verbatim.
		if inCode || inFenced {
			b.WriteRune(r)
			continue
		}
		// Emphasis cannot span a hard line break; reset so an unclosed ** on a
		// previous line can't make the next line's opener look like a closer.
		if r == '\n' {
			inEmphasis = false
			b.WriteRune(r)
			continue
		}

		if r == '*' && i+1 < n && runes[i+1] == '*' {
			b.WriteString("**")
			i++
			inEmphasis = !inEmphasis

			// Only a closer (emphasis just ended) hugging CJK punctuation needs
			// the trailing space; the same space after an opener would break it.
			if !inEmphasis && i >= 2 && !isSpace(runes[i-2]) && isCJKPunct(runes[i-2]) {
				b.WriteByte(' ')
			}
			continue
		}

		b.WriteRune(r)
	}
	return b.String()
}

// isCJKPunct reports whether r is a CJK full-width punctuation character.
// These are not classified as Unicode punctuation by the CommonMark spec,
// which breaks the "right-flanking delimiter run" check for emphasis.
func isCJKPunct(r rune) bool {
	if r <= 0x7F {
		return false // ASCII punctuation is handled correctly by CommonMark
	}
	// Fast path: common CJK punctuation ranges.
	switch {
	case r >= 0x3000 && r <= 0x303F: // CJK Symbols and Punctuation (。、etc.)
		return true
	case r >= 0xFF01 && r <= 0xFF0F: // Fullwidth Forms I (! " # $ etc.)
		return true
	case r >= 0xFF1A && r <= 0xFF20: // Fullwidth Forms II (: ; < = etc.)
		return true
	case r >= 0xFF3B && r <= 0xFF3F: // Fullwidth Forms III ([ \ ] ^ _)
		return true
	case r >= 0xFF5B && r <= 0xFF65: // Fullwidth Forms IV ({ | } ~ etc.)
		return true
	}
	// Fallback: any non-ASCII punctuation (e.g. Tibetan, Armenian).
	return unicode.IsPunct(r)
}

// isSpace reports whether r is a whitespace character.
func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

// renderBlocks 遍历父节点的所有子块节点并逐一渲染。
func (r *mdRenderer) renderBlocks(buf *strings.Builder, parent ast.Node, src []byte, indent int) {
	for c := parent.FirstChild(); c != nil; c = c.NextSibling() {
		r.renderBlock(buf, c, src, indent)
	}
}

// renderBlock 根据节点类型分发到对应的渲染方法。
// 支持标题、段落、文本块、列表、代码块、引用块、表格和分隔线。
// 未知类型的块节点会递归渲染其子节点，避免丢失内容。
func (r *mdRenderer) renderBlock(buf *strings.Builder, node ast.Node, src []byte, indent int) {
	switch n := node.(type) {
	case *ast.Heading:
		r.renderHeading(buf, n, src, indent)
	case *ast.Paragraph:
		r.renderParagraph(buf, n, src, indent)
	case *ast.TextBlock:
		// TextBlock is goldmark's container for tight-list-item inline content
		// (no trailing blank). Treat it like a paragraph but skip the spacer.
		r.renderTextBlock(buf, n, src, indent)
	case *ast.List:
		r.renderList(buf, n, src, indent)
	case *ast.FencedCodeBlock, *ast.CodeBlock:
		r.renderFenced(buf, n, src, indent)
	case *ast.Blockquote:
		r.renderBlockquote(buf, n, src, indent)
	case *extast.Table:
		r.renderTable(buf, n, src, indent)
	case *ast.ThematicBreak:
		w := r.width - indent
		if w < 8 {
			w = 8
		}
		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString(dim(strings.Repeat("─", w)))
		buf.WriteString("\n\n")
	default:
		// Unknown block: drop into children rather than dropping content.
		r.renderBlocks(buf, node, src, indent)
	}
}

// renderHeading 渲染标题节点。一级标题带强调色下划线，其他级别仅使用加粗和颜色。
func (r *mdRenderer) renderHeading(buf *strings.Builder, n *ast.Heading, src []byte, indent int) {
	inline := r.collectInline(n, src)
	buf.WriteString(strings.Repeat(" ", indent))
	buf.WriteString(bold(accent(inline)))
	buf.WriteString("\n")
	// Level-1 headings get an accent underline; deeper levels rely on
	// bold+colour alone so the hierarchy reads at a glance without piling
	// on visual weight on every "###" in a long response.
	if n.Level == 1 {
		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString(accent(strings.Repeat("─", visibleWidth(inline))))
		buf.WriteString("\n")
	}
	buf.WriteString("\n")
}

func (r *mdRenderer) renderParagraph(buf *strings.Builder, n *ast.Paragraph, src []byte, indent int) {
	r.renderInlineBlock(buf, n, src, indent, true)
}

func (r *mdRenderer) renderTextBlock(buf *strings.Builder, n *ast.TextBlock, src []byte, indent int) {
	r.renderInlineBlock(buf, n, src, indent, false)
}

func (r *mdRenderer) renderInlineBlock(buf *strings.Builder, n ast.Node, src []byte, indent int, trailingBlank bool) {
	inline := r.collectInline(n, src)
	prefix := strings.Repeat(" ", indent)
	wrapped := wrapAnsi(inline, r.width-indent)
	for _, line := range strings.Split(wrapped, "\n") {
		buf.WriteString(prefix)
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	if trailingBlank {
		buf.WriteString("\n")
	}
}

// renderList 渲染有序和无序列表。有序列表使用数字标记，无序列表使用 "•" 标记。
// 列表项的后续行会正确缩进以对齐标记后的文本。
func (r *mdRenderer) renderList(buf *strings.Builder, n *ast.List, src []byte, indent int) {
	idx := 1
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		item, ok := c.(*ast.ListItem)
		if !ok {
			continue
		}
		var marker string
		if n.IsOrdered() {
			marker = fmt.Sprintf("%d.", idx)
			idx++
		} else {
			marker = "•"
		}
		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString(accent(marker) + " ")
		markerW := visibleWidth(marker) + 1

		first := item.FirstChild()
		// goldmark uses TextBlock for tight list items, Paragraph for loose
		// ones; treat both as the marker-line carrier so the inline content
		// lands next to the bullet either way.
		inlineHost := inlineCarrier(first)
		if inlineHost != nil {
			inline := r.collectInline(inlineHost, src)
			wrapped := wrapAnsi(inline, r.width-indent-markerW)
			lines := strings.Split(wrapped, "\n")
			buf.WriteString(lines[0] + "\n")
			for _, l := range lines[1:] {
				buf.WriteString(strings.Repeat(" ", indent+markerW))
				buf.WriteString(l + "\n")
			}
			for s := first.NextSibling(); s != nil; s = s.NextSibling() {
				r.renderBlock(buf, s, src, indent+markerW)
			}
		} else {
			buf.WriteString("\n")
			r.renderBlocks(buf, item, src, indent+2)
		}
	}
	buf.WriteString("\n")
}

// renderFenced 渲染围栏代码块，每行前添加竖线前缀，代码文本使用强调色。
func (r *mdRenderer) renderFenced(buf *strings.Builder, n ast.Node, src []byte, indent int) {
	prefix := strings.Repeat(" ", indent) + dim("│ ")
	for i := 0; i < n.Lines().Len(); i++ {
		l := n.Lines().At(i)
		line := strings.TrimRight(string(l.Value(src)), "\n")
		buf.WriteString(prefix)
		buf.WriteString(accent(line))
		buf.WriteString("\n")
	}
	buf.WriteString("\n")
}

// renderBlockquote 渲染引用块，每行前添加竖线前缀，文本使用暗色显示。
func (r *mdRenderer) renderBlockquote(buf *strings.Builder, n *ast.Blockquote, src []byte, indent int) {
	var inner strings.Builder
	r.renderBlocks(&inner, n, src, 0)
	prefix := strings.Repeat(" ", indent) + dim("▎ ")
	for _, line := range strings.Split(strings.TrimRight(inner.String(), "\n"), "\n") {
		buf.WriteString(prefix)
		buf.WriteString(dim(line))
		buf.WriteString("\n")
	}
	buf.WriteString("\n")
}

// collectInline 遍历行内子树并返回带 ANSI 样式的扁平文本。
func (r *mdRenderer) collectInline(n ast.Node, src []byte) string {
	var b strings.Builder
	r.appendInline(&b, n, src)
	return b.String()
}

// appendInline 递归遍历行内节点树，将各类型节点转换为 ANSI 样式文本。
// 处理文本、强调（加粗/斜体）、代码片段、链接、自动链接、原始HTML和数学公式。
func (r *mdRenderer) appendInline(b *strings.Builder, n ast.Node, src []byte) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch v := c.(type) {
		case *ast.Text:
			b.Write(v.Segment.Value(src))
			switch {
			case v.HardLineBreak():
				b.WriteByte('\n')
			case v.SoftLineBreak():
				b.WriteByte(' ')
			}
		case *ast.Emphasis:
			var inner strings.Builder
			r.appendInline(&inner, v, src)
			if v.Level == 2 {
				b.WriteString(bold(inner.String()))
			} else {
				b.WriteString(italic(inner.String()))
			}
		case *ast.CodeSpan:
			var inner strings.Builder
			r.appendInline(&inner, v, src)
			b.WriteString(accent(inner.String()))
		case *ast.Link:
			var inner strings.Builder
			r.appendInline(&inner, v, src)
			b.WriteString(inner.String())
			b.WriteString(dim(" (" + string(v.Destination) + ")"))
		case *ast.AutoLink:
			b.WriteString(string(v.URL(src)))
		case *ast.RawHTML:
			// drop — rare in chat output and would print as literal escapes
		case *mathNode:
			b.WriteString(italic(v.value))
		case *ast.String:
			b.Write(v.Value)
		default:
			r.appendInline(b, c, src)
		}
	}
}

// renderTable lays out a GFM table as terminal columns separated by dim
// "│" rails with a "─┼─" rule under the header. Column widths auto-fit the
// widest cell in each column and are capped to a fair share of the terminal
// width so a wide table can't push the input off-screen. Long cells are
// wrapped across multiple visual rows (the whole logical row inflates to
// the tallest cell), not truncated, so no content is lost. Alignment is
// left-only — Markdown's ":---:" hints are read but not honoured yet.
func (r *mdRenderer) renderTable(buf *strings.Builder, n *extast.Table, src []byte, indent int) {
	var header []string
	var rows [][]string

	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch row := c.(type) {
		case *extast.TableHeader:
			header = r.collectCells(row, src)
		case *extast.TableRow:
			rows = append(rows, r.collectCells(row, src))
		}
	}
	if len(header) == 0 && len(rows) == 0 {
		return
	}

	cols := len(header)
	for _, row := range rows {
		if len(row) > cols {
			cols = len(row)
		}
	}
	if cols == 0 {
		return
	}

	// Initial widths fit the widest cell content per column.
	widths := make([]int, cols)
	pick := func(i, w int) {
		if i < cols && w > widths[i] {
			widths[i] = w
		}
	}
	for i, h := range header {
		pick(i, visibleWidth(h))
	}
	for _, row := range rows {
		for i, c := range row {
			pick(i, visibleWidth(c))
		}
	}

	// Cap each column so the whole table fits the terminal: total = sum of
	// widths + separators (3 chars each) + indent. Distribute the budget
	// proportionally to the natural widths so columns with rich content
	// keep more space than narrow ones.
	available := r.width - indent - 3*(cols-1)
	if available < cols*3 {
		available = cols * 3
	}
	total := 0
	for _, w := range widths {
		total += w
	}
	if total > available {
		for i := range widths {
			widths[i] = widths[i] * available / total
			if widths[i] < 3 {
				widths[i] = 3
			}
		}
	}

	prefix := strings.Repeat(" ", indent)
	sep := dim(" │ ")

	if len(header) > 0 {
		r.renderTableRow(buf, prefix, sep, header, widths, true)
		buf.WriteString(prefix)
		for i := range widths {
			if i > 0 {
				buf.WriteString(dim("─┼─"))
			}
			buf.WriteString(dim(strings.Repeat("─", widths[i])))
		}
		buf.WriteByte('\n')
	}
	for _, row := range rows {
		r.renderTableRow(buf, prefix, sep, row, widths, false)
	}
	buf.WriteByte('\n')
}

// renderTableRow 渲染表格的一行，当单元格内容超出列宽时会跨多行显示。
// 整行的视觉高度等于所有单元格中最大的换行行数，内容不足的单元格用空格填充以保持对齐。
func (r *mdRenderer) renderTableRow(buf *strings.Builder, prefix, sep string, cells []string, widths []int, isHeader bool) {
	cols := len(widths)
	wrapped := make([][]string, cols)
	maxLines := 1
	for i := 0; i < cols; i++ {
		var text string
		if i < len(cells) {
			text = cells[i]
		}
		wrapped[i] = strings.Split(wrapAnsi(text, widths[i]), "\n")
		if len(wrapped[i]) > maxLines {
			maxLines = len(wrapped[i])
		}
	}
	for line := 0; line < maxLines; line++ {
		buf.WriteString(prefix)
		for i := 0; i < cols; i++ {
			if i > 0 {
				buf.WriteString(sep)
			}
			var cell string
			if line < len(wrapped[i]) {
				cell = wrapped[i][line]
			}
			padded := padRight(cell, widths[i])
			if isHeader {
				padded = bold(padded)
			}
			buf.WriteString(padded)
		}
		buf.WriteByte('\n')
	}
}

// collectCells 遍历表头或表行节点，提取每个单元格的行内内容为 ANSI 样式字符串。
func (r *mdRenderer) collectCells(parent ast.Node, src []byte) []string {
	var out []string
	for c := parent.FirstChild(); c != nil; c = c.NextSibling() {
		if cell, ok := c.(*extast.TableCell); ok {
			out = append(out, strings.TrimSpace(r.collectInline(cell, src)))
		}
	}
	return out
}

// inlineCarrier 判断节点是否为段落或文本块（两者都持有行内内容）。
// 用于列表渲染，确保无论列表是紧凑还是松散格式，标记行都能获取到行内内容。
func inlineCarrier(n ast.Node) ast.Node {
	switch n.(type) {
	case *ast.Paragraph, *ast.TextBlock:
		return n
	}
	return nil
}

// wrapAnsi 将文本按指定列宽进行自动换行。
// 对于无法在单行内放下的长词（如 CJK 文本无空格分隔）会强制断行。
// ANSI SGR 转义码被保留且不计入宽度，宽字符计为两列。
// 底层使用 x/ansi 库的 Wrap 函数实现。
func wrapAnsi(text string, width int) string {
	if width < 4 {
		width = 4
	}
	return ansi.Wrap(text, width, "")
}
