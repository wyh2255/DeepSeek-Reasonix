// acp.go 实现了 ACP (Agent Client Protocol) 代理模式。
// 该文件使 Reasonix 可以作为 stdio JSON-RPC 服务器运行，供编辑器和其他宿主客户端驱动
// （通过 initialize、session/new、session/prompt、session/cancel 等 RPC 方法）。
// 它保持与 v1 版本通过 ACP 集成的众多工具的线路兼容性。
//
// 核心职责：
//   - 解析命令行参数并启动 ACP 服务
//   - 通过 acpFactory 为每个 ACP 会话构建 control.Controller
//   - 提供模型选择、努力级别（effort）等配置选项
//   - 管理 MCP 服务器和子代理（subagent）提供者解析
package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/acp"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/i18n"
	"reasonix/internal/netclient"
	"reasonix/internal/provider"
	"reasonix/internal/sandbox"
	"reasonix/internal/tool"
	"reasonix/internal/tool/builtin"
)

// acpCommand 运行 Reasonix 作为 Agent Client Protocol 代理：一个 stdio JSON-RPC 服务器，
// 由编辑器和其他宿主客户端驱动（initialize、session/new、session/prompt、session/cancel）。
// 它保持与 v1 版本通过 ACP 集成的众多工具的线路兼容性。
//
// stdin/stdout 是 JSON-RPC 通道——不允许其他内容写入 stdout，因此所有诊断信息输出到 stderr。
// 每个会话由 acpFactory 组装，以客户端打开的 cwd 为工作区根目录。
func acpCommand(args []string, version string) int {
	fs := flag.NewFlagSet("acp", flag.ContinueOnError)
	model := fs.String("model", "", "provider name (default: config default_model)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	factory := &acpFactory{model: *model}
	info := acp.AgentInfo{Name: "reasonix", Version: version}
	if err := acp.Serve(ctx, os.Stdin, os.Stdout, factory, info); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	return 0
}

// acpFactory 通过复用 boot.Build 为每个 ACP 会话构建一个 control.Controller，
// 以会话的 cwd 作为 WorkspaceRoot。这保持了 ACP 与 chat、desktop 和 serve 组装方式的一致性，
// 同时仅为当前会话添加宿主提供的 MCP 服务器。
type acpFactory struct {
	model string // 用户指定的模型引用，如 "provider/model"
}

// SessionDir 返回存储会话数据的目录路径。
func (f *acpFactory) SessionDir() string {
	return config.SessionDir()
}

// NewSession 为每个 ACP 会话组装控制器。资源（MCP 子进程）通过控制器的 Cleanup 方法释放，
// 在 ctrl.Close() 时运行。它复用 boot.Build 构建完整的控制器链路，
// 包括模型提供者、工具集、MCP 服务器等。
func (f *acpFactory) NewSession(ctx context.Context, p acp.SessionParams) (*control.Controller, error) {
	root := strings.TrimSpace(p.Cwd)
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	if root != "" && !filepath.IsAbs(root) {
		return nil, fmt.Errorf("session cwd must be an absolute path: %s", root)
	}
	return boot.Build(ctx, boot.Options{
		Model:                    firstNonEmpty(p.Model, f.model),
		RequireKey:               true,
		Sink:                     p.Sink,
		EffortOverride:           p.EffortOverride,
		Stderr:                   os.Stderr,
		WorkspaceRoot:            root,
		ExtraPlugins:             p.MCPServers,
		CleanupPendingReconciler: acp.ReconcileCleanupPending,
	})
}

// SessionConfigState 返回当前会话的配置状态，包括可用模型列表、当前模型、
// 努力级别（effort）选项等。供宿主客户端在 UI 中展示配置选项。
func (f *acpFactory) SessionConfigState(_ context.Context, p acp.SessionConfigStateParams) (acp.SessionConfigState, error) {
	root := strings.TrimSpace(p.Cwd)
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	if root != "" && !filepath.IsAbs(root) {
		return acp.SessionConfigState{}, fmt.Errorf("session cwd must be an absolute path: %s", root)
	}
	_, _ = config.MigrateLegacyIfNeeded()
	_, _ = config.MigrateMCPToUserConfigOnUpgrade([]string{root})
	cfg, err := config.LoadForRoot(root)
	if err != nil {
		return acp.SessionConfigState{}, err
	}

	ref := firstNonEmpty(p.Model, f.model, cfg.DefaultModel)
	if strings.TrimSpace(ref) == "" {
		return acp.SessionConfigState{}, fmt.Errorf("no default_model configured")
	}
	entry, ok := cfg.ResolveModel(ref)
	if !ok {
		return acp.SessionConfigState{}, fmt.Errorf("unknown model %q", ref)
	}
	if !entry.Configured() {
		return acp.SessionConfigState{}, fmt.Errorf("model %q is not configured", ref)
	}
	currentModel := entry.Name + "/" + entry.Model
	modelOptions, modelInfos := acpModelOptions(cfg)
	if !hasModelOption(modelOptions, currentModel) {
		modelOptions = append(modelOptions, acp.SessionConfigSelectOption{
			Value:       currentModel,
			Name:        currentModel,
			Description: entry.Name,
		})
		modelInfos = append(modelInfos, acp.ModelInfo{
			ModelID:     currentModel,
			Name:        currentModel,
			Description: entry.Name,
		})
	}

	effortEntry := *entry
	effortOverride := cloneStringPtr(p.EffortOverride)
	hadEffortOverride := effortOverride != nil
	if effortOverride != nil {
		if strings.TrimSpace(*effortOverride) == "" {
			effortEntry.Effort = ""
		} else {
			normalized, err := config.NormalizeEffort(&effortEntry, *effortOverride)
			if err != nil {
				effortEntry.Effort = ""
				cleared := ""
				effortOverride = &cleared
			} else {
				effortEntry.Effort = normalized
				effortOverride = &normalized
			}
		}
	}

	options := []acp.SessionConfigOption{{
		ID:           "model",
		Name:         "Model",
		Category:     "model",
		Type:         "select",
		CurrentValue: currentModel,
		Options:      modelOptions,
	}}
	if cap := config.EffortCapabilityForEntry(&effortEntry); cap.Supported {
		currentEffort := config.EffortDisplay(&effortEntry)
		if !containsString(cap.Levels, currentEffort) {
			currentEffort = "auto"
			auto := ""
			effortOverride = &auto
		}
		options = append(options, acp.SessionConfigOption{
			ID:           "effort",
			Name:         "Effort",
			Category:     "thought_level",
			Type:         "select",
			CurrentValue: currentEffort,
			Options:      acpEffortOptions(cap.Levels),
		})
	} else if hadEffortOverride {
		cleared := ""
		effortOverride = &cleared
	}

	return acp.SessionConfigState{
		Model:          currentModel,
		EffortOverride: effortOverride,
		Models: &acp.SessionModelState{
			AvailableModels: modelInfos,
			CurrentModelID:  currentModel,
		},
		ConfigOptions: options,
	}, nil
}

// acpBuiltinTools 根据配置构建内置工具集，包括 bash 执行、文件搜索等。
// 参数 cfg 是配置对象，cwd 是工作目录，writeRoots 是允许写入的根目录列表。
func acpBuiltinTools(cfg *config.Config, cwd string, writeRoots []string) []tool.Tool {
	bashSpec := sandbox.Spec{Mode: cfg.BashMode(), WriteRoots: writeRoots, Network: cfg.Sandbox.Network}
	ws := builtin.Workspace{
		Dir:         cwd,
		WriteRoots:  writeRoots,
		Bash:        bashSpec,
		BashTimeout: time.Duration(cfg.BashTimeoutSeconds()) * time.Second,
		Search:      builtin.ResolveSearch(cfg.Tools.Search.Engine, cfg.Tools.Search.RgPath, nil),
		ProxySpec:   cfg.NetworkProxySpec(),
	}
	return ws.Tools(cfg.Tools.Enabled...)
}

// acpModelOptions 从配置中提取所有已配置的模型选项，返回供 ACP 会话配置 UI 使用的
// 选择选项列表和模型信息列表。
func acpModelOptions(cfg *config.Config) ([]acp.SessionConfigSelectOption, []acp.ModelInfo) {
	if cfg == nil {
		return nil, nil
	}
	var options []acp.SessionConfigSelectOption
	var models []acp.ModelInfo
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !p.Configured() {
			continue
		}
		for _, model := range p.ChatModelList() {
			ref := p.Name + "/" + model
			options = append(options, acp.SessionConfigSelectOption{
				Value:       ref,
				Name:        ref,
				Description: p.Name,
			})
			models = append(models, acp.ModelInfo{
				ModelID:     ref,
				Name:        ref,
				Description: p.Name,
			})
		}
	}
	return options, models
}

