package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fastRetry 是测试用退避：最大尝试次数可控，退避时长压到微秒级，
// 使断言只依赖「重试了几次」而不是「等了多久」。
func fastRetry(maxAttempts int) RetryPolicy {
	return RetryPolicy{MaxAttempts: maxAttempts, BaseDelay: time.Microsecond, MaxDelay: 2 * time.Microsecond}
}

// TestChatWithTools_RetriesOnRateLimit 是 ADR-0021 决策 1 的承重墙：
// 上游 429 一次后恢复，任务必须成功，且重试信号经 OnRetry 外露。
func TestChatWithTools_RetriesOnRateLimit(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":"rate limited"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
	}))
	defer srv.Close()

	var gotStatus, gotAttempt int
	var gotDelay time.Duration
	c := NewClient(Config{
		BaseURL: srv.URL,
		Model:   "m",
		Retry:   fastRetry(3),
		OnRetry: func(attempt, status int, delay time.Duration) {
			gotAttempt, gotStatus, gotDelay = attempt, status, delay
		},
	})

	turn, err := c.ChatWithTools(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("ChatWithTools: %v", err)
	}
	if turn.Content != "ok" {
		t.Errorf("content = %q, want ok", turn.Content)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("upstream calls = %d, want 2 (1 次 429 + 1 次成功)", n)
	}
	if gotAttempt != 1 || gotStatus != http.StatusTooManyRequests {
		t.Errorf("OnRetry(attempt=%d,status=%d), want (1,429)", gotAttempt, gotStatus)
	}
	// Retry-After: 0 必须被采信并覆盖指数退避（否则这里会是 fastRetry 的 1µs 以上基数）。
	if gotDelay != 0 {
		t.Errorf("delay = %v, want 0（Retry-After: 0 优先于指数退避）", gotDelay)
	}
}

// TestChatWithTools_NoRetryOnClientError 守住「4xx 客户端错误不重试」：
// 重试鉴权错/请求体错只会把一次失败放大成 N 次计费。
func TestChatWithTools_NoRetryOnClientError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"bad request"}`)
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, Model: "m", Retry: fastRetry(3)})
	_, err := c.ChatWithTools(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil)

	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("err = %v, want *APIError 400", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("upstream calls = %d, want 1（400 不可重试）", n)
	}
}

// TestChatWithTools_RetryExhaustedReturnsAPIError 锁定错误契约：
// 重试耗尽后仍返回最后一次响应的 *APIError，上层 worker 的终态映射无需改动。
func TestChatWithTools_RetryExhaustedReturnsAPIError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":"overloaded"}`)
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, Model: "m", Retry: fastRetry(3)})
	_, err := c.ChatWithTools(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil)

	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want *APIError 503", err)
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Errorf("upstream calls = %d, want 3（MaxAttempts 含首次）", n)
	}
}

// TestChatWithTools_RetryAbortsOnContextCancel 守住「退避必须可被取消」：
// 任务超时 / 用户抢占 / 停机广播在退避期间同样生效，不允许出现「已取消还在睡」的假在飞。
func TestChatWithTools_RetryAbortsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	// 退避设为 5s：若实现忽略了 ctx，本测试会明显超时。
	c := NewClient(Config{BaseURL: srv.URL, Model: "m", Retry: RetryPolicy{MaxAttempts: 3, BaseDelay: 5 * time.Second, MaxDelay: 5 * time.Second}})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.ChatWithTools(ctx, []Message{{Role: "user", Content: "hi"}}, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want 可被 errors.Is(DeadlineExceeded) 识别", err)
	}
	if elapsed > time.Second {
		t.Errorf("elapsed = %v, want < 1s（退避未响应 ctx 取消）", elapsed)
	}
}

