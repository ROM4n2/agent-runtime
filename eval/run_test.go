package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"agent-runtime/agent"
	"agent-runtime/agent/tool"
	"agent-runtime/llm"
	"agent-runtime/prompts"
	"agent-runtime/retrieval"
)

// ---- 测试 fixture：语料 / 任务集 / 假 Chatter ----
//
// 坐标约定（与线上同一套）：retrieval 用 corpusDir 下的根相对路径做 doc_id，
// 任务集 expect_docs 与 grader 的引用匹配都按这个坐标，故测试语料必须放在
// 嵌套子目录下，用 "docs/adr/test.md" 这种路径型 id——过短 id 会让匹配语义
// 与线上脱节（grader_test.go 同款纪律）。

const (
	// evalDocID 是测试语料的 doc_id（corpusDir 根相对路径）。
	evalDocID = "docs/adr/test.md"
	// evalDocText 是初始语料正文。查询串必须是其中 ≥3 字的连续子串：
	// trigram 分词器对 2 字查询零命中（ADR-0017 修订注记）。
	evalDocText = "评测语料基准内容：分块事务、内容指纹与混合召回通道的设计约定。"
	// evalQuery 是假 Chatter 对 in-corpus 题发起的检索查询。
	evalQuery = "评测语料基准内容"
	// evalQueryOut 是 out-of-corpus 题的查询：语料里没有，检索应零命中。
	evalQueryOut = "世界杯冠军得主"
	// evalDocTextFresh 是测试 ⑦ 改版后的语料（陈旧索引必须重建后才能命中）。
	evalDocTextFresh = evalDocText + "重建后的新内容必须能被检索命中。"
	// freshMarker 只出现在改版后的语料里，是「用的是新索引」的判据。
	freshMarker = "重建后的新内容必须能被检索命中"
	// outMarker 是任务提示词里的语料外标记，假 Chatter 据此换不命中查询。
	outMarker = "2077"
)

// writeEvalDoc 在语料目录写测试文档，返回其 rune 长度。
// 短文档（<512 rune）只有一个 chunk，span 即 [0, runeLen]，任务集据此写期望区间。
func writeEvalDoc(t *testing.T, corpusDir, text string) int {
	t.Helper()
	full := filepath.Join(corpusDir, filepath.FromSlash(evalDocID))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", evalDocID, err)
	}
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		t.Fatalf("write %s: %v", evalDocID, err)
	}
	return utf8.RuneCountInString(text)
}

// writeEvalTaskSet 写 2 条任务的测试任务集：一条 in-corpus 直答（期望区间 =
// 唯一 chunk 的 span），一条 out-of-corpus 考察不编造（期望无引用）。
// 复用 task_test.go 的 writeTaskSet：任务集入口必须是「文件解析」这一层。
func writeEvalTaskSet(t *testing.T, spanEnd int) string {
	t.Helper()
	set := fmt.Sprintf(`[
  {
    "id": "q001",
    "prompt": "评测语料的基准内容是什么？（回答请引用 %s）",
    "scope": "in-corpus",
    "expect_docs": [%q],
    "expect_spans": [[0, %d]]
  },
  {
    "id": "q002",
    "prompt": "%s 年世界杯冠军是哪支队？",
    "scope": "out-of-corpus",
    "expect_docs": [%q],
    "expect_spans": []
  }
]`, evalDocID, evalDocID, spanEnd, outMarker, evalDocID)
	return writeTaskSet(t, set)
}

// evalFakeChatter 是走全链路的假 LLM。判定材料全部取自 agent.Run 回填的
// role=tool 消息，不自己编造检索结果——否则 grader 验的是假件而不是链路。
// 工具面（tools 形参）原样记录，供「组 → 工具开关」断言使用。
type evalFakeChatter struct {
	mu          sync.Mutex
	seenTools   [][]string // 每轮收到的工具名集合
	toolResults []string   // 收到的 role=tool 消息原文
}

