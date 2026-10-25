package worker

import (
	"agent-runtime/llm"
	"agent-runtime/store"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingChatter struct {
	mu           sync.Mutex
	calls        [][]llm.Message
	replyContent string
}

func (r *recordingChatter) ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (*llm.AssistantTurn, error) {
	r.mu.Lock()
	copied := make([]llm.Message, len(msgs))
	copy(copied, msgs)
	r.calls = append(r.calls, copied)
	reply := r.replyContent
	r.mu.Unlock()
	if reply == "" {
		reply = "mock response"
	}
	return &llm.AssistantTurn{Content: reply}, nil
}

func (r *recordingChatter) getCalls() [][]llm.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	res := make([][]llm.Message, len(r.calls))
	copy(res, r.calls)
	return res
}

func waitForDone(t *testing.T, s store.TaskStore, id string, timeout time.Duration) store.Task {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, err := s.Get(id)
		if err == nil && (task.Status == store.StatusDone || task.Status == store.StatusFailed) {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach terminal status within %v", id, timeout)
	return store.Task{}
}

// TestSession_EmptySessionID 验证当 SessionID 为空时，任务正常执行且不读写任何会话存储。
func TestSession_EmptySessionID(t *testing.T) {
	s := store.NewStore()
	chatter := &recordingChatter{replyContent: "hello world"}
	p := NewPool(1, 2*time.Second, s, chatter)
	p.SetSessionStore(s)
	p.Start()
	defer p.Stop()

	id := s.Create("task without session")
	if err := p.Enqueue(id); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	task := waitForDone(t, s, id, 2*time.Second)
	if task.Status != store.StatusDone {
		t.Fatalf("task status = %s, want done; error: %s", task.Status, task.Error)
	}
	if task.Result != "hello world" {
		t.Fatalf("task result = %s, want 'hello world'", task.Result)
	}

	calls := chatter.getCalls()
	if len(calls) != 1 {
		t.Fatalf("chatter calls count = %d, want 1", len(calls))
	}
	if len(calls[0]) != 1 || calls[0][0].Role != "user" || calls[0][0].Content != "task without session" {
		t.Fatalf("unexpected msgs passed to LLM: %+v", calls[0])
	}

	ctx := context.Background()
	msgs, err := s.GetSessionMessages(ctx, "", 10)
	if err == nil && len(msgs) > 0 {
		t.Fatalf("expected 0 session messages for empty session, got %d", len(msgs))
	}
}

// TestSession_MultiRound 验证多轮任务执行：
// 第 1 轮产生用户提示词与 assistant 回复入库；
// 第 2 轮带入第 1 轮的历史记录组装至 HistoryMessages 发送给 LLM。
func TestSession_MultiRound(t *testing.T) {
	s := store.NewStore()
	chatter := &recordingChatter{replyContent: "Alice"}
	p := NewPool(1, 2*time.Second, s, chatter)
	p.SetSessionStore(s)
	p.Start()
	defer p.Stop()

	ctx := context.Background()
	sessID := "sess_test_1"

	// 轮次 1
	id1 := s.CreateInSession("my name is Alice", sessID)
	if err := p.Enqueue(id1); err != nil {
		t.Fatalf("enqueue 1 failed: %v", err)
	}
	task1 := waitForDone(t, s, id1, 2*time.Second)
	if task1.Status != store.StatusDone {
		t.Fatalf("task1 status = %s, want done; error: %s", task1.Status, task1.Error)
	}

	// 验证 sessionStore 已有 2 条记录 (user + assistant)
	sessMsgs, err := s.GetSessionMessages(ctx, sessID, 100)
	if err != nil {
		t.Fatalf("GetSessionMessages failed: %v", err)
	}
	if len(sessMsgs) != 2 {
		t.Fatalf("expected 2 session messages after turn 1, got %d", len(sessMsgs))
	}
	if sessMsgs[0].Role != "user" || sessMsgs[0].Content != "my name is Alice" {
		t.Errorf("turn 1 msg 0 = %+v, want user 'my name is Alice'", sessMsgs[0])
	}
	if sessMsgs[1].Role != "assistant" || sessMsgs[1].Content != "Alice" {
		t.Errorf("turn 1 msg 1 = %+v, want assistant 'Alice'", sessMsgs[1])
	}

	// 轮次 2
	chatter.replyContent = "Your name is Alice."
	id2 := s.CreateInSession("what is my name?", sessID)
	if err := p.Enqueue(id2); err != nil {
		t.Fatalf("enqueue 2 failed: %v", err)
	}
	task2 := waitForDone(t, s, id2, 2*time.Second)
	if task2.Status != store.StatusDone {
		t.Fatalf("task2 status = %s, want done; error: %s", task2.Status, task2.Error)
	}

	// 验证 LLM 第二轮调用收到了包含第一轮上下文的消息列表
	calls := chatter.getCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 chatter calls, got %d", len(calls))
	}
	call2Msgs := calls[1]
	if len(call2Msgs) != 3 {
		t.Fatalf("expected 3 messages in call 2 (user, assistant, user), got %d: %+v", len(call2Msgs), call2Msgs)
	}
	if call2Msgs[0].Role != "user" || call2Msgs[0].Content != "my name is Alice" {
		t.Errorf("call 2 msg 0 = %+v", call2Msgs[0])
	}
	if call2Msgs[1].Role != "assistant" || call2Msgs[1].Content != "Alice" {
		t.Errorf("call 2 msg 1 = %+v", call2Msgs[1])
	}
	if call2Msgs[2].Role != "user" || call2Msgs[2].Content != "what is my name?" {
		t.Errorf("call 2 msg 2 = %+v", call2Msgs[2])
	}

	// 验证 sessionStore 现在共有 4 条记录
	allMsgs, err := s.GetSessionMessages(ctx, sessID, 100)
	if err != nil {
		t.Fatalf("GetSessionMessages after turn 2 failed: %v", err)
	}
	if len(allMsgs) != 4 {
		t.Fatalf("expected 4 session messages after turn 2, got %d", len(allMsgs))
	}
}

