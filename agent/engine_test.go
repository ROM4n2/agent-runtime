package agent

import (
	"agent-api/agent/tool"
	"agent-api/llm"
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeChatter struct {
	turns         []*llm.AssistantTurn
	i             int
	err           error
	receivedMsgs  [][]llm.Message
	receivedTools [][]llm.Tool
}

func (f *fakeChatter) ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (*llm.AssistantTurn, error) {
	// 记录调用参数以供断言
	var cpMsgs []llm.Message
	if msgs != nil {
		cpMsgs = make([]llm.Message, len(msgs))
		copy(cpMsgs, msgs)
	}
	f.receivedMsgs = append(f.receivedMsgs, cpMsgs)

	var cpTools []llm.Tool
	if tools != nil {
		cpTools = make([]llm.Tool, len(tools))
		copy(cpTools, tools)
	}
	f.receivedTools = append(f.receivedTools, cpTools)

	if f.err != nil {
		return nil, f.err
	}
	if f.i < len(f.turns) {
		turn := f.turns[f.i]
		f.i++
		return turn, nil
	}
	return &llm.AssistantTurn{Content: "done"}, nil
}

func TestRun_SingleTurnFinal(t *testing.T) {
	fc := &fakeChatter{
		turns: []*llm.AssistantTurn{
			{Content: "Paris is the capital of France."},
		},
	}

	var steps []TraceStep
	onStep := func(s TraceStep) {
		steps = append(steps, s)
	}

	reg := tool.NewRegistry()
	res, err := Run(context.Background(), fc, "What is the capital of France?", reg, onStep)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if res != "Paris is the capital of France." {
		t.Errorf("expected result 'Paris is the capital of France.', got: %q", res)
	}

	// 应发出 KindThink 与 KindFinal
	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d: %+v", len(steps), steps)
	}
	if steps[0].Kind != KindThink || steps[0].Detail != "Paris is the capital of France." {
		t.Errorf("step 0 mismatch: %+v", steps[0])
	}
	if steps[1].Kind != KindFinal || steps[1].Detail != "Paris is the capital of France." {
		t.Errorf("step 1 mismatch: %+v", steps[1])
	}
}

func TestRun_ToolCallLoop(t *testing.T) {
	fc := &fakeChatter{
		turns: []*llm.AssistantTurn{
			{
				Content: "Let me compute that.",
				ToolCalls: []llm.ToolCall{
					{
						ID:   "call_calc_1",
						Type: "function",
						Function: llm.ToolCallFn{
							Name:      "calculate",
							Arguments: `{"expression":"(10+5)*2"}`,
						},
					},
				},
			},
			{
				Content: "The result is 30.",
			},
		},
	}

	var steps []TraceStep
	onStep := func(s TraceStep) {
		steps = append(steps, s)
	}

	reg := tool.DefaultRegistry()
	res, err := Run(context.Background(), fc, "What is (10+5)*2?", reg, onStep)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if res != "The result is 30." {
		t.Errorf("expected result 'The result is 30.', got: %q", res)
	}

	// 预期步骤序列：
	// 0: KindThink (Let me compute that.)
	// 1: KindToolCall (calculate, args)
	// 2: KindToolResult (calculate, 30)
	// 3: KindThink (The result is 30.)
	// 4: KindFinal (The result is 30.)
	wantKinds := []string{KindThink, KindToolCall, KindToolResult, KindThink, KindFinal}
	if len(steps) != len(wantKinds) {
		t.Fatalf("expected %d steps, got %d: %+v", len(wantKinds), len(steps), steps)
	}
	for i, want := range wantKinds {
		if steps[i].Kind != want {
			t.Errorf("step %d: got kind %q, want %q", i, steps[i].Kind, want)
		}
	}

	if steps[1].Tool != "calculate" || steps[1].CallID != "call_calc_1" {
		t.Errorf("tool_call step mismatch: %+v", steps[1])
	}
	if steps[2].Tool != "calculate" || steps[2].CallID != "call_calc_1" || steps[2].Detail != "30" {
		t.Errorf("tool_result step mismatch: %+v", steps[2])
	}
	if steps[4].Detail != "The result is 30." {
		t.Errorf("final step mismatch: %+v", steps[4])
	}
}