func (f *evalFakeChatter) ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (*llm.AssistantTurn, error) {
	// ctx 感知：与真实 llm.Client 同款——假件比真件更宽容是假绿源。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	f.seenTools = append(f.seenTools, toolNames(tools))
	toolResult := lastToolMessage(msgs)
	if toolResult != "" {
		f.toolResults = append(f.toolResults, toolResult)
		// 有检索结果且其中出现期望文档 → 引用它给结论（in-corpus 成功形态）；
		// 结果里没有期望文档（如零命中）→ 拒答，不编造引用。
		if strings.Contains(toolResult, evalDocID) {
			return &llm.AssistantTurn{Content: "结论：依据 " + evalDocID + " 的记载，基准内容见检索结果。"}, nil
		}
		return &llm.AssistantTurn{Content: "语料库中没有相关信息，无法回答。"}, nil
	}
	if !hasSearchTool(tools) {
		// baseline 组：工具面里没有 search_docs，模型无从检索。
		return &llm.AssistantTurn{Content: "没有检索工具，只能凭记忆作答。"}, nil
	}
	if firstPromptHas(msgs, outMarker) {
		return &llm.AssistantTurn{Content: "先确认语料里有没有", ToolCalls: []llm.ToolCall{searchCall(evalQueryOut)}}, nil
	}
	return &llm.AssistantTurn{Content: "先检索", ToolCalls: []llm.ToolCall{searchCall(evalQuery)}}, nil
}

// anyTurnHasTool 判断任一轮收到的工具面里是否有 name。
func (f *evalFakeChatter) anyTurnHasTool(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, turn := range f.seenTools {
		for _, got := range turn {
			if got == name {
				return true
			}
		}
	}
	return false
}

// sawToolText 判断任一路工具返回原文里是否出现 sub（测试 ⑦ 的新内容判据）。
func (f *evalFakeChatter) sawToolText(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, res := range f.toolResults {
		if strings.Contains(res, sub) {
			return true
		}
	}
	return false
}

func toolNames(tools []llm.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Function.Name)
	}
	return names
}

func hasSearchTool(tools []llm.Tool) bool {
	for _, t := range tools {
		if t.Function.Name == searchToolName {
			return true
		}
	}
	return false
}

// lastToolMessage 取最后一条 role=tool 消息内容（agent.Run 的工具回填）。
func lastToolMessage(msgs []llm.Message) string {
	var out string
	for _, m := range msgs {
		if m.Role == "tool" && m.Content != "" {
			out = m.Content
		}
	}
	return out
}

// firstPromptHas 判断首条 user 消息（= 任务 prompt）是否含 sub。
func firstPromptHas(msgs []llm.Message, sub string) bool {
	for _, m := range msgs {
		if m.Role == "user" {
			return strings.Contains(m.Content, sub)
		}
	}
	return false
}

// searchCall 构造一次 search_docs 调用（ID 固定：每任务第一轮只调一次）。
func searchCall(query string) llm.ToolCall {
	args, err := json.Marshal(map[string]any{"query": query, "top_k": 5})
	if err != nil {
		panic(err) // 测试夹具：map 序列化不可能失败
	}
	return llm.ToolCall{
		ID:       "call-1",
		Type:     "function",
		Function: llm.ToolCallFn{Name: searchToolName, Arguments: string(args)},
	}
}

// newRunFixture 造一份最小可跑 fixture：临时语料 + 临时任务集 + 临时索引库 +
// 临时报表目录。默认 lexical 组 / repeat=2 / 假 Chatter，各测试按需改字段。
func newRunFixture(t *testing.T, text string) (RunConfig, *evalFakeChatter) {
	t.Helper()
	corpus := t.TempDir()
	spanEnd := writeEvalDoc(t, corpus, text)
	chatter := &evalFakeChatter{}
	cfg := RunConfig{
		SetPath:         writeEvalTaskSet(t, spanEnd),
		CorpusDir:       corpus,
		Group:           GroupLexical,
		Repeat:          2,
		OutDir:          t.TempDir(),
		DBPath:          filepath.Join(t.TempDir(), "index.db"),
		Seed:            7,
		Model:           "test-model",
		TemplateVersion: prompts.EvalV1,
		Chatter:         chatter,
	}
	return cfg, chatter
}

