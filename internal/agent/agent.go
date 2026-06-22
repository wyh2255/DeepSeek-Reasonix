// Package agent 实现了 AI 编码代理的核心运行循环（agent loop）。
//
// 代理循环是整个系统的核心引擎，驱动以下流程：
//  1. 用户输入 → 输入组合（注入记忆、推理语言偏好、计划模式标记等）
//  2. 调用 Provider.Stream() 获取流式 Chunk
//  3. 收集 Chunk：文本 → 实时显示，推理 → 思维链展示，工具调用 → 累积
//  4. 如果有工具调用 → executeBatch() 执行（只读工具并行×8，写入工具串行）
//  5. 工具结果追加到消息历史 → 回到步骤 2
//  6. 如果没有工具调用且有文本 → 会话结束
//
// 关键机制：
//   - 上下文压缩（compaction）：当 token 接近窗口限制时自动压缩历史
//   - 会话持久化：每个助手消息（含推理）追加到 JSONL 文件
//   - 流式中断恢复：连接断开时可追加尾部恢复提示而非重放
//   - 风暴断路器（storm breaker）：检测并中断重复失败的死循环
//   - 计划模式（plan mode）：只读探索模式，拒绝所有写入操作
//   - 就绪性检查（readiness check）：确保最终答案前所有任务已完成并验证
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"reasonix/internal/diff"
	"reasonix/internal/event"
	"reasonix/internal/evidence"
	"reasonix/internal/instruction"
	"reasonix/internal/jobs"
	"reasonix/internal/memory"
	"reasonix/internal/nilutil"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

// maxToolOutputBytes 限制单个工具结果进入模型上下文之前的大小上限。
// ~32KB 约等于 8K token——足够一次完整的文件读取或繁忙的 grep，
// 同时防止意外的"读取这个 5MB 日志"操作在下次压缩运行之前耗尽窗口。
const maxToolOutputBytes = 32 * 1024

// planModeDeniedTools 列出在计划模式下被无条件拒绝的工具。
// 这些工具不会展示给 LLM，即使代理以某种方式引用也无法调用。
// write_file、edit_file 和 multi_edit 是标准的文件写入工具；
// apply_patch 是结构化的写入变体。
var planModeDeniedTools = map[string]bool{
	"write_file":  true,
	"edit_file":   true,
	"multi_edit":  true,
	"apply_patch": true,
}

// planModeBashMetachars 定义了表示命令链接、重定向或替换的 shell 元字符。
// 在计划模式下，如果 bash 命令中出现这些字符中的任何一个，命令将被阻止——
// 即使命令前缀匹配了安全的只读条目——因为链接可以在原本安全的前缀之后引入副作用。
var planModeBashMetachars = []string{"&&", "||", ">>", "<<", "$(", "\x60", ";", "|", ">", "<", "&", "\n", "\r"}

// planModeSafeBashCommands 是在计划模式下可以安全运行的 bash 命令前缀列表。
// 每个条目作为前缀与修剪后的小写命令字符串进行匹配。
// 匹配要求前缀之后是 shell 参数边界：空白字符或字符串结尾——
// 因此 "echop" 永远不会匹配 "echo"。
var planModeSafeBashCommands = []string{
	"git status", "git diff", "git log", "git show",
	"git ls-files", "git grep", "git blame",
	"ls", "cat", "grep", "find", "head", "tail", "pwd",
	"echo", "wc", "which", "type", "uname", "hostname",
	"go version", "go list", "go doc", "go vet",
	"node -v", "npm list", "python --version",
}

// planModeFindWriteArgs 列出 find 命令中具有写入或执行副作用的参数。
// 在计划模式下，包含这些参数的 find 命令将被阻止。
var planModeFindWriteArgs = map[string]bool{
	"-delete":  true,
	"-exec":    true,
	"-execdir": true,
	"-ok":      true,
	"-okdir":   true,
	"-fprint":  true,
	"-fprintf": true,
	"-fls":     true,
}

// planModeGoWriteOrExecArgs 列出 go 命令中具有写入或执行副作用的参数。
// 在计划模式下，包含这些参数的 go 命令将被阻止。
var planModeGoWriteOrExecArgs = map[string]bool{
	"-fix":      true,
	"-mod":      true,
	"-modfile":  true,
	"-toolexec": true,
	"-vettool":  true,
}

// maxFinalReadinessBlocks 是最终就绪性检查失败的最大允许次数。
// 超过此次数将终止运行，防止代理无限重试。
const maxFinalReadinessBlocks = 3

// maxEmptyFinalBlocks 是模型给出空最终答案的最大允许次数。
// 超过此次数将终止运行。
const maxEmptyFinalBlocks = 3

// maxStreamRecoveries 是流式中断恢复的最大允许次数。
// 超过此次数将把错误传播给调用者。
const maxStreamRecoveries = 1

// maxExecutorHandoffNudges 是执行器交接提示的最大允许次数。
// 当执行器在没有使用任何工具的情况下给出答案时，会提示它使用工具。
const maxExecutorHandoffNudges = 1

// Renderer 将助手的最终答案文本重新绘制为带样式的输出。
// 它仅在回合的文本流完成后应用，因此用户先看到原始的 Markdown 流式传输，
// 然后一次重绘将其替换为格式化的输出。
// 渲染器被有意设计为接口形式，使代理保持独立于 CLI 的 Markdown 库选择。
// 由 TextSink 消费使用。
type Renderer interface {
	Render(text string) string
}

// Asker 向用户提出结构化的多选问题并阻塞等待答案。
// 代理为 `ask` 工具咨询它。接口形式使代理保持独立于前端；
// nil asker 表示无交互用户（无头运行），此时 `ask` 返回"自行决定"的结果。
// 交互式前端将控制器作为 Asker 接入。
type Asker interface {
	Ask(ctx context.Context, questions []event.AskQuestion) ([]event.AskAnswer, error)
}

// callContextKey 将正在执行的工具调用的身份带入 Execute 方法。
// 使用空结构体作为 context key 以避免与其他包冲突。
type callContextKey struct{}
type parentSessionContextKey struct{} // 父会话 ID 的 context key
type userImagesContextKey struct{}    // 用户图片附件的 context key

// callContext 是工具可以读取的每调用上下文。
// 包含当前执行的调用信息，以便工具可以与代理的基础设施交互。
type callContext struct {
	parentID string    // 正在执行的调用的 ID
	sink     event.Sink // 代理的事件接收器（`task` 工具用于嵌套子代理事件）
	asker    Asker     // 用于 `ask` 工具向用户提问
	planMode bool      // 是否处于计划模式
}

// withCallContext 将正在执行的调用的 ID、代理的 Sink 和 Asker 附加到 ctx 上。
// executeOne 在每次 Execute 之前设置此上下文；
// `task` 工具读取它（通过 CallContext）以嵌套子代理事件，
// `ask` 工具读取 Asker 以向用户提问。
func withCallContext(ctx context.Context, parentID string, sink event.Sink, asker Asker, planMode bool) context.Context {
	return context.WithValue(ctx, callContextKey{}, callContext{parentID: parentID, sink: sink, asker: asker, planMode: planMode})
}

// CallContext 返回正在执行的调用的 ID、代理的 Sink 和 Asker，
// 如果上下文是由代理的 executeOne 设置的。
// 对于普通上下文（无头工具测试、在运行循环之外进行的调用），ok 为 false。
func CallContext(ctx context.Context) (parentID string, sink event.Sink, asker Asker, ok bool) {
	cc, ok := ctx.Value(callContextKey{}).(callContext)
	if !ok {
		return "", nil, nil, false
	}
	return cc.parentID, cc.sink, cc.asker, true
}

// PlanModeFromContext 报告工具调用是否在代理的只读计划门控下执行。
// 本身是 ReadOnly 的工具可以使用此方法来避免在计划期间启用后续的仅写入表面。
func PlanModeFromContext(ctx context.Context) bool {
	cc, ok := ctx.Value(callContextKey{}).(callContext)
	return ok && cc.planMode
}

// WithParentSession 将活跃的父会话 ID 附加到回合上下文上，
// 以便持久化的子代理可以记录和强制其所属的对话。
func WithParentSession(ctx context.Context, parentSession string) context.Context {
	return context.WithValue(ctx, parentSessionContextKey{}, strings.TrimSpace(parentSession))
}

// ParentSession 返回回合上下文携带的活跃父会话 ID。
func ParentSession(ctx context.Context) string {
	parentSession, _ := ctx.Value(parentSessionContextKey{}).(string)
	return strings.TrimSpace(parentSession)
}

// WithUserImages 携带用户附加到此回合的图片的数据 URL，
// 由控制器（拥有附件管理）解析，因为代理不应依赖它。
// Run 将它们嵌入用户消息；仅当模型具有视觉能力时提供者才会发送它们。
func WithUserImages(ctx context.Context, images []string) context.Context {
	return context.WithValue(ctx, userImagesContextKey{}, images)
}

func userImages(ctx context.Context) []string {
	images, _ := ctx.Value(userImagesContextKey{}).([]string)
	return images
}

// Gate 在每次工具调用时决定是否允许运行。
// 代理在执行时（计划模式门控之后）咨询它。
// 接口形式的设计使代理保持独立于权限包和 "ask" 的解析方式
// （无头运行中静默解析，聊天 TUI 中交互式解析）。
// nil gate 表示无门控——每个调用都运行，为未接入门控的调用者保留行为。
// 当 allow 为 false 时，reason 反馈给模型；非 nil 的 err
// （例如等待批准时 ctx 被取消）被视为对该调用的阻止。
type Gate interface {
	Check(ctx context.Context, toolName string, args json.RawMessage, readOnly bool) (allow bool, reason string, err error)
}

// ToolHooks 在每个工具调用前后触发用户配置的 shell 钩子。
// PreToolUse 在调用前运行，可能阻止它（block=true；message 是反馈给模型的原因）；
// PostToolUse 在调用后运行，只向用户展示输出（不能阻止）。
// 接口形式的设计使代理保持独立于钩子包——nil hooks 字段完全禁用钩子触发。
type ToolHooks interface {
	// PreToolUse 在工具调用前运行。返回 block=true 可阻止调用。
	PreToolUse(ctx context.Context, name string, args json.RawMessage) (block bool, message string)
	// PostToolUse 在工具调用后运行。只能观察结果，不能阻止。
	PostToolUse(ctx context.Context, name string, args json.RawMessage, result string)

	// PostLLMCall 在每个模型回合完成（流式传输结束）后、推理内容存储之前触发。
	// 返回（可能已翻译的）推理字符串——未配置钩子时返回原始字符串。
	// HasPostLLMCall 报告此类钩子是否存在，以便代理在没有钩子时保持推理的实时流式传输。
	PostLLMCall(ctx context.Context, reasoning string, turn int) string
	HasPostLLMCall() bool

	// SubagentStop 在 `task` 子代理完成（前台）时触发。
	SubagentStop(ctx context.Context, last string)
	// PreCompact 在压缩过程之前触发，返回额外的摘要指导（其钩子的 stdout）
	// 以折叠到摘要提示中；无钩子贡献时返回 ""。
	PreCompact(ctx context.Context, trigger string) string
}

