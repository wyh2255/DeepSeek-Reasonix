// mcp_manager.go 实现了 /mcp 管理器的交互式 TUI 界面。
// 该文件负责：
//   - 定义 MCP 管理器的状态机（列表 -> 详情 -> 工具/日志/模式/确认删除）
//   - 处理键盘导航和页面切换
//   - 构建 MCP 服务器快照，合并配置信息和运行时状态
//   - 计算服务器的认证状态、传输类型和能力信息
//   - 提供分页显示、数字键快捷选择等 UI 辅助功能
package cli

import (
	"sort"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"reasonix/internal/config"
	"reasonix/internal/mcpdiag"
	"reasonix/internal/plugin"
)

const (
	mcpListMaxRows = 10 // 列表视图中最多显示的服务器行数
	mcpToolMaxRows = 14 // 工具详情视图中最多显示的工具行数
)

// mcpStage 表示 MCP 管理器的当前页面/阶段
type mcpStage int

const (
	mcpStageList             mcpStage = iota // 服务器列表页
	mcpStageDetail                           // 服务器详情页
	mcpStageTools                            // 工具列表页
	mcpStageLogs                             // 日志查看页
	mcpStageMode                             // 连接模式选择页
	mcpStageConfirmRemove                    // 确认删除对话框
	mcpStageConfirmClearAuth                 // 确认清除认证对话框
)

// mcpManager 是 MCP 管理器的核心状态结构，跟踪当前页面、选中项和确认状态。
type mcpManager struct {
	stage    mcpStage
	snapshot mcpSnapshot
	sel      int
	name     string
	action   int
	mode     int
	confirm  int
}

// mcpSnapshot 是 MCP 服务器状态的快照，包含所有服务器的视图数据和配置路径。
type mcpSnapshot struct {
	servers    []mcpServerView
	configPath string
	err        string
}

// mcpServerView 是单个 MCP 服务器的视图数据，合并了配置信息和运行时状态。
type mcpServerView struct {
	Name       string
	Transport  string
	Status     string
	BuiltIn    bool
	Configured bool
	AutoStart  bool
	Tier       string
	Command    string
	Args       []string
	URL        string
	EnvKeys    []string
	Tools      int
	Prompts    int
	Resources  int
	Error      string
	ToolList   []plugin.ToolInfo
	AuthStatus string
	AuthURL    string

	authConfigured bool
}

// mcpAction 表示 MCP 管理器中可执行的操作类型
type mcpAction string

const (
	mcpActionViewTools mcpAction = "view-tools"   // 查看工具列表
	mcpActionMode      mcpAction = "mode"          // 更改连接模式
	mcpActionEdit      mcpAction = "edit"          // 编辑配置文件
	mcpActionConnect   mcpAction = "connect"       // 连接/重连服务器
	mcpActionAuth      mcpAction = "auth"          // 进行认证
	mcpActionClearAuth mcpAction = "clear-auth"    // 清除认证信息
	mcpActionLogs      mcpAction = "logs"          // 查看日志
	mcpActionDisable   mcpAction = "disable"       // 禁用服务器
	mcpActionRemove    mcpAction = "remove"        // 移除服务器
)

// mcpActionItem 表示管理器详情页中的一个可选操作项
type mcpActionItem struct {
	kind  mcpAction
	label string
}

// mcpExternalDoneMsg 是外部进程（如编辑器、浏览器）完成后的消息
type mcpExternalDoneMsg struct {
	label  string
	target string
	err    error
}

// mcpTierChoices 是 MCP 服务器连接模式的可选项列表
var mcpTierChoices = []string{"background", "eager"}

// openMCPManager 打开 MCP 管理器界面。
// 如果指定了 name，则直接跳转到该服务器的详情页。
func (m *chatTUI) openMCPManager(name string) {
	m.mcp = &mcpManager{stage: mcpStageList, snapshot: m.buildMCPSnapshot()}
	if name != "" {
		m.mcp.selectName(name)
		m.mcp.stage = mcpStageDetail
	}
	m.mcp.clamp()
}

// refreshMCPManager 刷新 MCP 管理器的服务器快照数据。
func (m *chatTUI) refreshMCPManager() {
	if m.mcp == nil {
		return
	}
	m.mcp.snapshot = m.buildMCPSnapshot()
	m.mcp.clamp()
}

