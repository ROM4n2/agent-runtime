package worker

import (
	"agent-runtime/agent"
	"agent-runtime/agent/tool"
	"agent-runtime/metrics"
	"agent-runtime/store"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrQueueFull 表示任务队列已满（所有 worker 忙且缓冲占满），Enqueue 因此快速失败。
var ErrQueueFull = errors.New("task queue is full")

// ErrTaskNotRunning 表示取消请求未命中在飞任务（任务未开始、已终态或不存在）。
var ErrTaskNotRunning = errors.New("task is not running")

// DefaultMaxTraces 限制 Pool 维护的最大 Trace 数量，超出时 FIFO 淘汰。
const DefaultMaxTraces = 1000

// Pool 用固定数量的 worker goroutine 消费任务队列，
// 以此限制同时在飞的 LLM 请求数——上游的限流和计费按并发量算。
type Pool struct {
	// 缓冲区大小等于 worker 数：满了 Enqueue 会阻塞调用方（HTTP handler），
	// 使异步退化为同步。生产上应改为 select+default 快速失败或换持久化队列。
	queue       chan string
	tasks       store.TaskStore
	size        int
	taskTimeout time.Duration // 单次任务 LLM 调用上限，超时即失败
	// ctx/cancel 是关闭信号的广播机制：Stop 一次 cancel，
	// 所有 worker 与派生出去的请求 ctx 同时收到通知。
	// 用 cancel 而非 close(queue)，避免向已关闭 channel 发送导致 panic。
	ctx              context.Context
	cancel           context.CancelFunc
	stopWorkerCtx    context.Context
	stopWorkerCancel context.CancelFunc
	stopping         atomic.Bool
	// started 标记 worker 是否已 Start：RegisterTool 在 Start 后注入时，
	// 新工具只对后续任务可见（在飞任务已固定 tools 快照），需显式告警而非静默。
	started    atomic.Bool
	wg         sync.WaitGroup
	llm        agent.Chatter
	tools      *tool.Registry
	tracesMu   sync.Mutex
	traces     map[string]*agent.TraceBuffer
	maxTraces  int
	traceOrder []string
	// inFlight 保存每个在飞任务的取消句柄（taskID -> context.CancelCauseFunc），
	// 支撑外部 POST /tasks/{id}/cancel 主动抢占：直接物理中断上游 HTTP 调用。
	inFlight sync.Map
}

func NewPool(size int, taskTimeout time.Duration, s store.TaskStore, client agent.Chatter) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	stopWorkerCtx, stopWorkerCancel := context.WithCancel(context.Background())

	p := Pool{
		size:             size,
		taskTimeout:      taskTimeout,
		tasks:            s,
		queue:            make(chan string, size),
		ctx:              ctx,
		cancel:           cancel,
		stopWorkerCtx:    stopWorkerCtx,
		stopWorkerCancel: stopWorkerCancel,
		llm:              client,
		tools:            tool.DefaultRegistry(),
		traces:           make(map[string]*agent.TraceBuffer),
		maxTraces:        DefaultMaxTraces,
		traceOrder:       make([]string, 0, 64),
	}
	return &p
}

func (p *Pool) Start() {
	p.started.Store(true)
	for i := 0; i < p.size; i++ {
		p.wg.Add(1)
		go p.worker()
	}
}

// RegisterTool 在 Pool 的工具注册表里追加一个工具（如 retrieval.SearchTool 产出的
// search_docs）。幂等：同名工具经 Registry 的 map 覆盖，重复注册不增生 spec。
// 经 Registry 内部锁保护，Start 前后调用均安全；但装配约定是 Start 之前——
// Start 后 worker 已持 tools 快照跑在飞任务，新工具只对后续任务生效，此时仅告警不 panic。
func (p *Pool) RegisterTool(t tool.Tool) {
	if p.started.Load() {
		slog.Warn("worker: RegisterTool after Start; tool applies to subsequent tasks only",
			"tool", t.Spec().Function.Name)
	}
	p.tools.Register(t)
}

