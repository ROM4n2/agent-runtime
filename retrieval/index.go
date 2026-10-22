// Package retrieval 实现 ADR-0017「检索即工具」的索引底座：
// 独立 SQLite 库（与 store 包的同款连接配置分持两连接，ADR-0017 决策 2 的显式设计——
// store 管任务状态、retrieval 管检索索引，互不抢占写锁），
// FTS5 词法通道默认开启，vec0 向量通道仅当 EmbedDim>0 时启用（默认纯词法，决策 3）。
//
// 依赖方向单向向上（vault LAYERED-ARCHITECTURE §3 / ADR-0013）：
// 本包只消费 database/sql 与 modernc.org/sqlite（含同模块 vec 子包），
// Task 5 起经 EmbedderFunc 消费 llm 的嵌入能力；agent/worker/api 不得反向 import
// （architecture_test.go 静态门禁把守）。
package retrieval

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
	_ "modernc.org/sqlite/vec"
)

// EmbedderFunc 把一批文本嵌入为定长 float32 向量（dim 由配置方保证一致）。
//
// 占位说明：llm.Embedder 接口要到 Task 5 才在 llm 包落地，
// 本任务先用本包定义的函数类型自足占位，避免依赖尚未存在的类型；
// Task 5 由装配层把 llm.Client 的 Embed 适配成本函数类型注入，字段语义不变。
type EmbedderFunc func(ctx context.Context, texts []string) ([][]float32, error)

// Options 配置索引打开方式。
// Embedder 为 nil 或 EmbedDim<=0 → 纯词法模式：不建 vec0 表，
// 无 embedding 凭证时显式走 lexical（ADR-0017 决策 3，禁静默降级）。
type Options struct {
	Embedder EmbedderFunc
	EmbedDim int
}

// Index 是检索索引句柄：持有独立数据库连接，后续 Task 4+ 在其上构建/检索。
type Index struct {
	db   *sql.DB
	opts Options
}

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

// Open 打开（必要时创建）位于 path 的检索索引库并建齐 schema。
// EmbedDim>0 时才建 vec0 虚拟表；调用方负责 Close。
func Open(path string, opts Options) (*Index, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// 与 store/sql.go:27-52 同款的连接配置分持两连接（ADR-0017 决策 2 的显式设计）：
	// WAL 让检索读与 store 写互不阻塞；busy_timeout 让写冲突短等而非立即报错。
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, err
	}
	if err := createSchema(db, opts.EmbedDim); err != nil {
		db.Close()
		return nil, err
	}
	return &Index{db: db, opts: opts}, nil
}

// Close 释放底层数据库连接。进程停机时调用。
func (idx *Index) Close() error {
	return idx.db.Close()
}

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
