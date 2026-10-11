package store

import (
	"database/sql"
	"errors"
	"log/slog"
	"strconv"
	"sync"

	_ "modernc.org/sqlite"
)

// SQLStore 用嵌入式 SQLite 持久化任务，进程重启后任务不丢。
//
// 写操作整体串行（mu）：SQLite 同一时刻只允许一个写者，
// 并发写会直接报 "database is locked"；用 mu 把写串行化后，既避免该错误，
// 也保证 Create 里"取最大 id+1"的读-改-写不被打断。
// 读操作不加 mu，配合 WAL 模式可与写并发，互不阻塞。
type SQLStore struct {
	db   *sql.DB
	mu   sync.Mutex
	path string
}

// NewSQLStore 打开（必要时创建）位于 path 的 SQLite 数据库并建表。
// path 应为非空文件路径；调用方负责保证路径可写。
func NewSQLStore(path string) (*SQLStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// WAL：读不阻塞写、崩溃恢复更稳；busy_timeout 让写冲突时短等而非立刻报错。
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS tasks (
		id     TEXT PRIMARY KEY,
		status TEXT NOT NULL,
		prompt TEXT NOT NULL,
		result TEXT NOT NULL DEFAULT '',
		error  TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLStore{db: db, path: path}, nil
}

// Close 释放底层数据库连接。进程停机时调用。
func (s *SQLStore) Close() error {
	return s.db.Close()
}

// Create 在 mu 保护下取当前最大 id+1 作为新 id，写入 pending 任务并返回。
// 极端情况下写入失败返回空串，调用方会据此让该次提交失败而非静默丢失。
func (s *SQLStore) Create(prompt string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var n int
	_ = s.db.QueryRow("SELECT COALESCE(MAX(CAST(id AS INTEGER)), 0) FROM tasks").Scan(&n)
	id := strconv.Itoa(n + 1)

	if _, err := s.db.Exec(
		"INSERT INTO tasks (id, status, prompt) VALUES (?, ?, ?)",
		id, StatusPending, prompt,
	); err != nil {
		slog.Error("sqlstore: create", "id", id, "error", err)
		return ""
	}
	return id
}

// Get 按 id 读取任务；不存在返回 ErrNotFound（与内存实现语义一致）。
// 读不加 mu，允许与写并发。
func (s *SQLStore) Get(id string) (Task, error) {
	var t Task
	err := s.db.QueryRow(
		"SELECT id, status, prompt, result, error FROM tasks WHERE id = ?", id,
	).Scan(&t.ID, &t.Status, &t.Prompt, &t.Result, &t.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, err
	}
	return t, nil
}

// Update 只迁移状态；id 不存在返回 ErrNotFound。
func (s *SQLStore) Update(id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec("UPDATE tasks SET status = ? WHERE id = ?", status, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Complete 写入终态：err 为空记 done，否则记 failed。
// 终态规则与内存实现一致，由本方法统一裁决，杜绝"done 却带 error"。
func (s *SQLStore) Complete(ID, result string, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	status := StatusDone
	var e string
	if err != nil {
		status = StatusFailed
		e = err.Error()
	}

	res, execErr := s.db.Exec(
		"UPDATE tasks SET status = ?, result = ?, error = ? WHERE id = ?",
		status, result, e, ID,
	)
	if execErr != nil {
		return execErr
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete 按 id 删除任务；id 不存在返回 ErrNotFound（与内存实现语义一致）。
// 依赖 RowsAffected 判断是否命中，因为 DELETE 不命中是「成功删了 0 行」而非错误。
func (s *SQLStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec("DELETE FROM tasks WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
