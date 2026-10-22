# ADR-0014：用户主动取消与因果精准可观测（POST /tasks/{id}/cancel）

- **日期**：2026-10-22
- **状态**：已接受
- **上游依赖**：ADR-0011（任务超时与优雅停机取消树）、ADR-0013（分层解耦与依赖倒置）

## 背景

此前 `context.WithCancelCause` 只在两处被动触发：单任务 30s 超时与进程优雅停机。
对外没有任何「停止生成」入口。面试追问「用户点击停止按钮，后端如何响应」时，
只能回答“等超时”，与简历宣称的「协作式抢占调度」存在口径与代码脱节。

## 决策

1. **在飞句柄表**：`worker.Pool` 新增 `inFlight sync.Map`（`taskID -> context.CancelCauseFunc`）。
   Worker 在 `agent.Run` 之前派生 `context.WithCancelCause(p.ctx)`（外层携带原因）并叠加
   `context.WithTimeout`（内层保留单次期限），登记句柄；`defer` 中删除句柄并释放 ctx。
2. **物理中断**：`Pool.Cancel(id, reason)` 命中句柄后 `cancelCause(&userCancelCause{reason})`，
   已注入 `http.NewRequestWithContext` 的上游请求立即被中断，而不是等下一次轮询/超时。
3. **因果归因**：`process` 通过 `context.Cause(taskCtx)` 配合 `errors.As` 区分：
   - 用户取消 → `canceled: <sanitized reason>`
   - `DeadlineExceeded` → `task timeout`
   - `context.Canceled`（无自定义 cause）→ `server shutdown`
   - 其余 → `max steps exceeded` / `upstream error`
4. **API 契约**：`POST /tasks/{id}/cancel`，可选 body `{"reason":"user_aborted"}`。
   - 任务不存在 → 404；任务未在飞（pending/终态）→ **409**，绝不静默成功；
   - 命中 → `202 {"id":"...","status":"canceling"}`；
   - `reason` 仅保留字母/数字/`_`/`-`/`.`，最长 64 字节，空值回落 `user_aborted`，
     防止调用方注入任意文案污染 `task.error` 并回显给其他客户端。
5. **接口归属**：`api.TaskRunner`（消费者定义接口）新增 `Cancel` 与 `Subscribe`，
   `api` 仍不 import `worker`，架构防线测试继续守护单向依赖。

## 候选方案

- **A（采用）**：cancel-cause 句柄表 + 物理中断 + 409 语义。
- **B**：只在 DB 打一个 `cancel_requested` 标记，worker 在步骤间轮询检查。
  缺点：无法中断正在阻塞的上游 HTTP 调用，延迟取决于步数；实现简单但无法证明“抢占”。
- **C**：取消时直接杀 worker goroutine。缺点：无法回收资源、store 状态可能半写，是反模式。

## 后果

- ✅ “停止生成”从请求到上游断链是毫秒级物理中断，取消原因可归因、可持久化。
- ✅ 与超时、停机共用同一条取消树，不引入第二套生命周期机制。
- ⚠️ 取消与任务自然完成存在极小竞态窗口：`Cancel` 可能命中一个刚执行完的任务。
  此时取消不影响已写入的 `done` 终态，属于可接受的幂等无害行为。
- ⚠️ 已排队但未领单的任务暂不支持取消，返回 409；客户端可等待其进入 running 再取消。
