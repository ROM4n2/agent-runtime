# P4: 生产可用性加固与多专家审计缺陷修复实施计划 (Implementation Plan)

> **Goal**: 针对多专家联合审计（dfs-team）发现的 2 项 P0 严重缺陷（Trace 内存泄漏、停机生命周期倒挂丢任务）与 4 项 P1 关键缺陷（结构化日志语法、超时倒挂与环境变量死角、错误分类精确落库、SQLite 全表扫描性能）进行系统性加固与 TDD 修复，建立生产级可用性与稳定性。
> **Tech Stack**: Go 1.26.5 (Standard Library First, `modernc.org/sqlite`, `agent-api` 模块)
> **Spec Reference**: `docs/audit/ADR-0013-CODE-QUALITY-AUDIT.md`, `docs/audit/perf-concurrency-audit.md`, `docs/audit/sre-resilience-audit.md`
> **Global Constraints**: 纯标准库优先、严禁引入外部未审计依赖；所有测试必须满足 `-race` 零数据竞态；严格遵循 Errors-As-Messages 原则；GOWORK=off 环境隔离。
> **Branch**: `fix/audit-findings-remediation` — 从当前 `master` 签出，全部任务经 Maker-Checker 审核与逆向突变门禁验证后合并。

---

## 🏛️ Decisions So Far
- [ADR-0002](docs/adr/0002-async-worker-pool.md): 采用有界 Worker Pool 与 Channel 背压，拒绝无界 Goroutine。
- [ADR-0005](docs/adr/0005-polling-vs-server-push.md): 维持轮询，拒绝 WebSocket/全量推送复杂度。
- [ADR-0010](docs/adr/0010-sqlite-persistence.md): 采用嵌入式 SQLite 可选持久化。
- [ADR-0011](docs/adr/0011-task-timeout-and-graceful-shutdown.md): 引入任务超时与优雅停机取消树。
- [ADR-0013](docs/adr/0013-decouple-agent-kernel-and-dependency-inversion.md): 确立 `agent/` 认知内核域、`agent/tool/` 工具插件库、`api.TaskRunner` 消费者接口依赖倒置，`worker/` 纯化为调度池。

---

## 🎯 Active Frontier (Unblocked Tasks)

### Task 1: 结构化日志传参修复与 503 重试退避头 [Tier: 1-Light] [Mode: AFK] [Role: Solo Builder]
*(Tier: 1-Light = Solo Fast-Path + physical receipt, no Reviewer subagent)*

**Files:**
- Modify: `worker/pool.go:88,91,99,102,121,125,132`, `api/handler.go:106-108`
- Test: `api/handler_test.go`

**Interfaces:**
- Consumes: `log/slog.Error(msg string, args ...any)`, `http.ResponseWriter.Header().Set(key, val)`
- Produces: 符合标准键值对的结构化日志输出；503 响应携带 `Retry-After: 1`

**Injected Instincts (Compile-Time Rule Injection):**
- [ ] `[Instinct: Logging]`: 严禁在 `slog.Error` 的第一个参数中使用 `%s`、`%v` 等 Printf 格式化占位符；必须采用 `"key", value` 键值对传参，杜绝动态键污染与 Mapping 爆炸。
- [ ] `[Instinct: HTTP Semantics]`: 在返回 503 Service Unavailable 时必须下发 `Retry-After` 头指示退避时间。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Batch 1: 结构化日志传参修复与 503 重试退避头.
> Mode: AFK | Role: Solo Builder | Tier: 1-Light
> Goal: 
> 1. 将 `worker/pool.go` 中所有使用 Printf 占位符的 `slog.Error` 替换为标准结构化键值对（例如 `slog.Error(\"worker: task execution failed\", \"task_id\", id, \"error\", err)`）；
> 2. 在 `api/handler.go` 中当 `h.runner.Enqueue` 返回队列满时，在 `http.Error` 前增加 `w.Header().Set(\"Retry-After\", \"1\")`；
> 3. 在 `api/handler_test.go` 的 `TestHandleRun_QueueFullNoZombie` 中断言响应头包含 `Retry-After: 1`。
> Return: Summary with MANDATORY Physical Execution Receipt (exit code + test output).\"

