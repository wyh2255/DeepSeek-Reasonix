// model_view.go 负责将模型列表渲染为终端友好的格式化文本，
// 供 /model 命令在 TUI 滚动区域中展示。
package cli

import (
	"fmt"
	"strings"

	"reasonix/internal/i18n"
)

// renderModels 将模型引用列表渲染为格式化的终端文本，标记当前活跃模型，
// 并附带切换提示。width 参数用于控制输出宽度。
func renderModels(width int, refs []string, active string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", viewHeader("%s", i18n.M.ModelListHeader))
	for _, ref := range refs {
		status := ""
		if ref == active {
			status = "  " + viewStatus("active")
		}
		fmt.Fprintf(&b, "  %s%s\n", viewCompactText(ref, viewBudget(width, 2+visibleWidth(status))), status)
	}
	b.WriteString(viewHint(viewCompactText("switch with /model <provider/model>", viewBudget(width, 2))))
	return strings.TrimRight(b.String(), "\n")
}
