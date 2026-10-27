# ARCHITECTURE — agent-runtime

> 架构总览文档。回答三个核心问题：**实现了什么 Agent**、**基于什么架构**、**借鉴了什么**。
> 维护规则：随架构演进更新；重大变更须同步对应 ADR 与 `README.md`。
> 适用范围：当前主线（`master`，P1–P10 全部交付）。

---

## 0. 项目定位（一句话）

**一个高并发异步任务执行后端服务，内嵌一个 ReAct 式工具调用单智能体（tool-calling agent）。**
客户端 `POST /run` 提交 prompt → 服务落库入队（202）→ Worker Pool 内一个 worker 跑
`think → call tool → observe` 多步循环调用 DeepSeek → 客户端轮询或 SSE 取结果。

> 同名澄清：本项目**不是** GitHub 上 `NabilAziz99/agent-runtime`（那是 Python 移植 Claude Code 多智能体树、基于 LangChain）。本仓是 **Go 自研、后端化、单智能体、标准库优先** 的运行时。

---

## 1. 实现的 Agent（What Agent）

| 维度 | 说明 |
|---|---|
| **类型** | ReAct 式单智能体（非多智能体派发树，无子智能体生成） |
| **认知循环** | `think → call tool → observe` 多步闭环（`agent/engine.go`），`maxAgentSteps=5` 上限防无限工具环 |
| **内置工具** | `calculate`（自研递归下降 AST 算术求值，**不用 eval**，防代码注入）、`current_time`（IANA 时区）；P5 起增加 `search_docs`（混合检索即工具） |
| **执行形态** | 被 Worker Pool 并发调度的**后端服务**智能体，而非交互式 CLI / 本地 Agent |
| **对外契约** | 任务状态机 `pending → running → done/failed`；`POST /run` 提交、`GET /tasks/{id}` 轮询、`GET /tasks/{id}/events` SSE 流式 |
| **生产级能力** | 用户主动取消（context 物理中断上游）、掉电崩溃恢复、SSE 原生 token 级打字机、多轮会话状态机、滑动窗口上下文压缩、上游限流指数退避与 Retry-After 尊重、Token 成本计量持久化、独立诊断端口（pprof） |

**最小可信定义**：多步工具调用循环即 agent 的实质（ADR-0008 双镜论证）——区别于"LLM 聊天的异步封装"，使 "agent" 名副其实。

---

## 2. 基于的架构（What Architecture）

### 2.1 技术基座

- **Go 1.26.5**（`go.mod` 声明；Dockerfile 构建镜像仍为 `golang:1.24-alpine` + `GOTOOLCHAIN=auto` 自动升版）；**标准库优先**（`net/http` / `embed` / `go/parser` / `httptest` / `sync/atomic`）+ `slog`。
- 唯一 opt-in 第三方依赖：`modernc.org/sqlite`（**纯 Go 无 CGO**），仅当 `db_path` 非空才加载；留空则用内存存储。
- 默认零第三方依赖，Prometheus 文本指标用 `sync/atomic` 自实现。

### 2.2 分层拓扑（Clean Architecture + 依赖倒置）

严格单向 DAG，`api` 层仅依赖接口、不反向依赖 `worker`；根级 `architecture_test.go`
用 `go/parser` 扫描全仓锁定 import 白名单，并强制生产文件 ≤ 400 行、测试 ≤ 800 行。

