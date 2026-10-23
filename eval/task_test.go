package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTaskSet 把任务集 JSON 落成临时文件：LoadTasks 的入参就是文件路径，
// 测试必须走「文件解析」这一层，而不是把 JSON 直接喂给内部解析函数——
// 后者会漏掉读文件与语法错误的出口。
func writeTaskSet(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write task set: %v", err)
	}
	return path
}

// TestLoadTasks_ValidSet 固化合法任务集的解析契约：三种 scope 齐备，
// 字段逐一对上号，out-of-corpus 题允许 expect_spans 为空数组
// （ADR-0018：span 空表示期望无引用，但 expect_docs 仍须标注）。
func TestLoadTasks_ValidSet(t *testing.T) {
	const set = `[
  {
    "id": "q001",
    "prompt": "崩溃恢复把僵尸任务标记成什么错误文案？",
    "scope": "in-corpus",
    "expect_docs": ["docs/adr/0015-crash-recovery-and-task-rehydration.md"],
    "expect_spans": [[120, 168]]
  },
  {
    "id": "q002",
    "prompt": "取消语义分层与崩溃恢复标记如何协同？",
    "scope": "cross-chunk",
    "expect_docs": [
      "docs/adr/0014-cancellation-semantics.md",
      "docs/adr/0015-crash-recovery-and-task-rehydration.md"
    ],
    "expect_spans": [[10, 40], [200, 260]]
  },
  {
    "id": "q003",
    "prompt": "2027 年世界杯冠军是哪支队？",
    "scope": "out-of-corpus",
    "expect_docs": ["docs/adr/0015-crash-recovery-and-task-rehydration.md"],
    "expect_spans": []
  }
]`
	tasks, err := LoadTasks(writeTaskSet(t, set))
	if err != nil {
		t.Fatalf("LoadTasks: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d: %+v", len(tasks), tasks)
	}
	if tasks[0].Scope != ScopeInCorpus || tasks[1].Scope != ScopeCrossChunk || tasks[2].Scope != ScopeOutOfCorpus {
		t.Fatalf("scope parse mismatch: %+v", tasks)
	}
	if tasks[0].ID != "q001" || tasks[0].Prompt == "" {
		t.Errorf("task[0] id/prompt mismatch: %+v", tasks[0])
	}
	if len(tasks[0].ExpectSpans) != 1 || tasks[0].ExpectSpans[0] != [2]int{120, 168} {
		t.Errorf("task[0] spans mismatch: %+v", tasks[0].ExpectSpans)
	}
	if len(tasks[1].ExpectDocs) != 2 {
		t.Errorf("task[1] expect_docs mismatch: %+v", tasks[1].ExpectDocs)
	}
	// out-of-corpus：expect_docs 非空（仍标注会被误引的文档），expect_spans 为空。
	if len(tasks[2].ExpectDocs) != 1 || len(tasks[2].ExpectSpans) != 0 {
		t.Errorf("task[2] out-of-corpus shape mismatch: %+v", tasks[2])
	}
}

// TestLoadTasks_RejectsInvalid 覆盖 schema 校验的每一条：任一不合格都必须
// 显式报错而非静默放过——任务集非法 = harness 自身故障（ADR-0018 退出码语义），
// 放进来的非法题会在评测阶段变成无法归因的噪声。
func TestLoadTasks_RejectsInvalid(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		wantSub string // 错误信息必须提及的关键词，防「因错误的理由报错」
	}{
		{
			name:    "scope 不在白名单",
			json:    `[{"id":"q1","prompt":"p","scope":"in_corpus","expect_docs":["d"],"expect_spans":[[1,2]]}]`,
			wantSub: "scope",
		},
		{
			name:    "缺 ID",
			json:    `[{"id":"","prompt":"p","scope":"in-corpus","expect_docs":["d"],"expect_spans":[[1,2]]}]`,
			wantSub: "id",
		},
		{
			name:    "缺 prompt",
			json:    `[{"id":"q1","prompt":"","scope":"in-corpus","expect_docs":["d"],"expect_spans":[[1,2]]}]`,
			wantSub: "prompt",
		},
		{
			name:    "expect_docs 为空",
			json:    `[{"id":"q1","prompt":"p","scope":"in-corpus","expect_docs":[],"expect_spans":[[1,2]]}]`,
			wantSub: "expect_docs",
		},
		{
			name:    "span 倒挂 start>=end",
			json:    `[{"id":"q1","prompt":"p","scope":"in-corpus","expect_docs":["d"],"expect_spans":[[20,10]]}]`,
			wantSub: "expect_spans",
		},
		{
			name:    "span 零长 start==end",
			json:    `[{"id":"q1","prompt":"p","scope":"in-corpus","expect_docs":["d"],"expect_spans":[[10,10]]}]`,
			wantSub: "expect_spans",
		},
		{
			name:    "span 负起点",
			json:    `[{"id":"q1","prompt":"p","scope":"in-corpus","expect_docs":["d"],"expect_spans":[[-5,10]]}]`,
			wantSub: "expect_spans",
		},
		{
			name: "JSON 语法错",
			json: `[{"id":"q1","prompt":"p",}]`,
		},
		{
			name: "span 不是二元组",
			json: `[{"id":"q1","prompt":"p","scope":"in-corpus","expect_docs":["d"],"expect_spans":[[10]]}]`,
		},
		{
			name: "任务集为空数组",
			json: `[]`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tasks, err := LoadTasks(writeTaskSet(t, c.json))
			if err == nil {
				t.Fatalf("expected error, got tasks: %+v", tasks)
			}
			if c.wantSub != "" && !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error %q must mention %q", err, c.wantSub)
			}
		})
	}
}

// TestLoadTasks_MissingFile 补文件层出口：路径不存在必须是 error，
// 不能让调用方拿到空任务集当成「零条题跑完」。
func TestLoadTasks_MissingFile(t *testing.T) {
	if _, err := LoadTasks(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing task set file")
	}
}
