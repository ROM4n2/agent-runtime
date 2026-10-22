package retrieval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakeEmbedder 是可控的假嵌入器：计数调用次数，返回确定性伪向量。
// 伪向量首元素 = 文本字节长度、其余按位号递增——「按输入顺序聚合」可被断言，乱序即值错位。
type fakeEmbedder struct {
	mu    sync.Mutex
	calls int
	dim   int
	err   error // 非空则每次调用原样返回该错误
}

func (f *fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, f.dim)
		v[0] = float32(len(t))
		for j := 1; j < f.dim; j++ {
			v[j] = float32(j)
		}
		out[i] = v
	}
	return out, nil
}

func (f *fakeEmbedder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// openEmbedIndex 以给定 model/dim 打开带 Embedder 的索引（测试结束自动关闭）。
func openEmbedIndex(t *testing.T, path string, emb *fakeEmbedder, model string, dim int) *Index {
	t.Helper()
	idx, err := Open(path, Options{Embedder: emb, EmbedModel: model, EmbedDim: dim})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

// TestEmbedTexts_CacheHit 验证缓存命中：第二次同文本零调用，
// 且 BLOB round-trip 后的向量与首次一致、按输入顺序聚合。
func TestEmbedTexts_CacheHit(t *testing.T) {
	fake := &fakeEmbedder{dim: 4}
	idx := openEmbedIndex(t, filepath.Join(t.TempDir(), "hit.db"), fake, "m1", 4)
	ctx := context.Background()

	texts := []string{"检索系统", "向量数据库", "短"}
	got1, err := idx.embedTexts(ctx, texts)
	if err != nil {
		t.Fatalf("first embedTexts: %v", err)
	}
	if n := fake.callCount(); n != len(texts) {
		t.Fatalf("first embedTexts calls = %d, want %d", n, len(texts))
	}

	got2, err := idx.embedTexts(ctx, texts)
	if err != nil {
		t.Fatalf("second embedTexts: %v", err)
	}
	if n := fake.callCount(); n != len(texts) {
		t.Errorf("second embedTexts must be all cache hits: calls = %d, want %d", n, len(texts))
	}
	if !reflect.DeepEqual(got1, got2) {
		t.Errorf("cached vectors mismatch after BLOB round-trip: %v vs %v", got1, got2)
	}
	// 按输入顺序聚合：伪向量首元素 = 文本字节长度，错位即红。
	for i, txt := range texts {
		if got1[i][0] != float32(len(txt)) {
			t.Errorf("vector[%d][0] = %v, want len(%q) = %d (input order must be preserved)", i, got1[i][0], txt, len(txt))
		}
	}
}

// TestEmbedTexts_CacheKeyIncludesModelAndDim 是变异自证 ① 的承重墙：
// 缓存键 = SHA-256(model|dim|text)，同文本换 model 或换 dim 必须 Miss。
// 若键只含 text，第二个 Index 命中第一个的缓存 → calls 停在 1 → 本用例转红。
func TestEmbedTexts_CacheKeyIncludesModelAndDim(t *testing.T) {
	ctx := context.Background()

	// 两个 Index 共享同一库文件：缓存键的区分度只有在同一缓存池里才可观测。
	t.Run("different model", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "model.db")
		fake := &fakeEmbedder{dim: 4}
		a := openEmbedIndex(t, path, fake, "model-a", 4)
		b := openEmbedIndex(t, path, fake, "model-b", 4)
		if _, err := a.embedTexts(ctx, []string{"same text"}); err != nil {
			t.Fatalf("embed on model-a: %v", err)
		}
		if _, err := b.embedTexts(ctx, []string{"same text"}); err != nil {
			t.Fatalf("embed on model-b: %v", err)
		}
		if n := fake.callCount(); n != 2 {
			t.Errorf("same text under different models must not share cache: calls = %d, want 2", n)
		}
	})

	t.Run("different dim", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dim.db")
		fake := &fakeEmbedder{dim: 4} // 假向量维度与键无关，此处只验键的 dim 分量
		a := openEmbedIndex(t, path, fake, "m", 4)
		b := openEmbedIndex(t, path, fake, "m", 8)
		if _, err := a.embedTexts(ctx, []string{"same text"}); err != nil {
			t.Fatalf("embed on dim=4: %v", err)
		}
		if _, err := b.embedTexts(ctx, []string{"same text"}); err != nil {
			t.Fatalf("embed on dim=8: %v", err)
		}
		if n := fake.callCount(); n != 2 {
			t.Errorf("same text under different dims must not share cache: calls = %d, want 2", n)
		}
	})
}

