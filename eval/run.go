package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"agent-runtime/agent"
	"agent-runtime/agent/tool"
	"agent-runtime/llm"
	"agent-runtime/retrieval"
)

// 评测组（ADR-0018 决策 2.1 成对受控）：三组跑在同一任务集、同一模板版本、
// 同一 seed 下，每次只开/关一个自变量（检索有无 / 召回通道），指标变动
// 必须能归因到单一自变量。
const (
	GroupBaseline = "baseline" // registry 只有 calculate/current_time，无 search_docs
	GroupLexical  = "lexical"  // DefaultRegistry + SearchTool（Options{} 纯词法）
	GroupHybrid   = "hybrid"   // 带 Embedder 的 Options；无凭证显式失败，不静默降级
)

// agentMaxStepsDetail 是 agent.Run 步数耗尽时写入 trace 的 KindError detail
// （engine.go:139）。步数耗尽是业务结论（模型绕圈）而非 harness 故障：
// ADR-0018 决策 2.3 明令禁止把它当失败根因，故这类错误照常进判定、
// 不中断整轮评测；其余 agent.Run 错误（上游 LLM 故障 / ctx 取消）会污染
// 整组指标，出半成品报表比没有报表更坏，一律上浮为 harness 故障。
const agentMaxStepsDetail = "max steps exceeded"

// RunConfig 是一次评测运行的全部输入。环境四元组
// （Seed/Model/TemplateVersion/CorpusDir）是 ADR-0018 成对受控的锁定参数，
// 由本结构单处读取并原样进报表——禁止在调用点散落读取，否则三组环境漂移
// 而无处对账。
type RunConfig struct {
	SetPath         string // 任务集 JSON 文件路径
	CorpusDir       string // 检索语料目录（lexical/hybrid 组的新鲜度检查点）
	Group           string // baseline | lexical | hybrid
	Repeat          int    // 每任务重复次数 N（ADR-0018 决策 3）
	OutDir          string // 报表输出目录；空则只返回 Report 不落盘
	Seed            int64
	Model           string
	TemplateVersion string

	// DBPath 是检索索引库文件路径；空则 ":memory:"（一次性索引，跑完即弃）。
	// 与 Task 8 装配同源：main 的 -eval 分支传 conf.DbPath，测试传临时文件。
	DBPath string
	// IndexOptions 供 hybrid 组打开索引（Embedder + EmbedDim + EmbedModel）。
	// lexical 组强制纯词法（Options{}），baseline 组不建索引。
	IndexOptions retrieval.Options
	// Chatter 是 LLM 注入点：测试与降本跑法注入假件，nil 时按 LLMConfig
	// 构造真 client（agent.Chatter 的 DIP 用途，同 worker.NewPool）。
	Chatter agent.Chatter
	// LLMConfig 是 Chatter 为 nil 时构造真 llm.Client 的配置（装配层单一分送点）。
	LLMConfig llm.Config
	// TokenCounter 估算 (prompt, completion) 的 token 数。llm.Client 当前
	// 不返回 usage，故为可选注入：nil 时明细记 0 且报表显式标注
	// 「token 计数未接入」——禁静默填一个看起来正常的数字。
	// 调用约定：harness 分两侧各调一次，prompt 侧传 (prompt, "")、
	// completion 侧传 ("", answer)，实现忽略空侧即可。
	TokenCounter func(prompt, completion string) int
}

// TaskResult 是单次运行（一个任务 × 一次 repeat）的明细。
// 单次值只进明细、不进聚合（ADR-0018 决策 3）：聚合必出分布。
type TaskResult struct {
	TaskID           string
	Scope            string
	Success          bool
	SpanRecall       float64
	FirstError       string
	Steps            int
	LatencyMS        int64
	PromptTokens     int
	CompletionTokens int
	// CitedSpans / CitedDocs 是本次判定实际使用的引用快照（grader 原样带出，
	// 下标同源）。明细表「命中 span」列的数据源（ADR-0018 决策 4）。
	CitedSpans [][2]int
	CitedDocs  []string
}

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

// Report 是一组评测的完整产物：逐任务明细 + 组级分布 + 环境。
// 明细表是审计与复跑的锚，禁止只留汇总数字（ADR-0018 决策 4）。
type Report struct {
	Group       string
	Repeat      int
	TaskResults []TaskResult
	Aggregates  map[string]any
	Env         ReportEnv
	// MarkdownPath / JSONPath 是 Run 落盘后的产物路径（OutDir 为空时为空串），
	// 供装配层直接打日志，避免调用方再猜一遍文件名规则。
	MarkdownPath string
	JSONPath     string
}

