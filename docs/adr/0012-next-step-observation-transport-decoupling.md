# ADR-0012：下一步方向 —— 拒绝全量 SSE 推送，采「观测与传输解耦」的窄口径 Agent Trace

- **日期**：2026-10-11
- **状态**：已接受
- **上游依赖**：ADR-0002（异步架构）、ADR-0004（不做持久化队列）、ADR-0005（轮询 vs 推送）、ADR-0007（优化路线 ⏳P2）、ADR-0008（工具调用循环）、ADR-0011（超时与优雅停机）

## 背景

ADR-0007 执行状态把 **P2** 标为 ⏳ 待定：「SSE 流式 token；可选 SQLite 后端」。ADR-0010 已落地
可选 SQLite，于是 SSE 成为唯一未兑现的 P2 项。用户提问「这个项目下一步应该做什么」触发本轮
双镜审讯（`/dfs-grill`）。

被审讯方案 **P2-A**：给服务增加 SSE 流式输出 + 实时进度推送（兑现 ADR-0005 触发条件 1），
并把 `GET /` 的 Demo 升级为「实时 Agent Trace 观察器」，逐步展示 `think → tool_call → observe → final`。

竞争候选：**P2-B** 可靠性加固（不新增功能，只修既有缺陷）；
**P3** 多 Agent 编排。

**刻度判定**：Track 2（架构分水岭）——P2-A 反转已接受的 ADR-0005 客户端契约，并把服务从
「无状态短请求」推向「每客户端一条长连接」，属不可逆决策，故走完整双镜审讯并归档本 ADR。

## 双镜审讯结论（四席并行，证据均以 `path:line` 标注）

审讯由四个视角**独立**完成，罕见地收敛到同一判断：**P2-A 不是「加个端点」，而是把服务现有四条
优点逐条反转**，且**事件生产的成本被严重低估**。原始记录见 `docs/grill/adr-0012-sse-rejection/`。

### 🎯 产品与用户体验层

- **信号可能自造**：ADR-0005:49 的触发信号 1 要求「UX 要求低延迟流式」，但 ADR-0007:16 里唯一
  「用户」是演示者本人 → 信号源不真实，实为简历驱动。
- **方案错位**：ADR-0005:49 字面指向**方案 D（WebSocket）**，P2-A 实为**方案 C（SSE）**；
  且 ADR-0005:43-46 已把轮询定为决策前提，升级需回原 ADR 校准而非叠加。
- **「进度」两粒度成本天差地别**：ADR-0007:59 要流式 token，ADR-0008:19 要 step 轨迹；
  见技术层——前者需改 `llm.Client` 契约，后者不必。
- **失败体验倒退**：worker=3 且 `Enqueue` 满时快速 503（`worker/pool.go:145-151`），SSE 会把
  干净的 503 变成「连接活着却永远没数据」的僵尸流，比 503 更难理解。
- **可演示性做减法**：ADR-0007:27 原痛点是「只有 curl」，而 SSE 的实时感在 curl 外不可复现。

### 🤖 技术与系统工程层（并发）

- **无订阅原语**：`TaskStore` 仅 4 方法（`store/store.go:27-32`），worker 只写 running/终态两点
  （`worker/pool.go:90,114,121`），`runAgent` 中间 turn 只进本地 `turns`、无回调点
  （`worker/agent.go:77-88`）→ 必须改 `runAgent`/`process` 边界，而 ADR-0008:33-34 承诺「对外契约不变」。
- **两棵取消树无桥接**：执行绑 `p.ctx`（`worker/pool.go:104`），订阅需绑 `r.Context()`；退订若用
  `close(ch)` 会 send-on-closed panic，需持有 worker 侧取消权。
- **背压与泄漏**：worker 向订阅者阻塞 send 时，前端断连即让 worker 卡死 → `wg.Wait()`
  （`pool.go:68`）永久阻塞 → 进程无法停机。
- **静默错误的终态缺口**：Update/Get/Complete 失败仅 slog 后 return（`pool.go:90-100,121-123`），
  任务无终态 → SSE 订阅永久挂起。

### 🛡️ SRE 层

- **限流只护速率不护并发**：`newRateLimiter(20,40)` 建连扣 1 令牌后零消耗（`handler.go:42-43`、
  `middleware.go:116-126`），对长连接并发上界贡献为 0。
- **`Recover` 对已 flush 的流失效**：只能 `superfluous WriteHeader(500)`（`middleware.go:27-39`），
  客户端收到静默截断的 200 流。
- **停机前提崩塌**：`srv.Shutdown(30s)`（`main.go:99-108`）会等活跃连接变 idle，SSE 永不结束 →
  每次停机拖满 30s；`signal→Shutdown→defer p.Stop()` 两套生命周期不一致。
- **流式与硬超时冲突**：`http.Client{Timeout:60s}` 是含读完 body 的硬上限（`llm/client.go:66`），
  >60s 流被无声截断；且全仓 `ReadTimeout/WriteTimeout/IdleTimeout` **0 匹配**（`main.go:74-77`）。
- **多实例事件真相分裂**：任务在 A 跑、订阅落 B 收不到（`store/sql.go` 无变更钩子、`pool.go:49`
  本进程队列）→ 需粘性会话或事件总线（`adr/0005:26-28`），是 SRE 债务。

