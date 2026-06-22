// 文件：auto_plan_classifier.go
//
// 基于 LLM 的自动计划分类器实现。
// 当启发式评分不确定（score <= 2）时，通过一个小型 LLM 调用来判断
// 用户输入是否需要进入计划模式。使用极低的 token 预算（max_tokens=80）
// 和零温度以获得确定性结果。
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"reasonix/internal/event"
	"reasonix/internal/nilutil"
	"reasonix/internal/provider"
)

const autoPlanClassifierPrompt = `You classify whether a coding-agent user request should first enter read-only planning mode.
Return ONLY JSON: {"needs_plan":true|false,"reason":"short reason"}.
Use true for multi-step implementation, refactors, migrations, unclear cross-file work, PRD/spec/issue work, or tasks needing investigation before edits.
Use false for explanations, simple questions, single obvious edits, direct commands, or requests that should be answered without changing files.`

// ProviderAutoPlanClassifier 通过 LLM 提供商调用来判断是否需要计划模式。
// 它使用一个专门的系统提示，要求模型返回 JSON 格式的判断结果。
type ProviderAutoPlanClassifier struct {
	prov    provider.Provider // LLM 提供商，用于发起分类请求
	pricing *provider.Pricing // 定价信息，用于计费事件
	sink    event.Sink        // 事件发射器，用于报告分类器的 token 使用量
}

func NewProviderAutoPlanClassifier(prov provider.Provider) *ProviderAutoPlanClassifier {
	if nilutil.IsNil(prov) {
		return nil
	}
	return &ProviderAutoPlanClassifier{prov: prov}
}

func NewBillableProviderAutoPlanClassifier(prov provider.Provider, pricing *provider.Pricing, sink event.Sink) *ProviderAutoPlanClassifier {
	if nilutil.IsNil(prov) {
		return nil
	}
	if nilutil.IsNil(sink) {
		sink = event.Discard
	}
	return &ProviderAutoPlanClassifier{prov: prov, pricing: pricing, sink: sink}
}

func (c *ProviderAutoPlanClassifier) NeedsPlan(ctx context.Context, input string, score int) (bool, string, error) {
	if c == nil || nilutil.IsNil(c.prov) {
		return false, "", fmt.Errorf("auto plan classifier is not initialized")
	}
	ch, err := c.prov.Stream(ctx, provider.Request{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: autoPlanClassifierPrompt},
			{Role: provider.RoleUser, Content: fmt.Sprintf("heuristic_score=%d\n\nUSER_REQUEST:\n%s", score, input)},
		},
		Temperature: 0,
		MaxTokens:   80,
	})
	if err != nil {
		return false, "", err
	}

	var text strings.Builder
	var usage *provider.Usage
	for chunk := range ch {
		switch chunk.Type {
		case provider.ChunkText:
			text.WriteString(chunk.Text)
		case provider.ChunkUsage:
			usage = chunk.Usage
		case provider.ChunkError:
			return false, "", chunk.Err
		}
	}
	if usage != nil && usage.TotalTokens > 0 && !nilutil.IsNil(c.sink) {
		c.sink.Emit(event.Event{Kind: event.Usage, Usage: usage, Pricing: c.pricing, UsageSource: event.UsageSourceClassifier})
	}

	var out struct {
		NeedsPlan *bool  `json:"needs_plan"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(text.String())), &out); err != nil {
		return false, "", fmt.Errorf("decode classifier response: %w", err)
	}
	if out.NeedsPlan == nil {
		return false, "", fmt.Errorf("decode classifier response: missing needs_plan")
	}
	return *out.NeedsPlan, strings.TrimSpace(out.Reason), nil
}

func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start >= 0 && end >= start {
		return s[start : end+1]
	}
	return s
}