// Agent 驱动单个任务：将一个 Provider、一个工具 Registry 和一个 Session
// 连接到主循环中。它是 AI 编码代理的核心运行时结构。
type Agent struct {
	prov        provider.Provider // 模型提供者（如 OpenAI、DeepSeek 等）
	tools       *tool.Registry    // 工具注册表，管理所有可用工具
	session     *Session          // 当前会话，包含消息历史
	sessMu      sync.Mutex        // 保护 session 指针，用于外部 Session()/SetSession 的并发访问
	maxSteps    int               // 工具调用回合的最大步数（<=0 表示无限制）
	maxStepsKey string            // 配置键名，用于在达到上限时显示给用户

	// executorHandoffGuard 由协调器（Coordinator）为执行器代理启用。
	// Run 中的每回合标记检查使普通的单模型回合不受影响。
	executorHandoffGuard bool

	temperature float64          // 模型采样温度
	pricing     *provider.Pricing // 可选的定价信息，用于成本显示
	usageSource string            // 计费使用量来源标识

	// reasoningLanguage 控制可见推理语言偏好：auto|zh|en
	// 使用 atomic.Value 以便在回合间安全更新。
	reasoningLanguage atomic.Value

	// sink 接收回合的类型化事件流（推理/文本增量、工具分派/结果、使用量、通知）。
	// 代理不再自行格式化输出——前端的 Sink 决定如何渲染。
	// 永不为 nil；New 构造函数默认将其设为 event.Discard。
	sink event.Sink

	// lastUsage 缓存提供者报告的最近一次每回合遥测数据，
	// 以便 CLI 可以在不重新抓取使用量行的情况下暴露上下文仪表。
	// 运行循环写入它，前端的状态行读取它，因此使用 atomic。
	lastUsage atomic.Pointer[provider.Usage]

	// sessCacheHit/sessCacheMiss 累积本会话每次 API 调用的缓存 token 数，
	// 以便前端可以显示聚合命中率（Σhit/Σ(hit+miss)）——
	// 这是一个比单回合命中率更稳定的、面向成本的指标。
	// 它们不会在压缩时重置（压缩只重写 session.Messages），
	// 因此当前缀被摘要化时聚合值不会骤降。
	// 使用 atomic：运行循环累积它们，状态行读取它们。
	sessCacheHit  atomic.Int64
	sessCacheMiss atomic.Int64

	// lastPrefixShape 记录上一次提供者请求的可缓存前缀，
	// 以便使用量事件可以解释下一次请求的前缀流失。
	lastPrefixShape     PrefixShape
	haveLastPrefixShape bool

	// planMode 为 true 时，拒绝任何 ReadOnly() 返回 false 的工具调用。
	// 系统提示和工具列表不会随切换而改变，因此提示缓存前缀保持有效；
	// 限制发生在执行时，模型会看到一个 "blocked" 结果并可以适应。
	// 通过外部的 SetPlanMode 切换。
	planMode atomic.Bool

	// gate 非 nil 时，是在计划模式检查之后咨询的每调用权限门控。
	// nil 完全禁用门控。
	gate Gate

	// hooks 非 nil 时，在每个工具调用前后触发 PreToolUse/PostToolUse shell 钩子。
	// nil 禁用钩子触发。
	hooks ToolHooks

	// asker 非 nil 时，允许 `ask` 工具向用户提问。
	// 在无头运行（无交互用户）中为 nil。通过 SetAsker 设置。
	asker Asker

	// onPreEdit 非 nil 时，在写入工具运行之前被调用，传入预览的变更——
	// 这是检查点存储用来快照文件编辑前内容的接口。
	// 仅对实现了 tool.Previewer 的非 ReadOnly 工具触发
	// （因此 bash 从不被跟踪，因为其目标不可预知）。
	// 通过 SetPreEditHook 设置。
	onPreEdit func(diff.Change)

	// jobs 非 nil 时，是会话的后台任务管理器。
	// executeOne 将其附加到每个工具调用的上下文中，
	// 以便后台工具（bash run_in_background, task run_in_background,
	// bash_output/kill_shell/wait）可以访问它。
	// nil 使这些工具优雅降级。
	jobs *jobs.Manager

	// steerQueue 持有在代理运行期间排队的回合中途用户消息。
	// 每条消息在每次循环迭代中被消费一次，持久化到会话以供历史重放，
	// 并作为指导（而非新任务）发送给模型。
	// 下一次 API 调用的缓存未命中是不可避免的，但仅限于一次调用——
	// 前缀在其他情况下保持稳定。
	steerMu       sync.Mutex
	steerQueue    []string
	steerConsumed bool

	// evidence 是每用户回合的主机观察到的工具回执账本。
	// 它让 complete_step 可以验证引用的证据发生在声明之前。
	evidence *evidence.Ledger

	// todoState 是主机的规范任务列表：最近一次成功的 todo_write，
	// 加上 complete_step 应用的完成状态。
	// 与每回合账本不同，它跨越回合边界和压缩存活（它从不搭载在提示中），
	// 因此最终答案门控仍然能看到后续回合会隐藏的未完成计划。
	// 在 SetSession 中从会话重建。
	todoMu    sync.Mutex
	todoState []evidence.TodoItem

	// hostAdvanceSeq 保证跨回合的工具 ID 唯一性：
	// 每次 emitTodoState 调用都递增它，因此即使相同的面板索引在不同回合中被签出，
	// 前端也能看到新的分派。
	hostAdvanceSeq atomic.Int64

	// projectChecks 是结构化的项目指令，complete_step 可以在写入支持的完成之后
	// 对同回合的 bash 回执进行验证。
	projectChecks []instruction.VerifyCheck

	// memQueue 非 nil 时，允许 remember/forget 工具将刚做出的记忆变更的回合尾注
	// 折叠到下一回合中，使其在本会话中生效而不触及缓存稳定的前缀。
	// 通过 SetMemoryQueue 设置。
	memQueue memory.Queue

	// planModeAllowedTools 是免除计划模式门控的工具名称集合。
	// 非空时，这些工具绕过只读检查。
	// 在构造期间从 Options.PlanModeAllowedTools 填充。
	planModeAllowedTools map[string]bool

	// ===== 上下文管理 =====
	// 当回合的提示接近 contextWindow 时，会话中间的较旧消息被摘要化，
	// 保留一个 token 限制的最近尾部（recentKeep 是消息下限），
	// 并将原始消息归档到 archiveDir 下。
	// compactStuck 在压缩无法将提示降至窗口以下时锁定
	// （consecutiveCompacts 超过限制），使自动压缩暂停而非循环。
	// softCompactNoticed 控制一次性软比率通知，使其每次接近触发一次而非每回合。
	contextWindow       int         // 上下文窗口大小（token 数）
	softCompactRatio    float64     // 软压缩触发比率
	compactRatio        float64     // 自动压缩触发比率
	compactForceRatio   float64     // 强制压缩触发比率
	softCompactNoticed  bool        // 软压缩通知是否已发出
	recentKeep          int         // 压缩时保留的最近消息数
	archiveDir          string      // 原始消息的归档目录
	keepPolicy          KeepPolicy  // 压缩时保留消息的策略
	compactStuck        bool        // 压缩是否卡住（无法进一步压缩）
	consecutiveCompacts int         // 连续压缩次数

	// stormSig / stormCount 跟踪以相同方式连续失败的回合序列，
	// 以便循环可以打破死亡螺旋。
	// 签名是每个调用的 (tool, error) 按顺序排列，NOT (tool, args)：
	// 卡住的模型会可靠地对参数进行表面修改（重新措辞的文章、重新排序的对象），
	// 而调用每次都以相同方式失败——基于 args 的键控会完全错过循环
	// （在截断的工具调用参数上实时观察到）。
	// 因为嵌入其主题的错误（如 "file not found: /x"）因目标而异，
	// 真正的多样化探测不会坍缩为一个签名。
	// 当回合做了任何其他事情时重置（不同的失败形状，或任何成功）。
	// 参见 applyStormBreaker。
	stormSig   string
	stormCount int

	// repeatSuccessCounts 跟踪本用户回合中已经成功的写入类工具调用。
	// 这捕获了 stormSig 的互补循环形状：
	// 模型不断执行相同的成功写入，因此没有错误供仅关注失败的风暴断路器看到。
	repeatSuccessCounts map[string]int
}

// KeepPolicy 是一个位掩码，控制在压缩期间除了最近尾部之外还保留哪些消息。
type KeepPolicy int

const (
	// KeepErrors 保留包含错误的工具结果消息，便于调试。
	KeepErrors KeepPolicy = 1 << iota
	// KeepUserMarked 保留用户标记的消息。
	KeepUserMarked
)

// SetPlanMode 切换只读门控。
// 当为 true 时，executeOne 拒绝模型调用的任何非 ReadOnly 工具，
// 并返回 "blocked" 结果而非运行它。
// 缓存友好的部分——系统提示、工具模式、消息历史——保持不变，
// 因此切换在缓存命中方面不产生任何成本。
func (a *Agent) SetPlanMode(v bool) { a.planMode.Store(v) }

// SetTools 替换代理的工具注册表。
// 下一次 API 调用会使用新的工具模式；已经缓存在提供者前缀中的工具不受影响，
// 直到前缀被失效。在回合之间调用是安全的。
func (a *Agent) SetTools(tools *tool.Registry) {
	if a == nil {
		return
	}
	a.tools = tools
}

// SetReasoningLanguage 更新此代理发出的后续用户角色消息的可见推理语言偏好。
func (a *Agent) SetReasoningLanguage(lang string) {
	if a == nil {
		return
	}
	a.reasoningLanguage.Store(NormalizeReasoningLanguage(lang))
}

// SetGate 安装每调用的权限门控。
// 由交互式 CLI 会话使用，将启动时构建的无头门控替换为提示用户的交互式门控；
// nil 禁用门控。在运行循环开始之前调用是安全的。
func (a *Agent) SetGate(g Gate) {
	if nilutil.IsNil(g) {
		g = nil
	}
	a.gate = g
}

// withReasoningLanguage 将推理语言偏好注入到用户输入中。
// 如果偏好为 "auto" 或空，则不注入任何内容。
func (a *Agent) withReasoningLanguage(input string) string {
	if a == nil {
		return input
	}
	lang := "auto"
	if v := a.reasoningLanguage.Load(); v != nil {
		if s, ok := v.(string); ok {
			lang = s
		}
	}
	return WithReasoningLanguage(input, lang)
}

// SetAsker 安装 `ask` 工具用于向用户提问的提问器。
// 交互式前端接入一个实例；无头运行保持为 nil。
func (a *Agent) SetAsker(as Asker) { a.asker = as }

// SetMemoryQueue 安装 remember/forget 工具用于在当前会话中应用记忆变更的接收器。
// 控制器将自身接入。
func (a *Agent) SetMemoryQueue(q memory.Queue) { a.memQueue = q }

// SetPreEditHook 安装编辑前快照钩子（参见 onPreEdit）。
// 控制器将其连接到其每会话的检查点存储；nil 禁用捕获。
func (a *Agent) SetPreEditHook(fn func(diff.Change)) { a.onPreEdit = fn }

// Session 返回代理的当前会话，对于需要在回合之间读取消息日志的持久化钩子很有用。
// sessMu 将此指针读取与 SetSession 序列化，因此前端（serve 的并发 /history 和 /new 处理器）
// 不会在交换时产生竞争。
// 运行循环直接访问 a.session，只在空闲时通过 SetSession 交换，因此其读取不需要加锁。
func (a *Agent) Session() *Session {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	return a.session
}

// SetSession 整体替换代理的会话。
// 由 `reasonix --resume` 在第一个回合之前加载保存的 JSONL 转录文件，
// 使模型从上次中断的地方继续。
// 调用者将其与运行中的回合序列化（只在空闲时触发）；
// sessMu 保护指针交换本身。
func (a *Agent) SetSession(s *Session) {
	a.sessMu.Lock()
	a.session = s
	a.sessMu.Unlock()
	// 重置累积缓存统计
	a.sessCacheHit.Store(0)
	a.sessCacheMiss.Store(0)
	if s != nil {
		// 从会话消息中重建规范任务列表
		a.rebuildTodoState(s.Snapshot())
	}
}

// LastUsage 返回提供者报告的最近一次每回合 token 遥测数据（如果还没有回合运行则为 nil）。
// TUI 使用它在提示旁边显示上下文仪表；实际的缓存决策仍然在 maybeCompact 中。
func (a *Agent) LastUsage() *provider.Usage { return a.lastUsage.Load() }

// SessionCache 返回本会话每次 API 调用的累积缓存命中/未命中提示 token 数——
// 这是状态行聚合命中率的基础。
func (a *Agent) SessionCache() (hit, miss int) {
	return int(a.sessCacheHit.Load()), int(a.sessCacheMiss.Load())
}

// ContextWindow 返回配置的上下文窗口大小（token 数）。
// 0 表示此代理禁用了压缩。
func (a *Agent) ContextWindow() int { return a.contextWindow }

// MidTurnSteerPrefix 是回合中途引导消息的标记前缀。
// 通过 Steer 注入的用户消息以此前缀开头。
// 模型将其视为指令；前端将其显示为通知，而非普通的用户消息气泡。
const MidTurnSteerPrefix = "[Mid-turn steer queued by the user. Do not treat this as a new task; use it only as additional guidance for the current task after completing the current step.]"

// midTurnSteerMessage 将用户文本包装为回合中途引导消息，添加前缀和分隔符。
func midTurnSteerMessage(text string) string {
	return MidTurnSteerPrefix + "\n" + text
}

// SteerText 检查内容是否为回合中途的引导消息，如果是，则返回不含包装前缀的原始用户文本。
//
// 返回的文本保留用户的精确输入——只去除前缀和 midTurnSteerMessage 在前缀与用户文本之间
// 插入的 "\n" 分隔符；不修剪空格，以便历史重放与实时 Steer 事件渲染逐字符匹配。
func SteerText(content string) (string, bool) {
	after, found := strings.CutPrefix(content, MidTurnSteerPrefix)
	if !found {
		return "", false
	}
	// Strip only the "\n" separator, preserving the user's original text.
	after = strings.TrimPrefix(after, "\n")
	return after, true
}

// Steer 将消息排队用于回合中途注入。
// 消息将在下一次循环迭代中被消费，持久化到会话，并作为指导发送给模型。
func (a *Agent) Steer(text string) {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	a.steerQueue = append(a.steerQueue, text)
	a.steerConsumed = false
}

// SteerConsumed 返回引导队列在上次消费后是否变为空。
// 用于前端检测所有引导消息是否已被送达。
func (a *Agent) SteerConsumed() bool {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	return a.steerConsumed
}

// consumeSteer 从引导队列中消费一条消息（FIFO）。
// 返回消息内容和是否成功消费。队列为空时返回 false。
func (a *Agent) consumeSteer() (string, bool) {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	if len(a.steerQueue) == 0 {
		return "", false
	}
	t := a.steerQueue[0]
	a.steerQueue = a.steerQueue[1:]
	a.steerConsumed = len(a.steerQueue) == 0
	return t, true
}

// clearSteerQueue 清空引导队列，通常在回合结束时调用。
func (a *Agent) clearSteerQueue() {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	a.steerQueue = nil
	a.steerConsumed = false
}

// steerQueueLen 返回引导队列的当前长度。
func (a *Agent) steerQueueLen() int {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	return len(a.steerQueue)
}

// CompactRatio 返回自动压缩触发的窗口比例（例如 0.8）。
// 状态行使用它来显示距离下次压缩的剩余空间。
func (a *Agent) CompactRatio() float64 { return a.compactRatio }