// TestEmbedTexts_TTLExpiryRecomputes 是变异自证 ② 的承重墙：
// created_at 早于 now-TTL(7d) 的行视为未命中并删除，重新嵌入。
// 若不过期，第二次命中旧值 → calls 停在 1 → 本用例转红。
func TestEmbedTexts_TTLExpiryRecomputes(t *testing.T) {
	fake := &fakeEmbedder{dim: 4}
	idx := openEmbedIndex(t, filepath.Join(t.TempDir(), "ttl.db"), fake, "m", 4)
	ctx := context.Background()

	if _, err := idx.embedTexts(ctx, []string{"hello"}); err != nil {
		t.Fatalf("first embedTexts: %v", err)
	}
	if n := fake.callCount(); n != 1 {
		t.Fatalf("first embedTexts calls = %d, want 1", n)
	}

	// 手工把该行 created_at 改到 8 天前（超过 TTL 7 天）。
	stale := time.Now().Add(-8 * 24 * time.Hour).Unix()
	if _, err := idx.db.Exec("UPDATE embed_cache SET created_at = ? WHERE text_hash = ?", stale, idx.cacheKey("hello")); err != nil {
		t.Fatalf("stale the row: %v", err)
	}

	if _, err := idx.embedTexts(ctx, []string{"hello"}); err != nil {
		t.Fatalf("second embedTexts: %v", err)
	}
	if n := fake.callCount(); n != 2 {
		t.Errorf("expired cache row must trigger recompute: calls = %d, want 2", n)
	}

	// 过期行被删除后由新结果替换：created_at 回归 TTL 内。
	var createdAt int64
	if err := idx.db.QueryRow("SELECT created_at FROM embed_cache WHERE text_hash = ?", idx.cacheKey("hello")).Scan(&createdAt); err != nil {
		t.Fatalf("query refreshed row: %v", err)
	}
	if time.Since(time.Unix(createdAt, 0)) > embedCacheTTL {
		t.Error("cache row must be refreshed (created_at within TTL) after recompute")
	}
}

// TestEmbedTexts_CacheFIFO 是变异自证 ③ 的承重墙：
// 预填 1 万行（容量上限）后再嵌一个新文本 → 恰好删最旧 1 行，总量回落上限。
// 若超限时改成无条件全删，行数断言转红。
func TestEmbedTexts_CacheFIFO(t *testing.T) {
	fake := &fakeEmbedder{dim: 4}
	idx := openEmbedIndex(t, filepath.Join(t.TempDir(), "fifo.db"), fake, "m", 4)
	ctx := context.Background()

	// 预填容量上限行数，created_at 升序 = 插入顺序（最旧 = fifo-00001）。
	tx, err := idx.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 1; i <= embedCacheMaxRows; i++ {
		if _, err := tx.Exec(
			"INSERT INTO embed_cache(text_hash, model, dim, vector, created_at) VALUES (?, ?, ?, ?, ?)",
			fmt.Sprintf("fifo-%05d", i), "m", 4, encodeVec([]float32{float32(i)}), int64(i),
		); err != nil {
			_ = tx.Rollback()
			t.Fatalf("prefill %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit prefill: %v", err)
	}

	// 第 10001 个不同文本：触发容量收敛。
	if _, err := idx.embedTexts(ctx, []string{"overflow trigger"}); err != nil {
		t.Fatalf("embedTexts overflow: %v", err)
	}

	var n int
	if err := idx.db.QueryRow("SELECT COUNT(*) FROM embed_cache").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != embedCacheMaxRows {
		t.Errorf("embed_cache rows = %d, want %d (evict exactly the excess)", n, embedCacheMaxRows)
	}
	var oldest int
	if err := idx.db.QueryRow("SELECT COUNT(*) FROM embed_cache WHERE text_hash = ?", "fifo-00001").Scan(&oldest); err != nil {
		t.Fatalf("count oldest: %v", err)
	}
	if oldest != 0 {
		t.Error("oldest row fifo-00001 must be evicted (FIFO by created_at)")
	}
	var newest int
	if err := idx.db.QueryRow(
		"SELECT COUNT(*) FROM embed_cache WHERE text_hash = ?", fmt.Sprintf("fifo-%05d", embedCacheMaxRows)).Scan(&newest); err != nil {
		t.Fatalf("count newest: %v", err)
	}
	if newest != 1 {
		t.Error("newest prefilled row must survive FIFO eviction")
	}
}

// TestEmbedTexts_EmbedderErrorPropagates 验证 Embedder 出错时原样上抛（不吞），
// 且失败结果不落库（不缓存错误）。retryable/non-retryable 分类由 Task 7 工具层负责。
func TestEmbedTexts_EmbedderErrorPropagates(t *testing.T) {
	boom := errors.New("embed upstream boom")
	fake := &fakeEmbedder{dim: 4, err: boom}
	idx := openEmbedIndex(t, filepath.Join(t.TempDir(), "err.db"), fake, "m", 4)

	_, err := idx.embedTexts(context.Background(), []string{"x"})
	if !errors.Is(err, boom) {
		t.Fatalf("embedTexts must propagate embedder error verbatim: got %v", err)
	}
	var n int
	if err := idx.db.QueryRow("SELECT COUNT(*) FROM embed_cache").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("failed embed must not be cached: %d rows", n)
	}
}

