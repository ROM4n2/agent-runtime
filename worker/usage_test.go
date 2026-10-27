package worker

import (
	"agent-runtime/llm"
	"context"
	"errors"
	"testing"
	"time"
)

// usageChatter 返回带 usage 的终态回复（无工具调用 → 一步结束）。
type usageChatter struct {
	usage llm.Usage
}

func (u usageChatter) ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (*llm.AssistantTurn, error) {
	return &llm.AssistantTurn{Content: "ok", Usage: u.usage}, nil
}

// failAfterToolChatter 第一次返回「调用工具 + usage」，第二次返回错误。
// 用于验证「失败的调用照样计费」：用量在错误发生前已经产生。
type failAfterToolChatter struct {
	usage llm.Usage
	calls int
}

func (f *failAfterToolChatter) ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (*llm.AssistantTurn, error) {
	f.calls++
	if f.calls > 1 {
		return nil, errors.New("upstream boom")
	}
	return &llm.AssistantTurn{
		Usage: f.usage,
		ToolCalls: []llm.ToolCall{{
			ID:       "call_usage",
			Type:     "function",
			Function: llm.ToolCallFn{Name: "calculate", Arguments: `{"expression":"1+1"}`},
		}},
	}, nil
}

// TestProcess_RecordsUsageOnSuccess 是 ADR-0021 决策 2 的承重墙之一：
// 成功任务必须把 token 用量写进任务记录。
func TestProcess_RecordsUsageOnSuccess(t *testing.T) {
	mock := &mockTaskStore{}
	p := NewPool(1, time.Second, mock, usageChatter{usage: llm.Usage{PromptTokens: 11, CompletionTokens: 5, TotalTokens: 16}})
	p.process("task-usage")

	if !mock.usageCalled {
		t.Fatal("expected RecordUsage to be called, but it was not")
	}
	if mock.usageID != "task-usage" {
		t.Errorf("usage id = %q, want task-usage", mock.usageID)
	}
	if mock.usagePrompt != 11 || mock.usageCompletion != 5 {
		t.Errorf("usage = (%d,%d), want (11,5)", mock.usagePrompt, mock.usageCompletion)
	}
	if mock.completedErr != nil {
		t.Errorf("task should be done, got error %v", mock.completedErr)
	}
}

// TestProcess_RecordsUsageOnFailure 是成本口径的关键：失败的步进同样计费，
// 漏记失败任务的用量会让账单对不上。
func TestProcess_RecordsUsageOnFailure(t *testing.T) {
	mock := &mockTaskStore{}
	chatter := &failAfterToolChatter{usage: llm.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}}
	p := NewPool(1, time.Second, mock, chatter)
	p.process("task-fail-usage")

	if !mock.usageCalled {
		t.Fatal("expected RecordUsage to be called on the failure path, but it was not")
	}
	if mock.usagePrompt != 7 || mock.usageCompletion != 3 {
		t.Errorf("usage = (%d,%d), want (7,3)", mock.usagePrompt, mock.usageCompletion)
	}
	if mock.completedErr == nil {
		t.Error("expected task to be marked failed")
	}
}

// TestProcess_UsageRecordErrorKeepsTerminalState 守住「计量是旁路信号」：
// RecordUsage 失败只记日志，不得把成功的任务改成失败。
func TestProcess_UsageRecordErrorKeepsTerminalState(t *testing.T) {
	mock := &mockTaskStore{usageErr: errors.New("disk full")}
	p := NewPool(1, time.Second, mock, usageChatter{usage: llm.Usage{PromptTokens: 2, CompletionTokens: 1}})
	p.process("task-usage-err")

	if !mock.completeCalled {
		t.Fatal("expected Complete to be called")
	}
	if mock.completedErr != nil {
		t.Errorf("usage record failure must not fail the task, got %v", mock.completedErr)
	}
}

// TestProcess_SkipsUsageWhenUpstreamReportsNone 验证零用量不产生无意义的写操作：
// 上游未返回 usage（如部分兼容实现）时不应把 0 写成「已计量」。
func TestProcess_SkipsUsageWhenUpstreamReportsNone(t *testing.T) {
	mock := &mockTaskStore{}
	p := NewPool(1, time.Second, mock, usageChatter{})
	p.process("task-no-usage")

	if mock.usageCalled {
		t.Error("expected RecordUsage to be skipped when usage is zero")
	}
	if mock.completedErr != nil {
		t.Errorf("task should be done, got error %v", mock.completedErr)
	}
}

// TestSetSessionHistoryLimit 验证窗口配置只接受正数（0/负数回落到默认 100）。
func TestSetSessionHistoryLimit(t *testing.T) {
	p := NewPool(1, time.Second, &mockTaskStore{}, usageChatter{})
	if p.sessionHistoryLimit != 100 {
		t.Errorf("default = %d, want 100", p.sessionHistoryLimit)
	}
	p.SetSessionHistoryLimit(500)
	if p.sessionHistoryLimit != 500 {
		t.Errorf("after set = %d, want 500", p.sessionHistoryLimit)
	}
	p.SetSessionHistoryLimit(0)
	if p.sessionHistoryLimit != 500 {
		t.Errorf("zero must be ignored, got %d", p.sessionHistoryLimit)
	}
}
