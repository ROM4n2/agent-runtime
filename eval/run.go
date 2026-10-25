package eval

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent-runtime/agent"
	"agent-runtime/agent/tool"
	"agent-runtime/llm"
	"agent-runtime/prompts"
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
			TokenCounting:   tokenCountingState(cfg.TokenCounter, results),
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
	if _, err := prompts.Get(cfg.TemplateVersion); err != nil {
		return fmt.Errorf("eval: invalid TemplateVersion %q: %w", cfg.TemplateVersion, err)
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
	zero := 0.0
	cfg.LLMConfig.Temperature = &zero
	seed := cfg.Seed
	cfg.LLMConfig.Seed = &seed
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

// runOnce 执行一次（任务 × repeat）：跑 agent.RunWithOptions 全链路 → grader 判定 → 明细。
func runOnce(ctx context.Context, cfg RunConfig, chatter agent.Chatter, reg *tool.Registry, task Task) (TaskResult, error) {
	start := time.Now()
	buf := agent.NewTraceBuffer(0)
	// 判定数据源优先取工具返回全文；collector 无数据时才退回 trace（见下方 Grade）。
	collector := &toolOutputCollector{}
	sysPrompt, _ := prompts.Get(cfg.TemplateVersion)
	var promptTokens, completionTokens int
	onUsage := func(u llm.Usage) {
		promptTokens += u.PromptTokens
		completionTokens += u.CompletionTokens
	}
	answer, runErr := agent.RunWithOptions(ctx, chatter, task.Prompt, registryWithCollector(reg, collector), buf.Append, agent.RunOptions{
		SystemPrompt: sysPrompt,
		OnUsage:      onUsage,
	})
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
	var resPromptTokens, resCompletionTokens int
	if cfg.TokenCounter != nil {
		resPromptTokens = countTokens(cfg.TokenCounter, task.Prompt, "")
		resCompletionTokens = countTokens(cfg.TokenCounter, "", answer)
	} else if promptTokens > 0 || completionTokens > 0 {
		resPromptTokens = promptTokens
		resCompletionTokens = completionTokens
	}
	return TaskResult{
		TaskID:           task.ID,
		Scope:            task.Scope,
		Success:          g.Success,
		SpanRecall:       g.SpanRecall,
		FirstError:       g.FirstError,
		Steps:            len(trace),
		LatencyMS:        latencyMS,
		PromptTokens:     resPromptTokens,
		CompletionTokens: resCompletionTokens,
		CitedSpans:       g.CitedSpans,
		CitedDocs:        g.CitedDocs,
	}, nil
}

