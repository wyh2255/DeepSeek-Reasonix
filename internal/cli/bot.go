// bot.go 实现了 "reasonix bot" 子命令的 CLI 入口，提供多渠道 IM 机器人网关功能。
// 支持 QQ、飞书（Feishu/Lark）、微信（WeChat）等平台的消息接入。
// 主要功能包括：
//   - bot start: 启动机器人网关，连接各 IM 平台并处理消息
//   - bot doctor: 诊断机器人配置和环境变量是否正确
//   - bot weixin-login: 微信 iLink 二维码登录流程
//
// 所有密钥均通过环境变量读取，不存储在配置文件中。

package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"reasonix/internal/bot"
	"reasonix/internal/bot/weixin"
	"reasonix/internal/botruntime"
	"reasonix/internal/config"
)

// botCommand 是 "reasonix bot" 的顶层子命令分发器。
// 根据第一个参数（start/doctor/weixin-login/help）分发到对应的处理函数。
// 返回值为进程退出码：0 表示成功，1 表示运行错误，2 表示参数错误。
func botCommand(args []string, version string) int {
	if len(args) < 1 {
		botUsage()
		return 2
	}

	sub := args[0]
	rest := args[1:]

	switch sub {
	case "start":
		return botStart(rest, version)
	case "doctor":
		return botDoctor(rest)
	case "weixin-login":
		return botWeixinLogin(rest)
	case "help", "--help", "-h":
		botUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown bot subcommand %q\n\n", sub)
		botUsage()
		return 2
	}
}

// botStart 处理 "reasonix bot start" 命令，启动多渠道 IM 机器人网关。
// 支持的标志参数：
//   - --channels: 逗号分隔的平台列表（qq,feishu,lark,weixin）
//   - --dir: 工作目录路径
//   - --model: 模型名称（为空则使用配置中的默认模型）
//
// 启动流程包括：加载配置 -> 校验白名单 -> 构建网关配置 -> 注册信号处理 -> 启动网关。
func botStart(args []string, version string) int {
	fs := flag.NewFlagSet("bot start", flag.ContinueOnError)
	channels := fs.String("channels", "", "启用的平台，逗号分隔：qq,feishu,lark,weixin")
	dir := fs.String("dir", "", "工作目录")
	model := fs.String("model", "", "模型名（空则用 default_model）")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := loadBotCommandConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		return 1
	}

	if !cfg.Bot.Enabled {
		fmt.Fprintln(os.Stderr, "error: bot is not enabled in config — set [bot] enabled = true")
		return 1
	}
	if !cfg.Bot.Allowlist.AllowAll && (!cfg.Bot.Allowlist.Enabled || botruntime.AllowlistUserCount(cfg.Bot.Allowlist) == 0) {
		fmt.Fprintln(os.Stderr, "error: bot requires an explicit allowlist; set [bot.allowlist] enabled = true with platform user ids, or set allow_all = true intentionally")
		return 1
	}

	workspaceRoot := *dir
	if workspaceRoot == "" {
		if wd, err := os.Getwd(); err == nil {
			workspaceRoot = wd
		}
	}

	requestedChannels := splitBotChannels(*channels)
	enabledPlatforms, unknownChannels := botruntime.EnabledPlatforms(cfg, requestedChannels)
	for _, ch := range unknownChannels {
		fmt.Fprintf(os.Stderr, "warning: unknown channel %q\n", ch)
	}
	if !botruntime.HasEnabledPlatform(enabledPlatforms) {
		fmt.Fprintln(os.Stderr, "error: no bot channels enabled — enable at least one in config")
		return 1
	}

	modelName := botruntime.ModelName(cfg, *model)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// 创建远程消息记忆器，用于记录入站消息
	rememberInboundRemote := botruntime.NewRemoteRememberer(logger)

	// 构建网关配置
	gwCfg := bot.GatewayConfig{
		Model:              modelName,
		ToolApprovalMode:   cfg.Bot.ToolApprovalMode,
		MaxSteps:           cfg.Bot.MaxSteps,
		WorkspaceRoot:      workspaceRoot,
		Channels:           botruntime.ChannelConfigs(cfg.Bot.Connections, *model == "", *dir == ""),
		ConnectionChannels: botruntime.ConnectionChannelConfigs(cfg.Bot.Connections, *model == "", *dir == ""),
		Enabled:            enabledPlatforms,
		Allowlist: bot.AllowlistConfig{
			Enabled:  cfg.Bot.Allowlist.Enabled,
			AllowAll: cfg.Bot.Allowlist.AllowAll,
			Users: map[bot.Platform][]string{
				bot.PlatformQQ:     cfg.Bot.Allowlist.QQUsers,
				bot.PlatformFeishu: cfg.Bot.Allowlist.FeishuUsers,
				bot.PlatformWeixin: cfg.Bot.Allowlist.WeixinUsers,
			},
			Groups: map[bot.Platform][]string{
				bot.PlatformQQ:     cfg.Bot.Allowlist.QQGroups,
				bot.PlatformFeishu: cfg.Bot.Allowlist.FeishuGroups,
				bot.PlatformWeixin: cfg.Bot.Allowlist.WeixinGroups,
			},
		},
		Debounce:       time.Duration(cfg.Bot.DebounceMs) * time.Millisecond,
		OnInbound:      rememberInboundRemote,
		OnSessionReady: botruntime.NewSessionRemembererWithWorkspace(logger, workspaceRoot),
	}

	feishuDomains := botruntime.RequestedFeishuDomains(requestedChannels)
	gw := bot.NewGatewayWithAdapterBindings(gwCfg, botruntime.AdapterBindings(cfg, enabledPlatforms, feishuDomains, logger), logger)

	// 注册系统信号处理，收到 SIGINT/SIGTERM 时优雅关闭网关
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nshutting down...")
		cancel()
		gw.Stop()
	}()

	fmt.Fprintf(os.Stderr, "reasonix bot starting (model: %s, channels: %s)...\n", modelName, *channels)
	fmt.Fprintf(os.Stderr, "version: %s\n", version)

	if err := gw.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: start gateway: %v\n", err)
		return 1
	}

	// 等待信号或 context 取消
	<-ctx.Done()
	return 0
}

