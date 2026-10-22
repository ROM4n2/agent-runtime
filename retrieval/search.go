package retrieval

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"
)

// 检索模式标注（ADR-0017 决策 3）：Mode 回传调用方本次走的是哪条通道。
// 纯词法是默认（无嵌入凭证时显式走它，不是降级）；
// hybrid 需 Embedder 与 EmbedDim>0 同时就绪。
const (
	ModeLexical = "lexical"
	ModeHybrid  = "hybrid"
)

// RRF（Reciprocal Rank Fusion）融合参数。k=60 为原论文推荐值，
// 两路权重各 0.5（等权）：词法与向量互为补充，不让任一侧独裁。
// 包级常量集中一处且注释标明可配——融合公式必须单一实现，
// 禁止在调用点散落魔法数（改参只改这里，测试随之固定）。
const (
	rrfK         = 60
	rrfWeightLex = 0.5
	rrfWeightVec = 0.5
)

// searchCandidateLimit 是 Search 内部两路召回各取的候选数上限。
// RRF 融合前两路都必须取足候选：任一路提前截断都会直接决定最终排序，
// 使另一路的贡献被静默削掉。最终 top_k 由工具层（Task 7）决定，这里只保证融合输入充分。
const searchCandidateLimit = 50

// Hit 是单条召回结果。Span 为原文 rune 偏移（含头不含尾），
// 配合 DocID 构成上层引用硬契约，供回链原文（ADR-0017 决策 4）。
type Hit struct {
	ChunkID int64
	DocID   string
	Source  string
	Span    [2]int
	// Score 的语义随入口而变，各有注释：
	//   - SearchLexical：FTS5 bm25 原始值（负值，越小越相关）；
	//   - SearchVector：vec0 距离（越小越近）；
	//   - Search 出口：RRF 融合分（越大越好，跨模式可比）。
	Score float64
	Text  string
}

// SearchResult 是一次检索的产物。Mode ∈ {"lexical","hybrid"}；
// Truncated 表示 token 预算截断确实丢弃了尾部命中——观测点，不静默。
type SearchResult struct {
	Hits      []Hit
	Mode      string
	Truncated bool
}

// Search 执行一次检索：词法默认，hybrid 可选（ADR-0017 决策 3）。流程：
//  1. 索引非空校验：doc_chunk 零行 → ErrIndexMissing。新鲜度检查不在这里——
//     由工具层在调用前走 CheckFresh（Search 不做实时全量内容哈希，成本过高）；
//  2. hybrid（Embedder 非 nil 且 EmbedDim>0）：先嵌入查询，再跑词法与向量两路；
//  3. 否则 lexical-only；
//  4. RRF 融合后按分数降序；
//  5. token 预算截断（只从尾部丢低分命中）。
//
// 两路顺序执行而非并发：词法查询与查询嵌入无共享中间态，顺序执行让
// 「向量路径失败」的归因唯一（禁静默降级要求错误明确、可分类），
// 并发省的是一次网络往返，不值得为此引入 errgroup 与锁竞争面。
func (idx *Index) Search(ctx context.Context, query string, tokenBudget int) (SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return SearchResult{}, err
	}
	empty, err := idx.indexEmpty(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	if empty {
		return SearchResult{}, ErrIndexMissing
	}

	// 查询嵌入先行：失败立即整体返回，绝不「词法已经跑完了就顺手返回词法结果」
	// （No-Silent-Degrade；该错误由 Task 7 转成可重试的 ErrEmbeddingUnavailable）。
	hybrid := idx.hybridEnabled()
	var queryVec []float32
	if hybrid {
		vecs, err := idx.embedTexts(ctx, []string{query})
		if err != nil {
			return SearchResult{}, err
		}
		queryVec = vecs[0]
	}

	lexical, err := idx.SearchLexical(ctx, query, searchCandidateLimit)
	if err != nil {
		return SearchResult{}, err
	}

	mode := ModeLexical
	var vector []Hit
	if hybrid {
		vector, err = idx.knnHits(ctx, queryVec, searchCandidateLimit)
		if err != nil {
			return SearchResult{}, err
		}
		mode = ModeHybrid
	}

	hits, truncated := truncateHits(fuseHits(lexical, vector), tokenBudget)
	return SearchResult{Hits: hits, Mode: mode, Truncated: truncated}, nil
}

