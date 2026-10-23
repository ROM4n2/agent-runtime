package eval

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"agent-runtime/agent"
)

// FirstError 三分类（ADR-0018 决策 2.3 首错归因）："" 表示无错。
// 禁止把 "max steps exceeded" 当失败根因——那 99% 是表层症状。
const (
	FirstErrorNone                 = ""
	FirstErrorNoSearch             = "no-search"
	FirstErrorWrongSpan            = "wrong-span"
	FirstErrorRightSpanWrongReason = "right-span-wrong-reasoning"
)

// spanRecallThreshold 是 span 命中判定阈值：recall >= 0.5 记「找对了位置」。
// 0.5 与 ADR-0018 后果节黄卡预案「边界重叠按 0.5 权重计入」同源——部分相交时
// 交集/并集 < 1 是自然结果，代码里不需要为部分相交特判任何魔法数；
// 阈值本身是「期望区间被覆盖过半即算命中」的下限。
const spanRecallThreshold = 0.5

// searchToolName 是检索工具在 trace Tool 字段里的名字（ADR-0017 决策 4）。
const searchToolName = "search_docs"

// GradeResult 是单条任务的程序判定结果。
// 判定只用程序规则，禁 LLM-as-Judge（ADR-0018 决策 2.2）：这里是引用命中的
// 唯一判定实现，禁止第二套口径。
//
// 【计划偏差登记】实施计划 Productions 字面为 `type Grade struct` +
// `func Grade(task, trace, answer) Grade`，但 Go 包级作用域内 type 与 func
// 不可同名（redeclared in this block），该字面签名物理不可两全。
// 取舍：保留函数名 Grade（Task 11 的 Consumes 明确消费 eval.Grade），
// 结果类型改名 GradeResult；字段与 FirstError 取值集合不变。
type GradeResult struct {
	Success    bool
	SpanRecall float64
	FirstError string
	// CitedSpans / CitedDocs 是判定实际使用的引用快照：collectSearch 已解析出
	// hits——把判定用的实际引用带出来，不改判定逻辑。两切片按下标同源
	// （CitedSpans[i] 与 CitedDocs[i] 是同一 hit 的 span 与 doc_id）。
	// 报表明细表「命中 span」列据此渲染（ADR-0018 决策 4）：无此列时
	// 「哪次 repeat 命中了什么」只能回翻 trace 原文，复跑对账断一环。
	CitedSpans [][2]int
	CitedDocs  []string
}

// toolHit 是 grader 从 search_docs 返回 JSON 里取出的引用（doc_id + span）。
// 只声明判定需要的两个字段：text/score 与判定无关，多声明只会让
// 工具侧加字段时误以为这里也要跟着改。
type toolHit struct {
	DocID string `json:"doc_id"`
	Span  [2]int `json:"span"`
}

// Grade 对单条任务做判定。trace 是 agent.Run 的步骤快照，answer 是终态回答。
// 判定顺序即首错归因优先级（ADR-0018 决策 2.3），从早到晚：
//
//	① 未调用 search_docs             → no-search
//	② out-of-corpus 伪造引用          → wrong-span
//	③ span 命中不足（recall < 阈值）   → wrong-span
//	④ span 命中但回答未引用期望文档    → right-span-wrong-reasoning
//	⑤ 全部通过                        → Success
func Grade(task Task, trace []agent.TraceStep, answer string) GradeResult {
	called, hits := collectSearch(trace)
	spans, docs := hitSpans(hits), hitDocs(hits)
	recall := spanRecall(spans, task.ExpectSpans)
	g := GradeResult{SpanRecall: recall, CitedSpans: spans, CitedDocs: docs}

	// ① 无条件最高优先级：没有检索步骤，后面两条都无从谈起。
	// 「根本没搜」就是第一个偏差步骤，先于任何 span/结论判定。
	if !called {
		g.FirstError = FirstErrorNoSearch
		return g
	}

	if task.Scope == ScopeOutOfCorpus {
		// out-of-corpus 题考「不编造」：期望无引用。伪造引用有两种形态——
		// 检索命中了期望文档（trace 里留痕），或回答里直接出现期望文档 id
		// （无据直引，trace 里可能什么都没有）。两者都记 wrong-span。
		if docsIntersect(hitDocs(hits), task.ExpectDocs) || answerCitesAny(answer, task.ExpectDocs) {
			g.FirstError = FirstErrorWrongSpan
			return g
		}
		g.Success = true
		return g
	}

	if recall < spanRecallThreshold {
		g.FirstError = FirstErrorWrongSpan
		return g
	}
	// 结论正确性的可执行近似（ADR-0018 非目标声明不评生成质量）：
	// 「引用出现在最终回答」= 模型确实把检索到的文档用进了结论。
	// 空回答或通篇不含任一期望文档 id，说明检索白做。
	if strings.TrimSpace(answer) == "" || !answerCitesAny(answer, task.ExpectDocs) {
		g.FirstError = FirstErrorRightSpanWrongReason
		return g
	}
	g.Success = true
	return g
}

