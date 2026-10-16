package worker

import (
	"agent-api/llm"
	"agent-api/metrics"
	"agent-api/store"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// chatter 抽象 process 所需的 LLM 能力，
// 使测试可以注入不发真实请求的实现。
type chatter interface {
	// ChatWithTools 发送带工具定义的对话，返回模型本轮回复（终态文本或工具调用）。
	ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (*llm.AssistantTurn, error)
}

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
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	llm      chatter
	tools    []llm.Tool             // 注册给模型的工具描述
	handlers map[string]toolHandler // 工具名 -> 本地执行器
	tracesMu sync.Mutex
	traces   map[string]*traceBuffer
}

func NewPool(size int, taskTimeout time.Duration, s store.TaskStore, client chatter) *Pool {
	ctx, cancel := context.WithCancel(context.Background())

	p := Pool{
		size:        size,
		taskTimeout: taskTimeout,
		tasks:       s,
		queue:       make(chan string, size),
		ctx:         ctx,
		cancel:      cancel,
		llm:         client,
		traces:      make(map[string]*traceBuffer),
	}
	// 装配默认工具集（计算器、当前时间），让 worker 具备基础 agent 能力。
	p.tools, p.handlers = defaultTools()
	return &p
}

func (p *Pool) Start() {
	for i := 0; i < p.size; i++ {
		p.wg.Add(1)
		go p.worker()
	}
}

func (p *Pool) Stop() {
	p.cancel()
	p.wg.Wait()
}

// worker 是常驻 goroutine：循环抢队列里的任务，直到收到关闭信号。
// wg.Done 用 defer，保证 panic 退出时也不会让 Stop 永久阻塞在 Wait。
func (p *Pool) worker() {
	defer p.wg.Done()
	for {
		select {
		case id := <-p.queue:
			p.process(id)

		case <-p.ctx.Done():
			return
		}
	}
}

// process 执行单个任务：取 prompt → 调 LLM → 写回结果。
// 任何错误都在这里终结——worker 是 goroutine，没有调用方可以把 error 返回给谁。
func (p *Pool) process(id string) {
	// 先置 running，让轮询方立刻看到状态推进
	if err := p.tasks.Update(id, store.StatusRunning); err != nil {
		slog.Error("worker: update task %s to running: %v", id, err)
		metrics.IncFailed()
		if err := p.tasks.Complete(id, "", errors.New("state transition failed")); err != nil {
			slog.Error("worker: task %s complete failed: %v", id, err)
		}
		return
	}

	// 队列里只传了 ID，prompt 的权威副本在 store 中
	task, err := p.tasks.Get(id)
	if err != nil {
		slog.Error("worker: get task %s: %v", id, err)
		metrics.IncFailed()
		if err := p.tasks.Complete(id, "", errors.New("task state unavailable")); err != nil {
			slog.Error("worker: task %s complete failed: %v", id, err)
		}
		return
	}

	// 从 p.ctx 派生（而非 Background）：既有单次期限，
	// 又能在 Stop 时随父 ctx 一起取消在飞的请求，不必干等超时。
	taskCtx, cancel := context.WithTimeout(p.ctx, p.taskTimeout)
	defer cancel()

	tb := newTraceBuffer(maxTraceSteps)
	p.tracesMu.Lock()
	p.traces[id] = tb
	p.tracesMu.Unlock()

	res, err := runAgent(taskCtx, p.llm, task.Prompt, p.tools, p.execTool, tb.append)
	if err != nil {
		// 错误全文只进日志：它携带上游响应体，可能含配额、账单等内部信息。
		// task.Error 会经 GET /tasks/{id} 原样返回给调用方，因此只存粗粒度分类。
		slog.Error("worker: task %s failed: %v", id, err)

		metrics.IncFailed() // 终态：在飞数 -1，失败数 +1
		if err := p.tasks.Complete(id, "", errors.New("upstream error")); err != nil {
			slog.Error("worker: task %s complete failed: %v", id, err)
		}
		return
	}

	metrics.IncDone() // 终态：在飞数 -1，完成数 +1
	if err := p.tasks.Complete(id, res, nil); err != nil {
		slog.Error("worker: task %s complete failed: %v", id, err)
	}
}

// execTool 按工具名执行本地实现；错误也转成字符串返回，让模型有机会自我纠正。
func (p *Pool) execTool(tc llm.ToolCall) string {
	h, ok := p.handlers[tc.Function.Name]
	if !ok {
		return "error: unknown tool " + tc.Function.Name
	}
	out, err := h(json.RawMessage(tc.Function.Arguments))
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}

// ErrQueueFull 表示任务队列已满（所有 worker 忙且缓冲占满），Enqueue 因此快速失败。
var ErrQueueFull = errors.New("task queue is full")

// Enqueue 把任务 ID 送入队列。队列满（worker 全忙且缓冲已满）时立即返回
// ErrQueueFull 而非阻塞 handler —— 让「异步」退化为「同步失败」而不是「同步等待」，
// 调用方据此回 503，把背压显式交给客户端/网关，而非让 HTTP 连接挂死。
func (p *Pool) Enqueue(id string) error {
	select {
	case p.queue <- id:
		return nil
	default:
		return ErrQueueFull
	}
}

// Trace 返回指定任务已记录的步骤快照。若任务不存在或尚未记录 trace，返回 nil。
func (p *Pool) Trace(id string) []TraceStep {
	p.tracesMu.Lock()
	defer p.tracesMu.Unlock()

	tb, ok := p.traces[id]
	if !ok || tb == nil {
		return nil
	}
	return tb.snapshot()
}
