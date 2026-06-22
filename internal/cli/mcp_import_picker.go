// mcp_import_picker.go 实现了从 cc-switch 导入 MCP 服务器时的交互式选择器。
// 该文件负责：
//   - 显示可导入的 MCP 服务器候选列表
//   - 支持键盘上下导航和空格键切换选中状态
//   - 将选中的服务器批量导入到当前会话中（包括连接、更新和配置）
package cli

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"reasonix/internal/config"
)

// mcpImportPicker 是 MCP 导入选择器的状态结构，管理候选列表、光标位置和选中状态。
type mcpImportPicker struct {
	candidates []config.MCPImportCandidate
	cursor     int
	checked    []bool
}

// newMCPImportPicker 创建一个新的 MCP 导入选择器。
// 推荐（Recommended）的候选服务器默认被勾选。
func newMCPImportPicker(candidates []config.MCPImportCandidate) *mcpImportPicker {
	p := &mcpImportPicker{candidates: candidates, checked: make([]bool, len(candidates))}
	for i, c := range candidates {
		p.checked[i] = c.Recommended
	}
	return p
}

// openMCPImportPicker 打开 MCP 导入选择器。
// 从 cc-switch 配置加载候选服务器列表，如果没有候选项则显示提示信息。
func (m *chatTUI) openMCPImportPicker() {
	candidates, err := config.LoadCCSwitchMCPCandidates()
	if err != nil {
		m.notice("mcp import: " + err.Error())
		return
	}
	if len(candidates) == 0 {
		m.notice("mcp import: no candidates found")
		return
	}
	m.completion = completion{}
	m.mcpImport = newMCPImportPicker(candidates)
}

// handleMCPImportKey 处理 MCP 导入选择器的键盘输入。
// 支持 Esc/Ctrl+C 取消、上下键导航、空格键切换选中、Enter 确认导入。
// 导入时会调用控制器批量导入选中的服务器，并报告添加、更新、连接和失败的数量。
func (m chatTUI) handleMCPImportKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	p := m.mcpImport
	switch msg.String() {
	case "esc", "ctrl+c":
		m.mcpImport = nil
		return m, nil
	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
		}
	case "down", "j":
		if p.cursor < len(p.candidates)-1 {
			p.cursor++
		}
	case " ", "space":
		p.checked[p.cursor] = !p.checked[p.cursor]
	case "enter":
		var entries []config.PluginEntry
		for i, ok := range p.checked {
			if ok {
				entries = append(entries, p.candidates[i].Entry)
			}
		}
		m.mcpImport = nil
		if len(entries) == 0 {
			m.notice("mcp import: nothing selected")
			return m, nil
		}
		total, added, updated, connected, failed, skipped, err := m.ctrl.ImportMCPEntries(entries)
		if err != nil {
			m.notice("mcp import: " + err.Error())
			return m, nil
		}
		m.host = m.ctrl.Host()
		m.notice(fmt.Sprintf("imported %d selected MCP servers from cc-switch (%d added, %d updated, %d connected, %d failed, %d skipped)", total, added, updated, connected, failed, skipped))
	}
	return m, nil
}

// renderMCPImport 渲染 MCP 导入选择器的界面。
// 显示标题、操作提示和候选服务器列表，当前光标行高亮显示。
func (m chatTUI) renderMCPImport() string {
	p := m.mcpImport
	if p == nil {
		return ""
	}
	w := max(m.width, 10)
	var b strings.Builder
	b.WriteString(accent("Import MCP from cc-switch") + "\n")
	b.WriteString(dim("Space select · Enter import · Esc cancel") + "\n\n")
	for i, c := range p.candidates {
		box := "[ ]"
		if p.checked[i] {
			box = "[x]"
		}
		mark := " "
		if i == p.cursor {
			mark = "›"
		}
		reasons := strings.Join(c.Reasons, ", ")
		line := fmt.Sprintf("%s %s %-34s %s", mark, box, c.Entry.Name, dim(reasons))
		if i == p.cursor {
			line = reverse(line)
		}
		b.WriteString(line + "\n")
	}
	return choicePanelStyle.Width(w).Render(strings.TrimRight(b.String(), "\n"))
}
