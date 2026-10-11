# agent-api P2 — Agent Trace 观测（观测与传输解耦）实施计划

> **Goal**: 让客户端能观察到 agent 的 `think → tool_call → tool_result → final` 轨迹——通过在
> 现有 `runAgent` 增加 step 回调 + 进程内 ring buffer，并经现有 `GET /tasks/{id}` 新增 `steps` 字段暴露；
> 同时修复 ADR-0012 确认的 3 项既有缺陷。**不引入 SSE / 不改 `llm.Client` 契约 / 不改 store schema。**
> **Tech Stack**: Go 1.26.5（module `agent-api`）；标准库优先，唯一既有 opt-in 依赖 `modernc.org/sqlite`。
> **Spec Reference**: [ADR-0012](adr/0012-next-step-observation-transport-decoupling.md) ·
> 审讯证据 `docs/grill/adr-0012-sse-rejection/`。
> **Global Constraints**:
> - **零新依赖**（ADR-0004/0010）；不得引入 prometheus client 或第三方 ring buffer。
> - **不改 store 的 SQL schema**；trace 为**进程内**、观测用、非真相源（SQLite 模式下不持久化）。
> - **不改 `chatter` 接口**与 `llm.Client` 契约；仅扩展**私有** `runAgent` 签名。
> - **trace 的 `error` 步骤只存粗粒度分类**（`upstream error` / `max steps exceeded`），严禁回传上游响应体（对齐 `task.Error` 脱敏，ADR-0003）。
> - `api/demo.html` **零 innerHTML**（`textContent` / `createElement` / `append`）。
> - 每个任务：RED→GREEN→REFACTOR + **原始终端回执**（exit code/通过数/diff）+ 原子提交；
>   pre-commit 零泄漏钩子已启用，**禁止 `--no-verify`**。
> - worker 改动 **MUST** `go test -race ./worker/...` 通过。

---

## 🏛️ Decisions So Far

- [ADR-0012](adr/0012-next-step-observation-transport-decoupling.md)：**拒绝全量 SSE**，采「观测与传输解耦」窄口径 trace（本计划即其落地）。
- ADR-0005（**继续有效**）：维持轮询；客户端契约不变，`GET /tasks/{id}` 仍是唯一取数入口，仅**增字段**。
- ADR-0004 / ADR-0010：零新依赖；唯一第三方依赖 `modernc.org/sqlite` 且 opt-in。
- ADR-0003：错误只存粗粒度分类——trace 的 `error` 步骤沿用同一红线。
- ADR-0008：agent 循环对外契约不变——仅 `runAgent`（包内私有）签名追加回调参数。
- 环境事实：`chatter`（`worker/pool.go:17-20`）与 `TaskStore`（`store/store.go:27-32`）为唯一注入缝；
  `runAgent` 仅被 `worker/pool.go:107` 与 `worker/agent_test.go:51` 调用。

---

## 🎯 Active Frontier (Unblocked Tasks)

### 阶段 A：既有缺陷加固（Tasks 1–5，相互独立）

### Task 1: TaskStore 新增 Delete（接口 + 内存实现） [Tier: 2-Standard] [Mode: AFK] [Subagent: TDD Builder]

**Files:**
- Modify: `store/store.go:27-32`（接口）, `store/store.go`（新增 `(*Store).Delete`）
- Test: `store/store_test.go`

**Interfaces:**
- Consumes: `type TaskStore interface { Create; Update; Get; Complete }`
- Produces: `Delete(id string) error`（语义：存在则删除返回 nil；不存在返回 `ErrNotFound`）

