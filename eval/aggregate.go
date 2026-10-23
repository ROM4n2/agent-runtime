package eval

import (
	"math"
	"sort"
)

// 聚合键常量集中一处：报表（report.go）与测试都按常量取键，禁在调用点散落
// 字面量——键名漂移时要么编译期抓不到、要么报表静默缺列。
const (
	keySuccessMedian      = "success_median"
	keySuccessP25         = "success_p25"
	keySuccessP75         = "success_p75"
	keySpanRecallMedian   = "span_recall_median"
	keySpanRecallP25      = "span_recall_p25"
	keySpanRecallP75      = "span_recall_p75"
	keyFirstErrorDist     = "first_error_distribution"
	keyLatencyP50         = "latency_p50"
	keyLatencyP95         = "latency_p95"
	keyStepsMean          = "steps_mean"
	keyTasks              = "tasks"
	keyTaskID             = "id"
	keyTaskScope          = "scope"
	keyTaskSuccess        = "success"
	keyTaskSuccessRate    = "success_rate"
	keyTaskStepsMed       = "steps_median"
	keyTaskLatencyMSMed   = "latency_ms_median"
	keyTaskFirstErrorMode = "first_error"
)

// firstErrorNoneLabel 是首错分布里「无错」的计分标签。FirstErrorNone 本身是
// 空串，直接当 map 键会序列化成 JSON 空键，报表不可读。
const firstErrorNoneLabel = "none"

// majorityThreshold 是逐任务成功率的多数决阈值（严格大于）：repeat=2 且
// 1 胜 1 负时 0.5 不算过线——平局即未通过，不给「看起来像过了」的中间态。
const majorityThreshold = 0.5

// topFirstErrors 是报表「首错 Top3」的截断长度（ADR-0018 决策 4）。
const topFirstErrors = 3

// aggregate 把逐次明细（任务 × repeat）收敛成组级分布 + 逐任务跨 repeat 聚合
// （ADR-0018 决策 3：单次值只进明细，聚合必出分布）。
//
// 两档粒度不可互相替代：组级分位回答「这组整体如何」，逐任务聚合回答
// 「哪道题不稳」——后者是复跑挑题与优化归因的输入。
func aggregate(tasks []Task, results []TaskResult) map[string]any {
	byTask := groupByTask(tasks, results)

	// 组级分布：摊平全部单次值后取分位（不分任务，反映整组波动）。
	successes := make([]float64, 0, len(results))
	recalls := make([]float64, 0, len(results))
	latencies := make([]float64, 0, len(results))
	steps := make([]int, 0, len(results))
	firstErrDist := make(map[string]int, len(results))
	for _, r := range results {
		successes = append(successes, boolToFloat(r.Success))
		recalls = append(recalls, r.SpanRecall)
		latencies = append(latencies, float64(r.LatencyMS))
		steps = append(steps, r.Steps)
		firstErrDist[firstErrorLabel(r.FirstError)]++
	}

	perTask := make([]map[string]any, 0, len(tasks))
	for _, tk := range tasks {
		perTask = append(perTask, taskEntry(tk, byTask[tk.ID]))
	}

	return map[string]any{
		keySuccessMedian:    percentile(successes, 0.50),
		keySuccessP25:       percentile(successes, 0.25),
		keySuccessP75:       percentile(successes, 0.75),
		keySpanRecallMedian: percentile(recalls, 0.50),
		keySpanRecallP25:    percentile(recalls, 0.25),
		keySpanRecallP75:    percentile(recalls, 0.75),
		keyFirstErrorDist:   firstErrDist,
		keyLatencyP50:       percentile(latencies, 0.50),
		keyLatencyP95:       percentile(latencies, 0.95),
		keyStepsMean:        meanInt(steps),
		keyTasks:            perTask,
	}
}

// groupByTask 按任务 id 归拢单次结果，保留 results 内的相对顺序（repeat 内序）。
// 顺序是审计对账锚：明细表与逐任务聚合都按它渲染，乱序会让「哪次 repeat 出的错」
// 变得不可追问。
func groupByTask(tasks []Task, results []TaskResult) map[string][]TaskResult {
	byTask := make(map[string][]TaskResult, len(tasks))
	for _, r := range results {
		byTask[r.TaskID] = append(byTask[r.TaskID], r)
	}
	return byTask
}

// taskEntry 产出一条任务的跨 repeat 聚合条目。
func taskEntry(tk Task, rs []TaskResult) map[string]any {
	success, rate := majoritySuccess(rs)
	return map[string]any{
		keyTaskID:             tk.ID,
		keyTaskScope:          tk.Scope,
		keyTaskSuccess:        success,
		keyTaskSuccessRate:    rate,
		keySpanRecallMedian:   median(projectFloats(rs, func(r TaskResult) float64 { return r.SpanRecall })),
		keyTaskStepsMed:       median(projectFloats(rs, func(r TaskResult) float64 { return float64(r.Steps) })),
		keyTaskLatencyMSMed:   median(projectFloats(rs, func(r TaskResult) float64 { return float64(r.LatencyMS) })),
		keyTaskFirstErrorMode: modeFirstError(rs),
	}
}

// projectFloats 把单次结果投影成 float64 序列，供分位函数统一处理。
func projectFloats(rs []TaskResult, get func(TaskResult) float64) []float64 {
	out := make([]float64, 0, len(rs))
	for _, r := range rs {
		out = append(out, get(r))
	}
	return out
}

// majoritySuccess 按多数决定任务成败：成功率严格大于 0.5 才记通过，
// 同时返回原始成功率（明细表要显示这个数，不让读者只看到布尔）。
func majoritySuccess(rs []TaskResult) (bool, float64) {
	if len(rs) == 0 {
		return false, 0
	}
	wins := 0
	for _, r := range rs {
		if r.Success {
			wins++
		}
	}
	rate := float64(wins) / float64(len(rs))
	return rate > majorityThreshold, rate
}