// splitBotChannels 将逗号分隔的渠道字符串拆分为切片。
// 例如 "qq,feishu" -> ["qq", "feishu"]。
func splitBotChannels(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

// botDoctor 处理 "reasonix bot doctor" 命令，对机器人配置进行全面诊断。
// 检查项目包括：bot 启用状态、各平台（QQ/飞书/微信）的配置完整性、
// 环境变量是否设置、连接配置、白名单配置等。
// 支持 --json 标志以 JSON 格式输出诊断结果。
func botDoctor(args []string) int {
	fs := flag.NewFlagSet("bot doctor", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "JSON 格式输出")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := loadBotCommandConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		return 1
	}

	bc := cfg.Bot

	type checkResult struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail,omitempty"`
	}

	var results []checkResult

	addCheck := func(name, status, detail string) {
		results = append(results, checkResult{Name: name, Status: status, Detail: detail})
	}

	// 基础检查
	if bc.Enabled {
		addCheck("bot.enabled", "ok", "")
	} else {
		addCheck("bot.enabled", "disabled", "bot is not enabled in config")
	}

	// QQ 检查
	if bc.QQ.Enabled {
		addCheck("bot.qq.enabled", "ok", "")
		secret := os.Getenv(bc.QQ.AppSecretEnv)
		if secret == "" {
			addCheck("bot.qq.app_secret", "missing", bc.QQ.AppSecretEnv+" is not set")
		} else {
			addCheck("bot.qq.app_secret", "ok", bc.QQ.AppSecretEnv+" is set")
		}
		if bc.QQ.AppID == "" {
			addCheck("bot.qq.app_id", "missing", "app_id is empty")
		} else {
			addCheck("bot.qq.app_id", "ok", "app_id configured")
		}
	} else {
		addCheck("bot.qq", "disabled", "")
	}

	// 飞书检查
	if bc.Feishu.Enabled {
		addCheck("bot.feishu.enabled", "ok", "")
		secret := os.Getenv(bc.Feishu.AppSecretEnv)
		if secret == "" {
			addCheck("bot.feishu.app_secret", "missing", bc.Feishu.AppSecretEnv+" is not set")
		} else {
			addCheck("bot.feishu.app_secret", "ok", bc.Feishu.AppSecretEnv+" is set")
		}
		if bc.Feishu.AppID == "" {
			addCheck("bot.feishu.app_id", "missing", "app_id is empty")
		} else {
			addCheck("bot.feishu.app_id", "ok", "app_id configured")
		}
		mode := bc.Feishu.Mode
		if mode == "" {
			mode = "webhook"
		}
		addCheck("bot.feishu.mode", "ok", mode)
	} else {
		addCheck("bot.feishu", "disabled", "")
	}

	// 微信检查
	if bc.Weixin.Enabled {
		addCheck("bot.weixin.enabled", "ok", "")
		token := os.Getenv(bc.Weixin.TokenEnv)
		if token != "" {
			addCheck("bot.weixin.token", "ok", bc.Weixin.TokenEnv+" is set")
		} else if weixin.HasSavedAccount(bc.Weixin.AccountID) {
			addCheck("bot.weixin.token", "ok", "saved iLink account is available")
		} else {
			addCheck("bot.weixin.token", "missing", bc.Weixin.TokenEnv+" is not set; run `reasonix bot weixin-login` to save an iLink account")
		}
	} else {
		addCheck("bot.weixin", "disabled", "")
	}

	enabledConnections := 0
	for _, conn := range bc.Connections {
		if conn.Enabled {
			enabledConnections++
		}
	}
	addCheck("bot.connections", "ok", fmt.Sprintf("enabled=%d total=%d", enabledConnections, len(bc.Connections)))
	for _, conn := range bc.Connections {
		id := strings.TrimSpace(conn.ID)
		if id == "" {
			id = strings.TrimSpace(conn.Provider)
		}
		status := "ok"
		if !conn.Enabled {
			status = "disabled"
		} else if len(conn.SessionMappings) == 0 && (conn.Provider == string(bot.PlatformFeishu) || conn.Provider == string(bot.PlatformWeixin)) {
			status = "missing"
		}
		addCheck("bot.connection."+id+".session_mappings", status,
			fmt.Sprintf("provider=%s mappings=%d", conn.Provider, len(conn.SessionMappings)))
	}

	// Allowlist 检查
	if bc.Allowlist.AllowAll {
		addCheck("bot.allowlist", "open", "allow_all=true — every reachable user can trigger local tools")
	} else if bc.Allowlist.Enabled {
		addCheck("bot.allowlist", "enabled",
			fmt.Sprintf("qq=%d feishu=%d weixin=%d users",
				len(bc.Allowlist.QQUsers),
				len(bc.Allowlist.FeishuUsers),
				len(bc.Allowlist.WeixinUsers)))
	} else {
		addCheck("bot.allowlist", "missing", "bot start will refuse without allowlist or allow_all=true")
	}

	if *jsonOut {
		fmt.Println("[")
		for i, r := range results {
			comma := ","
			if i == len(results)-1 {
				comma = ""
			}
			fmt.Printf("  {\"name\":%q,\"status\":%q,\"detail\":%q}%s\n", r.Name, r.Status, r.Detail, comma)
		}
		fmt.Println("]")
	} else {
		for _, r := range results {
			marker := "✓"
			if r.Status == "missing" || r.Status == "disabled" {
				marker = "✗"
			}
			fmt.Printf("  %s %s: %s", marker, r.Name, r.Status)
			if r.Detail != "" {
				fmt.Printf(" — %s", r.Detail)
			}
			fmt.Println()
		}
	}

	return 0
}

