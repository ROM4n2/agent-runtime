# 审讯：P2-A —— SSE 流式输出 + 实时进度推送

**视角**：后端并发工程师 · 苏格拉底式边界审讯
**对象**：给 agent-api 增加 SSE 流式输出 + 实时进度推送，并把 Demo 升级为实时 Agent Trace 观察器
**方法**：只读真实代码后引用 `path:line`；聚焦 goroutine/连接生命周期、锁作用域、context 取消传播、channel 背压与泄漏、事务边界、静默错误。

> 注：本 worktree 仅含 ADR-0001 ~ 0009，缺 ADR-0010/0011（SQLite 可插拔、架构演进），故相关引用以代码为准。

---

## Q1. HTTP Server 没有 WriteTimeout，SSE 长连接为什么把"固定 3 并发"的资源模型直接击穿？
- **为何重要**：worker 侧资源是被 `size=3` 硬封顶的（`worker/pool.go:49` 缓冲=size），但 SSE 订阅者数是**完全无上限**的：每个订阅 = 1 个 handler goroutine + 1 条 TCP 连接，且按 `ADR-0005:26` 需长期占用。更致命的是服务端只声明了 `Addr` 与 `Handler`，**没有 `WriteTimeout`/`ReadHeaderTimeout`/`MaxHeaderBytes`**——慢消费者不读时 `w.Write` 阻塞，会把该 handler goroutine 永久钉死，`Recover` 的 defer 永不执行。这是一处**永不回收的 goroutine + 连接泄漏**。
- **证据**：`main.go:74-77`、`worker/pool.go:49`、`api/middleware.go:27-39`、`docs/adr/0005-polling-vs-server-push.md:26-28`

## Q2. `srv.Shutdown` 会等活跃连接变 idle，而 SSE 响应永不结束——P2-A 上线后优雅停机是不是每次都以退出码 1 结束？
- **为何重要**：`shutdown()` 用 30s ctx 调 `srv.Shutdown`，其语义是"关闭 listener 并等待活跃连接转为 idle"。SSE 是一条**永不结束的活跃响应**，会让 Shutdown 一直阻塞到 ctx 超时，返回 `context.DeadlineExceeded`（非 nil）→ `main.go` 走 `return 1`。即：**只要有一个 SSE 客户端在线，进程就是"异常退出 + 拖满 30s"**，编排/CI 会判为 crash。
- **证据**：`main.go:99-109`、`main.go:87-96`、`docs/adr/0005-polling-vs-server-push.md:21-22`

## Q3. `defer p.Stop()` 排在 `shutdown()` 之后——停机窗口里 worker 还在消费队列，这段时间谁负责 taskCtx？
- **为何重要**：执行顺序是 `ctx.Done()` → `shutdown(srv)`（可能因 SSE 阻塞 30s）→ 函数返回 → `defer p.Stop()` 才 cancel `p.ctx`。这 30s 内 `p.ctx` **仍是活的**，worker 继续从队列取任务并派生正常 taskCtx，任务边执行、连接边在排空——两套生命周期不一致。若用 K8s，SIGTERM→SIGKILL 的宽限期会在 SSE 排空期间到期，进程被硬杀，在飞任务无终态。
- **证据**：`main.go:68-69`、`main.go:99-109`、`worker/pool.go:66-69`

## Q4. `worker()` 的 `select` 同时候选"取任务"与"ctx.Done"，Stop 后 Go 随机二选一——排队任务为什么会被随机判 failed？
- **为何重要**：Stop 时若 `p.queue` 非空且 `p.ctx` 已 cancel，两者同时就绪，`select` **随机**选中分支。一旦选中 `<-p.queue`，就用**已取消的** `p.ctx` 派生 `taskCtx`（`pool.go:104`），派生即取消 → `runAgent` 立刻返回 ctx.Err → 任务被 `Complete(failed)`。即：停机把"本应保持 pending 的排队任务"随机转成 failed，行为不可复现；在 P2-A 下会向订阅者推出一条语义错误的"失败"事件。
- **证据**：`worker/pool.go:75-82`、`worker/pool.go:104-107`、`worker/pool.go:114-118`

