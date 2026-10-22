//go:build windows

package retrieval

import (
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"
)

// lockFile 以独占共享模式（shareMode=0）打开普通文件并保持句柄，
// 使随后的 os.ReadFile 失败（"正被另一进程使用"）——Windows 上 chmod 无法制造不可读文件，
// 故用内核句柄独占模拟「无读权限文件」。返回关闭句柄的恢复函数。
func lockFile(t *testing.T, path string) func() {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs %s: %v", path, err)
	}
	p, err := syscall.UTF16PtrFromString(abs)
	if err != nil {
		t.Fatalf("utf16ptr %s: %v", path, err)
	}
	const (
		genericRead  = 0x80000000
		openExisting = 3
	)
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	createFileW := kernel32.NewProc("CreateFileW")
	handle, _, callErr := createFileW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(genericRead),
		uintptr(0), // shareMode=0：拒绝所有共享访问
		uintptr(0),
		uintptr(openExisting),
		uintptr(0),
		uintptr(0),
	)
	if handle == 0 || handle == ^uintptr(0) {
		t.Fatalf("CreateFileW %s: %v", path, callErr)
	}
	h := syscall.Handle(handle)
	return func() { _ = syscall.CloseHandle(h) }
}
