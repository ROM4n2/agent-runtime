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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

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

// 哨兵错误（ADR-0017 决策 5/6）：索引缺失与语料陈旧均为不可重试的显式失败，
// 由上层（Task 7 工具层 / Task 8 装配层）映射为独立终态分类，绝不静默降级。
var (
	// ErrIndexMissing 表示索引尚未构建（corpus_meta 无记录）。
	ErrIndexMissing = errors.New("retrieval: index not built")
	// ErrCorpusStale 表示语料内容指纹与已建索引版本不符，需重建。
	ErrCorpusStale = errors.New("retrieval: corpus version mismatch, rebuild required")
)

// ChunkBatchSize 是分块事务的批量上限：每事务最多插入这么多个 chunk 即提交，
// 禁止全量单事务（长写持锁会阻塞任务写入，store <480 QPS 教训同源；ADR-0017 决策 6）。
const ChunkBatchSize = 50

// BuildReport 汇总一次 Rebuild 的结果。
// 语义约定：当指纹命中、跳过重建时，DocsIndexed=0 且 ChunksWritten=0（本次未重建），
// Fingerprint 仍回传当前指纹供对账；调用方据 ChunksWritten==0 判断「无需重建」。
type BuildReport struct {
	DocsIndexed   int    // 本次实际索引（读取并分块成功）的文档数
	DocsSkipped   int    // 读取失败被跳过的文档数（可观测，不静默）
	ChunksWritten int    // 本次写入 doc_chunk/doc_fts 的 chunk 数
	Fingerprint   string // 本次构建对应的目录内容指纹
}

// chunkRow 是待批量插入 doc_chunk 的一条分块记录。
type chunkRow struct {
	docID     string
	source    string
	spanStart int
	spanEnd   int
	text      string
}

// CorpusVersion 读取 corpus_meta 中的已建语料指纹；无记录（未建索引）返回空串。
func (idx *Index) CorpusVersion() (string, error) {
	var version string
	err := idx.db.QueryRow("SELECT version FROM corpus_meta WHERE singleton = 1").Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return version, nil
}

// CheckFresh 实时计算语料目录指纹并与已建版本比对（ADR-0017 决策 6 的调用时检查点）：
//   - 索引未建（corpus_meta 无记录）→ ErrIndexMissing；
//   - 指纹不符 → ErrCorpusStale；
//   - 一致 → nil。
//
// 与 Search 解耦：由工具层在检索前调用，Search 本身不做实时全量内容哈希。
func (idx *Index) CheckFresh(ctx context.Context, corpusDir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	version, err := idx.CorpusVersion()
	if err != nil {
		return err
	}
	if version == "" {
		return ErrIndexMissing
	}
	fp, err := dirFingerprint(corpusDir)
	if err != nil {
		return err
	}
	if fp != version {
		return ErrCorpusStale
	}
	return nil
}

// Rebuild 幂等地把 corpusDir 构建为 FTS5 可检索索引（ADR-0017 决策 6）：
//  1. 语料目录不存在/非目录 → 返回永久错误（启动失败，不静默）；
//  2. 计算目录内容指纹，与 corpus_meta 已建版本比对，相同即跳过（见 BuildReport 语义）；
//  3. 否则全量重建：清空旧索引 → 分块事务批量 INSERT（每 ≤ChunkBatchSize 个一提交，
//     事务间检查 ctx 取消）→ 成功后更新 corpus_meta 指纹。
//
// 崩溃半成品靠指纹不符触发下次重建（corpus_meta 仍记旧指纹）。本任务只建词法通道，
// 不碰 doc_vec（vec0 由 Task 6 负责）。
func (idx *Index) Rebuild(ctx context.Context, corpusDir string) (BuildReport, error) {
	info, err := os.Stat(corpusDir)
	if err != nil {
		return BuildReport{}, fmt.Errorf("retrieval: corpus dir %q: %w", corpusDir, err)
	}
	if !info.IsDir() {
		return BuildReport{}, fmt.Errorf("retrieval: corpus path %q is not a directory", corpusDir)
	}

	fp, err := dirFingerprint(corpusDir)
	if err != nil {
		return BuildReport{}, err
	}

	current, err := idx.CorpusVersion()
	if err != nil {
		return BuildReport{}, err
	}
	if current == fp {
		// 幂等跳过：索引已是最新。DocsIndexed=0/ChunksWritten=0 表示本次未重建。
		return BuildReport{Fingerprint: fp}, nil
	}

	return idx.rebuildChunks(ctx, corpusDir, fp)
}