**Step Breakdown:**
- [ ] **Step 1: Write failing test in `api/handler_test.go` asserting `Retry-After` header (RED)**
- [ ] **Step 2: Run test and verify failure**
- [ ] **Step 3: Modify `worker/pool.go` log calls and `api/handler.go` headers (GREEN)**
- [ ] **Step 4: Run `GOWORK=off go test ./api/ ./worker/ -count=1` and verify all green**
- [ ] **Step 5: Physical Evidence Gate**: Capture raw terminal execution receipt (Command + Exit code: 0)
- [ ] **Step 6: Git atomic commit**: `fix(worker,api): normalize structured logging and add Retry-After header on 503`

---

### Task 2: Trace 缓存容量淘汰与 UTF-8 安全截断 [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]
*(Tier: 2-Standard = Full TDD Builder + Reviewer Maker-Checker)*

**Files:**
- Modify: `agent/trace.go:48-54`, `worker/pool.go:35-37, 51, 112-115, 149-158`
- Test: `agent/trace_test.go`, `worker/pool_test.go`

**Interfaces:**
- Consumes: `unicode/utf8.RuneStart`, `agent.TraceStep`
- Produces: `agent.TraceBuffer.Append` 原地切片平移覆盖（0 额外堆扩容）与 UTF-8 字符边界对齐截断；`worker.Pool` 内置有界淘汰（上限 1000），消除无界内存泄漏。

**Injected Instincts (Compile-Time Rule Injection):**
- [ ] `[Instinct: Memory Boundedness]`: 所有常驻内存映射必须具备硬上限或淘汰策略；严禁向进程级 Map 单调写入无删除。
- [ ] `[Instinct: UTF-8 Safety]`: 字符串按字节截断时必须确保截断点在合法 UTF-8 字符边界，禁止截断出 `\ufffd` 乱码。

**Subagent Prompt Scaffold (for /dfs-exec):**
> \"Implement Task 2: Trace 缓存容量淘汰与 UTF-8 安全截断.
> Mode: AFK | Role: TDD Builder | Tier: 2-Standard
> Goal:
> 1. 在 `agent/trace.go` 中，优化 `TraceBuffer.Append`：当达到最大步数时，使用 `copy(b.steps, b.steps[1:])` 原地向前平移并覆盖末尾，保持切片 capacity 不变；对 Detail 截断时，若第 512 字节位于 UTF-8 字符中间，回退到合法的 `utf8.RuneStart` 边界；
> 2. 在 `worker/pool.go` 中，为 `Pool` 增加 `maxTraces int`（默认 1000）与 `traceOrder []string`；在 `process` 插入新的 `TraceBuffer` 时，若数量超过上限，驱逐并 `delete` 最旧的任务记录；
> 3. 编写 `TestTraceBuffer_SlidingWindowZeroCapCollapse`、`TestTraceBuffer_UTF8SafeTruncation` 与 `TestPool_TracesBoundedCapacity` 测试用例。
> Return: Summary with MANDATORY Physical Execution Receipt (exit code + test output).\"

**Step Breakdown:**
- [ ] **Step 1: Write failing tests in `agent/trace_test.go` and `worker/pool_test.go` (RED)**
- [ ] **Step 2: Run tests and verify failure**
- [ ] **Step 3: Implement slice copy, UTF-8 truncation in `agent/trace.go`, and LRU/FIFO eviction in `worker/pool.go` (GREEN)**
- [ ] **Step 4: Run `GOWORK=off go test -race ./agent/ ./worker/ -count=1` and verify all green**
- [ ] **Step 5: Refactor & verify 0 memory leaks**
- [ ] **Step 6: Physical Evidence Gate**: Capture raw terminal execution receipt (Command + Exit code: 0)
- [ ] **Step 7: Git atomic commit**: `fix(agent,worker): bound trace cache capacity and secure UTF-8 truncation`

---

### Task 3: 两阶段优雅停机与任务队列排空 [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]
*(Tier: 2-Standard = Full TDD Builder + Reviewer Maker-Checker)*

**Files:**
- Modify: `worker/pool.go:63-80`, `main.go:66-70, 84-94, 109-120`
- Test: `worker/pool_test.go`, `main_test.go`

**Interfaces:**
- Consumes: `context.Context` (停机上下文，含 30s 宽限期)
- Produces: `func (p *Pool) Stop(ctx context.Context) error`；统一编排停机；排空积压队列并标定 `server shutdown` 终态。

