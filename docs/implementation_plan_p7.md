# P7 Transactional Task Queue & Token Streaming Inference Implementation Plan

> **Goal**: 消除内存通道背压丢单瓶颈（构建基于 SQLite WAL 的事务性任务队列与原子抢占调度），并引入原生 SSE 打字机 Token 流式推断与级联广播。  
> **Tech Stack**: Go 1.24+ · modernc.org/sqlite (WAL 模式) · net/http · bufio · sync/atomic  
> **Spec Reference**: `docs/specs/2026-10-25-p7-transactional-queue-and-streaming-inference-design.md`, `docs/adr/0019-transactional-task-queue-and-streaming-inference.md`  
> **Global Constraints**: 强制 `GOWORK=off`；全仓 AST DAG 拓扑白名单守卫（`architecture_test.go`）；生产文件 ≤ 400 行、测试文件 ≤ 800 行；零新增外部第三方依赖；全量通过 `go test -race`。  
> **Branch**: `feat/p7-queue-and-streaming` — opened from `master` before Task 1, landed via PR.

---

## [NOTE] Decisions So Far

- [ADR-0002](docs/adr/0002-async-task-worker-pool-architecture.md): 建立异步任务 + Worker Pool 原生并发架构。
- [ADR-0010](docs/adr/0010-persistence-sqlite.md): 引入 `modernc.org/sqlite` 作为唯一可选持久化驱动。
- [ADR-0013](docs/adr/0013-decouple-agent-kernel-and-dependency-inversion.md): 抽取 `agent/` 内核与 `agent/tool/` 工具系统，解耦并发池。
- [ADR-0016](docs/adr/0016-sse-task-progress-stream.md): 实现基于 `TraceBuffer` 广播的步骤级 SSE (`GET /tasks/{id}/events`)。
- [ADR-0017 / 0018](docs/adr/0017-retrieval-as-tool.md): 混合检索工具化与受控评测 Harness，已于 P6 阶段全面闭环。
- [ADR-0019](docs/adr/0019-transactional-task-queue-and-streaming-inference.md): 决定放弃定长内存 channel，采用 SQLite WAL 事务性抢占调度；并引入 Token 级 SSE 打字机流式。

---

## [NOTE] Active Frontier (Unblocked Tasks)

### Task 1: Store 事务原子抢占接口与双实现 (`TaskStore.ClaimNext` & `PendingCount`) [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Modify: `store/store.go` (在 `TaskStore` 接口中增加 `ClaimNext`、`PendingCount`；为内存版 `Store` 实现 FIFO 抢占)
- Modify: `store/sql.go` (利用 SQLite `BEGIN IMMEDIATE` 事务实现排它性原子抢占：`SELECT ... LIMIT 1 + UPDATE`，及 `PendingCount`)
- Test: `store/store_test.go`, `store/sql_test.go`

**Interfaces:**
- Consumes: `sql.DB` transaction on SQLite WAL
- Produces:
  ```go
  var ErrNoTaskAvailable = errors.New("store: no pending task available")

  type TaskStore interface {
      // 既有方法保持不变...
      ClaimNext(ctx context.Context) (Task, error)
      PendingCount(ctx context.Context) (int64, error)
  }
  ```

**Injected Instincts:**
- [ ] `[Instinct: Concurrency]`: SQLite 事务抢占必须使用 `BEGIN IMMEDIATE`，并在抢占语句中使用 `WHERE id = ? AND status = 'pending'` 形成双重 CAS 保护。
- [ ] `[Instinct: Line-Budget]`: `store/sql.go` 严格控制在 400 行以内；若行数接近上限，按职责将 DDL 与队列表管理拆为 `store/sql_queue.go`。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 1: Store Transactional Claim & Queue Count (`TaskStore.ClaimNext` & `PendingCount`).
> Mode: AFK | Role: TDD Builder | Repo Invariants: Read `AGENTS.md` and `.agent-context.md`.
> Goal: Add `ClaimNext(ctx) (Task, error)` and `PendingCount(ctx) (int64, error)` to `TaskStore` interface and implement in both memory `Store` and `SQLStore`.
> Target Files: Modify `store/store.go`, `store/sql.go`; Test `store/store_test.go`, `store/sql_test.go`.
> TDD Steps:
> 1. RED: Write tests in `store/store_test.go` and `store/sql_test.go` testing that `ClaimNext` atomically transitions the oldest pending task to `running` and returns it. Test concurrency: 10 parallel goroutines claim 20 tasks, asserting every task is claimed exactly once with zero collisions and zero data races.
> 2. Run `GOWORK=off go test ./store/... -count=1` and verify RED.
> 3. GREEN: Implement `ClaimNext` and `PendingCount` in memory and SQLite. Return `ErrNoTaskAvailable` when no pending task exists.
> 4. Run `GOWORK=off go test -race ./store/... -count=1` and verify GREEN.
> 5. Verify file line budgets: `store/sql.go` and `store/store.go` <= 400 lines.
> Return: Summary with MANDATORY Physical Execution Receipt (exit code + stdout)."

