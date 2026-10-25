package eval

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ReportEnv 是报表「环境」固定节的环境快照（ADR-0018 决策 4）。
// SetHash 用内容 SHA-256：同 hash 即同题，复跑与审计都按它对账。
type ReportEnv struct {
	Group           string
	Seed            int64
	Model           string
	TemplateVersion string
	CorpusDir       string
	Repeat          int
	DBPath          string
	SetPath         string
	SetHash         string
	// TokenCounting 标注 token 计数口径："injected"（注入计数函数）或
	// "not-wired"（未接入，明细记 0）。
	TokenCounting string
}

// reportFileLayout 是报表文件名的时间布局（ADR-0018 决策 1）：
// YYYY-MM-DD-<group>-r<N>.{md,json}。日期用本地时区的当天——同一天跑的三组
// 报表按组名区分，跨天自然分文件，不需要额外版本号。
const reportFileLayout = "2006-01-02"

// WriteMarkdown 渲染人读报表（ADR-0018 决策 4 固定节）并落盘到 dir，
// 返回产物路径（装配层直接打日志，不用再猜文件名规则）。
// dir 不存在则创建：OutDir 默认 docs/eval，首次评测不该因目录缺席而失败。
func (r Report) WriteMarkdown(dir string) (string, error) {
	path := filepath.Join(dir, r.reportFileName("md"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("eval: mkdir report dir %s: %w", dir, err)
	}
	if err := os.WriteFile(path, []byte(r.renderMarkdown()), 0o644); err != nil {
		return "", fmt.Errorf("eval: write markdown report %s: %w", path, err)
	}
	return path, nil
}

// WriteJSON 落机器可读报表（复跑锚：逐任务明细全量进 JSON）。
// 与 WriteMarkdown 同源同目录：两份产物必须描述同一轮评测，
// 分目录存放会出现「md 说这轮、json 说上轮」的对账断裂。
func (r Report) WriteJSON(dir string) (string, error) {
	path := filepath.Join(dir, r.reportFileName("json"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("eval: mkdir report dir %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", fmt.Errorf("eval: marshal report json: %w", err)
	}
	b = append(b, '\n') // 末尾换行：git diff 干净，cat 也不粘行
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", fmt.Errorf("eval: write json report %s: %w", path, err)
	}
	return path, nil
}

// reportFileName 拼报表文件名。group 与 repeat 进文件名：同一天多组对照时
// 按名即可分辨是哪一轮，不依赖目录再分层。
func (r Report) reportFileName(ext string) string {
	return fmt.Sprintf("%s-%s-r%d.%s",
		time.Now().Format(reportFileLayout), r.Group, r.Repeat, ext)
}

// metrics 取聚合表；缺失时从明细现场聚合。手工构造的 Report（无 Aggregates）
// 也要能出完整报表——明细表是审计锚，静默出一张缺列的空表比没有报表更坏。
func (r Report) metrics() map[string]any {
	if len(r.Aggregates) > 0 {
		return r.Aggregates
	}
	return aggregate(tasksFromResults(r.TaskResults), r.TaskResults)
}

// tasksFromResults 从明细反推任务维度（id + scope，去重保序），供聚合兜底用。
func tasksFromResults(results []TaskResult) []Task {
	tasks := make([]Task, 0, len(results))
	seen := make(map[string]bool, len(results))
	for _, tr := range results {
		if seen[tr.TaskID] {
			continue
		}
		seen[tr.TaskID] = true
		tasks = append(tasks, Task{ID: tr.TaskID, Scope: tr.Scope})
	}
	return tasks
}

// taskAggs 取逐任务聚合视图（同样走 metrics 兜底）。
func (r Report) taskAggs() []taskAgg {
	return decodeTaskAggs(r.metrics())
}

// renderMarkdown 产出 markdown 全文。节序固定（ADR-0018 决策 4）：
// 任务集 → 环境 → 指标 → 逐任务明细 → 首错 Top3 → 结论。
// 节标题是 run_test.go 的断言对象，改名 = 审计断链，必须同步测试。
func (r Report) renderMarkdown() string {
	m := r.metrics()
	var b strings.Builder

	b.WriteString("# 检索能力评测报表\n\n")
	fmt.Fprintf(&b, "- 组：%s\n- 每任务重复：%d\n", r.Group, r.Repeat)
	b.WriteString("\n> ADR-0018 决策 4 固定节。明细表是审计与复跑的锚，禁止只留汇总数字；\n")
	b.WriteString("> 结论段由人工按反向判据填写，harness 不代劳。\n")

	// ① 任务集版本与 hash
	b.WriteString("\n## 任务集\n\n")
	fmt.Fprintf(&b, "- 路径：`%s`\n", r.Env.SetPath)
	fmt.Fprintf(&b, "- SHA-256：`%s`\n", r.Env.SetHash)
	fmt.Fprintf(&b, "- 任务数：%d\n", len(r.taskAggs()))

	// ② 环境（成对受控四元组的对账锚）
	b.WriteString("\n## 环境\n\n")
	b.WriteString("| 项 | 值 |\n| --- | --- |\n")
	for _, row := range [][2]string{
		{"组", r.Env.Group},
		{"seed", strconv.FormatInt(r.Env.Seed, 10)},
		{"模型", r.Env.Model},
		{"模板版本", r.Env.TemplateVersion},
		{"语料目录", r.Env.CorpusDir},
		{"每任务重复", strconv.Itoa(r.Env.Repeat)},
		{"索引库", r.Env.DBPath},
		{"token 计数口径", tokenCountingNote(r.Env.TokenCounting)},
	} {
		fmt.Fprintf(&b, "| %s | %s |\n", row[0], row[1])
	}

	// ③ 六指标（ADR-0018 决策 3 表）
	b.WriteString("\n## 指标\n\n")
	b.WriteString("| 指标 | 值 |\n| --- | --- |\n")
	fmt.Fprintf(&b, "| 任务成功率（中位数 [P25, P75]） | %s [%s, %s] |\n",
		f2(aggFloatOr(m, keySuccessMedian)), f2(aggFloatOr(m, keySuccessP25)), f2(aggFloatOr(m, keySuccessP75)))
	fmt.Fprintf(&b, "| 引用命中率（中位数 [P25, P75]） | %s [%s, %s] |\n",
		f2(aggFloatOr(m, keySpanRecallMedian)), f2(aggFloatOr(m, keySpanRecallP25)), f2(aggFloatOr(m, keySpanRecallP75)))
	fmt.Fprintf(&b, "| 首错分布 | %s |\n", firstErrorInline(m))
	fmt.Fprintf(&b, "| 延迟 P50 / P95（ms） | %s / %s |\n",
		f1(aggFloatOr(m, keyLatencyP50)), f1(aggFloatOr(m, keyLatencyP95)))
	fmt.Fprintf(&b, "| 平均步数 | %s |\n", f2(aggFloatOr(m, keyStepsMean)))
	promptTokens, completionTokens := r.tokenTotals()
	fmt.Fprintf(&b, "| token 成本（prompt + completion） | %d + %d（%s） |\n",
		promptTokens, completionTokens, r.Env.TokenCounting)

	// ④ 逐任务明细
	b.WriteString("\n## 逐任务明细\n\n")
	b.WriteString("| 任务 | scope | 组 | 成败 | 成功率 | 首错 | SpanRecall 中位 | 命中 span | 步数中位 | 延迟中位(ms) | token(p+c) |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	tokens := r.tokenByTask()
	cited := r.citedByTask()
	for _, ta := range r.taskAggs() {
		tk := tokens[ta.ID]
		// 零引用显式记「（无）」：空单元格与「没渲染」不可分辨（禁静默留空）。
		hitCell := cited[ta.ID]
		if hitCell == "" {
			hitCell = "（无）"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %d+%d |\n",
			ta.ID, ta.Scope, r.Group, passFail(ta.Success), f2(ta.SuccessRate), ta.FirstError,
			f2(ta.SpanRecallMed), hitCell, f0(ta.StepsMed), f1(ta.LatencyMSMed),
			tk.prompt, tk.completion)
	}

	// ⑤ 首错 Top3
	b.WriteString("\n## 首错 Top3\n\n")
	top := firstErrorTop(m, topFirstErrors)
	if len(top) == 0 {
		b.WriteString("（无首错记录）\n")
	}
	for i, fc := range top {
		fmt.Fprintf(&b, "%d. %s × %d\n", i+1, fc.Label, fc.Count)
	}

	// ⑥ 结论段（占位：harness 不出业务结论，ADR-0018 决策 1/4）
	b.WriteString("\n## 结论\n\n")
	b.WriteString("（待人工判定：合入 / 不合入 + 理由。反向判据见 ADR-0018 决策 3——\n")
	fmt.Fprintf(&b, "N=%d 次重复下 lexical 组任务成功率中位数相对 baseline 差值 ≤ ±2/%d 即判「无提升」，不合入。）\n",
		r.Repeat, len(r.taskAggs()))
	return b.String()
}

// tokenCountingNote 把口径标注翻成人类可读说明。not-wired 必须显式说出
// 「明细记 0」：否则读者会把 0 当成「没花 token」而不是「没计数」。
func tokenCountingNote(state string) string {
	switch state {
	case "injected":
		return "injected（已注入计数函数）"
	case "llm-usage":
		return "llm-usage（API 实测返回）"
	case "not-wired":
		return "not-wired（未接入，明细记 0）"
	default:
		return state
	}
}

// passFail 成败列的人类可读值。
func passFail(ok bool) string {
	if ok {
		return "成功"
	}
	return "失败"
}

// tokenPair 是单任务的 token 两侧累计。
type tokenPair struct {
	prompt     int
	completion int
}

// tokenByTask 按任务累计明细里的 token 两侧（逐任务明细表用）。
func (r Report) tokenByTask() map[string]tokenPair {
	out := make(map[string]tokenPair, len(r.TaskResults))
	for _, tr := range r.TaskResults {
		p := out[tr.TaskID]
		p.prompt += tr.PromptTokens
		p.completion += tr.CompletionTokens
		out[tr.TaskID] = p
	}
	return out
}

// tokenTotals 全组 token 两侧累计（六指标表用）。
func (r Report) tokenTotals() (prompt, completion int) {
	for _, tr := range r.TaskResults {
		prompt += tr.PromptTokens
		completion += tr.CompletionTokens
	}
	return prompt, completion
}

// citedByTask 按任务汇总明细里的实际引用（明细表「命中 span」列）：
// 跨 repeat 去重保序——同一引用命中多次只显示一次，不同引用按首次出现排列。
// 复跑对账时「哪道题引了哪段」一眼可读，不用回翻 trace 原文。
// 零引用任务不出现在返回表里：渲染层把缺席翻译成「（无）」。
func (r Report) citedByTask() map[string]string {
	out := make(map[string]string, len(r.TaskResults))
	seen := make(map[string]map[string]bool, len(r.TaskResults))
	for _, tr := range r.TaskResults {
		if seen[tr.TaskID] == nil {
			seen[tr.TaskID] = make(map[string]bool, len(tr.CitedSpans))
		}
		var fresh []string
		for i, sp := range tr.CitedSpans {
			// CitedSpans/CitedDocs 下标同源（grader 同一 hits 投影）；越界防御：
			// 手工构造的明细不该让渲染崩溃。
			doc := ""
			if i < len(tr.CitedDocs) {
				doc = tr.CitedDocs[i]
			}
			abbr := fmt.Sprintf("%s@[%d,%d)", abbrevDocID(doc), sp[0], sp[1])
			if seen[tr.TaskID][abbr] {
				continue
			}
			seen[tr.TaskID][abbr] = true
			fresh = append(fresh, abbr)
		}
		if len(fresh) == 0 {
			continue
		}
		if out[tr.TaskID] == "" {
			out[tr.TaskID] = strings.Join(fresh, " ")
			continue
		}
		out[tr.TaskID] += " " + strings.Join(fresh, " ")
	}
	return out
}

// abbrevDocID 把路径型 doc_id 缩成编号段（docs/adr/0017-xxx.md → 0017）：
// 明细表列宽有限，全路径会把表格撑爆；编号段在同目录内唯一可辨。
// 无 "-" 分隔（非编号命名）时退回 basename——禁静默显示空串。
func abbrevDocID(docID string) string {
	base := docID
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.IndexByte(base, '-'); i > 0 {
		return base[:i]
	}
	return base
}

// aggFloatOr 从聚合表取 float64；缺键或类型不符记 0（渲染层不因单键缺失
// 而整表失败，但 0.00 会露出来，比静默跳过更易被发现）。
func aggFloatOr(m map[string]any, key string) float64 {
	v, _ := m[key].(float64)
	return v
}

// firstErrorInline 把首错分布渲染成一行（六指标表用）。
func firstErrorInline(m map[string]any) string {
	counts := firstErrorCounts(m)
	if len(counts) == 0 {
		return "（无）"
	}
	parts := make([]string, 0, len(counts))
	for _, fc := range counts {
		parts = append(parts, fmt.Sprintf("%s×%d", fc.Label, fc.Count))
	}
	return strings.Join(parts, "，")
}

// 数字格式化：分位数固定两位（对照表对齐）、延迟一位（毫秒粒度足够）、
// 步数取整（均值展示不制造虚假精度）。
func f2(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
func f1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }
func f0(v float64) string { return strconv.Itoa(int(math.Round(v))) }