**Injected Instincts (Compile-Time Rule Injection):**
- [ ] `[Instinct: Graceful Lifecycle]`: 停机必须先停止新请求接入，再为在飞执行留足超时预算，最后兜底排空队列；严禁直接对在飞 Worker 广播 `cancel()`。
- [ ] `[Instinct: Terminal State Guarantee]`: 队列中未能执行的任务必须在停机时确切标记为终态，绝不允许永久滞留在 `pending`。

**Subagent Prompt Scaffold (for /dfs-exec):**
> \"Implement Task 3: 两阶段优雅停机与任务队列排空.
> Mode: AFK | Role: TDD Builder | Tier: 2-Standard
> Goal:
> 1. 改造 `worker/pool.go`：
>    - 增加 `stopOnce sync.Once` 与 `stopping atomic.Bool`；
>    - `Enqueue` 在停机时拒绝入队返回 `ErrQueueFull` 或特定停机错误；
>    - `Stop(ctx context.Context)`：等待正在运行的 worker 处理完成；若 `ctx` 超时才触发 `p.cancel()`；
>    - 停机时排空 `p.queue` 中积压的任务，对每一个积压的 id 调用 `p.tasks.Complete(id, \"\", errors.New(\"server shutdown\"))` 并 `metrics.IncFailed()`；
> 2. 改造 `main.go`：
>    - 统一停机逻辑 `shutdown(srv *http.Server, p *worker.Pool) int`：先 `srv.Shutdown(shutdownCtx)` 停 HTTP（约微秒/毫秒级），然后将同一个 `shutdownCtx` 传给 `p.Stop(shutdownCtx)`，使 Worker 能够充分使用剩余的 30s 宽限期完成在飞任务；
> 3. 编写 `TestShutdown_DrainsQueuedTasks` 测试验证停机后队列积压任务全部变为 `server shutdown` 终态而非滞留 `pending`。
> Return: Summary with MANDATORY Physical Execution Receipt (exit code + test output).\"

**Step Breakdown:**
- [ ] **Step 1: Write failing test in `worker/pool_test.go` simulating queue drain on shutdown (RED)**
- [ ] **Step 2: Run test and verify failure**
- [ ] **Step 3: Implement graceful drain in `worker/pool.go` and update `main.go` shutdown orchestration (GREEN)**
- [ ] **Step 4: Run `GOWORK=off go test -race ./worker/ . -count=1` and verify all green**
- [ ] **Step 5: Physical Evidence Gate**: Capture raw terminal execution receipt (Command + Exit code: 0)
- [ ] **Step 6: Git atomic commit**: `fix(worker,main): implement two-phase graceful shutdown and queue drain`

---

### Task 4: 细化任务异常落库分类 [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]
*(Tier: 2-Standard = Full TDD Builder + Reviewer Maker-Checker)*

**Files:**
- Modify: `worker/pool.go:117-128`
- Test: `worker/pool_test.go`

**Interfaces:**
- Consumes: `taskCtx.Err()`, `agent.Run(taskCtx, ...)` error
- Produces: 映射粗粒度分类错误落库（`task timeout` / `server shutdown` / `max steps exceeded` / `upstream error`）

**Injected Instincts (Compile-Time Rule Injection):**
- [ ] `[Instinct: Observability]`: 绝不使用通用的粗暴错误掩盖内部状态机死因；必须区分超时、取消与外部服务失败，保护运维诊断 SLA。

**Subagent Prompt Scaffold (for /dfs-exec):**
> \"Implement Task 4: 细化任务异常落库分类.
> Mode: AFK | Role: TDD Builder | Tier: 2-Standard
> Goal:
> 1. 在 `worker/pool.go` 的 `process()` 中，当 `agent.Run` 失败时：
>    - 若 `errors.Is(taskCtx.Err(), context.DeadlineExceeded)` -> 写入 `p.tasks.Complete(id, \"\", errors.New(\"task timeout\"))`；
>    - 若 `errors.Is(taskCtx.Err(), context.Canceled)` -> 写入 `p.tasks.Complete(id, \"\", errors.New(\"server shutdown\"))`；
>    - 若 `strings.Contains(err.Error(), \"max steps\")` -> 写入 `p.tasks.Complete(id, \"\", errors.New(\"max steps exceeded\"))`；
>    - 其余网络或模型错误 -> 写入 `p.tasks.Complete(id, \"\", errors.New(\"upstream error\"))`；
> 2. 在 `worker/pool_test.go` 中增加针对超时、取消、步数超限的细化断言测试。
> Return: Summary with MANDATORY Physical Execution Receipt (exit code + test output).\"

