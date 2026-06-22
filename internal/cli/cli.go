// Package cli 实现了 Reasonix 的命令行入口层，包括：
//   - 子命令路由（run、chat、setup、config、serve、mcp 等）
//   - 命令行标志（flag）解析
//   - 从配置文件组装控制器（Controller）
//   - 进程退出码管理
//
// 核心设计原则：配置驱动——模型提供者和工具均从配置中解析，
// 而非硬编码在代码中。这使得用户可以通过 reasonix.toml 灵活定制行为。
//
// 交互模式：
//   - reasonix          → 启动交互式聊天 TUI（Bubble Tea）
//   - reasonix run      → 非交互式执行单次提示（适合脚本/管道）
//   - reasonix serve    → 启动 HTTP+SSE 服务器，将事件流暴露给浏览器
//   - reasonix setup    → 配置向导
//   - reasonix config   → 子命令配置管理（auto-plan、reasoning-language）
package cli

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/i18n"
	"reasonix/internal/notify"
	"reasonix/internal/provider"
	"reasonix/internal/provider/openai"
	"reasonix/internal/serve"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/term"
)

// runInteractiveSession 是交互式会话的函数变量，默认指向 chatREPL。
// 使用变量而非直接调用，便于测试时注入 mock 实现。
var runInteractiveSession = chatREPL

// cliIsInteractive 是交互检测的函数变量，默认指向 isInteractive。
// 同样用于支持测试时的行为替换。
var cliIsInteractive = isInteractive

// Run 是 CLI 的主入口函数，返回进程退出码。
//
// 参数：
//   - args: 命令行参数（不含程序名），例如 ["run", "--model", "deepseek", "hello"]
//   - version: 构建时注入的版本号字符串
//
// 返回值：进程退出码（0=成功, 1=运行时错误, 2=用法/参数错误）
//
// 执行流程：
//  1. 检测 UI 语言（先从环境变量，再从配置文件）
//  2. 提取子命令名称（第一个非标志参数）
//  3. 执行必要的配置迁移（旧版 → 新版格式）
//  4. 根据子命令分发到对应的处理函数
//
// 子命令列表：
//   - run: 非交互式执行单次提示
//   - chat/code: 启动交互式聊天 TUI
//   - serve: 启动 HTTP+SSE 服务器
//   - setup: 配置向导
//   - config: 配置子命令管理
//   - init: 项目记忆初始化提示
//   - acp/mcp: Agent Communication Protocol / Model Context Protocol 管理
//   - doctor: 诊断工具
//   - review: 代码审查
//   - bot: 多渠道 IM 机器人
//   - upgrade/update: 版本升级
//   - version/help: 版本/帮助信息
func Run(args []string, version string) int {
	// 预先检测 UI 语言，确保即使是配置加载前的路径（如首次运行欢迎横幅）
	// 也能正确显示本地化文本。先用环境变量检测；如果配置文件存在且指定了语言，
	// 则以配置文件为准。
	i18n.DetectLanguage("")
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	// 将 --acp 标志映射为 "acp" 子命令
	if cmd == "--acp" {
		cmd = "acp"
	}
	// 如果第一个参数是交互模式的标志（如 --model、--continue），
	// 则视为空子命令，走交互式聊天路径
	if len(args) > 0 && isDefaultInteractiveFlag(cmd) {
		cmd = ""
	}
	// 对特定子命令执行旧版配置迁移（确保配置格式兼容）
	if shouldMigrateLegacyConfigForCLI(cmd) {
		migrateLegacyConfigForCLI()
	}
	// 加载配置并应用语言设置（配置中的语言优先于环境变量）
	if cfg, err := config.Load(); err == nil {
		if cfg.Language != "" {
			i18n.DetectLanguage(cfg.Language)
		}
	}

	// 无参数且终端交互模式 → 启动交互式聊天 TUI
	if len(args) == 0 && cliIsInteractive() {
		return runInteractiveSession(nil)
	}
	// 无参数且非交互模式 → 显示用法信息
	if len(args) == 0 {
		configureCLIThemeFromConfigForTTYOutput()
		usage()
		return 0
	}
	// 子命令为空（第一个参数是标志）→ 走交互式聊天路径，传入原始参数
	if cmd == "" {
		return runInteractiveSession(args)
	}

	rest := args[1:]
	switch cmd {
	case "run":
		return runAgent(rest)
	case "chat", "code": // "code" 是 v0.x 版本中交互式会话的旧名称
		return runInteractiveSession(rest)
	case "serve":
		return runServe(rest)
	case "setup":
		configureCLIThemeFromConfigForTTYOutput()
		return setupConfig(rest)
	case "config":
		configureCLIThemeFromConfigNoProbe()
		return configCommand(rest)
	case "init":
		// 项目记忆（AGENTS.md）由模型在会话中生成——`/init` 技能运行代码库分析。
		// 此 CLI 入口仅给出提示（同时指向 `setup` 进行配置），
		// 使 `reasonix init` 不会成为死胡同。
		configureCLIThemeFromConfigNoProbe()
		return initHint()
	case "acp":
		configureCLIThemeFromConfigNoProbe()
		return acpCommand(rest, version)
	case "mcp":
		configureCLIThemeFromConfigNoProbe()
		return mcpCommand(rest)
	case "doctor":
		configureCLIThemeFromConfigNoProbe()
		return doctorCommand(rest, version)
	case "review":
		configureCLIThemeFromConfigNoProbe()
		return reviewCommand(rest)
	case "bot":
		configureCLIThemeFromConfigNoProbe()
		return botCommand(rest, version)
	case "upgrade", "update":
		configureCLIThemeFromConfigNoProbe()
		return upgradeCommand(rest, version)
	case "version", "--version", "-v":
		fmt.Println("reasonix", version)
		return 0
	case "help", "--help", "-h":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, i18n.M.UnknownCommandFmt+"\n\n", cmd)
		usage()
		return 2
	}
}

// isDefaultInteractiveFlag 判断参数是否为交互模式的默认标志。
// 当 reasonix 后面直接跟这些标志时，视为进入交互式聊天模式（而非子命令）。
// 支持带等号的形式，如 --model=deepseek。
func isDefaultInteractiveFlag(arg string) bool {
	switch arg {
	case "--model", "--max-steps", "--continue", "-c", "--resume", "--dangerously-skip-permissions", "--yolo", "--dir":
		return true
	}
	if name, _, ok := strings.Cut(arg, "="); ok && isDefaultInteractiveFlag(name) {
		return true
	}
	return false
}

// shouldMigrateLegacyConfigForCLI 判断给定子命令是否需要执行旧版配置迁移。
// 主要的会话和配置相关命令都需要迁移，而 help/version 等不需要。
func shouldMigrateLegacyConfigForCLI(cmd string) bool {
	switch cmd {
	case "", "run", "chat", "code", "serve", "setup", "config", "init", "acp", "mcp", "doctor", "bot", "upgrade", "update":
		return true
	default:
		return false
	}
}

// migrateLegacyConfigForCLI 执行旧版配置文件的格式迁移。
// 将 v0.x 或早期版本的配置自动转换为当前版本格式。
// 迁移失败仅输出警告，不中断程序执行。
func migrateLegacyConfigForCLI() {
	if _, err := config.MigrateLegacyIfNeeded(); err != nil {
		fmt.Fprintln(os.Stderr, "warning: config migration failed:", err)
	}
}

// migrateMCPConfigForCLIWorkspace 将项目级 MCP 配置迁移到用户级配置。
// 确保旧版本中项目目录下的 MCP 服务器配置升级后仍然可用。
func migrateMCPConfigForCLIWorkspace() {
	if wd, err := os.Getwd(); err == nil {
		if _, err := config.MigrateMCPToUserConfigOnUpgrade([]string{wd}); err != nil {
			fmt.Fprintln(os.Stderr, "warning: MCP config migration failed:", err)
		}
	}
}

// configureCLIThemeFromConfig 从配置文件加载并应用 CLI 主题设置。
// 包括主题（auto/dark/light）和主题风格（graphite/aurora/slate 等）。
func configureCLIThemeFromConfig() {
	if cfg, err := config.Load(); err == nil {
		configureCLIThemeWithStyle(cfg.UITheme(), cfg.UIThemeStyle())
	} else {
		configureCLITheme("auto")
	}
}

// configureCLIThemeFromConfigForTTYOutput 在 TTY 输出时从配置加载主题。
// 如果 stdout 是终端则完整加载主题（包括终端探测），否则跳过探测。
// 这避免了在管道/重定向输出时进行不必要的终端能力检测。
func configureCLIThemeFromConfigForTTYOutput() {
	if isTTY(os.Stdout) {
		configureCLIThemeFromConfig()
		return
	}
	configureCLIThemeFromConfigNoProbe()
}

// configureCLIThemeFromConfigNoProbe 从配置加载主题但不执行终端探测。
// 用于非交互式子命令（如 config、doctor），避免干扰已有的终端状态。
func configureCLIThemeFromConfigNoProbe() {
	withoutTerminalProbe(configureCLIThemeFromConfig)
}

// setup 通过 boot.Build 从配置组装一个可用的 Controller。
// 这是一个薄适配层，实际的组装逻辑（模型解析、工具注册、权限门控、
// 双模型协调器）位于 internal/boot 包中，与桌面前端共享。
//
// 参数：
//   - ctx: 上下文，用于取消和超时控制
//   - modelName: 模型引用（如 "deepseek" 或 "deepseek/deepseek-v4-flash"），空字符串使用配置默认值
//   - maxStepsOverride: 工具调用轮次上限覆盖，0 表示使用配置值
//   - requireKey: 是否强制要求 API 密钥存在（run 命令为 true，chat 为 false 以便无密钥也能打开 UI）
//   - sink: 事件接收器，接收智能体的类型化事件流。runAgent 传入 TextSink 渲染到 stdout，
//     TUI 传入 event-channel sink 将事件转为 tea.Msg
//
// 返回值：
//   - *control.Controller: 可驱动的控制器实例
//   - error: 组装失败时返回错误（如模型未配置、API 密钥缺失等）
func setup(ctx context.Context, modelName string, maxStepsOverride int, requireKey bool, sink event.Sink) (*control.Controller, error) {
	migrateMCPConfigForCLIWorkspace()
	return boot.Build(ctx, boot.Options{
		Model:      modelName,
		MaxSteps:   maxStepsOverride,
		RequireKey: requireKey,
		Sink:       sink,
		SessionDir: resolveCLISessionDir(),
	})
}

