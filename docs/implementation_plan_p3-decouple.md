# ADR-0013 架构解耦与领域正名 Implementation Plan

> **Goal**: 依据 ADR-0013 将 `worker` 拆解为纯粹的并发执行池，抽取独立的 `agent` 认知内核与 `agent/tool` 工具系统，并在 `api` 层实现 `TaskRunner` 消费者接口倒置，彻底消除越层强耦合与循环依赖风险。
> **Tech Stack**: Go 1.26.5 (Standard Library First, modernc.org/sqlite)
> **Spec Reference**: `docs/adr/0013-decouple-agent-kernel-and-dependency-inversion.md`
> **Global Constraints**: 零破坏性重构（全部 57 项既有测试行为 100% 保持通过）；父级 Go workspace 隔离必须加 `GOWORK=off`；`api` 层绝对禁止依赖 `worker`。
> **Branch**: `refactor/agent-kernel-decouple`

---

## 🏛️ Decisions So Far

- [ADR-0001](docs/adr/0001-agent-project-as-phase-2-3.md): 项目作为阶段 2/3 载体与 Go 原生异步设计。
- [ADR-0002](docs/adr/0002-async-task-worker-pool-architecture.md): 异步任务 + Worker Pool 架构与快速响应。
- [ADR-0003](docs/adr/0003-security-resilience-design.md): 粗粒度错误脱敏与中间件链防御。
- [ADR-0004](docs/adr/0004-no-persistence-no-message-queue.md): YAGNI 原则，单机轻量。
- [ADR-0005](docs/adr/0005-polling-vs-server-push.md): 客户端维持无状态短轮询，避免过早引入双向推送。
- [ADR-0008](docs/adr/0008-agent-tool-calling-loop.md): Agent 工具调用循环与对外契约稳定。
- [ADR-0011](docs/adr/0011-task-timeout-and-graceful-shutdown.md): 可配置任务超时与优雅停机取消树。
- [ADR-0012](docs/adr/0012-next-step-observation-transport-decoupling.md): 拒绝 SSE 全量推送，采观测-传输解耦的 Trace 环形缓冲。
- [ADR-0013](docs/adr/0013-decouple-agent-kernel-and-dependency-inversion.md): 汲取 Pi 分层哲学，正名 `agent` 认知内核，抽取 `agent/tool` 插件库，并在 `api` 层推行 `TaskRunner` 接口倒置。

---

## 🎯 Active Frontier (Unblocked Tasks)

### Task 1: 独立工具系统 agent/tool [Tier: 2-Standard] [Mode: AFK] [Subagent: TDD Builder]

**Files:**
- Create: `agent/tool/tool.go`（Tool 接口与 Registry 注册表）
- Create: `agent/tool/calc.go`（CalculateTool 实现与 safe AST 算术解析器）
- Create: `agent/tool/time.go`（CurrentTimeTool 实现与 IANA 时区支持）
- Create: `agent/tool/default.go`（DefaultRegistry 工厂函数）
- Test: `agent/tool/calc_test.go`
- Test: `agent/tool/time_test.go`
- Test: `agent/tool/registry_test.go`

**Interfaces:**
- Consumes: `llm.Tool` · `llm.ToolCall`
- Produces:
  ```go
  package tool

  type Tool interface {
      Spec() llm.Tool
      Execute(args json.RawMessage) (string, error)
  }

  type Registry struct { ... }
  func NewRegistry() *Registry
  func (r *Registry) Register(t Tool)
  func (r *Registry) Get(name string) (Tool, bool)
  func (r *Registry) Specs() []llm.Tool
  func (r *Registry) Execute(call llm.ToolCall) string

  func NewCalculateTool() Tool
  func NewCurrentTimeTool() Tool
  func DefaultRegistry() *Registry
  ```