// CompactNow 立即运行一次压缩过程，无论使用率阈值如何。
// 由聊天 TUI 的 `/compact` 命令使用，使用户可以在前缀自然填满之前重置它。
func (a *Agent) CompactNow(ctx context.Context, instructions string) error {
	return a.compact(ctx, "manual", instructions, true)
}

// Options 配置 Agent 的各项参数。
type Options struct {
	MaxSteps    int    // 工具调用回合的最大步数（<=0 表示无限制）
	MaxStepsKey string // 配置键名，达到上限时显示给用户（空值默认为 "agent.max_steps"）

	Temperature float64          // 模型采样温度
	Pricing     *provider.Pricing // 可选的定价信息，用于每回合成本显示
	UsageSource string            // 可选的计费使用量来源；默认为 executor

	// Gate 是每调用的权限门控。nil 禁用门控。
	Gate Gate

	// ===== 上下文管理配置 =====
	// ContextWindow <= 0 禁用压缩。比率和 RecentKeep 未设置时回退到默认值。
	ContextWindow     int        // 上下文窗口大小（token 数）
	SoftCompactRatio  float64    // 软压缩触发比率（默认 0.7）
	CompactRatio      float64    // 自动压缩触发比率（默认 0.8）
	CompactForceRatio float64    // 强制压缩触发比率（默认 0.95）
	RecentKeep        int        // 压缩时保留的最近消息数
	ArchiveDir        string     // 原始消息的归档目录
	KeepPolicy        KeepPolicy // 压缩时保留消息的策略

	// Hooks 在工具调用前后触发 PreToolUse/PostToolUse shell 钩子。nil 禁用钩子。
	Hooks ToolHooks

	// Jobs 是会话的后台任务管理器（nil 禁用后台工具）。
	Jobs *jobs.Manager

	// ProjectChecks 是启动期间提取的主机可观察的结构化检查。
	ProjectChecks []instruction.VerifyCheck

	// ReasoningLanguage 控制可见推理语言偏好，作为临时的用户回合上下文。
	// 空值或 "auto" 不注入任何内容。
	ReasoningLanguage string

	// PlanModeAllowedTools 列出绕过计划模式只读门控的工具名称。
	// 当计划模式为 true 时，此处命名的工具将绕过"计划模式是只读的"阻止——
	// 即使其 ReadOnly 契约返回 false。
	// 谨慎使用；调用者负责确保工具调用在只读上下文中是安全的
	// （例如 bash 用于 git status）。
	PlanModeAllowedTools []string
}

// stringSet 将字符串切片转换为集合（map[string]bool）。
// 空切片返回 nil，避免不必要的内存分配。
func stringSet(ss []string) map[string]bool {
	if len(ss) == 0 {
		return nil
	}
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// New 构造一个 Agent 实例。
//
// 参数说明：
//   - prov: 模型提供者（如 OpenAI、DeepSeek 等）
//   - tools: 工具注册表，管理所有可用工具
//   - session: 会话实例，包含消息历史
//   - opts: 配置选项（最大步数、温度、上下文窗口等）
//   - sink: 事件接收器（nil 会被替换为 event.Discard）
//
// MaxSteps <= 0 表示无上限——运行循环持续到模型给出最终答案、
// 上下文被取消或提供者出错（压缩机制保持上下文有界）。
// nil sink 被替换为 event.Discard，使代理可以无条件地发射事件。
func New(prov provider.Provider, tools *tool.Registry, session *Session, opts Options, sink event.Sink) *Agent {
	if opts.SoftCompactRatio <= 0 {
		opts.SoftCompactRatio = defaultSoftCompactRatio
	}
	if opts.CompactRatio <= 0 {
		opts.CompactRatio = defaultCompactRatio
	}
	if opts.CompactForceRatio <= 0 {
		opts.CompactForceRatio = defaultCompactForceRatio
	}
	if opts.RecentKeep <= 0 {
		opts.RecentKeep = minRecentKeep
	}
	if nilutil.IsNil(sink) {
		sink = event.Discard
	}
	gate := opts.Gate
	if nilutil.IsNil(gate) {
		gate = nil
	}
	hooks := opts.Hooks
	if nilutil.IsNil(hooks) {
		hooks = nil
	}
	maxStepsKey := opts.MaxStepsKey
	if strings.TrimSpace(maxStepsKey) == "" {
		maxStepsKey = "agent.max_steps"
	}
	a := &Agent{
		prov:                 prov,
		tools:                tools,
		session:              session,
		maxSteps:             opts.MaxSteps,
		maxStepsKey:          maxStepsKey,
		temperature:          opts.Temperature,
		pricing:              opts.Pricing,
		usageSource:          usageSourceOrDefault(opts.UsageSource, event.UsageSourceExecutor),
		sink:                 sink,
		gate:                 gate,
		hooks:                hooks,
		jobs:                 opts.Jobs,
		evidence:             evidence.NewLedger(),
		projectChecks:        append([]instruction.VerifyCheck(nil), opts.ProjectChecks...),
		contextWindow:        opts.ContextWindow,
		softCompactRatio:     opts.SoftCompactRatio,
		compactRatio:         opts.CompactRatio,
		compactForceRatio:    opts.CompactForceRatio,
		recentKeep:           opts.RecentKeep,
		archiveDir:           opts.ArchiveDir,
		keepPolicy:           opts.KeepPolicy,
		planModeAllowedTools: stringSet(opts.PlanModeAllowedTools),
	}
	a.SetReasoningLanguage(opts.ReasoningLanguage)
	return a
}

// usageSourceOrDefault 返回使用量来源，如果为空则返回默认值。
func usageSourceOrDefault(source, fallback string) string {
	source = strings.TrimSpace(source)
	if source != "" {
		return source
	}
	return fallback
}

// Run 追加用户输入并驱动工具循环，直到模型返回最终答案（无工具调用）、
// 上下文被取消或提供者出错。
//
// 核心循环逻辑：
//  1. 发射 TurnStarted 事件，将用户输入添加到会话
//  2. 进入主循环，每轮迭代：
//     a. 消费排队的引导消息（steer）
//     b. 调用 stream() 获取模型的流式响应
//     c. 如果流中断且可恢复，追加恢复提示并重试
//     d. 将助手消息添加到会话（包含推理内容和工具调用）
//     e. 如果没有工具调用：进行就绪性检查，通过则返回
//     f. 如果有工具调用：执行批次，将结果添加到会话，可能触发压缩
//     g. 如果达到 maxSteps 上限，进入宽限期（grace round）
//
// 参数：
//   - ctx: 上下文，用于取消控制
//   - input: 用户的输入文本
//
// 返回值：错误信息，nil 表示正常完成。
func (a *Agent) Run(ctx context.Context, input string) error {
	// 清理引导队列：回合结束时清空，防止上一回合的引导消息泄漏到下一回合
	defer a.clearSteerQueue()
	a.steerMu.Lock()
	a.steerConsumed = false
	a.steerMu.Unlock()

	// 重置每回合状态：证据账本和重复成功计数器
	if a.evidence != nil {
		a.evidence.Reset()
	}
	a.repeatSuccessCounts = nil

	// 发射回合开始事件，通知前端重置渲染状态
	a.sink.Emit(event.Event{Kind: event.TurnStarted})

	// 注入推理语言偏好到用户输入中
	input = a.withReasoningLanguage(input)

	// 将用户消息添加到会话历史（包含可选的图片附件）
	a.session.Add(provider.Message{Role: provider.RoleUser, Content: input, Images: userImages(ctx)})

	// ===== 回合级计数器 =====
	finalReadinessBlocks := 0    // 最终就绪性检查失败次数
	emptyFinalBlocks := 0        // 空最终答案次数
	handoffNudges := 0           // 执行器交接提示次数
	usedAnyTool := false         // 本回合是否使用过任何工具
	streamRecoveries := 0        // 流恢复次数
	graceRound := false          // 是否处于宽限期
	executorHandoff := a.executorHandoffGuard && strings.Contains(input, executorHandoffMarker)

	// ===== 主循环 =====
	// maxSteps <= 0 时循环无界——自然终止是模型完成，
	// 真正的安全边界是用户取消和压缩，而非回合计数。
	// 正的 maxSteps 施加可选的硬防护，达到时显示可恢复的通知。
	for step := 0; a.maxSteps <= 0 || step < a.maxSteps || graceRound; step++ {
		// 消费排队的引导消息并持久化到会话，使其在标签切换和历史重放中存活。
		// 模型将其视为指导（带前缀），而非新任务。
		// 每次引导不可避免地导致一次缓存未命中——模型必须看到新指令。
		if text, ok := a.consumeSteer(); ok {
			a.session.Add(provider.Message{Role: provider.RoleUser, Content: a.withReasoningLanguage(midTurnSteerMessage(text))})
			a.sink.Emit(event.Event{Kind: event.Steer, Text: text})
		}
		schemas := a.tools.Schemas()
		prefixShape := a.capturePrefixShape(schemas)
		prevPrefixShape := a.lastPrefixShape
		if !a.haveLastPrefixShape {
			prevPrefixShape = prefixShape
		}

		text, reasoning, signature, calls, usage, interrupted, partialToolStarted, err := a.stream(ctx, step+1)
		if err != nil {
			if interrupted && streamRecoveries < maxStreamRecoveries {
				streamRecoveries++
				if hasVisibleFinalAnswer(text) {
					a.session.Add(provider.Message{
						Role:               provider.RoleAssistant,
						Content:            text,
						ReasoningContent:   reasoning,
						ReasoningSignature: signature,
					})
				}
				a.session.Add(provider.Message{
					Role:    provider.RoleUser,
					Content: a.withReasoningLanguage(streamRecoveryMessage(hasVisibleFinalAnswer(text), partialToolStarted)),
				})
				a.sink.Emit(event.Event{Kind: event.Retrying, RetryAttempt: streamRecoveries, RetryMax: maxStreamRecoveries})
				step-- // recovery retries do not consume the tool-round maxSteps budget
				continue
			}
			return err
		}
		streamRecoveries = 0
		cacheDiagnostics := CompareShape(prevPrefixShape, prefixShape, usage)
		a.lastPrefixShape = prefixShape
		a.haveLastPrefixShape = true
		if usage != nil && usage.TotalTokens > 0 {
			a.sink.Emit(event.Event{Kind: event.Usage, Usage: usage, Pricing: a.pricing,
				UsageSource:      a.usageSource,
				CacheDiagnostics: &cacheDiagnostics,
				SessionHit:       int(a.sessCacheHit.Load()), SessionMiss: int(a.sessCacheMiss.Load())})
		}
		if msg, ok := finishReasonMessage(usage); ok {
			a.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: msg})
		}

		// Keep reasoning_content on the assistant turn for display and session
		// archive. It is NOT re-uploaded to the API: the openai provider drops it
		// when building the request, since re-sent reasoning is billable prompt
		// input for no cache or coherence gain.
		calls = a.withPreviewFileDiffs(calls)
		a.session.Add(provider.Message{
			Role:               provider.RoleAssistant,
			Content:            text,
			ReasoningContent:   reasoning,
			ReasoningSignature: signature,
			ToolCalls:          calls,
		})

		if len(calls) == 0 {
			readiness := a.finalReadinessCheck()
			if readiness.reason != "" {
				finalReadinessBlocks++
				result := evidence.ReadinessBlocked
				if finalReadinessBlocks >= maxFinalReadinessBlocks {
					result = evidence.ReadinessErrored
					event.RecordReadinessAudit(a.sink, readiness.audit(result, false))
					return fmt.Errorf("final-answer readiness failed %d times: %s", finalReadinessBlocks, readiness.reason)
				}
				event.RecordReadinessAudit(a.sink, readiness.audit(result, false))
				a.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: "final-answer readiness blocked: " + readiness.reason})
				a.session.Add(provider.Message{Role: provider.RoleUser, Content: a.withReasoningLanguage(finalReadinessRetryMessage(readiness.reason))})
				a.maybeCompact(ctx, usage)
				continue
			}
			if !hasVisibleFinalAnswer(text) {
				emptyFinalBlocks++
				if emptyFinalBlocks >= maxEmptyFinalBlocks {
					return fmt.Errorf("model finished without a visible final answer %d times", emptyFinalBlocks)
				}
				a.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: emptyFinalNotice(a.prov.Name(), usage, len(reasoning))})
				a.session.Add(provider.Message{Role: provider.RoleUser, Content: a.withReasoningLanguage(emptyFinalRetryMessage())})
				a.maybeCompact(ctx, usage)
				continue
			}
			if executorHandoff && !usedAnyTool && handoffNudges < maxExecutorHandoffNudges && shouldNudgeExecutorHandoff(input, text) {
				handoffNudges++
				a.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: "executor answered without taking any action; nudging it to use its tools"})
				a.session.Add(provider.Message{Role: provider.RoleUser, Content: a.withReasoningLanguage(executorHandoffRetryMessage())})
				a.maybeCompact(ctx, usage)
				continue
			}
			if readiness.applies {
				event.RecordReadinessAudit(a.sink, readiness.audit(evidence.ReadinessAllowed, finalReadinessBlocks > 0))
			}
			if a.steerQueueLen() > 0 {
				continue
			}
			// A final-answer turn otherwise skips compaction, so a large context
			// carries into the next turn un-folded and can overflow the model window.
			// No-op below the trigger, so normal turns keep their warm cache.
			a.maybeCompact(ctx, usage)
			return nil // model gave a final answer
		}
		emptyFinalBlocks = 0
		usedAnyTool = true

		// Grace round guard: if we already gave the model one extra response
		// and it still wants to call tools, stop here.
		if graceRound {
			return fmt.Errorf("paused after %d tool-call rounds (%s) — the work so far is saved; send another message to continue, or set %s higher or to 0 for no limit", a.maxSteps, a.maxStepsKey, a.maxStepsKey)
		}

		results := a.executeBatch(ctx, calls)
		for i, call := range calls {
			a.session.Add(provider.Message{
				Role:       provider.RoleTool,
				Content:    results[i],
				ToolCallID: call.ID,
				Name:       call.Name,
			})
		}
		// If the context was cancelled during tool execution, return after storing
		// the batch results so the session keeps paired tool-call history.
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// The prompt only grows from here; compact before the next turn so it
		// stays within the model's window.
		a.maybeCompact(ctx, usage)

		// When the tool-call budget runs out this round, give the model
		// one grace round to produce a final answer from completed work.
		if a.maxSteps > 0 && step+1 >= a.maxSteps {
			graceRound = true
			nudge := fmt.Sprintf("Do not call any more tools — your tool-call round limit (%s) has been reached. Instead, synthesize a final answer from all the work already completed: summarize what was accomplished, what remains to be done, and any decisions the user should make. The user can increase %s or continue in the next turn if more work is needed.", a.maxStepsKey, a.maxStepsKey)
			a.session.Add(provider.Message{Role: provider.RoleUser, Content: a.withReasoningLanguage(nudge)})
			a.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: fmt.Sprintf("budget (%s=%d) exhausted: one grace round to finalize", a.maxStepsKey, a.maxSteps)})
		}
	}
	// Only reached when a positive maxSteps guard is configured. The work so far
	// is already in the session, so the user can just send another message to pick
	// up where it left off.
	return fmt.Errorf("paused after %d tool-call rounds (%s) — the work so far is saved; send another message to continue, or set %s higher or to 0 for no limit", a.maxSteps, a.maxStepsKey, a.maxStepsKey)
}