**Step Breakdown:**
- [ ] **Step 1: Write failing tests in `store/store_test.go` and `store/sql_test.go` (RED)**
- [ ] **Step 2: Run test and verify it fails with expected message**
- [ ] **Step 3: Implement minimal production code in `store/store.go` and `store/sql.go` (GREEN)**
- [ ] **Step 4: Run tests and verify all green with `-race`**
- [ ] **Step 5: Refactor & check line budgets (`<= 400` lines)**
- [ ] **Step 6: Physical Evidence Gate**: Raw execution receipt
- [ ] **Step 7: Git atomic commit**

---

### Task 2: Worker 并发调度器重构（Push-Notify Pull-Worker 事务性调度） [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Modify: `worker/pool.go` (移除定长 `queue chan string`，引入容量为 1 的 `wakeUp chan struct{}` 信标；Worker 改用 `ClaimNext` 主动拉单；重写 `Enqueue` 与优雅停机排空逻辑)
- Modify: `config/config.go` (增加 `MaxQueueSize int`，默认 10000，环境变量 `MAX_QUEUE_SIZE` 支持)
- Test: `worker/pool_test.go`, `config/config_test.go`

**Interfaces:**
- Consumes: `store.TaskStore.ClaimNext`, `store.TaskStore.PendingCount`
- Produces:
  ```go
  // worker/pool.go
  type Pool struct {
      // 移除 queue chan string
      wakeUp chan struct{}
      // ...
  }
  func (p *Pool) Enqueue(id string) error // 异步非阻塞信标唤醒
  ```

**Injected Instincts:**
- [ ] `[Instinct: Backpressure-Smoothing]`: `Enqueue` 不再是固定 3 个 channel 的脆弱快速失败，而是只要当前排队数未超 `MaxQueueSize`，即在毫秒级将请求登记入队并唤醒 Worker。
- [ ] `[Instinct: Clean-Shutdown]`: 停机时在飞任务正常跑完，未执行的任务保留为 `pending`，由下次启动或存活实例无损接管，不再强制抹杀为 failed。
- [ ] `[Instinct: Line-Budget]`: `worker/pool.go` 保持 ≤ 400 行；调度轮询逻辑若较重可拆出 `worker/dispatcher.go`。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 2: Worker Dispatcher Refactoring to Transactional Push-Notify Pull Model.
> Mode: AFK | Role: TDD Builder | Repo Invariants: Read `AGENTS.md` and `.agent-context.md`.
> Goal: Replace memory channel `queue chan string` with `p.wakeUp chan struct{}` and `tasks.ClaimNext(ctx)` loop in `worker/pool.go`. Add `MaxQueueSize` in `config/config.go`.
> Target Files: Modify `worker/pool.go`, `config/config.go`; Test `worker/pool_test.go`, `config/config_test.go`.
> TDD Steps:
> 1. RED: In `worker/pool_test.go`, test submitting 50 concurrent tasks through `Enqueue`; assert all succeed (no ErrQueueFull) and workers process all 50 to completion. Test shutdown cleanly waits for in-flight tasks without wiping pending tasks.
> 2. Run `GOWORK=off go test ./worker/... ./config/... -count=1` and verify RED.
> 3. GREEN: Implement `wakeUp` channel and pull loop.
> 4. Run `GOWORK=off go test -race ./worker/... ./config/... -count=1` and verify GREEN.
> 5. Verify line budgets <= 400 lines and `architecture_test.go` DAG compliance.
> Return: Summary with MANDATORY Physical Execution Receipt."

**Step Breakdown:**
- [ ] **Step 1: Write failing tests in `worker/pool_test.go` (RED)**
- [ ] **Step 2: Run test and verify RED**
- [ ] **Step 3: Implement minimal production code in `worker/pool.go` and `config/config.go` (GREEN)**
- [ ] **Step 4: Run tests and verify all green with `-race`**
- [ ] **Step 5: Verify line budget and DAG imports**
- [ ] **Step 6: Physical Evidence Gate**
- [ ] **Step 7: Git atomic commit**

---

