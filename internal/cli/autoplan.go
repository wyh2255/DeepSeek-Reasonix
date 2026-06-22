// autoplan.go 实现了 /auto-plan 斜杠命令的处理逻辑。
// 该命令用于切换自动规划模式的开启/关闭状态。
// 自动规划模式控制 AI 在回答问题前是否先制定执行计划。
// 配置会持久化到用户配置文件中，以便跨会话保持设置。

package cli

import (
	"fmt"
	"strings"

	"reasonix/internal/config"
)

// runAutoPlanCommand 处理 /auto-plan 命令的执行。
// 参数 input 是完整的命令输入字符串（如 "/auto-plan on"）。
// 当无参数时显示当前模式；当有参数时切换模式并持久化到配置文件。
// 在 AI 正在运行时不允许切换模式。
func (m *chatTUI) runAutoPlanCommand(input string) {
	args := tokenizeArgs(input)
	if len(args) < 2 {
		cfg, err := config.Load()
		if err != nil {
			m.notice("auto-plan: " + err.Error())
			return
		}
		m.notice(fmt.Sprintf("auto-plan: %s (usage: /auto-plan off|on)", cliAutoPlanMode(cfg.Agent.AutoPlan)))
		return
	}
	if len(args) > 2 {
		m.notice("usage: /auto-plan off|on")
		return
	}
	if m.ctrl != nil && m.ctrl.Running() {
		m.notice("finish or cancel the current turn before changing auto-plan")
		return
	}

	path := config.UserConfigPath()
	if path == "" {
		m.notice("auto-plan: cannot resolve config path")
		return
	}
	edit := config.LoadForEdit(path)
	if err := edit.SetAutoPlan(args[1]); err != nil {
		m.notice("auto-plan: " + err.Error())
		return
	}
	if err := edit.SaveTo(path); err != nil {
		m.notice("auto-plan: " + err.Error())
		return
	}

	mode := edit.Agent.AutoPlan
	if m.ctrl != nil {
		m.ctrl.SetAutoPlan(mode)
	}
	m.notice(fmt.Sprintf("auto-plan set to %s", mode))
}

// cliAutoPlanMode 将自动规划模式值标准化为 "on" 或 "off"。
// 将 "on" 和 "ask" 视为开启状态，其他值（包括空字符串）视为关闭。
func cliAutoPlanMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "on", "ask":
		return "on"
	default:
		return "off"
	}
}
