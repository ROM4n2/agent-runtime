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

// TraceBuffer 线程安全地维护固定容量的追踪记录环形/滑动缓冲。
type TraceBuffer struct {
	mu    sync.Mutex
	steps []TraceStep
	max   int
}

// NewTraceBuffer 创建指定最大容量的 TraceBuffer，若 max <= 0 则使用默认 MaxTraceSteps。
func NewTraceBuffer(max int) *TraceBuffer {
	if max <= 0 {
		max = MaxTraceSteps
	}
	return &TraceBuffer{
		max:   max,
		steps: make([]TraceStep, 0, max),
	}
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
}

// Snapshot 返回当前缓冲步骤的深拷贝切片。
func (b *TraceBuffer) Snapshot() []TraceStep {
	b.mu.Lock()
	defer b.mu.Unlock()

	cp := make([]TraceStep, len(b.steps))
	copy(cp, b.steps)
	return cp
}