// resolveCLISessionDir 返回 CLI 调用的会话目录。
// 当当前工作目录映射到项目会话目录时，使用项目目录以便 /resume 显示项目历史。
// 否则回退到全局会话目录。
//
// 返回值：会话目录的绝对路径
func resolveCLISessionDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return config.SessionDir()
	}
	if projDir := config.ProjectSessionDir(cwd); projDir != "" && projDir != config.SessionDir() {
		return projDir
	}
	return config.SessionDir()
}

// setupQuiet 与 setup 类似，但抑制插件子进程的 stderr 输出。
// 用于 Bubble Tea 会话中的模型切换，防止插件日志破坏 TUI 的终端原始模式。
//
// 参数：同 setup 函数
//
// 返回值：同 setup 函数
func setupQuiet(ctx context.Context, modelName string, maxStepsOverride int, requireKey bool, sink event.Sink) (*control.Controller, error) {
	return boot.Build(ctx, boot.Options{
		Model:      modelName,
		MaxSteps:   maxStepsOverride,
		RequireKey: requireKey,
		Sink:       sink,
		Stderr:     io.Discard,
	})
}

// chdirTo 处理 --dir 标志：在任何读取操作之前切换工作目录，
// 使配置发现、沙箱根目录和文件工具都从选定的项目根目录解析。
//
// 参数：
//   - dir: 目标目录路径，空字符串表示不切换
//
// 返回值：0=成功, 2=失败（已输出错误信息）
func chdirTo(dir string) int {
	if dir == "" {
		return 0
	}
	if err := os.Chdir(dir); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}
	return 0
}

// modelForResumePath 在恢复会话时确定应使用的模型。
// 如果用户显式指定了模型（modelName 非空），则使用用户指定的模型。
// 否则从会话文件中读取上次使用的模型，并验证其在当前配置中是否可用。
//
// 参数：
//   - modelName: 用户通过 --model 指定的模型名，空字符串表示未指定
//   - resumePath: 要恢复的会话文件路径
//   - cfg: 当前配置（用于验证模型是否可用），可为 nil
//
// 返回值：最终应使用的模型引用字符串
func modelForResumePath(modelName, resumePath string, cfg *config.Config) string {
	if strings.TrimSpace(modelName) != "" || strings.TrimSpace(resumePath) == "" {
		return modelName
	}
	sessionModel, ok := agent.LoadSessionModel(resumePath)
	if !ok {
		return modelName
	}
	if cfg == nil {
		return sessionModel
	}
	if _, ok := cfg.ResolveModel(sessionModel); !ok {
		return modelName
	}
	return sessionModel
}

// loadResumableSession 加载可恢复的会话文件。
// 如果会话有待处理的清理操作（如中断的写入），则拒绝加载以避免数据损坏。
//
// 参数：
//   - path: 会话文件路径
//
// 返回值：
//   - *agent.Session: 加载的会话数据
//   - error: 加载失败时返回错误（如文件不存在、有待处理清理等）
func loadResumableSession(path string) (*agent.Session, error) {
	if agent.IsCleanupPending(path) {
		return nil, fmt.Errorf("session is pending cleanup")
	}
	return agent.LoadSession(path)
}

// newNotificationSender 是创建平台通知发送器的工厂函数。
// 使用变量便于测试时注入 mock 实现。
var newNotificationSender = func() notify.Sender { return notify.NewPlatformSender() }

// withNotifications 在配置启用时为 CLI 事件流添加系统通知。
// 当 cfg.Notifications.Enabled 为 true 时，包装原始 sink 以发送桌面通知。
//
// 参数：
//   - sink: 原始事件接收器
//   - cfg: 配置对象（读取通知设置）
//
// 返回值：添加了通知能力的事件接收器
func withNotifications(sink event.Sink, cfg *config.Config) event.Sink {
	if cfg == nil || !cfg.Notifications.Enabled {
		return sink
	}
	return notify.NewSink(sink, newNotificationSender(), cfg.Notifications)
}

// runAgent 实现 `reasonix run` 子命令：非交互式执行单次提示。
//
// 支持的标志：
//   - --model: 指定模型提供者
//   - --max-steps: 工具调用轮次上限（0=使用配置值）
//   - --show-thinking: 显示思考文本（默认折叠）
//   - --metrics: 将 token/缓存/成本摘要写入 JSON 文件
//   - --dir: 切换工作目录后再执行
//   - --continue/-c: 恢复最近的保存会话
//   - --resume: 恢复指定的会话文件
//
// 提示来源（优先级从高到低）：
//  1. 命令行剩余参数
//  2. stdin 管道输入
//
// 返回值：进程退出码（0=成功, 1=运行时错误, 2=参数错误）
func runAgent(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	model := fs.String("model", "", "provider name (default: config default_model)")
	maxSteps := fs.Int("max-steps", 0, "max tool-call rounds (0 = use config/default)")
	showThinking := fs.Bool("show-thinking", false, "show thinking text instead of the collapsed thinking marker")
	metricsPath := fs.String("metrics", "", "write a JSON token/cache/cost summary of the run to this path")
	dir := fs.String("dir", "", "change to this directory first (project root); config, sandbox and file tools resolve from here")
	cont := fs.Bool("continue", false, "resume the most recent saved session")
	fs.BoolVar(cont, "c", false, "shorthand for --continue")
	resume := fs.String("resume", "", "resume a specific session file (non-interactive; takes precedence over --continue)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if rc := chdirTo(*dir); rc != 0 {
		return rc
	}
	cfg, _ := config.Load()
	configureCLIThemeFromConfigForTTYOutput()

	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		prompt = readStdin()
	}
	if prompt == "" {
		fmt.Fprintln(os.Stderr, i18n.M.UsageRunHint)
		return 2
	}

	var resumeSession *agent.Session
	if *resume != "" {
		var err error
		resumeSession, err = loadResumableSession(*resume)
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	// Live run: render the agent's event stream to stdout. Markdown post-stream
	// redraw (cursor moves) is enabled only on a TTY; piped / captured output
	// keeps the raw stream.
	var renderer agent.Renderer
	termW := 80
	if isTTY(os.Stdout) {
		if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
			termW = w
		}
		renderer = newMarkdownRenderer(termW)
	}
	textSink := agent.NewTextSink(os.Stdout, renderer, termW)
	textSink.SetShowReasoning(*showThinking)
	var sink event.Sink = textSink
	var metrics *metricsSink
	if *metricsPath != "" {
		metrics = &metricsSink{inner: textSink}
		sink = metrics
	}
	sink = withNotifications(sink, cfg)
	if *resume != "" {
		*model = modelForResumePath(*model, *resume, cfg)
	} else if *cont {
		if sessions, err := agent.ListSessions(resolveCLISessionDir()); err == nil && len(sessions) > 0 {
			*model = modelForResumePath(*model, sessions[0].Path, cfg)
		}
	}
	ctrl, err := setup(ctx, *model, *maxSteps, true, sink)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	defer ctrl.Close()

	// --resume: load a specific session file (non-interactive, meant for
	// MCP/API callers that manage their own per-project session). Takes
	// precedence over --continue.
	// --continue: resume the most recent saved session.
	if *resume != "" {
		ctrl.Resume(resumeSession, *resume)
	} else if *cont {
		sessions, err := agent.ListSessions(ctrl.SessionDir())
		if err != nil || len(sessions) == 0 {
			fmt.Fprintln(os.Stderr, i18n.M.NoSessionToResume)
			return 1
		}
		loaded, err := agent.LoadSession(sessions[0].Path)
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
		ctrl.Resume(loaded, sessions[0].Path)
	}
	if ctrl.SessionPath() == "" && ctrl.SessionDir() != "" {
		ctrl.SetSessionPath(agent.NewSessionPath(ctrl.SessionDir(), ctrl.Label()))
	}

	runErr := ctrl.Run(ctx, prompt)
	if cfg != nil {
		notify.SendEvent(newNotificationSender(), cfg.Notifications, event.Event{Kind: event.TurnDone, Err: runErr})
	}
	if metrics != nil {
		if err := writeMetrics(*metricsPath, metrics.m); err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		}
	}
	if runErr != nil {
		fmt.Fprintln(os.Stderr, "\n"+i18n.M.ErrorPrefix, runErr)
		return 1
	}
	return 0
}

