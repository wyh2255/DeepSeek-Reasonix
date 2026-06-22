// Package event 定义了代理（Agent）在执行一个回合（turn）时发出的类型化事件流，
// 以及事件流的目标接收器（Sink）。
//
// 本包的核心设计思想是将"发生了什么"（模型产生了推理、分派了一个工具调用、
// 一个回合消耗了 N 个 token）与"如何展示它"（终端中的 ANSI 回滚、Webview 中的卡片）
// 解耦开来。
//
// 架构说明：
//   - 代理（Agent）仅依赖 Sink 接口；每个前端实现自己的 Sink。
//   - 聊天 TUI 将事件渲染到终端回滚缓冲区；
//   - 无头运行（headless run）将事件渲染为纯 ANSI 输出到 stdout；
//   - 未来的 GUI/serve 传输层将事件转发到 Webview 或 WebSocket。
//
// 这种设计取代了旧的 io.Writer 契约：旧方案中代理直接写入预格式化的 ANSI，
// 消费者必须通过匹配行前缀来重新推导结构——这种方式脆弱且有损，
// 对于比终端更丰富的前端来说是不可接受的。
package event

import (
	"reasonix/internal/evidence"
	"reasonix/internal/nilutil"
	"reasonix/internal/provider"
)

// Kind 标识事件的类型。每种类型对应 Event 结构体中不同的有效字段，
// 详见各常量的文档说明。
type Kind int

const (
	// TurnStarted 标记一个顶层 Run（一个用户回合）的开始。
	// 接收器（Sink）应在此事件时重置所有回合级渲染状态。
	// 此事件不携带任何有效载荷。
	TurnStarted Kind = iota

	// Reasoning 是思维模式下的推理增量（Text 字段有效）。
	// 在可见答案之前流式传输；接收器通常将其以静音方式渲染在"思考"标题下，
	// 展示模型的思维链（chain-of-thought）过程。
	Reasoning

	// Text 是回答文本的增量（Text 字段有效）。
	// 每个事件携带一小段文本片段，接收器实时追加显示。
	Text

	// Message 标记助手回合的文本已完成：
	// Text 字段持有完整的回答文本，Reasoning 字段持有完整的推理链
	// （两者已通过上述增量事件流式传输完毕）。
	// 接收器可利用此事件将流式传输的原始文本重新渲染为带样式的 Markdown；
	// 简单的接收器可忽略此事件。
	Message

	// ToolDispatch 通知一个工具调用即将运行。
	// Tool 字段中 ID/Name/Args/ReadOnly 有效。
	// 在 executeBatch 中，每个调用都会预先发出此事件，以便前端按时间线展示。
	ToolDispatch

	// ToolResult 报告一个工具调用已完成。
	// Tool 字段中 Output/Err/Truncated 已填充。
	ToolResult

	// Usage 携带每回合的 token 遥测数据（Usage 字段有效）。
	// Pricing 字段可选，用于计算和显示成本。
	Usage

	// Notice 是带外消息——警告、截断通知、阻止通知或压缩通知。
	// Level + Text 字段有效。
	Notice

	// Phase 标记协调器的边界，例如从规划器（planner）到执行器（executor）的交接。
	// Text 字段为标签，如 "deepseek · planning"。
	Phase

	// ApprovalRequest 请求前端批准一个待处理的工具调用。
	// Approval 字段中 ID/Tool/Subject 有效。
	// 运行会阻塞，直到控制器的 Approve(ID, ...) 解决此请求；
	// 前端显示提示并等待用户回答。
	ApprovalRequest

	// AskRequest 请求前端向用户提出一个或多个结构化的多选问题。
	// Ask 字段中 ID + Questions 有效。
	// 运行会阻塞，直到控制器的 AnswerQuestion(ID, ...) 解决此请求。
	// 此事件驱动 `ask` 工具的交互功能。
	AskRequest

	// TurnDone 标记一个顶层 Run 的结束。
	// Err 字段非 nil 表示失败；nil 也可能表示用户取消（这不是错误）。
	// 此事件始终是一个回合的最后一个事件。
	TurnDone

	// CompactionStarted 标记一次上下文压缩（context-compaction）过程的开始。
	// Compaction 字段中 Trigger 有效。
	// 前端在摘要器运行期间显示"正在压缩..."占位符；
	// CompactionDone 事件将替换此占位符。
	// 与 ToolDispatch/ToolResult 的配对模式类似。
	CompactionStarted

	// CompactionDone 报告一次压缩过程已完成。
	// Compaction 字段中 Trigger/Messages/Summary/Archive 已填充。
	// 中止的压缩过程会发出此事件但 Summary 为空，以确保占位符仍能正常解析。
	// 取代了旧的纯 Notice 事件，使接收器能够渲染一个独立的、可展开的卡片。
	CompactionDone

	// ToolProgress 流式传输仍在运行的工具的组合输出片段。
	// Tool 字段中 ID + Output（新的片段）有效。
	// 在 ToolDispatch 和 ToolResult 之间发出，用于长时间运行的工具（如 bash），
	// 以便前端可以显示实时进度。
	// 放在最后以保持其之前的 Kind 值在线上格式的稳定性。
	ToolProgress

	// MCPSurfaceReady 在每个服务器的后台加载表面（prompts 或 resources）
	// 启动完成后触发一次。让 UI 可以刷新 /mcp 状态而无需轮询。
	// Text 字段携带 "<server>: <surface> ready (<count> items)" 格式的信息。
	// 放在最后以保持其之前的 Kind 值在线上格式的稳定性。
	MCPSurfaceReady

	// Retrying 在提供者（Provider）因瞬态失败而重新尝试连接+请求头阶段时，
	// 在每次退避休眠之前触发。
	// RetryAttempt 和 RetryMax 字段有效（1-based 的当前尝试次数和总尝试次数）。
	// 前端显示一个临时的"重试中 (n/m)"指示器，
	// 下一个流事件——或 TurnDone——会清除该指示器。
	// 放在最后以保持其之前的 Kind 值在线上格式的稳定性。
	Retrying

	// Steer 在回合中途的引导消息从队列中被消费并注入为用户消息时触发。
	// Text 字段携带原始的引导内容（不含包装前缀），
	// 前端可将其显示给用户作为确认。
	// 前端使用 Steer 事件来知晓已排队的消息已被送达。
	Steer
)

