# Design Specification: P7 Transactional Task Queue & Token Streaming Inference

**Date**: 2026-10-25  
**Status**: DRAFT  
**Author**: Maintainer & Hermes Agent  
**Target Path**: `docs/specs/2026-10-25-p7-transactional-queue-and-streaming-inference-design.md`  
**ADR Reference**: `docs/adr/0019-transactional-task-queue-and-streaming-inference.md`

---

## 1. Problem Statement & User Value

- **Why build this**:
  1. **吞吐韧性与丢单痛点**：目前 `worker.Pool` 队列为容量等于 worker 数（3）的内存 Go channel。突发流量下第 4 个请求立即被 503 快速失败拒绝，等同于向用户丢单；停机时为清理 channel 强行将排队任务标注为 `server shutdown`。生产级异步系统需要真正的**削峰填谷持久化队列**。
  2. **交互体验与打字机动效缺口**：目前 SSE 只在整步执行完毕时输出 `event: step`。对于长文本回答或复杂思考，用户需持续等待 5~15 秒，缺乏现代 LLM 产品标配的 Token 级逐字打字机（TTFT < 800ms）体验。
- **Target Scenario & Persona**:
  - Web/API 客户端调用方：提交长耗时复杂任务时，不会因短时并发激增被 503 拒绝，并通过 SSE 获得实时的逐字打字机流式输出与思考步骤回显。
  - 面试评审人与架构审查者：验证系统在面对高并发削峰、SQLite 事务抢占、网络连接管理与流式协议解析上的工程深度。
- **Cost of Doing Nothing**:
  - 维持内存通道使项目在面对面试官追问「突发 100 个请求如何削峰」时缺乏真实说服力；SSE 缺乏打字机效果使前端交互显得迟钝。

---

## 2. User Journey & Core Flow

- **Happy Path**:
  1. **任务入队（削峰不丢单）**：客户端 `POST /run` 提交 50 个高并发请求 → `store.Create` 在 SQLite WAL 中事务写入 50 条 `pending` 记录 → 系统在 5ms 内全量返回 `202 Accepted`，无任何 503 拒绝。
  2. **事务抢占调度**：后台 3 个 Worker 通过 SQLite `BEGIN IMMEDIATE` 事务无锁竞争地调用 `store.ClaimNext` 抢占排队任务，更新为 `running` 并执行。
  3. **打字机流式输出**：客户端连接 `GET /tasks/{id}/events` → 收到 `event: delta` 逐字打字帧（首字延迟 < 800ms），在工具调用时收到 `event: step`，终态收到 `event: done`。
- **Friction Points & Mitigation**:
  - **SQLite 单写锁竞争**：Worker 抢占频繁时可能造成锁冲突。采用 `BEGIN IMMEDIATE` 事务配合微秒级极简抢占语句（`SELECT ... LIMIT 1 + UPDATE`），持锁时间 < 0.5ms，单机支持 1000+ QPS 调度吞吐。
  - **网络波动与流中断**：若客户端中途断开 SSE，Worker 任务照常执行不受影响；客户端重连后先获取已完成步骤快照，再衔接后续事件。

---

## 3. Architecture & Data Models

### 3.1 事务性任务队列与抢占调度（Transactional Queue Engine）

1. **`store.TaskStore` 接口增强**：
   ```go
   // store/store.go
   var ErrNoTaskAvailable = errors.New("store: no pending task available")

   type TaskStore interface {
       // ...既有方法保持兼容...
       ClaimNext(ctx context.Context) (Task, error)
       PendingCount(ctx context.Context) (int64, error)
   }
   ```
2. **`store/sql.go` 事务抢占实现**：
   在 SQLite 事务内原子执行排它抢占：
   ```sql
   BEGIN IMMEDIATE;
   SELECT id, prompt FROM tasks 
   WHERE status = 'pending' 
   ORDER BY id ASC LIMIT 1;

   UPDATE tasks 
   SET status = 'running', updated_at = ? 
   WHERE id = ? AND status = 'pending';
   COMMIT;
   ```
   若未找到符合条件的行，回滚并返回 `ErrNoTaskAvailable`。
