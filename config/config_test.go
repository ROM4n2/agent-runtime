package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateEnv 清空可能干扰断言的环境变量，避免宿主环境里已设的 key 让测试失真。
func isolateEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("API_AUTH_KEY", "")
	t.Setenv("TASK_TIMEOUT_SECONDS", "")
	t.Setenv("EMBED_BASE_URL", "")
	t.Setenv("EMBED_API_KEY", "")
	t.Setenv("EMBED_MODEL", "")
	t.Setenv("EMBED_DIM", "")
	t.Setenv("MODEL", "")
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

func TestLoad_EmbedKeysParsedFromFile(t *testing.T) {
	isolateEnv(t)
	p := writeCfg(t, "embed_base_url: https://file.example.com/v1\nembed_api_key: sk-file-embed\nembed_model: file-embed-model\nembed_dim: 768\n")

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EmbedBaseURL != "https://file.example.com/v1" {
		t.Errorf("EmbedBaseURL = %q, want https://file.example.com/v1", cfg.EmbedBaseURL)
	}
	if cfg.EmbedAPIKey != "sk-file-embed" {
		t.Errorf("EmbedAPIKey = %q, want sk-file-embed", cfg.EmbedAPIKey)
	}
	if cfg.EmbedModel != "file-embed-model" {
		t.Errorf("EmbedModel = %q, want file-embed-model", cfg.EmbedModel)
	}
	if cfg.EmbedDim != 768 {
		t.Errorf("EmbedDim = %d, want 768", cfg.EmbedDim)
	}
}

func TestLoad_EmbedEnvPriority(t *testing.T) {
	isolateEnv(t)
	t.Setenv("EMBED_BASE_URL", "https://env.example.com/v1")
	t.Setenv("EMBED_API_KEY", "sk-env-embed")
	t.Setenv("EMBED_MODEL", "env-embed-model")
	t.Setenv("EMBED_DIM", "1024")
	p := writeCfg(t, "embed_base_url: https://file.example.com/v1\nembed_api_key: sk-file-embed\nembed_model: file-embed-model\nembed_dim: 768\n")

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EmbedBaseURL != "https://env.example.com/v1" {
		t.Errorf("EmbedBaseURL = %q, want https://env.example.com/v1（环境变量优先）", cfg.EmbedBaseURL)
	}
	if cfg.EmbedAPIKey != "sk-env-embed" {
		t.Errorf("EmbedAPIKey = %q, want sk-env-embed（环境变量优先）", cfg.EmbedAPIKey)
	}
	if cfg.EmbedModel != "env-embed-model" {
		t.Errorf("EmbedModel = %q, want env-embed-model（环境变量优先）", cfg.EmbedModel)
	}
	if cfg.EmbedDim != 1024 {
		t.Errorf("EmbedDim = %d, want 1024（环境变量优先）", cfg.EmbedDim)
	}
}

func TestLoad_EmbedDimInvalid(t *testing.T) {
	isolateEnv(t)
	for _, v := range []string{"abc", "-1"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("EMBED_DIM", v)
			if _, err := Load(""); err == nil {
				t.Errorf("EMBED_DIM=%s 应返回 error", v)
			}
		})
	}
}

func TestLoad_EmbedDimZero(t *testing.T) {
	isolateEnv(t)
	t.Setenv("EMBED_DIM", "0")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("EMBED_DIM=0 合法（0 = 不建 vec0 表），不应报错：%v", err)
	}
	if cfg.EmbedDim != 0 {
		t.Errorf("EmbedDim = %d, want 0", cfg.EmbedDim)
	}
}

// TestLoad_EmbedDimFromFileValidation 覆盖 yaml 路径的 embed_dim 校验：
// 负值必须启动失败（Task 2 Produces：EMBED_DIM 正整数校验），非整数同样拒绝，0 合法。
func TestLoad_EmbedDimFromFileValidation(t *testing.T) {
	isolateEnv(t)
	t.Run("负值应报错", func(t *testing.T) {
		p := writeCfg(t, "embed_dim: -1")
		if _, err := Load(p); err == nil {
			t.Error("embed_dim: -1 应返回 error（负值启动失败）")
		}
	})
	t.Run("非整数应报错", func(t *testing.T) {
		p := writeCfg(t, "embed_dim: abc")
		if _, err := Load(p); err == nil {
			t.Error("embed_dim: abc 应返回 error")
		}
	})
	t.Run("零合法", func(t *testing.T) {
		p := writeCfg(t, "embed_dim: 0")
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("embed_dim: 0 合法（0 = 不建 vec0 表），不应报错：%v", err)
		}
		if cfg.EmbedDim != 0 {
			t.Errorf("EmbedDim = %d, want 0", cfg.EmbedDim)
		}
	})
}

func TestLoad_UnknownKeyErrorListsAllKeys(t *testing.T) {
	isolateEnv(t)
	p := writeCfg(t, "nope: 1\n")
	_, err := Load(p)
	if err == nil {
		t.Fatal("未知配置项应报错")
	}
	msg := err.Error()
	for _, key := range []string{
		"deepseek_api_key", "api_auth_key", "db_path", "task_timeout_seconds",
		"embed_base_url", "embed_api_key", "embed_model", "embed_dim", "model",
	} {
		if !strings.Contains(msg, key) {
			t.Errorf("错误文案应列举合法 key %q：%s", key, msg)
		}
	}
}

func TestLoad_ModelParsedFromFile(t *testing.T) {
	isolateEnv(t)
	p := writeCfg(t, "model: deepseek-chat\n")

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "deepseek-chat" {
		t.Errorf("Model = %q, want deepseek-chat", cfg.Model)
	}
}

func TestLoad_EnvOverridesModel(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MODEL", "custom-model-from-env")
	p := writeCfg(t, "model: default-model-from-file\n")

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "custom-model-from-env" {
		t.Errorf("Model = %q, want custom-model-from-env (环境变量优先)", cfg.Model)
	}
}
