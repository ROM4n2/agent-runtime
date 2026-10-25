# 面试问答复盘：并发 / 队列 / 排障（背答案版）

> 来源：2026-09-03 vault-interview 陪练。**本文件为「标准答法」速记版**——只保留答对该说的关键词，复习时先用 `INTERVIEW` 思路自己答一遍再对照。
> 关联：[M1 异步任务 + Worker Pool](../interview/m1-async-worker-pool.md)、[项目深度 Q&A](../interview/project-qa.md)

## 使用方法

1. 只看 **Q**，合上文档口头作答（**出声说**，面试是说出来的）
2. 对照「标准答法」，重点比对**关键词是否漏了**
3. 「常见追问」自己也要能答，那才是区分度所在

---

## Q0 开场白（第一句就抛，定调）

**考点**：能否一句话让面试官知道项目价值，而非报组件名。

**标准答法**：
> agent-runtime 是一个高并发 Agent 异步运行时服务：`POST /run` 提交任务立即回 **202**，后台纯化的 worker pool 驱动带抢占式取消的 ReAct 认知循环，支持计算器、时间与 FTS5+vec0 向量混合知识检索工具，客户端可通过轮询或 SSE 实时获取事件流。
>
> 技术亮点：Worker Pool 调度限流、context 抢占式取消树、SQLite WAL 断电崩溃恢复（Rehydration）、依赖倒置与 `tool.TerminalClassifier` 错误协议下沉、根级 AST DAG 白名单与 400 行代码体积机械门禁、以及受控评测 Harness 实证提升。

---

## Q1 Goroutine 和 OS 线程有什么区别？

**考点**：Go 并发模型的地基。答错等于宣告"没搞懂并发"。

**标准答法**：
> - Goroutine 是 Go runtime 调度的**轻量协程**，**初始栈仅 2KB** 且可**动态扩缩**（不够就复制到一个更大的栈，用完缩回）；线程栈**固定**（常 8MB），不够直接 stack overflow。
> - 更关键：成千上万 goroutine 经 **GMP 模型多路复用**到少量 OS 线程上，切换在**用户态**完成，不进内核。
> - 所以 goroutine 又轻又敢开很多；线程贵在「切换要进内核」。

**常见追问**：
- *为什么 goroutine 敢开几万？* → 初始栈小 + 栈动态伸缩 + 用户态 M:N 调度，三者叠加，开几万 goroutine 内存也不过几十 MB。
- *术语红线* → 本项目全程一个进程、里面跑的是 goroutine。把 goroutine 说成"进程"或"线程"是一秒掉档。

---

## Q2 既然 goroutine 便宜，为什么 worker pool 还要限制成 3 个？不直接 `go process()` 吗？

**考点**：能否区分"**开 goroutine 的成本**"和"**goroutine 干的活的成本**"。这是 pool 在简历上的真正卖点。

**标准答法**：
> **限的从来不是 goroutine，是并发干活的数量。** goroutine 本身极便宜，但它干的活很贵（调 LLM API）：
> - 上游按**并发量计费**，且会**限流（429）/ 风控封号**
> - 去掉限制的后果不是"内存不够"，而是**上游把整个 IP 封掉**或一夜账单爆掉
>
> 所以 worker pool 本质是一个**并发闸门 / 信号量**，把对外并发数死死按住——护的是上游配额和我的预算，不是省内存。`size=3` 是演示值，生产应由上游 RPM/TPM 反推并做成配置项。

**常见追问**：
- *不用 pool，用 `chan struct{}` 信号量行不行？* → 行。`golang.org/x/sync/semaphore` 或容量为 N 的 channel 都能限并发。区别是 pool 的 goroutine **常驻复用**，信号量是**每任务新建**。
- *size 怎么定？* → 由上游速率限制反推，做成配置而非硬编码。

---

## Q3 队列满了之后 `Enqueue` 会发生什么？（易错点）

**考点**：能否看见自己代码的**缺陷**。答得出说明真懂架构边界。

