// Package provider 定义了模型后端的抽象层和自注册工厂。
//
// 核心设计:
//   - Provider 接口: 所有模型后端的统一入口，仅暴露 Stream 方法进行流式补全
//   - 工厂注册模式: 各具体实现（如 provider/openai、provider/anthropic）在 init()
//     中通过 Register("kind", NewFn) 注册自己，主程序通过 New("kind", cfg) 按名称实例化
//   - 消息规范化: NormalizeMessages 在发送前修复工具调用与结果的配对关系，
//     确保满足 OpenAI/Anthropic API 的协议约束
//
// 本包同时定义了消息(Message)、工具调用(ToolCall)、流式增量(Chunk)、
// 使用量(Usage)、定价(Pricing) 等传输无关的核心数据结构。
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"reasonix/internal/nilutil"
)

// Role 表示消息在对话中的角色。
type Role string

const (
	RoleSystem    Role = "system"    // 系统提示词，引导模型行为
	RoleUser      Role = "user"      // 用户输入
	RoleAssistant Role = "assistant" // 模型回复（可能包含文本、工具调用）
	RoleTool      Role = "tool"      // 工具执行结果
)

// Message 表示对话中的一条消息。
//
// 支持多模态（文本+图片）、思维链推理（thinking）、工具调用等能力。
// 消息在会话中以 Role 标识身份，在发送到具体提供者前会被 NormalizeMessages 修复。
type Message struct {
	Role             Role     `json:"role"`
	Content          string   `json:"content,omitempty"`
	Images           []string `json:"images,omitempty"`            // data URL 格式的图片（data:<mime>;base64,...），仅视觉模型使用
	ReasoningContent string   `json:"reasoning_content,omitempty"` // assistant 消息：思维链内容，多轮对话时需回传以满足 API 约束
	// ReasoningSignature 是提供者对思维链内容的签名证明，用于验证思维链的合法性。
	// Anthropic 要求当工具调用紧随思维链之后时，必须回传带签名的 thinking block；
	// 无签名推理的提供者（如 OpenAI 兼容层）留空此字段。
	// 与 ReasoningContent 配对在多轮对话中往返传递。
	ReasoningSignature string     `json:"reasoning_signature,omitempty"`
	ToolCalls          []ToolCall `json:"tool_calls,omitempty"`   // assistant 消息：模型请求的工具调用列表
	ToolCallID         string     `json:"tool_call_id,omitempty"` // tool 消息：关联到对应的工具调用 ID
	Name               string     `json:"name,omitempty"`         // tool 消息：工具名称
}

// ParseImageDataURL 将 data URL（格式为 data:<media-type>;base64,<payload>）拆分为
// MIME 类型和 base64 编码的图片数据。返回 ok=false 表示不是合法的 base64 data URL。
// 需要此拆分的提供者（如 Anthropic）会静默跳过非法 URL。
func ParseImageDataURL(dataURL string) (mediaType, base64Data string, ok bool) {
	rest, found := strings.CutPrefix(dataURL, "data:")
	if !found {
		return "", "", false
	}
	meta, payload, found := strings.Cut(rest, ",")
	if !found {
		return "", "", false
	}
	mt, found := strings.CutSuffix(meta, ";base64")
	if !found || mt == "" {
		return "", "", false
	}
	return mt, payload, true
}

// ToolCall 表示模型请求的一次工具调用。
// Arguments 是原始 JSON 字符串（未解析），Diff/Added/Removed 用于文件编辑类工具的变更统计。
type ToolCall struct {
	ID        string `json:"id"`                  // 工具调用的唯一标识，用于关联 tool 消息的 ToolCallID
	Name      string `json:"name"`                // 工具名称（如 "bash"、"write_file"）
	Arguments string `json:"arguments"`           // 工具参数的原始 JSON 字符串
	Diff      string `json:"diff,omitempty"`      // 文件编辑工具的 diff 输出
	Added     int    `json:"added,omitempty"`     // 文件编辑新增行数
	Removed   int    `json:"removed,omitempty"`   // 文件编辑删除行数
}

