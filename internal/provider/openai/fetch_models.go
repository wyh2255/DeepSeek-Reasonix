// fetch_models.go 实现了 OpenAI 兼容的模型列表获取功能。
//
// 通过 GET /models 端点查询提供者支持的模型列表，用于:
//   - 配置向导中的模型自动发现
//   - 验证用户配置的模型 ID 是否可用
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// modelFetchStatusError 是模型列表获取的 HTTP 错误。
type modelFetchStatusError struct {
	status int    // HTTP 状态码
	body   string // 响应体片段
}

func (e modelFetchStatusError) Error() string {
	return fmt.Sprintf("fetch models: status %d: %s", e.status, strings.TrimSpace(e.body))
}

// IsModelFetchEndpointMiss 判断错误是否表示请求到达了合法但未实现的端点路径。
// 404（Not Found）和 405（Method Not Allowed）表示提供者不支持 /models 端点。
func IsModelFetchEndpointMiss(err error) bool {
	var statusErr modelFetchStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	return statusErr.status == http.StatusNotFound || statusErr.status == http.StatusMethodNotAllowed
}

// FetchModels 调用 OpenAI 兼容的 GET /models 端点，返回可用的模型 ID 列表。
//
// 流程:
//   1. 构建请求 URL（自动补全 /models 后缀）
//   2. 设置认证头（如果有 API 密钥）
//   3. 解析响应 JSON 中的 data[].id 字段
//   4. 返回排序后的模型 ID 列表
func FetchModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	cli := &http.Client{Timeout: 10 * time.Second}
	url := strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(url, "/models") {
		url += "/models"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch models: build request: %w", err)
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch models: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return nil, fmt.Errorf("fetch models: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, modelFetchStatusError{status: resp.StatusCode, body: truncateFetchBody(string(body))}
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("fetch models: decode response: %w", err)
	}

	ids := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// truncateFetchBody 截断响应体到最大 512 个字符，用于错误信息展示。
func truncateFetchBody(body string) string {
	body = strings.TrimSpace(body)
	const max = 512
	if len([]rune(body)) <= max {
		return body
	}
	r := []rune(body)
	return string(r[:max]) + "..."
}
