// Package config 负责从 TOML 文件加载 Reasonix 的运行时配置。
//
// 配置解析优先级（从高到低）：
//  1. 命令行标志（flag）
//  2. 项目级 ./reasonix.toml
//  3. 用户级 config.toml（位于 OS 用户配置目录）
//  4. 内置默认值
//
// 用户全局运行时控制（如智能体步数限制）是文档化的例外情况。
// 敏感信息（如 API 密钥）通过 api_key_env 从环境变量获取，绝不存储在配置文件中。
//
// 配置文件格式为 TOML，使用 github.com/BurntSushi/toml 库解析。
// 支持 ${VAR} 和 ${VAR:-default} 环境变量扩展。
package config

import (
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"

	"reasonix/internal/fileutil"
	"reasonix/internal/netclient"
	"reasonix/internal/provider"
)

// validSkillName 是技能名称的正则表达式验证规则。
// 要求：以字母或数字开头，可包含字母、数字、点、下划线、连字符，长度 1-64。
var validSkillName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// IsValidSkillName 检查名称是否为可用的技能标识符。
// 技能标识符要求：以字母或数字开头，可包含字母、数字、点、下划线、连字符，长度 1-64。
//
// 参数：
//   - name: 要检查的技能名称
//
// 返回值：true 表示名称有效
func IsValidSkillName(name string) bool { return validSkillName.MatchString(name) }

// SkillNameKey 规范化技能标识符用于配置比较。
// 在 Windows 上转为小写（因为文件系统不区分大小写），其他平台保持原样。
// 无效的名称返回空字符串。
//
// 参数：
//   - name: 原始技能名称
//
// 返回值：规范化的名称（用于比较），无效名称返回空字符串
func SkillNameKey(name string) string {
	name = strings.TrimSpace(name)
	if !IsValidSkillName(name) {
		return ""
	}
	if runtime.GOOS == "windows" {
		return strings.ToLower(name)
	}
	return name
}

// Config 是 Reasonix 的运行时配置主结构体。
// 包含所有可配置的选项，从 TOML 文件加载并合并。
//
// 配置层级：
//   - 顶层：全局设置（默认模型、语言、凭证存储等）
//   - UI/Desktop: 界面相关设置（主题、语言、布局等）
//   - Agent: 智能体行为设置（系统提示、步数限制、温度等）
//   - Providers: 模型提供者列表（API 地址、密钥、模型等）
//   - Tools: 工具配置（启用的工具、超时等）
//   - Permissions: 权限策略（允许/询问/拒绝规则）
//   - Sandbox: 沙箱配置（写入限制、bash 沙箱等）
//   - Network: 网络配置（代理设置）
//   - Plugins: MCP 插件列表
//   - Skills: 技能发现配置
//   - LSP: 语言服务器协议配置
//   - Bot: 多渠道 IM 机器人配置
type Config struct {
	ConfigVersion    int                 `toml:"config_version"`    // 配置文件版本号，用于迁移检测
	DefaultModel     string              `toml:"default_model"`     // 默认模型引用（如 "deepseek" 或 "deepseek/deepseek-v4-flash"）
	Language         string              `toml:"language"`          // UI/模型语言标签（如 "zh"），空则从 $LANG/$REASONIX_LANG 自动检测
	CredentialsStore string              `toml:"credentials_store"` // 凭证存储模式（auto/legacy）
	UI               UIConfig            `toml:"ui"`               // CLI 界面设置（主题、快捷键布局等）
	Desktop          DesktopConfig       `toml:"desktop"`          // 桌面端 UI 设置（布局、主题、遥测等）
	Notifications    NotificationsConfig `toml:"notifications"`    // 系统通知设置
	Agent            AgentConfig         `toml:"agent"`            // 智能体行为设置（系统提示、步数、温度等）
	Providers        []ProviderEntry     `toml:"providers"`        // 模型提供者列表
	Tools            ToolsConfig         `toml:"tools"`            // 工具配置（启用列表、超时等）
	Permissions      PermissionsConfig   `toml:"permissions"`      // 权限策略（允许/询问/拒绝规则）
	Sandbox          SandboxConfig       `toml:"sandbox"`          // 沙箱配置（写入限制、bash 沙箱等）
	Network          NetworkConfig       `toml:"network"`          // 网络配置（代理设置）
	Plugins          []PluginEntry       `toml:"plugins"`          // MCP 插件列表
	Skills           SkillsConfig        `toml:"skills"`           // 技能发现配置
	Statusline       StatuslineConfig    `toml:"statusline"`       // 自定义状态行配置
	LSP              LSPConfig           `toml:"lsp"`              // 语言服务器协议配置
	Bot              BotConfig           `toml:"bot"`              // 多渠道 IM 机器人配置

	providerSources          map[string]providerSourceScope // 提供者来源（user/project），用于合并策略
	shadowedProjectProviders []ProviderEntry                // 被用户级配置遮蔽的项目级提供者
}

// providerSourceScope 标识提供者的配置来源。
type providerSourceScope string

const (
	providerSourceUser    providerSourceScope = "user"    // 来自用户级配置（~/.reasonix/config.toml）
	providerSourceProject providerSourceScope = "project" // 来自项目级配置（./reasonix.toml）
)

// UIConfig controls CLI presentation-only settings. Desktop appearance is kept in
// DesktopConfig so desktop preferences cannot alter terminal output or prompts.
type UIConfig struct {
	Theme          string `toml:"theme"`           // auto|dark|light; empty resolves to auto
	ThemeStyle     string `toml:"theme_style"`     // graphite|aurora|slate|carbon|nocturne|amber and legacy aliases
	ShortcutLayout string `toml:"shortcut_layout"` // classic|desktop; accepted for compatibility
	CloseBehavior  string `toml:"close_behavior"`  // legacy desktop close behavior; prefer desktop.close_behavior
	ShowReasoning  bool   `toml:"show_reasoning"`  // Ctrl+O / /verbose: show thinking text in CLI; false = collapsed
}

// DesktopConfig controls desktop-only UI preferences. It is intentionally
// separate from top-level language and [ui] so desktop choices do not affect CLI
// language, terminal colours, or provider-visible prompt/request data.
type DesktopConfig struct {
	Language       string   `toml:"language"`         // auto|en|zh; empty/auto = browser/OS auto-detect
	LayoutStyle    string   `toml:"layout_style"`     // classic|workbench|creation; desktop layout style
	Theme          string   `toml:"theme"`            // auto|dark|light; empty resolves to auto
	ThemeStyle     string   `toml:"theme_style"`      // graphite|aurora|slate|carbon|nocturne|amber and legacy aliases
	CloseBehavior  string   `toml:"close_behavior"`   // quit|background; desktop window close behavior
	DisplayMode    string   `toml:"display_mode"`     // standard|compact (legacy "minimal" maps to compact); transcript display mode
	StatusBarStyle string   `toml:"status_bar_style"` // icon|text; desktop status bar metric labels
	StatusBarItems []string `toml:"status_bar_items"` // ordered visible desktop status bar items
	CheckUpdates   *bool    `toml:"check_updates"`    // startup update checks; nil keeps the default enabled
	Telemetry      *bool    `toml:"telemetry"`        // anonymous launch ping (install id + version + OS); nil keeps the default enabled
	Metrics        *bool    `toml:"metrics"`          // aggregate desktop metrics (anonymous signal/bucket counts; no content); nil keeps the default enabled
	ProviderAccess []string `toml:"provider_access"`  // desktop-only list of provider entries shown in Settings > Model > Access
	ExpandThinking bool     `toml:"expand_thinking"`  // true = show reasoning text expanded by default; false = collapsed
}

// NotificationsConfig controls optional system notifications for CLI chat/run.
type NotificationsConfig struct {
	Enabled         bool `toml:"enabled"`
	TurnDone        bool `toml:"turn_done"`
	ApprovalRequest bool `toml:"approval_request"`
	AskRequest      bool `toml:"ask_request"`
}

// UITheme 规范化 ui.theme 为支持的值（"auto"|"dark"|"light"）。
//
// 返回值：规范化后的主题字符串
func (c *Config) UITheme() string {
	switch strings.ToLower(strings.TrimSpace(c.UI.Theme)) {
	case "dark":
		return "dark"
	case "light":
		return "light"
	default:
		return "auto"
	}
}

// UIThemeStyle 规范化 ui.theme_style。空表示"为解析后的亮/暗 shell 选择默认风格"。
//
// 返回值：规范化后的主题风格字符串
func (c *Config) UIThemeStyle() string {
	return normalizeThemeStyle(c.UI.ThemeStyle)
}

// UIShortcutLayout 规范化旧版 CLI 快捷键布局设置。保留用于兼容性；
// Shift+Tab 切换 Plan，Ctrl+Y 切换 YOLO 在两种布局中都有效。
//
// 返回值：规范化后的布局字符串（"classic"|"desktop"）
func (c *Config) UIShortcutLayout() string {
	switch strings.ToLower(strings.TrimSpace(c.UI.ShortcutLayout)) {
	case "desktop", "dual", "dual-axis", "dual_axis":
		return "desktop"
	default:
		return "classic"
	}
}

func normalizeThemeStyle(style string) string {
	switch strings.ToLower(strings.TrimSpace(style)) {
	case "graphite", "aurora", "slate", "carbon", "nocturne", "amber", "ember", "midnight", "sandstone", "porcelain", "linen", "glacier":
		return strings.ToLower(strings.TrimSpace(style))
	default:
		return ""
	}
}

func normalizeDesktopLayoutStyle(style string) string {
	switch strings.ToLower(strings.TrimSpace(style)) {
	case "classic":
		return "classic"
	case "workbench", "workspace":
		return "workbench"
	case "creation":
		return "creation"
	default:
		return "workbench"
	}
}

func normalizeCloseBehavior(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "quit", "exit":
		return "quit"
	default:
		return "background"
	}
}

// DesktopLanguage 规范化桌面端 UI 语言。空表示从浏览器/OS 区域设置自动检测；
// 故意不读取顶层 language，后者用于 CLI/模型面向的运行时。
//
// 返回值：规范化后的语言字符串（"en"|"zh" 或空）
func (c *Config) DesktopLanguage() string {
	switch strings.ToLower(strings.TrimSpace(c.Desktop.Language)) {
	case "en":
		return "en"
	case "zh":
		return "zh"
	default:
		return ""
	}
}

// DesktopTheme 规范化 desktop.theme。新桌面用户默认为 OS 自动 graphite 产品外观；
// 显式的 auto/light/dark 被保留。
//
// 返回值：规范化后的主题字符串
func (c *Config) DesktopTheme() string {
	switch strings.ToLower(strings.TrimSpace(c.Desktop.Theme)) {
	case "auto":
		return "auto"
	case "light":
		return "light"
	case "dark":
		return "dark"
	default:
		return "auto"
	}
}

// DesktopThemeStyle normalizes desktop.theme_style. Empty means the frontend
// chooses the default style for the resolved desktop theme.
func (c *Config) DesktopThemeStyle() string {
	return normalizeThemeStyle(c.Desktop.ThemeStyle)
}

// DesktopLayoutStyle normalizes the desktop layout style. New installs default
// to workbench; explicit classic remains respected.
func (c *Config) DesktopLayoutStyle() string {
	if strings.EqualFold(strings.TrimSpace(c.Desktop.ThemeStyle), "workbench") && strings.TrimSpace(c.Desktop.LayoutStyle) == "" {
		return "workbench"
	}
	return normalizeDesktopLayoutStyle(c.Desktop.LayoutStyle)
}

// DesktopCloseBehavior 规范化桌面关闭窗口偏好。回退到旧版 ui.close_behavior 值，
// 用于 [desktop] 存在之前编写的配置。
//
// 返回值：规范化后的行为字符串（"quit"|"background"）
func (c *Config) DesktopCloseBehavior() string {
	if strings.TrimSpace(c.Desktop.CloseBehavior) != "" {
		return normalizeCloseBehavior(c.Desktop.CloseBehavior)
	}
	return normalizeCloseBehavior(c.UI.CloseBehavior)
}

// UICloseBehavior is the legacy name for DesktopCloseBehavior.
func (c *Config) UICloseBehavior() string {
	return c.DesktopCloseBehavior()
}

// DesktopDisplayMode 规范化转录显示模式。默认为 "standard"（扁平渲染，无折叠）。
//
// 返回值：规范化后的显示模式（"standard"|"compact"）
func (c *Config) DesktopDisplayMode() string {
	switch strings.ToLower(strings.TrimSpace(c.Desktop.DisplayMode)) {
	case "standard":
		return "standard"
	case "compact", "minimal":
		return "compact"
	default:
		return "standard"
	}
}

// DesktopStatusBarStyle 规范化桌面状态栏指标标签样式。默认为 "text"；
// 显式的 "icon" 保留用户的紧凑选择。
//
// 返回值：规范化后的样式（"icon"|"text"）
func (c *Config) DesktopStatusBarStyle() string {
	switch strings.ToLower(strings.TrimSpace(c.Desktop.StatusBarStyle)) {
	case "icon":
		return "icon"
	case "text":
		return "text"
	default:
		return "text"
	}
}

var defaultDesktopStatusBarItems = []string{
	"model",
	"workspace",
	"git_branch",
	"cache",
	"cache_avg",
	"session_tokens",
	"turn_tokens",
	"turn_cost",
	"session_turns",
	"context",
	"compact",
	"cost",
	"balance",
}

var knownDesktopStatusBarItems = desktopStatusBarItemSet(defaultDesktopStatusBarItems)

func desktopStatusBarItemSet(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}

// DefaultDesktopStatusBarItems 返回默认的有序可见桌面状态栏项目列表。
//
// 返回值：默认状态栏项目列表
func DefaultDesktopStatusBarItems() []string {
	return append([]string(nil), defaultDesktopStatusBarItems...)
}

// DesktopStatusBarItems 规范化有序可见桌面状态栏项目。
// 未设置或空列表使用默认完整集合；显式非空列表保留用户顺序并省略隐藏项目。
//
// 返回值：规范化后的状态栏项目列表
func (c *Config) DesktopStatusBarItems() []string {
	return normalizeDesktopStatusBarItems(c.Desktop.StatusBarItems)
}

func normalizeDesktopStatusBarItems(items []string) []string {
	out := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, raw := range items {
		id := strings.TrimSpace(raw)
		if !knownDesktopStatusBarItems[id] || seen[id] {
			continue
		}
		out = append(out, id)
		seen[id] = true
	}
	if len(out) == 0 {
		return DefaultDesktopStatusBarItems()
	}
	return out
}

