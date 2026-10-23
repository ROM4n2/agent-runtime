package tool

import (
	"agent-runtime/llm"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type mockTool struct {
	name string
	fn   func(ctx context.Context, args json.RawMessage) (string, error)
}

func (m *mockTool) Spec() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.FunctionSpec{
			Name:        m.name,
			Description: "Mock tool for testing",
			Parameters:  map[string]any{"type": "object"},
		},
	}
}

func (m *mockTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if m.fn != nil {
		return m.fn(ctx, args)
	}
	return "mock result", nil
}

func TestDefaultRegistry(t *testing.T) {
	reg := DefaultRegistry()
	if reg == nil {
		t.Fatal("DefaultRegistry() returned nil")
	}

	specs := reg.Specs()
	if len(specs) != 2 {
		t.Fatalf("expected 2 specs in DefaultRegistry, got %d", len(specs))
	}

	foundCalc := false
	foundTime := false
	for _, s := range specs {
		if s.Function.Name == "calculate" {
			foundCalc = true
		}
		if s.Function.Name == "current_time" {
			foundTime = true
		}
	}
	if !foundCalc {
		t.Error("calculate tool not found in DefaultRegistry specs")
	}
	if !foundTime {
		t.Error("current_time tool not found in DefaultRegistry specs")
	}

	calcTool, ok := reg.Get("calculate")
	if !ok || calcTool == nil {
		t.Error("Get(\"calculate\") returned false or nil")
	}

	timeTool, ok := reg.Get("current_time")
	if !ok || timeTool == nil {
		t.Error("Get(\"current_time\") returned false or nil")
	}

	_, ok = reg.Get("nonexistent")
	if ok {
		t.Error("Get(\"nonexistent\") returned true, want false")
	}
}

func TestRegistry_Register(t *testing.T) {
	reg := NewRegistry()
	if len(reg.Specs()) != 0 {
		t.Errorf("expected 0 specs in empty registry, got %d", len(reg.Specs()))
	}

	tool := &mockTool{name: "echo"}
	reg.Register(tool)

	got, ok := reg.Get("echo")
	if !ok || got != tool {
		t.Errorf("Get(\"echo\") did not return registered tool")
	}

	specs := reg.Specs()
	if len(specs) != 1 || specs[0].Function.Name != "echo" {
		t.Errorf("expected 1 spec with name 'echo', got %v", specs)
	}
}

func TestRegistry_Execute(t *testing.T) {
	reg := DefaultRegistry()

	// 1. 未知工具
	t.Run("unknown tool", func(t *testing.T) {
		call := llm.ToolCall{
			Function: llm.ToolCallFn{
				Name:      "unknown_tool",
				Arguments: `{}`,
			},
		}
		res := reg.Execute(context.Background(), call)
		want := "error: unknown tool unknown_tool"
		if res != want {
			t.Errorf("Execute() = %q, want %q", res, want)
		}
	})

	// 2. 正常调用 calculate
	t.Run("execute calculate success", func(t *testing.T) {
		call := llm.ToolCall{
			Function: llm.ToolCallFn{
				Name:      "calculate",
				Arguments: `{"expression": "100 / 4"}`,
			},
		}
		res := reg.Execute(context.Background(), call)
		if res != "25" {
			t.Errorf("Execute() = %q, want %q", res, "25")
		}
	})

	// 3. calculate 出错
	t.Run("execute calculate error", func(t *testing.T) {
		call := llm.ToolCall{
			Function: llm.ToolCallFn{
				Name:      "calculate",
				Arguments: `{"expression": "10 / 0"}`,
			},
		}
		res := reg.Execute(context.Background(), call)
		if !strings.HasPrefix(res, "error: ") || !strings.Contains(res, "division by zero") {
			t.Errorf("Execute() = %q, want prefix 'error: ' with 'division by zero'", res)
		}
	})

	// 4. 正常调用 current_time
	t.Run("execute current_time success", func(t *testing.T) {
		call := llm.ToolCall{
			Function: llm.ToolCallFn{
				Name:      "current_time",
				Arguments: `{"timezone": "UTC"}`,
			},
		}
		res := reg.Execute(context.Background(), call)
		if strings.HasPrefix(res, "error: ") {
			t.Fatalf("unexpected error response: %s", res)
		}
		if _, err := time.Parse(time.RFC3339, res); err != nil {
			t.Errorf("Execute() returned invalid RFC3339 timestamp: %q", res)
		}
	})

	// 5. 错误转消息不 panic
	t.Run("execute tool returning error wrapped with error prefix", func(t *testing.T) {
		customReg := NewRegistry()
		customReg.Register(&mockTool{
			name: "failing_tool",
			fn: func(ctx context.Context, args json.RawMessage) (string, error) {
				return "", errors.New("something went wrong")
			},
		})

		call := llm.ToolCall{
			Function: llm.ToolCallFn{
				Name:      "failing_tool",
				Arguments: `{}`,
			},
		}
		res := customReg.Execute(context.Background(), call)
		want := "error: something went wrong"
		if res != want {
			t.Errorf("Execute() = %q, want %q", res, want)
		}
	})

	// 6. 并发安全
	t.Run("concurrent execute", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				call := llm.ToolCall{
					Function: llm.ToolCallFn{
						Name:      "calculate",
						Arguments: fmt.Sprintf(`{"expression": "%d + %d"}`, idx, idx),
					},
				}
				res := reg.Execute(context.Background(), call)
				expected := fmt.Sprintf("%d", idx+idx)
				if res != expected {
					t.Errorf("concurrent execute = %q, want %q", res, expected)
				}
			}(i)
		}
		wg.Wait()
	})
}

