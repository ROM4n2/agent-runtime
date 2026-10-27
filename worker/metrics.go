package worker

import "agent-runtime/metrics"

// 本文件集中承载 worker 的指标语义，使 pool.go 免于体积膨胀
// （架构门禁：生产代码 ≤ 400 行；pool.go 已贴上限，同包正交拆分是既有惯例，
// 见 worker/session.go 的同类拆分）。
//
// 核心不变式：running gauge 只在「抢占成功」时 +1，与终态 -1 严格成对。
// 若把 +1 挪到提交侧（IncSubmitted），running 就变成「已提交未终态」总数，
// 与 pending 并列时自相矛盾 —— 排障者会把积压误判为并发打满。

// markClaimed 在 worker 抢占到任务、即将执行时调用：running gauge 的唯一入口。
//
// 必须与随后的终态调用（IncDone / IncFailed）成对，否则 gauge 会漂移。
// 因此调用点限定在 ClaimNext 返回成功之后、process 之前的两处：
// 正常消费循环（pool.worker）与停机排空循环（pool.Stop）。
func markClaimed() {
	metrics.IncClaimed()
}
