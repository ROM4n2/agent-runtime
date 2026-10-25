package retrieval

import (
	"context"
	"sort"
	"strings"
	"unicode"
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

// ftsPhrase 把用户查询串净化为一个 FTS5 phrase：内部双引号双写（" → ""）后整体包 "..."。
// 这样查询串永远是「一段连续的原文」，其中的 ADR 编号、问号、括号、前导 *、
// AND/OR/NOT 等字符全部失去 FTS5 语法含义，不再触发语法错误；
// 用户真正想搜的原文子串仍按 trigram 命中（净化只包一层引号，不改写内容）。
// 布尔组合只由 ftsQuery 在词元层面生成（AND/OR 落在引号之外）：
// 模型传进来的任何字符都只当字面量，查询语言不对外暴露。
func ftsPhrase(query string) string {
	return `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
}

const (
	// ftsTermMinRunes 是 trigram 分词器的硬下限：trigram 索引只存 3 连字符，
	// 长度 <3 rune 的词元在 MATCH 里永远零命中（白跑一趟，还拉低召回信心）。
	ftsTermMinRunes = 3
	// ftsGramRunes 是召回窗口宽度：取 trigram 的分词单位（3 rune）。
	// 窗口再宽就要求窗口内逐字连续，长问句的窗口在原文里凑不齐。
	ftsGramRunes = 3
	// ftsVerbatimMaxRunes 是「整串大概率在原文逐字出现」的词元长度上限。
	// 不超过它的查询/词元保持精确 phrase 行为（Task 12 之前的行为）；
	// 超过它的基本是自然语言问句片段（"崩溃恢复标记僵尸任务是怎么实现的"）。
	ftsVerbatimMaxRunes = 6
	// ftsMaxTerms 是 MATCH 表达式里 OR 词项上限：3 连窗口数量随问句长度线性增长，
	// 不设上限会把表达式撑到几百项。词项按「整词元 phrase 优先、原序推进」生成，
	// 被裁掉的总是尾部词元（通常是提示语尾巴），不伤主问句。
	ftsMaxTerms = 64
)

// ftsQuery 把自然语言查询串编译成 FTS5 MATCH 表达式（Task 12 缺陷 E 的修复点）。
// trigram 分词器只做子串语义，长问句必须拆词元，整句 phrase 必然零命中
// （Task 12 缺陷 E 实证：整句包 phrase 后任务集 query 全零命中，评测 71/90 假红）：
// 任务集的 query 是「崩溃恢复新增的补偿接口签名是什么？……」这类问句，
// 问句尾巴（"是怎么实现的"、"回答请引用…"）在原文里根本不存在，
// 整句连续匹配不可能成立。故分两级：
//  1. 词元全部「短」（≤ ftsVerbatimMaxRunes rune）→ 各包 phrase 用 AND 连接，
//     与旧行为同构（"崩溃恢复"这类单词查询的表达式逐字节不变）；
//  2. 出现长词元 → 整词元 phrase + 其 3 连汉字窗口，全部 OR 连接：
//     任一窗口命中即召回，bm25 让命中窗口多的文档排前。
//     AND 在这一级不可用——问句尾巴在原文不存在，AND 必然把结果清零。
//
// 词元按空白与中英标点切分，长度 <3 rune 的直接丢弃（trigram 硬下限）；
// 切完一个词元都不剩（纯短词/纯标点查询）→ 退回整句 phrase：保持旧行为且不报错。
func ftsQuery(raw string) string {
	segs := ftsSegments(raw)
	if len(segs) == 0 {
		return ftsPhrase(raw)
	}
	if ftsAllVerbatim(segs) {
		parts := make([]string, len(segs))
		for i, s := range segs {
			parts[i] = ftsPhrase(s)
		}
		return strings.Join(parts, " AND ")
	}
	return ftsRecallQuery(segs)
}

// ftsSegments 按空白与中英标点切分查询，只留长度 ≥ ftsTermMinRunes rune 的词元。
func ftsSegments(raw string) []string {
	fields := strings.FieldsFunc(raw, isFTSSeparator)
	segs := make([]string, 0, len(fields))
	for _, f := range fields {
		if utf8.RuneCountInString(f) >= ftsTermMinRunes {
			segs = append(segs, f)
		}
	}
	return segs
}

// isFTSSeparator 判定查询切分点：空白 + 中英标点。
// 不切 '/' '-' '_' 之类连接符——路径（docs/adr/0015-…md）与 ADR 编号属于
// 「子串边界」，切开反而破坏它们在原文里的逐字可命中性。
func isFTSSeparator(r rune) bool {
	if unicode.IsSpace(r) {
		return true
	}
	switch r {
	case '？', '?', '，', ',', '。', '.', '！', '!', '；', ';', '：', ':',
		'、', '（', '）', '(', ')', '[', ']', '【', '】',
		'"', '\'', '“', '”', '‘', '’':
		return true
	}
	return false
}

// ftsAllVerbatim 判定全部词元是否都短到「整串逐字命中」可信。
func ftsAllVerbatim(segs []string) bool {
	for _, s := range segs {
		if utf8.RuneCountInString(s) > ftsVerbatimMaxRunes {
			return false
		}
	}
	return true
}

// ftsRecallQuery 生成宽松召回表达式：每个词元的整串 phrase + 其 3 连汉字窗口，
// 全部 OR 连接（去重，上限 ftsMaxTerms 项）。
func ftsRecallQuery(segs []string) string {
	terms := make([]string, 0, ftsMaxTerms)
	seen := make(map[string]struct{}, ftsMaxTerms)
	add := func(term string) bool {
		if _, dup := seen[term]; dup {
			return true
		}
		if len(terms) >= ftsMaxTerms {
			return false
		}
		seen[term] = struct{}{}
		terms = append(terms, term)
		return true
	}
	for _, seg := range segs {
		if !add(ftsPhrase(seg)) {
			break
		}
		if !hasHan(seg) {
			// 拉丁标识符（docs/adr/…、modernc.org/sqlite）整串就是最强信号，
			// 切 3 连窗口只会引入 "doc"/"org" 这类噪声词项。
			continue
		}
		runes := []rune(seg)
		full := false
		for i := 0; i+ftsGramRunes <= len(runes); i++ {
			if !add(ftsPhrase(string(runes[i : i+ftsGramRunes]))) {
				full = true
				break
			}
		}
		if full {
			break
		}
	}
	return strings.Join(terms, " OR ")
}

// hasHan 判断串内是否含汉字：只有汉字词元才做 3 连窗口展开。
func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

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
// 查询串先经 ftsQuery 编译成 MATCH 表达式再进查询：不对原文做 bigram 预切
// （trigram 下预切文本不可检索，实证见 index_test.go TestFTS_BigramSplitProducesEmptyIndex；
// 2 字查询的零命中是 trigram 分词器的硬限制，由工具层的 schema 提示引导用户给足上下文），
// 但也不把原文直传 MATCH——ADR 编号（"ADR-0004 的持久化方案是什么？"）会被 FTS5
// 当作列名解析，报 "no such column: 0004"；前导 * / 裸 NOT / 行尾 AND 同样是语法错误，
// 错误经工具层转 KindError 后 grader 记 recall=0（伪造的 wrong-span，CRV 黄卡 3 外溢）。
// 长问句的零命中则是另一件事（Task 12 缺陷 E）：整句 phrase 要求逐字连续，
// 拆词元/窗口的必要性与取舍见 ftsQuery 的注释。
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
		ftsQuery(query), limit)
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

// indexEmpty 判断 doc_chunk 是否零行。Search 只验「索引非空」——
// 新鲜度（语料是否被改动）由工具层 CheckFresh 负责，两者解耦。
func (idx *Index) indexEmpty(ctx context.Context) (bool, error) {
	var n int
	if err := idx.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM doc_chunk").Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
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