**Injected Instincts:**
- [ ] `[Instinct: Errors-As-Messages]`: 工具执行异常（解析失败、除以零、非法时区）必须返回错误信息字符串而非中断循环；未知工具返回 `"error: unknown tool " + name`。
- [ ] `[Instinct: Safe-Arithmetic]`: 算术表达式解析器保留纯递归下降 AST 分析，严禁使用 eval 或反射，杜绝代码注入。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 1: 独立工具系统 agent/tool.
> Mode: AFK | Role: TDD Builder.
> Goal: 独立抽取可插拔 Tool 接口与 Registry 注册表，迁入 calculate 与 current_time 工具。
> Target Files: Create `agent/tool/tool.go`, `agent/tool/calc.go`, `agent/tool/time.go`, `agent/tool/default.go`, Test `agent/tool/calc_test.go`, `agent/tool/time_test.go`, `agent/tool/registry_test.go`.
> TDD Steps:
> 1. 编写 RED 测试：TestCalculateTool_Execute、TestEvalArithmetic、TestCurrentTimeTool_Execute、TestRegistry_SpecsAndExecute（断言未知工具返回 `error: unknown tool ...`，正常工具执行返回字符串结果）。
> 2. 运行 `GOWORK=off go test ./agent/tool/ -count=1` 确认编译失败或测试转红。
> 3. 实现 `Tool` 接口、`Registry`、`CalculateTool`、`CurrentTimeTool`、`DefaultRegistry`（GREEN）。
> 4. 运行 `GOWORK=off go test -race ./agent/tool/ -count=1 -v` 验证全部通过。
> 5. 运行 `GOWORK=off go build ./...` 确认全局编译正常。
> Return: 摘要与真实终端回执（exit code + pass count）。"

**Step Breakdown:**
- [ ] **Step 1: Write the failing tests (RED)**: 在 `agent/tool/*_test.go` 编写工具契约测试
- [ ] **Step 2: Run test and verify it fails**: `GOWORK=off go test ./agent/tool/ -count=1`
- [ ] **Step 3: Implement minimal production code (GREEN)**: 完成 `agent/tool` 模块实现
- [ ] **Step 4: Run tests and verify all green**: `GOWORK=off go test -race ./agent/tool/ -count=1 -v`
- [ ] **Step 5: Refactor & Clean code (REFACTOR)**: 确保 errors-as-messages 契约一致
- [ ] **Step 6: Physical Evidence Gate**: 执行全包测试确认退出码为 0
- [ ] **Step 7: Git atomic commit**: `feat(agent/tool): extract modular tool interface and default registry`

---

### Task 2: 独立认知内核 agent [Tier: 2-Standard] [Mode: AFK] [Subagent: TDD Builder]

**Files:**
- Create: `agent/trace.go`（TraceStep 契约与 TraceBuffer 环形缓冲）
- Create: `agent/trace_test.go`（从 worker 迁移并增强 trace 测试）
- Create: `agent/engine.go`（ReAct 多步循环与 Chatter 接口）
- Create: `agent/engine_test.go`（Agent 循环与步进回调测试）

**Interfaces:**
- Consumes: `agent/tool.Registry` · `llm.Message` · `llm.Tool` · `llm.AssistantTurn`
- Produces:
  ```go
  package agent

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

  type TraceBuffer struct { ... }
  func NewTraceBuffer(max int) *TraceBuffer
  func (b *TraceBuffer) Append(s TraceStep)
  func (b *TraceBuffer) Snapshot() []TraceStep

  type Chatter interface {
      ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (*llm.AssistantTurn, error)
  }

  const MaxAgentSteps = 5
  func Run(ctx context.Context, c Chatter, prompt string, reg *tool.Registry, onStep func(TraceStep)) (string, error)
  ```

**Injected Instincts:**
- [ ] `[Instinct: Secret-Redaction]`: `error` 步骤严禁暴露原始错误全文（可能包含上游 HTTP 响应或敏感账单），只记录粗粒度分类（`"upstream error"`、`"max steps exceeded"`）。
- [ ] `[Instinct: Nil-Safety]`: `onStep` 为 nil 或 `reg` 为 nil 时具备默认防御，绝不 panic。
- [ ] `[Instinct: Concurrency]`: `TraceBuffer` 内部读写持锁，`Snapshot()` 必须返回切片独立拷贝。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 2: 独立认知内核 agent.
> Mode: AFK | Role: TDD Builder.
> Goal: 抽取独立的 agent 认知包，包含 TraceStep 契约、TraceBuffer 环形缓冲以及 ReAct 核心循环 Run。
> Target Files: Create `agent/trace.go`, `agent/trace_test.go`, `agent/engine.go`, `agent/engine_test.go`.
> TDD Steps:
> 1. 编写 RED 测试：`agent/trace_test.go`（锁死 JSON 契约、缓冲溢出淘汰、快照深拷贝、截断与并发测试）、`agent/engine_test.go`（模拟 1 次 tool call 交互验证顺序事件、错误粗粒度脱敏、nil 回调安全）。
> 2. 运行 `GOWORK=off go test ./agent/ -count=1` 确认测试转红。
> 3. 实现 `TraceStep`、`TraceBuffer`、`Chatter`、`Run`（GREEN）。
> 4. 运行 `GOWORK=off go test -race ./agent/ -count=1 -v` 验证全部通过。
> 5. 运行 `GOWORK=off go build ./...` 确认全局编译正常。
> Return: 摘要与真实终端回执（exit code + pass count）。"

