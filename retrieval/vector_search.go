package retrieval

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SearchVector 走 vec0 KNN 向量召回：嵌入查询串后取距离最近的 limit 个 chunk。
// 向量通道关闭（Options.Embedder 为 nil 或 EmbedDim<=0）→ 显式错误，
// 复用 ErrNoEmbedder 哨兵（语义即「纯词法模式下误用向量通道」）；
// 禁静默降级——词法照样能跑不代表可以偷偷返回词法结果。
func (idx *Index) SearchVector(ctx context.Context, query string, limit int) ([]Hit, error) {
	if idx.opts.Embedder == nil {
		return nil, ErrNoEmbedder
	}
	if idx.opts.EmbedDim <= 0 {
		return nil, fmt.Errorf("retrieval: vector channel disabled, EmbedDim=%d: %w", idx.opts.EmbedDim, ErrNoEmbedder)
	}
	vecs, err := idx.embedTexts(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	return idx.knnHits(ctx, vecs[0], limit)
}

// knnRow 是 vec0 KNN 的一条原始结果：向量行 rowid（=chunk_id）与距离。
type knnRow struct {
	rowid int64
	dist  float64
}

// knnHits 执行 vec0 KNN 并把结果补成完整 Hit。
// 两步而非 JOIN：vec0 要求 LIMIT/k 约束直接作用于 KNN 扫描本身，
// JOIN 形态下 modernc 报 "A LIMIT or 'k = ?' constraint is required on vec0 knn queries"
// （v1.58.0 实测）。先取 (rowid, distance)，再按 rowid 回查 doc_chunk 拿引用字段。
func (idx *Index) knnHits(ctx context.Context, queryVec []float32, limit int) ([]Hit, error) {
	if len(queryVec) == 0 {
		return nil, fmt.Errorf("retrieval: empty query vector, want dim %d", idx.opts.EmbedDim)
	}
	order, err := idx.knnOrder(ctx, queryVec, limit)
	if err != nil {
		return nil, err
	}
	// 按 rowid 回查 doc_chunk，保持 KNN 顺序（距离升序）。
	hits := make([]Hit, 0, len(order))
	for _, k := range order {
		h, ok, err := idx.chunkHit(ctx, k.rowid)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // 孤儿向量行（doc_vec 有、doc_chunk 无）：跳过，不补假数据
		}
		h.Score = k.dist
		hits = append(hits, h)
	}
	return hits, nil
}

// knnOrder 跑 vec0 KNN，返回按距离升序的 (rowid, distance) 序列。
// 游标在本函数内关闭：回查 doc_chunk 前不能占着连接。
func (idx *Index) knnOrder(ctx context.Context, queryVec []float32, limit int) ([]knnRow, error) {
	rows, err := idx.db.QueryContext(ctx,
		"SELECT rowid, distance FROM doc_vec WHERE embedding MATCH ? ORDER BY distance LIMIT ?",
		encodeVec(queryVec), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var order []knnRow
	for rows.Next() {
		var k knnRow
		if err := rows.Scan(&k.rowid, &k.dist); err != nil {
			return nil, err
		}
		order = append(order, k)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return order, nil
}

// chunkHit 按 chunk_id 取一条引用记录；无对应行返回 ok=false（孤儿向量行）。
func (idx *Index) chunkHit(ctx context.Context, chunkID int64) (Hit, bool, error) {
	var h Hit
	err := idx.db.QueryRowContext(ctx,
		"SELECT chunk_id, doc_id, source, span_start, span_end, text FROM doc_chunk WHERE chunk_id = ?", chunkID).
		Scan(&h.ChunkID, &h.DocID, &h.Source, &h.Span[0], &h.Span[1], &h.Text)
	if errors.Is(err, sql.ErrNoRows) {
		return Hit{}, false, nil
	}
	if err != nil {
		return Hit{}, false, err
	}
	return h, true, nil
}

// hybridEnabled 判定向量通道是否就绪：Embedder 非 nil 且 EmbedDim>0 同时满足才走 hybrid。
// 缺任一条件即纯词法（ADR-0017 决策 3），此时 doc_vec 表根本不存在，
// 门禁必须在发起向量查询之前——而不是靠 SQL 报错兜底。
func (idx *Index) hybridEnabled() bool {
	return idx.opts.Embedder != nil && idx.opts.EmbedDim > 0
}
