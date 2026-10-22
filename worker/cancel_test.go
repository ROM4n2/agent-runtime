package worker

import (
	"agent-api/store"
	"context"
	"errors"
	"testing"
	"time"
)

func waitForStatus(t *testing.T, s store.TaskStore, id, want string, timeout time.Duration) store.Task {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		task, err := s.Get(id)
		if err == nil && task.Status == want {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s did not reach status %q in %v (last=%+v, err=%v)", id, want, timeout, task, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPool_CancelInFlightTask(t *testing.T) {
	s := store.NewStore()
	p := NewPool(1, 30*time.Second, s, cancellableChatter{delay: 10 * time.Second})
	p.Start()
	defer p.Stop()

	id := s.Create("long running task")
	if err := p.Enqueue(id); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitForStatus(t, s, id, store.StatusRunning, time.Second)

	if err := p.Cancel(id, "user_aborted"); err != nil {
		t.Fatalf("Cancel = %v, want nil", err)
	}

	task := waitForStatus(t, s, id, store.StatusFailed, 2*time.Second)
	if task.Error != "canceled: user_aborted" {
		t.Errorf("task.Error = %q, want %q", task.Error, "canceled: user_aborted")
	}

	// 任务已终态，句柄应已被清理：再次取消必须明确失败而非静默成功。
	if err := p.Cancel(id, "user_aborted"); !errors.Is(err, ErrTaskNotRunning) {
		t.Errorf("second Cancel = %v, want ErrTaskNotRunning", err)
	}
}

func TestPool_CancelUnknownTask(t *testing.T) {
	s := store.NewStore()
	p := NewPool(1, time.Second, s, fakeChatter{})

	if err := p.Cancel("nope", "user_aborted"); !errors.Is(err, ErrTaskNotRunning) {
		t.Errorf("Cancel unknown = %v, want ErrTaskNotRunning", err)
	}
}

func TestPool_CancelQueuedTaskNotYetRunning(t *testing.T) {
	s := store.NewStore()
	// size=1 但不启动 worker：任务只停留在队列，没有在飞句柄。
	p := NewPool(1, time.Second, s, fakeChatter{})
	id := s.Create("queued")
	if err := p.Enqueue(id); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if err := p.Cancel(id, "user_aborted"); !errors.Is(err, ErrTaskNotRunning) {
		t.Errorf("Cancel queued = %v, want ErrTaskNotRunning", err)
	}
}

func TestPool_EnqueueWaitBlocksUntilSpace(t *testing.T) {
	s := store.NewStore()
	// 不启动 worker：size=1 的缓冲只能容纳一个，第二个 EnqueueWait 必须等待。
	p := NewPool(1, time.Second, s, fakeChatter{})

	if err := p.EnqueueWait(context.Background(), "first"); err != nil {
		t.Fatalf("first EnqueueWait: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.EnqueueWait(ctx, "second"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("second EnqueueWait = %v, want context.DeadlineExceeded", err)
	}
}

func TestPool_EnqueueWaitStopping(t *testing.T) {
	s := store.NewStore()
	p := NewPool(1, time.Second, s, fakeChatter{})
	p.stopping.Store(true)

	if err := p.EnqueueWait(context.Background(), "any"); !errors.Is(err, ErrQueueFull) {
		t.Errorf("EnqueueWait while stopping = %v, want ErrQueueFull", err)
	}
}
