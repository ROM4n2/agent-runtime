package retrieval

import (
	"os"
	"strings"
	"testing"
)

// bannedImports 是 retrieval 包禁止出现的上层依赖 import 串（ADR-0013 依赖单向）：
// retrieval 只许依赖 llm 与 agent/tool（Task 7 才引入），
// agent 认知内核、worker 产品层、api 接入层均在上层，禁任何反向 import。
// 用带引号的精确串匹配：既命中真实 import 行，也不放过注释里的假 import，
// 且不会误伤 "agent-runtime/agent/tool" 这类合法的下层子包（后缀不同）。
var bannedImports = []string{
	`"agent-runtime/agent"`,
	`"agent-runtime/worker"`,
	`"agent-runtime/api"`,
}

// TestArchitectureBarrier_RetrievalDoesNotImportUpperLayers 是自动化架构守护测试，
// 镜像 api/architecture_barrier_test.go 的 ReadFile+Contains 模式：
// 扫描 retrieval 目录全部 *.go（生产码与测试码一并门禁），
// 出现上层包 import 串即转红。唯一豁免是本文件自身——
// 其断言文本必然包含被禁串，不豁免将自我误报。
func TestArchitectureBarrier_RetrievalDoesNotImportUpperLayers(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read retrieval dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if e.Name() == "architecture_test.go" {
			continue // 自引用豁免
		}
		content, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("failed to read %s: %v", e.Name(), err)
		}
		text := string(content)
		for _, banned := range bannedImports {
			if strings.Contains(text, banned) {
				t.Errorf("architecture violation: %s must NOT import %s", e.Name(), banned)
			}
		}
	}
}
