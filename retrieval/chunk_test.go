package retrieval

import (
	"strings"
	"testing"
)

// makeRunes 生成 n 个 rune 的字符串（用可复用模式，避免构造巨型字面量）。
func makeRunes(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteRune(rune('文' + i%1000))
	}
	return b.String()
}

// TestChunkText_Constants 固定可调参数的缺省值（rune 计）。
func TestChunkText_Constants(t *testing.T) {
	if ChunkSize != 512 {
		t.Errorf("ChunkSize = %d, want 512", ChunkSize)
	}
	if ChunkOverlap != 64 {
		t.Errorf("ChunkOverlap = %d, want 64", ChunkOverlap)
	}
	if ChunkSize <= ChunkOverlap {
		t.Errorf("invariant broken: ChunkSize(%d) must exceed ChunkOverlap(%d)", ChunkSize, ChunkOverlap)
	}
}

// TestChunkText_SingleChunk 验证短于窗口的文本只产一个分块，span 覆盖全文。
func TestChunkText_SingleChunk(t *testing.T) {
	text := "短文本"
	chunks := chunkText(text)
	if len(chunks) != 1 {
		t.Fatalf("len(chunks) = %d, want 1", len(chunks))
	}
	c := chunks[0]
	if c.SpanStart != 0 || c.SpanEnd != len([]rune(text)) {
		t.Errorf("span = [%d,%d], want [0,%d]", c.SpanStart, c.SpanEnd, len([]rune(text)))
	}
	if c.Text != text {
		t.Errorf("Text = %q, want %q", c.Text, text)
	}
}

// TestChunkText_Empty 验证空文本不产分块。
func TestChunkText_Empty(t *testing.T) {
	if chunks := chunkText(""); chunks != nil {
		t.Errorf("chunkText(\"\") = %v, want nil", chunks)
	}
}

// TestChunkText_OverlapAndSpans 验证多分块场景：
//   - 相邻分块重叠恰为 ChunkOverlap 个 rune；
//   - 首块从 0 起、末块到文末；
//   - 每个分块窗口不超过 ChunkSize；
//   - Text 恒等于原文 [SpanStart:SpanEnd] 的子串（rune 偏移正确）。
func TestChunkText_OverlapAndSpans(t *testing.T) {
	const n = 1500 // > ChunkSize，必产多分块
	runes := []rune(makeRunes(n))
	text := string(runes)

	chunks := chunkText(text)
	if len(chunks) < 2 {
		t.Fatalf("len(chunks) = %d, want >= 2", len(chunks))
	}

	if chunks[0].SpanStart != 0 {
		t.Errorf("first SpanStart = %d, want 0", chunks[0].SpanStart)
	}
	if last := chunks[len(chunks)-1]; last.SpanEnd != n {
		t.Errorf("last SpanEnd = %d, want %d", last.SpanEnd, n)
	}

	for i, c := range chunks {
		if c.SpanEnd-c.SpanStart > ChunkSize {
			t.Errorf("chunk %d window = %d, exceeds ChunkSize %d", i, c.SpanEnd-c.SpanStart, ChunkSize)
		}
		if c.SpanStart < 0 || c.SpanEnd > n || c.SpanStart >= c.SpanEnd {
			t.Errorf("chunk %d invalid span [%d,%d]", i, c.SpanStart, c.SpanEnd)
		}
		// span 偏移正确性：Text 必须是原文对应 rune 切片
		if want := string(runes[c.SpanStart:c.SpanEnd]); c.Text != want {
			t.Errorf("chunk %d Text mismatch with runes[%d:%d]", i, c.SpanStart, c.SpanEnd)
		}
		// 相邻重叠恒为 ChunkOverlap（滑窗步长 = ChunkSize-ChunkOverlap）
		if i+1 < len(chunks) {
			next := chunks[i+1]
			if overlap := c.SpanEnd - next.SpanStart; overlap != ChunkOverlap {
				t.Errorf("overlap between chunk %d and %d = %d, want %d", i, i+1, overlap, ChunkOverlap)
			}
		}
	}
}

// TestChunkText_ExactBoundary 验证文本长度恰为 ChunkSize 时只产一个分块。
func TestChunkText_ExactBoundary(t *testing.T) {
	text := makeRunes(ChunkSize)
	chunks := chunkText(text)
	if len(chunks) != 1 {
		t.Fatalf("len(chunks) = %d, want 1", len(chunks))
	}
	if chunks[0].SpanEnd != ChunkSize {
		t.Errorf("SpanEnd = %d, want %d", chunks[0].SpanEnd, ChunkSize)
	}
}

// TestBigramSplit_Chinese 验证中文连续段被切成空格分隔的重叠 bigram。
func TestBigramSplit_Chinese(t *testing.T) {
	if got := bigramSplit("检索系统"); got != "检索 索系 系统" {
		t.Errorf("bigramSplit(检索系统) = %q, want %q", got, "检索 索系 系统")
	}
	if got := bigramSplit("中文测试"); got != "中文 文测 测试" {
		t.Errorf("bigramSplit(中文测试) = %q, want %q", got, "中文 文测 测试")
	}
}

// TestBigramSplit_NoCJK 验证纯非中文文本原样透传（无 bigram 化）。
func TestBigramSplit_NoCJK(t *testing.T) {
	if got := bigramSplit("hello world"); got != "hello world" {
		t.Errorf("bigramSplit(hello world) = %q, want unchanged", got)
	}
}

// TestBigramSplit_Mixed 验证中英混排：非 CJK 透传、CJK 段内部 bigram 化。
func TestBigramSplit_Mixed(t *testing.T) {
	if got := bigramSplit("a世界b"); got != "a世界b" {
		t.Errorf("bigramSplit(a世界b) = %q, want %q", got, "a世界b")
	}
}

// TestBigramSplit_SingleCJK 验证孤立单字 CJK（无 bigram 可切）原样输出。
func TestBigramSplit_SingleCJK(t *testing.T) {
	if got := bigramSplit("中"); got != "中" {
		t.Errorf("bigramSplit(中) = %q, want 中", got)
	}
}
