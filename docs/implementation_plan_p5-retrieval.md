# agent-api P5 — 检索即工具 + 评测 harness 实施计划

> **Goal**: 按 ADR-0017 v2 落地 `search_docs` 检索工具（独立 `retrieval` 包 / FTS5+vec0 / 默认 lexical /
> span 引用硬契约 / Tool 接口 ctx 化 + Retryable），并按 ADR-0018 落地评测 harness（任务集入仓 /
> `eval run` 对照 / N=3 分布 / 首错归因 / 无提升不合入）。**不引任何新运行时依赖。**
> **Tech Stack**: Go 1.26.5（module `agent-runtime`）；标准库优先，既有 opt-in 依赖 `modernc.org/sqlite v1.58.0`
> （FTS5 与 `modernc.org/sqlite/vec` 均在其模块内，`go mod tidy` 后 go.mod MUST 零变化——Task 3 物理验证）。
> **Spec Reference**: [ADR-0017](adr/0017-retrieval-as-tool.md) v2 · [ADR-0018](adr/0018-retrieval-evaluation-harness.md)
> · vault `01-Rules/LAYERED-ARCHITECTURE.md`（分层判据）。
> **Global Constraints**:
> - **零新依赖**：`go.mod` 的 require 块 MUST 与当前逐字节一致（vec 子包同模块，不算新依赖；若 tidy 产生变化即失败）。
> - **依赖方向单向向上**（Pi 判据）：`retrieval → llm`、`retrieval → agent/tool`；**任何包不得 import retrieval 的反向**；
>  `agent` 认知内核零检索依赖；`api` 仍禁 import `worker`（既有门禁）。
> - **Tool 接口只加不改**：`tool.Tool` 原子（Spec/Execute）保留，检索扩展字段在 retrieval 自己的类型上。
> - **禁止静默降级**：无 embedding 凭证 = `mode="lexical"` 显式标注；`mode`/`truncated` 必达 trace。
> - **错误脱敏红线**：`task.Error` 只存粗粒度分类（含新增 `"index missing"`）；错误全文只进 slog；
>  trace 的 `KindError` detail 只存粗分类（`"tool error: retryable"` / `"tool error: permanent"`）。
> - **取消语义到底**：`Execute(ctx, ...)` 全链透传；重试退避用 ctx 感知定时器，**禁止裸 `time.Sleep`**；
>  单次 search 内嵌 deadline = 剩余任务预算 50%。
> - **SQLite 写纪律**：重建 = 分块事务（每 50 行一提交），禁止全量单事务；WAL + busy_timeout 沿用 store 模式。
> - **指纹 = 内容 SHA-256 聚合**（禁用 mtime）；索引检查点在工具调用时，启动时仅提示。
> - 每个任务：RED→GREEN→REFACTOR + **原始终端回执**（exit code/通过数/diff）+ 原子提交；
>  pre-commit 零泄漏钩子已启用，**禁止 `--no-verify`**。
> - worker/retrieval/eval 改动 MUST `GOWORK=off go test -race ./... -count=1` 通过。
> - `AGENTS.md` / `.agent-context.md` 为仓库不变量锚点，每个 subagent brief 引用而非重述。
> **Branch**: `feat/retrieval-as-tool` — 从 `master` 开出，Task 1 前创建，经 PR 合回；禁止直推 master。

---

## [NOTE] Decisions So Far

- [ADR-0017 v2](adr/0017-retrieval-as-tool.md)：检索即工具；独立 `retrieval` 包；FTS5+vec0 内置能力；默认 lexical；
  span 硬契约；哨兵三错误；分块事务；内容哈希指纹。
- [ADR-0018](adr/0018-retrieval-evaluation-harness.md)：轻量 CLI harness；三条 MUST（成对受控/程序判定/首错归因）；
  任务集入仓先于实现；N=3 分布；**无提升不合入**（Task 12 HITL 判定）。
- ADR-0014：`Execute(ctx, ...)` + `WithCancelCause` 取消语义；本计划 Task 1 落地工具侧。
- ADR-0013：依赖倒置与单向依赖；Task 3 的 retrieval 门禁测试镜像 `api/architecture_barrier_test.go` 模式。
- vault LAYERED-ARCHITECTURE §3：可独立使用测试 = 物理移除上层依赖后底层仍编译+测试通过。
- 环境事实（已核）：`tool.Tool` 在 `agent/tool/tool.go:11-14`；`Registry.Execute` 压错误为串（`tool.go:64-74`）；
  `agent.Run` 循环在 `agent/engine.go:19-85`；`NewPool` 硬编码 `tool.DefaultRegistry()`（`worker/pool.go:70`）；
  `SQLStore` 连接模式在 `store/sql.go:27-52`（WAL + busy_timeout 5000）；ledger 格式见 `.vault-exec-ledger.json`。
- 用户已自行完成：`main.go` 模型名 `deepseek-chat` → `deepseek-flash`（2026-07-24 退役名迁移），本计划不含该改动。

---

## [NOTE] Active Frontier (Unblocked Tasks)

### Task 1: Tool 接口 ctx 化 + Retryable 错误分类 [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Modify: `agent/tool/tool.go`（接口 + Registry + 新增 RetryableError 与 ExecuteErr）
- Modify: `agent/tool/calc.go`、`agent/tool/time.go`（Execute 签名适配）
- Modify: `agent/engine.go:25-30,71-80`（exec 闭包走 ExecuteErr + 重试分支）
- Test: `agent/tool/registry_test.go`、`agent/engine_test.go`（新增可重试错误工具用例）

**Interfaces:**
- Consumes: `type Tool interface { Spec() llm.Tool; Execute(args json.RawMessage) (string, error) }`
- Produces:
  - `type RetryableError interface { error; Retryable() bool }`
  - `func (r *Registry) ExecuteErr(ctx context.Context, call llm.ToolCall) (string, error)`（原始错误出口）
  - `func (r *Registry) Execute(ctx context.Context, call llm.ToolCall) string`（既有压串语义的 ctx 化包装）
  - `type Tool interface { Spec() llm.Tool; Execute(ctx context.Context, args json.RawMessage) (string, error) }`

