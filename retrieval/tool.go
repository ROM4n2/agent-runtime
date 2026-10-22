package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agent-runtime/agent/tool"
	"agent-runtime/llm"
)

// ErrEmbeddingUnavailable 表示向量通道本次不可用（embedder 调用链故障：
// 网络/API/响应解析类的瞬时失败），值得 agent.Run 重试一次（ADR-0017 决策 5）。
// 与「根本没配 embedder」（ErrNoEmbedder，配置缺失，重试无用）是两回事，
// 完整分类表见 classifySearchError。
var ErrEmbeddingUnavailable = errors.New("retrieval: embedding channel unavailable")

// embeddingUnavailableError 把分类哨兵与原始病因包成同一个错误，
// 实现 tool.RetryableError（Retryable()=true）供 agent.Run 分支决策。
// Unwrap 返回双链：errors.Is(err, ErrEmbeddingUnavailable) 命中分类哨兵，
// errors.Is(err, 原始embedder错误) 命中病因——分类可判定、归因不丢失。
type embeddingUnavailableError struct{ cause error }

func (e *embeddingUnavailableError) Error() string {
	return ErrEmbeddingUnavailable.Error() + ": " + e.cause.Error()
}

// Retryable 实现 tool.RetryableError：向量通道瞬时不可用，值得重试一次。
func (e *embeddingUnavailableError) Retryable() bool { return true }

func (e *embeddingUnavailableError) Unwrap() []error {
	return []error{ErrEmbeddingUnavailable, e.cause}
}

// 入参默认值与上界。集中为常量：schema 文案（模型看到的）与解析默认值（引擎执行的）
// 必须同源，禁止两处各写一份数字——改默认值只改这里，Spec 与解析随之固定。
const (
	defaultTopK        = 5
	maxTopK            = 10
	defaultTokenBudget = 1500
)

// searchToolDescription 是喂给模型的工具描述，必须包含短查询提示：
// trigram 分词器要求匹配区间内 ≥3 个字符，2 字中文查询必然零命中
// （ADR-0017 修订注记的残留缺口，ADR-0018 评测单独观察）。
// 不写这条提示的后果是模型反复用 2 字查询，然后把「零命中」误判成语料里没有答案。
const searchToolDescription = `在已建索引的语料库中检索相关文档片段，返回可回链原文的引用区间。
中文查询词需至少 3 个字符（分词器限制）；英文短语可用空格分隔多词。
结果按相关度降序，每条命中含 chunk_id/doc_id/source/span/score/text，
回答引用请使用 doc_id + span。mode 标注本次召回通道（lexical/hybrid），
truncated=true 表示有命中因 token 预算或 top_k 被截断。`

// searchTool 把 Index 包成模型可调用的 search_docs 工具（ADR-0017 决策 4）。
// corpusDir 每次 Execute 都走 CheckFresh：语料可能被外部改动，
// 索引新鲜度是调用时检查点，不是打开时的一次性假设。
type searchTool struct {
	idx       *Index
	corpusDir string
}

// SearchTool 返回可注册进 tool.Registry 的检索工具。
// 依赖方向合法：retrieval → agent/tool（下层工具抽象），
// architecture_test.go 的门禁把守上层包不被反向 import。
func SearchTool(idx *Index, corpusDir string) tool.Tool {
	return &searchTool{idx: idx, corpusDir: corpusDir}
}

// Spec 返回工具的线上形态：名称、描述（含短查询提示）与三参数 JSON Schema。
func (t *searchTool) Spec() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.FunctionSpec{
			Name:        "search_docs",
			Description: searchToolDescription,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "检索查询词。中文需至少 3 个字符（分词器限制），英文短语可用空格分隔多词。",
					},
					"top_k": map[string]any{
						"type":        "integer",
						"description": "返回命中数上限，1..10，默认 5。",
						"minimum":     1,
						"maximum":     maxTopK,
						"default":     defaultTopK,
					},
					"token_budget": map[string]any{
						"type":        "integer",
						"description": "返回文本的 token 预算（rune 数/2 近似），默认 1500；超预算从尾部低分命中截断且 truncated=true，0 表示只保底返回最高分一条。",
						"minimum":     0,
						"default":     defaultTokenBudget,
					},
				},
				"required": []string{"query"},
			},
		},
	}
}

