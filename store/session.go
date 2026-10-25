package store

import (
	"context"
	"errors"
	"time"
)

// Session 代表一个多轮会话的元数据实体。
type Session struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SessionMessage 代表会话中单条持久化消息（角色、文本与时序）。
type SessionMessage struct {
	ID        int64     `json:"id"`
	SessionID string    `json:"session_id"`
	Role      string    `json:"role"` // "user" | "assistant" | "system"
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// SessionStore 定义多轮交互所需的会话元数据与消息流持久化契约。
type SessionStore interface {
	GetOrCreateSession(ctx context.Context, id string) (Session, error)
	AppendSessionMessage(ctx context.Context, msg SessionMessage) error
	GetSessionMessages(ctx context.Context, sessionID string, limit int) ([]SessionMessage, error)
}

// GetOrCreateSession 获取已有会话；若不存在则原子创建并返回。
// 加锁防止并发读写 map 引发 panic 与脏读。
func (s *Store) GetOrCreateSession(ctx context.Context, id string) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	if id == "" {
		return Session{}, errors.New("store: session id cannot be empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sessions == nil {
		s.sessions = make(map[string]Session)
	}

	if sess, ok := s.sessions[id]; ok {
		return sess, nil
	}

	now := time.Now().UTC()
	sess := Session{
		ID:        id,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.sessions[id] = sess
	return sess, nil
}

// AppendSessionMessage 追加一条消息至指定会话的消息流中，并更新会话的 UpdatedAt。
// 消息 ID 严格单调自增，时序未传时保底赋值当前 UTC 时间。
func (s *Store) AppendSessionMessage(ctx context.Context, msg SessionMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if msg.SessionID == "" {
		return errors.New("store: session id cannot be empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sessionMsgs == nil {
		s.sessionMsgs = make(map[string][]SessionMessage)
	}
	if s.sessions == nil {
		s.sessions = make(map[string]Session)
	}

	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	} else {
		msg.CreatedAt = msg.CreatedAt.UTC()
	}

	s.nextMsgID++
	msg.ID = s.nextMsgID
	s.sessionMsgs[msg.SessionID] = append(s.sessionMsgs[msg.SessionID], msg)

	if sess, ok := s.sessions[msg.SessionID]; ok {
		sess.UpdatedAt = msg.CreatedAt
		s.sessions[msg.SessionID] = sess
	} else {
		s.sessions[msg.SessionID] = Session{
			ID:        msg.SessionID,
			CreatedAt: msg.CreatedAt,
			UpdatedAt: msg.CreatedAt,
		}
	}

	return nil
}

// GetSessionMessages 按时序 ASC 获取指定会话的消息列表；limit <= 0 时默认拉取全部。
// 当 limit > 0 且消息总数超过 limit 时，截取尾部最新 limit 条消息（按 ASC 排列）。
func (s *Store) GetSessionMessages(ctx context.Context, sessionID string, limit int) ([]SessionMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	msgs := s.sessionMsgs[sessionID]
	if len(msgs) == 0 {
		return []SessionMessage{}, nil
	}

	sub := msgs
	if limit > 0 && len(msgs) > limit {
		sub = msgs[len(msgs)-limit:]
	}

	result := make([]SessionMessage, len(sub))
	copy(result, sub)
	return result, nil
}
