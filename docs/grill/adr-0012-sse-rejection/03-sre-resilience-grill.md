# P2-A（SSE 流式 + 实时进度推送）SRE 边界审讯

- **对象**：候选方案 P2-A —— 增加 SSE 流式输出 + 实时进度推送，客户端从轮询升级为
  EventSource 订阅；Demo 变实时 Agent Trace 观察器。
- **视角**：SRE / 可靠性工程。方法：苏格拉底式边界拷问（每问先给「为何重要」，再给代码/ADR 证据）。
- **基线**：当前分发模型 = 客户端轮询 `GET /tasks/{id}`（ADR-0005 方案 A）。
- **证据基线**：本 worktree `hermes-subagent/subagent-sa-2-ea0b5cca`，Go 1.26.5。

一句话结论：**P2-A 不是给现有服务「加一个端点」，而是把服务从「无状态、可任意水平扩容、
失败有界」推向「每客户端一条长连接 + 进程内订阅表、事件亲和到单实例、故障无界悬挂」。**
ADR-0005 之所以选轮询的每一条理由（LB 友好、零新协议、失败可恢复），都会被这一改动逐条反转。

---

## 1. 订阅原语根本不存在：这不是加路由，是把无状态服务改成有状态服务

**问**：`TaskStore` 接口只有 `Create/Update/Get/Complete`，worker 只在**终态**写一次结果——
请问「生成中，第 2/3 步」这条进度事件，由**谁**、存在**哪**、按**什么**分发给订阅者？

**为何重要**：SSE 要求服务端对「一条连接存续期内的事件流」负责。轮询模型下服务端对每个请求
都是即用即弃、无记忆；SSE 则必须新增一个**进程内连接注册表 + 订阅分发器**（谁订阅了哪个 task_id、
事件 buffer、慢消费者策略）。这是架构性质的变化，不是端点增删。

**证据**：`store/store.go:27-32` `TaskStore` 接口仅 4 个方法，**无任何 subscribe/notify/wait 原语**；
`worker/pool.go:88-124` 的 `process` 全程只写两个状态点（`pool.go:90` 置 running、`pool.go:114/121` 写终态），
中间 `runAgent` 的 think→tool 多步循环（`worker/agent.go:64-89`）**不产生任何对外可见的中间态**。
要推送 trace，必须把纯函数式的 worker 改成带副作用广播。

---

## 2. 限流器保护的是「速率」，SSE 消耗的是「并发」——两者错配，长连接完全不受限

**问**：`newRateLimiter(20,40)` 是**按请求发生次数**计费的令牌桶。一条建立后挂 10 分钟、
期间不产生任何请求的 SSE 连接，会消耗几个令牌？限流器对「并发连接数」有任何约束吗？

**为何重要**：轮询下，40 个 burst 令牌 = 40 个短命请求；SSE 下，同样 40 个令牌可在 1 秒内
建立 40 条长连接并各自占住 10 分钟。**限流器对 SSE 的并发上界贡献为 0**，
现有唯一的背压闸门（限流）在 SSE 面前失效。

**证据**：`api/handler.go:43` `limiter := newRateLimiter(20, 40)`；补令牌逻辑只按 `elapsed * rate`
（`api/middleware.go:116-120`），一个连接建立只扣 1 令牌后就再无消耗（`middleware.go:123-126`）。
另注：桶按 IP 惰性创建且**从不驱逐**（`middleware.go:82-113`），连接越多该 map 越大。

**追问**：要不要新增一个**独立的并发信号量**（per-IP + 全局 max-conns）？它的上界取多少？
超限时怎么拒绝一条 SSE —— 429 会让 EventSource 静默重连（见 §4）。

---

## 3. Auth 只认 `Authorization` header，而 EventSource 协议无法发送自定义 header

**问**：浏览器原生 `EventSource` 不允许设置请求头。当前 Auth 只从
`r.Header.Get("Authorization")` 取 Bearer（`api/middleware.go:70`）——
那 SSE 订阅该怎么带 token？

**为何重要**：这会把鉴权面从 header 挤到 **query string 或 cookie**。
query token 会随 URL 进入 access log、Referer、浏览器历史，安全面扩大；
cookie 又引入 CSRF 与跨域问题。无论哪条，都与现有「header-only」鉴权模型冲突，
需要重开安全评审，不是纯 SRE 问题但直接决定部署形态。

