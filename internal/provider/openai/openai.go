// Package openai 实现了 OpenAI 兼容的 /chat/completions 提供者。
//
// 自注册为 "openai" 类型，因此 DeepSeek、MiMo、MiniMax-M3 以及其他任何 OpenAI 兼容端点
// 都只是配置实例，无需额外代码。每个实例根据 base URL 自动选择 wire 格式：
//   - api.deepseek.com → 发送 thinking.type=enabled（DeepSeek 思维链）+ reasoning_effort 控制深度
//   - api.minimaxi.com → 发送 thinking.type=adaptive|disabled（M3 的二元开关）替代 reasoning_effort
//   - 其他端点（MiMo 等）使用标准 reasoning_effort 比例尺（low/medium/high）
//
// 核心特性:
//   - SSE 流式解析: 逐行解析 Server-Sent Events，实时转发文本/推理/工具调用增量
//   - 空闲超时看门狗: 120 秒无数据视为连接中断，避免 scanner.Scan() 永久阻塞
//   - 流重连逻辑: 连接中断且未输出 token 时自动重放请求（最多 3 次）
//   - ThinkSplitter: 解析 MiniMax 的 <think> 标签，将内联思维链提取为推理增量
//   - 工具调用累积: 按 index 累积流式工具调用片段，在 [DONE] 时发出完整调用
//   - 自动修复: 发送前调用 SanitizeToolPairing 修复工具调用配对问题
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"reasonix/internal/netclient"
	"reasonix/internal/provider"
)

// defaultStreamIdleTimeout 是 SSE 流的最大空闲超时时间。
// 半开的 TCP 连接（如代理在流式传输中切换）不会发送 RST，scanner.Scan() 会永久阻塞；
// 此超时将挂起转为可恢复错误。120 秒足够宽裕——正常流的 token/keepalive 远比这频繁。
// 存储在 client.idleTimeout 中，测试可缩短而不影响其他流的看门狗。
const defaultStreamIdleTimeout = 120 * time.Second

// init 在包加载时自动注册 "openai" 类型的提供者工厂。
// 主程序通过 provider.New("openai", cfg) 即可创建实例。
func init() {
	provider.Register("openai", New)
}

// New 从已解析的配置构建 OpenAI 兼容提供者。
//
// 初始化流程:
//   1. 验证必需字段（BaseURL、Model）
//   2. 根据 BaseURL 自动检测后端类型（DeepSeek/MiniMax/通用）
//   3. 根据后端类型验证和规范化 reasoning_effort 参数
//   4. 创建带代理支持的 HTTP 客户端
//   5. 返回配置好的 client 实例
func New(cfg provider.Config) (provider.Provider, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("openai: base_url is required for provider %q", cfg.Name)
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("openai: model is required for provider %q", cfg.Name)
	}
	name := cfg.Name
	if name == "" {
		name = "openai"
	}
	keyEnv, _ := cfg.Extra["api_key_env"].(string) // for actionable auth errors
	keySource, _ := cfg.Extra["api_key_source"].(string)
	effort, _ := cfg.Extra["effort"].(string)
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "auto" {
		effort = ""
	}
	protocol, _ := cfg.Extra["reasoning_protocol"].(string)
	protocol = normalizeReasoningProtocol(protocol)
	vision, _ := cfg.Extra["vision"].(bool)
	visionDetail, _ := cfg.Extra["vision_detail"].(string)
	visionDetail = strings.ToLower(strings.TrimSpace(visionDetail))
	if visionDetail != "low" && visionDetail != "high" {
		visionDetail = "" // auto — omit the field
	}
	deepseek := protocol == "deepseek" || (protocol == "" && IsDeepSeek(cfg.BaseURL))
	minimax := protocol == "" && IsMiniMax(cfg.BaseURL)
	switch {
	case protocol == "none":
		effort = ""
	case deepseek:
		switch effort {
		case "", "off": // "off" is a retired level (disabled thinking); fall back to the default depth
			effort = "high"
		case "high", "max":
		default:
			return nil, fmt.Errorf("openai: provider %q uses DeepSeek thinking; effort must be high or max", name)
		}
	case minimax:
		// M3's knob is binary. The config effort layer normalises user input
		// to "adaptive", "disabled", or "" (== auto). We keep "high"/"max"
		// (legacy DeepSeek) and "low"/"medium" (Anthropic) out — config-level
		// NormalizeEffort remaps them to "adaptive" already, so anything
		// reaching here is expected to be one of: "", "adaptive", "disabled".
		effort = strings.ToLower(strings.TrimSpace(effort))
		switch effort {
		case "": // auto — leave empty so the wire emits thinking.type=adaptive
		case "adaptive", "disabled":
		default:
			return nil, fmt.Errorf("openai: provider %q uses MiniMax thinking; effort must be adaptive or disabled", name)
		}
	case effort != "":
		// Non-DeepSeek backends use OpenAI's reasoning_effort scale (low/medium/
		// high); "max" is a DeepSeek-ism MiMo et al. reject with 400, so clamp it
		// to the OpenAI ceiling and reject other values at boot, not at request time.
		switch effort {
		case "max":
			effort = "high"
		case "low", "medium", "high":
		default:
			return nil, fmt.Errorf("openai: provider %q: effort must be low, medium, or high", name)
		}
	}
	httpClient, err := newHTTPClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("openai: network: %w", err)
	}
	return &client{
		name:         name,
		apiKey:       cfg.APIKey,
		keyEnv:       keyEnv,
		keySource:    keySource,
		baseURL:      strings.TrimRight(cfg.BaseURL, "/"),
		model:        cfg.Model,
		deepseek:     deepseek,
		minimax:      minimax,
		vision:       vision,
		visionDetail: visionDetail,
		effort:       effort,
		http:         httpClient,
		idleTimeout:  defaultStreamIdleTimeout,
	}, nil
}

