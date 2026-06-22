// mcp_manager_actions.go 实现了 /mcp 管理器中所有可执行操作的具体逻辑。
// 该文件负责：
//   - 连接/重连 MCP 服务器
//   - 禁用/移除 MCP 服务器
//   - 切换连接模式（background/eager）
//   - 打开配置文件编辑器
//   - 进行 OAuth 认证和清除认证信息
//   - 构建编辑器启动命令（支持 VISUAL/EDITOR 环境变量和系统默认编辑器）
//   - 跨平台打开 URL/文件（macOS/Linux/Windows）
package cli

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"

	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/mcpdiag"
	"reasonix/internal/plugin"
)

// applyMCPAction 根据操作类型分发执行对应的 MCP 管理操作。
// 支持的操作包括：查看工具、切换模式、编辑配置、认证、清除认证、连接、查看日志、禁用和移除。
func (m chatTUI) applyMCPAction(v mcpServerView, action mcpAction) (tea.Model, tea.Cmd) {
	switch action {
	case mcpActionViewTools:
		m.mcp.stage = mcpStageTools
	case mcpActionMode:
		m.mcp.stage = mcpStageMode
		m.mcp.mode = mcpModeIndex(v.Tier)
	case mcpActionEdit:
		return m.openMCPConfig()
	case mcpActionAuth:
		return m.authenticateMCP(v)
	case mcpActionClearAuth:
		m.mcp.stage = mcpStageConfirmClearAuth
		m.mcp.confirm = 1
	case mcpActionConnect:
		return m.connectSelectedMCP(v)
	case mcpActionLogs:
		m.mcp.stage = mcpStageLogs
	case mcpActionDisable:
		return m.disableSelectedMCP(v)
	case mcpActionRemove:
		m.mcp.stage = mcpStageConfirmRemove
		m.mcp.confirm = 1
	}
	return m, nil
}

// connectSelectedMCP 连接或重连选中的 MCP 服务器。
// 如果服务器已连接则先断开再重连，连接成功后刷新管理器视图。
func (m chatTUI) connectSelectedMCP(v mcpServerView) (tea.Model, tea.Cmd) {
	if m.ctrl == nil {
		m.notice("mcp: no active session")
		return m, nil
	}
	if v.Status == "connected" {
		m.ctrl.DisconnectMCPServer(v.Name)
	}
	n, err := m.ctrl.ConnectConfiguredMCPServer(v.Name)
	if err != nil {
		m.notice("mcp connect: " + err.Error())
		return m, nil
	}
	if m.mcpDisabled != nil {
		delete(m.mcpDisabled, v.Name)
	}
	m.host = m.ctrl.Host()
	m.refreshMCPManager()
	if m.mcp != nil {
		m.mcp.stage = mcpStageDetail
		m.mcp.selectName(v.Name)
	}
	m.notice(fmt.Sprintf("connected %s — %d tools (available next message)", v.Name, n))
	return m, nil
}

// disableSelectedMCP 在当前会话中禁用选中的 MCP 服务器。
// 禁用仅对当前会话生效，不会修改配置文件。
func (m chatTUI) disableSelectedMCP(v mcpServerView) (tea.Model, tea.Cmd) {
	if m.ctrl == nil {
		m.notice("mcp: no active session")
		return m, nil
	}
	persisted := false
	if m.mcpDisabled == nil {
		m.mcpDisabled = map[string]bool{}
	}
	m.mcpDisabled[v.Name] = true
	m.ctrl.DisconnectMCPServer(v.Name)
	m.host = m.ctrl.Host()
	m.refreshMCPManager()
	if m.mcp != nil {
		m.mcp.stage = mcpStageDetail
		m.mcp.selectName(v.Name)
	}
	if persisted {
		m.notice("disabled " + v.Name)
	} else {
		m.notice("disabled " + v.Name + " for this session")
	}
	return m, nil
}

