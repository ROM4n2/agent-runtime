package eval

import (
	"testing"
)

// realSetPath 是任务集 v1 的入仓位置（ADR-0018 决策 1：任务集先于检索实现入仓）。
const realSetPath = "tasks/retrieval_v1.json"

// TestLoadTasks_RealSet 固化任务集 v1 的入仓契约（ADR-0018 决策 1 防共谋纪律）：
// 真实文件必须完整通过 LoadTasks 的 schema 校验，且规模与三档配比满足下限。
// 配比下限的意义：纯语料内直答题会让切分器与题目共谋，虚高命中率——
// cross-chunk 考跨文档综合、out-of-corpus 考不编造，两类缺一不可
// （ADR-0018 决策 1 防共谋纪律）。
// 该测试随任务集一同入仓（先入仓后跑）；任务集任何变更必须同步
// eval/tasks/README.md 的人工核定记录，禁止跑完挑题。
func TestLoadTasks_RealSet(t *testing.T) {
	tasks, err := LoadTasks(realSetPath)
	if err != nil {
		t.Fatalf("LoadTasks(%s): %v", realSetPath, err)
	}

	// 规模：20-50 条。低于 20 则噪声带（±2/|set|）过宽、提升判定不稳定；
	// 高于 50 则一次性人工核定成本失控（ADR-0018 后果节已声明该成本）。
	if len(tasks) < 20 || len(tasks) > 50 {
		t.Fatalf("task count = %d, want 20..50", len(tasks))
	}

	// 三档配比下限：in-corpus ≥40%、cross-chunk ≥30%、out-of-corpus ≥15%。
	// 整数交叉相乘比较（count*100 >= minPct*total），避免浮点边界争议。
	counts := make(map[string]int, 3)
	for _, tk := range tasks {
		counts[tk.Scope]++
	}
	total := len(tasks)
	for _, c := range []struct {
		scope  string
		minPct int
	}{
		{ScopeInCorpus, 40},
		{ScopeCrossChunk, 30},
		{ScopeOutOfCorpus, 15},
	} {
		if got := counts[c.scope]; got*100 < c.minPct*total {
			t.Errorf("scope %s: %d/%d (%.1f%%), want >= %d%%",
				c.scope, got, total, float64(got)*100/float64(total), c.minPct)
		}
	}

	// 期望 span 契约：与 retrieval 同一套坐标（原文 rune 偏移 [start,end) 含头不含尾，
	// retrieval/chunk.go）。LoadTasks 已逐条校验 0<=start<end；这里对真实文件再显式
	// 断言一次——任务集是人工维护的 JSON，"反正有 schema 校验"不构成免除复述契约的理由。
	for _, tk := range tasks {
		for _, s := range tk.ExpectSpans {
			if s[0] < 0 || s[0] >= s[1] {
				t.Errorf("task %s: expect_span %v violates 0 <= start < end", tk.ID, s)
			}
		}
	}
}