// SearchLexical 走 FTS5 bm25 词法召回，返回按相关度降序的 limit 个命中。
// 查询原文直传 MATCH：不对查询做 bigram 预切（trigram 下预切文本不可检索，
// 实证见 index_test.go TestFTS_BigramSplitProducesEmptyIndex；2 字查询的
// 零命中是 trigram 分词器的硬限制，由工具层的 schema 提示引导用户给足上下文）。
//
// 用 JOIN 而非两步：外接内容表只能直接选 FTS 自己声明的列（text）与 rowid，
// doc_id/source/span 都在内容表 doc_chunk 里，JOIN 一次拿全且保序。
// 向量通道那边必须两步（vec0 要求 LIMIT 约束作用于 KNN 扫描本身），
// 两条通道的查询形态差异源于此，不是风格不统一。
func (idx *Index) SearchLexical(ctx context.Context, query string, limit int) ([]Hit, error) {
	rows, err := idx.db.QueryContext(ctx,
		`SELECT c.chunk_id, c.doc_id, c.source, c.span_start, c.span_end, c.text, bm25(doc_fts)
		 FROM doc_fts JOIN doc_chunk c ON c.chunk_id = doc_fts.rowid
		 WHERE doc_fts MATCH ? ORDER BY bm25(doc_fts) LIMIT ?`,
		query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hits []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.ChunkID, &h.DocID, &h.Source, &h.Span[0], &h.Span[1], &h.Text, &h.Score); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hits, nil
}

// SearchVector 走 vec0 KNN 向量召回：嵌入查询串后取距离最近的 limit 个 chunk。
// 向量通道关闭（Options.Embedder 为 nil 或 EmbedDim<=0）→ 显式错误，
// 复用 ErrNoEmbedder 哨兵（语义即「纯词法模式下误用向量通道」）；
// 禁静默降级——词法照样能跑不代表可以偷偷返回词法结果。
func (idx *Index) SearchVector(ctx context.Context, query string, limit int) ([]Hit, error) {
	if idx.opts.Embedder == nil {
		return nil, ErrNoEmbedder
	}
	if idx.opts.EmbedDim <= 0 {
		return nil, fmt.Errorf("retrieval: vector channel disabled, EmbedDim=%d: %w", idx.opts.EmbedDim, ErrNoEmbedder)
	}
	vecs, err := idx.embedTexts(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	return idx.knnHits(ctx, vecs[0], limit)
}

// knnRow 是 vec0 KNN 的一条原始结果：向量行 rowid（=chunk_id）与距离。
type knnRow struct {
	rowid int64
	dist  float64
}

// knnHits 执行 vec0 KNN 并把结果补成完整 Hit。
// 两步而非 JOIN：vec0 要求 LIMIT/k 约束直接作用于 KNN 扫描本身，
// JOIN 形态下 modernc 报 "A LIMIT or 'k = ?' constraint is required on vec0 knn queries"
// （v1.58.0 实测）。先取 (rowid, distance)，再按 rowid 回查 doc_chunk 拿引用字段。
func (idx *Index) knnHits(ctx context.Context, queryVec []float32, limit int) ([]Hit, error) {
	if len(queryVec) == 0 {
		return nil, fmt.Errorf("retrieval: empty query vector, want dim %d", idx.opts.EmbedDim)
	}
	order, err := idx.knnOrder(ctx, queryVec, limit)
	if err != nil {
		return nil, err
	}
	// 按 rowid 回查 doc_chunk，保持 KNN 顺序（距离升序）。
	hits := make([]Hit, 0, len(order))
	for _, k := range order {
		h, ok, err := idx.chunkHit(ctx, k.rowid)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // 孤儿向量行（doc_vec 有、doc_chunk 无）：跳过，不补假数据
		}
		h.Score = k.dist
		hits = append(hits, h)
	}
	return hits, nil
}