**Injected Instincts:**
- [ ] `[Instinct: Concurrency]`: 退避用 `select { case <-time.After(d): case <-ctx.Done(): }`，禁裸 Sleep；`-race` 全绿。
- [ ] `[Instinct: Add-Only-Interface]`: `tool.Tool` 只追加 ctx 参数，不删不改既有语义；calc/time 适配为最小改动。
- [ ] `[Instinct: Trace-Redline]`: 工具失败 emit `KindError`，detail 只放 `"tool error: retryable"` / `"tool error: permanent"` 粗分类。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 1: Tool ctx 化 + Retryable。Mode: AFK | Role: TDD Builder。
> Goal: 让工具执行可取消、错误可分类，供 agent.Run 分支决策（ADR-0017 决策 5）。
> Target Files: Modify `agent/tool/tool.go`（`RetryableError` 接口 + `Registry.ExecuteErr(ctx, call) (string, error)` + `Registry.Execute(ctx, call) string` 包装 + `Tool.Execute` 签名加 ctx）, Modify `agent/tool/calc.go` `agent/tool/time.go`（签名适配）, Modify `agent/engine.go`（exec 闭包改用 ExecuteErr；retryable 错误按 tc.ID 去重重试一次，退避 200ms→400ms ctx 感知；仍失败或 non-retryable → emit KindError 粗分类 + 错误串喂回模型）, Test `agent/tool/registry_test.go` `agent/engine_test.go`。
> Injected Instincts: Concurrency（禁裸 Sleep）；Add-Only-Interface；Trace-Redline（KindError 只存粗分类）。
> TDD Steps:
> 1. RED：`registry_test.go` 写 `TestRegistry_ExecuteErr_*`（ctx 取消时工具收到取消；错误经 ExecuteErr 原样返回不经压串）；`engine_test.go` 写假工具返回 `&retryableErr{}`（实现 Retryable()=true），断言 Run 内该工具被调用 2 次且 trace 含 KindError；non-retryable 假工具断言只调 1 次。
> 2. 运行 `GOWORK=off go test ./agent/... -count=1` 确认 RED（编译失败先补签名）。
> 3. GREEN：最小实现（上述 Produces 签名逐一命中）。
> 4. 运行 `GOWORK=off go test -race ./agent/... ./worker/... -count=1` 全绿。
> 5. REFACTOR：guard clause 展平（engine 重试分支 ≤2 层嵌套）；确认 `default.go` 无需改。
> Return: Summary + MANDATORY 物理回执（exit code + 通过数 + git diff --stat）。"

**Step Breakdown:**
- [ ] Step 1: 写失败测试（RED）
- [ ] Step 2: 运行并确认失败信息符合预期
- [ ] Step 3: 最小实现（GREEN）
- [ ] Step 4: `-race` 全绿验证
- [ ] Step 5: Refactor & guard-clause 展平
- [ ] Step 6: 物理证据门（exit code / 通过数 / diff）
- [ ] Step 7: 原子提交（分支 `feat/retrieval-as-tool`）

---

### Task 2: config 扩展 embedding 四项配置 [Tier: 1-Light] [Mode: AFK] [Role: Solo Builder]

**Files:**
- Modify: `config/config.go`（Config 四字段 + Load 环境变量 + loadFile 四个 case + 未知 key 错误文案）
- Modify: `config.yaml.example`（四行示例 + 注释）
- Test: `config/config_test.go`（镜像既有测试模式）

**Interfaces:**
- Consumes: `type Config struct { DeepSeekAPIKey, APIAuthKey, DbPath string; TaskTimeoutSeconds int }`
- Produces: `Config` 新增 `EmbedBaseURL, EmbedAPIKey, EmbedModel string; EmbedDim int`；env `EMBED_BASE_URL/EMBED_API_KEY/EMBED_MODEL/EMBED_DIM`；yaml key `embed_base_url/embed_api_key/embed_model/embed_dim`（EMBED_DIM 正整数校验，负值启动失败）

**Injected Instincts:**
- [ ] `[Instinct: Config-Fail-Loud]`: 未知/非法值一律启动失败不静默（对齐既有 task_timeout_seconds 模式）。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 2: config embedding 四项。Mode: AFK | Role: Solo Builder。
> Goal: 为向量通道提供独立凭证配置（默认空 = lexical only，ADR-0017 决策 3）。
> Target Files: Modify `config/config.go`（四字段 + 四 env + 四 yaml case + 错误文案更新为列举 8 个合法 key）, Modify `config.yaml.example`, Test `config/config_test.go`。
> TDD Steps:
> 1. RED：`config_test.go` 写 `TestLoad_EmbedEnvPriority`（env 覆盖 yaml）、`TestLoad_EmbedDimInvalid`（EMBED_DIM=abc/-1 返回 error）。
> 2. 运行 `GOWORK=off go test ./config/ -count=1` 确认 RED。
> 3. GREEN：最小实现。
> 4. 全绿 + `go vet ./config/`。
> Return: Summary + 物理回执（exit code + grep 计数：`grep -c 'embed' config/config.go`）。"

**Step Breakdown:**
- [ ] Step 1: RED 测试
- [ ] Step 2: 确认 RED
- [ ] Step 3: GREEN 实现
- [ ] Step 4: 全绿 + vet
- [ ] Step 5: 物理证据门
- [ ] Step 6: 原子提交

---

### Task 3: retrieval 包骨架 + DDL + 依赖方向门禁 [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Create: `retrieval/index.go`（`Open(path string, opts Options) (*Index, error)`；DDL：`doc_chunk`/`doc_fts`（FTS5 外接 `content=doc_chunk`）/`doc_vec`（vec0，dim>0 才建）/`embed_cache`/`corpus_meta`；WAL + busy_timeout 5000）
- Create: `retrieval/retrieval_test.go`（临时目录 DB 冒烟：Open→建表→Close）
- Create: `retrieval/architecture_test.go`（门禁：检索包源码禁 import `agent-runtime/agent`、`agent-runtime/worker`、`agent-runtime/api`——镜像 `api/architecture_barrier_test.go` 的 ReadFile+Contains 模式，扫描范围 = retrieval 目录全部 `*.go`）

**Interfaces:**
- Consumes: `store/sql.go` 的 PRAGMA 模式（WAL/busy_timeout，仅参照不 import）
- Produces:
  - `type Options struct { Embedder llm.Embedder; EmbedDim int }`（Embedder=nil 或 EmbedDim=0 → 纯词法模式，不建 vec0）
  - `func Open(path string, opts Options) (*Index, error)`
  - `func (idx *Index) Close() error`