// Level 对 Notice 事件进行分类，以便接收器可以设置不同的样式或进行过滤。
type Level int

const (
	// LevelInfo 表示信息性通知，通常以默认样式显示。
	LevelInfo Level = iota
	// LevelWarn 表示警告通知，通常以醒目的样式显示以引起用户注意。
	LevelWarn
)

// Profile 携带为此次调用解析的子代理（subagent）模型和努力程度信息。
// 用于 task/skill 工具调用，让前端显示子代理使用的具体模型配置。
type Profile struct {
	Model  string // 子代理使用的模型名称
	Effort string // 努力程度级别（如 "low", "medium", "high"）
}

// Tool 描述一个工具调用，用于 ToolDispatch 和 ToolResult 事件。
//
// 在 ToolDispatch（分派）阶段，只有 ID/Name/Args/ReadOnly 被设置；
// 在 ToolResult（结果）阶段，Output/Err/Truncated 被填充。
// Args 是原始的 JSON 参数——接收器会压缩它以便显示。
type Tool struct {
	ID         string // 工具调用的唯一标识符，用于关联 Dispatch 和 Result
	Name       string // 工具名称（如 "bash", "read_file", "write_file" 等）
	Args       string // 原始 JSON 参数字符串
	Output     string // ToolResult: 喂给模型的结果文本
	Err        string // ToolResult: 调用失败或被阻止时非空
	ReadOnly   bool   // 工具是否为只读类型
	Truncated  bool   // ToolResult: 输出在显示/发送给模型之前是否被截断（head+tail）
	DurationMs int64  // ToolResult: 挂钟执行时间（毫秒）

	// Partial 标记一个早期的 ToolDispatch，当调用开始时发出（ID/Name 已设置，
	// Args 仍在流式传输中），以便前端可以立即显示工具卡片。
	// 当调用完成时，会发出第二个完整的 ToolDispatch（Partial=false，Args 已设置）。
	// 前端通过 ID 合并这两个事件。
	Partial bool

	// ParentID 在被设置时，表示产生此调用的父工具调用的 ID。
	// 子代理的调用携带父 `task` 调用的 ID，以便前端可以将它们嵌套显示。
	// 顶级调用此字段为空。
	ParentID string

	FileDiff          // 预览的文件变更差异（写入工具的有效字段）
	Profile *Profile  // ToolDispatch: 子代理的模型/努力程度（task/skill 调用时设置）
}

