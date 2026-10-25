package store

import (
	"context"
	"errors"
	"testing"
)

func TestStore_Create(t *testing.T) {
	s := NewStore()

	id := s.Create("hello")

	// 断言 1：id 非空
	if id == "" {
		t.Errorf("Create 返回的 id 是空的")
	}

	task, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get 返回的 %v", err)
	}

	// 断言 2：s.Get(id) 能取回来，Prompt == "hello"
	if task.Prompt != "hello" {
		t.Errorf("Get 返回的 Prompt 不是 hello")
	}

	// 断言 3：取回来的 Status == StatusPending
	if task.Status != StatusPending {
		t.Errorf("Get 返回的 Status 不是 pending")
	}
}

func TestStore_GetNotFound(t *testing.T) {
	s := &Store{tasks: make(map[string]Task)}

	_, err := s.Get("不存在的id")

	// 断言：err == ErrNotFound
	if err != ErrNotFound {
		t.Fatalf("Get 返回的 %v", err)
	}
}

func TestStore_UpdateStatus(t *testing.T) {
	s := NewStore()

	if err := s.Update("不存在的id", StatusRunning); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update 返回的不是 ErrNotFound")
	}

	id := s.Create("hello")
	s.Update(id, StatusRunning)

	task, err := s.Get(id)

	// 断言：err == nil
	if err != nil {
		t.Fatalf("Get 返回的 %v", err)
	}
	// 断言：task.Status == StatusRunning
	if task.Status != StatusRunning {
		t.Errorf("Get 返回的 Status 不为 running")
	}

}

func TestStore_Complete_Done(t *testing.T) {
	s := NewStore()
	id := s.Create("hello")

	err := s.Complete(id, "world", nil)
	if err != nil {
		t.Fatalf("Complete 返回的错误是 %v", err)
	}

	task, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get 返回的错误是 %v", err)
	}

	if task.Status != StatusDone {
		t.Errorf("Get 返回的 Status 不为 done")
	}
	if task.Result != "world" {
		t.Errorf("Get 返回的 Result 不为 world")
	}
	if task.Error != "" {
		t.Errorf("Get 返回的 Error 不为空")
	}
}

func TestStore_Complete_Failed(t *testing.T) {
	s := NewStore()
	id := s.Create("hello")

	err := s.Complete(id, "", errors.New("boom"))
	if err != nil {
		t.Fatalf("Complete 返回的错误是 %v", err)
	}

	// 断言 Get 回来的状态
	task, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get 返回的错误是 %v", err)
	}

	if task.Status != StatusFailed {
		t.Errorf("Get 返回的 Status 不为 failed")
	}
	if task.Error != "boom" {
		t.Errorf("Get 返回的 Error 不为 boom")
	}
	if task.Result != "" {
		t.Errorf("Get 返回的 Result 不为空")
	}
}

func TestStore_Delete(t *testing.T) {
	s := NewStore()
	id := s.Create("hello")

	// 断言 1：删除已存在的任务成功，且 Get 之后返回 ErrNotFound
	if err := s.Delete(id); err != nil {
		t.Fatalf("Delete 返回的错误是 %v", err)
	}
	if _, err := s.Get(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后 Get 应返回 ErrNotFound，实际是 %v", err)
	}

	// 断言 2：删除不存在的 id 返回 ErrNotFound
	if err := s.Delete("不存在的id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete 不存在的 id 应返回 ErrNotFound，实际是 %v", err)
	}
}

func TestStore_ClaimNext(t *testing.T) {
	ctx := context.Background()
	s := NewStore()

	// 空 store 抢占返回 ErrNoTaskAvailable
	_, err := s.ClaimNext(ctx)
	if !errors.Is(err, ErrNoTaskAvailable) {
		t.Fatalf("ClaimNext on empty store = %v, want ErrNoTaskAvailable", err)
	}

	// 顺序创建 3 个任务
	id1 := s.Create("prompt 1")
	id2 := s.Create("prompt 2")
	id3 := s.Create("prompt 3")

	// 依次抢占，返回顺序必须严格遵循 FIFO（id1, id2, id3），且状态迁移为 StatusRunning
	t1, err := s.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext 1 failed: %v", err)
	}
	if t1.ID != id1 || t1.Status != StatusRunning || t1.Prompt != "prompt 1" {
		t.Errorf("ClaimNext 1 unexpected: %+v, want ID=%s Status=%s", t1, id1, StatusRunning)
	}

	t2, err := s.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext 2 failed: %v", err)
	}
	if t2.ID != id2 || t2.Status != StatusRunning || t2.Prompt != "prompt 2" {
		t.Errorf("ClaimNext 2 unexpected: %+v, want ID=%s Status=%s", t2, id2, StatusRunning)
	}

	t3, err := s.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext 3 failed: %v", err)
	}
	if t3.ID != id3 || t3.Status != StatusRunning || t3.Prompt != "prompt 3" {
		t.Errorf("ClaimNext 3 unexpected: %+v, want ID=%s Status=%s", t3, id3, StatusRunning)
	}

	// 任务池排空后，再次抢占返回 ErrNoTaskAvailable
	_, err = s.ClaimNext(ctx)
	if !errors.Is(err, ErrNoTaskAvailable) {
		t.Fatalf("ClaimNext on exhausted store = %v, want ErrNoTaskAvailable", err)
	}

	// 验证存储内部状态已更新为 running
	got1, err := s.Get(id1)
	if err != nil {
		t.Fatalf("Get(%s) error: %v", id1, err)
	}
	if got1.Status != StatusRunning {
		t.Errorf("task 1 status = %q, want %q", got1.Status, StatusRunning)
	}
}

func TestStore_PendingCount(t *testing.T) {
	ctx := context.Background()
	s := NewStore()

	count, err := s.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount error: %v", err)
	}
	if count != 0 {
		t.Fatalf("initial PendingCount = %d, want 0", count)
	}

	id1 := s.Create("p1")
	id2 := s.Create("p2")

	count, err = s.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount error: %v", err)
	}
	if count != 2 {
		t.Fatalf("PendingCount after 2 creates = %d, want 2", count)
	}

	// 抢占一个后，pending 计数应减 1
	_, err = s.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext error: %v", err)
	}

	count, err = s.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount error: %v", err)
	}
	if count != 1 {
		t.Fatalf("PendingCount after 1 claim = %d, want 1", count)
	}

	// 将剩余 pending 任务完成，pending 归零
	if err := s.Complete(id2, "done", nil); err != nil {
		t.Fatalf("Complete error: %v", err)
	}
	_ = id1 // claimed as running

	count, err = s.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount error: %v", err)
	}
	if count != 0 {
		t.Fatalf("PendingCount after complete = %d, want 0", count)
	}
}
