package prompts

import (
	"embed"
	"fmt"
)

// DefaultVersion 默认生产系统提示词版本。
const DefaultVersion = "default-v1"

// EvalV1 评测 baseline 版本（空系统提示词）。
const EvalV1 = "eval-template-v1"

// EvalV2 评测带 Grounded Citation 铁律的系统提示词版本。
const EvalV2 = "eval-template-v2"

//go:embed *.md
var promptFS embed.FS

// Get 获取指定版本的 Prompt 模板内容。
// 若 version 为空，返回错误；
// 若指定文件不存在，返回未知版本错误；
// 否则返回文件内容字符串。
func Get(version string) (string, error) {
	if version == "" {
		return "", fmt.Errorf("prompts: empty template version")
	}
	data, err := promptFS.ReadFile(version + ".md")
	if err != nil {
		return "", fmt.Errorf("prompts: unknown template version %q", version)
	}
	return string(data), nil
}