func (p *Pool) Stop(ctxs ...context.Context) error {
	var ctx context.Context
	if len(ctxs) > 0 && ctxs[0] != nil {
		ctx = ctxs[0]
	} else {
		ctx = context.Background()
	}
	p.stopping.Store(true)
	p.stopWorkerCancel() // 通知空闲 worker 退出循环，正在 process 的 worker 会跑完当前任务

	// 等待所有 worker 退出（或 ctx 超时）
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	var waitErr error
	select {
	case <-done:
	case <-ctx.Done():
		waitErr = ctx.Err()
		p.cancel() // 停机预算耗尽，强制取消仍在飞的任务
		<-done     // 等待 worker 收到 cancel 后退出
	}

	// 排空队列中残留未消费的任务，标定明确终态，消除 pending 僵尸任务
	for {
		select {
		case id := <-p.queue:
			metrics.IncFailed()
			if err := p.tasks.Complete(id, "", errors.New("server shutdown")); err != nil {
				slog.Error("worker: complete drained task failed", "task_id", id, "error", err)
			}
		default:
			return waitErr
		}
	}
}

// worker 是常驻 goroutine：循环抢队列里的任务，直到收到关闭信号。
// wg.Done 用 defer，保证 panic 退出时也不会让 Stop 永久阻塞在 Wait。
func (p *Pool) worker() {
	defer p.wg.Done()
	for {
		select {
		case <-p.stopWorkerCtx.Done():
			return
		default:
		}

		select {
		case id := <-p.queue:
			p.process(id)

		case <-p.stopWorkerCtx.Done():
			return
		}
	}
}

// process 执行单个任务：取 prompt → 调 LLM → 写回结果。
// 任何错误都在这里终结——worker 是 goroutine，没有调用方可以把 error 返回给谁。
func (p *Pool) process(id string) {
	// 先置 running，让轮询方立刻看到状态推进
	if err := p.tasks.Update(id, store.StatusRunning); err != nil {
		slog.Error("worker: update task to running failed", "task_id", id, "error", err)
		metrics.IncFailed()
		if err := p.tasks.Complete(id, "", errors.New("state transition failed")); err != nil {
			slog.Error("worker: task complete failed", "task_id", id, "error", err)
		}
		return
	}

	// 队列里只传了 ID，prompt 的权威副本在 store 中
	task, err := p.tasks.Get(id)
	if err != nil {
		slog.Error("worker: get task failed", "task_id", id, "error", err)
		metrics.IncFailed()
		if err := p.tasks.Complete(id, "", errors.New("task state unavailable")); err != nil {
			slog.Error("worker: task complete failed", "task_id", id, "error", err)
		}
		return
	}

	// 从 p.ctx 派生：继承停机广播，同时携带可归因的取消 cause。
	// 外层 WithCancelCause 让「用户主动取消」与「停机取消 / 超时」可精确区分，
	// 内层 WithTimeout 保留单次任务期限（子 ctx 超时优先暴露 DeadlineExceeded）。
	parentCtx, cancelCause := context.WithCancelCause(p.ctx)
	// 先登记句柄再派生超时子 ctx：即使取消请求落在两行之间，
	// 超时子 ctx 也会从已带 cause 的父 ctx 继承取消原因，不丢抢占。
	p.inFlight.Store(id, cancelCause)
	taskCtx, cancelTimeout := context.WithTimeout(parentCtx, p.taskTimeout)
	defer func() {
		p.inFlight.Delete(id)
		cancelTimeout()
		cancelCause(nil) // 释放资源；不覆盖已完成任务的既有取消原因
	}()

	tb := agent.NewTraceBuffer(agent.MaxTraceSteps)
	p.tracesMu.Lock()
	if p.maxTraces > 0 && len(p.traces) >= p.maxTraces && len(p.traceOrder) > 0 {
		oldest := p.traceOrder[0]
		p.traceOrder = p.traceOrder[1:]
		delete(p.traces, oldest)
	}
	p.traces[id] = tb
	p.traceOrder = append(p.traceOrder, id)
	p.tracesMu.Unlock()

	res, err := agent.Run(taskCtx, p.llm, task.Prompt, p.tools, tb.Append)
	if err != nil {
		// 错误全文只进日志：它携带上游响应体，可能含配额、账单等内部信息。
		// task.Error 会经 GET /tasks/{id} 原样返回给调用方，因此只存粗粒度分类。
		slog.Error("worker: task failed", "task_id", id, "error", err)

		metrics.IncFailed() // 终态：在飞数 -1，失败数 +1
		cause := context.Cause(taskCtx)
		var userCancel *userCancelCause
		var tc tool.TerminalClassifier
		var termErr error
		switch {
		case errors.As(cause, &userCancel):
			// 主动抢占：保留用户给的 reason，但由服务端统一前缀，
			// 不让调用方伪造任意错误文案。
			termErr = fmt.Errorf("canceled: %s", userCancel.reason)
		case errors.Is(taskCtx.Err(), context.DeadlineExceeded):
			termErr = errors.New("task timeout")
		case errors.Is(taskCtx.Err(), context.Canceled):
			termErr = errors.New("server shutdown")
		case strings.Contains(err.Error(), "max steps"):
			termErr = errors.New("max steps exceeded")
		case errors.As(err, &tc) && tc.TerminalStatus() != "":
			// 工具错误通过 TerminalClassifier 暴露脱敏终态文案（如 "index missing"），
			// 避免 worker 反向依赖具体工具包（ADR-0013 / ADR-0017），同时确保非空才覆盖。
			termErr = errors.New(tc.TerminalStatus())
		default:
			termErr = errors.New("upstream error")
		}
		if err := p.tasks.Complete(id, "", termErr); err != nil {
			slog.Error("worker: task complete failed", "task_id", id, "error", err)
		}
		return
	}

	metrics.IncDone() // 终态：在飞数 -1，完成数 +1
	if err := p.tasks.Complete(id, res, nil); err != nil {
		slog.Error("worker: task complete failed", "task_id", id, "error", err)
	}
}

