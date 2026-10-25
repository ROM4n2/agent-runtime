package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
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

func TestSQLStore_ClaimNext_FIFO(t *testing.T) {
	ctx := context.Background()
	s := newSQLStore(t)

	// 空库时返回 ErrNoTaskAvailable
	_, err := s.ClaimNext(ctx)
	if !errors.Is(err, ErrNoTaskAvailable) {
		t.Fatalf("ClaimNext on empty SQLStore = %v, want ErrNoTaskAvailable", err)
	}

	id1 := s.Create("task 1")
	id2 := s.Create("task 2")
	id3 := s.Create("task 3")

	t1, err := s.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext 1 failed: %v", err)
	}
	if t1.ID != id1 || t1.Status != StatusRunning || t1.Prompt != "task 1" {
		t.Errorf("unexpected t1: %+v", t1)
	}

	t2, err := s.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext 2 failed: %v", err)
	}
	if t2.ID != id2 || t2.Status != StatusRunning || t2.Prompt != "task 2" {
		t.Errorf("unexpected t2: %+v", t2)
	}

	t3, err := s.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext 3 failed: %v", err)
	}
	if t3.ID != id3 || t3.Status != StatusRunning || t3.Prompt != "task 3" {
		t.Errorf("unexpected t3: %+v", t3)
	}

	_, err = s.ClaimNext(ctx)
	if !errors.Is(err, ErrNoTaskAvailable) {
		t.Fatalf("ClaimNext on exhausted SQLStore = %v, want ErrNoTaskAvailable", err)
	}
}

func TestSQLStore_PendingCount(t *testing.T) {
	ctx := context.Background()
	s := newSQLStore(t)

	count, err := s.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount error: %v", err)
	}
	if count != 0 {
		t.Fatalf("initial PendingCount = %d, want 0", count)
	}

	id1 := s.Create("task 1")
	id2 := s.Create("task 2")

	count, err = s.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if count != 2 {
		t.Fatalf("PendingCount = %d, want 2", count)
	}

	_, err = s.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}

	count, err = s.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount after claim = %d, want 1", count)
	}
	if count != 1 {
		t.Fatalf("PendingCount = %d, want 1", count)
	}

	if err := s.Complete(id2, "done", nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	_ = id1 // claimed as running

	count, err = s.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if count != 0 {
		t.Fatalf("PendingCount = %d, want 0", count)
	}
}

func TestSQLStore_ClaimNext_Concurrency(t *testing.T) {
	ctx := context.Background()
	s := newSQLStore(t)

	const totalTasks = 20
	createdIDs := make(map[string]bool)
	for i := 1; i <= totalTasks; i++ {
		id := s.Create(fmt.Sprintf("prompt-%d", i))
		if id == "" {
			t.Fatalf("Create returned empty id for task %d", i)
		}
		createdIDs[id] = true
	}

	pending, err := s.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != totalTasks {
		t.Fatalf("initial pending = %d, want %d", pending, totalTasks)
	}

	var mu sync.Mutex
	claimedIDs := make([]string, 0, totalTasks)

	const numWorkers = 10
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	for i := 0; i < numWorkers; i++ {
		go func() {
			defer wg.Done()
			for {
				task, err := s.ClaimNext(ctx)
				if errors.Is(err, ErrNoTaskAvailable) {
					// 队列已被全部抢占完毕，Worker 退出
					return
				}
				if err != nil {
					t.Errorf("ClaimNext unexpected error: %v", err)
					return
				}
				mu.Lock()
				claimedIDs = append(claimedIDs, task.ID)
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	// 断言总抢占数严格等于 20
	if len(claimedIDs) != totalTasks {
		t.Fatalf("claimed count = %d, want %d", len(claimedIDs), totalTasks)
	}

	// 断言每个 task ID 只被唯一抢占一次，无碰撞、无遗漏
	seen := make(map[string]int)
	for _, id := range claimedIDs {
		seen[id]++
		if !createdIDs[id] {
			t.Errorf("claimed unknown task id: %s", id)
		}
	}
	if len(seen) != totalTasks {
		t.Errorf("unique claimed tasks = %d, want %d", len(seen), totalTasks)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("task %s was claimed %d times, want 1", id, count)
		}
	}

	// 验证所有任务在 SQLite 中状态均为 StatusRunning
	for id := range createdIDs {
		task, err := s.Get(id)
		if err != nil {
			t.Errorf("Get(%s) error: %v", id, err)
			continue
		}
		if task.Status != StatusRunning {
			t.Errorf("task %s status = %q, want %q", id, task.Status, StatusRunning)
		}
	}

	// 最终 PendingCount 应为 0
	pending, err = s.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 0 {
		t.Errorf("final pending count = %d, want 0", pending)
	}
}
