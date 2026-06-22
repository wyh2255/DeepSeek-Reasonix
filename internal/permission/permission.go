// Package permission 实现了工具调用的权限控制系统。
//
// 每次工具调用时，权限系统决定是放行（Allow）、拒绝（Deny）还是询问用户（Ask）。
//
// 架构设计：
//   - Policy（策略）：纯规则评估层，无 I/O，可独立测试
//   - Gate（门控）：Policy + 可选的交互式 Approver，是 Agent 在执行时实际咨询的对象
//   - Approver（审批者）：由前端（如 TUI）实现，负责向用户展示审批界面
//
// 三种审批模式：
//   - ask：每次工具调用都需要用户确认
//   - auto：自动批准只读工具，写入工具需要确认
//   - yolo：全部自动批准（不推荐生产使用）
//
// 规则优先级：deny > ask > allow > 回退默认值（只读工具默认 Allow，写入工具使用模式默认值）
package permission

import (
	"context"
	"encoding/json"
	"strings"
)

// Decision 是工具调用经过策略评估后的决策结果。
type Decision int

const (
	// Allow 表示允许执行工具，无需用户确认。
	Allow Decision = iota
	// Ask 表示需要交互式 Approver 来决定（非交互模式下等同于 Allow）。
	Ask
	// Deny 表示拒绝执行工具，在所有模式下都会阻止。
	Deny
)

func (d Decision) String() string {
	switch d {
	case Allow:
		return "allow"
	case Ask:
		return "ask"
	case Deny:
		return "deny"
	default:
		return "unknown"
	}
}

// ParseDecision 将配置字符串映射为 Decision 枚举值。
// 未知或空输入默认为 Ask — 这是写入工具的保守默认策略。
func ParseDecision(s string) Decision {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "allow":
		return Allow
	case "deny":
		return Deny
	default:
		return Ask
	}
}

// Rule 表示一条权限匹配规则，用于匹配工具调用。
//
// 字段说明：
//   - Tool: 工具名称（如 "bash", "edit_file"），或特殊值 "file_mutation" 匹配所有写入工具
//   - Subject: 匹配约束（如文件路径、命令内容）。为空时匹配该工具的所有调用
//   - Literal: 为 true 时按精确字符串匹配 Subject（'*' 和 '?' 视为普通字符），
//     为 false 时作为通配符 glob 匹配
type Rule struct {
	Tool    string // 工具名称
	Subject string // 匹配约束（路径、命令等），为空则匹配所有
	// Literal 控制 Subject 的匹配模式：
	//   true  = 精确匹配（已记忆的具体命令中的 '*'/'?' 保持字面含义）
	//   false = glob 通配符匹配（'*' 匹配任意字符序列，'?' 匹配单个字符）
	Literal bool
}

// ParseRule 解析规则字符串，支持三种格式：
//
//   - "ToolName"：匹配该工具的所有调用（无 Subject 约束）
//   - "ToolName(glob)"：匹配该工具中 Subject 与 glob 匹配的调用
//   - "ToolName=literal"：旧格式，精确匹配 Subject（不进行 glob 展开）
//
// "=literal" 格式（当 '=' 出现在 '(' 之前时）保持向后兼容，
// 用于在 Claude Code 风格的 Tool(specifier) 规则出现之前编写的配置。
//
// 返回 ok=false 表示格式错误（工具名为空），调用者应发出警告而非静默安装无效规则。
func ParseRule(s string) (Rule, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rule{}, false
	}
	if eq := strings.IndexByte(s, '='); eq > 0 {
		if paren := strings.IndexByte(s, '('); paren < 0 || eq < paren {
			tool := strings.TrimSpace(s[:eq])
			if tool == "" {
				return Rule{}, false
			}
			return Rule{Tool: tool, Subject: s[eq+1:], Literal: true}, true
		}
	}
	if i := strings.IndexByte(s, '('); i >= 0 && strings.HasSuffix(s, ")") {
		tool := strings.TrimSpace(s[:i])
		if tool == "" {
			return Rule{}, false
		}
		return Rule{Tool: tool, Subject: s[i+1 : len(s)-1]}, true
	}
	return Rule{Tool: s}, true
}

func parseRules(ss []string) []Rule {
	var out []Rule
	for _, s := range ss {
		if r, ok := ParseRule(s); ok {
			out = append(out, r)
		}
	}
	return out
}