// Enqueue 把任务 ID 送入队列。队列满（worker 全忙且缓冲已满）或正在停机时立即返回
// ErrQueueFull 而非阻塞 handler —— 让「异步」退化为「同步失败」而不是「同步等待」，
// 调用方据此回 503，把背压显式交给客户端/网关，而非让 HTTP 连接挂死。
func (p *Pool) Enqueue(id string) error {
	if p.stopping.Load() {
		return ErrQueueFull
	}
	select {
	case p.queue <- id:
		return nil
	default:
		return ErrQueueFull
	}
}

// Trace 返回指定任务已记录的步骤快照。若任务不存在或尚未记录 trace，返回 nil。
func (p *Pool) Trace(id string) []agent.TraceStep {
	p.tracesMu.Lock()
	defer p.tracesMu.Unlock()

	tb, ok := p.traces[id]
	if !ok || tb == nil {
		return nil
	}
	return tb.Snapshot()
}

// Subscribe 订阅指定任务的增量 TraceStep（供 SSE 流式推送）。
// 返回当前快照、增量通道、退订函数与是否命中；任务尚未被 worker 领单时 ok=false，
// 调用方可退化为轮询等待任务进入 running。
func (p *Pool) Subscribe(id string) (snapshot []agent.TraceStep, events <-chan agent.TraceStep, cancel func(), ok bool) {
	p.tracesMu.Lock()
	tb := p.traces[id]
	p.tracesMu.Unlock()

	if tb == nil {
		return nil, nil, func() {}, false
	}
	snapshot, events, cancel = tb.Subscribe()
	return snapshot, events, cancel, true
}

// userCancelCause 是用户主动取消的因果标记，用于与超时/停机取消精确区分。
type userCancelCause struct {
	reason string
}

func (c *userCancelCause) Error() string {
	return "user canceled: " + c.reason
}

// Cancel 主动抢占指定在飞任务：通过 context 因果链物理中断正在进行的上游调用。
// 任务不在飞（未开始、已终态或不存在）时返回 ErrTaskNotRunning，由 API 层映射为 409。
func (p *Pool) Cancel(id, reason string) error {
	v, ok := p.inFlight.Load(id)
	if !ok {
		return ErrTaskNotRunning
	}
	cancel, ok := v.(context.CancelCauseFunc)
	if !ok || cancel == nil {
		return ErrTaskNotRunning
	}
	cancel(&userCancelCause{reason: reason})
	return nil
}

// EnqueueWait 阻塞式入队，用于崩溃恢复时重灌 pending 积压：
// 既不丢任务（队列满时等待空位），也不无限挂死（停机或 ctx 取消即返回）。
func (p *Pool) EnqueueWait(ctx context.Context, id string) error {
	if p.stopping.Load() {
		return ErrQueueFull
	}
	select {
	case p.queue <- id:
		return nil
	case <-p.stopWorkerCtx.Done():
		return ErrQueueFull
	case <-ctx.Done():
		return ctx.Err()
	}
}