// hasModelOption 检查给定的模型引用是否已存在于选项列表中，避免重复添加。
func hasModelOption(options []acp.SessionConfigSelectOption, ref string) bool {
	for _, opt := range options {
		if opt.Value == ref {
			return true
		}
	}
	return false
}

// acpEffortOptions 将努力级别（effort level）字符串列表转换为 ACP 配置选择选项。
func acpEffortOptions(levels []string) []acp.SessionConfigSelectOption {
	out := make([]acp.SessionConfigSelectOption, 0, len(levels))
	for _, level := range levels {
		out = append(out, acp.SessionConfigSelectOption{Value: level, Name: effortOptionName(level)})
	}
	return out
}

// effortOptionName 将努力级别字符串转换为用户友好的显示名称（首字母大写）。
func effortOptionName(level string) string {
	if level == "" {
		return ""
	}
	if level == "xhigh" {
		return "XHigh"
	}
	return strings.ToUpper(level[:1]) + level[1:]
}

// firstNonEmpty 返回可变参数中第一个非空字符串，用于模型优先级回退逻辑。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// cloneStringPtr 深拷贝一个字符串指针，避免共享引用导致的意外修改。
func cloneStringPtr(p *string) *string {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// acpTaskProfileDefaults 从配置中提取任务子代理（task subagent）的默认模型和努力级别。
// 返回值分别为模型引用和努力级别字符串。
func acpTaskProfileDefaults(cfg *config.Config) (string, string) {
	if cfg == nil {
		return "", ""
	}
	model := strings.TrimSpace(cfg.Agent.SubagentModels["task"])
	if model == "" {
		model = strings.TrimSpace(cfg.Agent.SubagentModel)
	}
	effort := strings.TrimSpace(cfg.Agent.SubagentEfforts["task"])
	if effort == "" {
		effort = strings.TrimSpace(cfg.Agent.SubagentEffort)
	}
	return model, effort
}

// newACPSubagentProviderResolver 创建一个子代理提供者解析器函数。
// 该解析器根据模型引用和努力级别动态解析并创建提供者实例，
// 用于 ACP 会话中的子代理任务执行。
func newACPSubagentProviderResolver(cfg *config.Config, parent *config.ProviderEntry, proxySpec netclient.ProxySpec) func(string, string) (provider.Provider, *provider.Pricing, int, error) {
	return func(modelRef, effort string) (provider.Provider, *provider.Pricing, int, error) {
		modelRef = strings.TrimSpace(modelRef)
		effort = strings.TrimSpace(effort)

		var entry *config.ProviderEntry
		if modelRef != "" {
			var ok bool
			entry, ok = cfg.ResolveModel(modelRef)
			if !ok {
				return nil, nil, 0, fmt.Errorf("subagent_model %q is not a configured provider", modelRef)
			}
		} else {
			cp := *parent
			entry = &cp
		}

		if effort != "" {
			normalized, err := config.NormalizeEffort(entry, effort)
			if err != nil {
				return nil, nil, 0, err
			}
			entry.Effort = normalized
			if entry.Kind == "anthropic" && strings.TrimSpace(entry.Effort) != "" && strings.TrimSpace(entry.Thinking) == "" {
				entry.Thinking = "adaptive"
			}
		}

		prov, err := boot.NewProviderWithProxy(entry, proxySpec)
		if err != nil {
			return nil, nil, 0, err
		}
		return prov, entry.Price, entry.ContextWindow, nil
	}
}