// botWeixinLogin 处理 "reasonix bot weixin-login" 命令，执行微信 iLink 二维码登录。
// 登录成功后凭据会保存到 Reasonix 用户配置目录。
// 支持 --timeout 标志设置登录超时时间（默认 480 秒）。
func botWeixinLogin(args []string) int {
	fs := flag.NewFlagSet("bot weixin-login", flag.ContinueOnError)
	timeoutSeconds := fs.Int("timeout", 480, "登录超时时间（秒）")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := loadBotCommandConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		return 1
	}

	if !cfg.Bot.Weixin.Enabled {
		fmt.Fprintln(os.Stderr, "error: weixin bot is not enabled in config")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSeconds)*time.Second)
	defer cancel()
	result, err := weixin.Login(ctx, os.Stdout, time.Duration(*timeoutSeconds)*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: weixin login failed: %v\n", err)
		return 1
	}
	fmt.Printf("\n微信登录成功: account_id=%s user_id=%s base_url=%s\n", result.AccountID, result.UserID, result.BaseURL)
	fmt.Println("凭据已保存到 Reasonix 用户配置目录；也可以把 [bot.weixin] account_id 设置为该 account_id。")

	return 0
}

// loadBotCommandConfig 加载机器人命令所需的配置。
// 先加载全局配置，然后检查用户级配置文件。
// 如果用户配置中包含机器人相关设置，则合并覆盖全局配置。
func loadBotCommandConfig() (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	userPath := config.UserConfigPath()
	if strings.TrimSpace(userPath) == "" {
		return cfg, nil
	}
	if _, err := os.Stat(userPath); err != nil {
		return cfg, nil
	}
	userCfg := config.LoadForEdit(userPath)
	if botConfigIsUserOwned(userCfg.Bot) {
		cfg.Bot = userCfg.Bot
	}
	return cfg, nil
}

