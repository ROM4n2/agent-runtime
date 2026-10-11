# 审讯：方案 P2-A（SSE 流式输出 + 实时 Agent Trace 观察器）

> 视角：产品与用户体验架构师 · 苏格拉底式边界审讯
> 对象：给 agent-api 增加 SSE 流式 + 把 `GET /` 升级为「实时 Agent Trace 观察器」，
> 声称兑现 ADR-0005 触发条件 1。
> 所有编号引用均来自真实代码/ADR，已逐行核对。

---

## 1. 「触发条件 1」是被谁的证据兑现的，还是被自己的野心兑现的？

**问题**：ADR-0005 触发条件 1 的原文是「**UX 要求**低延迟实时进度」——这是需求侧的信号。
现在这条信号来自哪个真实用户、工单或可复现的体验抱怨？还是来自「我想让简历更亮」？

**为何重要**：如果触发信号是自造的，P2-A 就从「兑现已接受的架构决策」退化为
「用 ADR 给自己的偏好背书」，而 ADR-0005 当初拒绝推送的理由恰恰是「过度工程」。
面试官会顺着这条线问：「谁提的需求？」

**证据**：`docs/adr/0005-polling-vs-server-push.md:49`（触发条件 1 依赖「UX 要求」）；
`docs/adr/0005-polling-vs-server-push.md:45-46`（拒绝推送＝过度工程）；
`docs/adr/0007-resume-positioning-and-optimization.md:16`（唯一的"用户"是演示者/自己）。

---

## 2. 触发条件 1 白纸黑字指向「方案 D」，P2-A 为什么选了「方案 C」？

**问题**：ADR-0005 触发条件 1 明确写「→ 走**方案 D** + 改造 `llm.Client` 契约」，
方案 D 是 **WebSocket（双向）**；而 P2-A 选的是 **SSE（方案 C，单向）**。
是触发条件被重新解释，还是方案选错了？

**为何重要**：这不是命名之争——它暴露 P2-A 没有回到原 ADR 校准方案择取，
而是先定了 SSE 再补理由。要么修订 ADR-0005 的方案映射（说明 SSE 为何足够），
要么承认自己在做的是一个未经决策的新分叉。

**证据**：`docs/adr/0005-polling-vs-server-push.md:49`（触发 1 → 方案 D）；
`docs/adr/0005-polling-vs-server-push.md:38-39`（方案 C=SSE 单向 / 方案 D=WebSocket）。

---

## 3. 「实时进度」的粒度是 token 还是 step？这是两个不同的项目。

**问题**：P2-A 一句话里混装了两种东西：ADR-0005/0007 讲的「**流式 token**」
和 ADR-0008 讲的「**think→tool_call→observe 逐步轨迹**」。你要的是哪一个？

**为何重要**：二者成本完全不同——token 流式必须改 `llm.Client` 的返回契约
（当前 `ChatWithTools` 返回**整轮** `*AssistantTurn`，非 delta 流）；step trace 只需在
`runAgent` 循环里埋事件。若选了最贵的 SSE 却只推 5 个 step 事件，等于用流式协议
承载非流式数据。含糊其辞会让"可讲性"当场塌陷：面试官会问「你的 SSE 推的是 token 还是 step？」

**证据**：`llm/client.go:168-209`（`ChatWithTools` 整轮返回，无 delta/stream 字段）；
`llm/client.go:28-45`（请求/响应结构体无 `stream` 字段）；
`worker/agent.go:59-92`（循环内只累积本地 `turns`）；
`docs/adr/0005-polling-vs-server-push.md:49` 与 `docs/adr/0008-agent-tool-calling-loop.md:19`
（两处对"进度"的定义不同）。

---

## 4. 事件从哪来？`runAgent` 现在把整条中间轨迹**全丢了**。

**问题**：要 SSE 推 step，event 的源在哪？当前 `runAgent` 把中间 assistant/tool 消息
只 append 进本地 `turns`，最终 `return` 一个**最终字符串**（`agent.go:73` 或 `:91`）；
`process` 也没有任何 progress 回调（`pool.go:107` 只是一次 `runAgent` 调用）。

**为何重要**：这意味着 P2-A 必须给 `runAgent`/`process` 注入 emitter 或回调——
这是对 ADR-0008 明确承诺的边界（`process` 内部只是把 `Chat` 换成 `runAgent`，
**对外契约不受影响**）的结构性改动。P2-A 的表述「加个 SSE」严重低估了这项改动。

