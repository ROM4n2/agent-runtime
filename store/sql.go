package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// SQLStore 用嵌入式 SQLite 持久化任务，进程重启后任务不丢。
//
// 写操作整体串行（mu）：SQLite 同一时刻只允许一个写者，
// 并发写会直接报 "database is locked"；用 mu 把写串行化后避免该错误。
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
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		status     TEXT NOT NULL,
		prompt     TEXT NOT NULL,
		result     TEXT NOT NULL DEFAULT '',
		error      TEXT NOT NULL DEFAULT '',
		session_id TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		db.Close()
		return nil, err
	}
	// 兼容已有数据库迁移，确保 session_id 列存在
	if _, err := db.Exec("ALTER TABLE tasks ADD COLUMN session_id TEXT NOT NULL DEFAULT ''"); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		db.Close()
		return nil, fmt.Errorf("sqlstore: migrate tasks session_id: %w", err)
	}
	if err := initSessionSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLStore{db: db, path: path}, nil
}

// Close 释放底层数据库连接。进程停机时调用。
func (s *SQLStore) Close() error {
	return s.db.Close()
}

// CreateInSession 写入带会话关联的 pending 任务并由 SQLite 原生自增主键生成 id 返回。
// 极端情况下写入失败返回空串，调用方会据此让该次提交失败而非静默丢失。
func (s *SQLStore) CreateInSession(prompt, sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(
		"INSERT INTO tasks (status, prompt, session_id) VALUES (?, ?, ?)",
		StatusPending, prompt, sessionID,
	)
	if err != nil {
		slog.Error("sqlstore: create in session", "error", err)
		return ""
	}
	lastID, err := res.LastInsertId()
	if err != nil {
		slog.Error("sqlstore: last insert id", "error", err)
		return ""
	}
	return strconv.FormatInt(lastID, 10)
}

// Create 写入独立 pending 任务并返回 id。
func (s *SQLStore) Create(prompt string) string {
	return s.CreateInSession(prompt, "")
}

// Get 按 id 读取任务；不存在返回 ErrNotFound（与内存实现语义一致）。
// 读不加 mu，允许与写并发。
func (s *SQLStore) Get(id string) (Task, error) {
	var t Task
	err := s.db.QueryRow(
		"SELECT id, status, prompt, result, error, session_id FROM tasks WHERE id = ?", id,
	).Scan(&t.ID, &t.Status, &t.Prompt, &t.Result, &t.Error, &t.SessionID)
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

// RecoverTasks 实现掉电/被 Kill 后的启动补偿：
//   - 所有 pending 按自增主键升序返回，由 main 重新灌入 worker 队列；
//   - 所有 running 是上次进程非正常死亡的「幽灵任务」，批量标定 failed，
//     避免客户端永久轮询到一个永远不变的状态。
//
// 两步在同一事务内完成：要么全部补偿成功，要么全部回滚。
func (s *SQLStore) RecoverTasks(ctx context.Context) ([]string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback() // 已 Commit 后 Rollback 返回 ErrTxDone，可安全忽略

	rows, err := tx.QueryContext(ctx, "SELECT id FROM tasks WHERE status = ? ORDER BY id ASC", StatusPending)
	if err != nil {
		return nil, 0, err
	}
	var pending []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, 0, err
		}
		pending = append(pending, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, err
	}
	rows.Close()

	res, err := tx.ExecContext(ctx,
		"UPDATE tasks SET status = ?, error = ? WHERE status = ?",
		StatusFailed, ErrMsgServerRestart, StatusRunning,
	)
	if err != nil {
		return nil, 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, 0, err
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return pending, int(n), nil
}

// PendingCount 返回处于 pending 状态的任务积压数。
// 纯只读统计，WAL 模式下并发安全且不阻塞写操作。
func (s *SQLStore) PendingCount(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var count int64
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE status = ?", StatusPending).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

// ClaimNext 原子抢占并返回最老的一个 pending 任务，并将其状态迁移为 running。
// 利用互斥锁串行化写操作，结合 CAS（WHERE id = ? AND status = 'pending'）保证
// 高并发争抢下同一任务绝不被重复消费。无待处理任务时返回 ErrNoTaskAvailable。
func (s *SQLStore) ClaimNext(ctx context.Context) (Task, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var t Task
	err := s.db.QueryRowContext(ctx,
		"SELECT id, prompt, result, error, session_id FROM tasks WHERE status = ? ORDER BY id ASC LIMIT 1",
		StatusPending,
	).Scan(&t.ID, &t.Prompt, &t.Result, &t.Error, &t.SessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNoTaskAvailable
	}
	if err != nil {
		return Task{}, err
	}

	res, err := s.db.ExecContext(ctx,
		"UPDATE tasks SET status = ? WHERE id = ? AND status = ?",
		StatusRunning, t.ID, StatusPending,
	)
	if err != nil {
		return Task{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Task{}, err
	}
	if n == 0 {
		return Task{}, ErrNoTaskAvailable
	}

	t.Status = StatusRunning
	return t, nil
}
