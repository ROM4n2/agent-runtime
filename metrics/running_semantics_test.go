package metrics

import "testing"

// TestRunningCountsClaimedNotQueued 锁死 running gauge 的语义：
// 它必须表示「已被 worker 抢占、正在执行」的任务数，而不是「已提交未终态」的总数。
//
// 缺陷背景：IncSubmitted 曾同时把 running +1，使 running 实际等于
// submitted - done - failed（含排队中的 pending）。worker_concurrency=1 时
// 提交 5 个任务会报 running=5，而真正在跑的只有 1 个 —— 与 pending gauge
// 并列时自相矛盾，排障者会误判「并发度打满」而实际是积压。
func TestRunningCountsClaimedNotQueued(t *testing.T) {
	baseRunning := running.Load()

	// 提交 3 个任务：尚未被抢占，running 不得变化（它们属于 pending）。
	IncSubmitted()
	IncSubmitted()
	IncSubmitted()
	if got := running.Load(); got != baseRunning {
		t.Fatalf("IncSubmitted 不得改变 running：got %d, want %d", got, baseRunning)
	}

	// 抢占 1 个：这才算 running。
	IncClaimed()
	if got := running.Load(); got != baseRunning+1 {
		t.Fatalf("IncClaimed 后 running = %d, want %d", got, baseRunning+1)
	}

	// 终态回收：running 必须回落，保证 gauge 对称。
	IncDone()
	if got := running.Load(); got != baseRunning {
		t.Fatalf("IncDone 后 running = %d, want %d", got, baseRunning)
	}

	IncClaimed()
	IncFailed()
	if got := running.Load(); got != baseRunning {
		t.Fatalf("IncFailed 后 running = %d, want %d", got, baseRunning)
	}
}

// TestRecoveredDoesNotTouchRunning 锁死崩溃恢复的语义：
// 恢复出来的任务重新进入 pending 等待被抢占，因此不得直接计入 running。
// 它们会在被 worker 抢占时由 IncClaimed 计入，重复计数会让重启后 running 虚高。
func TestRecoveredDoesNotTouchRunning(t *testing.T) {
	baseRunning := running.Load()
	baseRecovered := recovered.Load()

	IncRecovered(2)

	if got := running.Load(); got != baseRunning {
		t.Errorf("IncRecovered 不得改变 running：got %d, want %d", got, baseRunning)
	}
	if got := recovered.Load(); got != baseRecovered+2 {
		t.Errorf("recovered = %d, want %d", got, baseRecovered+2)
	}
	IncRecovered(0) // no-op
	if got := recovered.Load(); got != baseRecovered+2 {
		t.Errorf("IncRecovered(0) 改变了 recovered：%d", got)
	}
}
