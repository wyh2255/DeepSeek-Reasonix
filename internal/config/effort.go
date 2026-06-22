// effort.go 实现了推理努力级别（Reasoning Effort）的配置和管理。
//
// 推理努力级别控制模型在生成回复时投入的推理深度。
// 不同的 AI 提供商使用不同的协议来表达这一概念：
//   - DeepSeek: thinking.type 字段（high/max）
//   - OpenAI: reasoning_effort 字段（low/medium/high）
//   - Anthropic: extended thinking budget（low/medium/high/xhigh/max）
//   - MiniMax: thinking 开关（adaptive/disabled）
//
// 用户通过 /effort 命令设置级别，配置层负责规范化、验证和映射到具体提供商的协议。
package config

import (
	"fmt"
	"strings"

	"reasonix/internal/provider/openai"
)

// 推理协议常量，标识提供商使用的推理控制协议。
const (
	ReasoningProtocolAuto     = "auto"     // 自动检测（根据提供商类型和模型名推断）
	ReasoningProtocolDeepSeek = "deepseek" // DeepSeek 协议（thinking.type）
	ReasoningProtocolOpenAI   = "openai"   // OpenAI 协议（reasoning_effort）
	ReasoningProtocolNone     = "none"     // 不支持推理控制
)

// EffortCapability 描述提供商/模型通过 /effort 命令可设置的抽象努力级别。
type EffortCapability struct {
	Supported bool     // 是否支持努力级别配置
	Levels    []string // 可用的级别列表（包含 "auto"）
	Default   string   // 默认级别
}

// modelReasoningCapability 描述特定模型的推理能力配置。
type modelReasoningCapability struct {
	Protocol string   // 使用的推理协议
	Levels   []string // 支持的级别
	Default  string   // 默认级别
}

// modelReasoningCapabilities 是已知模型的推理能力注册表。
// 用于在没有显式配置时自动推断模型的推理能力。
var modelReasoningCapabilities = map[string]modelReasoningCapability{
	"deepseek-v4-flash": {Protocol: ReasoningProtocolDeepSeek, Levels: []string{"high", "max"}, Default: "high"},
	"deepseek-v4-pro":   {Protocol: ReasoningProtocolDeepSeek, Levels: []string{"high", "max"}, Default: "high"},
}

// EffortCapabilityForEntry 返回已解析的提供商条目对应的用户可见 /effort 级别。
// 提供商实现仍然决定如何将存储的 effort 值序列化为请求参数。
//
// 优先级：显式配置 > 模型能力注册表 > 端点类型推断 > 通用默认值
func EffortCapabilityForEntry(e *ProviderEntry) EffortCapability {
	if explicitReasoningProtocol(e) == ReasoningProtocolNone {
		return EffortCapability{}
	}
	supported := normalizedSupportedEfforts(e)
	if len(supported) > 0 {
		levels := make([]string, 0, len(supported)+1)
		levels = append(levels, "auto")
		levels = append(levels, supported...)
		def := normalizeEffortLevel(e.DefaultEffort)
		if def == "" || !containsString(supported, def) {
			def = supported[0]
		}
		return EffortCapability{Supported: true, Levels: levels, Default: def}
	}
	switch explicitReasoningProtocol(e) {
	case ReasoningProtocolDeepSeek:
		return deepSeekEffortCapability()
	case ReasoningProtocolOpenAI:
		return openAIEffortCapability()
	}
	if cap, ok := resolvedModelReasoningCapability(e); ok {
		return effortCapabilityFromModel(cap)
	}
	switch ReasoningProtocolForEntry(e) {
	case ReasoningProtocolDeepSeek:
		return deepSeekEffortCapability()
	case ReasoningProtocolOpenAI:
		return openAIEffortCapability()
	}
	switch {
	case isMiniMaxEntry(e):
		// MiniMax-M3 only exposes a binary thinking knob (adaptive|disabled)
		// on its OpenAI-compatible endpoint, so /effort mirrors the API
		// vocabulary verbatim. Default is "adaptive" because the M3 model
		// runs with thinking on out of the box; "auto" means "don't override
		// the model default" (== adaptive for M3).
		return EffortCapability{Supported: true, Levels: []string{"auto", "adaptive", "disabled"}, Default: "adaptive"}
	case e != nil && e.Kind == "anthropic":
		return EffortCapability{Supported: true, Levels: []string{"auto", "low", "medium", "high", "xhigh", "max"}, Default: "auto"}
	default:
		return EffortCapability{}
	}
}

