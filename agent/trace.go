package agent

import (
	"sync"
	"unicode/utf8"
)

// TraceStep 表示 Agent 执行过程中的单步记录。
type TraceStep struct {
	Step   int    `json:"step"`
	Kind   string `json:"kind"`
	Tool   string `json:"tool,omitempty"`
	Detail string `json:"detail,omitempty"`
	CallID string `json:"call_id,omitempty"`
}

const (
	KindThink      = "think"
	KindToolCall   = "tool_call"
	KindToolResult = "tool_result"
	KindFinal      = "final"
	KindError      = "error"
)

const MaxTraceSteps = 32
const MaxTraceDetail = 512

// TraceBuffer 线程安全地维护固定容量的追踪记录环形/滑动缓冲，
// 并支持轻量级订阅广播（SSE 实时步进推送）。
type TraceBuffer struct {
	mu    sync.Mutex
	steps []TraceStep
	max   int
	// subs 是当前订阅者集合。Append 持锁向每个订阅者非阻塞投递：
	// 慢消费者只会丢弃自己的新事件，绝不会阻塞任务执行或拖垮其他订阅者。
	subs map[chan TraceStep]struct{}
}

// NewTraceBuffer 创建指定最大容量的 TraceBuffer，若 max <= 0 则使用默认 MaxTraceSteps。
func NewTraceBuffer(max int) *TraceBuffer {
	if max <= 0 {
		max = MaxTraceSteps
	}
	return &TraceBuffer{
		max:   max,
		steps: make([]TraceStep, 0, max),
		subs:  make(map[chan TraceStep]struct{}),
	}
}

// Subscribe 原子地返回当前步骤快照与后续增量事件通道，避免「先订阅后取快照」
// 之间漏步或重复。cancel 必须由调用方在结束时执行（幂等），负责退订并关闭通道。
// 通道容量等于缓冲容量，正常情况下不会丢失步骤；消费过慢时丢弃新事件而非阻塞写入方。
func (b *TraceBuffer) Subscribe() (snapshot []TraceStep, events <-chan TraceStep, cancel func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	snapshot = make([]TraceStep, len(b.steps))
	copy(snapshot, b.steps)

	ch := make(chan TraceStep, b.max)
	if b.subs == nil {
		b.subs = make(map[chan TraceStep]struct{})
	}
	b.subs[ch] = struct{}{}

	return snapshot, ch, func() { b.unsubscribe(ch) }
}

func (b *TraceBuffer) unsubscribe(ch chan TraceStep) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[ch]; !ok {
		return // 幂等：重复退订不重复 close（close 已关闭通道会 panic）
	}
	delete(b.subs, ch)
	close(ch)
}

// Append 向缓冲中追加一条记录。超长 Detail 会被截断，超出容量时丢弃最旧的项。
func (b *TraceBuffer) Append(s TraceStep) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(s.Detail) > MaxTraceDetail {
		cutoff := MaxTraceDetail
		for cutoff > 0 && !utf8.RuneStart(s.Detail[cutoff]) {
			cutoff--
		}
		s.Detail = s.Detail[:cutoff]
	}
	if b.max > 0 && len(b.steps) >= b.max {
		copy(b.steps, b.steps[1:])
		b.steps[len(b.steps)-1] = s
	} else {
		b.steps = append(b.steps, s)
	}

	for ch := range b.subs {
		select {
		case ch <- s:
		default:
			// 慢消费者：丢弃该条增量，其仍可通过 Snapshot 补齐（终态对账）。
		}
	}
}

// Snapshot 返回当前缓冲步骤的深拷贝切片。
func (b *TraceBuffer) Snapshot() []TraceStep {
	b.mu.Lock()
	defer b.mu.Unlock()

	cp := make([]TraceStep, len(b.steps))
	copy(cp, b.steps)
	return cp
}
