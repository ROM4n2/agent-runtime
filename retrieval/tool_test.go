package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"agent-runtime/agent/tool"
)

// execSearchTool 是工具调用的统一入口：ctx 用 Background（各用例按需传取消 ctx），
// args 原样透传——测试就要验「模型在网络上看到的 JSON」这一层。
func execSearchTool(tl tool.Tool, args string) (string, error) {
	return tl.Execute(context.Background(), json.RawMessage(args))
}

// hitRow 是工具返回 JSON 的单条命中表示（与 tool.go 的 hitJSON 字段一一对应，
// 在测试侧独立声明：契约由 wire 格式定义，不由 Go 结构体标签担保）。
type hitRow struct {
	ChunkID int64   `json:"chunk_id"`
	DocID   string  `json:"doc_id"`
	Source  string  `json:"source"`
	Span    [2]int  `json:"span"`
	Score   float64 `json:"score"`
	Text    string  `json:"text"`
}

// resultRow 是工具返回 JSON 的整体形状。
type resultRow struct {
	Mode      string   `json:"mode"`
	Truncated bool     `json:"truncated"`
	Hits      []hitRow `json:"hits"`
}

func parseResult(t *testing.T, out string) resultRow {
	t.Helper()
	var res resultRow
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("tool output is not valid JSON: %v\noutput: %s", err, out)
	}
	return res
}

// assertNotRetryable 钉死「非可重试」分类：errors.As 必须找不到 RetryableError。
// agent.Run 依此分支决策——把永久故障误判成可重试会让 agent 无意义空转。
func assertNotRetryable(t *testing.T, err error) {
	t.Helper()
	var re tool.RetryableError
	if errors.As(err, &re) {
		t.Errorf("error must be non-retryable, got Retryable()=%v: %v", re.Retryable(), err)
	}
}

// TestSearchTool_Spec 固化对外契约（变异自证 ④）：工具名、短查询提示、三参数 schema。
// 走 JSON 序列化而非直接类型断言：模型在网络上收到的就是这份 JSON，
// 断言必须落在「上线形态」而不是 Go 内部表示上。
func TestSearchTool_Spec(t *testing.T) {
	tl := SearchTool(newTestIndex(t), t.TempDir())
	b, err := json.Marshal(tl.Spec())
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	var spec struct {
		Type     string `json:"type"`
		Function struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	if spec.Function.Name != "search_docs" {
		t.Errorf("Name = %q, want search_docs", spec.Function.Name)
	}
	// 短查询提示必须出现在描述里：trigram 硬限制下 2 字中文查询零命中
	// （ADR-0017 修订注记的残留缺口），模型看不到这条提示就会反复用短查询，
	// 然后误判「库里有答案但检索不到」。
	// "3" 与 "字符" 共断言：裸查 "3" 会被描述里偶然出现的 3（版本号、注释残留）满足而假绿，
	// 两个词同时在场才构成「短查询提示」的实质内容。
	if !strings.Contains(spec.Function.Description, "3") || !strings.Contains(spec.Function.Description, "字符") {
		t.Errorf("Description must contain the >=3-char CJK hint, got %q", spec.Function.Description)
	}
	props, ok := spec.Function.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("Parameters.properties missing or not an object: %v", spec.Function.Parameters)
	}
	for _, key := range []string{"query", "top_k", "token_budget"} {
		if _, ok := props[key]; !ok {
			t.Errorf("Parameters.properties missing %q (schema must advertise all three inputs)", key)
		}
	}
	if got := props["query"].(map[string]any)["type"]; got != "string" {
		t.Errorf("query type = %v, want string", got)
	}
	if got := props["top_k"].(map[string]any)["type"]; got != "integer" {
		t.Errorf("top_k type = %v, want integer", got)
	}
	if got := props["token_budget"].(map[string]any)["type"]; got != "integer" {
		t.Errorf("token_budget type = %v, want integer", got)
	}
	required, _ := spec.Function.Parameters["required"].([]any)
	if len(required) != 1 || required[0] != "query" {
		t.Errorf("required = %v, want [query] (only the query is mandatory)", required)
	}
}