// NormalizeEffort 将用户通过 /effort 命令输入的级别映射为存储在配置中的值。
// 空字符串表示使用自动/提供商默认值。
//
// 不同提供商的级别映射：
//   - DeepSeek: high/max（low/medium 映射到 high，xhigh 映射到 max）
//   - OpenAI: low/medium/high
//   - MiniMax: adaptive/disabled（其他级别映射到最近的有效值）
//   - Anthropic: low/medium/high/xhigh/max
func NormalizeEffort(e *ProviderEntry, raw string) (string, error) {
	level := normalizeEffortLevel(raw)
	if level == "" {
		return "", fmt.Errorf("usage: /effort auto|<level>")
	}
	if level == "auto" {
		return "", nil
	}
	if explicitReasoningProtocol(e) == ReasoningProtocolNone {
		return "", effortNotConfigurableError(e)
	}
	supported := normalizedSupportedEfforts(e)
	if len(supported) > 0 {
		if containsString(supported, level) {
			return level, nil
		}
		return "", fmt.Errorf("usage: /effort auto|%s", strings.Join(supported, "|"))
	}
	switch ReasoningProtocolForEntry(e) {
	case ReasoningProtocolDeepSeek:
		switch level {
		case "high", "max":
			return level, nil
		case "low", "medium":
			return "high", nil
		case "xhigh":
			return "max", nil
		default:
			return "", fmt.Errorf("usage: /effort auto|high|max")
		}
	case ReasoningProtocolOpenAI:
		switch level {
		case "low", "medium", "high":
			return level, nil
		default:
			return "", fmt.Errorf("usage: /effort auto|low|medium|high")
		}
	}
	switch {
	case isMiniMaxEntry(e):
		// The M3 knob is binary; map Anthropic / OpenAI-style levels onto the
		// nearest valid value so a stale /effort high|low still works. "off"
		// is a retired DeepSeek level meaning "no thinking" — on M3 that maps
		// to "disabled" rather than the model default, since M3 actually
		// supports a "thinking off" mode and "off" is the natural request.
		switch level {
		case "adaptive", "disabled":
			return level, nil
		case "off":
			return "disabled", nil
		case "low", "medium", "high":
			return "adaptive", nil
		case "xhigh", "max":
			return "disabled", nil
		default:
			return "", fmt.Errorf("usage: /effort auto|adaptive|disabled")
		}
	case e != nil && e.Kind == "anthropic":
		switch level {
		case "low", "medium", "high", "xhigh", "max":
			return level, nil
		default:
			return "", fmt.Errorf("usage: /effort auto|low|medium|high|xhigh|max")
		}
	default:
		return "", effortNotConfigurableError(e)
	}
}

// EffortDisplay 返回当前选中的 /effort 级别，提供商默认值显示为 "auto"。
func EffortDisplay(e *ProviderEntry) string {
	if e == nil || strings.TrimSpace(e.Effort) == "" {
		return "auto"
	}
	return normalizeEffortLevel(e.Effort)
}

// EffectiveEffort 解析提供商可见的 effort 值。
//
// 优先级：
//  1. 显式的 ProviderEntry.Effort（用户通过 /effort 设置）
//  2. 配置的 SupportedEfforts 列表中的 DefaultEffort（或第一个支持的级别）
//  3. 空字符串 = 使用提供商默认值 / 省略提供商特定的 effort 字段
func EffectiveEffort(e *ProviderEntry) string {
	if e == nil {
		return ""
	}
	if effort := normalizeStoredEffort(e.Effort); effort != "" {
		return effort
	}
	supported := normalizedSupportedEfforts(e)
	if len(supported) == 0 {
		return ""
	}
	def := normalizeEffortLevel(e.DefaultEffort)
	if def == "" || !containsString(supported, def) {
		return supported[0]
	}
	return def
}

func normalizeEffortConfig(c *Config) {
	if c == nil {
		return
	}
	for i := range c.Providers {
		normalizeProviderEffortFields(&c.Providers[i])
	}
}

