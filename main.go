package main

import (
	"agent-runtime/api"
	"agent-runtime/config"
	"agent-runtime/llm"
	"agent-runtime/metrics"
	"agent-runtime/retrieval"
	"agent-runtime/store"
	"agent-runtime/worker"
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	os.Exit(run())
}

func run() int {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	// flag 先行解析：-index 子命令在任何重装配（LLM/store/pool/retrieval）之前短路退出。
	// 用独立 FlagSet + ContinueOnError：run() 返回 int 由 main 统一 os.Exit，
	// 不走 flag 包默认的 os.Exit，保持退出路径单一、可测。
	fs := flag.NewFlagSet("agent-runtime", flag.ContinueOnError)
	var indexDir, corpusDir string
	fs.StringVar(&indexDir, "index", "", "构建检索索引后退出：-index <语料目录>")
	fs.StringVar(&corpusDir, "corpus", "docs", "检索语料目录（search_docs 的新鲜度检查点）")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2 // flag 包已把用法错误写到 stderr
	}
	indexRequested := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "index" {
			indexRequested = true
		}
	})

	// 配置来源：环境变量优先，其次 config.yaml（文件可选）。
	conf, err := config.Load("")
	if err != nil {
		slog.Error("load config", slog.Any("error", err))
		return 1
	}

	// -index 子命令：建库 + 结构化摘要 + 退出，不启动服务、不校验 LLM 密钥。
	if indexRequested {
		if indexDir == "" {
			slog.Error("-index 需要语料目录参数，用法：go run . -index <语料目录>")
			return 2
		}
		return runIndexCmd(conf, indexDir)
	}

	apiKey := conf.DeepSeekAPIKey
	if apiKey == "" {
		slog.Error("DEEPSEEK_API_KEY is not set（环境变量或 config.yaml 任选其一）")
		return 1
	}

	cfg := llm.Config{
		APIKey:       apiKey,
		BaseURL:      "https://api.deepseek.com",
		Model:        "deepseek-flash",
		EmbedBaseURL: conf.EmbedBaseURL,
		EmbedAPIKey:  conf.EmbedAPIKey,
		EmbedModel:   conf.EmbedModel,
		EmbedDim:     conf.EmbedDim,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 检索索引与 store 共享 db_path 文件（DbPath 空则 ":memory:"，与内存 store 语义一致）：
	// 两者分持连接、WAL 下读写互不阻塞。Open 失败 = 启动失败（显式失败不静默）。
	idx, err := retrieval.Open(indexPath(conf), retrievalOptions(conf))
	if err != nil {
		slog.Error("open retrieval index", slog.Any("error", err))
		return 1
	}
	defer func() {
		if idx != nil {
			_ = idx.Close()
		}
	}()

	// 启动时新鲜度检查仅提示不阻断（ADR-0017 决策 6）：语料未建/已改是首次
	// search_docs 调用时才需显式失败的运维问题，不该阻止服务起来。
	if err := idx.CheckFresh(ctx, corpusDir); err != nil {
		slog.Warn("retrieval index not fresh at startup", "corpus", corpusDir, "error", err)
	}

	var backend store.TaskStore
	if conf.DbPath != "" {
		sqlStore, err := store.NewSQLStore(conf.DbPath)
		if err != nil {
			slog.Error("open sqlite store", slog.Any("error", err))
			return 1
		}
		defer sqlStore.Close()
		backend = sqlStore
	} else {
		backend = store.NewStore()
	}
	s := backend

	// 单次任务 LLM 调用上限：可配置，缺省 30s（远比原 60s 短，避免慢上游长期占住 worker）。
	taskTimeout := time.Duration(conf.TaskTimeoutSeconds) * time.Second
	if taskTimeout <= 0 {
		taskTimeout = 30 * time.Second
	}
	cfg.HTTPTimeout = taskTimeout
	p := worker.NewPool(3, taskTimeout, s, llm.NewClient(cfg))
	// search_docs 进 pool：必须在 Start 前注册，worker 消费任务时才看得到工具 spec。
	p.RegisterTool(retrieval.SearchTool(idx, corpusDir))

	// 启动补偿（Crash-Recovery / Rehydration）：先把上次进程残留的状态机修复完，
	// 再开始消费与对外提供服务，消除「幽灵 running」与「遗失 pending」。
	pendingIDs, recoveredRunning, err := s.RecoverTasks(ctx)
	if err != nil {
		slog.Error("recover tasks", "error", err)
		return 1
	}
	if recoveredRunning > 0 {
		slog.Warn("recovered interrupted tasks marked failed", "count", recoveredRunning)
	}

	p.Start()
	// pending 可能超过队列容量，阻塞式重灌放到后台，不阻塞 HTTP 监听启动。
	enqueueRecovered(ctx, p, pendingIDs)

	h := api.NewHandler(s, p, conf.APIAuthKey)
	mux := h.Routes()

	srv := newServer(mux)
	errCh := make(chan error, 1) // 缓冲 1，goroutine 送完就走，不会泄露

	go func() {
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		// 收到信号，正常停机
		return shutdown(srv, p)

	case err := <-errCh:
		// 启动失败，也要走停机流程，但退出码要非 0
		slog.Error("server error", "error", err)
		_ = p.Stop()
		return 1
	}
}

// enqueueRecovered 把崩溃恢复得到的 pending 任务重新灌入 worker 队列。
// 使用 EnqueueWait 而不是 Enqueue：积压超过队列容量时等待空位，而不是
// 静默丢弃（那会再次制造掉电丢任务）。若停机或 ctx 取消则中止，剩余任务
// 仍为 pending，下次启动会再次被恢复。
func enqueueRecovered(ctx context.Context, p *worker.Pool, ids []string) {
	if len(ids) == 0 {
		return
	}
	// 这些任务在上个进程已提交过：只恢复在飞 gauge，不重复计 submitted。
	metrics.IncRecovered(len(ids))
	go func() {
		for _, id := range ids {
			if err := p.EnqueueWait(ctx, id); err != nil {
				slog.Error("rehydrate: enqueue pending task failed", "task_id", id, "error", err)
				return
			}
		}
		slog.Info("rehydrate: pending tasks requeued", "count", len(ids))
	}()
}

// newServer 构造 HTTP 服务器并配置超时，避免慢客户端长期占用连接：
// ReadHeaderTimeout 限制请求头读取，WriteTimeout 限制响应写入，
// IdleTimeout 限制 keep-alive 空闲连接存活时间。
func newServer(handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              ":8080",
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func shutdown(srv *http.Server, p *worker.Pool) int {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	slog.Info("shutting down server")
	exitCode := 0
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server shutdown error", "error", err)
		exitCode = 1
	}

	// 停机预算完整传递给 worker pool，允许在飞任务跑完剩余时间
	if err := p.Stop(shutdownCtx); err != nil {
		slog.Error("worker pool shutdown error", "error", err)
		exitCode = 1
	}
	return exitCode
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
