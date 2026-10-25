package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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

// TestEmbed_Success 验证 Embed 打中 {base}/v1/embeddings：
// Bearer 鉴权用 embedAPIKey、请求体 {model, input}、响应 data[].embedding 解析为多维 float32。
func TestEmbed_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %v, want %v", r.Method, http.MethodPost)
		}
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %v, want /v1/embeddings", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer embed-key" {
			t.Errorf("Authorization = %v, want Bearer embed-key", got)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"text-embed-3"`) {
			t.Errorf("request body missing model: %s", body)
		}
		if !strings.Contains(string(body), `"input":["你好","world"]`) {
			t.Errorf("request body missing input: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"embedding":[0.5,-1.25,3,0]},{"embedding":[1,2,3,4]}]}`)
	}))
	defer srv.Close()

	c := NewClient(Config{EmbedBaseURL: srv.URL, EmbedAPIKey: "embed-key", EmbedModel: "text-embed-3"})
	got, err := c.Embed(context.Background(), []string{"你好", "world"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	want := [][]float32{{0.5, -1.25, 3, 0}, {1, 2, 3, 4}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Embed = %v, want %v", got, want)
	}
}

// TestEmbed_APIError 验证非 2xx → *APIError 且带 StatusCode。
func TestEmbed_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"boom"}`)
	}))
	defer srv.Close()

	c := NewClient(Config{EmbedBaseURL: srv.URL, EmbedAPIKey: "k", EmbedModel: "m"})
	_, err := c.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != 500 {
		t.Errorf("status = %d, want 500", apiErr.StatusCode)
	}
}

// TestEmbed_NotConfigured 验证向量通道默认关闭（ADR-0017 决策 3）：
// embed 配置缺失（全空或部分为空）→ ErrEmbedderNotConfigured，不静默降级。
func TestEmbed_NotConfigured(t *testing.T) {
	empty := NewClient(Config{})
	if _, err := empty.Embed(context.Background(), []string{"x"}); !errors.Is(err, ErrEmbedderNotConfigured) {
		t.Errorf("Embed with empty config = %v, want ErrEmbedderNotConfigured", err)
	}
	partial := NewClient(Config{EmbedBaseURL: "http://example.invalid", EmbedAPIKey: "k"}) // 缺 EmbedModel
	if _, err := partial.Embed(context.Background(), []string{"x"}); !errors.Is(err, ErrEmbedderNotConfigured) {
		t.Errorf("Embed with partial config = %v, want ErrEmbedderNotConfigured", err)
	}
}

// TestEmbed_ContextCancel 是变异自证 ④ 的承重墙：
// 预取消 ctx 必须中断请求（errors.Is context.Canceled）。
// 若 Embed 改用 http.NewRequest（不挂 ctx），请求会直达服务器且无错误 → 本用例转红。
func TestEmbed_ContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request must not reach the server when ctx is already cancelled")
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()

	c := NewClient(Config{EmbedBaseURL: srv.URL, EmbedAPIKey: "k", EmbedModel: "m"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预取消

	if _, err := c.Embed(ctx, []string{"x"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Embed with cancelled ctx = %v, want context.Canceled", err)
	}
}

// TestEmbed_EmptyTexts 验证空输入返回空切片且不发起请求（省一次无谓往返）。
func TestEmbed_EmptyTexts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("empty texts must not trigger a request")
	}))
	defer srv.Close()

	c := NewClient(Config{EmbedBaseURL: srv.URL, EmbedAPIKey: "k", EmbedModel: "m"})
	got, err := c.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("Embed(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Embed(nil) = %v, want empty slice", got)
	}
}

func TestJoinEndpoint(t *testing.T) {
	tests := []struct {
		baseURL      string
		endpointPath string
		want         string
	}{
		{
			baseURL:      "http://host/v1",
			endpointPath: "/v1/embeddings",
			want:         "http://host/v1/embeddings",
		},
		{
			baseURL:      "http://host",
			endpointPath: "/v1/embeddings",
			want:         "http://host/v1/embeddings",
		},
		{
			baseURL:      "http://host/v1/",
			endpointPath: "/v1/embeddings",
			want:         "http://host/v1/embeddings",
		},
		{
			baseURL:      "http://host/",
			endpointPath: "/v1/embeddings",
			want:         "http://host/v1/embeddings",
		},
		{
			baseURL:      "http://host/v1",
			endpointPath: "/v1/chat/completions",
			want:         "http://host/v1/chat/completions",
		},
		{
			baseURL:      "http://host",
			endpointPath: "/v1/chat/completions",
			want:         "http://host/v1/chat/completions",
		},
		{
			baseURL:      "http://host/v1/",
			endpointPath: "/v1/chat/completions",
			want:         "http://host/v1/chat/completions",
		},
		{
			baseURL:      "http://host/",
			endpointPath: "/v1/chat/completions",
			want:         "http://host/v1/chat/completions",
		},
		{
			baseURL:      "http://host/v1",
			endpointPath: "v1/chat/completions",
			want:         "http://host/v1/chat/completions",
		},
		{
			baseURL:      "http://host",
			endpointPath: "v1/chat/completions",
			want:         "http://host/v1/chat/completions",
		},
	}

	for _, tc := range tests {
		got := joinEndpoint(tc.baseURL, tc.endpointPath)
		if got != tc.want {
			t.Errorf("joinEndpoint(%q, %q) = %q, want %q", tc.baseURL, tc.endpointPath, got, tc.want)
		}
	}
}

func TestEmbed_KeylessLocalProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %v, want %v", r.Method, http.MethodPost)
		}
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %v, want /v1/embeddings", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization header should be omitted for keyless provider, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"embedding":[0.1,0.2,0.3]}]}`)
	}))
	defer srv.Close()

	// EmbedBaseURL 带 /v1，且 EmbedAPIKey 为空串
	c := NewClient(Config{
		EmbedBaseURL: srv.URL + "/v1",
		EmbedAPIKey:  "",
		EmbedModel:   "nomic-embed-text",
	})

	got, err := c.Embed(context.Background(), []string{"test document"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	want := [][]float32{{0.1, 0.2, 0.3}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Embed = %v, want %v", got, want)
	}
}

func TestChatWithTools_TemperatureAndSeed(t *testing.T) {
	t.Run("sends temperature and seed when configured", func(t *testing.T) {
		var receivedBody map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &receivedBody)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
		}))
		defer srv.Close()

		temp := 0.7
		seed := int64(12345)
		c := NewClient(Config{
			APIKey:      "test-key",
			BaseURL:     srv.URL,
			Model:       "test-model",
			Temperature: &temp,
			Seed:        &seed,
		})

		_, err := c.ChatWithTools(context.Background(), []Message{{Role: "user", Content: "hello"}}, nil)
		if err != nil {
			t.Fatalf("ChatWithTools: %v", err)
		}

		if got, ok := receivedBody["temperature"].(float64); !ok || got != 0.7 {
			t.Errorf("temperature = %v, want 0.7", receivedBody["temperature"])
		}
		if got, ok := receivedBody["seed"].(float64); !ok || int64(got) != 12345 {
			t.Errorf("seed = %v, want 12345", receivedBody["seed"])
		}
	})

	t.Run("omits temperature and seed when nil", func(t *testing.T) {
		var rawBody string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			rawBody = string(b)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
		}))
		defer srv.Close()

		c := NewClient(Config{
			APIKey:  "test-key",
			BaseURL: srv.URL,
			Model:   "test-model",
		})

		_, err := c.ChatWithTools(context.Background(), []Message{{Role: "user", Content: "hello"}}, nil)
		if err != nil {
			t.Fatalf("ChatWithTools: %v", err)
		}

		if strings.Contains(rawBody, `"temperature"`) {
			t.Errorf("rawBody should not contain temperature, got: %s", rawBody)
		}
		if strings.Contains(rawBody, `"seed"`) {
			t.Errorf("rawBody should not contain seed, got: %s", rawBody)
		}
	})
}

func TestChatWithTools_UsageParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"choices": [{"message": {"content": "result text"}}],
			"usage": {
				"prompt_tokens": 12,
				"completion_tokens": 5,
				"total_tokens": 17
			}
		}`)
	}))
	defer srv.Close()

	c := NewClient(Config{
		APIKey:  "test-key",
		BaseURL: srv.URL,
		Model:   "test-model",
	})

	turn, err := c.ChatWithTools(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("ChatWithTools: %v", err)
	}

	if turn.Content != "result text" {
		t.Errorf("Content = %q, want 'result text'", turn.Content)
	}
	wantUsage := Usage{
		PromptTokens:     12,
		CompletionTokens: 5,
		TotalTokens:      17,
	}
	if turn.Usage != wantUsage {
		t.Errorf("Usage = %+v, want %+v", turn.Usage, wantUsage)
	}
}
