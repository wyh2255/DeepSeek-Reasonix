// effort.go 实现了 /effort 斜杠命令的处理逻辑。
// 该命令用于设置和查看当前模型的推理努力级别（effort level）。
// 不同模型支持不同的努力级别（如 low/medium/high），设置后会：
//   - 持久化到用户配置文件
//   - 自动重建控制器以应用新设置
//   - 对 Anthropic 模型，自动启用 thinking 模式（如果未设置）

package cli

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"reasonix/internal/config"
)

// runEffortCommand 处理 /effort 斜杠命令。
// 无参数时显示当前模型的努力级别和支持的选项。
// 有参数时验证并设置新的努力级别，然后重建控制器以应用更改。
// 在 AI 正在运行时不允许更改。
func (m *chatTUI) runEffortCommand(input string) tea.Cmd {
	entry, ref, err := m.currentConfigProvider()
	if err != nil {
		m.notice("effort: " + err.Error())
		return nil
	}
	cap := config.EffortCapabilityForEntry(entry)
	if !cap.Supported {
		m.notice(fmt.Sprintf("effort is not configurable for %s", entry.Name))
		return nil
	}

	args := tokenizeArgs(input)
	if len(args) < 2 {
		current := config.EffortDisplay(entry)
		options := strings.Join(cap.Levels, "|")
		m.notice(fmt.Sprintf("effort for %s: %s (default: %s; options: %s)", entry.Name, current, cap.Default, options))
		return nil
	}
	if len(args) > 2 {
		m.notice("usage: /effort " + strings.Join(cap.Levels, "|"))
		return nil
	}
	effort, err := config.NormalizeEffort(entry, args[1])
	if err != nil {
		m.notice(err.Error())
		return nil
	}
	if m.buildController == nil {
		m.notice("model switching is unavailable in this session")
		return nil
	}
	if m.ctrl.Running() {
		m.notice("finish or cancel the current turn before changing effort")
		return nil
	}

	path := config.UserConfigPath()
	if path == "" {
		m.notice("effort: cannot resolve user config directory")
		return nil
	}
	edit := config.LoadForEdit(path)
	if _, ok := edit.Provider(entry.Name); !ok {
		if err := edit.UpsertProvider(*entry); err != nil {
			m.notice("effort: " + err.Error())
			return nil
		}
	}
	// Anthropic 模型设置非空 effort 时，自动启用 adaptive thinking 模式
	if entry.Kind == "anthropic" && effort != "" && entry.Thinking == "" {
		if err := edit.SetProviderThinking(entry.Name, "adaptive"); err != nil {
			m.notice("effort: " + err.Error())
			return nil
		}
	}
	if err := edit.SetProviderEffort(entry.Name, effort); err != nil {
		m.notice("effort: " + err.Error())
		return nil
	}
	if err := edit.SaveTo(path); err != nil {
		m.notice("effort: " + err.Error())
		return nil
	}

	display := effort
	if display == "" {
		display = "auto"
	}
	m.notice(fmt.Sprintf("setting effort for %s to %s…", entry.Name, display))
	carried := m.ctrl.History()
	prevPath := m.ctrl.SessionPath()
	if err := m.ctrl.Snapshot(); err != nil {
		m.notice("effort: snapshot: " + err.Error())
	}
	oldCtrl := m.ctrl
	build := m.buildController
	// 标记待执行的模型切换，并构建异步切换函数
	m.modelSwitchPending = true
	m.pendingModelSwitch = func() tea.Msg {
		c, err := build(ref, carried, prevPath)
		if err != nil {
			return modelSwitchMsg{ref: ref, err: err}
		}
		return modelSwitchMsg{
			ref:      ref,
			ctrl:     c,
			oldCtrl:  oldCtrl,
			label:    c.Label(),
			commands: c.Commands(),
			skills:   c.Skills(),
			host:     c.Host(),
		}
	}
	m.notice(fmt.Sprintf("effort for %s set to %s", entry.Name, display))
	return m.pendingModelSwitch
}

// currentConfigProvider 获取当前模型对应的配置提供商条目和完整的模型引用。
// 如果当前未设置模型引用，则使用配置中的默认模型。
// 返回的 ref 格式为 "provider/model"。
func (m *chatTUI) currentConfigProvider() (*config.ProviderEntry, string, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, "", err
	}
	ref := m.modelRef
	if strings.TrimSpace(ref) == "" {
		ref = cfg.DefaultModel
	}
	entry, ok := cfg.ResolveModel(ref)
	if !ok {
		return nil, "", fmt.Errorf("unknown model %q", ref)
	}
	if ref == entry.Name || !strings.Contains(ref, "/") {
		ref = entry.Name + "/" + entry.Model
	}
	return entry, ref, nil
}

// refreshEffortStatus 刷新状态栏中显示的努力级别信息。
// 如果当前模型不支持 effort 配置，则清空显示。
func (m *chatTUI) refreshEffortStatus() {
	m.effortLevel = ""
	entry, _, err := m.currentConfigProvider()
	if err != nil {
		return
	}
	if !config.EffortCapabilityForEntry(entry).Supported {
		return
	}
	m.effortLevel = config.EffortDisplay(entry)
}