**标准答法**：
> `queue` 缓冲大小 = worker 数 = 3。
> - 第 **4~6** 个请求：进缓冲**排队等**
> - 第 **7** 个（队列 + worker 全满）：`Enqueue` 是**裸 channel 发送 → 阻塞调用方 HTTP handler，不是拒绝**
>
> 危害：异步瞬间**退化成同步**，handler goroutine 堆积 → 可能拖垮服务器。这是已知缺陷，生产应改 `select + default` 快速失败。

**常见追问**：
- *为什么不是 503 拒绝？* → 因为代码里 `Enqueue` 是 `p.queue <- id` 的裸发送，没有 `default` 分支，所以满时阻塞而非报错。这正是要修的点。
- *无缓冲 channel（size 0）会怎样？* → 每次入队都得等某个 worker 正好在 `<-p.queue` 上才能交接，缓冲吸收突发流量的能力归零，阻塞概率大增。

---

## Q4 `Enqueue` 快速失败怎么写？（落到代码）

**考点**：能不能把"已知缺陷"变成"已修复 + 有测试"。

**标准答法**——给出代码并讲清语义：
```go
// ErrQueueFull 队列已满，调用方应快速失败（如返回 HTTP 503 让客户端退避重试）。
var ErrQueueFull = errors.New("worker: queue full")

// Enqueue 把任务 id 送入队列。队列满时立即返回 ErrQueueFull，
// 而非阻塞调用方——避免 HTTP handler 堆积，导致异步退化为同步。
func (p *Pool) Enqueue(id string) error {
    select {
    case p.queue <- id:
        return nil
    default:
        return ErrQueueFull
    }
}
```
> handler 收到 `ErrQueueFull` → 回 **503**；队列满时把已落库的任务标 `failed`，避免成为永远 `pending` 的孤儿任务。

**常见追问**：
- *为什么用 503 而不是 429？* → 429 是"**你**请求太频繁"（针对单客户端），503 是"**我**处理不过来"（服务端整体过载）。队列满属于后者。
- *测试怎么写？* → `NewPool(1, s, fakeChatter{})` 不 `Start`，先 `Enqueue` 成功，再 `Enqueue` 断言 `errors.Is(err, ErrQueueFull)`。

---

## Q5 半夜 `POST /run` 大量 503，但 CPU / 内存都低、worker 也闲着，怎么定位？（易错点）

**考点**：生产排障纪律——**先确认错误从哪一层产生**，再归因。

**标准答法**：
> 1. **先确认 503 从哪一层来。** 本系统 `POST /run` 成功即回 202，所以 503 **不可能是上游 LLM 返回的**，只能是你自己的 `ErrQueueFull`。别一看到 503 就甩锅上游。
> 2. 现象「CPU 低 + worker 闲 + 503 刷屏」说明：worker **不是闲，是阻塞在等慢上游 LLM 的网络 I/O**（故低 CPU）→ 队列满 → 新请求快速失败。
>
> 定位动作：看在飞指标是否高居不下；抓 goroutine 堆栈看是否停在 `net/http` 读；查 LLM 调用超时（代码中单次 60s）。

**常见追问**：
- *为什么 CPU 低却卡住？* → 网络 I/O 等待不占 CPU，goroutine 停在 `read()` 系统调用上，但也没空去 `<-p.queue` 取新任务。
- *怎么确认 worker 卡在等 LLM？* → pprof `/debug/pprof/goroutine` 看 3 个 worker 是否都停在 `net/http` 读响应处。

---

## Q6 上游偶发变慢（P99 2s→30s）导致半夜被 503 叫醒，不扩机器、不调大 pool，优先做哪件止血？

**考点**：事故 triage 的「止血 vs 健壮性」优先级判断。

**标准答法**——优先级 **A > B，事故期绝不做 D**：
> - **A. 短超时（首选止血）**：把超时 60s→8s。慢调用更快失败 → worker 更快释放 → 队列排空 → 503 停止。直接打在病因上。
> - **B. 客户端退避 + 指数重试**：避免丢请求、避免重试风暴。是健壮性改进，不是止血（worker 仍被占）。注意：已返回 503 的服务端无法「记录后重试」，能重试的只有客户端重新 `POST`。
> - **C. 给 `Enqueue` 加超时**：干扰项，反而让 handler 多占资源，更糟。
> - **D. LLM 调用加重试**：事故期最忌讳，会放大上游压力，worker 被占更久。