// Run 跑一组评测：任务集加载 → 按组构造工具面 → 索引新鲜度预检 →
// 每任务 × repeat 过 agent.Run + grader → 聚合 → 写报表。
//
// 返回 error 只表示 harness 自身故障（任务集非法、语料目录不存在、
// hybrid 无凭证、索引不可用、上游 LLM 故障）；业务结论（检索有没有帮助）
// 只在报表里，退出码不承载（ADR-0018 决策 1/4）。
func Run(ctx context.Context, cfg RunConfig) (Report, error) {
	if err := cfg.validate(); err != nil {
		return Report{}, err
	}
	tasks, err := LoadTasks(cfg.SetPath)
	if err != nil {
		return Report{}, err
	}
	setHash, err := fileSHA256(cfg.SetPath)
	if err != nil {
		return Report{}, err
	}
	// 语料目录存在性早失败：别等索引构建到一半才报，更别拿空语料跑报表。
	if err := checkCorpusDir(cfg.CorpusDir); err != nil {
		return Report{}, err
	}

	reg, idx, err := buildRegistry(ctx, cfg)
	if err != nil {
		return Report{}, err
	}
	if idx != nil {
		defer func() { _ = idx.Close() }()
	}

	chatter, err := cfg.resolveChatter()
	if err != nil {
		return Report{}, err
	}

	results := make([]TaskResult, 0, len(tasks)*cfg.Repeat)
	for _, task := range tasks {
		for i := 0; i < cfg.Repeat; i++ {
			tr, err := runOnce(ctx, cfg, chatter, reg, task)
			if err != nil {
				return Report{}, fmt.Errorf("eval: task %s repeat %d/%d: %w", task.ID, i+1, cfg.Repeat, err)
			}
			results = append(results, tr)
		}
	}

	report := Report{
		Group:       cfg.Group,
		Repeat:      cfg.Repeat,
		TaskResults: results,
		Aggregates:  aggregate(tasks, results),
		Env: ReportEnv{
			Group:           cfg.Group,
			Seed:            cfg.Seed,
			Model:           cfg.Model,
			TemplateVersion: cfg.TemplateVersion,
			CorpusDir:       cfg.CorpusDir,
			Repeat:          cfg.Repeat,
			DBPath:          dbPathOrDefault(cfg.DBPath),
			SetPath:         cfg.SetPath,
			SetHash:         setHash,
			TokenCounting:   tokenCountingState(cfg.TokenCounter),
		},
	}
	if cfg.OutDir != "" {
		mdPath, err := report.WriteMarkdown(cfg.OutDir)
		if err != nil {
			return Report{}, err
		}
		jsonPath, err := report.WriteJSON(cfg.OutDir)
		if err != nil {
			return Report{}, err
		}
		report.MarkdownPath, report.JSONPath = mdPath, jsonPath
	}
	return report, nil
}

// validate 校验入参。一律显式报错不做自动修正：静默补默认值会让
// 「跑的是哪一组、重复几次」变得不可对账。
func (cfg RunConfig) validate() error {
	if cfg.SetPath == "" {
		return errors.New("eval: SetPath is required")
	}
	switch cfg.Group {
	case GroupBaseline, GroupLexical, GroupHybrid:
	default:
		return fmt.Errorf("eval: group must be one of %s/%s/%s, got %q",
			GroupBaseline, GroupLexical, GroupHybrid, cfg.Group)
	}
	if cfg.Repeat < 1 {
		return fmt.Errorf("eval: repeat must be >= 1, got %d", cfg.Repeat)
	}
	if cfg.CorpusDir == "" {
		return errors.New("eval: CorpusDir is required")
	}
	return nil
}

// resolveChatter 决定用哪个 LLM：注入优先，nil 时按 LLMConfig 构造真 client。
// 两者皆空（无注入又无凭证）必须早失败——否则要等第一轮任务超时才知道
// 根本没配密钥，白跑一趟还留一份空报表。
func (cfg RunConfig) resolveChatter() (agent.Chatter, error) {
	if cfg.Chatter != nil {
		return cfg.Chatter, nil
	}
	if cfg.LLMConfig.APIKey == "" {
		return nil, errors.New("eval: no Chatter injected and LLMConfig.APIKey is empty (real LLM run requires credentials)")
	}
	return llm.NewClient(cfg.LLMConfig), nil
}

