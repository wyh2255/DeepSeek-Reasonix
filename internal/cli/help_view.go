// help_view.go 实现了 /help 命令的帮助信息渲染。
// 该文件负责：
//   - 渲染内置命令列表（如 /compact、/new、/clear 等）
//   - 渲染自定义命令、技能（skills）和 MCP prompts 的帮助信息
//   - 对超出限制数量的动态条目进行截断并显示"更多"提示
package cli

import (
	"fmt"
	"strings"

	"reasonix/internal/command"
	"reasonix/internal/i18n"
	"reasonix/internal/plugin"
	"reasonix/internal/skill"
)

// helpMaxDynamicItems 是自定义命令、技能和 prompts 在帮助视图中显示的最大数量
const helpMaxDynamicItems = 8

// showHelp 在聊天界面中显示帮助信息，包含所有可用的命令、技能和 prompts
func (m *chatTUI) showHelp() {
	m.commitLine(renderHelp(m.width, m.commands, m.skills, m.prompts()))
}

// renderHelp 渲染完整的帮助视图，按分类显示内置命令、自定义命令、技能和 MCP prompts。
// width 为终端宽度，用于控制文本换行和截断。
func renderHelp(width int, commands []command.Command, skills []skill.Skill, prompts []plugin.Prompt) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", viewHeader("commands"))
	writeHelpItems(&b, width, "built-in", builtinHelpItems(), 0)
	if len(commands) > 0 {
		writeHelpItems(&b, width, "custom", customHelpItems(commands), helpMaxDynamicItems)
	}
	if len(skills) > 0 {
		writeHelpItems(&b, width, "skills", skillHelpItems(skills), helpMaxDynamicItems)
	}
	if len(prompts) > 0 {
		writeHelpItems(&b, width, "MCP prompts", promptHelpItems(prompts), helpMaxDynamicItems)
	}
	b.WriteString(viewHint("type a command, or press Tab after / for completion"))
	return strings.TrimRight(b.String(), "\n")
}

// writeHelpItems 将一组帮助条目写入字符串构建器。
// limit > 0 时限制显示数量，超出部分显示"更多 N items"提示。
func writeHelpItems(b *strings.Builder, width int, title string, items []compItem, limit int) {
	if len(items) == 0 {
		return
	}
	b.WriteString(viewSubhead(title) + "\n")
	n := len(items)
	if limit > 0 && n > limit {
		n = limit
	}
	for _, item := range items[:n] {
		name := item.label
		used := 2 + viewPadWidth(name, 18) + 1
		hint := viewCompactText(item.hint, viewBudget(width, used))
		fmt.Fprintf(b, "  %-18s %s\n", name, viewMeta(hint))
	}
	if extra := len(items) - n; extra > 0 {
		b.WriteString(viewMore(extra, "items") + "\n")
	}
}

// builtinHelpItems 返回所有内置命令的帮助条目列表。
// 每个条目包含命令名和对应的国际化描述文本。
func builtinHelpItems() []compItem {
	return []compItem{
		{label: "/compact", hint: i18n.M.CmdCompact},
		{label: "/new", hint: i18n.M.CmdNew},
		{label: "/rename", hint: i18n.M.CmdRename},
		{label: "/clear", hint: i18n.M.CmdClear},
		{label: "/rewind", hint: i18n.M.CmdRewind},
		{label: "/tree", hint: i18n.M.CmdTree},
		{label: "/branch", hint: i18n.M.CmdBranch},
		{label: "/switch", hint: i18n.M.CmdSwitchBranch},
		{label: "/todo", hint: i18n.M.CmdTodo},
		{label: "/model", hint: i18n.M.CmdModel},
		{label: "/provider", hint: i18n.M.CmdProvider},
		{label: "/mcp", hint: i18n.M.CmdMcp},
		{label: "/skills", hint: i18n.M.CmdSkill},
		{label: "/hooks", hint: i18n.M.CmdHooks},
		{label: "/memory", hint: i18n.M.CmdMemory},
		{label: "/migrate", hint: i18n.M.CmdMigrate},
		{label: "/output-style", hint: i18n.M.CmdOutputStyle},
		{label: "/diff-fold", hint: i18n.M.CmdDiffFold},
		{label: "/sandbox", hint: i18n.M.CmdSandbox},
		{label: "/verbose", hint: i18n.M.CmdVerbose},
		{label: "/language", hint: i18n.M.CmdLanguage},
		{label: "/auto-plan", hint: i18n.M.CmdAutoPlan},
		{label: "/reasoning-language", hint: i18n.M.CmdReasonLang},
		{label: "/reload-cmd", hint: i18n.M.CmdReloadCmd},
		{label: "/help", hint: i18n.M.CmdHelp},
	}
}

// customHelpItems 将用户自定义命令转换为帮助条目列表。
func customHelpItems(commands []command.Command) []compItem {
	items := make([]compItem, 0, len(commands))
	for _, c := range commands {
		items = append(items, compItem{label: "/" + c.Name, hint: c.Description})
	}
	return items
}

// skillHelpItems 将技能列表转换为帮助条目，子代理类型的技能会添加 "subagent" 前缀标记。
func skillHelpItems(skills []skill.Skill) []compItem {
	items := make([]compItem, 0, len(skills))
	for _, s := range skills {
		hint := s.Description
		if s.RunAs == skill.RunSubagent {
			hint = "subagent · " + hint
		}
		items = append(items, compItem{label: "/" + s.Name, hint: hint})
	}
	return items
}

// promptHelpItems 将 MCP prompts 列表转换为帮助条目列表。
func promptHelpItems(prompts []plugin.Prompt) []compItem {
	items := make([]compItem, 0, len(prompts))
	for _, p := range prompts {
		items = append(items, compItem{label: "/" + p.Name, hint: p.Description})
	}
	return items
}
