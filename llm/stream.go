package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// DeltaHandler 是接收模型增量文本内容的回调函数。
type DeltaHandler func(delta string)

// StreamingChatter 抽象支持流式增量推断的聊天客户端。
type StreamingChatter interface {
	ChatWithToolsStream(ctx context.Context, msgs []Message, tools []Tool, onDelta DeltaHandler) (*AssistantTurn, error)
}

var _ StreamingChatter = (*Client)(nil)

type streamChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

// ChatWithToolsStream 发送带工具定义的对话并启用 SSE 流式推断，
// 通过 onDelta 实时推送文本增量，并在流结束时返回完整组装的 AssistantTurn。
func (c *Client) ChatWithToolsStream(ctx context.Context, msgs []Message, tools []Tool, onDelta DeltaHandler) (*AssistantTurn, error) {
	// 请求体中包含 stream=true 以及 stream_options.include_usage，
	// 确保模型在流式结束前返回完整的 token 使用量（用于指标与成本统计）。
	reqBody := struct {
		Model         string    `json:"model"`
		Messages      []Message `json:"messages"`
		Tools         []Tool    `json:"tools,omitempty"`
		Temperature   *float64  `json:"temperature,omitempty"`
		Seed          *int64    `json:"seed,omitempty"`
		Stream        bool      `json:"stream"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options,omitempty"`
	}{
		Model:       c.model,
		Messages:    msgs,
		Tools:       tools,
		Temperature: c.temperature,
		Seed:        c.seed,
		Stream:      true,
		StreamOptions: &struct {
			IncludeUsage bool `json:"include_usage"`
		}{IncludeUsage: true},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}

	// 流式发送同样走 doWithRetry，但不变式不同：只在「响应体尚未被消费」时重试。
	// 一旦 delta 已经下发给客户端，任何重试都会把同一段文本再打一遍，
	// 打字机体验直接崩坏。doWithRetry 仅在拿到非 200 响应或连接建立失败时重试，
	// 天然满足该约束；200 之后的中途断流不重试，按失败处理（ADR-0021）。
	resp, err := c.doWithRetry(ctx, func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, joinEndpoint(c.baseURL, chatCompletionsPath), bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("llm: create request: %w", err)
		}
		// 显式指定 Accept: text/event-stream，告知中间反向代理和上游网关保持非缓冲流式下发
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		return httpReq, nil
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 针对非 200 响应，限制读取上限为 maxResponseBytes，防止错误日志被超大上游 body 打爆
	if resp.StatusCode != http.StatusOK {
		limited := io.LimitReader(resp.Body, maxResponseBytes)
		b, _ := io.ReadAll(limited)
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(b)}
	}

	var turn AssistantTurn
	toolCallsMap := make(map[int]*ToolCall)

	// 使用 bufio.Reader 逐行读取以避开 bufio.Scanner 默认 64KB 缓冲区上限导致的长 chunk 解析溢出
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("llm: read stream: %w", err)
		}

		line = strings.TrimRight(line, "\r\n")
		// 忽略空行以及以冒号开头的 SSE 协议心跳/注释行（例如 : keepalive）
		if line != "" && !strings.HasPrefix(line, ":") {
			if strings.HasPrefix(line, "data: ") || strings.HasPrefix(line, "data:") {
				var payload string
				if strings.HasPrefix(line, "data: ") {
					payload = strings.TrimPrefix(line, "data: ")
				} else {
					payload = strings.TrimPrefix(line, "data:")
				}
				payload = strings.TrimSpace(payload)

				// OpenAI / DeepSeek 流结束标志
				if payload == "[DONE]" {
					break
				}
				if payload != "" {
					var chunk streamChunk
					if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
						return nil, fmt.Errorf("llm: decode stream chunk: %w", err)
					}
					// 若 chunk 携带了 usage，则覆盖记录终态 token 统计
					if chunk.Usage != nil {
						turn.Usage = *chunk.Usage
					}
					for _, choice := range chunk.Choices {
						if choice.Delta.Content != "" {
							turn.Content += choice.Delta.Content
							if onDelta != nil {
								onDelta(choice.Delta.Content)
							}
						}
						// 累加多 chunk 的工具调用参数与定义
						for _, tc := range choice.Delta.ToolCalls {
							acc, exists := toolCallsMap[tc.Index]
							if !exists {
								acc = &ToolCall{
									ID:   tc.ID,
									Type: tc.Type,
									Function: ToolCallFn{
										Name:      tc.Function.Name,
										Arguments: tc.Function.Arguments,
									},
								}
								toolCallsMap[tc.Index] = acc
							} else {
								if tc.ID != "" {
									if acc.ID == "" {
										acc.ID = tc.ID
									} else if acc.ID != tc.ID {
										acc.ID += tc.ID
									}
								}
								if tc.Type != "" {
									acc.Type = tc.Type
								}
								if tc.Function.Name != "" {
									if acc.Function.Name == "" {
										acc.Function.Name = tc.Function.Name
									} else if acc.Function.Name != tc.Function.Name {
										acc.Function.Name += tc.Function.Name
									}
								}
								if tc.Function.Arguments != "" {
									acc.Function.Arguments += tc.Function.Arguments
								}
							}
						}
					}
				}
			}
		}

		if err == io.EOF {
			break
		}
	}

	// 保证工具调用按原始 index 升序输出，消除 map 乱序遍历导致的上层状态机执行漂移
	if len(toolCallsMap) > 0 {
		indices := make([]int, 0, len(toolCallsMap))
		for idx := range toolCallsMap {
			indices = append(indices, idx)
		}
		sort.Ints(indices)
		turn.ToolCalls = make([]ToolCall, 0, len(indices))
		for _, idx := range indices {
			tc := toolCallsMap[idx]
			if tc.Type == "" {
				tc.Type = "function"
			}
			turn.ToolCalls = append(turn.ToolCalls, *tc)
		}
	}

	return &turn, nil
}