**证据**：`api/middleware.go:63-77` `Auth` 仅读 `Authorization` header；
Demo 页也是手动往 fetch 里塞 header（`api/demo.html:130` `authHeaders(token)`）
——**现有鉴权路径在 EventSource 下根本不可复用**。

---

## 4. 401 / 429 对 EventSource 不可读 → 鉴权失败触发无止境重连风暴

**问**：`Auth` 失败返回 401 纯文本（`middleware.go:72`）、`Limit` 超限返回 429 + `Retry-After`
（`middleware.go:134-135`）。EventSource 客户端能读到这些响应体吗？拿不到文案时它做什么？

**为何重要**：EventSource 规范下客户端**读不到非 2xx 的响应体，也不能自定义 header**，
只会触发 `onerror` 并按浏览器默认间隔（约 3s）**自动重连**。因此：
一个 token 填错的浏览器页签 = 每 3s 打一次 401，**永不停止**（除非前端主动 `close()`）；
每次重连还要再过 `Limit`（`handler.go:47` 链序 `limit(auth(...))`），反过来吃掉该 IP 令牌桶。
这正是「断线重连」在 SSE 下最容易被忽略的失败模式：**把鉴权失败伪装成网络抖动**。

**证据**：`api/middleware.go:72`（401）、`api/middleware.go:134-135`（429 + Retry-After，全仓唯一一处
Retry-After，仅用于轮询语义）；`api/handler.go:46-48` 中间件链 `Recover(RequestID(limit(auth(h))))`。

---

## 5. `Recover` 中间件对「已 flush 的 SSE 响应」完全失效

**问**：SSE handler 通常已 `200 OK` 并 flush 了若干 event 后再发生 panic。
此时 `Recover` 里的 `w.WriteHeader(500)` 还有意义吗？客户端看到的错误是什么？

**为何重要**：`Recover` 的设计前提是「响应尚未写出，可改成 500」（`middleware.go:26` 注释明说
「返回 500 而非让连接半开」）。SSE 一旦开始推流，响应头已落地、body 已 flush，
`Recover` 再 `WriteHeader(500)` 会变成 superfluous WriteHeader 日志，**客户端收到的是一个
静默截断的 200 流**，无法与「任务正常结束」区分。SSE handler 必须自建**错误事件帧**协议，
不能依赖现有 `Recover` 兜底。

**证据**：`api/middleware.go:27-39` `Recover` 在 defer 中无条件 `WriteHeader(http.StatusInternalServerError)`；
而 `api/handler.go:52-56` 所有端点都被 `Recover` 包裹。

---

## 6. 优雅停机 `Shutdown(30s)` 假设「请求是短的」——SSE 推翻了这个假设

**问**：SSE 连接的存续期是「分钟级且不设上限」。`srv.Shutdown(30s)` 会等存量请求结束——
一条永不结束的 SSE 连接会让它等到什么？等到 30s 超时强关后，客户端的 EventSource 会做什么？

**为何重要**：`Shutdown` 会挂满 30s 再强断所有 SSE；而 EventSource 收到断连后**自动重连**，
此刻进程正在关停或已退出，重连打到奶尽/已停的实例 → 停机期的**重连风暴**，
若 LB 尚未摘除该后端，风暴会持续到进程退出为止。ADR-0011 明确 `Shutdown` 只约束
「停止接新连接、等存量请求结束」，但它默认存量请求是短请求；
**「如何主动结束长连接并提示客户端改连别处」这个语义在代码与 ADR 里都不存在。**

**证据**：`main.go:99-108` `srv.Shutdown(context.WithTimeout(30s))`；`docs/adr/0011-...md:24`
「srv.Shutdown(30s) 只约束 HTTP server 的停机宽限期」——该论断对长连接不再成立。

---

## 7. 两层超时（client 60s / taskCtx 30s）与「流式契约」直接冲突

**问**：要真流式 token，`llm.Client` 必须从「返回完整字符串」改成「返回流 reader」（ADR-0005:49
自己点名了这一步）。但 `http.Client{Timeout: 60s}` 是**整个请求（含读完 body 到 EOF）**的
绝对上限——一条本该持续 90s 的流，会不会在第 60s 被传输层硬切？