// Policy 是权限系统的核心，包含一组规则和写入工具的回退模式。
// 它是纯规则评估层，无 I/O 操作，可独立测试。
//
// 字段说明：
//   - Mode: 当没有规则匹配时，写入工具的回退决策。只读工具始终回退为 Allow
//   - Allow: 放行规则列表
//   - Ask: 询问用户规则列表
//   - Deny: 拒绝规则列表
//
// 评估优先级：deny > ask > allow > 回退默认值
type Policy struct {
	Mode  Decision // 写入工具无规则匹配时的回退决策
	Allow []Rule   // 放行规则
	Ask   []Rule   // 询问规则
	Deny  []Rule   // 拒绝规则
}

// New 从配置字符串切片和模式字符串构建 Policy。
// mode 默认为 "ask"。格式错误的规则字符串会被静默丢弃。
func New(mode string, allow, ask, deny []string) Policy {
	return Policy{
		Mode:  ParseDecision(mode),
		Allow: parseRules(allow),
		Ask:   parseRules(ask),
		Deny:  parseRules(deny),
	}
}

// Decide 评估一次工具调用的权限决策。
//
// 参数：
//   - toolName: 工具名称
//   - readOnly: 工具自身的只读分类
//   - args: 模型发送的原始 JSON 参数，用于提取 Subject 进行 glob 匹配
//
// 对于有多个 Subject 的调用（如 move_file 的源路径和目标路径），
// 必须每个 Subject 都安全才允许执行。
//
// 评估优先级：deny > ask > allow > 回退默认值（只读工具为 Allow，写入工具为 Mode）
func (p Policy) Decide(toolName string, readOnly bool, args json.RawMessage) Decision {
	return p.DecideSubjects(toolName, readOnly, Subjects(args))
}

// DecideSubject 评估工具调用权限，调用者已从参数中提取了稳定的审批 Subject。
func (p Policy) DecideSubject(toolName string, readOnly bool, subject string) Decision {
	switch {
	case matchAny(p.Deny, toolName, subject):
		return Deny
	case matchAny(p.Ask, toolName, subject):
		return Ask
	case matchAny(p.Allow, toolName, subject):
		return Allow
	case readOnly:
		return Allow
	default:
		return p.Mode
	}
}

// DecideSubjects 评估工具调用涉及的每个 Subject 的权限。
//
// 这确保了双路径操作（如 move_file）的正确性：
//   - 任一端点被拒绝 → 整个操作被拒绝
//   - 任一端点需要询问 → 整个操作需要询问
//   - 所有端点都被允许 → 整个操作被允许
func (p Policy) DecideSubjects(toolName string, readOnly bool, subjects []string) Decision {
	if len(subjects) == 0 {
		return p.DecideSubject(toolName, readOnly, "")
	}
	out := Allow
	for _, subject := range subjects {
		switch p.DecideSubject(toolName, readOnly, subject) {
		case Deny:
			return Deny
		case Ask:
			out = Ask
		}
	}
	return out
}

// matchAny 检查规则列表中是否有任何规则匹配给定的 (toolName, subject) 对。
// 带 Subject 的规则无法匹配不暴露 Subject 的调用。
func matchAny(rules []Rule, toolName, subject string) bool {
	for _, r := range rules {
		if !ruleToolMatches(r.Tool, toolName) {
			continue
		}
		if r.Subject == "" {
			return true
		}
		if subject == "" {
			continue
		}
		if ruleSubjectMatches(r, subject) {
			return true
		}
	}
	return false
}

// RuleMatchesString 判断一个配置风格的规则字符串是否匹配给定的工具主题。
// 用于会话授权和持久化配置规则，使两条路径共享相同的匹配语义。
func RuleMatchesString(rule, toolName, subject string) bool {
	r, ok := ParseRule(rule)
	return ok && matchAny([]Rule{r}, toolName, subject)
}

// RuleCoversString 判断 existing 规则是否已覆盖 candidate 规则所代表的所有调用。
// 仅验证 Reasonix 自动创建的场景：精确规则被更宽泛的 glob 或裸工具规则覆盖、
// 完全重复的 glob、以及裸工具规则覆盖带主题的规则。
func RuleCoversString(existing, candidate string) bool {
	a, ok := ParseRule(existing)
	if !ok {
		return false
	}
	b, ok := ParseRule(candidate)
	if !ok {
		return false
	}
	if !ruleToolCompatible(a.Tool, b.Tool) {
		return false
	}
	if a.Subject == "" {
		return true
	}
	if b.Subject == "" {
		return false
	}
	if bashRulePrefixBaseMatches(a, b) {
		return true
	}
	if b.Literal || !hasGlobMeta(b.Subject) {
		return ruleSubjectMatches(a, b.Subject)
	}
	return !a.Literal && a.Subject == b.Subject
}