// removeSelectedMCP 从配置中移除选中的 MCP 服务器并断开连接。
// 操作不可撤销（从管理器面板中）。
func (m chatTUI) removeSelectedMCP() (tea.Model, tea.Cmd) {
	v, ok := m.mcp.selectedServer()
	if !ok {
		m.mcp.stage = mcpStageList
		return m, nil
	}
	if m.ctrl == nil {
		m.notice("mcp: no active session")
		return m, nil
	}
	disconnected, err := m.ctrl.RemoveMCPServer(v.Name)
	if err != nil {
		m.notice("mcp remove: " + err.Error())
		m.mcp.stage = mcpStageDetail
		return m, nil
	}
	if m.mcpDisabled != nil {
		delete(m.mcpDisabled, v.Name)
	}
	m.host = m.ctrl.Host()
	m.refreshMCPManager()
	if m.mcp != nil {
		m.mcp.stage = mcpStageList
		m.mcp.name = ""
	}
	if disconnected {
		m.notice("disconnected " + v.Name + " and removed it from config")
	} else {
		m.notice("removed " + v.Name + " from config")
	}
	return m, nil
}

// applyMCPMode 更改选中 MCP 服务器的连接模式（background/eager）。
// 保存配置后，如果服务器尚未连接则自动尝试连接。
func (m chatTUI) applyMCPMode(tier string) (tea.Model, tea.Cmd) {
	v, ok := m.mcp.selectedServer()
	if !ok {
		return m, nil
	}
	cfg, err := config.Load()
	if err != nil {
		m.notice("mcp mode: " + err.Error())
		return m, nil
	}
	found := false
	var selected config.PluginEntry
	for i := range cfg.Plugins {
		if cfg.Plugins[i].Name == v.Name {
			cfg.Plugins[i].Tier = normalizeMCPTierForCLI(tier)
			if !cfg.Plugins[i].ShouldAutoStart() {
				cfg.Plugins[i].AutoStart = mcpBoolPtr(true)
			}
			selected = cfg.Plugins[i]
			found = true
			break
		}
	}
	if !found {
		m.notice(fmt.Sprintf("mcp mode: no configured MCP server named %q", v.Name))
		return m, nil
	}
	if err := cfg.Save(); err != nil {
		m.notice("mcp mode: " + err.Error())
		return m, nil
	}
	if m.mcpDisabled != nil {
		delete(m.mcpDisabled, v.Name)
	}
	if m.ctrl != nil && !mcpConnected(m.ctrl, v.Name) {
		if _, err := m.ctrl.ConnectConfiguredMCPServer(v.Name); err != nil {
			recordMCPModePluginFailure(m.ctrl, selected, err)
			m.notice("saved connection mode, but connect failed: " + err.Error())
		}
		m.host = m.ctrl.Host()
	}
	m.refreshMCPManager()
	if m.mcp != nil {
		m.mcp.stage = mcpStageDetail
		m.mcp.selectName(v.Name)
	}
	m.notice("updated connection mode for " + v.Name)
	return m, nil
}

// recordMCPModePluginFailure 记录更改连接模式后自动连接失败的错误信息。
func recordMCPModePluginFailure(ctrl control.Capabilities, e config.PluginEntry, err error) {
	if ctrl == nil || ctrl.Host() == nil || err == nil {
		return
	}
	exp := e.ExpandedPlugin()
	ctrl.Host().RecordFailure(plugin.Spec{
		Name:    exp.Name,
		Type:    exp.Type,
		Command: exp.Command,
		Args:    exp.Args,
		Env:     exp.Env,
		URL:     exp.URL,
		Headers: exp.Headers,
	}, err)
}

// openMCPConfig 打开 MCP 配置文件进行编辑。
// 优先使用 VISUAL/EDITOR 环境变量指定的编辑器，否则尝试 vim/vi/nano，
// 最后回退到系统默认应用。
func (m chatTUI) openMCPConfig() (tea.Model, tea.Cmd) {
	path := ""
	if m.mcp != nil {
		path = m.mcp.snapshot.configPath
	}
	if strings.TrimSpace(path) == "" {
		path = mcpConfigLocation()
	}
	launch, err := mcpEditConfigLaunchCommand(path, exec.LookPath)
	if err != nil {
		m.notice("edit config: " + err.Error())
		return m, nil
	}
	if launch.systemDefault {
		m.notice("no terminal editor found; opened config with the system default app. Set EDITOR=vim to edit in terminal.")
	} else if launch.editor != "" {
		m.notice("opening config with " + launch.editor)
	}
	return m, tea.ExecProcess(launch.cmd, func(err error) tea.Msg {
		return mcpExternalDoneMsg{label: "edit config", target: path, err: err}
	})
}

