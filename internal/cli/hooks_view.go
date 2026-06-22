// hooks_view.go 实现了 /hooks 命令的视图渲染。
// 该文件负责：
//   - 渲染当前活跃的 hooks 列表，包括事件类型、作用域、匹配模式和命令
//   - 显示项目 hooks 的信任状态和配置文件路径提示
package cli

import (
	"fmt"
	"strings"

	"reasonix/internal/hook"
)

// renderHooks 渲染 hooks 管理视图，显示所有活跃的 hooks 及其配置信息。
// width 为终端宽度，hooks 为已解析的 hook 列表，trusted 表示项目 hooks 是否已受信任，
// projectDefines 表示项目是否定义了自己的 hooks。
func renderHooks(width int, hooks []hook.ResolvedHook, trusted bool, projectDefines bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", viewHeader("hooks (%d active)", len(hooks)))
	for _, h := range hooks {
		match := h.Match
		if h.Event == hook.PreToolUse || h.Event == hook.PostToolUse || h.Event == hook.PermissionRequest {
			if match == "" {
				match = "*"
			}
		} else {
			match = "-"
		}
		used := 2 + viewPadWidth(string(h.Event), 16) + 1 + 8 + 1 + 8 + 1
		fmt.Fprintf(&b, "  %-16s %s %s %s\n",
			h.Event, viewMeta(fmt.Sprintf("%-8s", h.Scope)), viewMeta(fmt.Sprintf("%-8s", match)), viewCompactText(h.Command, viewBudget(width, used)))
	}
	b.WriteByte('\n')
	switch {
	case projectDefines && !trusted:
		b.WriteString(viewHint(viewCompactText("project hooks are not trusted; run /hooks trust to enable shell-command hooks", viewBudget(width, 2))))
	case trusted:
		b.WriteString(viewHint(viewCompactText("project trusted · config: project .reasonix/settings.json + global ~/.reasonix/settings.json", viewBudget(width, 2))))
	default:
		b.WriteString(viewHint(viewCompactText("project not trusted · config: project .reasonix/settings.json + global ~/.reasonix/settings.json", viewBudget(width, 2))))
	}
	return strings.TrimRight(b.String(), "\n")
}