**Step Breakdown:**
- [ ] **Step 1: Write the failing tests (RED)**: 编写 trace 与 engine 单元测试
- [ ] **Step 2: Run test and verify it fails**: `GOWORK=off go test ./agent/ -count=1`
- [ ] **Step 3: Implement minimal production code (GREEN)**: 实现 `agent` 核心包
- [ ] **Step 4: Run tests and verify all green**: `GOWORK=off go test -race ./agent/ -count=1 -v`
- [ ] **Step 5: Refactor & Clean code (REFACTOR)**: 提炼 emit 辅助闭包与边界防御
- [ ] **Step 6: Physical Evidence Gate**: 执行 `go test ./agent/` 检查退出码为 0
- [ ] **Step 7: Git atomic commit**: `feat(agent): extract core cognitive loop and trace buffer`

---

### Task 3: API 依赖倒置与 TaskRunner 消费者接口 [Tier: 2-Standard] [Mode: AFK] [Subagent: TDD Builder]

**Files:**
- Modify: `api/handler.go`（声明 TaskRunner 接口，替换 pool 结构体为 runner 接口，Steps 字段类型换为 `agent.TraceStep`，移除 worker import）
- Modify: `api/handler_test.go`（Steps 类型对齐，增加纯 mock runner 测试）
- Modify: `api/task_contract_test.go`（兼容性验证）

**Interfaces:**
- Consumes: `agent.TraceStep` · `store.TaskStore`
- Produces:
  ```go
  package api

  type TaskRunner interface {
      Enqueue(id string) error
      Trace(id string) []agent.TraceStep
  }

  type Handler struct {
      store   store.TaskStore
      runner  TaskRunner
      authKey string
  }
  func NewHandler(s store.TaskStore, r TaskRunner, authKey string) *Handler

  type taskWithTrace struct {
      store.Task
      Steps []agent.TraceStep `json:"steps"`
  }
  ```

**Injected Instincts:**
- [ ] `[Instinct: Clean-Architecture]`: `api/handler.go` 源码中**严禁**出现 `import "agent-api/worker"`，实现彻底的依赖倒置。
- [ ] `[Instinct: Wire-Contract]`: 保持对外 JSON 字段（`id, status, prompt, result, error, steps`）完全平坦且 100% 向后兼容。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 3: API 依赖倒置与 TaskRunner 消费者接口.
> Mode: AFK | Role: TDD Builder.
> Goal: 在 api 层定义 TaskRunner 接口，消除对 worker 的直接导入，并在测试中验证 API 可使用轻量 Mock 独立运行。
> Target Files: Modify `api/handler.go`, `api/handler_test.go`.
> TDD Steps:
> 1. 在 `api/handler_test.go` 编写 RED 测试 `TestHandler_WithMockTaskRunner`：使用一个仅 10 行的 fakeTaskRunner 结构体注入 `NewHandler`，测试 HandleRun 与 HandleGet 正常工作。
> 2. 修改 `api/handler.go`：定义 `TaskRunner` 接口；将 `Handler.pool` 改为 `Handler.runner TaskRunner`；`NewHandler` 第二个参数改为 `TaskRunner`；`taskWithTrace` 的 `Steps` 改为 `[]agent.TraceStep`；删除 `import "agent-api/worker"`，改为 `import "agent-api/agent"`（GREEN）。
> 3. 更新 `api/handler_test.go` 中的 `Steps []worker.TraceStep` 为 `[]agent.TraceStep`。
> 4. 运行 `GOWORK=off go test -race ./api/ -count=1 -v` 验证全部 13+ 个用例通过。
> 5. 检查 `api/handler.go` 确认没有任何对 `worker` 的 import。
> Return: 摘要与真实终端回执（exit code + pass count）。"