**Step Breakdown:**
- [ ] **Step 1: Write failing tests in `worker/pool_test.go` asserting specific error messages on timeout/canceled (RED)**
- [ ] **Step 2: Run test and verify failure**
- [ ] **Step 3: Implement categorized error mapping in `worker/pool.go` (GREEN)**
- [ ] **Step 4: Run `GOWORK=off go test -race ./worker/ -count=1` and verify all green**
- [ ] **Step 5: Physical Evidence Gate**: Capture raw terminal execution receipt (Command + Exit code: 0)
- [ ] **Step 6: Git atomic commit**: `fix(worker): categorize terminal error states for accurate root-cause diagnosis`

---

### Task 5: 环境变量与传输层超时解耦 [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]
*(Tier: 2-Standard = Full TDD Builder + Reviewer Maker-Checker)*

**Files:**
- Modify: `config/config.go:35-41`, `llm/client.go:55-68`, `main.go:66`
- Test: `config/config_test.go`, `llm/client_test.go`

**Interfaces:**
- Consumes: `os.Getenv("TASK_TIMEOUT_SECONDS")`, `llm.Config.HTTPTimeout`
- Produces: 动态环境变量注入任务超时；`llm.Client` 支持可配置 HTTP 连接超时（默认 60s）

**Injected Instincts (Compile-Time Rule Injection):**
- [ ] `[Instinct: Config Hierarchy]`: 环境变量优先级绝对高于静态配置文件；无文件环境下必须完全支持纯环境变量引导。
- [ ] `[Instinct: Layered Timeouts]`: 底层 HTTP 传输超时必须能够随上层业务超时动态配置，严禁硬编码。

**Subagent Prompt Scaffold (for /dfs-exec):**
> \"Implement Task 5: 环境变量与传输层超时解耦.
> Mode: AFK | Role: TDD Builder | Tier: 2-Standard
> Goal:
> 1. 在 `config/config.go` 中解析 `os.Getenv(\"TASK_TIMEOUT_SECONDS\")`，若设置了非负整数则覆盖配置文件值；
> 2. 在 `llm/client.go` 的 `Config` 结构体中增加 `HTTPTimeout time.Duration`；在 `NewClient` 中若 `HTTPTimeout <= 0` 则保底为 60s，否则使用设置值；
> 3. 在 `main.go` 初始化 `llm.NewClient` 时，将单步 HTTP 超时配置为 `time.Duration(cfg.TaskTimeoutSeconds)*time.Second`（若已配置）；
> 4. 在 `config/config_test.go` 与 `llm/client_test.go` 中增加覆盖测试。
> Return: Summary with MANDATORY Physical Execution Receipt (exit code + test output).\"

**Step Breakdown:**
- [ ] **Step 1: Write failing tests in `config/config_test.go` and `llm/client_test.go` (RED)**
- [ ] **Step 2: Run tests and verify failure**
- [ ] **Step 3: Implement env parsing in `config/config.go` and timeout parameter in `llm/client.go` (GREEN)**
- [ ] **Step 4: Run `GOWORK=off go test ./config/ ./llm/ -count=1` and verify all green**
- [ ] **Step 5: Physical Evidence Gate**: Capture raw terminal execution receipt (Command + Exit code: 0)
- [ ] **Step 6: Git atomic commit**: `feat(config,llm): support TASK_TIMEOUT_SECONDS env and configurable HTTP timeout`

---

### Task 6: SQLite 原生自增主键消除全表扫描 [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]
*(Tier: 2-Standard = Full TDD Builder + Reviewer Maker-Checker)*

**Files:**
- Modify: `store/sql.go:41-47, 59-77`
- Test: `store/sql_test.go`

**Interfaces:**
- Consumes: `res.LastInsertId()`
- Produces: `INTEGER PRIMARY KEY AUTOINCREMENT` 建表；`Create` 单次插入微秒级完成，消除全表扫描与写互锁。

**Injected Instincts (Compile-Time Rule Injection):**
- [ ] `[Instinct: Database Performance]`: 严禁在排他写锁内执行全表扫描或聚合查询；利用存储引擎原生机制获取自增主键。