**Injected Instincts:**
- [ ] `[Instinct: Interface-Consistency]`: Delete 必须与 `Update`/`Complete` 的 `ErrNotFound` 语义一致。
- [ ] `[Instinct: Zero-Lock]`: 复用既有 `s.mu`，不得新增锁层级。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 1: TaskStore Delete (interface + memory impl).
> Mode: AFK | Role: TDD Builder.
> Goal: 为 `TaskStore` 增加 `Delete(id string) error`，并实现内存 `Store.Delete`。
> Target Files: Modify `store/store.go`（接口 + `func (s *Store) Delete(id string) error`）, Test `store/store_test.go`.
> TDD Steps:
> 1. 在 `store/store_test.go` 写失败测试 `TestStore_Delete`（RED）：Create→Delete→Get 应得 `ErrNotFound`；Delete 不存在 id 应得 `ErrNotFound`。
> 2. 运行 `go test ./store/ -run TestStore_Delete -count=1` 并确认失败（编译失败也算 RED，需先补签名使测试可编译后转红）。
> 3. 在 `store/store.go` 接口加 `Delete(id string) error` 并实现（GREEN）。
> 4. 运行 `go test ./store/ -count=1` 全绿。
> 5. 用 guard clause 扁平化实现。
> Return: 摘要 + **原始终端回执（exit code + 通过数 + git diff --stat）**。"

**Step Breakdown:**
- [ ] Step 1: 写失败测试 `TestStore_Delete`（RED）
- [ ] Step 2: 运行并确认按预期失败
- [ ] Step 3: 实现接口方法 + 内存实现（GREEN）
- [ ] Step 4: `go test ./store/ -count=1` 全绿
- [ ] Step 5: Guard clause 扁平化（REFACTOR）
- [ ] Step 6: 物理证据门（命令 + Exit code: 0 + 通过数 + git diff）
- [ ] Step 7: 原子提交

> ⚠️ 本任务只改**内存**实现；`SQLStore` 尚未实现 `Delete`，`go build ./...` 会报未实现接口——
> 因此 **Task 1 与 Task 2 必须连续执行、在同一阶段内合并验证**（或 Task 1 提交前先加 `SQLStore` 空实现占位）。
> **推荐**：Task 1 的提交仅针对 `store_test.go` 与 `store.go`，若编译被 `sql.go` 阻断，则与 Task 2 合并为一次提交。

---

### Task 2: SQLStore 实现 Delete [Tier: 2-Standard] [Mode: AFK] [Subagent: TDD Builder]

**Files:**
- Modify: `store/sql.go`（新增 `(*SQLStore).Delete`）
- Test: `store/sql_test.go`

**Interfaces:**
- Consumes: `type SQLStore struct { db *sql.DB; mu sync.Mutex }`
- Produces: `func (s *SQLStore) Delete(id string) error`（`RowsAffected()==0 → ErrNotFound`）

**Injected Instincts:**
- [ ] `[Instinct: Concurrency]`: 写操作必须持 `s.mu`（SQLite 单写者）。
- [ ] `[Instinct: Zero-Lock DDL]`: 本任务**无 DDL**，仅 `DELETE FROM tasks WHERE id=?`。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 2: SQLStore.Delete.
> Mode: AFK | Role: TDD Builder.
> Goal: 让 `SQLStore` 满足扩展后的 `TaskStore` 接口。
> Target Files: Modify `store/sql.go`, Test `store/sql_test.go`.
> TDD Steps: 1) RED：`TestSQLStore_Delete`（Create→Delete→Get 应 `ErrNotFound`；Delete 不存在 id 应 `ErrNotFound`）；
> 2) 运行 `go test ./store/ -run TestSQLStore_Delete -count=1` 确认失败；3) `s.mu.Lock()` 下 `DELETE`，`RowsAffected()==0` 返回 `ErrNotFound`（GREEN）；
> 4) `go test ./store/ -count=1` 全绿并确认 `go build ./...` 通过。
> Return: 摘要 + 原始终端回执（exit code + 通过数）。"

**Step Breakdown:**
- [ ] Step 1: 写失败测试 `TestSQLStore_Delete`（RED）
- [ ] Step 2: 运行并确认失败
- [ ] Step 3: 持锁实现 `DELETE`（GREEN）
- [ ] Step 4: `go test ./store/ -count=1` + `go build ./...` 全绿
- [ ] Step 5: 复用既有 error 处理风格（REFACTOR）
- [ ] Step 6: 物理证据门
- [ ] Step 7: 原子提交（可与 Task 1 合并）

---

### Task 3: Handler 在 Enqueue 失败时回收任务 [Tier: 2-Standard] [Mode: AFK] [Subagent: TDD Builder]

