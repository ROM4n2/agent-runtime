package tool

import (
	"agent-api/llm"
	"encoding/json"
	"sort"
	"sync"
)

// Tool 定义了 Agent 工具必须实现的接口。
type Tool interface {
	Spec() llm.Tool
	Execute(args json.RawMessage) (string, error)
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

// Execute 按工具名执行；未知工具返回 "error: unknown tool " + call.Function.Name；
// 出错返回 "error: " + err.Error()；成功返回结果。绝不 panic。
func (r *Registry) Execute(call llm.ToolCall) string {
	t, ok := r.Get(call.Function.Name)
	if !ok {
		return "error: unknown tool " + call.Function.Name
	}
	out, err := t.Execute(json.RawMessage(call.Function.Arguments))
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}