// ToolSchema 是暴露给模型的工具定义。Parameters 使用 JSON Schema 格式描述参数结构。
type ToolSchema struct {
	Name        string          `json:"name"`        // 工具名称
	Description string          `json:"description"` // 工具功能描述，供模型理解何时调用
	Parameters  json.RawMessage `json:"parameters"`  // 参数的 JSON Schema 定义
}

// Request 是一次补全请求，包含对话历史、可用工具、采样参数等。
// 各提供者在 Stream 方法中将其转换为各自的 wire format。
type Request struct {
	Messages    []Message     // 对话历史（已由调用方构造，发送前会经 NormalizeMessages 修复）
	Tools       []ToolSchema  // 暴露给模型的工具定义列表
	Temperature float64       // 采样温度（0-2），部分模型不支持
	MaxTokens   int           // 最大输出 token 数，0 表示使用提供者默认值
}

// interruptedToolResult 是为未完成的工具调用生成的占位符结果。
// 当一个携带 tool_calls 的 assistant 消息因中断（如崩溃、用户取消）而未收到对应结果时，
// 发送未配对的 tool_calls 会导致 OpenAI/DeepSeek 返回 400 错误。
// 此占位符确保每条 tool_calls 都有对应的 tool 消息，满足 API 协议约束。
const interruptedToolResult = "[no result: the previous turn was interrupted before this tool call completed]"

// SanitizeToolPairing 是 NormalizeMessages 的提供者端别名。
// 在发送到网络前修复对话历史，使其满足 OpenAI 兼容和 Anthropic API 的工具调用协议约束：
//   - 每条 assistant tool_calls 都有对应的 tool 结果消息
//   - 无孤立的 tool 消息
//   - 截断的参数 JSON 被修复为合法格式
//
// 保持独立命名以便调用点语义清晰："防御性 wire 准备" 而非 "会话修改"。
func SanitizeToolPairing(msgs []Message) []Message { return NormalizeMessages(msgs) }

// NormalizeMessages 修复对话历史使其满足 OpenAI 兼容和 Anthropic API 的工具调用协议。
//
// 修复内容包括:
//   - 为未回答的 tool_calls 补充占位符结果（保持回合完整）
//   - 删除孤立的 tool 消息（无对应 tool_calls 的结果）
//   - 从 tool 结果反向填充空的 tool-call 名称（#4727 兼容旧会话）
//   - 修复截断的工具调用参数 JSON（DeepSeek 会因半流式参数返回 400，#3953）
//
// 这是提供者请求的 wire-safe 入口。存储的会话加载使用 NormalizeSessionMessages，
// 共享 assistant 回合修复逻辑但不删除需要通过 reasonix --resume 往返的独立 tool 消息。
//
// 对于结构良好的历史（无未回答调用、无孤立结果、无空工具名、无截断参数），
// 返回原始切片（零分配），保持 prefix-cache 键稳定。
func NormalizeMessages(msgs []Message) []Message {
	return normalizeMessages(msgs, true)
}

// NormalizeSessionMessages 仅应用对持久化会话安全的修复。
// 与 NormalizeMessages 共享 assistant 回合修复逻辑，但保留现有 tool 消息而非删除，
// 确保 Save/LoadSession 对已在磁盘上的历史保持字节级往返一致性。
func NormalizeSessionMessages(msgs []Message) []Message {
	return normalizeMessages(msgs, false)
}

func normalizeMessages(msgs []Message, dropOrphanTools bool) []Message {
	if normalized, ok := tryNormalizeFastPath(msgs, dropOrphanTools); ok {
		return normalized // well-formed: pass through without allocating
	}
	out := make([]Message, 0, len(msgs))
	for i := 0; i < len(msgs); {
		m := msgs[i]
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			j := i + 1
			for j < len(msgs) && msgs[j].Role == RoleTool {
				j++
			}
			// Backfill empty tool-call names from the corresponding tool
			// results so the model sees which tool was invoked (#4727).
			// The wire-format fix (openai.go) ensures empty fields are
			// never omitted, so this backfill is a UX improvement, not a
			// correctness requirement.
			calls := backfillToolCallNames(m.ToolCalls, msgs[i+1:j])
			m.ToolCalls = calls
			out = append(out, repairToolCallArgs(m))
			if dropOrphanTools {
				out = append(out, pairToolResults(calls, msgs[i+1:j])...)
			} else {
				out = append(out, sessionToolResults(calls, msgs[i+1:j])...)
			}
			i = j
			continue
		}
		if m.Role == RoleTool {
			if !dropOrphanTools {
				out = append(out, m)
			}
			// Orphan tool message: provider sends drop it; session loads preserve it.
			i++
			continue
		}
		out = append(out, m)
		i++
	}
	return out
}

