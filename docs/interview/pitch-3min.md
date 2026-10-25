# 3分钟项目陈述 (Project Pitch)

本文件整理自 2026-08-17 对话，2026-10-25 根据 P8 阶段生产级闭环（会话状态机 + 滑动上下文安全压缩 + 事务性任务队列 + SSE 打字机流式推断 + 混合检索 + AST 架构门禁）全面重写。用于面试前的项目陈述复习。

---

## 30 秒电梯版（必须张口就来）

> **agent-runtime** 是一个用 Go 编写的高并发 Agent 异步运行时系统。核心解决 **LLM 慢调用阻塞 HTTP 连接** 与 **单体 Agent 多轮迭代后架构熵增腐化** 两大痛点。
> 架构拆为两阶段：接入层立即返回 `202 Accepted`，后台在纯化的 Worker Pool 中驱动带 Context 抢占与 Trace 观测的 ReAct 认知循环，支持多轮会话状态机与纯算法滑动上下文安全压缩、用户主动取消、崩溃恢复、FTS5+vec0 向量混合检索、基于 SQLite WAL 的事务性任务队列削峰与 SSE 打字机实时流式推断，并通过根级 AST DAG 白名单与 400 行体积预算机械门禁锁死分层边界。

---

## 3 分钟完整版

### 1. 痛点与背景 (0:00 - 0:30)

> 在生产级 Agent 系统中，有两大现实挑战：
> 1. **I/O 阻塞与吞吐**：LLM 接口与多步工具循环耗时长达数十秒，同步等待会占死 HTTP 连接，瞬间拖垮服务吞吐；
> 2. **架构熵增与反向依赖**：多版本迭代后，调度池为了识别终态容易直接穿刺依赖具体工具（如 Worker 直接 import 检索包），单文件膨胀失控，最终退化为网状泥潭。
> 
> 我主导设计的 **agent-runtime** 通过异步任务分层解耦与防御性机械门禁，构建了一个生产级高并发 Agent 运行底座。

### 2. 关键架构决策 (0:30 - 1:30)

> **决策一（状态唯一权威与协议下沉解耦）**：
> - 任务队列里**只传递 Task ID，不传实体大对象**。因为 Task 是有状态的可变对象，传 ID 强制 Worker 每次从持久化 Store 读取最新状态，杜绝快照过期，Store 是唯一权威。
> - 在多版本迭代后，工具终态需要向任务级脱敏状态映射（如 `"index missing"`）。我们没有让 Worker 直接 import `retrieval`，而是在底层 `agent/tool` 契约层抽象了 `TerminalClassifier` 接口，具体工具实现协议，Worker 仅面向接口做 `errors.As` 判别，彻底切断了通用并发池对具体工具的依赖。
>
> **决策二（AST 架构适应度门禁与文件体积预算）**：
> - 杜绝口头架构约定，在仓库根目录编写 `architecture_test.go`。利用 Go 标准库 `go/parser` 扫描全仓 import 拓扑，显式维护 12 个内部包的 DAG 白名单；
> - 遵循「Package 是封装边界、File 是阅读边界」的哲学，对超 400 行的文件在同包内按正交职责切分（如 `main.go` 拆 `cli.go`、`handler.go` 拆 `events.go`、`search.go` 拆 `vector_search.go`），并把「生产代码 ≤ 400 行、测试代码 ≤ 800 行」固化为每次 CI/测试必跑的硬性红线。
>
> **决策三（混合知识检索即工具与受控评测闭环）**：
> - 将混合知识检索以 `search_docs` 工具形式暴露给 ReAct 循环，基于 SQLite 原生 FTS5 BM25 词法检索与本地 Ollama `nomic-embed-text`（768 维）向量 KNN 进行 RRF 倒数排序融合；
> - 建设了自动化受控评测 Harness（ADR-0018），锁定 Seed、Temperature=0 与版本化 System Prompt，对 30 题任务集进行 N=3 受控对照，实测将跨文档综合成功率从 33% 提升至 100%，认知步数下降 0.88 步并节省 5.2% Token 成本。

### 3. 踩坑与排障 (1:30 - 2:30) ← 最重要