```
                          main (组合根：配置加载 → 依赖组装 → 优雅停机)
                             │
            ┌────────────────┼────────────────┐
            ▼                ▼                ▼
        ┌─────────┐     ┌─────────┐      ┌─────────┐
        │   api   │     │ worker  │      │  store  │
        └────┬────┘     └────┬────┘      └─────────┘
             │(仅依赖接口)    │(调度)
             ▼               ▼
        ┌────────────────────────┐
        │         agent          │  (认知内核：Engine / TraceStep / Buffer)
        └───────────┬────────────┘
                    ▼
        ┌────────────────────────┐
        │       agent/tool       │  (工具注册表与实现：calculate / time / search_docs)
        └───────────┬────────────┘
                    ▼
        ┌────────────────────────┐
        │          llm           │  (底层模型协议：ChatWithTools / Stream，OpenAI 兼容)
        └────────────────────────┘

   retrieval/  (独立顶层：FTS5 + vec0 + RRF 混合检索，零反向依赖)
   metrics/    (leaf：Prometheus 文本指标，sync/atomic)
   config/     (leaf：扁平配置解析，环境变量 > config.yaml > 默认值)
   prompts/    (版本化系统提示词，go:embed 内嵌)
```

### 2.3 包职责契约

| 包 | 领域定位 | 核心职责 | 允许依赖的内部包 |
|---|---|---|---|
| `llm/` | 模型协议适配 | OpenAI 兼容 HTTP 通信，提供 `ChatWithTools` / `ChatWithToolsStream` | 零内部依赖（纯标准库） |
| `agent/tool/` | 工具系统 | `Tool` 接口、参数解析与执行，内置 `calculate` / `current_time` | `llm/` |
| `agent/` | 认知内核 | `TraceStep`、`traceBuffer`、`Engine`（ReAct 循环）、步骤回调 | `llm/`, `agent/tool/` |
| `store/` | 任务状态持久化 | `TaskStore` / `SessionStore` 接口、内存与 SQLite 双实现 | 零内部依赖 |
| `worker/` | 并发调度器 | `Pool`（Goroutine 池）、抢占、超时、取消树、状态流转 | `store/`, `agent/`, `metrics/` |
| `api/` | HTTP 接入与网关 | 路由中间件链、JSON 契约、`TaskRunner` 消费者接口、Demo | `store/`, `agent/`, `metrics/` |
| `retrieval/` | 混合检索 | FTS5 词法 + vec0 向量 KNN + RRF 融合 | `llm/`（embed 客户端） |
| `main.go` | 组合根 | 读取配置、依赖注入组装、优雅停机 | 全部包 |

### 2.4 关键子系统

- **事务性持久队列（削峰）**：`store.TaskStore.ClaimNext` 用 SQLite WAL 下 CAS 原子抢占
  （`status='running' WHERE id=? AND status='pending'`）；`worker.Pool` 持有无缓冲通知信标
  `wakeUp chan struct{}` 做 Push-Notify 唤醒；`max_queue_size`（默认 10,000）削峰填谷，消除突发 503 丢单。
- **SSE 原生打字机流式**：`llm.Client.ChatWithToolsStream` 逐行解析上游 SSE 并组装分块工具参数；
  `agent.RunWithOptions` 引入 `OnDelta` 回调；`TraceBuffer` 支持 `AppendDelta` 与 `SubscribeEvents` 双轨广播；
  `api/events.go` 实时下发 `event: delta`，首字延迟（TTFT）降至数百毫秒。
- **多轮会话状态机**：`store.SessionStore` 抽象（`sessions` / `session_messages` 表 + 复合索引）；
  `POST /run` 可绑定 `session_id`，打破单轮任务孤岛。
- **纯算法滑动窗口压缩**：`agent.CompactMessages` 无堆逃逸 Token 估算，首尾强锚定（System 提示 + 最新一轮），
  中间轮次按 User 边界折半截断并插入系统折叠注记，**零 LLM 调用开销**根除 Context Window 爆炸。
- **可观测性与诊断**：`/metrics` 以 Prometheus 文本格式暴露 `agent_tasks_*` 任务生命周期指标与 `agent_llm_*` 上游重试与 Token 成本指标（`sync/atomic` 实现，零依赖）；`tasks` 表持久化单任务 `prompt_tokens`/`completion_tokens`；`/healthz` 存活探针；独立端口/mux 提供 `/debug/pprof/*` 性能分析；指标仅暴露聚合计数，不含任务内容或上游响应体。

---

## 3. 借鉴了什么（What It Borrowed）

