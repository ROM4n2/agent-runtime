package eval

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"agent-runtime/agent"
)

// docADR15 / docADR14 用真实风格的文档 id：判定器按子串匹配 doc_id，
// 用 "d1" 这类过短 id 会让匹配语义与线上（路径型 id）脱节。
const (
	docADR15 = "docs/adr/0015-crash-recovery-and-task-rehydration.md"
	docADR14 = "docs/adr/0014-cancellation-semantics.md"
)

// ---- trace 构造助手：形状与 agent.Run 的真实 emit 对齐（engine.go:82-92,119-129） ----

func searchCallStep() agent.TraceStep {
	return agent.TraceStep{Step: 1, Kind: agent.KindToolCall, Tool: searchToolName, CallID: "call-1"}
}

func searchResultStep(detail string) agent.TraceStep {
	return agent.TraceStep{Step: 2, Kind: agent.KindToolResult, Tool: searchToolName, Detail: detail, CallID: "call-1"}
}

// hitRow / resultRow 镜像 retrieval/tool.go 的线上返回 JSON（renderResult）。
// 在测试侧独立声明：契约由 wire 格式定义，不由 retrieval 的 Go 结构体担保——
// 哪天工具侧改了字段名，这里必须红。
type hitRow struct {
	ChunkID int64   `json:"chunk_id"`
	DocID   string  `json:"doc_id"`
	Source  string  `json:"source"`
	Span    [2]int  `json:"span"`
	Score   float64 `json:"score"`
	Text    string  `json:"text"`
}

type resultRow struct {
	Mode      string   `json:"mode"`
	Truncated bool     `json:"truncated"`
	Hits      []hitRow `json:"hits"`
}

func searchDetail(t *testing.T, hits ...hitRow) string {
	t.Helper()
	b, err := json.Marshal(resultRow{Mode: "lexical", Hits: hits})
	if err != nil {
		t.Fatalf("marshal search result: %v", err)
	}
	return string(b)
}

func inCorpusTask(spans ...[2]int) Task {
	return Task{
		ID:          "q001",
		Prompt:      "崩溃恢复把僵尸任务标记成什么错误文案？",
		Scope:       ScopeInCorpus,
		ExpectDocs:  []string{docADR15},
		ExpectSpans: spans,
	}
}

// closeEnough 用 1e-9 判浮点相等：交并比是有理数，double 精度下足够。
func closeEnough(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}

// TestSearchToolName 钉死与 retrieval/tool.go 的 Spec().Function.Name 字面一致：
// 常量写错 = 全量任务被判 no-search 的静默假红（判定器看不见任何检索）。
func TestSearchToolName(t *testing.T) {
	if searchToolName != "search_docs" {
		t.Fatalf("searchToolName = %q, want %q", searchToolName, "search_docs")
	}
}

// TestGrade_ExactHitSuccess：精确命中 recall=1 且回答引用了期望文档 → Success。
func TestGrade_ExactHitSuccess(t *testing.T) {
	task := inCorpusTask([2]int{120, 168})
	trace := []agent.TraceStep{
		{Step: 0, Kind: agent.KindThink, Detail: "先检索崩溃恢复"},
		searchCallStep(),
		searchResultStep(searchDetail(t, hitRow{ChunkID: 7, DocID: docADR15, Span: [2]int{120, 168}, Text: "server restarted unexpectedly"})),
		{Step: 3, Kind: agent.KindFinal, Detail: "结论"},
	}
	g := Grade(task, trace, "崩溃恢复把僵尸任务标记为 server restarted unexpectedly，依据 "+docADR15+"。")
	if !g.Success {
		t.Errorf("expected Success, got %+v", g)
	}
	if g.FirstError != FirstErrorNone {
		t.Errorf("FirstError = %q, want %q", g.FirstError, FirstErrorNone)
	}
	if g.SpanRecall != 1.0 {
		t.Errorf("SpanRecall = %v, want 1", g.SpanRecall)
	}
}

