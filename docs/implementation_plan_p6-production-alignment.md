# agent-runtime P6 — 生产级对齐、结构熵增治理与混合检索实证实施计划

> **Goal**: 试点落地「错误协议下沉 + 同包按正交职责切文件 + 根级 AST DAG 依赖与体积机械门禁」结构熵增治理三件套，补齐 LLM 端点归一化、Ollama 免密向量嵌入、采样确定性参数（`Temperature`/`Seed`）、跨步 Token 真实计量（`Usage`）与版本化 System Prompt 注入（`prompts/`），完成 `lexical` vs `hybrid`（Ollama `nomic-embed-text:latest`）v2 实证评测，并全面刷新 `README.md`、ADR 状态与 `docs/interview/` 面试资产。
> **Tech Stack**: Go 1.24+（module `agent-runtime`）；标准库优先（`go/parser`、`embed`），既有依赖 `modernc.org/sqlite`（含 `vec` 子包），**零新增第三方运行时依赖**。
> **Spec Reference**: [2026-10-23 P6 Design Spec](specs/2026-10-23-p6-production-alignment-and-hybrid-eval-design.md) · [ADR-0017](adr/0017-retrieval-as-tool.md) · [ADR-0018](adr/0018-retrieval-evaluation-harness.md) · `{vault_root}/01-Rules/LAYERED-ARCHITECTURE.md`（及 `99-Inbox/2026-10-23-多版本迭代后的结构熵增治理` 沉淀草稿）。
> **Repo Invariants Pointer**: `AGENTS.md` 与 `.agent-context.md`（每个 subagent brief 直接引用，不重复抄写全套不变式）。
> **Global Constraints**:
> - **零新依赖**：`go.mod` 的 `require` 块 MUST 保持不变。
> - **零隐私路径红线**：全仓任何文件（含注释、文档、测试、WORKMEMORY）MUST NOT 出现本机盘符或隐私目录字面量，一律使用 `{vault_root}` 契约。
> - **依赖方向单向向上（DAG 白名单）**：`worker` MUST NOT import `retrieval`（通过 `tool.TerminalClassifier` 协议下沉解耦）；根级 `architecture_test.go` 经 `go/parser` 扫描全仓强制执行。
> - **单文件体积预算（Line Budget）**：全仓所有生产 `.go` 文件（非 `*_test.go`）MUST `<= 400` 行，测试 `.go` 文件 MUST `<= 800` 行，由 `architecture_test.go` 机械把守。
> - **错误脱敏红线**：`task.Error` 只存粗粒度分类（含經 `tool.TerminalClassifier` 提取的 `"index missing"`）；trace `KindError` 只存 `"tool error: retryable"` / `"tool error: permanent"`。
> - **测试与竞态门禁**：所有任务验证命令统一加 `GOWORK=off`（`GOWORK=off go test -race ./... -count=1`）；提交前必过 `.githooks/pre-commit`，禁止 `--no-verify`。
> **Branch**: `feat/p6-production-alignment` — 从 `master` 开出，Task 1 前创建，全套计划与实现置于同一分支、经单个 PR 合回 `master`。

---

## [NOTE] Decisions So Far

- **[Design Spec 2026-10-23](specs/2026-10-23-p6-production-alignment-and-hybrid-eval-design.md)**：选定 Option C（P6 全栈旗舰闭环），并纳入大厂结构熵增治理三件套在本仓先行试点。
- **结构熵增治理沉淀（`99-Inbox/2026-10-23-多版本迭代后的结构熵增治理...`）**：
  1. 用 `agent/tool.TerminalClassifier` 接口下沉替代 `worker -> retrieval` 的跨层 import；
  2. 对当前超 400 行的 5 个生产文件（`retrieval/search.go` 473 行、`eval/run.go` 441 行、`retrieval/index.go` 438 行、`main.go` 410 行、`api/handler.go` 409 行）执行**同包按正交职责切文件**，零新增碎包、零导出面膨胀；
  3. 用根级 `architecture_test.go`（`go/parser` AST 扫描）锁死全仓 12 包 DAG 依赖矩阵与 400/800 行文件体积预算。
- **Ollama 本地向量实证**：本机已安装 `nomic-embed-text:latest`（768 维），通过修复 `llm.Client` 的 `/v1` 路径幂等拼接与允许空 `EmbedAPIKey`，直接驱动 `hybrid` 组实测。

---

## [NOTE] File-Occupancy Partitioning (Multi-Subagent Collision Guard)