// handleMCPManagerKey 处理 MCP 管理器的所有键盘输入。
// 根据当前页面阶段（stage）分发到不同的处理逻辑：
// - 列表页：上下导航、刷新、进入详情
// - 详情页：选择操作
// - 模式页：选择连接模式
// - 确认对话框：确认或取消操作
func (m chatTUI) handleMCPManagerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	p := m.mcp
	if p == nil {
		return m, nil
	}
	switch msg.String() {
	case "ctrl+c", "q":
		m.mcp = nil
		return m, nil
	case "esc", "left", "h":
		switch p.stage {
		case mcpStageList:
			m.mcp = nil
			return m, nil
		case mcpStageDetail:
			p.stage = mcpStageList
			p.action = 0
			return m, nil
		default:
			p.stage = mcpStageDetail
			p.action = 0
			if p.name == "" {
				p.stage = mcpStageList
			}
			return m, nil
		}
	}

	switch p.stage {
	case mcpStageList:
		switch msg.String() {
		case "up", "k":
			if p.sel > 0 {
				p.sel--
			}
		case "down", "j":
			if p.sel < len(p.snapshot.servers)-1 {
				p.sel++
			}
		case "r":
			p.snapshot = m.buildMCPSnapshot()
		case "enter", "right", "l":
			if len(p.snapshot.servers) > 0 {
				p.name = p.snapshot.servers[p.sel].Name
				p.stage = mcpStageDetail
				p.action = 0
			}
		}
	case mcpStageDetail:
		v, ok := p.selectedServer()
		if !ok {
			p.stage = mcpStageList
			return m, nil
		}
		actions := mcpActionsFor(v, p.snapshot.configPath)
		switch msg.String() {
		case "up", "k":
			if p.action > 0 {
				p.action--
			}
		case "down", "j":
			if p.action < len(actions)-1 {
				p.action++
			}
		case "enter":
			if len(actions) > 0 {
				return m.applyMCPAction(v, actions[p.action].kind)
			}
		default:
			if idx, ok := numberKeyIndex(msg.String(), len(actions)); ok {
				p.action = idx
				return m.applyMCPAction(v, actions[p.action].kind)
			}
		}
	case mcpStageMode:
		switch msg.String() {
		case "up", "k":
			if p.mode > 0 {
				p.mode--
			}
		case "down", "j":
			if p.mode < len(mcpTierChoices)-1 {
				p.mode++
			}
		case "enter":
			return m.applyMCPMode(mcpTierChoices[p.mode])
		default:
			if idx, ok := numberKeyIndex(msg.String(), len(mcpTierChoices)); ok {
				p.mode = idx
				return m.applyMCPMode(mcpTierChoices[p.mode])
			}
		}
	case mcpStageConfirmRemove:
		switch msg.String() {
		case "up", "k", "down", "j":
			if p.confirm == 0 {
				p.confirm = 1
			} else {
				p.confirm = 0
			}
		case "y":
			p.confirm = 0
			return m.removeSelectedMCP()
		case "n":
			p.stage = mcpStageDetail
		case "enter":
			if p.confirm == 0 {
				return m.removeSelectedMCP()
			}
			p.stage = mcpStageDetail
		}
	case mcpStageConfirmClearAuth:
		switch msg.String() {
		case "up", "k", "down", "j":
			if p.confirm == 0 {
				p.confirm = 1
			} else {
				p.confirm = 0
			}
		case "y":
			p.confirm = 0
			return m.clearSelectedMCPAuthentication()
		case "n":
			p.stage = mcpStageDetail
		case "enter":
			if p.confirm == 0 {
				return m.clearSelectedMCPAuthentication()
			}
			p.stage = mcpStageDetail
		}
	}
	return m, nil
}

// clamp 确保管理器的所有索引值在有效范围内，防止越界。
func (p *mcpManager) clamp() {
	if p.sel < 0 {
		p.sel = 0
	}
	if n := len(p.snapshot.servers); n > 0 && p.sel >= n {
		p.sel = n - 1
	}
	if p.name != "" {
		p.selectName(p.name)
	}
	if p.action < 0 {
		p.action = 0
	}
	if p.mode < 0 {
		p.mode = 0
	}
	if p.mode >= len(mcpTierChoices) {
		p.mode = len(mcpTierChoices) - 1
	}
	if p.confirm < 0 || p.confirm > 1 {
		p.confirm = 0
	}
}

// selectName 在服务器列表中查找并选中指定名称的服务器。
func (p *mcpManager) selectName(name string) bool {
	for i, s := range p.snapshot.servers {
		if s.Name == name {
			p.sel = i
			p.name = name
			return true
		}
	}
	return false
}

// selectedServer 返回当前选中的服务器视图数据。
// 优先按名称查找，找不到时按索引查找。
func (p *mcpManager) selectedServer() (mcpServerView, bool) {
	if p.name != "" {
		for _, s := range p.snapshot.servers {
			if s.Name == p.name {
				return s, true
			}
		}
	}
	if p.sel >= 0 && p.sel < len(p.snapshot.servers) {
		return p.snapshot.servers[p.sel], true
	}
	return mcpServerView{}, false
}

