package retrieval

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeRaw 在 dir 下写一个文件（relPath 用 / 分隔）。
func writeRaw(t *testing.T, dir, relPath, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", relPath, err)
	}
}

// TestDocHash_Deterministic 验证同内容同哈希（确定性）。
func TestDocHash_Deterministic(t *testing.T) {
	content := []byte("检索系统设计文档")
	if docHash(content) != docHash(content) {
		t.Error("docHash must be deterministic for identical content")
	}
}

// TestDocHash_SensitiveToOneByte 验证改 1 字节哈希即变（内容寻址敏感）。
func TestDocHash_SensitiveToOneByte(t *testing.T) {
	a := []byte("检索系统设计文档")
	b := []byte("检索系统设计文挡") // 最后一字不同
	if docHash(a) == docHash(b) {
		t.Error("docHash must change when content changes by one character")
	}
}

// TestDirFingerprint_IgnoresMtime 是变异自证 ② 的承重墙：
// 两个目录内容与相对路径完全一致、仅 mtime 不同，指纹必须相同。
// 若指纹混入 mtime，本用例转红。
func TestDirFingerprint_IgnoresMtime(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	writeRaw(t, dir1, "a.md", "相同内容甲")
	writeRaw(t, dir2, "a.md", "相同内容甲")

	old := time.Unix(1000, 0)
	newer := time.Unix(999999999, 0)
	if err := os.Chtimes(filepath.Join(dir1, "a.md"), old, old); err != nil {
		t.Fatalf("chtimes dir1: %v", err)
	}
	if err := os.Chtimes(filepath.Join(dir2, "a.md"), newer, newer); err != nil {
		t.Fatalf("chtimes dir2: %v", err)
	}

	fp1, err := dirFingerprint(dir1)
	if err != nil {
		t.Fatalf("dirFingerprint dir1: %v", err)
	}
	fp2, err := dirFingerprint(dir2)
	if err != nil {
		t.Fatalf("dirFingerprint dir2: %v", err)
	}
	if fp1 != fp2 {
		t.Errorf("mtime must NOT affect fingerprint: %s vs %s", fp1, fp2)
	}
}

// TestDirFingerprint_Stable 验证目录级聚合稳定（重复计算一致，与 map 迭代顺序无关）。
func TestDirFingerprint_Stable(t *testing.T) {
	dir := t.TempDir()
	writeRaw(t, dir, "a.md", "内容甲")
	writeRaw(t, dir, "b.md", "内容乙")
	writeRaw(t, dir, "sub/c.md", "内容丙")

	fp1, err := dirFingerprint(dir)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	fp2, err := dirFingerprint(dir)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if fp1 != fp2 {
		t.Errorf("dirFingerprint must be stable: %s vs %s", fp1, fp2)
	}
}

// TestDirFingerprint_ChangesOnContentEdit 验证改任一文档内容指纹即变。
func TestDirFingerprint_ChangesOnContentEdit(t *testing.T) {
	dir := t.TempDir()
	writeRaw(t, dir, "a.md", "原始内容")
	before, err := dirFingerprint(dir)
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	writeRaw(t, dir, "a.md", "修改后的内容")
	after, err := dirFingerprint(dir)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before == after {
		t.Error("dirFingerprint must change after content edit")
	}
}

// TestDirFingerprint_PathMatters 验证路径进聚合：同内容移到不同相对路径，指纹变。
// 这与 TestDirFingerprint_IgnoresMtime 互补——mtime 不进、路径进。
func TestDirFingerprint_PathMatters(t *testing.T) {
	dir := t.TempDir()
	writeRaw(t, dir, "a.md", "一样的内容")
	fpA, _ := dirFingerprint(dir)

	dir2 := t.TempDir()
	writeRaw(t, dir2, "renamed.md", "一样的内容")
	fpB, _ := dirFingerprint(dir2)

	if fpA == fpB {
		t.Error("dirFingerprint must differ when the same content sits at a different relative path")
	}
}

// TestListDocs_OnlyMarkdownExcludesEvalArtifacts 是 harness 缺陷修复的承重墙（RED→GREEN）：
// 语料限定 .md（大小写不敏感）且排除 docs/eval/（评测产物不是语料）。
// 若 listDocs 退回「收全部常规文件」→ .go/.db/.git 与 docs/eval 报告全部入列，本用例转红。
// 背景：二进制文件（索引库自身 .db/.db-wal）入语料会让库越索引越大、指纹永不命中，
// 形成 库大→索引多→库更大 的正反馈自噬循环；评测报表在评测期间被写进语料，指纹永不符。
func TestListDocs_OnlyMarkdownExcludesEvalArtifacts(t *testing.T) {
	dir := t.TempDir()
	writeRaw(t, dir, "a.md", "甲内容")
	writeRaw(t, dir, "notes.MD", "乙内容")                     // 大写后缀同样算 markdown
	writeRaw(t, dir, "sub/c.md", "丙内容")                     // 子目录 .md 照收
	writeRaw(t, dir, "main.go", "package x")                // 非 .md：排除
	writeRaw(t, dir, "agent-runtime.db", "SQLite format 3") // 二进制：排除
	writeRaw(t, dir, "agent-runtime.db-wal", "wal bytes")   // 二进制：排除
	writeRaw(t, dir, ".git/config", "[core]")               // 元数据：排除
	writeRaw(t, dir, "docs/eval/report.md", "评测报表")         // 评测产物：排除
	writeRaw(t, dir, "docs/adr/0004-x.md", "ADR 正文")        // 普通 md：照收

	docs, err := listDocs(dir)
	if err != nil {
		t.Fatalf("listDocs: %v", err)
	}
	var rels []string
	for _, d := range docs {
		rels = append(rels, d.relPath)
	}
	want := []string{"a.md", "docs/adr/0004-x.md", "notes.MD", "sub/c.md"}
	if len(rels) != len(want) {
		t.Fatalf("listDocs rels = %v, want %v", rels, want)
	}
	for i := range want {
		if rels[i] != want[i] {
			t.Fatalf("listDocs rels = %v, want %v (only .md, sorted, docs/eval excluded)", rels, want)
		}
	}
}

