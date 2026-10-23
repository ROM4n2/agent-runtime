package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// 任务 scope 三分类（ADR-0018 决策 1）：in-corpus 直答、cross-chunk 跨块综合、
// out-of-corpus 考察不编造。三 scope 必须同时存在于任务集——
// 纯语料内直答题会让切分器与题目共谋，虚高命中率。
const (
	ScopeInCorpus    = "in-corpus"
	ScopeCrossChunk  = "cross-chunk"
	ScopeOutOfCorpus = "out-of-corpus"
)

// Task 是评测集中的单条任务（ADR-0018 决策 1 的条目结构）。
// ExpectSpans 为原文 rune 偏移 [start,end)（含头不含尾），与 retrieval 的
// span 引用契约同一套坐标（ADR-0017 决策 4）。
//
// 【ADR-0018 修订注记】任务集格式由 YAML 改为 JSON 数组：仓库依赖纪律是
// 「默认零第三方依赖，引入前先立 ADR」（AGENTS.md §3），YAML 解析器不在
// 标准库；自写 YAML 子集解析器的风险（静默错解析）大于收益。
// 字段与 ADR-0018 决策 1 的 YAML 版一一对应（snake_case 标签不变），
// 文件后缀随之为 .json。判定逻辑与 span 口径不受影响。
type Task struct {
	ID          string   `json:"id"`
	Prompt      string   `json:"prompt"`
	Scope       string   `json:"scope"`
	ExpectDocs  []string `json:"expect_docs"`
	ExpectSpans [][2]int `json:"expect_spans"`
}

// LoadTasks 从 JSON 文件解析任务集并做 schema 校验。
// 任务集非法 = harness 自身故障（ADR-0018 退出码语义：非 0 只表示 harness 坏，
// 不承载业务结论），因此这里一律显式报错，绝不静默跳过坏题——
// 悄悄丢掉几条题会让任务集规模与噪声带（±2/|set|）对不上。
func LoadTasks(path string) ([]Task, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: read task set: %w", err)
	}
	var tasks []Task
	if err := json.Unmarshal(b, &tasks); err != nil {
		return nil, fmt.Errorf("eval: parse task set %s: %w", path, err)
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("eval: task set %s is empty", path)
	}
	for i, t := range tasks {
		if err := t.validate(); err != nil {
			// 带上条目序号：任务集一多，光有字段名定位不到是哪条题。
			return nil, fmt.Errorf("eval: task set %s entry %d: %w", path, i, err)
		}
	}
	return tasks, nil
}

// validate 逐条校验 schema。非法项一律 error，不做任何自动修正——
// 静默修题（如把倒挂区间翻过来）会让期望值与人工核定记录脱节。
func (t Task) validate() error {
	if t.ID == "" {
		return errors.New("id is required")
	}
	if t.Prompt == "" {
		return errors.New("prompt is required")
	}
	switch t.Scope {
	case ScopeInCorpus, ScopeCrossChunk, ScopeOutOfCorpus:
	default:
		return fmt.Errorf("scope must be one of %s/%s/%s, got %q",
			ScopeInCorpus, ScopeCrossChunk, ScopeOutOfCorpus, t.Scope)
	}
	// out-of-corpus 题的 expect_docs 仍须非空：那里标注的是「会被误引的文档」，
	// grader 靠它识别伪造引用；span 空表示期望无引用。
	if len(t.ExpectDocs) == 0 {
		return errors.New("expect_docs must not be empty (out-of-corpus 题也须标注文档 id，span 空表示期望无引用)")
	}
	for _, s := range t.ExpectSpans {
		// [start,end) 含头不含尾：start==end 是零长区间，倒挂更是无意义，
		// 两者都会让交并比退化（甚至 NaN），必须在入口挡住。
		if s[0] < 0 {
			return fmt.Errorf("expect_spans start must be >= 0, got %v", s)
		}
		if s[0] >= s[1] {
			return fmt.Errorf("expect_spans must be [start,end) with start < end, got %v", s)
		}
	}
	return nil
}
