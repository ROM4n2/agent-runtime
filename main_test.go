package main

import (
	"agent-api/store"
	"agent-api/worker"
	"net/http"
	"testing"
	"time"
)

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

func TestShutdown_Success(t *testing.T) {
	s := store.NewStore()
	p := worker.NewPool(1, time.Second, s, nil)
	p.Start()
	srv := &http.Server{}

	exitCode := shutdown(srv, p)
	if exitCode != 0 {
		t.Errorf("shutdown returned exitCode = %d, want 0", exitCode)
	}
}
