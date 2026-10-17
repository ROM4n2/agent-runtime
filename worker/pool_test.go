package worker

import (
	"agent-api/agent"
	"agent-api/llm"
	"agent-api/store"
	"context"
	"errors"
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
