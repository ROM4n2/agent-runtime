package agent

import (
	"fmt"
	"unicode/utf8"

	"agent-runtime/llm"
)

// CompactionConfig 控制上下文估算与压缩策略。
type CompactionConfig struct {
	MaxContextTokens     int     // 安全上下文 Token 预算上限，默认 16000
	CompactionRatio      float64 // 截断压缩目标保留比例，默认 0.5
	EstimateTokenPerChar float64 // 字符估算比例，默认 0.5 (中英文混合估算)
}

// DefaultCompactionConfig 返回标准的上下文压缩配置。
func DefaultCompactionConfig() CompactionConfig {
	return CompactionConfig{
		MaxContextTokens:     16000,
		CompactionRatio:      0.5,
		EstimateTokenPerChar: 0.5,
	}
}

// EstimateTokens 快速、无堆分配地估算一批消息的总 Token 消耗。
// 统计 Content 以及工具调用中的名称与入参字符数，避免字符串拼接带来的 GC 开销。
func EstimateTokens(msgs []llm.Message, ratio float64) int {
	if ratio <= 0 || ratio > 1 {
		ratio = 0.5
	}
	totalChars := 0
	for i := range msgs {
		totalChars += utf8.RuneCountInString(msgs[i].Content)
		for j := range msgs[i].ToolCalls {
			totalChars += utf8.RuneCountInString(msgs[i].ToolCalls[j].Function.Name)
			totalChars += utf8.RuneCountInString(msgs[i].ToolCalls[j].Function.Arguments)
		}
	}
	return int(float64(totalChars) * ratio)
}

// CompactMessages 对消息历史执行安全滑动窗口压缩：
// 1. 若估算总 Token <= cfg.MaxContextTokens，原样返回输入切片。
// 2. 若超限：
//    - 绝对保留开头的 System 消息（若第一个消息 Role=="system"）；
//    - 绝对保留末尾最近的 1 轮消息（最后一条 User 消息以及紧随其后的内容）；
//    - 对中间的历史消息，按 cfg.CompactionRatio 折半截断丢弃较早轮次，
//      并在截断处插入一条包含省略标记的系统级消息：
//      `[系统提示：为避免上下文超限，更早的 N 轮历史对话已自动折叠]`；
//    - 确保压缩后返回的消息切片有效且符合大模型交互时序。
func CompactMessages(msgs []llm.Message, cfg CompactionConfig) []llm.Message {
	if len(msgs) <= 1 {
		return msgs
	}

	// 防御性校验并应用默认参数，防止非法零值或负值引发截断异常
	if cfg.MaxContextTokens <= 0 {
		cfg.MaxContextTokens = 16000
	}
	if cfg.CompactionRatio <= 0 || cfg.CompactionRatio > 1 {
		cfg.CompactionRatio = 0.5
	}
	if cfg.EstimateTokenPerChar <= 0 || cfg.EstimateTokenPerChar > 1 {
		cfg.EstimateTokenPerChar = 0.5
	}

	// 未超限时原样直通，零内存拷贝
	if EstimateTokens(msgs, cfg.EstimateTokenPerChar) <= cfg.MaxContextTokens {
		return msgs
	}

	hasSys := msgs[0].Role == "system"
	historyStart := 0
	if hasSys {
		historyStart = 1
	}

	// 逆序查找最后一条 user 消息起始索引，定义最新的一轮对话（包含用户意图及后续可能的模型回答或工具调用）
	lastUserIdx := -1
	for i := len(msgs) - 1; i >= historyStart; i-- {
		if msgs[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}

	// 若没有 user 消息，或最后一条 user 消息已经是首条历史，则中间无可折叠轮次，原样返回
	if lastUserIdx <= historyStart {
		return msgs
	}

	// 将中间历史消息按「用户提问为轮次边界」划分为完整的轮次单元，防止把工具调用与工具返回斩断导致模型 API 报错
	middle := msgs[historyStart:lastUserIdx]
	var turns [][]llm.Message
	var curTurn []llm.Message
	for _, m := range middle {
		if m.Role == "user" && len(curTurn) > 0 {
			turns = append(turns, curTurn)
			curTurn = nil
		}
		curTurn = append(curTurn, m)
	}
	if len(curTurn) > 0 {
		turns = append(turns, curTurn)
	}

	if len(turns) == 0 {
		return msgs
	}

	totalTurns := len(turns)
	keepCount := int(float64(totalTurns) * cfg.CompactionRatio)
	dropCount := totalTurns - keepCount
	if dropCount <= 0 {
		dropCount = 1
		keepCount = totalTurns - 1
	}
	if dropCount > totalTurns {
		dropCount = totalTurns
		keepCount = 0
	}

	noticeMsg := llm.Message{
		Role:    "system",
		Content: fmt.Sprintf("[系统提示：为避免上下文超限，更早的 %d 轮历史对话已自动折叠]", dropCount),
	}

	// 预分配切片容量，避免动态扩容带来的二次内存拷贝
	out := make([]llm.Message, 0, 1+1+len(middle)+len(msgs)-lastUserIdx)
	if hasSys {
		out = append(out, msgs[0])
	}
	out = append(out, noticeMsg)
	for i := dropCount; i < totalTurns; i++ {
		out = append(out, turns[i]...)
	}
	out = append(out, msgs[lastUserIdx:]...)

	return out
}
