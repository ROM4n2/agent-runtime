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
	wakeUp       chan struct{} // 通知空闲 worker 有新任务可领的唤醒信号（容量 1 缓冲通道）
	tasks        store.TaskStore
	size         int
	maxQueueSize int           // 最大允许排队积压的任务数，超出回 ErrQueueFull
	taskTimeout  time.Duration // 单次任务 LLM 调用上限，超时即失败
	// ctx/cancel 是关闭信号的广播机制：Stop 一次 cancel，
	// 所有 worker 与派生出去的请求 ctx 同时收到通知。
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
	inFlight         sync.Map
	sessionStore     store.SessionStore
	maxContextTokens int
	// sessionHistoryLimit 是组装会话历史时拉取的最大消息条数（ADR-0021 决策 3）。
	// 这是「模型窗口」参数而非代码常量：128k 窗口配 100 条是浪费，
	// 16k 窗口配 100 条则会被 CompactMessages 反复折叠、白跑压缩。
	sessionHistoryLimit int
}

// SetMaxQueueSize 设置最大允许排队积压的任务数，供测试和自定义配置使用。
func (p *Pool) SetMaxQueueSize(n int) { p.maxQueueSize = n }

func NewPool(size int, taskTimeout time.Duration, s store.TaskStore, client agent.Chatter) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	stopWorkerCtx, stopWorkerCancel := context.WithCancel(context.Background())

	return &Pool{
		size:                size,
		maxQueueSize:        10000,
		taskTimeout:         taskTimeout,
		tasks:               s,
		wakeUp:              make(chan struct{}, 1),
		ctx:                 ctx,
		cancel:              cancel,
		stopWorkerCtx:       stopWorkerCtx,
		stopWorkerCancel:    stopWorkerCancel,
		llm:                 client,
		tools:               tool.DefaultRegistry(),
		traces:              make(map[string]*agent.TraceBuffer),
		maxTraces:           DefaultMaxTraces,
		traceOrder:          make([]string, 0, 64),
		maxContextTokens:    16000,
		sessionHistoryLimit: 100,
	}
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
	ctx := context.Background()
	if len(ctxs) > 0 && ctxs[0] != nil {
		ctx = ctxs[0]
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

	// 排空 store 中残留未消费的 pending 任务，标定明确终态，消除 pending 僵尸任务
	for {
		task, err := p.tasks.ClaimNext(context.Background())
		if err != nil {
			break
		}
		// 排空同样是一次抢占：与下方 IncFailed 成对，避免 running 负向漂移。
		markClaimed()
		metrics.IncFailed()
		if err := p.tasks.Complete(task.ID, "", errors.New("server shutdown")); err != nil {
			slog.Error("worker: complete drained task failed", "task_id", task.ID, "error", err)
		}
	}
	return waitErr
}

// worker 是常驻 goroutine：循环抢占 store 里的任务，直到收到关闭信号。
// wg.Done 用 defer，保证 panic 退出时也不会让 Stop 永久阻塞在 Wait。
func (p *Pool) worker() {
	defer p.wg.Done()
	for {
		select {
		case <-p.stopWorkerCtx.Done():
			return
		default:
		}

		// 主动从 store 抢占任务
		task, err := p.tasks.ClaimNext(p.ctx)
		if err == nil {
			// 抢占成功即在飞：running gauge 的唯一入口。
			markClaimed()
			p.process(task.ID)
			continue // 连续抢占直到无待执行任务
		}
		if !errors.Is(err, store.ErrNoTaskAvailable) {
			select {
			case <-p.stopWorkerCtx.Done():
				return
			default:
			}
		}

		// 无任务可用，挂起等待新任务信号或 100ms 兜底定时器或停机信号
		select {
		case <-p.wakeUp:
		case <-time.After(100 * time.Millisecond):
		case <-p.stopWorkerCtx.Done():
			return
		}
	}
}