// flakyErr 实现 RetryableError，用于验证「可重试」错误能被 agent.Run 分类识别。
type flakyErr struct{}

func (flakyErr) Error() string   { return "transient channel failure" }
func (flakyErr) Retryable() bool { return true }

// 编译期即钉死 RetryableError 的形状：error + Retryable() bool。
var _ RetryableError = flakyErr{}

func TestRegistry_ExecuteErr(t *testing.T) {
	call := func(name string) llm.ToolCall {
		return llm.ToolCall{
			Function: llm.ToolCallFn{Name: name, Arguments: `{}`},
		}
	}

	// 1. 未知工具：ExecuteErr 必须返回 error 而非压成字符串（原始错误出口）
	t.Run("unknown tool returns error", func(t *testing.T) {
		reg := NewRegistry()
		out, err := reg.ExecuteErr(context.Background(), call("nope"))
		if err == nil {
			t.Fatal("ExecuteErr(unknown tool) returned nil error, want error")
		}
		if out != "" {
			t.Errorf("ExecuteErr(unknown tool) out = %q, want empty", out)
		}
		if !strings.Contains(err.Error(), "unknown tool") || !strings.Contains(err.Error(), "nope") {
			t.Errorf("ExecuteErr(unknown tool) err = %v, want mention of unknown tool name", err)
		}
	})

	// 2. 工具错误原样返回：绝不被压成 "error: " 前缀串
	t.Run("tool error returned as-is", func(t *testing.T) {
		reg := NewRegistry()
		sentinel := flakyErr{}
		reg.Register(&mockTool{
			name: "boom",
			fn: func(ctx context.Context, args json.RawMessage) (string, error) {
				return "", sentinel
			},
		})
		out, err := reg.ExecuteErr(context.Background(), call("boom"))
		if out != "" {
			t.Errorf("ExecuteErr() out = %q, want empty", out)
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("ExecuteErr() err = %v, want original sentinel %v", err, sentinel)
		}
		// 原样返回：不得带 "error: " 前缀（那是 Execute 包装层的事）
		if strings.HasPrefix(err.Error(), "error: ") {
			t.Errorf("ExecuteErr() leaked Execute wrapper prefix: %q", err.Error())
		}
	})

	// 3. ctx 必须穿透到工具内部：取消的 ctx 原样抵达 Tool.Execute
	t.Run("canceled ctx reaches tool", func(t *testing.T) {
		reg := NewRegistry()
		var toolSawCtxErr error
		reg.Register(&mockTool{
			name: "ctx_probe",
			fn: func(ctx context.Context, args json.RawMessage) (string, error) {
				toolSawCtxErr = ctx.Err()
				return "probed", nil
			},
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // 执行前即取消
		out, err := reg.ExecuteErr(ctx, call("ctx_probe"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out != "probed" {
			t.Errorf("ExecuteErr() out = %q, want %q", out, "probed")
		}
		if toolSawCtxErr == nil {
			t.Error("tool received a non-canceled ctx; ExecuteErr must pass ctx through")
		}
	})

	// 4. Execute 仍是压串包装，且必须与 ExecuteErr 共用同一实现
	t.Run("Execute wraps error string", func(t *testing.T) {
		reg := NewRegistry()
		reg.Register(&mockTool{
			name: "boom",
			fn: func(ctx context.Context, args json.RawMessage) (string, error) {
				return "", errors.New("something went wrong")
			},
		})
		res := reg.Execute(context.Background(), call("boom"))
		want := "error: something went wrong"
		if res != want {
			t.Errorf("Execute() = %q, want %q", res, want)
		}
	})

	// 5. Execute 的成功路径与 ExecuteErr 一致（单一实现的包装语义）
	t.Run("Execute success passes through", func(t *testing.T) {
		reg := DefaultRegistry()
		call := llm.ToolCall{
			Function: llm.ToolCallFn{Name: "calculate", Arguments: `{"expression": "6*7"}`},
		}
		if res := reg.Execute(context.Background(), call); res != "42" {
			t.Errorf("Execute() = %q, want %q", res, "42")
		}
	})

	// 6. 单一实现守卫：Execute 必须只是 ExecuteErr 的包装（"error: " + err），
	// 两处口径一旦漂移（各自实现一套压串逻辑）就在这里转红。
	t.Run("Execute stays a thin wrapper of ExecuteErr", func(t *testing.T) {
		reg := NewRegistry()
		reg.Register(&mockTool{
			name: "boom",
			fn: func(ctx context.Context, args json.RawMessage) (string, error) {
				return "", errors.New("kaboom")
			},
		})

		unknown := call("missing_tool")
		if _, err := reg.ExecuteErr(context.Background(), unknown); err == nil {
			t.Fatal("expected error for unknown tool")
		} else if got, want := reg.Execute(context.Background(), unknown), "error: "+err.Error(); got != want {
			t.Errorf("Execute(unknown tool) = %q, want %q (must be wrapper of ExecuteErr)", got, want)
		}

		known := call("boom")
		if _, err := reg.ExecuteErr(context.Background(), known); err == nil {
			t.Fatal("expected error from failing tool")
		} else if got, want := reg.Execute(context.Background(), known), "error: "+err.Error(); got != want {
			t.Errorf("Execute(tool error) = %q, want %q (must be wrapper of ExecuteErr)", got, want)
		}
	})
}
