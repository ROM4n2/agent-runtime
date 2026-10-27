package store

import (
	"strings"
	"testing"
)

// planDetail 返回给定查询的 EXPLAIN QUERY PLAN 中所有 detail 行拼接结果。
// 用它把「是否全表扫描」变成可断言的机械事实，而不是靠读 DDL 猜。
func planDetail(t *testing.T, s *SQLStore, query string, args ...any) string {
	t.Helper()
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN %q: %v", query, err)
	}
	defer rows.Close()

	var parts []string
	for rows.Next() {
		var (
			id, parent, notused int
			detail              string
		)
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		parts = append(parts, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return strings.Join(parts, " | ")
}

// TestSQLStore_ClaimNextUsesIndex 是 P9 积压扫描缺陷的承重墙：
// 没有 idx_tasks_status 时，ClaimNext 的 SELECT 会对 tasks 全表 SCAN，
// 在 10,000 条积压（README 宣称的削峰容量）下每次抢占都扫全表。
// 本测试锁死「必须走索引」，防止索引被后续 DDL 改动静默删除。
func TestSQLStore_ClaimNextUsesIndex(t *testing.T) {
	s := newSQLStore(t)
	for i := 0; i < 200; i++ {
		s.Create("seed")
	}

	query := "SELECT id, prompt, result, error, session_id FROM tasks WHERE status = ? ORDER BY id ASC LIMIT 1"
	detail := planDetail(t, s, query, StatusPending)

	if strings.Contains(detail, "SCAN tasks") {
		t.Fatalf("ClaimNext 的 SELECT 仍全表扫描: %s\n"+
			"修复：在 tasks(status, id) 上建索引", detail)
	}
	if !strings.Contains(detail, "idx_tasks_status") {
		t.Fatalf("ClaimNext 的 SELECT 未使用 idx_tasks_status: %s", detail)
	}
}

// TestSQLStore_PendingCountUsesIndex 锁死 PendingCount 的聚合查询同样走索引：
// 它在每次 Enqueue 与 EnqueueWait 轮询（10ms 间隔）里被调用，全表扫描代价被轮询放大。
func TestSQLStore_PendingCountUsesIndex(t *testing.T) {
	s := newSQLStore(t)
	for i := 0; i < 200; i++ {
		s.Create("seed")
	}

	detail := planDetail(t, s, "SELECT COUNT(*) FROM tasks WHERE status = ?", StatusPending)

	if strings.Contains(detail, "SCAN tasks") {
		t.Fatalf("PendingCount 仍全表扫描: %s", detail)
	}
	if !strings.Contains(detail, "idx_tasks_status") {
		t.Fatalf("PendingCount 未使用 idx_tasks_status: %s", detail)
	}
}