func (a *Agent) finalReadinessFailure() string {
	return a.finalReadinessCheck().reason
}

// GoalReadinessFailure 返回最终就绪性失败原因——未完成的 todo 和未验证的项目检查的摘要——
// 如果没有则返回空字符串。
// 导出此方法以便控制器可以基于证据限制 [goal:complete]。
func (a *Agent) GoalReadinessFailure() string {
	return a.finalReadinessFailure()
}

// finalReadinessCheck 表示最终就绪性检查的结果。
type finalReadinessCheck struct {
	applies              bool   // 检查是否适用（是否有需要检查的条件）
	reason               string // 失败原因（空表示通过）
	missingProjectChecks int    // 缺失的项目检查数量
	incompleteTodos      int    // 未完成的 todo 数量
}

func (c finalReadinessCheck) audit(result evidence.ReadinessAuditResult, recovered bool) evidence.ReadinessAudit {
	return evidence.ReadinessAudit{
		Result:                 result,
		Recovered:              recovered,
		MissingProjectChecks:   c.missingProjectChecks,
		IncompleteTodos:        c.incompleteTodos,
		CommandMismatchMissing: c.missingProjectChecks,
	}
}

// finalReadinessCheck 执行最终就绪性检查，确保代理在给出最终答案之前
// 已完成所有必要的工作。
//
// 检查内容：
//  1. 如果有 todo 列表且有未完成项，阻止最终答案
//  2. 如果有项目检查且最近一次写入后未运行验证命令，阻止最终答案
//  3. 如果有 todo_write 但没有 complete_step 的证据，检查是否需要验证
func (a *Agent) finalReadinessCheck() finalReadinessCheck {
	if a.evidence == nil {
		return finalReadinessCheck{}
	}
	var missing []string
	out := finalReadinessCheck{}
	if !a.planMode.Load() {
		incomplete, hasTodos := a.evidence.IncompleteLatestTodos()
		if !hasTodos && a.evidence.HasAnySuccessfulReceipt() {
			incomplete, hasTodos = a.incompleteCanonicalTodos()
		}
		if hasTodos && len(incomplete) > 0 && a.evidence.HasSuccessfulTodoProgressReceipt() {
			out.applies = true
			out.incompleteTodos = len(incomplete)
			missing = append(missing, finalReadinessIncompleteTodos(incomplete))
		}
	}
	writer, hasWriter := a.evidence.LatestSuccessfulWriterIndex()
	if !hasWriter {
		if len(missing) > 0 {
			out.reason = strings.Join(missing, "; ")
		}
		return out
	}
	hasProjectChecks := len(a.projectChecks) > 0
	hasTodoReceipt := a.evidence.HasSuccessfulTodoWrite()
	if !hasProjectChecks && !hasTodoReceipt && len(missing) == 0 {
		return finalReadinessCheck{}
	}
	out.applies = true
	for _, check := range a.projectChecks {
		command := strings.TrimSpace(check.Command)
		if command == "" {
			continue
		}
		if !a.evidence.HasSuccessfulCommandAfter(command, writer) {
			out.missingProjectChecks++
			missing = append(missing, fmt.Sprintf("run %q from %s after the latest write", command, finalReadinessCheckSource(check)))
		}
	}

	if len(missing) == 0 {
		return out
	}
	out.reason = strings.Join(missing, "; ")
	return out
}

// finalReadinessIncompleteTodos 生成未完成 todo 项的描述字符串。
// 格式为 "todo 内容: 状态"，多个项用逗号分隔。
func finalReadinessIncompleteTodos(items []evidence.TodoStepMatch) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		label := strings.TrimSpace(item.Content)
		if label == "" {
			label = fmt.Sprintf("todo %d", item.Index)
		}
		parts = append(parts, fmt.Sprintf("%s: %s", label, item.Status))
	}
	return "latest successful todo_write still has incomplete items: " + strings.Join(parts, ", ")
}

// setTodoState 线程安全地设置规范任务列表。
func (a *Agent) setTodoState(todos []evidence.TodoItem) {
	a.todoMu.Lock()
	a.todoState = append([]evidence.TodoItem(nil), todos...)
	a.todoMu.Unlock()
}

// SeedTodoState 从主机生成的初始列表（如已批准的计划）初始化规范任务列表。
// 新的主机种子替换早期工作的陈旧状态，使 complete_step 与 UI 刚刚显示的计划匹配。
func (a *Agent) SeedTodoState(todos []evidence.TodoItem) {
	if len(todos) == 0 {
		return
	}
	a.setTodoState(todos)
}

// ReplaceTodoState 将主机生成的 todo 列表镜像到规范状态中。
// 当主机（而非模型）拥有完整状态转换时使用。
func (a *Agent) ReplaceTodoState(todos []evidence.TodoItem) {
	a.setTodoState(todos)
	a.recordTodoState(todos)
}

// CanonicalTodoState 返回主机重建的任务列表的副本。
func (a *Agent) CanonicalTodoState() []evidence.TodoItem {
	a.todoMu.Lock()
	defer a.todoMu.Unlock()
	return append([]evidence.TodoItem(nil), a.todoState...)
}

func (a *Agent) incompleteCanonicalTodos() ([]evidence.TodoStepMatch, bool) {
	a.todoMu.Lock()
	defer a.todoMu.Unlock()
	if len(a.todoState) == 0 {
		return nil, false
	}
	return evidence.IncompleteTodos(a.todoState), true
}

// advanceCanonicalTodo 将匹配已签出步骤的规范 todo 翻转为已完成
// （将下一个待处理项提升为 in_progress），并发射一个合成的 todo_write 事件，
// 以便任务面板反映此变更而无需模型重新发送整个列表。
// 当没有匹配项或已完成时为空操作。
func (a *Agent) advanceCanonicalTodo(step string) {
	a.todoMu.Lock()
	if len(a.todoState) == 0 {
		a.todoMu.Unlock()
		return
	}
	m, ok := evidence.MatchStep(step, a.todoState)
	if !ok || canonicalTodoStatus(a.todoState[m.Index-1].Status) == "completed" {
		a.todoMu.Unlock()
		return
	}
	a.todoState[m.Index-1].Status = "completed"
	promoteNextPendingTodo(a.todoState)
	snapshot := append([]evidence.TodoItem(nil), a.todoState...)
	a.todoMu.Unlock()
	a.recordTodoState(snapshot)
	a.emitTodoState(snapshot, m.Index)
}

// recordTodoState 将主机推进的列表记录为合成的 todo_write 回执，
// 以便每回合的最终门控（读取账本的最新 todo_write）看到推进——
// 模型不再需要重新发送 todo_write 来标记完成。
// 它绕过 todo_write 工具，因此完成转换守卫永远不会对它运行。
func (a *Agent) recordTodoState(todos []evidence.TodoItem) {
	if a.evidence == nil {
		return
	}
	args, err := json.Marshal(map[string]any{"todos": todos})
	if err != nil {
		return
	}
	a.evidence.Record(evidence.ReceiptFromToolCall("todo_write", json.RawMessage(args), true, true))
}

// promoteNextPendingTodo 将下一个待处理的 todo 项提升为进行中状态。
// 如果已经有进行中的项，则不做任何操作。
func promoteNextPendingTodo(todos []evidence.TodoItem) {
	for _, t := range todos {
		if canonicalTodoStatus(t.Status) == "in_progress" {
			return
		}
	}
	for i := range todos {
		if canonicalTodoStatus(todos[i].Status) == "pending" {
			todos[i].Status = "in_progress"
			return
		}
	}
}

// canonicalTodoStatus 返回规范化的 todo 状态字符串。
// 空字符串被视为 "pending"。
func canonicalTodoStatus(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "pending"
	}
	return s
}

// emitTodoState 发射一个合成的 todo_write 事件，以便前端任务面板反映主机推进的完成状态，
// 而无需模型重新发送列表。
// itemIndex 是已完成 todo 在面板中的 1-based 位置。
func (a *Agent) emitTodoState(todos []evidence.TodoItem, itemIndex int) {
	args, err := json.Marshal(map[string]any{"todos": todos})
	if err != nil {
		return
	}
	id := fmt.Sprintf("host-advance-%d-%d", a.hostAdvanceSeq.Add(1), itemIndex)
	t := event.Tool{ID: id, Name: "todo_write", Args: string(args), ReadOnly: true}
	a.sink.Emit(event.Event{Kind: event.ToolDispatch, Tool: t})
	t.Output = "task list advanced by complete_step"
	a.sink.Emit(event.Event{Kind: event.ToolResult, Tool: t})
}

// rebuildTodoState 从转录文件重建规范任务列表：
// 最近一次成功的 todo_write 作为基础，之后的每个 complete_step 推进一个项目。
// 从持久化的消息中确定性地重建，因此可以承受全新加载或回退
// （截断的历史产生历史状态）。
// 压缩丢弃 todo_write 后为空——不会比没有规范列表更糟。
func (a *Agent) rebuildTodoState(msgs []provider.Message) {
	successful := successfulToolCallIDs(msgs)
	var todos []evidence.TodoItem
	baseIdx := -1
	for i, msg := range msgs {
		for _, tc := range msg.ToolCalls {
			if tc.Name != "todo_write" || !successful[tc.ID] {
				continue
			}
			rec := evidence.ReceiptFromToolCall(tc.Name, json.RawMessage(tc.Arguments), true, true)
			// A successful empty todo_write is an explicit clear. Preserve it as the
			// latest base so history reloads do not resurrect an older non-empty list.
			todos = append([]evidence.TodoItem(nil), rec.Todos...)
			baseIdx = i
		}
	}
	if baseIdx < 0 {
		a.setTodoState(nil)
		return
	}
	for i := baseIdx; i < len(msgs); i++ {
		for _, tc := range msgs[i].ToolCalls {
			if tc.Name != "complete_step" || !successful[tc.ID] {
				continue
			}
			rec := evidence.ReceiptFromToolCall(tc.Name, json.RawMessage(tc.Arguments), true, true)
			if m, ok := evidence.MatchStep(rec.Step, todos); ok && canonicalTodoStatus(todos[m.Index-1].Status) != "completed" {
				todos[m.Index-1].Status = "completed"
				promoteNextPendingTodo(todos)
			}
		}
	}
	a.setTodoState(todos)
}

// successfulToolCallIDs 从消息历史中提取所有成功的工具调用 ID。
// 用于 rebuildTodoState 以确定哪些工具调用是成功的。
func successfulToolCallIDs(msgs []provider.Message) map[string]bool {
	successful := map[string]bool{}
	for _, msg := range msgs {
		if msg.Role != provider.RoleTool || msg.ToolCallID == "" {
			continue
		}
		if !toolResultFailed(msg.Content) {
			successful[msg.ToolCallID] = true
		}
	}
	return successful
}

// toolResultFailed 检查工具结果内容是否表示失败。
// 失败的标志：以 "error:", "blocked:", "Error:", "[error" 开头。
func toolResultFailed(content string) bool {
	content = strings.TrimSpace(content)
	return strings.HasPrefix(content, "error:") ||
		strings.HasPrefix(content, "blocked:") ||
		strings.HasPrefix(content, "Error:") ||
		strings.HasPrefix(content, "[error")
}