// aggFloat 从聚合表取 float64；缺键或类型不符即测试失败——
// 聚合表由 aggregate 单一实现产出，拿不到值说明键名/类型已漂移。
func aggFloat(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("aggregates[%q] missing or not float64: %v", key, m[key])
	}
	return v
}

// ---- ① 假链路全流程：repeat / 明细条数 / 分布键 ----

// TestRun_FakeChainDistributionAndAggregates 是主路径承重墙：2 任务 × repeat=2
// 必须产出 4 条明细，且聚合表必出中位数与 P25/P75（ADR-0018 决策 3：
// 单次值只进明细，聚合必出分布）。同时钉死环境四元组进报表与
// token 计数未接入时的显式标注。
func TestRun_FakeChainDistributionAndAggregates(t *testing.T) {
	cfg, _ := newRunFixture(t, evalDocText)
	report, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Repeat != 2 {
		t.Errorf("Repeat = %d, want 2", report.Repeat)
	}
	if len(report.TaskResults) != 4 {
		t.Fatalf("TaskResults = %d, want 4 (2 tasks x repeat 2)", len(report.TaskResults))
	}
	// 明细顺序：任务主序、repeat 内序（审计对账锚）。
	for i, want := range []string{"q001", "q001", "q002", "q002"} {
		if report.TaskResults[i].TaskID != want {
			t.Errorf("TaskResults[%d].TaskID = %q, want %q", i, report.TaskResults[i].TaskID, want)
		}
	}
	// 分布键：均值替代中位数的变异必须让这些断言转红。
	for _, key := range []string{"success_median", "success_p25", "success_p75", "span_recall_median"} {
		if _, ok := report.Aggregates[key]; !ok {
			t.Errorf("aggregates missing key %q (distribution, not a bare mean)", key)
		}
	}
	// 假链路下两题都应判定成功（检索命中 + 引用期望文档 / 语料外拒答）。
	if got := aggFloat(t, report.Aggregates, "success_median"); got != 1 {
		t.Errorf("success_median = %v, want 1 (both tasks succeed on the fake chain)", got)
	}
	if got := aggFloat(t, report.Aggregates, "span_recall_median"); got != 1 {
		t.Errorf("span_recall_median = %v, want 1", got)
	}
	// 环境四元组：RunConfig 单处读取，原样进报表（成对受控的对账锚）。
	env := report.Env
	if env.Group != GroupLexical || env.Seed != 7 || env.Model != "test-model" ||
		env.TemplateVersion != prompts.EvalV1 || env.CorpusDir != cfg.CorpusDir ||
		env.Repeat != 2 || env.SetPath != cfg.SetPath {
		t.Errorf("env mismatch: %+v (cfg group=%q seed=%d model=%q tpl=%q corpus=%q repeat=%d set=%q)",
			env, cfg.Group, cfg.Seed, cfg.Model, cfg.TemplateVersion, cfg.CorpusDir, cfg.Repeat, cfg.SetPath)
	}
	if env.SetHash == "" {
		t.Error("Env.SetHash empty, want the task-set file SHA-256")
	}
	// token 计数未接入：明细记 0 且报表显式标注，不静默填一个看起来正常的数字。
	if env.TokenCounting != "not-wired" {
		t.Errorf("Env.TokenCounting = %q, want %q (nil TokenCounter must be declared)", env.TokenCounting, "not-wired")
	}
	for _, r := range report.TaskResults {
		if r.PromptTokens != 0 || r.CompletionTokens != 0 {
			t.Errorf("task %s tokens = %d/%d, want 0/0 when the counter is not wired", r.TaskID, r.PromptTokens, r.CompletionTokens)
		}
	}
}

// ---- ② 组语义：baseline 无 search_docs，lexical 有 ----