// buildRegistry 按组构造工具面并预检索引（ADR-0018 决策 2.1 的单变量开关）：
//
//	baseline：tool.DefaultRegistry()（calculate/current_time），无 search_docs；
//	lexical ：DefaultRegistry + SearchTool，Options{} 纯词法；
//	hybrid  ：带 Embedder 的 Options，无凭证即显式错误（禁静默降级到词法）。
//
// 索引新鲜度也在这里预检：未建/陈旧一律自动 Rebuild——评测必须用新索引，
// staleness 是 harness 故障而不是业务结论（拿旧索引跑出的数字不可归因）。
func buildRegistry(ctx context.Context, cfg RunConfig) (*tool.Registry, *retrieval.Index, error) {
	reg := tool.DefaultRegistry()
	if cfg.Group == GroupBaseline {
		return reg, nil, nil
	}
	opts := retrieval.Options{}
	if cfg.Group == GroupHybrid {
		if cfg.IndexOptions.Embedder == nil || cfg.IndexOptions.EmbedDim <= 0 {
			return nil, nil, errors.New("eval: hybrid group requires embedder credentials")
		}
		opts = cfg.IndexOptions
	}
	idx, err := retrieval.Open(dbPathOrDefault(cfg.DBPath), opts)
	if err != nil {
		return nil, nil, fmt.Errorf("eval: open index %q: %w", dbPathOrDefault(cfg.DBPath), err)
	}
	if err := ensureFreshIndex(ctx, idx, cfg.CorpusDir); err != nil {
		_ = idx.Close()
		return nil, nil, err
	}
	reg.Register(retrieval.SearchTool(idx, cfg.CorpusDir))
	return reg, idx, nil
}

// ensureFreshIndex 预检索引新鲜度：未建（ErrIndexMissing）或陈旧
// （ErrCorpusStale）即自动 Rebuild。其余错误（如指纹计算失败）上浮——
// 那不是「重建一下就好」的问题。
func ensureFreshIndex(ctx context.Context, idx *retrieval.Index, corpusDir string) error {
	err := idx.CheckFresh(ctx, corpusDir)
	if err == nil {
		return nil
	}
	if !errors.Is(err, retrieval.ErrIndexMissing) && !errors.Is(err, retrieval.ErrCorpusStale) {
		return fmt.Errorf("eval: check index freshness: %w", err)
	}
	if _, err := idx.Rebuild(ctx, corpusDir); err != nil {
		return fmt.Errorf("eval: rebuild index for corpus %q: %w", corpusDir, err)
	}
	return nil
}

// toolOutputCollector 收集本次运行里 search_docs 的成功返回全文（缺陷 G 的修复点）。
//
// trace 的 tool_result detail 被 agent.MaxTraceDetail=512 截断——那是有界运行时
// 观测的正确设计，不改；但评测是离线分析，判定必须拿工具返回原文，
// 否则长 text 后面的引用被静默丢掉（实测 q009 完整 recall 0.640 → 0.091）。
type toolOutputCollector struct {
	mu      sync.Mutex
	outputs []string
}

// record 追加一条成功返回串。任务串行跑，但 Run 的调用方将来可能并发，
// 加锁的代价远低于「某天并发跑就数据竞争」的风险。
func (c *toolOutputCollector) record(out string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outputs = append(c.outputs, out)
}

// hits 把已记录的返回串解析成引用集合（多轮调用的结果合并计分）。
// ok=false 表示本次运行没有任何成功返回（工具未调用 / 全部失败），
// 调用方须退回 trace 路径——那是兜底，不是另一套判定。
func (c *toolOutputCollector) hits() ([]toolHit, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.outputs) == 0 {
		return nil, false
	}
	hits := make([]toolHit, 0, len(c.outputs))
	for _, out := range c.outputs {
		hits = append(hits, parseHits(out)...)
	}
	return hits, true
}

// collectingTool 是 tool.Tool 的 decorator：Spec/转发原样，只在 Execute 成功时
// 把完整返回串记进 collector。失败路径不记：错误串里没有可判定的引用。
// 内嵌 tool.Tool 让 Spec() 直接复用被包装工具的实现，避免两处描述漂移。
type collectingTool struct {
	tool.Tool
	sink *toolOutputCollector
}

