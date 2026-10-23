package retrieval

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// searchFakeEmbedder 按文本查表返回可控向量：让词法命中与向量命中精确落到指定 chunk 上，
// RRF 融合排序因此可被确定性判定（不依赖真实嵌入的偶然几何关系）。
// 另支持运行中翻转错误——Rebuild 阶段正常、Search 阶段失败，
// 用来验证「hybrid 嵌入失败不静默降级回词法」。
type searchFakeEmbedder struct {
	mu     sync.Mutex
	byText map[string][]float32
	err    error
}

func (f *searchFakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, ok := f.byText[t]
		if !ok {
			return nil, fmt.Errorf("searchFakeEmbedder: unregistered text %q", t)
		}
		out[i] = v
	}
	return out, nil
}

func (f *searchFakeEmbedder) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// hybridQuery 是混合召回测试用的查询词（4 字中文，trigram 下可命中）。
const hybridQuery = "检索系统"

// hybrid 语料四篇：查询词的词法频次与向量的几何距离被刻意做成「错位」，
// 使两路召回的 rank 序列互为交叉——只有正确的 RRF 求和才能得到预期融合序。
//
//	a.md：查询词 3 次 → 词法 rank1；向量最远 → 向量 rank4
//	b.md：查询词 1 次 → 词法 rank2；向量 rank2
//	x.md：词法不命中；    向量最近 → 向量 rank1
//	y.md：词法不命中；    向量 rank3
const (
	hybDocA = "检索系统检索系统检索系统甲乙丙丁戊己庚辛壬" // 21 rune
	hybDocB = "检索系统丙丁戊己庚辛壬癸甲乙丁戊己庚"    // 18 rune
)

// hybDocX/hybDocY 是 21 rune 的纯填充文档（词法不命中查询词）；
// 用 Repeat 而非字面量是为了让 rune 数精确可算（token 预算断言依赖它）。
var (
	hybDocX = strings.Repeat("甲", 21)
	hybDocY = strings.Repeat("乙", 21)
)

// hybridVecs 是查询与四篇文档的向量（dim=2，欧氏距离）：
// 查询=[10,0]，x=[10,0](d=0) < b=[10,1](d=1) < y=[10,2](d=2) < a=[10,3](d=3)。
// 距离互异，无 tie，KNN 顺序确定。
func hybridVecs() map[string][]float32 {
	return map[string][]float32{
		hybridQuery: {10, 0},
		hybDocA:     {10, 3},
		hybDocB:     {10, 1},
		hybDocX:     {10, 0},
		hybDocY:     {10, 2},
	}
}

func writeHybridCorpus(t *testing.T, dir string) {
	t.Helper()
	writeDoc(t, dir, "a.md", hybDocA)
	writeDoc(t, dir, "b.md", hybDocB)
	writeDoc(t, dir, "x.md", hybDocX)
	writeDoc(t, dir, "y.md", hybDocY)
}

// openHybridIndex 开一个 hybrid 索引（dim=2 + 可控 Embedder）。
func openHybridIndex(t *testing.T, emb *searchFakeEmbedder) *Index {
	t.Helper()
	idx, err := Open(filepath.Join(t.TempDir(), "hybrid.db"), Options{Embedder: emb, EmbedModel: "m", EmbedDim: 2})
	if err != nil {
		t.Fatalf("Open hybrid index: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func docIDs(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.DocID
	}
	return out
}

// TestSearch_LexicalOnly 验证无 Embedder 时走纯词法：Mode=lexical、bm25 有序、
// span/text 引用字段正确，且预算充足时不截断。
// 语料把查询词频次拉得足够开（4 次 vs 1 次）：bm25 由 tf 主导，
// 频次差距小的时候分差极小，排序断言会变成走钢丝。
func TestSearch_LexicalOnly(t *testing.T) {
	idx := newTestIndex(t)
	corpus := t.TempDir()
	writeDoc(t, corpus, "a.md", "检索系统检索系统检索系统检索系统")
	writeDoc(t, corpus, "b.md", "检索系统")
	writeDoc(t, corpus, "c.md", "向量数据库性能测试报告")
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	res, err := idx.Search(context.Background(), hybridQuery, 1000)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Mode != "lexical" {
		t.Errorf("Mode = %q, want lexical (no embedder configured)", res.Mode)
	}
	if res.Truncated {
		t.Error("Truncated = true, want false (budget 1000 is ample)")
	}
	// bm25 有序：查询词出现 4 次的 a.md 必须排在出现 1 次的 b.md 之前；c.md 不命中。
	if got, want := docIDs(res.Hits), []string{"a.md", "b.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hit order = %v, want %v (bm25 relevance order)", got, want)
	}
	// span 与 text 引用契约（ADR-0017 决策 4）：单 chunk 文档 span = [0, rune 数]。
	if got := res.Hits[0].Span; got != [2]int{0, 16} {
		t.Errorf("span = %v, want [0 16]", got)
	}
	if res.Hits[0].Text != "检索系统检索系统检索系统检索系统" {
		t.Errorf("text = %q, want the chunk text verbatim", res.Hits[0].Text)
	}
	if res.Hits[0].DocID != "a.md" || res.Hits[0].Source != "a.md" {
		t.Errorf("doc_id/source = %q/%q, want a.md", res.Hits[0].DocID, res.Hits[0].Source)
	}
	if res.Hits[0].ChunkID == 0 {
		t.Error("ChunkID must be populated for span back-reference")
	}
}