**Files:**
- Modify: `api/handler.go:95-100`
- Test: `api/handler_test.go`

**Interfaces:**
- Consumes: `h.store.Create(prompt) string` · `h.pool.Enqueue(id) error` · **新增** `h.store.Delete(id) error`
- Produces: Enqueue 失败分支：`store.Delete(id)` 清理后仍返回 503（禁止遗留 `pending` 僵尸任务）

**Injected Instincts:**
- [ ] `[Instinct: No-Zombie]`: 先 `Create` 后 `Enqueue` 的顺序不能反转（worker 可能立即取用），因此失败必须**补偿删除**。
- [ ] `[Instinct: Zero-Silent-Fail]`: `Delete` 自身失败也要 `slog.Error` 记录，但不得改变对外 503 语义。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 3: reclaim task on Enqueue failure.
> Mode: AFK | Role: TDD Builder.
> Goal: 修复 ADR-0012 缺陷①——`POST /run` 在 `Enqueue` 返回 `ErrQueueFull` 时只回 503、不回收任务，导致 store 中残留永久 `pending` 的不可达任务。
> Target Files: Modify `api/handler.go`（`HandleRun` 的 Enqueue 失败分支）, Test `api/handler_test.go`.
> TDD Steps: 1) RED：新测试 `TestHandleRun_QueueFullNoZombie`——用 `worker.NewPool(0, 30*time.Second, s, fakeLLM{})` 构造**确定性满队列**（0 worker + 无缓冲 → Enqueue 必失败），POST `/run` 断言 503 **且** `s.Get("1")` 返回 `ErrNotFound`（无僵尸）；
> 2) 运行 `go test ./api/ -run TestHandleRun_QueueFullNoZombie -count=1` 确认失败；
> 3) 在失败分支调用 `h.store.Delete(id)`（失败仅 `slog.Error`）后返回 503（GREEN）；
> 4) `go test ./api/ -count=1` 全绿。
> Return: 摘要 + 原始终端回执（exit code + 通过数）。"

**Step Breakdown:**
- [ ] Step 1: 写失败测试 `TestHandleRun_QueueFullNoZombie`（RED）
- [ ] Step 2: 运行并确认失败
- [ ] Step 3: 失败分支补偿删除（GREEN）
- [ ] Step 4: `go test ./api/ -count=1` 全绿
- [ ] Step 5: 扁平化错误处理（REFACTOR）
- [ ] Step 6: 物理证据门
- [ ] Step 7: 原子提交

---

### Task 4: Worker 早期失败写入终态 [Tier: 2-Standard] [Mode: AFK] [Subagent: TDD Builder]

**Files:**
- Modify: `worker/pool.go:88-100`（`process` 的 Update/Get 失败分支）
- Test: `worker/pool_test.go`

**Interfaces:**
- Consumes: `p.tasks.Update(id, store.StatusRunning) error` · `p.tasks.Get(id) (store.Task, error)` · `p.tasks.Complete(id, "", err) error`
- Produces: 两处失败分支均**尽力**调用 `p.tasks.Complete(id, "", errors.New(<粗粒度>))`，使任务收敛到 `failed`

**Injected Instincts:**
- [ ] `[Instinct: No-Hang]`: 轮询方必须在有界时间内看到终态——禁止任务卡在 `pending`/`running`。
- [ ] `[Instinct: Zero-Silent-Fail]`: 终态写入失败仍 `slog.Error`；错误文案粗粒度（不泄露内部错误）。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 4: terminal state on worker early failure.
> Mode: AFK | Role: TDD Builder.
> Goal: 修复 ADR-0012 缺陷②——`process` 在 `Update`/`Get` 失败时直接 `return`，任务永远停在中间态，轮询方无法收敛。
> Target Files: Modify `worker/pool.go`, Test `worker/pool_test.go`.
> TDD Steps: 1) 在 `worker/pool_test.go` 新增**桩 store**（实现 `store.TaskStore`，令 `Update` 或 `Get` 返回错误并记录 `Complete` 调用）与失败测试 `TestProcess_UpdateErrorSetsFailed` / `TestProcess_GetErrorSetsFailed`（RED）；
> 2) 运行 `go test ./worker/ -run TestProcess_ -count=1` 确认失败；
> 3) 在两个失败分支尽力调用 `Complete(id, \"\", errors.New(\"state transition failed\"))` / `errors.New(\"task state unavailable\")`（GREEN）；
> 4) `go test -race ./worker/ -count=1` 全绿。
> Return: 摘要 + 原始终端回执（exit code + `-race` 通过数）。"