### Task 3: LLM 客户端原生流式推断支持 (`llm.ChatWithToolsStream`) [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Create: `llm/stream.go` (实现 `ChatWithToolsStream` 与 OpenAI/DeepSeek 兼容 SSE 帧逐行解析)
- Test: `llm/stream_test.go`

**Interfaces:**
- Consumes: OpenAI-compatible SSE text/event-stream (`data: {"choices":[{"delta":...}]}`)
- Produces:
  ```go
  // llm/stream.go
  type DeltaHandler func(delta string)

  type StreamingChatter interface {
      ChatWithToolsStream(ctx context.Context, msgs []Message, tools []Tool, onDelta DeltaHandler) (*AssistantTurn, error)
  }

  func (c *Client) ChatWithToolsStream(ctx context.Context, msgs []Message, tools []Tool, onDelta DeltaHandler) (*AssistantTurn, error)
  ```

**Injected Instincts:**
- [ ] `[Instinct: Stream-Robustness]`: 用 `bufio.Reader` 逐行扫描；遇到 `data: [DONE]` 优雅终结；按 `index` 精确合并工具调用参数；流式末尾通过 `stream_options` 解析 `usage`。
- [ ] `[Instinct: Zero-Framework]`: 使用标准库 `bufio` 和 `encoding/json`，严禁引入第三方流式客户端库。
- [ ] `[Instinct: Line-Budget]`: `llm/stream.go` 保持 ≤ 400 行；`llm/client.go` 保持原有 335 行不变。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 3: LLM Client Streaming Inference (`llm.ChatWithToolsStream`).
> Mode: AFK | Role: TDD Builder | Repo Invariants: Read `AGENTS.md` and `.agent-context.md`.
> Goal: Create `llm/stream.go` implementing `ChatWithToolsStream` using standard library `bufio.Reader` for OpenAI/DeepSeek SSE streams.
> Target Files: Create `llm/stream.go`, `llm/stream_test.go`.
> TDD Steps:
> 1. RED: Write tests in `llm/stream_test.go` using `httptest.Server` simulating chunked SSE streaming (`data: {"choices":[{"delta":{"content":"..."}}]}`). Test incremental `onDelta` calls, multi-chunk tool argument assembly, and `Usage` token extraction.
> 2. Run `GOWORK=off go test ./llm/... -count=1` and verify RED.
> 3. GREEN: Implement `ChatWithToolsStream` in `llm/stream.go`.
> 4. Run `GOWORK=off go test -race ./llm/... -count=1` and verify GREEN.
> 5. Verify `llm/stream.go` and `llm/client.go` <= 400 lines.
> Return: Summary with MANDATORY Physical Execution Receipt."

**Step Breakdown:**
- [ ] **Step 1: Write failing tests in `llm/stream_test.go` (RED)**
- [ ] **Step 2: Run test and verify RED**
- [ ] **Step 3: Implement minimal code in `llm/stream.go` (GREEN)**
- [ ] **Step 4: Run tests and verify all green with `-race`**
- [ ] **Step 5: Verify line budget and DAG imports**
- [ ] **Step 6: Physical Evidence Gate**
- [ ] **Step 7: Git atomic commit**

---

### Task 4: 认知内核与事件缓冲实时增量广播 (`agent.RunOptions` & `TraceBuffer.AppendDelta`) [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Modify: `agent/engine.go` (`RunOptions` 增加 `OnDelta func(step int, text string)`；当 `c` 实现了 `StreamingChatter` 时调用流式接口并在每步生成时上浮增量)
- Modify: `agent/trace.go` (`TraceBuffer` 广播事件增加 `DeltaEvent` 支持，并提供 `AppendDelta(step int, text string)`)
- Test: `agent/engine_test.go`, `agent/trace_test.go`

**Interfaces:**
- Consumes: `llm.StreamingChatter`
- Produces:
  ```go
  // agent/engine.go
  type RunOptions struct {
      SystemPrompt string
      OnUsage      func(llm.Usage)
      OnDelta      func(step int, text string)
  }

  // agent/trace.go
  type TraceEvent struct {
      IsDelta bool
      Step    int
      Text    string
      StepVal TraceStep // IsDelta == false 时有效
  }
  func (tb *TraceBuffer) AppendDelta(step int, text string)
  func (tb *TraceBuffer) SubscribeEvents() (snapshot []TraceStep, events <-chan TraceEvent, cancel func())
  ```