// TestSearch_QueryNotBigramSplit 固化「查询原文直传 FTS5 MATCH」：
// 若对查询做 bigram 预切（ADR-0017 决策 3 的失效提案），"检索系统" 会变成
// "检索 索系 系统"，trigram 下零命中（同 TestFTS_BigramSplitProducesEmptyIndex）。
func TestSearch_QueryNotBigramSplit(t *testing.T) {
	idx := newTestIndex(t)
	corpus := t.TempDir()
	writeDoc(t, corpus, "a.md", "检索系统设计与实现")
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	res, err := idx.Search(context.Background(), hybridQuery, 1000)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("query must reach FTS5 verbatim; bigram-splitting the query yields zero hits under trigram")
	}
}

// TestSearchLexical_BM25Order 直接验词法通道：FTS5 bm25 负值、越小越相关，
// 故按 Score 升序即相关度降序（a.md 查询词 3 次 > b.md 1 次）。
func TestSearchLexical_BM25Order(t *testing.T) {
	idx := newTestIndex(t)
	corpus := t.TempDir()
	writeDoc(t, corpus, "a.md", hybDocA)
	writeDoc(t, corpus, "b.md", hybDocB)
	writeDoc(t, corpus, "x.md", hybDocX)
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	hits, err := idx.SearchLexical(context.Background(), hybridQuery, 10)
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	if got, want := docIDs(hits), []string{"a.md", "b.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lexical order = %v, want %v", got, want)
	}
	if hits[0].Score >= hits[1].Score {
		t.Errorf("bm25 scores not ascending: %v then %v (FTS5 bm25 is negative, smaller = more relevant)",
			hits[0].Score, hits[1].Score)
	}
}

// TestSearchLexical_QuerySanitizedNoFTSSyntaxError 是 harness 缺陷修复的承重墙（RED→GREEN）：
// 查询串包成 FTS5 phrase 后直进 MATCH——ADR 编号（"ADR-0004 …"）、问号、括号、
// 前导星号、裸布尔词（NOT / 行尾 AND）都不再触发 FTS5 语法错误。
// 若去掉 phrase 包装（查询原文直传 MATCH）→ ADR 编号 query 报
// "SQL logic error: no such column: 0004"，工具层转 KindError，grader 判 recall=0（伪造 wrong-span）。
// 含双引号的 query 也不能破坏 phrase：内部 " 双写成 "" 后整体包引号，原文仍可命中。
func TestSearchLexical_QuerySanitizedNoFTSSyntaxError(t *testing.T) {
	idx := newTestIndex(t)
	corpus := t.TempDir()
	writeDoc(t, corpus, "a.md", "ADR-0004 的持久化方案是什么？检索系统设计与实现")
	writeDoc(t, corpus, "q.md", `含"双引号"的查询检索系统设计与实现`)
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	cases := []struct {
		name    string
		query   string
		wantHit bool // 语料含该串原文：净化后仍应命中（证明不是靠「查无此项」规避错误）
	}{
		{"ADR number with question", "ADR-0004 的持久化方案是什么？", true},
		{"ADR number with parens", "ADR-0004 (持久化) 方案？", false},
		{"leading asterisk", "*星号检索", false},
		{"bare NOT keyword", "NOT", false},
		{"trailing AND keyword", "检索系统 AND", false},
		{"embedded double quotes", `含"双引号"的查询`, true},
		{"plain CJK query", "检索系统", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits, err := idx.SearchLexical(context.Background(), tc.query, 10)
			if err != nil {
				t.Fatalf("SearchLexical(%q) = error %v, want no FTS5 syntax error (query must be phrase-wrapped)", tc.query, err)
			}
			if tc.wantHit && len(hits) == 0 {
				t.Errorf("SearchLexical(%q) = 0 hits, want the verbatim corpus text to stay matchable after sanitization", tc.query)
			}
		})
	}
}