// TestRun_GroupToolSurface 验证 group 是工具面的单变量开关（ADR-0018 决策 2.1）：
// baseline 组的假 Chatter 任何一轮都不应看到 search_docs，lexical 组必须看到。
func TestRun_GroupToolSurface(t *testing.T) {
	t.Run("baseline 无 search_docs", func(t *testing.T) {
		cfg, chatter := newRunFixture(t, evalDocText)
		cfg.Group = GroupBaseline
		if _, err := Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if chatter.anyTurnHasTool(searchToolName) {
			t.Errorf("baseline 组工具面不得含 %s（检索前形态，禁混入自变量）", searchToolName)
		}
	})
	t.Run("lexical 含 search_docs", func(t *testing.T) {
		cfg, chatter := newRunFixture(t, evalDocText)
		if _, err := Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !chatter.anyTurnHasTool(searchToolName) {
			t.Errorf("lexical 组工具面必须含 %s", searchToolName)
		}
	})
}

// ---- ③ 报表落盘与固定节 ----

// TestRun_WritesReportFilesWithFixedSections 验证 OutDir 覆写下 md/json 双产物
// 落盘，且 markdown 含 ADR-0018 决策 4 的全部固定节标题（缺一节即审计断链）。
func TestRun_WritesReportFilesWithFixedSections(t *testing.T) {
	cfg, _ := newRunFixture(t, evalDocText)
	report, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.MarkdownPath == "" || report.JSONPath == "" {
		t.Fatalf("report paths empty: md=%q json=%q", report.MarkdownPath, report.JSONPath)
	}
	md, err := os.ReadFile(report.MarkdownPath)
	if err != nil {
		t.Fatalf("read markdown: %v", err)
	}
	for _, section := range []string{"## 任务集", "## 环境", "## 指标", "## 逐任务明细", "## 首错 Top3", "## 结论"} {
		if !strings.Contains(string(md), section) {
			t.Errorf("markdown missing fixed section %q (ADR-0018 决策 4)", section)
		}
	}
	// JSON 必须机器可读且带全量明细（复跑锚）。
	raw, err := os.ReadFile(report.JSONPath)
	if err != nil {
		t.Fatalf("read json: %v", err)
	}
	var parsed Report
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal report json: %v", err)
	}
	if len(parsed.TaskResults) != 4 || parsed.Group != GroupLexical || parsed.Repeat != 2 {
		t.Errorf("json report = group %q repeat %d results %d, want %q/2/4",
			parsed.Group, parsed.Repeat, len(parsed.TaskResults), GroupLexical)
	}
	// 产物名形态：YYYY-MM-DD-<group>-r<N>.md（ADR-0018 决策 1）。
	wantPrefix := time.Now().Format("2006-01-02") + "-" + GroupLexical + "-r2."
	if base := filepath.Base(report.MarkdownPath); !strings.HasPrefix(base, wantPrefix) || filepath.Ext(base) != ".md" {
		t.Errorf("markdown file name = %q, want prefix %q and .md suffix", base, wantPrefix)
	}
}

// TestRun_DetailTableCarriesCitedSpans：逐任务明细表必须带「命中 span」列
// （ADR-0018 决策 4）——判定用的实际引用（doc_id+span 缩写）随 repeat 明细
// 落表，复跑对账不用回翻 trace 原文；列缺失 = 审计断链。
// 零命中题显式记「（无）」：禁静默留空单元格（留空与「没渲染」不可分辨）。
func TestRun_DetailTableCarriesCitedSpans(t *testing.T) {
	cfg, _ := newRunFixture(t, evalDocText)
	report, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	md, err := os.ReadFile(report.MarkdownPath)
	if err != nil {
		t.Fatalf("read markdown: %v", err)
	}
	if !strings.Contains(string(md), "命中 span") {
		t.Error("markdown detail table missing 「命中 span」 column (ADR-0018 决策 4)")
	}
	// 单 chunk 短文档：命中 span 即 [0, runeLen]；doc_id 缩为 basename 编号段。
	wantCite := fmt.Sprintf("%s@[0,%d)", "test.md", utf8.RuneCountInString(evalDocText))
	var q001Row, q002Row string
	for _, line := range strings.Split(string(md), "\n") {
		switch {
		case strings.HasPrefix(line, "| q001 "):
			q001Row = line
		case strings.HasPrefix(line, "| q002 "):
			q002Row = line
		}
	}
	if !strings.Contains(q001Row, wantCite) {
		t.Errorf("q001 row missing cited span %q: %s", wantCite, q001Row)
	}
	if !strings.Contains(q002Row, "（无）") {
		t.Errorf("q002 row (zero hits) must render （无）, got: %s", q002Row)
	}
}

