package store

import (
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