**Step Breakdown:**
- [ ] Step 1: 写桩 store + 两个失败测试（RED）
- [ ] Step 2: 运行并确认失败
- [ ] Step 3: 失败分支写终态（GREEN）
- [ ] Step 4: `go test -race ./worker/ -count=1` 全绿
- [ ] Step 5: 抽公共 helper 去重两分支（REFACTOR）
- [ ] Step 6: 物理证据门（含 `-race`）
- [ ] Step 7: 原子提交

---

### Task 5: HTTP Server 超时配置 [Tier: 1-Light] [Mode: AFK] [Subagent: Solo]

**Files:**
- Modify: `main.go:74-77`（抽出 `newServer`）
- Test: `main_test.go`（新建，`package main`）

**Interfaces:**
- Consumes: `mux http.Handler`
- Produces: `func newServer(handler http.Handler) *http.Server`，含 `ReadHeaderTimeout` / `WriteTimeout` / `IdleTimeout`（均 > 0）

**Injected Instincts:**
- [ ] `[Instinct: Slowloris]`: 无 `ReadHeaderTimeout` 时慢头攻击可无限占连接（`03-Languages/GO/GO-STANDARDS` §8.4 沿线）。
- [ ] `[Instinct: No-Regression]`: `Addr` 仍为 `:8080`，行为不变，仅新增字段。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 5: server timeouts.
> Mode: AFK | Role: Solo (Tier 1).
> Goal: 修复 ADR-0012 缺陷③——`http.Server` 未设超时，慢连接可无限占用。
> Target Files: Modify `main.go`（抽 `newServer`）, Test `main_test.go`（新建）.
> Steps: 1) 新建 `main_test.go` 断言 `newServer(nil)` 的 `ReadHeaderTimeout`/`WriteTimeout`/`IdleTimeout` 均 > 0（RED：先无 `newServer` 编译失败）；
> 2) 在 `main.go` 抽出 `newServer` 并设 `ReadHeaderTimeout: 10s`、`WriteTimeout: 30s`、`IdleTimeout: 60s`，`run()` 改用之（GREEN）；
> 3) `go test . -count=1` 全绿 + `go build ./...` 通过。
> Return: 摘要 + 原始终端回执（exit code）。"

**Step Breakdown:**
- [ ] Step 1: 写失败测试 `TestNewServerHasTimeouts`（RED）
- [ ] Step 2: 运行并确认失败（缺 `newServer`）
- [ ] Step 3: 抽出 `newServer` 并配置超时（GREEN）
- [ ] Step 4: `go test . -count=1` + `go build ./...` 全绿
- [ ] Step 5: 常量命名 `serverReadHeaderTimeout` 等（REFACTOR）
- [ ] Step 6: 物理证据门（Tier 1 亦需 exit code）
- [ ] Step 7: 原子提交

---

### 阶段 B：Trace 观测链（Tasks 6–11，串行依赖 6→7→8→9→10→11）

### Task 6: TraceStep 类型与 JSON 契约 [Tier: 2-Standard] [Mode: AFK] [Subagent: TDD Builder]

**Files:**
- Create: `worker/trace.go`
- Test: `worker/trace_test.go`

**Interfaces:**
- Consumes: 无
- Produces:
  ```go
  type TraceStep struct {
      Step   int    `json:"step"`
      Kind   string `json:"kind"`
      Tool   string `json:"tool,omitempty"`
      Detail string `json:"detail,omitempty"`
      CallID string `json:"call_id,omitempty"`
  }
  const (
      KindThink      = "think"
      KindToolCall   = "tool_call"
      KindToolResult = "tool_result"
      KindFinal      = "final"
      KindError      = "error"
  )
  ```

