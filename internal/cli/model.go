// model.go 实现了 TUI 中的 /model 子命令，用于列出已配置的模型引用、
// 在当前会话中切换模型，以及将用户选择持久化到配置文件。
// 同时提供了模型引用和 Provider 名称的自动补全数据源。
package cli

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"reasonix/internal/config"
	"reasonix/internal/i18n"
)

// runModelSubcommand 处理 "/model" 命令：无参数时列出所有已配置的 (provider, model) 引用
// 并标记当前活跃模型；"/model <ref>" 将当前会话切换到指定模型，同时保留对话历史。
// 控制器的实际构建是异步进行的，不会阻塞 TUI 事件循环。
func (m *chatTUI) runModelSubcommand(input string) {
	args := tokenizeArgs(input) // args[0] == "/model"
	if len(args) < 2 {
		m.showModels()
		return
	}
	ref := args[1]
	if m.buildController == nil {
		m.notice(i18n.M.ModelSwitchUnavailable)
		return
	}
	if m.ctrl.Running() {
		m.notice(i18n.M.ModelSwitchBusy)
		return
	}
	if ref == m.modelRef {
		m.notice(fmt.Sprintf(i18n.M.ModelAlreadyOnFmt, ref))
		return
	}
	// Persist the user's choice to the user config.toml so the next
	// session starts on the same model instead of falling back to the global
	// default. Mirrors the pattern used by /theme (persistTheme), /effort, and
	// /language.
	m.persistModel(ref)
	carried := m.ctrl.History()
	prevPath := m.ctrl.SessionPath()
	if err := m.ctrl.Snapshot(); err != nil {
		m.notice("model: snapshot failed: " + err.Error())
	}
	m.notice(fmt.Sprintf(i18n.M.ModelSwitchingFmt, ref))

	// Capture old controller for cleanup after the async build succeeds.
	oldCtrl := m.ctrl
	build := m.buildController

	// Fire the build off the event loop; the result arrives as a tea.Cmd.
	// Both the build AND the old-controller close run in the goroutine so
	// neither blocks the bubbletea event loop. The old controller's Close
	// kills plugin subprocesses (incl. CodeGraph), which can disrupt the
	// terminal's cancelReader if called synchronously inside Update — so it
	// must happen here, before we hand the new controller back.
	m.modelSwitchPending = true
	m.pendingModelSwitch = func() tea.Msg {
		c, err := build(ref, carried, prevPath)
		if err != nil {
			return modelSwitchMsg{ref: ref, err: err}
		}
		// Do NOT close the old controller here. Controller.Close() runs
		// SessionEnd hooks (arbitrary shell commands) and kills plugin
		// subprocesses — operations that corrupt bubbletea's terminal raw
		// mode when executed from a goroutine. Instead, pass the old
		// controller back in the message so the Update handler can defer
		// its cleanup as a tea.Cmd that runs after the next render.
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
}

// showModels 列出所有已配置的 provider/model 引用，并标记当前活跃的模型。
func (m *chatTUI) showModels() {
	cfg, err := config.Load()
	if err != nil {
		m.notice("model: " + err.Error())
		return
	}
	var refs []string
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !p.Configured() {
			continue
		}
		for _, model := range p.ChatModelList() {
			refs = append(refs, p.Name+"/"+model)
		}
	}
	m.commitLine(renderModels(m.width, refs, m.modelRef))
}

// persistModel 将 ref（格式为 "provider/model" 字符串）写入用户配置文件的 default_model 字段，
// 使下次 CLI 启动时能从同一模型开始。内存中的切换不受持久化结果影响，
// 但每个步骤（验证拒绝、保存失败或持久化成功）都会通过 TUI 通知通道报告，
// 让用户知道他们的 /model 选择是否能在重启后保留。
// 在 Snapshot/ModelSwitchingFmt 之前运行，以便持久化结果首先显示在通知区域。
func (m *chatTUI) persistModel(ref string) {
	path := config.UserConfigPath()
	if path == "" {
		return
	}
	edit := config.LoadForEdit(path)
	if err := edit.SetDefaultModel(ref); err != nil {
		m.notice(fmt.Sprintf("model: persist refused: %v (ref=%s)", err, ref))
		return
	}
	if err := edit.SaveTo(path); err != nil {
		m.notice(fmt.Sprintf("model: persist save failed: %v (ref=%s, path=%s)", err, ref, path))
		return
	}
	m.notice(fmt.Sprintf("model: persisted (ref=%s, path=%s)", ref, path))
}

// modelRefs 返回所有已配置的 provider/model 引用列表，用于斜杠命令自动补全。
func modelRefs() []string {
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	var out []string
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !p.Configured() {
			continue
		}
		for _, model := range p.ChatModelList() {
			out = append(out, p.Name+"/"+model)
		}
	}
	return out
}

// providerNames 返回已配置的 Provider 名称列表，用于斜杠命令自动补全。
func providerNames() []string {
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	var out []string
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !p.Configured() {
			continue
		}
		out = append(out, p.Name)
	}
	return out
}
