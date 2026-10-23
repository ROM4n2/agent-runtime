package retrieval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestIndex 在临时目录开一个纯词法索引，测试结束自动关闭。
func newTestIndex(t *testing.T) *Index {
	t.Helper()
	idx, err := Open(filepath.Join(t.TempDir(), "retr.db"), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

// writeDoc 在语料目录写一篇文档（relPath 用 / 分隔）。
func writeDoc(t *testing.T, dir, relPath, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", relPath, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", relPath, err)
	}
}

// countChunks 直连底层库统计 doc_chunk 行数（物理证据，不走 Index 抽象）。
func countChunks(t *testing.T, idx *Index) int {
	t.Helper()
	var n int
	if err := idx.db.QueryRow("SELECT COUNT(*) FROM doc_chunk").Scan(&n); err != nil {
		t.Fatalf("count doc_chunk: %v", err)
	}
	return n
}

// ftsMatchCount 统计 doc_fts 中命中 query 的行数。
func ftsMatchCount(t *testing.T, idx *Index, query string) int {
	t.Helper()
	var n int
	if err := idx.db.QueryRow("SELECT COUNT(*) FROM doc_fts WHERE doc_fts MATCH ?", query).Scan(&n); err != nil {
		t.Fatalf("fts match %q: %v", query, err)
	}
	return n
}

// TestCorpusVersion_Empty 验证未建索引时 CorpusVersion 返回空串、不报错。
func TestCorpusVersion_Empty(t *testing.T) {
	idx := newTestIndex(t)
	v, err := idx.CorpusVersion()
	if err != nil {
		t.Fatalf("CorpusVersion: %v", err)
	}
	if v != "" {
		t.Errorf("CorpusVersion on unbuilt index = %q, want empty", v)
	}
}

// TestRebuild_Idempotent 是变异自证 ③ 的承重墙：
// 连续两次 Rebuild，doc_chunk 行数与 corpus_meta.version 不变，
// 且第二次 ChunksWritten=0（指纹命中即跳过，不重建）。
// 若删掉幂等跳过逻辑（每次都重建），第二次 ChunksWritten=0 断言转红。
func TestRebuild_Idempotent(t *testing.T) {
	idx := newTestIndex(t)
	corpus := t.TempDir()
	writeDoc(t, corpus, "a.md", "检索系统设计文档")
	writeDoc(t, corpus, "b.md", "向量数据库性能测试")

	r1, err := idx.Rebuild(context.Background(), corpus)
	if err != nil {
		t.Fatalf("first Rebuild: %v", err)
	}
	if r1.ChunksWritten == 0 {
		t.Fatal("first Rebuild should write chunks")
	}
	if r1.Fingerprint == "" {
		t.Error("first Rebuild report must carry fingerprint")
	}
	rows1 := countChunks(t, idx)
	ver1, err := idx.CorpusVersion()
	if err != nil {
		t.Fatalf("CorpusVersion after build: %v", err)
	}
	if ver1 == "" {
		t.Fatal("corpus_meta.version must be set after build")
	}
	if ver1 != r1.Fingerprint {
		t.Errorf("corpus_meta.version = %q, want report fingerprint %q", ver1, r1.Fingerprint)
	}

	r2, err := idx.Rebuild(context.Background(), corpus)
	if err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	if r2.ChunksWritten != 0 {
		t.Errorf("second Rebuild ChunksWritten = %d, want 0 (idempotent skip)", r2.ChunksWritten)
	}
	if rows2 := countChunks(t, idx); rows2 != rows1 {
		t.Errorf("doc_chunk row count changed on idempotent rebuild: %d -> %d", rows1, rows2)
	}
	ver2, err := idx.CorpusVersion()
	if err != nil {
		t.Fatalf("CorpusVersion after second build: %v", err)
	}
	if ver2 != ver1 {
		t.Errorf("corpus_meta.version changed on idempotent rebuild: %q -> %q", ver1, ver2)
	}
}