| Task | 独占修改/新建文件集 | 并发安全性 |
| --- | --- | --- |
| Task 1 | `agent/tool/tool.go`, `agent/tool/registry_test.go`, `retrieval/index.go`, `worker/pool.go`, `worker/pool_test.go` | 阶段 1 串行基石 |
| Task 2 | `main.go`, `cli.go`, `api/handler.go`, `api/events.go`, `retrieval/search.go`, `retrieval/vector_search.go`, `retrieval/schema.go`, `eval/run.go`, `eval/collector.go`, `architecture_test.go` | 依赖 Task 1 完成 `index.go`/`pool.go` 修改后执行 |
| Task 3 | `config/config.go`, `config/config_test.go`, `config.yaml.example`, `llm/client.go`, `llm/client_test.go` | 与 Task 1/2 文件集完全正交 |
| Task 4 | `prompts/prompts.go`, `prompts/prompts_test.go`, `prompts/*.md`, `agent/engine.go`, `agent/engine_test.go` | 依赖 Task 3 的 `llm.Usage` |
| Task 5 | `eval/run.go`, `eval/report.go`, `eval/run_test.go`, `cli.go`, `main_test.go` | 依赖 Task 2 (`cli.go`/`eval/run.go`) + Task 3 + Task 4 |
| Task 6 | `docs/eval/*` | HITL 实跑评测，依赖 Task 1–5 |
| Batch 7 | `README.md`, `docs/adr/0017-retrieval-as-tool.md`, `docs/adr/0018-retrieval-evaluation-harness.md`, `docs/interview/*.md`, `WORKMEMORY/*` | 收尾文档同步 |

---

## [NOTE] Active Frontier (Unblocked Tasks)

### Task 1: 错误协议下沉（`tool.TerminalClassifier`）斩断 `worker -> retrieval` 反向依赖 [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Modify: `agent/tool/tool.go`（新增 `TerminalClassifier` 接口）
- Modify: `retrieval/index.go:132-140`（`ErrIndexMissing` 改为实现 `TerminalStatus() string { return "index missing" }` 的具名错误类型）
- Modify: `worker/pool.go:1-17,213-244`（移除 `"agent-runtime/retrieval"` import，改用 `var tc tool.TerminalClassifier; errors.As(err, &tc)` 提取终态）
- Test: `agent/tool/registry_test.go`、`worker/pool_test.go`

**Interfaces:**
- Consumes: `errors.As(err, &target)`
- Produces:
  - `type TerminalClassifier interface { error; TerminalStatus() string }`（in `package tool`）
  - `retrieval.ErrIndexMissing` satisfies `errors.Is(err, retrieval.ErrIndexMissing)` AND `tool.TerminalClassifier` (`TerminalStatus() == "index missing"`)

**Injected Instincts:**
- [ ] `[Instinct: Protocol-Lowering]`: `worker/pool.go` MUST NOT import `"agent-runtime/retrieval"`；终态提取只认 `tool.TerminalClassifier`。
- [ ] `[Instinct: Error-Identity]`: `retrieval.ErrIndexMissing` 改类型后，既有的 `errors.Is(err, retrieval.ErrIndexMissing)` 与 `fmt.Errorf("%w: ...", ErrIndexMissing)` 包装链 MUST 100% 保持兼容。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 1: Error Protocol Lowering (`tool.TerminalClassifier`) to decouple `worker` from `retrieval`.
> Mode: AFK | Role: TDD Builder | Repo Invariants: Read `AGENTS.md` and `.agent-context.md`.
> Goal: Remove `import \"agent-runtime/retrieval\"` from `worker/pool.go` by introducing `tool.TerminalClassifier` in `agent/tool/tool.go`.
> Target Files: Modify `agent/tool/tool.go`, `retrieval/index.go`, `worker/pool.go`; Test `agent/tool/registry_test.go`, `worker/pool_test.go`.
> TDD Steps:
> 1. RED: In `agent/tool/registry_test.go`, add `TestTerminalClassifier_InterfaceContract` asserting an error implementing `TerminalStatus() string` is matched via `errors.As`. In `worker/pool_test.go`, add `TestPool_Process_TerminalClassifierMapsCustomStatus` using a fake `Chatter` that returns a custom `tool.TerminalClassifier` error (without importing `retrieval`) and asserting `task.Error == \"index missing\"`.
> 2. Run `GOWORK=off go test ./agent/tool/... ./worker/... -count=1` to verify RED.
> 3. GREEN: Add `TerminalClassifier` to `agent/tool/tool.go`; update `ErrIndexMissing` in `retrieval/index.go` to an unexported struct value `indexMissingError{}` implementing `Error() string` and `TerminalStatus() string { return \"index missing\" }`; update `worker/pool.go` to remove `\"agent-runtime/retrieval\"` and match `var tc tool.TerminalClassifier; errors.As(err, &tc) && tc.TerminalStatus() != \"\"`.
> 4. Run `GOWORK=off go test -race ./agent/... ./retrieval/... ./worker/... -count=1` and verify `go list -f '{{.Imports}}' ./worker` does NOT contain `retrieval`.
> Return: Summary + MANDATORY Physical Execution Receipt (exit code + `go list` output + git diff --stat)."

