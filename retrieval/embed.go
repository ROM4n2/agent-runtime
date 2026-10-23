package retrieval

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"
)

// embed_cache 的治理参数（ADR-0017 决策 5）：TTL 与容量上限必须有界，
// 否则 SQLite 缓存随语料增长无界膨胀（进程内 map 的 OOM 教训同源）。
const (
	// embedCacheTTL 是缓存行存活期：created_at 早于 now-TTL 视为未命中，读取时惰性删除。
	embedCacheTTL = 7 * 24 * time.Hour
	// embedCacheMaxRows 是容量上限：超限按 created_at FIFO 删最旧。
	embedCacheMaxRows = 10000
	// embedConcurrency 是未命中文本并发调 Embedder 的上限：嵌入走网络，限流防打爆上游。
	embedConcurrency = 4
)

// ErrNoEmbedder 表示未配置 Embedder（纯词法模式下误用向量通道的显式失败）。
// non-retryable 语义由 Task 7 工具层包装分类，本任务只保证错误明确、不被吞。
var ErrNoEmbedder = errors.New("retrieval: no embedder configured")

// embedTexts 把一批文本嵌入为 float32 向量，缓存优先（ADR-0017 决策 5）：
//  1. Options.Embedder 为 nil → ErrNoEmbedder（调用方不得静默降级到词法）；
//  2. 逐文本算缓存键 SHA-256(model|dim|text)，命中即反序列化返回；
//  3. 未命中文本并发（上限 embedConcurrency）调 Embedder，按输入顺序聚合；
//  4. 结果落库 embed_cache（INSERT OR REPLACE），TTL 7 天 + 容量上限 1 万行 FIFO。
//
// Embedder 出错原样上抛（不吞、不返回半成品），可重试性分类由 Task 7 工具层负责。
func (idx *Index) embedTexts(ctx context.Context, texts []string) ([][]float32, error) {
	if idx.opts.Embedder == nil {
		return nil, ErrNoEmbedder
	}
	if len(texts) == 0 {
		return [][]float32{}, nil
	}

	out := make([][]float32, len(texts))
	missIdx := make([]int, 0, len(texts))

	// 缓存查询：命中直接填位，未命中记录下标待嵌入。
	for i, t := range texts {
		vec, ok, err := idx.cacheGet(ctx, t)
		if err != nil {
			return nil, err
		}
		if ok {
			out[i] = vec
			continue
		}
		missIdx = append(missIdx, i)
	}
	if len(missIdx) == 0 {
		return out, nil
	}

	// 未命中并发嵌入，按输入顺序聚合。
	missTexts := make([]string, len(missIdx))
	for j, i := range missIdx {
		missTexts[j] = texts[i]
	}
	vecs, err := idx.embedMisses(ctx, missTexts)
	if err != nil {
		return nil, err
	}

	// 落库（失败即整体失败：缓存是优化不是真相源，不静默丢写入）。
	for j, i := range missIdx {
		out[i] = vecs[j]
		if err := idx.cachePut(ctx, texts[i], vecs[j]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// embedMisses 对未命中文本并发调 Embedder（上限 embedConcurrency），按输入顺序聚合。
// 任一失败即整批失败：向量检索要么全有要么显式报错，不返回半成品（No-Silent-Degrade）。
func (idx *Index) embedMisses(ctx context.Context, texts []string) ([][]float32, error) {
	results := make([][]float32, len(texts))
	sem := make(chan struct{}, embedConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}

	for i, t := range texts {
		wg.Add(1)
		go func(i int, t string) {
			defer wg.Done()
			// 令牌获取与 ctx 取消竞态：取消即不发起调用。
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				fail(ctx.Err())
				return
			}
			vecs, err := idx.opts.Embedder.Embed(ctx, []string{t})
			if err != nil {
				fail(err)
				return
			}
			if len(vecs) != 1 || len(vecs[0]) == 0 {
				fail(fmt.Errorf("retrieval: embedder returned %d vectors for 1 text", len(vecs)))
				return
			}
			results[i] = vecs[0] // 各 goroutine 写互异下标，无数据竞争
		}(i, t)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}

// cacheKey 是 embed_cache 主键：SHA-256(model|dim|text) 十六进制。
// model 与 dim 进键：同一文本换模型/维度必须视为不同向量（ADR-0017 决策 5）。
func (idx *Index) cacheKey(text string) string {
	sum := sha256.Sum256([]byte(idx.opts.EmbedModel + "|" + strconv.Itoa(idx.opts.EmbedDim) + "|" + text))
	return hex.EncodeToString(sum[:])
}

// cacheGet 查 embed_cache：未命中与过期（created_at 早于 now-TTL）均视为未命中，
// 过期行顺手删除（惰性清理，避免过期行永久占容量）。
func (idx *Index) cacheGet(ctx context.Context, text string) ([]float32, bool, error) {
	key := idx.cacheKey(text)
	var blob []byte
	var createdAt int64
	err := idx.db.QueryRowContext(ctx,
		"SELECT vector, created_at FROM embed_cache WHERE text_hash = ?", key).Scan(&blob, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if time.Since(time.Unix(createdAt, 0)) > embedCacheTTL {
		if _, err := idx.db.ExecContext(ctx, "DELETE FROM embed_cache WHERE text_hash = ?", key); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	return dencodeVec(blob), true, nil
}

// cachePut 把新嵌入结果落库（INSERT OR REPLACE 覆盖同键旧值），
// 写入后执行容量上限 FIFO 收敛（删最旧 created_at）。
func (idx *Index) cachePut(ctx context.Context, text string, vec []float32) error {
	key := idx.cacheKey(text)
	if _, err := idx.db.ExecContext(ctx,
		"INSERT OR REPLACE INTO embed_cache(text_hash, model, dim, vector, created_at) VALUES (?, ?, ?, ?, ?)",
		key, idx.opts.EmbedModel, idx.opts.EmbedDim, encodeVec(vec), time.Now().Unix()); err != nil {
		return err
	}
	return idx.evictOverflow(ctx)
}

// evictOverflow 把 embed_cache 收敛到容量上限：超过 embedCacheMaxRows 时
// 按 created_at 删最旧的 excess 行（FIFO），防缓存无界增长（ADR-0017 决策 5）。
func (idx *Index) evictOverflow(ctx context.Context) error {
	var n int
	if err := idx.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM embed_cache").Scan(&n); err != nil {
		return err
	}
	excess := n - embedCacheMaxRows
	if excess <= 0 {
		return nil
	}
	_, err := idx.db.ExecContext(ctx,
		"DELETE FROM embed_cache WHERE text_hash IN (SELECT text_hash FROM embed_cache ORDER BY created_at LIMIT ?)",
		excess)
	return err
}

// encodeVec 把 float32 向量编码为 little-endian BLOB。
// 这是全包唯一的向量编码实现（Task 6 写 doc_vec 复用）：格式与 sqlite-vec 的
// 物理存储布局一致（float32 小端），两边不许各写一份。
func encodeVec(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// dencodeVec 把 little-endian BLOB 解码回 float32 向量（encodeVec 的逆）。
func dencodeVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}