**Injected Instincts:**
- [ ] `[Instinct: Pi-Layer]`: retrieval 只许依赖 `llm` 与 `agent/tool`（Task 7 才引入）；本任务 import 仅 `database/sql`/`modernc.org/sqlite`。
- [ ] `[Instinct: Zero-New-Dep]`: 完成后 `go mod tidy && git diff --stat go.mod go.sum` 必须空。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 3: retrieval 骨架 + DDL + 门禁。Mode: AFK | Role: TDD Builder。
> Goal: 建可独立使用的 retrieval 包底座（ADR-0017 决策 2；vault LAYERED-ARCHITECTURE §3）。
> Target Files: Create `retrieval/index.go`（Open/Close + DDL + Options）, Create `retrieval/retrieval_test.go`（t.TempDir 冒烟）, Create `retrieval/architecture_test.go`（门禁扫描 retrieval 目录源码禁 import agent/worker/api）。
> DDL 细节：`doc_chunk(chunk_id INTEGER PRIMARY KEY AUTOINCREMENT, doc_id TEXT, source TEXT, span_start INT, span_end INT, text TEXT, corpus_version TEXT)` + `INDEX(doc_id)` + `INDEX(corpus_version)`；`doc_fts` FTS5 `content=doc_chunk, content_rowid=chunk_id, tokenize='trigram'`；`doc_vec` vec0 `embedding float[EmbedDim]`（EmbedDim>0 才建）；`embed_cache(text_hash TEXT PRIMARY KEY, model TEXT, dim INT, vector BLOB, created_at INT)`；`corpus_meta(singleton INT PRIMARY KEY CHECK(singleton=1), version TEXT, built_at INT)`。
> TDD Steps:
> 1. RED：`retrieval_test.go` 写 `TestOpen_CreatesSchema`（Open 后 `SELECT name FROM sqlite_master` 含全部表；EmbedDim=0 时无 doc_vec）；`architecture_test.go` 写 `TestArchitectureBarrier_RetrievalDoesNotImportUpperLayers`。
> 2. 运行 `GOWORK=off go test ./retrieval/ -count=1 -v` 确认 RED。
> 3. GREEN：实现 Open/Close/DDL/Options。
> 4. 全绿 + **物理零新依赖验证**：`go mod tidy` 后 `git diff --exit-code go.mod go.sum`。
> 5. REFACTOR：DDL 常量集中，SQL 串禁 fmt.Sprintf 拼用户值（dim 为受信整数例外，注释说明）。
> Return: Summary + 物理回执（exit code + go.mod diff 为空 + 通过数）。"

**Step Breakdown:**
- [ ] Step 1: RED 测试（schema + 门禁）
- [ ] Step 2: 确认 RED
- [ ] Step 3: GREEN 实现
- [ ] Step 4: `go mod tidy` 零 diff 物理验证
- [ ] Step 5: Refactor
- [ ] Step 6: 物理证据门
- [ ] Step 7: 原子提交

---

### Task 4: 索引构建（分块 + FTS5 + 内容哈希指纹 + 分块事务） [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Modify: `retrieval/index.go`（新增 Rebuild）
- Create: `retrieval/chunk.go`（文档扫描 + 分块 + bigram 预切）
- Create: `retrieval/fingerprint.go`（per-doc SHA-256 → 聚合指纹）
- Test: `retrieval/index_test.go`、`retrieval/chunk_test.go`、`retrieval/fingerprint_test.go`

**Interfaces:**
- Consumes: `func Open(path string, opts Options) (*Index, error)`
- Produces:
  - `type BuildReport struct { DocsIndexed, DocsSkipped, ChunksWritten int; Fingerprint string }`
  - `func (idx *Index) Rebuild(ctx context.Context, corpusDir string) (BuildReport, error)`
  - `func (idx *Index) CorpusVersion() (string, error)`（读 corpus_meta；无记录返回 ""）
  - chunk 常量：`ChunkSize=512`、`ChunkOverlap=64`（rune 计，注释标明为可调参数）

**Injected Instincts:**
- [ ] `[Instinct: Chunked-Txn]`: 每 50 行一提交；崩溃半成品靠指纹不符触发重建（ADR-0017 决策 6）。
- [ ] `[Instinct: Content-Hash]`: 指纹 = 各 doc SHA-256 排序聚合再 SHA-256；mtime 一律不参与。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 4: 索引构建。Mode: AFK | Role: TDD Builder。
> Goal: 幂等地把语料目录构建为 FTS5 可检索索引（ADR-0017 决策 6）。
> Target Files: Modify `retrieval/index.go`（Rebuild/CorpusVersion）, Create `retrieval/chunk.go` `retrieval/fingerprint.go`, Test 三个 _test.go。
> TDD Steps:
> 1. RED：`chunk_test.go`——中文文本按 rune 窗口分块且重叠正确、bigram 预切后以空格连接；`fingerprint_test.go`——同内容同指纹、改 1 字节指纹变、跨路径同内容指纹不变；`index_test.go`——`TestRebuild_Idempotent`（连续两次 Rebuild 后 doc_chunk 行数一致、corpus_meta.version 一致）、`TestRebuild_ChunkedTxn`（50+ 块语料构建成功）、`TestRebuild_EmptyDir`（零文档返回 report 不报错）。
> 2. 确认 RED。
> 3. GREEN：Rebuild = 计算指纹 → 与 corpus_meta 比对（相同即跳过返回旧 report）→ 分块事务 DELETE+INSERT → 更新 corpus_meta。
> 4. 全绿（`-race`）。
> 5. REFACTOR：guard clause 展平；span_start/end 记录原文字符偏移（供引用契约）。
> Return: Summary + 物理回执（exit code + 通过数）。"

**Step Breakdown:**
- [ ] Step 1: RED（chunk/fingerprint/index 三组）
- [ ] Step 2: 确认 RED
- [ ] Step 3: GREEN
- [ ] Step 4: `-race` 全绿
- [ ] Step 5: Refactor
- [ ] Step 6: 物理证据门
- [ ] Step 7: 原子提交

---

### Task 5: Embedder 接口 + llm.Client 实现 + retrieval 缓存 [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Modify: `llm/client.go`（Config 四字段 + Client 持有 embed 配置 + `Embed` 方法 + `Embed` 哨兵）
- Modify: `retrieval/index.go`（Options.Embedder 生效路径：`embedTexts` 缓存优先 → Embedder → 落缓存）
- Test: `llm/client_test.go`（httptest 假 embeddings 端点）、`retrieval/embed_test.go`

**Interfaces:**
- Consumes: `type Config struct { APIKey, BaseURL, Model string; HTTPTimeout time.Duration }`
- Produces:
  - `type Embedder interface { Embed(ctx context.Context, texts []string) ([][]float32, error) }`
  - `func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error)`（OpenAI 兼容 `/v1/embeddings`；embed 配置为空时返回 `ErrEmbedderNotConfigured`）
  - `var ErrEmbedderNotConfigured = errors.New("llm: embedder not configured")`
  - retrieval 侧：`embedTexts(ctx, texts)`（缓存键 = SHA-256(model|dim|text)）+ 并发上限 4（semaphore chan）

