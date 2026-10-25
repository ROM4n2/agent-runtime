package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestStore_ImplementsSessionStore(t *testing.T) {
	var _ SessionStore = (*Store)(nil)
}

func TestStore_GetOrCreateSession(t *testing.T) {
	ctx := context.Background()
	s := NewStore()

	// 1. 创建不存在的 Session
	sess1, err := s.GetOrCreateSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("unexpected error creating session: %v", err)
	}
	if sess1.ID != "session-1" {
		t.Errorf("expected session ID 'session-1', got %q", sess1.ID)
	}
	if sess1.CreatedAt.IsZero() || sess1.UpdatedAt.IsZero() {
		t.Errorf("timestamps should not be zero: %+v", sess1)
	}
	if sess1.CreatedAt.Location() != time.UTC {
		t.Errorf("expected UTC location for CreatedAt, got %v", sess1.CreatedAt.Location())
	}

	// 2. 再次获取应返回已有 Session（时间戳保持一致）
	sess2, err := s.GetOrCreateSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("unexpected error getting existing session: %v", err)
	}
	if sess2.ID != sess1.ID {
		t.Errorf("expected ID %q, got %q", sess1.ID, sess2.ID)
	}
	if !sess2.CreatedAt.Equal(sess1.CreatedAt) {
		t.Errorf("expected CreatedAt %v, got %v", sess1.CreatedAt, sess2.CreatedAt)
	}
	if !sess2.UpdatedAt.Equal(sess1.UpdatedAt) {
		t.Errorf("expected UpdatedAt %v, got %v", sess1.UpdatedAt, sess2.UpdatedAt)
	}

	// 3. 空 ID 校验
	if _, err := s.GetOrCreateSession(ctx, ""); err == nil {
		t.Errorf("expected error when passing empty session id, got nil")
	}

	// 4. Context 取消校验
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.GetOrCreateSession(canceledCtx, "session-canceled"); err == nil {
		t.Errorf("expected context cancellation error, got nil")
	}
}

func TestStore_AppendAndGetSessionMessages_FIFO(t *testing.T) {
	ctx := context.Background()
	s := NewStore()

	sess, err := s.GetOrCreateSession(ctx, "session-fifo")
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// 初始消息为空
	msgs, err := s.GetSessionMessages(ctx, "session-fifo", 100)
	if err != nil {
		t.Fatalf("failed to get empty messages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(msgs))
	}

	// 追加第一条消息
	m1 := SessionMessage{
		SessionID: "session-fifo",
		Role:      "user",
		Content:   "Hello Agent",
	}
	if err := s.AppendSessionMessage(ctx, m1); err != nil {
		t.Fatalf("failed to append m1: %v", err)
	}

	// 验证 session 的 UpdatedAt 被刷新
	sessAfterM1, err := s.GetOrCreateSession(ctx, "session-fifo")
	if err != nil {
		t.Fatalf("failed to get session: %v", err)
	}
	if sessAfterM1.UpdatedAt.Before(sess.UpdatedAt) {
		t.Errorf("expected UpdatedAt to advance, got before")
	}

	// 追加第二条与第三条消息
	m2 := SessionMessage{
		SessionID: "session-fifo",
		Role:      "assistant",
		Content:   "Hello! How can I help?",
	}
	m3 := SessionMessage{
		SessionID: "session-fifo",
		Role:      "user",
		Content:   "Show me ADR-0015",
	}
	if err := s.AppendSessionMessage(ctx, m2); err != nil {
		t.Fatalf("failed to append m2: %v", err)
	}
	if err := s.AppendSessionMessage(ctx, m3); err != nil {
		t.Fatalf("failed to append m3: %v", err)
	}

	// 读取消息流并验证严格 FIFO 顺序与自增 ID
	msgs, err = s.GetSessionMessages(ctx, "session-fifo", 10)
	if err != nil {
		t.Fatalf("failed to get messages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}

	expected := []struct {
		role    string
		content string
	}{
		{"user", "Hello Agent"},
		{"assistant", "Hello! How can I help?"},
		{"user", "Show me ADR-0015"},
	}

	for i, exp := range expected {
		if msgs[i].Role != exp.role || msgs[i].Content != exp.content {
			t.Errorf("msg[%d] mismatch: got role=%s content=%s, want role=%s content=%s",
				i, msgs[i].Role, msgs[i].Content, exp.role, exp.content)
		}
		if msgs[i].ID != int64(i+1) {
			t.Errorf("msg[%d] expected ID %d, got %d", i, i+1, msgs[i].ID)
		}
		if msgs[i].SessionID != "session-fifo" {
			t.Errorf("msg[%d] unexpected session ID: %s", i, msgs[i].SessionID)
		}
		if msgs[i].CreatedAt.IsZero() {
			t.Errorf("msg[%d] CreatedAt is zero", i)
		}
	}
}

func TestStore_GetSessionMessages_PaginationLimit(t *testing.T) {
	ctx := context.Background()
	s := NewStore()

	sessID := "session-limit"
	_, _ = s.GetOrCreateSession(ctx, sessID)

	for i := 1; i <= 5; i++ {
		err := s.AppendSessionMessage(ctx, SessionMessage{
			SessionID: sessID,
			Role:      "user",
			Content:   fmt.Sprintf("msg-%d", i),
		})
		if err != nil {
			t.Fatalf("failed to append msg %d: %v", i, err)
		}
	}

	// 1. limit = 3 应返回最新的 3 条消息（按 ASC 升序排列：msg-3, msg-4, msg-5）
	msgs, err := s.GetSessionMessages(ctx, sessID, 3)
	if err != nil {
		t.Fatalf("failed to get limited messages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	if msgs[0].Content != "msg-3" || msgs[1].Content != "msg-4" || msgs[2].Content != "msg-5" {
		t.Errorf("unexpected messages slice: %+v", msgs)
	}

	// 2. limit <= 0 应返回全部 5 条消息
	allMsgs, err := s.GetSessionMessages(ctx, sessID, 0)
	if err != nil {
		t.Fatalf("failed to get all messages: %v", err)
	}
	if len(allMsgs) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(allMsgs))
	}

	// 3. limit 大于消息总数时返回全部 5 条
	largeMsgs, err := s.GetSessionMessages(ctx, sessID, 100)
	if err != nil {
		t.Fatalf("failed to get large limit messages: %v", err)
	}
	if len(largeMsgs) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(largeMsgs))
	}

	// 4. 查询不存在的 session 返回空切片非 nil
	emptyMsgs, err := s.GetSessionMessages(ctx, "non-existent-session", 10)
	if err != nil {
		t.Fatalf("failed to query non existent session: %v", err)
	}
	if emptyMsgs == nil || len(emptyMsgs) != 0 {
		t.Errorf("expected empty non-nil slice, got: %#v", emptyMsgs)
	}
}