**证据**：`worker/agent.go:77-88`（中间 turn 仅进本地切片）；
`worker/agent.go:73,91`（只返回终态字符串）；
`worker/pool.go:107`（`process` 无进度钩子）；
`docs/adr/0008-agent-tool-calling-loop.md:33-34`（承诺对外契约不变）。

---

## 5. 客户端唯一的权威状态通道里根本没有 trace 字段——断线后时间线就断了。

**问题**：客户端能读到的状态只有 `GET /tasks/{id}` 返回的 `Task`，而 `Task` 只有
`ID/Status/Prompt/Result/Error` 五个字段（`store/store.go:52-58`）。SSE 若不落 store，
「实时 trace」与「权威状态」就是两个真相源。刷新页面/断线重连后，用户只能拿回
最后一帧状态，中间 step 永远补不回来。

**为何重要**：SSE 断线重连是常态（EventSource 自动重连）。现在 demo 断线重连后
`poll` 立刻能恢复（`demo.html:144-162`）；加了 trace 后用户看到的却是"半个时间线"，
体验不一定比今天干净地显示 `running` 更好。要闭合这个洞，就得给 `Task`/SQLite
加 trace 列（`store/sql.go:41-47` schema 是固定 5 列）——又是 P2-A 没提的成本。

**证据**：`store/store.go:52-58`（Task 无 steps/trace）；
`store/sql.go:41-47`（schema 固定列）；
`api/handler.go:120-127`（HandleGet 只回 Task）；
`api/demo.html:144-162`（当前重连靠重发 GET 恢复）。

---

## 6. EventSource **不能带 Authorization 头**，而你的 Auth 只认 header。

**问题**：`Auth` 中间件只从 `Authorization` header 取 token（`middleware.go:70`）。
浏览器原生 `EventSource` API **无法自定义请求头**。demo 的 Bearer 输入框
正是靠 header 传（`demo.html:115-117, 130, 139`）。生产鉴权模式下（`API_AUTH_KEY` 非空），
demo 的 SSE 建连怎么过 Auth？

**为何重要**：只有两条烂路——把 token 塞进 query string（泄进日志/Referer），
或给 SSE 单独开一个绕过统一鉴权链的路由（破坏"H 每路由同一中间件链"的叙事，
`handler.go:36-48`）。两者都侵蚀当前的安全故事，且面试官必问。

**证据**：`api/middleware.go:63-77`（Auth 仅读 header）；
`api/handler.go:46-48`（wrap 链）；
`api/demo.html:92-93, 115-117`（token 走 header / localStorage）。

---

## 7. 现有 per-IP 令牌桶会把你的实时流自己限流掉。

**问题**：限流器是**全局共享、按 IP、20/s burst 40**（`handler.go:43`），
其设计目标是「覆盖**正常轮询**」（`handler.go:42`）。SSE 的中间事件若走同一 limiter，
一条活跃流的 step 事件会迅速吃令牌；EventSource 抖动重连又会给同一 IP 叠加建连。
P2-A 想清楚 SSE 与这个令牌桶的交互了吗？

**为何重要**：这会导致「演示到一半被自己的限流器 429」——对"实时观察器"这个卖点是
致命的反演示。要么给 SSE 单独限额策略，要么承认限流模型需要重估（=ADR 级决策）。

**证据**：`api/handler.go:42-43`（limiter 目标与参数）；
`api/middleware.go:131-140`（Limit 每请求扣令牌）；
`api/middleware.go:104-128`（按 IP 的 token bucket）。

---

## 8. 任务在**排队**或**被 503 拒**时，SSE 流会「连接活着但永远没数据」——比 503 更难懂。

**问题**：只有 3 个 worker（`main.go:66`），`Enqueue` 满则快速失败 503
（`pool.go:145-152`, `handler.go:96-99`）。有了 SSE 后，用户可能先开出一条"连接成功"的流，
却因任务在排队而长时间收不到任何 step；甚至任务根本没进队却被流"挂着"。
**失败时的人性化提示**怎么表达"你的任务其实没进去"？

**为何重要**：现在 503 是一个干净的、可行动的失败；SSE 把一个清晰的失败
变成"看起来在正常等待、实则永远不会出结果"的僵尸流——这是失败态体验的**倒退**，
正好踩在产品架构师最该盯的点上。

**证据**：`main.go:66`（worker=3）；`worker/pool.go:145-152`（Enqueue 快速失败）；
`api/handler.go:96-99`（503 分支）。

---

## 9. 可演示性：curl 演示不了 SSE 的"体验"，旗舰卖点反而更难当场复现。