**Injected Instincts:**
- [ ] `[Instinct: Consumer-Interface]`: Embedder 定义在 llm（模型适配层），retrieval 只消费（依赖单向）。
- [ ] `[Instinct: Cache-Bounded]`: embed_cache 有 TTL（7 天）与容量上限（1 万行，超限 FIFO 删最旧 created_at）。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 5: Embedder + 缓存。Mode: AFK | Role: TDD Builder。
> Goal: 向量通道基础设施，默认关闭（ADR-0017 决策 3）。
> Target Files: Modify `llm/client.go`（Embedder 接口 + Client.Embed + Config 四字段 + ErrEmbedderNotConfigured；embed 请求复用 c.http 与 maxResponseBytes 纪律）, Modify `retrieval/index.go`（embedTexts：缓存→Embedder→落库；并发 semaphore=4；ErrEmbeddingUnavailable 转换在 Task 7 工具层，本任务只返回原始错误）, Test `llm/client_test.go` `retrieval/embed_test.go`。
> TDD Steps:
> 1. RED：`llm`——httptest 假端点验 `Embed` 正常解析/非 2xx 返回 APIError/未配置返回 ErrEmbedderNotConfigured/ctx 取消中断；`retrieval`——假 Embedder 计数验第二次同文本零调用（缓存命中）、容量上限触发 FIFO、TTL 过期重算。
> 2. 确认 RED。
> 3. GREEN。
> 4. 全绿 + `go vet ./llm/ ./retrieval/`。
> 5. REFACTOR：向量编解码（[]float32 ↔ BLOB little-endian）单一函数，禁散落。
> Return: Summary + 物理回执（exit code + 通过数）。"

**Step Breakdown:**
- [ ] Step 1: RED（llm + retrieval 两侧）
- [ ] Step 2: 确认 RED
- [ ] Step 3: GREEN
- [ ] Step 4: 全绿 + vet
- [ ] Step 5: Refactor
- [ ] Step 6: 物理证据门
- [ ] Step 7: 原子提交

---

### Task 6: 混合召回（FTS5 bm25 + vec0 KNN + RRF + token 预算截断） [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Create: `retrieval/search.go`（`Search` + 融合 + 截断）
- Test: `retrieval/search_test.go`

**Interfaces:**
- Consumes: `Open` / `Rebuild` / `embedTexts`（Tasks 3-5）
- Produces:
  - `type Hit struct { ChunkID int64; DocID, Source string; Span [2]int; Score float64; Text string }`
  - `type SearchResult struct { Hits []Hit; Mode string; Truncated bool }`（Mode ∈ {"lexical","hybrid"}）
  - `func (idx *Index) Search(ctx context.Context, query string, tokenBudget int) (SearchResult, error)`
  - `var ErrCorpusStale = errors.New("retrieval: corpus version mismatch, rebuild required")`
  - `var ErrIndexMissing = errors.New("retrieval: index not built")`

**Injected Instincts:**
- [ ] `[Instinct: No-Silent-Degrade]`: 无 Embedder → Mode="lexical"；Embedder 调用失败 → 本次 hybrid 尝试失败返回 ErrEmbeddingUnavailable（Task 7 转可重试），**不得**偷偷返回 lexical 结果。
- [ ] `[Instinct: Budget-Tail]`: 截断只从尾部低分 Hit 丢弃；Truncated=true 必达。
- [ ] `[Instinct: Corpus-Check]`: 新鲜度检查与搜索解耦——`CheckFresh(ctx, corpusDir)` 由工具层（Task 7）在 Search 前调用，Search 本身只验索引非空（空 → ErrIndexMissing）。Search 内不做实时全量内容哈希（成本过高）。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 6: 混合召回。Mode: AFK | Role: TDD Builder。
> Goal: lexical 默认 + hybrid 可选，span 引用与 mode 标注（ADR-0017 决策 3/4/5）。
> Target Files: Create `retrieval/search.go`（Hit/SearchResult/Search/CheckFresh + ErrCorpusStale/ErrIndexMissing）, Test `retrieval/search_test.go`。
> TDD Steps:
> 1. RED：无 Embedder 时 Search 返回 Mode=lexical 且 bm25 有序；有假 Embedder 时 Mode=hybrid 且两路结果经 RRF 融合（构造词法命中与向量命中不同 chunk 的场景验证融合排序）；tokenBudget=0 或极小 → Truncated=true 且保留头部；空索引 → ErrIndexMissing；`CheckFresh`：语料改动后返回 ErrCorpusStale、未改动返回 nil。
> 2. 确认 RED。
> 3. GREEN：词法 = `SELECT ... FROM doc_fts ORDER BY bm25(doc_fts) LIMIT k`；向量 = vec0 KNN `ORDER BY embedding MATCH ? LIMIT k`（Embedder 批量 embed 查询串）；RRF：`score = Σ 1/(k+rank)`，k=60，词法/向量权重各 0.5 可配（包级常量+注释）。
> 4. 全绿 + `-race`。
> 5. REFACTOR：两路召回为两个私有函数，融合单独函数；token 估算 = rune 数/2 近似（注释说明）。
> Return: Summary + 物理回执（exit code + 通过数）。"

**Step Breakdown:**
- [ ] Step 1: RED（lexical/hybrid/截断/哨兵/新鲜度）
- [ ] Step 2: 确认 RED
- [ ] Step 3: GREEN
- [ ] Step 4: `-race` 全绿
- [ ] Step 5: Refactor
- [ ] Step 6: 物理证据门
- [ ] Step 7: 原子提交

---

### Task 7: SearchTool（tool.Tool 实现 + span 契约 + 哨兵分类） [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Create: `retrieval/tool.go`（`SearchTool(idx, corpusDir)` + 参数校验 + 错误分类）
- Test: `retrieval/tool_test.go`

**Interfaces:**
- Consumes: `agent/tool.Tool` 接口（Task 1 后签名：`Execute(ctx, args) (string, error)`）；`idx.Search` / `idx.CheckFresh`
- Produces:
  - `func SearchTool(idx *Index, corpusDir string) tool.Tool`
  - `var ErrEmbeddingUnavailable = errors.New("retrieval: embedding channel unavailable")`（实现 `Retryable() bool { return true }`）
  - 返回 JSON：`{"mode":...,"truncated":...,"hits":[{"chunk_id","doc_id","span":[s,e],"score","text"}]}`；span 缺失即返回参数错误（`errors.New`，non-retryable）
  - 入参 schema：`{"query": string 必填, "top_k": int 1..10 默认 5, "token_budget": int 默认 1500}`

