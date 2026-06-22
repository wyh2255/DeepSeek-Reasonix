// rewind.go 实现了 TUI 中的 Esc-Esc / "/rewind" 回退选择器覆盖层。
// 它允许用户浏览会话的检查点（每个 turn 一个），选择要恢复的 turn 和恢复范围
// （对话+代码、仅对话、仅代码、分支、摘要等）。
package cli

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"reasonix/internal/checkpoint"
	"reasonix/internal/control"
	"reasonix/internal/i18n"
)

// rewindPicker 是 Esc-Esc / "/rewind" 的交互式覆盖层。阶段 0 列出会话的
// turn（每个对应一个检查点）；阶段 1 为选定的 turn 选择恢复方式。
// 按键通过 handleRewindKey 路由，渲染通过 renderRewind 完成，当 m.rewind 非 nil 时激活。
type rewindPicker struct {
	metas []checkpoint.Meta
	sel   int // selected turn (index into metas)
	stage int // 0 = pick turn, 1 = pick scope
	scope int // index into rewindScopes (stage 1)
}

// rewindActions 定义了回退选择器第二阶段的所有可用操作，
// 包括恢复对话+代码、仅对话、仅代码、分支、从该点摘要、摘要到该点。
var rewindActions = []struct {
	kind  string // "scope" | "fork" | "summ-from" | "summ-upto"
	scope control.RewindScope
}{
	{"scope", control.RewindBoth},
	{"scope", control.RewindConversation},
	{"scope", control.RewindCode},
	{"fork", 0},
	{"summ-from", 0},
	{"summ-upto", 0},
}

// openRewind 从会话的检查点列表中填充回退选择器，默认选中最新的 turn。
// 当没有可回退的内容时，显示通知并返回（不打开选择器）。
func (m *chatTUI) openRewind() {
	metas := m.ctrl.Checkpoints()
	if len(metas) == 0 {
		m.notice(i18n.M.RewindNone)
		return
	}
	m.rewind = &rewindPicker{metas: metas, sel: len(metas) - 1}
}

// handleRewindKey 处理回退选择器中的按键事件。阶段 0 用 ↑/↓/Enter 选择 turn；
// 阶段 1 用 ↑/↓/Enter 或快捷键（b/c/d/f/s/u）选择恢复操作，Esc 返回上一阶段或关闭。
func (m chatTUI) handleRewindKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	r := m.rewind
	switch msg.String() {
	case "esc":
		if r.stage == 1 {
			r.stage = 0
		} else {
			m.rewind = nil
		}
	case "up", "k":
		if r.stage == 0 {
			if r.sel > 0 {
				r.sel--
			}
		} else if r.scope > 0 {
			r.scope--
		}
	case "down", "j":
		if r.stage == 0 {
			if r.sel < len(r.metas)-1 {
				r.sel++
			}
		} else if r.scope < len(rewindActions)-1 {
			r.scope++
		}
	case "enter":
		if r.stage == 0 {
			r.stage = 1
		} else {
			return m.applyRewind()
		}
	case "b":
		if r.stage == 1 {
			r.scope = 0
			return m.applyRewind()
		}
	case "c":
		if r.stage == 1 {
			r.scope = 1
			return m.applyRewind()
		}
	case "d":
		if r.stage == 1 {
			r.scope = 2
			return m.applyRewind()
		}
	case "f":
		if r.stage == 1 {
			r.scope = 3
			return m.applyRewind()
		}
	case "s":
		if r.stage == 1 {
			r.scope = 4
			return m.applyRewind()
		}
	case "u":
		if r.stage == 1 {
			r.scope = 5
			return m.applyRewind()
		}
	}
	return m, nil
}