## Q5. `Enqueue` 既不看 `p.ctx`、`queue` 也没被关——Stop 之后它为什么还返回 nil？
- **为何重要**：`Enqueue` 是 `select{ case queue<-id: default: ErrQueueFull }`，只在**满**时失败；`Stop` 不关 channel（`ADR-0003` 决策 3 明确禁止 close）。因此 `Stop` 之后 `Enqueue` 仍返回 nil → handler 照常回 202 → 但已无消费者 → 任务**永久 pending 且永不会有终态**。加上 `handler.go` 先 `Create` 后 `Enqueue`，"已受理但永不执行"的僵尸任务在这条路径上必然产生，P2-A 的订阅者会永久挂起等待终态帧。
- **证据**：`worker/pool.go:145-152`、`worker/pool.go:66-69`、`api/handler.go:95-100`、`docs/adr/0003-security-resilience-design.md:70-72`

## Q6. 进度推送的"最直觉实现"是 worker 向订阅者 channel 发送——前端一断连，谁把 worker 从阻塞的 send 上救出来？
- **为何重要**：当前 worker 只有一个输出口 `Complete`（`pool.go:114/121`），发送方不会阻塞。P2-A 要加 per-task 事件扇出，若沿用阻塞发送且订阅者在客户端断开后不再读，**worker 会卡在 send** → 该 worker goroutine 永久泄漏 → `Stop` 的 `p.wg.Wait()`（`pool.go:68`）永久阻塞 → **进程无法停机**。这是 channel 背压最常见的死法：把"前端断连"转译成"后端停机死锁"。必须规定：对订阅者一律非阻塞 send + drop，或 `select` 里带 `ctx.Done()`。
- **证据**：`worker/pool.go:88-124`、`worker/pool.go:66-69`

## Q7. worker 跑在 `p.ctx` 下，SSE 订阅却绑在 `r.Context()` 上——这两棵取消树之间谁负责桥接？
- **为何重要**：任务执行 ctx 派生自 `p.ctx`（`pool.go:104`，进程级），而 SSE 订阅的存活由客户端连接决定（`r.Context()` 在断连时 Done）。若 broker 用 `close(evtCh)` 表示退订，worker 可能正在向该 ch 发送 → **send on closed channel panic**；若用 `r.Context()` 决定退订，则它与 worker 的 `p.ctx` **无任何关联**，停机时无法通知订阅者"服务器要关了"。必须在两者之间建立显式 join（如 `context.AfterFunc` 或每订阅 `select` 监听两个 ctx），否则断连×停机的交叉态必出竞态。
- **证据**：`worker/pool.go:104`、`api/handler.go:116-131`、`api/middleware.go:131-140`

## Q8. `runAgent` 是纯 request/response，`chatter` 只暴露一次性的 `ChatWithTools`——P2-A 的"实时进度"到底能观测到什么粒度？
- **为何重要**：`runAgent` 的签名与返回只有终态字符串，循环内部**没有任何回调/事件点**；`chatter` 接口也只有一次返回整轮的 `ChatWithTools`。在不改接口的前提下，"进度"最多只有 `pending/running/done` 三段——与现在的轮询等价，SSE 只是把轮询包了层壳。要真正的多步 trace 或 token 流，必须按 `ADR-0005:49` 的触发条件改造 `llm.Client` 契约（`Chat`→流），并给 `runAgent` 注入事件回调。
- **证据**：`worker/agent.go:61-92`、`worker/pool.go:17-20`、`docs/adr/0005-polling-vs-server-push.md:49`

