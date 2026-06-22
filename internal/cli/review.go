// review.go 实现了 `reasonix review` 命令，用于对 Git 变更进行代码审查。
// 它获取 diff、加载配置和模型、构建审查子代理并执行审查任务。
// 支持审查未提交的工作区变更、指定 commit 或指定 base 分支的差异。
package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/event"
	"reasonix/internal/skill"
	"reasonix/internal/tool"
)

// reviewCommand 是 `reasonix review` 的入口函数。它解析命令行参数，
// 获取 Git diff，加载配置和模型，构建审查子代理并运行代码审查任务。
// 支持 --base、--commit、--model 和 --instructions 参数。
func reviewCommand(args []string) int {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	base := fs.String("base", "", "base branch/commit to diff against (defaults to HEAD — reviews uncommitted working-tree changes)")
	commit := fs.String("commit", "", "review a specific commit (shows changes introduced by that commit)")
	model := fs.String("model", "", "provider name override (default: config default_model)")
	instructions := fs.String("instructions", "", "extra review instructions appended to the prompt")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// 1. Get the diff.
	diff, err := getReviewDiff(*base, *commit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if diff == "" {
		fmt.Println("No changes to review.")
		return 0
	}

	// 2. Load config and resolve model.
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: failed to load config:", err)
		return 1
	}
	modelName := *model
	if modelName == "" {
		modelName = cfg.DefaultModel
	}
	entry, ok := cfg.ResolveModel(modelName)
	if !ok {
		fmt.Fprintf(os.Stderr, "error: unknown model %q — check your config\n", modelName)
		return 1
	}
	if err := cfg.Validate(modelName); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	// 3. Create provider.
	prov, err := boot.NewProviderWithProxy(entry, cfg.NetworkProxySpec())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: failed to create provider:", err)
		return 1
	}

	// 4. Get the built-in review skill.
	root, _ := os.Getwd()
	skillStore := skill.New(skill.Options{ProjectRoot: root, Stderr: os.Stderr})
	reviewSk, ok := skillStore.Read("review")
	if !ok {
		fmt.Fprintln(os.Stderr, "error: built-in review skill not found")
		return 1
	}
	if reviewSk.RunAs != skill.RunSubagent {
		fmt.Fprintln(os.Stderr, "error: review skill is not a subagent skill")
		return 1
	}

	// 5. Build a review-scoped sub-agent registry.
	reg := buildReviewSubagentRegistry(reviewSk)

	// 6. Prepare the review prompt.
	task := buildReviewTask(diff, *instructions)

	// 7. Run the review subagent.
	ctx := context.Background()
	result, err := agent.RunSubAgentWithSession(ctx, prov, reg, agent.NewSession(reviewSk.Body), task, agent.Options{
		MaxSteps:      12,
		Temperature:   cfg.Agent.Temperature,
		Pricing:       entry.Price,
		ContextWindow: entry.ContextWindow,
	}, event.Discard)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: review failed:", err)
		return 1
	}

	fmt.Print(result)
	return 0
}

// buildReviewSubagentRegistry 构建审查子代理的工具注册表。
// 它从审查技能的允许工具列表中构建父注册表，
// 然后通过 agent.SubagentToolRegistry 过滤掉子代理不可用的后台能力。
func buildReviewSubagentRegistry(reviewSk skill.Skill) *tool.Registry {
	// The shared helper strips subagent-unavailable background capabilities while
	// preserving foreground bash. This direct CLI path does not go through boot,
	// so it first builds the small parent set from the review skill allow-list.
	parentReg := tool.NewRegistry()
	for _, name := range reviewSk.AllowedTools {
		if tl, ok := tool.LookupBuiltin(name); ok {
			parentReg.Add(tl)
		}
	}
	return agent.SubagentToolRegistry(parentReg, reviewSk.AllowedTools)
}

// getReviewDiff 执行适当的 git diff 命令并返回输出。
// - commit="abc": 显示 abc^..abc 的差异
// - base="main": 显示 main...HEAD 的差异
// - 两者都为空: 显示未提交的工作区变更（暂存区 + 非暂存区）
func getReviewDiff(base, commit string) (string, error) {
	cwd, _ := os.Getwd()
	ctx := context.Background()
	switch {
	case commit != "":
		return runGit(ctx, cwd, "diff", commit+"^.."+commit)
	case base != "":
		return runGit(ctx, cwd, "diff", base+"...HEAD")
	default:
		// Working tree changes: staged + unstaged.
		out, err := runGit(ctx, cwd, "diff", "HEAD")
		if err != nil {
			return "", err
		}
		if out == "" {
			// No working-tree changes; check for staged-only.
			out, err = runGit(ctx, cwd, "diff", "--cached")
		}
		return out, err
	}
}

// buildReviewTask 构建审查提示词，将 diff 内容和额外指令组合为发送给子代理的任务文本。
// 超大 diff 会被截断以保护审查子代理的上下文预算。
func buildReviewTask(diff string, extra string) string {
	var b strings.Builder
	b.WriteString("Review the following changes. ")
	if extra != "" {
		b.WriteString(extra)
		b.WriteString(" ")
	}
	b.WriteString("The diff is:\n\n```diff\n")
	// Truncate huge diffs to protect the review subagent's context budget.
	const maxLen = 16000
	if len(diff) > maxLen {
		b.WriteString(diff[:maxLen])
		b.WriteString("\n```\n\n(diff truncated at ")
		fmt.Fprint(&b, maxLen)
		b.WriteString(" chars — focus on the changes shown)")
	} else {
		b.WriteString(diff)
		b.WriteString("\n```")
	}
	return b.String()
}
