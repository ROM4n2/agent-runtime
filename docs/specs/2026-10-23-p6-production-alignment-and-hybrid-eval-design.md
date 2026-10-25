# Design Specification: P6 Production Alignment, Structural Governance & Hybrid Retrieval Evaluation

**Date**: 2026-10-23  
**Status**: DRAFT  
**Author**: Haoyu & Hermes Agent  
**Target Path**: `docs/specs/2026-10-23-p6-production-alignment-and-hybrid-eval-design.md`

---

## 1. Problem Statement & User Value

- **Why build this**:
  1. **评测与契约闭环缺口（ADR-0017 / ADR-0018 Deferred Gaps）**：P5 完成了词法检索（`lexical`）的接入与评测，但暴露出 3 处生产级契约未闭环：① `agent.Run` 尚未注入版本化 System Prompt（无 `prompts/` 模板加载），导致 `out-of-corpus` 语料外问题仅 `1/7` 通过（模型强行引用无关片段而不拒答），`cross-chunk` 跨分块问题仅 `4/10` 通过（`right-span-wrong-reasoning × 16`，模型搜到了正确区间但回答中未显式标注 `doc_id`）；② `llm.Client` 未透传 `temperature` 与 `seed`，也未解析返回 `usage`，导致评测报表中 `token 成本` 恒为 `0 + 0 (not-wired)`；③ `llm.Client` 的 `embeddingsPath` 硬拼接 `/v1/embeddings` 且强制要求非空 `EmbedAPIKey`，阻塞了本地免密 Ollama（`nomic-embed-text:latest`）运行 `hybrid` 混合检索组。
  2. **多版本演进后的结构熵增与耦合漂移（Structural Entropy）**：经历 P0–P5 六个版本迭代后，代码库出现三处典型架构退化：① `worker/pool.go`（并发调度器）为了映射 `"index missing"` 终态，在 `pool.go:7,233` 直接 `import "agent-runtime/retrieval"`，使通用并发池反向耦合了具体工具实现；② `main.go`（410 行）混杂了守护进程装配、HTTP 优雅停机、`-index` 与 `-eval` CLI 子命令实现；③ 架构门禁分散且脆弱（`api/architecture_barrier_test.go` 仅硬编码检查 2 个文件名，且缺乏单文件物理行数上限门禁）。
  3. **对外门面与面试资产断代**：`README.md` 与 `docs/interview/` 仍停留在 P1（25 个测试、6 个包、内存存储），与当前 11 包 / 238 测试 / FTS5+vec0 混合检索的真实工程底座严重脱节。
- **Target Scenario & Persona**:
  - 平台/AI 基础设施工程师及面试评审人：通过阅读 `README.md`、架构门禁测试、ADR 与 `docs/eval/` 对照报表，在 3 分钟内验证系统的分层纯净度、并发韧性与检索评测真值。
- **Cost of Doing Nothing**:
  - 若不治理耦合与文件膨胀，`worker` 未来每加一个工具都要引一个领域包，最终退化为网状大泥球；若不闭合 System Prompt、Token 计量与 Ollama `hybrid` 评测，P5 评测报表中的 `not-wired` 与 `1/7` 拒答率将成为技术深挖时的明显短板。

---

## 2. User Journey & Core Flow

- **Happy Path (<= 3 steps to value)**:
  1. **Step 1（架构与门禁自检）**：开发者运行 `GOWORK=off go test -race ./... -count=1` → 根级 `architecture_test.go` 自动校验全仓 10 包 DAG 依赖矩阵（验证 `worker` 零 `retrieval` 依赖）与单文件行数预算（生产文件 `<= 400` 行），全部绿灯通过。
  2. **Step 2（三组受控实证评测）**：开发者启动本地 Ollama（`nomic-embed-text:latest`，768 维），运行 `go run . -eval -group lexical -template eval-template-v2` 与 `-group hybrid` → 系统注入版本化 System Prompt、固定 `seed`/`temperature=0`、跨步累加真实 Token Usage，在 `docs/eval/` 下生成包含真实 Token 成本与高拒答率/引用率的 Markdown + JSON 对照报表。
  3. **Step 3（作品审阅与面试检阅）**：评审人浏览 `README.md`、`docs/adr/0017..0018` 与 `docs/interview/` → 全部架构图、指标数据、命令示例与最新代码 100% 物理对齐。