**Injected Instincts:**
- [ ] `[Instinct: Retryable-Contract]`: ErrIndexMissing/ErrCorpusStale non-retryable；ErrEmbeddingUnavailable retryable（供 Task 1 的 Run 分支）。
- [ ] `[Instinct: Trace-Visibility]`: 工具成功返回的 JSON 由 Run 循环 emit KindToolResult 天然入 trace（mode/truncated 在内）。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 7: SearchTool。Mode: AFK | Role: TDD Builder。
> Goal: 把召回包成模型可调用的工具，span 硬契约（ADR-0017 决策 4）。
> Target Files: Create `retrieval/tool.go`（SearchTool + 入参校验 + 三哨兵 + ErrEmbeddingUnavailable 实现 tool.RetryableError）, Test `retrieval/tool_test.go`。
> TDD Steps:
> 1. RED：Spec() 的 Function.Name=="search_docs" 且 Parameters 含 query/top_k/token_budget；缺 query → non-retryable 参数错误；CheckFresh 失败 → ErrCorpusStale 且 `errors.As` 不到 RetryableError；Embedder 故障路径 → ErrEmbeddingUnavailable 且 Retryable()==true；正常路径 JSON 含全部字段且 hits 按分数降序。
> 2. 确认 RED。
> 3. GREEN。
> 4. 全绿。
> 5. REFACTOR：入参解析 guard clause 化；错误文案面向模型可自纠（如 'index missing: run go run . -index <dir>'）。
> Return: Summary + 物理回执（exit code + 通过数）。"

**Step Breakdown:**
- [ ] Step 1: RED（Spec/参数/三哨兵/正常路径）
- [ ] Step 2: 确认 RED
- [ ] Step 3: GREEN
- [ ] Step 4: 全绿
- [ ] Step 5: Refactor
- [ ] Step 6: 物理证据门
- [ ] Step 7: 原子提交

---

### Task 8: 装配（-index 子命令 + Pool 注入 + 终态分类补 index missing） [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Modify: `main.go`（flag 分支：`-index <dir>` 构建后退出 0；`-eval` 由 Task 11 加；启动路径按 conf.DbPath 开 retrieval；检索索引与 store 共享 db_path 文件）
- Modify: `worker/pool.go`（`NewPool` 增加可选检索工具注入——`NewPool(size, timeout, s, client, extraTools ...tool.Tool)` 或构造后 `RegisterTool`；选后者：`func (p *Pool) RegisterTool(t tool.Tool)`，在 Start 前调用）
- Modify: `worker/pool.go:205-218`（终态 switch 增加 `errors.Is(err, retrieval.ErrIndexMissing) → "index missing"`）
- Test: `main_test.go`（`-index` 子命令冒烟）、`worker/pool_test.go`（RegisterTool + index missing 终态分类）

**Interfaces:**
- Consumes: `retrieval.Open` / `retrieval.Index.Rebuild` / `retrieval.SearchTool`；`tool.DefaultRegistry()`
- Produces:
  - `func (p *Pool) RegisterTool(t tool.Tool)`（幂等；Start 后调用返回错误——编译期用注释+运行期 log）
  - 终态分类新增 `"index missing"` 分支

**Injected Instincts:**
- [ ] `[Instinct: Assembly-Only]`: main 只装配不写逻辑；retrieval 的 Open 失败 = 启动失败退出码 1（显式失败不静默）。
- [ ] `[Instinct: Dependency-Direction]`: worker → retrieval 合法（产品层消费中间层）；api 不得经任何路径依赖 retrieval。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 8: 装配。Mode: AFK | Role: TDD Builder。
> Goal: -index 命令 + search_docs 进 pool + index missing 独立终态（ADR-0017 决策 6）。
> Target Files: Modify `main.go`（flag.Parse；`-index <dir>` → Open+Rebuild+结构化摘要日志+退出；正常路径：conf.DbPath!="" 时 retrieval.Open(conf.DbPath, Options{Embedder: 按 conf 构造 llm embedder 或 nil}) + Rebuild + p.RegisterTool(retrieval.SearchTool(...))；语料目录 = `-corpus <dir>` flag 默认 `docs`）, Modify `worker/pool.go`（RegisterTool + 终态 switch 加 index missing 分支）, Test `main_test.go` `worker/pool_test.go`。
> TDD Steps:
> 1. RED：`pool_test.go`——RegisterTool 后假 Chatter 收到 search_docs spec；任务因 ErrIndexMissing 失败时 task.Error=="index missing"（经 errors.Is 匹配，import retrieval 于测试文件允许——测试不属生产门禁范围？否：门禁扫生产码，worker 生产码 import retrieval 合法；测试同）。
> 2. 确认 RED。
> 3. GREEN。
> 4. 全绿 + `go build ./... && go vet ./...`。
> 5. REFACTOR：main.go run() 控制流 ≤2 层；embedder 构造抽小函数。
> Return: Summary + 物理回执（exit code + 通过数 + `go run . -index docs` 实跑输出）。"

**Step Breakdown:**
- [ ] Step 1: RED（RegisterTool + 终态分类）
- [ ] Step 2: 确认 RED
- [ ] Step 3: GREEN
- [ ] Step 4: 全绿 + build/vet + `-index` 实跑
- [ ] Step 5: Refactor
- [ ] Step 6: 物理证据门
- [ ] Step 7: 原子提交

---

### Task 9: eval 包骨架 + grader（span 程序判定 + 边界案例） [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Create: `eval/task.go`（任务集 YAML schema + 解析 + 校验）
- Create: `eval/grader.go`（引用命中判定 + 首错三分类）
- Create: `eval/grader_test.go`、`eval/task_test.go`

**Interfaces:**
- Consumes: `agent.TraceStep`（trace 快照）；任务集 YAML 结构（ADR-0018 决策 1）
- Produces:
  - `type Task struct { ID, Prompt, Scope string; ExpectDocs []string; ExpectSpans [][2]int }`
  - `func LoadTasks(path string) ([]Task, error)`（schema 校验：scope ∈ {in-corpus,cross-chunk,out-of-corpus}；expect 缺失即非法）
  - `type Grade struct { Success bool; SpanRecall float64; FirstError string }`（FirstError ∈ {"", "no-search","wrong-span","right-span-wrong-reasoning"}）
  - `func Grade(task Task, trace []agent.TraceStep, answer string) Grade`
  - span 计分：区间交集/并集；部分相交按 0.5 权重计入（边界案例单测覆盖）

**Injected Instincts:**
- [ ] `[Instinct: Single-Truth]`: grader 是引用命中的唯一判定实现（ADR-0018 决策 2）；禁第二套口径。
- [ ] `[Instinct: No-LLM-Judge]`: 判定只用程序规则；结论正确性暂由任务集 expect_docs 命中近似（out-of-corpus 题期望无引用），**生成质量不在本 harness 判定范围**（ADR-0018 非目标）。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 9: eval grader。Mode: AFK | Role: TDD Builder。
> Goal: 程序判定器，禁 LLM 裁判（ADR-0018 决策 2）。
> Target Files: Create `eval/task.go` `eval/grader.go` + 两测试。
> TDD Steps:
> 1. RED：`task_test.go`——合法集解析；非法 scope/缺 expect 报错。`grader_test.go`——精确命中 recall=1；部分相交 recall=0.5*|交|/|并| 合并；trace 无 tool_call(search_docs) → FirstError=no-search；有调用但 span 全不交 → wrong-span；span 命中但 answer 为空/不含期望结论标记 → right-span-wrong-reasoning。
> 2. 确认 RED。
> 3. GREEN。
> 4. 全绿。
> 5. REFACTOR：区间运算纯函数；trace 解析用单次遍历。
> Return: Summary + 物理回执（exit code + 通过数）。"