// applyRewind 执行回退选择器的确认操作，根据选择的操作类型调用控制器的
// Fork、SummarizeFrom、SummarizeUpTo 或 Rewind 方法。
// 对话/代码恢复后会将该 turn 的提示词预填到输入框中，方便用户重新发送或编辑。
func (m chatTUI) applyRewind() (tea.Model, tea.Cmd) {
	r := m.rewind
	meta := r.metas[r.sel]
	act := rewindActions[r.scope]
	m.rewind = nil
	// The controller emits a notice for the outcome (success or failure) of each of
	// these, so the picker doesn't add its own — it would double on the CLI.
	switch act.kind {
	case "fork":
		if _, err := m.ctrl.Fork(meta.Turn); err == nil {
			m.replayActiveBranch(fmt.Sprintf("branched from turn %d", meta.Turn+1))
		}
		return m, nil // the branch is a new session
	case "summ-from":
		_ = m.ctrl.SummarizeFrom(context.Background(), meta.Turn)
		return m, nil
	case "summ-upto":
		_ = m.ctrl.SummarizeUpTo(context.Background(), meta.Turn)
		return m, nil
	}
	if err := m.ctrl.Rewind(meta.Turn, act.scope); err != nil {
		return m, nil
	}
	// The controller emits a notice marking the rewind point; the committed
	// transcript stays in terminal scrollback (v2 has no managed viewport), so for a
	// conversation/both rewind we prefill the composer with that turn's prompt to
	// re-send or edit — Claude Code's behavior — while the model's context is
	// truncated underneath.
	if act.scope != control.RewindCode && strings.TrimSpace(meta.Prompt) != "" {
		m.input.SetValue(meta.Prompt)
		m.growInputToFit()
	}
	return m, nil
}

// renderRewind 渲染回退选择器的界面。阶段 0 显示 turn 列表；
// 阶段 1 显示选定 turn 的恢复操作列表，使用 choicePanelStyle 样式化输出。
func (m chatTUI) renderRewind() string {
	r := m.rewind
	if r == nil {
		return ""
	}
	w := max(m.width, 10)
	var b strings.Builder
	if r.stage == 0 {
		b.WriteString(accent(i18n.M.RewindPickTitle) + "\n")
		for i, meta := range r.metas {
			b.WriteString(rowLine(i == r.sel, meta.Turn+1, "", turnLabel(meta, w), false) + "\n")
		}
		b.WriteString(dim(i18n.M.RewindPickHint))
		return choicePanelStyle.Width(w).Render(b.String())
	}
	meta := r.metas[r.sel]
	b.WriteString(accent(fmt.Sprintf(i18n.M.RewindRestoreTitleFmt, meta.Turn+1)) + dim(oneLine(meta.Prompt, 48)) + "\n")
	for i := range rewindActions {
		b.WriteString(rowLine(i == r.scope, i+1, "", rewindActionLabel(i), false) + "\n")
	}
	b.WriteString(dim(i18n.M.RewindApplyHint))
	return choicePanelStyle.Width(w).Render(b.String())
}

// rewindActionLabel 根据操作索引返回对应的本地化标签文本。
func rewindActionLabel(i int) string {
	switch i {
	case 0:
		return i18n.M.RewindCodeConversation
	case 1:
		return i18n.M.RewindConversationOnly
	case 2:
		return i18n.M.RewindCodeOnly
	case 3:
		return i18n.M.RewindFork
	case 4:
		return i18n.M.RewindSummarizeFrom
	case 5:
		return i18n.M.RewindSummarizeUpto
	default:
		return ""
	}
}

// turnLabel 生成 turn 列表中每行的标签文本，包含提示词预览和修改文件数。
func turnLabel(meta checkpoint.Meta, w int) string {
	label := oneLine(meta.Prompt, max(20, w-30))
	if n := len(meta.Paths); n > 0 {
		s := ""
		if n != 1 {
			s = "s"
		}
		label += dim(fmt.Sprintf("  (%d file%s)", n, s))
	}
	return label
}

// oneLine 将多行文本压缩为单行，并截断到显示宽度 n。
func oneLine(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if s == "" {
		return i18n.M.RewindEmpty
	}
	return ansi.Truncate(s, n, "…")
}
