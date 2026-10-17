package tool

import (
	"agent-api/llm"
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

func (t *currentTimeTool) Execute(args json.RawMessage) (string, error) {
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