// finalReadinessCheckSource 返回项目检查的来源描述。
// 优先使用 SourcePath，否则使用 "project memory"；如果有行号则附加。
func finalReadinessCheckSource(check instruction.VerifyCheck) string {
	source := strings.TrimSpace(check.SourcePath)
	if source == "" {
		source = "project memory"
	}
	if check.Line > 0 {
		return fmt.Sprintf("%s:%d", source, check.Line)
	}
	return source
}

// finalReadinessRetryMessage 生成最终就绪性检查失败的重试提示消息。
// 指示模型在给出最终答案之前解决缺失的主机可观察回执。
func finalReadinessRetryMessage(reason string) string {
	return "Host final-answer readiness check failed. Before giving a final answer, address the missing host-observable receipts: " + reason + ". Run the required tool calls, then answer when readiness is satisfied."
}

// shouldNudgeExecutorHandoff 判断是否应该提示执行器使用工具。
// 如果执行器的纯文本回答是合理的（例如任务本身就是文本输出），则不提示。
func shouldNudgeExecutorHandoff(input, answer string) bool {
	return !executorHandoffAllowsTextOnly(input, answer)
}

// executorHandoffAllowsTextOnly 判断执行器的纯文本回答是否合理。
// 检查逻辑：
//  1. 如果回答看起来像延迟/推辞（如"好的"、"没问题"），不允许
//  2. 解析执行器交接消息，提取任务和计划
//  3. 如果任务本身是纯文本类型（如"总结"、"解释"），允许
//  4. 如果计划是纯文本类型（如"告诉用户"、"无需工具"），允许
func executorHandoffAllowsTextOnly(input, answer string) bool {
	if looksLikeExecutorHandoffDeferral(answer) {
		return false
	}
	task, plan, ok := parseExecutorHandoff(input)
	if !ok {
		return false
	}
	if handoffTaskLooksTextOnly(task) {
		return true
	}
	return handoffPlanLooksTextOnly(plan)
}

// parseExecutorHandoff 解析执行器交接消息，提取原始任务和规划器输出。
// 消息格式：
//
//	# <executorHandoffMarker>
//	...
//	Original task:
//	<task>
//	Planner output:
//	<plan>
//	Executor instructions:
//	...
func parseExecutorHandoff(input string) (task, plan string, ok bool) {
	input = StripTransientUserBlocks(input)
	marker := "# " + executorHandoffMarker
	i := strings.Index(input, marker)
	if i < 0 {
		return "", "", false
	}
	input = input[i+len(marker):]
	_, input, ok = strings.Cut(input, "\n\nOriginal task:\n")
	if !ok {
		return "", "", false
	}
	task, input, ok = strings.Cut(input, "\n\nPlanner output:\n")
	if !ok {
		return "", "", false
	}
	plan, _, ok = strings.Cut(input, "\n\nExecutor instructions:")
	if !ok {
		return "", "", false
	}
	if beforeToolContext, _, found := strings.Cut(plan, "\n\nExecutor tool context:"); found {
		plan = beforeToolContext
	}
	return strings.TrimSpace(task), strings.TrimSpace(plan), true
}

// looksLikeExecutorHandoffDeferral 判断执行器的回答是否看起来像延迟/推辞。
// 延迟回答包括：
//   - 空回答
//   - 包含延迟短语的回答（如"计划看起来不错"、"我可以实现"）
//   - 简短的确认回答（如"ok"、"好的"、"没问题"）
func looksLikeExecutorHandoffDeferral(answer string) bool {
	lower := strings.ToLower(strings.TrimSpace(answer))
	if lower == "" {
		return true
	}
	if containsAnySubstring(lower, executorHandoffDeferralPhrases) {
		return true
	}
	switch strings.Trim(lower, " \t\r\n.!?。！？") {
	case "ok", "okay", "sounds good", "done", "好的", "可以", "没问题", "收到":
		return true
	default:
		return false
	}
}

// handoffTaskLooksTextOnly 判断任务描述是否看起来是纯文本类型。
// 如果任务包含工作请求术语（如"实现"、"修复"、"编辑"），则不是纯文本。
// 如果任务包含纯文本术语（如"总结"、"解释"、"怎么办"），则是纯文本。
func handoffTaskLooksTextOnly(task string) bool {
	lower := strings.ToLower(strings.TrimSpace(task))
	if lower == "" {
		return false
	}
	if containsAnySubstring(lower, executorHandoffWorkRequestTerms) {
		return false
	}
	return containsAnySubstring(lower, executorHandoffTextOnlyTaskTerms)
}

// handoffPlanLooksTextOnly 判断计划描述是否看起来是纯文本类型。
// 如果计划包含本地操作术语（如"write_file"、"bash"、"文件"），则不是纯文本。
// 如果计划包含纯文本术语（如"告诉用户"、"总结"、"无需工具"），则是纯文本。
// 如果计划包含问号（?），也视为纯文本（向用户提问）。
func handoffPlanLooksTextOnly(plan string) bool {
	lower := strings.ToLower(strings.TrimSpace(plan))
	if lower == "" {
		return false
	}
	if containsAnySubstring(lower, executorHandoffLocalActionTerms) {
		return false
	}
	if containsAnySubstring(lower, executorHandoffTextOnlyPlanTerms) {
		return true
	}
	return strings.Contains(lower, "?")
}

// containsAnySubstring 检查字符串 s 是否包含 terms 中的任何一个子串。
func containsAnySubstring(s string, terms []string) bool {
	for _, term := range terms {
		if strings.Contains(s, term) {
			return true
		}
	}
	return false
}

// executorHandoffDeferralPhrases 是执行器延迟/推辞回答的短语列表。
// 包含中英文的各种确认和延迟表达。
var executorHandoffDeferralPhrases = []string{
	"plan looks", "looks good", "should be easy", "should be straightforward",
	"i can implement", "i'll implement", "i will implement", "i'll get started",
	"let me ", "i will now", "i'll now", "i can do that",
	"计划看起来", "可以实现", "我会", "我将", "接下来我", "马上开始",
}

// executorHandoffWorkRequestTerms 是工作请求术语列表。
// 如果任务包含这些术语，说明需要实际的工具操作而非纯文本回答。
var executorHandoffWorkRequestTerms = []string{
	"implement", "fix", "refactor", "migrate", "edit", "write", "create", "delete",
	"update", "remove", "add ", "test", "build", "repair", "patch",
	"修改", "修复", "实现", "新增", "重构", "迁移", "补齐", "更新", "删除", "移除",
}

// executorHandoffTextOnlyTaskTerms 是纯文本任务术语列表。
// 如果任务包含这些术语，说明纯文本回答是合理的。
var executorHandoffTextOnlyTaskTerms = []string{
	"now what", "what next", "tl;dr", "tldr", "summarize", "summary", "explain",
	"i installed", "i just installed", "i turned on", "i enabled", "it's on", "it is on",
	"怎么办", "下一步", "然后呢", "总结", "解释", "说明", "装了", "装好了", "安装了", "开了", "开启了", "打开了",
}

// executorHandoffLocalActionTerms 是本地操作术语列表。
// 如果计划包含这些术语，说明需要实际的工具操作而非纯文本回答。
var executorHandoffLocalActionTerms = []string{
	"write_file", "read_file", "apply_patch", "bash",
	"workspace", "repo", "repository", "codebase", "file", "path",
	"write ", "edit ", "modify ", "create ", "delete ", "remove ", "update ", "add ", "patch ", "refactor ", "implement ",
	"run ", "command", "test", "build",
	"文件", "路径", "仓库", "代码", "写入", "编辑", "修改", "创建", "删除", "移除", "更新", "新增", "运行", "命令", "测试", "构建",
}

// executorHandoffTextOnlyPlanTerms 是纯文本计划术语列表。
// 如果计划包含这些术语，说明纯文本回答是合理的。
var executorHandoffTextOnlyPlanTerms = []string{
	"tell the user", "ask the user", "guide the user", "explain to the user",
	"summarize", "summary", "tl;dr", "tldr", "answer the user", "respond to the user",
	"provide guidance", "walk the user", "instruct the user", "have the user",
	"user should", "the user should", "user can", "the user can", "manual", "manually",
	"no tools needed", "no tool calls needed", "does not need tools", "needs no tools",
	"listen", "play a song", "compare the difference", "checkbox",
	"告诉用户", "询问用户", "问用户", "让用户", "请用户", "指导用户", "解释", "总结", "回答",
	"手动", "无需工具", "不需要工具", "试听", "听歌", "对比", "勾选",
}

// executorHandoffRetryMessage 生成执行器交接重试的提示消息。
// 当执行器在没有使用任何工具的情况下给出答案时，提示它使用工具执行任务。
func executorHandoffRetryMessage() string {
	return `You are already in the executor phase. The planner's read-only limitations do not apply to you.

The tool schema is still attached to this executor request. Do not invent that MCP servers or tools are unavailable; only report an unavailable tool after a real tool call or host error proves it.

Do not answer as the planner and do not ask how to trigger the executor.
Use your available tools now to carry out the task. If a write or command is blocked by permissions or workspace boundaries, state that specific blocker and ask for the needed approval/path.`
}

// hasVisibleFinalAnswer 检查模型的响应是否包含可见的最终答案文本。
// 空白字符串被视为没有可见答案（模型可能只输出了推理而没有回答）。
func hasVisibleFinalAnswer(text string) bool {
	return strings.TrimSpace(text) != ""
}

// emptyFinalRetryMessage 生成空最终答案重试的提示消息。
// 当模型给出空的最终答案（只有推理没有回答文本）时，提示它继续并提供可见的答案。
func emptyFinalRetryMessage() string {
	return "The previous assistant response finished without any visible answer text. Continue the same task now and provide a concise visible answer to the user. Do not send reasoning only."
}

// emptyFinalNotice 生成空最终答案的通知消息。
// 包含提供者名称、完成原因和推理长度等诊断信息。
func emptyFinalNotice(prov string, u *provider.Usage, reasoningLen int) string {
	finish := "unknown"
	if u != nil && u.FinishReason != "" {
		finish = u.FinishReason
	}
	return fmt.Sprintf("empty final answer blocked: %s returned no visible answer text (finish=%s, reasoning=%d chars); retrying", prov, finish, reasoningLen)
}

// streamRecoveryMessage 生成流中断恢复的提示消息。
//
// 根据中断时的状态生成不同的恢复指令：
//   - hadPartialTool: 工具调用正在流式传输时中断——指示模型从头发出新的完整工具调用
//   - hasPartialText: 有部分回答文本——指示模型从中断处继续，不重复已有文本
//   - 默认: 在可见答案文本完成之前中断——指示模型提供下一个有用的响应
func streamRecoveryMessage(hasPartialText, hadPartialTool bool) string {
	switch {
	case hadPartialTool:
		return "The previous assistant response was interrupted while a tool call was streaming. Continue the same task now. If a tool is still needed, issue a fresh complete tool call from scratch; do not rely on any partial tool-call arguments from the interrupted stream."
	case hasPartialText:
		return "The previous assistant response was interrupted during streaming. Continue the same task from immediately after the partial assistant message above. Do not repeat text that is already visible."
	default:
		return "The previous assistant response was interrupted during streaming before visible answer text was completed. Continue the same task now and provide the next useful response."
	}
}

