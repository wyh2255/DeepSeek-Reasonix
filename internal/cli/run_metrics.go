// run_metrics.go 实现了运行指标的收集和持久化功能。
// metricsSink 作为事件接收器，累计每次模型调用的 token 使用量、缓存命中率和成本，
// 最终通过 writeMetrics 将结构化的 RunMetrics 写入 JSON 文件，
// 供基准测试工具读取而无需解析标准输出。
package cli

import (
	"encoding/json"
	"os"

	"reasonix/internal/event"
	"reasonix/internal/evidence"
)

// RunMetrics 是机器可读的 token/缓存/成本汇总结构体，
// 由 `run --metrics` 写入，供基准测试工具读取运行成本而无需解析标准输出。
type RunMetrics struct {
	PromptTokens                  int     `json:"prompt_tokens"`
	CompletionTokens              int     `json:"completion_tokens"`
	CacheHitTokens                int     `json:"cache_hit_tokens"`
	CacheMissTokens               int     `json:"cache_miss_tokens"`
	Steps                         int     `json:"steps"` // model calls (one per stream, incl. tool rounds)
	Cost                          float64 `json:"cost"`
	Currency                      string  `json:"currency"`
	Compactions                   int     `json:"compactions"`
	ReadinessChecks               int     `json:"readiness_checks"`
	ReadinessAllowed              int     `json:"readiness_allowed"`
	ReadinessBlocks               int     `json:"readiness_blocks"`
	ReadinessRecoveries           int     `json:"readiness_recoveries"`
	ReadinessErrors               int     `json:"readiness_errors"`
	ReadinessMissingProjectChecks int     `json:"readiness_missing_project_checks"`
	ReadinessIncompleteTodos      int     `json:"readiness_incomplete_todos"`
	ReadinessCommandMismatches    int     `json:"readiness_command_mismatches"`
}

// metricsSink 将每个事件转发给真实的 sink，同时将每次调用的 Usage 事件
// 累积到 RunMetrics 中。缓存总量按每次调用求和（而非从累计的 SessionHit/Miss 读取），
// 使其与 PromptTokens 精确匹配。
type metricsSink struct {
	inner event.Sink
	m     RunMetrics
}

// Emit 处理每个事件：累计 Usage 事件中的 token 计数和成本，
// 统计 CompactionStarted 事件，并将事件转发给内部的真实 sink。
func (s *metricsSink) Emit(e event.Event) {
	if e.Kind == event.Usage && e.Usage != nil {
		u := e.Usage
		s.m.PromptTokens += u.PromptTokens
		s.m.CompletionTokens += u.CompletionTokens
		s.m.CacheHitTokens += u.CacheHitTokens
		s.m.CacheMissTokens += u.CacheMissTokens
		s.m.Steps++
		if p := e.Pricing; p != nil {
			s.m.Cost += (float64(u.CacheHitTokens)*p.CacheHit +
				float64(u.CacheMissTokens)*p.Input +
				float64(u.CompletionTokens)*p.Output) / 1e6
			s.m.Currency = p.Currency
		}
	}
	if e.Kind == event.CompactionStarted {
		s.m.Compactions++
	}
	s.inner.Emit(e)
}

// RecordReadinessAudit 累积就绪性审计的统计数据，包括检查次数、
// 允许/阻止/错误计数、恢复次数以及缺失项目检查等指标。
func (s *metricsSink) RecordReadinessAudit(a evidence.ReadinessAudit) {
	if s == nil {
		return
	}
	s.m.ReadinessChecks++
	switch a.Result {
	case evidence.ReadinessAllowed:
		s.m.ReadinessAllowed++
	case evidence.ReadinessBlocked:
		s.m.ReadinessBlocks++
	case evidence.ReadinessErrored:
		s.m.ReadinessErrors++
	}
	if a.Recovered {
		s.m.ReadinessRecoveries++
	}
	s.m.ReadinessMissingProjectChecks += a.MissingProjectChecks
	s.m.ReadinessIncompleteTodos += a.IncompleteTodos
	s.m.ReadinessCommandMismatches += a.CommandMismatchMissing
}

// writeMetrics 将 RunMetrics 以格式化的 JSON 写入指定文件路径。
func writeMetrics(path string, m RunMetrics) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