**Step Breakdown:**
- [ ] **Step 1: Write the failing test (RED)**
- [ ] **Step 2: Run test and verify it fails with expected message**
- [ ] **Step 3: Implement minimal production code (GREEN)**
- [ ] **Step 4: Run tests and verify all green (`-race`)**
- [ ] **Step 5: Refactor & Flatten with guard clauses (REFACTOR)**
- [ ] **Step 6: Physical Evidence Gate**: Capture raw terminal execution receipt (`GOWORK=off go test -race ./... -count=1` + `go list -f '{{.Imports}}' ./worker`)
- [ ] **Step 7: Git atomic commit** (on branch `feat/p6-production-alignment`)

---

### Task 2: 同包按职责文件切分（消灭 >400 行生产文件）+ 根级 AST 架构与体积门禁 [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Create: `architecture_test.go`（根包 `package main`：AST 全仓 DAG 依赖白名单断言 + 生产文件 `<=400` 行 / 测试文件 `<=800` 行预算断言）
- Create: `cli.go`（从 `main.go` 拆出 `cliFlags`、`parseFlags`、`evalOpts`、`evalCorpusDir`、`runIndexCmd`、`runEvalCmd`、`buildIndex`、`indexPath`、`retrievalOptions`、`newEmbedder`）
- Modify: `main.go`（仅保留 `main`、`run`、`runWithArgs`、服务装配、`enqueueRecovered`、`newServer`、`shutdown`，控制在 250 行以内）
- Create: `api/events.go`（从 `api/handler.go` 拆出 `isTerminalStatus`、`writeSSE`、`HandleEvents`、`streamEvents`）
- Modify: `api/handler.go`（保留路由与 REST CRUD/Cancel，控制在 240 行以内）
- Create: `retrieval/vector_search.go`（从 `retrieval/search.go` 拆出 `SearchVector`、`knnRow`、`knnHits`、`knnOrder`、`chunkHit`、`hybridEnabled`）
- Modify: `retrieval/search.go`（保留 FTS5 查询编译、`Search`、`SearchLexical`、`fuseHits`、`truncateHits`，控制在 360 行以内）
- Create: `retrieval/schema.go`（从 `retrieval/index.go` 拆出 DDL 常量、`createSchema`、`clearIndex`、`tableExists`、`writeCorpusMeta`）
- Modify: `retrieval/index.go`（保留 `Options`、`Index`、`Open`、`Close`、`CheckFresh`、`Rebuild`、`rebuildChunks`，控制在 320 行以内）
- Create: `eval/collector.go`（从 `eval/run.go` 拆出 `toolOutputCollector`、`collectingTool`、`registryWithCollector`、`traceHasMaxSteps`、`countTokens`、`tokenCountingState`、`checkCorpusDir`、`fileSHA256`、`dbPathOrDefault`）
- Modify: `eval/run.go`（保留 `RunConfig`、`TaskResult`、`Report`、`Run`、`validate`、`resolveChatter`、`buildRegistry`、`ensureFreshIndex`、`runOnce`，控制在 290 行以内）

**Interfaces:**
- Consumes: `go/parser.ParseFile(fset, path, nil, parser.ImportsOnly)`
- Produces:
  - `TestArchitecture_DAGImports(t *testing.T)` in `architecture_test.go`
  - `TestArchitecture_FileLineBudget(t *testing.T)` in `architecture_test.go`（`maxProdLines = 400`, `maxTestLines = 800`）
  - 全部现有包导出函数签名 100% 保持不变（纯同包内聚切文件）

**Injected Instincts:**
- [ ] `[Instinct: Cohesive-File-Split]`: 同包内切文件 MUST NOT 改变任何函数签名或新建无意义子包；切分后 `git diff` 仅表现为函数搬迁。
- [ ] `[Instinct: Zero-Blind-Spot-Gate]`: `architecture_test.go` MUST 断言 `scannedFiles > 0`（防空扫假绿），且对 `worker`、`api`、`retrieval`、`agent`、`agent/tool`、`store`、`llm`、`config`、`metrics`、`eval` 全部包验证内部 import 白名单。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 2: Cohesive Intra-Package File Splitting + Root AST Architecture & Line Budget Gate.
> Mode: AFK | Role: TDD Builder | Repo Invariants: Read `AGENTS.md` and `.agent-context.md`.
> Goal: Create `architecture_test.go` enforcing the full-repo DAG import matrix and file line budgets (prod <= 400 lines, test <= 800 lines), then split the 5 files exceeding 400 lines (`main.go`, `api/handler.go`, `retrieval/search.go`, `retrieval/index.go`, `eval/run.go`) into cohesive same-package files.
> Target Files: Create `architecture_test.go`, `cli.go`, `api/events.go`, `retrieval/vector_search.go`, `retrieval/schema.go`, `eval/collector.go`; Modify `main.go`, `api/handler.go`, `retrieval/search.go`, `retrieval/index.go`, `eval/run.go`.
> TDD Steps:
> 1. RED: Write `architecture_test.go` with `TestArchitecture_DAGImports` (using `go/parser` `ImportsOnly` across all repo dirs, asserting `scanned >= 50`) and `TestArchitecture_FileLineBudget` (asserting prod `.go` <= 400 lines and `*_test.go` <= 800 lines).
> 2. Run `GOWORK=off go test . -run TestArchitecture -count=1` and verify it FAILS (RED) naming the 5 oversized production files (`retrieval/search.go`, `eval/run.go`, `retrieval/index.go`, `main.go`, `api/handler.go`).
> 3. GREEN: Split each of the 5 files along orthogonal responsibilities within their respective packages (`cli.go`, `api/events.go`, `retrieval/vector_search.go`, `retrieval/schema.go`, `eval/collector.go`) without changing any function behavior.
> 4. Run `GOWORK=off go test -race ./... -count=1` and verify all 11 packages + `TestArchitecture_*` pass.
> Return: Summary + MANDATORY Physical Execution Receipt (RED failure output + GREEN pass output + `wc -l` of split files)."

