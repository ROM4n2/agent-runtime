package agent

import (
	"strings"
	"testing"

	"agent-runtime/llm"
)

// Test 1: Messages within budget (EstimateTokens <= MaxContextTokens) returned untouched.
func TestCompactMessages_WithinBudget(t *testing.T) {
	cfg := CompactionConfig{
		MaxContextTokens:     1000,
		CompactionRatio:      0.5,
		EstimateTokenPerChar: 0.5,
	}

	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "round 1 user"},
		{Role: "assistant", Content: "round 1 reply"},
		{Role: "user", Content: "round 2 user"},
		{Role: "assistant", Content: "round 2 reply"},
		{Role: "user", Content: "round 3 user"},
	}

	got := CompactMessages(msgs, cfg)
	if len(got) != len(msgs) {
		t.Fatalf("expected %d messages, got %d", len(msgs), len(got))
	}
	for i := range msgs {
		if got[i].Role != msgs[i].Role || got[i].Content != msgs[i].Content {
			t.Errorf("msg[%d] mismatch: got %+v, want %+v", i, got[i], msgs[i])
		}
	}
}

// Test 2: Messages with no system prompt exceeding budget -> keeps trailing messages, drops oldest, inserts fold notice.
func TestCompactMessages_ExceedingBudget_NoSystemPrompt(t *testing.T) {
	cfg := CompactionConfig{
		MaxContextTokens:     10, // 预算极小，强制触发压缩
		CompactionRatio:      0.5,
		EstimateTokenPerChar: 0.5,
	}

	msgs := []llm.Message{
		{Role: "user", Content: "round 1 question: what is the weather today?"},
		{Role: "assistant", Content: "round 1 answer: it is sunny and warm."},
		{Role: "user", Content: "round 2 question: what should I wear?"},
		{Role: "assistant", Content: "round 2 answer: wear a t-shirt."},
		{Role: "user", Content: "round 3 question: thank you!"},
	}

	got := CompactMessages(msgs, cfg)

	// 预期：
	// 中间轮次有 2 轮（round 1, round 2），CompactionRatio=0.5 -> drop 1 轮, keep 1 轮
	// 首条消息为折叠系统提示：[系统提示：为避免上下文超限，更早的 1 轮历史对话已自动折叠]
	// 随后保留 round 2 (user + assistant)
	// 最后保留末尾最近一轮 (round 3 user)
	if len(got) == 0 {
		t.Fatal("expected non-empty messages")
	}

	if got[0].Role != "system" {
		t.Errorf("expected got[0].Role to be system, got %s", got[0].Role)
	}
	wantNotice := "[系统提示：为避免上下文超限，更早的 1 轮历史对话已自动折叠]"
	if got[0].Content != wantNotice {
		t.Errorf("got[0].Content = %q, want %q", got[0].Content, wantNotice)
	}

	// 校验末尾 user 消息必须绝对保留
	lastGot := got[len(got)-1]
	if lastGot.Role != "user" || lastGot.Content != "round 3 question: thank you!" {
		t.Errorf("trailing message mismatch: got %+v", lastGot)
	}

	// 校验 round 1 已被剔除，round 2 仍存在
	for _, m := range got {
		if strings.Contains(m.Content, "round 1") {
			t.Errorf("expected round 1 to be folded, but found: %s", m.Content)
		}
	}
	foundRound2 := false
	for _, m := range got {
		if strings.Contains(m.Content, "round 2 question") {
			foundRound2 = true
			break
		}
	}
	if !foundRound2 {
		t.Error("expected round 2 to be preserved")
	}
}