**Subagent Prompt Scaffold (for /dfs-exec):**
> \"Implement Task 6: SQLite 原生自增主键消除全表扫描.
> Mode: AFK | Role: TDD Builder | Tier: 2-Standard
> Goal:
> 1. 在 `store/sql.go` 中，建表 SQL 改为 `id INTEGER PRIMARY KEY AUTOINCREMENT`；
> 2. 重写 `Create(prompt string) string`：执行 `INSERT INTO tasks (status, prompt) VALUES (?, ?)`，通过 `res.LastInsertId()` 获取自增数字，转为 `strconv.FormatInt(lastID, 10)` 返回；
> 3. 彻底删除 `SELECT COALESCE(MAX(CAST(id AS INTEGER)), 0) FROM tasks`；
> 4. `Get`, `Update`, `Complete`, `Delete` 保持对外 API `id string` 签名不变；
> 5. 运行 `store/sql_test.go` 确保所有既有功能测试与并发测试全部通过。
> Return: Summary with MANDATORY Physical Execution Receipt (exit code + test output).\"

**Step Breakdown:**
- [ ] **Step 1: Write benchmark / test in `store/sql_test.go` verifying autoincrement ID generation (RED)**
- [ ] **Step 2: Run test and verify failure**
- [ ] **Step 3: Refactor schema and Create implementation in `store/sql.go` (GREEN)**
- [ ] **Step 4: Run `GOWORK=off go test -race ./store/ -count=1` and verify all green**
- [ ] **Step 5: Physical Evidence Gate**: Capture raw terminal execution receipt (Command + Exit code: 0)
- [ ] **Step 6: Git atomic commit**: `perf(store): adopt sqlite autoincrement primary key and eliminate full table scan`

---

### Task 7: 全仓回归与物理证据门禁 [Tier: 1-Light] [Mode: AFK] [Role: Solo Builder]
*(Tier: 1-Light = Solo Fast-Path + physical receipt)*

**Files:**
- Verify: 全仓 9 个 package (`agent-api`, `agent`, `agent/tool`, `api`, `config`, `llm`, `metrics`, `store`, `worker`)

**Interfaces:**
- Consumes: 全量自动化测试套件
- Produces: 100% 绿灯物理证据与 0 竞态证明

**Injected Instincts (Compile-Time Rule Injection):**
- [ ] `[Instinct: Zero-Regression]`: 任何架构重构与修复必须保证既有契约和测试 100% 回归通过，绝无隐式降级。

**Subagent Prompt Scaffold (for /dfs-exec):**
> \"Implement Task 7: 全仓回归与物理证据门禁.
> Mode: AFK | Role: Solo Builder | Tier: 1-Light
> Goal:
> 1. 执行 `GOWORK=off go test -race ./... -count=1` 验证全包零竞态；
> 2. 执行 `GOWORK=off go build ./... && GOWORK=off go vet ./...` 验证编译与静态分析；
> 3. 统计测试总数，验证所有 P0/P1 修复生效且无回归；
> 4. 运行 `bash .githooks/pre-commit` 确保无敏感信息泄漏。
> Return: Summary with MANDATORY Physical Execution Receipt (exit code + test counts).\"

**Step Breakdown:**
- [ ] **Step 1: Run `GOWORK=off go test -race ./... -count=1`**
- [ ] **Step 2: Run `GOWORK=off go vet ./...` and `GOWORK=off go build ./...`**
- [ ] **Step 3: Run `.githooks/pre-commit`**
- [ ] **Step 4: Physical Evidence Gate**: Capture terminal receipts (Exit code 0, 115+ test cases)
- [ ] **Step 5: Git atomic commit**: `chore(ci): verify full repository regression and race checks`

---

## 🌫️ Fog of War (Not Yet Specified)
- **[Unknown 1: 分布式事件总线]**: 多实例部署下的任务状态同步与跨节点事件广播（当前属于单实例进程内架构，等待多机部署需求触发）。
- **[Unknown 2: Prometheus 直方图与细粒度标签]**: 指标直方图引入（等待监控告警体系对接规范确认）。

---

## 🚫 Out of Scope
- 重写 HTTP 协议栈引入 WebSocket / gRPC（ADR-0005 维持轮询架构已决策）。
- 引入 Redis / Kafka 等第三方重型中间件（ADR-0004 维持嵌入式/标准库极简架构已决策）。
- 更改对外 HTTP RESTful JSON 结构（已有测试 `TestTaskJSONContract` 锁死契约）。