**Step Breakdown:**
- [ ] **Step 1: Write the failing tests (RED)**: 编写使用 Mock TaskRunner 的解耦测试
- [ ] **Step 2: Run test and verify it fails**: `GOWORK=off go test ./api/ -run TestHandler_WithMockTaskRunner`
- [ ] **Step 3: Implement minimal production code (GREEN)**: 在 handler.go 声明 TaskRunner 接口并重构
- [ ] **Step 4: Run tests and verify all green**: `GOWORK=off go test -race ./api/ -count=1 -v`
- [ ] **Step 5: Refactor & Clean code (REFACTOR)**: 确保包导入干净无残留
- [ ] **Step 6: Physical Evidence Gate**: 执行 `go test ./api/...` 并确认退出码 0
- [ ] **Step 7: Git atomic commit**: `refactor(api): invert dependency via TaskRunner interface and decouple from worker`

---

### Task 4: 并发池 worker 纯化与大杂烩清理 [Tier: 2-Standard] [Mode: AFK] [Subagent: TDD Builder]

**Files:**
- Modify: `worker/pool.go`（纯化为只管并发池与任务调度，引入 `tool.Registry` 与 `agent.TraceBuffer`，代理至 `agent.Run`）
- Modify: `worker/pool_test.go`（引用对齐 `agent.TraceStep` 与 `agent.KindFinal`）
- Modify: `worker/pool_bench_test.go`（基准测试调用对齐 `agent.Run`）
- Delete: `worker/agent.go`（已移至 `agent/` 与 `agent/tool/`）
- Delete: `worker/agent_test.go`（已移至 `agent/` 与 `agent/tool/`）
- Delete: `worker/trace.go`（已移至 `agent/trace.go`）
- Delete: `worker/trace_test.go`（已移至 `agent/trace_test.go`）

**Interfaces:**
- Consumes: `agent.Run` · `agent.TraceStep` · `agent.TraceBuffer` · `agent/tool.DefaultRegistry`
- Produces:
  ```go
  package worker

  type Pool struct {
      queue       chan string
      tasks       store.TaskStore
      size        int
      taskTimeout time.Duration
      ctx         context.Context
      cancel      context.CancelFunc
      wg          sync.WaitGroup
      llm         agent.Chatter
      tools       *tool.Registry
      tracesMu    sync.Mutex
      traces      map[string]*agent.TraceBuffer
  }

  func NewPool(size int, taskTimeout time.Duration, s store.TaskStore, client agent.Chatter) *Pool
  func (p *Pool) Enqueue(id string) error
  func (p *Pool) Trace(id string) []agent.TraceStep
  ```

**Injected Instincts:**
- [ ] `[Instinct: Single-Responsibility]`: `worker` 仅保留队列管理、协程池限流、超时控制、优雅停机与任务终态扭转，纯化为 Job Concurrency Executor。
- [ ] `[Instinct: Delete-Dead-Code]`: 被抽离到独立包的代码物理删除，严禁在 `worker` 遗留重复实现。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 4: 并发池 worker 纯化与大杂烩清理.
> Mode: AFK | Role: TDD Builder.
> Goal: 改造 worker.Pool 使其依赖 agent.Run 与 tool.DefaultRegistry()，并物理删除 worker 目录中旧的 agent.go、agent_test.go、trace.go、trace_test.go。
> Target Files: Modify `worker/pool.go`, `worker/pool_test.go`, `worker/pool_bench_test.go`, Delete `worker/agent.go`, `worker/agent_test.go`, `worker/trace.go`, `worker/trace_test.go`.
> TDD Steps:
> 1. 修改 `worker/pool.go`：将 `traces` 字段类型更新为 `map[string]*agent.TraceBuffer`；在 `NewPool` 中初始化 `p.tools = tool.DefaultRegistry()`；在 `process` 中调用 `agent.Run(taskCtx, p.llm, task.Prompt, p.tools, tb.Append)`；`Trace(id)` 返回 `tb.Snapshot()`；删除旧的 `defaultTools`、`calcTool`、`timeTool`、`execTool`、`evalArithmetic` 等函数。
> 2. 物理删除 `worker/agent.go`、`worker/agent_test.go`、`worker/trace.go`、`worker/trace_test.go`。
> 3. 更新 `worker/pool_test.go` 与 `worker/pool_bench_test.go` 中的引用（如 `worker.TraceStep` -> `agent.TraceStep`，`worker.KindFinal` -> `agent.KindFinal`）。
> 4. 运行 `GOWORK=off go test -race ./worker/ -count=1 -v` 验证并发池所有测试全部通过。
> 5. 运行 `GOWORK=off go build ./...` 验证全局构建干净。
> Return: 摘要与真实终端回执（exit code + pass count + 删除文件清单）。"