**为何重要**：`http.Client.Timeout` 覆盖 DNS+连接+写请求+读完整 body 的全过程，是**硬上限**，
不像 `taskCtx` 那样能在停机时优雅取消。当前它靠「`taskCtx` 30s 先打断、60s 仅作兜底」
（ADR-0011:20）正交共存，这个前提对「一次一次性响应」成立；对一条持续流**不成立**——
任何 >60s 的 trace 会被无声截断。改流式必须把 `client.Timeout` 拆成
Dial / ResponseHeader 超时 + 每个 SSE chunk 的独立 ctx 期限。

**证据**：`llm/client.go:66` `http.Client{Timeout: 60 * time.Second}`；
`llm/client.go:97` `c.http.Do(httpReq)`（一次性拿全 body，无流式）；
`main.go:62-65` / `worker/pool.go:104` `taskCtx` 默认 30s；`docs/adr/0005-...md:49`。

---

## 8. 慢消费者无背压上界：SSE 写阻塞 + 服务端未设任何超时 = 无限 goroutine/内存

**问**：若客户端读得慢（或 TCP 零窗口），SSE 的 `w.Write(event)` 会阻塞在 socket buffer。
这个阻塞占用什么？`http.Server` 有没有 `WriteTimeout` / `ReadTimeout` / `IdleTimeout` 来兜底？

**为何重要**：Go 每连接一个 goroutine，写阻塞 = 占住一个 goroutine + 该连接携带的事件缓冲，
且**没有退出时间**。上游故障时把连接攒起来更容易 OOM/耗尽 goroutine。
当前连接级上界只有一个隐性的 `MAX_HEADER`，没有事件缓冲上限、没有慢读丢帧策略。

**证据**：`main.go:74-77` `http.Server` **只设了 Addr 和 Handler**，
`WriteTimeout`/`ReadTimeout`/`IdleTimeout` 全未设（全仓 grep 无命中）；
轮询模型下这无害（响应瞬间写完），SSE 下这是**未设防资源上界**。

---

## 9. 全仓无熔断器：SSE 把「有界快速失败」升级为「长连接静默悬挂」

**问**：当前上游变慢时的背压路径是什么？它是有界的吗？换成 SSE 后，
用户看到一个卡在 90% 的进度条，还会像看到 503 那样**立刻退避**吗？

**为何重要**：现状失败是**有界**的：队列满 → `Enqueue` 返回 `ErrQueueFull` → 503
（`worker/pool.go:145-151` / `api/handler.go:96-99`），ADR-0004:27-28 把「失败有界、无级联雪崩」
作为不引入 DB/MQ 的核心理由之一。SSE 的「生成中…」UI **天然鼓励用户保持连接并等待**，
而上游 P99 从 2s 飙到 30s 是已记录的真实事故（ADR-0011:10）；
worker 仍固定 3 个（`main.go:66` → `worker/pool.go:42`），但连接数可无限增长。
故障形态从「快速失败、可观测、可退避」变成「长连接静默卡住」，**且全仓没有任何熔断器**
（grep `breaker|circuit` 无命中）来在 upststream 抖动时切断。

**证据**：`worker/pool.go:145-151`（`ErrQueueFull` 快速失败）、`api/handler.go:96-99`（503）、
`docs/adr/0004-...md:27-28`（有界失败论）、`docs/adr/0011-...md:10`（P99 2s→30s 事故）、
`main.go:66`（并发硬编码 3）。

---

## 10. 事件的「权威真相」在多实例下会分裂：worker 在 A、订阅在 B → 事件永远收不到

**问**：`store` 的两种实现（内存 map / 本地 SQLite）**都没有变更订阅**。
当一个任务在实例 A 的 worker 上跑、SSE 订阅落在实例 B，B 靠什么知道 A 的进度？

**为何重要**：轮询模型下这不成问题——客户端随时 `GET` 到任一实例，读的都是同一个真相
（当真持久化到共享存储时）。SSE 要求**事件被主动送达订阅者**，于是必须满足
「产生事件的后端 == 订阅者所在后端」。这要么加**粘性会话**把 client 钉在提交它的实例，
要么加**跨实例事件总线（Redis pub/sub 之类）**——两者都是 ADR-0004 明确拒绝过的
「引入 broker 运维」范畴。这是 P2-A 从无状态→有状态最硬的技术约束。