// TestGrade_PartialOverlapScoresIntersectionOverUnion：部分相交按交集/并集自然计入
// （got[10,20] vs want[15,25] → 5/15 = 1/3 < 0.5 → 未命中）。
// 这正是 ADR-0018 后果节黄卡预案「边界重叠按 0.5 权重计入」的口径来源：
// 阈值 0.5 与交并比同源，代码里不为部分相交特判任何魔法数。
func TestGrade_PartialOverlapScoresIntersectionOverUnion(t *testing.T) {
	task := inCorpusTask([2]int{15, 25})
	trace := []agent.TraceStep{
		searchCallStep(),
		searchResultStep(searchDetail(t, hitRow{ChunkID: 1, DocID: docADR15, Span: [2]int{10, 20}, Text: "部分重叠"})),
	}
	g := Grade(task, trace, "依据 "+docADR15+" 作答。")
	if want := 1.0 / 3.0; !closeEnough(g.SpanRecall, want) {
		t.Errorf("SpanRecall = %v, want %v (intersection/union)", g.SpanRecall, want)
	}
	if g.Success {
		t.Errorf("recall 1/3 must not be Success: %+v", g)
	}
	if g.FirstError != FirstErrorWrongSpan {
		t.Errorf("FirstError = %q, want %q", g.FirstError, FirstErrorWrongSpan)
	}
}

// TestGrade_MultipleWantsTakeBestMatchPerWant：多个期望区间各取最大匹配后求和。
// want[[0,10],[100,110]]，got[[3,7],[100,110]]：
// 第一个 want 的最佳匹配是 [3,7]（4/10=0.4），第二个是 [100,110]（1.0）→ recall 0.7。
// 该值同时排除两种错误实现：首匹配即锁（0.4/2=0.2）与直接求和（1.4，>1 无意义）。
func TestGrade_MultipleWantsTakeBestMatchPerWant(t *testing.T) {
	task := inCorpusTask([2]int{0, 10}, [2]int{100, 110})
	trace := []agent.TraceStep{
		searchCallStep(),
		searchResultStep(searchDetail(t,
			hitRow{ChunkID: 1, DocID: docADR15, Span: [2]int{3, 7}, Text: "低分但覆盖第一个期望"},
			hitRow{ChunkID: 2, DocID: docADR15, Span: [2]int{100, 110}, Text: "精确命中第二个期望"},
		)),
	}
	g := Grade(task, trace, "综合 "+docADR15+" 的两处说明。")
	if want := 0.7; !closeEnough(g.SpanRecall, want) {
		t.Errorf("SpanRecall = %v, want %v", g.SpanRecall, want)
	}
	if !g.Success {
		t.Errorf("recall 0.7 >= threshold must be Success: %+v", g)
	}
}

// TestGrade_NoSearchToolCall：trace 里没有 search_docs 的 tool_call → no-search。
// 该分支优先级最高：没有检索步骤，span 与结论判定都无从谈起。
func TestGrade_NoSearchToolCall(t *testing.T) {
	task := inCorpusTask([2]int{10, 20})
	trace := []agent.TraceStep{
		{Step: 0, Kind: agent.KindThink, Detail: "我凭记忆回答"},
		{Step: 1, Kind: agent.KindFinal, Detail: "结论"},
	}
	g := Grade(task, trace, "凭记忆：崩溃恢复标记为 server restarted unexpectedly（"+docADR15+"）。")
	if g.Success {
		t.Errorf("no search must not be Success: %+v", g)
	}
	if g.FirstError != FirstErrorNoSearch {
		t.Errorf("FirstError = %q, want %q", g.FirstError, FirstErrorNoSearch)
	}
	if g.SpanRecall != 0 {
		t.Errorf("SpanRecall = %v, want 0", g.SpanRecall)
	}
}

