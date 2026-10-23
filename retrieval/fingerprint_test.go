package retrieval

import (
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
