// Package metrics 提供进程内任务计数，并以 Prometheus 文本格式在 /metrics 暴露。
//
// 零依赖：只用 sync/atomic，不引入 prometheus client 库（严守 ADR-0004 的 YAGNI）。
// 只暴露聚合计数（提交/在飞/完成/失败），绝不出现任务内容或上游响应体。
package metrics

import (
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
)

// IncSubmitted 在任务创建时调用；同时把"在飞"数 +1，便于观测积压。
func IncSubmitted() {
	submitted.Add(1)
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

// IncRecovered 在崩溃恢复重灌 pending 任务时调用：
// 只恢复在飞 gauge 并计入独立的重启恢复计数，不重复计入 submitted
// （这些任务在上个进程已经提交过）。
func IncRecovered(n int) {
	if n <= 0 {
		return
	}
	recovered.Add(int64(n))
	running.Add(int64(n))
}

// Handler 返回 Prometheus 文本格式的指标端点（text/plain; version=0.0.4）。
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprint(w, expose())
	})
}

// expose 拼出 Prometheus  exposition 格式的文本。
func expose() string {
	var b []byte
	b = appendMetric(b, "agent_tasks_submitted", "counter", "Total tasks submitted", submitted.Load())
	b = appendMetric(b, "agent_tasks_running", "gauge", "Tasks currently in flight", running.Load())
	b = appendMetric(b, "agent_tasks_done", "counter", "Tasks completed successfully", done.Load())
	b = appendMetric(b, "agent_tasks_failed", "counter", "Tasks that failed", failed.Load())
	b = appendMetric(b, "agent_tasks_recovered", "counter", "Tasks rehydrated after restart", recovered.Load())
	return string(b)
}

func appendMetric(b []byte, name, typ, help string, val int64) []byte {
	b = append(b, "# HELP "+name+" "+help+"\n"...)
	b = append(b, "# TYPE "+name+" "+typ+"\n"...)
	b = append(b, name+" "+strconv.FormatInt(val, 10)+"\n"...)
	return b
}
