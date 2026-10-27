package main

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// 本文件承载诊断端口（ADR-0021 决策 4）：默认关闭，开启时独立端口、独立 mux。

// pprofMux 返回只挂诊断端点的独立 mux。
//
// 不挂进 api.Handler.Routes() 的主 mux：主 mux 的 "/" 是公开 Demo 页，
// profile 端点混进去等于把 goroutine 快照与调用栈暴露给公网；而且 pprof 的
// index 页会枚举全部 profile，只给入口加鉴权保护不了内容。
// 隔离端口 + 默认关闭是更小的暴露面。
func pprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// startPprof 在 addr 上启动诊断服务；addr 为空表示不启用，返回 nil。
// 返回的 server 由调用方负责 Close（停机时关闭）。
func startPprof(addr string) *http.Server {
	if addr == "" {
		return nil
	}
	if !isLoopbackAddr(addr) {
		// 容器/内网抓 profile 是合理用法，但必须让运维在日志里看到暴露面。
		slog.Warn("pprof listening on non-loopback address; profiles expose process internals", "addr", addr)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           pprofMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("pprof server error", "addr", addr, "error", err)
		}
	}()
	slog.Info("pprof endpoint enabled", "addr", addr)
	return srv
}

// isLoopbackAddr 判断监听地址是否只绑定回环接口。
// ":6060" 这类空 host 表示绑定全部网卡，因此不算回环。
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