// tryNormalizeFastPath 检查消息历史是否无需修复。
// 如果所有 tool_calls/tool_result 回合都结构良好，直接返回原始切片（零分配）；
// 结构不良的回合触发慢速路径进行修复。
func tryNormalizeFastPath(msgs []Message, dropOrphanTools bool) ([]Message, bool) {
	for i := 0; i < len(msgs); {
		m := msgs[i]
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			j := i + 1
			for j < len(msgs) && msgs[j].Role == RoleTool {
				j++
			}
			if !toolTurnWellFormed(m.ToolCalls, msgs[i+1:j]) || needsToolCallArgRepair(m.ToolCalls) {
				return nil, false
			}
			i = j
			continue
		}
		if m.Role == RoleTool && dropOrphanTools {
			return nil, false
		}
		i++
	}
	return msgs, true
}

func toolTurnWellFormed(calls []ToolCall, results []Message) bool {
	if len(calls) != len(results) {
		return false
	}
	for _, tc := range calls {
		if tc.Name == "" {
			return false
		}
	}
	for k, tc := range calls {
		if results[k].ToolCallID != tc.ID {
			return false
		}
	}
	return true
}

func needsToolCallArgRepair(calls []ToolCall) bool {
	for _, tc := range calls {
		if tc.Arguments != "" && !json.Valid([]byte(tc.Arguments)) {
			return true
		}
	}
	return false
}

// repairToolCallArgs 修复消息中无法解码的工具调用参数为合法 JSON。
// 采用写时复制策略，不修改原始历史。空参数原样保留（某些网关对无参工具发送 ""）。
func repairToolCallArgs(m Message) Message {
	broken := false
	for _, tc := range m.ToolCalls {
		if tc.Arguments != "" && !json.Valid([]byte(tc.Arguments)) {
			broken = true
			break
		}
	}
	if !broken {
		return m
	}
	calls := make([]ToolCall, len(m.ToolCalls))
	copy(calls, m.ToolCalls)
	for i := range calls {
		if calls[i].Arguments == "" || json.Valid([]byte(calls[i].Arguments)) {
			continue
		}
		calls[i].Arguments = closeTruncatedJSON(calls[i].Arguments)
	}
	m.ToolCalls = calls
	return m
}

// closeTruncatedJSON 尽力补全被截断的 JSON 文档。
// 处理场景：未闭合的字符串、未关闭的括号、悬挂的逗号/冒号。
// 补全后仍无效的降级为 "{}"。
//
// 典型场景：DeepSeek 在流式传输工具调用参数时可能因连接中断而截断，
// 回放这些半截参数会导致 API 400 错误。
func closeTruncatedJSON(s string) string {
	var stack []byte
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	out := s
	if esc {
		out = out[:len(out)-1]
	}
	if inStr {
		out += `"`
	}
	trimmed := strings.TrimRight(out, " \t\r\n")
	switch {
	case strings.HasSuffix(trimmed, ","):
		out = trimmed[:len(trimmed)-1]
	case strings.HasSuffix(trimmed, ":"):
		out = trimmed + "null"
	}
	for i := len(stack) - 1; i >= 0; i-- {
		out += string(stack[i])
	}
	if !json.Valid([]byte(out)) {
		return "{}"
	}
	return out
}