// DesktopCheckUpdates 检查桌面端是否应在启动时检查更新。
// 缺失配置默认为 true，使现有用户继续收到更新通知。
//
// 返回值：true 表示启用更新检查
func (c *Config) DesktopCheckUpdates() bool {
	if c == nil || c.Desktop.CheckUpdates == nil {
		return true
	}
	return *c.Desktop.CheckUpdates
}

// ColdResumePruneEnabled 检查当会话恢复超过提供者缓存窗口时是否省略过期的工具结果。
// 默认 true（冷启动更便宜）；用户可通过禁用来保留完整历史。
//
// 返回值：true 表示启用冷恢复修剪
func (c *Config) ColdResumePruneEnabled() bool {
	if c == nil || c.Agent.ColdResumePrune == nil {
		return true
	}
	return *c.Agent.ColdResumePrune
}

// ReasoningLanguage 规范化 agent.reasoning_language。空值表示自动：
// 可见推理遵循 LanguagePolicy 描述的对话语言。旧版 "default" 视为自动。
//
// 返回值：规范化后的推理语言（"auto"|"zh"|"en"）
func (c *Config) ReasoningLanguage() string {
	if c == nil {
		return "auto"
	}
	return NormalizeReasoningLanguage(c.Agent.ReasoningLanguage)
}

// NormalizeReasoningLanguage 将各种语言标识规范化为 auto|zh|en 之一。
// 支持多种别名（如 "cn"/"chinese"/"中文" → "zh"）。
//
// 参数：
//   - lang: 原始语言标识
//
// 返回值：规范化后的语言标识
func NormalizeReasoningLanguage(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "", "auto", "follow", "conversation", "detect", "default", "model", "model-default", "model_default", "provider":
		return "auto"
	case "zh", "cn", "chinese", "中文":
		return "zh"
	case "en", "english":
		return "en"
	default:
		return "auto"
	}
}

// DesktopTelemetry 检查桌面端是否发送匿名启动 ping。
// 不携带对话、密钥或文件数据——见 desktop/README.md。
//
// 返回值：true 表示启用遥测
func (c *Config) DesktopTelemetry() bool {
	if c == nil || c.Desktop.Telemetry == nil {
		return true
	}
	return *c.Desktop.Telemetry
}

// DesktopMetrics 检查桌面端是否发送聚合桌面指标——
// 匿名（信号、桶）计数器，不包含内容。默认开启。
//
// 返回值：true 表示启用指标收集
func (c *Config) DesktopMetrics() bool {
	if c == nil || c.Desktop.Metrics == nil {
		return true
	}
	return *c.Desktop.Metrics
}

// LSPConfig governs the optional Language Server Protocol tools (lsp_definition,
// lsp_references, lsp_hover, lsp_diagnostics). Enabled defaults to true; the
// servers themselves are never bundled — each resolves on PATH and the tool
// returns an install hint when it is missing, so the capability is dormant until
// the user installs a server. Servers overrides or extends the built-in language
// → server map, keyed by language id (e.g. "go", "rust", "python").
type LSPConfig struct {
	Enabled bool                 `toml:"enabled"`
	Servers map[string]LSPServer `toml:"servers"`
}

// LSPServer overrides a built-in language's server or, when keyed by a new
// language, adds one. An empty field falls back to the built-in default for that
// language; Extensions is required when adding a language the built-ins don't
// cover (e.g. ".ex" for Elixir) so files route to it.
type LSPServer struct {
	Command     string            `toml:"command"`
	Args        []string          `toml:"args"`
	Env         map[string]string `toml:"env"`
	LanguageID  string            `toml:"language_id"`
	Extensions  []string          `toml:"extensions"`
	InstallHint string            `toml:"install_hint"`
}

// StatuslineConfig configures a custom status line. Command, when set, is run at
// startup and after each turn; its first line of stdout replaces the built-in
// status data row. A JSON payload (model, context tokens, cwd) is fed on stdin.
type StatuslineConfig struct {
	Command string `toml:"command"`
}

// BotConfig 控制多渠道 IM bot 消息网关。
type BotConfig struct {
	Enabled          bool                  `toml:"enabled"`
	Model            string                `toml:"model"` // 用于 bot 的模型名，空则用 default_model
	ToolApprovalMode string                `toml:"tool_approval_mode"`
	MaxSteps         int                   `toml:"max_steps"`
	DebounceMs       int                   `toml:"debounce_ms"` // 消息合并窗口，毫秒
	Allowlist        BotAllowlist          `toml:"allowlist"`
	QQ               QQBotConfig           `toml:"qq"`
	Feishu           FeishuBotConfig       `toml:"feishu"`
	Weixin           WeixinBotConfig       `toml:"weixin"`
	Connections      []BotConnectionConfig `toml:"connections"`
}

// BotAllowlist 控制哪些用户可以使用 bot。
type BotAllowlist struct {
	Enabled      bool     `toml:"enabled"`
	AllowAll     bool     `toml:"allow_all"`
	QQUsers      []string `toml:"qq_users"`
	FeishuUsers  []string `toml:"feishu_users"`
	WeixinUsers  []string `toml:"weixin_users"`
	QQGroups     []string `toml:"qq_groups"`
	FeishuGroups []string `toml:"feishu_groups"`
	WeixinGroups []string `toml:"weixin_groups"`
}

// QQBotConfig QQ 官方 Bot API v2 配置。
type QQBotConfig struct {
	Enabled      bool   `toml:"enabled"`
	AppID        string `toml:"app_id"`
	AppSecretEnv string `toml:"app_secret_env"` // 环境变量名，如 QQ_BOT_APP_SECRET
	Sandbox      bool   `toml:"sandbox"`        // true 使用 QQ 沙箱 API / gateway
}

// FeishuBotConfig 飞书自建应用 Bot 配置。
type FeishuBotConfig struct {
	Enabled           bool   `toml:"enabled"`
	Domain            string `toml:"domain"` // feishu（默认）| lark
	AppID             string `toml:"app_id"`
	AppSecretEnv      string `toml:"app_secret_env"`     // 如 FEISHU_BOT_APP_SECRET
	VerificationToken string `toml:"verification_token"` // 事件订阅验证 token
	Mode              string `toml:"mode"`               // webhook（默认）| websocket
	WebhookPort       int    `toml:"webhook_port"`       // webhook 模式端口
	RequireMention    bool   `toml:"require_mention"`
}

// WeixinBotConfig 微信 iLink Bot 配置。
type WeixinBotConfig struct {
	Enabled   bool   `toml:"enabled"`
	AccountID string `toml:"account_id"`
	TokenEnv  string `toml:"token_env"` // 环境变量名，如 WEIXIN_BOT_TOKEN
	APIBase   string `toml:"api_base"`  // iLink API base URL
}

// BotConnectionConfig is the desktop-friendly connection record for IM bot
// channels. It keeps install/runtime state separate from legacy per-provider
// knobs so the UI can expose a simple "connect first" flow while old configs
// keep working.
type BotConnectionConfig struct {
	ID               string                        `toml:"id"`
	Provider         string                        `toml:"provider"` // qq|feishu|weixin
	Domain           string                        `toml:"domain"`   // feishu|lark|weixin|qq
	Label            string                        `toml:"label"`
	Enabled          bool                          `toml:"enabled"`
	Status           string                        `toml:"status"` // disconnected|pending|connected|error
	Model            string                        `toml:"model"`
	ToolApprovalMode string                        `toml:"tool_approval_mode"`
	WorkspaceRoot    string                        `toml:"workspace_root"`
	Credential       BotConnectionCredential       `toml:"credential"`
	SessionMappings  []BotConnectionSessionMapping `toml:"session_mappings"`
	LastError        string                        `toml:"last_error"`
	CreatedAt        string                        `toml:"created_at"`
	UpdatedAt        string                        `toml:"updated_at"`
}

type BotConnectionCredential struct {
	AppID        string `toml:"app_id"`
	AppSecretEnv string `toml:"app_secret_env"`
	AccountID    string `toml:"account_id"`
	TokenEnv     string `toml:"token_env"`
}

type BotConnectionSessionMapping struct {
	RemoteID      string `toml:"remote_id"`
	SessionID     string `toml:"session_id"`
	SessionSource string `toml:"session_source"`
	ChatType      string `toml:"chat_type"`
	UserID        string `toml:"user_id"`
	ThreadID      string `toml:"thread_id"`
	Scope         string `toml:"scope"`
	WorkspaceRoot string `toml:"workspace_root"`
	UpdatedAt     string `toml:"updated_at"`
}

// NetworkConfig 控制普通的出站 HTTP 流量，如模型提供者、钱包余额查询、
// 更新检查、CodeGraph 下载和 web_fetch。web_fetch 复用这些代理设置，
// 同时保持自己的 SSRF 防护拨号器。
type NetworkConfig struct {
	// ProxyMode 代理模式："auto"（默认；目前使用环境代理）、"env"、"custom" 或 "off"。
	// auto 为将来的 OS 代理检测留出空间，无需更改配置形状。
	ProxyMode string `toml:"proxy_mode"`
	// ProxyURL 高级自定义覆盖，如 "socks5://127.0.0.1:7890"。
	// 设置且 proxy_mode = "custom" 时，优先于结构化代理表。
	ProxyURL string `toml:"proxy_url"`
	// NoProxy 对自定义代理生效。env/auto 模式使用进程环境中的 NO_PROXY。
	NoProxy string             `toml:"no_proxy"`
	Proxy   NetworkProxyConfig `toml:"proxy"` // 结构化代理配置
}

// NetworkProxyConfig is the structured custom-proxy editor shape. Password is
// optional and supports ${VAR} expansion, so users can avoid storing it literally.
type NetworkProxyConfig struct {
	Type     string `toml:"type"` // http|https|socks5|socks5h
	Server   string `toml:"server"`
	Port     int    `toml:"port"`
	Username string `toml:"username"`
	Password string `toml:"password"`
}

// NetworkProxySpec 返回 netclient 使用的扩展代理设置。
// 所有支持 ${VAR} 扩展的字段都已展开。
//
// 返回值：代理规范结构体
func (c *Config) NetworkProxySpec() netclient.ProxySpec {
	return netclient.ProxySpec{
		Mode:        c.Network.ProxyMode,
		URL:         ExpandVars(c.Network.ProxyURL),
		NoProxy:     ExpandVars(c.Network.NoProxy),
		Type:        c.Network.Proxy.Type,
		Server:      ExpandVars(c.Network.Proxy.Server),
		Port:        c.Network.Proxy.Port,
		Username:    ExpandVars(c.Network.Proxy.Username),
		Password:    ExpandVars(c.Network.Proxy.Password),
		DirectHosts: c.directProxyHosts(),
	}
}

