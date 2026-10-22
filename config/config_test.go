package config

import (
	"os"
	"path/filepath"
	"testing"
)

// isolateEnv 清空可能干扰断言的环境变量，避免宿主环境里已设的 key 让测试失真。
func isolateEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("API_AUTH_KEY", "")
	t.Setenv("TASK_TIMEOUT_SECONDS", "")
}

func writeCfg(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_MissingFileIsNotAnError(t *testing.T) {
	isolateEnv(t)
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("配置文件可选，不应报错：%v", err)
	}
	if cfg != (Config{}) {
		t.Errorf("cfg = %+v, want zero", cfg)
	}
}

func TestLoad_ParsesFileAndStripsQuotes(t *testing.T) {
	isolateEnv(t)
	p := writeCfg(t, "# 注释行\ndeepseek_api_key: sk-file\napi_auth_key: \"s3cret\"\n\n")

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeepSeekAPIKey != "sk-file" {
		t.Errorf("DeepSeekAPIKey = %q, want sk-file", cfg.DeepSeekAPIKey)
	}
	if cfg.APIAuthKey != "s3cret" {
		t.Errorf("APIAuthKey = %q, want s3cret（引号应被剥掉）", cfg.APIAuthKey)
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "sk-env")
	t.Setenv("API_AUTH_KEY", "")
	p := writeCfg(t, "deepseek_api_key: sk-file\n")

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeepSeekAPIKey != "sk-env" {
		t.Errorf("DeepSeekAPIKey = %q, want sk-env（环境变量优先）", cfg.DeepSeekAPIKey)
	}
}

func TestLoad_UnknownKeyRejected(t *testing.T) {
	isolateEnv(t)
	p := writeCfg(t, "nope: 1\n")
	if _, err := Load(p); err == nil {
		t.Error("未知配置项应报错，避免拼错的 key 被静默忽略")
	}
}

func TestLoad_MalformedLineRejected(t *testing.T) {
	isolateEnv(t)
	p := writeCfg(t, "deepseek_api_key sk-file\n")
	if _, err := Load(p); err == nil {
		t.Error("缺少冒号的行应报错")
	}
}

func TestLoad_DbPathAndTimeoutParsed(t *testing.T) {
	isolateEnv(t)
	t.Setenv("DB_PATH", "")
	p := writeCfg(t, "db_path: agent-runtime.db\ntask_timeout_seconds: 8\n")

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DbPath != "agent-runtime.db" {
		t.Errorf("DbPath = %q, want agent-runtime.db", cfg.DbPath)
	}
	if cfg.TaskTimeoutSeconds != 8 {
		t.Errorf("TaskTimeoutSeconds = %d, want 8", cfg.TaskTimeoutSeconds)
	}

	// DB_PATH 环境变量应覆盖文件中的值
	t.Setenv("DB_PATH", "from-env.db")
	cfg, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DbPath != "from-env.db" {
		t.Errorf("DbPath = %q, want from-env.db（环境变量优先）", cfg.DbPath)
	}
}

func TestLoad_NonIntTimeoutRejected(t *testing.T) {
	isolateEnv(t)
	p := writeCfg(t, "task_timeout_seconds: notanint\n")
	if _, err := Load(p); err == nil {
		t.Error("非整数 task_timeout_seconds 应报错")
	}
}

func TestLoad_EnvOverridesTaskTimeout(t *testing.T) {
	isolateEnv(t)
	p := writeCfg(t, "task_timeout_seconds: 10\n")
	t.Setenv("TASK_TIMEOUT_SECONDS", "45")

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TaskTimeoutSeconds != 45 {
		t.Errorf("TaskTimeoutSeconds = %d, want 45", cfg.TaskTimeoutSeconds)
	}
}

func TestLoad_InvalidEnvTimeoutRejected(t *testing.T) {
	isolateEnv(t)
	t.Setenv("TASK_TIMEOUT_SECONDS", "invalid")
	if _, err := Load(""); err == nil {
		t.Error("invalid TASK_TIMEOUT_SECONDS should return error")
	}
}
