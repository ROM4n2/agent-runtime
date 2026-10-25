package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// initSessionSchema 初始化 sessions 与 session_messages 相关表及索引。
func initSessionSchema(db *sql.DB) error {
	schema := `
CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS session_messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL,
    role TEXT NOT NULL,
    content TEXT NOT NULL,
    created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_session_messages_session_id ON session_messages(session_id, id ASC);
`
	_, err := db.Exec(schema)
	return err
}

// GetOrCreateSession 获取已有会话；若不存在则持久化插入新会话。
// 读操作先走无锁并发查询，缺失时加锁排他写入，兼顾并发吞吐与幂等安全。
func (s *SQLStore) GetOrCreateSession(ctx context.Context, id string) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	if id == "" {
		return Session{}, errors.New("store: session id cannot be empty")
	}

	var sess Session
	err := s.db.QueryRowContext(ctx, "SELECT id, created_at, updated_at FROM sessions WHERE id = ?", id).
		Scan(&sess.ID, &sess.CreatedAt, &sess.UpdatedAt)
	if err == nil {
		sess.CreatedAt = sess.CreatedAt.UTC()
		sess.UpdatedAt = sess.UpdatedAt.UTC()
		return sess, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Session{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 双重校验，避免锁竞争窗口期重复插入
	err = s.db.QueryRowContext(ctx, "SELECT id, created_at, updated_at FROM sessions WHERE id = ?", id).
		Scan(&sess.ID, &sess.CreatedAt, &sess.UpdatedAt)
	if err == nil {
		sess.CreatedAt = sess.CreatedAt.UTC()
		sess.UpdatedAt = sess.UpdatedAt.UTC()
		return sess, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Session{}, err
	}

	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, "INSERT INTO sessions (id, created_at, updated_at) VALUES (?, ?, ?)", id, now, now)
	if err != nil {
		return Session{}, err
	}
	return Session{ID: id, CreatedAt: now, UpdatedAt: now}, nil
}

// AppendSessionMessage 追加消息至 session_messages 并更新会话 updated_at。
// 事务内原子完成消息插入与父会话时间戳刷新，保证数据一致性。
func (s *SQLStore) AppendSessionMessage(ctx context.Context, msg SessionMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if msg.SessionID == "" {
		return errors.New("store: session id cannot be empty")
	}

	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	} else {
		msg.CreatedAt = msg.CreatedAt.UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		"INSERT INTO session_messages (session_id, role, content, created_at) VALUES (?, ?, ?, ?)",
		msg.SessionID, msg.Role, msg.Content, msg.CreatedAt,
	)
	if err != nil {
		return err
	}

	res, err := tx.ExecContext(ctx, "UPDATE sessions SET updated_at = ? WHERE id = ?", msg.CreatedAt, msg.SessionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_, err = tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO sessions (id, created_at, updated_at) VALUES (?, ?, ?)",
			msg.SessionID, msg.CreatedAt, msg.CreatedAt,
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// GetSessionMessages 按时序 ASC 获取会话消息流，限制返回最近 limit 条。
// 利用子查询倒序 LIMIT 截取最新 N 条，外层重排回 ASC 顺序，无锁并发读取。
func (s *SQLStore) GetSessionMessages(ctx context.Context, sessionID string, limit int) ([]SessionMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}

	query := `SELECT id, session_id, role, content, created_at FROM (
		SELECT id, session_id, role, content, created_at FROM session_messages
		WHERE session_id = ? ORDER BY id DESC LIMIT ?
	) ORDER BY id ASC`

	rows, err := s.db.QueryContext(ctx, query, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	msgs := make([]SessionMessage, 0)
	for rows.Next() {
		var m SessionMessage
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.CreatedAt = m.CreatedAt.UTC()
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return msgs, nil
}
