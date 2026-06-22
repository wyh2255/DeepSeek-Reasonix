// clear_confirm.go 实现了 /clear 命令的确认对话框逻辑。
// 当用户执行 /clear 命令时，会弹出确认面板让用户选择"清除"或"取消"。
// 清除操作会删除当前会话的转录记录，仅保留系统提示词。

package cli

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"reasonix/internal/i18n"
)

// clearConfirm 表示清除确认对话框的状态。
// confirm 字段记录当前选中的选项：0 表示"清除"，1 表示"取消"。
type clearConfirm struct {
	confirm int // 0 = clear, 1 = cancel
}

// handleClearConfirmKey 处理清除确认对话框中的键盘输入。
// 支持方向键/j/k/Tab 切换选项，y/Y 确认清除，n/N/esc/ctrl+c 取消。
// Enter 键执行当前高亮选项的操作。
func (m chatTUI) handleClearConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "down", "left", "right", "j", "k", "tab", "shift+tab":
		if m.clearConfirm.confirm == 0 {
			m.clearConfirm.confirm = 1
		} else {
			m.clearConfirm.confirm = 0
		}
	case "y", "Y":
		return m.confirmClearContext()
	case "n", "N", "esc", "ctrl+c":
		m.clearConfirm = nil
	case "enter":
		if m.clearConfirm.confirm == 0 {
			return m.confirmClearContext()
		}
		m.clearConfirm = nil
	}
	return m, nil
}

// confirmClearContext 执行清除操作：调用控制器清除会话，重置界面状态，并刷新屏幕。
func (m chatTUI) confirmClearContext() (tea.Model, tea.Cmd) {
	m.clearConfirm = nil
	if err := m.ctrl.ClearSession(); err != nil {
		m.notice(fmt.Sprintf("%s: %v", i18n.M.SlashClearFailed, err))
		return m, nil
	}
	m.resetFreshContextView(true)
	m.notice(i18n.M.SlashClearDone)
	return m, tea.ClearScreen
}

// resetFreshContextView 将聊天界面重置为全新会话的初始状态。
// 清除待处理消息、推理内容、选择器等，重新渲染横幅。
// clearTranscript 参数控制是否清空转录记录和视口内容。
func (m *chatTUI) resetFreshContextView(clearTranscript bool) {
	m.finalizeStreamed()
	m.pending.Reset()
	m.reasoning.Reset()
	m.todoArgs = ""
	m.chooser = nil
	m.pendingApproval = nil
	m.bubblePending = false
	m.turnDiscarded = false
	if clearTranscript {
		m.transcript = nil
		m.wrappedLines = nil
		m.viewport.SetContent("")
	} else {
		m.commitLine("")
	}
	m.commitLine(strings.TrimRight(renderTUIBanner(m.label, "", transcriptContentWidth(m.width, m.nativeScrollback)), "\n"))
	m.transcriptDirty = true
	m.forceGotoBottom = true
}

// renderClearConfirm 渲染清除确认对话框，显示提示信息和"清除"/"取消"两个选项。
func (m chatTUI) renderClearConfirm() string {
	if m.clearConfirm == nil {
		return ""
	}
	w := max(viewWidth(m.width), 40)
	var b strings.Builder
	b.WriteString(i18n.M.SlashClearPrompt + "\n")
	b.WriteString(viewMeta("This deletes the current transcript from local history and keeps only the system prompt.") + "\n\n")
	b.WriteString(rowLine(m.clearConfirm.confirm == 0, 1, "", "Clear", false) + "\n")
	b.WriteString(rowLine(m.clearConfirm.confirm == 1, 2, "", "Cancel", false))
	return choicePanelStyle.Width(w).Render(b.String())
}
