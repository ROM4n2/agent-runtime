package api

import (
	"agent-runtime/agent"
	"agent-runtime/store"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

func isTerminalStatus(status string) bool {
	return status == store.StatusDone || status == store.StatusFailed
}

// writeSSE 写一帧 SSE（event + JSON data）并立刻 Flush，确保低延迟逐帧到达客户端。
func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// HandleEvents 以 text/event-stream 实时推送任务步进与终态：
// GET /tasks/{id}/events。与原 GET /tasks/{id} 轮询接口并存（向下兼容）。
//
// 帧格式：
//
//	event: step    data: {"step":..,"kind":"think",...}
//	event: done    data: {"id":..,"status":"done|failed",...}
//
// 已终态任务直接回放 Trace 快照后推送 done，不挂起连接。
func (h *Handler) HandleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	task, err := h.store.Get(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "task not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// SSE 是长连接：清除 Server.WriteTimeout 的写截止时间，否则 30s 后被切断。
	// httptest.Recorder 等实现不支持时忽略（ErrNotSupported），不影响主流程。
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no") // 告知 Nginx 等反代不要缓冲
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	h.streamEvents(w, flusher, id, task, r.Context())
}

func (h *Handler) streamEvents(w http.ResponseWriter, flusher http.Flusher, id string, task store.Task, ctx context.Context) {
	var (
		events      <-chan agent.TraceStep
		unsubscribe func()
		sent        int
	)
	defer func() {
		if unsubscribe != nil {
			unsubscribe()
		}
	}()

	subscribe := func() {
		if h.runner == nil || events != nil {
			return
		}
		snapshot, ch, cancel, ok := h.runner.Subscribe(id)
		if !ok {
			return
		}
		unsubscribe = cancel
		events = ch
		for _, s := range snapshot {
			if err := writeSSE(w, flusher, "step", s); err != nil {
				return
			}
			sent++
		}
	}

	// 任务已终态：回放记录后立即收尾，不建立长连接。
	if isTerminalStatus(task.Status) {
		if h.runner != nil {
			snapshot := h.runner.Trace(id)
			for _, s := range snapshot {
				if err := writeSSE(w, flusher, "step", s); err != nil {
					return
				}
			}
		}
		_ = writeSSE(w, flusher, "done", task)
		return
	}

	subscribe()

	statusTicker := time.NewTicker(100 * time.Millisecond)
	defer statusTicker.Stop()
	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case s, ok := <-events:
			if !ok {
				events = nil // 已关闭，停止从该通道读取
				continue
			}
			if err := writeSSE(w, flusher, "step", s); err != nil {
				return
			}
			sent++

		case <-keepAlive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()

		case <-statusTicker.C:
			t, err := h.store.Get(id)
			if err != nil {
				return // 任务被删除或存储异常，静默断流
			}

			// 任务可能一开始还在队列里（trace 尚未注册），进入 running 后再订阅。
			if events == nil && !isTerminalStatus(t.Status) {
				subscribe()
			}

			if !isTerminalStatus(t.Status) {
				continue
			}

			// 终态：先把已缓冲的增量消费完，再用快照补齐可能因慢消费被丢弃的步骤。
		drain:
			for {
				select {
				case s, ok := <-events:
					if !ok {
						break drain
					}
					if err := writeSSE(w, flusher, "step", s); err != nil {
						return
					}
					sent++
				default:
					break drain
				}
			}
			if h.runner != nil {
				snapshot := h.runner.Trace(id)
				for sent < len(snapshot) {
					if err := writeSSE(w, flusher, "step", snapshot[sent]); err != nil {
						return
					}
					sent++
				}
			}
			_ = writeSSE(w, flusher, "done", t)
			return
		}
	}
}