// Execute 实现 tool.Tool。ctx 全程透传（Task 1 后的接口签名，ADR-0014）：
// 检索在 hybrid 下必然发嵌入 HTTP，无 ctx 则 worker 的抢占式取消中断不了它。
// 流程：解析入参 → 新鲜度检查 → 检索 → 错误分类 → 序列化。
func (t *searchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	query, topK, tokenBudget, err := parseSearchArgs(args)
	if err != nil {
		return "", err
	}
	// 新鲜度检查与 Search 解耦（Task 6 分工）：语料被改动必须显式失败，
	// 而不是拿着旧索引给模型一个看起来正常的结果。
	if err := t.idx.CheckFresh(ctx, t.corpusDir); err != nil {
		// ErrIndexMissing/ErrCorpusStale 经 freshnessHint 附上可操作文案（均 non-retryable）：
		// Task 8 交付 -index CLI 后，模型/运维才拿得到「下一步该跑什么」的指引（CRV 黄卡 4）。
		return "", freshnessHint(err)
	}
	res, err := t.idx.Search(ctx, query, tokenBudget)
	if err != nil {
		return "", classifySearchError(t.idx, err)
	}
	return renderResult(res, topK)
}

// parseSearchArgs 解析并校验模型入参，返回 (query, topK, tokenBudget)。
// 指针字段区分「缺省」与「显式传 0」：top_k=0 是越界（报错），
// token_budget=0 是合法语义（Task 6：保底一条 + Truncated），两者不可混为一谈。
func parseSearchArgs(args json.RawMessage) (query string, topK, tokenBudget int, err error) {
	var in struct {
		Query       string `json:"query"`
		TopK        *int   `json:"top_k"`
		TokenBudget *int   `json:"token_budget"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", 0, 0, fmt.Errorf("retrieval: invalid search args: %w", err)
	}
	// TrimSpace 后判空：纯空白查询对 FTS5 与嵌入都是无意义输入，
	// 且前导/尾随空格直进 MATCH 有触发 FTS 语法错误的风险。
	query = strings.TrimSpace(in.Query)
	if query == "" {
		return "", 0, 0, errors.New("retrieval: query is required")
	}

	topK = defaultTopK
	if in.TopK != nil {
		topK = *in.TopK
	}
	// 越界即报错而非 clamp（防呆）：模型幻觉出的参数必须得到明确反馈让它自纠，
	// 静默修正会让「我要 10 条」悄悄变成 5，模型无从得知口径已变。
	if topK < 1 || topK > maxTopK {
		return "", 0, 0, fmt.Errorf("retrieval: top_k must be in 1..%d, got %d", maxTopK, topK)
	}

	tokenBudget = defaultTokenBudget
	if in.TokenBudget != nil {
		tokenBudget = *in.TokenBudget
	}
	if tokenBudget < 0 {
		return "", 0, 0, fmt.Errorf("retrieval: token_budget must be >= 0, got %d", tokenBudget)
	}
	return query, topK, tokenBudget, nil
}

// classifySearchError 是 Search 错误的单一分类表（一处定义，注释即文档）：
//
//	| Search 返回的错误                        | 工具层返回              | 可重试 | 理由 |
//	| ErrIndexMissing / ErrCorpusStale         | 原样返回                | 否     | 索引未建/语料需重建，重试无意义 |
//	| ErrNoEmbedder                            | 原样返回                | 否     | 配置缺失，重试再多次也不会出现 embedder |
//	| ctx 取消/超时                            | 原样返回                | 否     | 上层抢占语义（ADR-0014），重试只会在已取消的 ctx 上再失败一次，且污染取消归因 |
//	| 其他错误 且 idx hybrid 就绪              | ErrEmbeddingUnavailable | 是     | hybrid 下只可能是 embedder 调用链（网络/API/解析）瞬时故障 |
//	| 其他错误 且纯词法模式                    | 原样返回                | 否     | SQL/游标类故障，与 embedder 无关 |
//
// 「未配置 embedder」与「embedder 调用失败」必须分开：前者是配置问题
// （non-retryable），后者是瞬时故障（retryable）——把前者也判可重试，
// agent 会对一个永远不可能成功的调用空转重试。
// ErrNoEmbedder 分支是防御性保留：Search 的门禁保证只有 hybrid 才调 embedTexts，
// 该哨兵正常情况下到不了这里；列出来是防未来路径混入它时被误包装成可重试。
func classifySearchError(idx *Index, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrIndexMissing), errors.Is(err, ErrCorpusStale), errors.Is(err, ErrNoEmbedder):
		return err
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case idx.hybridEnabled():
		return &embeddingUnavailableError{cause: err}
	default:
		return err
	}
}

// freshnessHint 给两个新鲜度哨兵附上用户可操作的下一步文案（CRV Task 7 黄卡 4 的
// trigger：Task 8 交付 -index CLI 后补，此前写死命令会指向不存在的入口）。
// 用 %w 保留哨兵：errors.Is 仍命中分类，不挂 Retryable 方法（两者本就 non-retryable），
// 归因不丢、只增可读性。未建索引用「请先运行」，语料陈陈旧用「请重新运行」——
// 两者下一步动作不同，文案必须可判别。非哨兵错误原样返回，只给运维能自修的两类加指引。
func freshnessHint(err error) error {
	switch {
	case errors.Is(err, ErrIndexMissing):
		return fmt.Errorf("%w: 请先运行 go run . -index <语料目录> 构建索引", err)
	case errors.Is(err, ErrCorpusStale):
		return fmt.Errorf("%w: 请重新运行 go run . -index <语料目录> 重建索引", err)
	default:
		return err
	}
}

// hitJSON / resultJSON 是工具的线上返回形状（ADR-0017 决策 4）：
// span 为原文 rune 偏移 [start,end)（含头不含尾），配合 doc_id 供上层回链原文；
// score 只序列化融合分口径——Search 出口的 Hit.Score 已是 RRF 融合分
// （0.5/(60+rank) 累加，越大越好，跨模式可比）；词法模式同样经 fuseHits
// 折算为 RRF 序位分（0.5/(60+词法rank)），FTS5 bm25 的原始负值只存在于
// SearchLexical 单路出口，绝不漏到这里。
type hitJSON struct {
	ChunkID int64   `json:"chunk_id"`
	DocID   string  `json:"doc_id"`
	Source  string  `json:"source"`
	Span    [2]int  `json:"span"`
	Score   float64 `json:"score"`
	Text    string  `json:"text"`
}

type resultJSON struct {
	Mode      string    `json:"mode"`
	Truncated bool      `json:"truncated"`
	Hits      []hitJSON `json:"hits"`
}

// renderResult 把 SearchResult 序列化为工具返回 JSON，并落实 top_k 与 span 硬契约。
// top_k 在本层生效：Search 只保证两路融合输入充分（每路候选上限 50），
// 最终返回条数由工具层决定（Task 6 注释的明确分工）。
func renderResult(res SearchResult, topK int) (string, error) {
	hits := res.Hits
	// top_k 截断同样丢尾部低分命中，必须并进 truncated——不静默。
	truncated := res.Truncated
	if len(hits) > topK {
		hits = hits[:topK]
		truncated = true
	}
	// 空切片而非 nil：JSON 出 "[]" 不出 null，模型解析端不因 null 歧义出错。
	out := resultJSON{Mode: res.Mode, Truncated: truncated, Hits: make([]hitJSON, 0, len(hits))}
	for _, h := range hits {
		// span 硬契约防御：Rebuild 写出的 chunk 必然 span_end>span_start，
		// 零值 span + 非空 text 只可能是实现错误——无 span 的命中无法回链，
		// 宁可明确失败也不把「可引用」变成空话（ADR-0017 决策 4）。
		if h.Span == [2]int{} && h.Text != "" {
			return "", errors.New("retrieval: hit missing span (implementation error)")
		}
		out.Hits = append(out.Hits, hitJSON{
			ChunkID: h.ChunkID,
			DocID:   h.DocID,
			Source:  h.Source,
			Span:    h.Span,
			Score:   h.Score,
			Text:    h.Text,
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("retrieval: marshal search result: %w", err)
	}
	return string(b), nil
}