**证据**：`store/store.go:27-32`（接口无订阅）、`store/sql.go:19-23,81-93`（本地 SQLite，无变更钩子）、
`main.go:47-58`（后端按 `db_path` 在内存/SQLite 间二选一，均单实例本地）、
`worker/pool.go:49`（任务入的是**本进程** channel 队列）。

---

## 11. 自增整数 ID + 无事件序号：既可枚举探测，又无 `Last-Event-ID` 续传

**问**：task_id 是 `strconv.Itoa(s.nextID)` / `MAX(id)+1`（递增整数），且 `Task` 无事件序号/事件历史。
那 SSE 的断线续传靠什么？服务重启后（内存 store 丢全部任务）重连的引用会拿到什么？

**为何重要**：
(a) **枚举探测**：递增整数 ID 让攻击者用极低成本建立大量 SSE 连接探测任务存在性；
轮询下每次探测是一次性请求（受 `Limit` 次数约束），SSE 把它变成**可用性约束**（连接数）。
(b) **无续传**：`Task` 只有 `pending/running/done/failed` 四态、无单调事件序号，
服务端无法回应 `Last-Event-ID`，重连只能从头/无续传 → 用户看到进度回退或重复。
(c) **语义混淆**：任务已 `failed` 后进程重启（内存态丢失，ADR-0003:29-40），
重连 SSE 得到 404，EventSource 无法从状态码区分「任务永久失败」与「实例临时不可达」，
客户端必须自行 `close()` 判断——这在轮询里只是一个明确的 404 终态。

**证据**：`store/store.go:74-75`（`s.nextID++; Itoa`）、`store/sql.go:66-67`（`MAX(id)+1`）、
`store/store.go:52-58`（`Task` 无事件序号字段）、`api/handler.go:121-123`（不存在 ID → 404）、
`docs/adr/0003-...md:29-40`（内存态、重启即丢）。

---

## 12. 粘性会话不是「配置项」而是 SRE 债务：滚动发布 / 故障搬迁 / 热点全被放大

**问**：ADR-0005:26-28 已点名「跨 LB 时 SSE/WS 需粘性会话」。假设接受粘性——
滚动发布、节点故障、弹性扩缩容时，被你钉住的这些长连接怎么迁移？NAT 后多用户共享出口 IP 时，
「client IP 粘性」会把它们全钉到同一实例，热点与故障域怎么算？

**为何重要**：粘性会话把「无状态、可任意水平扩容、单点重启无损」的部署模型
（ADR-0005:64-66 的 ✅ 项）换成「连接亲和、发布即断流、故障有损」。而且常见云 LB / k8s Service
默认是 L4 轮询，只给「client IP→实例」粘性而非「task→实例」亲和，
与 §10 要求的**任务级亲和**并不等价。ADR-0005:45-46 选轮询的核心理由
（「任务耗时本来就长，轮询延迟无感」）被 P2-A 的「实时」诉求直接推翻——
**这意味着 ADR-0005 需要被正式重估，而不是叠加一个方案。**

**证据**：`docs/adr/0005-...md:26-28`（粘性会话约束）、`:45-46`（决策理由）、`:64-66`（无状态对 LB 友好）、
`main.go:74-77`（单端口、无 instance 标识、无环境感知）。

---

## 附：若坚持推进 P2-A，审计时必须能回答的问题清单

1. 并发连接上界：per-IP 与全局各是多少？用什么信号量实现？超限如何拒绝、如何让 EventSource 不退避风暴？
2. 事件真相源：worker→订阅者的推送通道是什么？多实例下是否退化为粘性会话或引入事件总线？
3. 流式契约：`llm.Client` 如何改成 reader？`http.Client.Timeout` 如何从「整体 60s」拆成「连接/首字节/每 chunk」？
4. 断线重连：是否实现事件序号 + `Last-Event-ID`？重启后（内存 store 丢失）如何给出确定终态而非 404 抖动？
5. 停机：`Shutdown(30s)` 如何主动结束长连接并让客户端改连？是否需要 `503 + Retry-After` 帧？
6. 熔断/降级：上游抖动时，除了 worker=3 的队列背压，SSE 侧是否有切断/降级回轮询的开关？
