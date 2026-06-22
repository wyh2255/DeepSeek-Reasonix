// toolcard.go 负责将工具调用格式化为 Claude 风格的卡片行。
// 例如将原始的 "-> bash {\"command\":\"ls\"}" 渲染为 "● Bash(ls)"，
// 并通过 "⎿" 连接符将工具输出/思考内容与卡片头关联起来。
// 同时根据工具类别（读取/写入/执行/进程控制）着色状态圆点。
package cli

import (
	"encoding/json"
	"strconv"
	"strings"

	"reasonix/internal/tool"
)

// connector is the Claude-style "⎿" gutter that ties a continuation block (tool
// output, streamed thinking) to the header line above it.
const connector = "  ⎿  "

// connectorBlock 渲染连接块：第一行带 "⎿" 连接符前缀，后续行与之对齐。
// 用于工具输出、流式思考等内容的缩进显示。输入为空时返回空字符串。
func connectorBlock(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	indent := strings.Repeat(" ", len([]rune(connector)))
	out := dim(connector) + lines[0]
	for _, ln := range lines[1:] {
		out += "\n" + indent + ln
	}
	return out
}

// toolVerb maps a tool's snake_case id to the verb shown in its card.
var toolVerb = map[string]string{
	"bash":          "Bash",
	"bash_output":   "Output",
	"kill_shell":    "Kill",
	"wait":          "Wait",
	"read_file":     "Read",
	"write_file":    "Write",
	"edit_file":     "Update",
	"multi_edit":    "Update",
	"move_file":     "Move",
	"delete_range":  "Update",
	"delete_symbol": "Update",
	"notebook_edit": "Update",
	"glob":          "Glob",
	"grep":          "Search",
	"ls":            "List",
	"web_fetch":     "Fetch",
	"web_search":    "Search",
	"complete_step": "Step",
	"task":          "Task",
}

// toolArgKey is the JSON field shown in parentheses for each tool (wait is
// special-cased — it carries a job_ids array, not a scalar).
var toolArgKey = map[string]string{
	"bash":          "command",
	"bash_output":   "job_id",
	"kill_shell":    "job_id",
	"read_file":     "path",
	"write_file":    "path",
	"edit_file":     "path",
	"multi_edit":    "path",
	"move_file":     "source_path",
	"delete_range":  "path",
	"delete_symbol": "name",
	"notebook_edit": "path",
	"glob":          "pattern",
	"grep":          "pattern",
	"ls":            "path",
	"web_fetch":     "url",
	"web_search":    "query",
	"complete_step": "summary",
	"task":          "description",
}

// toolDot 返回带颜色的状态圆点 "●"，颜色由工具类别决定：
// 读取类为青色、写入类为绿色、执行类为黄色、进程控制类为紫色、其他为强调色。
func toolDot(name string) string {
	var c cliColor
	switch toolCategory[name] {
	case "read":
		c = activeCLITheme.toolRead
	case "write":
		c = activeCLITheme.success
	case "exec":
		c = activeCLITheme.warn
	case "proc":
		c = activeCLITheme.toolProc
	default:
		c = activeCLITheme.accent
	}
	return themeFg(c, "●")
}

// toolCategory 将工具名称映射到其类别，用于决定状态圆点的颜色。
var toolCategory = map[string]string{
	"read_file": "read", "ls": "read", "glob": "read", "grep": "read",
	"web_fetch": "read", "web_search": "read", "bash_output": "read",
	"write_file": "write", "edit_file": "write", "multi_edit": "write",
	"move_file": "write", "delete_range": "write", "delete_symbol": "write", "notebook_edit": "write",
	"bash": "exec",
	"wait": "proc", "kill_shell": "proc",
}

// toolDisplayName 返回工具在卡片中显示的动词名称。
// 对 MCP 工具（mcp__server__tool 格式）提取短名称，内置工具使用映射表，
// 其他工具直接返回原始 id。
func toolDisplayName(name string) string {
	if _, short, ok := tool.SplitMCPName(name); ok {
		return short
	}
	if v, ok := toolVerb[name]; ok {
		return v
	}
	return name
}

// toolArg 从工具的 JSON 参数中提取卡片括号内显示的主要参数值。
// 例如 bash 工具显示 command 字段，read_file 工具显示 path 字段。
// wait 工具特殊处理，显示 job_ids 数组。
func toolArg(name, args string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return ""
	}
	if name == "wait" {
		return argList(m["job_ids"])
	}
	v, ok := m[toolArgKey[name]]
	if !ok {
		return ""
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case []any:
		return argList(x)
	case float64:
		return strconv.Itoa(int(x))
	default:
		return ""
	}
}

// argList 将任意值数组转换为逗号分隔的字符串列表。
func argList(v any) string {
	arr, ok := v.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

// toolCard 渲染工具调用的分派行，格式为 "  ● Verb(arg)"，参数会截断以适应宽度。
func toolCard(name, args string, width int) string {
	return "  " + toolDot(name) + " " + toolHead(name, toolArg(name, args), width)
}

// toolHead 构建 "Verb(arg)" 格式的头部文本，动词加粗显示，参数截断以适应剩余宽度。
// 被 toolCard 和 diff 块头部共用。
func toolHead(name, arg string, width int) string {
	label := toolDisplayName(name)
	head := bold(label)
	if arg != "" {
		avail := width - 4 - len([]rune(label)) - 2
		head += dim("(") + clampPlain(arg, avail) + dim(")")
	}
	return head
}
