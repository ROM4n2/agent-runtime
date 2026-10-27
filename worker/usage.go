package worker

import (
	"agent-runtime/llm"
	"agent-runtime/metrics"
	"log/slog"
)

// usageAccumulator 累计一次任务执行期间所有 LLM 步进的 Token 消耗（ADR-0021）。
//
// 只在 agent.RunWithOptions 的同一 goroutine 内被 OnUsage 回调写入，因此不需要加锁。
// 跨 goroutine 复用它就会变成数据竞争——一次任务一个实例，禁止提升为 Pool 字段。
type usageAccumulator struct {
	promptTokens     int
	completionTokens int
}

// add 满足 agent.RunOptions.OnUsage 的签名。
func (u *usageAccumulator) add(us llm.Usage) {
	u.promptTokens += us.PromptTokens
	u.completionTokens += us.CompletionTokens
}

// recordUsage 把本次任务的用量写进进程指标与任务记录。
//
// 成功与失败路径都要调用：失败的调用照样计费，漏记失败任务的用量会让账单对不上。
// 落库失败只记日志、不改任务终态——计量是旁路信号，不该让业务结果失败。
func (p *Pool) recordUsage(id string, u *usageAccumulator) {
	if u == nil || (u.promptTokens == 0 && u.completionTokens == 0) {
		return
	}
	metrics.AddTokens(u.promptTokens, u.completionTokens)
	if err := p.tasks.RecordUsage(id, u.promptTokens, u.completionTokens); err != nil {
		slog.Warn("worker: record token usage failed", "task_id", id, "error", err)
	}
}
