package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestChat_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"hello"}}]}`)

		if r.Method != http.MethodPost {
			t.Errorf("method = %v, want %v", r.Method, http.MethodPost)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %v, want %v", r.URL.Path, "/v1/chat/completions")
		}

		want := "Bearer test-key"
		if got := r.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization = %v, want %v", got, want)
		}

	}))
	defer srv.Close()

	c := NewClient(Config{APIKey: "test-key", BaseURL: srv.URL, Model: "DeepSeek"})

	got, err := c.Chat(context.Background(), "hi")
	if err != nil {
		t.Errorf("Chat: %v", err)
	}
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}

}

func TestChat_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"401","message":"Unauthorized"}}`)
	}))
	defer srv.Close()

	c := NewClient(Config{APIKey: "test-key", BaseURL: srv.URL, Model: "DeepSeek"})

	_, err := c.Chat(context.Background(), "hi")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != 401 {
		t.Errorf("status = %d, want 401", apiErr.StatusCode)
	}

}

func TestChat_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"hello"}}]}`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	c := NewClient(Config{APIKey: "test-key", BaseURL: srv.URL, Model: "DeepSeek"})
	_, err := c.Chat(ctx, "hi")

	// 断言：errors.Is(err, context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Chat: %v", err)
	}

}

func TestNewClient_HTTPTimeout(t *testing.T) {
	cDefault := NewClient(Config{})
	if cDefault.http.Timeout != 60*time.Second {
		t.Errorf("default http.Timeout = %v, want 60s", cDefault.http.Timeout)
	}

	cCustom := NewClient(Config{HTTPTimeout: 15 * time.Second})
	if cCustom.http.Timeout != 15*time.Second {
		t.Errorf("custom http.Timeout = %v, want 15s", cCustom.http.Timeout)
	}
}