// runServe 实现 `reasonix serve` 子命令：通过 HTTP+SSE 暴露控制器。
//
// 事件流通过 Server-Sent Events 推送到浏览器，命令通过 JSON POST 接收。
// Broadcaster 作为控制器的事件接收器，使同一类型化事件流
// （聊天 TUI 消费的同一个流）也到达 Web 客户端——
// 这是一个传输层无关的控制器，由第二个前端驱动。
//
// 支持的标志：
//   - --model: 指定模型提供者
//   - --max-steps: 工具调用轮次上限
//   - --addr: 监听地址（默认 127.0.0.1:8787）
//   - --resume: 恢复指定的会话文件
//
// 返回值：进程退出码
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	model := fs.String("model", "", "provider name (default: config default_model)")
	maxSteps := fs.Int("max-steps", 0, "max tool-call rounds (0 = use config/default)")
	addr := fs.String("addr", "127.0.0.1:8787", "listen address")
	resume := fs.String("resume", "", "resume a saved session file")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx := context.Background()
	bc := serve.NewBroadcaster()
	cfg, _ := config.Load()
	var resumeSession *agent.Session
	if *resume != "" {
		var err error
		resumeSession, err = loadResumableSession(*resume)
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
	}
	*model = modelForResumePath(*model, *resume, cfg)
	ctrl, err := setup(ctx, *model, *maxSteps, true, bc)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	defer ctrl.Close()

	// Auto-save target: reuse the resumed file, else a fresh one — same as chat.
	if *resume != "" {
		ctrl.Resume(resumeSession, *resume)
	} else if ctrl.SessionDir() != "" {
		ctrl.SetSessionPath(agent.NewSessionPath(ctrl.SessionDir(), ctrl.Label()))
	}

	fmt.Printf("reasonix serve — %s on http://%s\n", ctrl.Label(), *addr)
	// Use graceful shutdown so SIGINT/SIGTERM drain active connections.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve.New(ctrl, bc).RunGraceful(ctx, *addr); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	return 0
}

// chatREPL 实现交互式聊天会话：一个持久的智能体/会话和提示循环，
// 跨轮次保持对话上下文。使用 Bubble Tea TUI 框架渲染界面。
//
// 退出方式：输入 'exit'/'quit' 或按 Ctrl-D。
//
// 支持的标志：
//   - --model: 指定模型提供者
//   - --max-steps: 工具调用轮次上限
//   - --continue/-c: 恢复最近的保存会话
//   - --resume: 交互式选择要恢复的会话
//   - --dangerously-skip-permissions/--yolo: 自动批准所有工具调用（跳过权限检查）
//   - --dir: 切换工作目录
//
// 返回值：进程退出码
func chatREPL(args []string) int {
	fs := flag.NewFlagSet("reasonix", flag.ContinueOnError)
	model := fs.String("model", "", "provider name (default: config default_model)")
	maxSteps := fs.Int("max-steps", 0, "max tool-call rounds (0 = use config/default)")
	cont := fs.Bool("continue", false, "resume the most recent saved session")
	fs.BoolVar(cont, "c", false, "shorthand for --continue")
	resume := fs.Bool("resume", false, "list saved sessions and pick one to resume")
	yolo := fs.Bool("dangerously-skip-permissions", false, "YOLO: auto-approve approval-gated tool calls this session; same runtime mode as Ctrl+Y")
	fs.BoolVar(yolo, "yolo", false, "alias for --dangerously-skip-permissions")
	dir := fs.String("dir", "", "change to this directory first (project root); config, sandbox and file tools resolve from here")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if rc := chdirTo(*dir); rc != 0 {
		return rc
	}
	cfg, err := config.Load()
	if err == nil {
		configureCLIThemeWithStyle(cfg.UITheme(), cfg.UIThemeStyle())
	}

	// Decide whether we're starting fresh or resuming. --resume opens an
	// interactive picker; --continue / -c jumps straight into the newest.
	var resumePath string
	switch {
	case *resume:
		path, rc := pickSessionToResume()
		if rc != 0 {
			return rc
		}
		resumePath = path
	case *cont:
		sessions, err := agent.ListSessions(resolveCLISessionDir())
		if err != nil || len(sessions) == 0 {
			fmt.Fprintln(os.Stderr, i18n.M.NoSessionToResume)
			return 1
		}
		resumePath = sessions[0].Path
	}

	ctx := context.Background()
	*model = modelForResumePath(*model, resumePath, cfg)

	// Plumb the controller's typed event stream through a channel so each event
	// can become a tea.Msg inside the TUI's update loop. Buffered generously:
	// streaming bursts (tool results, long answers) shouldn't backpressure the
	// agent goroutine.
	eventCh := make(chan event.Event, 1024)

	var sink event.Sink = &eventSink{ch: eventCh}
	sink = withNotifications(sink, cfg)
	ctrl, err := setup(ctx, *model, *maxSteps, false, sink)
	if err != nil && errors.Is(err, boot.ErrUnknownModel) && isInteractive() && config.SourcePath() == "" {
		// True first run whose default model can't resolve: guide setup, then retry.
		// With a config present, fall through to the descriptive error — re-running
		// the wizard would overwrite the user's config (#2856).
		fmt.Fprintln(os.Stderr, i18n.M.ReconfigureOnUnknownModel)
		if rc := interactiveSetup(defaultConfigTarget(), defaultEnvTarget()); rc != 0 {
			return rc
		}
		ctrl, err = setup(ctx, *model, *maxSteps, false, sink)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}

	// Decide where this conversation's auto-save lands. A resume reuses the
	// file so closing/reopening keeps appending to the same history; a fresh
	// session lands in a new file stamped with the model name.
	if resumePath != "" {
		if loaded, err := agent.LoadSession(resumePath); err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		} else {
			ctrl.Resume(loaded, resumePath)
		}
	} else if ctrl.SessionDir() != "" {
		ctrl.SetSessionPath(agent.NewSessionPath(ctrl.SessionDir(), ctrl.Label()))
	}

	// Surface a missing-key warning inside the TUI banner so the first message
	// failing is at least pre-announced; the user can still enter chat.
	missing := ""
	if cfg, loadErr := config.Load(); loadErr == nil {
		name := *model
		if name == "" {
			name = cfg.DefaultModel
		}
		if vErr := cfg.Validate(name); vErr != nil {
			missing = vErr.Error()
		}
	}

	// Initial terminal width — the TUI re-flows on every WindowSizeMsg so
	// this is just a starting estimate before the first resize event lands.
	termW := 80
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		termW = w
	}

	// Route "ask" decisions to the TUI: the controller emits an ApprovalRequest
	// event and blocks until the user answers via ctrl.Approve. Sub-agents (the
	// task tool) keep their headless gate from setup — no UI to prompt through.
	ctrl.EnableInteractiveApproval()
	// YOLO: skip every tool approval request for the session (deny rules still
	// apply; ask questions and plan approvals still wait for the user).
	if *yolo {
		ctrl.SetAutoApproveTools(true)
	}

	m := newChatTUI(ctrl, missing, eventCh, termW)
	if cfg, err := config.Load(); err == nil {
		m.outputStyle = cfg.Agent.OutputStyle    // shown as the active entry in /output-style
		m.statuslineCmd = cfg.Statusline.Command // custom status-line command, "" = built-in row
		m.showReasoning = cfg.UI.ShowReasoning   // /verbose persistence: start with config default
		m.cfg = cfg
	}

	// /model support: a pure builder the TUI calls to rebuild on a different
	// model (carrying the conversation). It must NOT touch the running model —
	// runModelSubcommand performs the swap on the live copy. The same stable sink
	// feeds the new controller, so events keep flowing to this TUI.
	m.buildController = func(ref string, carry []provider.Message, resumePath string) (*control.Controller, error) {
		c, err := setupQuiet(ctx, ref, *maxSteps, false, sink)
		if err != nil {
			return nil, err
		}
		// Keep the carried conversation in its existing file so the switch doesn't
		// orphan a duplicate (#2807).
		path := agent.ContinueSessionPath(resumePath, c.SessionDir(), c.Label())
		if len(carry) > 0 {
			c.Resume(&agent.Session{Messages: carry}, path)
		} else if path != "" {
			c.SetSessionPath(path)
		}
		c.EnableInteractiveApproval()
		if *yolo {
			c.SetAutoApproveTools(true)
		}
		return c, nil
	}
	if cfg, e := config.Load(); e == nil {
		name := *model
		if name == "" {
			name = cfg.DefaultModel
		}
		if entry, ok := cfg.ResolveModel(name); ok {
			m.modelRef = entry.Name + "/" + entry.Model
		}
	}
	m.refreshEffortStatus()

	if m.nativeScrollback {
		prepareNativeScrollback(os.Stdout, m.bottomRows())
	}

	// Non-Termux terminals use an alt-screen transcript viewport. Termux stays
	// in the normal buffer so native touch scrollback and soft-keyboard focus
	// keep working; finalized transcript lines are emitted via tea.Println.
	p := tea.NewProgram(m)
	// SSH drop (SIGHUP) or service stop (SIGTERM): persist the conversation
	// before the terminal goes away, then unwind through the normal close path
	// so resume picks up the interrupted session (#3772).
	hangup := make(chan os.Signal, 1)
	signal.Notify(hangup, syscall.SIGHUP, syscall.SIGTERM)
	go func() {
		for range hangup {
			_ = ctrl.Snapshot()
			p.Quit()
		}
	}()
	final, runErr := p.Run()
	signal.Stop(hangup)
	// Close the active controller plus any retired ones from /model switches.
	// Retired controllers were stashed rather than closed at switch time
	// because Controller.Close() runs SessionEnd hooks and kills plugin
	// subprocesses — operations that corrupt bubbletea's terminal raw mode
	// when executed while the TUI is alive.
	if fm, ok := final.(chatTUI); ok {
		for _, oc := range fm.oldControllers {
			oc.Close()
		}
		if fm.ctrl != nil {
			fm.ctrl.Close()
		} else {
			ctrl.Close()
		}
	} else {
		ctrl.Close()
	}
	if runErr != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, runErr)
		return 1
	}
	return 0
}

// prepareNativeScrollback 为 Termux 等使用原生滚动缓冲区的终端准备环境。
// 清除终端的滚动历史，使重新打开的聊天从干净状态开始。
func prepareNativeScrollback(w io.Writer, rows int) {
	// Clear the terminal's scrollback history so a reopened chat starts
	// with a clean slate (Termux stays in the normal buffer, so prior
	// output would otherwise remain visible above the banner).
	fmt.Fprint(w, "\x1B[3J\x1B[2J\x1B[H")
	reserveNativeScrollbackFrame(w, rows)
}

