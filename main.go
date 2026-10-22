package main

import (
	"agent-api/api"
	"agent-api/config"
	"agent-api/llm"
	"agent-api/metrics"
	"agent-api/store"
	"agent-api/worker"
	"context"
	"errors"
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

	// 配置来源：环境变量优先，其次 config.yaml（文件可选）。
	conf, err := config.Load("")
	if err != nil {
		slog.Error("load config", slog.Any("error", err))
		return 1
	}
	apiKey := conf.DeepSeekAPIKey
	if apiKey == "" {
		slog.Error("DEEPSEEK_API_KEY is not set（环境变量或 config.yaml 任选其一）")
		return 1
	}

	cfg := llm.Config{
		APIKey:  apiKey,
		BaseURL: "https://api.deepseek.com",
		Model:   "deepseek-chat",
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