**Step Breakdown:**
- [ ] **Step 1: Write `architecture_test.go` (RED on file line budget)**
- [ ] **Step 2: Run test and verify it fails naming the 5 files > 400 lines**
- [ ] **Step 3: Split the 5 files into cohesive same-package files (GREEN)**
- [ ] **Step 4: Run `GOWORK=off go test -race ./... -count=1` and verify all green**
- [ ] **Step 5: Verify mutation guard (temporarily inject banned import or 401-line check in test)**
- [ ] **Step 6: Physical Evidence Gate**: Capture raw terminal execution receipt
- [ ] **Step 7: Git atomic commit**

---

### Task 3: LLM 适配层与配置工业化对齐（`/v1` 幂等拼接、免密 Embedding、`Temperature`/`Seed`、`Usage` 与 `config.Model`） [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Modify: `config/config.go`（`Config` 新增 `Model string` 字段，支持 `MODEL` 环境变量与 `model` YAML 键，默认空串由装配层兜底 `"deepseek-chat"`）
- Modify: `config.yaml.example`（补充 `model` 与免密 `embed_api_key` 说明）
- Modify: `llm/client.go`（新增 `joinEndpoint(baseURL, path string) string`；`Embed` 允许 `embedAPIKey == ""` 免密请求；`Config` 新增 `Temperature *float64`、`Seed *int64`；新增 `Usage` 结构体并挂载到 `AssistantTurn.Usage`）
- Test: `config/config_test.go`、`llm/client_test.go`

**Interfaces:**
- Consumes: OpenAI compatible JSON response `{"choices":[...], "usage":{"prompt_tokens":N,"completion_tokens":M}}`
- Produces:
  - `config.Config.Model string`（YAML key `model`, env `MODEL`）
  - `type Usage struct { PromptTokens int `json:"prompt_tokens"`; CompletionTokens int `json:"completion_tokens"` }`（in `package llm`）
  - `llm.AssistantTurn.Usage Usage`
  - `llm.Config.Temperature *float64` & `llm.Config.Seed *int64`
  - `joinEndpoint(baseURL, endpointPath string) string`（幂等处理尾随 `/` 与 `/v1` 前缀重叠）

**Injected Instincts:**
- [ ] `[Instinct: Endpoint-Idempotency]`: `joinEndpoint("http://localhost:11434/v1", "/v1/embeddings")` 与 `joinEndpoint("http://localhost:11434", "/v1/embeddings")` MUST 均返回 `"http://localhost:11434/v1/embeddings"`。
- [ ] `[Instinct: Keyless-Local-Provider]`: `Client.Embed` 在 `embedBaseURL != "" && embedModel != ""` 但 `embedAPIKey == ""` 时 MUST 正常发请求且**不带** `Authorization` 头。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 3: LLM Client & Config Production Alignment.
> Mode: AFK | Role: TDD Builder | Repo Invariants: Read `AGENTS.md` and `.agent-context.md`.
> Goal: Support `model` in `config.Config`; fix `/v1` URL duplication via `joinEndpoint`; allow empty `embedAPIKey` for local Ollama; wire `Temperature`/`Seed` and response `Usage` in `llm.Client`.
> Target Files: Modify `config/config.go`, `config.yaml.example`, `llm/client.go`; Test `config/config_test.go`, `llm/client_test.go`.
> TDD Steps:
> 1. RED: Add tests in `config/config_test.go` for `model` YAML/env loading; add tests in `llm/client_test.go` for: (a) `Embed` with `EmbedBaseURL: srv.URL + \"/v1\"` and empty `EmbedAPIKey` hitting `/v1/embeddings` with no `Authorization` header; (b) `ChatWithTools` sending `temperature` and `seed` when configured and parsing `resp.Usage.PromptTokens`/`CompletionTokens`.
> 2. Run `GOWORK=off go test ./config/... ./llm/... -count=1` and verify RED.
> 3. GREEN: Implement `Config.Model` in `config/config.go`; implement `joinEndpoint`, optional `Authorization` header in `Embed`, `Temperature`/`Seed` request fields, and `Usage` parsing in `llm/client.go`. Keep `llm/client.go` <= 400 lines.
> 4. Run `GOWORK=off go test -race ./... -count=1` to confirm all tests and `TestArchitecture_FileLineBudget` pass.
> Return: Summary + MANDATORY Physical Execution Receipt."