// reserveNativeScrollbackFrame 在终端中预留指定行数的滚动帧空间。
// 用于 Termux 环境，确保软键盘弹出时内容不被遮挡。
func reserveNativeScrollbackFrame(w io.Writer, rows int) {
	for i := 0; i < rows; i++ {
		fmt.Fprintln(w)
	}
}

// setupTargets 定义配置向导的写入目标：TOML 配置文件和凭证存储。
// API 密钥始终写入 reasonix 全局凭证存储，绝不写入项目自身的 .env 文件。
// 只有配置文件位置在 --local 模式下才是项目本地的。
type setupTargets struct {
	config string
	env    string
}

// defaultConfigTarget 返回默认配置文件路径。
// 优先使用用户全局配置文件（~/.reasonix/config.toml），
// 仅在无法解析用户配置目录时回退到项目本地 reasonix.toml。
func defaultConfigTarget() string {
	if p := config.UserConfigPath(); p != "" {
		return p
	}
	return "reasonix.toml"
}

// defaultEnvTarget 返回 reasonix 全局凭证存储的显示路径。
// 用于向导输出，告知用户 API 密钥将存储在何处。
func defaultEnvTarget() string {
	return config.CredentialsTargetDescription()
}

// resolveSetupTargets 确定 `reasonix setup` 的写入位置。
// API 密钥始终写入全局凭证存储。配置文件的写入位置取决于参数：
//   - 默认：用户全局配置目录
//   - --local/-l：项目本地 ./reasonix.toml
//   - 显式路径参数：指定的路径
//
// 参数：
//   - args: 命令行参数（不含 "setup" 子命令）
//
// 返回值：配置向导的写入目标
func resolveSetupTargets(args []string) setupTargets {
	t := setupTargets{config: defaultConfigTarget(), env: defaultEnvTarget()}
	for _, a := range args {
		switch a {
		case "--local", "-l":
			t.config = "reasonix.toml"
		default:
			t.config = a
		}
	}
	return t
}

// displayPath 将路径缩短为相对于主目录的 ~/... 形式，使向导输出更易读。
// 例如 "/home/user/.reasonix/config.toml" → "~/.reasonix/config.toml"
func displayPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

// setupConfig 实现 `reasonix setup` 子命令：运行配置向导。
//
// 写入目标：
//   - 配置文件：用户全局目录（默认）或 ./reasonix.toml（--local 模式）
//   - API 密钥：reasonix 全局凭证存储（绝不写入项目 .env）
//
// 向导流程：
//  1. 检查是否已有配置文件（非交互模式下拒绝覆盖）
//  2. 交互模式下确认是否重新配置
//  3. 运行交互式向导或写入默认配置
//
// 参数：
//   - args: 命令行参数（如 ["--local"]）
//
// 返回值：进程退出码
func setupConfig(args []string) int {
	t := resolveSetupTargets(args)
	path := t.config
	if _, err := os.Stat(path); err == nil {
		// Non-interactive must not clobber an existing config silently.
		if !isInteractive() {
			fmt.Fprintf(os.Stderr, i18n.M.NotOverwritingFmt+"\n", path)
			return 1
		}
		in := bufio.NewScanner(os.Stdin)
		if !confirmReconfigureExistingConfig(path, in, os.Stdout) {
			fmt.Println(i18n.M.KeepingExisting)
			return 0
		}
	}

	// Interactive wizard on a TTY; fall back to the annotated default when piped.
	if isInteractive() {
		rc := interactiveSetup(t.config, t.env)
		if rc == 0 {
			fmt.Printf(i18n.M.TryHintFmt+"\n", bold("reasonix"))
		}
		return rc
	}
	return writeDefaultConfig(t.config)
}

// confirmReconfigureExistingConfig 在配置文件已存在时询问用户是否重新配置。
// 返回 true 表示用户确认重新配置，false 表示保留现有配置。
func confirmReconfigureExistingConfig(path string, in *bufio.Scanner, w io.Writer) bool {
	ans := ask(in, w, fmt.Sprintf(i18n.M.ConfirmReconfigureFmt, path), "y/N")
	return ans == "y" || ans == "Y"
}

// writeDefaultConfig 写入默认配置文件。
// 用于非交互模式（管道/脚本），直接写入内置默认配置。
//
// 参数：
//   - path: 配置文件写入路径
//
// 返回值：进程退出码
func writeDefaultConfig(path string) int {
	c := config.Default()
	if err := c.SaveTo(path); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.WriteConfigErr, err)
		return 1
	}
	fmt.Printf(i18n.M.WroteFileFmt+"\n", displayPath(path))
	fmt.Println(i18n.M.NextHint)
	return 0
}

// initHint 处理 `reasonix init` 命令。
// 与配置脚手架不同，项目记忆（AGENTS.md）由模型在会话中通过分析代码库生成，
// 因此它作为会话内的 `/init` 技能存在，而非 CLI 命令。
// 此入口仅给出提示，引导用户使用正确的方式，使命令不会成为死胡同。
func initHint() int {
	fmt.Println(i18n.M.InitHint)
	return 0
}

// interactiveSetup 运行交互式配置向导。
//
// 向导流程（有意保持简洁）：
//  1. 选择语言（中文/英文）——先选语言，使后续提示都用用户语言显示
//  2. 选择启用的模型提供者（DeepSeek / 自定义 / Anthropic 等）
//  3. 输入 API 密钥
//  4. 写入配置文件和凭证存储
//
// 设计决策：双模型协作（planner_model）留作手动配置编辑，
// 首次运行不会让新手面对高级选项。
//
// 参数：
//   - configPath: 配置文件写入路径
//   - envPath: 凭证存储的显示路径（实际写入由 config.StoreCredentialLines 决定）
//
// 返回值：进程退出码（0=成功, 1=失败/取消）
func interactiveSetup(configPath, envPath string) int {
	// Seed from the existing config when reconfiguring, so a re-run to fix a key
	// preserves the user's providers / agent settings instead of resetting to
	// defaults. First run (no file) falls back to the built-in defaults.
	cfg := config.LoadForEdit(configPath)
	prevDefault := cfg.DefaultModel

	lang, err := selectLanguage()
	if err != nil {
		fmt.Fprintln(os.Stderr, "\nsetup cancelled.")
		return 1
	}
	cfg.Language = lang
	cfg.ApplyDeepSeekOfficialDefaultPricing()
	i18n.DetectLanguage(lang)

	// Now that the catalogue matches the user's choice, show the welcome banner
	// in their language before any substantive prompt.
	fmt.Println()
	fmt.Print(boxed([]string{
		accent("◆") + " " + fmt.Sprintf(i18n.M.WelcomeTitleFmt, bold("reasonix")),
		"",
		dim(i18n.M.NoConfigYet),
	}))
	fmt.Println()

	enabled, err := selectEnabledProviders(cfg.Providers, cfg.DeepSeekOfficialPricingLanguage())
	if err != nil {
		fmt.Fprintln(os.Stderr, "\n"+i18n.M.SetupCancelled)
		return 1
	}

	envLines := configureKeys(enabled, os.Stdin, os.Stdout)

	cfg.Providers = enabled
	// Keep the previous default model if it's still enabled; otherwise fall back
	// to the first selected provider.
	cfg.DefaultModel = enabled[0].Name
	for _, p := range enabled {
		if p.Name == prevDefault {
			cfg.DefaultModel = prevDefault
			break
		}
	}

	if err := cfg.SaveTo(configPath); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.WriteConfigErr, err)
		return 1
	}
	fmt.Printf("\n%s %s\n", green("✓"), fmt.Sprintf(i18n.M.WroteFileFmt, displayPath(configPath)))

	if len(envLines) > 0 {
		target, err := config.StoreCredentialLines(envLines)
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.WriteEnvErr, err)
			return 1
		}
		if target == "" {
			target = envPath
		}
		fmt.Printf("%s %s\n", green("✓"), fmt.Sprintf(i18n.M.WroteFileFmt, displayPath(target)))
	}

	fmt.Printf("\n%s %s\n", accent("◆"), i18n.M.SetupComplete)
	return 0
}

// pickSessionToResume 扫描会话目录，取最近 10 个会话，
// 显示单选菜单（包含时间戳、轮次数和首条用户消息）供用户选择。
//
// 返回值：
//   - string: 选中的会话文件路径
//   - int: 进程退出码（0=成功, 1=无可选会话或用户取消）
func pickSessionToResume() (string, int) {
	sessions, err := agent.ListSessions(resolveCLISessionDir())
	if err != nil || len(sessions) == 0 {
		fmt.Fprintln(os.Stderr, i18n.M.NoSessionToResume)
		return "", 1
	}
	if !isInteractive() {
		fmt.Fprintln(os.Stderr, i18n.M.ResumeRequiresTTY)
		return "", 1
	}
	const cap = 10
	if len(sessions) > cap {
		sessions = sessions[:cap]
	}
	items := make([]menuItem, len(sessions))
	for i, s := range sessions {
		when := s.ModTime.Local().Format("01-02 15:04")
		preview := s.Preview
		if preview == "" {
			preview = "(no user message yet)"
		}
		items[i] = menuItem{
			name: when,
			desc: fmt.Sprintf("%d turns · %s", s.Turns, preview),
		}
	}
	idx, err := selectOne(i18n.M.PickSessionLabel, items)
	if err != nil {
		return "", 1
	}
	return sessions[idx].Path, 0
}

// selectLanguage 是向导的第一个提示：以原生形式显示两种 UI 语言，
// 并预选环境检测到的语言（单按 Enter 确认自动检测，方向键+Enter 选择另一种）。
// 标签使用双语，因为此时还不确定使用哪个语言目录。
//
// 返回值：
//   - string: 语言标签（"en" 或 "zh"）
//   - error: 选择失败时返回错误
func selectLanguage() (string, error) {
	detected := i18n.DetectLanguage("")
	items := []menuItem{{name: "English"}, {name: "中文 (简体)"}}
	tags := []string{"en", "zh"}
	if detected == "zh" {
		items[0], items[1] = items[1], items[0]
		tags[0], tags[1] = tags[1], tags[0]
	}
	idx, err := selectOne("Language · 语言", items)
	if err != nil {
		return "", err
	}
	return tags[idx], nil
}

