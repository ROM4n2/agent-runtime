# ADR-0015：状态机幂等与掉电崩溃恢复（Crash-Recovery / Rehydration）

- **日期**：2026-10-22
- **状态**：已接受
- **上游依赖**：ADR-0010（SQLite 持久化）、ADR-0011（超时与优雅停机）

## 背景

任务状态写入 SQLite，但派发依赖内存 `chan string`。若进程在执行中被 Kill 或掉电：

- DB 中处于 `pending` 的任务永远不会再被消费（幽灵积压）；
- DB 中处于 `running` 的任务对应的 goroutine 已消失，客户端会永久轮询到一个不变的状态。

单机场景下，这是“持久化只做了一半”的典型缺陷。

## 决策

1. **状态机闭环**：`pending`（已落盘提交）→ `running`（worker 已领单）→ `done / failed`（终态）。
   终态仍只由 `TaskStore.Complete` 裁决。
2. **存储层新增补偿接口**：
   ```go
   RecoverTasks(ctx context.Context) (pendingIDs []string, recoveredRunning int, err error)
   ```
   - `pending`：按自增主键升序返回，保证重放的提交顺序；
   - `running`：上一次进程非正常死亡的残留，批量更新为
     `status='failed', error='server restarted unexpectedly'`。
   - SQL 实现把「查询 pending + 批量标死 running」放在**同一事务**内；
     内存实现保持相同语义，便于测试与后端可插拔。
3. **启动顺序**：`main` 在 `p.Start()` 前调用 `RecoverTasks`，随后把 pending 任务
   通过新增的 `Pool.EnqueueWait(ctx, id)` **阻塞式**重灌，再接受 HTTP 流量。
   - 用 `EnqueueWait` 而非 `Enqueue`：积压可能超过队列容量，等待空位而不是再次静默丢弃；
   - 重灌放在后台 goroutine，避免拖慢监听启动；停机时中止，剩余任务仍为 `pending`，
     下次启动会再次恢复（幂等）。
4. **与优雅停机的区分**：优雅停机排空队列写入的是 `server shutdown`；
   崩溃恢复写入 `server restarted unexpectedly`，两类根因在 `task.error` 与日志中可区分。

## 候选方案

- **A（采用）**：启动补偿扫描 + 事务内修复 + 阻塞重灌。
- **B**：改用外部队列（Redis/NSQ）由 broker 保证投递。缺点：引入第三方依赖，
  违背 ADR-0004 的单机零依赖定位，面试项目中收益/成本比低。
- **C**：启动时把 running 一律重置为 pending 重跑。缺点：对已产生副作用的任务不幂等，
  可能重复调用上游/工具，风险高于收益。

## 后果

- ✅ Kill -9 / 掉电后重启：pending 任务不丢，zombie running 有明确终态，客户端不会永久挂起。
- ✅ 补偿是幂等的：重复重启只会重复扫描已修复的数据。
- ⚠️ “至少一次”语义：未被标死的任务若在终态写入前崩溃，会以 pending 重跑；
  工具调用需保持只读/可重放（当前内置工具均为无副作用计算）。
- ⚠️ 内存后端重启仍会丢任务（这是其定义），恢复逻辑对它是 no-op；
  生产持久化请启用 `db_path`。