func (t *collectingTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	out, err := t.Tool.Execute(ctx, args)
	if err != nil {
		return out, err
	}
	t.sink.record(out)
	return out, nil
}

// registryWithCollector 返回本次运行专用的注册表副本：search_docs 换成
// collectingTool，其余工具原样复用。注册表本身在 Run 里只建一次
// （带索引重建，代价高），故按运行复制而不是按运行重建工具面——
// 工具面对模型必须与组设定完全一致，副本只换执行包装、不换 Spec。
func registryWithCollector(reg *tool.Registry, sink *toolOutputCollector) *tool.Registry {
	out := tool.NewRegistry()
	for _, spec := range reg.Specs() {
		name := spec.Function.Name
		t, ok := reg.Get(name)
		if !ok {
			continue
		}
		if name == searchToolName {
			t = &collectingTool{Tool: t, sink: sink}
		}
		out.Register(t)
	}
	return out
}

// runOnce 执行一次（任务 × repeat）：跑 agent.Run 全链路 → grader 判定 → 明细。
func runOnce(ctx context.Context, cfg RunConfig, chatter agent.Chatter, reg *tool.Registry, task Task) (TaskResult, error) {
	start := time.Now()
	buf := agent.NewTraceBuffer(0)
	// 判定数据源优先取工具返回全文；collector 无数据时才退回 trace（见下方 Grade）。
	collector := &toolOutputCollector{}
	answer, runErr := agent.Run(ctx, chatter, task.Prompt, registryWithCollector(reg, collector), buf.Append)
	latencyMS := time.Since(start).Milliseconds()
	trace := buf.Snapshot()

	// 步数耗尽是业务结论（模型绕圈），照常判定；其余错误是 harness/上游故障，
	// 继续跑只会用坏数据填满中位数，故中断本轮评测。
	if runErr != nil && !traceHasMaxSteps(trace) {
		return TaskResult{}, fmt.Errorf("agent.Run: %w", runErr)
	}
	g := Grade(task, trace, answer)
	if hits, ok := collector.hits(); ok {
		g = GradeWithHits(task, hits, answer)
	}
	return TaskResult{
		TaskID:           task.ID,
		Scope:            task.Scope,
		Success:          g.Success,
		SpanRecall:       g.SpanRecall,
		FirstError:       g.FirstError,
		Steps:            len(trace),
		LatencyMS:        latencyMS,
		PromptTokens:     countTokens(cfg.TokenCounter, task.Prompt, ""),
		CompletionTokens: countTokens(cfg.TokenCounter, "", answer),
		CitedSpans:       g.CitedSpans,
		CitedDocs:        g.CitedDocs,
	}, nil
}

// traceHasMaxSteps 判断 trace 是否以步数耗尽收尾（engine.go:139 的 KindError）。
func traceHasMaxSteps(trace []agent.TraceStep) bool {
	for _, s := range trace {
		if s.Kind == agent.KindError && s.Detail == agentMaxStepsDetail {
			return true
		}
	}
	return false
}

// countTokens 估算单侧 token 数。nil（未接入）记 0：口径由报表显式标注，
// 不静默填一个看起来正常的数字。
func countTokens(fn func(prompt, completion string) int, prompt, completion string) int {
	if fn == nil {
		return 0
	}
	return fn(prompt, completion)
}

// tokenCountingState 把「计数函数在不在」翻译成报表用的口径标注。
func tokenCountingState(fn func(prompt, completion string) int) string {
	if fn == nil {
		return "not-wired"
	}
	return "injected"
}

// checkCorpusDir 校验语料目录存在且是目录。
func checkCorpusDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("eval: corpus dir %q: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("eval: corpus path %q is not a directory", dir)
	}
	return nil
}

// fileSHA256 对任务集文件算内容哈希：同 hash = 同题，是复跑与审计的对账锚。
func fileSHA256(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("eval: hash task set %s: %w", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// dbPathOrDefault 空 DBPath 落 ":memory:"（与 store 的内存语义一致）：
// 一次性索引跑完即弃，不留文件。
func dbPathOrDefault(p string) string {
	if p == "" {
		return ":memory:"
	}
	return p
}
