package api

import (
	"os"
	"strings"
	"testing"
)

// TestArchitectureBarrier_APIDoesNotImportWorker 是一项自动化架构守护测试（ADR-0013 门禁）。
// 确保 api 层的任何生产代码绝不反向依赖 worker 包，维持清晰的单向依赖与依赖倒置（DIP）。
func TestArchitectureBarrier_APIDoesNotImportWorker(t *testing.T) {
	paths := []string{"handler.go", "middleware.go"}
	for _, p := range paths {
		content, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("failed to read %s: %v", p, err)
		}
		text := string(content)
		if strings.Contains(text, `"agent-runtime/worker"`) {
			t.Errorf("architecture violation: %s must NOT import \"agent-runtime/worker\"", p)
		}
		if strings.Contains(text, "worker.") {
			t.Errorf("architecture violation: %s must NOT reference worker symbols directly", p)
		}
	}
}