**Step Breakdown:**
- [ ] **Step 1: Write failing tests in `config_test.go` and `client_test.go` (RED)**
- [ ] **Step 2: Run tests and verify failure**
- [ ] **Step 3: Implement `config.Model` and `llm.Client` enhancements (GREEN)**
- [ ] **Step 4: Run `GOWORK=off go test -race ./... -count=1` and verify green**
- [ ] **Step 5: Check line count of `llm/client.go` (<= 400 lines)**
- [ ] **Step 6: Physical Evidence Gate**
- [ ] **Step 7: Git atomic commit**

---

### Task 4: 版本化 System Prompt 包（`prompts/`）与 `agent.RunWithOptions`（系统提示词注入 + 跨步 Usage 累加） [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Create: `prompts/prompts.go`（`go:embed *.md` + `Get(version string) (string, error)` + 常量 `DefaultVersion = "default-v1"`, `EvalV1 = "eval-template-v1"`, `EvalV2 = "eval-template-v2"`）
- Create: `prompts/eval-template-v1.md`（空内容，保持 v1 历史对照幂等）
- Create: `prompts/eval-template-v2.md`（Grounded Citation 铁律与语料外拒答规范）
- Create: `prompts/default-v1.md`（生产服务默认 Agent 系统提示词）
- Create: `prompts/prompts_test.go`
- Modify: `agent/engine.go`（新增 `RunOptions{SystemPrompt string; OnUsage func(llm.Usage)}` 与 `RunWithOptions`，`Run` 委托调用 `RunWithOptions`）
- Modify: `architecture_test.go`（将 `agent-runtime/prompts` 加入 DAG 白名单：零内部依赖，允许 `main` 与 `eval` import）
- Test: `agent/engine_test.go`

**Interfaces:**
- Consumes: `llm.Usage` from `ChatWithTools`
- Produces:
  - `func Get(version string) (string, error)`（in `package prompts`）
  - `type RunOptions struct { SystemPrompt string; OnUsage func(llm.Usage) }`（in `package agent`）
  - `func RunWithOptions(ctx context.Context, c Chatter, prompt string, reg *tool.Registry, onStep func(TraceStep), opts RunOptions) (string, error)`（in `package agent`）

**Injected Instincts:**
- [ ] `[Instinct: Backward-Compatibility]`: `agent.Run` 签名保持不变；当 `opts.SystemPrompt == ""` 时不插入空 `system` 消息（保持既有测试中 `msgs[0].Role == "user"` 的断言 100% 兼容）。
- [ ] `[Instinct: Grounded-Citation-Prompt]`: `eval-template-v2.md` MUST 明确要求：① 事实问题必调 `search_docs`（中文查询词 ≥3 字，复杂问题可分步检索不同关键词）；② 若命中依据，最终回答必须原样写出完整的 `doc_id` 路径（如 `docs/adr/0015-crash-recovery-and-task-rehydration.md`）；③ 若语料库无相关依据（out-of-corpus），必须明确拒绝编造且**严禁在回答中提及任何 `doc_id` 路径，也不要继续盲搜不相关文档**。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 4: Versioned Prompts Package (`prompts/`) & `agent.RunWithOptions`.
> Mode: AFK | Role: TDD Builder | Repo Invariants: Read `AGENTS.md` and `.agent-context.md`.
> Goal: Create `prompts` package with `go:embed` templates (`eval-template-v1`, `eval-template-v2`, `default-v1`) and add `agent.RunWithOptions` supporting `SystemPrompt` injection and multi-step `OnUsage` callback.
> Target Files: Create `prompts/prompts.go`, `prompts/prompts_test.go`, `prompts/eval-template-v1.md`, `prompts/eval-template-v2.md`, `prompts/default-v1.md`; Modify `agent/engine.go`, `agent/engine_test.go`, `architecture_test.go`.
> TDD Steps:
> 1. RED: Write `prompts/prompts_test.go` testing `Get(\"eval-template-v1\") == \"\"`, `Get(\"eval-template-v2\") != \"\"`, and unknown version returning error. In `agent/engine_test.go`, add `TestRunWithOptions_SystemPromptAndUsage` asserting that when `SystemPrompt` is non-empty, `msgs[0]` has `Role: \"system\"`, and `OnUsage` is invoked on every step accumulating token counts across tool turns.
> 2. Run `GOWORK=off go test ./prompts/... ./agent/... -count=1` and verify RED.
> 3. GREEN: Implement `prompts` package and `agent.RunWithOptions`, register `agent-runtime/prompts` in `architecture_test.go`.
> 4. Run `GOWORK=off go test -race ./... -count=1` and verify all green.
> Return: Summary + MANDATORY Physical Execution Receipt."

