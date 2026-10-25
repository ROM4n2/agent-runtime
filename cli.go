package main

import (
	"agent-runtime/config"
	"agent-runtime/eval"
	"agent-runtime/llm"
	"agent-runtime/retrieval"
	"context"
	"errors"
	"flag"
	"log/slog"
)

// cliFlags 收敛命令行解析结果：parseFlags 只解析，runWithArgs 只执行。
type cliFlags struct {
	indexDir       string
	corpusDir      string
	indexRequested bool // -index 是否显式传入（默认值无法区分「没写」与「写了空值」）
	evalRequested  bool
	corpusExplicit bool // -corpus 是否显式传入（决定 -eval 分支是否覆写语料口径）
	helpRequested  bool // -h/-help 已打印用法：退出码 0，但不进入任何执行路径
	eval           evalOpts
}

// parseFlags 解析命令行：退出码 0 = 解析成功或帮助已打印（-h/-help），
// 2 = 用法错误（未知 flag 或 flag 缺值）。
// 用独立 FlagSet + ContinueOnError：不走 flag 包默认的 os.Exit，
// 退出路径单一地由 main → run → runWithArgs 收敛。
func parseFlags(args []string) (cliFlags, int) {
	var f cliFlags
	fs := flag.NewFlagSet("agent-runtime", flag.ContinueOnError)
	fs.StringVar(&f.indexDir, "index", "", "构建检索索引后退出：-index <语料目录>")
	fs.StringVar(&f.corpusDir, "corpus", "docs", "检索语料目录（search_docs 的新鲜度检查点）")
	// -eval 子命令族（ADR-0018 决策 1：一条命令跑完整对照）。与 -index 同款
	// 短路退出，不启动服务。评测语料口径是仓库根，见 evalCorpusDir。
	fs.BoolVar(&f.evalRequested, "eval", false, "跑对照评测后退出：-eval -group baseline|lexical|hybrid")
	fs.StringVar(&f.eval.set, "set", "eval/tasks/retrieval_v1.json", "评测任务集 JSON 路径（-eval）")
	fs.StringVar(&f.eval.group, "group", "", "评测组：baseline|lexical|hybrid（-eval，必填，无默认）")
	fs.IntVar(&f.eval.repeat, "repeat", 3, "每任务重复次数 N（-eval，ADR-0018 决策 3 默认 N=3）")
	fs.Int64Var(&f.eval.seed, "seed", 0, "评测随机种子（-eval，成对受控锁定量，对照组须同值）")
	fs.StringVar(&f.eval.model, "model", "deepseek-flash", "评测记录所用模型（-eval，与服务路径同值）")
	fs.StringVar(&f.eval.template, "template", "eval-template-v1", "prompt 模板版本标识（-eval，成对受控锁定量）")
	fs.StringVar(&f.eval.outDir, "out", "docs/eval", "报表输出目录（-eval）")
	if err := fs.Parse(args); err != nil {
		// -h/-help：flag 包已打印用法，帮助是「显式请求」而不是用法错误 → 退出码 0。
		// 但必须用标记短路，否则 runWithArgs 会继续 config.Load 并起服务。
		if errors.Is(err, flag.ErrHelp) {
			f.helpRequested = true
			return f, 0
		}
		return cliFlags{}, 2 // flag 包已把用法错误写到 stderr
	}
	// Visit 只遍历「显式传入」的 flag：两个布尔标记只能从这里得到。
	fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "index":
			f.indexRequested = true
		case "corpus":
			f.corpusExplicit = true
		}
	})
	return f, 0
}

// evalOpts 收敛 -eval 子命令的 flag 值：runWithArgs 只负责收集，本结构只负责
// 往 eval.RunConfig 上映射一次（RunConfig 是环境四元组的单一读取点）。
type evalOpts struct {
	set      string // 任务集 JSON 路径
	group    string // baseline | lexical | hybrid
	corpus   string // 评测语料目录（仓库根）
	repeat   int    // 每任务重复次数 N
	outDir   string // 报表输出目录
	seed     int64
	model    string
	template string
}

// evalCorpusDir 决定 -eval 分支的语料口径：评测的语料是仓库根（HITL 口径），
// 与 search_docs 的默认 "docs" 本就不同范围；只有用户显式写了 -corpus 才尊重用户值。
// 服务路径不经过这里，它的默认 "docs" 一动不动。
func evalCorpusDir(corpusDir string, explicit bool) string {
	if !explicit {
		return "."
	}
	return corpusDir
}

// indexPath 返回检索索引库路径：与 store 共享 conf.DbPath 文件；
// DbPath 空（纯内存 store）时检索同样落 ":memory:"，两者生命周期一致。
func indexPath(conf config.Config) string {
	if conf.DbPath != "" {
		return conf.DbPath
	}
	return ":memory:"
}