// TestChatWithToolsStream_RetriesBeforeFirstDelta 验证流式在「尚未下发任何 delta」时
// 可以重试：429 后恢复，客户端只看到一份完整文本，不出现重复。
func TestChatWithToolsStream_RetriesBeforeFirstDelta(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var deltas []string
	c := NewClient(Config{BaseURL: srv.URL, Model: "m", Retry: fastRetry(3)})
	turn, err := c.ChatWithToolsStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, func(d string) {
		deltas = append(deltas, d)
	})
	if err != nil {
		t.Fatalf("ChatWithToolsStream: %v", err)
	}
	if turn.Content != "hello" {
		t.Errorf("content = %q, want hello", turn.Content)
	}
	if len(deltas) != 2 {
		t.Errorf("deltas = %v, want 恰好 2 段（重试不得重放已下发的增量）", deltas)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("upstream calls = %d, want 2", n)
	}
}

// TestChatWithToolsStream_NoRetryAfterPartialStream 锁定流式的取舍：
// 200 之后中途断流不重试——重试会把已经打出去的文本再打一遍。
func TestChatWithToolsStream_NoRetryAfterPartialStream(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// 声明比实际更长的 Content-Length：客户端读到半截即 unexpected EOF，
		// 模拟上游流式传输中途断连。
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n")
	}))
	defer srv.Close()

	var deltas []string
	c := NewClient(Config{BaseURL: srv.URL, Model: "m", Retry: fastRetry(3)})
	_, err := c.ChatWithToolsStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, func(d string) {
		deltas = append(deltas, d)
	})

	if err == nil {
		t.Fatal("expected error on truncated stream, got nil")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("upstream calls = %d, want 1（已产出 delta 后不得重试）", n)
	}
	if len(deltas) != 1 {
		t.Errorf("deltas = %v, want 恰好 1 段（不得重复下发）", deltas)
	}
}

// TestParseRetryAfter 覆盖 delta-seconds / HTTP-date / 非法值三种形态。
func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		in     string
		want   time.Duration
		wantOK bool
	}{
		{"seconds", "3", 3 * time.Second, true},
		{"zero", "0", 0, true},
		{"padded", "  7  ", 7 * time.Second, true},
		{"http-date", now.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second, true},
		{"past-date", now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"empty", "", 0, false},
		{"garbage", "soon", 0, false},
		{"negative", "-1", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tc.in, now)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("parseRetryAfter(%q) = (%v,%v), want (%v,%v)", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestRetryPolicyBackoff 锁定指数退避与封顶：1×, 2×, 4×… 不超过 MaxDelay。
func TestRetryPolicyBackoff(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 5, BaseDelay: 100 * time.Millisecond, MaxDelay: 350 * time.Millisecond}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 350 * time.Millisecond, 350 * time.Millisecond}
	for i, w := range want {
		if got := p.backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

// TestRetryPolicyNormalized 验证零值 Config 也能拿到生产默认（调用方只需覆盖想改的项）。
func TestRetryPolicyNormalized(t *testing.T) {
	got := RetryPolicy{}.normalized()
	def := DefaultRetryPolicy()
	if got != def {
		t.Errorf("normalized() = %+v, want %+v", got, def)
	}
	// MaxDelay < BaseDelay 的非法组合必须被拉平，否则退避会「越等越短」。
	got = RetryPolicy{MaxAttempts: 2, BaseDelay: time.Second, MaxDelay: time.Millisecond}.normalized()
	if got.MaxDelay != got.BaseDelay {
		t.Errorf("MaxDelay = %v, want 拉平到 BaseDelay %v", got.MaxDelay, got.BaseDelay)
	}
}

// TestRetryableStatus 白名单：只重试「上游自己说现在不行」的信号。
func TestRetryableStatus(t *testing.T) {
	retryable := []int{429, 500, 502, 503, 504}
	for _, code := range retryable {
		if !retryableStatus(code) {
			t.Errorf("retryableStatus(%d) = false, want true", code)
		}
	}
	notRetryable := []int{200, 400, 401, 403, 404, 422}
	for _, code := range notRetryable {
		if retryableStatus(code) {
			t.Errorf("retryableStatus(%d) = true, want false", code)
		}
	}
}