**Step Breakdown:**
- [ ] Step 1: RED（schema + grader 五场景）
- [ ] Step 2: 确认 RED
- [ ] Step 3: GREEN
- [ ] Step 4: 全绿
- [ ] Step 5: Refactor
- [ ] Step 6: 物理证据门
- [ ] Step 7: 原子提交

---

### Batch 10 [Tier: 1-Light]: 任务集 v1 入仓 [Mode: HITL] [Role: Solo Builder + 人工核定]

**Files:**
- Create: `eval/tasks/retrieval_v1.yaml`（20-50 条：≥40% in-corpus、≥30% cross-chunk、≥15% out-of-corpus；语料 = 仓库 `docs/` + `README.md`）
- Create: `eval/tasks/README.md`（构造纪律：先入仓后跑、禁跑完挑题、期望 span 人工核定记录）

**Interfaces:**
- Consumes: `eval.LoadTasks` schema（Task 9）
- Produces: 合法任务集文件（`go test ./eval/ -run TestLoadTasks_RealSet` 解析通过）

**Injected Instincts:**
- [ ] `[Instinct: Pre-Committed-Set]`: 任务集先于检索实现入仓；本任务 HITL——期望 span 由人工（用户）抽查核定 ≥10 条。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Batch 10: 任务集 v1。Mode: HITL | Role: Solo Builder。
> Goal: ADR-0018 决策 1 的防共谋任务集。
> Target Files: Create `eval/tasks/retrieval_v1.yaml`（20-50 条按上述配比）, Create `eval/tasks/README.md`。
> Steps:
> 1. 通读 docs/ 全部 ADR 选材（覆盖 0004/0010/0013/0014/0015/0017/0018 的确定性事实）。
> 2. 写题：直答（in-corpus）/ 跨 ADR 综合（cross-chunk）/ 语料外事实（out-of-corpus，期望模型不编造——expect_spans 空且 FirstError 判定走 answer 不含伪造引用）。
> 3. 期望 span 按行号区间人工核定（用户抽查 ≥10 条）。
> 4. `go test ./eval/ -run TestLoadTasks_RealSet -count=1` 绿（该测试读真实文件解析——若 Task 9 未含则补一个）。
> Return: Summary + 物理回执（yaml 行数 + grep -c 'id:' 计数 + 测试 exit code）。"

**Step Breakdown:**
- [ ] Step 1: 选材通读
- [ ] Step 2: 写题 + 期望 span 人工核定（HITL）
- [ ] Step 3: 解析测试绿
- [ ] Step 4: 物理证据门
- [ ] Step 5: 原子提交

---

### Task 11: eval run 命令（N 重复 + 报告 + 首错归因汇总） [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Create: `eval/run.go`（`Run(ctx, cfg RunConfig) (Report, error)`：逐任务过 `agent.Run` + grader + 统计）
- Create: `eval/report.go`（markdown + JSON 落盘 `docs/eval/`）
- Modify: `main.go`（`-eval` flag 分支：`-set` `-repeat` `-group` `-corpus`）
- Test: `eval/run_test.go`（假 Chatter + 假工具链跑 2 条任务 × N=2，验分布字段与报告文件生成）

**Interfaces:**
- Consumes: `agent.Run`；`tool.Registry`（含/不含 search_docs 由 group 决定）；`eval.LoadTasks`；`eval.Grade`
- Produces:
  - `type RunConfig struct { SetPath, CorpusDir, Group string; Repeat int; OutDir string }`
  - `type Report struct { Group string; Repeat int; TaskResults []TaskResult; Aggregates map[string]any }`
  - `func Run(ctx context.Context, cfg RunConfig) (Report, error)`
  - 报告固定节（ADR-0018 决策 4）：任务集 hash/环境（模型/seed/模板版本/mode）/六指标对照表/逐任务明细/首错 Top3/结论段
  - 退出码：0 = 跑完（不论业务结论）；非 0 = harness 自身故障

**Injected Instincts:**
- [ ] `[Instinct: Paired-Control]`: group ∈ {baseline,lexical,hybrid} 决定 registry 是否含 search_docs + embedder 有无；模板版本与 seed 从 RunConfig 单处读取。
- [ ] `[Instinct: Distribution]`: 聚合必出中位数 + P25-P75；单次值只进明细。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 11: eval run。Mode: AFK | Role: TDD Builder。
> Goal: 一条命令跑对照并产出可审计报表（ADR-0018 决策 1/3/4）。
> Target Files: Create `eval/run.go` `eval/report.go`, Modify `main.go`（-eval 分支）, Test `eval/run_test.go`。
> TDD Steps:
> 1. RED：`run_test.go`——2 任务 × repeat=2 的假链路（假 Chatter 返回含 tool_call 的 turn 序列）；断言 Report 含 repeat=2、聚合含 median/p25/p75、docs/eval 临时目录生成 md+json、group=baseline 时 registry 无 search_docs。
> 2. 确认 RED。
> 3. GREEN。
> 4. 全绿 + build/vet。
> 5. REFACTOR：main.go -eval 分支 ≤10 行（转 eval.Run）。
> Return: Summary + 物理回执（exit code + 通过数 + 一次 -eval 实跑产物路径）。"

**Step Breakdown:**
- [ ] Step 1: RED（假链路 + 分布 + 报告 + group 语义）
- [ ] Step 2: 确认 RED
- [ ] Step 3: GREEN
- [ ] Step 4: 全绿 + build/vet + 实跑
- [ ] Step 5: Refactor
- [ ] Step 6: 物理证据门
- [ ] Step 7: 原子提交

---

### Task 12: 全量回归 + 基线对照评测 + 合入判定 [Tier: 3-Critical] [Mode: HITL] [Role: Solo + 人工判定]

**Files:**
- Modify: `docs/eval/`（产出报告）；`WORKMEMORY/work.log`（WORK_END 事件）；`PROJECT_OVERVIEW.md`（结果回写）

**Interfaces:**
- Consumes: Tasks 1-11 全部产物
- Produces: 对照表（baseline vs lexical）+ 合入/不合入结论