- **Friction Points & Mitigation**:
  - **痛点**：用户在配置 `embed_base_url` 时既可能写 `http://localhost:11434` 也可能写 `http://localhost:11434/v1`，且本地 Ollama 无需 API Key。
  - **消解**：`llm.Client` 内置 `joinEndpoint` 幂等归一化 `/v1` 前缀；`EmbedAPIKey` 允许为空（非空时才发 `Authorization` 头），激活判据收敛为 `EmbedBaseURL != "" && EmbedModel != "" && EmbedDim > 0`。
- **Error Feedback & Recovery**:
  - 当未知 `TemplateVersion` 传入时，`prompts.Load(version)` 立即返回显式错误，拒绝静默退回空提示词；
  - 当工具返回实现了 `tool.TerminalClassifier` 的错误时，`worker` 通过接口提取脱敏终态名（如 `"index missing"`），无需知晓具体工具包。

---

## 3. Architecture & Data Models

### 3.1 架构防腐与文件瘦身（Structural Governance）

1. **接口下沉斩断 `worker -> retrieval` 耦合（Protocol Lowering）**：
   - 在 `agent/tool/tool.go` 新增可选契约：
     ```go
     // TerminalClassifier 允许工具错误声明其对应的任务级脱敏终态文案（如 "index missing"）。
     // 仅当该错误一直冒泡使 agent.Run 终止时，由上层执行器提取写入 Task.Error。
     type TerminalClassifier interface {
         error
         TerminalStatus() string
     }
     ```
   - `retrieval` 包中的索引缺失哨兵错误实现 `TerminalStatus() string { return "index missing" }`（或经包装类型实现）；
   - `worker/pool.go:233` 改为使用 `var tc tool.TerminalClassifier; errors.As(err, &tc)` 判定终态，**彻底移除 `worker/pool.go` 中对 `"agent-runtime/retrieval"` 的 import**。
2. **文件级职责切分（Cohesive File Splitting）**：
   - **`main` 包按运行模式切分**：`main.go` 仅保留守护进程装配、信号监听与 HTTP 优雅停机（约 240 行）；将 `-index` 与 `-eval` CLI 标志辅助、入口函数拆至同包的 `cli.go`（约 170 行）。
   - **`retrieval` 包按职责切分**：将 `retrieval/search.go`（473 行）中的 `vec0` 向量检索与 `embedTexts` 辅助逻辑拆至 `retrieval/vector_search.go`，保留词法检索与 RRF 融合在 `retrieval/search.go`（各约 240 行）。
   - **`api` 包按协议切分**：将 `api/handler.go`（409 行）中的 SSE 流式处理器 `HandleEvents` 及辅助函数拆至 `api/events.go`，使 `api/handler.go` 降至 300 行以内。
3. **全仓 DAG 依赖矩阵与文件体积机械门禁（`architecture_test.go`）**：
   - 在仓库根目录新增 `architecture_test.go`（`package main`），使用标准库 `go/parser`（`parser.ImportsOnly`）：
     - **DAG 白名单断言**：扫描全仓所有生产 `.go` 文件，断言每个包的内部 import 严格属于允许集合（显式锁死 `worker` 只允许依赖 `agent`、`agent/tool`、`store`、`metrics`；`api` 只允许依赖 `agent`、`store`、`metrics`；`retrieval` 只允许依赖 `llm`、`agent/tool`；`agent` 只允许依赖 `llm`、`agent/tool`）。
     - **文件体积门禁**：扫描全仓所有非测试 `.go` 文件（`!strings.HasSuffix(name, "_test.go")`），断言单文件物理行数 `<= 400` 行、测试文件 `<= 800` 行，从根源阻断后续迭代的“温水煮青蛙式膨胀”。

