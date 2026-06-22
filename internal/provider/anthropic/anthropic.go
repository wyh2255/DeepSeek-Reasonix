// Package anthropic 实现了 Anthropic Messages API 提供者（POST /v1/messages，SSE 流式）。
// 使用纯 net/http 客户端实现（无 SDK 依赖），自注册为 "anthropic" 类型。
//
// 设计要点:
//
//   - Extended Thinking（扩展思维）: 通过配置 thinking="adaptive" 开启。
//     Anthropic 要求当工具调用紧随思维链之后时，必须回传带签名的 thinking block。
//     Message 结构体通过 ReasoningSignature 字段支持签名的往返传递。
//     默认关闭，因为此字段是 Anthropic 特有的——OpenAI 兼容网关（如 DeepSeek）会拒绝它。
//     （redacted_thinking blocks 尚未支持捕获/回传。）
//
//   - 无 temperature/top_p: 当前 Claude 模型（Opus 4.8/4.7）会以 400 拒绝采样参数；
//     Anthropic 通过 prompting 引导行为。
//
//   - Prompt 缓存: 在 system、tools、messages 的最后一个块上设置 ephemeral 缓存控制，
//     实现增量缓存命中。
//
//   - 消息转换: 传输无关的 Message 转换为 Anthropic 的 content block 格式，
//     包括 system 消息提升到顶层、tool_use/tool_result 块的转换、连续同角色消息合并。
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
const defaultStreamIdleTimeout = 120 * time.Second

const (
	// anthropicVersion 是必需的 API 版本头部值。
	anthropicVersion = "2023-06-01"
	// defaultBaseURL 是 Anthropic 官方端点；配置可覆盖（如使用网关）。
	// Bedrock/Vertex 使用不同的请求格式，不在范围内。
	defaultBaseURL = "https://api.anthropic.com"
	// defaultMaxTokens 是请求未指定 MaxTokens 时的默认输出上限。
	// Anthropic *要求* max_tokens 字段，agent 当前不设置此值，因此这是实际上限。
	// 32K 足够慷慨（只为实际生成的 token 付费），且在所有模型限制内（Sonnet/Haiku 64K，Opus 128K）。
	defaultMaxTokens = 32768
)

// init 在包加载时自动注册 "anthropic" 类型的提供者工厂。
func init() {
	provider.Register("anthropic", New)
}

// New 从已解析的配置构建 Anthropic 提供者。
//
// 初始化流程:
//   1. 验证必需字段（Model）
//   2. 处理 baseURL：去除尾部 /v1（用户可能粘贴完整的 OpenAI 兼容 URL）
//   3. 解析 thinking、effort、vision 等配置
//   4. 创建 HTTP 客户端
//   5. 返回配置好的 client 实例
func New(cfg provider.Config) (provider.Provider, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("anthropic: model is required for provider %q", cfg.Name)
	}
	name := cfg.Name
	if name == "" {
		name = "anthropic"
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	keyEnv, _ := cfg.Extra["api_key_env"].(string) // for actionable auth errors
	keySource, _ := cfg.Extra["api_key_source"].(string)
	thinking, _ := cfg.Extra["thinking"].(string)
	effort, _ := cfg.Extra["effort"].(string)
	vision, _ := cfg.Extra["vision"].(bool)
	httpClient, err := newHTTPClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("anthropic: network: %w", err)
	}
	// Anthropic's API surface is at {root}/v1/messages, so c.baseURL stores
	// the *root* — without any trailing /v1. The setup wizard, however, lets
	// users paste a full OpenAI-compatible URL (e.g.
	// "https://proxy.example.com/v1") because that's what /models probes
	// expect. Stripping the trailing /v1 here makes both forms land on the
	// same endpoint without forcing users to remember Anthropic's quirky
	// root-vs-versioned split. Without this, a user pasting
	// "https://proxy.example.com/v1" would probe /v1/models successfully
	// but get the chat client concatenating onto
	// "https://proxy.example.com/v1/v1/messages" — a 404.
	root := strings.TrimRight(baseURL, "/")
	root = strings.TrimSuffix(root, "/v1")
	if root == "" {
		root = defaultBaseURL
	}
	return &client{
		name:        name,
		apiKey:      cfg.APIKey,
		keyEnv:      keyEnv,
		keySource:   keySource,
		baseURL:     root,
		model:       cfg.Model,
		thinking:    thinking,
		effort:      effort,
		vision:      vision,
		http:        httpClient, // no overall timeout; lifecycle is ctx-driven
		idleTimeout: defaultStreamIdleTimeout,
	}, nil
}

