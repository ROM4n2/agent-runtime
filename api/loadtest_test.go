package api

import (
	"agent-runtime/agent"
	"agent-runtime/store"
	"bytes"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件是提交路径的本地压测口径（P10）：回答「本进程在积压前能扛多少提交」，
// 而不是「LLM 有多快」。上游延迟是外部成本，不属于本进程的吞吐。
//
// 刻意绕开中间件链（limiter 每秒 20 / 突发 40 是**有意的**限流上限，
// 不是待优化的瓶颈）：压测目的是量化落库+入队这一段自身的开销，
// 把限流混进来只会得到一个「限流器有多准」的数字。
//
// 用法：
//   go test ./api/ -run TestSubmitLatencyPercentiles -v -count=1
//   go test ./api/ -bench BenchmarkSubmit -benchmem -run '^$'

// benchRunner 是并发安全的 TaskRunner 桩：只做原子计数，不产生任何阻塞。
type benchRunner struct {
	enqueued atomic.Int64
}

func (b *benchRunner) Enqueue(id string) error {
	b.enqueued.Add(1)
	return nil
}

func (b *benchRunner) Trace(id string) []agent.TraceStep { return nil }

func (b *benchRunner) Cancel(id, reason string) error { return nil }

func (b *benchRunner) Subscribe(id string) ([]agent.TraceStep, <-chan agent.TraceStep, func(), bool) {
	return nil, nil, func() {}, false
}

func (b *benchRunner) SubscribeEvents(id string) ([]agent.TraceStep, <-chan agent.TraceEvent, func(), bool) {
	return nil, nil, func() {}, false
}

func newBenchHandler() *Handler {
	return NewHandler(store.NewStore(), &benchRunner{}, "")
}

var benchBody = []byte(`{"prompt":"benchmark prompt"}`)

// BenchmarkSubmit 串行提交，衡量单请求的纯开销与分配次数。
func BenchmarkSubmit(b *testing.B) {
	h := newBenchHandler()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		h.HandleRun(rec, httptest.NewRequest(http.MethodPost, "/run", newBodyReader()))
		if rec.Code != http.StatusAccepted {
			b.Fatalf("status = %d, want 202", rec.Code)
		}
	}
}

// BenchmarkSubmitParallel 并发提交，衡量共享存储锁下的吞吐上限。
func BenchmarkSubmitParallel(b *testing.B) {
	h := newBenchHandler()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rec := httptest.NewRecorder()
			h.HandleRun(rec, httptest.NewRequest(http.MethodPost, "/run", newBodyReader()))
			if rec.Code != http.StatusAccepted {
				b.Errorf("status = %d, want 202", rec.Code)
			}
		}
	})
}

// TestSubmitLatencyPercentiles 是 P10 的实证口径：
// 32 并发提交 2000 次，断言零丢失（全部 202），并输出 p50/p95/p99 与吞吐。
// 断言部分防回归，分位数部分供文档与面试引用（不同机器上数字会变，机制不会）。
func TestSubmitLatencyPercentiles(t *testing.T) {
	if testing.Short() {
		t.Skip("load probe skipped in -short")
	}

	const (
		requests = 2000
		workers  = 32
	)

	h := newBenchHandler()
	jobs := make(chan struct{})

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		lat   = make([]time.Duration, 0, requests)
		notOK atomic.Int64
		start = time.Now()
	)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]time.Duration, 0, requests/workers+1)
			for range jobs {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/run", newBodyReader())
				begin := time.Now()
				h.HandleRun(rec, req)
				local = append(local, time.Since(begin))
				if rec.Code != http.StatusAccepted {
					notOK.Add(1)
				}
			}
			mu.Lock()
			lat = append(lat, local...)
			mu.Unlock()
		}()
	}

	for i := 0; i < requests; i++ {
		jobs <- struct{}{}
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)

	if got := int64(len(lat)); got != requests {
		t.Fatalf("collected %d samples, want %d", got, requests)
	}
	// 承重断言：提交路径不允许丢单。若这里非零，说明 202 契约被破坏。
	if n := notOK.Load(); n != 0 {
		t.Fatalf("%d submissions returned non-202", n)
	}

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p := func(q float64) time.Duration {
		idx := int(q * float64(len(lat)-1))
		return lat[idx]
	}

	t.Logf("submit latency over %d requests @ %d workers: p50=%v p95=%v p99=%v max=%v throughput=%.0f req/s",
		requests, workers, p(0.50), p(0.95), p(0.99), lat[len(lat)-1],
		float64(requests)/elapsed.Seconds())
}

// newBodyReader 每次返回全新的 body reader：请求体不可重读，复用会让后续请求拿到空 body。
func newBodyReader() *bytes.Reader {
	return bytes.NewReader(benchBody)
}