// collectSearch 单次遍历 trace，取出 search_docs 的调用事实与全部引用。
// 调用事实只看 KindToolCall：工具执行失败也会先 emit tool_call（engine.go:120），
// 只是随后是 KindError 而不是 KindToolResult——「调了但没拿到结果」仍算调过。
// 引用只看 KindToolResult 的 detail：那是工具返回串原文（emit 的实参就是 res）。
func collectSearch(trace []agent.TraceStep) (called bool, hits []toolHit) {
	for _, s := range trace {
		if s.Tool != searchToolName {
			continue
		}
		switch s.Kind {
		case agent.KindToolCall:
			called = true
		case agent.KindToolResult:
			hits = append(hits, parseHits(s.Detail)...)
		}
	}
	return called, hits
}

// parseHits 从 search_docs 的 tool_result detail 中解析引用集合。
// agent.TraceBuffer.Append 会把 detail 截到 512 字节（AGENTS.md §3「Trace 有界」），
// 而检索返回的 text 动辄上千字节——线上 trace 里的 tool_result 几乎必然是残串。
// 因此完整 JSON 解析失败时走 salvageHits 抢救其中已完整的 hit：
// 否则全量任务会被静默判成 wrong-span，评测报表变成假红。
// 非 JSON 文本（如 "error: ..."）两条路径都安全返回 nil。
func parseHits(detail string) []toolHit {
	var res struct {
		Hits []toolHit `json:"hits"`
	}
	if err := json.Unmarshal([]byte(detail), &res); err == nil {
		return res.Hits
	}
	return salvageHits(detail)
}

// salvageHits 从被截断的返回串里抢救引用（Task 12 缺陷 F 的修复点）。
// 走字段级提取：renderResult 的字段序是 doc_id → source → span → score → text，
// 截断点（agent.MaxTraceDetail=512）几乎必然落在某个 hit 的 text 里，
// 而该 hit 的 doc_id/span 早已完整——按「doc_id 出现 + 紧随的 span」配对即能全部救出。
//
// 为什么不再逐条 json.Decode：截断点落在第一个 hit 的 text 内部时，
// Decode 立刻失败 → 一条都救不回来（512 字节连第一条 text 都放不下，
// 缺陷 F 实证：全量任务被判 wrong-span）。字段级提取对截断点位置不敏感。
// 解析出 0 对（非 JSON 文本、"error: …"、空串）时退回 json 前缀解码路径，
// 完整 JSON 场景由 parseHits 的 Unmarshal 先接住，语义不变。
func salvageHits(detail string) []toolHit {
	if hits := salvageFieldPairs(detail); len(hits) > 0 {
		return hits
	}
	return salvageJSONPrefix(detail)
}

// 字段级抢救用的正则：不解析 JSON 结构，只按 wire 字段序抽成对引用。
// 预编译为包级 var——每次解析 trace 都会调用，热路径上不重编译。
// 文本内部的引号是转义态（\"），不会误命中 "doc_id":"…" 这种形态。
var (
	salvageDocIDRe = regexp.MustCompile(`"doc_id"\s*:\s*"([^"]*)"`)
	salvageSpanRe  = regexp.MustCompile(`"span"\s*:\s*\[\s*(-?\d+)\s*,\s*(-?\d+)\s*\]`)
)

// salvageFieldPairs 按出现顺序把 doc_id 与紧随其后的 span 配成一条引用：
// 截断点之前的完整对全部救出；截断点落在 doc_id 与 span 之间时该对不完整，丢弃。
func salvageFieldPairs(detail string) []toolHit {
	ids := salvageDocIDRe.FindAllStringSubmatchIndex(detail, -1)
	if len(ids) == 0 {
		return nil
	}
	spans := salvageSpanRe.FindAllStringSubmatchIndex(detail, -1)

	var hits []toolHit
	next := 0
	for _, id := range ids {
		for next < len(spans) && spans[next][0] < id[1] {
			next++ // span 若落在本 doc_id 之前，说明它属于上一条或无处配对
		}
		if next >= len(spans) {
			break // 截断点落在 doc_id 与 span 之间：这条引用不完整
		}
		sp := spans[next]
		next++
		start, errStart := strconv.Atoi(detail[sp[2]:sp[3]])
		end, errEnd := strconv.Atoi(detail[sp[4]:sp[5]])
		if errStart != nil || errEnd != nil {
			continue // 数字段被截断污染：宁可少计，不补假 span
		}
		hits = append(hits, toolHit{DocID: detail[id[2]:id[3]], Span: [2]int{start, end}})
	}
	return hits
}