## Q9. 上游 LLM 是非流式的（LimitReader + 全量 Decode，60s 超时）——SSE 连接在两步之间静默 30–60s，谁会被中间代理先掐断？
- **为何重要**：`ChatWithTools` 用 `io.LimitReader` + 一次性 `Decode`，且 `http.Client{Timeout:60s}`；单步耗时最长 60s，而 `taskTimeout` 缺省 30s（`pool.go:104`）。即便服务端做到了"每步推一帧"，**两步之间仍可能静默 30–60s 无字节**——SSE 上表现为连接空转，被 LB/反代的 idle timeout 切断，客户端随即重连，而你的 handler 仍持有旧连接。P2-A 必须配套**心跳（`: comment` 帧）**与读空闲阈值，并让它与 `taskTimeout` 对齐。
- **证据**：`llm/client.go:66`、`llm/client.go:192-205`、`worker/pool.go:104`

## Q10. `Update`/`Get` 失败只 `slog` 后 return，任务停在 pending/running——SSE 的"必发终态"契约怎么兑现？
- **为何重要**：`process` 里 `Update(running)` 失败或 `Get` 失败都只记日志就 `return`，任务**永远停在 pending/running，没有任何终态**；同理 `Complete` 失败也仅记日志（`pool.go:121-123`）。SSE 隐含契约是"订阅最终必收到一个终态事件"，这类静默错误会让订阅**永久挂起**——比轮询更糟，因为轮询至少每轮都能看到"还是 running"。必须在所有 early-return 路径补上"向订阅者广播错误终态"。
- **证据**：`worker/pool.go:90-100`、`worker/pool.go:121-123`、`worker/pool.go:113-116`

## Q11. `Recover` 在 panic 后写 500，但 SSE 早已写出 200 并 flush——客户端收到的是什么？
- **为何重要**：`Recover` 的模式是 panic 时 `w.WriteHeader(500) + Encode(json)`。但 SSE 一旦开始就会写出 `200` 并 flush 事件帧，此后任何 `WriteHeader` 都变成 `http: superfluous response.WriteHeader`（被忽略、仅日志）。于是**流内 panic 只能表现为"流被静默截断"，客户端拿不到任何错误信号**，也无法区分"任务还在跑"与"服务端崩了"。SSE 的错误必须以**流内 `event: error` 帧**表达，而不是靠中间件改状态码。
- **证据**：`api/middleware.go:27-39`、`api/handler.go:103-111`

## Q12. `SQLStore.Create` 的 SELECT MAX + INSERT 不在事务里，`Get` 又不加锁——P2-A 落事件表时，状态与事件的写入边界在哪里？
- **为何重要**：`Create` 的"取最大 id+1 → INSERT"只靠**进程内 `mu`** 保证，不是 DB 事务（`sql.go:62-75`），且 `Get` **完全不持锁**（`sql.go:81`）。P2-A 若把进度/事件落库，"更新任务状态 + 追加事件"会跨两条语句：没有显式 `tx` 就**无法保证 SSE 不会推给一个状态写入失败的任务**（`Complete` 失败仅记日志，见 Q10），也会与 SQLite 单写者 + `busy_timeout` 语义冲突。事件表还必须有单调 seq 才能支撑断线续传（`ADR-0005:22` 的"消息序号"要求）。
- **证据**：`store/sql.go:61-77`、`store/sql.go:81-93`、`store/sql.go:12-23`、`store/sql.go:112-133`、`docs/adr/0005-polling-vs-server-push.md:22`

---

## 未决/需正面回答的前置问题（拷问收束）
1. P2-A 是"per-step trace"还是"token 流"？若是后者，是否已接受 `ADR-0005:49` 要求的 `llm.Client` 契约重写？
2. SSE 端点由谁限流/限连？`rateLimiter` 在连接建立时放行一次，之后长连接不再计数——是否需要一个独立的并发连接上限（对应 worker 的 `size=3`）？
3. 停机时对排队任务与在飞任务分别承诺什么终态？当前代码在二者上都只做"丢/失败"，P2-A 必须在协议里写死。
4. `X-Forwarded-For` 被无条件信任用于限流键（`middleware.go:144-149`），SSE 端点暴露后是否会成为按伪造 IP 绕开限流、无限建流的通道？
