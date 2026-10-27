package worker

// 本文件承载 worker 测试的 TaskStore 桩（从 pool_test.go 拆出）：
// 架构门禁要求测试文件 ≤ 800 行，桩会随接口扩展增长，
// 与生产侧「同包正交拆分」同一手法。

import (
	"agent-runtime/store"
	"context"
)

type mockTaskStore struct {
	updateErr   error
	getErr      error
	completeErr error

	completedID     string
	completedResult string
	completedErr    error
	completeCalled  bool

	// usage 记录 RecordUsage 的入参，供 ADR-0021 的成本计量断言使用。
	usageID         string
	usagePrompt     int
	usageCompletion int
	usageCalled     bool
	usageErr        error
}

func (m *mockTaskStore) Create(prompt string) string                     { return "mock-id" }
func (m *mockTaskStore) CreateInSession(prompt, sessionID string) string { return m.Create(prompt) }
func (m *mockTaskStore) Update(id, status string) error                  { return m.updateErr }
func (m *mockTaskStore) Get(id string) (store.Task, error) {
	if m.getErr != nil {
		return store.Task{}, m.getErr
	}
	return store.Task{ID: id, Prompt: "mock prompt"}, nil
}
func (m *mockTaskStore) Complete(id, result string, err error) error {
	m.completeCalled = true
	m.completedID = id
	m.completedResult = result
	m.completedErr = err
	return m.completeErr
}
func (m *mockTaskStore) Delete(id string) error                                  { return nil }
func (m *mockTaskStore) RecoverTasks(ctx context.Context) ([]string, int, error) { return nil, 0, nil }
func (m *mockTaskStore) ClaimNext(ctx context.Context) (store.Task, error) {
	return store.Task{}, store.ErrNoTaskAvailable
}
func (m *mockTaskStore) PendingCount(ctx context.Context) (int64, error) { return 0, nil }

// RecordUsage 记录计量入参；usageErr 非 nil 时用于验证「计量失败不改任务终态」。
func (m *mockTaskStore) RecordUsage(id string, promptTokens, completionTokens int) error {
	m.usageID, m.usagePrompt, m.usageCompletion = id, promptTokens, completionTokens
	m.usageCalled = true
	return m.usageErr
}