// Test 3: Messages with leading system prompt exceeding budget -> preserves system prompt at index 0, preserves final user message, folds middle.
func TestCompactMessages_ExceedingBudget_WithLeadingSystemPrompt(t *testing.T) {
	cfg := CompactionConfig{
		MaxContextTokens:     10,
		CompactionRatio:      0.5,
		EstimateTokenPerChar: 0.5,
	}

	sysPrompt := "You are a professional mathematician assistant."
	msgs := []llm.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: "round 1: compute 2+2"},
		{Role: "assistant", Content: "round 1: 4"},
		{Role: "user", Content: "round 2: compute 3*3"},
		{Role: "assistant", Content: "round 2: 9"},
		{Role: "user", Content: "round 3: compute 4*4"},
		{Role: "assistant", Content: "round 3: 16"},
		{Role: "user", Content: "round 4: compute 5*5"},
	}

	got := CompactMessages(msgs, cfg)

	// 中间轮次 3 轮（round 1, 2, 3）。CompactionRatio=0.5 -> keep int(3*0.5)=1 轮, drop 2 轮
	// 结构预期：
	// index 0: 系统提示词
	// index 1: 折叠占位符 (更早的 2 轮历史对话已自动折叠)
	// index 2..3: round 3
	// index 4: round 4
	if len(got) < 3 {
		t.Fatalf("expected at least 3 messages, got %d", len(got))
	}

	if got[0].Role != "system" || got[0].Content != sysPrompt {
		t.Errorf("system prompt at index 0 was corrupted: %+v", got[0])
	}

	wantNotice := "[系统提示：为避免上下文超限，更早的 2 轮历史对话已自动折叠]"
	if got[1].Role != "system" || got[1].Content != wantNotice {
		t.Errorf("fold notice mismatch: got %+v, want %q", got[1], wantNotice)
	}

	lastGot := got[len(got)-1]
	if lastGot.Role != "user" || lastGot.Content != "round 4: compute 5*5" {
		t.Errorf("trailing user message corrupted: %+v", lastGot)
	}

	// 确认 round 1 和 round 2 被折叠，round 3 保留
	for _, m := range got {
		if strings.Contains(m.Content, "round 1:") || strings.Contains(m.Content, "round 2:") {
			t.Errorf("found folded round in result: %s", m.Content)
		}
	}
	foundRound3 := false
	for _, m := range got {
		if strings.Contains(m.Content, "round 3: compute 4*4") {
			foundRound3 = true
			break
		}
	}
	if !foundRound3 {
		t.Error("expected round 3 to be preserved")
	}
}

// Test 4: Extreme edge cases: nil/empty slice, single message, already under limit.
func TestCompactMessages_EdgeCases(t *testing.T) {
	cfg := DefaultCompactionConfig()

	// nil 切片
	if got := CompactMessages(nil, cfg); got != nil {
		t.Errorf("expected nil for nil input, got %+v", got)
	}

	// 空切片
	empty := []llm.Message{}
	if got := CompactMessages(empty, cfg); len(got) != 0 {
		t.Errorf("expected empty for empty input, got %+v", got)
	}

	// 单条消息
	single := []llm.Message{{Role: "user", Content: "hi"}}
	if got := CompactMessages(single, cfg); len(got) != 1 || got[0].Content != "hi" {
		t.Errorf("expected untouched single message, got %+v", got)
	}

	// 单条超长系统消息（即使预算极小，单条也无法且不应折叠）
	smallCfg := CompactionConfig{MaxContextTokens: 1}
	singleSys := []llm.Message{{Role: "system", Content: "long system prompt"}}
	if got := CompactMessages(singleSys, smallCfg); len(got) != 1 || got[0].Content != "long system prompt" {
		t.Errorf("expected untouched single system message, got %+v", got)
	}

	// 仅包含 System + 1 轮 User（中间无可折叠轮次）
	twoMsgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "user prompt"},
	}
	if got := CompactMessages(twoMsgs, smallCfg); len(got) != 2 {
		t.Errorf("expected untouched two messages when no middle turns exist, got %d", len(got))
	}
}

// Test 5: Ratio validation (invalid ratio <= 0 or > 1 defaults to 0.5; MaxContextTokens <= 0 defaults to 16000).
func TestCompactMessages_ValidationAndDefaults(t *testing.T) {
	d := DefaultCompactionConfig()
	if d.MaxContextTokens != 16000 || d.CompactionRatio != 0.5 || d.EstimateTokenPerChar != 0.5 {
		t.Errorf("unexpected DefaultCompactionConfig: %+v", d)
	}

	// 验证非法配置触发默认值降级
	msgs := []llm.Message{
		{Role: "user", Content: "round 1"},
		{Role: "assistant", Content: "ans 1"},
		{Role: "user", Content: "round 2"},
		{Role: "assistant", Content: "ans 2"},
		{Role: "user", Content: "round 3"},
	}

	// 1. MaxContextTokens <= 0 时默认回退到 16000（此时 msgs 远小于 16000 tokens，不应被折叠）
	cfgInvalidMax := CompactionConfig{
		MaxContextTokens:     -10,
		CompactionRatio:      0.5,
		EstimateTokenPerChar: 0.5,
	}
	got1 := CompactMessages(msgs, cfgInvalidMax)
	if len(got1) != len(msgs) {
		t.Errorf("expected untouched messages when MaxContextTokens <= 0 defaults to 16000, got len=%d", len(got1))
	}

	// 2. CompactionRatio <= 0 或 > 1 时默认回退到 0.5
	cfgInvalidRatio := CompactionConfig{
		MaxContextTokens:     5,
		CompactionRatio:      -0.2, // 非法，回退到 0.5
		EstimateTokenPerChar: 0.5,
	}
	got2 := CompactMessages(msgs, cfgInvalidRatio)
	if len(got2) == 0 || got2[0].Role != "system" || !strings.Contains(got2[0].Content, "折叠") {
		t.Errorf("expected fold notice with default 0.5 ratio, got: %+v", got2)
	}

	cfgInvalidRatioHigh := CompactionConfig{
		MaxContextTokens:     5,
		CompactionRatio:      1.5, // 非法，回退到 0.5
		EstimateTokenPerChar: 0.5,
	}
	got3 := CompactMessages(msgs, cfgInvalidRatioHigh)
	if len(got3) == 0 || got3[0].Role != "system" || !strings.Contains(got3[0].Content, "折叠") {
		t.Errorf("expected fold notice with default 0.5 ratio, got: %+v", got3)
	}
}

