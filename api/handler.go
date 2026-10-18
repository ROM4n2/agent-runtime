// Package api 提供任务提交与查询的 HTTP 接口。
//
// 这一层只做「解析请求 → 落库 → 入队 → 立刻应答」，不执行任何任务逻辑；
// 真正的执行在 worker 包中异步进行。
package api

import (
	"agent-api/agent"
	"agent-api/metrics"
	"agent-api/store"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

// TaskRunner 定义 API 层所需的任务执行器接口（消费者定义接口）。
type TaskRunner interface {
	Enqueue(id string) error
	Trace(id string) []agent.TraceStep
}

// Handler 持有处理请求所需的依赖，由 main 装配后注入。
type Handler struct {
	store  store.TaskStore
	runner TaskRunner
	// authKey 由 main 从 config 注入，不在 api 层读环境变量：
	// 配置来源只有一个（main），测试才能直接注入不同密钥。
	authKey string
}

func NewHandler(s store.TaskStore, r TaskRunner, authKey string) *Handler {
	return &Handler{store: s, runner: r, authKey: authKey}
}

// Routes 返回已注册全部路由的 mux。
//
// 用的是 Go 1.22+ ServeMux 的方法路由语法："METHOD /path"，
// 方法与路径一起参与匹配，因此不必在 handler 内部自己判断 r.Method。
//
// 每个路由都经过同一中间件链：Recover（防 panic 拖垮进程）→
// RequestID（日志串联）→ Limit（按 IP 限流护住 LLM 配额）→
// Auth（Bearer 鉴权，生产必须设 API_AUTH_KEY）。
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()

	// 限流器：每秒 20 请求、突发 40，足以覆盖正常轮询又挡住刷量。
	limiter := newRateLimiter(20, 40)
	auth := Auth(h.authKey)

	wrap := func(hh http.Handler) http.Handler {
		return Recover(RequestID(limiter.Limit(auth(hh))))
	}

	// 探针与指标：只包 Recover+RequestID，不走 Limit/Auth，
	// 确保 LB 探活与指标抓取永远可达；/metrics 仅暴露聚合计数，无敏感数据。
	mux.Handle("GET /healthz", Recover(RequestID(http.HandlerFunc(healthz))))
	mux.Handle("GET /metrics", Recover(RequestID(metrics.Handler())))
	// 单页 Demo：GET / 返回内嵌的 demo.html；不走 Auth 以便直接打开，
	// 页面自身带可选 token 输入框以适配生产鉴权模式。
	mux.Handle("GET /", Recover(RequestID(http.HandlerFunc(serveDemo))))

	mux.Handle("POST /run", wrap(http.HandlerFunc(h.HandleRun)))
	// 路径是复数 tasks，与 HandleGet 的取值 r.PathValue("id") 配套
	mux.Handle("GET /tasks/{id}", wrap(http.HandlerFunc(h.HandleGet)))
	return mux
}

// healthz 是存活探针：进程能响应即视为活着，不涉及任务状态，也不鉴权，
// 因此不被 Limit/Auth 包裹，避免探针因限流或缺少 token 而误报不健康。
func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

// HandleRun 接收任务并立即返回 task_id，不等待执行结果。
//
// 返回 202 而非 200：任务此刻尚未开始执行，200 的语义是「已处理完成」，
// 用它会误导调用方。调用方应拿 task_id 轮询 GET /tasks/{id}。
func (h *Handler) HandleRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt string
	}
	// 限制请求体大小，避免客户端发大文件导致内存耗尽。
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if req.Prompt == "" {
		http.Error(w, "prompt is empty", http.StatusBadRequest)
		return
	}

	// 先落库再入队：Enqueue 之后 worker 可能立刻开始处理，
	// 此时该 ID 必须已经存在于 store 中。
	id := h.store.Create(req.Prompt)
	if err := h.runner.Enqueue(id); err != nil {
		if delErr := h.store.Delete(id); delErr != nil {
			slog.Error("reclaim task on enqueue failure", "task_id", id, "error", delErr)
		}
		// 队列满：显式 503，并下发 Retry-After 头指示客户端退避 1 秒
		w.Header().Set("Retry-After", "1")
		http.Error(w, "service unavailable: task queue is full", http.StatusServiceUnavailable)
		return
	}
	metrics.IncSubmitted() // 提交即计数，同时在飞数 +1

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	err = json.NewEncoder(w).Encode(map[string]string{"task_id": id})

	// 响应头已写出，此时无法再改状态码，编码失败只能记日志。
	// 且这类失败基本只源于客户端断连，重试没有意义。
	if err != nil {
		slog.Error("encode response", "error", err)
	}

}

type taskWithTrace struct {
	store.Task
	Steps []agent.TraceStep `json:"steps"`
}

// HandleGet 返回任务当前状态，供调用方轮询。
func (h *Handler) HandleGet(w http.ResponseWriter, r *http.Request) {
	// {id} 由 mux 从路径中解析，不是查询参数
	id := r.PathValue("id")

	task, err := h.store.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	steps := []agent.TraceStep{}
	if h.runner != nil {
		if s := h.runner.Trace(id); s != nil {
			steps = s
		}
	}
	resp := taskWithTrace{
		Task:  task,
		Steps: steps,
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	err = json.NewEncoder(w).Encode(resp)
	if err != nil {
		slog.Error("encode response", "error", err)
	}
}
