// branch.go 实现了对话分支（branch）相关的 CLI 命令和渲染逻辑。
// 对话分支允许用户从任意历史回合创建新分支，实现多路径对话探索。
// 支持的命令包括：
//   - /tree: 显示分支树状结构
//   - /branch: 创建新分支（可从指定回合分叉）
//   - /switch: 切换到指定分支

package cli

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"reasonix/internal/agent"
	"reasonix/internal/control"
)

// showBranchTree 获取并显示当前会话的分支树状结构。
// 树中包含所有分支的 ID、标题和元信息，当前活跃分支会高亮标记。
func (m *chatTUI) showBranchTree() {
	branches, err := m.ctrl.Branches()
	if err != nil {
		m.notice("tree: " + err.Error())
		return
	}
	current := agent.BranchID(m.ctrl.SessionPath())
	tree := renderBranchTree(control.FormatBranchTree(branches, current))
	m.commitLine(ansi.Hardwrap(tree, max(m.width, 20), false))
}

// renderBranchTree 对分支树的每一行进行语法高亮渲染。
// 将树状结构的纯文本转换为带有颜色的终端输出。
func renderBranchTree(tree string) string {
	lines := strings.Split(tree, "\n")
	for i, line := range lines {
		lines[i] = renderBranchTreeLine(line)
	}
	return strings.Join(lines, "\n")
}

// renderBranchTreeLine 渲染分支树中的单行文本。
// 对树状结构符号（├─、└─）使用暗色显示，分支 ID 使用暗色，
// 标题使用默认颜色，"current" 标记使用强调色高亮。
func renderBranchTreeLine(line string) string {
	if line == "branches:" {
		return accent(line)
	}
	joint := strings.LastIndex(line, "├─ ")
	if alt := strings.LastIndex(line, "└─ "); alt > joint {
		joint = alt
	}
	if joint < 0 {
		return line
	}
	treePrefix := line[:joint+len("├─ ")]
	parts := strings.SplitN(line[joint+len("├─ "):], "  ", 3)
	if len(parts) < 3 {
		return line
	}
	id, title, meta := parts[0], parts[1], parts[2]

	turns := meta
	current := ""
	if before, after, ok := strings.Cut(meta, "  "); ok {
		turns = before
		if strings.TrimSpace(after) == "current" {
			current = "  " + accent("current")
		} else if strings.TrimSpace(after) != "" {
			current = "  " + after
		}
	}
	return dim(treePrefix) + dim(id) + "  " + title + "  " + dim(turns) + current
}

// runBranchCommand 处理 /branch 斜杠命令。
// 支持两种用法：
//   - /branch: 从当前分支末尾创建新分支
//   - /branch N [name]: 从显示的第 N 个回合创建新分支（可选命名）
func (m *chatTUI) runBranchCommand(input string) {
	cmd := strings.Fields(input)[0]
	args := strings.TrimSpace(strings.TrimPrefix(input, cmd))

	// /branch 3 optional-name branches from displayed turn 3. Plain /branch
	// branches from the current tip.
	if n, name, fromTurn, err := control.ParseBranchTarget(args); err != nil {
		m.notice(err.Error())
		return
	} else if fromTurn {
		if _, err := m.ctrl.ForkNamed(n-1, name); err != nil {
			return
		}
		m.replayActiveBranch(fmt.Sprintf("branched from turn %d", n))
		return
	} else {
		if _, err := m.ctrl.Branch(name); err != nil {
			return
		}
	}
	m.showBranchTree()
}

// runSwitchCommand 处理 /switch 斜杠命令，切换到指定的分支。
// 参数可以是分支 ID 或分支名称。
// 切换成功后会重新播放目标分支的对话历史。
func (m *chatTUI) runSwitchCommand(input string) {
	ref := strings.TrimSpace(strings.TrimPrefix(input, strings.Fields(input)[0]))
	if ref == "" {
		m.notice("usage: /switch <branch id|name>")
		return
	}
	if _, err := m.ctrl.SwitchBranch(ref); err != nil {
		return
	}
	m.replayActiveBranch("switched branch")
}

// replayActiveBranch 重新渲染当前活跃分支的完整对话历史。
// 先清理当前界面状态（待处理消息、推理内容、选择器等），
// 然后清空转录记录，最后重新渲染横幅和历史对话。
// title 参数显示在分隔线上方（如 "branched from turn 3"）。
func (m *chatTUI) replayActiveBranch(title string) {
	m.finalizeStreamed()
	m.pending.Reset()
	m.reasoning.Reset()
	m.todoArgs = ""
	m.chooser = nil
	m.pendingApproval = nil
	m.bubblePending = false
	m.turnDiscarded = false

	// 清空旧会话的转录记录，确保视口仅显示新加载的会话内容。
	// 否则转录会在每次 /resume、/switch、/rewind、/branch 操作中累积，
	// 导致内存膨胀和滚动位置偏移问题（参见 #4584）。
	m.transcript = nil
	m.transcriptDirty = true
	m.forceGotoBottom = true

	m.commitLine("")
	if title != "" {
		m.commitLine(dim("  -- " + title + " --"))
	}
	contentW := transcriptContentWidth(m.width, m.nativeScrollback)
	m.commitLine(strings.TrimRight(renderTUIBanner(m.label, "", contentW), "\n"))
	for _, section := range replaySectionsFor(m.ctrl.History(), contentW, m.renderer) {
		m.commitLine(strings.TrimRight(section, "\n"))
	}
}