// TestSearch_HybridRRFFusion 是 RRF 融合的承重墙（变异自证 ①）：
// 词法 rank 序列 [a,b] 与向量 rank 序列 [x,b,y,a] 交叉错位。
// 正确参数（k=60, wLex=wVec=0.5）下融合序必须是 b > a > x > y：
//
//	b = 0.5/62 + 0.5/62 = 0.016129 （词法2 + 向量2，两路都命中 → 叠加后最高）
//	a = 0.5/61 + 0.5/64 = 0.016009 （词法1 + 向量4）
//	x = 0.5/61           = 0.008197 （仅向量1）
//	y = 0.5/63           = 0.007937 （仅向量3）
//
// 任一变体都会翻转该排序：
//   - 权重改成 1.0/0.0（只剩词法）→ a 居首（b 掉到第三）
//   - 权重改成 0.0/1.0（只剩向量）→ x 居首
//   - k 从 60 改成 0 → a 居首（0.5+0.125 > 0.25+0.25）
//   - 融合从求和改成取 max → a/x 并列居首，b 掉到第三
func TestSearch_HybridRRFFusion(t *testing.T) {
	idx := openHybridIndex(t, &searchFakeEmbedder{byText: hybridVecs()})
	corpus := t.TempDir()
	writeHybridCorpus(t, corpus)
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	res, err := idx.Search(context.Background(), hybridQuery, 1000)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Mode != "hybrid" {
		t.Fatalf("Mode = %q, want hybrid (embedder + EmbedDim>0)", res.Mode)
	}
	if res.Truncated {
		t.Error("Truncated = true, want false (budget 1000 is ample)")
	}
	if got, want := docIDs(res.Hits), []string{"b.md", "a.md", "x.md", "y.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RRF fusion order = %v, want %v", got, want)
	}
	// 两路都命中的 chunk 必须排在只命中一路的 chunk 之前（融合叠加的直接后果）。
	if res.Hits[0].DocID != "b.md" {
		t.Errorf("top hit = %q, want b.md (hit by both channels ranks highest)", res.Hits[0].DocID)
	}
	for i := 1; i < len(res.Hits); i++ {
		if res.Hits[i-1].Score < res.Hits[i].Score {
			t.Errorf("scores not descending at %d: %v then %v", i, res.Hits[i-1].Score, res.Hits[i].Score)
		}
	}
}

// TestSearch_TokenBudgetTruncatesTail 是预算截断的承重墙（变异自证 ②）：
// 截断只从尾部低分 Hit 丢，Truncated=true 必达；头部（最高分）任何预算下都保留。
// 语料四篇的 token 估算（rune/2）：b=9, a=10, x=10, y=10，合计 39。
// 若把截断改成从头部丢 → 每个子用例的首个断言转红。
func TestSearch_TokenBudgetTruncatesTail(t *testing.T) {
	idx := openHybridIndex(t, &searchFakeEmbedder{byText: hybridVecs()})
	corpus := t.TempDir()
	writeHybridCorpus(t, corpus)
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	ctx := context.Background()

	cases := []struct {
		name      string
		budget    int
		wantDocs  []string
		truncated bool
	}{
		{"zero budget keeps head only", 0, []string{"b.md"}, true},
		{"budget below second hit", 9, []string{"b.md"}, true},
		{"budget fits exactly two", 19, []string{"b.md", "a.md"}, true},
		{"budget fits exactly three", 38, []string{"b.md", "a.md", "x.md"}, true},
		{"budget fits all exactly", 39, []string{"b.md", "a.md", "x.md", "y.md"}, false},
		{"budget ample", 1000, []string{"b.md", "a.md", "x.md", "y.md"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := idx.Search(ctx, hybridQuery, tc.budget)
			if err != nil {
				t.Fatalf("Search(budget=%d): %v", tc.budget, err)
			}
			if res.Truncated != tc.truncated {
				t.Errorf("Truncated = %v, want %v", res.Truncated, tc.truncated)
			}
			if got := docIDs(res.Hits); !reflect.DeepEqual(got, tc.wantDocs) {
				t.Errorf("kept hits = %v, want %v (tail-first truncation, head preserved)", got, tc.wantDocs)
			}
			if len(res.Hits) == 0 {
				t.Fatal("head hit must survive any budget")
			}
			if res.Hits[0].DocID != "b.md" {
				t.Errorf("head hit = %q, want b.md (highest score)", res.Hits[0].DocID)
			}
		})
	}
}

