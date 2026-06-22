// 文件：auto_plan.go
//
// 自动计划模式——判断用户输入是否需要先进行只读计划。
// 当用户提交复杂任务时，自动进入计划模式，让模型先研究代码库并提出分层计划，
// 用户批准后再执行。本文件包含启发式评分、LLM 分类器集成和计划模式门控逻辑。
package control

import (
	"context"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"reasonix/internal/agent"
	"reasonix/internal/nilutil"
)

const (
	autoPlanOff = "off"
	autoPlanOn  = "on"
)

var numberedListRE = regexp.MustCompile(`(?m)^\s*(?:[-*]|\d+[.)])\s+\S`)

// AutoPlanClassifier 是自动计划分类器的接口。当启发式评分不够确定时，
// 通过 LLM 调用来判断用户输入是否需要进入计划模式。
type AutoPlanClassifier interface {
	// NeedsPlan 判断给定输入是否需要计划模式。输入包括用户文本和启发式评分。
	// 返回 (是否需要计划, 原因说明, 错误)。
	NeedsPlan(ctx context.Context, input string, score int) (bool, string, error)
}

type autoPlanClassifier = AutoPlanClassifier

func normalizeAutoPlan(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case autoPlanOn, "ask": // "ask" is a legacy synonym for on.
		return autoPlanOn
	case "", autoPlanOff:
		return autoPlanOff
	default:
		return autoPlanOff
	}
}

// maybeAutoPlan 在每个轮次开始时检查是否应自动进入计划模式。
// 如果启发式评分和可选的 LLM 分类器都认为输入是多步骤任务，
// 则自动开启计划模式，让模型先研究代码库并提出计划。
func (c *Controller) maybeAutoPlan(ctx context.Context, input string) {
	if c.shouldAutoPlan(ctx, input) {
		c.SetPlanMode(true)
		c.notice("auto plan: task looks multi-step; drafting a plan first")
	}
}

func (c *Controller) shouldAutoPlan(ctx context.Context, input string) bool {
	c.mu.Lock()
	mode := c.autoPlan
	plan := c.planMode
	classifier := c.classifier
	c.mu.Unlock()
	if mode == autoPlanOff || plan || c.goals.active() {
		return false
	}
	score := autoPlanScore(input)
	if score <= 0 {
		return false
	}
	if classifier != nil && score <= 2 {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		needsPlan, reason, err := classifier.NeedsPlan(ctx, input, score)
		if err == nil {
			if needsPlan && reason != "" {
				c.notice("auto plan classifier: " + reason)
			}
			return needsPlan
		}
		c.notice("auto plan classifier failed; falling back to heuristic: " + err.Error())
	}
	return score >= 2
}

// TaskWarrantsPlanner reports whether a task turn is worth a planner pass in
// two-model mode. Empty input, slash commands, and low-risk informational asks
// (explain / show / what / why / 解释 / 查一下 …) skip straight to the executor;
// anything that reads like a work request — even a terse one — still gets planned.
func TaskWarrantsPlanner(input string) bool {
	text := strings.TrimSpace(agent.StripTransientUserBlocks(input))
	text = stripActiveGoalBlock(text)
	if text == "" || strings.HasPrefix(text, "/") || strings.HasPrefix(text, PlanModeMarker) {
		return false
	}
	if IsSyntheticUserMessage(text) {
		return false
	}
	return !isLowRiskQuestion(strings.ToLower(text))
}

func autoPlanScore(input string) int {
	text := strings.TrimSpace(input)
	text = stripActiveGoalBlock(text)
	if text == "" || strings.HasPrefix(text, "/") || strings.HasPrefix(text, PlanModeMarker) {
		return 0
	}
	if IsSyntheticUserMessage(text) {
		return 0
	}
	lower := strings.ToLower(text)
	if isLowRiskQuestion(lower) {
		return 0
	}

	score := 0
	if utf8.RuneCountInString(text) >= 160 {
		score++
	}
	if numberedListRE.MatchString(text) {
		score++
	}
	if strings.Count(text, "\n") >= 2 {
		score++
	}
	if containsAny(lower, complexIntentTerms) {
		score++
	}
	if containsAny(lower, multiSurfaceTerms) {
		score++
	}
	if containsAny(lower, docsAndIssueTerms) {
		score++
	}
	if strings.Count(text, "@") >= 2 || strings.Count(lower, ".go")+
		strings.Count(lower, ".ts")+strings.Count(lower, ".tsx")+strings.Count(lower, ".js") >= 2 {
		score++
	}
	return score
}