// knnOrder 跑 vec0 KNN，返回按距离升序的 (rowid, distance) 序列。
// 游标在本函数内关闭：回查 doc_chunk 前不能占着连接。
func (idx *Index) knnOrder(ctx context.Context, queryVec []float32, limit int) ([]knnRow, error) {
	rows, err := idx.db.QueryContext(ctx,
		"SELECT rowid, distance FROM doc_vec WHERE embedding MATCH ? ORDER BY distance LIMIT ?",
		encodeVec(queryVec), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var order []knnRow
	for rows.Next() {
		var k knnRow
		if err := rows.Scan(&k.rowid, &k.dist); err != nil {
			return nil, err
		}
		order = append(order, k)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return order, nil
}

// chunkHit 按 chunk_id 取一条引用记录；无对应行返回 ok=false（孤儿向量行）。
func (idx *Index) chunkHit(ctx context.Context, chunkID int64) (Hit, bool, error) {
	var h Hit
	err := idx.db.QueryRowContext(ctx,
		"SELECT chunk_id, doc_id, source, span_start, span_end, text FROM doc_chunk WHERE chunk_id = ?", chunkID).
		Scan(&h.ChunkID, &h.DocID, &h.Source, &h.Span[0], &h.Span[1], &h.Text)
	if errors.Is(err, sql.ErrNoRows) {
		return Hit{}, false, nil
	}
	if err != nil {
		return Hit{}, false, err
	}
	return h, true, nil
}

// indexEmpty 判断 doc_chunk 是否零行。Search 只验「索引非空」——
// 新鲜度（语料是否被改动）由工具层 CheckFresh 负责，两者解耦。
func (idx *Index) indexEmpty(ctx context.Context) (bool, error) {
	var n int
	if err := idx.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM doc_chunk").Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
}

// hybridEnabled 判定向量通道是否就绪：Embedder 非 nil 且 EmbedDim>0 同时满足才走 hybrid。
// 缺任一条件即纯词法（ADR-0017 决策 3），此时 doc_vec 表根本不存在，
// 门禁必须在发起向量查询之前——而不是靠 SQL 报错兜底。
func (idx *Index) hybridEnabled() bool {
	return idx.opts.Embedder != nil && idx.opts.EmbedDim > 0
}

// fuseHits 用 RRF 融合两路召回：score = wLex/(k+rankLex) + wVec/(k+rankVec)。
// 两路输入各自按通道内相关度排序（rank 从 1 起）；只命中一路的 chunk 按另一路缺失计（无加分），
// 两路都命中的 chunk 分数叠加——这正是融合优于任一单路的原因。
// 输出按融合分降序；同分按 chunk_id 升序兜底，保证输出确定（vec0 距离可能相等）。
func fuseHits(lexical, vector []Hit) []Hit {
	acc := make(map[int64]*Hit, len(lexical)+len(vector))
	for rank, h := range lexical {
		accumulate(acc, h, rrfWeightLex/(rrfK+float64(rank+1)))
	}
	for rank, h := range vector {
		accumulate(acc, h, rrfWeightVec/(rrfK+float64(rank+1)))
	}

	out := make([]Hit, 0, len(acc))
	for _, h := range acc {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ChunkID < out[j].ChunkID
	})
	return out
}

// accumulate 把一路的 rank 贡献累加到对应 chunk 上。首次出现的 chunk 以该路的 Hit
// 为基准拷贝（引用字段两路一致，Score 随后被融合分整体覆盖）。
func accumulate(acc map[int64]*Hit, h Hit, contrib float64) {
	if cur, ok := acc[h.ChunkID]; ok {
		cur.Score += contrib
		return
	}
	cp := h
	cp.Score = contrib
	acc[h.ChunkID] = &cp
}

// truncateHits 按 token 预算从尾部丢弃低分命中：从头累加 estimateTokens，
// 超预算即止，剩余全部丢弃并置 Truncated=true（截断必须可观测，不静默）。
// 头部保底：无论如何保留最高分一条——否则 budget=0 会返回空结果集，
// 调用方无法区分「无命中」与「预算为零」，工具层也拿不到任何可引用 span。
func truncateHits(hits []Hit, tokenBudget int) ([]Hit, bool) {
	if len(hits) == 0 {
		return hits, false
	}
	used, kept := 0, 0
	for i, h := range hits {
		cost := estimateTokens(h.Text)
		if i > 0 && used+cost > tokenBudget {
			break
		}
		used += cost
		kept++
	}
	if kept == len(hits) {
		return hits, false
	}
	return hits[:kept], true
}

// estimateTokens 用 rune 数/2 近似 token 数：中文场景约 1.5-2 字符/token，
// 取保守的一半——宁可少估不可超估，超估会让预算提前耗尽、丢掉有用命中。
// 按 rune 而非字节计数（中文 3 字节/字符，按字节会高估 3 倍）。
// 粗估够用：预算截断是护栏不是精确计费，上精确 tokenize 需要引入分词器，不值当。
func estimateTokens(text string) int {
	return utf8.RuneCountInString(text) / 2
}