// directProxyHosts collects the base_url hosts of providers marked no_proxy, so
// netclient bypasses the proxy for them without knowing any provider by name.
//
// Only for an auto-detected proxy (auto/env): that proxy is typically a
// GFW-circumvention one not meant for domestic endpoints (e.g. mimo), so keep
// them direct. An explicit proxy_mode = "custom" is the user saying "route
// everything through this" — e.g. a mandatory corporate proxy — so honor it for
// every provider; a custom-proxy user who wants a host direct uses
// network.no_proxy instead (#3635).
func (c *Config) directProxyHosts() []string {
	if c.NetworkProxyMode() == netclient.ModeCustom {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range c.Providers {
		if !p.NoProxy {
			continue
		}
		u, err := url.Parse(strings.TrimSpace(p.BaseURL))
		if err != nil {
			continue
		}
		if h := u.Hostname(); h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// NetworkProxyMode 规范化 network.proxy_mode 为已知值。
//
// 返回值：规范化后的代理模式字符串
func (c *Config) NetworkProxyMode() string {
	return netclient.NormalizeMode(c.Network.ProxyMode)
}

// SkillsConfig 配置技能发现。Paths 添加额外的"自定义"范围技能根目录——
// 每个都是 SKILL.md / <name>.md 剧本的目录——在项目根目录
// （工作区下的 .reasonix/.agents/.agent/.claude）和全局根目录之间扫描。
// ExcludedPaths 隐藏匹配的发现根目录而不删除文件夹。
// 支持 ~、相对路径和 ${VAR} 扩展。DisabledSkills 从智能体提示、
// 斜杠调用和技能工具中隐藏命名技能，同时保持它们可管理。
type SkillsConfig struct {
	Paths          []string `toml:"paths"`           // 额外的自定义技能根目录
	ExcludedPaths  []string `toml:"excluded_paths"`   // 隐藏的技能发现根目录
	DisabledSkills []string `toml:"disabled_skills"`  // 禁用的技能名称列表
	MaxDepth       int      `toml:"max_depth"`        // 嵌套技能发现的最大深度（默认 3，最大 5）
}

// SkillCustomPaths 返回配置的自定义技能根目录，${VAR} 已扩展；空条目被丢弃。
//
// 返回值：自定义技能根目录列表
func (c *Config) SkillCustomPaths() []string {
	var out []string
	for _, p := range c.Skills.Paths {
		if p = ExpandVars(p); strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// SkillExcludedPaths returns configured skill roots that should be hidden from
// discovery, with ${VAR} expanded and empty entries dropped.
func (c *Config) SkillExcludedPaths() []string {
	var out []string
	for _, p := range c.Skills.ExcludedPaths {
		if p = ExpandVars(p); strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// SkillMaxDepth 限制嵌套技能发现的深度。深度 3 有利于打包的技能包，
// 同时 Store 通过要求描述来保持嵌套 markdown 的安全性。
//
// 返回值：最大深度（1-5，默认 3）
func (c *Config) SkillMaxDepth() int {
	const (
		defaultDepth = 3
		maxDepth     = 5
	)
	if c == nil || c.Skills.MaxDepth == 0 {
		return defaultDepth
	}
	if c.Skills.MaxDepth < 1 {
		return 1
	}
	if c.Skills.MaxDepth > maxDepth {
		return maxDepth
	}
	return c.Skills.MaxDepth
}

// DisabledSkillNames 返回有效的禁用技能标识符，保留第一次出现的拼写，
// 丢弃重复项和空条目。
//
// 返回值：禁用的技能名称列表
func (c *Config) DisabledSkillNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range c.Skills.DisabledSkills {
		name = strings.TrimSpace(name)
		if !IsValidSkillName(name) {
			continue
		}
		key := SkillNameKey(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	return out
}

// IsSkillDisabled 检查指定名称的技能是否被配置为禁用。
//
// 参数：
//   - name: 技能名称
//
// 返回值：true 表示技能已禁用
func (c *Config) IsSkillDisabled(name string) bool {
	key := SkillNameKey(name)
	if key == "" {
		return false
	}
	for _, disabled := range c.DisabledSkillNames() {
		if SkillNameKey(disabled) == key {
			return true
		}
	}
	return false
}

// SandboxConfig 限制工具调用的影响范围（Phase 0：文件写入限制）。
// WorkspaceRoot 是内置文件写入工具（write_file/edit_file/multi_edit/move_file）
// 可以修改的目录；空表示当前工作目录，因此默认写入保留在项目内。
// AllowWrite 列出写入工具还可以触及的额外目录（如兄弟仓库或临时目录）。
// 两者都支持 ${VAR} / ${VAR:-default} 扩展。读取不受限制；
// 限制 `bash` 是 Phase 1（OS 级沙箱）。
type SandboxConfig struct {
	WorkspaceRoot string   `toml:"workspace_root"` // 文件写入工具的工作区根目录
	AllowWrite    []string `toml:"allow_write"`     // 额外允许写入的目录列表
	// Bash 是 bash 工具的 OS 沙箱模式："enforce"（默认）限制每个命令，
	// "off" 不限制运行。Phase 1；目前仅 macOS，其他平台优雅降级。
	Bash string `toml:"bash"`
	// Network 允许从 bash 沙箱内进行网络出口。默认 true 使模块/包下载继续工作；
	// 边界 then 是写入。
	Network bool `toml:"network"`
}

// WriteRoots 返回文件写入工具可以修改的目录列表：
// 工作区根目录（未设置时默认为当前工作目录），加上 AllowWrite 的额外目录，
// ${VAR} 已扩展。根目录按原样返回（相对或绝对）；限制器将其解析为绝对、无符号链接的路径。
// 结果始终非空，因此默认启用限制。
//
// 返回值：允许写入的目录列表
func (c *Config) WriteRoots() []string {
	return c.WriteRootsForRoot(".")
}

// WriteRootsForRoot is like WriteRoots but falls back to fallbackRoot when the
// config doesn't explicitly set a workspace_root. Desktop tabs pass their
// project root here so tool confinement is correct without changing cwd.
func (c *Config) WriteRootsForRoot(fallbackRoot string) []string {
	root := ExpandVars(c.Sandbox.WorkspaceRoot)
	if root == "" {
		root = fallbackRoot
		if root == "" || root == "." {
			if wd, err := os.Getwd(); err == nil {
				root = wd
			} else {
				root = "."
			}
		}
	}
	roots := []string{root}
	for _, d := range c.Sandbox.AllowWrite {
		if d = ExpandVars(d); d != "" {
			roots = append(roots, d)
		}
	}
	return roots
}

// BashMode 规范化 bash 沙箱模式：只有显式的 "off" 才禁用它；
// 空值或任何其他值都解析为 "enforce"，因此沙箱默认开启且安全失败。
//
// 返回值："enforce" 或 "off"
func (c *Config) BashMode() string {
	if c.Sandbox.Bash == "off" {
		return "off"
	}
	return "enforce"
}

// AgentConfig configures the harness loop. PlannerModel is optional: when set
// to another provider's name it enables two-model collaboration, where the
// planner handles low-frequency planning in its own session (kept separate so
// each model's prompt prefix stays cache-stable). SubagentModel is the optional
// default for runAs=subagent skills; SubagentModels overrides it per skill name.
// AgentConfig 配置智能体的行为。PlannerModel 可选：设置为另一个提供者的名称时
// 启用双模型协作，规划器在自己的会话中处理低频规划（保持分离以使每个模型的
// 提示前缀保持缓存稳定）。SubagentModel 是 runAs=subagent 技能的可选默认值；
// SubagentModels 可按技能名称覆盖。
type AgentConfig struct {
	SystemPrompt     string            `toml:"system_prompt"`       // 系统提示文本（直接嵌入）
	SystemPromptFile string            `toml:"system_prompt_file"` // 系统提示文件路径（优先于 system_prompt）
	MaxSteps         int               `toml:"max_steps"`          // 每轮工具调用轮次上限；0 = 无限制
	PlannerMaxSteps  int               `toml:"planner_max_steps"`  // 规划器只读工具调用轮次上限；0 = 无限制
	Temperature      float64           `toml:"temperature"`        // 模型温度参数
	PlannerModel     string            `toml:"planner_model"`      // 规划器模型引用（用于双模型协作）
	SubagentModel    string            `toml:"subagent_model"`     // 子智能体默认模型
	SubagentModels   map[string]string `toml:"subagent_models"`    // 每技能的子智能体模型覆盖
	SubagentEffort   string            `toml:"subagent_effort"`    // 子智能体默认推理深度
	SubagentEfforts  map[string]string `toml:"subagent_efforts"`   // 每技能的子智能体推理深度覆盖
	// OutputStyle selects a persona/tone block folded into the system prompt at
	// startup (a built-in like "explanatory"/"learning"/"concise", or a custom
	// .reasonix/output-styles/<name>.md). Empty = the unmodified prompt.
	OutputStyle string `toml:"output_style"`
	// AutoPlan controls whether interactive turns that look multi-step start in
	// plan mode automatically: "off" keeps plan mode manual, "on" enables the
	// approval gate. Legacy "ask" is treated as "on".
	AutoPlan string `toml:"auto_plan"`
	// ReasoningLanguage controls the preferred language for visible reasoning
	// text. Empty/auto follows the conversation language. Applied as transient
	// turn context, not the stable prompt.
	ReasoningLanguage string `toml:"reasoning_language"`
	// AutoPlanClassifier optionally names a provider/model used to classify
	// borderline auto-plan decisions. Empty keeps the zero-cost heuristic path.
	AutoPlanClassifier string `toml:"auto_plan_classifier"`
	// Compaction window fractions: soft = notice only, compact = trigger, force = hard ceiling.
	SoftCompactRatio  float64 `toml:"soft_compact_ratio"`
	CompactRatio      float64 `toml:"compact_ratio"`
	CompactForceRatio float64 `toml:"compact_force_ratio"`
	// Keep controls which compactable messages stay verbatim beyond the current
	// user-fact/digest floor and recent tail. Empty uses the conservative default
	// of keeping error tool results.
	Keep       []string `toml:"keep"`
	RecentKeep int      `toml:"recent_keep"`
	// ColdResumePrune elides stale tool results when a session reopens past the
	// provider cache window. nil = default enabled.
	ColdResumePrune *bool `toml:"cold_resume_prune"`
	// PlanModeAllowedTools names tools that are exempt from the plan-mode read-only
	// gate. When a tool named here is called while in plan mode, it executes without
	// the "plan mode is read-only" block. Use sparingly — prefer the built-in safe
	// bash commands for read-only exploration.
	PlanModeAllowedTools []string `toml:"plan_mode_allowed_tools"`
}

// ProviderEntry declares a model provider instance. ContextWindow is the model's
// token budget; the harness compacts older history as a turn's prompt approaches
// it (see agent compaction). 0 disables compaction for the instance.
// ProviderEntry 声明一个模型提供者实例。
// ContextWindow 是模型的 token 预算；当轮次的提示接近此值时，
// 智能体会压缩旧历史（见 agent compaction）。0 禁用该实例的压缩。
type ProviderEntry struct {
	Name          string                       `toml:"name"`           // 提供者名称（如 "deepseek"、"custom-token-xxx"）
	Kind          string                       `toml:"kind"`           // 提供者类型（"openai"、"anthropic"）
	BaseURL       string                       `toml:"base_url"`       // API 端点基础 URL
	Model         string                       `toml:"model"`          // 单个模型（向后兼容）
	Models        []string                     `toml:"models"`         // 供应商的模型列表（一个 base_url/key，多个模型）
	ModelsURL     string                       `toml:"models_url"`     // 启动时从此 URL 自动获取模型列表
	Default       string                       `toml:"default"`        // Models 设置时的默认模型（否则为 Models[0]）
	APIKeyEnv     string                       `toml:"api_key_env"`    // API 密钥的环境变量名
	BalanceURL    string                       `toml:"balance_url"`    // 可选：提供者特定的钱包余额端点。空 = 不读取余额
	ContextWindow int                          `toml:"context_window"` // 模型的 token 上下文窗口大小
	Price         *provider.Pricing            `toml:"price"`          // 旧版/提供者级回退价格
	Prices        map[string]*provider.Pricing `toml:"prices"`         // 可选：每模型价格（键为模型 ID）
	// Thinking / Effort are provider-kind-specific knobs forwarded to the provider
	// via Config.Extra. The anthropic provider reads Thinking="adaptive" to enable
	// extended thinking and Effort ("low".."max") to tune depth. The
	// openai-compatible provider forwards Effort as reasoning_effort for
	// thinking-capable models; DeepSeek accepts high|max.
	// Empty = provider default.
	Thinking string `toml:"thinking"`
	Effort   string `toml:"effort"`
	// Vision marks the model as accepting image input. When set, images the user
	// attaches are embedded in the request (image_url for openai-kind, base64
	// blocks for anthropic). Off by default: text-only models 400 on image input,
	// and image tokens are heavy — gating keeps text-only flows cheap (the prompt
	// prefix is byte-identical with no image, so the cache is unaffected either way).
	Vision bool `toml:"vision"`
	// VisionModels narrows image input support to specific models in a multi-model
	// provider. This lets one provider expose both text-only and multimodal chat
	// models without enabling image payloads for every model.
	VisionModels []string `toml:"vision_models"`
	// VisionDetail sets the openai image_url detail hint (low|high); empty = auto
	// (the field is omitted). "low" caps an image to a fixed ~85 tokens for cheap
	// coarse reads; ignored by providers without the knob (e.g. anthropic).
	VisionDetail string `toml:"vision_detail"`
	// ReasoningProtocol selects the request shape for OpenAI-compatible reasoning
	// models. Empty/auto uses the model capability registry plus endpoint
	// heuristics; none disables automatic reasoning controls for this provider.
	ReasoningProtocol string `toml:"reasoning_protocol"`
	// SupportedEfforts lists the /effort levels this provider/model exposes.
	// When non-empty, it overrides the built-in defaults derived from
	// Kind/BaseURL and makes /effort configurable. "auto" is the implicit
	// prefix — always accepted. DefaultEffort resolves it; omit DefaultEffort
	// (or set one outside this list) to fall back to SupportedEfforts[0].
	SupportedEfforts []string `toml:"supported_efforts"`
	// DefaultEffort is the /effort level used when the user picks "auto" or
	// has not set Effort. Ignored when SupportedEfforts is empty.
	DefaultEffort string `toml:"default_effort"`
	// NoProxy reaches this provider's base_url directly, never through the proxy.
	// For China-only endpoints a foreign-exit proxy resets the TLS handshake (#2803).
	NoProxy bool `toml:"no_proxy"`
}

// ModelList 返回此提供者暴露的模型列表：显式的 `models` 列表，
// 或单个 `model` 作为单元素列表（向后兼容）。两者都未设置则返回 nil。
//
// 返回值：模型名称列表
func (e *ProviderEntry) ModelList() []string {
	if len(e.Models) > 0 {
		return e.Models
	}
	if e.Model != "" {
		return []string{e.Model}
	}
	return nil
}

// IsLikelyChatModel 检查模型 ID 是否看起来像聊天/补全模型，
// 而非专门的音频/视觉/嵌入模型。使用保守的基于名称的启发式方法——
// OpenAI 兼容的 /models API 不返回能力/模态元数据，
// 因此在提供者添加此类字段之前，这是最可靠的回退方案。
//
// 启发式检查分两步：
//  1. 多词子串检查：检测跨分隔符的复合词（如 "text-embedding"、"text-to-speech"）
//  2. 词元级检查：按常见分隔符（- _ . / :）分割模型 ID，
//     将每个词元与已知的非聊天关键词集合比较
//
// 注意："voice" 故意不在非聊天集合中，因为它太宽泛——
// 合法的未来聊天模型可能在名称中包含它。
//
// 参数：
//   - model: 模型 ID 字符串
//
// 返回值：true 表示可能是聊天模型
func IsLikelyChatModel(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	lower := strings.ToLower(model)

	// Pass 1: compound terms that span separator boundaries.
	var compoundNonChat = []string{
		"text-embedding", "text-to-speech", "speech-to-text",
	}
	for _, c := range compoundNonChat {
		if strings.Contains(lower, c) {
			return false
		}
	}

	// Pass 2: token-level check.
	tokens := strings.FieldsFunc(lower, func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == '/' || r == ':'
	})
	var nonChatTokens = map[string]bool{
		"asr": true, "stt": true, "tts": true,
		"whisper": true, "embedding": true,
		"moderation": true, "rerank": true, "dall": true,
		"transcription": true,
	}
	for _, tok := range tokens {
		if nonChatTokens[tok] {
			return false
		}
	}
	return true
}

// ChatModelList 返回过滤后的聊天/补全模型列表。
// 排除非聊天模型（TTS、STT、ASR、嵌入等），使它们不出现在聊天模型选择器中。
// 仅在需要完整的原始提供者模型列表时使用 ModelList()，
// 如配置序列化、提供者诊断或模型获取编辑。
//
// 返回值：可能的聊天模型名称列表
func (e *ProviderEntry) ChatModelList() []string {
	raw := e.ModelList()
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, m := range raw {
		if IsLikelyChatModel(m) {
			out = append(out, m)
		}
	}
	return out
}

// DefaultModel 返回提供者的默认模型：显式的 `default` 字段，
// 否则 ModelList 的第一个元素。
//
// 返回值：默认模型名称
func (e *ProviderEntry) DefaultModel() string {
	if e.Default != "" {
		return e.Default
	}
	if l := e.ModelList(); len(l) > 0 {
		return l[0]
	}
	return ""
}

// HasModel 检查指定模型是否为此提供者的模型之一。
//
// 参数：
//   - m: 模型名称
//
// 返回值：true 表示包含该模型
func (e *ProviderEntry) HasModel(m string) bool {
	for _, x := range e.ModelList() {
		if x == m {
			return true
		}
	}
	return false
}

// PriceForModel 返回指定模型的配置价格（每百万 token）。
// 每模型价格优先；旧版提供者级价格作为回退。
//
// 参数：
//   - model: 模型名称
//
// 返回值：价格信息指针（未配置则返回 nil）
func (e *ProviderEntry) PriceForModel(model string) *provider.Pricing {
	if e == nil {
		return nil
	}
	if e.Prices != nil {
		if p := e.Prices[strings.TrimSpace(model)]; p != nil {
			return clonePricing(p)
		}
	}
	return clonePricing(e.Price)
}

func (e *ProviderEntry) applyModelPrice() {
	if e == nil {
		return
	}
	e.Price = e.PriceForModel(e.Model)
}

func clonePricing(p *provider.Pricing) *provider.Pricing {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// ToolsConfig 选择启用哪些内置工具。空列表表示全部启用。
type ToolsConfig struct {
	Enabled            []string             `toml:"enabled"`              // 启用的工具名称列表（空=全部启用）
	BashTimeoutSeconds *int                 `toml:"bash_timeout_seconds"` // bash 工具超时秒数（nil=120s 默认值）
	BackgroundJobs     BackgroundJobsConfig `toml:"background_jobs"`      // 后台作业配置
	Search             SearchConfig         `toml:"search"`               // 搜索引擎配置（ripgrep/原生）
	Shell              ShellConfig          `toml:"shell"`                // shell 解释器配置
}

const (
	defaultBashTimeoutSeconds             = 120
	defaultBackgroundJobStalledWarningSec = 900
	maxBackgroundJobStalledWarningSec     = 86400
)

// BashTimeoutSeconds 返回前台 bash 超时（秒）。
// 未配置时保持历史 120 秒安全上限；显式 0 禁用工具本地上限；
// 正值设置自定义上限。负值回退到默认值，防止输入错误静默移除安全网。
//
// 返回值：超时秒数
func (c *Config) BashTimeoutSeconds() int {
	if c.Tools.BashTimeoutSeconds == nil || *c.Tools.BashTimeoutSeconds < 0 {
		return defaultBashTimeoutSeconds
	}
	return *c.Tools.BashTimeoutSeconds
}

// BackgroundJobsConfig tunes parent-created background jobs.
type BackgroundJobsConfig struct {
	StalledWarningSeconds *int `toml:"stalled_warning_seconds"`
}

// BackgroundJobStalledWarningSeconds returns the stalled warning threshold in
// seconds. Omitted/negative values keep the default, explicit 0 disables the
// notice, and oversized values clamp to one day so a typo cannot become
// effectively invisible.
func (c *Config) BackgroundJobStalledWarningSeconds() int {
	if c.Tools.BackgroundJobs.StalledWarningSeconds == nil || *c.Tools.BackgroundJobs.StalledWarningSeconds < 0 {
		return defaultBackgroundJobStalledWarningSec
	}
	if *c.Tools.BackgroundJobs.StalledWarningSeconds > maxBackgroundJobStalledWarningSec {
		return maxBackgroundJobStalledWarningSec
	}
	return *c.Tools.BackgroundJobs.StalledWarningSeconds
}

// SearchConfig tunes the grep tool's engine. Engine is "auto" (default — use
// ripgrep when it's on PATH, else the native Go scanner), "native" (always Go),
// or "rg" (require ripgrep; warn at startup and fall back to native if absent).
// RgPath optionally points at a specific ripgrep binary instead of a PATH lookup.
type SearchConfig struct {
	Engine string `toml:"engine"`
	RgPath string `toml:"rg_path"`
}

// ShellConfig chooses the interpreter the bash tool runs commands under. Prefer
// is "auto" (default — real bash when present, else PowerShell on Windows),
// "bash", or "powershell"/"pwsh" (force it; warn at startup and fall back to
// auto if absent). Path optionally points at a specific shell executable.
type ShellConfig struct {
	Prefer string `toml:"prefer"`
	Path   string `toml:"path"`
}

// PermissionsConfig 声明每次调用的权限策略（见 internal/permission）。
// Mode 是写入工具在无规则匹配时的回退决策（"ask"|"allow"|"deny"，默认 "ask"）；
// 只读工具始终回退为 allow。Allow/Ask/Deny 是规则列表，格式为 "ToolName" 或
// "ToolName(glob)"。优先级：deny > ask > allow > fallback。
type PermissionsConfig struct {
	Mode  string   `toml:"mode"`  // 回退决策模式（"ask"|"allow"|"deny"）
	Allow []string `toml:"allow"` // 允许规则列表
	Ask   []string `toml:"ask"`   // 询问规则列表
	Deny  []string `toml:"deny"`  // 拒绝规则列表
}

// PluginEntry 声明一个外部 MCP（Model Context Protocol）服务器。
// Type 选择传输方式："stdio"（默认）将 Command/Args/Env 作为子进程启动；
// "http"（又称 streamable-http）和 "sse" 连接到远程 URL（可选静态 Headers）。
// 字符串字段支持 ${VAR} / ${VAR:-default} 扩展，使密钥（bearer tokens、keys）
// 来自环境变量而非文件。字段镜像 Claude Code 的 mcpServers 规范，
// 因此条目可以来自 reasonix.toml 的 [[plugins]] 或项目根目录的 .mcp.json。
type PluginEntry struct {
	Name    string            `toml:"name"`    // 插件名称（用于标识和合并）
	Type    string            `toml:"type"`    // 传输类型："stdio"（默认）|"http"|"sse"
	Command string            `toml:"command"` // stdio 模式的启动命令
	Args    []string          `toml:"args"`    // 命令参数
	Env     map[string]string `toml:"env"`     // 环境变量
	URL     string            `toml:"url"`     // http/sse 模式的远程 URL
	Headers map[string]string `toml:"headers"` // http/sse 模式的请求头
	// AutoStart controls whether the server connects during session startup.
	// Nil preserves historical behavior: configured servers start automatically.
	AutoStart *bool `toml:"auto_start"`
	// Tier is a legacy compatibility field. New config rendering omits it; enabled
	// MCP servers connect automatically in the background unless auto_start=false.
	// Historical values are accepted for old files:
	//   "eager"      — blocks startup until the handshake completes; required for
	//                  servers whose tools the system prompt depends on.
	//   "lazy"       — legacy alias for background.
	//   "background" — placeholder + spawn fired at boot but not waited on;
	//                  swap happens once the spawn finishes.
	// Empty defaults to "background" so enabled MCPs connect automatically
	// without blocking chat. Unknown non-empty values fall back to "background".
	Tier string `toml:"tier"`
}

// ShouldAutoStart 检查插件是否应自动启动。
// 未设置 AutoStart（nil）时默认为 true。
//
// 返回值：true 表示应自动启动
func (e PluginEntry) ShouldAutoStart() bool {
	return e.AutoStart == nil || *e.AutoStart
}

// ResolvedTier 返回规范化的层级（"eager"|"background"），应用项目默认值。
// 旧版 lazy 和未知值回退到 background，使启用的 MCP 无需手动连接即可使用。
//
// 返回值：规范化后的层级字符串
func (e PluginEntry) ResolvedTier() string {
	return resolvedMCPTier(e.Tier)
}

func resolvedMCPTier(tier string) string {
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

// AutoStartPlugins 返回配置为自动启动的插件列表。
// 用于在会话启动时自动连接 MCP 服务器。
//
// 返回值：应自动启动的插件列表
func (c *Config) AutoStartPlugins() []PluginEntry {
	out := make([]PluginEntry, 0, len(c.Plugins))
	for _, p := range c.Plugins {
		if p.ShouldAutoStart() {
			out = append(out, p)
		}
	}
	return out
}

// DefaultSystemPrompt is used when config provides none.
const DefaultSystemPrompt = `You are Reasonix, a coding agent focused on executing code tasks.
Use the provided tools to read and write files and run shell commands.
Principles: understand the request before acting; verify with tools instead of
guessing; keep changes minimal and correct; briefly summarize what you did.
When the request leaves a real choice to the user — which approach or library,
the scope, or a consequential or ambiguous decision — call the ask tool to offer
2-4 concrete options rather than guessing or burying the question in prose. Skip
it when there's an obvious default; don't ask just to confirm. Approval-bypass
modes do not answer ask questions or approve plans for the user. If no
interactive user is available, the ask tool returns a model-assumption fallback;
state the assumption you made before proceeding.
For multi-step work, track progress with the todo_write tool: lay out the steps,
keep exactly one in_progress, and flip each to completed as you finish it — update
the list as you go, not just at the end.
In plan mode the harness blocks writer tools: do read-only research, then write a
concise plan as your reply and stop. The user is asked to approve before anything
is changed; once approved, work through the steps, updating the task list as you go.`

// LanguagePolicy is the auto fallback appended to the system prompt when no
// concrete UI language is resolved. It is static English text, so it stays part
// of the cache-stable prefix and avoids per-turn language injection.
const LanguagePolicy = `Reply in the same language the user is using in their most recent message: ` +
	`if they write in Chinese answer in Chinese, in English answer in English, and switch ` +
	`whenever they switch. Let this also guide the language you think in. Always keep code, ` +
	`identifiers, file paths, shell commands, and technical terms in their original form — never translate them.`

// Default 返回内置的默认配置。
// 包含所有配置项的默认值，如默认模型（deepseek-flash）、
// 系统提示、权限策略、沙箱设置等。
//
// 返回值：默认配置对象指针
func Default() *Config {
	return &Config{
		ConfigVersion:    3,
		DefaultModel:     "deepseek-flash",
		CredentialsStore: CredentialsStoreAuto,
		UI:               UIConfig{Theme: "auto"},
		Notifications: NotificationsConfig{
			Enabled:         false,
			TurnDone:        true,
			ApprovalRequest: true,
			AskRequest:      true,
		},
		Agent: AgentConfig{
			SystemPrompt: DefaultSystemPrompt,
			// 0 = no step cap: the agent loops until the model gives a final answer,
			// the user cancels, or the provider errors. Context stays bounded by
			// compaction, not by a round count. Set a positive agent.max_steps only
			// if you want a hard guard against runaway.
			MaxSteps:          0,
			PlannerMaxSteps:   0,
			AutoPlan:          "off",
			SoftCompactRatio:  0.5,
			CompactRatio:      0.8,
			CompactForceRatio: 0.9,
		},
		// Mode "ask" with no rules keeps `reasonix run` autonomous (no TTY → ask
		// resolves to allow) while `reasonix` prompts before writers. Users add
		// deny/allow rules to harden or quiet specific tools.
		Permissions: PermissionsConfig{Mode: "ask"},
		// Sandbox on by default: bash is jailed (macOS), network allowed so
		// builds/downloads work. Set bash = "off" to disable. Network=true here
		// so an absent [sandbox] in a user's file keeps egress (zero value would
		// wrongly deny it).
		Sandbox: SandboxConfig{Bash: "enforce", Network: true},
		// LSP tools on by default, but dormant until a language server is on PATH;
		// a missing server yields an install hint rather than an error.
		LSP:     LSPConfig{Enabled: true},
		Network: NetworkConfig{ProxyMode: netclient.ModeAuto},
		Bot: BotConfig{
			ToolApprovalMode: "ask",
			MaxSteps:         25,
			DebounceMs:       1500,
			Allowlist:        BotAllowlist{Enabled: true},
			QQ:               QQBotConfig{AppSecretEnv: "QQ_BOT_APP_SECRET"},
			Feishu:           FeishuBotConfig{Domain: "feishu", AppSecretEnv: "FEISHU_BOT_APP_SECRET", Mode: "webhook", WebhookPort: 8080, RequireMention: true},
			Weixin:           WeixinBotConfig{AccountID: "default", TokenEnv: "WEIXIN_BOT_TOKEN", APIBase: "https://ilinkai.weixin.qq.com"},
		},
		Providers: []ProviderEntry{
			{Name: "deepseek-flash", Kind: "openai", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash", APIKeyEnv: "DEEPSEEK_API_KEY", BalanceURL: "https://api.deepseek.com/user/balance", ContextWindow: 1_000_000, Price: deepSeekV4FlashPrice()},
			{Name: "deepseek-pro", Kind: "openai", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-pro", APIKeyEnv: "DEEPSEEK_API_KEY", BalanceURL: "https://api.deepseek.com/user/balance", ContextWindow: 1_000_000, Price: deepSeekV4ProPrice()},
		},
	}
}

func deepSeekV4FlashPrice() *provider.Pricing {
	return &provider.Pricing{CacheHit: 0.02, Input: 1, Output: 2, Currency: "¥"}
}

func deepSeekV4ProPrice() *provider.Pricing {
	return &provider.Pricing{CacheHit: 0.025, Input: 3, Output: 6, Currency: "¥"}
}

func deepSeekV4Prices() map[string]*provider.Pricing {
	return map[string]*provider.Pricing{
		"deepseek-v4-flash": deepSeekV4FlashPrice(),
		"deepseek-v4-pro":   deepSeekV4ProPrice(),
	}
}

func deepSeekV4FlashPriceUSD() *provider.Pricing {
	return &provider.Pricing{CacheHit: 0.0028, Input: 0.14, Output: 0.28, Currency: "$"}
}

func deepSeekV4ProPriceUSD() *provider.Pricing {
	return &provider.Pricing{CacheHit: 0.003625, Input: 0.435, Output: 0.87, Currency: "$"}
}

func deepSeekV4PricesUSD() map[string]*provider.Pricing {
	return map[string]*provider.Pricing{
		"deepseek-v4-flash": deepSeekV4FlashPriceUSD(),
		"deepseek-v4-pro":   deepSeekV4ProPriceUSD(),
	}
}

// DeepSeekV4PricesForLanguage 保持设置/模板调用点稳定，而官方 DeepSeek 默认值
// 移动到人民币。持久化的价格仍然优先；这仅用于模板和缺失默认值的回填。
//
// 参数：
//   - lang: 语言（目前不影响返回值）
//
// 返回值：模型价格映射
func DeepSeekV4PricesForLanguage(lang string) map[string]*provider.Pricing {
	_ = lang
	return deepSeekV4Prices()
}

func deepSeekV4PricesForConfig(c *Config) map[string]*provider.Pricing {
	_ = c
	return deepSeekV4Prices()
}

func deepSeekV4PriceForModel(lang, model string) *provider.Pricing {
	_ = lang
	return clonePricing(deepSeekV4Prices()[strings.TrimSpace(model)])
}

// DeepSeekOfficialPricingLanguage 保留用于设置/模板兼容性。
// 官方 DeepSeek 提供者现在默认使用人民币价格；配置中的显式用户价格仍覆盖这些默认值。
//
// 返回值：价格语言标识（目前固定返回 "zh"）
func (c *Config) DeepSeekOfficialPricingLanguage() string {
	_ = c
	return "zh"
}

// ApplyDeepSeekOfficialDefaultPricing 刷新仍匹配已知官方默认值的内置/官方 DeepSeek 价格。
// 用户自定义价格不受影响。
//
// 参数：无（修改接收者 c）
func (c *Config) ApplyDeepSeekOfficialDefaultPricing() {
	applyDeepSeekOfficialDefaultPricing(c)
}

func applyDeepSeekOfficialDefaultPricing(c *Config) {
	if c == nil {
		return
	}
	lang := c.DeepSeekOfficialPricingLanguage()
	for i := range c.Providers {
		p := &c.Providers[i]
		if officialProviderKind(p) != "deepseek" {
			continue
		}
		if isKnownDeepSeekOfficialPricing(p.Model, p.Price) {
			p.Price = deepSeekV4PriceForModel(lang, p.Model)
		}
		for model, price := range p.Prices {
			if isKnownDeepSeekOfficialPricing(model, price) {
				p.Prices[model] = deepSeekV4PriceForModel(lang, model)
			}
		}
	}
}

func mimoV25ProPrice() *provider.Pricing {
	return &provider.Pricing{CacheHit: 0.025, Input: 3, Output: 6, Currency: "¥"}
}

func mimoV25Price() *provider.Pricing {
	return &provider.Pricing{CacheHit: 0.02, Input: 1, Output: 2, Currency: "¥"}
}

func mimoV2FlashPrice() *provider.Pricing {
	return &provider.Pricing{CacheHit: 0.07, Input: 0.70, Output: 2.10, Currency: "¥"}
}

func mimoDomesticPrices(models []string) map[string]*provider.Pricing {
	prices := map[string]*provider.Pricing{}
	for _, model := range models {
		switch strings.TrimSpace(model) {
		case "mimo-v2.5-pro", "mimo-v2-pro":
			prices[model] = mimoV25ProPrice()
		case "mimo-v2.5", "mimo-v2-omni":
			prices[model] = mimoV25Price()
		case "mimo-v2-flash":
			prices[model] = mimoV2FlashPrice()
		}
	}
	return prices
}

// ResetOfficialProviderPricingOnUpgrade 在桌面升级时将官方 DeepSeek 价格
// 重置为当前内置的人民币默认值（仅执行一次）。故意从桌面应用启动路径运行，
// 而非每次 config Load()，以保留升级后用户所做的编辑。
//
// 参数：
//   - path: 配置文件路径
//
// 返回值：
//   - bool: 是否执行了重置
//   - error: 操作失败时返回错误
func ResetOfficialProviderPricingOnUpgrade(path string) (bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return false, nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var header Config
	if _, err := toml.DecodeFile(path, &header); err != nil {
		return false, fmt.Errorf("config %s: %w", path, err)
	}
	if header.ConfigVersion >= Default().ConfigVersion {
		return false, nil
	}
	cfg := LoadForEdit(path)
	resetOfficialProviderPricingDefaults(cfg)
	cfg.ConfigVersion = Default().ConfigVersion
	if err := cfg.SaveTo(path); err != nil {
		return false, err
	}
	return true, nil
}

func resetOfficialProviderPricingDefaults(c *Config) {
	if c == nil {
		return
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		switch {
		case officialProviderKind(p) == "deepseek":
			resetDeepSeekOfficialPricing(p)
		}
	}
}

func resetDeepSeekOfficialPricing(p *ProviderEntry) {
	if p == nil {
		return
	}
	defaults := deepSeekV4Prices()
	p.Price = nil
	if strings.TrimSpace(p.Model) != "" && len(p.Models) == 0 {
		if price := defaults[strings.TrimSpace(p.Model)]; price != nil {
			p.Price = clonePricing(price)
			p.Prices = nil
			return
		}
	}
	if p.Prices == nil {
		p.Prices = map[string]*provider.Pricing{}
	}
	for model, price := range defaults {
		if p.HasModel(model) {
			p.Prices[model] = clonePricing(price)
		}
	}
}

func isKnownDeepSeekOfficialPricing(model string, price *provider.Pricing) bool {
	model = strings.TrimSpace(model)
	if model == "" || price == nil {
		return false
	}
	for _, prices := range []map[string]*provider.Pricing{deepSeekV4Prices(), deepSeekV4PricesUSD()} {
		if samePricing(price, prices[model]) {
			return true
		}
	}
	return false
}

func samePricing(a, b *provider.Pricing) bool {
	if a == nil || b == nil {
		return false
	}
	return a.CacheHit == b.CacheHit && a.Input == b.Input && a.Output == b.Output && a.Currency == b.Currency
}

// Load 构建完整配置：按优先级合并多个配置源。
//
// 合并顺序（从低到高）：
//  1. 内置默认值
//  2. 用户级 config.toml（~/.reasonix/config.toml）
//  3. 项目级 reasonix.toml（当前工作目录）
//  4. Claude Code 的 .mcp.json（项目根目录）
//  5. v0.x ~/.reasonix/config.json 的 mcpServers（最低优先级兼容层）
//
// 工作目录中的 .env 文件会首先加载，使 api_key_env 能正确解析。
//
// 返回值：
//   - *Config: 合并后的配置对象
//   - error: 加载失败时返回错误
func Load() (*Config, error) {
	return LoadForRoot(".")
}

// LoadForRoot 从指定根目录构建配置（而非当前工作目录）。
// 当 root 为 "" 或 "." 时，行为与 Load() 相同。
//
// 这是工作区感知的入口点：桌面标签页使用它，
// 使每个项目的 reasonix.toml + .env + .mcp.json 独立解析，
// 无需更改进程的工作目录。
//
// 参数：
//   - root: 项目根目录路径
//
// 返回值：
//   - *Config: 合并后的配置对象
//   - error: 加载失败时返回错误
func LoadForRoot(root string) (*Config, error) {
	root = resolveRoot(root)
	loadDotEnvForRoot(root)
	cfg := Default()
	cfg.CredentialsStore = credentialsStoreMode()

	projectTOML := "reasonix.toml"
	if root != "." {
		projectTOML = filepath.Join(root, "reasonix.toml")
	}

	var tomlSources []string
	if uc := userConfigLoadPath(); uc != "" {
		tomlSources = append(tomlSources, uc)
		if err := mergeRuntimeTOMLFile(cfg, uc); err != nil {
			return nil, err
		}
	}
	globalMaxSteps := cfg.Agent.MaxSteps
	globalPlannerMaxSteps := cfg.Agent.PlannerMaxSteps

	tomlSources = append(tomlSources, projectTOML)
	if err := mergeRuntimeTOMLFile(cfg, projectTOML); err != nil {
		return nil, err
	}
	// Runtime step caps are user/global controls, not project policy. Keep the
	// project config's other fields, but do not let ./reasonix.toml override
	// the user's execution and planner round limits.
	cfg.Agent.MaxSteps = globalMaxSteps
	cfg.Agent.PlannerMaxSteps = globalPlannerMaxSteps
	// toml.DecodeFile replaces [[plugins]] wholesale, so cfg.Plugins now holds
	// only the last file's. Re-merge by name across all sources (later wins) so a
	// project reasonix.toml doesn't drop the global config's MCP servers.
	plugins, err := mergeTOMLPlugins(tomlSources)
	if err != nil {
		return nil, err
	}
	cfg.Plugins = plugins
	if providers, providerSources, shadowedProjectProviders, ok, err := mergeTOMLProviders(tomlSources); err != nil {
		return nil, err
	} else if ok {
		cfg.Providers = providers
		cfg.providerSources = providerSources
		cfg.shadowedProjectProviders = shadowedProjectProviders
	}
	if access, ok, err := mergeTOMLProviderAccess(tomlSources); err != nil {
		return nil, err
	} else if ok {
		cfg.Desktop.ProviderAccess = access
	}

	// Claude Code's .mcp.json (project root) is read last and merged into
	// [[plugins]], so a server configured for Claude works here unchanged.
	// reasonix.toml wins on a name collision (see mergeMCPJSON).
	mcpFile := mcpJSONFile
	if root != "." {
		mcpFile = filepath.Join(root, mcpJSONFile)
	}
	entries, err := loadMCPJSON(mcpFile)
	if err != nil {
		return nil, err
	}
	cfg.mergeMCPJSON(entries)

	// Lowest priority: the v0.x ~/.reasonix/config.json's mcpServers, so upgrading
	// from the TypeScript line keeps MCP servers without rewriting them. Anything
	// the v2 config or .mcp.json already declared wins on a name collision.
	cfg.mergeMCPJSON(loadLegacyMCP(legacyConfigPath()))
	normalizePluginCommandLines(cfg)
	normalizeLegacyEffort(cfg)
	normalizeLegacyMCPTiers(cfg)
	normalizeLegacyMimoCustomProviders(cfg)
	normalizeLegacyProviderModels(cfg)
	normalizeDesktopOfficialProviderAccess(cfg)
	normalizeOfficialDeepSeekModels(cfg)
	applyDeepSeekOfficialDefaultPricing(cfg)
	backfillDeepSeekOfficialPrices(cfg)
	normalizeEffortConfig(cfg)
	backfillDeepSeekPro(cfg)
	cfg.Agent.AutoPlan = userAutoPlanMode()
	cfg.CredentialsStore = credentialsStoreMode()
	return cfg, nil
}

func userAutoPlanMode() string {
	cfg := Default()
	if uc := userConfigLoadPath(); uc != "" {
		_ = mergeFile(cfg, uc)
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Agent.AutoPlan)) {
	case "on", "ask":
		return "on"
	default:
		return "off"
	}
}

// backfillDeepSeekPro restores deepseek-pro for configs the pre-fix setup wizard
// wrote with only deepseek-v4-flash: a keyless /models probe used to drop the Pro
// SKU, leaving users unable to switch to it. In-memory only — the user's file is
// untouched. Narrowly scoped to the official DeepSeek endpoint (which is known to
// serve pro) so a custom flash-only deployment isn't given an entry that 404s.
func backfillDeepSeekPro(c *Config) {
	const flashModel, proModel = "deepseek-v4-flash", "deepseek-v4-pro"
	var flash *ProviderEntry
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Name == "deepseek-pro" {
			return
		}
		for _, m := range p.ModelList() {
			switch m {
			case proModel:
				return // pro already reachable
			case flashModel:
				if strings.Contains(p.BaseURL, "api.deepseek.com") {
					flash = p
				}
			}
		}
	}
	if flash == nil {
		return
	}
	// If the user has explicitly curated a model list for the flash provider
	// (e.g. unchecked pro in Settings), respect that choice and do not backfill.
	if len(flash.Models) > 0 {
		return
	}
	for _, bp := range Default().Providers {
		if bp.Name == "deepseek-pro" {
			bp.APIKeyEnv = flash.APIKeyEnv
			bp.Price = deepSeekV4PriceForModel(c.DeepSeekOfficialPricingLanguage(), proModel)
			c.Providers = append(c.Providers, bp)
			return
		}
	}
}

func backfillDeepSeekOfficialPrices(c *Config) {
	if c == nil {
		return
	}
	defaults := deepSeekV4PricesForConfig(c)
	for i := range c.Providers {
		p := &c.Providers[i]
		if officialProviderKind(p) != "deepseek" {
			continue
		}
		if p.Price != nil {
			continue
		}
		if p.Prices == nil {
			p.Prices = map[string]*provider.Pricing{}
		}
		for model, price := range defaults {
			if p.HasModel(model) && p.Prices[model] == nil {
				p.Prices[model] = clonePricing(price)
			}
		}
	}
}

func officialProviderKind(p *ProviderEntry) string {
	if p == nil {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(p.BaseURL))
	if err != nil {
		return ""
	}
	if strings.EqualFold(u.Hostname(), "api.deepseek.com") {
		return "deepseek"
	}
	return ""
}

func resolveRoot(root string) string {
	if root == "" || root == "." {
		return "."
	}
	return filepath.Clean(root)
}

// normalizeLegacyEffort migrates the retired DeepSeek effort="off" (the old
// /thinking off that disabled thinking) to the provider default, so a config
// written by an older version keeps loading instead of erroring on a value the
// provider no longer accepts.
func normalizeLegacyEffort(c *Config) {
	for i := range c.Providers {
		if strings.EqualFold(strings.TrimSpace(c.Providers[i].Effort), "off") {
			c.Providers[i].Effort = ""
		}
	}
}

// mergeTOMLPlugins merges [[plugins]] across TOML sources by name (later source wins).
func mergeTOMLPlugins(paths []string) ([]PluginEntry, error) {
	var merged []PluginEntry
	index := map[string]int{}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		var f Config
		if _, err := toml.DecodeFile(path, &f); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
		for _, p := range f.Plugins {
			p, _ = NormalizePluginCommandLine(p)
			if i, ok := index[p.Name]; ok {
				merged[i] = p
				continue
			}
			index[p.Name] = len(merged)
			merged = append(merged, p)
		}
	}
	return merged, nil
}

// mergeTOMLProviders merges [[providers]] across TOML sources by provider name.
// User-global providers win over same-named project providers; project providers
// only fill names the global config does not define. Keep official legacy aliases
// distinct here: they can carry different default models and effort capabilities,
// and the later desktop normalization layer handles canonical Settings access.
func mergeTOMLProviders(paths []string) ([]ProviderEntry, map[string]providerSourceScope, []ProviderEntry, bool, error) {
	var merged []ProviderEntry
	var shadowedProject []ProviderEntry
	index := map[string]int{}
	sources := map[string]providerSourceScope{}
	saw := false
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		var f Config
		if _, err := toml.DecodeFile(path, &f); err != nil {
			return nil, nil, nil, false, fmt.Errorf("config %s: %w", path, err)
		}
		if len(f.Providers) == 0 {
			continue
		}
		saw = true
		source := providerSourceForPath(path)
		for _, p := range f.Providers {
			normalizeProviderEffortFields(&p)
			key := providerMergeKey(p)
			if i, ok := index[key]; ok {
				if sources[key] == providerSourceProject && source == providerSourceUser {
					shadowedProject = append(shadowedProject, merged[i])
					merged[i] = p
					sources[key] = source
				} else if sources[key] == providerSourceUser && source == providerSourceProject {
					shadowedProject = append(shadowedProject, p)
				}
				continue
			} else {
				index[key] = len(merged)
				merged = append(merged, p)
				sources[key] = source
			}
		}
	}
	return merged, sources, shadowedProject, saw, nil
}

func providerSourceForPath(path string) providerSourceScope {
	if isUserConfigPath(path) {
		return providerSourceUser
	}
	return providerSourceProject
}

func providerMergeKey(p ProviderEntry) string {
	return strings.TrimSpace(p.Name)
}

// mergeTOMLProviderAccess merges desktop.provider_access across TOML sources so
// project desktop settings do not hide account-level providers from the desktop
// model switcher.
func mergeTOMLProviderAccess(paths []string) ([]string, bool, error) {
	var merged []string
	seen := map[string]bool{}
	saw := false
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		var f Config
		meta, err := toml.DecodeFile(path, &f)
		if err != nil {
			return nil, false, fmt.Errorf("config %s: %w", path, err)
		}
		if !meta.IsDefined("desktop", "provider_access") {
			continue
		}
		saw = true
		for _, name := range f.Desktop.ProviderAccess {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			merged = append(merged, name)
		}
	}
	return merged, saw, nil
}

// LoadForEdit 返回用于 `reasonix setup` 向导的配置。
// 将内置默认值与指定路径的文件（如果存在）合并，使重新配置时
// 保留用户现有的提供者和智能体设置，而不是重置为默认值。
// 加载 .env 以便向导在决定哪些密钥缺失时能正确解析 api_key_env。
//
// 参数：
//   - path: 配置文件路径
//
// 返回值：合并后的配置对象（加载失败时返回默认值）
func LoadForEdit(path string) *Config {
	cfg, err := loadForEditStrict(path, true)
	if err == nil {
		return cfg
	}
	slog.Warn("config: load for edit failed, using defaults", "path", path, "err", err)
	loadDotEnvForEditPath(path)
	cfg = Default()
	normalizeConfigForEdit(cfg)
	return cfg
}

// LoadForEditWithoutCredentials 与 LoadForEdit 类似，但不加载凭证文件。
// 用于不需要 API 密钥的编辑场景（如设置 UI）。
func LoadForEditWithoutCredentials(path string) *Config {
	cfg, err := loadForEditStrict(path, false)
	if err == nil {
		return cfg
	}
	slog.Warn("config: load for edit failed, using defaults", "path", path, "err", err)
	cfg = Default()
	normalizeConfigForEdit(cfg)
	return cfg
}

func loadForEditStrict(path string, loadCredentials bool) (*Config, error) {
	if loadCredentials {
		loadDotEnvForEditPath(path)
	}
	cfg := Default()
	if _, err := os.Stat(path); err == nil {
		if err := migrateLegacyMCPTiersFile(path); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
	}
	if err := mergeFile(cfg, path); err != nil {
		return nil, err
	}
	migratedMimo := normalizeConfigForEdit(cfg)
	if migratedMimo && strings.TrimSpace(path) != "" {
		if _, err := os.Stat(path); err == nil {
			if err := cfg.SaveTo(path); err != nil {
				return nil, err
			}
		}
	}
	return cfg, nil
}

func normalizeConfigForEdit(cfg *Config) bool {
	normalizePluginCommandLines(cfg)
	normalizeLegacyEffort(cfg)
	normalizeLegacyMCPTiers(cfg)
	migratedMimo := normalizeLegacyMimoCustomProviders(cfg)
	normalizeLegacyProviderModels(cfg)
	normalizeDesktopOfficialProviderAccess(cfg)
	applyDeepSeekOfficialDefaultPricing(cfg)
	backfillDeepSeekOfficialPrices(cfg)
	normalizeEffortConfig(cfg)
	return migratedMimo
}

func loadDotEnvForEditPath(path string) {
	path = strings.TrimSpace(path)
	if path == "" || isUserConfigPath(path) {
		loadDotEnv()
		return
	}
	loadDotEnvForRoot(filepath.Dir(path))
}

// mergeFile decodes a TOML file onto cfg if it exists. An absent file is not an error.
func mergeFile(cfg *Config, path string) error {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	return nil
}

func mergeRuntimeTOMLFile(cfg *Config, path string) error {
	if _, err := os.Stat(path); err == nil {
		if err := migrateLegacyMCPTiersFile(path); err != nil {
			slog.Warn("config: legacy mcp tier migration failed", "path", path, "err", err)
		}
	}
	return mergeFile(cfg, path)
}

// normalizeLegacyMCPTiers keeps loaded legacy config files on the new product
// behavior: enabled MCP servers connect in the background by default, and the
// retired per-server startup tier is no longer a user-facing setting.
func normalizeLegacyMCPTiers(c *Config) {
	if c == nil {
		return
	}
	for i := range c.Plugins {
		c.Plugins[i].Tier = ""
	}
}

func migrateLegacyMCPTiersFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	next, changed := stripLegacyMCPTierLines(string(raw))
	if !changed {
		return nil
	}
	return os.WriteFile(path, []byte(next), info.Mode().Perm())
}

func stripLegacyMCPTierLines(raw string) (string, bool) {
	lines := strings.Split(raw, "\n")
	section := ""
	changed := false
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if header := tomlSectionHeader(line); header != "" {
			section = header
		}
		if section == "plugins" && isTOMLKeyAssignment(line, "tier") {
			changed = true
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n"), changed
}

func tomlSectionHeader(line string) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "[") {
		return ""
	}
	if i := strings.Index(trimmed, "#"); i >= 0 {
		trimmed = strings.TrimSpace(trimmed[:i])
	}
	switch trimmed {
	case "[[plugins]]":
		return "plugins"
	default:
		return "other"
	}
}

func isTOMLKeyAssignment(line, key string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") || !strings.HasPrefix(trimmed, key) {
		return false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(trimmed, key))
	return strings.HasPrefix(rest, "=")
}

// normalizeLegacyProviderModels repairs provider entries written by older
// desktop builds that carried the official provider name/endpoint but omitted the
// model field. The repair is intentionally narrow: valid user-provided model
// lists are left untouched, while known official aliases get the model implied by
// their preset name so model pickers and provider validation have an option.
func normalizeLegacyProviderModels(c *Config) {
	if c == nil {
		return
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		if providerHasAnyModel(*p) {
			continue
		}
		if model := legacyOfficialProviderModel(p.Name); model != "" {
			p.Model = model
		}
	}
}

func normalizeLegacyMimoProviderCatalogs(c *Config) bool {
	if c == nil {
		return false
	}
	changed := false
	for i := range c.Providers {
		p := &c.Providers[i]
		if legacyMimoProviderName(p.Name) == "" || len(p.Models) > 0 {
			continue
		}
		switch officialProviderHost(p.BaseURL) {
		case "api.xiaomimimo.com":
			if applyLegacyMimoCatalog(p, legacyMimoAPIModels(), []string{"mimo-v2.5", "mimo-v2-omni"}, "mimo-v2.5-pro") {
				changed = true
			}
		case "token-plan-cn.xiaomimimo.com":
			if applyLegacyMimoCatalog(p, legacyMimoTokenPlanModels(), []string{"mimo-v2.5"}, "mimo-v2.5-pro") {
				changed = true
			}
		}
	}
	return changed
}

func applyLegacyMimoCatalog(p *ProviderEntry, models, visionModels []string, fallbackDefault string) bool {
	if p == nil || len(models) == 0 {
		return false
	}
	beforeModels := append([]string(nil), p.Models...)
	beforeVision := append([]string(nil), p.VisionModels...)
	beforeDefault := p.Default
	beforeModel := p.Model
	beforeWindow := p.ContextWindow
	beforeNoProxy := p.NoProxy
	beforePricesLen := len(p.Prices)

	currentDefault := strings.TrimSpace(p.Default)
	if currentDefault == "" {
		currentDefault = strings.TrimSpace(p.Model)
	}
	p.Models = mergeModelLists(models, p.ModelList())
	p.Model = p.Models[0]
	p.Default = firstKnownModel(currentDefault, p.Models, fallbackDefault)
	p.VisionModels = mergeModelLists(visionModels, p.VisionModels)
	backfillOfficialContextWindow(p, 1_048_576)
	p.NoProxy = true
	if p.Prices == nil {
		p.Prices = mimoDomesticPrices(models)
	} else {
		for model, price := range mimoDomesticPrices(models) {
			if p.Prices[model] == nil {
				p.Prices[model] = price
			}
		}
	}

	return !stringSlicesEqual(beforeModels, p.Models) ||
		!stringSlicesEqual(beforeVision, p.VisionModels) ||
		beforeDefault != p.Default ||
		beforeModel != p.Model ||
		beforeWindow != p.ContextWindow ||
		beforeNoProxy != p.NoProxy ||
		beforePricesLen != len(p.Prices)
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func normalizeOfficialDeepSeekModels(c *Config) {
	if c == nil {
		return
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		if officialProviderHost(p.BaseURL) != "api.deepseek.com" {
			continue
		}
		switch strings.TrimSpace(p.Name) {
		case "deepseek":
			ensureProviderModels(p, []string{"deepseek-v4-flash", "deepseek-v4-pro"}, "deepseek-v4-flash")
		case "deepseek-flash":
			ensureProviderModels(p, []string{"deepseek-v4-flash"}, "deepseek-v4-flash")
		case "deepseek-pro":
			ensureProviderModels(p, []string{"deepseek-v4-pro"}, "deepseek-v4-pro")
		}
	}
}

func officialProviderHost(baseURL string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func ensureProviderModels(p *ProviderEntry, required []string, fallbackDefault string) {
	if p == nil {
		return
	}
	// If the user has explicitly curated a model list (via Settings), respect
	// that choice and do not merge additional required models.
	if len(p.Models) > 0 {
		return
	}
	models := mergeModelLists(required, p.ModelList())
	if len(models) == 0 {
		return
	}
	p.Model = models[0]
	if len(models) > 1 {
		p.Models = models
		p.Default = firstKnownModel(p.Default, models, fallbackDefault)
		return
	}
	p.Models = nil
	p.Default = ""
}

func legacyOfficialProviderModel(name string) string {
	switch strings.TrimSpace(name) {
	case "deepseek-flash":
		return "deepseek-v4-flash"
	case "deepseek-pro":
		return "deepseek-v4-pro"
	case "mimo", "xiaomi-mimo", "xiaomi_mimo", "mimo-api", "mimo-token-plan", "mimo-pro":
		return "mimo-v2.5-pro"
	case "mimo-flash":
		return "mimo-v2.5"
	default:
		return ""
	}
}

func normalizeLegacyMimoCustomProviders(c *Config) bool {
	return normalizeLegacyMimoCustomProvidersForRefs(c, legacyMimoConfigRefs(c)...)
}

// NormalizeLegacyMimoCustomProvidersForRefs 追加旧版引用所需的自定义 OpenAI 兼容 MiMo 提供者，
// 这些引用位于 reasonix.toml 之外（如恢复的桌面标签页状态）。
//
// 参数：
//   - c: 配置对象（会被修改）
//   - refs: 模型引用列表
//
// 返回值：true 表示配置被修改
func NormalizeLegacyMimoCustomProvidersForRefs(c *Config, refs ...string) bool {
	return normalizeLegacyMimoCustomProvidersForRefs(c, refs...)
}

func normalizeLegacyMimoCustomProvidersForRefs(c *Config, refs ...string) bool {
	if c == nil {
		return false
	}
	needed := map[string]bool{}
	addRef := func(ref string) {
		if name := legacyMimoProviderNameForRef(ref); name != "" {
			needed[name] = true
		}
	}
	for _, ref := range refs {
		addRef(ref)
	}
	changed := normalizeLegacyMimoProviderCatalogs(c)
	for name := range needed {
		if _, ok := c.Provider(name); ok {
			continue
		}
		c.Providers = append(c.Providers, legacyMimoCustomProvider(name))
		changed = true
	}
	if normalizeLegacyMimoProviderCatalogs(c) {
		changed = true
	}
	return changed
}

func legacyMimoConfigRefs(c *Config) []string {
	if c == nil {
		return nil
	}
	refs := []string{
		c.DefaultModel,
		c.Agent.PlannerModel,
		c.Agent.SubagentModel,
		c.Agent.AutoPlanClassifier,
		c.Bot.Model,
	}
	for _, ref := range c.Agent.SubagentModels {
		refs = append(refs, ref)
	}
	for _, conn := range c.Bot.Connections {
		refs = append(refs, conn.Model)
	}
	refs = append(refs, c.Desktop.ProviderAccess...)
	return refs
}

func legacyMimoProviderName(ref string) string {
	switch strings.TrimSpace(ref) {
	case "mimo", "xiaomi-mimo", "xiaomi_mimo", "mimo-api", "mimo-token-plan", "mimo-pro", "mimo-flash":
		return strings.TrimSpace(ref)
	default:
		return ""
	}
}

func legacyMimoProviderNameForRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	providerName, _, hasModel := strings.Cut(ref, "/")
	if name := legacyMimoProviderName(providerName); name != "" {
		return name
	}
	if hasModel {
		return ""
	}
	switch ref {
	case "mimo-v2.5-pro":
		return "mimo-pro"
	case "mimo-v2.5":
		return "mimo-flash"
	case "mimo-v2-omni":
		return "mimo-api"
	default:
		return ""
	}
}

func legacyMimoAPIModels() []string {
	return []string{"mimo-v2.5-pro", "mimo-v2.5", "mimo-v2-omni"}
}

func legacyMimoTokenPlanModels() []string {
	return []string{"mimo-v2.5-pro", "mimo-v2.5"}
}

func legacyMimoCustomProvider(name string) ProviderEntry {
	switch strings.TrimSpace(name) {
	case "mimo", "xiaomi-mimo", "xiaomi_mimo", "mimo-api":
		models := legacyMimoAPIModels()
		return ProviderEntry{
			Name:          strings.TrimSpace(name),
			Kind:          "openai",
			BaseURL:       "https://api.xiaomimimo.com/v1",
			Models:        models,
			VisionModels:  []string{"mimo-v2.5", "mimo-v2-omni"},
			Default:       "mimo-v2.5-pro",
			APIKeyEnv:     "MIMO_API_KEY",
			ContextWindow: 1_048_576,
			Prices:        mimoDomesticPrices(models),
			NoProxy:       true,
		}
	case "mimo-token-plan":
		models := legacyMimoTokenPlanModels()
		return ProviderEntry{
			Name:          "mimo-token-plan",
			Kind:          "openai",
			BaseURL:       "https://token-plan-cn.xiaomimimo.com/v1",
			Models:        models,
			VisionModels:  []string{"mimo-v2.5"},
			Default:       "mimo-v2.5-pro",
			APIKeyEnv:     "MIMO_API_KEY",
			ContextWindow: 1_048_576,
			Prices:        mimoDomesticPrices(models),
			NoProxy:       true,
		}
	case "mimo-flash":
		return ProviderEntry{Name: "mimo-flash", Kind: "openai", BaseURL: "https://token-plan-cn.xiaomimimo.com/v1", Model: "mimo-v2.5", APIKeyEnv: "MIMO_API_KEY", ContextWindow: 1_000_000, Price: mimoV25Price(), NoProxy: true}
	default:
		return ProviderEntry{Name: "mimo-pro", Kind: "openai", BaseURL: "https://token-plan-cn.xiaomimimo.com/v1", Model: "mimo-v2.5-pro", APIKeyEnv: "MIMO_API_KEY", ContextWindow: 1_000_000, Price: mimoV25ProPrice(), NoProxy: true}
	}
}

func normalizeDesktopOfficialProviderAccess(c *Config) {
	if c == nil || len(c.Desktop.ProviderAccess) == 0 {
		return
	}
	seen := desktopProviderAccessMap(nil)
	next := make([]string, 0, len(c.Desktop.ProviderAccess))
	for _, name := range c.Desktop.ProviderAccess {
		name = desktopProviderAccessNameForConfig(c, name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		next = append(next, name)
	}
	c.Desktop.ProviderAccess = next
	if seen["deepseek"] {
		ensureDeepSeekOfficialProvider(c)
	}
	normalizeLegacyMimoProviderCatalogs(c)
	retargetDesktopOfficialRefs(c, seen)
}

// NormalizeLegacyDesktopProviderAccess 为 Settings 跟踪显式提供者访问之前
// 编写的配置种子化桌面提供者访问列表。调用方应仅在确定 TOML 未声明
// provider_access 时使用此函数；显式空列表表示用户移除了所有访问条目。
//
// 参数：
//   - c: 配置对象（会被修改）
func NormalizeLegacyDesktopProviderAccess(c *Config) {
	if c == nil || len(c.Desktop.ProviderAccess) > 0 {
		return
	}
	seen := desktopProviderAccessMap(nil)
	var access []string
	add := func(name string) {
		name = desktopProviderAccessNameForConfig(c, name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		access = append(access, name)
	}
	addRef := func(ref string) {
		if entry, ok := c.ResolveModel(ref); ok {
			if !entry.Configured() {
				return
			}
			add(entry.Name)
		}
	}
	addRef(c.DefaultModel)
	addRef(c.Agent.PlannerModel)
	addRef(c.Agent.SubagentModel)
	addRef(c.Agent.AutoPlanClassifier)
	for _, ref := range c.Agent.SubagentModels {
		addRef(ref)
	}
	addRef(c.Bot.Model)
	for _, conn := range c.Bot.Connections {
		addRef(conn.Model)
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		if legacyMimoProviderName(p.Name) != "" && len(p.ModelList()) > 0 {
			add(p.Name)
			continue
		}
		if p.Configured() && len(p.ModelList()) > 0 {
			add(p.Name)
		}
	}
	if len(access) == 0 {
		return
	}
	c.Desktop.ProviderAccess = access
	normalizeDesktopOfficialProviderAccess(c)
}

func canonicalDesktopOfficialProviderName(name string) string {
	switch strings.TrimSpace(name) {
	case "deepseek-flash", "deepseek-pro":
		return "deepseek"
	default:
		return strings.TrimSpace(name)
	}
}

func desktopProviderAccessNameForConfig(c *Config, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	canonical := canonicalDesktopOfficialProviderName(name)
	if canonical == name {
		return name
	}
	if c == nil {
		return canonical
	}
	if p, ok := c.Provider(name); ok && !providerEntryMatchesCanonicalOfficialAccess(p, canonical) {
		return name
	}
	return canonical
}

func providerEntryMatchesCanonicalOfficialAccess(p *ProviderEntry, canonical string) bool {
	if p == nil {
		return false
	}
	switch canonical {
	case "deepseek":
		return officialProviderKind(p) == "deepseek"
	default:
		return false
	}
}

// CanonicalDesktopOfficialProviderName 返回内置官方提供者别名的 Settings Center 提供者 ID。
// 例如 "deepseek-flash" → "deepseek"。
//
// 参数：
//   - name: 提供者名称
//
// 返回值：规范化的提供者 ID
func CanonicalDesktopOfficialProviderName(name string) string {
	return canonicalDesktopOfficialProviderName(name)
}

func desktopProviderAccessMap(names []string) map[string]bool {
	out := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name != "" {
			out[name] = true
		}
	}
	return out
}

func ensureDeepSeekOfficialProvider(c *Config) {
	if p, ok := c.Provider("deepseek"); ok {
		if officialProviderKind(p) == "deepseek" {
			backfillOfficialContextWindow(p, 1_000_000)
		}
		return
	}
	entry := ProviderEntry{
		Name:          "deepseek",
		Kind:          "openai",
		BaseURL:       "https://api.deepseek.com",
		Models:        []string{"deepseek-v4-flash", "deepseek-v4-pro"},
		Default:       "deepseek-v4-flash",
		APIKeyEnv:     "DEEPSEEK_API_KEY",
		BalanceURL:    "https://api.deepseek.com/user/balance",
		ContextWindow: 1_000_000,
		Prices:        deepSeekV4PricesForConfig(c),
	}
	if old, ok := c.Provider("deepseek-flash"); ok {
		entry = officialProviderFromLegacy(entry, old)
		entry.Prices = deepSeekV4PricesForConfig(c)
		entry.Models = mergeModelLists([]string{"deepseek-v4-flash", "deepseek-v4-pro"}, old.ModelList())
		entry.Default = firstKnownModel(entry.Default, entry.Models, "deepseek-v4-flash")
	}
	backfillOfficialContextWindow(&entry, 1_000_000)
	c.Providers = append(c.Providers, entry)
}

func isOpenAIProviderKind(e *ProviderEntry) bool {
	return e != nil && strings.EqualFold(strings.TrimSpace(e.Kind), "openai")
}

func mergeCuratedModelsIntoProvider(e *ProviderEntry, models []string, fallback string) {
	// If the user has explicitly curated a model list (via Settings), respect
	// that choice and do not merge additional curated models.
	if len(e.Models) > 0 {
		return
	}
	currentDefault := e.Default
	if strings.TrimSpace(currentDefault) == "" {
		currentDefault = e.Model
	}
	e.Models = mergeModelLists(models, e.ModelList())
	e.Default = firstKnownModel(currentDefault, e.Models, fallback)
}

func backfillOfficialContextWindow(e *ProviderEntry, fallback int) {
	if e != nil && e.ContextWindow <= 0 {
		e.ContextWindow = fallback
	}
}

func officialProviderFromLegacy(entry ProviderEntry, old *ProviderEntry) ProviderEntry {
	entry.Kind = old.Kind
	entry.BaseURL = old.BaseURL
	entry.ModelsURL = old.ModelsURL
	entry.APIKeyEnv = old.APIKeyEnv
	entry.BalanceURL = old.BalanceURL
	entry.ContextWindow = old.ContextWindow
	entry.Price = old.Price
	entry.Thinking = old.Thinking
	entry.Effort = old.Effort
	entry.ReasoningProtocol = old.ReasoningProtocol
	entry.SupportedEfforts = append([]string(nil), old.SupportedEfforts...)
	entry.DefaultEffort = old.DefaultEffort
	entry.NoProxy = old.NoProxy
	return entry
}

func mergeModelLists(primary, extra []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(primary)+len(extra))
	for _, list := range [][]string{primary, extra} {
		for _, model := range list {
			model = strings.TrimSpace(model)
			if model == "" || seen[model] {
				continue
			}
			seen[model] = true
			out = append(out, model)
		}
	}
	return out
}

func firstKnownModel(current string, models []string, fallback string) string {
	current = strings.TrimSpace(current)
	for _, model := range models {
		if model == current {
			return current
		}
	}
	for _, model := range models {
		if model == fallback {
			return fallback
		}
	}
	if len(models) > 0 {
		return models[0]
	}
	return ""
}

func retargetDesktopOfficialRefs(c *Config, access map[string]bool) {
	c.DefaultModel = retargetDesktopOfficialRef(c.DefaultModel, access)
	c.Agent.PlannerModel = retargetDesktopOfficialRef(c.Agent.PlannerModel, access)
	c.Agent.SubagentModel = retargetDesktopOfficialRef(c.Agent.SubagentModel, access)
	c.Agent.AutoPlanClassifier = retargetDesktopOfficialRef(c.Agent.AutoPlanClassifier, access)
	for skill, ref := range c.Agent.SubagentModels {
		c.Agent.SubagentModels[skill] = retargetDesktopOfficialRef(ref, access)
	}
}

func retargetDesktopOfficialRef(ref string, access map[string]bool) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	provider, model, hasModel := strings.Cut(ref, "/")
	switch provider {
	case "deepseek-flash":
		if !access["deepseek"] {
			return ref
		}
		if !hasModel || strings.TrimSpace(model) == "" {
			model = "deepseek-v4-flash"
		}
		return "deepseek/" + model
	case "deepseek-pro":
		if !access["deepseek"] {
			return ref
		}
		if !hasModel || strings.TrimSpace(model) == "" {
			model = "deepseek-v4-pro"
		}
		return "deepseek/" + model
	default:
		return ref
	}
}

func userConfigPath() string {
	dir := userConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "config.toml")
}

func userConfigDir() string {
	return reasonixHomeDir()
}

func reasonixHomeDir() string {
	if dir := cleanEnvDir("REASONIX_HOME"); dir != "" {
		return dir
	}
	if runtime.GOOS != "windows" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, ".reasonix")
		}
		return ""
	}
	dir := osUserConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "reasonix")
}