// selectEnabledProviders 显示模型提供者家族的多选菜单
// （DeepSeek / 自定义 / Anthropic 等），返回用户选择的 ProviderEntry 列表。
//
// 工作流程：
//  1. 过滤旧版向导遗留的无效条目
//  2. 合并内置提供者家族
//  3. 按家族分组显示多选菜单
//  4. 对每个选中的家族，尝试 OpenAI 兼容的 GET /models 端点获取实时模型列表
//  5. 获取失败时（离线、无密钥、不支持 /models）回退到预设的静态模型列表
//  6. 显示模型多选菜单，构建 ProviderEntry
//
// 所有路径都通过 fetchOrFallback / buildFamilyEntry 辅助函数统一处理，
// 添加新家族只需在 familyOf 中增加一个 case。
//
// 参数：
//   - providers: 当前配置中的提供者列表
//   - pricingLanguage: 价格语言（影响内置提供者的价格显示）
//
// 返回值：
//   - []config.ProviderEntry: 用户选择启用的提供者列表
//   - error: 选择失败时返回错误
func selectEnabledProviders(providers []config.ProviderEntry, pricingLanguage string) ([]config.ProviderEntry, error) {
	providers, stale := filterStaleCustomEntries(providers)
	for _, s := range stale {
		fmt.Fprintf(os.Stderr, "  %s\n", dim(fmt.Sprintf(i18n.M.SkipStaleCustomEntryFmt, s.Name, s.BaseURL)))
	}
	providers = withBuiltinFamiliesForLanguage(providers, pricingLanguage)

	famOrder, famMembers, famInfo := groupByFamily(providers)

	famItems := make([]menuItem, len(famOrder))
	for i, k := range famOrder {
		famItems[i] = menuItem{name: famInfo[k].name, desc: famInfo[k].desc}
	}
	customIdx := len(famItems)
	famItems = append(famItems, menuItem{name: i18n.M.CustomProviderLabel, desc: i18n.M.CustomProviderDesc})
	anthropicIdx := len(famItems)
	famItems = append(famItems, menuItem{name: i18n.M.AnthropicProviderLabel, desc: i18n.M.AnthropicProviderDesc})

	famIdxs, err := selectMany(i18n.M.SelectProvidersLabel, famItems)
	if err != nil {
		return nil, err
	}

	var enabled []config.ProviderEntry
	for _, fi := range famIdxs {
		switch fi {
		case customIdx:
			cps, err := promptCustomProvider()
			if err != nil {
				fmt.Fprintf(os.Stderr, "custom provider error: %v\n", err)
				continue
			}
			enabled = append(enabled, cps...)
			continue
		case anthropicIdx:
			aps, err := promptAnthropicProvider()
			if err != nil {
				fmt.Fprintf(os.Stderr, "anthropic provider error: %v\n", err)
				continue
			}
			enabled = append(enabled, aps...)
			continue
		}

		familyKey := famOrder[fi]
		probe := providers[famMembers[familyKey][0]]
		famName := famInfo[familyKey].name

		// Seed the probe's static list with every member of the family (e.g. the
		// flash and pro SKUs), not just the first — so a failed /models probe
		// falls back to the whole family instead of collapsing to one model.
		probe.Models = familyStaticModels(providers, famMembers[familyKey])

		// Collect the key before probing /models: a keyless probe 401s and the
		// fallback would hide the live SKUs. Mirrors the custom/anthropic flows;
		// configureKeys later sees the env var set and won't ask twice.
		ensureProbeKey(&probe, famName)

		models := fetchOrFallback(&probe, famName)
		if len(models) == 0 {
			fmt.Fprintf(os.Stderr, "  %s\n", dim(fmt.Sprintf(i18n.M.NoModelsAvailableFmt, famName)))
			continue
		}

		items := make([]menuItem, len(models))
		for i, m := range models {
			items[i] = menuItem{name: m}
		}
		idxs, err := selectMany(fmt.Sprintf(i18n.M.SelectModelsLabel, famName), items)
		if err != nil || len(idxs) == 0 {
			continue
		}

		selected := make([]string, 0, len(idxs))
		for _, idx := range idxs {
			selected = append(selected, models[idx])
		}
		members := make([]config.ProviderEntry, 0, len(famMembers[familyKey]))
		for _, idx := range famMembers[familyKey] {
			members = append(members, providers[idx])
		}
		enabled = append(enabled, buildFamilyEntries(probe, members, selected)...)
	}
	return enabled, nil
}