**Injected Instincts:**
- [ ] `[Instinct: Physical-Gate]`: `GOWORK=off go test -race ./... -count=1` + `go build ./...` + `go vet ./...` + 架构门禁 + pre-commit 五道全绿才准入评测。
- [ ] `[Instinct: Reverse-Killswitch]`: 判定规则 = ADR-0018 决策 3 阈值（lexical 中位数提升 ≤ ±2/|set| 即无提升 → 不合入，work.log 记录决定与数字）。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 12: 回归 + 对照 + 判定。Mode: HITL | Role: Solo + 人工。
> Goal: 出数字、做判定、收口或收尸（ADR-0017 DoD / ADR-0018 反向判据）。
> Steps:
> 1. `GOWORK=off go test -race ./... -count=1`（102+ 测试全绿基线对照）。
> 2. `go build ./... && go vet ./...`。
> 3. `go run . -index docs` 建索引；`go run . -eval -group baseline -repeat 3 -set eval/tasks/retrieval_v1.yaml`；同参 `-group lexical`。
> 4. 汇总对照表；按阈值判定；报告入 docs/eval/。
> 5. work.log WORK_END + PROJECT_OVERVIEW 回写。
> Return: Summary + 物理回执（各命令 exit code + 两报告中位数表）。"

**Step Breakdown:**
- [ ] Step 1: 五道物理门
- [ ] Step 2: baseline 组跑测
- [ ] Step 3: lexical 组跑测
- [ ] Step 4: 对照表 + 判定（HITL）
- [ ] Step 5: 回写 + WORK_END 事件
- [ ] Step 6: PR 合回 master（经用户批准）

---

## [NOTE] Fog of War (Not Yet Specified)

- **[hybrid 组实测]**：无 embedding 凭证，hybrid 组只能待凭证到位后跑。Trigger：用户提供 OpenAI 兼容 embed 凭证（EMBED_BASE_URL/EMBED_API_KEY/EMBED_MODEL/EMBED_DIM）。
- **[语料目录默认值]**：`-corpus` 默认 `docs` 是否够用（README 是否纳入）。Trigger：baseline 组 out-of-corpus 题误判率 >20%。
- **[demo.html 检索步骤展示]**：终态通过后再谈 SSE 展示 mode/命中。Trigger：Task 12 判定合入。
- **[任务集规模终值]**：20 还是 50 条。Trigger：首轮 20 条噪声带过宽（提升判定不稳定）。

## [NOTE] Deferred Gap Register

| gap | why deferred | trigger that forces revisiting | owner |
| --- | --- | --- | --- |
| hybrid 向量通道无实测 | 无凭证，开箱即挂属如实边界（ADR-0017 决策 3） | 用户提供 embed 凭证 | user |
| 增量索引 | 首版全量重建可接受（ADR-0017 决策 6 已写明限制） | 全量重建耗时 >5min（语料增长） | implementer |
| RRF 权重/k=60 调优 | 默认值未经验证 | lexical 基线报告出炉后 cross-chunk 命中率 <50% | implementer |
| rerank 独立服务 | ADR-0017 非目标 | hybrid 组 P95 劣化且 wrong-span 集中于同源文档 | 需 ADR 修订 |
| demo.html 展示 mode/truncated | 先出数字再谈展示 | Task 12 合入判定通过 | implementer |
| deepseek-chat→flash 迁移 | 用户已自行完成 | 无需 revisit | — |

## [NOTE] Out of Scope

- 分布式检索 / Kafka / Redis / 向量数据库服务（ADR-0004）。
- Web 前端与多模态检索；rerank 独立服务化；Agent 编排框架。
- 多轮对话评测与用户模拟器仿真（vault 重型规则全貌，ADR-0018 非目标）。
- 增量索引；llm 生成质量本身评测。

## [NOTE] Deviation Table (post-execution, filled before merge)

