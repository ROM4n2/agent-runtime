package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestStore_RecoverTasks(t *testing.T) {
	s := NewStore()
	first := s.Create("first")
	second := s.Create("second")
	third := s.Create("third")

	if err := s.Update(second, StatusRunning); err != nil {
		t.Fatalf("update running: %v", err)
	}
	if err := s.Complete(third, "ok", nil); err != nil {
		t.Fatalf("complete done: %v", err)
	}

	pending, recovered, err := s.RecoverTasks(context.Background())
	if err != nil {
		t.Fatalf("RecoverTasks: %v", err)
	}
	if recovered != 1 {
		t.Errorf("recoveredRunning = %d, want 1", recovered)
	}
	if len(pending) != 1 || pending[0] != first {
		t.Errorf("pending = %v, want [%s]", pending, first)
	}

	zombie, err := s.Get(second)
	if err != nil {
		t.Fatalf("get zombie: %v", err)
	}
	if zombie.Status != StatusFailed || zombie.Error != ErrMsgServerRestart {
		t.Errorf("zombie = %+v, want failed with %q", zombie, ErrMsgServerRestart)
	}

	done, _ := s.Get(third)
	if done.Status != StatusDone {
		t.Errorf("done task status = %q, want %q", done.Status, StatusDone)
	}
}

func TestStore_RecoverTasksCanceledContext(t *testing.T) {
	s := NewStore()
	s.Create("pending")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := s.RecoverTasks(ctx); err == nil {
		t.Error("RecoverTasks with canceled ctx should return error")
	}
}

func TestSQLStore_RecoverTasks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recover.db")
	s, err := NewSQLStore(path)
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}

	pendingID := s.Create("pending")
	runningID := s.Create("running")
	doneID := s.Create("done")

	if err := s.Update(runningID, StatusRunning); err != nil {
		t.Fatalf("update running: %v", err)
	}
	if err := s.Complete(doneID, "ok", nil); err != nil {
		t.Fatalf("complete done: %v", err)
	}

	// 模拟进程被 Kill 后重启：关闭并重新打开同一个数据库。
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s, err = NewSQLStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	pending, recovered, err := s.RecoverTasks(context.Background())
	if err != nil {
		t.Fatalf("RecoverTasks: %v", err)
	}
	if recovered != 1 {
		t.Errorf("recoveredRunning = %d, want 1", recovered)
	}
	if len(pending) != 1 || pending[0] != pendingID {
		t.Errorf("pending = %v, want [%s]", pending, pendingID)
	}

	zombie, err := s.Get(runningID)
	if err != nil {
		t.Fatalf("get zombie: %v", err)
	}
	if zombie.Status != StatusFailed || zombie.Error != ErrMsgServerRestart {
		t.Errorf("zombie = %+v, want failed with %q", zombie, ErrMsgServerRestart)
	}

	done, _ := s.Get(doneID)
	if done.Status != StatusDone {
		t.Errorf("done task status = %q, want %q", done.Status, StatusDone)
	}
}