**Injected Instincts:**
- [ ] `[Instinct: Backward-Compatibility]`: 既有非流式 `Chatter` 运行不受任何影响；现有 `TraceBuffer.Subscribe()` 保持兼容，新增的 `SubscribeEvents` 供流式 SSE 消费。
- [ ] `[Instinct: Non-Blocking-Broadcast]`: `AppendDelta` 严格沿用非阻塞 `select + default` 投递，慢订阅者不拖累 Agent 认知决策。
- [ ] `[Instinct: Line-Budget]`: `agent/engine.go`（当前 166 行）与 `agent/trace.go`（当前 116 行）严格保持 ≤ 400 行。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 4: Agent Kernel Streaming Hook & TraceBuffer Delta Broadcast.
> Mode: AFK | Role: TDD Builder | Repo Invariants: Read `AGENTS.md` and `.agent-context.md`.
> Goal: Update `agent/engine.go` to support `RunOptions.OnDelta` when chatter implements `StreamingChatter`. Update `agent/trace.go` to support `AppendDelta` and event subscription.
> Target Files: Modify `agent/engine.go`, `agent/trace.go`; Test `agent/engine_test.go`, `agent/trace_test.go`.
> TDD Steps:
> 1. RED: Write tests verifying `agent.RunWithOptions` calls `OnDelta` per streamed chunk while still producing correct `TraceStep`. Test `TraceBuffer` broadcasting delta events to subscribers.
> 2. Run `GOWORK=off go test ./agent/... -count=1` and verify RED.
> 3. GREEN: Implement delta streaming hooks in `agent/engine.go` and `agent/trace.go`.
> 4. Run `GOWORK=off go test -race ./agent/... -count=1` and verify GREEN.
> 5. Verify line budgets <= 400 lines and DAG imports.
> Return: Summary with MANDATORY Physical Execution Receipt."

**Step Breakdown:**
- [ ] **Step 1: Write failing tests in `agent/engine_test.go` and `agent/trace_test.go` (RED)**
- [ ] **Step 2: Run test and verify RED**
- [ ] **Step 3: Implement minimal code in `agent/engine.go` and `agent/trace.go` (GREEN)**
- [ ] **Step 4: Run tests and verify all green with `-race`**
- [ ] **Step 5: Verify line budget**
- [ ] **Step 6: Physical Evidence Gate**
- [ ] **Step 7: Git atomic commit**

---

### Task 5: API 传输层打字机 SSE 推送与削峰背压集成 (`api/events.go` & `api/handler.go`) [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Modify: `api/events.go` (`streamEvents` 支持推送 `event: delta` 并调用 `w.(http.Flusher).Flush()`)
- Modify: `api/handler.go` (`HandleRun` 调用 `store.PendingCount(ctx)` 校验是否超出 `MaxQueueSize`，未超出时平滑入队并回 202)
- Test: `api/events_test.go`, `api/handler_test.go`, `api/integration_test.go`

**Interfaces:**
- Consumes: `agent.TraceEvent`, `store.TaskStore.PendingCount`
- Produces:
  ```text
  event: delta
  data: {"step":1,"text":"根据"}
  ```

**Injected Instincts:**
- [ ] `[Instinct: Flusher-Protection]`: 调用 `http.Flusher` 必须先作类型断言保护（测试中的非 Flusher mock 不崩溃）。
- [ ] `[Instinct: Snapshot-Self-Healing]`: 客户端重连时先发完整的 `event: step` 历史快照，再发后续的 `event: delta`，兼顾打字机呈现与状态一致性。
- [ ] `[Instinct: Line-Budget]`: `api/events.go`（当前 192 行）与 `api/handler.go`（当前 228 行）严格保持 ≤ 400 行。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 5: API Layer Streaming SSE Push & Backpressure Integration.
> Mode: AFK | Role: TDD Builder | Repo Invariants: Read `AGENTS.md` and `.agent-context.md`.
> Goal: Update `api/events.go` to push `event: delta` frames with immediate flush. Update `api/handler.go` to use `PendingCount` for queue capacity checks.
> Target Files: Modify `api/events.go`, `api/handler.go`; Test `api/events_test.go`, `api/handler_test.go`, `api/integration_test.go`.
> TDD Steps:
> 1. RED: In `api/events_test.go` and `api/integration_test.go`, test HTTP client connecting to `/tasks/{id}/events` receives `event: delta` frames in real-time before `event: step` and `event: done`. Test submitting requests up to capacity returns 202 without 503.
> 2. Run `GOWORK=off go test ./api/... -count=1` and verify RED.
> 3. GREEN: Implement delta frame streaming and backpressure check.
> 4. Run `GOWORK=off go test -race ./api/... -count=1` and verify GREEN.
> 5. Verify line budgets <= 400 lines and DAG imports.
> Return: Summary with MANDATORY Physical Execution Receipt."

