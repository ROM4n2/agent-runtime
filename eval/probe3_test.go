package eval

import (
	"context"
	"testing"

	"agent-runtime/agent"
	"agent-runtime/retrieval"
)

// TestProbe3_CaliberCheck 是一次性口径诊断探针（Task 12，无 LLM 消耗）：
// 用真实检索返回构造 512B 截断的 trace，喂给现有 grader，观察 recall 的
// 实际量级——判断剩余假红是「chunk span 与 expect span 口径差」还是别的。
func TestProbe3_CaliberCheck(t *testing.T) {
	tasks, err := LoadTasks("tasks/retrieval_v1.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	idx, err := retrieval.Open("../agent-runtime.db", retrieval.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer idx.Close()
	tool := retrieval.SearchTool(idx, "..")

	wanted := map[string]bool{"q001": true, "q009": true, "q012": true, "q018": true}
	for _, task := range tasks {
		if !wanted[task.ID] {
			continue
		}
		out, err := tool.Execute(context.Background(), []byte(`{"query":`+jsonStr(task.Prompt)+`,"top_k":5}`))
		if err != nil {
			t.Logf("[%s] tool ERROR: %v", task.ID, err)
			continue
		}
		trunc := out
		if len(trunc) > agent.MaxTraceDetail {
			trunc = trunc[:agent.MaxTraceDetail]
		}
		trace := []agent.TraceStep{
			{Step: 0, Kind: agent.KindToolCall, Tool: "search_docs"},
			{Step: 0, Kind: agent.KindToolResult, Tool: "search_docs", Detail: trunc},
		}
		full := Grade(task, []agent.TraceStep{
			{Step: 0, Kind: agent.KindToolCall, Tool: "search_docs"},
			{Step: 0, Kind: agent.KindToolResult, Tool: "search_docs", Detail: out},
		}, "参见 docs/adr/xxx.md")
		cut := Grade(task, trace, "参见 docs/adr/xxx.md")
		t.Logf("[%s] scope=%s want_spans=%v want_docs=%v", task.ID, task.Scope, task.ExpectSpans, task.ExpectDocs)
		t.Logf("   full-detail: recall=%.3f firstErr=%q success=%v citedSpans=%v", full.SpanRecall, full.FirstError, full.Success, full.CitedSpans)
		t.Logf("   truncated:   recall=%.3f firstErr=%q citedSpans=%v", cut.SpanRecall, cut.FirstError, cut.CitedSpans)
	}
}
