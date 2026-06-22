// 文件：port.go
//
// 控制器驱动端口定义（接口隔离层）。
// 本文件定义了所有前端（CLI、Desktop、Bot、HTTP/ACP）与控制器交互的类型化接口面。
// 每个前端仅依赖它实际使用的子接口（接口隔离原则），例如 Bot 永远不需要看到
// 检查点或记忆方法。
//
// 子接口同时也是 Controller 自身的分解边界：接口先行定义，后续的协作者拆分
// 都以此为规格说明。*Controller 实现了所有子接口（编译时断言见文件末尾）。
// 完整的 SessionAPI 组合接口将在更多前端迁移后逐步完善。
package control

import (
	"context"

	"reasonix/internal/agent"
	"reasonix/internal/billing"
	"reasonix/internal/checkpoint"
	"reasonix/internal/command"
	"reasonix/internal/config"
	"reasonix/internal/event"
	"reasonix/internal/hook"
	"reasonix/internal/jobs"
	"reasonix/internal/memory"
	"reasonix/internal/plugin"
	"reasonix/internal/provider"
	"reasonix/internal/skill"
)

// Lifecycle 覆盖会话的标识与生命周期：创建新会话、恢复旧会话、清除会话、
// 定位当前活跃会话。前端通过这些方法管理会话的诞生和消亡。
type Lifecycle interface {
	// NewSession 快照当前对话，轮转到一个全新会话文件，并将执行器重置为
	// 携带相同系统提示的干净会话。会触发旧会话的 SessionEnd 和新会话的
	// SessionStart 钩子。
	NewSession() error
	// ClearSession 丢弃当前对话（不保留在历史中），然后轮转到干净会话。
	// 与 NewSession 不同，旧会话的内容不会被保留用于恢复。
	ClearSession() error
	// Resume 从已加载的会话转录中播种会话，并将活动文件固定到该路径，
	// 以便自动保存持续追加。
	Resume(s *agent.Session, path string)
	// SetSessionPath 固定自动保存的目标路径。当没有恢复路径适用时，
	// 调用者应生成一个新的会话文件路径。
	SetSessionPath(p string)
	// SessionPath 返回当前对话自动保存的目标文件路径。
	// 当持久化被禁用时返回空字符串。
	SessionPath() string
	// SessionDir 返回新会话文件存放的目录。空字符串表示禁用持久化。
	SessionDir() string
	// Label 返回人类可读的模型标签，例如 "deepseek-flash"。
	Label() string
	// WorkspaceRoot 返回此控制器会话的工作区根目录，
	// 文件写入器和 @ 引用都限定在此范围内。空表示无限定。
	WorkspaceRoot() string
	// Close 停止插件子进程并释放资源。曾经启动过的会话会触发 SessionEnd
	// 钩子，以便拆卸钩子可以运行。
	Close()
}

