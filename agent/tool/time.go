package tool

import (
	"agent-runtime/llm"
	"context"
	"encoding/json"
	"time"
)

type currentTimeTool struct{}

// NewCurrentTimeTool 返回当前时间工具实例。
func NewCurrentTimeTool() Tool {
	return &currentTimeTool{}
}

func (t *currentTimeTool) Spec() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.FunctionSpec{
			Name:        "current_time",
			Description: "Return the current date and time. Optionally pass an IANA timezone name, e.g. \"Asia/Shanghai\".",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"timezone": map[string]any{"type": "string", "description": "IANA timezone name. Defaults to UTC."},
				},
			},
		},
	}
}

// Execute 实现 Tool 接口。ctx 当前未被使用（取系统时间无阻塞点），
// 保留参数以保持取消语义在所有工具上一致（接口只加不改）。
func (t *currentTimeTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Timezone string `json:"timezone"`
	}
	_ = json.Unmarshal(args, &p) // timezone 可选，忽略解析错误
	loc := time.UTC
	if p.Timezone != "" {
		if l, err := time.LoadLocation(p.Timezone); err == nil {
			loc = l
		}
	}
	return time.Now().In(loc).Format(time.RFC3339), nil
}