// pairToolResults 为每个 tool_call 配对对应的结果，未回答的调用填充占位符。
//
// 配对策略：
//   - ID 唯一且非空时：按 ID 配对（结果可重排以匹配调用顺序）
//   - ID 为空或重复时：按位置配对（某些网关按 index 流式传输工具调用，无 ID）
//
// 调用顺序始终被保留（循环按调用顺序追加结果）。
func pairToolResults(calls []ToolCall, avail []Message) []Message {
	out := make([]Message, 0, len(calls))
	if idDistinct(calls) {
		byID := make(map[string]Message, len(avail))
		for _, r := range avail {
			byID[r.ToolCallID] = r
		}
		for _, tc := range calls {
			if r, ok := byID[tc.ID]; ok {
				out = append(out, r)
			} else {
				out = append(out, Message{Role: RoleTool, ToolCallID: tc.ID, Name: tc.Name, Content: interruptedToolResult})
			}
		}
		return out
	}
	for k, tc := range calls {
		if k < len(avail) {
			r := avail[k]
			r.ToolCallID = tc.ID
			out = append(out, r)
		} else {
			out = append(out, Message{Role: RoleTool, ToolCallID: tc.ID, Name: tc.Name, Content: interruptedToolResult})
		}
	}
	return out
}

// sessionToolResults 保留所有已存储的 tool 结果，仅为无记录结果的调用追加占位符。
// 加载时的规范化不能丢弃或重排用户历史；提供者发送时仍可使用 pairToolResults 进行严格的 wire 格式化。
func sessionToolResults(calls []ToolCall, avail []Message) []Message {
	out := append([]Message(nil), avail...)
	if idDistinct(calls) {
		answered := make(map[string]struct{}, len(avail))
		for _, r := range avail {
			answered[r.ToolCallID] = struct{}{}
		}
		for _, tc := range calls {
			if _, ok := answered[tc.ID]; !ok {
				out = append(out, Message{Role: RoleTool, ToolCallID: tc.ID, Name: tc.Name, Content: interruptedToolResult})
			}
		}
		return out
	}
	for k := len(avail); k < len(calls); k++ {
		tc := calls[k]
		out = append(out, Message{Role: RoleTool, ToolCallID: tc.ID, Name: tc.Name, Content: interruptedToolResult})
	}
	return out
}

// backfillToolCallNames 从匹配的 tool 结果中反向填充空的工具调用名称。
// 旧会话（#4727）可能保存了空名称的 assistant tool_calls；反向填充为模型重放提供有用上下文。
//
// 配对策略：先按 ID 匹配，再按位置匹配。
// 常见情况（无空名称）直接返回输入，零分配。未配对的调用保留空名称，由 wire 格式修复（openai.go）处理。
func backfillToolCallNames(calls []ToolCall, results []Message) []ToolCall {
	missing := false
	for _, c := range calls {
		if c.Name == "" {
			missing = true
			break
		}
	}
	if !missing {
		return calls
	}
	out := make([]ToolCall, len(calls))
	copy(out, calls)
	if idDistinct(calls) {
		byID := make(map[string]string, len(results))
		for _, r := range results {
			if r.Name != "" {
				byID[r.ToolCallID] = r.Name
			}
		}
		for k := range out {
			if out[k].Name == "" {
				if n, ok := byID[out[k].ID]; ok {
					out[k].Name = n
				}
			}
		}
		return out
	}
	// Fallback: positional pairing (same order as pairToolResults).
	for k := range out {
		if out[k].Name == "" && k < len(results) {
			out[k].Name = results[k].Name
		}
	}
	return out
}

// idDistinct 检查调用批次中的每个 tool_call 是否都有非空且唯一的 ID。
// 只有满足此条件时，按 ID 配对才是安全的。
func idDistinct(calls []ToolCall) bool {
	seen := make(map[string]struct{}, len(calls))
	for _, tc := range calls {
		if tc.ID == "" {
			return false
		}
		if _, dup := seen[tc.ID]; dup {
			return false
		}
		seen[tc.ID] = struct{}{}
	}
	return true
}

// ChunkType 标识流式增量的类型。
// Stream 方法通过 channel 发送 Chunk，调用方根据 Type 字段读取对应的数据。
type ChunkType int

const (
	ChunkText          ChunkType = iota // 可见文本增量
	ChunkReasoning                      // 思维链增量（在可见回答之前的推理过程）
	ChunkToolCallStart                  // 工具调用开始（含 ID+Name，参数仍在流式传输中）
	ChunkToolCall                       // 完整的工具调用（参数已全部接收）
	ChunkUsage                          // 本次补全的 token 使用量
	ChunkDone                           // 补全正常结束
	ChunkError                          // 发生错误
)