**Step Breakdown:**
- [ ] **Step 1: Write failing tests in `api/events_test.go` and `api/integration_test.go` (RED)**
- [ ] **Step 2: Run test and verify RED**
- [ ] **Step 3: Implement minimal code in `api/events.go` and `api/handler.go` (GREEN)**
- [ ] **Step 4: Run tests and verify all green with `-race`**
- [ ] **Step 5: Verify line budget**
- [ ] **Step 6: Physical Evidence Gate**
- [ ] **Step 7: Git atomic commit**

---

### Batch 6 [Tier: 1-Light]: 门面文档、ADR-0019 状态、面试手册与 WORKMEMORY 物理刷新 [Mode: AFK] [Role: Solo Builder]

**Files:**
- Modify: `README.md` (更新系统架构图、事务队列削峰原理、SSE delta 协议说明、重算测试数与代码行数)
- Modify: `docs/adr/0019-transactional-task-queue-and-streaming-inference.md` (状态更新为已实施)
- Modify: `docs/interview/pitch-3min.md`, `interview-answers.md`, `project-qa.md` (更新 Q3 队列阻塞问题已彻底由 SQLite 事务抢占解决，增加 SSE 流式打字机高频问答)
- Modify: `.agent-context.md`, `WORKMEMORY/PROJECT_OVERVIEW.md`, `WORKMEMORY/work.log` (物理同步所有指标)

**Interfaces:**
- Consumes: Task 1–5 的真实测试数与代码统计
- Produces: 100% 物理对齐的文档资产

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Batch 6: Refresh Full Documentation, ADR-0019, Interview Manuals, and WORKMEMORY.
> Mode: AFK | Role: Solo Builder | Repo Invariants: Read `AGENTS.md` and `.agent-context.md`.
> Goal: Update `README.md`, `docs/adr/0019-*.md`, `docs/interview/*.md`, `.agent-context.md`, and `WORKMEMORY/` with P7 transactional queue and streaming inference capabilities.
> Verification: Recompute all numbers via terminal commands; run `GOWORK=off go test -race ./... -count=1`, `architecture_test.go`, and privacy grep."

**Step Breakdown:**
- [ ] **Step 1: Recompute exact test count and Go LOC via terminal**
- [ ] **Step 2: Refresh `README.md` and `docs/adr/0019-*.md`**
- [ ] **Step 3: Update `docs/interview/*.md` with P7 queue & streaming answers**
- [ ] **Step 4: Update `.agent-context.md` and `WORKMEMORY/`**
- [ ] **Step 5: Run full verification gates (`go test -race`, `TestArchitecture`, privacy grep, `pre-commit`) and commit**

---

## [NOTE] Fog of War (Not Yet Specified)

- **[分布式多节点选主与协调]**：当前单机 SQLite WAL 下的 `BEGIN IMMEDIATE` 事务抢占已足以支撑单进程 1000+ QPS 调度吞吐。若未来需要跨多服务器水平扩展，再评估轻量级基于 Raft 或数据库分布式租约机制，当前保持 YAGNI。

---

## [NOTE] Deferred Gap Register

| gap | why deferred | trigger that forces revisiting | owner |
| --- | --- | --- | --- |
| 历史任务 Token 级增量重放（Replay Granular Deltas） | 历史步骤在数据库与快照中已完整保存整步文本（`TraceStep`），在断网重连后快照对账已足够满足一致性，存储每一字 delta 会导致数据库体积膨胀 10 倍 | 客户端有精确录屏回放需求或审计监管要求记录击键级延迟时 | implementer |
| 基于优先级的任务插队调度（Priority Queue） | 当前业务场景均为同等优先级的异步 Agent 决策，严格 FIFO 队列满足全部场景 | 引入 VIP 用户分级或后台低优先级批量任务通道时 | implementer |

---

## [NOTE] Out of Scope

- 不引入外部独立消息队列服务（Redis、RabbitMQ、Kafka）。
- 不破坏现有 12 个 Go 包的 DAG 单向依赖结构。

---

## [NOTE] Deviation Table (post-execution, filled before merge)

| plan text | what was actually built | why |
| --- | --- | --- |
| Task 1–5 接口与架构实现 | 均严格按计划规范交付（TaskStore 事务抢占接口、Worker Push-Notify 唤醒池、llm/stream.go 流式解析、agent 内核增量钩子、api/events.go SSE 打字机分发） | 无偏差，所有接口方法签名、错误语义、并发安全与行数预算 100% 对齐规格 |
