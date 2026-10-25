package worker

import (
	"agent-runtime/agent"
	"agent-runtime/llm"
	"agent-runtime/store"
	"context"
	"fmt"
)

// SetSessionStore 注入会话存储接口，用于多轮对话历史拉取与写回。
func (p *Pool) SetSessionStore(ss store.SessionStore) {
	p.sessionStore = ss
}

// SetMaxContextTokens 设置会话历史上下文安全预算。
func (p *Pool) SetMaxContextTokens(tokens int) {
	p.maxContextTokens = tokens
}

// assembleSessionMessages 拉取指定 sessionID 的历史消息，与系统提示词和当前 prompt 组装，
// 并通过 agent.CompactMessages 执行滑动窗口压缩，返回符合大模型上下文预算的消息列表。
func (p *Pool) assembleSessionMessages(ctx context.Context, sessionID, prompt, sysPrompt string) ([]llm.Message, error) {
	if p.sessionStore == nil || sessionID == "" {
		return nil, nil
	}

	msgs, err := p.sessionStore.GetSessionMessages(ctx, sessionID, 100)
	if err != nil {
		return nil, err
	}

	rawMsgs := make([]llm.Message, 0, len(msgs)+2)
	if sysPrompt != "" {
		rawMsgs = append(rawMsgs, llm.Message{Role: "system", Content: sysPrompt})
	}
	for _, m := range msgs {
		rawMsgs = append(rawMsgs, llm.Message{Role: m.Role, Content: m.Content})
	}
	rawMsgs = append(rawMsgs, llm.Message{Role: "user", Content: prompt})

	maxTokens := p.maxContextTokens
	if maxTokens <= 0 {
		maxTokens = 16000
	}

	compacted := agent.CompactMessages(rawMsgs, agent.CompactionConfig{
		MaxContextTokens:     maxTokens,
		CompactionRatio:      0.5,
		EstimateTokenPerChar: 0.5,
	})
	return compacted, nil
}

// recordSessionTurn 在任务成功执行后，将本轮的 user 提示词与 assistant 回复追加至会话存储。
func (p *Pool) recordSessionTurn(ctx context.Context, sessionID, prompt, reply string) error {
	if p.sessionStore == nil || sessionID == "" {
		return nil
	}

	if err := p.sessionStore.AppendSessionMessage(ctx, store.SessionMessage{
		SessionID: sessionID,
		Role:      "user",
		Content:   prompt,
	}); err != nil {
		return fmt.Errorf("append user message: %w", err)
	}
	if err := p.sessionStore.AppendSessionMessage(ctx, store.SessionMessage{
		SessionID: sessionID,
		Role:      "assistant",
		Content:   reply,
	}); err != nil {
		return fmt.Errorf("append assistant message: %w", err)
	}
	return nil
}