### 🎨 前端与状态机层

- 现有并发控制是「`runId` 守卫式」而非真状态机（`api/demo.html:113,144-162`）；换成事件回调后
  无天然检查点，漏贴守卫即旧事件污染新输出区。
- `EventSource` 无 `close()` 即资源泄漏 + 多写入者（`demo.html:176` 只自增 `runId`）。
- 现有渲染是整块 `textContent` 重写（`demo.html:151,155,158`），O(N) 且滚动跳动；增量需
  `createElement`+`append` 容器，但节点数上限与裁剪策略未决。

### 🔗 传输层与鉴权的硬冲突（横跨三席）

`EventSource` 无法携带自定义请求头，而 `Auth` 只认 `Authorization: Bearer`
（`api/middleware.go:70`、`demo.html:115-117`）→ token 只能进 query（入 URL/日志，安全隐患）或
cookie；且 EventSource 对 401/429 不可读、原生重连约 3s 死循环、不看 `Retry-After`
（`middleware.go:134-135`），会把限流器变成重连风暴放大器。

## 候选方案

- **方案 A（推荐）**：**观测与传输解耦**。不引 SSE / 不改 `llm.Client` 契约 / 不动 ADR-0005。
  在 `runAgent` 增加 step 回调（`think` / `tool_call` / `tool_result` / `final`），进程内 ring buffer
  记录，经**现有** `GET /tasks/{id}` 新增 `steps` 字段暴露；Demo 复用 1s 轮询逐步渲染轨迹。
- **方案 B**：先做 P2-B 可靠性加固，不新增功能。
- **方案 C**：完整 P2-A（含真 token 流）。须新立 ADR 正式重估 ADR-0005，并接受 `llm.Client`
  流式契约改造 + 事件表 schema + 多实例事件亲和约束。

## 决策

**采用方案 A**，并把方案 B 中三项**边界清晰、证据确凿**的既有缺陷并入同一轮交付（见下）。

理由：**「Agent Trace 观测」与「SSE 传输」是两件可解耦的事**。现有 Demo 已在每秒轮询渲染状态；
把 step trace 作为 `GET /tasks/{id}` 的新增字段暴露，即可拿到全部演示与叙事价值
（ADR-0007:20/42 要的「记忆点」），而**零新协议、零长连接、零 `llm.Client` 契约改动、不反转 ADR-0005**，
规避上文全部风险。方案 C 的复杂度（取消树桥接、事件真相源、schema、粘性会话）远超此阶段收益，是过度工程。

本 ADR **同时确认**：ADR-0005 的「已接受轮询」决定**继续有效**，其触发信号 1 并未被真实需求兑现。

### 并入交付的既有缺陷（本轮实证确认）

1. **`Enqueue` 失败的僵尸任务**：`HandleRun` 先 `store.Create` 再 `Enqueue`，Enqueue 失败仅回 503、
   **不回收该任务**（`api/handler.go:95-100`）→ 内存 store 中积累永久 `pending` 的不可达任务。
2. **worker 早期失败的终态缺口**：`Update`/`Get` 失败即 `return`（`worker/pool.go:90-100`），
   任务永远停在 `running`/`pending`，轮询方无法收敛。
3. **服务端无慢连接超时**：`http.Server` 未设 `ReadHeaderTimeout`/`WriteTimeout`（`main.go:74-77`），
   慢客户端可无限占用连接。

## 触发重估方案 C 的条件（届时另立 ADR）

沿用 ADR-0005:48-51，未触发前坚持 YAGNI：

1. **真实 UX 需求**（非演示者自造）要求低延迟流式 token —— 此时选方案 D 并改造 `llm.Client` 流式契约；
2. 客户端规模变大，轮询空转成为可观负担；
3. 多实例部署且需推送 —— 必须与 ADR-0004 的持久化触发一并评估，配套粘性会话或消息总线。

## 与已学知识的映射

| 设计点 | 已学技能 / 规范 |
|--------|----------------|
| YAGNI / Ladder of Reuse（Level 0-2） | 1.0 指导原则、ADR-0004 |
| 取消传播与 goroutine 生命周期 | `01-Rules/CONCURRENCY-PATTERNS.md`、[[03-Languages/GO/GO-STANDARDS]] §8.4（`context` 传播取消） |
| 幂等 GET / 无状态短请求对 LB 友好 | HTTP 语义、ADR-0005 |
| 失败模式有界性优先于功能叠加 | SRE 失效分析、ADR-0004:27-28 |
| 决策记录防蒸发 | `01-Rules/CROSS-AGENT-MEMORY.md`（ADR 归档） |

## 后果

- ✅ 保住服务四条优点：无状态可扩容、失败有界、LB 友好、客户端极简。
- ✅ 以最小代价拿到「可讲、可演示」的 Agent Trace 记忆点（面试叙事强化）。
- ✅ 顺手消除三项既有缺陷（僵尸任务、终态缺口、慢连接无界）。
- ⚠️ 不做 token 级流式——若面试明确追问「流式」需如实说明这是**有依据的取舍**，并引用本 ADR。
- ⚠️ 新增 `steps` 字段会触碰 `api/task_contract_test.go` 钉死的 JSON 契约，须同步更新契约测试。
