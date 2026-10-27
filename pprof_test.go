package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPprofMuxServesDiagnostics 验证诊断端点确实挂在独立 mux 上（ADR-0021 决策 4）。
func TestPprofMuxServesDiagnostics(t *testing.T) {
	mux := pprofMux()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("index status = %d, want 200", rec.Code)
	}
	// index 页列出所有可用 profile：出现 goroutine 即证明标准 handler 已注册。
	if !strings.Contains(rec.Body.String(), "goroutine") {
		t.Errorf("index body missing goroutine profile; got:\n%s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/cmdline", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("cmdline status = %d, want 200", rec.Code)
	}
}

// TestPprofMuxRejectsUnknownPath 证明它是独立 mux，不继承主 mux 的路由。
func TestPprofMuxRejectsUnknownPath(t *testing.T) {
	rec := httptest.NewRecorder()
	pprofMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/run", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404（诊断 mux 不得暴露业务路由）", rec.Code)
	}
}

// TestIsLoopbackAddr 决定启动时是否打「暴露面」告警，必须判得准。
func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:6060", true},
		{"[::1]:6060", true},
		{"localhost:6060", true},
		{":6060", false},
		{"0.0.0.0:6060", false},
		{"192.168.1.10:6060", false},
		{"6060", false},
	}
	for _, tc := range cases {
		if got := isLoopbackAddr(tc.addr); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// TestStartPprofDisabledByDefault 默认关闭：空地址不启动任何监听。
func TestStartPprofDisabledByDefault(t *testing.T) {
	if srv := startPprof(""); srv != nil {
		_ = srv.Close()
		t.Fatal("startPprof(\"\") must return nil（默认不启用诊断端口）")
	}
}