// TestListDocs_ExcludesHiddenPaths 是 harness 缺陷 D 的承重墙（RED→GREEN）：
// relPath 的任一路径段以 . 开头（隐藏文件/隐藏目录）一律不进语料。
// 背景（Task 12 缺陷 D 实证）：真实语料 49 个 .md 中有 9 个是隐藏路径
// （.agent-context.md、.agent-rules.md、.claude/CLAUDE.md、.codebuddy/memory/*.md、
// .trae/rules/git-commit-message.md），它们由 IDE/agent 会话后台持续改写 →
// corpus 指纹永不稳定 → CheckFresh 恒返回 ErrCorpusStale → 每次 search_docs 都失败
// （评测 72/90 wrong-span 全属假红）。若 listDocs 丢掉 isHiddenPath 过滤，本用例转红。
func TestListDocs_ExcludesHiddenPaths(t *testing.T) {
	dir := t.TempDir()
	writeRaw(t, dir, "a.md", "甲内容")              // 普通 md：照收
	writeRaw(t, dir, "docs/x.md", "乙内容")         // 普通子目录 md：照收
	writeRaw(t, dir, ".hidden/secret.md", "丙内容") // 隐藏目录：排除
	writeRaw(t, dir, ".root-hidden.md", "丁内容")   // 根下隐藏文件：排除

	docs, err := listDocs(dir)
	if err != nil {
		t.Fatalf("listDocs: %v", err)
	}
	var rels []string
	for _, d := range docs {
		rels = append(rels, d.relPath)
	}
	want := []string{"a.md", "docs/x.md"}
	if len(rels) != len(want) || rels[0] != want[0] || rels[1] != want[1] {
		t.Fatalf("listDocs rels = %v, want %v (hidden files/dirs must be excluded)", rels, want)
	}
}

// TestRebuild_ExcludesHiddenDocs 是缺陷 D 的集成承重墙：隐藏路径既不进 doc_chunk
// 也不参与指纹聚合——因此它被 IDE/agent 会话后台改写不会令索引陈旧。
func TestRebuild_ExcludesHiddenDocs(t *testing.T) {
	idx := newTestIndex(t)
	corpus := t.TempDir()
	writeDoc(t, corpus, "a.md", "可见文档甲内容")
	writeDoc(t, corpus, "docs/x.md", "可见文档乙内容")
	writeDoc(t, corpus, ".hidden/secret.md", "隐藏目录丙内容")
	writeDoc(t, corpus, ".root-hidden.md", "根下隐藏丁内容")

	r, err := idx.Rebuild(context.Background(), corpus)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	var hidden int
	if err := idx.db.QueryRow(`SELECT COUNT(*) FROM doc_chunk WHERE source LIKE '.%' OR source LIKE '%/.%'`).Scan(&hidden); err != nil {
		t.Fatalf("count hidden doc_chunk: %v", err)
	}
	if hidden != 0 {
		t.Errorf("doc_chunk contains %d hidden-path rows, want 0", hidden)
	}
	if n := ftsMatchCount(t, idx, "隐藏目录丙内容"); n != 0 {
		t.Errorf("hidden doc must not be searchable, got %d FTS hits", n)
	}
	if n := ftsMatchCount(t, idx, "可见文档甲内容"); n == 0 {
		t.Error("visible doc must be searchable")
	}

	// 指纹必须与「只放可见文档」的语料一致：隐藏路径完全不参与聚合。
	same := t.TempDir()
	writeDoc(t, same, "a.md", "可见文档甲内容")
	writeDoc(t, same, "docs/x.md", "可见文档乙内容")
	wantFP, err := dirFingerprint(same)
	if err != nil {
		t.Fatalf("dirFingerprint same: %v", err)
	}
	if r.Fingerprint != wantFP {
		t.Errorf("fingerprint polluted by hidden paths: %s, want %s", r.Fingerprint, wantFP)
	}
}

// TestDirFingerprint_EmptyDir 验证空目录合法（返回稳定的空聚合指纹，不报错）。
func TestDirFingerprint_EmptyDir(t *testing.T) {
	fp, err := dirFingerprint(t.TempDir())
	if err != nil {
		t.Fatalf("dirFingerprint empty: %v", err)
	}
	if fp == "" {
		t.Error("empty dir fingerprint must be non-empty (well-defined empty aggregate)")
	}
}
