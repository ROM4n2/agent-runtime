# ADR-0021：上游限流退避重试、Token 成本可观测与诊断端口

- **日期**：2026-10-27
- **状态**：已实施（P10 里程碑）
- **上游依赖**：ADR-0004（YAGNI 有界失败论）、ADR-0011（任务超时与优雅停机）、ADR-0019（事务性任务队列与流式推断）、ADR-0020（会话状态机与上下文压缩）
- **下游影响**：`llm/retry.go` 退避重试、`llm.Client` 三处调用点、`metrics` 三个新指标、`store.Task` 两个 token 字段与 `RecordUsage`、`worker/usage.go`、`worker/cancel.go` 拆分、`config.SessionHistoryLimit` / `config.PprofAddr`、根级 `pprof.go`
- **上位规范**：vault `01-Rules/LAYERED-ARCHITECTURE.md`（同包文件拆分与行数门禁）

---

## 背景与痛点

P0–P9 之后，运行时已经具备事务性队列、崩溃恢复、抢占取消、SSE 流式与会话压缩。但三处**生产运维面的缺口**在压测与故障复盘里暴露出来：

1. **上游瞬时失败被当成终态失败（最贵的一条）**。
   `llm.Client` 对任何非 200 直接构造 `APIError` 返回。DeepSeek 在配额边界、滚动发布、网关抖动时返回 `429` / `503` 并附 `Retry-After`，而我们把它当成任务失败写进 `task.error`，客户端看到的是 `upstream error`。这类失败**重试一次就成功**，但当前实现把可恢复故障和真故障混为一谈：既浪费已完成的 agent 步进（`MaxAgentSteps=5` 走到第 4 步被限流，整轮作废），又把「配额问题」误报成「服务故障」误导排障方向。
2. **Token 成本完全不可见**。
   `llm.Usage` 只在评测 harness（`eval/run.go`）里被消费，服务运行期既不进 `/metrics`，也不落库。运维只能看「完成了多少任务」，无法回答「今天花了多少 token」「哪个任务最贵」「一次失败重试消耗了多少」。成本是 LLM 服务的一等运营指标，缺失它等于没有账单对账能力。
3. **会话历史窗口与诊断能力都是硬编码**。
   `worker/session.go:28` 的 `GetSessionMessages(ctx, sessionID, 100)` 把历史窗口写死；不同模型上下文窗口差异很大（16k vs 128k），改窗口必须改代码重新发版。同时进程内没有任何 `pprof` 端点——goroutine 泄漏、内存增长这类问题只能靠重启复现。

---

## 决策

### 决策 1：可重试失败分类 + `Retry-After` 尊重 + 有界重试

在 `llm` 包新增 `retry.go`，把「发请求」抽成 `doWithRetry(ctx, newReq)`：

- **可重试状态码白名单**：`429` / `500` / `502` / `503` / `504`。其余 4xx（`400` / `401` / `403` / `404` /`422`）**绝不重试**——鉴权错、请求体错、上下文超限重试一万次也是同样的错，只会把一次失败放大成 N 次计费。
- **退避节奏**：`BaseDelay × 2^(attempt-1)`，单次等待上限 `MaxDelay`；默认 `MaxAttempts=3`（含首次）、`BaseDelay=500ms`、`MaxDelay=5s`。
- **`Retry-After` 优先**：上游给了 `Retry-After`（秒数或 HTTP-date）就按它等，仍受 `MaxDelay` 封顶——上游可以要求我们等 60s，但占住一个 worker 60s 的代价高于放弃这次重试。
- **可取消**：退避用 `select { case <-timer.C: case <-ctx.Done() }`，任务超时/用户取消/停机广播在退避期间同样生效，不会出现「已取消但还在睡」的假在飞。
- **每次重试重新构造请求**：`http.Request.Body` 是不可重读的 `io.Reader`，`newReq` 必须是闭包，每次返回全新 request + 全新 `bytes.Reader`。
- **错误契约不变**：重试耗尽后仍返回最后一次的响应/错误，`APIError` 语义与 P9 之前完全一致，上层 `worker` 的终态映射不需要改。
- **重试计数外露**：`Client.OnRetry(attempt, status, delay)` 回调。`llm` 在 AST 白名单里允许的 import 集为空，不能直接依赖 `metrics`，因此用回调由装配层注入（与 `metrics.SetPendingSource` 同一手法）。

**流式的不变式（关键）**：`ChatWithToolsStream` 只在**响应体尚未被消费**时重试（即非 200 响应或连接建立失败）。一旦流开始下发 `delta` 给客户端，任何重试都会把已经打出去的字重复一遍，打字机体验直接崩坏。因此流式中途断流**不重试**，按失败处理——这是有意的取舍：确定性优先于「多试一次」。

### 决策 2：Token 用量双通道（进程级指标 + 任务级持久化）

