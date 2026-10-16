package worker

import (
	"agent-api/llm"
	"agent-api/store"
	"context"
	"testing"
	"time"
)

// BenchmarkPoolEnqueue 量化「提交 + 异步执行」的调度吞吐：
// 用零延迟 fakeChatter，循环 Enqueue，3 个 worker 并发消费。
func BenchmarkPoolEnqueue(b *testing.B) {
	s := store.NewStore()
	p := NewPool(3, 30*time.Second, s, fakeChatter{0, nil})
	p.Start()
	defer p.Stop()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := s.Create("bench")
		p.Enqueue(id)
	}
}

// BenchmarkRunAgent 直接测 agent 单步循环（无工具调用）的开销，
// 隔离掉 LLM 网络与 worker 调度，只看 think→observe 循环本身。
func BenchmarkRunAgent(b *testing.B) {
	c := fakeChatter{0, nil}
	tools, _ := defaultTools()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = runAgent(context.Background(), c, "hi", tools, func(tc llm.ToolCall) string { return "ok" }, nil)
	}
}