func userConfigLoadPath() string {
	primary := userConfigPath()
	if primary == "" {
		return legacyUserConfigPath()
	}
	if _, err := os.Stat(primary); err == nil {
		return primary
	}
	if legacy := legacyUserConfigPath(); legacy != "" {
		if _, err := os.Stat(legacy); err == nil {
			return legacy
		}
	}
	for _, legacy := range legacyXDGConfigPaths() {
		if legacy == "" || samePath(legacy, primary) {
			continue
		}
		if _, err := os.Stat(legacy); err == nil {
			return legacy
		}
	}
	return primary
}

func legacyUserConfigPath() string {
	dir := legacyOSSupportDir()
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, "config.toml")
	if primary := userConfigPath(); primary != "" && samePath(path, primary) {
		return ""
	}
	return path
}

func userConfigCandidatePaths() []string {
	var paths []string
	if p := userConfigPath(); p != "" {
		paths = append(paths, p)
	}
	if p := legacyUserConfigPath(); p != "" {
		paths = append(paths, p)
	}
	paths = append(paths, legacyXDGConfigPaths()...)
	return paths
}

func legacyXDGConfigPaths() []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	seen := map[string]bool{}
	var paths []string
	add := func(path string) {
		if path == "" {
			return
		}
		path = filepath.Clean(path)
		if seen[path] {
			return
		}
		seen[path] = true
		paths = append(paths, path)
	}
	if dir := cleanEnvDir("XDG_CONFIG_HOME"); dir != "" {
		add(filepath.Join(dir, "reasonix", "config.toml"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(filepath.Join(home, ".config", "reasonix", "config.toml"))
	}
	return paths
}

func userSupportDir() string {
	if dir := cleanEnvDir("REASONIX_STATE_HOME"); dir != "" {
		return dir
	}
	return reasonixHomeDir()
}

func legacyOSSupportDir() string {
	dir := osUserConfigDir()
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, "reasonix")
	if current := reasonixHomeDir(); current != "" && samePath(path, current) {
		return ""
	}
	return path
}

