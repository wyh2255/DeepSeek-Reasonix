// reasoning_language.go 实现了 TUI 中的 /reasoning-language 子命令，
// 用于查看和设置推理语言模式（auto/zh/en），
// 并将用户选择持久化到配置文件。
package cli

import (
	"fmt"
	"strings"

	"reasonix/internal/config"
)

// runReasoningLanguageCommand 处理 "/reasoning-language" 命令。
// 无参数时显示当前推理语言模式；带参数时解析并持久化设置，
// 同时在运行中的控制器上生效。支持 auto、zh、en 三种模式。
func (m *chatTUI) runReasoningLanguageCommand(input string) {
	args := tokenizeArgs(input)
	if len(args) < 2 {
		cfg, err := config.Load()
		if err != nil {
			m.notice("reasoning-language: " + err.Error())
			return
		}
		m.notice(fmt.Sprintf("reasoning-language: %s (usage: /reasoning-language auto|zh|en)", cliReasoningLanguageMode(cfg.ReasoningLanguage())))
		return
	}
	if len(args) > 2 {
		m.notice("usage: /reasoning-language auto|zh|en")
		return
	}
	if m.ctrl != nil && m.ctrl.Running() {
		m.notice("finish or cancel the current turn before changing reasoning-language")
		return
	}
	mode, err := parseCLIReasoningLanguage(args[1])
	if err != nil {
		m.notice("reasoning-language: " + err.Error())
		return
	}

	path := config.UserConfigPath()
	if path == "" {
		m.notice("reasoning-language: cannot resolve config path")
		return
	}
	edit := config.LoadForEdit(path)
	if err := edit.SetReasoningLanguage(mode); err != nil {
		m.notice("reasoning-language: " + err.Error())
		return
	}
	if err := edit.SaveTo(path); err != nil {
		m.notice("reasoning-language: " + err.Error())
		return
	}

	mode = edit.ReasoningLanguage()
	if m.ctrl != nil {
		m.ctrl.SetReasoningLanguage(mode)
	}
	m.notice(fmt.Sprintf("reasoning-language set to %s", mode))
}

// parseCLIReasoningLanguage 解析用户输入的推理语言模式字符串，
// 返回规范化的模式名称。仅接受 auto、zh、en 三种值。
func parseCLIReasoningLanguage(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "auto":
		return "auto", nil
	case "zh":
		return "zh", nil
	case "en":
		return "en", nil
	default:
		return "", fmt.Errorf("reasoning_language %q: must be auto|zh|en", mode)
	}
}

// cliReasoningLanguageMode 将配置中的推理语言模式规范化为标准值（auto/zh/en），
// 未知值默认返回 "auto"。
func cliReasoningLanguageMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "zh":
		return "zh"
	case "en":
		return "en"
	default:
		return "auto"
	}
}