// TestGrade_CalledButSpansDisjoint：调用了检索但 span 全不交 → wrong-span。
func TestGrade_CalledButSpansDisjoint(t *testing.T) {
	task := inCorpusTask([2]int{120, 168})
	trace := []agent.TraceStep{
		searchCallStep(),
		searchResultStep(searchDetail(t, hitRow{ChunkID: 1, DocID: docADR15, Span: [2]int{500, 600}, Text: "命中了同一文档的错误区间"})),
	}
	g := Grade(task, trace, "依据 "+docADR15+" 作答。")
	if g.Success {
		t.Errorf("disjoint spans must not be Success: %+v", g)
	}
	if g.FirstError != FirstErrorWrongSpan {
		t.Errorf("FirstError = %q, want %q", g.FirstError, FirstErrorWrongSpan)
	}
	if g.SpanRecall != 0 {
		t.Errorf("SpanRecall = %v, want 0", g.SpanRecall)
	}
}

// TestGrade_SpanHitButAnswerMissingDocID：span 命中但结论没用到检索结果
// → right-span-wrong-reasoning。空回答与「通篇不含期望文档 id」都算——
// 引用没进结论，检索就等于白做（ADR-0018 非目标：不评生成质量，
// 此处用「引用出现在最终回答」作为结论正确性的可执行近似）。
func TestGrade_SpanHitButAnswerMissingDocID(t *testing.T) {
	task := inCorpusTask([2]int{120, 168})
	trace := []agent.TraceStep{
		searchCallStep(),
		searchResultStep(searchDetail(t, hitRow{ChunkID: 1, DocID: docADR15, Span: [2]int{120, 168}, Text: "server restarted unexpectedly"})),
	}
	cases := []struct {
		name   string
		answer string
	}{
		{"空回答", ""},
		{"纯空白回答", "   \n  "},
		{"回答不含期望文档 id", "崩溃恢复会标记僵尸任务，但不写依据。"},
		{"回答只引了别的文档", "参见 " + docADR14 + " 的取消语义。"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := Grade(task, trace, c.answer)
			if g.Success {
				t.Errorf("must not be Success: %+v", g)
			}
			if g.FirstError != FirstErrorRightSpanWrongReason {
				t.Errorf("FirstError = %q, want %q", g.FirstError, FirstErrorRightSpanWrongReason)
			}
			if g.SpanRecall != 1.0 {
				t.Errorf("SpanRecall = %v, want 1（span 本身是命中的）", g.SpanRecall)
			}
		})
	}
}

// TestGrade_OutOfCorpus_FabricatedReference：out-of-corpus 题模型仍检索并引用了
// 期望文档（伪造引用）→ wrong-span。期望 span 为空，recall 记 1.0，
// 但「无期望引用」的口径由 doc 集合比对落实，不是由 recall 落实。
func TestGrade_OutOfCorpus_FabricatedReference(t *testing.T) {
	task := Task{
		ID:         "q003",
		Prompt:     "2027 年世界杯冠军是哪支队？",
		Scope:      ScopeOutOfCorpus,
		ExpectDocs: []string{docADR15},
	}
	trace := []agent.TraceStep{
		searchCallStep(),
		searchResultStep(searchDetail(t, hitRow{ChunkID: 1, DocID: docADR15, Span: [2]int{120, 168}, Text: "无关内容"})),
	}
	g := Grade(task, trace, "根据 "+docADR15+"，2027 年世界杯冠军是……")
	if g.Success {
		t.Errorf("fabricated reference must not be Success: %+v", g)
	}
	if g.FirstError != FirstErrorWrongSpan {
		t.Errorf("FirstError = %q, want %q", g.FirstError, FirstErrorWrongSpan)
	}
	if g.SpanRecall != 1.0 {
		t.Errorf("SpanRecall = %v, want 1（无期望 span 记满分）", g.SpanRecall)
	}
}

