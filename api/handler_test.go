package api

import (
	"agent-api/llm"
	"agent-api/store"
	"agent-api/worker"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeLLM struct{}

func (fakeLLM) ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (*llm.AssistantTurn, error) {
	return &llm.AssistantTurn{Content: "fake response"}, nil
}

func newTestMux() (*http.ServeMux, *store.Store) {
	s := store.NewStore()
	p := worker.NewPool(3, 30*time.Second, s, fakeLLM{})
	h := NewHandler(s, p, "") // 空密钥 = 开发模式，放行
	return h.Routes(), s
}

func TestHandleRun_CreatesTask(t *testing.T) {
	mux, s := newTestMux()

	req, err := http.NewRequest("POST", "/run", strings.NewReader(`{"prompt":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	// 断言 1：状态码
	if rec.Code != http.StatusAccepted {
		t.Errorf("code = %d, want 202", rec.Code)
	}

	// 断言 2：解析 body 拿 ta
	var resp map[string]string
	err = json.NewDecoder(rec.Body).Decode(&resp)
	if err != nil {
		t.Fatal(err)
	}
	if resp["task_id"] == "" {
		t.Fatalf("task_id 为空")
	}

	// 断言 3：store 里有这任务，pending + prompt
	task, err := s.Get(resp["task_id"])
	if err != nil {
		t.Fatalf("Get 报错 %v", err)
	}
	if task.Prompt != "hi" {
		t.Errorf("prompt = %q, want hi", task.Prompt)
	}
	if task.Status != store.StatusPending {
		t.Errorf("status = %q, want pending", task.Status)
	}
}

func TestHandleRun_BadJSON(t *testing.T) {
	mux, _ := newTestMux()

	req, err := http.NewRequest("POST", "/run", strings.NewReader(`{"prompt"`))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rec.Code)
	}

}

func TestHandleRun_EmptyPrompt(t *testing.T) {
	mux, _ := newTestMux()
	req, err := http.NewRequest("POST", "/run", strings.NewReader(`{"prompt":""}`))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rec.Code)
	}
}

func TestHandleGet_Found(t *testing.T) {
	mux, _ := newTestMux()

	// POST 建任务，拿真 task_id
	postReq := httptest.NewRequest("POST", "/run", strings.NewReader(`{"prompt":"hi"}`))
	postRec := httptest.NewRecorder()
	mux.ServeHTTP(postRec, postReq)

	var resp map[string]string
	err := json.NewDecoder(postRec.Body).Decode(&resp)
	if err != nil {
		t.Fatal(err)
	}
	id := resp["task_id"]

	// GET 真单号 -> 应该 200
	getReq := httptest.NewRequest("GET", "/tasks/"+id, nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", getRec.Code)
	}

	// 解析 body 中的 status
	var body struct {
		Status string
	}

	err = json.NewDecoder(getRec.Body).Decode(&body)
	if err != nil {
		t.Fatal(err)
	}

	if body.Status == "" {
		t.Errorf("status 为空")
	}
}

func TestHandleGet_NotFound(t *testing.T) {
	mux, _ := newTestMux()
	req, err := http.NewRequest("GET", "/tasks/"+"123", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", rec.Code)
	}

}

// TestAuth_UsesInjectedKey 验证鉴权密钥来自注入而非环境变量，
// 配置链路（config → main → Handler）断了这里会红。
func TestAuth_UsesInjectedKey(t *testing.T) {
	s := store.NewStore()
	p := worker.NewPool(1, 30*time.Second, s, fakeLLM{})
	h := NewHandler(s, p, "s3cret")
	mux := h.Routes()

	noAuth := httptest.NewRequest("POST", "/run", strings.NewReader(`{"prompt":"hi"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, noAuth)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("无 token: code = %d, want 401", rec.Code)
	}

	withAuth := httptest.NewRequest("POST", "/run", strings.NewReader(`{"prompt":"hi"}`))
	withAuth.Header.Set("Authorization", "Bearer s3cret")
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, withAuth)
	if rec2.Code != http.StatusAccepted {
		t.Errorf("带正确 token: code = %d, want 202", rec2.Code)
	}
}

func TestHealthz(t *testing.T) {
	mux, _ := newTestMux()
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Errorf("body = %q, want ok", got)
	}
}

func TestMetrics(t *testing.T) {
	mux, _ := newTestMux()
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE agent_tasks_submitted counter",
		"# TYPE agent_tasks_running gauge",
		"agent_tasks_done",
		"agent_tasks_failed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q; got:\n%s", want, body)
		}
	}
}

func TestDemoPage(t *testing.T) {
	mux, _ := newTestMux()
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "agent-api") {
		t.Errorf("demo body missing expected marker")
	}
}

func TestHandleRun_QueueFullNoZombie(t *testing.T) {
	s := store.NewStore()
	p := worker.NewPool(0, 30*time.Second, s, fakeLLM{})
	h := NewHandler(s, p, "")
	mux := h.Routes()

	req := httptest.NewRequest("POST", "/run", strings.NewReader(`{"prompt":"test"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", rec.Code)
	}

	_, err := s.Get("1")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound for task 1 after enqueue failure, got err: %v", err)
	}
}
