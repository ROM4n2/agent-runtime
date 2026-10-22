# ADR-0016：实时可观测与 SSE 事件流（GET /tasks/{id}/events）

- **日期**：2026-10-22
- **状态**：已接受
- **上游依赖**：ADR-0005（轮询 vs 服务端推送）、ADR-0012（Trace 观测与传输解耦）、ADR-0013（分层解耦）

## 背景

长耗时 Agent 任务只提供 `GET /tasks/{id}` 轮询：客户端无法平滑展示“思考步骤实时回显”，
高频轮询还会浪费连接与配额。ADR-0005 已论证过轮询是当时的正确选择，但缺少一个
低成本的实时通道来补齐 Agent 场景的核心 UX。

## 决策

1. **在 `agent.TraceBuffer` 上增加订阅广播**：
   - `Subscribe() (snapshot []TraceStep, events <-chan TraceStep, cancel func())`
     在**同一把锁内**原子地取快照并注册通道，消除“先订阅后取快照”的漏步/重复；
   - `Append` 对每个订阅者**非阻塞**投递，慢消费者只丢自己的增量，绝不阻塞任务执行；
   - `cancel` 幂等：退订并 `close(ch)`，重复调用不 panic。
2. **Pool 暴露只读订阅**：`Pool.Subscribe(id)` 从 traces 注册表取出缓冲并委托订阅；
   任务尚未被 worker 领单时返回 `ok=false`，API 层退化为短轮询等待。
3. **SSE 端点**：`GET /tasks/{id}/events` 返回 `text/event-stream`：
   ```text
   event: step
   data: {"step":0,"kind":"think","detail":"calculating 2+3"}

   event: done
   data: {"id":"1","status":"done","prompt":"...","result":"5","error":""}
   ```
   - 已终态任务：回放 Trace 快照后立即推送 `done` 并关闭，不建立长连接；
   - 活跃任务：先推快照，再推增量；每 15s 发送 `: keepalive` 注释帧；
   - 每 100ms 对账一次 store 状态，终态时先排空通道、再用快照补齐（应对慢消费丢弃），
     最后推送 `done` 并退出；
   - 通过 `http.ResponseController.SetWriteDeadline(time.Time{})` 清除服务器
     `WriteTimeout`（30s）对长连接的限制，不支持该能力的 ResponseWriter 静默忽略。
4. **向下兼容**：保留 `GET /tasks/{id}` 轮询接口，客户端可自由选择轮询或 SSE。

## 候选方案

- **A（采用）**：SSE + 进程内 channel 广播。零第三方依赖，单向推送天然匹配任务状态流。
- **B**：WebSocket。缺点：需要额外握手/心跳/分片协议库或手写帧，收益与 SSE 相同，
  对“只读任务进度”属于过度设计。
- **C**：继续纯轮询。缺点：无法实时回显，长任务下的网络与配额浪费。

## 后果

- ✅ Agent 思考步骤毫秒级可回显；客户端实现只需 `EventSource`。
- ✅ 广播路径无锁竞争放大的风险：Append 持锁时间 O(订阅者数)，投递不阻塞。
- ⚠️ 每个 SSE 连接占用一个 goroutine 与一条订阅通道，需由连接数上限/限流保护；
  当前沿用现有 20 req/s 限流与 HTTP 服务器超时配置。
- ⚠️ 事件仅代表“进程内已记录步骤”；跨重启的历史步骤由轮询接口的 `steps` 快照兜底
  （TraceBuffer 本身不持久化，符合 ADR-0012 的定位）。