// TestSearch_EmptyIndexReturnsErrIndexMissing 是哨兵的承重墙（变异自证 ⑤）：
// doc_chunk 零行 → ErrIndexMissing。删掉该检查的行为变化是
// 「FTS 查询对空库返回空结果而非错误」，调用方将无法区分「没建索引」与「无命中」。
// 第二个子用例覆盖「空语料已建 corpus_meta 但零 chunk」——哨兵看的是行数而非版本记录。
func TestSearch_EmptyIndexReturnsErrIndexMissing(t *testing.T) {
	ctx := context.Background()

	t.Run("never built", func(t *testing.T) {
		idx := newTestIndex(t)
		if _, err := idx.Search(ctx, hybridQuery, 100); !errors.Is(err, ErrIndexMissing) {
			t.Fatalf("Search on unbuilt index = %v, want ErrIndexMissing", err)
		}
	})

	t.Run("built but zero chunks", func(t *testing.T) {
		idx := newTestIndex(t)
		corpus := t.TempDir() // 空目录：Rebuild 成功且写 corpus_meta，但零 chunk
		if _, err := idx.Rebuild(ctx, corpus); err != nil {
			t.Fatalf("Rebuild empty corpus: %v", err)
		}
		if _, err := idx.Search(ctx, hybridQuery, 100); !errors.Is(err, ErrIndexMissing) {
			t.Fatalf("Search on zero-chunk index = %v, want ErrIndexMissing", err)
		}
	})
}

// TestSearch_HybridEmbedderErrorNoDegrade 是禁静默降级的承重墙（变异自证 ③）：
// hybrid 模式下查询嵌入失败必须整体返回该错误，且结果为零值
// （Mode 不出现、Hits 为 nil）。若改成「失败就顺手返回词法结果」→ 本用例转红。
func TestSearch_HybridEmbedderErrorNoDegrade(t *testing.T) {
	boom := errors.New("embed upstream boom")
	emb := &searchFakeEmbedder{byText: hybridVecs()}
	idx := openHybridIndex(t, emb)
	corpus := t.TempDir()
	writeHybridCorpus(t, corpus)
	// Rebuild 阶段嵌入正常（词法通道同时建成，降级诱惑真实存在）。
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	emb.setErr(boom) // 运行中翻转：此后查询嵌入必失败

	res, err := idx.Search(context.Background(), hybridQuery, 1000)
	if !errors.Is(err, boom) {
		t.Fatalf("Search must propagate embedder error verbatim: got %v", err)
	}
	if res.Mode != "" {
		t.Errorf("Mode = %q, want empty (no silent degrade to lexical)", res.Mode)
	}
	if res.Hits != nil {
		t.Errorf("Hits = %v, want nil (lexical results must not leak on embedder failure)", res.Hits)
	}
	if res.Truncated {
		t.Error("Truncated = true, want false on error path")
	}
}

