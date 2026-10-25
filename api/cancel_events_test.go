package api

import (
	"agent-runtime/agent"
	"agent-runtime/store"
	"bufio"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newCancelMux() (*http.ServeMux, *store.Store, *fakeTaskRunner) {
	s := store.NewStore()
	fr := &fakeTaskRunner{
		traces:  make(map[string][]agent.TraceStep),
		buffers: make(map[string]*agent.TraceBuffer),
	}
	return NewHandler(s, fr, "").Routes(), s, fr
}

func TestHandleCancel_Accepted(t *testing.T) {
	mux, s, fr := newCancelMux()
	id := s.Create("long task")

	req := httptest.NewRequest("POST", "/tasks/"+id+"/cancel", strings.NewReader(`{"reason":"user_aborted!"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(fr.cancelled) != 1 {
		t.Fatalf("cancelled calls = %d, want 1", len(fr.cancelled))
	}
	if fr.cancelled[0].id != id {
		t.Errorf("cancelled id = %q, want %q", fr.cancelled[0].id, id)
	}
	// 非法字符被过滤，防止任意文案注入 task.error。
	if fr.cancelled[0].reason != "user_aborted" {
		t.Errorf("reason = %q, want sanitized %q", fr.cancelled[0].reason, "user_aborted")
	}
}

func TestHandleCancel_DefaultReason(t *testing.T) {
	mux, s, fr := newCancelMux()
	id := s.Create("task")

	req := httptest.NewRequest("POST", "/tasks/"+id+"/cancel", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202", rec.Code)
	}
	if fr.cancelled[0].reason != "user_aborted" {
		t.Errorf("reason = %q, want default %q", fr.cancelled[0].reason, "user_aborted")
	}
}

func TestHandleCancel_NotRunning(t *testing.T) {
	mux, s, fr := newCancelMux()
	id := s.Create("task")
	fr.cancelErr = errors.New("task is not running")

	req := httptest.NewRequest("POST", "/tasks/"+id+"/cancel", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("code = %d, want 409", rec.Code)
	}
}

func TestHandleCancel_NotFound(t *testing.T) {
	mux, _, _ := newCancelMux()

	req := httptest.NewRequest("POST", "/tasks/999/cancel", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", rec.Code)
	}
}

func TestHandleCancel_BadJSON(t *testing.T) {
	mux, s, _ := newCancelMux()
	id := s.Create("task")

	req := httptest.NewRequest("POST", "/tasks/"+id+"/cancel", strings.NewReader(`{"reason":`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rec.Code)
	}
}

// TestHandleEvents_ReplaysTerminalTask 验证 SSE 对已终态任务直接回放并收尾，
// 不挂起长连接（向下兼容轮询客户端的历史任务）。
func TestHandleEvents_ReplaysTerminalTask(t *testing.T) {
	mux, s, fr := newCancelMux()
	id := s.Create("done task")
	if err := s.Complete(id, "5", nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
	fr.traces[id] = []agent.TraceStep{
		{Step: 0, Kind: agent.KindThink, Detail: "calculating 2+3"},
		{Step: 0, Kind: agent.KindFinal, Detail: "5"},
	}

	req := httptest.NewRequest("GET", "/tasks/"+id+"/events", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	if strings.Count(body, "event: step") != 2 {
		t.Errorf("step frames = %d, want 2 (body=%q)", strings.Count(body, "event: step"), body)
	}
	if !strings.Contains(body, "event: done") || !strings.Contains(body, `"status":"done"`) {
		t.Errorf("missing done frame: %q", body)
	}
}

// TestHandleEvents_StreamsLiveSteps 用真实 httptest server 验证边执行边推送：
// 订阅后追加的步骤立即到达，任务终态后推送 done 并关闭连接。
func TestHandleEvents_StreamsLiveSteps(t *testing.T) {
	s := store.NewStore()
	id := s.Create("live task")
	fr := &fakeTaskRunner{
		traces:  make(map[string][]agent.TraceStep),
		buffers: map[string]*agent.TraceBuffer{id: agent.NewTraceBuffer(16)},
	}
	srv := httptest.NewServer(NewHandler(s, fr, "").Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/tasks/" + id + "/events")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	fr.buffers[id].Append(agent.TraceStep{Step: 0, Kind: agent.KindThink, Detail: "calculating 2+3"})
	fr.buffers[id].Append(agent.TraceStep{Step: 1, Kind: agent.KindToolCall, Tool: "calculate"})
	if err := s.Complete(id, "5", nil); err != nil {
		t.Fatalf("complete: %v", err)
	}

	var (
		body     strings.Builder
		steps    int
		sawDone  bool
		deadline = time.Now().Add(3 * time.Second)
	)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		body.WriteString(line + "\n")
		if line == "event: step" {
			steps++
		}
		if line == "event: done" {
			sawDone = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
	}

	if steps != 2 {
		t.Errorf("live step frames = %d, want 2 (body=%q)", steps, body.String())
	}
	if !sawDone {
		t.Errorf("did not receive done frame, body=%q", body.String())
	}
	if !strings.Contains(body.String(), `"detail":"calculating 2+3"`) {
		t.Errorf("missing live step payload, body=%q", body.String())
	}
}

// TestHandleEvents_StreamsLiveDeltas 验证 SSE 流式推送逐字增量文本（打字机效果）与步骤完成事件。
func TestHandleEvents_StreamsLiveDeltas(t *testing.T) {
	s := store.NewStore()
	id := s.Create("live delta task")
	tb := agent.NewTraceBuffer(16)
	fr := &fakeTaskRunner{
		traces:  make(map[string][]agent.TraceStep),
		buffers: map[string]*agent.TraceBuffer{id: tb},
	}
	srv := httptest.NewServer(NewHandler(s, fr, "").Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/tasks/" + id + "/events")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	time.Sleep(50 * time.Millisecond)

	tb.Append(agent.TraceStep{Step: 0, Kind: agent.KindThink, Detail: "thinking"})
	tb.AppendDelta(1, "calculating ")
	tb.AppendDelta(1, "2+3")
	tb.Append(agent.TraceStep{Step: 1, Kind: agent.KindFinal, Detail: "5"})
	if err := s.Complete(id, "5", nil); err != nil {
		t.Fatalf("complete: %v", err)
	}

	var (
		body        strings.Builder
		stepEvents  int
		deltaEvents []string
		sawDone     bool
		deadline    = time.Now().Add(3 * time.Second)
	)
	scanner := bufio.NewScanner(resp.Body)
	var currentEvent string
	for scanner.Scan() {
		line := scanner.Text()
		body.WriteString(line + "\n")
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			if currentEvent == "step" {
				stepEvents++
			} else if currentEvent == "done" {
				sawDone = true
				break
			}
		} else if strings.HasPrefix(line, "data: ") && currentEvent == "delta" {
			deltaEvents = append(deltaEvents, strings.TrimPrefix(line, "data: "))
			currentEvent = ""
		}
		if time.Now().After(deadline) {
			break
		}
	}

	if stepEvents != 2 {
		t.Errorf("step frames = %d, want 2 (body=%q)", stepEvents, body.String())
	}
	if len(deltaEvents) != 2 {
		t.Errorf("delta frames = %d, want 2 (body=%q)", len(deltaEvents), body.String())
	}
	wantDelta1 := `{"step":1,"text":"calculating "}`
	wantDelta2 := `{"step":1,"text":"2+3"}`
	if len(deltaEvents) >= 1 && deltaEvents[0] != wantDelta1 {
		t.Errorf("delta[0] = %q, want %q", deltaEvents[0], wantDelta1)
	}
	if len(deltaEvents) >= 2 && deltaEvents[1] != wantDelta2 {
		t.Errorf("delta[1] = %q, want %q", deltaEvents[1], wantDelta2)
	}
	if !sawDone {
		t.Errorf("did not receive done frame, body=%q", body.String())
	}
	if !strings.Contains(body.String(), `"detail":"thinking"`) {
		t.Errorf("missing step 0 thinking payload, body=%q", body.String())
	}
	if !strings.Contains(body.String(), `"detail":"5"`) {
		t.Errorf("missing step 1 final payload, body=%q", body.String())
	}
}
