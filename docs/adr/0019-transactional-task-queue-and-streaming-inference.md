# ADR-0019：事务性持久化任务队列与 SSE 实时打字机流式推断

- **日期**：2026-10-25
- **状态**：已实施（P7 里程碑通过，2026-10-25）
- **上游依赖**：ADR-0002（异步任务与并发池）、ADR-0005（轮询 vs 推送）、ADR-0010（SQLite 持久化）、ADR-0013（分层解耦）、ADR-0014（抢占式取消）、ADR-0016（SSE 任务进度流）
- **下游影响**：`store.TaskStore` 扩展、`worker.Pool` 队列模型重构、`llm.Client` 流式解析、`agent.RunWithOptions` 增量回调、`api/events.go` 流式帧推送
- **上位规范**：vault `01-Rules/LAYERED-ARCHITECTURE.md`（依赖单向性与分层契约）、`01-Rules/CONCURRENCY-PATTERNS.md`

---

## 背景与痛点

在 P0–P6 演进中，系统已具备了成熟的异步 ReAct 认知循环、抢占式取消、崩溃恢复与混合知识检索能力。但在面对工业级生产流量与交互体验时，仍存在两个根本性的架构瓶颈：

1. **内存通道背压瓶颈与丢单风险（In-Memory Channel Bottleneck）**：
   - 当前 `worker.Pool` 依赖固定容量为 3 的内存 Go channel（`queue chan string`）。
   - 遭遇并发突发时，第 4 个并发请求在 `select + default` 处被判定为队列满，直接向客户端返回 `HTTP 503`。在业务层面，503 等同于直接拒绝用户请求、造成丢单。
   - 在持久化模式下，虽然 `RecoverTasks` 可在重启时扫描 SQLite 中的 pending 任务，但依赖后台 goroutine 阻塞往 channel 中重灌（`EnqueueWait`）。当积压数较多时，内存 channel 成为不必要的吞吐咽喉。
2. **缺乏 Token 级打字机实时回显（Lack of Token-by-Token Streaming）**：
   - 当前 SSE（`GET /tasks/{id}/events`，ADR-0016）仅在单步执行完毕（`TraceStep` 产生）时推送 `event: step`。
   - 当 Agent 在某一轮生成 500~1000 字的详细推导或终态回答时，LLM 调用持续 5~15 秒。在此期间，SSE 只能发送 `: keepalive`，客户端无法呈现打字机动效，交互等待感知强烈。

---

## 决策

### 1. 采用基于 SQLite WAL 的事务性任务队列（Transactional Task Queue）

摒弃脆弱的定长内存 channel，采用基于现有 SQLite WAL 的 **Outbox 事务抢占调度模式**：

- **原子落库即入队**：`store.Create(prompt)` 在单个事务中写入任务，状态即为 `pending`，无需任何跨进程协调，保证落库即进入队列。
- **原子事务抢占（Transactional Claim）**：
  在 `store.TaskStore` 接口中定义：
  ```go
  ClaimNext(ctx context.Context) (Task, error)
  PendingCount(ctx context.Context) (int64, error)
  ```
  在 `store/sql.go` 中，利用 SQLite `BEGIN IMMEDIATE` 独占写事务原子完成「读取最老 pending 任务 + 更新为 running」：
  ```sql
  SELECT id, prompt FROM tasks WHERE status = 'pending' ORDER BY id ASC LIMIT 1;
  UPDATE tasks SET status = 'running', updated_at = ? WHERE id = ? AND status = 'pending';
  ```
- **条件唤醒与轮询兜底（Push-Notify Pull-Worker）**：
  - `worker.Pool` 不再持有 `chan string`，改为持有无缓冲通知信标 `wakeUp chan struct{}`；
  - `Enqueue(id)` 执行非阻塞唤醒投递：`select { case p.wakeUp <- struct{}{}: default: }`；
  - 空闲 Worker 在 `ClaimNext` 失败时阻塞等待 `wakeUp` 信号或 100ms 兜底定时器（防虚假信号与唤醒合并）。
- **真实异步削峰填谷**：
  - 背压通过配置项 `max_queue_size`（默认 10,000）控制；
  - 只要当前排队数未超上限，所有请求均能在数毫秒内获得 `202 Accepted`，由后台 Worker 队列稳健消化，彻底消除突发流量下的 503 丢单。

### 2. 引入 OpenAI/DeepSeek 兼容的 SSE 流式推断与级联广播

在不破坏现有非流式方法与单测的前提下，引入原生 Token 级打字机流式支持：

- **`llm.Client` 流式接口**：
  新增方法：
  ```go
  type DeltaHandler func(delta string)
  func (c *Client) ChatWithToolsStream(ctx context.Context, msgs []Message, tools []Tool, onDelta DeltaHandler) (*AssistantTurn, error)
  ```
  向 `/v1/chat/completions` 发送 `stream: true`，利用 `bufio.Reader` 逐帧解析上游 SSE，实时提取 `delta.content` 并回调 `onDelta`；在流末尾汇总工具调用参数及 `Usage` 结构。
- **`agent.RunWithOptions` 管道透传**：
  在 `agent.RunOptions` 中扩展 `OnDelta func(step int, delta string)` 回调，在模型思考与文本生成时实时上浮。
- **`TraceBuffer` 与 `api/events.go` 实时广播**：
  - `agent.TraceBuffer` 新增轻量级事件广播，向订阅通道推送 `event: delta`：
    ```text
    event: delta
    data: {"step":1,"text":"根据"}

    event: delta
    data: {"step":1,"text":"检索"}
    ```
  - `api/events.go` 在收到增量时立即通过 `w.(http.Flusher).Flush()` 推送给浏览器。
  - **优雅自愈**：快照仍然存储完整 `TraceStep`。如果客户端断网重连，重连后先获取完整历史快照，再衔接实时增量，平衡打字机体验与状态一致性。

---

## 边界与防腐约定

1. **依赖方向守恒**：
   - 依赖严格遵循 `architecture_test.go` AST DAG 白名单，禁止 `worker` 越权依赖未允许的包；
   - `llm` 保持底层叶子包地位，流式解析完全内聚于 `llm`，不引入外部第三方流式框架。
2. **零新外部依赖（Zero-Dependency Invariant）**：
   - 继续坚持纯 Go 标准库（`bufio`、`net/http`、`sync`、`context`）+ `modernc.org/sqlite`，绝不引入外部 Redis/RabbitMQ/Kafka。
3. **单文件体积预算（Line Budget ≤ 400 行）**：
   - `llm/client.go` 的流式解析逻辑拆至同包 `llm/stream.go`；
   - `worker/pool.go` 的任务拉取调度逻辑若超标则拆至同包 `worker/dispatcher.go`；
   - 生产文件严格受 `TestArchitecture_FileLineBudget` 守卫保护。

---

## 后果与价值

- ✅ **消除 503 丢单**：突发流量平滑进入 SQLite 持久化队列，吞吐缓冲能力从 3 跃升至 10,000+。
- ✅ **打字机实时体验**：SSE 首字返回延迟（TTFT）从 5~15 秒大幅降低至 300~800 毫秒。
- ✅ **状态一致性提升**：彻底解决停机与崩溃时内存 channel 中丢弃任务的问题，任务调度完全基于数据库事务。
