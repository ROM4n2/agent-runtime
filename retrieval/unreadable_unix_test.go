//go:build !windows

package retrieval

import (
	"os"
	"testing"
)

// lockFile 通过移除读权限（chmod 0000）使普通文件不可读（Unix）。
// 返回恢复读权限的函数。
func lockFile(t *testing.T, path string) func() {
	t.Helper()
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod 000 %s: %v", path, err)
	}
	return func() { _ = os.Chmod(path, 0o644) }
}
