package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-runtime/llm"
	"agent-runtime/metrics"
	"agent-runtime/store"
)

// gaugeValue 从 /metrics 端点抓取指定指标名的最新值。
// 走真实的 Handler 而不是读包内变量，确保测的是对外契约而非内部状态。
func gaugeValue(t *testing.T, name string) int64 {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == name {
			v, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				t.Fatalf("parse %s value %q: %v", name, fields[1], err)
			}
			return v
		}
	}
	t.Fatalf("metric %s not found in /metrics output:\n%s", name, rec.Body.String())
	return 0
}

// gateChatter 阻塞在 gate 上，直到测试主动放行。
// 用它把「任务正在执行」这一瞬态钉成可控状态，避免靠 sleep 猜时序。
type gateChatter struct {
	gate    chan struct{}
	started chan struct{}
}

func (g gateChatter) ChatWithTools(ctx context.Context, _ []llm.Message, _ []llm.Tool) (*llm.AssistantTurn, error) {
	select {
	case g.started <- struct{}{}:
	default:
	}
	select {
	case <-g.gate:
		return &llm.AssistantTurn{Content: "done"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestMetrics_RunningEqualsConcurrencyUnderBacklog 是 P9 指标语义缺陷的承重墙。
//
// 缺陷：running 曾在 IncSubmitted 时 +1，于是它等于「已提交未终态」总数。
// worker_concurrency=1 提交 3 个任务时会报 running=3 —— 排障者据此误判
// 「并发度已打满」，而真相是 1 个在跑、2 个在排队。
//
// 本测试锁死：running 必须等于并发度，pending 才表示积压。
// 必须走完整的提交路径（IncSubmitted + Enqueue），否则只测到抢占一侧，
// 无法复现「提交即计入 running」这一原始缺陷。
func TestMetrics_RunningEqualsConcurrencyUnderBacklog(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{}, 4)

	// 基线差值断言：metrics 是进程级全局状态，同包其他测试可能已留下计数。
	baseRunning := gaugeValue(t, "agent_tasks_running")

	s := store.NewStore()
	// 复刻装配层的接线（main.go 做的同一件事）：metrics 不 import store，
	// pending 数据源必须由装配方注入。测试里手动接上，才能验证对外契约。
	metrics.SetPendingSource(func(ctx context.Context) (int64, error) {
		return s.PendingCount(ctx)
	})
	t.Cleanup(func() { metrics.SetPendingSource(nil) })

	// 并发度 1：同一时刻最多 1 个任务在执行。
	p := NewPool(1, 30*time.Second, s, gateChatter{gate: gate, started: started})
	p.SetMaxQueueSize(100)
	p.Start()
	defer func() {
		close(gate) // 放行被门控的调用，避免 Stop 等待超时
		_ = p.Stop()
	}()

	const submitted = 3
	for i := 0; i < submitted; i++ {
		// 复刻 api.HandleRun 的完整提交路径：先计提交，再落库入队。
		metrics.IncSubmitted()
		id := s.Create("hi")
		if err := p.Enqueue(id); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	// 等到唯一那个 worker 真的进入执行，再断言瞬态。
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker 未在 2s 内开始执行任务")
	}

	if got := gaugeValue(t, "agent_tasks_running") - baseRunning; got != 1 {
		t.Fatalf("running 增量 = %d, want 1（并发度为 1，多出的任务属于 pending）", got)
	}
	// 3 个提交 - 1 个在执行 = 2 个排队；断言 >= 1 以容忍抢占时序的微小差异。
	if got := gaugeValue(t, "agent_tasks_pending"); got < 1 {
		t.Fatalf("pending = %d, want >= 1（另有 %d 个任务在排队）", got, submitted-1)
	}
}