// botConfigIsUserOwned 判断机器人配置是否包含用户自定义的设置。
// 当配置中启用了 bot、配置了连接、启用了任何平台、或设置了白名单时返回 true。
// 用于决定是否用用户配置覆盖全局配置。
func botConfigIsUserOwned(bc config.BotConfig) bool {
	if bc.Enabled || len(bc.Connections) > 0 || bc.QQ.Enabled || bc.Feishu.Enabled || bc.Weixin.Enabled {
		return true
	}
	if bc.Allowlist.AllowAll || botruntime.AllowlistUserCount(bc.Allowlist) > 0 {
		return true
	}
	return len(bc.Allowlist.QQGroups)+len(bc.Allowlist.FeishuGroups)+len(bc.Allowlist.WeixinGroups) > 0
}

// botUsage 打印 "reasonix bot" 命令的使用帮助信息，包括子命令说明和配置指引。
func botUsage() {
	fmt.Print(`reasonix bot — multi-channel IM bot gateway (QQ / Feishu / WeChat)

Usage:
  reasonix bot start   [--channels qq,feishu,lark,weixin] [--dir PATH] [--model NAME]
  reasonix bot doctor  [--json]
  reasonix bot weixin-login [--timeout SECONDS]

Subcommands:
  start         启动 bot 网关
  doctor        诊断 bot 配置和连通性
  weixin-login  微信 iLink 二维码登录

Examples:
  reasonix bot start --channels qq,feishu
  reasonix bot start --dir /path/to/project --model deepseek-pro
  reasonix bot doctor --json

Configuration:
  Edit reasonix.toml:
    [bot]           enabled / model / max_steps
    [bot.allowlist]  enabled / qq_users / feishu_users / weixin_users
    [bot.qq]         enabled / app_id / app_secret_env
    [bot.feishu]     enabled / app_id / app_secret_env / verification_token / mode
    [bot.weixin]     enabled / account_id / token_env / api_base

  All secrets are read from environment variables; never put keys in config files.
`)
}
