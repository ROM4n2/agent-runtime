package llm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryPolicy 描述上游限流/瞬时故障的重试节奏。
//
// 语义：MaxAttempts 是「总尝试次数（含首次）」，<=1 表示不重试。
// 退避为 BaseDelay × 2^(attempt-1)，单次等待上限 MaxDelay。
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// RetryFunc 在每次决定重试前被调用，用于外露重试信号（指标/日志）。可为 nil。
// statusCode 为 0 表示失败发生在连接建立阶段（没有拿到 HTTP 响应）。
type RetryFunc func(attempt int, statusCode int, delay time.Duration)

// DefaultRetryPolicy 返回生产默认值：最多 3 次尝试，退避 500ms / 1s，单次等待上限 5s。
//
// 上限存在的原因：上游可以用 Retry-After 要求我们等 60s，但一个 worker 被占住
// 60s 的代价高于放弃这次重试——任务超时预算（默认 30s）本来就撑不住。
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: 3,
		BaseDelay:   500 * time.Millisecond,
		MaxDelay:    5 * time.Second,
	}
}

// normalized 补齐零值字段，使 Config 只需显式覆盖想改的那一项。
func (p RetryPolicy) normalized() RetryPolicy {
	def := DefaultRetryPolicy()
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = def.MaxAttempts
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = def.BaseDelay
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = def.MaxDelay
	}
	if p.MaxDelay < p.BaseDelay {
		p.MaxDelay = p.BaseDelay
	}
	return p
}

// backoff 返回第 attempt 次失败后的等待时长（attempt 从 1 开始）。
func (p RetryPolicy) backoff(attempt int) time.Duration {
	d := p.BaseDelay
	for i := 1; i < attempt && d < p.MaxDelay; i++ {
		d *= 2
	}
	if d > p.MaxDelay {
		d = p.MaxDelay
	}
	return d
}

// retryableStatus 判定该状态码是否值得重试。
//
// 只收「上游自己说它现在不行」的信号：限流(429) 与 5xx 网关/服务错误。
// 其余 4xx（400 请求体错、401/403 鉴权错、404、422 语义错）重试一万次也是
// 同一个错，只会把一次失败放大成 N 次计费，因此一律不重试。
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// parseRetryAfter 解析 Retry-After 头：既支持 delta-seconds 也支持 HTTP-date。
// 第二个返回值为 false 表示头缺失或无法解析，调用方应回落到指数退避。
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// doWithRetry 发送请求，并对可重试的失败按策略退避重试。
//
// newReq 每次调用都必须返回一个全新的 *http.Request：请求体是不可重读的
// io.Reader，复用同一个 request 会让第二次发送拿到空 body。
//
// 返回值契约（与 P9 之前保持一致，上层无需改动）：
//   - 成功或不可重试的状态码 → (resp, nil)，由调用方检查 StatusCode 并构造 APIError；
//   - 重试耗尽且最后一次仍拿到响应 → (resp, nil)，仍由调用方转 APIError；
//   - 重试耗尽且最后一次是连接错误 / 上下文结束 → (nil, err)，err 用 %w 包住原因。
func (c *Client) doWithRetry(ctx context.Context, newReq func() (*http.Request, error)) (*http.Response, error) {
	policy := c.retry.normalized()

	for attempt := 1; ; attempt++ {
		req, reqErr := newReq()
		if reqErr != nil {
			return nil, reqErr // 构造请求失败是本地错误，重试无意义
		}

		resp, err := c.http.Do(req)

		if err == nil && !retryableStatus(resp.StatusCode) {
			return resp, nil
		}

		// 上下文已结束（超时 / 用户取消 / 停机）：立即返回，不再退避空等。
		if ctxErr := ctx.Err(); ctxErr != nil {
			closeBody(resp)
			return nil, fmt.Errorf("llm: send request: %w", ctxErr)
		}

		last := attempt >= policy.MaxAttempts
		if last {
			if err != nil {
				return nil, fmt.Errorf("llm: send request: %w", err)
			}
			// 把最后一次响应原样交回：错误文案与 APIError 语义保持既有契约。
			return resp, nil
		}

		delay := policy.backoff(attempt)
		status := 0
		if err == nil {
			status = resp.StatusCode
			if d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
				delay = d
			}
			if delay > policy.MaxDelay {
				delay = policy.MaxDelay
			}
			// 排空并关闭，避免连接泄漏到下一次尝试。
			closeBody(resp)
		}

		if c.OnRetry != nil {
			c.OnRetry(attempt, status, delay)
		}
		if sleepErr := sleepCtx(ctx, delay); sleepErr != nil {
			return nil, fmt.Errorf("llm: send request: %w", sleepErr)
		}
	}
}

// closeBody 排空少量响应体后关闭，使底层连接可以复用而不是被丢弃。
func closeBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	_ = resp.Body.Close()
}

// sleepCtx 等待 d，但可被 ctx 提前打断（任务超时 / 用户取消 / 停机广播）。
// 返回 nil 表示等满了；非 nil 是 ctx.Err()。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