// TestRebuild_ChunkedTxn 是变异自证 ① 的承重墙之一：
// >50 chunk 的语料必须构建成功且可查（FTS5 命中）。
// 若分块事务改成「永不提交」，数据不落库 → 可查断言转红。
func TestRebuild_ChunkedTxn(t *testing.T) {
	idx := newTestIndex(t)
	corpus := t.TempDir()
	// 单篇大文档：每 chunk 512 rune、步长 448，此规模产 >50 chunk，跨多个事务。
	big := strings.Repeat("检索系统性能测试文档内容", 3000)
	writeDoc(t, corpus, "big.md", big)

	r, err := idx.Rebuild(context.Background(), corpus)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if r.ChunksWritten <= 50 {
		t.Errorf("ChunksWritten = %d, want > 50 (chunked across txns)", r.ChunksWritten)
	}
	if got := countChunks(t, idx); got != r.ChunksWritten {
		t.Errorf("doc_chunk rows = %d, want ChunksWritten %d", got, r.ChunksWritten)
	}
	// 可查：3+ 字子串查询必须命中（原文插入 trigram 表）。
	if n := ftsMatchCount(t, idx, "检索系统"); n == 0 {
		t.Error("expected FTS match for '检索系统' after chunked rebuild")
	}
}

// TestRebuild_EmptyDir 验证空目录合法：0 文档、不报错，且写入 corpus_meta（可判新鲜。
func TestRebuild_EmptyDir(t *testing.T) {
	idx := newTestIndex(t)
	corpus := t.TempDir()

	r, err := idx.Rebuild(context.Background(), corpus)
	if err != nil {
		t.Fatalf("Rebuild empty: %v", err)
	}
	if r.DocsIndexed != 0 || r.DocsSkipped != 0 || r.ChunksWritten != 0 {
		t.Errorf("empty corpus report = %+v, want all zeros", r)
	}
	v, err := idx.CorpusVersion()
	if err != nil {
		t.Fatalf("CorpusVersion: %v", err)
	}
	if v == "" {
		t.Error("empty corpus build should still record a corpus version")
	}
	if err := idx.CheckFresh(context.Background(), corpus); err != nil {
		t.Errorf("CheckFresh after empty build = %v, want nil", err)
	}
}

