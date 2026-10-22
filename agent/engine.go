package agent

import (
	"agent-runtime/agent/tool"
	"agent-runtime/llm"
	"context"
	"errors"
	"time"
)

// Chatter 抽象支持工具调用的 LLM 对话能力。
type Chatter interface {
	ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (*llm.AssistantTurn, error)
}

// MaxAgentSteps 限制 agent 单轮任务的工具调用轮数，防止模型陷入无限工具循环。
const MaxAgentSteps = 5

// retryBackoff 是可重试工具错误首次重试前的退避间隔。退避阶梯的下一档是 400ms，
// 但当前策略是「按 tc.ID 去重后只重试一次」，故只用到第一档。
const retryBackoff = 200 * time.Millisecond

// isRetryable 判断错误是否值得重试一次：仅当错误显式实现 tool.RetryableError
// 且 Retryable() 返回 true。未实现的错误一律 permanent——宁可少重试，
// 也不把确定性故障放大成重试风暴。
func isRetryable(err error) bool {
	var re tool.RetryableError
	return errors.As(err, &re) && re.Retryable()
}

// toolErrorClass 把工具错误映射成 trace 用的粗分类。
// 红线：trace 的 KindError detail 只存粗分类，错误全文只进 slog / 喂回模型的消息。
func toolErrorClass(err error) string {
	if isRetryable(err) {
		return "tool error: retryable"
	}
	return "tool error: permanent"
}

// waitBackoff 用 ctx 感知定时器等待 d：ctx 取消（用户取消 / 任务超时 / 停机）时
// 立即返回 false。禁止裸 time.Sleep——那会让取消语义无法穿透到工具重试路径。
func waitBackoff(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// Run 执行 think -> tool_call -> tool_result 的多步认知循环，直到模型给出终态文本或步数耗尽。
func Run(ctx context.Context, c Chatter, prompt string, reg *tool.Registry, onStep func(TraceStep)) (string, error) {
	var tools []llm.Tool
	if reg != nil {
		tools = reg.Specs()
	}

	// retried 按工具调用 ID 去重：同一个 tc.ID 最多重试一次，
	// 否则模型重复同一调用会把一次瞬时故障放大成重试风暴。
	retried := make(map[string]bool)

	// execWithRetry 执行工具调用；遇到可重试错误时按 tc.ID 去重后重试一次，
	// 退避用 ctx 感知定时器（ctx 取消则放弃重试，错误照常分类上报）。
	// 抽成闭包是为了让下面的循环体保持 guard clause 式平铺。
	execWithRetry := func(tc llm.ToolCall) (string, error) {
		if reg == nil {
			return "error: no tool registry", nil
		}
		res, err := reg.ExecuteErr(ctx, tc)
		if err == nil || !isRetryable(err) || retried[tc.ID] {
			return res, err
		}
		retried[tc.ID] = true
		if !waitBackoff(ctx, retryBackoff) {
			return res, err
		}
		return reg.ExecuteErr(ctx, tc)
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

			res, err := execWithRetry(tc)
			if err != nil {
				// 仍失败或 non-retryable：只把粗分类写进 trace，错误串喂回模型自纠。
				emit(KindError, tc.Function.Name, toolErrorClass(err), tc.ID)
				res = "error: " + err.Error()
			} else {
				emit(KindToolResult, tc.Function.Name, res, tc.ID)
			}

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