func hasGlobMeta(s string) bool {
	return strings.ContainsAny(s, "*?")
}

func bashRulePrefixBaseMatches(existing, candidate Rule) bool {
	if canonicalRuleTool(existing.Tool) != "bash" || canonicalRuleTool(candidate.Tool) != "bash" {
		return false
	}
	existingBase, ok := bashPrefixBase(existing.Subject)
	if !ok {
		return false
	}
	candidateBase, ok := bashPrefixBase(candidate.Subject)
	return ok && existingBase == candidateBase
}

// subjectKeys 是按优先级排列的 JSON 参数键，携带工具调用的"主题"——
// Subject glob 匹配的目标。设计为通用的，使工具无需实现权限专用方法：
// bash 暴露 command，文件工具暴露 path / file_path，grep 和 glob 暴露 pattern。
var subjectKeys = []string{"command", "file_path", "path", "source_path", "destination_path", "pattern"}

// Subject 从调用的原始 JSON 参数中提取主要可匹配的主题字符串，
// 当没有已知键存在时返回 ""（此类调用仅匹配裸 "ToolName" 规则）。
// 对于需要考虑每个涉及端点的权限决策，请使用 Subjects。
func Subject(args json.RawMessage) string {
	subjects := Subjects(args)
	if len(subjects) > 0 {
		return subjects[0]
	}
	return ""
}

// Subjects 从调用的原始 JSON 参数中提取所有可匹配的主题。
// 大多数工具暴露一个主题；move_file 同时暴露 source_path 和 destination_path，
// 使路径范围的权限规则可以保护任一端点。
func Subjects(args json.RawMessage) []string {
	if len(args) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return nil
	}
	src := stringArg(m, "source_path")
	dst := stringArg(m, "destination_path")
	if src != "" && dst != "" {
		out := []string{src}
		if dst != src {
			out = append(out, dst)
		}
		return out
	}
	for _, k := range subjectKeys {
		if s := stringArg(m, k); s != "" {
			return []string{s}
		}
	}
	return nil
}