**Injected Instincts:**
- [ ] `[Instinct: Wire-Contract]`: JSON tag 是**对外契约**，必须显式声明（对齐 `store.Task` 的 tag 纪律）。
- [ ] `[Instinct: Zero-Emoji]`: `Kind` 只用 ASCII 字面量，便于前端与日志。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 6: TraceStep type + JSON contract.
> Mode: AFK | Role: TDD Builder.
> Goal: 定义 trace 步骤类型与 5 个 `Kind` 常量，并锁死其 JSON 字段名。
> Target Files: Create `worker/trace.go`, Test `worker/trace_test.go`.
> TDD Steps: 1) RED：`TestTraceStep_JSONContract` 将 `TraceStep{Step:0,Kind:KindThink,Tool:"calculate",Detail:"x",CallID:"1"}` 序列化，断言键为 `step`/`kind`/`tool`/`detail`/`call_id`，且空 `Tool`/`Detail`/`CallID` 被省略；
> 2) `go test ./worker/ -run TestTraceStep_JSONContract -count=1` 确认失败；
> 3) 创建 `worker/trace.go` 定义类型与常量（GREEN）；
> 4) `go test ./worker/ -count=1` 全绿。
> Return: 摘要 + 原始终端回执（exit code + 通过数）。"

**Step Breakdown:**
- [ ] Step 1: 写失败测试 `TestTraceStep_JSONContract`（RED）
- [ ] Step 2: 运行并确认失败
- [ ] Step 3: 定义类型与 `Kind` 常量（GREEN）
- [ ] Step 4: `go test ./worker/ -count=1` 全绿
- [ ] Step 5: 常量分组注释（REFACTOR）
- [ ] Step 6: 物理证据门
- [ ] Step 7: 原子提交

---

### Task 7: 有界环形缓冲 traceBuffer [Tier: 2-Standard] [Mode: AFK] [Subagent: TDD Builder]

**Files:**
- Modify: `worker/trace.go`
- Test: `worker/trace_test.go`

**Interfaces:**
- Consumes: `TraceStep`
- Produces:
  ```go
  const maxTraceSteps = 32
  const maxTraceDetail = 512
  type traceBuffer struct { /* mu sync.Mutex; steps []TraceStep; max int */ }
  func newTraceBuffer(max int) *traceBuffer
  func (b *traceBuffer) append(s TraceStep)      // 超出 max 丢弃最旧；Detail 截断至 maxTraceDetail
  func (b *traceBuffer) snapshot() []TraceStep   // 返回**副本**，调用方改动不影响内部
  ```

**Injected Instincts:**
- [ ] `[Instinct: Concurrency]`: `append` 与 `snapshot` 必须持锁；`snapshot` 返回副本以消除数据竞争。
- [ ] `[Instinct: Bounded-Memory]`: 有界缓冲防止单任务事件无上限增长；`Detail` 截断防止巨串。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 7: bounded trace ring buffer.
> Mode: AFK | Role: TDD Builder.
> Goal: 提供并发安全、有界的步骤缓冲。
> Target Files: Modify `worker/trace.go`, Test `worker/trace_test.go`.
> TDD Steps: 1) RED：`TestTraceBuffer_OverflowDropsOldest`（append 40 条，snapshot 长度==32 且首条为第 9 条）、`TestTraceBuffer_SnapshotIsCopy`（改返回值不影响内部）、`TestTraceBuffer_TruncatesDetail`（>512 的 Detail 被截断）、`TestTraceBuffer_ConcurrentAppend`（多 goroutine append 后 snapshot 长度==max）；
> 2) `go test -race ./worker/ -run TestTraceBuffer -count=1` 确认失败；
> 3) 实现（GREEN）；
> 4) `go test -race ./worker/ -count=1` 全绿。
> Return: 摘要 + 原始终端回执（含 `-race`）。"

**Step Breakdown:**
- [ ] Step 1: 写 4 个失败测试（RED）
- [ ] Step 2: `-race` 运行并确认失败
- [ ] Step 3: 实现有界缓冲（GREEN）
- [ ] Step 4: `go test -race ./worker/ -count=1` 全绿
- [ ] Step 5: 提取 `truncateDetail` helper（REFACTOR）
- [ ] Step 6: 物理证据门（含 `-race`）
- [ ] Step 7: 原子提交

