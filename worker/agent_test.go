package worker

import (
	"agent-api/llm"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// scriptedChatter 按预设顺序返回模型回复，并记下是否收到 role=tool 的回填消息。
type scriptedChatter struct {
	turns         []*llm.AssistantTurn
	i             int
	gotToolResult bool
}

func (s *scriptedChatter) ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (*llm.AssistantTurn, error) {
	for _, m := range msgs {
		if m.Role == "tool" {
			s.gotToolResult = true
		}
	}
	if s.i >= len(s.turns) {
		return &llm.AssistantTurn{Content: "done"}, nil
	}
	t := s.turns[s.i]
	s.i++
	return t, nil
}

func TestRunAgent_ExecutesToolAndFeedsBack(t *testing.T) {
	specs, handlers := defaultTools()

	sc := &scriptedChatter{turns: []*llm.AssistantTurn{
		{ToolCalls: []llm.ToolCall{{
			ID:       "1",
			Type:     "function",
			Function: llm.ToolCallFn{Name: "calculate", Arguments: `{"expression":"2*(3+4)"}`},
		}}},
		{Content: "the answer is 14"},
	}}

	var executed string
	exec := func(tc llm.ToolCall) string {
		h := handlers[tc.Function.Name]
		out, _ := h(json.RawMessage(tc.Function.Arguments))
		executed = out
		return out
	}

	res, err := runAgent(context.Background(), sc, "what is 2*(3+4)?", specs, exec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res != "the answer is 14" {
		t.Errorf("res = %q, want %q", res, "the answer is 14")
	}
	if executed != "14" {
		t.Errorf("tool executed result = %q, want %q", executed, "14")
	}
	if !sc.gotToolResult {
		t.Errorf("tool result was not fed back into the conversation")
	}
}

func TestRunAgent_EmitsTrace(t *testing.T) {
	specs, handlers := defaultTools()

	sc := &scriptedChatter{turns: []*llm.AssistantTurn{
		{
			Content: "let me calculate that",
			ToolCalls: []llm.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: llm.ToolCallFn{Name: "calculate", Arguments: `{"expression":"2*(3+4)"}`},
			}},
		},
		{Content: "the answer is 14"},
	}}

	exec := func(tc llm.ToolCall) string {
		h := handlers[tc.Function.Name]
		out, _ := h(json.RawMessage(tc.Function.Arguments))
		return out
	}

	var trace []TraceStep
	onStep := func(s TraceStep) {
		trace = append(trace, s)
	}

	res, err := runAgent(context.Background(), sc, "what is 2*(3+4)?", specs, exec, onStep)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res != "the answer is 14" {
		t.Fatalf("res = %q, want %q", res, "the answer is 14")
	}

	wantKinds := []string{KindThink, KindToolCall, KindToolResult, KindThink, KindFinal}
	if len(trace) != len(wantKinds) {
		t.Fatalf("trace len = %d, want %d: %+v", len(trace), len(wantKinds), trace)
	}
	for i, want := range wantKinds {
		if trace[i].Kind != want {
			t.Errorf("trace[%d].Kind = %q, want %q", i, trace[i].Kind, want)
		}
	}
	if trace[1].Tool != "calculate" || trace[1].CallID != "call_1" {
		t.Errorf("trace[1] tool_call unexpected: %+v", trace[1])
	}
	if trace[2].Tool != "calculate" || trace[2].CallID != "call_1" || trace[2].Detail != "14" {
		t.Errorf("trace[2] tool_result unexpected: %+v", trace[2])
	}
	if trace[4].Detail != "the answer is 14" {
		t.Errorf("trace[4] final detail = %q, want %q", trace[4].Detail, "the answer is 14")
	}
}

func TestRunAgent_ErrorStepIsCoarse(t *testing.T) {
	specs, _ := defaultTools()
	secretErr := errors.New("upstream postgres password leaked: supersecret")
	fc := fakeChatter{err: secretErr}

	var trace []TraceStep
	onStep := func(s TraceStep) {
		trace = append(trace, s)
	}

	exec := func(tc llm.ToolCall) string { return "" }

	_, err := runAgent(context.Background(), fc, "hello", specs, exec, onStep)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if len(trace) != 1 {
		t.Fatalf("expected 1 trace step, got %d: %+v", len(trace), trace)
	}
	if trace[0].Kind != KindError {
		t.Errorf("trace[0].Kind = %q, want %q", trace[0].Kind, KindError)
	}
	if trace[0].Detail != "upstream error" {
		t.Errorf("trace[0].Detail = %q, want %q", trace[0].Detail, "upstream error")
	}
	if strings.Contains(trace[0].Detail, "secret") {
		t.Errorf("trace[0].Detail contains leaked string: %q", trace[0].Detail)
	}
}

func TestRunAgent_NilCallbackNoPanic(t *testing.T) {
	specs, handlers := defaultTools()
	sc := &scriptedChatter{turns: []*llm.AssistantTurn{
		{
			Content: "thinking",
			ToolCalls: []llm.ToolCall{{
				ID:       "1",
				Type:     "function",
				Function: llm.ToolCallFn{Name: "calculate", Arguments: `{"expression":"1+1"}`},
			}},
		},
		{Content: "2"},
	}}
	exec := func(tc llm.ToolCall) string {
		h := handlers[tc.Function.Name]
		out, _ := h(json.RawMessage(tc.Function.Arguments))
		return out
	}

	res, err := runAgent(context.Background(), sc, "1+1", specs, exec, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res != "2" {
		t.Errorf("res = %q, want %q", res, "2")
	}
}

func TestEvalArithmetic(t *testing.T) {
	cases := map[string]float64{
		"2*(3+4)": 14,
		"1+2*3":   7,
		"(1+2)*3": 9,
		"10/4":    2.5,
		"-3+5":    2,
		"2.5*2":   5,
	}
	for expr, want := range cases {
		got, err := evalArithmetic(expr)
		if err != nil {
			t.Fatalf("%q: %v", expr, err)
		}
		if got != want {
			t.Errorf("%q = %v, want %v", expr, got, want)
		}
	}

	if _, err := evalArithmetic("1/0"); err == nil {
		t.Errorf("expected division-by-zero error")
	}
	if _, err := evalArithmetic("1+"); err == nil {
		t.Errorf("expected parse error for incomplete expression")
	}
}
