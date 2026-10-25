package worker

import (
	"agent-runtime/agent"
	"agent-runtime/agent/tool"
	"agent-runtime/llm"
	"agent-runtime/store"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeChatter struct {
	delay time.Duration
	err   error
}

func (f fakeChatter) ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (*llm.AssistantTurn, error) {
	// 睡一下模拟耗时，让并发测试能观察到状态
	// 返回固定字符串，断言才写得出来
	time.Sleep(f.delay)
	return &llm.AssistantTurn{Content: "fake response"}, f.err
}

func TestWorker_ProcessesTask(t *testing.T) {
	s := store.NewStore()
	p := NewPool(3, 30*time.Second, s, fakeChatter{100 * time.Millisecond, nil})
	p.Start()
	defer p.Stop()

	id := s.Create("hello")
	p.queue <- id

	deadline := time.Now().Add(time.Second) // 最多等 1s
	for {
		task, _ := s.Get(id)
		if task.Status == store.StatusRunning {
			break // 等到 running，出循环
		}
		if time.Now().After(deadline) {
			t.Fatal("1s 内没变成 running") // 超时 = 失败
		}
		time.Sleep(10 * time.Millisecond) // 喘口气再查
	}

	deadline = time.Now().Add(4 * time.Second) // 最多等 4s
	for {
		task, _ := s.Get(id)
		if task.Status == store.StatusDone {
			break // 等到 done
		}
		if time.Now().After(deadline) {
			t.Fatal("4s 内没变成 done") // 超时 = 失败
		}
		time.Sleep(10 * time.Millisecond) // 喘口气再查
	}

}

func TestWorker_UpdateNotFound(t *testing.T) {
	s := store.NewStore()
	p := NewPool(1, 30*time.Second, s, fakeChatter{0, nil})
	p.Start()
	defer p.Stop()

	p.queue <- "不存在的id"

	// 等一下，确认没变成 running
	time.Sleep(200 * time.Millisecond)
	task, _ := s.Get("不存在的id")
	if task.Status == store.StatusRunning {
		t.Errorf("不存在的任务不应变成 running")
	}
}

func TestPool_LimitsConcurrency(t *testing.T) {
	s := store.NewStore()
	p := NewPool(3, 30*time.Second, s, fakeChatter{100 * time.Millisecond, nil}) // 3 个 worker
	p.Start()
	defer p.Stop()

	// Create 6 个任务，ID 存进 ids（名字用 strconv.Itoa 拼：s.Create("task-" + strconv.Itoa(i))）
	// 全塞进 p.queue
	var ids []string
	for i := 0; i < 6; i++ {
		ids = append(ids, s.Create("task-"+strconv.Itoa(i)))
	}
	for _, id := range ids {
		p.queue <- id
	}

	// 轮询（1s deadline）：等到 running 数量 == 3
	//   统计 running 数：for 循环遍历 ids，s.Get(id) 数 status == StatusRunning 的
	deadline := time.Now().Add(time.Second)
	for {
		running := 0
		for _, id := range ids { // 遍历所有任务数一下
			task, _ := s.Get(id)
			if task.Status == store.StatusRunning {
				running++
			}
		}
		if running == 3 {
			break
		} // 3 个在跑 = 条件满足
		if time.Now().After(deadline) {
			t.Fatal("1s 内没到 3 个 running")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 断言：此时 pending 数量 == 3（遍历数 status == StatusPending）
	pending := 0
	for _, id := range ids {
		task, _ := s.Get(id)
		if task.Status == store.StatusPending {
			pending++
		}
	}
	if pending != 3 {
		t.Errorf("pending 数量 %d, want 3", pending)
	}

	// 轮询（5s deadline）：等到全部 6 个 done
	deadline = time.Now().Add(5 * time.Second)
	for {
		done := 0
		for _, id := range ids {
			task, _ := s.Get(id)
			if task.Status == store.StatusDone {
				done++
			}
		}
		if done == 6 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("5s 内没全 done")
		}
		time.Sleep(10 * time.Millisecond)
	}

}

func TestWorker_ChatFailure(t *testing.T) {
	s := store.NewStore()
	fake := fakeChatter{err: errors.New("boom")}
	p := NewPool(1, 30*time.Second, s, fake)
	p.Start()
	defer p.Stop()

	id := s.Create("hello")
	p.queue <- id

	// 等到 done（不是 running）——因为失败后 Complete 会把状态设为 failed
	deadline := time.Now().Add(time.Second)
	for {
		task, _ := s.Get(id)
		if task.Status == store.StatusFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("1s 内没变成 failed")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 断言 Error 字段
	task, _ := s.Get(id)
	if task.Error != "upstream error" {
		t.Errorf("Error = %q, want %q", task.Error, "upstream error")
	}
}

func TestEnqueue_Full(t *testing.T) {
	s := store.NewStore()
	p := NewPool(1, 30*time.Second, s, fakeChatter{0, nil})
	p.Start()
	defer p.Stop()

	// 占住唯一的缓冲位（size=1），且不消费，使队列满
	occupy := s.Create("occupy")
	p.queue <- occupy

	if err := p.Enqueue("extra"); !errors.Is(err, ErrQueueFull) {
		t.Errorf("Enqueue on full queue = %v, want ErrQueueFull", err)
	}
}

type mockTaskStore struct {
	updateErr   error
	getErr      error
	completeErr error

	completedID     string
	completedResult string
	completedErr    error
	completeCalled  bool
}

func (m *mockTaskStore) Create(prompt string) string {
	return "mock-id"
}

func (m *mockTaskStore) Update(id, status string) error {
	if m.updateErr != nil {
		return m.updateErr
	}
	return nil
}

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

func (m *mockTaskStore) Delete(id string) error {
	return nil
}

func (m *mockTaskStore) RecoverTasks(ctx context.Context) ([]string, int, error) {
	return nil, 0, nil
}

func (m *mockTaskStore) ClaimNext(ctx context.Context) (store.Task, error) {
	return store.Task{}, store.ErrNoTaskAvailable
}

func (m *mockTaskStore) PendingCount(ctx context.Context) (int64, error) {
	return 0, nil
}

func TestProcess_UpdateErrorSetsFailed(t *testing.T) {
	mock := &mockTaskStore{
		updateErr: errors.New("db update error"),
	}
	p := NewPool(1, time.Second, mock, fakeChatter{})
	p.process("test-id")

	if !mock.completeCalled {
		t.Fatal("expected Complete to be called when Update fails, but it was not")
	}
	if mock.completedID != "test-id" {
		t.Errorf("completed ID = %q, want %q", mock.completedID, "test-id")
	}
	if mock.completedErr == nil {
		t.Fatal("expected non-nil error passed to Complete, got nil")
	}
	if !strings.Contains(mock.completedErr.Error(), "state transition failed") {
		t.Errorf("completed error = %q, want containing %q", mock.completedErr.Error(), "state transition failed")
	}
}

func TestProcess_GetErrorSetsFailed(t *testing.T) {
	mock := &mockTaskStore{
		getErr: errors.New("db get error"),
	}
	p := NewPool(1, time.Second, mock, fakeChatter{})
	p.process("test-id")

	if !mock.completeCalled {
		t.Fatal("expected Complete to be called when Get fails, but it was not")
	}
	if mock.completedID != "test-id" {
		t.Errorf("completed ID = %q, want %q", mock.completedID, "test-id")
	}
	if mock.completedErr == nil {
		t.Fatal("expected non-nil error passed to Complete, got nil")
	}
	if !strings.Contains(mock.completedErr.Error(), "task state unavailable") {
		t.Errorf("completed error = %q, want containing %q", mock.completedErr.Error(), "task state unavailable")
	}
}

func TestPool_TraceAfterProcess(t *testing.T) {
	s := store.NewStore()
	p := NewPool(1, 5*time.Second, s, fakeChatter{0, nil})
	p.Start()
	defer p.Stop()

	id := s.Create("what is the weather")
	if err := p.Enqueue(id); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		task, err := s.Get(id)
		if err == nil && task.Status == store.StatusDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("task did not finish within deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}

	steps := p.Trace(id)
	if len(steps) == 0 {
		t.Fatal("expected trace steps, got none")
	}
	last := steps[len(steps)-1]
	if last.Kind != agent.KindFinal {
		t.Errorf("last step kind = %q, want %q", last.Kind, agent.KindFinal)
	}
}

func TestPool_TraceUnknownID(t *testing.T) {
	s := store.NewStore()
	p := NewPool(1, 5*time.Second, s, fakeChatter{0, nil})
	steps := p.Trace("non-existent-id")
	if steps != nil {
		t.Errorf("expected nil trace for unknown ID, got %+v", steps)
	}
}

func TestPool_TracesBoundedCapacity(t *testing.T) {
	s := store.NewStore()
	p := NewPool(1, time.Second, s, fakeChatter{0, nil})
	p.maxTraces = 5

	var ids []string
	for i := 0; i < 10; i++ {
		id := s.Create(fmt.Sprintf("task-%d", i))
		ids = append(ids, id)
		p.process(id)
	}

	p.tracesMu.Lock()
	defer p.tracesMu.Unlock()

	if len(p.traces) > 5 {
		t.Fatalf("expected len(p.traces) <= 5, got %d", len(p.traces))
	}

	for i := 0; i < 5; i++ {
		if _, ok := p.traces[ids[i]]; ok {
			t.Errorf("expected oldest task %s to be evicted, but still present in traces", ids[i])
		}
	}

	for i := 5; i < 10; i++ {
		if _, ok := p.traces[ids[i]]; !ok {
			t.Errorf("expected recent task %s to be present in traces, but was evicted", ids[i])
		}
	}
}

func TestShutdown_DrainsQueuedTasks(t *testing.T) {
	s := store.NewStore()
	p := NewPool(1, 30*time.Second, s, fakeChatter{delay: 100 * time.Millisecond})
	p.Start()

	id1 := s.Create("task-1")
	if err := p.Enqueue(id1); err != nil {
		t.Fatalf("enqueue task-1 failed: %v", err)
	}

	// 等待 task-1 变为 running 状态，确保 worker 已经被该任务占用
	deadline := time.Now().Add(time.Second)
	for {
		task, err := s.Get(id1)
		if err == nil && task.Status == store.StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("task-1 1s 内未进入 running 状态")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 此时 worker 忙于执行 task-1，Enqueue 一个新任务进入队列排队
	id2 := s.Create("task-2")
	if err := p.Enqueue(id2); err != nil {
		t.Fatalf("enqueue task-2 failed: %v", err)
	}

	// 启动 goroutine 执行 Stop(context.Background())
	stopDone := make(chan error, 1)
	go func() {
		stopDone <- p.Stop(context.Background())
	}()

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("p.Stop returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("p.Stop 3s 内未完成")
	}

	// 耗时任务 task-1 应正常完成
	t1, err := s.Get(id1)
	if err != nil {
		t.Fatalf("get task-1: %v", err)
	}
	if t1.Status != store.StatusDone {
		t.Errorf("task-1 status = %q, want %q", t1.Status, store.StatusDone)
	}

	// 队列中排队的任务 task-2 状态应被更新为 StatusFailed，错误为 "server shutdown"
	t2, err := s.Get(id2)
	if err != nil {
		t.Fatalf("get task-2: %v", err)
	}
	if t2.Status != store.StatusFailed {
		t.Errorf("task-2 status = %q, want %q", t2.Status, store.StatusFailed)
	}
	if t2.Error != "server shutdown" {
		t.Errorf("task-2 error = %q, want %q", t2.Error, "server shutdown")
	}

	// 验证 store 中不再有残留的 pending 任务
	for _, id := range []string{id1, id2} {
		tk, err := s.Get(id)
		if err != nil {
			t.Errorf("get task %s: %v", id, err)
			continue
		}
		if tk.Status == store.StatusPending {
			t.Errorf("task %s remained in pending state", id)
		}
	}
}

type cancellableChatter struct {
	delay time.Duration
}

func (c cancellableChatter) ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (*llm.AssistantTurn, error) {
	select {
	case <-time.After(c.delay):
		return &llm.AssistantTurn{Content: "response"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestShutdown_TimeoutCancelsInFlightTask(t *testing.T) {
	s := store.NewStore()
	p := NewPool(1, 30*time.Second, s, cancellableChatter{delay: 2 * time.Second})
	p.Start()

	id := s.Create("task-slow")
	if err := p.Enqueue(id); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	// 等待 task 变为 running
	deadline := time.Now().Add(time.Second)
	for {
		task, err := s.Get(id)
		if err == nil && task.Status == store.StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("task did not enter running state in time")
		}
		time.Sleep(5 * time.Millisecond)
	}

	timeoutCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := p.Stop(timeoutCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Stop err = %v, want context.DeadlineExceeded", err)
	}

	task, err := s.Get(id)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.Status != store.StatusFailed {
		t.Errorf("task status = %q, want %q", task.Status, store.StatusFailed)
	}
}

func TestEnqueue_Stopping(t *testing.T) {
	s := store.NewStore()
	p := NewPool(1, time.Second, s, fakeChatter{0, nil})
	p.stopping.Store(true)

	if err := p.Enqueue("any"); !errors.Is(err, ErrQueueFull) {
		t.Errorf("Enqueue when stopping = %v, want ErrQueueFull", err)
	}
}

type loopingToolChatter struct{}

func (l loopingToolChatter) ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (*llm.AssistantTurn, error) {
	return &llm.AssistantTurn{
		ToolCalls: []llm.ToolCall{
			{
				ID:       "call_loop",
				Type:     "function",
				Function: llm.ToolCallFn{Name: "calculate", Arguments: `{"expression":"1+1"}`},
			},
		},
	}, nil
}

func TestWorker_TaskTimeout(t *testing.T) {
	s := store.NewStore()
	p := NewPool(1, 20*time.Millisecond, s, cancellableChatter{delay: 100 * time.Millisecond})
	p.Start()
	defer p.Stop()

	id := s.Create("task-timeout")
	if err := p.Enqueue(id); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		task, err := s.Get(id)
		if err == nil && (task.Status == store.StatusFailed || task.Status == store.StatusDone) {
			if task.Status != store.StatusFailed {
				t.Fatalf("task status = %q, want %q", task.Status, store.StatusFailed)
			}
			if task.Error != "task timeout" {
				t.Fatalf("task error = %q, want %q", task.Error, "task timeout")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("task did not finish in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWorker_MaxStepsExceeded(t *testing.T) {
	s := store.NewStore()
	p := NewPool(1, 5*time.Second, s, loopingToolChatter{})
	p.Start()
	defer p.Stop()

	id := s.Create("task-loop")
	if err := p.Enqueue(id); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		task, err := s.Get(id)
		if err == nil && (task.Status == store.StatusFailed || task.Status == store.StatusDone) {
			if task.Status != store.StatusFailed {
				t.Fatalf("task status = %q, want %q", task.Status, store.StatusFailed)
			}
			if task.Error != "max steps exceeded" {
				t.Fatalf("task error = %q, want %q", task.Error, "max steps exceeded")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("task did not finish in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fakeSearchTool 是名为 search_docs 的假工具，供 RegisterTool 注入测试使用：
// 只关心 Spec（agent 视野里的工具定义）与一个可执行的 Execute，
// 验证检索工具被装配进 pool 后，Registry 里出现且仅出现一份 search_docs。
type fakeSearchTool struct{}

func (fakeSearchTool) Spec() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.FunctionSpec{
			Name:        "search_docs",
			Description: "fake search tool for RegisterTool injection test",
			Parameters:  map[string]any{"type": "object"},
		},
	}
}

func (fakeSearchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "fake search result", nil
}

// fakeCompanionTool 是另一个假工具，用于验证 RegisterTool 是「增量注入」而非
// 「整体替换」：注册 search_docs 之后再注册它，两者必须并存。
type fakeCompanionTool struct{}

func (fakeCompanionTool) Spec() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.FunctionSpec{
			Name:        "fake_companion",
			Description: "second tool to prove RegisterTool is additive",
			Parameters:  map[string]any{"type": "object"},
		},
	}
}

func (fakeCompanionTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "companion result", nil
}

// TestPool_RegisterTool_InjectsSearchDocsSpec 是装配承重墙（变异自证 ②）：
// RegisterTool 必须把 search_docs 注入既有 Registry，重复注册幂等（同名 map 覆盖），
// 之后再注册别的工具不得抹掉已注入的 search_docs——非幂等/破坏性实现（如每次重建
// registry）会让 agent 视野里的 search_docs 定义时有时无。变异自证：把 RegisterTool
// 改成「重建 registry 再注册」，search_docs spec count 断言转红。
func TestPool_RegisterTool_InjectsSearchDocsSpec(t *testing.T) {
	s := store.NewStore()
	p := NewPool(1, 30*time.Second, s, fakeChatter{})

	p.RegisterTool(fakeSearchTool{})
	p.RegisterTool(fakeSearchTool{})    // 幂等：重复注册不得增生
	p.RegisterTool(fakeCompanionTool{}) // 增量：不得抹掉 search_docs

	var searchDocs, companion, defaults int
	for _, sp := range p.tools.Specs() {
		switch sp.Function.Name {
		case "search_docs":
			searchDocs++
		case "fake_companion":
			companion++
		case "calculate", "current_time":
			defaults++
		}
	}
	if searchDocs != 1 {
		t.Errorf("search_docs spec count = %d, want 1 (RegisterTool must be idempotent and additive)", searchDocs)
	}
	if companion != 1 {
		t.Errorf("fake_companion spec count = %d, want 1 (RegisterTool must be additive, not replace)", companion)
	}
	if defaults != 2 {
		t.Errorf("default tool count = %d, want 2 (RegisterTool must not wipe calculate/current_time)", defaults)
	}
}

// TestPool_RegisterToolAfterStart_WarnsButKeepsWorking 覆盖 RegisterTool 的
// started 分支（现有用例全在 Start 之前注册，该分支零覆盖）：Start 之后注册
// 不得 panic、必须走 slog.Warn 告警（装配约定被违反时要显式可见），且工具仍
// 进 Registry 对后续任务可见（告警不等于丢注册）。
// 变异自证：删掉 RegisterTool 里的 started 判断，本用例的 WARN 断言转红。
func TestPool_RegisterToolAfterStart_WarnsButKeepsWorking(t *testing.T) {
	s := store.NewStore()
	p := NewPool(1, 30*time.Second, s, fakeChatter{})
	p.Start()
	defer p.Stop()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	p.RegisterTool(fakeSearchTool{}) // Start 之后：只告警，不 panic、不丢注册

	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "RegisterTool after Start") {
		t.Fatalf("slog output = %q, want a WARN mentioning %q", out, "RegisterTool after Start")
	}
	if !strings.Contains(out, "search_docs") {
		t.Errorf("slog output = %q, want the offending tool name %q", out, "search_docs")
	}

	var found bool
	for _, sp := range p.tools.Specs() {
		if sp.Function.Name == "search_docs" {
			found = true
		}
	}
	if !found {
		t.Error("search_docs missing from Specs after post-Start RegisterTool (warn must not drop the tool)")
	}
}

// fakeTerminalErr 用于测试 tool.TerminalClassifier 协议下沉与脱敏。
type fakeTerminalErr struct {
	msg    string
	status string
}

var _ tool.TerminalClassifier = fakeTerminalErr{}

func (e fakeTerminalErr) Error() string {
	return e.msg
}

func (e fakeTerminalErr) TerminalStatus() string {
	return e.status
}

// TestProcess_IndexMissingTerminalClassification 固化基于 tool.TerminalClassifier 的终态脱敏协议：
// worker 仅依赖下层 tool.TerminalClassifier 接口，不再直接依赖 retrieval 包；
// 当错误链携带实现了 TerminalClassifier 的错误且 TerminalStatus 非空时，映射为对应粗粒度终态；
// 若 TerminalStatus 为空或未实现接口，则回退至 "upstream error"。
func TestProcess_IndexMissingTerminalClassification(t *testing.T) {
	t.Run("classifier with status", func(t *testing.T) {
		s := store.NewStore()
		// 经 Chatter 上抛包装了 fakeTerminalErr 的错误链：agent.Run 对 Chatter 错误原样返回，
		// 终态分类据此经 errors.As 匹配 tool.TerminalClassifier 并提取脱敏终态文案。
		chat := fakeChatter{err: fmt.Errorf("tool failed: %w", fakeTerminalErr{msg: "sensitive internal error", status: "index missing"})}
		p := NewPool(1, 30*time.Second, s, chat)

		id := s.Create("query the docs")
		p.process(id)

		task, err := s.Get(id)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if task.Status != store.StatusFailed {
			t.Fatalf("status = %q, want %q", task.Status, store.StatusFailed)
		}
		if task.Error != "index missing" {
			t.Errorf("task.Error = %q, want %q (TerminalClassifier deserves its own terminal class)", task.Error, "index missing")
		}
	})

	t.Run("classifier with empty status falls through", func(t *testing.T) {
		s := store.NewStore()
		chat := fakeChatter{err: fmt.Errorf("tool failed: %w", fakeTerminalErr{msg: "empty status error", status: ""})}
		p := NewPool(1, 30*time.Second, s, chat)

		id := s.Create("query with empty status")
		p.process(id)

		task, err := s.Get(id)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if task.Status != store.StatusFailed {
			t.Fatalf("status = %q, want %q", task.Status, store.StatusFailed)
		}
		if task.Error != "upstream error" {
			t.Errorf("task.Error = %q, want %q (empty status must fall through to upstream error)", task.Error, "upstream error")
		}
	})

	t.Run("plain non-classifier error falls through", func(t *testing.T) {
		s := store.NewStore()
		chat := fakeChatter{err: fmt.Errorf("plain wrapped error: %w", errors.New("raw failure"))}
		p := NewPool(1, 30*time.Second, s, chat)

		id := s.Create("query with plain error")
		p.process(id)

		task, err := s.Get(id)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if task.Status != store.StatusFailed {
			t.Fatalf("status = %q, want %q", task.Status, store.StatusFailed)
		}
		if task.Error != "upstream error" {
			t.Errorf("task.Error = %q, want %q (non-classifier error must be upstream error)", task.Error, "upstream error")
		}
	})
}
