// Package metrics 提供进程内任务计数，并以 Prometheus 文本格式在 /metrics 暴露。
//
// 零依赖：只用 sync/atomic，不引入 prometheus client 库（严守 ADR-0004 的 YAGNI）。
// 只暴露聚合计数（提交/在飞/完成/失败），绝不出现任务内容或上游响应体。
package metrics

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
)

var (
	submitted atomic.Int64
	running   atomic.Int64
	done      atomic.Int64
	failed    atomic.Int64
	recovered atomic.Int64

	// 上游重试与 Token 成本（ADR-0021）：前者说明上游稳不稳，后者是账单口径。
	retries          atomic.Int64
	promptTokens     atomic.Int64
	completionTokens atomic.Int64

	// pendingSource 由装配层注入（见 SetPendingSource），在 scrape 时刻回调取积压数。
	// 用回调而不是让 metrics import store：architecture_test.go 的 AST DAG 白名单
	// 里 metrics 的允许 import 集为空，反向依赖会直接让架构门禁失败。
	// 用 atomic.Pointer 承载，避免 scrape 与注入并发时的数据竞争。
	pendingSource atomic.Pointer[pendingSourceFunc]
)

// pendingSourceFunc 返回当前 pending 任务数；存储故障时返回 error。
type pendingSourceFunc = func(context.Context) (int64, error)

// SetPendingSource 注入 pending 任务数的数据来源；传 nil 表示本进程不度量积压
// （例如 -index / -eval 子命令不跑 worker，暴露一个恒为 0 的 gauge 只会误导排障）。
func SetPendingSource(fn pendingSourceFunc) {
	if fn == nil {
		pendingSource.Store(nil)
		return
	}
	pendingSource.Store(&fn)
}

// IncSubmitted 在任务创建时调用。
// 注意：它不得改动 running —— 提交的任务先进入 pending 排队，只有被 worker
// 抢占（IncClaimed）后才算「在飞」。早期实现把两者合并，导致 running 实际是
// 「已提交未终态」总数，与 pending gauge 并列时自相矛盾。
func IncSubmitted() {
	submitted.Add(1)
}

// IncClaimed 在 worker 抢占到任务、开始执行时调用：这是在飞 gauge 的唯一入口。
func IncClaimed() {
	running.Add(1)
}

// IncDone 任务成功完成时调用；在飞数 -1。
func IncDone() {
	done.Add(1)
	running.Add(-1)
}

// IncFailed 任务失败时调用；在飞数 -1。
func IncFailed() {
	failed.Add(1)
	running.Add(-1)
}

// IncRecovered 在崩溃恢复重灌 pending 任务时调用：只计独立的重启恢复计数，
// 不重复计入 submitted（这些任务在上个进程已经提交过），
// 也不计入 running（它们回到 pending 排队，等被抢占时由 IncClaimed 计入）。
func IncRecovered(n int) {
	if n <= 0 {
		return
	}
	recovered.Add(int64(n))
}

// IncLLMRetry 在 llm 层决定退避重试时调用（由装配层经 llm.Config.OnRetry 注入）。
// 它衡量的是「上游现在稳不稳」：持续上涨说明该查配额或降并发，而不是加 worker。
func IncLLMRetry() {
	retries.Add(1)
}

// AddTokens 累加一次任务消耗的 Token 数（ADR-0021）。
// 失败的调用同样计费，因此调用方应在拿到 usage 后立即累加，而不是等任务成功。
func AddTokens(prompt, completion int) {
	if prompt > 0 {
		promptTokens.Add(int64(prompt))
	}
	if completion > 0 {
		completionTokens.Add(int64(completion))
	}
}

// Handler 返回 Prometheus 文本格式的指标端点（text/plain; version=0.0.4）。
// 请求上下文会传给 pendingSource，使 scrape 能随客户端断连及时释放。
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprint(w, expose(r.Context()))
	})
}

// expose 拼出 Prometheus exposition 格式的文本。
func expose(ctx context.Context) string {
	var b []byte
	b = appendMetric(b, "agent_tasks_submitted", "counter", "Total tasks submitted", submitted.Load())
	b = appendMetric(b, "agent_tasks_running", "gauge", "Tasks currently in flight", running.Load())
	b = appendMetric(b, "agent_tasks_done", "counter", "Tasks completed successfully", done.Load())
	b = appendMetric(b, "agent_tasks_failed", "counter", "Tasks that failed", failed.Load())
	b = appendMetric(b, "agent_tasks_recovered", "counter", "Tasks rehydrated after restart", recovered.Load())
	b = appendMetric(b, "agent_llm_retries_total", "counter", "Upstream LLM retries performed", retries.Load())
	b = appendMetric(b, "agent_llm_prompt_tokens_total", "counter", "Prompt tokens consumed by upstream LLM calls", promptTokens.Load())
	b = appendMetric(b, "agent_llm_completion_tokens_total", "counter", "Completion tokens consumed by upstream LLM calls", completionTokens.Load())
	if v, ok := pendingValue(ctx); ok {
		b = appendMetric(b, "agent_tasks_pending", "gauge", "Tasks waiting in the queue", v)
	}
	return string(b)
}

// pendingValue 读取注入的 pending 数据源。
// 第二个返回值为 false 表示本进程不度量积压（未装配 source），此时不输出该指标。
// source 报错时返回 -1：Prometheus 约定 -1 表示「未知」，既让告警可写
// （pending == -1 即存储故障），又不让整页 scrape 500 而丢掉其余信号。
func pendingValue(ctx context.Context) (int64, bool) {
	p := pendingSource.Load()
	if p == nil {
		return 0, false
	}
	n, err := (*p)(ctx)
	if err != nil {
		return -1, true
	}
	return n, true
}

func appendMetric(b []byte, name, typ, help string, val int64) []byte {
	b = append(b, "# HELP "+name+" "+help+"\n"...)
	b = append(b, "# TYPE "+name+" "+typ+"\n"...)
	b = append(b, name+" "+strconv.FormatInt(val, 10)+"\n"...)
	return b
}