// stream 执行一次模型补全（completion），将推理和文本增量作为类型化事件发射，
// 并收集完整的工具调用。
//
// 流式处理流程：
//  1. 调用 Provider.Stream() 获取流式 Chunk 通道
//  2. 遍历 Chunk，按类型处理：
//     - ChunkReasoning: 累积推理文本，实时发射 Reasoning 事件（除非有 PostLLMCall 钩子）
//     - ChunkText: 累积回答文本，实时发射 Text 事件
//     - ChunkToolCallStart: 发射部分 ToolDispatch 事件（ID/Name 已知，Args 仍在流式传输）
//     - ChunkToolCall: 收集完整的工具调用
//     - ChunkUsage: 记录 token 使用量，更新缓存统计
//     - ChunkError: 处理错误，区分可恢复的流中断和不可恢复的错误
//  3. 流完成后，如果有 PostLLMCall 钩子，转换并发射完整的推理文本
//  4. 发射 Message 事件关闭文本流，让 Sink 可以重新渲染为带样式的 Markdown
//
// 返回值：
//   - text: 累积的回答文本
//   - reasoning: 累积的推理文本（可能经过 PostLLMCall 钩子转换）
//   - signature: 提供者签发的推理证明（Anthropic thinking 模式）
//   - calls: 完整的工具调用列表
//   - usage: token 使用量数据
//   - interrupted: 流是否被中断（可恢复）
//   - partialToolStarted: 是否有工具调用已开始流式传输
//   - err: 错误信息
func (a *Agent) stream(ctx context.Context, turn int) (string, string, string, []provider.ToolCall, *provider.Usage, bool, bool, error) {
	// 注入重试通知回调：当提供者因瞬态失败重试时，发射 Retrying 事件通知前端
	ctx = provider.WithRetryNotify(ctx, func(info provider.RetryInfo) {
		a.sink.Emit(event.Event{Kind: event.Retrying, RetryAttempt: info.Attempt, RetryMax: info.Max})
	})

	// 调用提供者的流式 API，获取 Chunk 通道
	ch, err := a.prov.Stream(ctx, provider.Request{
		Messages:    a.session.Messages, // 完整的消息历史
		Tools:       a.tools.Schemas(),  // 工具模式列表
		Temperature: a.temperature,      // 采样温度
	})
	if err != nil {
		return "", "", "", nil, nil, false, false, err
	}

	// PostLLMCall 钩子会重写整个推理块，因此当钩子存在时，
	// 我们静默缓冲推理文本，在流完成后一次性发射转换后的文本。
	// 没有钩子时，推理按块实时流式传输——常见情况不能丢失实时的"思考中..."显示。
	transformReasoning := a.hooks != nil && a.hooks.HasPostLLMCall()

	// ===== 累积器 =====
	var text, reasoning strings.Builder // 回答文本和推理文本的累积器
	var signature string                // 提供者签发的推理证明（Anthropic thinking 模式）
	var calls []provider.ToolCall       // 收集的完整工具调用
	var usage *provider.Usage           // token 使用量数据
	var partialToolStarted bool         // 是否有工具调用已开始流式传输

	// finishReasoning 完成推理文本的处理：
	// 如果有 PostLLMCall 钩子，转换推理文本并发射；
	// 如果有签名（Anthropic thinking），存储原始文本以便重放验证。
	// 返回值：stored（存储的文本）、display（显示的文本）
	finishReasoning := func() (stored, display string) {
		original := reasoning.String()
		display = original
		if transformReasoning && original != "" {
			// 通过钩子转换推理文本（如翻译、摘要等）
			display = a.hooks.PostLLMCall(ctx, original, turn)
			if display != "" {
				a.sink.Emit(event.Event{Kind: event.Reasoning, Text: display})
			}
		}
		stored = display
		if signature != "" {
			// 有签名时存储原始文本，因为签名验证要求原始内容不变
			stored = original
		}
		return stored, display
	}

	// ===== 流式 Chunk 处理循环 =====
	// 这是流式处理的核心：逐块消费模型输出，实时发射事件
	for chunk := range ch {
		switch chunk.Type {
		case provider.ChunkReasoning:
			// 推理增量：累积到 reasoning 缓冲区
			reasoning.WriteString(chunk.Text)
			if chunk.Signature != "" {
				signature = chunk.Signature // 捕获提供者的签名
			}
			// 没有 PostLLMCall 钩子时，实时发射推理增量
			if chunk.Text != "" && !transformReasoning {
				a.sink.Emit(event.Event{Kind: event.Reasoning, Text: chunk.Text})
			}

		case provider.ChunkText:
			// 回答文本增量：累积并实时发射
			text.WriteString(chunk.Text)
			a.sink.Emit(event.Event{Kind: event.Text, Text: chunk.Text})

		case provider.ChunkToolCallStart:
			// 工具调用开始：标记部分工具已开始，发射早期的 ToolDispatch 事件
			partialToolStarted = true
			// 在调用开始时立即显示工具卡片——在其（可能很大的）参数流式传输完成之前——
			// 这样用户看到的是正在工作而非卡顿。
			// executeBatch 在调用完成后发射完整的分派（带参数）；前端通过 ID 合并。
			if tc := chunk.ToolCall; tc != nil {
				a.sink.Emit(event.Event{Kind: event.ToolDispatch, Tool: event.Tool{
					ID: tc.ID, Name: tc.Name, ReadOnly: a.toolReadOnly(tc.Name), Partial: true,
				}})
			}

		case provider.ChunkToolCall:
			// 完整的工具调用：收集到 calls 列表中
			partialToolStarted = true
			calls = append(calls, *chunk.ToolCall)

		case provider.ChunkUsage:
			// token 使用量：记录并更新缓存统计
			usage = chunk.Usage
			a.lastUsage.Store(chunk.Usage)
			a.sessCacheHit.Add(int64(chunk.Usage.CacheHitTokens))
			a.sessCacheMiss.Add(int64(chunk.Usage.CacheMissTokens))

		case provider.ChunkError:
			// 错误处理：区分可恢复的流中断和不可恢复的错误
			if provider.IsStreamInterrupted(chunk.Err) {
				// 流中断：完成推理处理，返回已收集的内容，标记为可中断
				stored, _ := finishReasoning()
				return text.String(), stored, signature, calls, usage, true, partialToolStarted, chunk.Err
			}
			// 不可恢复的错误：直接返回
			return "", "", "", nil, nil, false, false, chunk.Err
		}
	}
	// ===== 流完成后的处理 =====

	// 如果有 PostLLMCall 钩子，上面的实时流被抑制了；
	// 现在转换完整的推理文本并一次性发射，使 Sink 永远看不到未翻译的文本。
	// 没有钩子时跳过此步骤——逐块事件已经发射过了。
	stored, display := finishReasoning()

	// 存储转换后的推理文本——除了当提供者签名将其固定到原始文本时
	// （Anthropic 扩展思维模式）。那个签名的思维块会在下一个工具调用回合
	// 原样重放；在原始签名下重新上传转换后的文本会被拒绝，
	// 因此存储原始文本，而用户仍然看到转换后的版本实时显示。
	// finishReasoning 已经在上面做出了这个选择。

	// 关闭文本流：Sink 现在可以将流式传输的原始文本重新渲染为带样式的 Markdown。
	// 推理文本也一起传递，以便 Sink 在需要时拥有完整的推理链。
	if text.Len() > 0 || display != "" {
		a.sink.Emit(event.Event{Kind: event.Message, Text: StripGoalMarkers(text.String()), Reasoning: display})
	}
	return text.String(), stored, signature, calls, usage, false, false, nil
}

// capturePrefixShape 捕获当前请求的可缓存前缀形状，用于缓存流失归因。
func (a *Agent) capturePrefixShape(schemas []provider.ToolSchema) PrefixShape {
	return CaptureShape(a.systemPrompt(), schemas, a.session.RewriteVersion())
}

// systemPrompt 从会话消息中提取系统提示文本。
// 如果有多个系统消息，用换行符连接。
func (a *Agent) systemPrompt() string {
	var b strings.Builder
	for _, m := range a.session.Messages {
		if m.Role != provider.RoleSystem {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(m.Content)
	}
	return b.String()
}

// executeBatch 分派一个模型回合中的所有工具调用。
//
// 执行策略：
//  1. 首先为每个调用发射 ToolDispatch 事件（按调用顺序），以便前端按时间线展示
//  2. 将连续的已知只读调用分组为可并行批次（最多 8 个 goroutine）
//  3. 未知工具和写入工具作为单调用串行段运行，保持写入/读取顺序与提供者一致
//  4. 所有调用完成后，按调用顺序发射 ToolResult 事件（即使执行是并行的）
//
// 参数：
//   - ctx: 上下文，用于取消控制
//   - calls: 工具调用列表
//
// 返回值：每个调用的结果字符串数组（与 calls 一一对应）
func (a *Agent) executeBatch(ctx context.Context, calls []provider.ToolCall) []string {
	// ===== 第一阶段：发射所有 ToolDispatch 事件 =====
	// 按调用顺序预先发射，以便前端可以按时间线展示
	for _, c := range calls {
		t, ok := a.tools.Get(c.Name)
		ev := event.Tool{ID: c.ID, Name: c.Name, Args: c.Arguments, ReadOnly: ok && t.ReadOnly()}
		ev.FileDiff = event.FileDiff{Diff: c.Diff, Added: c.Added, Removed: c.Removed}

		// 如果提供者没有附带差异预览，尝试通过工具的 PreviewChange 接口生成
		if ok && ev.Diff == "" && ev.Added == 0 && ev.Removed == 0 {
			if ch, ok := tool.PreviewChange(t, json.RawMessage(c.Arguments)); ok {
				ev.FileDiff = event.FileDiff{Diff: ch.Diff, Added: ch.Added, Removed: ch.Removed}
			}
		}

		// 解析子代理配置文件（task/skill 调用时）
		if ok {
			if pr, ok := t.(interface {
				ResolveProfile(json.RawMessage) *event.Profile
			}); ok {
				ev.Profile = pr.ResolveProfile(json.RawMessage(c.Arguments))
			}
		}
		a.sink.Emit(event.Event{Kind: event.ToolDispatch, Tool: ev})
	}

	// ===== 第二阶段：执行工具调用 =====
	results := make([]string, len(calls))
	outcomes := make([]toolOutcome, len(calls))
	durations := make([]int64, len(calls))

	// run 是单个工具调用的执行包装器，记录执行时间和结果
	run := func(i int) {
		start := time.Now()
		outcomes[i] = a.executeOne(ctx, calls[i])
		durations[i] = time.Since(start).Milliseconds()
		results[i] = outcomes[i].output
	}

	cancelled := false
	// markCancelled 将从 start 开始的所有未执行调用标记为已取消
	markCancelled := func(start int) {
		errMsg := context.Canceled.Error()
		if err := ctx.Err(); err != nil {
			errMsg = err.Error()
		}
		output := "cancelled: context cancelled before execution"
		for j := start; j < len(calls); j++ {
			results[j] = output
			outcomes[j] = toolOutcome{output: output, errMsg: errMsg}
		}
		cancelled = true
	}

	// 按批次执行：只读工具可并行（最多 8 个），写入工具串行
	for _, batch := range partitionToolCalls(a.tools, calls) {
		// 在开始新批次前检查上下文是否已取消
		if ctx.Err() != nil {
			markCancelled(batch.start)
			break
		}

		if batch.parallel && batch.end-batch.start > 1 {
			// 并行批次：多个只读工具同时执行
			ranUntil := runParallel(ctx, batch.start, batch.end, run)
			// 并行执行完成后，再次检查上下文是否已取消
			if ctx.Err() != nil {
				markCancelled(ranUntil)
				break
			}
			continue
		}

		// 串行批次：逐个执行
		for i := batch.start; i < batch.end; i++ {
			// 在执行下一个工具前检查上下文是否已取消
			if ctx.Err() != nil {
				markCancelled(i)
				break
			}
			run(i)
			// 每个工具执行后也检查上下文是否已取消
			if ctx.Err() != nil {
				markCancelled(i + 1)
				break
			}
		}
		if cancelled {
			break
		}
	}

	// ===== 第三阶段：发射所有 ToolResult 事件 =====
	// 按调用顺序发射，保持事件发射的串行性
	for i, c := range calls {
		o := outcomes[i]
		t, ok := a.tools.Get(c.Name)
		a.sink.Emit(event.Event{Kind: event.ToolResult, Tool: event.Tool{
			ID:         c.ID,
			Name:       c.Name,
			Args:       c.Arguments,
			Output:     o.output,
			Err:        o.errMsg,
			ReadOnly:   ok && t.ReadOnly(),
			Truncated:  o.truncated,
			DurationMs: durations[i],
		}})
		// 如果输出被截断，发射额外的通知事件
		if o.truncated && o.truncMsg != "" {
			a.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: o.truncMsg})
		}
	}

	// 应用风暴断路器：检测重复失败的死循环
	if !cancelled {
		a.applyStormBreaker(calls, outcomes, results)
	}
	return results
}

// withPreviewFileDiffs 为工具调用列表中的写入工具生成文件差异预览。
// 如果提供者没有附带差异预览，尝试通过工具的 PreviewChange 接口生成。
// 返回带有填充了 Diff/Added/Removed 的工具调用列表。
func (a *Agent) withPreviewFileDiffs(calls []provider.ToolCall) []provider.ToolCall {
	if len(calls) == 0 {
		return calls
	}
	out := make([]provider.ToolCall, len(calls))
	copy(out, calls)
	for i := range out {
		if out[i].Diff != "" || out[i].Added != 0 || out[i].Removed != 0 {
			continue
		}
		t, ok := a.tools.Get(out[i].Name)
		if !ok {
			continue
		}
		if ch, ok := tool.PreviewChange(t, json.RawMessage(out[i].Arguments)); ok {
			out[i].Diff = ch.Diff
			out[i].Added = ch.Added
			out[i].Removed = ch.Removed
		}
	}
	return out
}

// toolCallBatch 表示一个工具调用批次。
// start 和 end 定义了在 calls 列表中的范围 [start, end)。
// parallel 标记该批次是否可以并行执行（只读工具）。
type toolCallBatch struct {
	start    int  // 批次的起始索引（包含）
	end      int  // 批次的结束索引（不包含）
	parallel bool // 是否可并行执行
}

// partitionToolCalls 在保持提供者顺序的同时，让连续的已知只读工具可以一起运行。
//
// 分区策略：
//   - 连续的只读工具 → 一个并行批次（parallel=true）
//   - 未知工具或写入工具 → 单调用串行批次
//   - complete_step 和 todo_write 虽然是只读的，但不参与并行运行：
//     它们读取回合的证据账本，因此每个先前调用的回执必须在它们运行之前被记录
//
// 这种设计确保写入/读取顺序与提供者一致，同时最大化只读工具的并行度。
func partitionToolCalls(r *tool.Registry, calls []provider.ToolCall) []toolCallBatch {
	var batches []toolCallBatch
	for i := 0; i < len(calls); {
		if parallelisable(r, calls[i].Name) {
			start := i
			i++
			for i < len(calls) && parallelisable(r, calls[i].Name) {
				i++
			}
			batches = append(batches, toolCallBatch{start: start, end: i, parallel: true})
			continue
		}
		batches = append(batches, toolCallBatch{start: i, end: i + 1})
		i++
	}
	return batches
}