// ---- ④ ⑤ ⑥ harness 自身故障 → error ----

// TestRun_InvalidTaskSetIsHarnessFailure：任务集非法 = harness 故障，
// 必须显式 error（退出码语义的根），不能静默跳过坏题。
func TestRun_InvalidTaskSetIsHarnessFailure(t *testing.T) {
	corpus := t.TempDir()
	writeEvalDoc(t, corpus, evalDocText)
	cfg := RunConfig{
		SetPath:   writeTaskSet(t, `[{"id":"q1","prompt":"p","scope":"nope","expect_docs":["d"],"expect_spans":[[1,2]]}]`),
		CorpusDir: corpus,
		Group:     GroupLexical,
		Repeat:    1,
		DBPath:    filepath.Join(t.TempDir(), "index.db"),
		Chatter:   &evalFakeChatter{},
	}
	if _, err := Run(context.Background(), cfg); err == nil {
		t.Fatal("expected error for invalid task set (harness failure, not a business outcome)")
	}
}

// TestRun_MissingCorpusDirIsHarnessFailure：语料目录不存在必须早失败，
// 不能等索引构建到一半才报，更不能拿空语料跑出一份看起来正常的报表。
func TestRun_MissingCorpusDirIsHarnessFailure(t *testing.T) {
	corpus := t.TempDir()
	spanEnd := writeEvalDoc(t, corpus, evalDocText)
	cfg := RunConfig{
		SetPath:   writeEvalTaskSet(t, spanEnd),
		CorpusDir: filepath.Join(t.TempDir(), "no-such-dir"),
		Group:     GroupLexical,
		Repeat:    1,
		DBPath:    filepath.Join(t.TempDir(), "index.db"),
		Chatter:   &evalFakeChatter{},
	}
	if _, err := Run(context.Background(), cfg); err == nil {
		t.Fatal("expected error for missing corpus dir")
	}
}

// TestRun_HybridWithoutCredentialsFailsLoud 钉死 ADR-0017 决策 3 的禁静默降级：
// hybrid 组无 embedding 凭证必须显式报错，而不是偷偷按 lexical 跑
// （那会让「混合召回无提升」的结论建立在一次从没跑过 hybrid 的报表上）。
func TestRun_HybridWithoutCredentialsFailsLoud(t *testing.T) {
	cfg, _ := newRunFixture(t, evalDocText)
	cfg.Group = GroupHybrid // fixture 不设 IndexOptions.Embedder：无凭证
	_, err := Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for hybrid group without embedder credentials")
	}
	if !strings.Contains(err.Error(), "hybrid group requires embedder credentials") {
		t.Errorf("error = %v, want it to name the missing embedder credentials", err)
	}
}

// ---- ⑦ 陈旧索引自动重建 ----