// authenticateMCP 打开浏览器进行 MCP 服务器的 OAuth 认证。
// 如果服务器没有返回授权 URL，则提示用户查看日志。
func (m chatTUI) authenticateMCP(v mcpServerView) (tea.Model, tea.Cmd) {
	u := mcpAuthURL(v)
	if u == "" {
		m.notice("mcp auth: no authorization URL was returned; view logs for details")
		return m, nil
	}
	cmd, err := mcpOpenCommand(u)
	if err != nil {
		m.notice("mcp auth: " + err.Error())
		return m, nil
	}
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		return mcpExternalDoneMsg{label: "authorization page", target: u, err: err}
	})
}

// clearSelectedMCPAuthentication 清除选中服务器的认证信息（确认后执行）。
func (m chatTUI) clearSelectedMCPAuthentication() (tea.Model, tea.Cmd) {
	if m.mcp == nil {
		return m, nil
	}
	v, ok := m.mcp.selectedServer()
	if !ok {
		m.mcp.stage = mcpStageList
		return m, nil
	}
	return m.clearMCPAuthentication(v)
}

// clearMCPAuthentication 执行清除 MCP 服务器认证信息的操作。
// 清除配置文件中的认证头、环境变量和 URL token，
// 断开服务器连接并清除失败记录，然后刷新管理器视图。
func (m chatTUI) clearMCPAuthentication(v mcpServerView) (tea.Model, tea.Cmd) {
	if v.BuiltIn {
		m.notice("managed MCP servers do not store authentication")
		return m, nil
	}
	_, changed, _, err := config.ClearPluginAuthenticationInSource(v.Name)
	if err != nil {
		m.notice("clear authentication: " + err.Error())
		return m, nil
	}
	if m.ctrl != nil {
		m.ctrl.DisconnectMCPServer(v.Name)
		if h := m.ctrl.Host(); h != nil {
			h.ClearFailure(v.Name)
		}
		m.host = m.ctrl.Host()
	}
	m.refreshMCPManager()
	if m.mcp != nil {
		m.mcp.stage = mcpStageDetail
		m.mcp.selectName(v.Name)
	}
	if changed {
		m.notice("cleared authentication for " + v.Name + "; reconnect to authorize again")
	} else {
		m.notice("cleared local authentication state for " + v.Name)
	}
	return m, nil
}

// mcpModeIndex 返回连接模式在 mcpTierChoices 中的索引位置。
func mcpModeIndex(tier string) int {
	tier = normalizeMCPTierForCLI(tier)
	for i, choice := range mcpTierChoices {
		if choice == tier {
			return i
		}
	}
	return 0
}

// normalizeMCPTierForCLI 将连接模式名称标准化为 CLI 使用的形式。
// "lazy" 映射为 "background"，其他未知值也默认为 "background"。
func normalizeMCPTierForCLI(tier string) string {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "eager":
		return "eager"
	case "background", "lazy":
		return "background"
	case "":
		return "background"
	default:
		return "background"
	}
}

// mcpConfigLocation 返回 MCP 配置文件的路径。
// 优先使用项目级配置源路径，其次检查 .mcp.json，最后使用用户级配置路径。
func mcpConfigLocation() string {
	if path := config.SourcePath(); path != "" {
		return path
	}
	if _, err := os.Stat(".mcp.json"); err == nil {
		return ".mcp.json"
	}
	if path := config.UserConfigPath(); path != "" {
		return path
	}
	return "reasonix.toml"
}

// mcpEditConfigLaunch 封装了打开配置文件编辑器所需的命令和元信息。
type mcpEditConfigLaunch struct {
	cmd           *exec.Cmd
	editor        string
	systemDefault bool
}

