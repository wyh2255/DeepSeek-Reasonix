// mathnode.go 实现了 Goldmark Markdown 解析器的数学公式扩展。
// 该文件负责：
//   - 定义 mathNode AST 节点类型，用于表示行内数学公式
//   - 实现 mathParser 内联解析器，识别 $...$ 和 $$...$$ 分隔的数学表达式
//   - 包含货币符号保护逻辑，避免 "$5 and $10" 被误判为数学公式
//   - 将解析到的 LaTeX 表达式转换为 Unicode 显示形式
package cli

import (
	"bytes"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// kindMath 是数学公式节点的 AST 节点类型标识
var kindMath = ast.NewNodeKind("Math")

// mathNode 表示 Markdown 中的行内数学公式节点。
// value 存储已转换为 Unicode 的数学表达式，display 标记是否为块级显示模式。
type mathNode struct {
	ast.BaseInline
	value   string
	display bool
}

// Kind 返回数学公式节点的类型标识
func (n *mathNode) Kind() ast.NodeKind { return kindMath }

// Dump 输出节点的调试信息
func (n *mathNode) Dump(src []byte, level int) { ast.DumpHelper(n, src, level, nil, nil) }

// mathParser 是 Goldmark 的行内解析器扩展，负责识别 $ 符号包围的数学公式。
type mathParser struct{}

// Trigger 返回触发此解析器的字符，即美元符号 $
func (p *mathParser) Trigger() []byte { return []byte{'$'} }

// Parse 尝试从当前位置解析一个数学公式节点。
// 支持 $...$（行内）和 $$...$$（块级）两种分隔符。
// 包含货币符号保护：单 $ 模式下，如果开头有空格、结尾有空格、或闭合符后紧跟数字，
// 则不视为数学公式（避免 "$5 and $10" 被误解析）。
func (p *mathParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	line, _ := block.PeekLine()
	if len(line) == 0 || line[0] != '$' {
		return nil
	}
	display := len(line) >= 2 && line[1] == '$'
	delim := 1
	if display {
		delim = 2
	}

	rest := line[delim:]
	var closeAt int
	if display {
		closeAt = bytes.Index(rest, []byte("$$"))
	} else {
		closeAt = bytes.IndexByte(rest, '$')
	}
	if closeAt < 0 {
		return nil
	}
	inner := rest[:closeAt]
	if len(bytes.TrimSpace(inner)) == 0 {
		return nil
	}

	// Currency guard (markdown-it-texmath rule): a single-$ span only counts as
	// math when the open isn't followed by space, the close isn't preceded by
	// space, and the char after the close isn't a digit — so "$5 and $10" stays
	// prose. Display $$ is unambiguous and skips the check.
	if !display {
		after := closeAt + 1
		if inner[0] == ' ' || inner[len(inner)-1] == ' ' ||
			(after < len(rest) && rest[after] >= '0' && rest[after] <= '9') {
			return nil
		}
	}

	block.Advance(delim + closeAt + delim)
	return &mathNode{value: latexToUnicode(string(bytes.TrimSpace(inner))), display: display}
}
