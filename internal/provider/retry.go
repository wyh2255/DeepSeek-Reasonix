// retry.go 实现了 HTTP 请求的重试和退避逻辑。
//
// 核心功能:
//   - SendWithRetry: 带重试的 POST 请求发送器，覆盖连接+头部阶段
//   - 指数退避 + 抖动: 退避时间随重试次数指数增长，加上随机抖动避免雷群效应
//   - Retry-After 支持: 尊重服务器返回的 Retry-After 头部
//   - 认证重试: 对已认证过的密钥进行有限次数的 401/403 重试（处理瞬态拒绝）
//   - 可重试状态判断: 408/429/5xx 可重试，其他 4xx 是客户端错误不重试
//   - 连接重置检测: IsConnReset 用于判断流式传输中的连接中断是否可恢复
//
// 重试仅覆盖请求头部阶段——一旦 body 开始流式传输，中间失败不重试（模型已输出 token）。
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// MaxRetries 是 SendWithRetry 在初始尝试后重试连接+头部阶段的最大次数。
// 总尝试次数 = MaxRetries + 1（初始尝试 + 重试）。
const MaxRetries = 10

const maxBackoff = 15 * time.Second

// maxAuthRetries 限制对已认证密钥的 401/403 重试次数。
// 瞬态服务器端拒绝（配额/网关/速率限制）通常在几次尝试后恢复；
// 从未工作过的密钥是真正的配置错误，快速失败。
const maxAuthRetries = 2

// SendOptions 携带 SendWithRetry 所需的请求级上下文，用于标记错误和判断是否值得重试 401。
type SendOptions struct {
	Provider   string // 提供者实例名称，用于错误信息
	KeyEnv     string // 密钥来源的环境变量名（如 "DEEPSEEK_API_KEY"）
	KeySource  string // KeyEnv 的人类可读来源描述
	KeyPresent bool   // 是否发送了非空密钥（区分 "被拒绝" 和 "未配置"）
	RetryAuth  bool   // 密钥是否曾经认证成功（true 时重试瞬态 401，false 时快速失败）
}

// RetryInfo 描述即将发生的退避重试信息。
// 用于通知回调，让 agent 可以展示 "重试中 (n/m)" 的状态。
type RetryInfo struct {
	Attempt int           // 当前重试次数（1-based）
	Max     int           // 最大重试次数
	Delay   time.Duration // 本次退避等待时间
	Err     error         // 触发重试的错误
}

type RetryNotify func(RetryInfo)

type retryNotifyKey struct{}

// WithRetryNotify 将重试通知回调附加到 context。
// SendWithRetry 在每次退避休眠前调用此回调，agent 可借此展示 "重试中 (n/m)" 的瞬态状态。
func WithRetryNotify(ctx context.Context, fn RetryNotify) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, retryNotifyKey{}, fn)
}

func retryNotifyFromContext(ctx context.Context) RetryNotify {
	fn, _ := ctx.Value(retryNotifyKey{}).(RetryNotify)
	return fn
}

// APIError 报告非认证失败的 HTTP 错误状态码。
// Status 用于显示层映射到可操作的本地化消息；Body 是截断的响应片段。
type APIError struct {
	Provider string // 提供者实例名称
	Status   int    // HTTP 状态码
	Body     string // 截断的响应体片段
}

func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s: status %d", e.Provider, e.Status)
	}
	return fmt.Sprintf("%s: status %d: %s", e.Provider, e.Status, e.Body)
}

// RetryableStatus 判断 HTTP 状态码是否可通过退避重试恢复。
// 可重试: 408（请求超时）、429（速率限制）、5xx（含 Anthropic 的 529）。
// 其他 4xx（400/401/402/422 等）是客户端/配置问题，重试无法修复。
func RetryableStatus(s int) bool {
	return s == http.StatusRequestTimeout || s == http.StatusTooManyRequests || (s >= 500 && s <= 599)
}

func transientErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

// IsConnReset 判断错误是否为连接级中断（对端重置、截断响应体、关闭的 socket），
// 而非协议或调用方错误。连接级中断的流可以安全地从头重放（不同于解码错误或 4xx 错误）。
//
// 常见触发场景：本地代理（v2rayN/sing-box）在推理模型的首个 token 间隙
// 空闲关闭了长时间的 SSE 连接。
func IsConnReset(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// backoffDelay 计算退避延迟时间。
// 优先使用服务器返回的 Retry-After（上限 maxBackoff）；
// 否则采用指数退避（500ms * 2^(attempt-1)）+ 随机抖动（0-250ms）。
func backoffDelay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > maxBackoff {
			return maxBackoff
		}
		return retryAfter
	}
	d := time.Duration(1<<(attempt-1)) * 500 * time.Millisecond
	if d > maxBackoff {
		d = maxBackoff
	}
	return d + time.Duration(rand.Intn(250))*time.Millisecond
}

// parseRetryAfter 从响应头解析 Retry-After 值（秒数）。
// 仅支持数字格式（如 "5"），不支持 HTTP 日期格式。
func parseRetryAfter(resp *http.Response) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// SendWithRetry 发送带重试的 POST 请求并返回 OK 响应。
//
// 重试策略:
//   - 覆盖范围: 仅连接+头部阶段（body 流式传输后的失败不重试）
//   - 最大重试: MaxRetries 次（加上初始尝试共 MaxRetries+1 次）
//   - 退避算法: 指数退避 + 抖动，尊重 Retry-After 头部
//   - 可重试条件: 瞬态网络错误 或 408/429/5xx 状态码
//   - 认证处理: 401/403 对未认证过的密钥快速失败，对已认证过的密钥重试最多 maxAuthRetries 次
//     （MiMo 等网关在负载下会返回瞬态 401）
//
// ctx 中的 RetryNotify 回调在每次休眠前触发，用于展示重试状态。
func SendWithRetry(ctx context.Context, httpClient *http.Client, opts SendOptions, newReq func(context.Context) (*http.Request, error)) (*http.Response, error) {
	notify := retryNotifyFromContext(ctx)
	var lastErr error
	var retryAfter time.Duration
	authRetries := 0

	for attempt := 0; attempt <= MaxRetries; attempt++ {
		if attempt > 0 {
			delay := backoffDelay(attempt, retryAfter)
			if notify != nil {
				notify(RetryInfo{Attempt: attempt, Max: MaxRetries, Delay: delay, Err: lastErr})
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		retryAfter = 0

		req, err := newReq(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: build request: %w", opts.Provider, err)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			if !transientErr(err) {
				return nil, fmt.Errorf("%s: request failed: %w", opts.Provider, err)
			}
			lastErr = fmt.Errorf("%s: request failed: %w", opts.Provider, err)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}

		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		retryAfter = parseRetryAfter(resp)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			authErr := &AuthError{Provider: opts.Provider, KeyEnv: opts.KeyEnv, KeySource: opts.KeySource, Status: resp.StatusCode, HasKey: opts.KeyPresent}
			if opts.RetryAuth && authRetries < maxAuthRetries {
				authRetries++
				lastErr = authErr
				continue
			}
			return nil, authErr
		}
		apiErr := &APIError{Provider: opts.Provider, Status: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
		if !RetryableStatus(resp.StatusCode) {
			return nil, apiErr
		}
		lastErr = apiErr
	}
	return nil, lastErr
}