// newHTTPClient 创建带代理支持的 HTTP 客户端。
// 配置连接超时、keepalive、TLS 握手超时和响应头超时（模型可能思考很久才输出第一个 token）。
func newHTTPClient(cfg provider.Config) (*http.Client, error) {
	spec, _ := cfg.Extra["proxy_spec"].(netclient.ProxySpec)
	return netclient.NewHTTPClient(spec, netclient.TransportOptions{
		DialTimeout:           30 * time.Second,
		KeepAlive:             30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second, // models can think for a while before the first token
	})
}

// client 是 OpenAI 兼容提供者的内部实现。
// 每个实例对应一个配置好的模型端点（如 DeepSeek Reasoner、MiMo、MiniMax-M3）。
type client struct {
	name         string        // 提供者实例名称（如 "deepseek"、"mimo"）
	apiKey       string        // API 密钥
	keyEnv       string        // 密钥来源的环境变量名，用于认证错误信息
	keySource    string        // 密钥来源的人类可读描述
	baseURL      string        // API 端点基础 URL（不含尾部斜杠）
	model        string        // 模型标识符
	http         *http.Client  // 带代理支持的 HTTP 客户端
	deepseek     bool          // 是否为 DeepSeek 后端（影响 thinking 协议和 reasoning_effort）
	minimax      bool          // 是否为 MiniMax 后端（api.minimaxi.com），使用 thinking.type 替代 reasoning_effort
	vision       bool          // 是否支持图片输入（嵌入 image_url 内容块）
	visionDetail string        // 图片细节级别提示（low|high），"" 表示自动/省略
	effort       string        // 推理深度参数：OpenAI 用 reasoning_effort，MiniMax 用 thinking.type，"" 表示自动
	idleTimeout  time.Duration // SSE 空闲看门狗超时窗口，测试可覆盖
	authed       atomic.Bool   // 是否曾有请求成功（用于判断是否重试瞬态 401）
}

func (c *client) Name() string { return c.name }

// sendOpts 构建发送选项，携带提供者上下文信息用于错误标记和认证重试判断。
func (c *client) sendOpts() provider.SendOptions {
	return provider.SendOptions{
		Provider:   c.name,
		KeyEnv:     c.keyEnv,
		KeySource:  c.keySource,
		KeyPresent: c.apiKey != "",
		RetryAuth:  c.authed.Load(),
	}
}