---

### Task 8: runAgent 增加 step 回调 [Tier: 2-Standard] [Mode: AFK] [Subagent: TDD Builder]

**Files:**
- Modify: `worker/agent.go:59-92`（`runAgent` 签名 + 5 处 emit）
- Modify: `worker/pool.go:107`（调用点传 `nil` 占位，Task 9 再接线）
- Test: `worker/agent_test.go:51`（更新调用签名）+ 新增断言

**Interfaces:**
- Consumes: `c.ChatWithTools(...)` · `exec(llm.ToolCall) string`
- Produces: `func runAgent(ctx context.Context, c chatter, prompt string, tools []llm.Tool, exec func(llm.ToolCall) string, onStep func(TraceStep)) (string, error)`
  - 每轮 LLM 返回后 emit `think`（`Detail=resp.Content`）
  - 每个 tool call 前 emit `tool_call`（`Tool`/`CallID`/`Detail=Arguments`），执行后 emit `tool_result`（`Detail=输出`）
  - 成功收尾 emit `final`（`Detail=resp.Content`）
  - `ChatWithTools` 出错 emit `error`（`Detail="upstream error"`）；步数耗尽 emit `error`（`Detail="max steps exceeded"`）
  - `onStep == nil` 时全部跳过

**Injected Instincts:**
- [ ] `[Instinct: Secret-Redaction]`: `error` 步骤**严禁**携带原始 error 文本（可能含上游响应体）——只写粗粒度分类。
- [ ] `[Instinct: Nil-Safety]`: `onStep` 为 nil 不得 panic（现有测试与未来调用方可能不关心 trace）。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 8: runAgent step hook.
> Mode: AFK | Role: TDD Builder.
> Goal: 让 agent 循环在关键节点回调上报步骤。
> Target Files: Modify `worker/agent.go`, `worker/pool.go:107`, Test `worker/agent_test.go`.
> TDD Steps: 1) RED：`TestRunAgent_EmitsTrace`（脚本化 1 次 tool call，收集回调，断言有序 kinds == [think, tool_call, tool_result, think, final]）、`TestRunAgent_ErrorStepIsCoarse`（chat 返回 error → `error` 步骤 Detail 不含原始 error 串）、`TestRunAgent_NilCallbackNoPanic`；
> 2) `go test -race ./worker/ -run TestRunAgent -count=1` 确认失败（编译失败即 RED）；同时把 `worker/pool.go:107` 与既有 `agent_test.go` 调用补第 6 个参数 `nil` 使编译通过再转红；
> 3) 实现 emit（GREEN）；4) `go test -race ./worker/ -count=1` 全绿。
> Return: 摘要 + 原始终端回执（含 `-race`）。"

**Step Breakdown:**
- [ ] Step 1: 写 3 个失败测试 + 补既有调用点签名（RED）
- [ ] Step 2: `-race` 运行并确认失败
- [ ] Step 3: 实现 5 处 emit（GREEN）
- [ ] Step 4: `go test -race ./worker/ -count=1` 全绿
- [ ] Step 5: 抽 `emit()` 微 helper 降重（REFACTOR）
- [ ] Step 6: 物理证据门（含 `-race`）
- [ ] Step 7: 原子提交

---

### Task 9: Pool 持有 trace 并提供 Trace(id) [Tier: 2-Standard] [Mode: AFK] [Subagent: TDD Builder]

**Files:**
- Modify: `worker/pool.go`（`Pool` 增 `traces map[string]*traceBuffer` + `tracesMu sync.Mutex`；`NewPool` 初始化；`process` 建缓冲并接线）
- Test: `worker/pool_test.go`

**Interfaces:**
- Consumes: `newTraceBuffer` · `runAgent(..., onStep)` · `taskBuffer.append`
- Produces: `func (p *Pool) Trace(id string) []TraceStep`（未知 id 返回 `nil`；返回**副本**）；`process` 内 `p.putTrace(id, tb)` 后把 `tb.append` 传入 `runAgent`