// Usage 报告一次补全的 token 使用量。
//
// 缓存命中/未命中数据来自两种格式之一（openai 提供者统一归一化）：
//   - DeepSeek: 顶层 prompt_cache_hit_tokens / prompt_cache_miss_tokens
//   - OpenAI/MiMo: 嵌套的 prompt_tokens_details.cached_tokens
//
// ReasoningTokens 是思维模式模型报告的推理 token 子集（属于 CompletionTokens）。
// FinishReason 携带模型最后报告的终止原因，用于 agent 展示异常终止（如 "length"、"content_filter"）。
type Usage struct {
	PromptTokens     int    // 输入 token 总数
	CompletionTokens int    // 输出 token 总数
	TotalTokens      int    // 输入+输出 token 总数
	CacheHitTokens   int    // 命中缓存的输入 token 数
	CacheMissTokens  int    // 未命中缓存的输入 token 数
	ReasoningTokens  int    // 推理 token 数（CompletionTokens 的子集）
	FinishReason     string // 终止原因: "stop"、"tool_calls"、"length"、"content_filter"、"repetition_truncation" 等
}

// Pricing 是提供者的每百万 token 价格，用于估算费用。
// Currency 是显示符号或 ISO 代码（默认 "¥"）。toml 标签支持从配置文件解码。
type Pricing struct {
	CacheHit float64 `toml:"cache_hit"` // 每百万缓存命中 token 的价格
	Input    float64 `toml:"input"`     // 每百万未缓存输入 token 的价格
	Output   float64 `toml:"output"`    // 每百万输出 token 的价格
	Currency string  `toml:"currency"`  // 货币符号或代码（如 "¥"、"USD"）
}

// Cost 根据使用量估算本次补全的费用。
// 当缓存命中/未命中数据不完整时，从 PromptTokens 推导。
func (p *Pricing) Cost(u *Usage) float64 {
	if p == nil || u == nil {
		return 0
	}
	hit := u.CacheHitTokens
	miss := u.CacheMissTokens
	if hit+miss == 0 && u.PromptTokens > 0 {
		miss = u.PromptTokens
	} else if miss == 0 && hit > 0 && u.PromptTokens > hit {
		miss = u.PromptTokens - hit
	}
	return (float64(hit)*p.CacheHit +
		float64(miss)*p.Input +
		float64(u.CompletionTokens)*p.Output) / 1e6
}

// Symbol returns the currency display symbol, defaulting to "¥".
func (p *Pricing) Symbol() string {
	if p == nil || p.Currency == "" {
		return "¥"
	}
	return currencySymbol(p.Currency)
}

func currencySymbol(currency string) string {
	value := strings.TrimSpace(currency)
	if value == "" {
		return "¥"
	}
	switch strings.ToLower(value) {
	case "cny", "rmb", "yuan", "renminbi", "cnh":
		return "¥"
	case "usd", "dollar", "dollars", "us dollar", "us dollars", "us$":
		return "$"
	case "eur", "euro", "euros":
		return "€"
	case "gbp", "pound", "pounds", "sterling":
		return "£"
	case "jpy", "yen":
		return "¥"
	}
	switch value {
	case "￥", "¥":
		return "¥"
	case "$", "€", "£":
		return value
	}
	// any embedded currency sign → keep as-is (compact symbols like A$, HK$).
	for _, r := range value {
		if unicode.Is(unicode.Sc, r) {
			return value
		}
	}
	if isThreeLetterCurrencyCode(value) {
		return strings.ToUpper(value) + " "
	}
	return "¥"
}