> 原则：**借鉴工程哲学与成熟模式，而非照搬实现**。刻意复制的是架构范式，差异化的才是本项目价值。

| 借鉴来源 | 借鉴了什么 | 本项目落地 / 关键取舍 |
|---|---|---|
| **ReAct（Yao et al., 2022）** | `think → act → observe` 多步认知循环范式 | `agent/engine.go` 循环，`maxAgentSteps` 封顶（ADR-0008） |
| **Pi Agent（`earendil-works/pi`）** | 内核最小化、工具三层递进、errors-as-messages 工程哲学 | ADR-0013 据 Pi 的 `ARCHITECTURE.md` 重构出独立 `agent/` 内核与 `tool/` 插件库；**明确不复制** Pi 的 TUI / 会话树 / 双向 WebSocket——本仓定位高并发异步服务，无状态轮询更合适 |
| **自建知识库 Agent SDK 规范** | vault 内 `01-Rules/AGENT-SDK-PATTERNS.md`、`08-Projects/Pi-Agent/ARCHITECTURE.md`、`01-Rules/LAYERED-ARCHITECTURE.md` | 支撑分层解耦、工具错误绝不打断循环（回喂模型）、消费者定义接口、行数门禁等决策（ADR-0013 即据此审视） |
| **Claude Code 上下文压缩思路** | compaction 概念（首尾锚定、防窗口爆炸） | **关键分歧**：Claude Code 用 LLM 摘要式压缩；本项目 ADR-0020 改用**纯算法确定性滑动窗口**（零额外网络 / 无幻觉） |
| **OpenAI function calling 线协议** | 工具调用 wire 结构 | 复用 `llm` 包已有 OpenAI 兼容结构，未引入新依赖（ADR-0008） |
| **混合检索标准做法** | FTS5 BM25 词法召回 + vec0 向量 KNN + RRF 倒数排名融合 | `retrieval/` 独立实现；ADR-0018 成对受控评测实证：混合组跨文档综合题成功率 33.3% → 100% |
| **Clean Architecture / DIP** | 高层不依赖低层细节；Go "accept interfaces, return structs" | `api` 仅依赖 `TaskRunner` 接口，`worker` 不反向依赖 `retrieval` |
| **SQLite WAL 事务队列** | 原子抢占 + 持久化削峰的成熟模式 | `store/sql.go` CAS 抢占 + 崩溃恢复重灌（ADR-0015） |

---

## 4. 设计权衡（为什么这样选）

- **单智能体 vs 多智能体树**：本仓是后端服务，高并发异步执行是核心价值；多智能体派发（如 Claude Code 树）更适合交互式 IDE 场景，故不引入，保持无状态轮询架构（ADR-0005 / ADR-0012）。
- **纯算法压缩 vs LLM 摘要压缩**：避免额外网络延迟、递归调用故障面与幻觉风险，换取确定性、零成本、可重放（ADR-0020）。
- **标准库优先 vs 引入框架**：降低供应链与 CGO 复杂度，依赖图可控，配合 `architecture_test.go` 体积门禁防腐化。
- **消费者定义接口**：`api` 包声明 `TaskRunner`（仅 `Enqueue` / `Trace`），`*worker.Pool` 隐式实现，`api` 层可无真实池做轻量单测。

---

## 5. 相关文档索引

- 决策记录：`docs/adr/0001`（项目定位）、`0002`（Worker Pool）、`0008`（Agent 工具循环）、
  `0013`（分层解耦，Pi 哲学）、`0015`（崩溃恢复）、`0016`（SSE 流式）、`0017/0018`（检索与评测）、
  `0019`（事务队列与流式推断）、`0020`（会话状态机与压缩）、`0021`（上游限流退避、Token 成本可观测与诊断端口）。
- 实现计划：`docs/implementation_plan_p1.md` … `p8.md`。
- 评测实证：`docs/eval/2026-10-25-hybrid-r3.md` 等。
- 运行与 API 契约详见 `README.md`。