// TestRun_StaleIndexAutoRebuild 验证评测预检：索引陈旧时 Run 必须自动 Rebuild
// （staleness 是 harness 故障不是业务结论），且重建后检索确实命中新内容。
// 变异自证 ③ 承重墙：把自动重建删掉 → 工具返回 ErrCorpusStale，本断言转红。
func TestRun_StaleIndexAutoRebuild(t *testing.T) {
	corpus := t.TempDir()
	writeEvalDoc(t, corpus, evalDocText)
	dbPath := filepath.Join(t.TempDir(), "index.db")

	// 先按旧语料建索引（上次评测的残留）。
	old, err := retrieval.Open(dbPath, retrieval.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := old.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild (old corpus): %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 改语料 → 已建索引的指纹与目录不符（陈旧）。
	writeEvalDoc(t, corpus, evalDocTextFresh)

	chatter := &evalFakeChatter{}
	report, err := Run(context.Background(), RunConfig{
		SetPath:         writeEvalTaskSet(t, utf8.RuneCountInString(evalDocTextFresh)),
		CorpusDir:       corpus,
		Group:           GroupLexical,
		Repeat:          1,
		OutDir:          t.TempDir(),
		DBPath:          dbPath,
		Chatter:         chatter,
		Model:           "test-model",
		TemplateVersion: prompts.EvalV1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !chatter.sawToolText(freshMarker) {
		t.Errorf("stale index must be rebuilt before the run: tool result never contained %q", freshMarker)
	}
	if len(report.TaskResults) != 2 {
		t.Errorf("TaskResults = %d, want 2 (1 task set entry x 2 tasks x repeat 1)", len(report.TaskResults))
	}
}

// ---- ⑨ 缺陷 G：评测取工具返回全文，不受 trace 512B 截断影响 ----

// fakeSearchTool 是 search_docs 的返回来注入点：用来把「返回串长度」变成
// 受控变量——真实索引的返回串长度不可控，测不到截断边界。
type fakeSearchTool struct{ out string }

func (f *fakeSearchTool) Spec() llm.Tool {
	return llm.Tool{
		Type:     "function",
		Function: llm.FunctionSpec{Name: searchToolName, Parameters: map[string]any{"type": "object"}},
	}
}

func (f *fakeSearchTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return f.out, nil
}

// TestRun_CollectorUsesFullToolOutput 是缺陷 G 的承重墙（变异自证 ②）：
// 假工具返回 >512 字节的 JSON，匹配 span 只在第 2 条命中里——trace 的 512 字节
// detail 必然丢掉它。runOnce 若不从包装工具取全文（改回只用 trace），
// SpanRecall 会掉到阈值以下，本断言转红。
func TestRun_CollectorUsesFullToolOutput(t *testing.T) {
	task := Task{
		ID:          "q009",
		Prompt:      "评测语料的分块约定是什么？（回答请引用 " + evalDocID + "）",
		Scope:       ScopeInCorpus,
		ExpectDocs:  []string{evalDocID},
		ExpectSpans: [][2]int{{441, 780}},
	}
	full := searchDetail(t,
		hitRow{ChunkID: 1, DocID: evalDocID, Span: [2]int{0, 512}, Text: strings.Repeat("甲", 400)},
		hitRow{ChunkID: 2, DocID: evalDocID, Span: [2]int{441, 780}, Text: strings.Repeat("乙", 400)},
	)
	if len(full) <= 512 {
		t.Fatalf("test setup: fake tool output = %d bytes, want > 512 (trace detail budget)", len(full))
	}

	reg := tool.NewRegistry()
	reg.Register(&fakeSearchTool{out: full})
	tr, err := runOnce(context.Background(), RunConfig{}, &evalFakeChatter{}, reg, task)
	if err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if !closeEnough(tr.SpanRecall, 1) {
		t.Errorf("SpanRecall = %v, want 1（判定必须取工具返回全文，不是 512B 截断的 trace）", tr.SpanRecall)
	}
	if !tr.Success || tr.FirstError != FirstErrorNone {
		t.Errorf("expected Success from full tool output, got %+v", tr)
	}

	// 前提自证：同一份返回串截断到 512 字节后判定确实失真，否则本用例没测到差异。
	truncated := truncateDetail512(full)
	g := Grade(task, []agent.TraceStep{searchCallStep(), searchResultStep(truncated)}, "依据 "+evalDocID+" 作答。")
	if g.SpanRecall >= spanRecallThreshold {
		t.Fatalf("test setup: truncated trace recall = %v, want < threshold（本用例没测到全文与截断的差异）", g.SpanRecall)
	}
}

// ---- ⑧ TokenCounter 注入 ----

// TestRun_TokenCounterInjection 验证可选注入点被真正调用，且明细两侧都有值；
// 报表环境同步标注计数已接入。
func TestRun_TokenCounterInjection(t *testing.T) {
	cfg, _ := newRunFixture(t, evalDocText)
	var calls int32
	cfg.TokenCounter = func(prompt, completion string) int {
		atomic.AddInt32(&calls, 1)
		// 粗估：按字节数计（测试只验「被调用且两侧非零」，不验精度）。
		return len(prompt) + len(completion)
	}
	report, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Error("TokenCounter injected but never called")
	}
	if report.Env.TokenCounting != "injected" {
		t.Errorf("Env.TokenCounting = %q, want %q", report.Env.TokenCounting, "injected")
	}
	for _, r := range report.TaskResults {
		if r.PromptTokens <= 0 || r.CompletionTokens <= 0 {
			t.Errorf("task %s tokens = %d/%d, want both > 0", r.TaskID, r.PromptTokens, r.CompletionTokens)
		}
	}
}

func TestRun_InvalidTemplateVersion_FailsFast(t *testing.T) {
	cfg, _ := newRunFixture(t, evalDocText)
	cfg.TemplateVersion = "non-existent-template"
	_, err := Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for non-existent template version, got nil")
	}
	if !strings.Contains(err.Error(), "invalid TemplateVersion") {
		t.Fatalf("expected error containing %q, got: %v", "invalid TemplateVersion", err)
	}
}

type usageMockChatter struct {
	turn int
}

func (m *usageMockChatter) ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (*llm.AssistantTurn, error) {
	m.turn++
	if m.turn == 1 {
		return &llm.AssistantTurn{
			ToolCalls: []llm.ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: llm.ToolCallFn{
						Name:      "calculate",
						Arguments: `{"expression":"1+1"}`,
					},
				},
			},
			Usage: llm.Usage{
				PromptTokens:     10,
				CompletionTokens: 5,
				TotalTokens:      15,
			},
		}, nil
	}
	return &llm.AssistantTurn{
		Content: "result is 2",
		Usage: llm.Usage{
			PromptTokens:     8,
			CompletionTokens: 4,
			TotalTokens:      12,
		},
	}, nil
}