**Injected Instincts:**
- [ ] `[Instinct: Concurrency]`: `traces` map 读写必须持锁；`Trace` 返回 `tb.snapshot()`（副本）。
- [ ] `[Instinct: Stable-Signature]`: `process` 的**对外行为不变**（仍写 running/终态），只增内部 trace——ADR-0008 契约不动。
- [ ] `[Instinct: Bounded-Memory]`: trace 生命周期 == 任务在 store 中的生命周期（内存 store 本就不淘汰，ADR-0004 已接受）；显式说明其增长面与 store 同阶。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 9: pool trace registry.
> Mode: AFK | Role: TDD Builder.
> Goal: 让 Pool 保存每个任务的步骤缓冲并对外提供只读快照。
> Target Files: Modify `worker/pool.go`, Test `worker/pool_test.go`.
> TDD Steps: 1) RED：`TestPool_TraceAfterProcess`（fakeChatter 处理一个任务后 `p.Trace(id)` 非空且末条 Kind==final）、`TestPool_TraceUnknownID`（返回 nil/空）；
> 2) `go test -race ./worker/ -run TestPool_Trace -count=1` 确认失败；
> 3) 在 `Pool` 加 `traces`/`tracesMu`，`process` 建 `newTraceBuffer(maxTraceSteps)`、注册、把 `tb.append` 传给 `runAgent`（GREEN）；
> 4) `go test -race ./worker/ -count=1` 全绿。
> Return: 摘要 + 原始终端回执（含 `-race`）。"

**Step Breakdown:**
- [ ] Step 1: 写 `TestPool_TraceAfterProcess` / `TestPool_TraceUnknownID`（RED）
- [ ] Step 2: `-race` 运行并确认失败
- [ ] Step 3: 接线缓冲与回调（GREEN）
- [ ] Step 4: `go test -race ./worker/ -count=1` 全绿
- [ ] Step 5: `putTrace`/`Trace` 成对方法封装（REFACTOR）
- [ ] Step 6: 物理证据门（含 `-race`）
- [ ] Step 7: 原子提交

---

### Task 10: API 暴露 steps 字段 [Tier: 2-Standard] [Mode: AFK] [Subagent: TDD Builder]

**Files:**
- Modify: `api/handler.go:116-131`（`HandleGet` 响应组装）
- Test: `api/task_contract_test.go`（更新 want 列表）, `api/handler_test.go`

**Interfaces:**
- Consumes: `h.store.Get(id)` · **新增** `h.pool.Trace(id) []worker.TraceStep`
- Produces:
  ```go
  type taskWithTrace struct {
      store.Task
      Steps []worker.TraceStep `json:"steps"`
  }
  ```
  `GET /tasks/{id}` 的 JSON 新增 `steps` 数组（无步骤时为 `[]`，**不得为 `null`**）

**Injected Instincts:**
- [ ] `[Instinct: Wire-Contract]`: 内嵌 `store.Task` 使其 5 个字段保持**扁平**输出，旧字段名与语义不变。
- [ ] `[Instinct: Non-Nil-Slice]`: nil slice 会序列化成 `null`；必须归一化为 `[]`，保持前端可无条件 `.forEach`。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 10: expose steps on GET /tasks/{id}.
> Mode: AFK | Role: TDD Builder.
> Goal: 在既有查询响应上**增字段**（不改旧字段），暴露 trace 步骤。
> Target Files: Modify `api/handler.go`, Test `api/task_contract_test.go` + `api/handler_test.go`.
> TDD Steps: 1) RED：更新 `TestTaskJSONContract` 的 want 列表加入 `steps` 并断定为 JSON 数组（首字节 `[`）；
> 2) `go test ./api/ -run TestTaskJSONContract -count=1` 确认失败；
> 3) 定义 `taskWithTrace`，`HandleGet` 用 `h.pool.Trace(id)` 组装，nil→`[]`（GREEN）；
> 4) `go test ./api/ -count=1` 全绿。
> Return: 摘要 + 原始终端回执（exit code + 通过数）。"