**常见追问**：
- *上游恢复后该固化什么？* → **限并发 + 限速率 + 短超时**三件套：worker pool 控并发，限流器控每秒请求数，短超时防单请求卡死。

---

## 加分项清单（能主动讲的点）

- **为什么用轮询而非 WebSocket/SSE 推送**：服务端无状态、客户端易实现、天然适配「任务可能很慢」。
- **内存存储 vs SQLite**：重启丢数据是**有意决定**（默认内存，零依赖、易演示），配 `db_path` 即切 SQLite 持久化——把「缺点」讲成「决策」。
- **`map + Mutex` vs `sync.Map`**：本项目读写都轻，锁竞争不是瓶颈，`Mutex` 更简单；`sync.Map` 无法原子地做读-改-写（`Update` 需要）。
- **优雅停机**：`Pool.Stop` 用 **cancel 而非 close(queue)**，避免向已关闭 channel 发送导致 panic；`wg.Done` 用 `defer` 防 worker panic 卡死 `Stop`。
- **秘密扫描 git hook**：防密钥误提交。

---

## 能力雷达（自我评估 · 2026-09-03）

| 维度 | 分数 | 亮点 | 待补 |
|---|---|---|---|
| 底层原理 | 7.0 | goroutine 轻量 / 2KB 栈 / 比线程省内存 | GMP 调度、栈动态扩容底层细节 |
| 并发与内存 | 6.5 | 深刻理解 pool=3 护上游成本 / 防风控 | queue 满**阻塞 vs 拒绝**混淆；`select+default` 未掌握 |
| 生产排障 | 6.5 | 「CPU 低先排除本地算力」直觉对 | 503 误判为上游；未先确认**错误来源层** |
| 架构权衡 | 8.5 | 限并发 / 限速率 / 短超时能串讲 | 事故 triage「止血 vs 健壮性」优先级需练 |

**Verdict：🟢 INTERN-READY（偏上）** —— 最大杀招是「知道为什么这样写」。扣分项集中在并发源码细节与生产排障纪律，可短期补齐。

---

## 自检清单

复习完能否**不看文档**说出以下每一条：

- [ ] Goroutine vs 线程：2KB 动态栈 + GMP 用户态多路复用
- [ ] pool 限的是"干活的数量"而非 goroutine 数；M2 后对应限流与成本
- [ ] 队列满 → **阻塞** handler（非拒绝）→ 异步退化为同步；`select+default` 修法
- [ ] `Enqueue` 快速失败代码 + handler 回 503 + 孤儿任务处理
- [ ] 503 先确认来源层：本系统 202 后不可能由上游返回 503
- [ ] 排障链条：慢上游 → worker 阻塞在 I/O（低 CPU）→ 队列满 → 快速失败 503
- [ ] triage 优先级 A > B，事故期绝不做 D（加重试）

---

## 已知待改进项（面试主动提，是加分项）

| 缺陷 | 影响 | 修法 | 状态 |
|---|---|---|---|
| 队列满阻塞 handler | 异步退化为同步 | `select` + `default` → 503 | ✅ 已修（`ErrQueueFull` + `TestEnqueue_Full`） |
| store 为内存态 | 进程重启丢全部任务 | 持久化（SQLite / Redis） | ✅ SQLite 可插拔（`db_path`） |
| 无 graceful shutdown | 实为已实现（见下）| `signal.NotifyContext` + `srv.Shutdown` + `defer p.Stop()` 取消树；在飞任务标记 failed 而非静默丢弃 | ✅ 已实现 |
| 单次 LLM 超时 60s 偏长 | 慢上游时 worker 占太久 | 可配置 `task_timeout_seconds`（默认 30s，事故期可降到 8s 止血）| ✅ 已实现 |
| pool size 硬编码 | 无法按环境调整 | 提为配置项 / 环境变量 | 待做 |