func TestRun_RealLLMUsageAccumulation(t *testing.T) {
	set := `[
  {
    "id": "q_usage",
    "prompt": "calculate 1+1",
    "scope": "in-corpus",
    "expect_docs": ["doc1.md"],
    "expect_spans": []
  }
]`
	setPath := writeTaskSet(t, set)
	chatter := &usageMockChatter{}
	cfg := RunConfig{
		SetPath:         setPath,
		Group:           GroupBaseline,
		Repeat:          1,
		CorpusDir:       t.TempDir(),
		TemplateVersion: prompts.EvalV1,
		Chatter:         chatter,
	}
	report, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(report.TaskResults) != 1 {
		t.Fatalf("expected 1 task result, got %d", len(report.TaskResults))
	}
	tr := report.TaskResults[0]
	if tr.PromptTokens != 18 {
		t.Errorf("PromptTokens = %d, want 18", tr.PromptTokens)
	}
	if tr.CompletionTokens != 9 {
		t.Errorf("CompletionTokens = %d, want 9", tr.CompletionTokens)
	}
	if report.Env.TokenCounting != "llm-usage" {
		t.Errorf("TokenCounting = %q, want %q", report.Env.TokenCounting, "llm-usage")
	}
	if note := tokenCountingNote("llm-usage"); note != "llm-usage（API 实测返回）" {
		t.Errorf("tokenCountingNote(\"llm-usage\") = %q, want %q", note, "llm-usage（API 实测返回）")
	}
}
