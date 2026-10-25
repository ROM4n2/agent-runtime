package agent

import (
	"agent-runtime/agent/tool"
	"agent-runtime/llm"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeChatter struct {
	turns         []*llm.AssistantTurn
	i             int
	err           error
	receivedMsgs  [][]llm.Message
	receivedTools [][]llm.Tool
}

func (f *fakeChatter) ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (*llm.AssistantTurn, error) {
	// 忠于真实 llm.Client：ctx 已取消时上游调用必然失败。
	// 没有这一条，取消路径的测试会拿到假绿的终态文本。
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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

// retryableErr 实现 tool.RetryableError（Retryable()=true），模拟「瞬时故障值得重试一次」。
type retryableErr struct{ msg string }

func (e *retryableErr) Error() string   { return e.msg }
func (e *retryableErr) Retryable() bool { return true }

// permanentErr 不实现 RetryableError，走默认的 permanent 分支。
type permanentErr struct{ msg string }

func (e *permanentErr) Error() string { return e.msg }

// flakyTool 是可精确控制失败次数的假工具：前 failN 次调用返回 err，其余成功。
type flakyTool struct {
	name    string
	failN   int
	err     error
	calls   int
	lastCtx error // 记录最后一次调用时工具看到的 ctx.Err()
}

func (f *flakyTool) Spec() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.FunctionSpec{
			Name:        f.name,
			Description: "Flaky tool for testing",
			Parameters:  map[string]any{"type": "object"},
		},
	}
}

func (f *flakyTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	f.calls++
	if ctx != nil {
		f.lastCtx = ctx.Err()
	}
	if f.calls <= f.failN {
		return "", f.err
	}
	return "flaky ok", nil
}

// toolCallTurn 构造一轮只含单个工具调用的 AssistantTurn。
func toolCallTurn(id, name, args string) *llm.AssistantTurn {
	return &llm.AssistantTurn{
		Content: "calling tool",
		ToolCalls: []llm.ToolCall{
			{ID: id, Type: "function", Function: llm.ToolCallFn{Name: name, Arguments: args}},
		},
	}
}

