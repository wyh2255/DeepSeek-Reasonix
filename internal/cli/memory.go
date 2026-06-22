// memory.go 实现了 TUI 中的 /memory 和 /forget 子命令。
// /memory 展示当前加载的记忆文档和自动记忆存储路径，
// /forget 按名称删除已保存的自动记忆条目。
package cli

import (
	"fmt"

	"reasonix/internal/i18n"
)

// showMemory 报告当前加载了哪些记忆以及它们的存储位置，是 Claude Code /memory 命令的
// TUI 对应实现。它展示文档文件和自动记忆存储路径，方便用户直接打开编辑，
// 因为终端内 UI 不会调用外部编辑器。
func (m *chatTUI) showMemory() {
	set := m.ctrl.Memory()
	if set == nil || (set.Empty() && len(set.Store.ListArchived()) == 0) {
		m.notice(i18n.M.MemoryNone)
		return
	}
	m.commitLine(renderMemory(m.width, set))
}

// forgetMemory 按名称（即 /memory 中显示的 slug）删除已保存的自动记忆。
// 它是模型 `forget` 工具的手动对应操作。
func (m *chatTUI) forgetMemory(name string) {
	if name == "" {
		m.notice(i18n.M.ForgetUsage)
		return
	}
	if err := m.ctrl.ForgetMemory(name); err != nil {
		m.notice(fmt.Sprintf("forget: %v", err))
		return
	}
	m.notice(fmt.Sprintf(i18n.M.ForgetDoneFmt, name))
}