| plan text | what was actually built | why |
| --- | --- | --- |
| Task 1「指数退避 200ms→400ms」 | 单档 200ms 常量（`engine.go:19-21`，注释说明） | 一次重试策略下第二档不可达，补阶梯即死代码（YAGNI）；ADR-0017 决策 5 的「200ms/400ms」为阶梯形状描述，不回改已 accepted 决策 |
| Task 1 Files 清单未列 `calc_test.go`/`time_test.go` | 连带适配 `context.Background()` + import | `Tool.Execute` 签名变更的机械连带，零断言变更，最小必要 |
| Task 1 未声明 fakeChatter 变更 | `ChatWithTools` 增加 ctx 感知（镜像真实 `llm.Client` 行为） | 假件比真件更宽容是假绿源；且是 `TestRun_RetryBackoffRespectsCtx` 的承重墙 |
| Task 1 计划外测试缺口（CRV 黄卡 1） | 未补：`isRetryable` 的 `&& re.Retryable()` 守卫无测试捕获 | orchestrator 突变实证（删守卫 exit 0 不变红）；风险低（守卫存在且有 retryable 路径测试兜底），**trigger：任何触碰 `isRetryable`/`RetryableError` 的改动前必须补「实现 RetryableError 但 Retryable()=false」用例** |
| Task 1 `Registry.Execute` 无生产调用方 | 按计划保留（测试引用） | 计划 Produces 明确要求；**trigger：若 Task 7/8 后仍无调用方，在 ADR-0017 补注保留理由防误删** |
| Task 1 注释「错误全文只进 slog」 | agent 包无 slog，实际只进喂回模型的消息 | 红线未违反（trace 干净）；**trigger：Task 8 装配层统一补工具错误结构化 slog 时修正注释** |
| Task 3 Options.Embedder 类型 | `EmbedderFunc func(ctx, texts) ([][]float32, error)` 占位（llm.Embedder 尚不存在） | brief 设计的占位方案；**trigger：Task 5 brief 必须携带「替换为 llm.Embedder」提示（CRV 黄卡 2）** |
| Task 3 门禁豁免自身 | architecture_test.go 自引用豁免 | 断言文本必含被禁串，不自豁免即假红；orchestrator 突变实证：豁免扩大化后门禁静默 PASS（exit 0）——**trigger：任何修改 architecture_test.go 的任务必须同时补 `scanned==0` 计数守卫（CRV 黄卡 1）** |
| Task 3 未断言 PRAGMA/分词细节 | busy_timeout=5000/trigram/CHECK(singleton=1) 无测试断言 | 核心行为（schema 存在性/vec0 条件）已覆盖；**trigger：检索行为异常时先查 PRAGMA 与分词器配置（CRV 黄卡 3）** |
| Task 3 门禁仅查 import 串 | 不查符号级引用（`worker.` 形式） | 镜像 api 基准同款原理性局限；**trigger：出现经变量/接口间接引用上层包的规避手法时升级门禁（CRV 黄卡 4）** |
| Task 3 vec blank import | `_ "modernc.org/sqlite/vec"` 超出 brief import 白名单 | vec0 不注册则 DDL 必失败（变异实证），同模块子包 go.mod 零变化（tidy 实证）；非可选美化 |
| Task 4 bigram 预切接入 | 未接入：Rebuild 插原文；`bigramSplit` 保留+单测但生产路径不用 | ADR-0017 决策 3 机制失效——trigram 下 bigram 预切产生空索引（CPE 实证 + `TestFTS_BigramSplitProducesEmptyIndex` 固化）；ADR-0017 已补修订注记（黄卡 1 要求）；**trigger：modernc trigram 行为变化或换 CJK 分词策略时重估；2 字查询策略归 Task 6 brief** |
| Task 4 unreadable 双 build-tag 测试文件 | unreadable_windows_test.go / unreadable_unix_test.go（计划清单外） | 目录型文件方案触发不了 DocsSkipped 路径（listDocs 非常规过滤）；最小跨平台手段，零生产码 |
| Task 4 clearIndex 不清 doc_vec | 未改（Task 5 brief 预授权） | EmbedDim=0 无运行时危害；hybrid 接入（Task 5）后旧 vec 行成孤儿——**trigger：Task 5 brief 必须携带 clearIndex 增加条件 DELETE FROM doc_vec 的授权（CRV 黄卡 2）** |
| Task 4 事务边界无断言 | 「巨型单事务」变异无测试捕获 | 「永不提交」有捕获；**trigger：引入可注入事务计数器时补批量断言；unix 助手加 Geteuid==0 skip 守卫（CRV 黄卡 4）** |
| Task 5 Options.EmbedModel 字段 | 新增（预授权清单外） | 缓存键强制含 model 而 Embedder 接口无 Model()；比改接口更小（CRV 判定最小必要）；Task 8 装配从同一 config 分送 |
| Task 5 TTL 惰性删除无独立断言 | 未补 | 过期行永不被服务 + 容量 evict 兜底；**trigger：触碰 embed.go 缓存逻辑时补 COUNT(*) 断言（CRV 黄卡 1）** |
| Task 5 FIFO 秒级并列 | 未改 | created_at 秒级精度同秒并列任取，有界性不受影响；**trigger：缓存淘汰行为异常时引入单调序号列（CRV 黄卡 2）** |
| Task 6 hybrid 写入时机 | 事务外批量嵌入 → 事务内写 doc_vec（原计划"事务内调 embedTexts"） | 实测写事务内调 embedTexts 必 SQLITE_BUSY（embed_cache 另连接抢 WAL 写锁）；原子性不变量（doc_chunk/doc_fts/doc_vec 同事务）保持，缓存非不变量成员 |
| Task 6 查询形态不对称 | 词法 JOIN doc_chunk / 向量两步 KNN | 原理性强制：vec0 的 LIMIT/k 必须绑定 KNN 扫描本身（JOIN 形态驱动报错）；词法改两步是性能换装饰性对称（YAGNI） |
| Task 6 tokenBudget=0 语义 | 保留最高分一条 + Truncated=true | 计划 RED 步骤原文"保留头部"即此语义（CRV 裁定计划合规）；优于空集（"无命中"与"零预算"不可区分） |
| Task 6 Hit.Score 三量纲 | 注释说明（bm25 负/vec0 距离/RRF 融合分） | 导出字段跨模式不可比；**trigger：Task 7 出口只序列化融合分口径（CRV 黄卡）** |
| Task 5 gofmt 遗留 | orchestrator 跑 gofmt -w retrieval/embed_test.go 修复 | Task 5 CPE 报告的 TestVecCodec_RoundTrip doc comment 缩进（Go 1.19+ 规则），零逻辑变更 |
| Task 7 ErrEmbeddingUnavailable 形态 | plain 哨兵 + 包装类型（Retryable()=true，Unwrap 双链） | 计划字面"var+实现 RetryableError"物理不可两全（plain error 值挂不了方法）；双链同时满足 errors.Is 与 errors.As |
| Task 7 top_k 落地层 | renderResult 截断（hits[:topK] 保头部，并入 truncated） | search.go:32 注释明言"最终 top_k 由工具层决定"；与 schema 承诺一致 |
| Task 7 分类表 ctx 分支 | 新增（计划字面表未列） | 防 hybrid 取消被误包装为可重试（取消语义分层红线）；TestClassifySearchError_CanceledCtxNeverRetryable 承重 |
| Task 7 错误文案自纠延迟 | ErrIndexMissing/ErrCorpusStale 原样返回，未加 "run go run . -index" 文案 | -index CLI 属 Task 8；现在写死命令文案会指向不存在的入口——**trigger：Task 8 落地 -index 子命令时同步补文案（CRV 黄卡 4）** |
| Task 7 FTS5 元字符未净化 | 未修（hybrid 下语法错误误分类为可重试） | 当前 lexical 模式 non-retryable 原样返回正确；误分类仅 hybrid 可达——**trigger：Task 12 hybrid 组开跑前必修（SearchLexical phrase 包装或 classify 增 fts5 syntax error 分支）（CRV 黄卡 3）** |
| Task 7 schema 数值无断言 | 未补（default/maximum 靠行为测试间接兜底） | **trigger：触碰 tool.go schema 时常量改错需测试捕获（CRV 黄卡 1）** |
| Task 7 变异自查两假绿 | 修正后复杀（builtLexicalTool 建真实索引；'3'+'字符'双 token 断言） | CPE 自查发现：参数校验测试被 ErrIndexMissing 掩盖、变异注释含 '3' 污染断言靶标；work.log 将记 PITFALL |
| Task 8 buildIndex 签名 | (conf config.Config, corpusDir)——遵测试先行 | 第一任 CPE 已写测试固定签名；另抽 retrievalOptions helper + Pool.started atomic.Bool 为最小必要 |
| Task 8 RegisterTool post-Start warn 无测试 | 未补 | **trigger：Task 12 合入门前补最小用例（post-Start 注册不 panic 且 spec 仍注入）（CRV 黄卡 1）** |
| Task 8 -index flag 接线层无测试 | 未补 | indexRequested/空值 exit 2/Parse 错误 exit 2/密钥校验前短路均只间接口入——**trigger：Task 12 前补 flag 层冒烟（CRV 黄卡 2）** |
| Task 8 幂等断言深度不足 | 未补（只断言 exit 0） | **trigger：Task 12 前补 DocsIndexed==0/ChunksWritten==0/Fingerprint 不变三断言（CRV 黄卡 3）** |
| Task 8 默认配置 -index 报 :memory: 易失索引 | 未修（计划书面认可的 DbPath 空 → :memory: 设计后果） | exit 0 + {docs:N} 摘要令运维误以为已持久化——**trigger：Task 12 HITL 前必须补 slog.Warn 明示易失或在 README 写明（CRV 黄卡 4，误导性成功报告，acceptance gate 3 近亲）** |
| Task 1 engine.go:32 注释「错误全文只进 slog」 | 未修正（Task 8 未执行该 trigger） | agent 包无 slog；**trigger：Task 12 前修正注释措辞（Task 1 偏差表登记项）** |
