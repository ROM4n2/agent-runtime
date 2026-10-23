package retrieval

import "strings"

// 分块窗口参数（rune 计）。可调：窗口大小与相邻窗口重叠量。
// ChunkSize 为单块最大字符数；ChunkOverlap 为相邻块重叠字符数
// （保留跨块边界的上下文，利于引用回链与召回）。
const (
	ChunkSize    = 512
	ChunkOverlap = 64
)

// Chunk 是单个分块产物。span 记录在原文中的字符偏移（rune 索引，含头不含尾），
// 供上层引用硬契约（doc_id + span）回链到原文（ADR-0017 决策 4）。
type Chunk struct {
	SpanStart int    // 原文 rune 偏移，含
	SpanEnd   int    // 原文 rune 偏移，不含
	Text      string // 原始正文片段
}

// chunkText 按 rune 窗口滑窗切分：窗口 ChunkSize，步长 ChunkSize-ChunkOverlap，
// 相邻分块重叠 ChunkOverlap 个 rune。span 为原文 rune 偏移（含头不含尾）。
func chunkText(text string) []Chunk {
	runes := []rune(text)
	n := len(runes)
	if n == 0 {
		return nil
	}
	step := ChunkSize - ChunkOverlap // 常量保证 > 0，循环必前进
	var chunks []Chunk
	for start := 0; start < n; start += step {
		end := start + ChunkSize
		if end > n {
			end = n
		}
		chunks = append(chunks, Chunk{
			SpanStart: start,
			SpanEnd:   end,
			Text:      string(runes[start:end]),
		})
		if end == n {
			break
		}
	}
	return chunks
}

// isCJK 判断是否为 CJK 统一表意文字基本区（U+4E00–U+9FFF，常用汉字）。
// 扩展区/日文假名等不在范围内，按非 CJK 原样透传。
func isCJK(r rune) bool {
	return r >= 0x4E00 && r <= 0x9FFF
}

// bigramSplit 把连续 CJK 段落切成重叠 bigram（2 字窗口、步长 1）以空格连接；
// 非 CJK 字符（英文/数字/空白/标点）原样保留，充当天然分隔。
// 例："检索系统" → "检索 索系 系统"。
//
// [ADR-0017 决策 3 的实证修正] ADR 原意在插入 FTS5 前预切 bigram，以规避 trigram
// 分词器对 2 字查询零命中。但 modernc.org/sqlite v1.58.0 的 trigram 分词器对
// bigram 预切文本（2 字 run 以空格分隔）产生空索引——每个 2 字 run 不足以构成一个
// trigram，导致该段完全不可检索（见 index_test.go TestFTS_BigramSplitProducesEmptyIndex）。
// 故 Rebuild 实际插入原文（3+ 字子串查询可命中），本函数保留供查询侧或 ADR 修订后启用，
// 当前未接入生产路径。
func bigramSplit(text string) string {
	runes := []rune(text)
	var b strings.Builder
	i := 0
	for i < len(runes) {
		if !isCJK(runes[i]) {
			b.WriteRune(runes[i])
			i++
			continue
		}
		// CJK 连续段 [i, j)
		j := i
		for j < len(runes) && isCJK(runes[j]) {
			j++
		}
		// 段长 >= 2：输出重叠 bigram；段长 == 1：无 bigram 可切，原样输出单字。
		for k := i; k+1 < j; k++ {
			if k > i {
				b.WriteByte(' ')
			}
			b.WriteRune(runes[k])
			b.WriteRune(runes[k+1])
		}
		if j-i == 1 {
			b.WriteRune(runes[i])
		}
		i = j
	}
	return b.String()
}