// findSteps 返回 trace 中指定 Kind 的步骤，供断言使用。
func findSteps(steps []TraceStep, kind string) []TraceStep {
	var out []TraceStep
	for _, s := range steps {
		if s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}

// TestRun_RetryableToolErrorRetriedOnce 验证：可重试错误被重试恰好一次（共 2 次调用），
// 仍失败则 emit KindError 粗分类（脱敏：不出现错误全文），错误串喂回模型。
func TestRun_RetryableToolErrorRetriedOnce(t *testing.T) {
	secret := &retryableErr{msg: "vector store timeout: token=sk-secret"}
	ft := &flakyTool{name: "flaky", failN: 99, err: secret}

	fc := &fakeChatter{
		turns: []*llm.AssistantTurn{
			toolCallTurn("call_flaky_1", "flaky", `{}`),
			{Content: "recovered without tool"},
		},
	}

	var steps []TraceStep
	reg := tool.NewRegistry()
	reg.Register(ft)

	start := time.Now()
	res, err := Run(context.Background(), fc, "do it", reg, func(s TraceStep) { steps = append(steps, s) })
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if res != "recovered without tool" {
		t.Errorf("expected 'recovered without tool', got: %q", res)
	}

	// 去重重试一次：初始 + 重试 = 恰好 2 次
	if ft.calls != 2 {
		t.Errorf("flaky tool called %d times, want 2", ft.calls)
	}
	// 工具看到的 ctx 未被取消（Run 的 ctx 原样穿透）
	if ft.lastCtx != nil {
		t.Errorf("tool saw canceled ctx: %v", ft.lastCtx)
	}
	// 退避必须真实发生（ctx 感知定时器下限 200ms）
	if elapsed < 200*time.Millisecond {
		t.Errorf("elapsed %v, want >= 200ms backoff before retry", elapsed)
	}

	errSteps := findSteps(steps, KindError)
	if len(errSteps) != 1 {
		t.Fatalf("expected exactly 1 KindError step, got %d: %+v", len(errSteps), steps)
	}
	if errSteps[0].Detail != "tool error: retryable" {
		t.Errorf("KindError detail = %q, want %q", errSteps[0].Detail, "tool error: retryable")
	}
	if errSteps[0].Tool != "flaky" || errSteps[0].CallID != "call_flaky_1" {
		t.Errorf("KindError step mismatch: %+v", errSteps[0])
	}

	// 脱敏红线：trace 的任何 detail 都不得出现错误全文
	for _, s := range steps {
		if strings.Contains(s.Detail, "sk-secret") || strings.Contains(s.Detail, "vector store timeout") {
			t.Errorf("trace leaked error text: %+v", s)
		}
	}

	// 错误串喂回模型：最后一轮 LLM 输入应含 role=tool 且带 "error: " 前缀
	if len(fc.receivedMsgs) < 2 {
		t.Fatalf("expected >= 2 chatter rounds, got %d", len(fc.receivedMsgs))
	}
	var fedBack string
	for _, m := range fc.receivedMsgs[1] {
		if m.Role == "tool" {
			fedBack = m.Content
		}
	}
	if !strings.HasPrefix(fedBack, "error: ") || !strings.Contains(fedBack, "vector store timeout") {
		t.Errorf("error string not fed back to model, got tool msg %q", fedBack)
	}
}

// TestRun_RetryDedupByCallID 验证：同一个 tc.ID 在整个任务内最多重试一次。
// 模型在多轮里重发同一调用 ID（或同轮重复）时，绝不再叠加重试——否则一次瞬时故障
// 会被放大成 2^N 次调用风暴。
func TestRun_RetryDedupByCallID(t *testing.T) {
	ft := &flakyTool{name: "flaky", failN: 99, err: &retryableErr{msg: "transient"}}

	fc := &fakeChatter{
		turns: []*llm.AssistantTurn{
			toolCallTurn("call_same", "flaky", `{}`),
			toolCallTurn("call_same", "flaky", `{}`), // 同 ID 重发
			{Content: "finally gave up"},
		},
	}

	var steps []TraceStep
	reg := tool.NewRegistry()
	reg.Register(ft)

	res, err := Run(context.Background(), fc, "do it", reg, func(s TraceStep) { steps = append(steps, s) })
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if res != "finally gave up" {
		t.Errorf("expected 'finally gave up', got: %q", res)
	}
	// 首轮：初始 + 重试 = 2 次；第二轮同 ID 已重试过，不再重试 = 1 次。合计 3 次。
	if ft.calls != 3 {
		t.Errorf("flaky tool called %d times, want 3 (dedup must cap retries at one per call ID)", ft.calls)
	}
	if errs := findSteps(steps, KindError); len(errs) != 2 {
		t.Errorf("expected 2 KindError steps (one per failed call), got %d: %+v", len(errs), steps)
	}
}

// TestRun_RetryableToolErrorThenSucceeds 验证：重试成功则不再视为失败，无 KindError。
func TestRun_RetryableToolErrorThenSucceeds(t *testing.T) {
	ft := &flakyTool{name: "flaky", failN: 1, err: &retryableErr{msg: "transient"}}

	fc := &fakeChatter{
		turns: []*llm.AssistantTurn{
			toolCallTurn("call_flaky_1", "flaky", `{}`),
			{Content: "done after retry"},
		},
	}

	var steps []TraceStep
	reg := tool.NewRegistry()
	reg.Register(ft)

	res, err := Run(context.Background(), fc, "do it", reg, func(s TraceStep) { steps = append(steps, s) })
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if res != "done after retry" {
		t.Errorf("expected 'done after retry', got: %q", res)
	}
	if ft.calls != 2 {
		t.Errorf("flaky tool called %d times, want 2", ft.calls)
	}
	if errs := findSteps(steps, KindError); len(errs) != 0 {
		t.Errorf("expected no KindError step after successful retry, got: %+v", errs)
	}
	// 重试成功的结果照常 emit KindToolResult 并喂回模型
	results := findSteps(steps, KindToolResult)
	if len(results) != 1 || results[0].Detail != "flaky ok" {
		t.Errorf("expected single KindToolResult 'flaky ok', got: %+v", results)
	}
}

// TestRun_NonRetryableToolErrorNotRetried 验证：non-retryable 错误绝不重试（只调 1 次），
// emit KindError 粗分类 "tool error: permanent"，错误串喂回模型。
func TestRun_NonRetryableToolErrorNotRetried(t *testing.T) {
	ft := &flakyTool{name: "bad", failN: 99, err: &permanentErr{msg: "bad args: secret=sk-secret"}}

	fc := &fakeChatter{
		turns: []*llm.AssistantTurn{
			toolCallTurn("call_bad_1", "bad", `{}`),
			{Content: "gave up"},
		},
	}

	var steps []TraceStep
	reg := tool.NewRegistry()
	reg.Register(ft)

	res, err := Run(context.Background(), fc, "do it", reg, func(s TraceStep) { steps = append(steps, s) })
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if res != "gave up" {
		t.Errorf("expected 'gave up', got: %q", res)
	}
	if ft.calls != 1 {
		t.Errorf("bad tool called %d times, want 1 (no retry for permanent error)", ft.calls)
	}

	errSteps := findSteps(steps, KindError)
	if len(errSteps) != 1 {
		t.Fatalf("expected exactly 1 KindError step, got %d: %+v", len(errSteps), steps)
	}
	if errSteps[0].Detail != "tool error: permanent" {
		t.Errorf("KindError detail = %q, want %q", errSteps[0].Detail, "tool error: permanent")
	}
	for _, s := range steps {
		if strings.Contains(s.Detail, "sk-secret") {
			t.Errorf("trace leaked error text: %+v", s)
		}
	}

	var fedBack string
	for _, m := range fc.receivedMsgs[1] {
		if m.Role == "tool" {
			fedBack = m.Content
		}
	}
	if !strings.HasPrefix(fedBack, "error: ") || !strings.Contains(fedBack, "bad args") {
		t.Errorf("error string not fed back to model, got tool msg %q", fedBack)
	}
}

// TestRun_RetryBackoffRespectsCtx 验证：退避是 ctx 感知的——退避窗口内 ctx 取消即放弃重试，
// 绝不裸睡满 200ms。
func TestRun_RetryBackoffRespectsCtx(t *testing.T) {
	ft := &flakyTool{name: "flaky", failN: 99, err: &retryableErr{msg: "transient"}}

	fc := &fakeChatter{
		turns: []*llm.AssistantTurn{
			toolCallTurn("call_flaky_1", "flaky", `{}`),
			{Content: "never reached"},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	// 首次工具调用已返回可重试错误，此刻进入退避；50ms 后取消应立刻打断退避。
	time.AfterFunc(50*time.Millisecond, cancel)

	reg := tool.NewRegistry()
	reg.Register(ft)

	start := time.Now()
	_, err := Run(ctx, fc, "do it", reg, nil)
	elapsed := time.Since(start)
	cancel() // 幂等，防止 context 泄漏告警

	if err == nil {
		t.Fatal("expected error after ctx cancellation during backoff, got nil")
	}
	if ft.calls != 1 {
		t.Errorf("flaky tool called %d times, want 1 (retry must be skipped once ctx is done)", ft.calls)
	}
	if elapsed > 190*time.Millisecond {
		t.Errorf("elapsed %v, want backoff aborted well before 200ms on ctx cancellation", elapsed)
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

func TestRunWithOptions_SystemPromptInjection(t *testing.T) {
	t.Run("with system prompt", func(t *testing.T) {
		fc := &fakeChatter{
			turns: []*llm.AssistantTurn{
				{Content: "answer with sys"},
			},
		}
		opts := RunOptions{
			SystemPrompt: "You are a helpful assistant.",
		}
		res, err := RunWithOptions(context.Background(), fc, "hello", nil, nil, opts)
		if err != nil {
			t.Fatalf("expected nil error, got: %v", err)
		}
		if res != "answer with sys" {
			t.Errorf("expected 'answer with sys', got %q", res)
		}
		if len(fc.receivedMsgs) == 0 || len(fc.receivedMsgs[0]) < 2 {
			t.Fatalf("expected at least 2 messages, got: %+v", fc.receivedMsgs)
		}
		if fc.receivedMsgs[0][0].Role != "system" || fc.receivedMsgs[0][0].Content != "You are a helpful assistant." {
			t.Errorf("expected messages[0] to be system with prompt, got %+v", fc.receivedMsgs[0][0])
		}
		if fc.receivedMsgs[0][1].Role != "user" || fc.receivedMsgs[0][1].Content != "hello" {
			t.Errorf("expected messages[1] to be user with prompt, got %+v", fc.receivedMsgs[0][1])
		}
	})

	t.Run("empty system prompt backward compatibility", func(t *testing.T) {
		fc := &fakeChatter{
			turns: []*llm.AssistantTurn{
				{Content: "answer without sys"},
			},
		}
		opts := RunOptions{
			SystemPrompt: "",
		}
		res, err := RunWithOptions(context.Background(), fc, "hello", nil, nil, opts)
		if err != nil {
			t.Fatalf("expected nil error, got: %v", err)
		}
		if res != "answer without sys" {
			t.Errorf("expected 'answer without sys', got %q", res)
		}
		if len(fc.receivedMsgs) == 0 || len(fc.receivedMsgs[0]) != 1 {
			t.Fatalf("expected exactly 1 message, got: %+v", fc.receivedMsgs)
		}
		if fc.receivedMsgs[0][0].Role != "user" || fc.receivedMsgs[0][0].Content != "hello" {
			t.Errorf("expected messages[0] to be user, got %+v", fc.receivedMsgs[0][0])
		}
	})
}

func TestRunWithOptions_OnUsageAccumulation(t *testing.T) {
	fc := &fakeChatter{
		turns: []*llm.AssistantTurn{
			{
				Content: "step 1 calling tool",
				ToolCalls: []llm.ToolCall{
					{
						ID:   "call_calc",
						Type: "function",
						Function: llm.ToolCallFn{
							Name:      "calculate",
							Arguments: `{"expression":"1+1"}`,
						},
					},
				},
				Usage: llm.Usage{
					PromptTokens:     10,
					CompletionTokens: 5,
					TotalTokens:      15,
				},
			},
			{
				Content: "final answer 2",
				Usage: llm.Usage{
					PromptTokens:     8,
					CompletionTokens: 4,
					TotalTokens:      12,
				},
			},
		},
	}

	var recordedUsages []llm.Usage
	opts := RunOptions{
		OnUsage: func(u llm.Usage) {
			recordedUsages = append(recordedUsages, u)
		},
	}

	reg := tool.DefaultRegistry()
	res, err := RunWithOptions(context.Background(), fc, "what is 1+1?", reg, nil, opts)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if res != "final answer 2" {
		t.Errorf("expected result 'final answer 2', got %q", res)
	}

	if len(recordedUsages) != 2 {
		t.Fatalf("expected 2 usage events, got %d: %+v", len(recordedUsages), recordedUsages)
	}
	expectedFirst := llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	if recordedUsages[0] != expectedFirst {
		t.Errorf("usage[0] mismatch: got %+v, want %+v", recordedUsages[0], expectedFirst)
	}
	expectedSecond := llm.Usage{PromptTokens: 8, CompletionTokens: 4, TotalTokens: 12}
	if recordedUsages[1] != expectedSecond {
		t.Errorf("usage[1] mismatch: got %+v, want %+v", recordedUsages[1], expectedSecond)
	}
}
