package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"agent-runtime/agent"
	"agent-runtime/agent/tool"
)

// toolOutputCollector 收集本次运行里 search_docs 的成功返回全文（缺陷 G 的修复点）。
//
// trace 的 tool_result detail 被 agent.MaxTraceDetail=512 截断——那是有界运行时
// 观测的正确设计，不改；但评测是离线分析，判定必须拿工具返回原文，
// 否则长 text 后面的引用被静默丢掉（实测 q009 完整 recall 0.640 → 0.091）。
type toolOutputCollector struct {
	mu      sync.Mutex
	outputs []string
}

// record 追加一条成功返回串。任务串行跑，但 Run 的调用方将来可能并发，
// 加锁的代价远低于「某天并发跑就数据竞争」的风险。
func (c *toolOutputCollector) record(out string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outputs = append(c.outputs, out)
}

// hits 把已记录的返回串解析成引用集合（多轮调用的结果合并计分）。
// ok=false 表示本次运行没有任何成功返回（工具未调用 / 全部失败），
// 调用方须退回 trace 路径——那是兜底，不是另一套判定。
func (c *toolOutputCollector) hits() ([]toolHit, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.outputs) == 0 {
		return nil, false
	}
	hits := make([]toolHit, 0, len(c.outputs))
	for _, out := range c.outputs {
		hits = append(hits, parseHits(out)...)
	}
	return hits, true
}

// collectingTool 是 tool.Tool 的 decorator：Spec/转发原样，只在 Execute 成功时
// 把完整返回串记进 collector。失败路径不记：错误串里没有可判定的引用。
// 内嵌 tool.Tool 让 Spec() 直接复用被包装工具的实现，避免两处描述漂移。
type collectingTool struct {
	tool.Tool
	sink *toolOutputCollector
}

func (t *collectingTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	out, err := t.Tool.Execute(ctx, args)
	if err != nil {
		return out, err
	}
	t.sink.record(out)
	return out, nil
}

// registryWithCollector 返回本次运行专用的注册表副本：search_docs 换成
// collectingTool，其余工具原样复用。注册表本身在 Run 里只建一次
// （带索引重建，代价高），故按运行复制而不是按运行重建工具面——
// 工具面对模型必须与组设定完全一致，副本只换执行包装、不换 Spec。
func registryWithCollector(reg *tool.Registry, sink *toolOutputCollector) *tool.Registry {
	out := tool.NewRegistry()
	for _, spec := range reg.Specs() {
		name := spec.Function.Name
		t, ok := reg.Get(name)
		if !ok {
			continue
		}
		if name == searchToolName {
			t = &collectingTool{Tool: t, sink: sink}
		}
		out.Register(t)
	}
	return out
}

// agentMaxStepsDetail 是 agent.Run 步数耗尽时写入 trace 的 KindError detail
// （engine.go:139）。步数耗尽是业务结论（模型绕圈）而非 harness 故障：
// ADR-0018 决策 2.3 明令禁止把它当失败根因，故这类错误照常进判定、
// 不中断整轮评测；其余 agent.Run 错误（上游 LLM 故障 / ctx 取消）会污染
// 整组指标，出半成品报表比没有报表更坏，一律上浮为 harness 故障。
const agentMaxStepsDetail = "max steps exceeded"

// traceHasMaxSteps 判断 trace 是否以步数耗尽收尾（engine.go:139 的 KindError）。
func traceHasMaxSteps(trace []agent.TraceStep) bool {
	for _, s := range trace {
		if s.Kind == agent.KindError && s.Detail == agentMaxStepsDetail {
			return true
		}
	}
	return false
}

// countTokens 估算单侧 token 数。nil（未接入）记 0：口径由报表显式标注，
// 不静默填一个看起来正常的数字。
func countTokens(fn func(prompt, completion string) int, prompt, completion string) int {
	if fn == nil {
		return 0
	}
	return fn(prompt, completion)
}

// tokenCountingState 把「计数函数在不在」翻译成报表用的口径标注。
func tokenCountingState(fn func(prompt, completion string) int) string {
	if fn == nil {
		return "not-wired"
	}
	return "injected"
}

// checkCorpusDir 校验语料目录存在且是目录。
func checkCorpusDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("eval: corpus dir %q: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("eval: corpus path %q is not a directory", dir)
	}
	return nil
}

// fileSHA256 对任务集文件算内容哈希：同 hash = 同题，是复跑与审计的对账锚。
func fileSHA256(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("eval: hash task set %s: %w", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// dbPathOrDefault 空 DBPath 落 ":memory:"（与 store 的内存语义一致）：
// 一次性索引跑完即弃，不留文件。
func dbPathOrDefault(p string) string {
	if p == "" {
		return ":memory:"
	}
	return p
}
