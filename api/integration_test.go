package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-runtime/store"
	"agent-runtime/worker"
)

// TestIntegrationRunToPoll 跑通「提交 → 异步执行 → 轮询至完成」全链路，
// 验证 handler + store + pool + 中间件在真实联动下工作（用 fakeLLM 替代真实 LLM）。
func TestIntegrationRunToPoll(t *testing.T) {
	s := store.NewStore()
	p := worker.NewPool(3, 30*time.Second, s, fakeLLM{})
	p.Start()
	defer p.Stop()

	h := NewHandler(s, p, "") // 空密钥 = 开发模式，放行
	mux := h.Routes()

	// 提交任务
	postReq := httptest.NewRequest("POST", "/run", strings.NewReader(`{"prompt":"hello"}`))
	postRec := httptest.NewRecorder()
	mux.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusAccepted {
		t.Fatalf("run code = %d, want 202", postRec.Code)
	}
	var created struct {
		TaskID string `json:"task_id"`
	}
	if err := json.NewDecoder(postRec.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.TaskID == "" {
		t.Fatal("empty task_id")
	}

	// 轮询直到终态
	deadline := time.Now().Add(3 * time.Second)
	for {
		getReq := httptest.NewRequest("GET", "/tasks/"+created.TaskID, nil)
		getRec := httptest.NewRecorder()
		mux.ServeHTTP(getRec, getReq)

		var task struct {
			Status string `json:"status"`
			Result string `json:"result"`
		}
		if err := json.NewDecoder(getRec.Body).Decode(&task); err != nil {
			t.Fatal(err)
		}
		switch task.Status {
		case store.StatusDone:
			if task.Result != "fake response" {
				t.Fatalf("result = %q, want 'fake response'", task.Result)
			}
			// 顺带验证指标已累计
			metricsReq := httptest.NewRequest("GET", "/metrics", nil)
			metricsRec := httptest.NewRecorder()
			mux.ServeHTTP(metricsRec, metricsReq)
			if !strings.Contains(metricsRec.Body.String(), "agent_tasks_submitted") {
				t.Errorf("metrics missing submitted counter")
			}
			return
		case store.StatusFailed:
			t.Fatalf("task unexpectedly failed")
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for done, last status %q", task.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
