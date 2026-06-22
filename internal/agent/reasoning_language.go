// reasoning_language.go 实现了推理语言偏好系统。
//
// 推理语言偏好控制 AI 模型在可见的思维链（reasoning/thinking）文本中使用的语言。
// 当提供商暴露推理文本时（如 DeepSeek 的 thinking 内容），该偏好会注入到对话中，
// 引导模型使用指定语言进行推理。
//
// 关键设计：
//   - 偏好以 transient 用户轮次上下文注入，不属于稳定的系统提示
//   - 注入到对话尾部以保持缓存稳定（避免破坏 prompt cache）
//   - 支持通过 context 传递给子 Agent，保持跨 Agent 的一致性
//   - 代码、标识符、文件路径、shell 命令和技术术语始终保持原文
package agent

import (
	"context"
	"strings"
)

// reasoningLanguageContextKey 是 context 中推理语言偏好的存储键。
type reasoningLanguageContextKey struct{}

// NormalizeReasoningLanguage 将各种语言输入规范化为 "auto"、"zh" 或 "en"。
// 接受中英文变体：zh/cn/chinese/中文 -> "zh"，en/english -> "en"，其他 -> "auto"。
// 保持在 agent 包内部，使子 Agent 可以继承偏好而不依赖 config 包。
func NormalizeReasoningLanguage(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "zh", "cn", "chinese", "中文":
		return "zh"
	case "en", "english":
		return "en"
	default:
		return "auto"
	}
}

// ReasoningLanguageBlock 生成推理语言偏好的 transient 用户轮次上下文块。
// 它故意不属于稳定的系统提示或工具 Schema，而是作为临时注入。
//
// 返回值是 XML 标签格式的指令块，告知模型在暴露推理文本时使用指定语言。
// 对于 "auto" 模式返回空字符串（不注入任何指令）。
func ReasoningLanguageBlock(lang string) string {
	switch NormalizeReasoningLanguage(lang) {
	case "zh":
		return "<reasoning-language>\nVisible reasoning/thinking text preference: use Simplified Chinese when the provider exposes reasoning text. Keep code, identifiers, file paths, shell commands, and untranslated technical terms in their original form. This preference does not override an explicit user request for the final answer language.\n</reasoning-language>"
	case "en":
		return "<reasoning-language>\nVisible reasoning/thinking text preference: use English when the provider exposes reasoning text. Keep code, identifiers, file paths, shell commands, and untranslated technical terms in their original form. This preference does not override an explicit user request for the final answer language.\n</reasoning-language>"
	default:
		return ""
	}
}

// WithReasoningLanguage 在内容前添加推理语言偏好块，除非内容已经以推理语言块开头。
// 用户在提示中后续提到该标签不会抑制已配置的偏好。
//
// 设计要点：
//   - 注入到对话尾部以保持 prompt cache 稳定
//   - 已存在推理语言块时不重复注入
//   - 跳过 memory-update 和 background-jobs 等其他 transient 块来检测
func WithReasoningLanguage(content, lang string) string {
	block := ReasoningLanguageBlock(lang)
	if block == "" || hasLeadingReasoningLanguageBlock(content) {
		return content
	}
	return block + "\n\n" + content
}

// hasLeadingReasoningLanguageBlock 检查内容是否以推理语言偏好块开头。
// 会跳过 memory-update 和 background-jobs 等其他 transient 块来找到真正的开头。
func hasLeadingReasoningLanguageBlock(content string) bool {
	s := strings.TrimLeft(content, " \t\r\n")
	for {
		switch {
		case strings.HasPrefix(s, "<reasoning-language>"):
			return strings.Contains(s, "</reasoning-language>")
		case strings.HasPrefix(s, "<memory-update>"):
			var ok bool
			s, ok = trimLeadingTransientBlock(s, "memory-update")
			if !ok {
				return false
			}
		case strings.HasPrefix(s, "<background-jobs>"):
			var ok bool
			s, ok = trimLeadingTransientBlock(s, "background-jobs")
			if !ok {
				return false
			}
		default:
			return false
		}
	}
}

// trimLeadingTransientBlock 跳过指定标签的 transient 块，返回剩余内容。
// 用于在检测推理语言块之前跳过 memory-update、background-jobs 等块。
func trimLeadingTransientBlock(content, tag string) (string, bool) {
	closeTag := "</" + tag + ">"
	i := strings.Index(content, closeTag)
	if i < 0 {
		return content, false
	}
	return strings.TrimLeft(content[i+len(closeTag):], " \t\r\n"), true
}

// WithReasoningLanguagePreference 将运行时推理语言偏好携带到衍生工具中，
// 特别是子 Agent（其首个用户轮次在父控制器之外创建）。
//
// 显式存储 "auto" 值，以便运行时从 zh/en 切换到 auto 时能清除子路径中的旧启动偏好。
func WithReasoningLanguagePreference(ctx context.Context, lang string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, reasoningLanguageContextKey{}, NormalizeReasoningLanguage(lang))
}

// ReasoningLanguageFromContext 从 context 中提取推理语言偏好，返回 "auto"、"zh" 或 "en"。
// context 为 nil 或未设置偏好时返回 "auto"。
func ReasoningLanguageFromContext(ctx context.Context) string {
	if ctx == nil {
		return "auto"
	}
	if v, ok := ctx.Value(reasoningLanguageContextKey{}).(string); ok {
		return NormalizeReasoningLanguage(v)
	}
	return "auto"
}
