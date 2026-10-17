package agent

import (
	"agent-api/agent/tool"
	"agent-api/llm"
	"context"
	"errors"
)

// Chatter 抽象支持工具调用的 LLM 对话能力。
type Chatter interface {
	ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (*llm.AssistantTurn, error)
}

// MaxAgentSteps 限制 agent 单轮任务的工具调用轮数，防止模型陷入无限工具循环。
const MaxAgentSteps = 5

// Run 执行 think -> tool_call -> tool_result 的多步认知循环，直到模型给出终态文本或步数耗尽。
func Run(ctx context.Context, c Chatter, prompt string, reg *tool.Registry, onStep func(TraceStep)) (string, error) {
	var tools []llm.Tool
	if reg != nil {
		tools = reg.Specs()
	}

	exec := func(tc llm.ToolCall) string {
		if reg == nil {
			return "error: no tool registry"
		}
		return reg.Execute(tc)
	}

	turns := []llm.Message{{Role: "user", Content: prompt}}
	step := 0
	emit := func(kind, toolName, detail, callID string) {
		if onStep != nil {
			onStep(TraceStep{
				Step:   step,
				Kind:   kind,
				Tool:   toolName,
				Detail: detail,
				CallID: callID,
			})
		}
	}

	for ; step < MaxAgentSteps; step++ {
		resp, err := c.ChatWithTools(ctx, turns, tools)
		if err != nil {
			emit(KindError, "", "upstream error", "")
			return "", err
		}
		if resp == nil {
			emit(KindError, "", "upstream error", "")
			return "", errors.New("nil response from chatter")
		}

		if resp.Content != "" {
			emit(KindThink, "", resp.Content, "")
		}

		if len(resp.ToolCalls) == 0 {
			emit(KindFinal, "", resp.Content, "")
			return resp.Content, nil
		}

		turns = append(turns, llm.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})
		for _, tc := range resp.ToolCalls {
			emit(KindToolCall, tc.Function.Name, tc.Function.Arguments, tc.ID)
			res := exec(tc)
			emit(KindToolResult, tc.Function.Name, res, tc.ID)
			turns = append(turns, llm.Message{
				Role:       "tool",
				Content:    res,
				ToolCallID: tc.ID,
			})
		}
	}

	emit(KindError, "", "max steps exceeded", "")
	return "", errors.New("agent exceeded max steps")
}