func stringArg(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// matchGlob 判断 name 是否匹配 pattern，其中 '*' 匹配任意字符序列（包括分隔符），
// '?' 匹配恰好一个字符。与 path.Match 不同，'*' 不会被 '/' 阻止，
// 这是命令行和路径前缀（如 "rm -rf*"、"/etc/*"）的直觉预期。
// 线性时间带回溯，面向字节。
func matchGlob(pattern, name string) bool {
	var px, nx, starPx, starNx int
	starPx = -1
	for nx < len(name) {
		switch {
		case px < len(pattern) && (pattern[px] == '?' || pattern[px] == name[nx]):
			px++
			nx++
		case px < len(pattern) && pattern[px] == '*':
			starPx = px
			starNx = nx
			px++
		case starPx != -1:
			px = starPx + 1
			starNx++
			nx = starNx
		default:
			return false
		}
	}
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}

// Approver 是交互式审批接口，用于解决 Ask 决策。
// 实现位于前端（如 TUI 聊天界面）；非交互模式传入 nil，Gate 会将其视为 "允许"。
type Approver interface {
	// Approve 向用户询问是否允许一个待处理的工具调用。
	//
	// 返回值：
	//   - allow: 是否允许执行
	//   - remember: 是否将此选择记忆为新规则（"始终允许"）
	//   - err: 非 nil 错误（如等待期间 context 被取消）会中止本轮
	Approve(ctx context.Context, toolName, subject string, args json.RawMessage) (allow, remember bool, err error)
}

// Gate 是 Agent 在执行时实际咨询的权限门控：Policy + 可选的 Approver。
// 它结构化地满足 Agent 的 Gate 接口。
type Gate struct {
	Policy   Policy    // 纯规则评估策略
	Approver Approver  // 交互式审批器（非交互模式为 nil）

	// OnRemember 当用户选择 "始终允许" 时被调用，传递新规则字符串（如 "Bash(go build)"），
	// 以便前端持久化该规则到配置文件。
	OnRemember func(rule string)
}

// NewGate 将 Policy 与 Approver 组合为 Gate。非交互模式下 Approver 传 nil。
func NewGate(p Policy, a Approver) *Gate { return &Gate{Policy: p, Approver: a} }

// Check 决定一个工具调用是否可以执行。这是 Agent 的 Gate 接口期望的方法。
//
// 处理流程：
//  1. 对 bash 工具进行只读命令检测（如 "ls", "git status" 等被视为只读）
//  2. 通过 Policy.Decide 评估权限
//  3. Deny：直接拒绝，返回拒绝原因
//  4. Ask：如果有 Approver 则交互询问；无 Approver 则放行（保持自主性）
//  5. Allow：直接放行
//
// 返回值：
//   - allow: 是否允许执行
//   - reason: 拒绝或用户拒绝时的原因描述，供模型理解
//   - err: 审批过程中的错误（如 context 取消）
func (g *Gate) Check(ctx context.Context, toolName string, args json.RawMessage, readOnly bool) (bool, string, error) {
	if toolName == "bash" && !readOnly {
		subject := Subject(args)
		if isReadOnlyBashSubject(subject) {
			readOnly = true
		}
	}
	switch g.Policy.Decide(toolName, readOnly, args) {
	case Deny:
		return false, "denied by permission policy — this tool/command is on the deny list. Do not retry it; choose another approach or stop and explain.", nil
	case Ask:
		if g.Approver == nil {
			return true, "", nil // non-interactive: preserve autonomy
		}
		subject := Subject(args)
		allow, remember, err := g.Approver.Approve(ctx, toolName, subject, args)
		if err != nil {
			return false, "approval aborted", err
		}
		if !allow {
			return false, "the user declined this tool call — do not retry it; ask how they would like to proceed or choose another approach.", nil
		}
		if remember && g.OnRemember != nil {
			// "Always allow" is tool-wide: persist the bare tool name so any
			// later subject (a different file / command) is allowed without
			// re-prompting. Deny rules still take precedence on every call.
			g.OnRemember(toolName)
			// Also add the rule to the in-memory Policy immediately so it
			// takes effect in the current session without requiring a restart.
			// The session-level grant (controller.granted) already covers the
			// Approver path, but any code path that consults Policy.Decide()
			// directly would miss the rule until the next controller build.
			if rule, ok := ParseRule(toolName); ok {
				g.Policy.Allow = append(g.Policy.Allow, rule)
			}
		}
		return true, "", nil
	default:
		return true, "", nil
	}
}

// rememberRule 构建用户选择 "始终允许" 时持久化的规则字符串。
//
// 策略：
//   - bash 命令：优先使用安全的命令前缀（如 "go test:*"），
//     使 "始终允许" 覆盖使用不同参数的类似调用
//   - 文件写入工具：按工具名记忆（"Edit"），批准一个文件编辑即覆盖所有文件
//   - 其他工具：按工具名记忆
//   - deny 和 ask 规则始终保持更高优先级
func rememberRule(toolName, subject string) string {
	return RememberRuleForScope(toolName, subject)
}

// RememberRuleForScope 构建用户选择"始终允许"时持久化的规则字符串。
// bash 命令优先使用安全前缀（如 "go test:*"），使类似调用（不同搜索词、不同测试包）匹配；
// 无法提取安全前缀时使用精确命令。文件写入工具始终按工具名记忆（Edit）。
// 其他工具使用裸工具名。deny 规则在每次调用时仍然优先。
func RememberRuleForScope(toolName, subject string) string {
	subject = strings.TrimSpace(subject)
	if subject != "" && toolName == "bash" {
		if pattern := BashCommandPrefix(subject); pattern != "" {
			return "Bash(" + pattern + ")"
		}
		return "Bash(" + subject + ")"
	}
	if IsFileMutationTool(toolName) {
		return "Edit"
	}
	return toolName
}

// SessionGrantKey 返回"本会话允许"的内存规则。
// bash 优先使用命令前缀，不安全时回退到精确命令。文件写入工具共享单个 Edit 授权。
func SessionGrantKey(toolName, subject string) string {
	return SessionGrantRuleForScope(toolName, subject)
}

// SessionGrantRuleForScope 返回会话授权的内存规则。
// bash 优先使用命令前缀；文件写入工具共享单个 Edit 授权；其他工具返回裸工具名。
func SessionGrantRuleForScope(toolName, subject string) string {
	subject = strings.TrimSpace(subject)
	if toolName == "bash" && subject != "" {
		if pattern := BashCommandPrefix(subject); pattern != "" {
			return "Bash(" + pattern + ")"
		}
		return "Bash(" + subject + ")"
	}
	if IsFileMutationTool(toolName) {
		return "Edit"
	}
	return toolName
}

// BashCommandPrefix 为 bash 命令生成保守的前缀规则，用于"类似命令"的审批。
//
// 规则：
//   - 避免 shell 语法（管道、重定向等）
//   - 保持前缀在命令词边界，例如 "go test ./..." 生成 "go test:*" 而非更宽泛的 "go *"
//   - 包管理器的 "run" 子命令保留三段前缀（如 "npm run build:*"）
//   - 危险命令不生成前缀规则
func BashCommandPrefix(subject string) string {
	cmd := strings.TrimSpace(subject)
	if cmd == "" || containsShellSyntax(cmd) {
		return ""
	}
	if BashDangerWarning(cmd) != "" {
		return ""
	}
	fields := strings.Fields(cmd)
	if len(fields) < 2 {
		return ""
	}
	base := strings.ToLower(fields[0])
	if isPackageManagerRun(base) && len(fields) >= 3 && strings.ToLower(fields[1]) == "run" {
		return fields[0] + " " + fields[1] + " " + fields[2] + ":*"
	}
	return fields[0] + " " + fields[1] + ":*"
}

func isPackageManagerRun(base string) bool {
	switch base {
	case "npm", "pnpm", "yarn", "bun":
		return true
	default:
		return false
	}
}

// IsFileMutationTool 判断内置工具是否会修改工作区文件。
func IsFileMutationTool(toolName string) bool {
	switch toolName {
	case "write_file", "edit_file", "multi_edit", "move_file", "notebook_edit", "delete_range", "delete_symbol":
		return true
	default:
		return false
	}
}

func ruleToolMatches(ruleTool, toolName string) bool {
	ruleTool = canonicalRuleTool(ruleTool)
	return ruleTool == toolName || (ruleTool == "file_mutation" && IsFileMutationTool(toolName))
}

func ruleToolCompatible(existingTool, candidateTool string) bool {
	existingTool = canonicalRuleTool(existingTool)
	candidateTool = canonicalRuleTool(candidateTool)
	return existingTool == candidateTool ||
		(existingTool == "file_mutation" && (candidateTool == "file_mutation" || IsFileMutationTool(candidateTool)))
}

func canonicalRuleTool(toolName string) string {
	switch strings.TrimSpace(toolName) {
	case "Bash", "bash":
		return "bash"
	case "Edit", "edit", "file_mutation":
		return "file_mutation"
	default:
		return toolName
	}
}

func ruleSubjectMatches(rule Rule, subject string) bool {
	if rule.Subject == "" {
		return true
	}
	if subject == "" {
		return false
	}
	if rule.Literal {
		return rule.Subject == subject
	}
	if canonicalRuleTool(rule.Tool) == "bash" {
		if base, ok := bashColonPrefixBase(rule.Subject); ok {
			return bashPrefixMatches(base, subject)
		}
		if base, ok := legacyBashSpaceStarPrefixBase(rule.Subject); ok {
			return bashPrefixMatches(base, subject)
		}
	}
	return matchGlob(rule.Subject, subject)
}

func bashColonPrefixBase(pattern string) (string, bool) {
	if !strings.HasSuffix(pattern, ":*") {
		return "", false
	}
	base := strings.TrimSuffix(pattern, ":*")
	return base, base != ""
}

func legacyBashSpaceStarPrefixBase(pattern string) (string, bool) {
	if !strings.HasSuffix(pattern, " *") {
		return "", false
	}
	base := strings.TrimSuffix(pattern, " *")
	return base, base != ""
}

func bashPrefixBase(pattern string) (string, bool) {
	if base, ok := bashColonPrefixBase(pattern); ok {
		return base, true
	}
	return legacyBashSpaceStarPrefixBase(pattern)
}

func bashPrefixMatches(base, subject string) bool {
	if containsShellSyntax(subject) {
		return false
	}
	return subject == base || strings.HasPrefix(subject, base+" ")
}
