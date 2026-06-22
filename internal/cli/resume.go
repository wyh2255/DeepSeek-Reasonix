// resume.go 实现了 TUI 中的 /resume 子命令，用于列出最近保存的会话并恢复指定会话。
// 同时提供了会话序号的自动补全数据源和会话摘要生成函数。
package cli

import (
	"fmt"
	"strconv"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/i18n"
)

// resumeListCap 限制 /resume 列表中显示的最大会话数量，
// 确保列表中的 1-based 索引与 /resume <n> 命令和自动补全一致。
const resumeListCap = 10

// recentSessions 返回 dir 目录下最新的保存会话列表（已截断到 resumeListCap）。
// 目录不存在或读取错误时返回空列表。
func recentSessions(dir string) []agent.SessionInfo {
	if dir == "" {
		return nil
	}
	sessions, err := agent.ListSessions(dir)
	if err != nil {
		return nil
	}
	if len(sessions) > resumeListCap {
		sessions = sessions[:resumeListCap]
	}
	return sessions
}

// runResumeCommand 处理 "/resume" 命令：无参数时列出最近保存的会话（最新在前，
// 当前活跃的标记高亮）；"/resume <n>" 将第 n 个会话加载到当前控制器中，
// 保留当前模型并将对话记录回放到滚动区域。
func (m *chatTUI) runResumeCommand(input string) {
	sessions := recentSessions(m.ctrl.SessionDir())
	if len(sessions) == 0 {
		m.notice(i18n.M.NoSessionToResume)
		return
	}

	args := tokenizeArgs(input) // args[0] == "/resume"
	if len(args) < 2 {
		m.showSessions(sessions) // write list to scrollback (above input)
		m.openResumePicker()     // open interactive picker below
		return
	}
	if m.ctrl.Running() {
		m.notice(i18n.M.ResumeBusy)
		return
	}
	idx, err := strconv.Atoi(strings.TrimSpace(args[1]))
	if err != nil || idx < 1 || idx > len(sessions) {
		m.notice(fmt.Sprintf(i18n.M.ResumeBadIndexFmt, len(sessions)))
		return
	}
	target := sessions[idx-1]
	if target.Path == m.ctrl.SessionPath() {
		m.notice(i18n.M.ResumeAlreadyActive)
		return
	}
	loaded, err := agent.LoadSession(target.Path)
	if err != nil {
		m.notice("resume: " + err.Error())
		return
	}
	// Persist the conversation we're leaving so switching back later restores it.
	_ = m.ctrl.Snapshot()
	m.ctrl.Resume(loaded, target.Path)
	m.replayActiveBranch(i18n.M.ResumedTitle)
}

// showSessions 将最近会话列表渲染为带 1-based 索引、时间戳、轮次数和预览的文本，
// 并标记当前活跃的会话。
func (m *chatTUI) showSessions(sessions []agent.SessionInfo) {
	active := m.ctrl.SessionPath()
	var b strings.Builder
	b.WriteString(dim("  · " + i18n.M.ResumeListHeader + "\n"))
	for i, s := range sessions {
		marker := "  "
		if s.Path == active {
			marker = accent("› ")
		}
		fmt.Fprintf(&b, "%s%d  %s  %s\n", marker, i+1,
			s.ModTime.Local().Format("01-02 15:04"), dim(sessionSummary(s)))
	}
	m.notice(strings.TrimRight(b.String(), "\n"))
}

// resumeArgItems 为 "/resume <n>" 命令提供索引参数的自动补全。
// 输入命令词后，列出最近会话的 1-based 索引，
// 以"时间戳 + 轮次数 + 预览"作为提示信息。索引与 showSessions 一致，
// 因为两者都通过 recentSessions 获取数据。
func (m *chatTUI) resumeArgItems(val string) ([]compItem, int, bool) {
	cmdEnd := strings.IndexAny(val, " \t")
	if cmdEnd < 0 || val[:cmdEnd] != "/resume" {
		return nil, 0, false
	}
	from := strings.LastIndexAny(val, " \t") + 1
	if len(strings.Fields(val[:from])) != 1 || m.ctrl == nil {
		return nil, from, true
	}
	cur := val[from:]
	var out []compItem
	for i, s := range recentSessions(m.ctrl.SessionDir()) {
		idx := strconv.Itoa(i + 1)
		if cur != "" && !strings.HasPrefix(idx, cur) {
			continue
		}
		hint := fmt.Sprintf("%s · %s", s.ModTime.Local().Format("01-02 15:04"), sessionSummary(s))
		out = append(out, compItem{label: idx, insert: idx, hint: hint})
	}
	return out, from, true
}

// sessionSummary 生成会话摘要行（"N turns · topicTitle/first message"），
// 供 /resume 列表和参数补全共用。当设置了 TopicTitle（通过 /rename 或桌面端）时，
// 优先显示标题而非原始预览，方便用户快速识别会话。
func sessionSummary(s agent.SessionInfo) string {
	preview := s.Preview
	if s.TopicTitle != "" {
		preview = s.TopicTitle
	}
	if preview == "" {
		preview = "(no user message yet)"
	}
	return fmt.Sprintf("%d turns · %s", s.Turns, preview)
}
