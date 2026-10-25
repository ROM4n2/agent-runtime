package api

import (
	"agent-runtime/agent"
	"agent-runtime/store"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type sessionMockRunner struct{}

func (sessionMockRunner) Enqueue(id string) error           { return nil }
func (sessionMockRunner) Trace(id string) []agent.TraceStep { return nil }
func (sessionMockRunner) Cancel(id, reason string) error    { return nil }
func (sessionMockRunner) Subscribe(id string) ([]agent.TraceStep, <-chan agent.TraceStep, func(), bool) {
	return nil, nil, func() {}, false
}
func (sessionMockRunner) SubscribeEvents(id string) ([]agent.TraceStep, <-chan agent.TraceEvent, func(), bool) {
	return nil, nil, func() {}, false
}

// TestHandleRun_WithSessionID 验证提交带 session_id 的任务时，
// 任务入库正确记录 SessionID，且应答 JSON 同时包含 task_id 与 session_id。
func TestHandleRun_WithSessionID(t *testing.T) {
	s := store.NewStore()
	runner := sessionMockRunner{}
	h := NewHandler(s, runner, "")
	mux := h.Routes()

	body := `{"prompt":"hello","session_id":"sess_test_1"}`
	req := httptest.NewRequest("POST", "/run", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected status 202, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}

	taskID := resp["task_id"]
	if taskID == "" {
		t.Fatalf("expected non-empty task_id in response")
	}
	if resp["session_id"] != "sess_test_1" {
		t.Fatalf("expected session_id sess_test_1, got %q", resp["session_id"])
	}

	task, err := s.Get(taskID)
	if err != nil {
		t.Fatalf("get task failed: %v", err)
	}
	if task.SessionID != "sess_test_1" {
		t.Fatalf("expected task.SessionID sess_test_1, got %q", task.SessionID)
	}
}

// TestHandleGetSession 验证查询会话元数据接口 GET /sessions/{id}，
// 不存在时自动创建并返回 200 OK 与非零时序。
func TestHandleGetSession(t *testing.T) {
	s := store.NewStore()
	runner := sessionMockRunner{}
	h := NewHandler(s, runner, "")
	mux := h.Routes()

	req := httptest.NewRequest("GET", "/sessions/sess_1", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var sess store.Session
	if err := json.NewDecoder(rec.Body).Decode(&sess); err != nil {
		t.Fatalf("decode session failed: %v", err)
	}

	if sess.ID != "sess_1" {
		t.Fatalf("expected session id sess_1, got %q", sess.ID)
	}
	if sess.CreatedAt.IsZero() {
		t.Fatalf("expected non-zero created_at")
	}
	if sess.UpdatedAt.IsZero() {
		t.Fatalf("expected non-zero updated_at")
	}
}

// TestHandleGetSessionMessages 验证会话消息查询接口 GET /sessions/{id}/messages：
// 包括全量 FIFO 顺序、limit 截取最新消息、以及空会话返回 [] 而非 null。
func TestHandleGetSessionMessages(t *testing.T) {
	s := store.NewStore()
	ctx := context.Background()
	_ = s.AppendSessionMessage(ctx, store.SessionMessage{SessionID: "sess_1", Role: "user", Content: "m1"})
	_ = s.AppendSessionMessage(ctx, store.SessionMessage{SessionID: "sess_1", Role: "assistant", Content: "m2"})
	_ = s.AppendSessionMessage(ctx, store.SessionMessage{SessionID: "sess_1", Role: "user", Content: "m3"})

	runner := sessionMockRunner{}
	h := NewHandler(s, runner, "")
	mux := h.Routes()

	// 1. 获取全量 3 条消息，验证 FIFO 顺序
	req1 := httptest.NewRequest("GET", "/sessions/sess_1/messages", nil)
	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body: %s)", rec1.Code, rec1.Body.String())
	}

	var msgs1 []store.SessionMessage
	if err := json.NewDecoder(rec1.Body).Decode(&msgs1); err != nil {
		t.Fatalf("decode messages failed: %v", err)
	}
	if len(msgs1) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs1))
	}
	if msgs1[0].Content != "m1" || msgs1[1].Content != "m2" || msgs1[2].Content != "m3" {
		t.Fatalf("unexpected message order: %+v", msgs1)
	}

	// 2. 带 limit=2，验证返回最新的 2 条消息（升序）
	req2 := httptest.NewRequest("GET", "/sessions/sess_1/messages?limit=2", nil)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body: %s)", rec2.Code, rec2.Body.String())
	}

	var msgs2 []store.SessionMessage
	if err := json.NewDecoder(rec2.Body).Decode(&msgs2); err != nil {
		t.Fatalf("decode messages failed: %v", err)
	}
	if len(msgs2) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs2))
	}
	if msgs2[0].Content != "m2" || msgs2[1].Content != "m3" {
		t.Fatalf("unexpected message slice with limit: %+v", msgs2)
	}

	// 3. 空消息会话返回 [] 而非 null
	reqEmpty := httptest.NewRequest("GET", "/sessions/sess_empty/messages", nil)
	recEmpty := httptest.NewRecorder()
	mux.ServeHTTP(recEmpty, reqEmpty)
	if recEmpty.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body: %s)", recEmpty.Code, recEmpty.Body.String())
	}
	raw := strings.TrimSpace(recEmpty.Body.String())
	if raw != "[]" {
		t.Fatalf("expected JSON [] for empty messages, got %q", raw)
	}
}