// 补充验证：EstimateTokens 算法精度与边界
func TestEstimateTokens_RuneCalculation(t *testing.T) {
	cases := []struct {
		name  string
		msgs  []llm.Message
		ratio float64
		want  int
	}{
		{
			name:  "empty messages",
			msgs:  nil,
			ratio: 0.5,
			want:  0,
		},
		{
			name: "ascii text 10 runes * 0.5 = 5",
			msgs: []llm.Message{
				{Role: "user", Content: "0123456789"},
			},
			ratio: 0.5,
			want:  5,
		},
		{
			name: "chinese text 4 runes * 0.5 = 2",
			msgs: []llm.Message{
				{Role: "user", Content: "你好世界"},
			},
			ratio: 0.5,
			want:  2,
		},
		{
			name: "tool calls runes included",
			msgs: []llm.Message{
				{
					Role:    "assistant",
					Content: "calc", // 4 runes
					ToolCalls: []llm.ToolCall{
						{
							Function: llm.ToolCallFn{
								Name:      "calc", // 4 runes
								Arguments: "1+1",  // 3 runes
							},
						},
					},
				},
			},
			ratio: 0.5,
			want:  5, // (4 + 4 + 3) * 0.5 = 5.5 -> 5
		},
		{
			name: "invalid ratio defaults to 0.5",
			msgs: []llm.Message{
				{Role: "user", Content: "1234"},
			},
			ratio: -1.0,
			want:  2, // 4 * 0.5 = 2
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EstimateTokens(tc.msgs, tc.ratio)
			if got != tc.want {
				t.Errorf("EstimateTokens() = %d, want %d", got, tc.want)
			}
		})
	}
}

// 补充验证：复合轮次（含工具调用链）在折叠时不被内部切断
func TestCompactMessages_ToolCallsWithinTurnNotSplit(t *testing.T) {
	cfg := CompactionConfig{
		MaxContextTokens:     10,
		CompactionRatio:      0.5,
		EstimateTokenPerChar: 0.5,
	}

	msgs := []llm.Message{
		{Role: "user", Content: "round 1 user"},
		{Role: "assistant", Content: "round 1 reply"},
		// round 2 包含完整的工具调用三连（assistant call -> tool result -> assistant final）
		{Role: "user", Content: "round 2 user"},
		{
			Role: "assistant",
			ToolCalls: []llm.ToolCall{
				{ID: "call_1", Function: llm.ToolCallFn{Name: "calc", Arguments: "2+2"}},
			},
		},
		{Role: "tool", ToolCallID: "call_1", Content: "4"},
		{Role: "assistant", Content: "round 2 final reply: 4"},
		// 末尾 round 3
		{Role: "user", Content: "round 3 user"},
	}

	got := CompactMessages(msgs, cfg)

	// round 1 被折叠，round 2 必须完整保留（不能将工具调用与工具返回斩断）
	foundToolCall := false
	foundToolResult := false
	for _, m := range got {
		if len(m.ToolCalls) > 0 {
			foundToolCall = true
		}
		if m.Role == "tool" && m.ToolCallID == "call_1" {
			foundToolResult = true
		}
	}
	if !foundToolCall || !foundToolResult {
		t.Fatalf("turn with tool call was corrupted or split: foundToolCall=%v, foundToolResult=%v", foundToolCall, foundToolResult)
	}
}