func isLowRiskQuestion(lower string) bool {
	lower = strings.TrimSpace(lower)
	normalized := strings.ReplaceAll(lower, "'", "")
	if strings.HasPrefix(lower, "what ") || strings.HasPrefix(normalized, "whats ") ||
		strings.HasPrefix(lower, "why ") || strings.HasPrefix(lower, "how ") ||
		strings.HasPrefix(lower, "who ") || strings.HasPrefix(lower, "where ") ||
		strings.HasPrefix(lower, "when ") || strings.HasPrefix(lower, "which ") ||
		strings.HasPrefix(lower, "whose ") || strings.HasPrefix(lower, "whom ") ||
		strings.HasPrefix(lower, "explain ") || strings.HasPrefix(lower, "describe ") ||
		strings.HasPrefix(lower, "tell ") || strings.HasPrefix(lower, "show ") ||
		strings.HasPrefix(lower, "list ") || strings.HasPrefix(lower, "summarize ") ||
		strings.HasPrefix(lower, "summarise ") || strings.HasPrefix(lower, "compare ") ||
		strings.HasPrefix(lower, "difference ") || strings.HasPrefix(lower, "is ") ||
		strings.HasPrefix(lower, "are ") || strings.HasPrefix(lower, "can ") ||
		strings.HasPrefix(lower, "could ") || strings.HasPrefix(lower, "do ") ||
		strings.HasPrefix(lower, "does ") || strings.HasPrefix(lower, "did ") ||
		strings.HasPrefix(lower, "should ") || strings.HasPrefix(lower, "would ") ||
		strings.HasPrefix(lower, "will ") || strings.HasPrefix(lower, "run ") ||
		strings.HasPrefix(lower, "what's") || strings.HasPrefix(normalized, "whats") ||
		strings.HasPrefix(lower, "解释") || strings.HasPrefix(lower, "说明") ||
		strings.HasPrefix(lower, "怎么看") || strings.HasPrefix(lower, "查一下") ||
		strings.HasPrefix(lower, "运行") || strings.HasPrefix(lower, "介绍一下") ||
		strings.HasPrefix(lower, "说一下") || strings.HasPrefix(lower, "帮我看") ||
		strings.HasPrefix(lower, "帮我查") || strings.HasPrefix(lower, "是什么") ||
		strings.HasPrefix(lower, "有没有") || strings.HasPrefix(lower, "能不能") ||
		strings.HasPrefix(lower, "可以吗") || strings.HasPrefix(lower, "对吗") ||
		strings.HasPrefix(lower, "是不是") || strings.HasPrefix(lower, "请问") {
		return !containsAny(lower, complexIntentTerms) && !containsAny(lower, lowRiskWorkRequestTerms)
	}
	return false
}

func stripActiveGoalBlock(text string) string {
	const open = "<active-goal>"
	const close = "</active-goal>"
	if !strings.Contains(text, open) {
		return text
	}
	end := strings.Index(text, close)
	if end < 0 {
		return text
	}
	after := strings.TrimSpace(text[end+len(close):])
	if after == "" {
		return text
	}
	return after
}

func containsAny(s string, terms []string) bool {
	for _, term := range terms {
		if strings.Contains(s, term) {
			return true
		}
	}
	return false
}

var complexIntentTerms = []string{
	"implement", "add support", "refactor", "migrate", "redesign", "end-to-end",
	"e2e", "wire up", "integration", "fix the issue", "build a",
	"实现", "新增", "支持", "重构", "迁移", "改造", "端到端", "联调", "接入",
	"修复这个问题", "修一下这个问题", "补齐", "设计",
}

var lowRiskWorkRequestTerms = []string{
	"fix", "update", "remove", "delete", "edit", "write", "create", "add ",
	"repair", "patch", "run ", "build", "修改", "修复", "更新", "删除", "移除",
	"编辑", "写入", "创建", "新增", "运行", "构建",
}

var multiSurfaceTerms = []string{
	"multiple files", "several files", "across", "frontend", "backend", "config",
	"tests", "docs", "ui", "api", "database", "schema",
	"多个文件", "多处", "前端", "后端", "配置", "测试", "文档", "接口", "数据库",
}

var docsAndIssueTerms = []string{
	"prd", "issue", "requirements", "spec", "proposal", "roadmap",
	"需求", "产品文档", "接口文档", "方案", "规划",
}

func NewPlannerGate(classifier AutoPlanClassifier) func(string) bool {
	if nilutil.IsNil(classifier) {
		return TaskWarrantsPlanner
	}
	return func(input string) bool {
		if !TaskWarrantsPlanner(input) {
			return false
		}
		score := autoPlanScore(input)
		if score <= 2 {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			needsPlan, _, err := classifier.NeedsPlan(ctx, input, score)
			if err == nil {
				return needsPlan
			}
		}
		return true
	}
}
