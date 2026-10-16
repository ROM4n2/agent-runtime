package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTaskJSONContract 钉住 GET /tasks/{id} 的 JSON 字段名。
// demo.html 的 JS 按小写字段读取；若后端退回无 json tag 的默认序列化（首字母大写），
// 前端就会读到 undefined。此测试锁死这份前后端契约。
func TestTaskJSONContract(t *testing.T) {
	mux, _ := newTestMux()

	postReq := httptest.NewRequest("POST", "/run", strings.NewReader(`{"prompt":"hi"}`))
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

	getReq := httptest.NewRequest("GET", "/tasks/"+created.TaskID, nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(getRec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"id", "status", "prompt", "result", "error", "steps"} {
		if _, ok := raw[want]; !ok {
			t.Errorf("task JSON missing key %q; body = %s", want, getRec.Body.String())
		}
	}
	stepsRaw := strings.TrimSpace(string(raw["steps"]))
	if stepsRaw == "" || stepsRaw == "null" || !strings.HasPrefix(stepsRaw, "[") {
		t.Errorf("task JSON steps must be a non-null JSON array, got: %s", stepsRaw)
	}
}