// parallelisable 判断一个工具调用是否可以参与并行执行。
//
// 并行条件：
//   - 工具必须存在于注册表中
//   - 工具必须是只读的（ReadOnly() 返回 true）
//   - 工具不能是 complete_step 或 todo_write（它们需要读取证据账本，
//     必须在所有先前调用完成后才能运行）
func parallelisable(r *tool.Registry, name string) bool {
	if name == "complete_step" || name == "todo_write" {
		return false
	}
	t, ok := r.Get(name)
	return ok && t.ReadOnly()
}

// runParallel 并行执行从 start 到 end 的工具调用，最多使用 8 个 goroutine。
//
// 使用信号量（semaphore）模式控制并发度：
//   - 创建容量为 maxParallel 的 channel 作为信号量
//   - 每个 goroutine 启动前获取信号量，完成后释放
//   - 支持上下文取消：如果 ctx 被取消，停止启动新的 goroutine
//
// 返回值：实际执行到的索引位置（用于取消时标记未执行的调用）。
func runParallel(ctx context.Context, start, end int, run func(int)) int {
	const maxParallel = 8                              // 最大并行度
	sem := make(chan struct{}, maxParallel)             // 信号量 channel
	var wg sync.WaitGroup                              // 等待组，用于等待所有 goroutine 完成
	ranUntil := start                                  // 记录实际执行到的位置
launch:
	for i := start; i < end; i++ {
		if ctx.Err() != nil {
			break
		}
		select {
		case sem <- struct{}{}: // 获取信号量
		case <-ctx.Done(): // 上下文已取消
			break launch
		}
		if ctx.Err() != nil {
			<-sem // 释放信号量
			break
		}
		i := i // 捕获循环变量
		wg.Add(1)
		ranUntil = i + 1
		go func() {
			defer wg.Done()              // 完成时减少等待计数
			defer func() { <-sem }()     // 完成时释放信号量
			run(i)                       // 执行工具调用
		}()
	}
	wg.Wait() // 等待所有 goroutine 完成
	return ranUntil
}

// stormBreakThreshold 是相同工具以相同方式连续失败多少次后，
// 循环停止回显原始错误并返回改变方法的指令。
// 两次自然的自我纠正是健康的；第三次相同的失败是死亡螺旋——
// 主要情况是工具调用的参数在输出 token 上限处被截断，
// 然后模型重新发出（重新措辞但仍然过长），再次以相同方式截断。
const stormBreakThreshold = 3

// repeatSuccessBreakThreshold 是代理在拒绝同一用户回合中的另一次复制之前
// 允许的相同写入类成功次数。两次给模型留出自然自我纠正的空间；
// 第三次重复通常是无操作/写入循环，应重定向到不同的工具或最终答案。
const repeatSuccessBreakThreshold = 2

// applyStormBreaker 检测连续以相同方式失败的回合，并在超过阈值后将
// 面向模型的结果（results[0]）重写为改变方法的指令。
//
// 风暴断路器的工作原理：
//   - 基于每个调用的 (tool, error) 而非 args 来键控——因为卡住的模型会对参数
//     进行表面修改（重新措辞、重新排序）而每次都以相同方式失败
//   - 一个回合是"固执"候选者，仅当它的每个调用都出错且没有一个被计划模式/权限阻止
//     （那些携带了模型可以自行处理的清晰、独特的消息）
//   - 任何成功、任何阻止或不同的批次形状都是多样化的工作，因此重置计数器
//
// 这涵盖了单调用螺旋和重复的多调用批次两种情况。
// 硬性的 maxSteps 防护仍然是最终的安全网；
// 这个机制只是防止循环在相同失败上消耗整个预算。
func (a *Agent) applyStormBreaker(calls []provider.ToolCall, outcomes []toolOutcome, results []string) {
	sig, ok := batchStormSignature(calls, outcomes)
	if !ok {
		a.stormSig, a.stormCount = "", 0
		return
	}
	if sig != a.stormSig {
		a.stormSig, a.stormCount = sig, 1
		return
	}
	a.stormCount++
	if a.stormCount < stormBreakThreshold {
		return
	}
	subject := fmt.Sprintf("%q", calls[0].Name)
	short := calls[0].Name
	if len(calls) > 1 {
		subject = fmt.Sprintf("this batch of %d tool calls", len(calls))
		short = fmt.Sprintf("a batch of %d calls", len(calls))
	}
	results[0] = outcomes[0].output + fmt.Sprintf(
		"\n\n[loop guard] %s has now failed %d times in a row with the same error. Re-sending it — even with the wording changed — will not help: the calls keep failing the same way. Change approach: if an argument is being truncated, write less in one call and split the work into several smaller calls; otherwise fix the arguments, use a different tool, or explain the blocker in your final answer.",
		subject, a.stormCount)
	a.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: fmt.Sprintf(
		"loop guard: %s failed %d× the same way — nudging the model to change approach",
		short, a.stormCount)})
}

// batchStormSignature 返回每回合的"固执"签名——每个调用的 (name, error) 按顺序排列。
//
// 返回值：
//   - string: 签名字符串（空表示无签名）
//   - bool: ok=true 仅当每个调用都出错且没有一个被阻止。
//     ok=false（任何成功或阻止）意味着回合取得了多样化的工作，调用者应重置计数器。
//
// 设计决策：基于 error 而非 args 进行键控——因为卡住的模型会对参数进行表面修改
// （重新措辞、重新排序）而每次都以相同方式失败，因此基于 args 的匹配会错过循环。
func batchStormSignature(calls []provider.ToolCall, outcomes []toolOutcome) (string, bool) {
	if len(calls) == 0 {
		return "", false
	}
	var sb strings.Builder
	for i := range calls {
		if outcomes[i].errMsg == "" || outcomes[i].blocked {
			return "", false
		}
		sb.WriteString(calls[i].Name)
		sb.WriteByte(0)
		sb.WriteString(outcomes[i].errMsg)
		sb.WriteByte(0)
	}
	return sb.String(), true
}

// toolOutcome 是单个工具调用的结果，分为面向模型的输出和面向显示的通知部分。
//
// 字段说明：
//   - output: 面向模型的结果文本（成功时为正常输出，失败时包含错误详情）
//   - blocked: 是否被阻止（计划模式或权限拒绝）
//   - errMsg: 短的失败原因（成功时为空）——被拒绝的调用、未知工具或执行错误，
//     以便 Sink 将结果渲染为失败（"⊘ name <errMsg>" / 红色卡片）而非 OK
//   - truncated: 输出是否被截断（head+tail）
//   - truncMsg: 截断通知消息（未截断时为空）
// (without the "· " prefix) when the output was head+tailed.
type toolOutcome struct {
	output    string
	blocked   bool
	errMsg    string
	truncated bool
	truncMsg  string
}

// executeOne 运行单个工具调用。
//
// 执行流程：
//  1. 验证工具是否存在于注册表中
//  2. 检查重复成功阻止（防止模型反复执行相同的成功写入）
//  3. 检查计划模式限制（只读模式下拒绝写入工具）
//  4. 检查权限门控（Gate）
//  5. 触发 PreToolUse 钩子（可能阻止调用）
//  6. 对写入工具进行文件快照（用于检查点/回滚）
//  7. 构建工具调用上下文（包含 Sink、Asker、证据账本等）
//  8. 执行工具的 Execute 方法
//  9. 记录证据回执
//  10. 触发 PostToolUse 钩子
//  11. 处理结果（截断、错误处理等）
//
// 此方法对事件发射器是纯的——调用者负责发射 ToolDispatch/ToolResult——
// 因此可以从并行的 goroutine 中安全调用。
func (a *Agent) executeOne(ctx context.Context, call provider.ToolCall) toolOutcome {
	t, ok := a.tools.Get(call.Name)
	if !ok {
		return toolOutcome{
			output: fmt.Sprintf("error: unknown tool %q", call.Name),
			errMsg: fmt.Sprintf("unknown tool %q", call.Name),
		}
	}
	if out, blocked := a.repeatedSuccessBlock(call, t); blocked {
		return toolOutcome{
			output:  out,
			blocked: true,
			errMsg:  "blocked by loop guard",
		}
	}
	if a.planMode.Load() {
		if blocked, msg := a.planModeBlocked(call.Name, t.ReadOnly(), json.RawMessage(call.Arguments)); blocked {
			return toolOutcome{
				output:  msg,
				blocked: true,
				errMsg:  "blocked: plan mode is read-only",
			}
		}
	}
	if a.gate != nil {
		allow, reason, err := a.gate.Check(ctx, call.Name, json.RawMessage(call.Arguments), t.ReadOnly())
		if err != nil {
			return toolOutcome{
				output:  fmt.Sprintf("blocked: %s (%v)", reason, err),
				blocked: true,
				errMsg:  fmt.Sprintf("blocked: %v", err),
			}
		}
		if !allow {
			return toolOutcome{
				output:  "blocked: " + reason,
				blocked: true,
				errMsg:  "blocked by permission policy",
			}
		}
	}
	// PreToolUse hooks run after permission is granted but before the call: a
	// gating hook (exit 2) refuses it, surfaced to the model like a gate denial.
	if a.hooks != nil {
		if block, msg := a.hooks.PreToolUse(ctx, call.Name, json.RawMessage(call.Arguments)); block {
			if msg == "" {
				msg = "blocked by a PreToolUse hook"
			}
			return toolOutcome{
				output:  "blocked: " + msg,
				blocked: true,
				errMsg:  "blocked by PreToolUse hook",
			}
		}
	}
	// Checkpoint the file this writer is about to change, so the turn can be
	// rewound. Fires after all gating (the edit is cleared to run) and only for
	// tools that can describe their change; a Preview error means the edit will
	// likely fail anyway, so we skip rather than snapshot a stale state.
	if a.onPreEdit != nil && !t.ReadOnly() {
		if pv, ok := t.(tool.Previewer); ok {
			if change, perr := pv.Preview(json.RawMessage(call.Arguments)); perr == nil {
				a.onPreEdit(change)
			}
		}
	}
	cctx := withCallContext(ctx, call.ID, a.sink, a.asker, a.planMode.Load())
	if a.evidence != nil {
		cctx = evidence.WithLedger(cctx, a.evidence)
		cctx = evidence.WithSessionMessages(cctx, a.session.Snapshot())
	}
	if len(a.projectChecks) > 0 {
		cctx = instruction.WithChecks(cctx, a.projectChecks)
	}
	if a.jobs != nil {
		cctx = jobs.WithManager(cctx, a.jobs)
	}
	if v := a.reasoningLanguage.Load(); v != nil {
		if lang, ok := v.(string); ok {
			cctx = WithReasoningLanguagePreference(cctx, lang)
		}
	}
	if a.memQueue != nil {
		cctx = memory.WithQueue(cctx, a.memQueue)
	}
	callID := call.ID
	cctx = tool.WithProgress(cctx, func(chunk string) {
		a.sink.Emit(event.Event{Kind: event.ToolProgress, Tool: event.Tool{ID: callID, Output: chunk}})
	})
	result, err := t.Execute(cctx, json.RawMessage(call.Arguments))
	if a.evidence != nil {
		if call.Name == "complete_step" {
			if err == nil {
				rec := evidence.ReceiptFromToolCall(call.Name, json.RawMessage(call.Arguments), true, t.ReadOnly())
				a.evidence.Record(rec)
				a.advanceCanonicalTodo(rec.Step)
			}
		} else {
			rec := evidence.ReceiptFromToolCall(call.Name, json.RawMessage(call.Arguments), err == nil, t.ReadOnly())
			a.evidence.Record(rec)
			if err == nil && call.Name == "todo_write" {
				a.setTodoState(rec.Todos)
			}
		}
	}
	// PostToolUse hooks observe the result (they can't block); fired whether the
	// call succeeded or errored, since the tool did run.
	if a.hooks != nil {
		a.hooks.PostToolUse(ctx, call.Name, json.RawMessage(call.Arguments), result)
	}
	if err != nil {
		detail := result
		// Malformed-args failures are a transient model JSON glitch (e.g. options
		// written as ["a":"b"] → "invalid character ':' after array element"). The
		// args can't be safely re-parsed, but echoing the tool's schema makes the
		// retry land valid instead of repeating the same broken shape.
		if !json.Valid([]byte(call.Arguments)) {
			detail = strings.TrimRight(detail, "\n") + "\nThe arguments were not valid JSON. Re-emit them exactly per this schema:\n" + string(t.Schema())
		}
		body, truncMsg := truncateToolOutput(fmt.Sprintf("error: %v\n%s", err, detail))
		return toolOutcome{output: body, errMsg: firstLine(err.Error()), truncated: truncMsg != "", truncMsg: truncMsg}
	}
	a.recordRepeatSuccess(call, t)
	// A foreground `task` sub-agent just finished — its result is the final answer.
	// (A backgrounded one returns a "Started…" string and stops later in a job, so
	// it doesn't fire here.) SubagentStop lets a hook react to delegated work.
	if a.hooks != nil && call.Name == "task" && !isBackgroundTaskCall(call.Arguments) {
		a.hooks.SubagentStop(ctx, result)
	}
	body, truncMsg := truncateToolOutput(result)
	return toolOutcome{output: body, truncated: truncMsg != "", truncMsg: truncMsg}
}

