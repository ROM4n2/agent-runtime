package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const chatCompletionsPath = "/v1/chat/completions"
const embeddingsPath = "/v1/embeddings"
const maxResponseBytes = 1024 * 1024 // 1MB

// joinEndpoint 幂等拼接 baseURL 与 endpointPath，规范化处理尾随斜杠与 /v1 前缀重叠。
func joinEndpoint(baseURL, endpointPath string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.HasPrefix(endpointPath, "/") {
		endpointPath = "/" + endpointPath
	}
	if strings.HasSuffix(baseURL, "/v1") && strings.HasPrefix(endpointPath, "/v1/") {
		endpointPath = strings.TrimPrefix(endpointPath, "/v1")
	}
	return baseURL + endpointPath
}

// APIError 表示上游返回了非 2xx 状态码。
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("llm: status %d: %s", e.StatusCode, e.Body)
}

// Embedder 把一批文本嵌入为定长 float32 向量（dim 由配置方保证一致）。
// 定义在 llm（模型适配层）：retrieval 等上层只消费该接口，依赖单向（ADR-0013）。
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// ErrEmbedderNotConfigured 表示 embed 配置不完整。
// 向量通道默认关闭（ADR-0017 决策 3）：缺任一项即显式失败，由上层走词法通道，禁静默降级。
var ErrEmbedderNotConfigured = errors.New("llm: embedder not configured")

// 请求
type chatRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// 响应
type chatResponse struct {
	Choices []choice `json:"choices"`
}

type choice struct {
	Message message `json:"message"`
}

// Client 调用 OpenAI 兼容的 chat completions 接口。
type Client struct {
	apiKey       string
	baseURL      string
	model        string
	temperature  *float64
	seed         *int64
	http         *http.Client // 复用连接，且用来设超时

	// 向量通道独立凭证（与 chat 分离，ADR-0017 决策 3）：
	// embedBaseURL 或 embedModel 为空 → Embed 返回 ErrEmbedderNotConfigured。
	embedBaseURL string
	embedAPIKey  string
	embedModel   string
}

type Config struct {
	APIKey      string
	BaseURL     string
	Model       string
	Temperature *float64
	Seed        *int64
	HTTPTimeout time.Duration
	// 向量通道独立凭证：默认全空 = 仅词法检索（lexical only，ADR-0017 决策 3）。
	EmbedBaseURL string
	EmbedAPIKey  string
	EmbedModel   string
	EmbedDim     int
}

func NewClient(cfg Config) *Client {
	timeout := cfg.HTTPTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Client{
		apiKey:       cfg.APIKey,
		baseURL:      cfg.BaseURL,
		model:        cfg.Model,
		temperature:  cfg.Temperature,
		seed:         cfg.Seed,
		http:         &http.Client{Timeout: timeout},
		embedBaseURL: cfg.EmbedBaseURL,
		embedAPIKey:  cfg.EmbedAPIKey,
		embedModel:   cfg.EmbedModel,
	}
}

// Chat 发送 prompt，返回模型回复的文本。
func (c *Client) Chat(ctx context.Context, prompt string) (string, error) {
	// 1. 构造 chatRequest（model + 一条 user message）
	chatReq := chatRequest{
		Model: c.model,
		Messages: []message{
			{Role: "user", Content: prompt},
		},
	}

	// 2. json.Marshal 成 []byte
	chatReqJSON, err := json.Marshal(chatReq)
	if err != nil {
		return "", fmt.Errorf("llm: marshal request: %w", err)
	}

	// 3. http.NewRequestWithContext 建请求（POST、URL、body）
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, joinEndpoint(c.baseURL, chatCompletionsPath), bytes.NewReader(chatReqJSON))
	if err != nil {
		return "", fmt.Errorf("llm: create request: %w", err)
	}

	// 4. 设两个 header：Authorization、Content-Type
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	// 5. c.http.Do(req) 发送；检查 StatusCode
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("llm: send request: %w", err)
	}

	defer resp.Body.Close()
	// 限制读取的最大字节数，防止返回过大导致内存占用过高
	body := io.LimitReader(resp.Body, maxResponseBytes)

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(body)
		return "", &APIError{StatusCode: resp.StatusCode, Body: string(bodyBytes)}
	}

	// 6. json.Decode 响应 → 取 choices[0].message.content 返回
	var chatResp chatResponse
	if err := json.NewDecoder(body).Decode(&chatResp); err != nil {
		return "", fmt.Errorf("llm: decode response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return "", errors.New("llm: no choices in response")
	}
	return chatResp.Choices[0].Message.Content, nil
}