### 3.2 LLM 工业化适配、Prompt 版本化与跨步 Token 计量

1. **`llm.Client` 端点归一化、免密 Embedding 与采样参数/Usage 契约**：
   - **端点归一化 `joinEndpoint(baseURL, suffix string) string`**：剥离 `baseURL` 尾随 `/`；若 `suffix` 以 `/v1/` 开头且 `baseURL` 已以 `/v1` 结尾，自动去重，确保 `https://api.deepseek.com`、`http://localhost:11434` 与 `http://localhost:11434/v1` 行为完全一致。
   - **免密 Embedding 支持**：`Client.Embed` 仅校验 `c.embedBaseURL == "" || c.embedModel == ""`；仅当 `c.embedAPIKey != ""` 时设置 `Authorization: Bearer ...` 请求头。`main.go` 的 `newEmbedder` 同步改为检查 `conf.EmbedBaseURL != "" && conf.EmbedModel != "" && conf.EmbedDim > 0`。
   - **采样确定性与 Usage 返回**：
     - `llm.Config` 新增可选字段 `Temperature *float64` 与 `Seed *int64`，`ChatWithTools` 请求体按需序列化 `"temperature,omitempty"` 与 `"seed,omitempty"`。
     - `llm` 包新增 `type Usage struct { PromptTokens int `json:"prompt_tokens"`; CompletionTokens int `json:"completion_tokens"` }`，`AssistantTurn` 增加 `Usage Usage` 字段，`ChatWithTools` 解析响应顶层 `usage` 对象并回填。
2. **`config.Config` 模型名可配化**：
   - `config.Config` 新增 `Model string`（键名 `model` / 环境变量 `MODEL`，默认值 `"deepseek-chat"`，兼容覆盖）。
3. **版本化 System Prompt 注入（`prompts/` 包）**：
   - 新增轻量包 `prompts/`（`prompts/prompts.go` + `go:embed *.md`），零内部依赖：
     - `eval-template-v1.md`：空系统提示词（保持历史 v1 基线可复现对账）；
     - `eval-template-v2.md`（及默认服务模板 `default_v1.md`）：明确规定工具使用与 **Grounded Citation 铁律**——① 回答事实性问题必须先调用 `search_docs`（中文查询词不少于 3 个字符，跨概念问题可分步检索）；② 最终回答正文中**必须原样写出所依据的 `doc_id` 完整路径**（如 `docs/adr/0015-crash-recovery-and-task-rehydration.md`）；③ 若检索结果与问题无关或语料库中无相关依据，**必须明确声明语料库中未包含该信息，严禁猜测或附会引用任何 `doc_id`**。
   - `agent.Run` 支持可选系统提示词或配置扩展：在 `agent` 包新增 `RunOptions`（含 `SystemPrompt string` 与 `OnUsage func(llm.Usage)`）及 `RunWithOptions(ctx, c, prompt, reg, onStep, opts)`，原 `Run` 函数保持签名完全兼容并委托调用 `RunWithOptions`。当 `opts.SystemPrompt != ""` 时，初始 `turns` 为 `[{Role: "system", Content: opts.SystemPrompt}, {Role: "user", Content: prompt}]`。
   - 每轮 `c.ChatWithTools` 返回后，若 `opts.OnUsage != nil`，调用 `opts.OnUsage(resp.Usage)`，由调用方（如 `eval.runOnce`）跨步累加整道题的真实 `PromptTokens` 与 `CompletionTokens`。

---

## 4. Edge Cases & Resilience