// process 执行单个任务：取 prompt → 调 LLM → 写回结果。
// 任何错误都在这里终结——worker 是 goroutine，没有调用方可以把 error 返回给谁。
func (p *Pool) process(id string) {
	// 从 p.ctx 派生：继承停机广播，同时携带可归因的取消 cause。
	// 在状态更新为 running 前先登记 inFlight 句柄，消除 TOCTOU 竞态。
	parentCtx, cancelCause := context.WithCancelCause(p.ctx)
	p.inFlight.Store(id, cancelCause)
	taskCtx, cancelTimeout := context.WithTimeout(parentCtx, p.taskTimeout)
	defer func() {
		p.inFlight.Delete(id)
		cancelTimeout()
		cancelCause(nil)
	}()

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

	history, err := p.assembleSessionMessages(taskCtx, task.SessionID, task.Prompt, "")
	if err != nil {
		slog.Warn("worker: assemble session messages failed", "task_id", id, "session_id", task.SessionID, "error", err)
	}
	var usage usageAccumulator
	res, err := agent.RunWithOptions(taskCtx, p.llm, task.Prompt, p.tools, tb.Append, agent.RunOptions{
		HistoryMessages: history, OnDelta: tb.AppendDelta, OnUsage: usage.add,
	})
	// 用量无论成败都要落：失败的步进同样计费（ADR-0021）。
	p.recordUsage(id, &usage)
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
	if task.SessionID != "" {
		if rErr := p.recordSessionTurn(p.ctx, task.SessionID, task.Prompt, res); rErr != nil {
			slog.Warn("worker: record session turn failed", "task_id", id, "session_id", task.SessionID, "error", rErr)
		}
	}
	if err := p.tasks.Complete(id, res, nil); err != nil {
		slog.Error("worker: task complete failed", "task_id", id, "error", err)
	}
}

// Enqueue 通知调度器有新任务可执行。
// 若池正在停机或 worker 数为 0，返回 ErrQueueFull；
// 若 pending 任务数超过 maxQueueSize，快速失败返回 ErrQueueFull，调用方据此回 503；
// 否则以非阻塞方式向 wakeUp 发送信号唤醒空闲 worker。
func (p *Pool) Enqueue(id string) error {
	if p.stopping.Load() || p.size == 0 {
		return ErrQueueFull
	}
	if p.maxQueueSize > 0 {
		cnt, err := p.tasks.PendingCount(p.ctx)
		if err == nil && cnt > int64(p.maxQueueSize) {
			return ErrQueueFull
		}
	}
	select {
	case p.wakeUp <- struct{}{}:
	default:
	}
	return nil
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

// SubscribeEvents 订阅指定任务的步骤快照与后续实时事件流（含逐字增量文本与步骤记录）。
// 供 SSE 端点推送 event: delta 与 event: step。任务未领单时 ok=false。
func (p *Pool) SubscribeEvents(id string) (snapshot []agent.TraceStep, events <-chan agent.TraceEvent, cancel func(), ok bool) {
	p.tracesMu.Lock()
	tb := p.traces[id]
	p.tracesMu.Unlock()

	if tb == nil {
		return nil, nil, func() {}, false
	}
	snapshot, events, cancel = tb.SubscribeEvents()
	return snapshot, events, cancel, true
}

// EnqueueWait 阻塞式入队，用于崩溃恢复时重灌 pending 积压：
// 循环等待直到待处理任务数未超出容量限制，既不丢任务也不无限挂死。
func (p *Pool) EnqueueWait(ctx context.Context, id string) error {
	if p.stopping.Load() || p.size == 0 {
		return ErrQueueFull
	}
	for {
		if p.stopping.Load() {
			return ErrQueueFull
		}
		if p.maxQueueSize > 0 {
			cnt, err := p.tasks.PendingCount(ctx)
			if err == nil && cnt <= int64(p.maxQueueSize) {
				return p.Enqueue(id)
			}
		} else {
			return p.Enqueue(id)
		}

		select {
		case <-time.After(10 * time.Millisecond):
		case <-p.stopWorkerCtx.Done():
			return ErrQueueFull
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