// TurnControl 覆盖驱动模型轮次和观察其运行状态的方法：各种提交/运行入口、
// 取消、转向引导和状态读取。这是前端与模型交互的核心通道。
type TurnControl interface {
	// Submit 是简单前端的统一入口：接收原始用户输入，自动完成斜杠命令分派、
	// @ 引用展开、计划模式组合等所有处理，并以事件形式发出所有输出。
	Submit(input string)
	// SubmitDisplay 运行一个轮次，同时记住用户界面的显示文本，以便在控制器
	// 侧组合扩展输入时用于会话回放。
	SubmitDisplay(display, input string)
	// SubmitHTTP 接受来自未认证 localhost HTTP 前端的输入。它故意省略了
	// 仅受信任 TUI 使用的 "!cmd" shell 快捷方式，并且仅通过控制器的工作区
	// 根目录解析文件引用。
	SubmitHTTP(input string)
	// SubmitUserTurn 启动一个普通模型轮次，不解释 shell 或斜杠命令。
	// 它仍然解析引用，因此调用者可以提交受信任的用户编写的提示文本，
	// 而不会扩展命令面。
	SubmitUserTurn(input, display string)
	// Send 启动一个使用未组合消息的轮次。控制器在异步轮次路径内应用
	// 自动计划、计划模式、记忆和后台作业框架，前端不会因分类器 I/O 而阻塞。
	Send(input string)
	// SendWithRaw 启动一个具有分离的模型输入和原始提示文本的轮次。
	// 原始提示仅用于自动计划评分；它故意排除已解析的 @ 引用有效载荷，
	// 以免引用的文件内容膨胀复杂度分数。
	SendWithRaw(input, raw string)
	// Run 同步执行一个轮次，返回代理的错误。用于无头的 `reasonix run`
	// 路径，其中 Sink 渲染到 stdout，调用者只需要退出状态。
	Run(ctx context.Context, input string) error
	// RunTurn 通过与交互式前端相同的生命周期同步执行一个前台轮次：
	// 自动计划、瞬态记忆/后台作业组合、检查点、钩子和计划审批。
	// 适用于需要阻塞请求/响应边界的传输层，如 ACP session/prompt。
	RunTurn(ctx context.Context, input string) error
	// RunShell 直接执行 shell 命令（绕过模型），并将输出作为
	// ToolDispatch/ToolProgress/ToolResult 事件流式传输。
	RunShell(command string)
	// Cancel 中止正在飞行的轮次。等待审批的 goroutine 通过已取消的
	// 上下文解除阻塞。
	Cancel()
	// Steer 在不中断正在飞行的请求的情况下，将中途引导信息排队。
	Steer(text string)
	// SteerConsumed 返回 true 表示在上次消费后转向队列为空。
	SteerConsumed() bool
	// Running 报告当前是否有轮次正在飞行中。
	Running() bool
	// CancelRequested 报告是否已对活动轮次请求了取消。
	CancelRequested() bool
	// RuntimeStatus 报告前台控制器拥有的活动工作的快照。
	RuntimeStatus() RuntimeStatus
	// Turn 返回当前轮次编号（第一次提交前为 0）。
	Turn() int
	// History 返回执行器的当前消息日志（用于重新填充恢复的前端视图）。
	History() []provider.Message
	// ToolResult 按 ID 在会话历史中查找工具调用，返回完整的参数和输出。
	// 当工具 ID 未找到时返回 nil。
	ToolResult(toolID string) *ToolResultData
}

// Approvals 覆盖工具审批和提问提示，以及运行时审批姿态（ask/auto/yolo）。
// 它与 approvalManager 的表面一一对应。
type Approvals interface {
	// Approve 按 ID 回答一个待处理的 ApprovalRequest：allow 运行该调用，
	// session 在会话剩余时间内记住授权，persist 将规则写入配置。
	// 未知/过期的 ID 被静默忽略。
	Approve(id string, allow, session, persist bool)
	// AnswerQuestion 按 ID 用用户的选择解决一个待处理的 AskRequest。
	AnswerQuestion(id string, answers []event.AskAnswer)
	// Ask 实现 agent.Asker 接口：发出 AskRequest 并阻塞直到
	// AnswerQuestion 回答或 ctx 被取消。promptMu 将其序列化，
	// 使得同时最多只有一个用户提示在飞行中。
	Ask(ctx context.Context, questions []event.AskQuestion) ([]event.AskAnswer, error)
	// ReplayPendingPrompts 重新发出每个当前阻塞运行循环的审批/提问事件。
	// 用于前端在原始事件之后重新连接或重新加载的场景。
	ReplayPendingPrompts()
	// PendingPrompt 报告当前轮次是否正在等待用户审批、计划审批、
	// 记忆审批或 ask 工具回答。
	PendingPrompt() bool
	// EnableInteractiveApproval 将执行器的门控替换为一个通过 ApprovalRequest
	// 事件将审批决策路由到前端的门控，并将控制器作为执行器的 Asker 接入。
	EnableInteractiveApproval()
	// ToolApprovalMode 返回规范化的运行时审批姿态。
	ToolApprovalMode() string
	// SetToolApprovalMode 更改权限门控工具的运行时审批姿态。
	// 它不回答业务提问或计划审批。
	SetToolApprovalMode(mode string)
	// AutoApproveTools 报告 YOLO/完全访问工具自动审批是否开启。
	AutoApproveTools() bool
	// SetAutoApproveTools 开启或关闭 YOLO 模式：开启时每个工具审批请求
	// 都自动允许。Ask 请求和计划审批仍然到达用户。拒绝规则仍然生效。
	SetAutoApproveTools(on bool)
	// Bypass 是 AutoApproveTools 的遗留名称，保留用于现有的桌面绑定。
	Bypass() bool
	// SetBypass 是 SetAutoApproveTools 的遗留名称。
	SetBypass(on bool)
	// SetMode 同时应用计划（只读）和工具自动审批，避免在组合模式切换后
	// 观察到半应用的门控。
	SetMode(plan, autoApproveTools bool)
}