**Step Breakdown:**
- [ ] **Step 1: Write failing tests in `prompts_test.go` and `engine_test.go` (RED)**
- [ ] **Step 2: Verify RED**
- [ ] **Step 3: Implement `prompts` package and `agent.RunWithOptions` (GREEN)**
- [ ] **Step 4: Run `GOWORK=off go test -race ./... -count=1`**
- [ ] **Step 5: Refactor & check line budgets**
- [ ] **Step 6: Physical Evidence Gate**
- [ ] **Step 7: Git atomic commit**

---

### Task 5: 评测 Harness 与 CLI 接线闭环（Prompt 模板加载、确定性 `Seed`/`Temperature=0`、真实 `llm-usage` 统计与免密 Embedder 装配） [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Modify: `eval/run.go`、`eval/collector.go`、`eval/report.go`（`Run` 加载 `prompts.Get(cfg.TemplateVersion)`；`resolveChatter` 为真实 LLM 默认注入 `Temperature=0` 与 `&cfg.Seed`；`runOnce` 调用 `agent.RunWithOptions` 注入 `SystemPrompt` 并累加 `llm.Usage`；当 `TokenCounter == nil` 且累加 `Usage > 0` 时报表标注 `"llm-usage（API 实测返回）"`）
- Modify: `cli.go`、`main.go`（`newEmbedder` 放宽为 `conf.EmbedBaseURL != "" && conf.EmbedModel != "" && conf.EmbedDim > 0` 即可构造，无需非空 `EmbedAPIKey`；默认模型收敛为 `defaultModel(conf.Model)` 即非空取 `conf.Model` 否则 `"deepseek-chat"`）
- Test: `eval/run_test.go`、`main_test.go`

**Interfaces:**
- Consumes: `prompts.Get(cfg.TemplateVersion)`, `agent.RunWithOptions`, `llm.Usage`
- Produces:
  - `eval.Run` 自动加载 `cfg.TemplateVersion` 对应系统提示词（未知模板版本早失败返回 error）
  - `ReportEnv.TokenCounting` 支持 `"llm-usage"` -> `tokenCountingNote("llm-usage") == "llm-usage（API 实测返回）"`
  - `newEmbedder(conf)` 在 `EmbedAPIKey == ""` 时仍返回非 nil `llm.Embedder`

**Injected Instincts:**
- [ ] `[Instinct: Controlled-Eval]`: `eval.Run` 对未知的 `TemplateVersion` MUST 立即在 `validate`/预检阶段返回 error，绝不静默当空串跑完。
- [ ] `[Instinct: Honest-Accounting]`: 只有当 `TokenCounter == nil` 且本轮真实 `Usage` 累计 token `> 0` 时才标 `"llm-usage"`；若假件未返回 `Usage` 且无 `TokenCounter`，保持 `"not-wired"`，绝不伪造。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 5: Wire Prompts, Deterministic Sampling, Real LLM Usage, and Keyless Embedder into `eval` & `main`.
> Mode: AFK | Role: TDD Builder | Repo Invariants: Read `AGENTS.md` and `.agent-context.md`.
> Goal: Connect `prompts.Get(cfg.TemplateVersion)` and `agent.RunWithOptions` in `eval`, accumulate multi-step `llm.Usage` into `TaskResult`, pass `Temperature=0` and `Seed` to `llm.NewClient`, and allow `newEmbedder` in `cli.go` to work without `EmbedAPIKey` (for local Ollama).
> Target Files: Modify `eval/run.go`, `eval/collector.go`, `eval/report.go`, `cli.go`, `main.go`; Test `eval/run_test.go`, `main_test.go`.
> TDD Steps:
> 1. RED: In `eval/run_test.go`, add `TestRun_LoadsPromptTemplateAndAccumulatesLLMUsage` (verifying `eval-template-v2` injects a system message into `Chatter` and `AssistantTurn.Usage` is summed into `Report` with `TokenCounting == \"llm-usage\"`), and `TestRun_UnknownTemplateFailsFast`. In `main_test.go`, update/add test verifying `newEmbedder` returns non-nil when `EmbedBaseURL`, `EmbedModel`, `EmbedDim` are set and `EmbedAPIKey` is empty.
> 2. Run `GOWORK=off go test ./eval/... . -count=1` and verify RED.
> 3. GREEN: Implement the wiring in `eval/` and `cli.go`/`main.go`. Keep all files <= 400 lines.
> 4. Run `GOWORK=off go test -race ./... -count=1` and verify all 12 packages pass.
> Return: Summary + MANDATORY Physical Execution Receipt."