**问题**：ADR-0007 记录的原始痛点是「不可演示：只有一个 curl」（`ADR-0007:27`）。
但 SSE 的卖点（时间线动画）只在浏览器里成立——curl 只能看到原始事件流文本，
演示不了"实时感"。P2-A 把一个无法用 curl 复现的东西作为旗舰卖点，
是否反而抬高了"面试官当场验证"的门槛？

**为何重要**：现有 demo 是单文件 `go:embed`、离线可用、零依赖（`ADR-0007:76`），
这正是"10 秒看懂"的基础。升级为观察器若引入对 JS/建连顺序的依赖，会让
「一键演示」变成「先讲怎么开流」。可演示性是减法，不是加法。

**证据**：`docs/adr/0007-resume-positioning-and-optimization.md:27`（原痛点=curl）；
`docs/adr/0007-resume-positioning-and-optimization.md:55,76`（demo 定位/嵌入）；
`api/handler.go:54-56`（demo 零依赖直开）。

---

## 10. 差异化记忆点到底是什么？——SSE 到 2026 已是任何聊天 UI 的标配。

**问题**：ADR-0007 的目标是「从中游跃升为**有明确记忆点**的上游项目」（`ADR-0007:42`）。
可 2026 年流式输出是任何 LLM 前端的默认配置。P2-A 在面试官眼里是
「又一个打字机效果」，还是「一个别人不会的工程决策」？

**为何重要**：记忆点必须落在**别人不会做或想不到**的地方。当下这个项目真正的差异化
是 ADR 资产（`ADR-0007:20`）和自研解析器/取消树这类硬决策。
SSE 本身作为卖点是"谁都会"，作为工程叙事才有价值——若 P2-A 答不出它的记忆点
落在 SSE 还是 step trace，它可能既非真实痛点、也非记忆点。

**证据**：`docs/adr/0007-resume-positioning-and-optimization.md:20,42,44-58`（差异化/排序）。

---

## 11. YAGNI 自查：凭哪条**新证据**判定「此时」不再是过度工程？

**问题**：`ADR-0007:68-69` 要求「每个新增项都应有对应 ADR 或注释说明"为什么**此时**做"」；
`ADR-0004:52` 又立了「未触发前坚持 YAGNI」。P2-A 能拿出的"此时"证据是什么？

**为何重要**：如果答案是"想让简历更亮"，那这是**简历驱动**而非**需求驱动**工程，
与项目自诩的 YAGNI 纪律自相矛盾——这恰恰是面试官最爱抓的逻辑不一致。
反之，若能证明"ADR-0005 触发条件 1 已被 X 事件满足"，则 P2-A 成立。
这个自检非做不可，且结论直接决定 P2-A 该不该做。

**证据**：`docs/adr/0007-resume-positioning-and-optimization.md:68-69`；
`docs/adr/0004-no-persistence-no-message-queue.md:52`；
`docs/adr/0005-polling-vs-server-push.md:45-46`。

---

## 12. 新引入的失败态，如何映射成一句对用户有意义、又不泄露内部信息的话？

**问题**：P2-A 在流里会新暴露哪些失败？至少有：流中断、agent 超步数
（`agent.go:91` `"agent exceeded max steps"` 当前会被 `process` 吞成粗粒度 `"upstream error"`，
`pool.go:114`）、单任务超时（ADR-0011）。它们在 SSE 里如何变成一句人话？

**为何重要**：`ADR-0003` 决策 1 划了红线——`task.Error` 只允许粗粒度分类，
原始错误只进日志。当前 demo 只做了 401 的人话化（`demo.html:119-125`）。
SSE 若把内部错误分类直接推进事件流，就**同时**踩了两条线：泄露过度、
且对用户无行动价值。这正是"失败时人性化提示"该在哪一层做、做到什么粒度的问题。

**证据**：`docs/adr/0003-security-resilience-design.md:14-25`（错误脱敏决策 1）；
`worker/agent.go:91` 与 `worker/pool.go:114`（错误被粗粒度化）；
`api/demo.html:119-125, 154-157`（当前仅 401 人话化 / failed 直接显示 error）。

---

## 一句话结论（供父 agent 参考）

P2-A 的**方向**（兑现 ADR-0005 触发条件）需要先回答 **Q1/Q11**（是否真有需求信号）
与 **Q2**（SSE 与触发条件指向的方案 D 不一致）；即便成立，成本被严重低估
（**Q3/Q4/Q5** 涉及改 `llm.Client` 契约、改 `runAgent` 边界、动 store schema），
且会**恶化**演示与失败体验（**Q6/Q7/Q8/Q9**）。建议：要么把它收窄为
「不碰 llm 契约、仅内存级 step trace」的可讲版本，要么老实做 **P2-B**。
