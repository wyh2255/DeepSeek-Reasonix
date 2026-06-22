// 文件：approval.go
//
// 审批管理器——工具审批和用户提问的簿记与运行时姿态管理。
// approvalManager 拥有审批/提示的簿记和运行时审批姿态（ask/auto/yolo），
// 使用自己的锁（不依赖 c.mu）。它是一个严格的叶子：其方法仅触及自身状态，
// 永远不会回调 Controller。
//
// Controller 保留了 I/O 编排（发出事件、触发钩子、重建执行器门控），
// 这些需要其他协作者——approval 与目标 FSM 不同，它阻塞在用户输入上并有副作用，
// 因此只提取了簿记部分，而非编排部分。
package control

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"reasonix/internal/event"
	"reasonix/internal/permission"
)

// approvalManager 拥有审批/提问提示的簿记和运行时审批姿态（ask/auto/yolo），
// 使用自己的锁（不依赖 c.mu）。它是一个严格的叶子：其方法仅触及自身状态，
// 永远不会回调 Controller。Controller 保留了 I/O 编排（发出事件、触发钩子、
// 重建执行器门控）——审批与目标 FSM 不同，它阻塞在用户输入上并有副作用，
// 因此只提取了簿记部分，而非编排部分。
type approvalManager struct {
	// policy 是不可变的基础权限策略，在构造时捕获。
	// 用于判断工具调用是否会在写入回退模式下自动批准（autoApprovalWouldAllowLocked）；
	// Controller 保留自己的副本用于构建执行器门控。
	policy permission.Policy

	// mu 保护提示映射和姿态字段；每个临界区尽量短且非阻塞。
	mu        sync.Mutex
	approvals map[string]pendingApproval // 待处理的工具审批映射
	asks      map[string]pendingAsk     // 待处理的提问映射
	granted   map[string]bool           // 本会话已授权的规则集合
	nextID    int                       // 下一个审批/提问的自增 ID
	// toolApprovalMode 是运行时审批姿态：
	//   - "ask": 每次都提示用户
	//   - "auto": 策略自动批准写入回退，同时保留 ask/deny 规则
	//   - "yolo": 跳过所有工具审批提示（计划审批除外）
	toolApprovalMode string
	// approvalTimeout 限制 requestApproval/Ask 阻塞等待用户决策的时长。
	// 零值表示无限等待（适用于交互式终端）；
	// bot/无头前端设置此值以防止离开的用户永久阻塞会话（#4626, #4402）。
	// 构造时一次性写入。
	approvalTimeout time.Duration
	// planAutoApprove 在刚批准的计划执行期间自动允许写入工具调用，无需提示。
	// 由轮次循环设置，由旁路检查读取。计划批准意味着放行，
	// 因此模型不应为每个写入操作再次提示。
	planAutoApprove bool

	// promptMu 序列化待处理的提示，确保同时最多只有一个用户决策在飞行中。
	// 在阻塞等待期间持有，因此解析路径（Approve/AnswerQuestion）绝不能获取此锁。
	promptMu sync.Mutex
}

func newApprovalManager(policy permission.Policy, mode string, timeout time.Duration) approvalManager {
	return approvalManager{
		policy:           policy,
		approvals:        map[string]pendingApproval{},
		asks:             map[string]pendingAsk{},
		granted:          map[string]bool{},
		toolApprovalMode: mode,
		approvalTimeout:  timeout,
	}
}

// preApproved 判断工具调用是否可以跳过提示——要么姿态绕过了它（YOLO / 计划执行窗口），
// 要么会话授权已覆盖该范围。
func (a *approvalManager) preApproved(tool, subject string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.bypassAllowsLocked(tool) || a.sessionGrantAllowsLocked(tool, subject)
}

// register 分配一个审批 ID，记录待处理的提示，并返回解析路径将发送信号的回复通道。
func (a *approvalManager) register(tool, subject string) (string, chan approvalReply) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextID++
	id := strconv.Itoa(a.nextID)
	reply := make(chan approvalReply, 1)
	a.approvals[id] = pendingApproval{tool: tool, subject: subject, autoDrain: a.autoApprovalWouldAllowLocked(tool, subject), reply: reply}
	return id, reply
}

// grantSession 记录一个会话范围的授权，使同范围内的后续调用可以短路跳过。
func (a *approvalManager) grantSession(tool, subject string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.granted[permission.SessionGrantRuleForScope(tool, subject)] = true
}

// cancel 移除一个待处理的审批（超时/中止路径）。
func (a *approvalManager) cancel(id string) {
	a.mu.Lock()
	delete(a.approvals, id)
	a.mu.Unlock()
}

// resolve 移除并返回指定 ID 的待处理审批（Approve 路径）。
func (a *approvalManager) resolve(id string) pendingApproval {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.approvals[id]
	delete(a.approvals, id)
	return p
}

// registerAsk 分配一个提问 ID，记录待处理的问题批次，并返回回复通道。
func (a *approvalManager) registerAsk(questions []event.AskQuestion) (string, chan []event.AskAnswer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextID++
	id := strconv.Itoa(a.nextID)
	reply := make(chan []event.AskAnswer, 1)
	a.asks[id] = pendingAsk{questions: questions, reply: reply}
	return id, reply
}