// newHTTPClient 创建带代理支持的 HTTP 客户端。
// Anthropic 的 HTTP 客户端无整体超时——生命周期由 context 驱动。
func newHTTPClient(cfg provider.Config) (*http.Client, error) {
	spec, _ := cfg.Extra["proxy_spec"].(netclient.ProxySpec)
	return netclient.NewHTTPClient(spec, netclient.TransportOptions{})
}

// client 是 Anthropic 提供者的内部实现。
type client struct {
	name        string        // 提供者实例名称（如 "anthropic"、"claude"）
	apiKey      string        // API 密钥
	keyEnv      string        // 密钥来源的环境变量名，用于认证错误信息
	keySource   string        // 密钥来源的人类可读描述
	baseURL     string        // API 根 URL（不含 /v1）
	model       string        // 模型标识符（如 "claude-opus-4-0520"）
	thinking    string        // "adaptive" 开启扩展思维，"" 关闭
	effort      string        // 输出配置: low|medium|high|xhigh|max，"" 使用提供者默认值
	vision      bool          // 是否支持图片输入（嵌入 base64 image blocks）
	http        *http.Client  // 带代理支持的 HTTP 客户端
	idleTimeout time.Duration // SSE 空闲看门狗超时窗口
	authed      atomic.Bool   // 是否曾有请求成功（用于判断是否重试瞬态 401）
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

// bufPool reuses byte buffers for JSON-marshalled request bodies, reducing GC
// churn from repeated alloc/free of ~10-100KB buffers per turn.
var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// Stream 启动流式补全，将增量事件推送到返回的 channel。
//
// 流程:
//   1. 构建请求体（JSON 编码），使用 bufPool 复用缓冲区减少 GC 压力
//   2. 通过 SendWithRetry 发送请求（带重试和退避）
//   3. 标记认证成功（authed），后续瞬态 401 可重试
//   4. 启动 goroutine 执行流式读取（Anthropic 不支持流重连）
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
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq.Header.Set("x-api-key", c.apiKey)
		httpReq.Header.Set("anthropic-version", anthropicVersion)
		return httpReq, nil
	}
	resp, err := provider.SendWithRetry(ctx, c.http, c.sendOpts(), newReq)
	if err != nil {
		return nil, err
	}
	c.authed.Store(true)

	out := make(chan provider.Chunk)
	go c.readStream(resp, out)
	return out, nil
}

