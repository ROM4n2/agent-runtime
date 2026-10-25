package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestSQLStore_ImplementsSessionStore(t *testing.T) {
	var _ SessionStore = (*SQLStore)(nil)
}

func TestSQLStore_GetOrCreateSession(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "session_test.db")

	store, err := NewSQLStore(dbPath)
	if err != nil {
		t.Fatalf("failed to open sql store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// 1. 创建新 Session
	sess1, err := store.GetOrCreateSession(ctx, "sql-sess-1")
	if err != nil {
		t.Fatalf("failed to get or create session: %v", err)
	}
	if sess1.ID != "sql-sess-1" {
		t.Errorf("expected ID 'sql-sess-1', got %q", sess1.ID)
	}
	if sess1.CreatedAt.IsZero() || sess1.UpdatedAt.IsZero() {
		t.Errorf("timestamps should not be zero: %+v", sess1)
	}

	// 2. 获取已有 Session，CreatedAt 与 UpdatedAt 保持一致
	sess2, err := store.GetOrCreateSession(ctx, "sql-sess-1")
	if err != nil {
		t.Fatalf("failed to get existing session: %v", err)
	}
	if sess2.ID != sess1.ID {
		t.Errorf("expected ID %q, got %q", sess1.ID, sess2.ID)
	}
	if !sess2.CreatedAt.Equal(sess1.CreatedAt) {
		t.Errorf("expected CreatedAt %v, got %v", sess1.CreatedAt, sess2.CreatedAt)
	}

	// 3. 空 ID 校验
	if _, err := store.GetOrCreateSession(ctx, ""); err == nil {
		t.Errorf("expected error for empty session id, got nil")
	}

	// 4. Context 取消校验
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.GetOrCreateSession(canceledCtx, "sql-sess-canceled"); err == nil {
		t.Errorf("expected context cancellation error, got nil")
	}
}

func TestSQLStore_AppendAndGetSessionMessages_FIFO(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "session_fifo.db")

	store, err := NewSQLStore(dbPath)
	if err != nil {
		t.Fatalf("failed to open sql store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sessID := "sql-fifo"
	sess, err := store.GetOrCreateSession(ctx, sessID)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// 初始查询空切片
	msgs, err := store.GetSessionMessages(ctx, sessID, 10)
	if err != nil {
		t.Fatalf("failed to get messages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(msgs))
	}

	// 追加 3 条消息
	m1 := SessionMessage{
		SessionID: sessID,
		Role:      "user",
		Content:   "Query 1",
	}
	m2 := SessionMessage{
		SessionID: sessID,
		Role:      "assistant",
		Content:   "Answer 1",
	}
	m3 := SessionMessage{
		SessionID: sessID,
		Role:      "user",
		Content:   "Query 2",
	}

	if err := store.AppendSessionMessage(ctx, m1); err != nil {
		t.Fatalf("failed to append m1: %v", err)
	}
	if err := store.AppendSessionMessage(ctx, m2); err != nil {
		t.Fatalf("failed to append m2: %v", err)
	}
	if err := store.AppendSessionMessage(ctx, m3); err != nil {
		t.Fatalf("failed to append m3: %v", err)
	}

	// 验证 session updated_at 更新
	sessAfter, err := store.GetOrCreateSession(ctx, sessID)
	if err != nil {
		t.Fatalf("failed to get session: %v", err)
	}
	if sessAfter.UpdatedAt.Before(sess.UpdatedAt) {
		t.Errorf("expected updated_at to advance, got before")
	}

	// 查询消息并验证严格递增 ID 与 FIFO
	msgs, err = store.GetSessionMessages(ctx, sessID, 10)
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
		{"user", "Query 1"},
		{"assistant", "Answer 1"},
		{"user", "Query 2"},
	}

	for i, exp := range expected {
		if msgs[i].Role != exp.role || msgs[i].Content != exp.content {
			t.Errorf("msg[%d] mismatch: got role=%s content=%s, want role=%s content=%s",
				i, msgs[i].Role, msgs[i].Content, exp.role, exp.content)
		}
		if msgs[i].ID != int64(i+1) {
			t.Errorf("msg[%d] expected ID %d, got %d", i, i+1, msgs[i].ID)
		}
		if msgs[i].CreatedAt.IsZero() {
			t.Errorf("msg[%d] CreatedAt is zero", i)
		}
	}
}

func TestSQLStore_LimitPagination(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "session_limit.db")

	store, err := NewSQLStore(dbPath)
	if err != nil {
		t.Fatalf("failed to open sql store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sessID := "sql-limit"
	_, _ = store.GetOrCreateSession(ctx, sessID)

	for i := 1; i <= 6; i++ {
		err := store.AppendSessionMessage(ctx, SessionMessage{
			SessionID: sessID,
			Role:      "user",
			Content:   fmt.Sprintf("item-%d", i),
		})
		if err != nil {
			t.Fatalf("failed to append item %d: %v", i, err)
		}
	}

	// 1. limit = 3 应返回最新的 3 条消息，且按 ASC 排序（item-4, item-5, item-6）
	msgs, err := store.GetSessionMessages(ctx, sessID, 3)
	if err != nil {
		t.Fatalf("failed to get limited messages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	if msgs[0].Content != "item-4" || msgs[1].Content != "item-5" || msgs[2].Content != "item-6" {
		t.Errorf("unexpected messages slice: %+v", msgs)
	}

	// 2. limit <= 0 默认取最多 100 条（此处全 6 条）
	defaultMsgs, err := store.GetSessionMessages(ctx, sessID, 0)
	if err != nil {
		t.Fatalf("failed to get default messages: %v", err)
	}
	if len(defaultMsgs) != 6 {
		t.Fatalf("expected 6 messages, got %d", len(defaultMsgs))
	}
	if defaultMsgs[0].Content != "item-1" || defaultMsgs[5].Content != "item-6" {
		t.Errorf("unexpected default messages order: %+v", defaultMsgs)
	}
}

func TestSQLStore_PersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "session_persist.db")

	ctx := context.Background()
	sessID := "persist-sess"

	// 第一次打开并写入
	store1, err := NewSQLStore(dbPath)
	if err != nil {
		t.Fatalf("open store1 failed: %v", err)
	}
	sessCreated, err := store1.GetOrCreateSession(ctx, sessID)
	if err != nil {
		t.Fatalf("create session failed: %v", err)
	}
	err = store1.AppendSessionMessage(ctx, SessionMessage{
		SessionID: sessID,
		Role:      "user",
		Content:   "Persisted query",
	})
	if err != nil {
		t.Fatalf("append message failed: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("close store1 failed: %v", err)
	}

	// 第二次打开同一个文件，验证数据完好保留
	store2, err := NewSQLStore(dbPath)
	if err != nil {
		t.Fatalf("open store2 failed: %v", err)
	}
	defer store2.Close()

	sessLoaded, err := store2.GetOrCreateSession(ctx, sessID)
	if err != nil {
		t.Fatalf("load session failed: %v", err)
	}
	if sessLoaded.ID != sessCreated.ID {
		t.Errorf("expected ID %s, got %s", sessCreated.ID, sessLoaded.ID)
	}
	if !sessLoaded.CreatedAt.Equal(sessCreated.CreatedAt) {
		t.Errorf("expected CreatedAt %v, got %v", sessCreated.CreatedAt, sessLoaded.CreatedAt)
	}

	msgs, err := store2.GetSessionMessages(ctx, sessID, 10)
	if err != nil {
		t.Fatalf("get messages failed: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Content != "Persisted query" || msgs[0].Role != "user" {
		t.Errorf("unexpected message content: %+v", msgs[0])
	}
}

func TestSQLStore_ConcurrentAppends(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "session_concurrent.db")

	store, err := NewSQLStore(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sessID := "sql-concurrent"
	_, _ = store.GetOrCreateSession(ctx, sessID)

	const n = 30
	var wg sync.WaitGroup
	wg.Add(n)

	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			_ = store.AppendSessionMessage(ctx, SessionMessage{
				SessionID: sessID,
				Role:      "user",
				Content:   fmt.Sprintf("concurrent-%d", idx),
			})
		}(i)
	}
	wg.Wait()

	msgs, err := store.GetSessionMessages(ctx, sessID, 100)
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

func TestSQLStore_ValidationAndContext(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "session_validation.db")

	store, err := NewSQLStore(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// 1. 空 SessionID 追加报错
	err = store.AppendSessionMessage(ctx, SessionMessage{
		SessionID: "",
		Role:      "user",
		Content:   "missing id",
	})
	if err == nil {
		t.Errorf("expected error for empty SessionID, got nil")
	}

	// 2. Context 取消校验
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	err = store.AppendSessionMessage(canceledCtx, SessionMessage{
		SessionID: "some-sess",
		Role:      "user",
		Content:   "canceled",
	})
	if err == nil {
		t.Errorf("expected context cancellation error, got nil")
	}
}

// TestSQLStore_CreateInSession 验证 SQLite 中任务绑定会话创建及 SessionID 字段持久化与获取。
func TestSQLStore_CreateInSession(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_session_task.db")
	store, err := NewSQLStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLStore failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	id := store.CreateInSession("sql prompt", "sess_sql_123")
	if id == "" {
		t.Fatal("expected non-empty task ID")
	}

	task, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if task.SessionID != "sess_sql_123" {
		t.Errorf("task.SessionID = %q, want %q", task.SessionID, "sess_sql_123")
	}

	// 验证 ClaimNext 同样能扫描出 session_id
	claimed, err := store.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext failed: %v", err)
	}
	if claimed.SessionID != "sess_sql_123" {
		t.Errorf("claimed.SessionID = %q, want %q", claimed.SessionID, "sess_sql_123")
	}
}
