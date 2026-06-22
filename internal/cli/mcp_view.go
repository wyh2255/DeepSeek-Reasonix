// mcp_view.go 实现了 /mcp 命令的静态状态视图渲染。
// 该文件负责：
//   - 渲染已连接 MCP 服务器的列表，显示工具/提示/资源数量
//   - 渲染连接失败的服务器及错误信息
//   - 列出每个服务器的 prompts 和 resources 子项
//   - 提供文本截断和压缩的辅助函数（compactEnd、compactMiddle 等）
//   - 提供计数文本格式化（如 "1 tool" vs "3 tools"）
package cli

import (
	"fmt"
	"sort"
	"strings"

	"reasonix/internal/plugin"
)

// mcpMaxItemsPerSection 是每个服务器在状态视图中显示的 prompts/resources 最大数量
const mcpMaxItemsPerSection = 6

// renderMCPStatus 渲染 MCP 服务器的状态概览视图。
// 显示所有已连接服务器的工具、提示和资源信息，以及连接失败的服务器错误。
func renderMCPStatus(width int, servers []plugin.ServerStatus, prompts []plugin.Prompt, resources []plugin.Resource, failures []plugin.Failure) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", viewHeader("MCP servers (%d)", len(servers)))

	promptsByServer := map[string][]plugin.Prompt{}
	for _, p := range prompts {
		promptsByServer[p.Server] = append(promptsByServer[p.Server], p)
	}
	resourcesByServer := map[string][]plugin.Resource{}
	for _, r := range resources {
		resourcesByServer[r.Server] = append(resourcesByServer[r.Server], r)
	}

	seen := map[string]bool{}
	for _, s := range servers {
		seen[s.Name] = true
		writeMCPServer(&b, width, s, promptsByServer[s.Name], resourcesByServer[s.Name])
	}
	for _, name := range extraMCPServers(seen, promptsByServer, resourcesByServer) {
		writeMCPServer(&b, width, plugin.ServerStatus{Name: name}, promptsByServer[name], resourcesByServer[name])
	}
	for _, f := range failures {
		writeMCPFailure(&b, width, f)
	}
	return strings.TrimRight(b.String(), "\n")
}