- **进程级**：`metrics` 新增 `agent_llm_prompt_tokens_total` / `agent_llm_completion_tokens_total` 两个 counter，由 `metrics.AddTokens(prompt, completion)` 累加。零依赖、`sync/atomic`、Prometheus 文本格式，与既有 6 个指标同构。
- **任务级**：`store.Task` 新增 `prompt_tokens` / `completion_tokens` 字段（JSON 对外可见），`TaskStore` 新增 `RecordUsage(id, promptTokens, completionTokens) error`；SQLite 侧加两列 `INTEGER NOT NULL DEFAULT 0` 并走既有 `ALTER TABLE ... duplicate column name` 容错迁移模式（老库无损升级）。
- **采集点**：`worker/usage.go` 的 `usageAccumulator` 作为 `RunOptions.OnUsage` 回调累加一次任务内所有步进的用量；`process` 在 `agent.RunWithOptions` 返回后**无论成功失败都记录**——失败的调用照样计费，漏记失败任务的用量会让账单对不上。
- **为什么不做成 `Complete` 的参数**：`Complete` 是状态机终态裁决点，把计费数据塞进去会让「状态一致性」和「计量」两个关注点耦合；独立 `RecordUsage` 也允许未来在任务执行中途落一次中间用量。

### 决策 3：会话历史窗口配置化

`config.SessionHistoryLimit`（默认 100，`session_history_limit` / `SESSION_HISTORY_LIMIT`）→ `Pool.SetSessionHistoryLimit` → `assembleSessionMessages`。理由：这是**模型能力参数**而非代码常量，128k 窗口的模型配 100 条历史是浪费，16k 的模型配 100 条则会被 `CompactMessages` 反复折叠、白跑压缩。

### 决策 4：`pprof` 独立端口、默认关闭

- `config.PprofAddr`（默认空 = 不启用，`pprof_addr` / `PPROF_ADDR`）。
- 非空时在**独立端口的独立 mux** 上注册 `/debug/pprof/*`，不挂进主 mux：主 mux 的 `/` 是公开 Demo 页，把 profile 端点混进去等于把堆栈和 goroutine 快照暴露给公网。
- 监听非回环地址时打 `WARN` 日志——容器/内网抓 profile 是合理用法，但必须让运维在日志里看到「你把它开到了 0.0.0.0」。

---

## 被否决的方案

| 方案 | 否决理由 |
|---|---|
| 引入 `hashicorp/go-retryablehttp` | 依赖一个库只为 60 行退避逻辑，违反 ADR-0004；且它不理解「流式已产出不可重试」这条本项目专属不变式 |
| 无限重试 / 重试到成功 | 有界失败论（ADR-0004）：无限重试会把上游限流转成自身 goroutine 堆积，最终拖垮进程 |
| 把 `Usage` 只写进 `TraceBuffer` | trace 是 1000 条 FIFO 环形缓冲，会被驱逐；成本数据必须可持久查询 |
| 用 `Complete` 承载 token 字段 | 状态机终态裁决与计量耦合，且失败路径的用量记录会被状态语义绑架 |
| `pprof` 挂进主 mux 加鉴权中间件 | `pprof` 的 index 页会枚举所有 profile，鉴权只保护入口不保护内容；独立端口 + 默认关闭是更小的暴露面 |
| 给流式断流也做重试 | 会重复下发已产出的 delta，客户端看到重复文本；确定性优先 |

---

## 后果

**正向**
- 上游 `429`/`5xx` 抖动不再把整轮 agent 步进作废，成功率与 token 浪费同时改善；`agent_llm_retries_total` 让「上游不稳」从猜测变成可观测事实。
- 成本从「不可见」变成「进程级 counter + 任务级字段」，`GET /tasks/{id}` 直接给出该任务花了多少 token。
- 运维可以按模型窗口调历史深度；线上 goroutine/内存问题可以用 `pprof` 现场取证而不必重启复现。

**负向 / 代价**
- 重试会延长单任务在飞时间，最坏情况 `500ms + 1s = 1.5s` 额外延迟（`MaxAttempts=3`）。任务超时预算（默认 30s）不变，退避被 ctx 取消覆盖，不会突破预算。
- `tasks` 表新增两列，老库首次启动执行一次 `ALTER TABLE`（瞬时，WAL 下不阻塞读）。
- 流式断流仍然不可恢复——这是已知边界，需在文档中显式声明，避免被当成 bug。

---

## 验证口径

- `llm/retry_test.go`：`429` + `Retry-After: 0` 后成功 → 断言请求次数 2、`OnRetry` 收到 status=429；`400` → 断言只请求 1 次；重试耗尽 → 断言返回 `*APIError` 且 `StatusCode` 为最后一次的状态；退避期间 ctx 取消 → 断言立即返回且错误可被 `errors.Is(err, context.Canceled)` 识别；流式已产出 delta 后断流 → 断言不重试。
- `store`：`RecordUsage` 内存/SQL 双实现断言回读一致；SQL 侧 `EXPLAIN`/`PRAGMA table_info` 断言列存在，老库迁移不报错。
- `metrics`：`AddTokens` / `IncLLMRetry` 后 exposition 文本包含对应行。
- `worker`：`process` 失败路径同样落 usage（构造一个「第一步成功、第二步失败」的假 chatter）。
- 架构门禁：`pool.go` 拆出 `cancel.go` 后仍在 400 行预算内。
