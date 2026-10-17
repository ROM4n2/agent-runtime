package worker

import (
	"agent-api/agent"
	"sync"
)

type TraceStep = agent.TraceStep

const (
	KindThink      = agent.KindThink
	KindToolCall   = agent.KindToolCall
	KindToolResult = agent.KindToolResult
	KindFinal      = agent.KindFinal
	KindError      = agent.KindError
)

const maxTraceSteps = 32
const maxTraceDetail = 512

type traceBuffer struct {
	mu    sync.Mutex
	steps []TraceStep
	max   int
}

func newTraceBuffer(max int) *traceBuffer {
	if max <= 0 {
		max = maxTraceSteps
	}
	return &traceBuffer{
		max:   max,
		steps: make([]TraceStep, 0, max),
	}
}

func (b *traceBuffer) append(s TraceStep) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(s.Detail) > maxTraceDetail {
		s.Detail = s.Detail[:maxTraceDetail]
	}
	if b.max > 0 && len(b.steps) >= b.max {
		b.steps = b.steps[1:]
	}
	b.steps = append(b.steps, s)
}

func (b *traceBuffer) snapshot() []TraceStep {
	b.mu.Lock()
	defer b.mu.Unlock()

	cp := make([]TraceStep, len(b.steps))
	copy(cp, b.steps)
	return cp
}
