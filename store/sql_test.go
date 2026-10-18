package store

import (
	"errors"
	"path/filepath"
	"testing"
)

// newSQLStore 在临时目录开一个 SQLite 后端，测试结束自动关闭。
func newSQLStore(t *testing.T) *SQLStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tasks.db")
	s, err := NewSQLStore(path)
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSQLStore_CreateGet(t *testing.T) {
	s := newSQLStore(t)
	id := s.Create("hello")
	if id == "" {
		t.Fatal("Create 返回空 id")
	}
	task, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if task.Prompt != "hello" || task.Status != StatusPending {
		t.Errorf("unexpected task: %+v", task)
	}
}

func TestSQLStore_UpdateNotFound(t *testing.T) {
	s := newSQLStore(t)
	if err := s.Update("不存在的id", StatusRunning); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update 应返回 ErrNotFound, got %v", err)
	}
}

func TestSQLStore_Complete_Done(t *testing.T) {
	s := newSQLStore(t)
	id := s.Create("hello")
	if err := s.Complete(id, "world", nil); err != nil {
		t.Fatal(err)
	}
	task, _ := s.Get(id)
	if task.Status != StatusDone || task.Result != "world" || task.Error != "" {
		t.Errorf("unexpected task: %+v", task)
	}
}

func TestSQLStore_Complete_Failed(t *testing.T) {
	s := newSQLStore(t)
	id := s.Create("hello")
	if err := s.Complete(id, "", errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	task, _ := s.Get(id)
	if task.Status != StatusFailed || task.Error != "boom" || task.Result != "" {
		t.Errorf("unexpected task: %+v", task)
	}
}

// TestSQLStore_PersistsAcrossReopen 是持久化价值的核心验证：
// 关库再开同一文件，之前写的任务仍在。这正是内存存储做不到的。
func TestSQLStore_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")

	s1, err := NewSQLStore(path)
	if err != nil {
		t.Fatal(err)
	}
	id := s1.Create("persist me")
	if err := s1.Complete(id, "done-result", nil); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := NewSQLStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	task, err := s2.Get(id)
	if err != nil {
		t.Fatalf("reopen 后 Get: %v", err)
	}
	if task.Status != StatusDone || task.Result != "done-result" {
		t.Errorf("未持久化: %+v", task)
	}
}

func TestSQLStore_Delete(t *testing.T) {
	s := newSQLStore(t)
	id := s.Create("hello")

	// 断言 1：删除已存在的任务成功，且 Get 之后返回 ErrNotFound
	if err := s.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后 Get 应返回 ErrNotFound, got %v", err)
	}

	// 断言 2：删除不存在的 id 返回 ErrNotFound
	if err := s.Delete("不存在的id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete 不存在的 id 应返回 ErrNotFound, got %v", err)
	}
}

func TestSQLStore_AutoIncrementID(t *testing.T) {
	s := newSQLStore(t)
	id1 := s.Create("task 1")
	id2 := s.Create("task 2")
	id3 := s.Create("task 3")
	if id1 != "1" || id2 != "2" || id3 != "3" {
		t.Fatalf("expected IDs 1, 2, 3, got %q, %q, %q", id1, id2, id3)
	}

	// 删除最大的 id3
	if err := s.Delete(id3); err != nil {
		t.Fatalf("Delete id3: %v", err)
	}

	// 原旧代码如果执行 SELECT MAX，此时表里只有 1 和 2，MAX 为 2，新建任务会重用 id 3！
	// 原生 AUTOINCREMENT 会记录 sqlite_sequence，新建任务必须生成 id 4！
	id4 := s.Create("task 4")
	if id4 != "4" {
		t.Fatalf("expected AUTOINCREMENT ID 4 after deleting max id 3, got %q (old code reused max+1)", id4)
	}
}