// Goals 覆盖活动目标有限状态机和计划模式。目标 FSM 驱动自主多轮执行，
// 计划模式使模型进入只读研究阶段。
type Goals interface {
	// Goal 返回当前会话范围的活动目标文本。
	Goal() string
	// GoalStatus 返回目标的当前状态（running/complete/blocked/stopped）。
	GoalStatus() string
	// SetGoal 存储一个会话范围的活动目标。Compose 将其注入出站用户轮次中，
	// 而非系统提示或工具模式，因此不会干扰缓存稳定的前缀。
	SetGoal(goal string)
	// SetGoalWithResearchMode 设置目标并指定研究模式（自动/开启/关闭）。
	SetGoalWithResearchMode(goal string, researchMode GoalResearchMode)
	// GoalStrict 启用或禁用严格目标模式。在严格模式下，代理不能覆盖
	// 未完成的 todo 拦截——它必须实际完成或更新所有项目。
	GoalStrict(strict bool)
	// ClearGoal 清除活动目标。
	ClearGoal()
	// AutoStartResearchGoal 将强烈的长周期普通提示升级为
	// Goal + AutoResearch 运行。返回目标文本和是否成功升级。
	AutoStartResearchGoal(input string) (string, bool)
	// ResetPlannerSession 清除规划器的对话历史，以便下一个计划从头开始。
	// 在双模型（Plan+Execute）模式下，防止来自先前会话的过时规划器输出
	// 污染当前执行器的交接。
	ResetPlannerSession()
	// PlanMode 报告出站轮次当前是否接收计划模式标记。
	PlanMode() bool
	// SetPlanMode 翻转执行器的只读门控，而不触及缓存稳定的提示前缀。
	SetPlanMode(v bool)
	// SetAutoPlan 更新后续轮次的交互式自动计划门控。
	SetAutoPlan(mode string)
}