// TestGrade_OutOfCorpus_AnswerFabricatesWithoutSearch：检索没引期望文档，
// 但回答里出现了期望文档 id（无据直引）→ 同样记 wrong-span。
// 少了这条，模型可以零检索 hallucinate 一个 doc_id 而拿到满分。
func TestGrade_OutOfCorpus_AnswerFabricatesWithoutSearch(t *testing.T) {
	task := Task{
		ID:         "q003",
		Prompt:     "2027 年世界杯冠军是哪支队？",
		Scope:      ScopeOutOfCorpus,
		ExpectDocs: []string{docADR15},
	}
	trace := []agent.TraceStep{
		searchCallStep(),
		searchResultStep(searchDetail(t, hitRow{ChunkID: 1, DocID: docADR14, Span: [2]int{0, 40}, Text: "取消语义"})),
	}
	g := Grade(task, trace, "虽然语料里没有，但照 "+docADR15+" 的说法应该是……")
	if g.Success {
		t.Errorf("answer-side fabrication must not be Success: %+v", g)
	}
	if g.FirstError != FirstErrorWrongSpan {
		t.Errorf("FirstError = %q, want %q", g.FirstError, FirstErrorWrongSpan)
	}
}

// TestGrade_OutOfCorpus_CorrectRefusal：检索了但没引期望文档、回答也未伪造 → Success。
func TestGrade_OutOfCorpus_CorrectRefusal(t *testing.T) {
	task := Task{
		ID:         "q003",
		Prompt:     "2027 年世界杯冠军是哪支队？",
		Scope:      ScopeOutOfCorpus,
		ExpectDocs: []string{docADR15},
	}
	trace := []agent.TraceStep{
		searchCallStep(),
		searchResultStep(searchDetail(t, hitRow{ChunkID: 1, DocID: docADR14, Span: [2]int{0, 40}, Text: "取消语义"})),
	}
	g := Grade(task, trace, "语料库中没有相关信息，无法回答。")
	if !g.Success {
		t.Errorf("expected Success for correct refusal, got %+v", g)
	}
	if g.FirstError != FirstErrorNone {
		t.Errorf("FirstError = %q, want %q", g.FirstError, FirstErrorNone)
	}
	if g.SpanRecall != 1.0 {
		t.Errorf("SpanRecall = %v, want 1", g.SpanRecall)
	}
}

// TestGrade_SpanRecallAtThresholdPasses：recall 恰等于阈值 0.5 记命中（>= 语义）。
// got[0,10] vs want[0,20]：交集 10 / 并集 20 = 0.5。
func TestGrade_SpanRecallAtThresholdPasses(t *testing.T) {
	task := inCorpusTask([2]int{0, 20})
	trace := []agent.TraceStep{
		searchCallStep(),
		searchResultStep(searchDetail(t, hitRow{ChunkID: 1, DocID: docADR15, Span: [2]int{0, 10}, Text: "覆盖期望区间前半"})),
	}
	g := Grade(task, trace, "依据 "+docADR15+" 作答。")
	if want := 0.5; !closeEnough(g.SpanRecall, want) {
		t.Errorf("SpanRecall = %v, want %v", g.SpanRecall, want)
	}
	if !g.Success {
		t.Errorf("recall == threshold must be Success: %+v", g)
	}
}

// TestGrade_OtherToolResultsIgnored：非 search_docs 的 tool_result 不得被当成引用，
// 没有检索调用时照样判 no-search（防把 calculate 的结果误读成检索命中）。
func TestGrade_OtherToolResultsIgnored(t *testing.T) {
	task := inCorpusTask([2]int{120, 168})
	trace := []agent.TraceStep{
		{Step: 1, Kind: agent.KindToolCall, Tool: "calculate", CallID: "c1"},
		{Step: 2, Kind: agent.KindToolResult, Tool: "calculate", Detail: "30", CallID: "c1"},
		{Step: 3, Kind: agent.KindFinal, Detail: "30"},
	}
	g := Grade(task, trace, "30（"+docADR15+"）")
	if g.Success || g.FirstError != FirstErrorNoSearch {
		t.Errorf("expected no-search, got %+v", g)
	}
}

