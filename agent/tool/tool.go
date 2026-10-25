package tool

import (
	"agent-runtime/llm"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// RetryableError 由工具错误实现，向 agent.Run 表达「该错误值得重试一次」
// （如向量通道瞬时不可用）。未实现该接口的错误一律按 permanent 处理——
// 宁可少重试，也不把确定性故障放大成重试风暴。
type RetryableError interface {
	error
	Retryable() bool
}

// TerminalClassifier 由工具错误实现，声明该错误冒泡终止 agent.Run 时对应的任务级脱敏终态文案（如 "index missing"）。
type TerminalClassifier interface {
	error
	TerminalStatus() string
}

// Tool 定义了 Agent 工具必须实现的接口。
// Execute 携带 ctx：取消/超时必须能穿透到工具执行内部（ADR-0014、ADR-0017 决策 5），
// 否则 worker 的抢占式取消只能中断 LLM 调用，工具侧仍会跑满全程。
type Tool interface {
	Spec() llm.Tool
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry 管理已注册的工具集，并提供统一的查找与执行入口。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry 创建一个空的工具注册表。
func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

// Register 向注册表注册一个工具。若重名则覆盖。
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Spec().Function.Name] = t
}

// Get 根据工具名获取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Specs 返回当前已注册工具的 Spec 列表（按工具名稳定排序）。
func (r *Registry) Specs() []llm.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)

	specs := make([]llm.Tool, 0, len(names))
	for _, name := range names {
		specs = append(specs, r.tools[name].Spec())
	}
	return specs
}

// ExecuteErr 是原始错误出口：未知工具返回 error，工具错误原样返回（不压串），
// 供 agent.Run 按 RetryableError 分类做重试/放弃的分支决策（ADR-0017 决策 5）。
func (r *Registry) ExecuteErr(ctx context.Context, call llm.ToolCall) (string, error) {
	t, ok := r.Get(call.Function.Name)
	if !ok {
		return "", fmt.Errorf("unknown tool %s", call.Function.Name)
	}
	return t.Execute(ctx, json.RawMessage(call.Function.Arguments))
}

// Execute 是 ExecuteErr 的包装，沿用「错误压成字符串喂回模型」的既有语义
// （Errors-As-Messages：让模型看到错误并自纠，而不是让整轮任务失败）。
// 单一实现：压串逻辑只此一份，避免两份代码各自漂移。
func (r *Registry) Execute(ctx context.Context, call llm.ToolCall) string {
	out, err := r.ExecuteErr(ctx, call)
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}