// SessionHistory 覆盖检查点/回退、分支/分叉以及日志重构操作（压缩、摘要）。
// 这些操作允许用户在对话历史中导航和管理不同版本。
type SessionHistory interface {
	// Checkpoints 列出会话的回退点（每个用户轮次一个），按时间从旧到新排序。
	Checkpoints() []checkpoint.Meta
	// CheckpointHasBoundary 报告给定轮次是否有有效的对话回退边界。
	// 压缩后旧轮次的边界可能失效。
	CheckpointHasBoundary(turn int) bool
	// Rewind 将会话恢复到 turn 的开始：Code 恢复该轮次更改的文件，
	// Conversation 截断消息日志，Both 执行两者。运行中的轮次会拒绝此操作。
	Rewind(turn int, scope RewindScope) error
	// Fork 在 turn 的开始处分叉对话到一个新会话文件，保留当前会话作为分支点，
	// 并切换到新分支。返回新会话路径。
	Fork(turn int) (string, error)
	// ForkNamed 与 Fork 相同，但允许为新分支指定名称。
	ForkNamed(turn int, name string) (string, error)
	// ForkSession 将 turn 处的对话复制到新会话文件，但不切换控制器到它。
	// Desktop 用此在新标签页中打开分支。
	ForkSession(turn int, name string) (string, error)
	// Branch 将当前对话复制到一个子分支并切换到它。与 Fork 不同，
	// 它在当前提示处分叉，不需要检查点。
	Branch(name string) (string, error)
	// Branches 列出此控制器会话目录中保存的对话分支。
	Branches() ([]agent.BranchInfo, error)
	// BranchTreeText 返回分支树的格式化文本表示。
	BranchTreeText() string
	// SwitchBranch 切换到由 ref 标识的分支（可以是 ID、名称或路径）。
	SwitchBranch(ref string) (agent.BranchInfo, error)
	// Compact 按需在执行器的会话上运行一次压缩过程。
	// instructions 是可选的 `/compact <focus>` 指导，用于引导保留什么内容。
	Compact(ctx context.Context, instructions string) error
	// CompactRatio 返回自动压缩阈值，以窗口比例表示。
	CompactRatio() float64
	// SummarizeFrom 从 turn 开始将对话压缩为一个摘要。
	SummarizeFrom(ctx context.Context, turn int) error
	// SummarizeUpTo 将 turn 之前的所有内容压缩为一个摘要。
	SummarizeUpTo(ctx context.Context, turn int) error
}

// MemoryControl 覆盖会话/项目记忆的读取和变更操作。记忆系统允许用户和模型
// 持久化重要信息，在后续会话中自动注入。
type MemoryControl interface {
	// Memory 返回加载的记忆快照（记忆禁用时返回 nil）。
	// 返回的 *Set 是不可变的——变更通过 QuickAdd/SaveDoc 进行。
	Memory() *memory.Set
	// QuickAdd 向指定范围的记忆文档文件追加一行注释。
	// 这是 "#<note>" 快捷方式的写入端。返回写入的文件路径。
	QuickAdd(scope memory.Scope, note string) (string, error)
	// SaveDoc 覆盖一个已识别的记忆文档——桌面面板就地编辑器的保存端。
	SaveDoc(path, body string) (string, error)
	// SaveMemory 写入一个活动的自动记忆事实并刷新会话内快照。
	// 它是 remember 工具的显式用户确认对应物。
	SaveMemory(m memory.Memory) (string, error)
	// ForgetMemory 按名称移除已保存的自动记忆——面板/TUI 的遗忘操作。
	// 文件会被归档以便追溯。
	ForgetMemory(name string) error
	// QueueMemory 实现 memory.Queue 接口：当模型运行 remember/forget 工具时，
	// 工具调用此方法排队一个注释，该注释将搭乘下一个轮次，使变更在本会话内
	// 生效而不触及缓存稳定的系统前缀。
	QueueMemory(note string)
}