// buildRequest 将传输无关的 Request 转换为 Anthropic Messages API 格式。
//
// 消息转换规则:
//   - RoleSystem → 提升到顶层 system 字段（Anthropic 不支持 system role 消息）
//   - RoleUser → user 消息，图片转为 base64 image blocks（如果开启 vision）
//   - RoleTool → tool_result blocks，合并到前一个 user 消息中
//   - RoleAssistant → assistant 消息，包含 text/tool_use/thinking blocks
//     - 思维链回传: 当 thinking 开启且有签名时，插入 thinking block
//     - 工具调用: 转换为 tool_use blocks（input 为空时填充 {}）
//
// 连续同角色消息合并: API 要求 user/assistant 严格交替（tool_result 属于 user 回合）。
func (c *client) buildRequest(req provider.Request) anthRequest {
	var system []textBlock
	var msgs []anthMessage

	// appendBlocks adds blocks under role, merging into the previous message when
	// it shares the role (keeps user/assistant strictly alternating).
	appendBlocks := func(role string, blocks ...contentBlock) {
		if len(blocks) == 0 {
			return
		}
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Content = append(msgs[n-1].Content, blocks...)
			return
		}
		msgs = append(msgs, anthMessage{Role: role, Content: blocks})
	}

	for _, m := range provider.SanitizeToolPairing(req.Messages) {
		switch m.Role {
		case provider.RoleSystem:
			if m.Content != "" {
				system = append(system, textBlock{Type: "text", Text: m.Content})
			}
		case provider.RoleUser:
			if m.Content != "" {
				appendBlocks("user", contentBlock{Type: "text", Text: m.Content})
			}
			if c.vision {
				for _, url := range m.Images {
					if mt, data, ok := provider.ParseImageDataURL(url); ok {
						appendBlocks("user", contentBlock{Type: "image", Source: &imageSource{Type: "base64", MediaType: mt, Data: data}})
					}
				}
			}
		case provider.RoleTool:
			content := m.Content
			if content == "" {
				content = "(no output)" // tool_result content must be non-empty
			}
			appendBlocks("user", contentBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: content})
		case provider.RoleAssistant:
			var blocks []contentBlock
			// Replay the signed thinking block first (Anthropic requires it precede
			// the tool_use it led to). Only when thinking is on and we have both the
			// text and its signature — reasoning without a signature (e.g. from an
			// openai-compatible provider) can't be replayed as a thinking block.
			if c.thinking != "" && m.ReasoningContent != "" && m.ReasoningSignature != "" {
				blocks = append(blocks, contentBlock{Type: "thinking", Thinking: m.ReasoningContent, Signature: m.ReasoningSignature})
			}
			if m.Content != "" {
				blocks = append(blocks, contentBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Arguments)
				if len(input) == 0 {
					input = json.RawMessage("{}") // input is required, even when empty
				}
				blocks = append(blocks, contentBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: input})
			}
			appendBlocks("assistant", blocks...)
		}
	}

	var tools []anthTool
	for _, t := range req.Tools {
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, anthTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}

	// Prompt 缓存断点（ephemeral，前缀匹配）。
	// 渲染顺序: tools → system → messages。
	// 在最后一个 system 块上标记可缓存 tools+system；
	// 无 system 时标记最后一个 tool。
	// 在最后一条消息的最后一个块上标记可缓存对话前缀，
	// 随着回合追加增量累积命中。最多 4 个断点，我们使用 ≤2 个。
	if n := len(system); n > 0 {
		system[n-1].CacheControl = ephemeral()
	} else if n := len(tools); n > 0 {
		tools[n-1].CacheControl = ephemeral()
	}
	if n := len(msgs); n > 0 {
		if k := len(msgs[n-1].Content); k > 0 {
			msgs[n-1].Content[k-1].CacheControl = ephemeral()
		}
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	r := anthRequest{
		Model:     c.model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  msgs,
		Tools:     tools,
		Stream:    true,
	}
	// Extended thinking is opt-in and Anthropic-specific (a compatible gateway like
	// DeepSeek's would reject the field). "summarized" display streams the reasoning
	// text; the default omits it but still emits the signature we round-trip.
	if c.thinking == "adaptive" {
		r.Thinking = &thinkingConfig{Type: "adaptive", Display: "summarized"}
		if c.effort != "" {
			r.OutputConfig = &outputConfig{Effort: c.effort}
		}
	}
	return r
}