// retrievalOptions 从同一份 config 分送检索选项：embedder 按 conf 构造（缺凭证即 nil
// = 纯词法），EmbedModel/EmbedDim 直接透传。Task 8 装配层的单一分送点，
// 禁在调用点散落字段——改配置映射只改这里。
func retrievalOptions(conf config.Config) retrieval.Options {
	return retrieval.Options{
		Embedder:   newEmbedder(conf),
		EmbedModel: conf.EmbedModel,
		EmbedDim:   conf.EmbedDim,
	}
}

// buildIndex 打开（必要时创建）检索索引库并全量重建语料，返回构建报告。
// 供 -index 子命令与其测试共用：索引连接在本函数内开闭，报告数字即 Rebuild 出口，
// 调用方据此核对落库结果（db_path 非空则同文件，否则 ":memory:"）。
func buildIndex(conf config.Config, corpusDir string) (retrieval.BuildReport, error) {
	idx, err := retrieval.Open(indexPath(conf), retrievalOptions(conf))
	if err != nil {
		return retrieval.BuildReport{}, err
	}
	defer idx.Close()
	return idx.Rebuild(context.Background(), corpusDir)
}

// runIndexCmd 是 -index 子命令入口：构建索引 + 结构化摘要日志，返回进程退出码。
// 构建失败 → slog.Error + 退出码 1（显式失败不静默）；成功 → 摘要日志 + 退出码 0。
func runIndexCmd(conf config.Config, corpusDir string) int {
	report, err := buildIndex(conf, corpusDir)
	if err != nil {
		slog.Error("index build failed", slog.Any("error", err))
		return 1
	}
	slog.Info("index built",
		"docs", report.DocsIndexed,
		"skipped", report.DocsSkipped,
		"chunks", report.ChunksWritten,
		"fingerprint", report.Fingerprint)
	// db_path 为空时索引落 ":memory:"，进程退出即蒸发：上面的「index built」
	// 若被当成持久化成功，就是一条误导性成功报告（CRV Task 8 黄卡 4）。
	// 不改判退出码（构建本身成功），但必须把「只在内存里」显式喊出来。
	if conf.DbPath == "" {
		slog.Warn("index built in memory only; set db_path in config.yaml for persistence", "db_path", "")
	}
	return 0
}

// runEvalCmd 是 -eval 子命令入口：组装 RunConfig → eval.Run → 摘要日志。
// Chatter 留 nil（CLI 无注入点）：eval 按 LLMConfig 构造真 client，缺密钥时
// 由 resolveChatter 显式报错，不静默降级成空跑。
// 退出码语义（ADR-0018 决策 1）：0 = 跑完（不论业务结论）；1 = harness 故障。
func runEvalCmd(conf config.Config, opts evalOpts) int {
	report, err := eval.Run(context.Background(), eval.RunConfig{
		SetPath:         opts.set,
		CorpusDir:       opts.corpus,
		Group:           opts.group,
		Repeat:          opts.repeat,
		OutDir:          opts.outDir,
		Seed:            opts.seed,
		Model:           opts.model,
		TemplateVersion: opts.template,
		// 与 Task 8 装配同源：索引与 store 共享 conf.DbPath，空则 :memory:
		// （一次性索引，跑完即弃，不给仓库留半份索引文件）。
		DBPath:       conf.DbPath,
		IndexOptions: retrievalOptions(conf),
		LLMConfig: llm.Config{
			APIKey:       conf.DeepSeekAPIKey,
			BaseURL:      "https://api.deepseek.com",
			Model:        opts.model,
			EmbedBaseURL: conf.EmbedBaseURL,
			EmbedAPIKey:  conf.EmbedAPIKey,
			EmbedModel:   conf.EmbedModel,
			EmbedDim:     conf.EmbedDim,
		},
	})
	if err != nil {
		slog.Error("eval run failed", slog.Any("error", err))
		return 1
	}
	slog.Info("eval run finished",
		"group", report.Group,
		"repeat", report.Repeat,
		"tasks", len(report.TaskResults),
		"markdown", report.MarkdownPath,
		"json", report.JSONPath)
	return 0
}

// newEmbedder 按配置构造向量通道 embedder（ADR-0017 决策 3「默认关闭」）：
// EmbedBaseURL/EmbedAPIKey/EmbedModel 三者全非空才返回实现了 llm.Embedder 的
// *llm.Client（其 Embed 方法即接口实现），缺任一即返回 nil（纯词法模式，禁静默半配）。
func newEmbedder(conf config.Config) llm.Embedder {
	if conf.EmbedBaseURL == "" || conf.EmbedAPIKey == "" || conf.EmbedModel == "" {
		return nil
	}
	return llm.NewClient(llm.Config{
		APIKey:       conf.EmbedAPIKey,
		BaseURL:      conf.EmbedBaseURL,
		Model:        conf.EmbedModel,
		EmbedBaseURL: conf.EmbedBaseURL,
		EmbedAPIKey:  conf.EmbedAPIKey,
		EmbedModel:   conf.EmbedModel,
	})
}