// Capabilities 覆盖会话的可插拔表面——MCP 服务器、技能、斜杠命令、钩子——
// 以及解析提示/命令/技能输入。这些是会话的扩展能力。
type Capabilities interface {
	// Host 返回正在运行的 MCP 宿主（无插件时为 nil），供前端列出服务器
	// 或解析 MCP 提示。
	Host() *plugin.Host
	// Commands 返回已加载的自定义斜杠命令。
	Commands() []command.Command
	// ReloadCommands 重新扫描所有命令目录并热交换 slash_command 工具和
	// 内部命令切片——无需 MCP 重启，无需钩子重跑。
	ReloadCommands(ctx context.Context) error
	// Skills 返回可发现的技能（用于斜杠菜单和 `/skills`）。
	Skills() []skill.Skill
	// AllSkills 返回所有可发现的技能，包括已禁用的。
	AllSkills() []skill.Skill
	// DisabledSkills 返回配置中已禁用的所有可发现技能。
	DisabledSkills() []skill.Skill
	// SkillEnabled 报告一个可发现的技能是否已启用。
	SkillEnabled(name string) bool
	// SetSkillEnabled 持久化技能的启用/禁用偏好。
	SetSkillEnabled(name string, enabled bool) error
	// HookRunner 返回会话的钩子运行器（nil 安全；可能持有零个钩子）。
	HookRunner() *hook.Runner
	// CustomCommand 解析 "/name args…" 行，匹配已加载的自定义斜杠命令，
	// 返回要发送的渲染提示。未匹配时 found=false。
	CustomCommand(input string) (sent string, found bool)
	// MCPPrompt 解析 "/mcp__server__prompt args…" 行：将位置参数映射到
	// 提示的声明参数，并从 MCP 服务器获取渲染的提示。
	MCPPrompt(ctx context.Context, input string) (sent string, found bool, err error)
	// RunSkill 解析 "/<name> args…" 行，匹配已加载的技能，返回技能的
	// 渲染主体作为轮次。通过斜杠调用的技能始终内联其主体。
	RunSkill(input string) (sent string, found bool)
	// AddMCPServer 实时连接一个 MCP 服务器并将其持久化到配置文件。
	// 其工具立即注册并在下一个轮次可用。返回服务器暴露的工具数量。
	AddMCPServer(e config.PluginEntry) (int, error)
	// ConnectMCPServer 连接一个 MCP 服务器条目到本会话，但不写入配置。
	ConnectMCPServer(e config.PluginEntry) (int, error)
	// ConnectConfiguredMCPServer 按名称连接一个已在配置中的 MCP 服务器。
	ConnectConfiguredMCPServer(name string) (int, error)
	// DisconnectMCPServer 断开一个活动服务器的本会话连接而不触及配置。
	DisconnectMCPServer(name string) bool
	// RemoveMCPServer 断开一个活动 MCP 服务器并从配置文件中移除。
	RemoveMCPServer(name string) (disconnected bool, err error)
	// ConfiguredMCPNames 返回配置文件中声明的所有 MCP 服务器名称。
	ConfiguredMCPNames() []string
	// DisconnectedMCPNames 返回已配置但未连接的 MCP 服务器名称。
	DisconnectedMCPNames() []string
	// UnregisterMCPServerTools 仅对此控制器隐藏共享的 MCP 服务器。
	// Desktop 共享宿主路径用此实现每标签页的连接器切换。
	UnregisterMCPServerTools(name string) bool
	// ImportMCPEntries 持久化选定的 MCP 条目并尝试实时连接。
	ImportMCPEntries(entries []config.PluginEntry) (total, added, updated, connected, failed, skipped int, err error)
}

// Status 覆盖只读的运行/使用量/计费遥测数据。前端通过这些方法获取
// 当前会话的状态信息，用于状态栏和仪表盘显示。
type Status interface {
	// ContextSnapshot 返回 (提示令牌数, 上下文窗口大小)。
	// 两者都为零表示尚无数据——仪表应隐藏自身。
	ContextSnapshot() (int, int)
	// LastUsage 返回最近一个轮次的令牌遥测数据（第一个轮次前为 nil），
	// 前端可由此推导提示缓存命中率用于状态行。
	LastUsage() *provider.Usage
	// Balance 查询活动提供商的钱包余额。提供商未声明 balance_url 时
	// 返回 (nil, nil)——调用者将"未配置"和"已获取"同等对待。
	Balance(ctx context.Context) (*billing.Balance, error)
	// Jobs 返回仍在运行的后台作业列表（禁用后台作业时返回 nil），
	// 用于状态栏显示。
	Jobs() []jobs.View
}