// TestGrade_MultipleSearchCallsAccumulate：模型多次调用 search_docs 时，
// 所有 tool_result 的引用合并计分（第二次补上的命中也要算）。
func TestGrade_MultipleSearchCallsAccumulate(t *testing.T) {
	task := inCorpusTask([2]int{0, 10}, [2]int{100, 110})
	trace := []agent.TraceStep{
		searchCallStep(),
		searchResultStep(searchDetail(t, hitRow{ChunkID: 1, DocID: docADR15, Span: [2]int{3, 7}, Text: "第一次"})),
		{Step: 3, Kind: agent.KindToolCall, Tool: searchToolName, CallID: "call-2"},
		searchResultStep(searchDetail(t, hitRow{ChunkID: 2, DocID: docADR15, Span: [2]int{100, 110}, Text: "第二次"})),
	}
	g := Grade(task, trace, "依据 "+docADR15+"。")
	if want := 0.7; !closeEnough(g.SpanRecall, want) {
		t.Errorf("SpanRecall = %v, want %v", g.SpanRecall, want)
	}
	if !g.Success {
		t.Errorf("expected Success, got %+v", g)
	}
}

// TestGrade_TruncatedToolResultDetail：agent.TraceBuffer.Append 会把 detail 截到
// 512 字节（AGENTS.md §3「Trace 有界」）。检索返回的 text 动辄上千字节，
// 线上 trace 里的 tool_result 几乎必然是残串——判定器必须能抢救出其中
// 已完整的 hit，否则全量任务会被静默判成 wrong-span（假红报表）。
func TestGrade_TruncatedToolResultDetail(t *testing.T) {
	const shortDoc = "docs/adr/0015.md" // 短 id：512 字节预算内放得下第一条完整 hit
	task := Task{ID: "q001", Scope: ScopeInCorpus, ExpectDocs: []string{shortDoc}, ExpectSpans: [][2]int{{120, 168}}}

	long := strings.Repeat("语料内容", 400)
	full := searchDetail(t,
		hitRow{ChunkID: 1, DocID: shortDoc, Span: [2]int{120, 168}, Text: strings.Repeat("x", 300)},
		hitRow{ChunkID: 2, DocID: shortDoc, Span: [2]int{900, 950}, Text: long},
	)
	truncated := truncateDetail512(full)
	if len(truncated) != 512 {
		t.Fatalf("test setup: truncated len = %d, want 512", len(truncated))
	}
	// 前置条件：残串必须是非法 JSON，否则本用例没测到抢救路径。
	if json.Valid([]byte(truncated)) {
		t.Fatal("test setup: truncated detail unexpectedly still valid JSON")
	}

	trace := []agent.TraceStep{searchCallStep(), searchResultStep(truncated)}
	g := Grade(task, trace, "依据 "+shortDoc+" 作答。")
	if g.SpanRecall != 1.0 {
		t.Errorf("SpanRecall = %v, want 1（残串里第一条 hit 完整应被抢救）", g.SpanRecall)
	}
	if !g.Success || g.FirstError != FirstErrorNone {
		t.Errorf("expected Success on salvaged hits, got %+v", g)
	}

	// 对照组：同一份未截断的 detail 必须给出相同判定（抢救不是另一套口径）。
	gFull := Grade(task, []agent.TraceStep{searchCallStep(), searchResultStep(full)}, "依据 "+shortDoc+" 作答。")
	if gFull.SpanRecall != g.SpanRecall || gFull.Success != g.Success {
		t.Errorf("salvaged %+v diverges from full %+v", g, gFull)
	}
}

// TestSpanScore 固化区间交并比的单一实现：相同=1、不相交=0、包含=交/并、
// 部分相交=交/并、零长区间记 0（绝不允许 NaN 渗进 recall）。
func TestSpanScore(t *testing.T) {
	cases := []struct {
		name      string
		got       [2]int
		span      [2]int
		wantScore float64
	}{
		{"完全相同", [2]int{10, 20}, [2]int{10, 20}, 1},
		{"不相交", [2]int{10, 20}, [2]int{30, 40}, 0},
		{"got 包含 span", [2]int{10, 30}, [2]int{15, 20}, 5.0 / 20.0},
		{"部分相交", [2]int{10, 20}, [2]int{15, 25}, 5.0 / 15.0},
		{"首尾相接不算相交", [2]int{10, 20}, [2]int{20, 30}, 0},
		{"零长 got", [2]int{10, 10}, [2]int{10, 20}, 0},
		{"双向零长", [2]int{10, 10}, [2]int{10, 10}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := spanScore(c.got, c.span); !closeEnough(got, c.wantScore) {
				t.Errorf("spanScore(%v, %v) = %v, want %v", c.got, c.span, got, c.wantScore)
			}
		})
	}
}