// TestEmbedTexts_NilEmbedder 验证未配 Embedder 时返回明确错误（禁静默降级）。
func TestEmbedTexts_NilEmbedder(t *testing.T) {
	idx := newTestIndex(t) // Options{}：无 Embedder
	if _, err := idx.embedTexts(context.Background(), []string{"x"}); !errors.Is(err, ErrNoEmbedder) {
		t.Fatalf("embedTexts with nil embedder = %v, want ErrNoEmbedder", err)
	}
}

// TestVecCodec_RoundTrip 钉死 []float32 ↔ BLOB little-endian 的单一编解码实现：
//  round-trip 精确无损；1.0f 的字节序为 00 00 80 3F（与 vec0 BLOB 布局一致）。
func TestVecCodec_RoundTrip(t *testing.T) {
	orig := []float32{0, 1.5, -2.25, 3.14159, 1e-30, -1e30}
	got := dencodeVec(encodeVec(orig))
	if !reflect.DeepEqual(orig, got) {
		t.Errorf("round-trip mismatch: %v -> %v", orig, got)
	}
	if b := encodeVec([]float32{1}); !bytes.Equal(b, []byte{0x00, 0x00, 0x80, 0x3F}) {
		t.Errorf("encodeVec(1.0) = % x, want 00 00 80 3f (little-endian float32)", b)
	}
}

// TestRebuild_ClearsDocVec 是预授权改动 2 的承重墙：
// 上一轮嵌入通道在 doc_vec 留下与 chunk_id 对齐的向量行，语料变更触发重建时
// 必须一并清空（只删内容表会留下 rowid 指向已删 chunk 的孤儿向量行）。
// 反向由纯词法模式的既有重建测试把守：无条件 DELETE FROM doc_vec 会因
// "no such table: doc_vec" 让全部 Rebuild 用例转红。
func TestRebuild_ClearsDocVec(t *testing.T) {
	idx := openEmbedIndex(t, filepath.Join(t.TempDir(), "vec.db"), &fakeEmbedder{dim: 4}, "m", 4)
	corpus := t.TempDir()
	writeDoc(t, corpus, "a.md", "检索系统设计")

	// 第一轮 Rebuild：doc_chunk + doc_fts 一致落库（insertChunk 同事务保不变量）。
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("first Rebuild: %v", err)
	}

	// 模拟 Task 6 嵌入通道的残留：doc_vec 行 rowid 与 doc_chunk.chunk_id 对齐。
	var chunkID int64
	if err := idx.db.QueryRow("SELECT chunk_id FROM doc_chunk LIMIT 1").Scan(&chunkID); err != nil {
		t.Fatalf("query chunk_id: %v", err)
	}
	if _, err := idx.db.Exec("INSERT INTO doc_vec(rowid, embedding) VALUES (?, ?)", chunkID, encodeVec([]float32{1, 2, 3, 4})); err != nil {
		t.Fatalf("insert doc_vec: %v", err)
	}

	// 改动语料 → 指纹变化 → 第二轮 Rebuild 真正执行 clearIndex。
	writeDoc(t, corpus, "a.md", "检索系统设计修订版")
	if _, err := idx.Rebuild(context.Background(), corpus); err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}

	var n int
	if err := idx.db.QueryRow("SELECT COUNT(*) FROM doc_vec").Scan(&n); err != nil {
		t.Fatalf("count doc_vec: %v", err)
	}
	if n != 0 {
		t.Errorf("doc_vec rows = %d after rebuild, want 0 (orphan vector rows must be cleared)", n)
	}
}
