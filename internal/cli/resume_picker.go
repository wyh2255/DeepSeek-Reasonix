// resume_picker.go 实现了 TUI 中 /resume 命令的交互式会话选择器覆盖层。
// 用户可通过 ↑/↓ 键浏览保存的会话列表，按 Enter 确认恢复，
// 按 Esc 取消。其模式与 rewindPicker 类似。
package cli

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"reasonix/internal/agent"
	"reasonix/internal/i18n"
)

// resumePicker 是 /resume 命令的交互式覆盖层，允许用户通过 ↑/↓ 导航浏览
// 已保存的会话，按 Enter 确认恢复。按键通过 handleResumePickerKey 路由，
// 渲染通过 renderResumePicker 完成，当 m.resumePick 非 nil 时激活。
type resumePicker struct {
	sessions []agent.SessionInfo
	sel      int // selected index
	active   int // index of the currently-active session (-1 when none)
}

// openResumePicker 从会话目录加载数据并打开会话选择器。
// 当没有已保存的会话时，显示通知并返回（不打开选择器）。
func (m *chatTUI) openResumePicker() {
	sessions := recentSessions(m.ctrl.SessionDir())
	if len(sessions) == 0 {
		m.notice(i18n.M.NoSessionToResume)
		return
	}
	active := m.ctrl.SessionPath()
	activeIdx := -1
	for i, s := range sessions {
		if s.Path == active {
			activeIdx = i
			break
		}
	}
	// Default selection: the first session after the active one, else 0.
	sel := 0
	if activeIdx >= 0 && activeIdx+1 < len(sessions) {
		sel = activeIdx + 1
	}
	m.resumePick = &resumePicker{sessions: sessions, sel: sel, active: activeIdx}
}

// handleResumePickerKey 处理会话选择器中的按键事件：
// ↑/k 向上导航、↓/j 向下导航、Enter 确认选择、Esc 关闭选择器。
func (m chatTUI) handleResumePickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	r := m.resumePick
	if r == nil {
		return m, nil
	}
	switch msg.String() {
	case "up", "k":
		if r.sel > 0 {
			r.sel--
		}
	case "down", "j":
		if r.sel < len(r.sessions)-1 {
			r.sel++
		}
	case "enter":
		return m.applyResumePick()
	case "esc":
		m.resumePick = nil
	}
	return m, nil
}

// applyResumePick 执行会话选择器的确认操作：加载选中的会话，
// 保存当前会话快照，恢复目标会话并回放其对话记录。
func (m chatTUI) applyResumePick() (tea.Model, tea.Cmd) {
	r := m.resumePick
	if r == nil || r.sel < 0 || r.sel >= len(r.sessions) {
		return m, nil
	}
	target := r.sessions[r.sel]
	m.resumePick = nil
	if target.Path == m.ctrl.SessionPath() {
		m.notice(i18n.M.ResumeAlreadyActive)
		return m, nil
	}
	if m.ctrl.Running() {
		m.notice(i18n.M.ResumeBusy)
		return m, nil
	}
	loaded, err := agent.LoadSession(target.Path)
	if err != nil {
		m.notice("resume: " + err.Error())
		return m, nil
	}
	_ = m.ctrl.Snapshot()
	m.ctrl.Resume(loaded, target.Path)
	m.replayActiveBranch(i18n.M.ResumedTitle)
	return m, nil
}

// renderResumePicker 渲染会话选择器的界面，包括标题、会话列表（带选中高亮）
// 和操作提示，使用 choicePanelStyle 样式化输出。
func (m chatTUI) renderResumePicker() string {
	r := m.resumePick
	if r == nil {
		return ""
	}
	w := max(m.width, 10)
	var b strings.Builder
	b.WriteString(accent(i18n.M.ResumePickTitle) + "\n")
	for i, s := range r.sessions {
		label := sessionPickerLabel(s)
		if i == r.active {
			label = dim(label) + " " + dim("(active)")
		}
		b.WriteString(rowLine(i == r.sel, i+1, "", label, false) + "\n")
	}
	b.WriteString(dim(i18n.M.ResumePickHint))
	return choicePanelStyle.Width(w).Render(b.String())
}

// sessionPickerLabel 生成会话选择器中每行的标签文本（"N turns · topicTitle/first message"），
// 截断以适应显示宽度。当设置了 TopicTitle 时优先显示标题而非原始预览。
func sessionPickerLabel(s agent.SessionInfo) string {
	preview := s.Preview
	if s.TopicTitle != "" {
		preview = s.TopicTitle
	}
	if preview == "" {
		preview = "(no user message yet)"
	}
	return fmt.Sprintf("%d turns · %s", s.Turns, ansi.Truncate(preview, 60, "…"))
}
