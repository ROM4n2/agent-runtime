package tool

// DefaultRegistry 返回预装 calculate 与 current_time 工具的注册表。
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(NewCalculateTool())
	r.Register(NewCurrentTimeTool())
	return r
}