func userCacheDir() string {
	if dir := cleanEnvDir("REASONIX_CACHE_HOME"); dir != "" {
		return dir
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "reasonix")
}

func osUserConfigDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return dir
}

func cleanEnvDir(name string) string {
	dir := strings.TrimSpace(os.Getenv(name))
	if dir == "" {
		return ""
	}
	dir = ExpandVars(dir)
	if dir == "~" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			dir = home
		}
	} else if strings.HasPrefix(dir, "~/") || strings.HasPrefix(dir, `~\`) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			dir = filepath.Join(home, dir[2:])
		}
	}
	if !filepath.IsAbs(dir) {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
	}
	return filepath.Clean(dir)
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	aa, aerr := filepath.Abs(a)
	bb, berr := filepath.Abs(b)
	if aerr == nil {
		a = aa
	}
	if berr == nil {
		b = bb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// userConfigDisplayPath is userConfigPath collapsed to a ~-relative form for
// comments rendered into the user's own config.toml, so Windows users see the
// real location instead of a hardcoded ~/.reasonix path.
func userConfigDisplayPath() string {
	p := userConfigPath()
	if p == "" {
		return "<os-config-dir>/reasonix/config.toml"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return p
}

// UserConfigPath 返回用户全局 config.toml 的路径。
// 位于 Reasonix 主目录下：REASONIX_HOME/config.toml，
// Unix 系统上为 ~/.reasonix/config.toml，
// Windows 上为 %AppData%/reasonix/config.toml。
// 无法解析用户配置目录时返回空字符串。
func UserConfigPath() string { return userConfigPath() }

// LegacyUserConfigPath is the old OS app-support config.toml path when it
// differs from UserConfigPath. It is read as a compatibility fallback when the
// primary user config does not exist.
func LegacyUserConfigPath() string { return legacyUserConfigPath() }

// LegacyUserConfigPaths returns every known legacy user config path that differs
// from the current v1.8.1 Reasonix-home config path.
func LegacyUserConfigPaths() []string {
	primary := userConfigPath()
	var out []string
	add := func(path string) {
		if path == "" || samePath(path, primary) {
			return
		}
		for _, existing := range out {
			if samePath(existing, path) {
				return
			}
		}
		out = append(out, path)
	}
	add(legacyUserConfigPath())
	for _, path := range legacyXDGConfigPaths() {
		add(path)
	}
	return out
}

// ReasonixHomeDir 返回当前的 Reasonix 主目录。
// 优先使用 REASONIX_HOME 环境变量，否则：
//   - macOS/Linux: ~/.reasonix
//   - Windows: %APPDATA%/reasonix
func ReasonixHomeDir() string { return reasonixHomeDir() }

// UserCredentialsPath 返回 Reasonix 全局凭证文件的路径。
// 位于 Reasonix 主目录下，包含 KEY=value 行，由 loadDotEnv 加载到环境变量中。
// 设置向导将 API 密钥写入此文件，故意不命名为 .env：
// 密钥永远不会落入项目的 .env（无法选择性 gitignore），
// 永远不会被提交，并且从任何工作目录都能解析。
// 无法解析 Reasonix 主目录时返回空字符串。
func UserCredentialsPath() string {
	dir := userSupportDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "credentials")
}

// ArchiveDir 返回压缩对话历史的归档目录（每次压缩一个带时间戳的 .jsonl 文件）。
// 用于可追溯性。无法解析用户状态目录时返回空字符串，此时跳过归档。
func ArchiveDir() string {
	dir := userSupportDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "archive")
}

// SessionDir 返回聊天会话的持久化目录（每个会话一个 .jsonl 文件）。
// 由 `reasonix --continue` / `--resume` 用于查找最近的会话。
// 无法解析用户状态目录时返回空字符串——此时会话不会被保存。
func SessionDir() string {
	dir := userSupportDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "sessions")
}

// ProjectSessionDir is the per-workspace session directory the desktop sidebar
// lists: <state root>/projects/<slug>/sessions. Empty when either the state root
// or workspaceRoot doesn't resolve.
func ProjectSessionDir(workspaceRoot string) string {
	base := MemoryUserDir()
	root := strings.TrimSpace(workspaceRoot)
	if base == "" || root == "" {
		return ""
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return filepath.Join(base, "projects", WorkspaceSlug(root), "sessions")
}

// WorkspaceSlug 将绝对工作区路径扁平化为 <config root>/projects 下使用的目录名。
// 路径分隔符和特殊字符被替换为连字符。
//
// 参数：
//   - absPath: 绝对工作区路径
//
// 返回值：扁平化的目录名
func WorkspaceSlug(absPath string) string {
	return strings.NewReplacer(string(os.PathSeparator), "-", "/", "-", "\\", "-", ":", "-").Replace(absPath)
}

// CacheDir 返回每用户的缓存根目录，用于可重新生成的产物：
// MCP 握手快照、插件启动延迟遥测等。OS 目录不可用时返回空字符串——
// 调用方必须容忍这种情况（缓存是尽力而为的）。
func CacheDir() string {
	dir := userCacheDir()
	if dir == "" {
		return ""
	}
	return dir
}

// MemoryUserDir 返回 Reasonix 用户状态根目录（…/reasonix），
// 用户全局 REASONIX.md 和每项目的自动记忆存储都在此目录下。
// 无法解析用户状态目录时返回空字符串，此时禁用用户范围的记忆功能。
func MemoryUserDir() string {
	return userSupportDir()
}

// ConventionDirs 是扫描智能体资产（技能、命令）的父目录列表，按规范优先顺序排列。
// .reasonix 是我们的目录；.agents / .agent / .claude 让用户可以直接放入
// 为其他智能体工具编写的资产，无需移动文件。技能（internal/skill）和命令（CommandDirs）
// 共享此列表以发现相同的目录集。
//
// 注意：钩子（hooks）不在这些目录中扫描——.claude/settings.json 使用不同的钩子架构，
// 无法解析为我们的格式，因此钩子保留在 .reasonix/settings.json 中（见 internal/hook）。
var ConventionDirs = []string{".reasonix", ".agents", ".agent", ".claude"}

// conventionSubdirsAsc joins sub under each ConventionDir of base, in ascending
// priority (reverse of ConventionDirs) so the canonical .reasonix ends up the
// highest-priority entry — command.Load lets a later directory win on a clash.
func conventionSubdirsAsc(base, sub string) []string {
	out := make([]string, 0, len(ConventionDirs))
	for i := len(ConventionDirs) - 1; i >= 0; i-- {
		out = append(out, filepath.Join(base, ConventionDirs[i], sub))
	}
	return out
}

// CommandDirs 返回扫描自定义斜杠命令的目录列表，最低优先级在前，
// 使后面的（更具体的）目录在名称冲突时覆盖前面的。
//
// 扫描顺序：
//  1. 主目录约定目录（~/.claude/commands ... ~/.reasonix/commands）
//  2. Reasonix 主目录的 commands 目录
//  3. 旧版 OS 应用支持目录（如果不同）
//  4. 项目的约定目录（.claude/commands ... .reasonix/commands）
//
// 扫描 .claude / .agents / .agent 目录使为其他智能体工具编写的命令
// （相同的 .md + frontmatter 格式）可以在此 unchanged 使用。
func CommandDirs() []string {
	return CommandDirsForRoot(".")
}

// CommandDirsForRoot is like CommandDirs but resolves the project convention
// dirs under root instead of the current working directory. Global dirs are
// unchanged — they are always user-scoped.
func CommandDirsForRoot(root string) []string {
	root = resolveRoot(root)
	var dirs []string
	add := func(dir string) {
		if dir == "" {
			return
		}
		for _, existing := range dirs {
			if samePath(existing, dir) {
				return
			}
		}
		dirs = append(dirs, dir)
	}
	if dir := legacyOSSupportDir(); dir != "" {
		add(filepath.Join(dir, "commands"))
	}
	for _, legacy := range legacyXDGConfigPaths() {
		add(filepath.Join(filepath.Dir(legacy), "commands"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, dir := range conventionSubdirsAsc(home, "commands") {
			add(dir)
		}
	}
	if dir := userConfigDir(); dir != "" {
		add(filepath.Join(dir, "commands"))
	}
	if dir := userSupportDir(); dir != "" && !samePath(dir, userConfigDir()) {
		add(filepath.Join(dir, "commands"))
	}
	for _, dir := range conventionSubdirsAsc(root, "commands") {
		add(dir)
	}
	return dirs
}

// SourcePath 返回存在的最高优先级配置文件路径，不存在则返回空字符串。
// 优先级：项目级 reasonix.toml > 用户级 config.toml。
func SourcePath() string {
	return SourcePathForRoot(".")
}

// SourcePathForRoot returns the highest-priority config file that exists under
// root, or "" if none. Equivalent to SourcePath() when root is ".".
func SourcePathForRoot(root string) string {
	root = resolveRoot(root)
	projectTOML := "reasonix.toml"
	if root != "." {
		projectTOML = filepath.Join(root, "reasonix.toml")
	}
	if _, err := os.Stat(projectTOML); err == nil {
		return projectTOML
	}
	if uc := userConfigLoadPath(); uc != "" {
		if _, err := os.Stat(uc); err == nil {
			return uc
		}
	}
	return ""
}

// WriteFile 将配置以带注释的 TOML 格式写入指定路径。
// 写入是原子性的（atomic + fsync），确保中断的写入或断电
// 永远不会将主配置截断为无法解析的状态（避免应用无可用模型）。
//
// 参数：
//   - path: 写入路径
//
// 返回值：写入失败时返回错误
func (c *Config) WriteFile(path string) error {
	return fileutil.AtomicWriteFile(path, []byte(RenderTOMLForScope(c, renderScopeForPath(path))), configFilePerm(path))
}

// Provider 根据名称查找提供者条目。
//
// 参数：
//   - name: 提供者名称（如 "deepseek"）
//
// 返回值：
//   - *ProviderEntry: 找到的提供者条目指针
//   - bool: 是否找到
func (c *Config) Provider(name string) (*ProviderEntry, bool) {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i], true
		}
	}
	return nil, false
}

// ResolveModel 将模型引用解析为提供者条目（Model 字段为选中的模型字符串）。
// 返回的是副本，配置的列表保持不变。
//
// 支持的引用格式：
//   - "provider/model" — 该提供者下的指定模型（如 "deepseek/deepseek-v4-flash"）
//   - 提供者名称 — 该提供者的默认模型（如 "deepseek"）
//   - 裸模型名称 — 列出该模型的第一个提供者（如 "deepseek-v4-flash"）
//
// 返回的条目可直接用于构建提供者（NewProvider 读取 .Model），
// 因此单个"多模型供应商"条目可以为每个模型生成一个实例，
// 无需复制 base_url/api_key_env。
//
// 参数：
//   - ref: 模型引用字符串
//
// 返回值：
//   - *ProviderEntry: 解析后的提供者条目（Model 已设置为目标模型）
//   - bool: 是否成功解析
func (c *Config) ResolveModel(ref string) (*ProviderEntry, bool) {
	if ref == "" {
		return nil, false
	}
	if access := desktopProviderAccessMap(c.Desktop.ProviderAccess); len(access) > 0 {
		ref = retargetDesktopOfficialRef(ref, access)
	}
	// "provider/model"
	if prov, model, ok := strings.Cut(ref, "/"); ok {
		if e, found := c.Provider(prov); found && e.HasModel(model) {
			cp := *e
			cp.Model = model
			cp.applyModelPrice()
			return &cp, true
		}
	}
	// a provider name → its default model
	if e, found := c.Provider(ref); found {
		cp := *e
		cp.Model = e.DefaultModel()
		cp.applyModelPrice()
		return &cp, true
	}
	// a bare model name → the provider that lists it
	for i := range c.Providers {
		if c.Providers[i].HasModel(ref) {
			cp := c.Providers[i]
			cp.Model = ref
			cp.applyModelPrice()
			return &cp, true
		}
	}
	return nil, false
}

// ResolveModelWithFallback 将模型引用解析为桌面运行时使用的规范 "provider/model" 形式。
// 如果引用过期或为空，先尝试用户配置的 default_model，
// 再回退到第一个已配置的提供者——确保用户偏好不被迭代顺序覆盖。
//
// 参数：
//   - ref: 模型引用字符串
//
// 返回值：
//   - resolvedRef: 解析后的规范引用（如 "deepseek/deepseek-v4-flash"）
//   - fallback: 是否使用了回退（即 ref 本身解析失败）
//   - ok: 是否成功解析
func (c *Config) ResolveModelWithFallback(ref string) (resolvedRef string, fallback bool, ok bool) {
	ref = strings.TrimSpace(ref)
	if ref != "" {
		if e, found := c.ResolveModel(ref); found {
			return e.Name + "/" + e.Model, false, true
		}
	}
	// Before falling back to the first configured provider (which may not be the
	// user's preferred choice), try the configured default_model.  Skip when ref
	// already WAS the DefaultModel (it already failed above, so retrying won't
	// help) or when the default provider has no API key configured.
	if ref != c.DefaultModel && c.DefaultModel != "" {
		if e, found := c.ResolveModel(c.DefaultModel); found && e.Configured() {
			return e.Name + "/" + e.Model, true, true
		}
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		// Skip providers with no models or no API key: falling back onto a keyless
		// provider just boots the tab onto something that fails on first use. Mirrors
		// the Configured() gate the provider-removal/selection paths already apply.
		if len(p.ModelList()) == 0 || !p.Configured() {
			continue
		}
		return p.Name + "/" + p.DefaultModel(), true, true
	}
	return "", false, false
}

// APIKey 从 api_key_env 环境变量解析提供者的 API 密钥。
//
// 返回值：API 密钥字符串（未设置则返回空字符串）
func (e *ProviderEntry) APIKey() string {
	if e.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(e.APIKeyEnv)
}

// RequiresAPIKey 检查此提供者是否需要 API 密钥。
// 空的 api_key_env 表示提供者故意不需要认证（如本地网关）。
// 回环/私有端点也允许在没有解析到密钥的情况下运行。
//
// 返回值：true 表示需要 API 密钥
func (e *ProviderEntry) RequiresAPIKey() bool {
	if e == nil {
		return false
	}
	if strings.TrimSpace(e.APIKeyEnv) == "" {
		return providerBaseURLRequiresAPIKey(e.BaseURL)
	}
	return !providerBaseURLAllowsMissingAPIKey(e.BaseURL)
}

func providerBaseURLRequiresAPIKey(raw string) bool {
	switch officialProviderHost(raw) {
	case "api.deepseek.com", "api.xiaomimimo.com", "token-plan-cn.xiaomimimo.com", "api.minimaxi.com", "api.openai.com":
		return true
	default:
		return false
	}
}

func providerBaseURLAllowsMissingAPIKey(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := strings.Trim(strings.ToLower(u.Hostname()), "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast()
}

// Configured 检查提供者是否可选（即已正确配置）。
// 不需要 API 密钥的提供者天然已配置；需要密钥的提供者要求环境变量已设置，
// 除非端点是本地/私有的。
//
// 返回值：true 表示提供者已配置可用
func (e *ProviderEntry) Configured() bool {
	return e != nil && (!e.RequiresAPIKey() || e.APIKey() != "")
}

// ResolveSystemPrompt 返回系统提示文本。
// 如果设置了 system_prompt_file，则从文件读取；否则使用配置中的 system_prompt。
// 两者都为空时返回 DefaultSystemPrompt。
//
// 返回值：
//   - string: 系统提示文本
//   - error: 文件读取失败时返回错误
func (c *Config) ResolveSystemPrompt() (string, error) {
	return c.ResolveSystemPromptForRoot(".")
}

// ResolveSystemPromptForRoot 与 ResolveSystemPrompt 类似，但将相对的
// system_prompt_file 解析为相对于 root 的路径。桌面标签页在此传入其工作区根目录，
// 使提示文件按项目范围解析，即使进程 cwd 在其他地方。
//
// 参数：
//   - root: 项目根目录
//
// 返回值：
//   - string: 系统提示文本
//   - error: 文件读取失败时返回错误
func (c *Config) ResolveSystemPromptForRoot(root string) (string, error) {
	if c.Agent.SystemPromptFile != "" {
		path := c.Agent.SystemPromptFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(resolveRoot(root), path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("system_prompt_file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if strings.TrimSpace(c.Agent.SystemPrompt) == "" {
		return DefaultSystemPrompt, nil
	}
	return c.Agent.SystemPrompt, nil
}

// Validate 检查所选模型的提供者是否可用。
// 验证内容：模型是否存在、kind 和 base_url 是否已设置、API 密钥是否已配置。
//
// 参数：
//   - model: 模型引用字符串
//
// 返回值：验证失败时返回描述性错误
func (c *Config) Validate(model string) error {
	e, ok := c.ResolveModel(model)
	if !ok {
		return fmt.Errorf("unknown model %q (configured: %s)", model, c.providerNames())
	}
	if e.Kind == "" {
		return fmt.Errorf("provider %q: kind is required", model)
	}
	if e.BaseURL == "" {
		return fmt.Errorf("provider %q: base_url is required", model)
	}
	if e.RequiresAPIKey() && e.APIKey() == "" {
		return fmt.Errorf("provider %q: missing env %s", model, e.APIKeyEnv)
	}
	return nil
}

func (c *Config) providerNames() string {
	names := make([]string, len(c.Providers))
	for i, p := range c.Providers {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}
