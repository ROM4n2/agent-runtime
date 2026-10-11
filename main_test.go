package main

import "testing"

// TestNewServerHasTimeouts 校验 newServer 工厂返回的服务器配置：
// 监听地址固定为 :8080，且三类超时均已设置为正值（避免慢客户端无限占用连接）。
func TestNewServerHasTimeouts(t *testing.T) {
	srv := newServer(nil)

	if srv.Addr != ":8080" {
		t.Errorf("Addr = %q, want %q", srv.Addr, ":8080")
	}
	if srv.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %v, want > 0", srv.ReadHeaderTimeout)
	}
	if srv.WriteTimeout <= 0 {
		t.Errorf("WriteTimeout = %v, want > 0", srv.WriteTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout = %v, want > 0", srv.IdleTimeout)
	}
}