**Step Breakdown:**
- [ ] **Step 1: Write failing tests in `eval/run_test.go` and `main_test.go` (RED)**
- [ ] **Step 2: Verify RED**
- [ ] **Step 3: Implement wiring in `eval/` and `cli.go`/`main.go` (GREEN)**
- [ ] **Step 4: Run `GOWORK=off go test -race ./... -count=1`**
- [ ] **Step 5: Verify line budgets (`<= 400` lines)**
- [ ] **Step 6: Physical Evidence Gate**
- [ ] **Step 7: Git atomic commit**

---

### Task 6: 三组受控实证评测（`lexical` v2 + 本地 Ollama `nomic-embed-text` `hybrid` v2） [Tier: 3-Critical] [Mode: HITL] [Role: Solo + Empirical Verification]

**Files:**
- Modify: `docs/eval/2026-10-23-lexical-r3.md`、`docs/eval/2026-10-23-lexical-r3.json`
- Create: `docs/eval/2026-10-23-hybrid-r3.md`、`docs/eval/2026-10-23-hybrid-r3.json`

**Interfaces:**
- Consumes: 本地 Ollama `http://localhost:11434` (`nomic-embed-text:latest`, `EMBED_DIM=768`) + DeepSeek API + `eval-template-v2`
- Produces: 包含真实 `llm-usage` Token 成本、`eval-template-v2` 下的 `lexical` 与 `hybrid` N=3 对照报表及人工判定结论段

**Injected Instincts:**
- [ ] `[Instinct: Zero-Fabrication]`: 严禁编造任何评测数字；必须通过真实执行 `go run . -eval` 落盘生成 JSON 与 Markdown 报表，若 API 或 Ollama 报错则如实排查。
- [ ] `[Instinct: Corpus-Hygiene]`: 跑完评测后确认临时测试数据库文件未污染工作区（或使用临时路径后清理）。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 6: Empirical Evaluation of `lexical` and `hybrid` (with local Ollama `nomic-embed-text:latest`, dim=768) under `eval-template-v2`.
> Mode: HITL | Role: Empirical Evaluator.
> Steps:
> 1. Verify Ollama is serving `nomic-embed-text:latest` (`curl -s http://localhost:11434/v1/embeddings -d '{\"model\":\"nomic-embed-text:latest\",\"input\":[\"probe\"]}' | head -c 200`).
> 2. Run `lexical` group with `eval-template-v2`: `GOWORK=off go run . -eval -group lexical -repeat 3 -template eval-template-v2`.
> 3. Run `hybrid` group with `EMBED_BASE_URL=http://localhost:11434/v1 EMBED_MODEL=nomic-embed-text:latest EMBED_DIM=768 GOWORK=off go run . -eval -group hybrid -repeat 3 -template eval-template-v2`.
> 4. Compare `baseline` vs `lexical` (v1 vs v2) vs `hybrid` (v2) across `in-corpus`, `cross-chunk`, `out-of-corpus`, P50/P95 latency, and real Token usage; append the structured conclusion to the generated `docs/eval/*.md` reports.
> Return: Summary table + raw exit codes."

**Step Breakdown:**
- [ ] **Step 1: Probe local Ollama `nomic-embed-text:latest` vector dimension (768)**
- [ ] **Step 2: Run `-eval -group lexical -repeat 3 -template eval-template-v2`**
- [ ] **Step 3: Run `-eval -group hybrid -repeat 3 -template eval-template-v2` with Ollama env vars**
- [ ] **Step 4: Fill in the empirical conclusion section in `docs/eval/2026-10-23-*-r3.md`**
- [ ] **Step 5: Clean up any transient `.db` artifacts and run `GOWORK=off go test -race ./... -count=1`**
- [ ] **Step 6: Git atomic commit**

---

### Batch 7 [Tier: 1-Light]: 全仓门面文档、ADR 状态与面试防御手册刷新 [Mode: AFK] [Role: Solo Builder]

**Files:**
- Modify: `README.md`（更新为 12 包结构、`-index`/`-eval` CLI 子命令、9 项配置含免密 Ollama `hybrid` 示例、最新测试数与评测对照摘要）
- Modify: `docs/adr/0017-retrieval-as-tool.md`、`docs/adr/0018-retrieval-evaluation-harness.md`（状态从 `已接受（未实施）` 更新为 `已实施（2026-10-23，PR #1 & P6 闭环）`，补充 `hybrid` 实证与 `eval-template-v2` 结论索引）
- Modify: `docs/interview/pitch-3min.md`、`docs/interview/interview-answers.md`、`docs/interview/project-qa.md`（从早期 M1/P1 口径升级至 P6 全栈口径：覆盖两阶段停机、抢占式取消、Rehydration 崩溃恢复、Pi 三层解耦与 AST 架构门禁、FTS5+vec0 RRF 混合检索及评测三关）
- Modify: `.agent-context.md`、`WORKMEMORY/PROJECT_OVERVIEW.md`、`WORKMEMORY/work.log`（同步最终测试数、12 包清單与 `WORK_END` 事件）