// TestSearchVector_RequiresVectorChannel 验证向量通道关闭时显式失败：
// 无 Embedder 或 EmbedDim<=0 → 错误（复用 ErrNoEmbedder 哨兵），
// 绝不因为「词法照样能跑」就静默返回词法结果。
func TestSearchVector_RequiresVectorChannel(t *testing.T) {
	ctx := context.Background()

	t.Run("nil embedder", func(t *testing.T) {
		idx := newTestIndex(t)
		if _, err := idx.SearchVector(ctx, hybridQuery, 5); !errors.Is(err, ErrNoEmbedder) {
			t.Fatalf("SearchVector without embedder = %v, want ErrNoEmbedder", err)
		}
	})

	t.Run("zero embed dim", func(t *testing.T) {
		emb := &searchFakeEmbedder{byText: hybridVecs()}
		idx, err := Open(filepath.Join(t.TempDir(), "dim0.db"), Options{Embedder: emb, EmbedModel: "m", EmbedDim: 0})
		if err != nil {
			t.Fatalf("Open dim0 index: %v", err)
		}
		t.Cleanup(func() { _ = idx.Close() })
		if _, err := idx.SearchVector(ctx, hybridQuery, 5); !errors.Is(err, ErrNoEmbedder) {
			t.Fatalf("SearchVector with EmbedDim=0 = %v, want ErrNoEmbedder", err)
		}
	})
}

// TestSearchVector_KNNOrder 直接验向量通道：vec0 KNN 按距离升序，
// 可控向量下必须是 x(0) < b(1) < y(2) < a(3)。
func TestSearchVector_KNNOrder(t *testing.T) {
	idx := openHybridIndex(t, &searchFakeEmbedder{byText: hybridVecs()})
	corpus := t.TempDir()
	writeHybridCorpus(t, corpus)
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	hits, err := idx.SearchVector(context.Background(), hybridQuery, 10)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if got, want := docIDs(hits), []string{"x.md", "b.md", "y.md", "a.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("KNN order = %v, want %v", got, want)
	}
	// Hit.Score 为 vec0 距离（越小越近）。
	for i := 1; i < len(hits); i++ {
		if hits[i-1].Score > hits[i].Score {
			t.Errorf("distances not ascending at %d: %v then %v", i, hits[i-1].Score, hits[i].Score)
		}
	}
}

// TestRebuild_HybridWritesDocVec 是 hybrid 写入路径的承重墙（变异自证 ④）：
// EmbedDim>0 + Embedder 时，Rebuild 必须把每个 chunk 的向量写进 doc_vec，
// 且 rowid 与 chunk_id 对齐（clearIndex 的孤儿行清理依赖该不变量）。
// 删掉写入路径 → doc_vec 行数断言转红（且 Search 的 hybrid 模式将无向量可召回）。
func TestRebuild_HybridWritesDocVec(t *testing.T) {
	idx := openHybridIndex(t, &searchFakeEmbedder{byText: hybridVecs()})
	corpus := t.TempDir()
	writeHybridCorpus(t, corpus)

	r, err := idx.Rebuild(context.Background(), corpus)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if r.ChunksWritten != 4 {
		t.Fatalf("ChunksWritten = %d, want 4", r.ChunksWritten)
	}
	var n int
	if err := idx.db.QueryRow("SELECT COUNT(*) FROM doc_vec").Scan(&n); err != nil {
		t.Fatalf("count doc_vec: %v", err)
	}
	if n != r.ChunksWritten {
		t.Errorf("doc_vec rows = %d, want %d (one vector per chunk)", n, r.ChunksWritten)
	}
	// rowid 对齐：doc_vec 每一行都必须能 join 到 doc_chunk。
	var aligned int
	if err := idx.db.QueryRow(
		"SELECT COUNT(*) FROM doc_vec v JOIN doc_chunk c ON c.chunk_id = v.rowid").Scan(&aligned); err != nil {
		t.Fatalf("count aligned: %v", err)
	}
	if aligned != r.ChunksWritten {
		t.Errorf("aligned doc_vec rows = %d, want %d (rowid must equal chunk_id)", aligned, r.ChunksWritten)
	}

	// 反向把守：纯词法模式（Options{}）不得建也不写 doc_vec。
	lexIdx := newTestIndex(t)
	lexCorpus := t.TempDir()
	writeDoc(t, lexCorpus, "a.md", hybDocA)
	if _, err := lexIdx.Rebuild(context.Background(), lexCorpus); err != nil {
		t.Fatalf("lexical Rebuild: %v", err)
	}
	var exists int
	if err := lexIdx.db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE name = 'doc_vec'").Scan(&exists); err != nil {
		t.Fatalf("check doc_vec existence: %v", err)
	}
	if exists != 0 {
		t.Error("lexical-only index must not create doc_vec")
	}
}