// readStream 解析 Anthropic Messages API 的 SSE 流为 Chunk 事件。
//
// 事件处理:
//   - message_start: 提取输入 token 和缓存统计（inTok/cacheCreate/cacheRead）
//   - content_block_start: 工具调用开始（提取 ID+Name，发出 ChunkToolCallStart）
//   - content_block_delta: 内容增量
//     - text_delta → ChunkText
//     - thinking_delta → ChunkReasoning（思维内容）
//     - signature_delta → ChunkReasoning（思维签名）
//     - input_json_delta → 累积工具调用参数
//   - content_block_stop: 工具调用完成（发出 ChunkToolCall）
//   - message_delta: 提取输出 token 和停止原因
//   - message_stop: 流完成
//   - error: 流错误
//
// 使用量在流结束后从 message_start + message_delta 组装并发出。
// 空闲看门狗与 OpenAI 提供者相同（120 秒超时）。
func (c *client) readStream(resp *http.Response, out chan<- provider.Chunk) {
	defer resp.Body.Close()
	defer close(out)

	// Close the body if the stream stalls past c.idleTimeout so scanner.Scan()
	// unblocks instead of hanging on a half-open connection. The watchdog owns the
	// timer; the read loop only pings the buffered activity channel (no Timer.Reset
	// race). A context cancel already unblocks the scan via the transport.
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

	tools := map[int]*provider.ToolCall{} // tool_use blocks, keyed by content index
	var inTok, outTok, cacheCreate, cacheRead int
	var stopReason string
	haveUsage := false

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		select { // ping the idle watchdog; non-blocking so a full buffer is fine
		case activity <- struct{}{}:
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		// SSE carries `event:` and `data:` lines; the data JSON's own `type` field
		// is authoritative, so we only need the data payloads.
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}

		var ev streamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			out <- provider.Chunk{Type: provider.ChunkError, Err: fmt.Errorf("%s: decode stream: %w", c.name, err)}
			return
		}

		switch ev.Type {
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				inTok = ev.Message.Usage.InputTokens
				cacheCreate = ev.Message.Usage.CacheCreationInputTokens
				cacheRead = ev.Message.Usage.CacheReadInputTokens
				haveUsage = true
			}
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
				tc := &provider.ToolCall{ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}
				tools[ev.Index] = tc
				out <- provider.Chunk{Type: provider.ChunkToolCallStart, ToolCall: &provider.ToolCall{ID: tc.ID, Name: tc.Name}}
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					out <- provider.Chunk{Type: provider.ChunkText, Text: ev.Delta.Text}
				}
			case "thinking_delta":
				if ev.Delta.Thinking != "" {
					out <- provider.Chunk{Type: provider.ChunkReasoning, Text: ev.Delta.Thinking}
				}
			case "signature_delta":
				if ev.Delta.Signature != "" {
					out <- provider.Chunk{Type: provider.ChunkReasoning, Signature: ev.Delta.Signature}
				}
			case "input_json_delta":
				if tc := tools[ev.Index]; tc != nil {
					tc.Arguments += ev.Delta.PartialJSON
				}
			}
		case "content_block_stop":
			if tc := tools[ev.Index]; tc != nil {
				out <- provider.Chunk{Type: provider.ChunkToolCall, ToolCall: tc}
				delete(tools, ev.Index)
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				outTok = ev.Usage.OutputTokens
				haveUsage = true
			}
		case "message_stop":
			// Stream complete; fall through to finalize below.
		case "error":
			msg := "stream error"
			if ev.Error != nil && ev.Error.Message != "" {
				msg = ev.Error.Message
			}
			out <- provider.Chunk{Type: provider.ChunkError, Err: fmt.Errorf("%s: %s", c.name, msg)}
			return
		}
	}

	if stalled.Load() {
		out <- provider.Chunk{Type: provider.ChunkError, Err: fmt.Errorf("%s: stream stalled — no data for %s, connection likely dropped", c.name, idleTimeout)}
		return
	}
	if err := scanner.Err(); err != nil {
		out <- provider.Chunk{Type: provider.ChunkError, Err: fmt.Errorf("%s: read stream: %w", c.name, err)}
		return
	}

	if haveUsage {
		out <- provider.Chunk{Type: provider.ChunkUsage, Usage: &provider.Usage{
			PromptTokens:     inTok + cacheCreate + cacheRead,
			CompletionTokens: outTok,
			TotalTokens:      inTok + cacheCreate + cacheRead + outTok,
			CacheHitTokens:   cacheRead,
			CacheMissTokens:  inTok + cacheCreate, // uncached input + cache writes (billed ≥1×)
			FinishReason:     mapStopReason(stopReason),
		}}
	}
	out <- provider.Chunk{Type: provider.ChunkDone}
}

// mapStopReason 将 Anthropic 的停止原因转换为 OpenAI 风格的 finish reason。
// agent 已识别这些标准值（如 "length" 表示异常终止）。
func mapStopReason(s string) string {
	switch s {
	case "end_turn", "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return s // "refusal", "pause_turn", "" — pass through
	}
}

// --- Messages API wire protocol 类型定义 ---
// 以下类型定义了 Anthropic Messages API 的请求和响应 JSON 结构。

// ephemeral 创建 ephemeral 缓存控制标记，用于 prompt 缓存断点。
func ephemeral() *cacheControl { return &cacheControl{Type: "ephemeral"} }

// cacheControl 是 Anthropic 的 prompt 缓存控制标记。
type cacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// anthRequest 是发送到 /v1/messages 的请求体。
type anthRequest struct {
	Model        string          `json:"model"`                    // 模型标识符
	MaxTokens    int             `json:"max_tokens"`               // 最大输出 token 数（必需）
	System       []textBlock     `json:"system,omitempty"`         // 系统提示词（顶层字段）
	Messages     []anthMessage   `json:"messages"`                 // 对话消息列表
	Tools        []anthTool      `json:"tools,omitempty"`          // 工具定义列表
	Thinking     *thinkingConfig `json:"thinking,omitempty"`       // 扩展思维配置
	OutputConfig *outputConfig   `json:"output_config,omitempty"`  // 输出配置（effort 级别）
	Stream       bool            `json:"stream"`                   // 是否流式输出
}

// thinkingConfig 控制 Anthropic 的扩展思维功能。
type thinkingConfig struct {
	Type    string `json:"type"`              // "adaptive"（自动决定是否使用思维）
	Display string `json:"display,omitempty"` // "summarized" 使推理文本可流式输出
}