// modeFirstError 取首错众数。计数并列时取首次出现者：results 顺序确定，
// 故同一份输入的聚合结果可复现（复跑对账的前提）。
func modeFirstError(rs []TaskResult) string {
	counts := make(map[string]int, len(rs))
	order := make([]string, 0, len(rs))
	for _, r := range rs {
		label := firstErrorLabel(r.FirstError)
		if _, seen := counts[label]; !seen {
			order = append(order, label)
		}
		counts[label]++
	}
	best, bestN := "", 0
	for _, label := range order {
		if counts[label] > bestN {
			best, bestN = label, counts[label]
		}
	}
	return best
}

// firstErrorLabel 把空串首错翻译成可读标签（见 firstErrorNoneLabel）。
func firstErrorLabel(v string) string {
	if v == FirstErrorNone {
		return firstErrorNoneLabel
	}
	return v
}

// percentile 线性插值分位（与 numpy 默认 linear 口径一致）。内部排序副本：
// 禁就地排序入参——results 的顺序是审计锚，被排序过就对不上账了。
// 空序列记 0：调用方（Run）保证 repeat>=1，这里是防御而非正常路径。
func percentile(vals []float64, q float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := q * float64(len(sorted)-1)
	lo := math.Floor(pos)
	hi := math.Ceil(pos)
	if lo == hi {
		return sorted[int(pos)]
	}
	frac := pos - lo
	return sorted[int(lo)]*(1-frac) + sorted[int(hi)]*frac
}

// median 即 P50：分位数集中在 percentile 一处实现，禁第二套插值口径。
func median(vals []float64) float64 {
	return percentile(vals, 0.50)
}

// meanInt 求整型序列均值（平均步数）。空序列记 0。
func meanInt(vals []int) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0
	for _, v := range vals {
		sum += v
	}
	return float64(sum) / float64(len(vals))
}

// boolToFloat 把布尔成败翻成 0/1：成功率分位与其他指标共用一套 percentile，
// 不需要为布尔单开一条统计路径。
func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// ---- 聚合表读取（report.go 渲染用；键名与上面的常量同一处） ----

// taskAgg 是单任务跨 repeat 的聚合视图：markdown 逐任务明细表的数据源。
type taskAgg struct {
	ID            string
	Scope         string
	Success       bool
	SuccessRate   float64
	SpanRecallMed float64
	StepsMed      float64
	LatencyMSMed  float64
	FirstError    string
}

// decodeTaskAggs 从聚合表读回逐任务条目。容忍两种底层形态：内存态
// []map[string]any 与 JSON 往返后的 []any（元素为 map[string]any）——
// 报表要能被「读 JSON 再渲染」的路径复用，类型断言写死一种会静默出空表。
func decodeTaskAggs(m map[string]any) []taskAgg {
	switch raw := m[keyTasks].(type) {
	case []map[string]any:
		out := make([]taskAgg, 0, len(raw))
		for _, e := range raw {
			out = append(out, decodeTaskAgg(e))
		}
		return out
	case []any:
		out := make([]taskAgg, 0, len(raw))
		for _, e := range raw {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, decodeTaskAgg(em))
		}
		return out
	}
	return nil
}

func decodeTaskAgg(e map[string]any) taskAgg {
	return taskAgg{
		ID:            stringField(e, keyTaskID),
		Scope:         stringField(e, keyTaskScope),
		Success:       boolField(e, keyTaskSuccess),
		SuccessRate:   floatField(e, keyTaskSuccessRate),
		SpanRecallMed: floatField(e, keySpanRecallMedian),
		StepsMed:      floatField(e, keyTaskStepsMed),
		LatencyMSMed:  floatField(e, keyTaskLatencyMSMed),
		FirstError:    stringField(e, keyTaskFirstErrorMode),
	}
}

func stringField(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func boolField(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}

func floatField(m map[string]any, key string) float64 {
	v, _ := m[key].(float64)
	return v
}

// firstErrorCounts 取首错分布全量并按计数降序排（并列按键名升序，保证可复现）。
// 渲染在 report.go：聚合只负责把顺序排定，不负责排版。
// 容忍两种底层形态（与 decodeTaskAggs 同风格）：内存态 map[string]int 与
// JSON 往返后的 map[string]any（计数值是 float64，也可能是手工构造的 int）——
// 报表要能被「读 JSON 再渲染」的路径复用，只断言一种会让往返后的分布
// 静默渲染成「（无）」的假空报表。
func firstErrorCounts(m map[string]any) []firstErrorCount {
	var dist map[string]int
	switch raw := m[keyFirstErrorDist].(type) {
	case map[string]int:
		dist = raw
	case map[string]any:
		dist = make(map[string]int, len(raw))
		for label, v := range raw {
			switch c := v.(type) {
			case float64:
				dist[label] = int(c)
			case int:
				dist[label] = c
			}
		}
	default:
		return nil
	}
	counts := make([]firstErrorCount, 0, len(dist))
	for label, c := range dist {
		counts = append(counts, firstErrorCount{Label: label, Count: c})
	}
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].Count != counts[j].Count {
			return counts[i].Count > counts[j].Count
		}
		return counts[i].Label < counts[j].Label
	})
	return counts
}

// firstErrorTop 截取首错分布前 N 项（报表「首错 Top3」用）。
func firstErrorTop(m map[string]any, n int) []firstErrorCount {
	counts := firstErrorCounts(m)
	if len(counts) > n {
		counts = counts[:n]
	}
	return counts
}

// firstErrorCount 是首错分布的一项（标签 + 计数）。
type firstErrorCount struct {
	Label string
	Count int
}
