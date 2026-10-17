package tool

import (
	"agent-api/llm"
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
	fn   func(args json.RawMessage) (string, error)
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

func (m *mockTool) Execute(args json.RawMessage) (string, error) {
	if m.fn != nil {
		return m.fn(args)
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
		res := reg.Execute(call)
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
		res := reg.Execute(call)
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
		res := reg.Execute(call)
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
		res := reg.Execute(call)
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
			fn: func(args json.RawMessage) (string, error) {
				return "", errors.New("something went wrong")
			},
		})

		call := llm.ToolCall{
			Function: llm.ToolCallFn{
				Name:      "failing_tool",
				Arguments: `{}`,
			},
		}
		res := customReg.Execute(call)
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
				res := reg.Execute(call)
				expected := fmt.Sprintf("%d", idx+idx)
				if res != expected {
					t.Errorf("concurrent execute = %q, want %q", res, expected)
				}
			}(i)
		}
		wg.Wait()
	})
}
