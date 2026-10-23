package retrieval

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// inspect 以独立连接读取落盘 schema 与 journal 模式。
// 刻意不走 Index 内部句柄：验证的是「物理落库的 schema」而非内存对象，
// 与 vault LAYERED-ARCHITECTURE §3「可独立使用」判据同向。
func inspect(t *testing.T, path string) (names map[string]bool, journalMode string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open inspect connection: %v", err)
	}
	defer db.Close()

	names = map[string]bool{}
	rows, err := db.Query("SELECT name FROM sqlite_master")
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan name: %v", err)
		}
		names[n] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}

	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	return names, journalMode
}

// TestOpen_CreatesSchema 验证 Open 一次性建齐检索底座 schema：
// doc_chunk/doc_fts/embed_cache/corpus_meta 及其索引恒建；
// doc_vec（vec0）仅 EmbedDim>0 时建——纯词法模式零向量残留（ADR-0017 决策 3）。
func TestOpen_CreatesSchema(t *testing.T) {
	t.Run("lexical only", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "lex.db")
		idx, err := Open(path, Options{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer idx.Close()

		names, journalMode := inspect(t, path)
		for _, want := range []string{
			"doc_chunk", "doc_fts", "embed_cache", "corpus_meta",
			"idx_doc_chunk_doc", "idx_doc_chunk_ver",
		} {
			if !names[want] {
				t.Errorf("schema missing %q; got %v", want, names)
			}
		}
		if names["doc_vec"] {
			t.Error("doc_vec must NOT exist when EmbedDim=0 (pure lexical mode)")
		}
		if journalMode != "wal" {
			t.Errorf("journal_mode = %q, want wal", journalMode)
		}
	})

	t.Run("with embed dim", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "vec.db")
		idx, err := Open(path, Options{EmbedDim: 8})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer idx.Close()

		names, _ := inspect(t, path)
		if !names["doc_vec"] {
			t.Errorf("doc_vec must exist when EmbedDim=8; got %v", names)
		}
	})
}

// TestOpen_Twice_Idempotent 验证同路径二次 Open 不报错：
// 全部 DDL 走 IF NOT EXISTS，重启/重开必须是幂等操作。
func TestOpen_Twice_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "twice.db")

	idx1, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := idx1.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	idx2, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if err := idx2.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
