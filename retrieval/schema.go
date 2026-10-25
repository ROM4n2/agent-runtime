package retrieval

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DDL 全部集中于此并一律 IF NOT EXISTS：重复 Open / 重启必须是幂等操作。
const (
	ddlDocChunk = `CREATE TABLE IF NOT EXISTS doc_chunk (
		chunk_id       INTEGER PRIMARY KEY AUTOINCREMENT,
		doc_id         TEXT NOT NULL,
		source         TEXT NOT NULL,
		span_start     INTEGER NOT NULL,
		span_end       INTEGER NOT NULL,
		text           TEXT NOT NULL,
		corpus_version TEXT NOT NULL
	)`
	ddlIdxDocChunkDoc = `CREATE INDEX IF NOT EXISTS idx_doc_chunk_doc ON doc_chunk(doc_id)`
	ddlIdxDocChunkVer = `CREATE INDEX IF NOT EXISTS idx_doc_chunk_ver ON doc_chunk(corpus_version)`
	// doc_fts 为外接内容表（external content）FTS5：正文字符只存 doc_chunk 一份，
	// content_rowid 指向 chunk_id；trigram tokenizer 让 CJK 无分词也能子串匹配。
	ddlDocFTS = `CREATE VIRTUAL TABLE IF NOT EXISTS doc_fts USING fts5(
		text, content=doc_chunk, content_rowid=chunk_id, tokenize='trigram'
	)`
	ddlEmbedCache = `CREATE TABLE IF NOT EXISTS embed_cache (
		text_hash  TEXT PRIMARY KEY,
		model      TEXT NOT NULL,
		dim        INTEGER NOT NULL,
		vector     BLOB NOT NULL,
		created_at INTEGER NOT NULL
	)`
	// singleton 上的 CHECK(singleton=1) 把 corpus_meta 钉成单行表：
	// 全库只允许一条语料版本记录，插入第二行直接违反约束。
	ddlCorpusMeta = `CREATE TABLE IF NOT EXISTS corpus_meta (
		singleton INTEGER PRIMARY KEY CHECK(singleton=1),
		version    TEXT NOT NULL,
		built_at   INTEGER NOT NULL
	)`
	// ddlDocVecFormat 是全包唯一允许 fmt.Sprintf 拼接的 SQL 模板：
	// vec0 的维度只能是 DDL 字面量整数，SQL 没有参数绑定途径；
	// %d 来自受信配置（config 层已做正整数校验），不是终端用户输入。
	ddlDocVecFormat = `CREATE VIRTUAL TABLE IF NOT EXISTS doc_vec USING vec0(embedding float[%d])`
)

// createSchema 按维度决策建表：EmbedDim<=0 走纯词法模式，不建 vec0。
func createSchema(db *sql.DB, embedDim int) error {
	stmts := []string{
		ddlDocChunk,
		ddlIdxDocChunkDoc,
		ddlIdxDocChunkVer,
		ddlDocFTS,
		ddlEmbedCache,
		ddlCorpusMeta,
	}
	if embedDim > 0 {
		stmts = append(stmts, fmt.Sprintf(ddlDocVecFormat, embedDim))
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("retrieval: create schema: %w", err)
		}
	}
	return nil
}

// clearIndex 在同一事务内清空旧索引：删 FTS 索引 + 内容表（外接内容表两者都清才算全量重建）。
// vec0 表存在时（EmbedDim>0 建的库）同步清空 doc_vec：doc_vec 的 rowid 与 doc_chunk.chunk_id
// 对齐，只删内容表会留下无主向量行（孤儿行会被 KNN 检索误命中）。
// 按 sqlite_master 物理存在性决策而非内存 opts：库可能以 EmbedDim>0 创建、后以 EmbedDim<=0 重开。
func (idx *Index) clearIndex(ctx context.Context) error {
	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM doc_fts"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM doc_chunk"); err != nil {
		return err
	}
	vecExists, err := tableExists(ctx, tx, "doc_vec")
	if err != nil {
		return err
	}
	if vecExists {
		if _, err := tx.ExecContext(ctx, "DELETE FROM doc_vec"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// tableExists 检查库中是否存在名为 name 的表/视图（含 vec0 虚拟表，其 sqlite_master type 为 table）。
func tableExists(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var n string
	err := tx.QueryRowContext(ctx, "SELECT name FROM sqlite_master WHERE name = ?", name).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// writeCorpusMeta 在同一事务内把 corpus_meta 更新为单行新指纹（CHECK(singleton=1) 钉死单行）。
func (idx *Index) writeCorpusMeta(ctx context.Context, version string, builtAt int64) error {
	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM corpus_meta"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO corpus_meta(singleton, version, built_at) VALUES (1, ?, ?)", version, builtAt); err != nil {
		return err
	}
	return tx.Commit()
}