// planModeBlocked 检查工具调用是否在计划模式下被阻止。
//
// 检查顺序：
//  1. 如果工具是只读的，允许
//  2. 如果工具在 planModeDeniedTools 列表中，拒绝
//  3. 如果工具在 planModeAllowedTools 列表中，允许（白名单豁免）
//  4. 如果是 bash 工具，检查命令是否安全（只读前缀、无危险元字符）
//  5. 其他写入工具，拒绝
func (a *Agent) planModeBlocked(toolName string, readOnly bool, args json.RawMessage) (blocked bool, message string) {
	if readOnly {
		return false, ""
	}
	if planModeDeniedTools[toolName] {
		return true, fmt.Sprintf("blocked: %q is not available in plan mode. Keep exploring with read-only tools — the user will be asked to approve the plan before any changes are made.", toolName)
	}
	if a.planModeAllowedTools != nil && a.planModeAllowedTools[toolName] {
		return false, ""
	}
	if toolName == "bash" {
		if blocked, msg := planModeBashBlocked(args); blocked {
			return true, msg
		}
		return false, ""
	}
	return true, fmt.Sprintf("blocked: %q is a writer tool and plan mode is read-only. Keep exploring with read-only tools, then write your plan as your reply — the user will be asked to approve it before any changes are made.", toolName)
}

// planModeBashBlocked 检查 bash 命令在计划模式下是否被阻止。
//
// 检查策略：
//  1. 拒绝包含 shell 元字符的命令（链接、管道、重定向、替换）
//  2. 检查命令前缀是否在安全只读白名单中
//  3. 即使命令前缀安全，也检查参数是否包含写入操作（如 find -exec, go -fix）
func planModeBashBlocked(args json.RawMessage) (bool, string) {
	var p struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &p); err != nil || p.Command == "" {
		return false, ""
	}
	cmd := strings.TrimSpace(p.Command)
	lower := strings.ToLower(cmd)

	// Reject commands containing shell metacharacters — chaining, piping,
	// redirection, or command substitution can introduce side effects after
	// an otherwise safe prefix.
	for _, mc := range planModeBashMetachars {
		if strings.Contains(lower, mc) {
			return true, fmt.Sprintf("blocked: bash command in plan mode must not contain shell operators (%q). Use separate calls for chained commands.", mc)
		}
	}

	// Check the command prefix against the safe read-only whitelist. Require a
	// shell-argument boundary after the match to avoid prefix collisions.
	for _, safe := range planModeSafeBashCommands {
		if !planModeBashMatchesSafePrefix(lower, safe) {
			continue
		}
		if arg := planModeUnsafeSafeCommandArg(cmd, safe); arg != "" {
			return true, fmt.Sprintf("blocked: bash command in plan mode uses a write-capable argument (%q). Use a read-only command while planning.", arg)
		}
		return false, ""
	}

	return true, fmt.Sprintf("blocked: bash commands in plan mode must be read-only. %q is not in the safe command list. Use read-only tools for exploration, then exit plan mode to run this command.", cmd)
}

// planModeBashMatchesSafePrefix 检查小写命令是否以安全前缀开头，
// 并要求前缀之后是 shell 参数边界（空白字符或字符串结尾）。
// 这确保 "echop" 不会匹配 "echo"。
func planModeBashMatchesSafePrefix(lower, safe string) bool {
	if !strings.HasPrefix(lower, safe) {
		return false
	}
	if len(lower) == len(safe) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(lower[len(safe):])
	return unicode.IsSpace(r)
}

// planModeUnsafeSafeCommandArg 检查安全命令的参数中是否包含不安全的写入操作参数。
// 例如：
//   - find 命令的 -delete, -exec, -execdir 等参数
//   - go list/vet 的 -fix, -mod, -toolexec 等参数
//   - git 命令的 --output, --ext-diff 等参数
// 返回第一个不安全参数，如果没有则返回空字符串。
func planModeUnsafeSafeCommandArg(cmd, safe string) string {
	fields := strings.Fields(cmd)
	base := strings.Fields(safe)
	if len(fields) <= len(base) {
		return ""
	}
	args := fields[len(base):]
	lowerArgs := make([]string, len(args))
	for i, arg := range args {
		lowerArgs[i] = strings.ToLower(arg)
	}
	if strings.HasPrefix(safe, "git ") {
		for _, arg := range lowerArgs {
			if arg == "--output" || strings.HasPrefix(arg, "--output=") || arg == "--ext-diff" {
				return arg
			}
		}
	}
	switch safe {
	case "git grep":
		for i, arg := range args {
			lowerArg := lowerArgs[i]
			if arg == "-O" || strings.HasPrefix(arg, "-O") || strings.HasPrefix(lowerArg, "--open-files-in-pager") {
				return arg
			}
		}
	case "find":
		for _, arg := range lowerArgs {
			if planModeFindWriteArgs[arg] {
				return arg
			}
		}
	case "go list", "go vet":
		for _, arg := range lowerArgs {
			if planModeGoWriteOrExecArgs[arg] || strings.HasPrefix(arg, "-mod=mod") || strings.HasPrefix(arg, "-modfile=") || strings.HasPrefix(arg, "-toolexec=") || strings.HasPrefix(arg, "-vettool=") {
				return arg
			}
		}
	}
	return ""
}

// repeatedSuccessBlock 检查写入类工具调用是否已在本回合中成功执行了多次。
// 如果是，阻止调用并返回阻止消息，防止模型反复执行相同的成功写入。
// 这是风暴断路器的互补机制：风暴断路器关注重复失败，此机制关注重复成功。
func (a *Agent) repeatedSuccessBlock(call provider.ToolCall, t tool.Tool) (string, bool) {
	sig, ok := repeatSuccessSignature(call, t)
	if !ok || a.repeatSuccessCounts == nil {
		return "", false
	}
	count := a.repeatSuccessCounts[sig]
	if count < repeatSuccessBreakThreshold {
		return "", false
	}
	return fmt.Sprintf(
		"blocked: [loop guard] %q has already succeeded %d times with the same write-like arguments in this user turn. Re-running it is unlikely to help and may burn tokens or repeat file writes. Change approach: use edit_file or multi_edit for file changes, verify with a read/test command, or explain the blocker in your final answer.",
		call.Name, count), true
}

// recordRepeatSuccess 记录写入类工具调用的成功执行，用于重复成功检测。
func (a *Agent) recordRepeatSuccess(call provider.ToolCall, t tool.Tool) {
	sig, ok := repeatSuccessSignature(call, t)
	if !ok {
		return
	}
	if a.repeatSuccessCounts == nil {
		a.repeatSuccessCounts = make(map[string]int)
	}
	a.repeatSuccessCounts[sig]++
}

// repeatSuccessSignature 为写入类工具调用生成用于重复检测的签名。
// 只读工具返回 ok=false（不跟踪）；写入工具返回工具名+规范化参数的签名。
// bash 命令需要特殊处理：只有文件写入命令才跟踪，后台命令不跟踪。
func repeatSuccessSignature(call provider.ToolCall, t tool.Tool) (string, bool) {
	if t.ReadOnly() {
		return "", false
	}
	switch call.Name {
	case "write_file", "edit_file", "multi_edit", "move_file", "notebook_edit":
		return call.Name + "\x00" + canonicalToolArgs(call.Arguments), true
	case "bash":
		var p struct {
			Command         string `json:"command"`
			RunInBackground bool   `json:"run_in_background"`
		}
		if err := json.Unmarshal([]byte(call.Arguments), &p); err != nil {
			return "", false
		}
		if p.RunInBackground || !isShellFileWriteCommand(p.Command) {
			return "", false
		}
		return "bash\x00" + normalizeShellCommand(p.Command), true
	default:
		return "", false
	}
}

// canonicalToolArgs 将工具参数规范化为紧凑的 JSON 字符串。
// 先解析为任意值，再重新序列化，确保语义相同但格式一致的参数产生相同的签名。
func canonicalToolArgs(raw string) string {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return strings.TrimSpace(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return strings.TrimSpace(raw)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, b); err != nil {
		return string(b)
	}
	return compact.String()
}

// normalizeShellCommand 将 shell 命令规范化为单空格分隔的形式。
// 这确保了语义相同但空白不同的命令产生相同的签名。
func normalizeShellCommand(command string) string {
	return strings.Join(strings.Fields(command), " ")
}

// isShellFileWriteCommand 检查 shell 命令是否是文件写入命令。
// 用于重复成功检测：只有文件写入命令才需要跟踪，普通命令不跟踪。
// 检测的模式包括：
//   - Python 的 open() 写入模式
//   - PowerShell 的 Set-Content/Add-Content/Out-File
//   - sed -i / perl -pi 原地编辑
//   - shell 重定向（>）
func isShellFileWriteCommand(command string) bool {
	lower := strings.ToLower(command)
	switch {
	case shellPythonOpenWrites(lower):
		return true
	case strings.Contains(lower, "set-content") || strings.Contains(lower, "add-content") || strings.Contains(lower, "out-file"):
		return true
	case strings.Contains(lower, "sed -i") || strings.Contains(lower, "perl -pi"):
		return true
	case hasShellWriteRedirect(command):
		return true
	default:
		return false
	}
}

// shellPythonOpenWrites 检查 shell 命令中是否包含 Python 的 open() 文件写入操作。
// 检测模式：
//   - open(...).write(...) 调用
//   - open(..., 'w'/'a'/'x', ...) 写入模式
//   - open(..., mode='w'/'a'/'x', ...) 关键字参数形式
func shellPythonOpenWrites(lower string) bool {
	if !strings.Contains(lower, "open(") {
		return false
	}
	if strings.Contains(lower, ".write(") {
		return true
	}
	for _, marker := range []string{", 'w", `, "w`, ", 'a", `, "a`, ", 'x", `, "x`, "mode='w", `mode="w`, "mode='a", `mode="a`, "mode='x", `mode="x`} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// hasShellWriteRedirect 检查 shell 命令中是否包含输出重定向（>）。
// 正确处理引号内的 > 字符（不视为重定向）。
// 注意：2>（标准错误重定向）不被视为文件写入。
func hasShellWriteRedirect(command string) bool {
	var quote rune
	var prev rune
	for _, r := range command {
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			prev = r
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			prev = r
			continue
		}
		if r == '>' {
			if prev == '2' {
				prev = r
				continue
			}
			return true
		}
		prev = r
	}
	return false
}

// isBackgroundTaskCall 报告 `task` 调用是否设置了 run_in_background，
// 以便火-and-return 的分派不会被误认为是已停止的子代理。
func isBackgroundTaskCall(args string) bool {
	var p struct {
		RunInBackground bool `json:"run_in_background"`
	}
	_ = json.Unmarshal([]byte(args), &p)
	return p.RunInBackground
}

// toolReadOnly 按名称报告工具的 ReadOnly 分类（未知工具返回 false），
// 用于在早期的 ToolDispatch 事件中标记工具类型。
func (a *Agent) toolReadOnly(name string) bool {
	t, ok := a.tools.Get(name)
	return ok && t.ReadOnly()
}

// firstLine 返回 s 中第一个换行符之前的部分——用于显示的单行失败摘要，
// 而完整的错误保留在面向模型的输出中。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// truncateToolOutput 当 s 超过 maxToolOutputBytes 时进行 head+tail 截断，
// 在 rune 边界上切片以确保不会拆分多字节字符。
//
// 截断策略：保留头部和尾部各一半的字节预算，中间部分用省略标记替换。
// 这样既保留了输出的开头（通常是最重要的信息）和结尾（通常是总结），
// 又防止了过大的输出耗尽模型的上下文窗口。
//
// 返回值：
//   - string: 可能被修剪的文本内容
//   - string: 发生截断时的单行用户通知（未截断时为空）
func truncateToolOutput(s string) (string, string) {
	if len(s) <= maxToolOutputBytes {
		return s, ""
	}
	keep := maxToolOutputBytes / 2
	head := snapToRuneBoundary(s, 0, keep)
	tail := snapToRuneBoundary(s, len(s)-keep, len(s))
	omitted := len(s) - len(head) - len(tail)
	notice := fmt.Sprintf("tool output truncated: %d of %d bytes elided", omitted, len(s))
	body := head + fmt.Sprintf("\n\n…[truncated %d of %d bytes — rerun with narrower args to see the middle]…\n\n", omitted, len(s)) + tail
	return body, notice
}

// snapToRuneBoundary 返回 s[lo:hi]，将边界向外调整直到两个边界都落在 rune 起始位置。
// 这确保了切片操作不会拆分多字节的 Unicode 字符。
func snapToRuneBoundary(s string, lo, hi int) string {
	for lo > 0 && !utf8.RuneStart(s[lo]) {
		lo--
	}
	for hi < len(s) && !utf8.RuneStart(s[hi]) {
		hi++
	}
	return s[lo:hi]
}

// finishReasonMessage 将异常的 finish_reason 映射为单行警告消息。
//
// 对于正常的终止（"stop", "tool_calls"）和 nil 的 usage 返回 ok=false。
// Sink 负责渲染消息；"! " 前缀是展示层的约定。
//
// 异常终止类型：
//   - "length": 输出被截断，达到了最大输出 token 限制
//   - "content_filter": 输出被内容过滤器阻止
//   - "repetition_truncation": 检测到模型重复，输出被截断
func finishReasonMessage(u *provider.Usage) (string, bool) {
	if u == nil {
		return "", false
	}
	switch u.FinishReason {
	case "length":
		return "response truncated: hit max output tokens", true
	case "content_filter":
		return "response blocked by content filter", true
	case "repetition_truncation":
		return "response truncated: model repetition detected", true
	default:
		return "", false
	}
}
