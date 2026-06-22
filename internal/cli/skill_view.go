// skill_view.go 负责将技能列表、技能详情和技能路径渲染为终端友好的格式化文本，
// 供 /skills list、/skills show 和 /skills paths 命令在 TUI 滚动区域中展示。
package cli

import (
	"fmt"
	"strings"

	"reasonix/internal/skill"
)

// skillShowMaxLines 控制技能详情展示时的最大正文行数。
const skillShowMaxLines = 80

// renderSkillList 将技能列表渲染为格式化的终端文本，显示技能名称、作用域、
// 描述和状态标签（subagent、disabled），附带操作提示。
func renderSkillList(width int, skills []skill.Skill, disabled map[string]bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", viewHeader("skills (%d)", len(skills)))
	for _, s := range skills {
		name := "/" + s.Name
		scope := "(" + string(s.Scope) + ")"
		tag := ""
		if s.RunAs == skill.RunSubagent {
			tag = "  " + viewStatus("subagent")
		}
		if disabled[s.Name] {
			tag += "  " + viewMeta("disabled")
		}
		used := 2 + viewPadWidth(name, 18) + 1 + visibleWidth(scope) + 2 + visibleWidth(tag)
		desc := viewCompactText(s.Description, viewBudget(width, used))
		fmt.Fprintf(&b, "  %-18s %s  %s%s\n", name, viewMeta(scope), desc, tag)
	}
	b.WriteString(viewHint(viewCompactText("invoke: /<name> [args] · manage: /skills manage · author: /skills new <name>", viewBudget(width, 2))))
	return strings.TrimRight(b.String(), "\n")
}

// renderSkillShow 将单个技能的详情渲染为格式化的终端文本，
// 包括名称、作用域、状态、描述、路径和正文预览。
func renderSkillShow(width int, s skill.Skill, disabled bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", viewHeader("skill:"), viewCompactText(s.Name, viewBudget(width, 7)))
	if s.RunAs == skill.RunSubagent {
		fmt.Fprintf(&b, "  %s  %s\n", viewMeta(string(s.Scope)), viewStatus("subagent"))
	} else {
		fmt.Fprintf(&b, "  %s\n", viewMeta(string(s.Scope)))
	}
	if disabled {
		fmt.Fprintf(&b, "  %s\n", viewMeta("disabled"))
	}
	if strings.TrimSpace(s.Description) != "" {
		fmt.Fprintf(&b, "  %s\n", viewCompactText(s.Description, viewBudget(width, 2)))
	}
	if strings.TrimSpace(s.Path) != "" {
		fmt.Fprintf(&b, "  %s\n", viewMeta(viewCompactPath(s.Path, viewBudget(width, 2))))
	}
	body, extra := viewBodyPreview(s.Body, skillShowMaxLines)
	if strings.TrimSpace(body) != "" {
		b.WriteString("\n")
		b.WriteString(viewProtectLines(body, width))
	}
	if extra > 0 {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(viewMore(extra, "lines"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderSkillPaths 将技能搜索路径列表渲染为格式化的终端文本，
// 显示优先级、作用域、状态和路径，附带配置提示。
func renderSkillPaths(width int, roots []skill.Root) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", viewHeader("skill paths"))
	for _, r := range roots {
		leftWidth := 2 + 4 + 8 + 1 + 13 + 1
		scope := viewMeta(fmt.Sprintf("%-8s", string(r.Scope)))
		status := fmt.Sprintf("%-13s", string(r.Status))
		if r.Status == skill.StatusOK {
			status = viewStatus(status)
		} else {
			status = viewMeta(status)
		}
		fmt.Fprintf(&b, "  %2d. %s %s %s\n",
			r.Priority+1, scope, status, viewCompactPath(r.Dir, viewBudget(width, leftWidth)))
	}
	b.WriteString(viewHint(viewCompactText("priority: project > custom > global > builtin · configure [skills] paths in reasonix.toml", viewBudget(width, 2))))
	return strings.TrimRight(b.String(), "\n")
}
