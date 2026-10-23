package tool

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestCurrentTimeTool_Spec(t *testing.T) {
	tool := NewCurrentTimeTool()
	spec := tool.Spec()

	if spec.Type != "function" {
		t.Errorf("spec.Type = %q, want %q", spec.Type, "function")
	}
	if spec.Function.Name != "current_time" {
		t.Errorf("spec.Function.Name = %q, want %q", spec.Function.Name, "current_time")
	}
	if spec.Function.Description == "" {
		t.Error("spec.Function.Description is empty")
	}
	if spec.Function.Parameters == nil {
		t.Error("spec.Function.Parameters is nil")
	}
}

func TestCurrentTimeTool_Execute(t *testing.T) {
	tool := NewCurrentTimeTool()

	// 1. nil 参数 -> 默认 UTC
	t.Run("nil args defaults to UTC", func(t *testing.T) {
		out, err := tool.Execute(context.Background(), nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		parsed, err := time.Parse(time.RFC3339, out)
		if err != nil {
			t.Fatalf("failed to parse output as RFC3339: %v, raw: %q", err, out)
		}
		_, offset := parsed.Zone()
		if offset != 0 {
			t.Errorf("expected UTC offset 0, got %d", offset)
		}
	})

	// 2. 空 JSON 参数 -> 默认 UTC
	t.Run("empty json args defaults to UTC", func(t *testing.T) {
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		parsed, err := time.Parse(time.RFC3339, out)
		if err != nil {
			t.Fatalf("failed to parse output as RFC3339: %v, raw: %q", err, out)
		}
		_, offset := parsed.Zone()
		if offset != 0 {
			t.Errorf("expected UTC offset 0, got %d", offset)
		}
	})

	// 3. 合法时区 Asia/Shanghai -> offset +08:00
	t.Run("valid timezone Asia/Shanghai", func(t *testing.T) {
		out, err := tool.Execute(context.Background(), json.RawMessage(`{"timezone": "Asia/Shanghai"}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		parsed, err := time.Parse(time.RFC3339, out)
		if err != nil {
			t.Fatalf("failed to parse output as RFC3339: %v, raw: %q", err, out)
		}
		_, offset := parsed.Zone()
		if offset != 8*3600 {
			t.Errorf("expected Asia/Shanghai offset 28800 (+08:00), got %d", offset)
		}
	})

	// 4. 非法时区优雅降级 -> 回退到 UTC
	t.Run("invalid timezone graceful degradation to UTC", func(t *testing.T) {
		out, err := tool.Execute(context.Background(), json.RawMessage(`{"timezone": "NonExistent/Timezone"}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		parsed, err := time.Parse(time.RFC3339, out)
		if err != nil {
			t.Fatalf("failed to parse output as RFC3339: %v, raw: %q", err, out)
		}
		_, offset := parsed.Zone()
		if offset != 0 {
			t.Errorf("expected fallback to UTC offset 0, got %d", offset)
		}
	})

	// 5. 非法 JSON -> 忽略解析错误，降级回退到 UTC
	t.Run("malformed json fallback to UTC", func(t *testing.T) {
		out, err := tool.Execute(context.Background(), json.RawMessage(`not a json`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		parsed, err := time.Parse(time.RFC3339, out)
		if err != nil {
			t.Fatalf("failed to parse output as RFC3339: %v, raw: %q", err, out)
		}
		_, offset := parsed.Zone()
		if offset != 0 {
			t.Errorf("expected fallback to UTC offset 0, got %d", offset)
		}
	})
}
