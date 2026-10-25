# 项目深度 Q&A 复盘手册

本文件整理自 2026-08-17 模拟对线，记录本项目核心的并发、网络、存储和接口设计问题。

## 核心问答汇总

### Q1：既然 Goroutine 极轻量，为什么还要在 [worker/pool.go](../../worker/pool.go) 里用 Worker Pool 限制并发数？
*   **标准回答**：
    > **限的从来不是 Goroutine，而是具体任务执行的资源成本。**
    > Goroutine 本身虽便宜（初始栈仅 2KB），但其执行的任务（LLM API 调用）极其昂贵。如果不设限，无限并发会引发以下后果：
    > 1. **上游限流警告**：触发模型服务商的速率限制，返回 `HTTP 429 Too Many Requests`，甚至直接封禁 IP。
    > 2. **账单成本失控**：单位时间内 Token 消费速度极高，极易超出预算。
    > 3. **击穿下游资源**：在生产环境中，这会瞬间打满下游数据库连接池，造成服务瘫痪。
    > 因此，使用 Worker Pool 限制并发数是一个典型的**业务与成本控制权衡**。

### Q2：在 [store/store.go](../../store/store.go) 里，`Get()` 方法为什么返回结构体副本（值传递），而不是指针 `*Task`？
*   **标准回答**：
    > **核心目的是消除数据竞争（Data Race），保障并发安全。**
    > 如果返回指针 `*Task`，外部调用方（如 HTTP Handler）在拿到指针后，会在锁外直接进行读写。此时如果后台 Worker Goroutine 也在并发修改该 Task 的内部状态，就会产生无锁并发读写，导致 Data Race。
    > **性能考量**：[`Task`](../../store/store.go#L38) 结构体非常小，其大部分字段为 `string`。在 Go 中，`string` 底层仅拷贝指针和长度标头（16字节），不拷贝文本实体。因此，值传递的内存开销微乎其微。这是**用极低的内存代价换取绝对并发安全**的经典设计。

### Q3：如果任务队列满了，[`Enqueue`](../../worker/pool.go#L114) 会发生什么？会有什么影响？该如何修复？
*   **标准回答**：
    > **演进背景**：早期版本中，如果队列满，`Enqueue` 是定长 channel 裸发送，会在 [`HandleRun`](../../api/handler.go#L42) 中发生阻塞，导致 HTTP Goroutine 挂起，异步退化为同步。
    > **P7 终极方案（事务持久队列 + Push-Notify 抢占）**：
    > 1. **落库即入队**：`store.Create` 写入即为 `pending` 状态，进入持久化存储；
    > 2. **削峰背压**：通过 `max_queue_size`（默认 10,000）控制积压上限，`Enqueue` 仅非阻塞投递给容量为 1 的 `wakeUp` 信标。未超载时快速返回 `202 Accepted`，超载时才快速返回 **`HTTP 503 Service Unavailable`** + `Retry-After: 1` 并清理孤儿记录；
    > 3. **原子事务抢占**：Worker 在 `ClaimNext` 中通过原子 CAS 事务（`UPDATE tasks SET status='running' WHERE id=? AND status='pending'`）主动拉单，彻底根治并发丢单与停机孤儿任务。

### Q4：`worker` 包需要调用大模型，却并没有导入 `llm` 包；在接入检索后，也没有导入 `retrieval` 包，这是怎么实现的？为什么要这么设计？
*   **标准回答**：
    > 采用 **依赖倒置原则（DIP）与协议下沉（Protocol Lowering）**。
    > 1. **LLM 解耦**：在 `agent` 包定义小接口 `agent.Chatter`，`worker` 仅面向接口消费，实际由外部注入实现。
    > 2. **检索错误解耦**：当工具发生特定失败（如检索索引未建立），上层调度器需要将其分类为任务终态 `"index missing"`。如果直接让 `worker` 去 import `retrieval.ErrIndexMissing`，就会造成调度池反向耦合具体工具实现。
    > 3. **解决方案**：我们在底层契约包 `agent/tool` 中下沉定义了 `TerminalClassifier` 接口：
    >    ```go
    >    type TerminalClassifier interface {
    >        error
    >        TerminalStatus() string
    >    }
    >    ```
    >    由 `retrieval` 内部的未导出错误类型实现该方法返回 `"index missing"`，`worker` 内部仅需使用 `errors.As(err, &tc)` 判定，彻底移除了对 `retrieval` 包的 import 依赖。
    >    此依赖单向性由根目录的 `architecture_test.go` AST DAG 白名单进行每次编译必测的机械防护。

### Q5：在 [store/store.go](../../store/store.go) 里，`Update` 和 `Complete` 分离的设计初衷是什么？
*   **标准回答**：
    > **为了保护“状态与数据的一致性约束（State Invariants）”。**
    > 任务的完成具有“成功”与“失败”两个互斥的终态，它们绑定的数据大不相同：
    > *   若成功：状态必为 `done`，且只有 `Result` 有值。
    > *   若失败：状态必为 `failed`，且只有 `Error` 有值。
    > 如果只提供通用的 `Update` 接口，外部 Worker 调用时可能会拼出矛盾的状态（如“done”却带 error）。因此我们通过 [`Complete`](../../store/store.go#L96) 方法收归这个逻辑：传入 `err` 是否为 `nil` 决定终态和数据写入，在 Store 内部完成规则闭环，防止外部逻辑写坏数据。

### Q6：SSE 流式接口是如何同时兼顾逐字打字机推断与断连自愈的？
*   **标准回答**：
    > **双轨分层设计（Dual-Track Event Model）**：
    > 1. **原生流式推断**：`llm.Client` 实现 `ChatWithToolsStream` 逐行解析上游 SSE，通过回调将增量 token 实时上浮至 `agent.RunWithOptions` 的 `OnDelta`。
    > 2. **事件分层广播**：`TraceBuffer` 维护完整的步进历史切片 `steps`（容量 32），同时向流式订阅通道推送 `event: delta`（逐字增量）与 `event: step`（步骤完成）。
    > 3. **优雅自愈与降级**：客户端断网重连时，`SubscribeEvents` 首先原子地返回历史步骤快照，重放历史，再接入后续实时增量；任务终态前排空增量队列，最后下发 `event: done`，兼顾打字机流畅感与离线对账可靠性。
