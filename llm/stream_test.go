package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestChatWithToolsStream_TextStreaming(t *testing.T) {
	var receivedAuth, receivedContentType, receivedAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedContentType = r.Header.Get("Content-Type")
		receivedAccept = r.Header.Get("Accept")

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// 模拟 OpenAI/DeepSeek chunked SSE 响应
		flusher, ok := w.(http.Flusher)
		fmt.Fprint(w, ": keepalive\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		if ok {
			flusher.Flush()
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n")
		if ok {
			flusher.Flush()
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"!\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3,\"total_tokens\":13}}\n\n")
		if ok {
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if ok {
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := NewClient(Config{
		APIKey:  "test-api-key",
		BaseURL: srv.URL,
		Model:   "deepseek-chat",
	})

	var deltas []string
	onDelta := func(delta string) {
		deltas = append(deltas, delta)
	}

	turn, err := c.ChatWithToolsStream(context.Background(), []Message{{Role: "user", Content: "Hi"}}, nil, onDelta)
	if err != nil {
		t.Fatalf("ChatWithToolsStream failed: %v", err)
	}

	if receivedAuth != "Bearer test-api-key" {
		t.Errorf("Authorization header = %q, want %q", receivedAuth, "Bearer test-api-key")
	}
	if receivedContentType != "application/json" {
		t.Errorf("Content-Type header = %q, want %q", receivedContentType, "application/json")
	}
	if receivedAccept != "text/event-stream" {
		t.Errorf("Accept header = %q, want %q", receivedAccept, "text/event-stream")
	}

	wantDeltas := []string{"Hello", " world", "!"}
	if !reflect.DeepEqual(deltas, wantDeltas) {
		t.Errorf("deltas = %#v, want %#v", deltas, wantDeltas)
	}

	if turn.Content != "Hello world!" {
		t.Errorf("turn.Content = %q, want %q", turn.Content, "Hello world!")
	}

	wantUsage := Usage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13}
	if turn.Usage != wantUsage {
		t.Errorf("turn.Usage = %+v, want %+v", turn.Usage, wantUsage)
	}
}

func TestChatWithToolsStream_ToolCallsAssembly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_123\",\"type\":\"function\",\"function\":{\"name\":\"calculate\",\"arguments\":\"{\\\"expr\\\":\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"1+2\\\"}\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewClient(Config{
		APIKey:  "test-api-key",
		BaseURL: srv.URL,
		Model:   "deepseek-chat",
	})

	turn, err := c.ChatWithToolsStream(context.Background(), []Message{{Role: "user", Content: "calc 1+2"}}, nil, nil)
	if err != nil {
		t.Fatalf("ChatWithToolsStream failed: %v", err)
	}

	if len(turn.ToolCalls) != 1 {
		t.Fatalf("len(turn.ToolCalls) = %d, want 1", len(turn.ToolCalls))
	}

	tc := turn.ToolCalls[0]
	if tc.ID != "call_123" {
		t.Errorf("tc.ID = %q, want %q", tc.ID, "call_123")
	}
	if tc.Type != "function" {
		t.Errorf("tc.Type = %q, want %q", tc.Type, "function")
	}
	if tc.Function.Name != "calculate" {
		t.Errorf("tc.Function.Name = %q, want %q", tc.Function.Name, "calculate")
	}
	if tc.Function.Arguments != `{"expr":"1+2"}` {
		t.Errorf("tc.Function.Arguments = %q, want %q", tc.Function.Arguments, `{"expr":"1+2"}`)
	}
}

func TestChatWithToolsStream_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid_api_key"}`)
	}))
	defer srv.Close()

	c := NewClient(Config{
		APIKey:  "bad-key",
		BaseURL: srv.URL,
		Model:   "deepseek-chat",
	})

	turn, err := c.ChatWithToolsStream(context.Background(), []Message{{Role: "user", Content: "Hi"}}, nil, nil)
	if err == nil {
		t.Fatalf("expected error, got turn: %+v", turn)
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T (%v)", err, err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("apiErr.StatusCode = %d, want 401", apiErr.StatusCode)
	}
	if apiErr.Body != `{"error":"invalid_api_key"}` {
		t.Errorf("apiErr.Body = %q, want %q", apiErr.Body, `{"error":"invalid_api_key"}`)
	}
}

func TestChatWithToolsStream_EmptyStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 不写入任何 SSE 数据即结束
	}))
	defer srv.Close()

	c := NewClient(Config{
		APIKey:  "test-api-key",
		BaseURL: srv.URL,
		Model:   "deepseek-chat",
	})

	turn, err := c.ChatWithToolsStream(context.Background(), []Message{{Role: "user", Content: "Hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("ChatWithToolsStream returned unexpected error on empty stream: %v", err)
	}
	if turn == nil {
		t.Fatal("turn is nil")
	}
	if turn.Content != "" {
		t.Errorf("turn.Content = %q, want empty", turn.Content)
	}
	if len(turn.ToolCalls) != 0 {
		t.Errorf("turn.ToolCalls = %+v, want empty", turn.ToolCalls)
	}
}

func TestChatWithToolsStream_MultipleToolCallsSorted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// 模拟先下发 index=1 的 tool_call，再下发 index=0 的 tool_call
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_2\",\"type\":\"function\",\"function\":{\"name\":\"tool_b\",\"arguments\":\"{}\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"tool_a\",\"arguments\":\"{}\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewClient(Config{
		APIKey:  "test-api-key",
		BaseURL: srv.URL,
		Model:   "deepseek-chat",
	})

	turn, err := c.ChatWithToolsStream(context.Background(), []Message{{Role: "user", Content: "test"}}, nil, nil)
	if err != nil {
		t.Fatalf("ChatWithToolsStream failed: %v", err)
	}

	if len(turn.ToolCalls) != 2 {
		t.Fatalf("len(turn.ToolCalls) = %d, want 2", len(turn.ToolCalls))
	}
	if turn.ToolCalls[0].ID != "call_1" || turn.ToolCalls[0].Function.Name != "tool_a" {
		t.Errorf("turn.ToolCalls[0] = %+v, want call_1 / tool_a", turn.ToolCalls[0])
	}
	if turn.ToolCalls[1].ID != "call_2" || turn.ToolCalls[1].Function.Name != "tool_b" {
		t.Errorf("turn.ToolCalls[1] = %+v, want call_2 / tool_b", turn.ToolCalls[1])
	}
}