// mcpEditConfigLaunchCommand 构建打开配置文件编辑器的命令。
// 按优先级尝试：VISUAL 环境变量 -> EDITOR 环境变量 -> vim/vi/nano -> 系统默认应用。
func mcpEditConfigLaunchCommand(path string, lookPath func(string) (string, error)) (mcpEditConfigLaunch, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return mcpEditConfigLaunch{}, fmt.Errorf("no config path available")
	}
	if editor := strings.TrimSpace(os.Getenv("VISUAL")); editor != "" {
		return mcpEditConfigLaunch{
			cmd:    exec.Command("sh", "-lc", editor+" "+shellQuote(path)),
			editor: mcpEditorDisplayName(editor),
		}, nil
	}
	if editor := strings.TrimSpace(os.Getenv("EDITOR")); editor != "" {
		return mcpEditConfigLaunch{
			cmd:    exec.Command("sh", "-lc", editor+" "+shellQuote(path)),
			editor: mcpEditorDisplayName(editor),
		}, nil
	}
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	for _, editor := range []string{"vim", "vi", "nano"} {
		if bin, err := lookPath(editor); err == nil && strings.TrimSpace(bin) != "" {
			return mcpEditConfigLaunch{
				cmd:    exec.Command(bin, path),
				editor: editor,
			}, nil
		}
	}
	cmd, err := mcpOpenCommand(path)
	if err != nil {
		return mcpEditConfigLaunch{}, err
	}
	return mcpEditConfigLaunch{cmd: cmd, systemDefault: true}, nil
}

// mcpEditorDisplayName 从编辑器命令字符串中提取编辑器名称（取第一个空格前的部分）。
func mcpEditorDisplayName(editor string) string {
	fields := strings.Fields(editor)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// mcpOpenCommand 构建跨平台打开 URL 或文件的命令。
// macOS 使用 open，Windows 使用 rundll32，Linux 使用 xdg-open。
func mcpOpenCommand(target string) (*exec.Cmd, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, fmt.Errorf("empty target")
	}
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target), nil
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target), nil
	default:
		return exec.Command("xdg-open", target), nil
	}
}

// mcpAuthURL 返回服务器的认证 URL，如果不需要认证则返回空字符串。
func mcpAuthURL(v mcpServerView) string {
	auth := mcpAuthDiagnosis(v)
	if auth.Status != mcpdiag.AuthRequired {
		return ""
	}
	return strings.TrimSpace(auth.URL)
}

// mcpAuthStatus 返回服务器的认证状态诊断结果。
func mcpAuthStatus(v mcpServerView) string {
	return mcpAuthDiagnosis(v).Status
}

// mcpAuthDiagnosis 获取服务器的完整认证诊断信息。
// 优先使用视图中已缓存的诊断结果，否则重新计算。
func mcpAuthDiagnosis(v mcpServerView) mcpdiag.AuthDiagnosis {
	if v.AuthStatus != "" {
		return mcpdiag.AuthDiagnosis{Status: v.AuthStatus, URL: v.AuthURL}
	}
	return mcpdiag.DiagnoseAuth(v.Transport, v.Status, v.Error, v.URL, v.authConfigured)
}

// mcpCanClearAuth 判断服务器是否支持清除认证操作。
// 内置服务器和未配置的服务器不支持，远程传输类型的服务器支持。
func mcpCanClearAuth(v mcpServerView) bool {
	if !v.Configured || v.BuiltIn {
		return false
	}
	if v.authConfigured || mcpAuthStatus(v) != mcpdiag.AuthNone {
		return true
	}
	return mcpdiag.IsRemoteTransport(v.Transport)
}

// mcpConnected 检查指定名称的 MCP 服务器是否已连接。
func mcpConnected(ctrl control.Capabilities, name string) bool {
	if ctrl == nil || ctrl.Host() == nil {
		return false
	}
	for _, s := range ctrl.Host().Servers() {
		if s.Name == name {
			return true
		}
	}
	return false
}

// shellQuote 对字符串进行 shell 引号转义，用于安全地拼接 shell 命令。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// mcpBoolPtr 返回布尔值的指针，用于设置配置中的 AutoStart 字段。
func mcpBoolPtr(v bool) *bool { return &v }