// TestRebuild_MissingDir 验证语料目录不存在 → 返回永久错误（启动失败）。
func TestRebuild_MissingDir(t *testing.T) {
	idx := newTestIndex(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := idx.Rebuild(context.Background(), missing); err == nil {
		t.Fatal("Rebuild on missing dir must return error")
	}
}

// TestRebuild_SkipsUnreadableFile 验证单文件读失败：跳过并 DocsSkipped+1，不报错。
// Windows 上用 syscall 独占锁制造不可读普通文件（chmod 在 Windows 无效）。
func TestRebuild_SkipsUnreadableFile(t *testing.T) {
	idx := newTestIndex(t)
	corpus := t.TempDir()
	writeDoc(t, corpus, "ok.md", "可读文档内容")

	locked := filepath.Join(corpus, "locked.md")
	if err := os.WriteFile(locked, []byte("不可读内容"), 0o644); err != nil {
		t.Fatalf("write locked: %v", err)
	}
	unlock := lockFile(t, locked)
	t.Cleanup(unlock) // 先于 t.TempDir 清理执行，避免 Windows 句柄占用

	r, err := idx.Rebuild(context.Background(), corpus)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if r.DocsIndexed != 1 {
		t.Errorf("DocsIndexed = %d, want 1", r.DocsIndexed)
	}
	if r.DocsSkipped != 1 {
		t.Errorf("DocsSkipped = %d, want 1 (unreadable file skipped, not silent)", r.DocsSkipped)
	}
}

// TestRebuild_CorpusExcludesNonMarkdownAndEval 是 harness 缺陷修复的承重墙（RED→GREEN）：
// 混合语料 Rebuild 后 doc_chunk 只能含 .md 文档且 docs/eval 被排除（物理证据，不走抽象）。
// 若 listDocs 不限定 .md → .go/.db/.git 内容进索引，本用例转红；
// 若 docs/eval 不被排除 → 评测报表在评测期间写入即改指纹，触发无谓全量 Rebuild。
func TestRebuild_CorpusExcludesNonMarkdownAndEval(t *testing.T) {
	idx := newTestIndex(t)
	corpus := t.TempDir()
	writeDoc(t, corpus, "a.md", "检索系统设计文档")
	writeDoc(t, corpus, "sub/b.md", "向量数据库性能测试")
	writeDoc(t, corpus, "main.go", "package retrieval // 不该入索引")
	writeDoc(t, corpus, "agent-runtime.db", "SQLite format 3 二进制不该入索引")
	writeDoc(t, corpus, "docs/eval/report.md", "评测报表不该入语料")

	r, err := idx.Rebuild(context.Background(), corpus)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if r.DocsIndexed != 2 {
		t.Errorf("DocsIndexed = %d, want 2 (only the two .md docs outside docs/eval)", r.DocsIndexed)
	}
	rows, err := idx.db.Query("SELECT DISTINCT doc_id FROM doc_chunk ORDER BY doc_id")
	if err != nil {
		t.Fatalf("query doc_id: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if want := []string{"a.md", "sub/b.md"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("doc_chunk doc_ids = %v, want %v", got, want)
	}
}

// TestCheckFresh_NotBuilt 验证索引未建 → ErrIndexMissing。
func TestCheckFresh_NotBuilt(t *testing.T) {
	idx := newTestIndex(t)
	corpus := t.TempDir()
	writeDoc(t, corpus, "a.md", "内容")
	if err := idx.CheckFresh(context.Background(), corpus); !errors.Is(err, ErrIndexMissing) {
		t.Fatalf("CheckFresh on unbuilt index = %v, want ErrIndexMissing", err)
	}
}

// TestCheckFresh_Fresh 验证语料未变 → nil。
func TestCheckFresh_Fresh(t *testing.T) {
	idx := newTestIndex(t)
	corpus := t.TempDir()
	writeDoc(t, corpus, "a.md", "内容")
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if err := idx.CheckFresh(context.Background(), corpus); err != nil {
		t.Errorf("CheckFresh on fresh index = %v, want nil", err)
	}
}

// TestCheckFresh_Stale 验证语料改动 → ErrCorpusStale。
func TestCheckFresh_Stale(t *testing.T) {
	idx := newTestIndex(t)
	corpus := t.TempDir()
	writeDoc(t, corpus, "a.md", "原始内容")
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	writeDoc(t, corpus, "a.md", "改动后的内容")
	if err := idx.CheckFresh(context.Background(), corpus); !errors.Is(err, ErrCorpusStale) {
		t.Fatalf("CheckFresh after corpus edit = %v, want ErrCorpusStale", err)
	}
}

// TestRebuild_ContextCancel 验证已取消 ctx → 返回 ctx.Err()（取消语义到底）。
func TestRebuild_ContextCancel(t *testing.T) {
	idx := newTestIndex(t)
	corpus := t.TempDir()
	writeDoc(t, corpus, "big.md", strings.Repeat("检索系统性能测试", 3000)) // 大语料

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预取消

	if _, err := idx.Rebuild(ctx, corpus); !errors.Is(err, context.Canceled) {
		t.Fatalf("Rebuild with cancelled ctx = %v, want context.Canceled", err)
	}
}

// TestFTS_OriginalTextMatchable 固化「Rebuild 插入原文、3+ 字子串查询可命中」的语义。
func TestFTS_OriginalTextMatchable(t *testing.T) {
	idx := newTestIndex(t)
	corpus := t.TempDir()
	writeDoc(t, corpus, "a.md", "检索系统设计与实现")
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if n := ftsMatchCount(t, idx, "检索系统"); n == 0 {
		t.Error("original CJK text must be matchable by 3+ char substring query")
	}
}

// TestFTS_BigramSplitProducesEmptyIndex 固化实证发现，作为不采用 bigram 预切的依据：
// ADR-0017 决策 3 提议的 bigram 预切文本在现代 C trigram 分词器下产生空索引
// （2 字 run 不足以构成 trigram），连 3+ 字查询都零命中。
// 若未来有人改回 bigram 预切，本用例转红警示。
func TestFTS_BigramSplitProducesEmptyIndex(t *testing.T) {
	idx := newTestIndex(t)
	res, err := idx.db.Exec(
		"INSERT INTO doc_chunk(doc_id, source, span_start, span_end, text, corpus_version) VALUES ('probe','probe',0,4,'检索系统','v')")
	if err != nil {
		t.Fatalf("insert probe chunk: %v", err)
	}
	rid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	if _, err := idx.db.Exec("INSERT INTO doc_fts(rowid, text) VALUES (?, ?)", rid, bigramSplit("检索系统")); err != nil {
		t.Fatalf("insert probe fts: %v", err)
	}
	var n int
	if err := idx.db.QueryRow(
		"SELECT COUNT(*) FROM doc_fts WHERE doc_fts MATCH ? AND rowid = ?", "检索系统", rid).Scan(&n); err != nil {
		t.Fatalf("query probe: %v", err)
	}
	if n != 0 {
		t.Errorf("bigram-split text must be unsearchable under trigram (empty index); got %d matches", n)
	}
}