// rebuildChunks 执行全量重建的写入阶段（清空 → 分块事务插入 → 更新 corpus_meta）。
func (idx *Index) rebuildChunks(ctx context.Context, corpusDir, fingerprint string) (BuildReport, error) {
	// 入口取消检查：已取消 ctx 立即返回，避免无谓扫描与写入。
	if err := ctx.Err(); err != nil {
		return BuildReport{}, err
	}

	docs, err := listDocs(corpusDir)
	if err != nil {
		return BuildReport{}, err
	}

	report := BuildReport{Fingerprint: fingerprint}
	builtAt := time.Now().Unix()

	// 全量重建先清空旧索引（外接内容表需同时删 FTS 索引与内容表）。
	// 崩溃半成品靠指纹不符触发下次重建（corpus_meta 仍记旧指纹）。
	if err := idx.clearIndex(ctx); err != nil {
		return BuildReport{}, err
	}

	var pending []chunkRow
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		tx, err := idx.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() // 已 Commit 后 Rollback 返回 ErrTxDone，可安全忽略
		for _, c := range pending {
			if err := insertChunk(ctx, tx, c, fingerprint); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		report.ChunksWritten += len(pending)
		pending = pending[:0]
		return nil
	}

	for _, d := range docs {
		content, err := os.ReadFile(d.absPath)
		if err != nil {
			// 单文件读失败：跳过并计入 DocsSkipped（可观测，不静默）。
			slog.Warn("retrieval: rebuild skip unreadable doc", "path", d.relPath, "error", err)
			report.DocsSkipped++
			continue
		}
		for _, ch := range chunkText(string(content)) {
			pending = append(pending, chunkRow{
				docID:     d.relPath,
				source:    d.relPath,
				spanStart: ch.SpanStart,
				spanEnd:   ch.SpanEnd,
				text:      ch.Text,
			})
			if len(pending) >= ChunkBatchSize {
				if err := flush(); err != nil {
					return BuildReport{}, err
				}
				// 事务间检查取消：取消即返回，半成品靠指纹不符触发下次重建。
				if err := ctx.Err(); err != nil {
					return BuildReport{}, err
				}
			}
		}
		report.DocsIndexed++
	}
	if err := flush(); err != nil {
		return BuildReport{}, err
	}

	// 成功后更新 corpus_meta（删旧行 + 插新指纹，紧随最后一个分块事务）。
	if err := idx.writeCorpusMeta(ctx, fingerprint, builtAt); err != nil {
		return BuildReport{}, err
	}
	return report, nil
}

// insertChunk 在事务内插入一条分块到 doc_chunk，并同步写入外接内容 FTS 索引 doc_fts。
// doc_fts 用 content=doc_chunk 外接内容表：显式提供 rowid(=chunk_id) 与要索引的文本（原文）。
func insertChunk(ctx context.Context, tx *sql.Tx, c chunkRow, version string) error {
	res, err := tx.ExecContext(ctx,
		"INSERT INTO doc_chunk(doc_id, source, span_start, span_end, text, corpus_version) VALUES (?, ?, ?, ?, ?, ?)",
		c.docID, c.source, c.spanStart, c.spanEnd, c.text, version)
	if err != nil {
		return err
	}
	rowid, err := res.LastInsertId()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO doc_fts(rowid, text) VALUES (?, ?)", rowid, c.text)
	return err
}

// clearIndex 在同一事务内清空旧索引：删 FTS 索引 + 内容表（外接内容表两者都清才算全量重建）。
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
	return tx.Commit()
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