**Interfaces:**
- Consumes: Task 1–6 的真实测试计数、包结构与 `docs/eval/` 实测指标
- Produces: 与代码 100% 物理对齐的文档与面试资产，且 `git grep -n -E 'Obsidian|Haoyu|[CDcd]:[/\\]'` 保持零隐私路径命中

**Injected Instincts:**
- [ ] `[Instinct: Zero-Hallucinated-Numbers]`: 文档中的所有测试数、包数、评测通过率、Token 数 MUST 从终端实测命令重算得出，严禁凭记忆手填。
- [ ] `[Instinct: Zero-PII-Paths]`: 提交前必跑隐私路径正则扫描与 `.githooks/pre-commit`。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Batch 7: Full Documentation, ADR Status, and Interview Arsenal Refresh.
> Mode: AFK | Role: Solo Builder.
> Goal: Align `README.md`, `docs/adr/0017..0018.md`, `docs/interview/*.md`, `.agent-context.md`, and `WORKMEMORY/` with the completed P6 codebase and empirical evaluation results.
> Verification: Recompute `func Test` count, `=== RUN` count, and package count via terminal commands; run privacy regex check and `GOWORK=off go test -race ./... -count=1`."

**Step Breakdown:**
- [ ] **Step 1: Update `docs/adr/0017-retrieval-as-tool.md` and `0018-retrieval-evaluation-harness.md` status headers & empirical notes**
- [ ] **Step 2: Refresh `README.md` with 12-package tree, CLI commands, Ollama hybrid config, and test count**
- [ ] **Step 3: Upgrade `docs/interview/pitch-3min.md`, `interview-answers.md`, and `project-qa.md` to P6 architecture**
- [ ] **Step 4: Sync `.agent-context.md`, `WORKMEMORY/PROJECT_OVERVIEW.md`, and append `WORK_END` to `WORKMEMORY/work.log`**
- [ ] **Step 5: Run full verification gates (`go test -race`, privacy grep, `pre-commit`) and commit**

---

## [NOTE] Fog of War (Not Yet Specified)

- **[Cross-Encoder Reranker 独立阶段]**：当前 `hybrid` 采用 BM25 + `vec0` KNN 双路等权 RRF（`k=60`）。若未来语料规模扩大至万级 chunk 且 Top-5 排序精度成为瓶颈，再评估本地 Cross-Encoder 重排。

---

## [NOTE] Deferred Gap Register

| gap | why deferred | trigger that forces revisiting | owner |
| --- | --- | --- | --- |
| 将「结构熵增治理三件套」并入 `dfs-*` 技能库（`dfs-plan` / `dfs-review` / `dfs-gate`） | 用户明确指示：本会话仅在知识库 `99-Inbox` 完成沉淀并在 `agent-runtime` P6 先行试点，暂不跨仓改写 `dfs-*` 技能定义 | `agent-runtime` P6 PR 合入 `master`、证明 AST DAG + 400 行体积闸与协议下沉零误报后，在专门的 `skill-dev` 会话中执行 `/dfs-inbox-consolidate` 并升级技能 | user / hermes-agent |
| `demo.html` 前端展示检索引用高亮与 Token 消耗 | 当前单页 Demo 已完整展示 `steps` 轨迹（含 `search_docs` 调用与返回）；前端视觉美化非后端核心考核点 | 需要录制可视化演示视频或参加带 UI 评审的路演时 | implementer |
| 增量文件级索引更新（Incremental Indexing） | 当前语料（约 40 份 Markdown）全量分块 + 本地 Ollama 嵌入重建耗时在秒级，全量指纹幂等跳过已足够 | 语料库增长至 >500 份文档或单次 `-index` 重建耗时 >60s 时 | implementer |

---

## [NOTE] Out of Scope

- 不在本会话中直接修改 `~/.cc-switch/skills/` 或 `skill-dev` 仓库中的 `dfs-*` 技能源码（已按指令完成 `99-Inbox` 沉淀与登记，本仓做试点落地）。
- 不引入任何外部重型框架（LangChain / LlamaIndex）或新的第三方 Go 依赖。

---

## [NOTE] Deviation Table (post-execution, filled before merge)

| plan text | what was actually built | why |
| --- | --- | --- |
| Task 2 同包切分清单提及 `retrieval/build.go` 与 `eval/batch.go` | 实际切分为 `retrieval/schema.go`（聚焦 SQLite DDL 与版本指纹）与 `eval/collector.go`（聚焦全文证据收集与计数辅助） | 文件职责内聚度更高，符合 Go 标准库命名惯例，行数预算（<=400 行）完全达成 |
| 计划提及使用 `scripts/vault_exec_state.py` 驱动 ledger | 本地不存在该脚本，降级使用 Hermes 原生 `todo_list` 状态机配合实施计划的 Step Breakdown 进行物理状态追踪 | 零阻碍推进，10 项任务状态全链路清晰可审 |