3. **`worker.Pool` 调度器重构（Push-Notify Pull-Worker）**：
   - 移除 `p.queue chan string`；
   - 新增 `p.wakeUp chan struct{}`（容量为 1）；
   - `Enqueue(id)` 执行非阻塞唤醒：
     ```go
     func (p *Pool) Enqueue(id string) error {
         if p.stopping.Load() {
             return ErrQueueFull
         }
         select {
         case p.wakeUp <- struct{}{}:
         default:
         }
         return nil
     }
     ```
   - Worker 循环：优先拉取 `ClaimNext`，无任务时挂起等待 `case <-p.wakeUp`、`case <-time.After(100*time.Millisecond)` 或 `case <-p.stopWorkerCtx.Done()`。
   - `config.Config` 支持 `MaxQueueSize int`（默认 10,000）。在 `api.HandleRun` 中若当前 `PendingCount >= MaxQueueSize`，才下发 503 背压。

### 3.2 LLM SSE 流式推断与级联广播（Streaming Inference）

1. **`llm/stream.go` 流式响应解析**：
   - 在 `llm` 包新增同包文件 `stream.go`（保持 `client.go <= 400` 行）：
   - `ChatWithToolsStream(ctx, msgs, tools, onDelta)` 发起带 `stream: true` 的 HTTP POST 请求；
   - 用 `bufio.Reader` 循环读取 `data: {...}` 行：
     - 若 `delta.content != ""`，调用 `onDelta(content)`；
     - 若包含 `delta.tool_calls`，按 `index` 累积函数名与参数字符串；
     - 若包含顶层 `usage`（`stream_options: {"include_usage": true}`），记录最终 Token 消耗；
     - 读取到 `data: [DONE]` 时干净结束，构造并返回完整 `*AssistantTurn`。
2. **`agent/engine.go` 与 `agent.RunOptions` 透传**：
   - `RunOptions` 新增 `OnDelta func(step int, text string)`；
   - 若 `c` 实现了带流式支持的 `StreamingChatter` 接口，在生成思考或终态回答时触发 `OnDelta`。
3. **`agent.TraceBuffer` 与 `api/events.go` 事件帧推送**：
   - `TraceBuffer` 广播支持 `TraceEvent`（联合体或包含 `IsDelta bool; Step int; Text string`）；
   - `api/events.go` 推送：
     ```text
     event: delta
     data: {"step":1,"text":"..."}
     ```
   - 保持既有 `event: step` 与 `event: done` 协议完全兼容。

---

## 4. Edge Cases & Resilience

- **SQLite 锁重试**：SQLite 在高并发写入时可能返回 `busy`，`SQLStore` 初始化时确保开启 `PRAGMA busy_timeout = 5000;` 与 `PRAGMA journal_mode = WAL;`。
- **上游流提前断开**：若 LLM 流在发送 `[DONE]` 前由于网络中断而 EOF，`ChatWithToolsStream` 必须返回明确错误，由外层 ReAct 循环或重试逻辑捕获，不得拼接出残损的 JSON 引发后续解析 panic。
- **并发停机排空**：停机时，`Pool.Stop` 直接标记 `stopping`，现有在飞 Worker 执行完当前任务即退出，未抢占的 `pending` 任务安稳留在 SQLite 中，待下次重启无缝重灌，彻底消除“停机写坏状态”的隐患。

---

## 5. Test Strategy

1. **单元测试与并发竞争**：
   - `store/sql_test.go`：多 goroutine 并发调用 `ClaimNext`，断言每个任务被且仅被一个消费者抢占，无重复消费、无数据竞争；
   - `llm/stream_test.go`：用 `httptest.Server` 模拟 OpenAI/DeepSeek 流式响应分块与丢包场景，测试参数拼接与 `Usage` 解析；
   - `worker/pool_test.go`：测试高并发任务入队削峰，断言无 503 丢失；测试两阶段停机与任务状态一致性。
2. **架构门禁保护**：
   - `architecture_test.go`：所有新老文件必须严格遵守生产代码 ≤ 400 行、测试代码 ≤ 800 行；12 包 DAG 白名单无破坏。

---

## 6. Scope Boundaries & Fog of War

- **Out of Scope**:
  - 不引入分布式外部中间件（Redis、RabbitMQ、Kafka 等）；
  - 不改动现有 12 个 Go 包的物理目录结构，优先同包切分。
- **Fog of War**:
  - 真实的 DeepSeek 流式端点在启用工具调用时的 delta 结构兼容性（先行通过 mock 规范化测试覆盖，后续真机受控验证）。