> **故障链一：HTTP 客户端沉默挂起**
> - `http.Client` 若未设 Timeout（默认零值 = 永不超时），遇到上游 TCP 握手成功但服务端思考时不回字节的「沉默挂起」，`Do()` 将无限阻塞。
> - 导致 3 个 Worker goroutine 全数僵死，缓冲队列塞满，后续请求在 Handler 的 `Enqueue` 裸发送处全站卡死。
> - **两层防御**：`llm.NewClient` 设置 60s 库级安全网超时；在 Worker 任务调度中派生 `context.WithTimeout` 并配合 `select + default` 快速失败回退 503 + `Retry-After: 1`。
>
> **故障链二：SSE 长连接写超时掐断**
> - Go 标准库 `http.Server.WriteTimeout` 是整个连接生命周期的硬上限（如 30s）。当 Agent 思考或工具调用较多时，SSE 流式连接会被底层主动掐死。
> - **解决之道**：在流式 Handler 中通过 `http.NewResponseController(w).SetWriteDeadline(time.Time{})` 动态清空写截止时间，并为客户端提供非阻塞投递与快照对账重放机制。

### 4. 演进历程与工程成熟度 (2:30 - 3:00)

> 本项目经历从 M1 到 P8 的严密演进：
> - **P2/P3**：实现两阶段优雅停机、Prometheus 指标、SSE 实时事件流与依赖倒置（DIP）解耦；
> - **P4**：基于 SQLite WAL 实现断电重灌与崩溃恢复（Rehydration），基于 `context.WithCancelCause` 实现用户主动抢占式取消；
> - **P5/P6**：完成混合知识检索作为工具接入、本地 Ollama 免密向量嵌入、API 真实 Usage 跨步累加，以及 AST DAG 白名单与体积预算双门禁；
> - **P7**：落地基于 SQLite WAL 的事务性任务队列与 Push-Notify 抢占调度，消除内存 channel 503 丢单并支持 10,000+ 削峰排队；接入 OpenAI/DeepSeek 兼容原生 SSE 流式推断，实现首字延迟从秒级降至毫秒级的实时打字机回显；
> - **P8**：实现 SessionStore 会话状态机（内存/SQLite 双实现），打破单轮任务孤岛；引入纯算法无堆分配滑动窗口压缩器（`agent.CompactMessages`），自动防范多轮对话 Context Window 爆炸；解耦 Worker 会话拼装（`worker/session.go`），实现并发度与上下文窗口弹性配置（`worker_concurrency` 与 `max_context_tokens`）。
> 全仓 12 个包通过 321 个单元测试（498 个分支）全绿且 100% 通过 `-race` 并发竞争检测，无 CGO 依赖，纯 Go 跨平台自包含。

---

## 自检要点

- [ ] 30 秒电梯版能张口就来
- [ ] 语速平缓，控制在 2.5 - 3 分钟内
- [ ] **故障链五步**完整（沉默挂起 → Do() 阻塞 → worker 僵死 → 队列满 → handler 阻塞 → 全站宕机）
- [ ] 说的是 `http.Client.Timeout` 零值，不是 ctx
- [ ] 决策一先讲**状态唯一权威**，再讲性能
- [ ] 决策二讲到**类型系统绑定**这一层，不只是 BaseURL
- [ ] 技术债说清**为什么现在不做**
- [ ] `defer p.Stop()` 永不执行这个具体 bug 要讲（自己发现的，不是教科书知识）

## 高频追问预案

| 追问 | 答法 |
|---|---|
| Task 才几十字节，传副本有什么问题？ | 不是性能问题，是状态一致性——store 必须是唯一权威 |
| 上游挂了不是会返回错误吗？ | 不是断连，是沉默挂起：TCP 连上但不返回字节，`Do()` 无法区分"正在想"和"死了" |
| `http.Client.Timeout` 和 ctx 有什么区别？ | 见上面对照表：库级安全网 vs 调用方控制、全局 vs 单次、不可取消 vs 可取消 |
| 为什么不用官方 SDK？ | 类型系统绑定是最硬的理由，其次零依赖、供应链风险 |
| 队列满了会怎样？ | P7 中已通过 SQLite WAL 事务队列 + Push-Notify 重构，支持 10,000+ 削峰；超限在 Enqueue 处非阻塞快速失败返回 503 + Retry-After: 1，彻底消除了内存 channel 满时阻塞调用方的缺陷 |
| 多轮会话怎么防止 Token 爆炸？ | P8 实现了纯算法确定性滑动截断（`agent.CompactMessages`）：绝对锚定开头的 System 提示词和末尾最新 1 轮消息，按 User 轮次边界折半截断中间历史，并插入省略系统注记；不引入额外的 LLM 总结开销，零额外延迟，避免模型二次幻觉与嵌套递归风险 |