**Step Breakdown:**
- [ ] **Step 1: Update pool.go & pool_test.go**: 接入 agent 与 agent/tool 依赖
- [ ] **Step 2: Physically delete redundant files**: 删除 `worker/agent*.go` 与 `worker/trace*.go`
- [ ] **Step 3: Run worker tests and verify green**: `GOWORK=off go test -race ./worker/ -count=1 -v`
- [ ] **Step 4: Verify whole project compilation**: `GOWORK=off go build ./...`
- [ ] **Step 5: Refactor & Clean code (REFACTOR)**: 确认 Pool 无任何死代码遗留
- [ ] **Step 6: Physical Evidence Gate**: 执行 `go test ./worker/...` 并确认退出码 0
- [ ] **Step 7: Git atomic commit**: `refactor(worker): purify worker pool as concurrency executor and prune kitchen sink`

---

### Task 5: 架构防线门禁与全仓回归 [Tier: 1-Light] [Mode: AFK] [Subagent: Solo]

**Files:**
- Create: `api/architecture_barrier_test.go`（编写静态测试断言 `api/handler.go` 源码不含 `"agent-api/worker"`，断言 `go list` 依赖树隔离）
- Modify: `main.go`（复核主程序依赖装配与停机生命周期）
- Test: 全仓回归

**Interfaces:**
- Consumes: 全仓各包
- Produces: `func TestArchitectureBarrier_APIDoesNotImportWorker(t *testing.T)`

**Injected Instincts:**
- [ ] `[Instinct: Architectural-Fitness]`: 将架构分层约束物化为自动化测试用例，从 CI / 本地测试级别严防反向依赖劣化。

**Subagent Prompt Scaffold (for /dfs-exec):**
> "Implement Task 5: 架构防线门禁与全仓回归.
> Mode: AFK | Role: Solo.
> Goal: 添加架构防线测试固化解耦收益，执行全项目回归与静态检查。
> Target Files: Create `api/architecture_barrier_test.go`.
> Steps:
> 1. 创建 `api/architecture_barrier_test.go`，测试读取 `api/handler.go` 文本内容断言其不含字符串 `\"agent-api/worker\"`。
> 2. 运行 `GOWORK=off go test ./api/ -run TestArchitectureBarrier_ -v` 验证通过。
> 3. 运行全仓测试：`GOWORK=off go test -race ./... -count=1`，统计并断言通过数 $\ge 57$。
> 4. 运行静态检查：`GOWORK=off go build ./... && GOWORK=off go vet ./...` 确保 0 警告 0 报错。
> Return: 物理终端回执（exit code + 全仓测试总数 + git status）。"

**Step Breakdown:**
- [ ] **Step 1: Write architecture barrier test**: 编写防线静态单测
- [ ] **Step 2: Run barrier test**: `GOWORK=off go test ./api/ -run TestArchitectureBarrier_`
- [ ] **Step 3: Run full repo test with -race**: `GOWORK=off go test -race ./... -count=1`
- [ ] **Step 4: Run go vet and go build**: `GOWORK=off go build ./... && GOWORK=off go vet ./...`
- [ ] **Step 5: Physical Evidence Gate**: 输出完整回归对账单
- [ ] **Step 6: Git atomic commit**: `test(api): add architecture barrier test and complete ADR-0013 decoupling`

---

## 🌫️ Fog of War (Not Yet Specified)

- **[Unknown 1] 动态工具参数 JSON Schema 校验器**: 当前各工具采用结构体 `json.Unmarshal` 做基础解析。未来是否引入完整的 jsonschema 库，取决于后续是否需要支持第三方动态加载工具——在此之前不引入额外依赖。
- **[Unknown 2] 多模型动态切换与 Fallback 链**: ADR-0013 完成了内核与工具解耦，为模型切换打好了基础；多模型 Fallback 链规划在后续专门 ADR 中探讨。

---

## 🚫 Out of Scope

- 引入反射型依赖注入框架（Wire/Dig/Fx）：坚持 Go 原生手动构造装配（`main.go` Composition Root）。
- 重构存储层 `TaskStore`（保持既有接口不变）。
- 改动对外 HTTP API JSON 字段命名。
- 引入除标准库与现有依赖之外的任何新第三方包。