// salvageJSONPrefix 是字段级提取的兜底：逐条 Decode "hits":[ 之后的完整对象。
// 截断点落在第一个 hit 内部时会返回空（这正是缺陷 F 的原始行为），
// 保留它是为了「完整 JSON 但顶层形状不认识」等异常输入仍能按原语义处理。
func salvageJSONPrefix(detail string) []toolHit {
	const key = `"hits":[`
	i := strings.Index(detail, key)
	if i < 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(detail[i+len(key):]))
	var hits []toolHit
	for dec.More() {
		var h toolHit
		if err := dec.Decode(&h); err != nil {
			break // 截断点落在这个 hit 内部：已抢救出的完整 hit 照常计分
		}
		hits = append(hits, h)
	}
	return hits
}

// spanScore 是单个引用区间与单个期望区间的吻合度 = |交集| / |并集|（交并比）。
// 区间语义 [start,end) 含头不含尾，与 retrieval 的 span 契约一致。
// 部分相交自然落到 (0,1)：这正是 ADR-0018 黄卡预案「边界重叠按 0.5 权重计入」
// 的口径来源，无需特判。零长区间（实现错误或未初始化）记 0 而非 NaN——
// NaN 的所有比较都为 false，会把命中判定变成不可解释的随机结果。
func spanScore(got, want [2]int) float64 {
	inter := spanIntersect(got, want)
	if inter <= 0 {
		return 0
	}
	union := (got[1] - got[0]) + (want[1] - want[0]) - inter
	if union <= 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// spanIntersect 返回两个 [start,end) 区间的交集长度（不相交或零长为 0）。
// guard clause 展平：lo/hi 各取一次极值即得交集，无需分支讨论包含关系。
func spanIntersect(a, b [2]int) int {
	lo := a[0]
	if b[0] > lo {
		lo = b[0]
	}
	hi := a[1]
	if b[1] < hi {
		hi = b[1]
	}
	if hi <= lo {
		return 0
	}
	return hi - lo
}

// spanRecall 对每个期望区间取与实际引用的最大吻合度，求和后除以期望区间数。
// 每个 want 独立取最大匹配：模型多引几条、换个写法引都不惩罚，
// 只要求每个期望点都被覆盖到。want 为空 → 1.0：无期望引用题（out-of-corpus）
// 的真实口径是「无伪造引用」，由 Grade 的 out-of-corpus 分支经 doc 集合落实；
// 此处记满分是为了不把「没有期望」误算成「全不命中」。
func spanRecall(gotSpans, wantSpans [][2]int) float64 {
	if len(wantSpans) == 0 {
		return 1.0
	}
	var sum float64
	for _, want := range wantSpans {
		var best float64
		for _, got := range gotSpans {
			if s := spanScore(got, want); s > best {
				best = s
			}
		}
		sum += best
	}
	return sum / float64(len(wantSpans))
}

// hitSpans / hitDocs 把引用集合投影成 span 集合与文档 id 集合。
func hitSpans(hits []toolHit) [][2]int {
	spans := make([][2]int, 0, len(hits))
	for _, h := range hits {
		spans = append(spans, h.Span)
	}
	return spans
}

func hitDocs(hits []toolHit) []string {
	docs := make([]string, 0, len(hits))
	for _, h := range hits {
		docs = append(docs, h.DocID)
	}
	return docs
}

// docsIntersect 判断两个文档 id 集合是否有交集。
func docsIntersect(got, want []string) bool {
	for _, g := range got {
		for _, w := range want {
			if g == w {
				return true
			}
		}
	}
	return false
}

// answerCitesAny 判断回答文本是否出现 want 中任一文档 id。
// 子串匹配而非词匹配：doc_id 是路径（docs/adr/0015-...），回答里以引用形式
// 出现时前后缀不定（括号、句号、markdown 链接），词边界切分会漏掉这些引用。
func answerCitesAny(answer string, want []string) bool {
	for _, id := range want {
		if id != "" && strings.Contains(answer, id) {
			return true
		}
	}
	return false
}