// builtLexicalTool 返回一个已建成的纯词法索引及其语料目录。
// 参数校验类测试必须跑在「已建成且新鲜」的索引上：否则 ErrIndexMissing/
// ErrCorpusStale 会先于参数校验返回错误，「删掉/改坏校验」的变异被下游哨兵
// 掩盖而假绿（第一版实测幸存：未建索引的 ErrIndexMissing 替 clamp 变异报了错）。
func builtLexicalTool(t *testing.T) (*Index, string) {
	t.Helper()
	idx := newTestIndex(t)
	corpus := t.TempDir()
	writeDoc(t, corpus, "a.md", "检索系统设计与实现")
	writeDoc(t, corpus, "b.md", "检索系统")
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	return idx, corpus
}

// TestSearchTool_MissingQueryIsPermanent 是参数校验的承重墙（变异自证同族）：
// query 空/缺失必须明确报错且 non-retryable——模型幻觉出空参数时，
// 它需要看到「缺哪个参数」的反馈来自纠，而不是一次静默的全库检索或一次无谓重试。
func TestSearchTool_MissingQueryIsPermanent(t *testing.T) {
	idx, corpus := builtLexicalTool(t)
	tl := SearchTool(idx, corpus)
	cases := []struct{ name, args string }{
		{"absent field", `{}`},
		{"empty string", `{"query":""}`},
		{"whitespace only", `{"query":"   "}`},
		{"json null", `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execSearchTool(tl, tc.args)
			if err == nil {
				t.Fatalf("Execute(%s) = %q, want error (query is required)", tc.args, out)
			}
			assertNotRetryable(t, err)
			if !strings.Contains(err.Error(), "query is required") {
				t.Errorf("error text = %q, want it to name the missing parameter for model self-correction", err)
			}
		})
	}
}

// TestSearchTool_MalformedArgsArePermanent：类型错误的入参（如 query 传数字）
// 必须明确报错。json.Unmarshal 失败不是 embedder 故障，绝不包装成可重试。
// 同样跑在已建索引上，防下游哨兵掩盖解析失败。
func TestSearchTool_MalformedArgsArePermanent(t *testing.T) {
	idx, corpus := builtLexicalTool(t)
	tl := SearchTool(idx, corpus)
	if _, err := execSearchTool(tl, `{"query":123}`); err == nil {
		t.Fatal("Execute with non-string query = nil error, want a parse error")
	} else {
		assertNotRetryable(t, err)
	}
}

// TestSearchTool_InvalidParamsArePermanent 是 top_k/token_budget 校验的承重墙
// （变异自证 ③）：越界即报错而非 clamp。模型幻觉出的参数必须得到明确反馈，
// 静默修正会让「我要 10 条」悄悄变成 10（或 5），模型无从得知口径已变。
// 索引必须已建成：跑在未建索引上时 ErrIndexMissing 会替参数校验「报错」，
// clamp 变异因此假绿（第一版实测幸存，已修正）。
func TestSearchTool_InvalidParamsArePermanent(t *testing.T) {
	idx, corpus := builtLexicalTool(t)
	tl := SearchTool(idx, corpus)
	cases := []struct{ name, args, wantText string }{
		{"top_k above max", `{"query":"检索系统","top_k":99}`, "top_k must be in 1..10"},
		{"top_k zero", `{"query":"检索系统","top_k":0}`, "top_k must be in 1..10"},
		{"top_k negative", `{"query":"检索系统","top_k":-3}`, "top_k must be in 1..10"},
		{"token_budget negative", `{"query":"检索系统","token_budget":-1}`, "token_budget must be >= 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execSearchTool(tl, tc.args)
			if err == nil {
				t.Fatalf("Execute(%s) = %q, want error (clamping would silently rewrite the model's parameter)", tc.args, out)
			}
			assertNotRetryable(t, err)
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error text = %q, want it to name the rejected parameter (%q) for model self-correction", err, tc.wantText)
			}
		})
	}
}

// TestSearchTool_CheckFreshSentinelsArePermanent 固化两个 non-retryable 哨兵：
// 未建索引 / 语料改动都必须原样上抛。重试一个没建的索引或一个改坏的语料
// 是 agent  busy-loop——errors.As 必须找不到 RetryableError。
func TestSearchTool_CheckFreshSentinelsArePermanent(t *testing.T) {
	ctx := context.Background()

	t.Run("index missing", func(t *testing.T) {
		tl := SearchTool(newTestIndex(t), t.TempDir())
		_, err := execSearchTool(tl, `{"query":"检索系统"}`)
		if !errors.Is(err, ErrIndexMissing) {
			t.Fatalf("Execute on unbuilt index = %v, want ErrIndexMissing", err)
		}
		// 用户可操作文案（Task 8 交付 -index CLI 后补，CRV 黄卡 4）：模型/运维看到
		// 「请先运行 …」知道下一步，而不是一句无指引的 "index not built"。
		// 变异自证 ④：删掉 tool.go 的文案包装，本断言转红。
		if !strings.Contains(err.Error(), "请先运行") {
			t.Errorf("error text = %q, want actionable hint containing 请先运行", err)
		}
		assertNotRetryable(t, err)
	})

	t.Run("corpus stale", func(t *testing.T) {
		idx := newTestIndex(t)
		corpus := t.TempDir()
		writeDoc(t, corpus, "a.md", hybDocA)
		if _, err := idx.Rebuild(ctx, corpus); err != nil {
			t.Fatalf("Rebuild: %v", err)
		}
		writeDoc(t, corpus, "b.md", hybDocB) // 语料改动 → 目录指纹与已建版本不符
		tl := SearchTool(idx, corpus)
		_, err := execSearchTool(tl, `{"query":"检索系统"}`)
		if !errors.Is(err, ErrCorpusStale) {
			t.Fatalf("Execute after corpus change = %v, want ErrCorpusStale", err)
		}
		// 陈旧索引的指引是「重新运行重建」，与未建的「请先运行」区分：
		// 两者下一步动作不同，文案必须可判别。变异自证 ④ 同族。
		if !strings.Contains(err.Error(), "请重新运行") {
			t.Errorf("error text = %q, want actionable hint containing 请重新运行", err)
		}
		assertNotRetryable(t, err)
	})
}

// TestSearchTool_EmbedderFailureIsRetryable 是可重试通道的承重墙（变异自证 ①②）：
// hybrid 就绪但 embedder 调用失败 → ErrEmbeddingUnavailable 且 Retryable()==true；
// 原始病因必须留在 Unwrap 链里——可重试的是分类，日志归因靠的是原错误。
func TestSearchTool_EmbedderFailureIsRetryable(t *testing.T) {
	boom := errors.New("embed upstream boom")
	emb := &searchFakeEmbedder{byText: hybridVecs()}
	idx := openHybridIndex(t, emb)
	corpus := t.TempDir()
	writeHybridCorpus(t, corpus)
	// Rebuild 阶段嵌入正常（词法通道同时建成，「失败就顺手返回词法」的诱惑真实存在）。
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	emb.setErr(boom) // 运行中翻转：此后查询嵌入必失败

	tl := SearchTool(idx, corpus)
	out, err := execSearchTool(tl, `{"query":"检索系统"}`)
	if err == nil {
		t.Fatalf("Execute with failing embedder = %q, want ErrEmbeddingUnavailable", out)
	}
	if !errors.Is(err, ErrEmbeddingUnavailable) {
		t.Errorf("error = %v, want ErrEmbeddingUnavailable sentinel", err)
	}
	var re tool.RetryableError
	if !errors.As(err, &re) {
		t.Fatalf("error must implement tool.RetryableError, got %T: %v", err, err)
	}
	if !re.Retryable() {
		t.Error("Retryable() = false, want true (transient embedder outage deserves one retry)")
	}
	if !errors.Is(err, boom) {
		t.Errorf("wrapped error lost the original cause: %v", err)
	}
}

// TestSearchTool_LexicalModeNeverSurfacesErrNoEmbedder 验证纯词法模式下
// ErrNoEmbedder 不可能出现：Search 在 EmbedDim=0 时不调 embedTexts，
// 工具层因此也绝不应把「没配 embedder」误分类成 embedding 通道故障。
func TestSearchTool_LexicalModeNeverSurfacesErrNoEmbedder(t *testing.T) {
	idx := newTestIndex(t) // Options{}：无 Embedder、EmbedDim=0
	corpus := t.TempDir()
	writeDoc(t, corpus, "a.md", "检索系统设计与实现")
	writeDoc(t, corpus, "b.md", "检索系统")
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	tl := SearchTool(idx, corpus)
	out, err := execSearchTool(tl, `{"query":"检索系统"}`)
	if err != nil {
		t.Fatalf("lexical Execute = %v, want success (ErrNoEmbedder must not surface: Search never calls embedTexts when EmbedDim=0)", err)
	}
	if got := parseResult(t, out).Mode; got != ModeLexical {
		t.Errorf("Mode = %q, want %q", got, ModeLexical)
	}
}

// TestSearchTool_SuccessJSONContract 是正常路径的承重墙：JSON 形状全字段、
// 融合分口径（RRF 序位分，不是 bm25 负值）、降序、span 硬契约。
// 语料与 search.go 的 RRF 融合测试同款：期望序 b > a > x > y，最高分 0.5/62+0.5/62。
func TestSearchTool_SuccessJSONContract(t *testing.T) {
	idx := openHybridIndex(t, &searchFakeEmbedder{byText: hybridVecs()})
	corpus := t.TempDir()
	writeHybridCorpus(t, corpus)
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	tl := SearchTool(idx, corpus)
	out, err := execSearchTool(tl, `{"query":"检索系统"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	res := parseResult(t, out)
	if res.Mode != ModeHybrid {
		t.Errorf("Mode = %q, want %q", res.Mode, ModeHybrid)
	}
	if res.Truncated {
		t.Error("Truncated = true, want false (default budget 1500 is ample for four short docs)")
	}
	if len(res.Hits) == 0 {
		t.Fatal("hits must not be empty on the success path")
	}
	// RRF 融合序：b（两路都命中）> a > x > y。
	if got, want := docIDsFromHits(res.Hits), []string{"b.md", "a.md", "x.md", "y.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("hit order = %v, want %v", got, want)
	}
	for i := 1; i < len(res.Hits); i++ {
		if res.Hits[i-1].Score < res.Hits[i].Score {
			t.Errorf("scores not descending at %d: %v then %v", i, res.Hits[i-1].Score, res.Hits[i].Score)
		}
	}
	// 融合分口径：RRF 序位分必然落在 (0,1)；FTS5 bm25 原始值是负数，
	// 若出口漏出 bm25 负分（未过 fuseHits）本断言转红。
	top := res.Hits[0]
	if top.Score <= 0 || top.Score >= 1 {
		t.Errorf("top score = %v, want RRF fusion scale in (0,1)", top.Score)
	}
	if want := 0.5/62 + 0.5/62; math.Abs(top.Score-want) > 1e-9 {
		t.Errorf("top score = %v, want RRF fusion %v (0.5/(60+2) + 0.5/(60+2))", top.Score, want)
	}
	// span 硬契约（ADR-0017 决策 4）：两元素、含头不含尾、引用字段齐全。
	for i, h := range res.Hits {
		if h.Span[0] < 0 || h.Span[1] <= h.Span[0] {
			t.Errorf("hit %d span = %v, want [start,end) with end>start", i, h.Span)
		}
		if h.Text == "" {
			t.Errorf("hit %d text empty, want the chunk text verbatim", i)
		}
		if h.ChunkID == 0 || h.DocID == "" || h.Source == "" {
			t.Errorf("hit %d reference fields incomplete: %+v", i, h)
		}
	}
}

// TestSearchTool_MissingSpanIsImplementationError 是 span 防御的承重墙（变异自证 ⑤）：
// 绕过 Rebuild 直接插一行 span 全 0 的坏数据，工具层必须拒绝把它喂给模型——
// 无 span 的命中无法回链原文，等于把「可引用」契约变成空话。
func TestSearchTool_MissingSpanIsImplementationError(t *testing.T) {
	idx := newTestIndex(t)
	corpus := t.TempDir() // 空目录：Rebuild 写 corpus_meta 但零 chunk
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild empty corpus: %v", err)
	}

	// 手工插入 span 全 0 的坏行（Rebuild 产出的 chunk 必然 span_end>span_start，
	// 零值 span 只可能是实现错误）：doc_fts 同步插，词法查询才命得中它。
	const badText = "无引用区间违规数据甲"
	ins, err := idx.db.Exec(
		"INSERT INTO doc_chunk(doc_id, source, span_start, span_end, text, corpus_version) VALUES (?, ?, 0, 0, ?, ?)",
		"bad.md", "bad.md", badText, "manual")
	if err != nil {
		t.Fatalf("insert bad chunk: %v", err)
	}
	rowid, err := ins.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	if _, err := idx.db.Exec("INSERT INTO doc_fts(rowid, text) VALUES (?, ?)", rowid, badText); err != nil {
		t.Fatalf("insert bad fts row: %v", err)
	}

	tl := SearchTool(idx, corpus)
	out, err := execSearchTool(tl, `{"query":"违规数据"}`)
	if err == nil {
		t.Fatalf("Execute = %q, want implementation error for zero-span hit", out)
	}
	if !strings.Contains(err.Error(), "missing span") {
		t.Errorf("error = %v, want 'retrieval: hit missing span (implementation error)'", err)
	}
	assertNotRetryable(t, err)
}

// TestClassifySearchError_CanceledCtxNeverRetryable 钉死分类表里的 ctx 取消分支
// （实现时对计划的防御性增补）：hybrid 就绪时，若 Search 因 ctx 被抢占而失败，
// 绝不可包装成 ErrEmbeddingUnavailable——重试一个已取消的调用只会再失败一次，
// 还会把「用户取消/task timeout」的归因污染成「embedder 故障」（ADR-0014）。
// CheckFresh 先行拦下了大部分已取消 ctx，此分支守的是两者之间的竞态窗口。
func TestClassifySearchError_CanceledCtxNeverRetryable(t *testing.T) {
	idx := openHybridIndex(t, &searchFakeEmbedder{byText: hybridVecs()})
	for _, cancelled := range []error{context.Canceled, context.DeadlineExceeded} {
		err := classifySearchError(idx, cancelled)
		if !errors.Is(err, cancelled) {
			t.Errorf("classifySearchError(%v) = %v, want the ctx error verbatim", cancelled, err)
		}
		assertNotRetryable(t, err)
	}
}

// TestSearchTool_TopKAndBudgetShapeHits 验证 top_k 与 token_budget 真实生效：
// schema 向模型承诺了这两个参数，工具层就必须兑现——search.go 只负责融合输入充分，
// 最终 top_k 由本层决定（Task 6 注释的明确分工）。截断必须可观测（truncated=true）。
func TestSearchTool_TopKAndBudgetShapeHits(t *testing.T) {
	idx := openHybridIndex(t, &searchFakeEmbedder{byText: hybridVecs()})
	corpus := t.TempDir()
	writeHybridCorpus(t, corpus)
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	tl := SearchTool(idx, corpus)

	cases := []struct {
		name          string
		args          string
		wantHits      int
		wantTruncated bool
		wantHead      string
	}{
		{"default top_k keeps all four", `{"query":"检索系统"}`, 4, false, "b.md"},
		{"top_k cuts tail", `{"query":"检索系统","top_k":2}`, 2, true, "b.md"},
		{"top_k above hit count keeps all", `{"query":"检索系统","top_k":10}`, 4, false, "b.md"},
		{"zero budget keeps head only", `{"query":"检索系统","token_budget":0}`, 1, true, "b.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execSearchTool(tl, tc.args)
			if err != nil {
				t.Fatalf("Execute(%s): %v", tc.args, err)
			}
			res := parseResult(t, out)
			if len(res.Hits) != tc.wantHits {
				t.Errorf("hits = %d, want %d", len(res.Hits), tc.wantHits)
			}
			if res.Truncated != tc.wantTruncated {
				t.Errorf("Truncated = %v, want %v (truncation must stay observable)", res.Truncated, tc.wantTruncated)
			}
			if len(res.Hits) > 0 && res.Hits[0].DocID != tc.wantHead {
				t.Errorf("head hit = %q, want %q (only the low-score tail may be dropped)", res.Hits[0].DocID, tc.wantHead)
			}
		})
	}
}

// docIDsFromHits 复用 search_test.go 的 docIDs 思路：这里操作的是 wire 形状的 hitRow。
func docIDsFromHits(hits []hitRow) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.DocID
	}
	return out
}