// extraMCPServers 查找在 prompts 或 resources 中出现但不在已连接服务器列表中的服务器名称。
// 这些服务器可能没有工具但提供了提示或资源。
func extraMCPServers(seen map[string]bool, prompts map[string][]plugin.Prompt, resources map[string][]plugin.Resource) []string {
	set := map[string]bool{}
	for name := range prompts {
		if !seen[name] {
			set[name] = true
		}
	}
	for name := range resources {
		if !seen[name] {
			set[name] = true
		}
	}
	var names []string
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// writeMCPServer 将单个已连接 MCP 服务器的信息写入构建器。
// 显示服务器名称、传输类型、工具/提示/资源数量，以及提示和资源的子列表。
func writeMCPServer(b *strings.Builder, width int, s plugin.ServerStatus, prompts []plugin.Prompt, resources []plugin.Resource) {
	transport := s.Transport
	if transport == "" {
		transport = "unknown"
	}
	meta := fmt.Sprintf("(%s)  %s · %s · %s", transport, countText(s.Tools, "tool"), countText(len(prompts), "prompt"), countText(len(resources), "resource"))
	name := viewCompactText(s.Name, viewBudget(width, 4+2+1+visibleWidth(meta)))
	fmt.Fprintf(b, "    %s %s %s\n", accent("✓"), bold(name), viewMeta(meta))
	if len(prompts) > 0 {
		writeMCPPromptList(b, width, prompts)
	}
	if len(resources) > 0 {
		writeMCPResourceList(b, width, resources)
	}
}

// writeMCPFailure 将连接失败的 MCP 服务器信息写入构建器。
func writeMCPFailure(b *strings.Builder, width int, f plugin.Failure) {
	transport := f.Transport
	if transport == "" {
		transport = "unknown"
	}
	meta := fmt.Sprintf("(%s)  %s", transport, oneLineText(f.Error))
	name := viewCompactText(f.Name, viewBudget(width, 4+2+1+visibleWidth(meta)))
	fmt.Fprintf(b, "    %s %s %s\n", yellow("!"), bold(name), viewMeta(viewCompactText(meta, viewBudget(width, 10+visibleWidth(name)))))
}

// writeMCPPromptList 渲染 MCP 服务器的 prompts 子列表。
func writeMCPPromptList(b *strings.Builder, width int, prompts []plugin.Prompt) {
	b.WriteString(viewSubhead("    prompts") + "\n")
	limit := len(prompts)
	if limit > mcpMaxItemsPerSection {
		limit = mcpMaxItemsPerSection
	}
	for _, p := range prompts[:limit] {
		writeMCPItem(b, width, "      ", "/"+p.Name, p.Description)
	}
	if extra := len(prompts) - limit; extra > 0 {
		fmt.Fprintf(b, "    %s\n", viewMore(extra, "prompts"))
	}
}

// writeMCPResourceList 渲染 MCP 服务器的 resources 子列表。
func writeMCPResourceList(b *strings.Builder, width int, resources []plugin.Resource) {
	b.WriteString(viewSubhead("    resources") + "\n")
	limit := len(resources)
	if limit > mcpMaxItemsPerSection {
		limit = mcpMaxItemsPerSection
	}
	for _, r := range resources[:limit] {
		label := strings.TrimSpace(r.Name)
		if label == "" {
			label = strings.TrimSpace(r.Description)
		}
		if r.MimeType != "" {
			if label == "" {
				label = r.MimeType
			} else {
				label += " [" + r.MimeType + "]"
			}
		}
		writeMCPItem(b, width, "      ", "@"+r.Server+":"+r.URI, label)
	}
	if extra := len(resources) - limit; extra > 0 {
		fmt.Fprintf(b, "    %s\n", viewMore(extra, "resources"))
	}
}

// writeMCPItem 渲染单个 MCP 条目（prompt 或 resource），包含引用和描述文本。
// 当空间不足时只显示引用，描述被截断或省略。
func writeMCPItem(b *strings.Builder, width int, indent, ref, desc string) {
	desc = oneLineText(desc)
	ref = oneLineText(ref)
	available := viewBudget(width, visibleWidth(indent))
	if desc == "" || available < 30 {
		b.WriteString(indent + compactMiddle(ref, available))
		b.WriteByte('\n')
		return
	}
	descBudget := min(40, max(12, available/2))
	refBudget := available - 2 - descBudget
	if refBudget < 16 {
		refBudget = min(16, available)
		descBudget = available - refBudget - 2
	}
	line := indent + compactMiddle(ref, refBudget)
	if descBudget >= 12 {
		line += "  " + viewMeta(viewCompactText(desc, descBudget))
	}
	b.WriteString(line)
	b.WriteByte('\n')
}

// oneLineText 将多行文本合并为单行，用空格替换所有空白字符。
func oneLineText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// countText 生成带数量的名词文本，自动处理单复数（如 "1 tool" vs "3 tools"）。
func countText(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// compactEnd 从末尾截断字符串到指定宽度，超出部分用 "…" 替代。
func compactEnd(s string, maxWidth int) string {
	if maxWidth <= 0 || visibleWidth(s) <= maxWidth {
		return s
	}
	if maxWidth <= 1 {
		return "…"
	}
	var out strings.Builder
	for _, r := range s {
		next := out.String() + string(r)
		if visibleWidth(next)+1 > maxWidth {
			break
		}
		out.WriteRune(r)
	}
	return out.String() + "…"
}

// compactMiddle 从中间截断字符串到指定宽度，保留首尾部分，中间用 "…" 替代。
// 适用于文件路径等需要保留开头和结尾信息的场景。
func compactMiddle(s string, maxWidth int) string {
	if maxWidth <= 0 || visibleWidth(s) <= maxWidth {
		return s
	}
	if maxWidth <= 3 {
		return compactEnd(s, maxWidth)
	}
	keep := maxWidth - 1
	leftWidth := keep / 2
	rightWidth := keep - leftWidth
	left := takeLeftWidth(s, leftWidth)
	right := takeRightWidth(s, rightWidth)
	return left + "…" + right
}

// takeLeftWidth 从字符串左侧取指定显示宽度的字符。
func takeLeftWidth(s string, maxWidth int) string {
	var out strings.Builder
	for _, r := range s {
		next := out.String() + string(r)
		if visibleWidth(next) > maxWidth {
			break
		}
		out.WriteRune(r)
	}
	return out.String()
}

// takeRightWidth 从字符串右侧取指定显示宽度的字符。
func takeRightWidth(s string, maxWidth int) string {
	var out []rune
	width := 0
	for _, r := range reverseRunes([]rune(s)) {
		w := visibleWidth(string(r))
		if width+w > maxWidth {
			break
		}
		out = append(out, r)
		width += w
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// reverseRunes 返回 rune 切片的反转副本。
func reverseRunes(in []rune) []rune {
	out := append([]rune(nil), in...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