// TestSessionRoutes_AuthProtected 验证会话路由受 Auth 中间件保护：
// 未传 Token 返回 401，携带合法 Token 返回 200。
func TestSessionRoutes_AuthProtected(t *testing.T) {
	s := store.NewStore()
	runner := sessionMockRunner{}
	h := NewHandler(s, runner, "test-secret-key")
	mux := h.Routes()

	// 1. 无 Authorization 请求 GET /sessions/sess_1 -> 401
	reqNoAuth := httptest.NewRequest("GET", "/sessions/sess_1", nil)
	recNoAuth := httptest.NewRecorder()
	mux.ServeHTTP(recNoAuth, reqNoAuth)

	if recNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", recNoAuth.Code)
	}

	// 2. 带有效 Bearer Token -> 200
	reqWithAuth := httptest.NewRequest("GET", "/sessions/sess_1", nil)
	reqWithAuth.Header.Set("Authorization", "Bearer test-secret-key")
	recWithAuth := httptest.NewRecorder()
	mux.ServeHTTP(recWithAuth, reqWithAuth)

	if recWithAuth.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", recWithAuth.Code)
	}

	// 3. 无 Authorization 请求 GET /sessions/sess_1/messages -> 401
	reqMsgNoAuth := httptest.NewRequest("GET", "/sessions/sess_1/messages", nil)
	recMsgNoAuth := httptest.NewRecorder()
	mux.ServeHTTP(recMsgNoAuth, reqMsgNoAuth)

	if recMsgNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for messages, got %d", recMsgNoAuth.Code)
	}
}

type dummyTaskStoreWithoutSession struct {
	store.TaskStore
}

// TestHandleGetSession_UnsupportedStore 验证当底座 store 未实现 SessionStore 时返回 501。
func TestHandleGetSession_UnsupportedStore(t *testing.T) {
	h := NewHandler(dummyTaskStoreWithoutSession{}, sessionMockRunner{}, "")
	mux := h.Routes()

	req := httptest.NewRequest("GET", "/sessions/sess_1", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 Not Implemented, got %d", rec.Code)
	}
}

// TestHandleGetSessionMessages_UnsupportedStore 验证当底座 store 未实现 SessionStore 时获取消息返回 501。
func TestHandleGetSessionMessages_UnsupportedStore(t *testing.T) {
	h := NewHandler(dummyTaskStoreWithoutSession{}, sessionMockRunner{}, "")
	mux := h.Routes()

	req := httptest.NewRequest("GET", "/sessions/sess_1/messages", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 Not Implemented, got %d", rec.Code)
	}
}