// TestSpanRecall_EmptyWantIsFullScore：want 为空 → recall 记 1.0。
// 无期望引用题（out-of-corpus）的口径是「无伪造引用」，由 Grade 的
// out-of-corpus 分支经 doc 集合比对落实；此处记满分是为了不把
// 「没有期望」误算成「全不命中」——否则空 want 会让所有题恒为 0。
func TestSpanRecall_EmptyWantIsFullScore(t *testing.T) {
	if got := spanRecall([][2]int{{0, 10}}, nil); got != 1.0 {
		t.Errorf("spanRecall(got, nil) = %v, want 1", got)
	}
	if got := spanRecall(nil, nil); got != 1.0 {
		t.Errorf("spanRecall(nil, nil) = %v, want 1", got)
	}
}

// TestSpanRecall_BestPerWant：每个 want 独立取最大匹配后求和再平均，
// got 为空但 want 非空 → 0。
func TestSpanRecall_BestPerWant(t *testing.T) {
	got := [][2]int{{3, 7}, {100, 110}}
	want := [][2]int{{0, 10}, {100, 110}}
	if r := spanRecall(got, want); !closeEnough(r, 0.7) {
		t.Errorf("spanRecall = %v, want 0.7", r)
	}
	if r := spanRecall(nil, want); r != 0 {
		t.Errorf("spanRecall(nil, want) = %v, want 0", r)
	}
}

// TestGrade_CitedFieldsCarryActualHits：GradeResult 必须把判定用的实际引用
// （collectSearch 已解析出的 hits）原样带出——ADR-0018 决策 4 明细表
// 「命中 span」列的数据源。只加输出字段：Success/SpanRecall/FirstError
// 的判定路径不动（该 trace 的 recall 0.7 语义由上面的既有用例覆盖）。
func TestGrade_CitedFieldsCarryActualHits(t *testing.T) {
	task := inCorpusTask([2]int{0, 10}, [2]int{100, 110})
	trace := []agent.TraceStep{
		searchCallStep(),
		searchResultStep(searchDetail(t,
			hitRow{ChunkID: 1, DocID: docADR15, Span: [2]int{3, 7}, Text: "低分但覆盖第一个期望"},
			hitRow{ChunkID: 2, DocID: docADR14, Span: [2]int{100, 110}, Text: "精确命中第二个期望"},
		)),
	}
	g := Grade(task, trace, "综合 "+docADR15+" 与 "+docADR14+" 的两处说明。")
	wantSpans := [][2]int{{3, 7}, {100, 110}}
	wantDocs := []string{docADR15, docADR14}
	if !reflect.DeepEqual(g.CitedSpans, wantSpans) {
		t.Errorf("CitedSpans = %v, want %v（判定用的实际引用必须落字段）", g.CitedSpans, wantSpans)
	}
	if !reflect.DeepEqual(g.CitedDocs, wantDocs) {
		t.Errorf("CitedDocs = %v, want %v", g.CitedDocs, wantDocs)
	}
}