func normalizeProviderEffortFields(e *ProviderEntry) {
	if e == nil {
		return
	}
	e.Effort = normalizeStoredEffort(e.Effort)
	e.ReasoningProtocol = normalizeReasoningProtocol(e.ReasoningProtocol)
	e.DefaultEffort = normalizeEffortLevel(e.DefaultEffort)
	e.SupportedEfforts = normalizedSupportedEfforts(e)
}

func normalizeStoredEffort(raw string) string {
	level := normalizeEffortLevel(raw)
	if level == "auto" || level == "off" {
		return ""
	}
	return level
}

// ReasoningProtocolForEntry 解析提供商的推理控制协议。
//
// 优先级：
//  1. 显式配置的 ReasoningProtocol
//  2. 模型能力注册表中的协议
//  3. 旧版端点启发式检测（如 DeepSeek API 端点）
func ReasoningProtocolForEntry(e *ProviderEntry) string {
	if explicit := explicitReasoningProtocol(e); explicit != "" {
		return explicit
	}
	if cap, ok := resolvedModelReasoningCapability(e); ok {
		return cap.Protocol
	}
	if isDeepSeekEntry(e) {
		return ReasoningProtocolDeepSeek
	}
	return ""
}

func explicitReasoningProtocol(e *ProviderEntry) string {
	if e == nil {
		return ""
	}
	protocol := normalizeReasoningProtocol(e.ReasoningProtocol)
	if protocol == ReasoningProtocolAuto {
		return ""
	}
	return protocol
}

func normalizeReasoningProtocol(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", ReasoningProtocolAuto:
		return ""
	case ReasoningProtocolDeepSeek, ReasoningProtocolOpenAI, ReasoningProtocolNone:
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return ""
	}
}

// isDeepSeekEntry reports whether the entry points at DeepSeek's API. The
// actual host matching lives in provider/openai so the openai package and
// the config layer stay in lockstep when new gateways are added.
func isDeepSeekEntry(e *ProviderEntry) bool {
	return e != nil && e.Kind == "openai" && openai.IsDeepSeek(e.BaseURL)
}

// isMiniMaxEntry reports whether the entry points at MiniMax's OpenAI-compatible
// endpoint. See openai.IsMiniMax for the host-matching rule; the entry-wrapper
// just gates on the openai kind.
func isMiniMaxEntry(e *ProviderEntry) bool {
	return e != nil && e.Kind == "openai" && openai.IsMiniMax(e.BaseURL)
}

func resolvedModelReasoningCapability(e *ProviderEntry) (modelReasoningCapability, bool) {
	if e == nil || e.Kind != "openai" {
		return modelReasoningCapability{}, false
	}
	cap, ok := modelReasoningCapabilities[strings.ToLower(strings.TrimSpace(e.Model))]
	return cap, ok
}

func effortCapabilityFromModel(cap modelReasoningCapability) EffortCapability {
	levels := make([]string, 0, len(cap.Levels)+1)
	levels = append(levels, "auto")
	levels = append(levels, cap.Levels...)
	def := normalizeEffortLevel(cap.Default)
	if def == "" || !containsString(cap.Levels, def) {
		def = "auto"
	}
	return EffortCapability{Supported: true, Levels: levels, Default: def}
}

func deepSeekEffortCapability() EffortCapability {
	return EffortCapability{Supported: true, Levels: []string{"auto", "high", "max"}, Default: "high"}
}

func openAIEffortCapability() EffortCapability {
	return EffortCapability{Supported: true, Levels: []string{"auto", "low", "medium", "high"}, Default: "auto"}
}

func effortNotConfigurableError(e *ProviderEntry) error {
	name := ""
	if e != nil {
		name = e.Name
	}
	if name == "" {
		name = "this model"
	}
	return fmt.Errorf("effort is not configurable for %s", name)
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func normalizeEffortLevel(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func normalizedSupportedEfforts(e *ProviderEntry) []string {
	if e == nil || len(e.SupportedEfforts) == 0 {
		return nil
	}
	out := make([]string, 0, len(e.SupportedEfforts))
	seen := map[string]bool{}
	for _, raw := range e.SupportedEfforts {
		level := normalizeEffortLevel(raw)
		if level == "" || level == "auto" || seen[level] {
			continue
		}
		seen[level] = true
		out = append(out, level)
	}
	return out
}