- **Network / Transport Drops**:
  - 本地 Ollama 或远程 LLM 响应超时仍受 `http.Client.Timeout` 与 `taskCtx` 双层控制；Embedding 瞬时网络错误经 `classifySearchError` 包装为 `ErrEmbeddingUnavailable`（`Retryable() = true`），由 `agent.Run` 按 `tc.ID` 退避 200ms 重试一次。
- **Concurrency & Races**:
  - `prompts` 包通过编译期只读 `embed.FS` 读取模板，无可变全局状态，天然并发安全；
  - `eval.runOnce` 中的 Token 累加器为单次运行栈内局部变量，无锁竞争。
- **Error Boundaries**:
  - **向后兼容**：`eval-template-v1` 保留为空系统提示词，确保既有单测与 v1 复跑语义不变；
  - **Token 统计口径如实披露**：当注入自定义 `TokenCounter` 时沿用 `"injected"`；当使用 `llm.Usage` 原生累加且累计值 `> 0` 时，`ReportEnv.TokenCounting` 标注为 `"llm-usage（API 实测返回）"`；若假件 `Chatter` 未返回 `Usage` 且未注入 `TokenCounter`，仍如实标注 `"not-wired"`，绝不伪造非零数字。

---

## 5. Test Strategy

- **Unit Tests**:
  - `architecture_test.go`：`TestArchitecture_DAGImports`（校验全仓 import 白名单，含突变验证）与 `TestArchitecture_FileLineBudget`（校验生产代码 `<= 400` 行、测试代码 `<= 800` 行）。
  - `llm/client_test.go`：验证 `joinEndpoint` 对带/不带 `/v1` 及尾随 `/` 的组合；验证 `Embed` 在 `EmbedAPIKey` 为空时不发 `Authorization` 头且正常请求；验证 `ChatWithTools` 序列化 `Temperature`/`Seed` 并正确解析 `Usage`。
  - `agent/engine_test.go`：验证 `RunWithOptions` 注入 `SystemPrompt` 时首条消息为 `role=system`，且多步循环跨轮次正确触发 `OnUsage` 累加；验证 `tool.TerminalClassifier` 契约。
  - `worker/pool_test.go`：验证实现了 `tool.TerminalClassifier` 的错误在 `worker.process` 中被映射为指定脱敏终态（如 `"index missing"`），且 `worker` 包零 `retrieval` import。
  - `prompts/prompts_test.go`：验证 `eval-template-v1`、`eval-template-v2` 加载与未知版本显式报错。
- **Integration & Empirical Evaluation Scenarios**:
  - 使用 DeepSeek API + 本地 Ollama `nomic-embed-text:latest`（768 维）运行 `-eval`（`eval-template-v2`），产出 `docs/eval/` 最新对照报表。
- **Verification Commands**:
  ```bash
  GOWORK=off go test -race ./... -count=1
  GOWORK=off go build ./...
  GOWORK=off go vet ./...
  bash .githooks/pre-commit
  ```

---

## 6. Fog of War & Scope Boundaries

- **Fog of War (Not Yet Specified)**:
  - 本地 Ollama `nomic-embed-text:latest` 在 CJK + 英文混合 ADR 语料上的实际 RRF 融合增益幅度需以 Stage 3 实跑报表数据为准（按 ADR-0018 纪律，如实记录分布，不预设数字）。
- **Out of Scope (Explicit Non-Goals)**:
  - 不引入任何新的第三方 Go 依赖（保持 `go.mod` 仅 `modernc.org/sqlite` 与 `sqlite-vec`）；
  - 不修改 HTTP 路由路径或对外 JSON 响应结构契约；
  - 不做分布式队列或重排（Reranker）外部服务。

---

## Self-Review Checklist
- [x] Zero "TODO" or "TBD" placeholders.
- [x] No ambiguous assumptions or hidden scope creep.
- [x] The Ladder of Reuse checked: preferred stdlib (`go/parser`, `embed`) or existing modules (`agent/tool`) where applicable.
