// Package config 加载启动所需的敏感配置。
//
// 优先级：环境变量 > config.yaml > 零值。
// 配置文件可选：文件不存在时按"没配"处理，纯环境变量的用法不受影响。
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config 是启动所需的外部配置。
type Config struct {
	DeepSeekAPIKey     string
	APIAuthKey         string
	DbPath             string // 非空则启用 SQLite 持久化（重启不丢），空则用内存存储
	TaskTimeoutSeconds int    // 单次任务 LLM 调用超时（秒），0 表示用默认 30s
	// 向量通道独立凭证：默认空 = 仅词法检索（lexical only，ADR-0017 决策 3）。
	EmbedBaseURL string
	EmbedAPIKey  string
	EmbedModel   string
	EmbedDim     int // 0 = 不建 vec0 表；>0 才启用混合召回；负值启动失败
}

// defaultPath 是约定俗成的配置文件位置，相对进程工作目录。
const defaultPath = "config.yaml"

// Load 读取配置文件，再用环境变量覆盖同名项。
func Load(path string) (Config, error) {
	if path == "" {
		path = defaultPath
	}
	fileCfg, err := loadFile(path)
	if err != nil {
		return Config{}, err
	}
	timeoutSec := fileCfg.TaskTimeoutSeconds
	if envTimeout := os.Getenv("TASK_TIMEOUT_SECONDS"); envTimeout != "" {
		v, err := strconv.Atoi(envTimeout)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid TASK_TIMEOUT_SECONDS %q: %w", envTimeout, err)
		}
		if v < 0 {
			return Config{}, fmt.Errorf("config: TASK_TIMEOUT_SECONDS cannot be negative: %d", v)
		}
		timeoutSec = v
	}
	embedDim := fileCfg.EmbedDim
	if envDim := os.Getenv("EMBED_DIM"); envDim != "" {
		v, err := strconv.Atoi(envDim)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid EMBED_DIM %q: %w", envDim, err)
		}
		if v < 0 {
			return Config{}, fmt.Errorf("config: EMBED_DIM cannot be negative: %d", v)
		}
		embedDim = v
	}
	return Config{
		DeepSeekAPIKey:     firstNonEmpty(os.Getenv("DEEPSEEK_API_KEY"), fileCfg.DeepSeekAPIKey),
		APIAuthKey:         firstNonEmpty(os.Getenv("API_AUTH_KEY"), fileCfg.APIAuthKey),
		DbPath:             firstNonEmpty(os.Getenv("DB_PATH"), fileCfg.DbPath),
		TaskTimeoutSeconds: timeoutSec,
		EmbedBaseURL:       firstNonEmpty(os.Getenv("EMBED_BASE_URL"), fileCfg.EmbedBaseURL),
		EmbedAPIKey:        firstNonEmpty(os.Getenv("EMBED_API_KEY"), fileCfg.EmbedAPIKey),
		EmbedModel:         firstNonEmpty(os.Getenv("EMBED_MODEL"), fileCfg.EmbedModel),
		EmbedDim:           embedDim,
	}, nil
}

// firstNonEmpty 返回第一个非空值，用于实现"环境变量覆盖文件"。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// loadFile 解析扁平配置文件。
//
// 支持范围只有这一个子集：每行一个 "key: value"，# 开头为注释，值可用引号包裹。
// 超出子集的内容（嵌套、列表、缺冒号的行）一律报错，不静默误解析。
// 配置解析出错比启动失败更难排查，宁可启动失败也要把错误说清楚。
func loadFile(path string) (Config, error) {
	var cfg Config

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 配置文件是可选的，缺文件不是错误
			return cfg, nil
		}
		return cfg, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return cfg, fmt.Errorf("config: %s:%d 不是 key: value 格式：%q", path, lineNo, line)
		}
		key = strings.TrimSpace(key)
		value = unquote(strings.TrimSpace(value))

		switch key {
		case "deepseek_api_key":
			cfg.DeepSeekAPIKey = value
		case "api_auth_key":
			cfg.APIAuthKey = value
		case "db_path":
			cfg.DbPath = value
		case "task_timeout_seconds":
			n, err := strconv.Atoi(value)
			if err != nil {
				return cfg, fmt.Errorf("config: %s:%d task_timeout_seconds 必须是整数：%q", path, lineNo, value)
			}
			cfg.TaskTimeoutSeconds = n
		case "embed_base_url":
			cfg.EmbedBaseURL = value
		case "embed_api_key":
			cfg.EmbedAPIKey = value
		case "embed_model":
			cfg.EmbedModel = value
		case "embed_dim":
			n, err := strconv.Atoi(value)
			if err != nil {
				return cfg, fmt.Errorf("config: %s:%d embed_dim 必须是整数：%q", path, lineNo, value)
			}
			if n < 0 {
				return cfg, fmt.Errorf("config: %s:%d embed_dim 不能为负：%d", path, lineNo, n)
			}
			cfg.EmbedDim = n
		default:
			return cfg, fmt.Errorf("config: %s:%d 未知配置项 %q（只支持 deepseek_api_key、api_auth_key、db_path、task_timeout_seconds、embed_base_url、embed_api_key、embed_model、embed_dim）", path, lineNo, key)
		}
	}
	if err := sc.Err(); err != nil {
		return cfg, fmt.Errorf("config: read %s: %w", path, err)
	}
	return cfg, nil
}

// unquote 剥掉成对的首尾引号，让值里可以安全的以空格或 # 开头。
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