// TestSalvageHits_FieldLevelExtraction 是 Task 12 缺陷 F 的承重墙（变异自证 ②）：
// 截断点落在某个 hit 的 text 内部时，该 hit 的 doc_id/span 早已完整，
// 字段级提取必须把它们全部救出；改回逐条 json.Decode 则只剩截断点之前的完整对象
// （线上 512 字节连第一条 text 都放不下 → 一条都救不回，全量任务被判 wrong-span）。
func TestSalvageHits_FieldLevelExtraction(t *testing.T) {
	thirdText := "第三段原文。" + strings.Repeat("语", 400) // 长 text：截断点必然落在它内部
	full := searchDetail(t,
		hitRow{ChunkID: 1, DocID: docADR15, Source: docADR15, Span: [2]int{120, 168}, Text: "第一段原文。" + strings.Repeat("甲", 20)},
		hitRow{ChunkID: 2, DocID: docADR15, Source: docADR15, Span: [2]int{300, 340}, Text: "第二段原文。" + strings.Repeat("乙", 20)},
		hitRow{ChunkID: 3, DocID: docADR14, Source: docADR14, Span: [2]int{9, 21}, Text: thirdText},
	)
	wantAll := []toolHit{
		{DocID: docADR15, Span: [2]int{120, 168}},
		{DocID: docADR15, Span: [2]int{300, 340}},
		{DocID: docADR14, Span: [2]int{9, 21}},
	}

	t.Run("cut inside third text rescues every complete pair", func(t *testing.T) {
		i := strings.Index(full, thirdText)
		if i < 0 {
			t.Fatal("test setup: third hit text not found in marshalled detail")
		}
		cut := i + len("第三段原文。") + 3 // 落进第三条 text 内部（"语" 3 字节，切点仍在 rune 边界）
		detail := full[:cut]
		if json.Valid([]byte(detail)) {
			t.Fatal("test setup: truncated detail unexpectedly still valid JSON")
		}
		if got := salvageHits(detail); !reflect.DeepEqual(got, wantAll) {
			t.Errorf("salvageHits = %+v, want %+v（第三条的 doc_id/span 在 text 之前，必须被救出）", got, wantAll)
		}
	})

	t.Run("cut between doc_id and span drops the incomplete pair", func(t *testing.T) {
		i := strings.LastIndex(full, `"span":[9,21]`)
		if i < 0 {
			t.Fatal("test setup: third hit span not found")
		}
		detail := full[:i+len(`"span":[9`)] // span 数字段被截断
		if got, want := salvageHits(detail), wantAll[:2]; !reflect.DeepEqual(got, want) {
			t.Errorf("salvageHits = %+v, want %+v（不完整的 doc_id/span 对必须丢弃，不补假引用）", got, want)
		}
	})

	t.Run("no pairs fall back to nil without panic", func(t *testing.T) {
		for _, detail := range []string{
			"",
			"error: index missing; run `-index .` first",
		} {
			if got := parseHits(detail); got != nil {
				t.Errorf("parseHits(%q) = %+v, want nil", detail, got)
			}
			if got := salvageHits(detail); got != nil {
				t.Errorf("salvageHits(%q) = %+v, want nil", detail, got)
			}
		}
		// 空 hits 数组走 Unmarshal 原生路径：零长切片（历史语义），关键是零条且不 panic。
		if got := parseHits(`{"mode":"lexical","truncated":false,"hits":[]}`); len(got) != 0 {
			t.Errorf("parseHits(empty hits) = %+v, want zero-length", got)
		}
	})

	t.Run("complete JSON keeps the unmarshal semantics", func(t *testing.T) {
		got := parseHits(full)
		if !reflect.DeepEqual(got, wantAll) {
			t.Fatalf("parseHits(complete JSON) = %+v, want %+v", got, wantAll)
		}
		// 字段级提取对完整串必须给同一答案（两条路径同口径，不是两套判定）。
		if alt := salvageHits(full); !reflect.DeepEqual(alt, got) {
			t.Errorf("salvageHits(complete JSON) = %+v, want %+v (diverges from unmarshal path)", alt, got)
		}
	})
}

// truncateDetail512 镜像 agent.TraceBuffer.Append 的 detail 截断逻辑
// （512 字节硬上限 + utf8.RuneStart 回退到字符边界）。
func truncateDetail512(detail string) string {
	const max = 512
	if len(detail) <= max {
		return detail
	}
	cutoff := max
	for cutoff > 0 && !utf8.RuneStart(detail[cutoff]) {
		cutoff--
	}
	return detail[:cutoff]
}