func TestStore_AppendSessionMessage_Validation(t *testing.T) {
	ctx := context.Background()
	s := NewStore()

	// 1. 空 SessionID 校验
	err := s.AppendSessionMessage(ctx, SessionMessage{
		SessionID: "",
		Role:      "user",
		Content:   "invalid",
	})
	if err == nil {
		t.Errorf("expected error for empty session_id, got nil")
	}

	// 2. CreatedAt 为零值时自动填充 UTC now
	tZero := time.Time{}
	err = s.AppendSessionMessage(ctx, SessionMessage{
		SessionID: "sess-val",
		Role:      "user",
		Content:   "auto time",
		CreatedAt: tZero,
	})
	if err != nil {
		t.Fatalf("unexpected append error: %v", err)
	}
	msgs, err := s.GetSessionMessages(ctx, "sess-val", 1)
	if err != nil {
		t.Fatalf("unexpected get error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 msg, got %d", len(msgs))
	}
	if msgs[0].CreatedAt.IsZero() {
		t.Errorf("expected auto populated CreatedAt, got zero")
	}
	if msgs[0].CreatedAt.Location() != time.UTC {
		t.Errorf("expected UTC location, got %v", msgs[0].CreatedAt.Location())
	}

	// 3. Context 取消校验
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	err = s.AppendSessionMessage(canceledCtx, SessionMessage{
		SessionID: "sess-val",
		Role:      "user",
		Content:   "canceled",
	})
	if err == nil {
		t.Errorf("expected context cancellation error, got nil")
	}
}

func TestStore_ConcurrentSessionAppends(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	sessID := "concurrent-sess"
	_, _ = s.GetOrCreateSession(ctx, sessID)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)

	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			_ = s.AppendSessionMessage(ctx, SessionMessage{
				SessionID: sessID,
				Role:      "user",
				Content:   fmt.Sprintf("msg-%d", idx),
			})
		}(i)
	}
	wg.Wait()

	msgs, err := s.GetSessionMessages(ctx, sessID, 100)
	if err != nil {
		t.Fatalf("failed to get messages: %v", err)
	}
	if len(msgs) != n {
		t.Fatalf("expected %d messages, got %d", n, len(msgs))
	}

	// 验证自增 ID 唯一且无重复
	idSeen := make(map[int64]bool)
	for _, m := range msgs {
		if idSeen[m.ID] {
			t.Errorf("duplicate message ID: %d", m.ID)
		}
		idSeen[m.ID] = true
	}
}