// Embed 调用 OpenAI 兼容的 embeddings 端点，把一批文本嵌入为 float32 向量。
// 与 Chat 同纪律：复用 c.http 与 maxResponseBytes 1MB 响应上限，非 2xx → APIError。
// 向量通道默认关闭（ADR-0017 决策 3）：embedBaseURL 或 embedModel 为空即 ErrEmbedderNotConfigured。
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	// 配置守卫先行：缺任一项必填即显式失败，不发起半截请求。允许 embedAPIKey 为空以支持 Ollama 等本地免密引擎。
	if c.embedBaseURL == "" || c.embedModel == "" {
		return nil, ErrEmbedderNotConfigured
	}
	if len(texts) == 0 {
		return [][]float32{}, nil // 空输入不发起请求
	}

	embedReq := struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}{Model: c.embedModel, Input: texts}

	reqJSON, err := json.Marshal(embedReq)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, joinEndpoint(c.embedBaseURL, embeddingsPath), bytes.NewReader(reqJSON))
	if err != nil {
		return nil, fmt.Errorf("llm: create request: %w", err)
	}
	if c.embedAPIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.embedAPIKey)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llm: send request: %w", err)
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, maxResponseBytes)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(limited)
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(b)}
	}

	var embedResp struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(limited).Decode(&embedResp); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w", err)
	}
	out := make([][]float32, len(embedResp.Data))
	for i, d := range embedResp.Data {
		out[i] = d.Embedding
	}
	return out, nil
}

// ---- 工具调用（Agent 能力） ----

// Message 是一条对话消息。Role 取值 "system"/"user"/"assistant"/"tool"。
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"` // tool 角色回填时用
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // assistant 请求调用工具时
}

// Tool 是注册给模型的函数工具描述（OpenAI 兼容格式）。
type Tool struct {
	Type     string       `json:"type"` // 固定 "function"
	Function FunctionSpec `json:"function"`
}

// FunctionSpec 描述一个函数的名称、用途与入参 JSON Schema。
type FunctionSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"` // JSON Schema 对象（如 map[string]any）
}

// ToolCall 是模型要求执行的工具调用。
type ToolCall struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"` // 固定 "function"
	Function ToolCallFn `json:"function"`
}

// ToolCallFn 携带被调函数的名字与 JSON 参数串。
type ToolCallFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串，需再 json.Unmarshal
}

// Usage 记录模型调用的 token 消耗。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// AssistantTurn 抽象模型一次返回：要么有最终文本，要么请求调用工具。
type AssistantTurn struct {
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls"`
	Usage     Usage      `json:"usage"`
}

// ChatWithTools 发送带工具定义的对话，返回模型本轮的回复。
// 若回复含 tool_calls，调用方需执行对应工具并把结果作为 role=tool 的消息回填，
// 再次调用本方法，直到 ToolCalls 为空（终态文本）。
func (c *Client) ChatWithTools(ctx context.Context, msgs []Message, tools []Tool) (*AssistantTurn, error) {
	reqBody := struct {
		Model       string    `json:"model"`
		Messages    []Message `json:"messages"`
		Tools       []Tool    `json:"tools,omitempty"`
		Temperature *float64  `json:"temperature,omitempty"`
		Seed        *int64    `json:"seed,omitempty"`
	}{
		Model:       c.model,
		Messages:    msgs,
		Tools:       tools,
		Temperature: c.temperature,
		Seed:        c.seed,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, joinEndpoint(c.baseURL, chatCompletionsPath), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llm: send request: %w", err)
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, maxResponseBytes)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(limited)
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(b)}
	}

	var parsed struct {
		Choices []struct {
			Message AssistantTurn `json:"message"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	if err := json.NewDecoder(limited).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, errors.New("llm: no choices in response")
	}
	turn := parsed.Choices[0].Message
	turn.Usage = parsed.Usage
	return &turn, nil
}