// familyStaticModels unions the preset model lists of every entry in the family,
// preserving order and dropping duplicates. It is the fallback offered when the
// live /models probe fails, so a family with separate flash/pro preset entries
// still surfaces both rather than only the first member's model.
//
// familyStaticModels 合并家族中所有成员的预设模型列表，保持顺序并去重。
// 当实时 /models 探测失败时，此列表作为回退方案提供给用户选择。
//
// 参数：
//   - providers: 完整的提供者列表
//   - idxs: 家族成员在 providers 中的索引
//
// 返回值：去重后的模型名称列表
func familyStaticModels(providers []config.ProviderEntry, idxs []int) []string {
	var out []string
	seen := map[string]bool{}
	for _, i := range idxs {
		for _, m := range providers[i].ModelList() {
			if m != "" && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// ensureProbeKey 在家族的 API 密钥未设置时提示用户输入一次，
// 使 /models 探测能够运行并返回实时的 SKU 列表。
// 输入的值直接设置到环境变量中供探测使用；
// 后续 configureKeys 会看到环境变量已设置，不会重复询问。
// 空输入也是允许的——静态回退方案会覆盖这种情况。
//
// 参数：
//   - probe: 提供者条目（读取 APIKeyEnv）
//   - famName: 家族名称（用于提示显示）
func ensureProbeKey(probe *config.ProviderEntry, famName string) {
	if probe.APIKeyEnv == "" || os.Getenv(probe.APIKeyEnv) != "" {
		return
	}
	fmt.Printf("  %s\n", dim(fmt.Sprintf(i18n.M.FamilyKeyPromptFmt, famName)))
	in := bufio.NewScanner(os.Stdin)
	if key := strings.TrimSpace(ask(in, os.Stdout, "  "+probe.APIKeyEnv, "")); key != "" {
		os.Setenv(probe.APIKeyEnv, key)
	}
}

// fetchOrFallback 尝试通过 OpenAI 兼容的 GET /models 端点获取实时模型列表
// （优先使用条目的 ModelsURL），返回模型 ID 列表。
//
// 失败时（无 base URL、密钥未设置、网络/认证错误、供应商不支持 /models）
// 静默返回预设的静态模型列表，确保向导始终有内容可显示。
// 获取有 10 秒超时，是尽力而为的操作。
//
// 参数：
//   - probe: 提供者条目（包含 BaseURL、ModelsURL 等）
//   - famName: 家族名称（用于日志/提示显示）
//
// 返回值：模型名称列表（实时获取或静态回退）
func fetchOrFallback(probe *config.ProviderEntry, famName string) []string {
	static := probe.ModelList()
	if probe.BaseURL == "" {
		return static
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	models, err := probe.FetchModels(ctx)
	if err != nil || len(models) == 0 {
		if len(static) > 0 {
			fmt.Fprintf(os.Stderr, "  %s\n", dim(fmt.Sprintf(i18n.M.FetchModelsUsingPresetsFmt, famName)))
		}
		return static
	}
	fmt.Printf("  %s\n", green(fmt.Sprintf(i18n.M.FetchModelsSuccessFmt, len(models), famName)))
	return models
}

// fetchModelListCompat 遍历给定 base URL 的所有模型列表 URL 候选项
// （根路径、/v1、已知的 OpenAI/Anthropic 兼容后缀），返回第一个成功的获取结果。
//
// 这是向导阶段对 *用户提供的* 自定义提供者的探测——其 baseURL 是用户粘贴的，
// 可能是 https://x.com（根路径，探测 /v1/models）或 https://x.com/v1
// （版本化路径，直接探测 /v1/models）。
//
// 设计背景：之前向导硬编码 `baseURL + "/models"`，对 OpenAI 格式的 URL 有效，
// 但对 Anthropic 格式的根路径会静默失败——导致向导的"有哪些模型"与聊天客户端的
// 实际端点不一致。在完全未命中时返回空切片（非错误），使向导可以回退到手动输入。
//
// 参数：
//   - ctx: 上下文（用于超时控制）
//   - baseURL: 提供者的 base URL
//   - apiKey: API 密钥（用于认证探测请求）
//
// 返回值：
//   - []string: 模型名称列表（空表示完全未命中）
//   - error: 非端点未命里的错误（如认证失败、5xx、TLS 错误）
func fetchModelListCompat(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	candidates, err := config.BuildModelFetchURLs(baseURL, "")
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, u := range candidates {
		models, err := openai.FetchModels(ctx, u, apiKey)
		if err == nil {
			return models, nil
		}
		lastErr = err
		// An endpoint-miss is not a hard error — try the next candidate.
		// Anything else (auth, 5xx, bad TLS) bubbles up immediately because
		// retrying it on a sibling URL won't help.
		if !openai.IsModelFetchEndpointMiss(err) {
			return nil, err
		}
	}
	if lastErr != nil {
		slog.Debug("model-list probe: all candidates missed", "base_url", baseURL, "err", lastErr)
	}
	return nil, nil
}

// buildFamilyEntries 将用户选择的模型分配回家族的预设成员中，
// 使每个模型保持自己的条目——从而保持各自的价格、上下文窗口和余额 URL。
//
// 例如 DeepSeek 家族将 flash 和 pro 作为独立预设（价格不同）发布；
// 如果合并为一个条目，pro 会按 flash 的费率计费。实时 /models 列表返回的
// 不匹配任何预设的新 SKU 归属到探测条目下。
//
// 参数：
//   - probe: 探测条目（包含 base URL、API key 等共享信息）
//   - members: 家族的预设成员列表
//   - selected: 用户选择的模型名称列表
//
// 返回值：按成员分组的 ProviderEntry 列表（保持成员顺序和选择顺序）
func buildFamilyEntries(probe config.ProviderEntry, members []config.ProviderEntry, selected []string) []config.ProviderEntry {
	tmpl := map[string]config.ProviderEntry{probe.Name: probe}
	ownerName := map[string]string{}
	for _, m := range members {
		tmpl[m.Name] = m
		for _, id := range m.ModelList() {
			ownerName[id] = m.Name
		}
	}
	var order []string
	groups := map[string][]string{}
	for _, sm := range selected {
		name, ok := ownerName[sm]
		if !ok {
			name = probe.Name
		}
		if _, seen := groups[name]; !seen {
			order = append(order, name)
		}
		groups[name] = append(groups[name], sm)
	}
	out := make([]config.ProviderEntry, 0, len(order))
	for _, name := range order {
		out = append(out, buildFamilyEntry(tmpl[name], groups[name]))
	}
	return out
}

// buildFamilyEntry 构建单个家族条目，将用户选择的模型列表设置到探测条目中。
// 保留预设的 API key env、base URL、kind、context window、pricing 和 effort。
// 如果 Default 指向用户未选择的模型，则重置为第一个选中的模型。
//
// 参数：
//   - probe: 探测条目模板
//   - selected: 用户选择的模型名称列表
//
// 返回值：配置好的 ProviderEntry
func buildFamilyEntry(probe config.ProviderEntry, selected []string) config.ProviderEntry {
	entry := probe
	entry.Models = selected
	entry.Model = selected[0]
	if entry.Default == "" || !containsString(selected, entry.Default) {
		entry.Default = selected[0]
	}
	return entry
}

// containsString 检查字符串切片中是否包含指定值。
// 用于简单的线性查找场景。
func containsString(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// filterStaleCustomEntries 过滤掉旧版向导写入的魔术名称条目
// （Name="custom" + Kind="openai" 或 Name="anthropic" + Kind="anthropic"）。
//
// 这些条目在重新运行向导时会与菜单项冲突，显示为重复的损坏条目。
// 新版向导使用基于主机名的 slug（如 "custom-token-sensenova-cn"），
// 因此命中魔术名称的条目确定是过期的。
//
// 返回值：
//   - kept: 保留的有效条目
//   - dropped: 被过滤的过期条目（调用方应警告用户手动清理）
func filterStaleCustomEntries(providers []config.ProviderEntry) (kept, dropped []config.ProviderEntry) {
	for _, p := range providers {
		if p.Name == "custom" && p.Kind == "openai" {
			dropped = append(dropped, p)
			continue
		}
		if p.Name == "anthropic" && p.Kind == "anthropic" {
			dropped = append(dropped, p)
			continue
		}
		kept = append(kept, p)
	}
	return
}

// providerSlug 从 base URL 派生出稳定、人类可读的自定义提供者条目名称。
// 例如 "custom-token-sensenova-cn" 或 "anthropic-api-anthropic-com"。
//
// 不能复用向导的菜单项标签（"custom"/"anthropic"），因为会与菜单项本身冲突，
// 在后续重新运行 `reasonix setup` 时显示为重复条目。
// 基于主机名的 slug 也给用户一个有意义的名称，便于在 reasonix.toml 中搜索。
// 当 URL 无法解析时，回退到原始 URL 的短 sha1 哈希，确保即使格式错误的输入也能产生唯一名称。
//
// 参数：
//   - kind: 提供者类型（"custom" 或 "anthropic"）
//   - baseURL: 提供者的 base URL
//
// 返回值：生成的 slug 名称
func providerSlug(kind, baseURL string) string {
	var host string
	if u, err := url.Parse(baseURL); err == nil {
		host = u.Host
	}
	if host == "" {
		sum := sha1.Sum([]byte(baseURL))
		return kind + "-" + hex.EncodeToString(sum[:4])
	}
	host = strings.ToLower(strings.TrimPrefix(host, "www."))
	var b strings.Builder
	prevDash := false
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	return kind + "-" + strings.TrimRight(b.String(), "-")
}

// providerFamily 是向导专用的提供者 SKU 按供应商分组。
// 配置文件中不存在此概念，因为编辑 reasonix.toml 的用户直接处理 SKU 名称。
type providerFamily struct {
	key  string
	name string
	desc string
}

// familyOf 根据提供者名称确定其所属的家族。
// 用于向导中的分组显示，例如 "deepseek-flash" 和 "deepseek-pro" 都属于 "deepseek" 家族。
//
// 参数：
//   - name: 提供者名称
//
// 返回值：提供者家族信息
func familyOf(name string) providerFamily {
	switch {
	case strings.HasPrefix(name, "deepseek"):
		return providerFamily{key: "deepseek", name: "DeepSeek", desc: "fast & cheap, plus a stronger Pro SKU"}
	default:
		return providerFamily{key: name, name: name}
	}
}

// promptCustomProvider 处理自定义提供者的添加流程。
// 显示方法选择菜单（手动输入或从 URL 获取），然后调用对应的处理函数。
//
// 返回值：
//   - []config.ProviderEntry: 用户配置的提供者条目列表
//   - error: 配置失败时返回错误
func promptCustomProvider() ([]config.ProviderEntry, error) {
	methodIdx, err := selectOne(i18n.M.CustomAddMethodLabel, []menuItem{
		{name: i18n.M.CustomMethodManual},
		{name: i18n.M.CustomMethodURL},
	})
	if err != nil {
		return nil, err
	}
	if methodIdx == 0 {
		return promptCustomProviderManual()
	}
	return promptCustomProviderFromURL()
}

// promptCustomProviderManual 处理手动输入自定义提供者信息的流程。
// 提示用户输入 base URL、API key 环境变量名、API key 和模型名称。
func promptCustomProviderManual() ([]config.ProviderEntry, error) {
	return promptCustomProviderManualWith(bufio.NewScanner(os.Stdin), "", "", "")
}

// promptCustomProviderManualWith 是手动输入的共享后端。
// 预填充值（baseURL、keyEnv、apiKey）在非空时直接复用，使 URL 获取流程
// 可以回退到手动输入而无需重复询问用户已输入的信息。
// 空 apiKey 是允许的——密钥步骤在向导的后续阶段进行，届时更新凭证存储。
//
// 参数：
//   - in: 输入扫描器
//   - baseURL: 预填充的 base URL（空则提示输入）
//   - keyEnv: 预填充的环境变量名（空则提示输入）
//   - apiKey: 预填充的 API key（空则提示输入）
//
// 返回值：
//   - []config.ProviderEntry: 配置好的提供者条目
//   - error: 输入失败时返回错误
func promptCustomProviderManualWith(in *bufio.Scanner, baseURL, keyEnv, apiKey string) ([]config.ProviderEntry, error) {
	fmt.Println()
	if baseURL == "" {
		baseURL = ask(in, os.Stdout, i18n.M.CustomPromptBaseURL, "")
		if baseURL == "" {
			return nil, fmt.Errorf("base URL is required")
		}
	}
	if keyEnv == "" {
		keyEnv = ask(in, os.Stdout, i18n.M.CustomPromptKeyEnv, "CUSTOM_API_KEY")
	}
	if apiKey == "" {
		apiKey = ask(in, os.Stdout, i18n.M.CustomPromptAPIKey, "")
	}
	if apiKey != "" {
		os.Setenv(keyEnv, apiKey)
	}
	modelName := ask(in, os.Stdout, i18n.M.CustomPromptModel, "")
	if modelName == "" {
		return nil, fmt.Errorf("model name is required")
	}
	entry := config.ProviderEntry{
		Name: providerSlug("custom", baseURL), Kind: "openai", BaseURL: baseURL,
		Model: modelName, APIKeyEnv: keyEnv, ContextWindow: 128000,
	}
	fmt.Printf("  %s\n", green(fmt.Sprintf(i18n.M.CustomAddedFmt, entry.Name+"/"+modelName)))
	return []config.ProviderEntry{entry}, nil
}

// promptCustomProviderFromURL 尝试通过 OpenAI 兼容的 GET /models 端点获取模型列表，
// 并显示返回模型的复选框供用户选择。如果请求失败（网络错误、认证失败、
// 供应商不支持 /models），则回退到手动输入，复用用户已输入的 URL 和密钥。
//
// 返回值：
//   - []config.ProviderEntry: 用户配置的提供者条目
//   - error: 配置失败时返回错误
func promptCustomProviderFromURL() ([]config.ProviderEntry, error) {
	in := bufio.NewScanner(os.Stdin)
	fmt.Println()

	baseURL := ask(in, os.Stdout, i18n.M.CustomPromptBaseURL, "")
	if baseURL == "" {
		return nil, fmt.Errorf("base URL is required")
	}
	keyEnv := ask(in, os.Stdout, i18n.M.CustomPromptKeyEnv, "CUSTOM_API_KEY")
	apiKey := ask(in, os.Stdout, i18n.M.CustomPromptAPIKey, "")
	if apiKey != "" {
		os.Setenv(keyEnv, apiKey)
	}

	fmt.Printf("  %s\n", dim(fmt.Sprintf(i18n.M.FetchingModelsFmt, "custom")))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	models, err := fetchModelListCompat(ctx, baseURL, apiKey)
	if err != nil || len(models) == 0 {
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s\n", dim(fmt.Sprintf(i18n.M.FetchModelsFailedFmt, "custom", err)))
		} else {
			fmt.Fprintf(os.Stderr, "  %s\n", dim(i18n.M.CustomFetchEmpty))
		}
		return promptCustomProviderManualWith(in, baseURL, keyEnv, apiKey)
	}
	fmt.Printf("  %s\n", green(fmt.Sprintf(i18n.M.FetchModelsSuccessFmt, len(models), "custom")))

	items := make([]menuItem, len(models))
	for i, m := range models {
		items[i] = menuItem{name: m}
	}
	idxs, err := selectMany(fmt.Sprintf(i18n.M.SelectModelsLabel, "custom"), items)
	if err != nil || len(idxs) == 0 {
		return nil, fmt.Errorf("no models selected")
	}
	var selected []string
	for _, i := range idxs {
		selected = append(selected, models[i])
	}
	entry := config.ProviderEntry{
		Name: providerSlug("custom", baseURL), Kind: "openai", BaseURL: baseURL,
		Models: selected, Model: selected[0], APIKeyEnv: keyEnv, ContextWindow: 128000,
	}
	fmt.Printf("  %s\n", green(fmt.Sprintf(i18n.M.CustomAddedFmt, entry.Name+"/"+selected[0])))
	return []config.ProviderEntry{entry}, nil
}

// promptAnthropicProvider 处理 Anthropic 兼容提供者的添加流程。
// 显示方法选择菜单（手动输入或从 URL 获取），然后调用对应的处理函数。
//
// 返回值：
//   - []config.ProviderEntry: 用户配置的提供者条目列表
//   - error: 配置失败时返回错误
func promptAnthropicProvider() ([]config.ProviderEntry, error) {
	methodIdx, err := selectOne(i18n.M.AnthropicAddMethodLabel, []menuItem{
		{name: i18n.M.AnthropicMethodManual},
		{name: i18n.M.AnthropicMethodURL},
	})
	if err != nil {
		return nil, err
	}
	if methodIdx == 0 {
		return promptAnthropicProviderManual()
	}
	return promptAnthropicProviderFromURL()
}

// promptAnthropicProviderManual 处理手动输入 Anthropic 兼容提供者信息的流程。
func promptAnthropicProviderManual() ([]config.ProviderEntry, error) {
	return promptAnthropicProviderManualWith(bufio.NewScanner(os.Stdin), "", "", "")
}

// promptAnthropicProviderManualWith 是 Anthropic 兼容提供者手动输入的共享后端。
// 预填充值在非空时直接复用，使 URL 获取流程可以回退到手动输入而无需重复询问。
func promptAnthropicProviderManualWith(in *bufio.Scanner, baseURL, keyEnv, apiKey string) ([]config.ProviderEntry, error) {
	fmt.Println()
	if baseURL == "" {
		baseURL = ask(in, os.Stdout, i18n.M.AnthropicPromptBaseURL, "")
		if baseURL == "" {
			return nil, fmt.Errorf("base URL is required")
		}
	}
	if keyEnv == "" {
		keyEnv = ask(in, os.Stdout, i18n.M.AnthropicPromptKeyEnv, "ANTHROPIC_API_KEY")
	}
	if apiKey == "" {
		apiKey = ask(in, os.Stdout, i18n.M.AnthropicPromptAPIKey, "")
	}
	if apiKey != "" {
		os.Setenv(keyEnv, apiKey)
	}
	modelName := ask(in, os.Stdout, i18n.M.AnthropicPromptModel, "")
	if modelName == "" {
		return nil, fmt.Errorf("model name is required")
	}
	entry := config.ProviderEntry{
		Name: providerSlug("anthropic", baseURL), Kind: "anthropic", BaseURL: baseURL,
		Model: modelName, APIKeyEnv: keyEnv, ContextWindow: 128000,
	}
	fmt.Printf("  %s\n", green(fmt.Sprintf(i18n.M.AnthropicAddedFmt, entry.Name+"/"+modelName)))
	return []config.ProviderEntry{entry}, nil
}

// promptAnthropicProviderFromURL 尝试通过 OpenAI 兼容的 GET /models 端点获取模型列表
// （部分 Anthropic 兼容代理确实暴露了此端点）。大多数不支持——
// Anthropic 自身的 API 没有公开的模型列表——因此任何失败都会回退到手动输入，
// 并复用已填写的 URL 和密钥，而不是中止向导。
func promptAnthropicProviderFromURL() ([]config.ProviderEntry, error) {
	in := bufio.NewScanner(os.Stdin)
	fmt.Println()

	baseURL := ask(in, os.Stdout, i18n.M.AnthropicPromptBaseURL, "")
	if baseURL == "" {
		return nil, fmt.Errorf("base URL is required")
	}
	keyEnv := ask(in, os.Stdout, i18n.M.AnthropicPromptKeyEnv, "ANTHROPIC_API_KEY")
	apiKey := ask(in, os.Stdout, i18n.M.AnthropicPromptAPIKey, "")
	if apiKey != "" {
		os.Setenv(keyEnv, apiKey)
	}

	fmt.Printf("  %s\n", dim(fmt.Sprintf(i18n.M.AnthropicFetchingModelsFmt, "anthropic")))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	models, err := fetchModelListCompat(ctx, baseURL, apiKey)
	if err != nil || len(models) == 0 {
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s\n", dim(fmt.Sprintf(i18n.M.AnthropicFetchModelsFailedFmt, "anthropic", err)))
		} else {
			fmt.Fprintf(os.Stderr, "  %s\n", dim(i18n.M.AnthropicFetchEmpty))
		}
		return promptAnthropicProviderManualWith(in, baseURL, keyEnv, apiKey)
	}
	fmt.Printf("  %s\n", green(fmt.Sprintf(i18n.M.AnthropicFetchModelsSuccessFmt, len(models), "anthropic")))

	items := make([]menuItem, len(models))
	for i, m := range models {
		items[i] = menuItem{name: m}
	}
	idxs, err := selectMany(fmt.Sprintf(i18n.M.AnthropicSelectModelsLabel, "anthropic"), items)
	if err != nil || len(idxs) == 0 {
		return nil, fmt.Errorf("no models selected")
	}
	var selected []string
	for _, i := range idxs {
		selected = append(selected, models[i])
	}
	entry := config.ProviderEntry{
		Name: providerSlug("anthropic", baseURL), Kind: "anthropic", BaseURL: baseURL,
		Models: selected, Model: selected[0], APIKeyEnv: keyEnv, ContextWindow: 128000,
	}
	fmt.Printf("  %s\n", green(fmt.Sprintf(i18n.M.AnthropicAddedFmt, entry.Name+"/"+selected[0])))
	return []config.ProviderEntry{entry}, nil
}

// groupByFamily 将提供者列表按家族分组，返回家族顺序、成员索引映射和家族信息。
// 用于向导中的分组显示和选择。
//
// 参数：
//   - providers: 提供者列表
//
// 返回值：
//   - []string: 家族键的顺序列表
//   - map[string][]int: 家族键 → 成员在 providers 中的索引列表
//   - map[string]providerFamily: 家族键 → 家族信息
func groupByFamily(providers []config.ProviderEntry) ([]string, map[string][]int, map[string]providerFamily) {
	var order []string
	members := map[string][]int{}
	info := map[string]providerFamily{}
	for i, p := range providers {
		f := familyOf(p.Name)
		if _, seen := members[f.key]; !seen {
			order = append(order, f.key)
			info[f.key] = f
		}
		members[f.key] = append(members[f.key], i)
	}
	return order, members, info
}

// withBuiltinFamilies 保证向导始终提供内置的 DeepSeek 家族，
// 即使加载的配置替换了默认值。已存在于用户配置中的内置条目保持不变（保留自定义）；
// 现有家族中缺失的内置条目会被追加，使模型选择器始终显示完整目录。
func withBuiltinFamilies(providers []config.ProviderEntry) []config.ProviderEntry {
	return withBuiltinFamiliesForLanguage(providers, "")
}

// withBuiltinFamiliesForLanguage 与 withBuiltinFamilies 类似，
// 但接受价格语言参数以支持不同语言的价格显示。
//
// 参数：
//   - providers: 当前提供者列表
//   - pricingLanguage: 价格语言（如 "zh"、"en"）
//
// 返回值：合并了内置家族的提供者列表
func withBuiltinFamiliesForLanguage(providers []config.ProviderEntry, pricingLanguage string) []config.ProviderEntry {
	haveName := map[string]bool{}
	for _, p := range providers {
		haveName[p.Name] = true
	}
	defaults := config.Default()
	defaults.Language = pricingLanguage
	defaults.ApplyDeepSeekOfficialDefaultPricing()
	for _, bp := range defaults.Providers {
		if !haveName[bp.Name] {
			providers = append(providers, bp)
		}
	}
	return providers
}

// providersWithMissingKeys 返回活动配置实际引用的（默认/规划/子智能体模型）
// 且 api_key_env 已声明但未设置的提供者。
// 仅可用的提供者保持静默；聊天横幅仍会在用户切换到密钥缺失的模型时发出警告。
//
// 参数：
//   - cfg: 配置对象
//
// 返回值：密钥缺失的提供者列表
func providersWithMissingKeys(cfg *config.Config) []config.ProviderEntry {
	if cfg == nil {
		return nil
	}
	refs := []string{
		cfg.DefaultModel,
		cfg.Agent.PlannerModel,
		cfg.Agent.SubagentModel,
	}
	if !strings.EqualFold(strings.TrimSpace(cfg.Agent.AutoPlan), "off") {
		refs = append(refs, cfg.Agent.AutoPlanClassifier)
	}
	if len(cfg.Agent.SubagentModels) > 0 {
		keys := make([]string, 0, len(cfg.Agent.SubagentModels))
		for key := range cfg.Agent.SubagentModels {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			refs = append(refs, cfg.Agent.SubagentModels[key])
		}
	}

	var out []config.ProviderEntry
	seen := map[string]bool{}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		p, ok := cfg.ResolveModel(ref)
		if !ok || p.APIKeyEnv == "" || os.Getenv(p.APIKeyEnv) != "" || seen[p.APIKeyEnv] {
			continue
		}
		seen[p.APIKeyEnv] = true
		out = append(out, *p)
	}
	return out
}

// configureKeys 协调每个已启用提供者的 API 密钥与环境变量。
//
// 对每个不同的 api_key_env：
//   - 如果变量已设置，询问是否重新输入；Enter 保持并重新固定现有值
//   - 否则每个环境变量询问一次（跨共享同一变量的提供者去重，如两个 DeepSeek 模型）
//
// 返回 KEY=value 行列表，用于写入凭证存储。重新固定很重要，因为 loadDotEnv
// 是"先到先得"的，凭证文件中较早留下的过期密钥会遮蔽新值。
//
// 参数：
//   - selected: 已启用的提供者列表
//   - r: 输入源（通常是 os.Stdin）
//   - w: 输出目标（通常是 os.Stdout）
//
// 返回值：KEY=value 格式的环境变量行列表
func configureKeys(selected []config.ProviderEntry, r io.Reader, w io.Writer) []string {
	in := bufio.NewScanner(r)
	fmt.Fprintln(w, "\n"+i18n.M.EnterAPIKeysHeader)

	seen := map[string]bool{}
	var envLines []string
	for _, p := range selected {
		if p.APIKeyEnv == "" || seen[p.APIKeyEnv] {
			continue
		}
		seen[p.APIKeyEnv] = true

		if cur := os.Getenv(p.APIKeyEnv); cur != "" {
			reset := ask(in, w, "  "+fmt.Sprintf(i18n.M.APIKeyResetPromptFmt, p.APIKeyEnv), "y/N")
			if reset == "y" || reset == "Y" {
				if key := ask(in, w, "  "+p.APIKeyEnv, ""); key != "" {
					envLines = append(envLines, p.APIKeyEnv+"="+key)
					continue
				}
			}
			fmt.Fprintf(w, "  %s %s\n", green("✓"), fmt.Sprintf(i18n.M.APIKeyAlreadySetFmt, p.APIKeyEnv))
			envLines = append(envLines, p.APIKeyEnv+"="+cur)
			continue
		}

		if key := ask(in, w, "  "+p.APIKeyEnv, ""); key != "" {
			envLines = append(envLines, p.APIKeyEnv+"="+key)
		}
	}
	return envLines
}

// ask 向 w 输出提示并返回输入行，输入为空时返回默认值 def。
//
// 参数：
//   - in: 输入扫描器
//   - w: 输出目标
//   - label: 提示标签
//   - def: 默认值（输入为空时使用）
//
// 返回值：用户输入的字符串或默认值
func ask(in *bufio.Scanner, w io.Writer, label, def string) string {
	if def != "" {
		fmt.Fprintf(w, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(w, "%s: ", label)
	}
	if !in.Scan() {
		return def
	}
	if v := strings.TrimSpace(in.Text()); v != "" {
		return v
	}
	return def
}

// isInteractive 检测是否连接到真正的终端（stdin 和 stdout 都是 TTY）。
// 这是交互式提示的前提条件。重定向或管道的 I/O 不是交互式的，
// 因此向导在脚本和 CI 中永远不会阻塞或自动使用默认值。
func isInteractive() bool {
	return isTTY(os.Stdin) && isTTY(os.Stdout)
}

// isTTY 检测给定文件是否为终端设备。
func isTTY(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// appendEnv 将 KEY=value 行合并到 .env 文件中。
//
// 处理逻辑：
//  1. 先删除文件中即将写入的键的现有赋值
//  2. 然后追加新值——使重新运行 `reasonix setup` 时修正的密钥替换过期密钥，
//     而不是堆叠重复项（loadDotEnv 是"先到先得"的，简单追加会使旧密钥继续生效）
//  3. 新值也被固定到当前进程环境变量中，使 init 后立即启动的聊天会话
//     无需重启即可获取新密钥
//
// 参数：
//   - path: .env 文件路径
//   - lines: KEY=value 格式的行列表
//
// 返回值：写入失败时返回错误
func appendEnv(path string, lines []string) error {
	target := map[string]bool{}
	for _, l := range lines {
		if k, _, ok := strings.Cut(l, "="); ok {
			target[strings.TrimSpace(k)] = true
		}
	}

	var kept []string
	if data, err := os.ReadFile(path); err == nil {
		for _, raw := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(raw)
			check := strings.TrimPrefix(trimmed, "export ")
			if k, _, ok := strings.Cut(check, "="); ok && target[strings.TrimSpace(k)] {
				continue
			}
			kept = append(kept, raw)
		}
		// strings.Split on a string ending with \n leaves a trailing empty
		// element; trim it so we don't grow a blank line on every rewrite.
		if n := len(kept); n > 0 && kept[n-1] == "" {
			kept = kept[:n-1]
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	var b strings.Builder
	for _, l := range kept {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
		if k, v, ok := strings.Cut(l, "="); ok {
			os.Setenv(strings.TrimSpace(k), v)
		}
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// readStdin 读取管道输入（如果存在）；交互式终端返回空字符串。
// 用于 `reasonix run` 从管道接收提示，如 `echo "hello" | reasonix run`。
func readStdin() string {
	stat, err := os.Stdin.Stat()
	if err != nil || stat.Mode()&os.ModeCharDevice != 0 {
		return ""
	}
	data, _ := io.ReadAll(os.Stdin)
	return strings.TrimSpace(string(data))
}

// usage 输出 CLI 的用法信息。
func usage() {
	fmt.Print(i18n.M.UsageBody)
}

// configCommand 实现 `reasonix config` 子命令：配置管理。
// 支持的子命令：auto-plan、reasoning-language。
//
// 参数：
//   - args: 子命令参数（如 ["auto-plan", "on"]）
//
// 返回值：进程退出码
func configCommand(args []string) int {
	if len(args) == 0 {
		configUsage()
		return 2
	}
	switch args[0] {
	case "auto-plan":
		return configAutoPlanCommand(args[1:])
	case "reasoning-language":
		return configReasoningLanguageCommand(args[1:])
	default:
		configUsage()
		return 2
	}
}

// configAutoPlanCommand 实现 `reasonix config auto-plan` 子命令。
// 读取或设置自动计划模式（off/on）。自动计划模式是用户级设置，不支持 --local。
//
// 参数：
//   - args: 子命令参数（如 ["on"] 或 [] 用于读取当前值）
//
// 返回值：进程退出码
func configAutoPlanCommand(args []string) int {
	fs := flag.NewFlagSet("config auto-plan", flag.ContinueOnError)
	local := fs.Bool("local", false, "unsupported; auto-plan is user-level only")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *local {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "auto-plan is user-level only; --local is not supported")
		return 2
	}
	rest := fs.Args()
	if len(rest) > 1 {
		configAutoPlanUsage()
		return 2
	}
	if len(rest) == 0 {
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
		mode := cfg.Agent.AutoPlan
		mode = cliAutoPlanMode(mode)
		fmt.Printf("auto_plan = %q\n", mode)
		return 0
	}
	path := config.UserConfigPath()
	if path == "" {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "cannot resolve config path")
		return 1
	}
	cfg := config.LoadForEdit(path)
	if err := cfg.SetAutoPlan(rest[0]); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}
	if err := cfg.SaveTo(path); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	fmt.Printf("auto_plan = %q (%s)\n", cfg.Agent.AutoPlan, displayPath(path))
	return 0
}

// configReasoningLanguageCommand 实现 `reasonix config reasoning-language` 子命令。
// 读取或设置推理语言（auto/zh/en）。支持 --local 标志写入项目本地配置。
//
// 参数：
//   - args: 子命令参数（如 ["--local", "zh"] 或 [] 用于读取当前值）
//
// 返回值：进程退出码
func configReasoningLanguageCommand(args []string) int {
	fs := flag.NewFlagSet("config reasoning-language", flag.ContinueOnError)
	local := fs.Bool("local", false, "write ./reasonix.toml instead of the user config")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) > 1 {
		configReasoningLanguageUsage()
		return 2
	}
	if len(rest) == 0 {
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
		fmt.Printf("reasoning_language = %q\n", cliReasoningLanguageMode(cfg.ReasoningLanguage()))
		return 0
	}
	mode, err := parseCLIReasoningLanguage(rest[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}
	path := config.UserConfigPath()
	if *local {
		path = "reasonix.toml"
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "cannot resolve config path")
		return 1
	}
	if *local {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			lang, err := config.SaveMinimalProjectReasoningLanguage(path, mode)
			if err != nil {
				fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
				return 1
			}
			fmt.Printf("reasoning_language = %q (%s)\n", lang, displayPath(path))
			return 0
		} else if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
	}
	cfg := config.LoadForEdit(path)
	if err := cfg.SetReasoningLanguage(mode); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}
	if err := cfg.SaveTo(path); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	fmt.Printf("reasoning_language = %q (%s)\n", cfg.ReasoningLanguage(), displayPath(path))
	return 0
}

// configUsage 输出 `reasonix config` 的用法信息。
func configUsage() {
	fmt.Print(`Usage:
  reasonix config auto-plan [off|on]
  reasonix config reasoning-language [--local] [auto|zh|en]
`)
}

// configAutoPlanUsage 输出 `reasonix config auto-plan` 的用法信息。
func configAutoPlanUsage() {
	fmt.Print(`Usage:
  reasonix config auto-plan [off|on]
`)
}

// configReasoningLanguageUsage 输出 `reasonix config reasoning-language` 的用法信息。
func configReasoningLanguageUsage() {
	fmt.Print(`Usage:
  reasonix config reasoning-language [--local] [auto|zh|en]
`)
}
