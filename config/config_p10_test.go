package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestLoad_SessionHistoryLimitFromFile 会话历史窗口是模型能力参数（ADR-0021 决策 3），
// 必须能只改配置而不用改代码发版。
func TestLoad_SessionHistoryLimitFromFile(t *testing.T) {
	isolateEnv(t)
	p := writeCfg(t, "session_history_limit: 250\n")

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SessionHistoryLimit != 250 {
		t.Errorf("SessionHistoryLimit = %d, want 250", cfg.SessionHistoryLimit)
	}
}

// TestLoad_SessionHistoryLimitEnvPriority 环境变量优先于文件。
func TestLoad_SessionHistoryLimitEnvPriority(t *testing.T) {
	isolateEnv(t)
	p := writeCfg(t, "session_history_limit: 250\n")
	t.Setenv("SESSION_HISTORY_LIMIT", "40")

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SessionHistoryLimit != 40 {
		t.Errorf("SessionHistoryLimit = %d, want 40（环境变量优先）", cfg.SessionHistoryLimit)
	}
}

// TestLoad_SessionHistoryLimitValidation 非法值必须启动失败而不是静默回落到默认：
// 静默回落会让「我配了 0」变成「跑了 100」，排障时极难发现。
func TestLoad_SessionHistoryLimitValidation(t *testing.T) {
	isolateEnv(t)
	if _, err := Load(writeCfg(t, "session_history_limit: 0\n")); err == nil {
		t.Error("session_history_limit: 0 应报错")
	}
	if _, err := Load(writeCfg(t, "session_history_limit: abc\n")); err == nil {
		t.Error("session_history_limit: abc 应报错")
	}

	t.Setenv("SESSION_HISTORY_LIMIT", "-5")
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("SESSION_HISTORY_LIMIT=-5 应报错")
	}
	t.Setenv("SESSION_HISTORY_LIMIT", "abc")
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("SESSION_HISTORY_LIMIT=abc 应报错")
	}
}

// TestLoad_PprofAddr 诊断端口默认关闭（空），文件与环境变量都能开启。
func TestLoad_PprofAddr(t *testing.T) {
	isolateEnv(t)
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PprofAddr != "" {
		t.Errorf("PprofAddr = %q, want 空（默认关闭）", cfg.PprofAddr)
	}

	cfg, err = Load(writeCfg(t, "pprof_addr: 127.0.0.1:6060\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PprofAddr != "127.0.0.1:6060" {
		t.Errorf("PprofAddr = %q, want 127.0.0.1:6060", cfg.PprofAddr)
	}

	t.Setenv("PPROF_ADDR", "localhost:6061")
	cfg, err = Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PprofAddr != "localhost:6061" {
		t.Errorf("PprofAddr = %q, want localhost:6061（环境变量优先）", cfg.PprofAddr)
	}
}

// TestLoad_UnknownKeyErrorMentionsNewKeys 未知项报错文案必须列出新键，
// 否则运维照旧文案排查会漏掉 session_history_limit / pprof_addr。
func TestLoad_UnknownKeyErrorMentionsNewKeys(t *testing.T) {
	isolateEnv(t)
	_, err := Load(writeCfg(t, "nope_key: 1\n"))
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	for _, want := range []string{"session_history_limit", "pprof_addr"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q: %v", want, err)
		}
	}
}