func TestRun_UpstreamErrorIsCoarse(t *testing.T) {
	secretErr := errors.New("database conn refused: password=leaked db password")
	fc := &fakeChatter{err: secretErr}

	var steps []TraceStep
	onStep := func(s TraceStep) {
		steps = append(steps, s)
	}

	_, err := Run(context.Background(), fc, "hello", nil, onStep)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, secretErr) && err.Error() != secretErr.Error() {
		t.Errorf("expected underlying error returned, got: %v", err)
	}

	if len(steps) != 1 {
		t.Fatalf("expected 1 step, got %d: %+v", len(steps), steps)
	}
	if steps[0].Kind != KindError {
		t.Errorf("expected KindError, got: %q", steps[0].Kind)
	}
	if steps[0].Detail != "upstream error" {
		t.Errorf("expected 'upstream error', got: %q", steps[0].Detail)
	}
	if strings.Contains(steps[0].Detail, "password") || strings.Contains(steps[0].Detail, "leaked") {
		t.Errorf("step detail leaked secret: %q", steps[0].Detail)
	}
}

func TestRun_MaxStepsExceeded(t *testing.T) {
	// 每次都返回 tool call，使循环达到 MaxAgentSteps
	var infiniteTurns []*llm.AssistantTurn
	for i := 0; i < MaxAgentSteps+2; i++ {
		infiniteTurns = append(infiniteTurns, &llm.AssistantTurn{
			Content: "thinking more",
			ToolCalls: []llm.ToolCall{
				{
					ID:       "call_loop",
					Type:     "function",
					Function: llm.ToolCallFn{Name: "calculate", Arguments: `{"expression":"1+1"}`},
				},
			},
		})
	}
	fc := &fakeChatter{turns: infiniteTurns}

	var steps []TraceStep
	onStep := func(s TraceStep) {
		steps = append(steps, s)
	}

	reg := tool.DefaultRegistry()
	_, err := Run(context.Background(), fc, "loop forever", reg, onStep)
	if err == nil {
		t.Fatal("expected error when max steps exceeded, got nil")
	}
	if err.Error() != "agent exceeded max steps" {
		t.Errorf("expected 'agent exceeded max steps', got: %v", err)
	}

	// 最后一个 step 应为 KindError, Detail 为 "max steps exceeded"
	if len(steps) == 0 {
		t.Fatal("expected trace steps, got none")
	}
	last := steps[len(steps)-1]
	if last.Kind != KindError {
		t.Errorf("expected last step to be KindError, got: %q", last.Kind)
	}
	if last.Detail != "max steps exceeded" {
		t.Errorf("expected last step detail 'max steps exceeded', got: %q", last.Detail)
	}
}

func TestRun_NilCallbackNoPanic(t *testing.T) {
	fc := &fakeChatter{
		turns: []*llm.AssistantTurn{
			{
				Content: "calculating",
				ToolCalls: []llm.ToolCall{
					{
						ID:       "1",
						Type:     "function",
						Function: llm.ToolCallFn{Name: "calculate", Arguments: `{"expression":"2+2"}`},
					},
				},
			},
			{Content: "4"},
		},
	}

	reg := tool.DefaultRegistry()
	// 传入 onStep = nil，绝不 panic
	res, err := Run(context.Background(), fc, "2+2", reg, nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if res != "4" {
		t.Errorf("expected '4', got: %q", res)
	}
}

func TestRun_NilRegistryHandled(t *testing.T) {
	fc := &fakeChatter{
		turns: []*llm.AssistantTurn{
			{
				Content: "try tool",
				ToolCalls: []llm.ToolCall{
					{
						ID:       "1",
						Type:     "function",
						Function: llm.ToolCallFn{Name: "calculate", Arguments: `{"expression":"1+1"}`},
					},
				},
			},
			{Content: "fallback done"},
		},
	}

	var steps []TraceStep
	onStep := func(s TraceStep) {
		steps = append(steps, s)
	}

	// reg == nil
	res, err := Run(context.Background(), fc, "test nil reg", nil, onStep)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res != "fallback done" {
		t.Errorf("expected 'fallback done', got: %q", res)
	}

	// 工具执行结果应为 "error: no tool registry"
	var foundResult bool
	for _, s := range steps {
		if s.Kind == KindToolResult {
			foundResult = true
			if s.Detail != "error: no tool registry" {
				t.Errorf("expected tool_result 'error: no tool registry', got: %q", s.Detail)
			}
		}
	}
	if !foundResult {
		t.Error("expected tool_result step emitted")
	}

	// 传入模型的 tools 应为 nil
	if len(fc.receivedTools) > 0 && fc.receivedTools[0] != nil {
		t.Errorf("expected tools passed to Chatter to be nil, got: %+v", fc.receivedTools[0])
	}
}