// cancelAsk 移除一个待处理的提问（超时/中止路径）。
func (a *approvalManager) cancelAsk(id string) {
	a.mu.Lock()
	delete(a.asks, id)
	a.mu.Unlock()
}

// resolveAsk 移除并返回指定 ID 的待处理提问（AnswerQuestion 路径）。
func (a *approvalManager) resolveAsk(id string) (pendingAsk, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.asks[id]
	delete(a.asks, id)
	return p, ok
}

// clearAll 移除所有飞行中的提示而不发送信号——取消路径中，
// 被阻塞的等待者通过已取消的 context 解除阻塞。
func (a *approvalManager) clearAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	clear(a.approvals)
	clear(a.asks)
}

// hasPending 报告是否有提示正在等待用户决策。
func (a *approvalManager) hasPending() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.approvals) > 0 || len(a.asks) > 0
}

// mode 返回规范化的运行时审批姿态。
func (a *approvalManager) mode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return normalizeToolApprovalMode(a.toolApprovalMode)
}

// setMode 应用（预规范化的）姿态，并排空新姿态应自动允许的待处理审批，
// 返回它们的回复通道以便调用者在解锁后发送信号。
func (a *approvalManager) setMode(mode string) []chan approvalReply {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.toolApprovalMode = mode
	switch mode {
	case ToolApprovalAuto:
		return a.drainLocked(false)
	case ToolApprovalYolo:
		return a.drainLocked(true)
	}
	return nil
}

// setPlanAutoApprove 切换刚批准的计划执行窗口。
func (a *approvalManager) setPlanAutoApprove(on bool) {
	a.mu.Lock()
	a.planAutoApprove = on
	a.mu.Unlock()
}

// waitContext 在设置了 approvalTimeout 时为阻塞等待添加超时边界。
func (a *approvalManager) waitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if a.approvalTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, a.approvalTimeout)
}

// snapshotPrompts 复制飞行中的提示，用于向重新连接的前端重新发出（ReplayPendingPrompts）。
func (a *approvalManager) snapshotPrompts() ([]event.Approval, []event.Ask) {
	a.mu.Lock()
	defer a.mu.Unlock()
	approvals := make([]event.Approval, 0, len(a.approvals))
	for id, p := range a.approvals {
		approvals = append(approvals, event.Approval{ID: id, Tool: p.tool, Subject: p.subject})
	}
	asks := make([]event.Ask, 0, len(a.asks))
	for id, p := range a.asks {
		asks = append(asks, event.Ask{ID: id, Questions: p.questions})
	}
	return approvals, asks
}

// --- 决策辅助函数（调用者持有 a.mu） ---

func (a *approvalManager) bypassAllowsLocked(tool string) bool {
	if requiresFreshApprovalTool(tool) {
		return false
	}
	return a.toolApprovalMode == ToolApprovalYolo || a.planAutoApprove
}

func (a *approvalManager) autoApprovalWouldAllowLocked(tool, subject string) bool {
	if requiresFreshApprovalTool(tool) {
		return false
	}
	policy := a.policy
	policy.Mode = permission.Allow
	return policy.DecideSubject(tool, false, subject) == permission.Allow
}

func (a *approvalManager) sessionGrantAllowsLocked(tool, subject string) bool {
	if requiresFreshApprovalTool(tool) {
		return false
	}
	for rule := range a.granted {
		if permission.RuleMatchesString(rule, tool, subject) {
			return true
		}
	}
	return false
}

// drainLocked 移除新姿态应自动允许的所有待处理审批，并返回它们的回复通道；
// 调用者持有 a.mu，在解锁后发送 {allow:true}。
func (a *approvalManager) drainLocked(includeExplicitAsk bool) []chan approvalReply {
	pending := make([]chan approvalReply, 0, len(a.approvals))
	for id, approval := range a.approvals {
		if requiresFreshApprovalTool(approval.tool) {
			continue
		}
		if !includeExplicitAsk && !approval.autoDrain {
			continue
		}
		delete(a.approvals, id)
		pending = append(pending, approval.reply)
	}
	return pending
}

// --- 纯审批辅助函数 ---

func normalizeToolApprovalMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ToolApprovalAuto, "approve", "allow":
		return ToolApprovalAuto
	case ToolApprovalYolo, "full", "full-access", "bypass":
		return ToolApprovalYolo
	default:
		return ToolApprovalAsk
	}
}

func requiresFreshApprovalTool(tool string) bool {
	switch tool {
	case planApprovalTool, memoryRememberTool, memoryForgetTool:
		return true
	default:
		return false
	}
}

func approvalNotificationText(tool, subject string) string {
	if requiresFreshApprovalTool(tool) {
		return "approval needed: " + tool
	}
	if subject == "" {
		return "approval needed: " + tool
	}
	return "approval needed: " + tool + " " + subject
}

func permissionRequestHookPayload(tool, subject string, args json.RawMessage) (string, json.RawMessage, bool) {
	switch tool {
	case planApprovalTool:
		return "", nil, false
	case memoryRememberTool, memoryForgetTool:
		return "", nil, true
	default:
		return subject, args, true
	}
}