// FileDiff 是写入工具的完整 ToolDispatch 和 ApprovalRequest 上携带的预览变更。
// 前端可以在工具调用运行之前渲染 +/- 行的差异视图。
type FileDiff struct {
	Diff    string // 统一差异格式（unified diff）的字符串。只读工具、二进制文件或无变更时为空。
	Added   int    // 新增的行数
	Removed int    // 删除的行数
}

// Approval 标识一个待处理的工具调用批准请求，用于 ApprovalRequest 事件。
// ID 将请求与控制器的 Approve(ID, ...) 回复关联起来。
type Approval struct {
	ID      string // 唯一标识符，用于关联请求和回复
	Tool    string // 需要批准的工具名称
	Subject string // 批准的主题描述（如文件路径、命令内容等）
}

// AskOption 是用户在 AskQuestion 中可以选择的一个选项。
type AskOption struct {
	Label       string // 选项的显示标签
	Description string // 可选的单行说明，显示在标签下方
}

// AskQuestion 是 `ask` 工具向用户提出的一个结构化问题。
type AskQuestion struct {
	ID      string      // 每个问题的稳定标识符，用于将答案关联回问题
	Header  string      // 短标签（用作标签页标题）
	Prompt  string      // 问题的完整文本
	Options []AskOption // 可供选择的选项列表
	Multi   bool        // 是否允许选择多个选项
}

// Ask 携带一个 AskRequest：一批问题和用于关联控制器 AnswerQuestion(ID, ...) 回复的 ID。
type Ask struct {
	ID        string        // 唯一标识符，用于关联请求和回复
	Questions []AskQuestion // 向用户提出的问题列表
}

// Compaction 携带一次上下文压缩过程的信息，用于 CompactionStarted 和 CompactionDone 事件。
//
// 在 CompactionStarted 时只有 Trigger 被设置；
// 在 CompactionDone 时，Messages/Summary/Archive 被填充
// （中止的压缩过程 Summary 为空）。
type Compaction struct {
	Trigger  string // 触发原因："auto"（提示达到窗口阈值）或 "manual"（用户运行 /compact）
	Messages int    // 压缩完成: 被折叠进摘要的消息数量
	Summary  string // 压缩完成: 代理继续依赖的摘要内容
	Archive  string // 压缩完成: 被丢弃的原始消息的归档路径（无归档时为空）
}

// AskAnswer 是用户对一个 AskQuestion 的回复：选定的选项标签。
// 自由输入的答案作为单个 Selected 条目携带。
type AskAnswer struct {
	QuestionID string   // 对应的 AskQuestion 的 ID
	Selected   []string // 用户选定的选项标签列表
}

// CacheDiagnostics 描述可缓存前缀（cacheable prefix）自上一回合以来是否以及为何发生了变化。
// 它搭载在 Usage 事件上，以便每个前端都可以展示缓存流失的归因信息。
type CacheDiagnostics struct {
	PrefixHash          string   // 当前可缓存前缀的哈希值
	PrefixChanged       bool     // 前缀是否发生了变化
	PrefixChangeReasons []string // 变化原因列表："system"（系统提示）、"tools"（工具列表）、"log_rewrite"（日志重写）
	SystemHash          string   // 系统提示的哈希值
	ToolsHash           string   // 工具列表的哈希值
	LogRewriteVersion   int      // 日志重写版本号
	ToolSchemaTokens    int      // 工具模式（schema）消耗的 token 数量
	CacheMissTokens     int      // 缓存未命中的 token 数量
	CacheHitTokens      int      // 缓存命中的 token 数量
}