**Step Breakdown:**
- [ ] Step 1: 更新契约测试加 `steps` 断言（RED）
- [ ] Step 2: 运行并确认失败
- [ ] Step 3: 组装响应 + nil 归一（GREEN）
- [ ] Step 4: `go test ./api/ -count=1` 全绿
- [ ] Step 5: 抽 `normalizeSteps` helper（REFACTOR）
- [ ] Step 6: 物理证据门
- [ ] Step 7: 原子提交

---

### Task 11: Demo 渲染 Agent 轨迹 [Tier: 2-Standard] [Mode: AFK] [Subagent: TDD Builder]

**Files:**
- Modify: `api/demo.html`（轮询循环渲染 `task.steps`）
- Test: `api/handler_test.go`（扩展 `TestDemoPage`）

**Interfaces:**
- Consumes: `GET /tasks/{id}` 响应中的 `steps: TraceStep[]`
- Produces: Demo 中新增轨迹列表容器；每条步骤以 `createElement` + `textContent` + `append` 渲染（**零 innerHTML**）

**Injected Instincts:**
- [ ] `[Instinct: Zero-innerHTML]`: 全程 `createElement`/`textContent`/`append`；**禁止** `innerHTML`/`insertAdjacentHTML`。
- [ ] `[Instinct: Incremental-Render]`: 逐条 append，避免整块重写（O(1) 增量 + 不跳滚动）。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 11: render agent trace in demo.
> Mode: AFK | Role: TDD Builder.
> Goal: 让内嵌 Demo 在轮询中逐步展示 `think/tool_call/tool_result/final` 轨迹。
> Target Files: Modify `api/demo.html`, Test `api/handler_test.go`.
> TDD Steps: 1) RED：扩展 `TestDemoPage`，断言内嵌 HTML **不含** `innerHTML` 且**含**轨迹容器标记（如 `id=\"trace\"`）；
> 2) `go test ./api/ -run TestDemoPage -count=1` 确认失败；
> 3) 在 `demo.html` 加轨迹容器 + 逐条 `createElement`/`textContent`/`append` 渲染 `task.steps`（GREEN）；
> 4) `go test ./api/ -count=1` 全绿。
> Return: 摘要 + 原始终端回执（exit code + 通过数）。"

**Step Breakdown:**
- [ ] Step 1: 扩展 `TestDemoPage`（断言无 `innerHTML` + 有轨迹容器）（RED）
- [ ] Step 2: 运行并确认失败
- [ ] Step 3: 实现增量渲染（GREEN）
- [ ] Step 4: `go test ./api/ -count=1` 全绿
- [ ] Step 5: 抽 `renderStep(step)` 函数（REFACTOR）
- [ ] Step 6: 物理证据门
- [ ] Step 7: 原子提交

---

## 🌫️ Fog of War (Not Yet Specified)

- **[Unknown 1] trace 的持久化与容量策略**：当前 trace 生命周期 == 任务在 store 中的生命周期
  （内存实现本不淘汰）。何时需要 LRU 上限或把 trace 落 SQLite，取决于真实任务量与观测需求——
  先不猜。
- **[Unknown 2] token 级流式**：ADR-0012 已否决；仅当 ADR-0005 触发信号 1（真实 UX 要求流式 token）成立时，
  另立 ADR 重估（届时需同时重估 `WriteTimeout` 与 `llm.Client` 契约）。
- **[Unknown 3] 事件 schema 版本化 / `Last-Event-ID` 续传**：仅在被测方案（SSE）复活时才需要。
- **[Unknown 4] 测试计数对账**：ADR-0007 记「25 passed」，而当前 `func Test` 计数与之不符——执行本计划前
  **MUST** 以 `go test ./... -count=1 -v | grep -c '^=== RUN'` 实测对账，不得引用记忆数字。

## 🚫 Out of Scope

- SSE / WebSocket 端点与任何服务端推送（ADR-0012 明确否决）。
- `llm.Client` 流式（delta）契约改造。
- store 的事件表 / schema 变更。
- 多 Agent 编排（P3，无 ADR 依据）。
- 引入 Prometheus client 或任何第三方依赖。
- `store.Task` 的 `Status`/`Error` 补 JSON tag（与本计划无关的既有小瑕疵，不顺手改）。