// normalizeReasoningProtocol 规范化推理协议配置值。
// 支持 "deepseek"、"openai"、"none"，其他值返回空字符串（自动检测）。
func normalizeReasoningProtocol(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "deepseek", "openai", "none":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return ""
	}
}

// bufPool reuses byte buffers for JSON-marshalled request bodies. Each turn
// allocates a buffer, marshals the request, and sends it — pooling avoids the
// GC churn from repeated alloc/free of ~10-100KB buffers. The pool is
// provider-level (not global) so OpenAI and Anthropic don't compete.
var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// Stream 启动流式补全，将增量事件推送到返回的 channel。
//
// 流程:
//   1. 构建请求体（JSON 编码），使用 bufPool 复用缓冲区减少 GC 压力
//   2. 通过 SendWithRetry 发送请求（带重试和退避）
//   3. 标记认证成功（authed），后续瞬态 401 可重试
//   4. 启动 goroutine 执行带重连的流式读取
func (c *client) Stream(ctx context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	if err := json.NewEncoder(buf).Encode(c.buildRequest(req)); err != nil {
		bufPool.Put(buf)
		return nil, fmt.Errorf("%s: marshal request: %w", c.name, err)
	}
	body := make([]byte, buf.Len())
	copy(body, buf.Bytes())
	bufPool.Put(buf)

	newReq := func(ctx context.Context) (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		httpReq.Header.Set("Accept", "text/event-stream")
		return httpReq, nil
	}
	resp, err := provider.SendWithRetry(ctx, c.http, c.sendOpts(), newReq)
	if err != nil {
		return nil, err
	}
	c.authed.Store(true)

	out := make(chan provider.Chunk)
	go c.streamWithReconnect(ctx, resp, newReq, out)
	return out, nil
}

// maxStreamReconnects bounds how many times a mid-stream connection drop is
// replayed from scratch before the error is surfaced — each replay re-runs the
// whole request (cheap under prompt caching, but not free).
const maxStreamReconnects = 3

// streamWithReconnect 驱动 readStream 并在连接中断时尝试重连。
//
// 重连策略:
//   - 未输出任何 token 时: 可安全重放请求（最多 maxStreamReconnects 次）
//   - 已输出 token 时: 不能重放（会导致重复输出），返回 StreamInterruptedError
//   - 非连接重置错误: 直接返回错误（如解码错误、API 错误）
func (c *client) streamWithReconnect(ctx context.Context, resp *http.Response, newReq func(context.Context) (*http.Request, error), out chan<- provider.Chunk) {
	defer close(out)
	for attempt := 0; ; attempt++ {
		emitted, err := c.readStream(ctx, resp, out)
		if err == nil {
			return
		}
		if !provider.IsConnReset(err) {
			out <- provider.Chunk{Type: provider.ChunkError, Err: err}
			return
		}
		if emitted {
			out <- provider.Chunk{Type: provider.ChunkError, Err: &provider.StreamInterruptedError{Err: err}}
			return
		}
		if attempt >= maxStreamReconnects {
			out <- provider.Chunk{Type: provider.ChunkError, Err: err}
			return
		}
		next, rerr := provider.SendWithRetry(ctx, c.http, c.sendOpts(), newReq)
		if rerr != nil {
			out <- provider.Chunk{Type: provider.ChunkError, Err: rerr}
			return
		}
		resp = next
	}
}