// outputConfig 控制输出配置。
type outputConfig struct {
	Effort string `json:"effort,omitempty"` // 推理深度: low | medium | high | xhigh | max
}

// textBlock 是系统提示词中的文本块。
type textBlock struct {
	Type         string        `json:"type"`                      // "text"
	Text         string        `json:"text"`                      // 文本内容
	CacheControl *cacheControl `json:"cache_control,omitempty"`   // 缓存控制标记
}

// anthMessage 是请求中的单条消息。
type anthMessage struct {
	Role    string         `json:"role"`    // "user" 或 "assistant"
	Content []contentBlock `json:"content"` // 内容块列表
}

// contentBlock 是请求中的内容块联合体。
// 根据 Type 字段的不同，使用不同的字段组合：
//   - "text": 文本块（Text）
//   - "thinking": 思维链块（Thinking + Signature）
//   - "tool_use": 工具调用块（ID + Name + Input）
//   - "tool_result": 工具结果块（ToolUseID + Content）
//   - "image": 图片块（Source）
type contentBlock struct {
	Type         string          `json:"type"`                      // 块类型
	Text         string          `json:"text,omitempty"`            // text: 文本内容
	Thinking     string          `json:"thinking,omitempty"`        // thinking: 思维链内容
	Signature    string          `json:"signature,omitempty"`       // thinking: 思维链签名
	ID           string          `json:"id,omitempty"`              // tool_use: 调用 ID
	Name         string          `json:"name,omitempty"`            // tool_use: 工具名称
	Input        json.RawMessage `json:"input,omitempty"`           // tool_use: 参数 JSON
	ToolUseID    string          `json:"tool_use_id,omitempty"`     // tool_result: 关联的调用 ID
	Content      string          `json:"content,omitempty"`         // tool_result: 结果内容
	Source       *imageSource    `json:"source,omitempty"`          // image: 图片数据源
	CacheControl *cacheControl   `json:"cache_control,omitempty"`   // 缓存控制标记
}

// imageSource 是图片内容块的数据源（base64 编码）。
type imageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // MIME 类型（如 "image/png"）
	Data      string `json:"data"`       // base64 编码的图片数据
}

// anthTool 是请求中的工具定义。
type anthTool struct {
	Name         string          `json:"name"`                      // 工具名称
	Description  string          `json:"description,omitempty"`     // 工具描述
	InputSchema  json.RawMessage `json:"input_schema"`              // 参数的 JSON Schema
	CacheControl *cacheControl   `json:"cache_control,omitempty"`   // 缓存控制标记
}

// streamEvent 是 SSE 流中的鉴别联合事件。
// 根据 Type 字段读取对应的子结构：
//   - "message_start": 消息开始（含输入 token 统计）
//   - "content_block_start": 内容块开始（工具调用的 ID+Name）
//   - "content_block_delta": 内容块增量（文本/思维/签名/参数）
//   - "content_block_stop": 内容块结束（工具调用完成）
//   - "message_delta": 消息增量（输出 token + 停止原因）
//   - "message_stop": 消息结束
//   - "error": 流错误
type streamEvent struct {
	Type    string `json:"type"`  // 事件类型
	Index   int    `json:"index"` // 内容块索引（用于关联 tool_use 块）
	Message *struct {
		Usage *wireUsage `json:"usage"` // message_start: 输入 token 统计
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"` // content_block_start: 块类型（如 "tool_use"）
		ID   string `json:"id"`   // content_block_start: 工具调用 ID
		Name string `json:"name"` // content_block_start: 工具名称
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`         // content_block_delta: 增量类型
		Text        string `json:"text"`         // text_delta: 文本增量
		Thinking    string `json:"thinking"`     // thinking_delta: 思维链增量
		Signature   string `json:"signature"`    // signature_delta: 思维签名
		PartialJSON string `json:"partial_json"` // input_json_delta: 工具参数增量
		StopReason  string `json:"stop_reason"`  // message_delta: 停止原因
	} `json:"delta"`
	Usage *wireUsage `json:"usage"` // message_delta: 输出 token 统计
	Error *struct {
		Type    string `json:"type"`    // 错误类型
		Message string `json:"message"` // 错误消息
	} `json:"error"`
}

// wireUsage 是 Anthropic 的 token 使用量统计。
type wireUsage struct {
	InputTokens              int `json:"input_tokens"`               // 输入 token 数
	OutputTokens             int `json:"output_tokens"`              // 输出 token 数
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"` // 缓存创建的输入 token 数
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`    // 缓存读取的输入 token 数
}
