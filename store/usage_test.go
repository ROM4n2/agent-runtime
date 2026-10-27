package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// TestStore_RecordUsage 内存实现：写入后回读一致，且不影响状态机字段。
func TestStore_RecordUsage(t *testing.T) {
	s := NewStore()
	id := s.Create("hello")

	if err := s.RecordUsage(id, 12, 34); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	task, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if task.PromptTokens != 12 || task.CompletionTokens != 34 {
		t.Errorf("usage = (%d,%d), want (12,34)", task.PromptTokens, task.CompletionTokens)
	}
	if task.Status != StatusPending {
		t.Errorf("status = %q, want pending（计量不得改动状态机）", task.Status)
	}
}

// TestStore_RecordUsageNotFound 不存在的 id 不得被静默创建出来。
func TestStore_RecordUsageNotFound(t *testing.T) {
	s := NewStore()
	if err := s.RecordUsage("nope", 1, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestSQLStore_RecordUsage SQLite 实现：写入后回读一致，且能覆盖已有值。
func TestSQLStore_RecordUsage(t *testing.T) {
	s := newSQLStore(t)
	id := s.Create("hello")

	if err := s.RecordUsage(id, 12, 34); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	task, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if task.PromptTokens != 12 || task.CompletionTokens != 34 {
		t.Errorf("usage = (%d,%d), want (12,34)", task.PromptTokens, task.CompletionTokens)
	}

	// 同一任务重试后可能再次计量：必须是覆盖而不是累加，
	// 累加会把「重试 3 次」的用量算成 3 份，账单直接翻倍。
	if err := s.RecordUsage(id, 5, 6); err != nil {
		t.Fatalf("RecordUsage (overwrite): %v", err)
	}
	task, _ = s.Get(id)
	if task.PromptTokens != 5 || task.CompletionTokens != 6 {
		t.Errorf("usage after overwrite = (%d,%d), want (5,6)", task.PromptTokens, task.CompletionTokens)
	}
}

// TestSQLStore_RecordUsageNotFound 不存在 id → ErrNotFound（与内存实现同语义）。
func TestSQLStore_RecordUsageNotFound(t *testing.T) {
	s := newSQLStore(t)
	if err := s.RecordUsage("nope", 1, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestSQLStore_ClaimNextCarriesUsage 抢占返回的 Task 必须带上计量列，
// 否则 worker 重放/排空路径会看到「用量归零」的假象。
func TestSQLStore_ClaimNextCarriesUsage(t *testing.T) {
	s := newSQLStore(t)
	id := s.Create("hello")
	if err := s.RecordUsage(id, 9, 8); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	task, err := s.ClaimNext(context.Background())
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if task.PromptTokens != 9 || task.CompletionTokens != 8 {
		t.Errorf("claimed usage = (%d,%d), want (9,8)", task.PromptTokens, task.CompletionTokens)
	}
}

// TestSQLStore_MigratesLegacyTaskTable 是 ADR-0021 决策 2 的升级门：
// 老库（无 token 列）必须能直接打开并自动补列，否则升级即启动失败。
func TestSQLStore_MigratesLegacyTaskTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// 手工建一个 P9 时代的 tasks 表（只有 session_id，无 token 列）。
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE tasks (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		status     TEXT NOT NULL,
		prompt     TEXT NOT NULL,
		result     TEXT NOT NULL DEFAULT '',
		error      TEXT NOT NULL DEFAULT '',
		session_id TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := legacy.Exec("INSERT INTO tasks (status, prompt) VALUES (?, ?)", StatusPending, "legacy"); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	// 新版本打开同一个库：补列成功、老行可读且用量为 0。
	s, err := NewSQLStore(path)
	if err != nil {
		t.Fatalf("NewSQLStore on legacy db: %v", err)
	}
	defer s.Close()

	task, err := s.Get("1")
	if err != nil {
		t.Fatalf("Get legacy row: %v", err)
	}
	if task.Prompt != "legacy" {
		t.Errorf("prompt = %q, want legacy", task.Prompt)
	}
	if task.PromptTokens != 0 || task.CompletionTokens != 0 {
		t.Errorf("legacy usage = (%d,%d), want (0,0)", task.PromptTokens, task.CompletionTokens)
	}
	if err := s.RecordUsage("1", 3, 4); err != nil {
		t.Fatalf("RecordUsage on migrated db: %v", err)
	}
}