// TestSession_CompactionApplied 验证当会话历史超出 maxContextTokens 预算时，
// 滑动窗口压缩被自动触发并插入系统折叠标记。
func TestSession_CompactionApplied(t *testing.T) {
	s := store.NewStore()
	chatter := &recordingChatter{replyContent: "acknowledged"}
	p := NewPool(1, 2*time.Second, s, chatter)
	p.SetSessionStore(s)
	// 设置极小的 Token 预算，强制触发折叠
	p.SetMaxContextTokens(40)
	p.Start()
	defer p.Stop()

	ctx := context.Background()
	sessID := "sess_compact_test"

	// 预置 6 轮较长对话历史（每条文本约 50 字符，远超 40 token 预算）
	for i := 1; i <= 6; i++ {
		_ = s.AppendSessionMessage(ctx, store.SessionMessage{
			SessionID: sessID,
			Role:      "user",
			Content:   fmt.Sprintf("This is a rather long user question for turn %d to consume token budget", i),
		})
		_ = s.AppendSessionMessage(ctx, store.SessionMessage{
			SessionID: sessID,
			Role:      "assistant",
			Content:   fmt.Sprintf("This is a rather long assistant answer for turn %d to consume token budget", i),
		})
	}

	id := s.CreateInSession("what was the summary of everything?", sessID)
	if err := p.Enqueue(id); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}
	task := waitForDone(t, s, id, 2*time.Second)
	if task.Status != store.StatusDone {
		t.Fatalf("task status = %s, want done; error: %s", task.Status, task.Error)
	}

	calls := chatter.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 chatter call, got %d", len(calls))
	}
	received := calls[0]

	// 必须包含折叠提示系统消息
	hasFoldNotice := false
	for _, m := range received {
		if m.Role == "system" && strings.Contains(m.Content, "历史对话已自动折叠") {
			hasFoldNotice = true
			break
		}
	}
	if !hasFoldNotice {
		t.Fatalf("expected fold notice in compacted messages, but none found: %+v", received)
	}

	// 最终一条必须是当前的 user prompt
	last := received[len(received)-1]
	if last.Role != "user" || last.Content != "what was the summary of everything?" {
		t.Fatalf("last message should be current prompt, got: %+v", last)
	}
}

// TestSession_NilSessionStore 验证当 SessionStore 为 nil 时，即使任务带有 SessionID 也能正常执行不崩溃。
func TestSession_NilSessionStore(t *testing.T) {
	s := store.NewStore()
	chatter := &recordingChatter{replyContent: "ok"}
	p := NewPool(1, 2*time.Second, s, chatter)
	// 显式不设置 SessionStore
	p.Start()
	defer p.Stop()

	id := s.CreateInSession("hello without session store", "sess_orphan")
	if err := p.Enqueue(id); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}
	task := waitForDone(t, s, id, 2*time.Second)
	if task.Status != store.StatusDone {
		t.Fatalf("task status = %s, want done; error: %s", task.Status, task.Error)
	}
	if task.Result != "ok" {
		t.Fatalf("task result = %s, want 'ok'", task.Result)
	}
}