// SessionPersistence 覆盖会话快照和磁盘状态清理。这些方法管理会话数据
// 的持久化生命周期。
type SessionPersistence interface {
	// Snapshot 将执行器的对话写入活动会话文件。无操作时执行器不存在或
	// 会话从未使用过。每次轮调后调用，使崩溃最多丢失一个飞行中的提示。
	Snapshot() error
	// SnapshotActivity 写入活动对话并标记会话为最近活跃。仅在真正的
	// 用户/模型轮次改变转录后使用。
	SnapshotActivity() error
	// SessionCache 返回会话的累积缓存命中/未命中提示令牌数，
	// 以便前端渲染聚合（会话范围）缓存命中率。
	SessionCache() (hit, miss int)
	// BeginDestroySession 标记会话正在离开活动使用并取消其后台作业。
	// 调用 Wait 在移动/删除工件之前等待，然后在持久清理完成后调用 Finish。
	BeginDestroySession(sessionPath string) SessionDestroyHandle
	// CloseAfterDestroy 在调用者已经开始会话特定的作业拆卸后释放控制器资源。
	CloseAfterDestroy()
	// IsDestroyingSession 报告 sessionPath 是否当前处于此控制器作业管理器
	// 的销毁窗口中。
	IsDestroyingSession(sessionPath string) bool
	// ReleaseResources 停止插件子进程并释放资源，但不触发 SessionEnd。
	// 仅在为同一逻辑会话替换控制器时使用。
	ReleaseResources()
}

// Input 覆盖轮次文本的组合（计划/目标/记忆注入）和提交前的 @ 引用解析。
// 这些方法处理用户输入的预处理，使其适合发送给模型。
type Input interface {
	// Compose 应用计划模式标记到轮次文本（当计划模式开启时），返回实际
	// 发送给模型的消息。前端继续显示原始文本作为用户气泡。同时注入：
	// - 活动目标块（当目标正在运行时）
	// - 推理语言偏好
	// - 会话中添加的记忆注释（不触及缓存稳定的系统前缀）
	// - 已完成的后台作业通知
	Compose(text string) string
	// ComposeSynthetic 为合成的用户消息（如计划批准消息）应用推理语言偏好，
	// 但不注入目标/记忆/作业等上下文。
	ComposeSynthetic(text string) string
	// ResolveRefs 将一行中的 @ 引用解析为单个带标签的上下文块
	//（文件/目录内容、MCP 资源体），以及每个引用失败的错误字符串。
	// 可安全地在前端事件循环之外调用；尊重 ctx 进行资源读取。
	ResolveRefs(ctx context.Context, line string) (block string, errs []string)
	// HasRefs 报告一行是否包含任何可解析的 @ 引用，以便前端可以决定
	// 仅在需要时才在其事件循环之外解析。
	HasRefs(line string) bool
}

// Settings 覆盖不适合更丰富领域的运行时会话设置。
type Settings interface {
	// SetReasoningLanguage 更新后续轮次的可见推理语言偏好。
	SetReasoningLanguage(lang string)
	// SetDisplayRecorder 安装一个可选钩子，供持久化比完整模型提示更短的
	// 用户界面转录的前端使用。
	SetDisplayRecorder(fn func(content, display string))
}

// SessionAPI 是完整的驱动端口——所有子接口的组合。丰富的前端（HTTP 服务器、
// 桌面应用、TUI）依赖此接口；精简的前端（bot、acp）仅依赖它们使用的子接口。
type SessionAPI interface {
	Lifecycle
	TurnControl
	Approvals
	Goals
	SessionHistory
	MemoryControl
	Capabilities
	Status
	SessionPersistence
	Input
	Settings
}

// 编译时证明：具体控制器满足每个子接口和完整端口，因此前端迁移到接口是
// 机械性的，并且永远不会与实现静默漂移。
var (
	_ Lifecycle          = (*Controller)(nil)
	_ TurnControl        = (*Controller)(nil)
	_ Approvals          = (*Controller)(nil)
	_ Goals              = (*Controller)(nil)
	_ SessionHistory     = (*Controller)(nil)
	_ MemoryControl      = (*Controller)(nil)
	_ Capabilities       = (*Controller)(nil)
	_ Status             = (*Controller)(nil)
	_ SessionPersistence = (*Controller)(nil)
	_ Input              = (*Controller)(nil)
	_ Settings           = (*Controller)(nil)
	_ SessionAPI         = (*Controller)(nil)
)
