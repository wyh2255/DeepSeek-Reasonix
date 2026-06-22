// host.go 提供基于 URL 的提供者类型自动检测功能。
//
// 通过解析 base URL 的主机名，自动识别 DeepSeek 和 MiniMax 后端，
// 以便选择正确的 thinking 协议和 reasoning_effort 格式。
// 支持精确匹配（api.deepseek.com）和通配符子域名匹配（*.deepseek.com）。
package openai

import (
	"net/url"
	"strings"
)

// matchesVendorHost 判断 baseURL 是否指向指定厂商的主机名。
//
// 匹配规则:
//   - 精确匹配 canonical 列表中的主机名（如 api.minimaxi.com）
//   - 通配符子域名匹配 apex（如 *.minimaxi.com → eu.minimaxi.com、us.minimaxi.com）
//   - 裸 apex 域名（如 minimaxi.com）故意不匹配——这是配置错误
//
// 区分 canonical 和 apex 的原因：canonical 是特定端点，apex 用于子域名通配；
// 区域子域名（如 eu.minimaxi.com）的 wire 格式与主域名相同，只是托管在不同区域。
func matchesVendorHost(baseURL, apex string, canonical ...string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, c := range canonical {
		if host == c {
			return true
		}
	}
	return strings.HasSuffix(host, "."+apex)
}

// IsDeepSeek 判断 baseURL 是否指向 DeepSeek API（api.deepseek.com 或 *.deepseek.com）。
func IsDeepSeek(baseURL string) bool {
	return matchesVendorHost(baseURL, "deepseek.com", "api.deepseek.com")
}

// IsMiniMax 判断 baseURL 是否指向 MiniMax 的 OpenAI 兼容端点（api.minimaxi.com 或 *.minimaxi.com）。
//
// 主机名精确匹配 "minimaxi"（而非 "minimax"），避免与未来的 minimax 品牌网关冲突。
func IsMiniMax(baseURL string) bool {
	return matchesVendorHost(baseURL, "minimaxi.com", "api.minimaxi.com")
}