// UsageSource 常量定义了 token 使用量的来源类型，
// 用于区分不同组件产生的 API 调用成本。
const (
	UsageSourceExecutor   = "executor"   // 执行器（主代理循环）产生的使用量
	UsageSourcePlanner    = "planner"    // 规划器产生的使用量
	UsageSourceSubagent   = "subagent"   // 子代理产生的使用量
	UsageSourceCompaction = "compaction" // 上下文压缩过程产生的使用量
	UsageSourceClassifier = "classifier" // 分类器产生的使用量
	UsageSourceTitle      = "title"      // 标题生成产生的使用量
)

// Event 是回合事件流中的一个增量项。根据 Kind 的值，读取对应的字段；其他字段为零值。
// 这是事件流系统的核心数据结构，代理通过 Sink 接口逐个发出这些事件。
type Event struct {
	Kind Kind   // 事件类型，决定哪些其他字段有效
	Text string // Reasoning / Text / Message / Notice / Phase 事件的文本内容

	Reasoning string // Message 事件: 完整的推理链文本

	Tool Tool // ToolDispatch / ToolResult 事件: 工具调用的详细信息

	Usage            *provider.Usage   // Usage 事件: token 使用量数据
	Pricing          *provider.Pricing // Usage 事件: 用于成本显示（nil 表示不显示成本）
	UsageSource      string            // Usage 事件: 计费调用来源；空值兼容地表示 executor
	CacheDiagnostics *CacheDiagnostics // Usage 事件: 缓存流失归因（nil 表示不适用）

	// SessionHit/SessionMiss 携带整个会话的累积缓存 token 数（仅 Usage 事件），
	// 以便前端可以显示聚合命中率——该指标不会因短回合或压缩而骤降——
	// 与 Usage 的单回合数字并列显示。
	SessionHit  int // Usage 事件: 本会话累积的缓存命中提示 token 数
	SessionMiss int // Usage 事件: 本会话累积的缓存未命中提示 token 数

	Level      Level      // Notice 事件: 通知级别（Info 或 Warn）
	Approval   Approval   // ApprovalRequest 事件: 待批准的工具调用信息
	Ask        Ask        // AskRequest 事件: 向用户提出的问题
	Err        error      // TurnDone 事件: 非 nil 表示失败
	Compaction Compaction // CompactionStarted/CompactionDone 事件: 压缩信息

	RetryAttempt int // Retrying 事件: 即将进行的尝试次数（1-based）
	RetryMax     int // Retrying 事件: 放弃前的总尝试次数
}

// ReadinessAuditSink 是一个可选的接收器能力接口。
// 不关心就绪性审计回执的接收器只需实现 Sink 接口，会自动忽略审计事件。
type ReadinessAuditSink interface {
	RecordReadinessAudit(evidence.ReadinessAudit)
}

// RecordReadinessAudit 将就绪性审计回执转发给选择接收的 Sink。
// 如果 Sink 为 nil 或未实现 ReadinessAuditSink 接口，则静默跳过。
func RecordReadinessAudit(s Sink, a evidence.ReadinessAudit) {
	if nilutil.IsNil(s) {
		return
	}
	if rs, ok := s.(ReadinessAuditSink); ok {
		rs.RecordReadinessAudit(a)
	}
}

// Sink 是事件流的消费接口。代理从其运行循环中串行调用 Emit
// （工具执行可能在多个 goroutine 中扇出，但事件发射不会），
// 因此实现无需保证 Emit 的并发安全性。
//
// 重要约束：Emit 不能无限期阻塞——基于 channel 的 Sink 应该有缓冲区，
// 或者由活跃的读取者持续排空。
type Sink interface {
	Emit(Event)
}

// FuncSink 将一个普通函数适配为 Sink 接口。
// 便于快速创建简单的 Sink 实现。
type FuncSink func(Event)

// Emit 调用被包装的函数。如果 f 为 nil，则静默忽略。
func (f FuncSink) Emit(e Event) {
	if f != nil {
		f(e)
	}
}

// Discard 是一个丢弃所有事件的 Sink。
// 在测试中以及只关心最终会话状态的运行中非常有用。
var Discard Sink = FuncSink(func(Event) {})