// buildRequest 将传输无关的 Request 转换为 OpenAI 兼容的 wire format。
//
// 关键处理:
//   - 修复工具调用配对（SanitizeToolPairing）
//   - DeepSeek 思维链: 回传 reasoning_content（防止 400 错误）
//   - 图片消息: 转换为 image_url 内容块（视觉模型）
//   - 后端特定的 thinking 协议: DeepSeek 用 thinking.type=enabled，MiniMax 用 adaptive/disabled
func (c *client) buildRequest(req provider.Request) chatRequest {
	// Repair tool-call pairing before sending: an interrupted/resumed history can
	// carry an assistant tool_calls turn whose results never landed, which DeepSeek
	// rejects with a 400 ("must be followed by tool messages …").
	src := provider.SanitizeToolPairing(req.Messages)
	msgs := make([]chatMessage, len(src))
	for i, m := range src {
		cm := chatMessage{
			Role:       string(m.Role),
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		// DeepSeek thinking mode 400s a tool_calls turn whose reasoning_content was
		// dropped on a cache-miss replay ("reasoning_content … must be passed back"),
		// so round it back — but only on the turn that carries the tool calls.
		if c.deepseek && m.Role == provider.RoleAssistant && len(m.ToolCalls) > 0 {
			cm.ReasoningContent = m.ReasoningContent
		}
		for _, tc := range m.ToolCalls {
			wire := chatToolCall{ID: tc.ID, Type: "function"}
			wire.Function.Name = tc.Name
			wire.Function.Arguments = tc.Arguments
			cm.ToolCalls = append(cm.ToolCalls, wire)
		}
		switch {
		case c.vision && m.Role == provider.RoleUser && len(m.Images) > 0:
			cm.Content = imageContentParts(m.Content, m.Images, c.visionDetail)
		case m.Role != provider.RoleAssistant || len(cm.ToolCalls) == 0 || m.Content != "":
			cm.Content = m.Content
		}
		msgs[i] = cm
	}

	var tools []chatTool
	for _, t := range req.Tools {
		tools = append(tools, chatTool{
			Type:     "function",
			Function: chatFunction{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
		})
	}

	out := chatRequest{
		Model:           c.model,
		Messages:        msgs,
		Tools:           tools,
		Stream:          true,
		StreamOptions:   &streamOptions{IncludeUsage: true},
		Temperature:     req.Temperature,
		MaxTokens:       req.MaxTokens,
		ReasoningEffort: c.effort,
	}
	switch {
	case c.deepseek:
		// DeepSeek's CoT is controlled by `thinking` (always on) plus
		// `reasoning_effort` for depth. We never disable thinking for DeepSeek.
		out.Thinking = &thinkingMode{Type: "enabled"}
	case c.minimax:
		// M3 uses a single `thinking.type` field with two valid values:
		// "adaptive" (default, thinking on) and "disabled" (off). Reasoning
		// depth is not a knob on M3, so reasoning_effort is omitted entirely.
		t := c.effort
		if t == "" {
			t = "adaptive" // /effort auto == the M3 model default
		}
		out.Thinking = &thinkingMode{Type: t}
		out.ReasoningEffort = ""
	}
	return out
}

// readStream 解析一个 SSE 响应为流式增量事件。
//
// SSE 解析流程:
//   1. 逐行扫描响应体（bufio.Scanner）
//   2. 跳过空行和非 "data:" 前缀的行（SSE 协议中的注释和事件类型）
//   3. 解析 JSON 数据到 streamResponse 结构
//   4. 根据 delta 字段类型分发事件:
//      - reasoning_content → ChunkReasoning（DeepSeek 思维链）
//      - content → 通过 thinkSplitter 提取 MiniMax 的 <think> 标签，剩余为 ChunkText
//      - tool_calls → 按 index 累积，name 可用时立即发出 ChunkToolCallStart
//   5. 遇到 [DONE] 时，发出所有累积的工具调用和 ChunkDone
//
// 空闲看门狗:
//   - 启动后台 goroutine 监控数据活动
//   - 120 秒无数据视为连接中断，关闭响应体使 scanner.Scan() 退出
//   - 每次读到数据时重置计时器
//
// 返回值:
//   - emitted: 是否已转发任何模型输出（用于判断是否可安全重连）
//   - error: nil 表示流正常结束，非 nil 表示致命错误
func (c *client) readStream(ctx context.Context, resp *http.Response, out chan<- provider.Chunk) (emitted bool, _ error) {
	defer resp.Body.Close()

	// Close the response body when the context is canceled (user interrupt) or the
	// stream stalls past c.idleTimeout, so scanner.Scan() unblocks instead of
	// hanging on a half-open connection. done lets the watchdog exit on a normal
	// return — otherwise it outlives the call and blocks forever on a non-cancellable
	// context whose Done() is nil. The watchdog owns the timer; the read loop only
	// pings the buffered activity channel, so there's no Timer.Reset race.
	idleTimeout := c.idleTimeout
	if idleTimeout <= 0 { // zero-value client (constructed without New)
		idleTimeout = defaultStreamIdleTimeout
	}
	done := make(chan struct{})
	defer close(done)
	activity := make(chan struct{}, 1)
	var stalled atomic.Bool
	go func() {
		idle := time.NewTimer(idleTimeout)
		defer idle.Stop()
		for {
			select {
			case <-ctx.Done():
				resp.Body.Close()
				return
			case <-idle.C:
				stalled.Store(true)
				resp.Body.Close()
				return
			case <-activity:
				if !idle.Stop() {
					select {
					case <-idle.C:
					default:
					}
				}
				idle.Reset(idleTimeout)
			case <-done:
				return
			}
		}
	}()

	acc := map[int]*provider.ToolCall{}
	started := map[int]bool{}
	var order []int
	var lastFinishReason string
	var sawDone bool
	var think thinkSplitter

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		select { // ping the idle watchdog; non-blocking so a full buffer is fine
		case activity <- struct{}{}:
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			sawDone = true
			break
		}

		var sr streamResponse
		if err := json.Unmarshal([]byte(data), &sr); err != nil {
			return emitted, fmt.Errorf("%s: decode stream: %w", c.name, err)
		}
		if sr.Error != nil {
			return emitted, fmt.Errorf("%s: %s", c.name, sr.Error.Message)
		}
		if len(sr.Choices) > 0 && sr.Choices[0].FinishReason != nil && *sr.Choices[0].FinishReason != "" {
			lastFinishReason = *sr.Choices[0].FinishReason
		}
		if sr.Usage != nil {
			u := normaliseUsage(sr.Usage)
			u.FinishReason = lastFinishReason
			emitted = true
			out <- provider.Chunk{Type: provider.ChunkUsage, Usage: u}
		}
		if len(sr.Choices) == 0 {
			continue
		}

		delta := sr.Choices[0].Delta
		if delta.ReasoningContent != "" {
			emitted = true
			out <- provider.Chunk{Type: provider.ChunkReasoning, Text: delta.ReasoningContent}
		}
		if delta.Content != "" {
			r, txt := think.push(delta.Content)
			if r != "" {
				emitted = true
				out <- provider.Chunk{Type: provider.ChunkReasoning, Text: r}
			}
			if txt != "" {
				emitted = true
				out <- provider.Chunk{Type: provider.ChunkText, Text: txt}
			}
		}
		for _, tc := range delta.ToolCalls {
			cur, ok := acc[tc.Index]
			if !ok {
				cur = &provider.ToolCall{}
				acc[tc.Index] = cur
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Function.Name != "" {
				cur.Name = tc.Function.Name
			}
			cur.Arguments += tc.Function.Arguments
			// Signal the call's start the moment its name is known, so a frontend
			// can show the tool card immediately rather than only after its
			// (possibly large) arguments finish streaming.
			if !started[tc.Index] && cur.Name != "" {
				started[tc.Index] = true
				emitted = true
				out <- provider.Chunk{Type: provider.ChunkToolCallStart, ToolCall: &provider.ToolCall{ID: cur.ID, Name: cur.Name}}
			}
		}
	}

	if stalled.Load() {
		return emitted, fmt.Errorf("%s: stream stalled — no data for %s, connection likely dropped", c.name, idleTimeout)
	}
	if err := scanner.Err(); err != nil {
		return emitted, fmt.Errorf("%s: read stream: %w", c.name, err)
	}
	// A proxy that idle-closes with a clean FIN ends the scan with no error. Without
	// this check the turn would be committed as complete — including half-streamed
	// tool-call arguments, which then 400 on every replay (#3953).
	if !sawDone && lastFinishReason == "" {
		return emitted, fmt.Errorf("%s: stream ended before completion: %w", c.name, io.ErrUnexpectedEOF)
	}

	if r, txt := think.flush(); r != "" || txt != "" {
		if r != "" {
			out <- provider.Chunk{Type: provider.ChunkReasoning, Text: r}
		}
		if txt != "" {
			out <- provider.Chunk{Type: provider.ChunkText, Text: txt}
		}
	}

	sort.Ints(order)
	for _, idx := range order {
		tc := acc[idx]
		if tc.ID == "" {
			// Some OpenAI-compatible gateways stream tool calls by index with no id.
			// Synthesize a stable one so the result can be paired back to its call —
			// an empty tool_call_id collapses multi-tool turns downstream.
			tc.ID = fmt.Sprintf("call_%d", idx)
		}
		out <- provider.Chunk{Type: provider.ChunkToolCall, ToolCall: tc}
	}
	out <- provider.Chunk{Type: provider.ChunkDone}
	return emitted, nil
}

// normaliseUsage 将 OpenAI 兼容生态中的两种缓存命中格式归一化为统一的 Usage。
//
// 两种格式:
//   - DeepSeek: 顶层 prompt_cache_hit_tokens / prompt_cache_miss_tokens
//   - OpenAI/MiMo: 嵌套的 prompt_tokens_details.cached_tokens
//
// 归一化策略: 哪边报告非零就用哪边；仅知道 hit 时从 PromptTokens 推导 miss。
// 推理 token 从 completion_tokens_details.reasoning_tokens 提取。
func normaliseUsage(u *wireUsage) *provider.Usage {
	hit := u.PromptCacheHitTokens
	miss := u.PromptCacheMissTokens
	if hit == 0 && u.PromptTokensDetails != nil {
		hit = u.PromptTokensDetails.CachedTokens
	}
	if miss == 0 && hit > 0 && u.PromptTokens > hit {
		miss = u.PromptTokens - hit
	}
	reasoning := 0
	if u.CompletionTokensDetails != nil {
		reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	return &provider.Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		CacheHitTokens:   hit,
		CacheMissTokens:  miss,
		ReasoningTokens:  reasoning,
	}
}

// --- OpenAI 兼容 wire protocol 类型定义 ---
// 以下类型定义了 OpenAI /chat/completions API 的请求和响应 JSON 结构。

// chatRequest 是发送到 /chat/completions 的请求体。
type chatRequest struct {
	Model           string         `json:"model"`
	Messages        []chatMessage  `json:"messages"`
	Tools           []chatTool     `json:"tools,omitempty"`
	Stream          bool           `json:"stream"`
	StreamOptions   *streamOptions `json:"stream_options,omitempty"`
	Temperature     float64        `json:"temperature,omitempty"`
	MaxTokens       int            `json:"max_tokens,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	Thinking        *thinkingMode  `json:"thinking,omitempty"`
}

// thinkingMode 控制 DeepSeek/MiniMax 的思维模式。
// DeepSeek: type="enabled"（始终开启）
// MiniMax: type="adaptive"（默认，自动决定）或 "disabled"
type thinkingMode struct {
	Type string `json:"type"`
}

// streamOptions 控制流式响应的附加选项。
// IncludeUsage=true 使服务器在流末尾发送 usage 统计。
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// chatMessage 是请求中的单条消息。
// Content 字段始终存在（不省略）：DeepSeek 的严格反序列化器会拒绝缺少此字段的消息。
// 纯 tool_calls 的 assistant 回合序列化为 null（nil）；
// 其他文本消息为字符串（空字符串也行，null 会被某些后端拒绝）；
// 携带图片的 vision 用户回合为 []chatContentPart 数组。
type chatMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content"`                      // null | string | []chatContentPart
	ReasoningContent string         `json:"reasoning_content,omitempty"`  // DeepSeek 思维链内容（需回传）
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`         // assistant 消息的工具调用列表
	ToolCallID       string         `json:"tool_call_id,omitempty"`       // tool 消息关联的调用 ID
	Name             string         `json:"name,omitempty"`               // tool 消息的工具名称
}

// chatContentPart 是 vision 消息的内容块，支持文本和图片两种类型。
type chatContentPart struct {
	Type     string        `json:"type"`                // "text" 或 "image_url"
	Text     string        `json:"text,omitempty"`      // 文本内容
	ImageURL *chatImageURL `json:"image_url,omitempty"` // 图片 URL
}

// chatImageURL 是图片内容块的 URL 和细节级别。
type chatImageURL struct {
	URL    string `json:"url"`               // data URL 或 HTTP URL
	Detail string `json:"detail,omitempty"`  // 细节级别（low/high），"" 表示自动
}

// imageContentParts 将文本和图片列表组合为 OpenAI 格式的内容块数组。
// 文本块在前，图片块在后。
func imageContentParts(text string, images []string, detail string) []chatContentPart {
	parts := make([]chatContentPart, 0, len(images)+1)
	if text != "" {
		parts = append(parts, chatContentPart{Type: "text", Text: text})
	}
	for _, url := range images {
		parts = append(parts, chatContentPart{Type: "image_url", ImageURL: &chatImageURL{URL: url, Detail: detail}})
	}
	return parts
}

// chatTool 是请求中的工具定义。
type chatTool struct {
	Type     string       `json:"type"`     // 始终为 "function"
	Function chatFunction `json:"function"` // 函数定义
}

// chatFunction 是工具的函数定义。
type chatFunction struct {
	Name        string          `json:"name"`                  // 函数名称
	Description string          `json:"description,omitempty"` // 函数描述
	Parameters  json.RawMessage `json:"parameters,omitempty"`  // 参数的 JSON Schema
}

// chatToolCall 是响应中的工具调用（流式增量）。
// Index 用于累积同一个工具调用的多个片段；ID 和 Name 在首个片段中到达；
// Arguments 随后续片段逐步累积。
type chatToolCall struct {
	Index    int    `json:"index"`           // 工具调用的索引（用于多工具调用场景）
	ID       string `json:"id,omitempty"`    // 调用 ID（首个片段到达）
	Type     string `json:"type,omitempty"`  // 始终为 "function"
	Function struct {
		Name      string `json:"name"`      // 函数名称（首个片段到达）
		Arguments string `json:"arguments"` // 参数 JSON（逐步累积）
	} `json:"function"`
}

// streamResponse 是 SSE 流中的单条 JSON 数据。
// 每条 data: 行解析为此结构，根据字段内容分发为不同的 Chunk 事件。
type streamResponse struct {
	Choices []struct {
		Delta struct {
			Content          string         `json:"content"`           // 文本增量
			ReasoningContent string         `json:"reasoning_content"` // DeepSeek 思维链增量
			ToolCalls        []chatToolCall `json:"tool_calls"`        // 工具调用增量
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"` // 终止原因（最后一条消息）
	} `json:"choices"`
	Usage *wireUsage `json:"usage"` // token 使用量（仅在 stream_options.include_usage=true 时出现）
	Error *struct {
		Message string `json:"message"` // 错误消息
	} `json:"error"`
}

// wireUsage 覆盖 DeepSeek 的顶层缓存字段和 OpenAI/MiMo 的嵌套详情。
// normaliseUsage 选择报告非零值的一侧进行归一化。
type wireUsage struct {
	PromptTokens          int `json:"prompt_tokens"`           // 输入 token 总数
	CompletionTokens      int `json:"completion_tokens"`       // 输出 token 总数
	TotalTokens           int `json:"total_tokens"`            // 总 token 数
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"` // DeepSeek: 缓存命中 token 数
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"` // DeepSeek: 缓存未命中 token 数
	PromptTokensDetails   *struct {
		CachedTokens int `json:"cached_tokens"` // OpenAI/MiMo: 缓存命中 token 数
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"` // 推理 token 数（思维模式模型）
	} `json:"completion_tokens_details"`
}