// buildMCPSnapshot 构建 MCP 服务器状态的完整快照。
// 合并三个来源的数据：
// 1. 控制器中已连接的服务器及其工具/提示/资源信息
// 2. 控制器中连接失败的服务器及错误信息
// 3. 配置文件中定义但尚未连接的服务器
func (m chatTUI) buildMCPSnapshot() mcpSnapshot {
	snap := mcpSnapshot{configPath: mcpConfigLocation()}
	cfg, err := config.Load()
	if err != nil {
		snap.err = err.Error()
	}
	configured := map[string]config.PluginEntry{}
	var configuredEntries []config.PluginEntry
	if cfg != nil {
		configuredEntries = append(configuredEntries, cfg.Plugins...)
		for _, p := range configuredEntries {
			configured[p.Name] = p
		}
	}
	seen := map[string]bool{}
	if m.host != nil {
		for _, s := range m.host.Servers() {
			v := mcpServerView{
				Name: s.Name, Transport: fallbackText(s.Transport, "stdio"), Status: "connected",
				Tools: s.Tools, Prompts: s.Prompts, Resources: s.Resources,
				ToolList: append([]plugin.ToolInfo(nil), s.ToolList...),
			}
			if p, ok := configured[s.Name]; ok {
				v = withMCPPluginConfig(v, p)
			}
			snap.servers = append(snap.servers, v)
			seen[s.Name] = true
		}
		for _, f := range m.host.Failures() {
			v := mcpServerView{
				Name: f.Name, Transport: fallbackText(f.Transport, "stdio"), Status: "failed",
				Error: f.Error,
			}
			if p, ok := configured[f.Name]; ok {
				v = withMCPPluginConfig(v, p)
			}
			snap.servers = append(snap.servers, v)
			seen[f.Name] = true
		}
		for _, name := range m.host.ConnectingServers() {
			if seen[name] {
				continue
			}
			v := mcpServerView{Name: name, Status: "initializing"}
			if p, ok := configured[name]; ok {
				v = withMCPPluginConfig(v, p)
			}
			snap.servers = append(snap.servers, v)
			seen[name] = true
		}
	}
	for _, p := range configuredEntries {
		if seen[p.Name] {
			continue
		}
		v := mcpServerView{Name: p.Name}
		switch {
		case m.mcpDisabled[p.Name] || !p.ShouldAutoStart():
			v.Status = "disabled"
		default:
			v.Status = "deferred"
		}
		v = withMCPPluginConfig(v, p)
		snap.servers = append(snap.servers, v)
		seen[p.Name] = true
	}
	return snap
}

// withMCPPluginConfig 将配置文件中的插件信息合并到服务器视图中。
// 设置传输类型、自动启动、连接层级、命令/URL、环境变量和认证诊断信息。
func withMCPPluginConfig(v mcpServerView, p config.PluginEntry) mcpServerView {
	transport := strings.ToLower(strings.TrimSpace(p.Type))
	if transport == "" {
		transport = "stdio"
	}
	v.Transport = transport
	v.Configured = true
	v.AutoStart = p.ShouldAutoStart()
	v.Tier = p.ResolvedTier()
	v.Command = p.Command
	v.Args = append([]string(nil), p.Args...)
	v.URL = p.URL
	v.authConfigured = mcpdiag.HasAuthConfig(p.Headers, p.Env, p.URL)
	if len(p.Env) > 0 {
		v.EnvKeys = make([]string, 0, len(p.Env))
		for k := range p.Env {
			v.EnvKeys = append(v.EnvKeys, k)
		}
		sort.Strings(v.EnvKeys)
	}
	auth := mcpdiag.DiagnoseAuth(v.Transport, v.Status, v.Error, v.URL, v.authConfigured)
	v.AuthStatus = auth.Status
	v.AuthURL = auth.URL
	return v
}

// visibleRange 计算分页显示的可见范围。
// 当总数超过限制时，确保选中项在可视区域内居中显示。
func visibleRange(total, sel, limit int) (int, int) {
	if limit <= 0 || total <= limit {
		return 0, total
	}
	if sel < 0 {
		sel = 0
	}
	if sel >= total {
		sel = total - 1
	}
	start := sel - limit/2
	if start < 0 {
		start = 0
	}
	if start+limit > total {
		start = total - limit
	}
	return start, start + limit
}

// numberKeyIndex 将数字键（1-9）转换为列表索引，用于快捷选择操作。
// 返回索引和是否有效的布尔值。
func numberKeyIndex(s string, limit int) (int, bool) {
	if len(s) != 1 || s[0] < '1' || s[0] > '9' {
		return 0, false
	}
	idx := int(s[0] - '1')
	return idx, idx < limit
}

// fallbackText 当字符串为空或仅含空白时返回备用文本。
func fallbackText(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// titleText 将字符串首字母大写，用于标题显示。
func titleText(s string) string {
	if s == "" {
		return "MCP"
	}
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && size == 0 {
		return s
	}
	return strings.ToUpper(string(r)) + s[size:]
}