// TestSearchLexical_NaturalLanguageQuestion 是 Task 12 缺陷 E 的承重墙（变异自证 ①）：
// 任务集的 query 是自然语言长问句（"崩溃恢复…是怎么实现的？"），问句尾巴在原文里
// 根本不存在——整句包成一个 FTS5 phrase 时，trigram 只认「逐字连续」，必然零命中。
// 把 ftsQuery 改回整句 phrase（`return ftsPhrase(raw)`）→ 本用例前两个子用例转红。
func TestSearchLexical_NaturalLanguageQuestion(t *testing.T) {
	idx := newTestIndex(t)
	corpus := t.TempDir()
	writeDoc(t, corpus, "recovery.md", "崩溃恢复标记僵尸任务：超时 running 任务重新置为 pending 后按重试策略处理。")
	writeDoc(t, corpus, "adr0004.md", "ADR-0004 记录了候选持久化方案：不引入 DB 与 MQ，坚持内存队列。")
	writeDoc(t, corpus, "other.md", "向量检索的性能测试报告与基线数据。")
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	ctx := context.Background()

	cases := []struct {
		name    string
		query   string
		wantDoc string
	}{
		{"long CJK question hits the verbatim clause", "崩溃恢复标记僵尸任务是怎么实现的？", "recovery.md"},
		{"ADR number long question", "ADR-0004 的持久化方案是什么？", "adr0004.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits, err := idx.SearchLexical(ctx, tc.query, 10)
			if err != nil {
				t.Fatalf("SearchLexical(%q): %v", tc.query, err)
			}
			if !containsDoc(hits, tc.wantDoc) {
				t.Errorf("hits = %v, want %q（长问句必须按词元/窗口召回，整句 phrase 必然零命中）", docIDs(hits), tc.wantDoc)
			}
		})
	}

	t.Run("single short word keeps the verbatim phrase", func(t *testing.T) {
		if got, want := ftsQuery("崩溃恢复"), ftsPhrase("崩溃恢复"); got != want {
			t.Errorf("ftsQuery(单短词) = %q, want %q（与 Task 12 之前逐字节一致）", got, want)
		}
		hits, err := idx.SearchLexical(ctx, "崩溃恢复", 10)
		if err != nil {
			t.Fatalf("SearchLexical(单短词): %v", err)
		}
		if got, want := docIDs(hits), []string{"recovery.md"}; !reflect.DeepEqual(got, want) {
			t.Errorf("hits = %v, want %v（短词仍是精确 phrase 匹配，不引入窗口噪声）", got, want)
		}
	})

	t.Run("pure punctuation returns empty without error", func(t *testing.T) {
		hits, err := idx.SearchLexical(ctx, "？？！……（）", 10)
		if err != nil {
			t.Fatalf("标点查询不得触发 FTS5 语法错误: %v", err)
		}
		if len(hits) != 0 {
			t.Errorf("hits = %v, want empty", docIDs(hits))
		}
	})
}

// containsDoc 判断命中集合里是否出现过该 doc_id。
func containsDoc(hits []Hit, docID string) bool {
	for _, h := range hits {
		if h.DocID == docID {
			return true
		}
	}
	return false
}

// TestEstimateTokens_Boundaries 钉死 rune 数/2 的近似口径：
// 按 rune 而非字节计数（中文 3 字节/字符，按字节会高估 3 倍），奇数向下取整。
func TestEstimateTokens_Boundaries(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"", 0},
		{"a", 0},                      // 1 rune / 2 = 0
		{"ab", 1},                     // 2 / 2 = 1
		{"abc", 1},                    // 3 / 2 = 1（向下取整）
		{"abcd", 2},                   // 4 / 2 = 2
		{"检索系统", 2},                   // 4 rune（12 字节）→ 2，不是 6
		{"检索系统设计", 3},                 // 6 rune → 3
		{strings.Repeat("甲", 21), 10}, // 21 rune → 10
	}
	for _, tc := range cases {
		if got := estimateTokens(tc.text); got != tc.want {
			t.Errorf("estimateTokens(%q) = %d, want %d", tc.text, got, tc.want)
		}
	}
}