func isThreeLetterCurrencyCode(value string) bool {
	if len(value) != 3 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

// Chunk 是一个流式事件。根据 Type 字段读取对应的数据字段。
type Chunk struct {
	Type      ChunkType   // 事件类型
	Text      string      // ChunkText / ChunkReasoning: 文本或推理内容增量
	Signature string      // ChunkReasoning: 推理签名（Anthropic thinking signature），用于回传验证
	ToolCall  *ToolCall   // ChunkToolCallStart (仅 ID+Name) / ChunkToolCall (完整调用)
	Usage     *Usage      // ChunkUsage: token 使用量
	Err       error       // ChunkError: 错误信息
}

// StreamInterruptedError 标记在调用方已接收到模型输出后发生的可恢复传输中断。
// 提供者不能自行重放此类请求（会导致文本或工具调用重复），
// agent 应通过追加尾部恢复提示来处理。
type StreamInterruptedError struct {
	Err error // 底层错误（通常是连接重置）
}

func (e *StreamInterruptedError) Error() string {
	if e == nil || e.Err == nil {
		return "stream interrupted"
	}
	return e.Err.Error()
}

func (e *StreamInterruptedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func IsStreamInterrupted(err error) bool {
	var interrupted *StreamInterruptedError
	return errors.As(err, &interrupted)
}

// Provider 是所有聊天模型后端的核心接口。
//
// 实现者在 init() 中通过 provider.Register("kind", NewFn) 注册自己，
// 主程序通过 provider.New("kind", cfg) 按名称实例化。
type Provider interface {
	// Name 返回提供者实例名称（如 "deepseek"、"mimo"、"anthropic"）。
	Name() string
	// Stream 启动流式补全，将增量事件推送到返回的 channel。
	// 取消 ctx 必须中止底层请求；channel 关闭表示补全结束。
	// 返回的 channel 可能包含: 文本增量、推理增量、工具调用、使用量、完成信号或错误。
	Stream(ctx context.Context, req Request) (<-chan Chunk, error)
}

// Config 是已解析的提供者实例配置。
// 由配置系统从用户配置文件加载并解析环境变量后传入工厂函数。
type Config struct {
	Name    string         // 实例名称（如 "deepseek"、"mimo"）
	BaseURL string         // API 端点 URL（OpenAI 兼容或 Anthropic 原生）
	Model   string         // 模型标识符（如 "deepseek-reasoner"、"claude-opus-4-0520"）
	APIKey  string         // 从 api_key_env 环境变量解析的 API 密钥
	Extra   map[string]any // 提供者类型特有的选项（如 reasoning_protocol、vision、effort 等）
}

// AuthError 报告提供者拒绝了 API 密钥（HTTP 401/403）。
// 错误信息已面向用户且可操作——包含提供者名称和密钥来源的环境变量名，
// CLI 可直接展示而无需转储原始状态体。
type AuthError struct {
	Provider  string // 提供者实例名称（如 "deepseek"）
	KeyEnv    string // 密钥来源的环境变量名（如 "DEEPSEEK_API_KEY"）
	KeySource string // KeyEnv 的人类可读来源描述
	Status    int    // HTTP 状态码（401 或 403）
	HasKey    bool   // 是否发送了非空密钥（true=服务器拒绝，false=未配置密钥）
}

func (e *AuthError) Error() string {
	key := "the API key"
	if e.KeyEnv != "" {
		key = e.KeyEnv
	}
	if e.KeySource != "" {
		key += " from " + e.KeySource
	}
	return fmt.Sprintf("authentication failed for provider %q (HTTP %d): %s is invalid or expired — update it (in .env or your environment) and retry, or run `reasonix setup`",
		e.Provider, e.Status, key)
}

// Factory 是从已解析的 Config 构建 Provider 的工厂函数。
type Factory func(cfg Config) (Provider, error)

// registry 存储所有已注册的提供者工厂，key 为提供者类型名称。
var registry = map[string]Factory{}

// Register 注册一个提供者工厂（如 "openai"、"anthropic"）。
// 专为 init() 函数使用。重复注册同一类型会 panic，因为这是编译时的接线错误。
func Register(kind string, f Factory) {
	if _, dup := registry[kind]; dup {
		panic("provider: duplicate kind " + kind)
	}
	registry[kind] = f
}

// New 按类型名称实例化提供者。
// 从注册表中查找对应工厂并调用，返回错误表示类型未知或工厂构建失败。
func New(kind string, cfg Config) (Provider, error) {
	f, ok := registry[kind]
	if !ok {
		return nil, fmt.Errorf("provider: unknown kind %q (registered: %v)", kind, Kinds())
	}
	p, err := f(cfg)
	if err != nil {
		return nil, err
	}
	if nilutil.IsNil(p) {
		return nil, fmt.Errorf("provider: factory %q returned nil provider", kind)
	}
	return p, nil
}

// Kinds 返回所有已注册的提供者类型名称（已排序）。
// 用于错误提示中列出可用的类型。
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
