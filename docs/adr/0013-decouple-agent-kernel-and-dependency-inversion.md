# ADR-0013：领域架构正名与分层解耦 —— 汲取 Pi 分层哲学，抽取 agent 内核、tool 插件库与 API 依赖倒置

- **日期**：2026-10-16
- **状态**：已接受
- **上游依赖**：ADR-0001（项目定位）、ADR-0002（异步 Worker Pool）、ADR-0008（Agent 工具调用循环）、ADR-0011（超时与优雅停机）、ADR-0012（Trace 观测与传输解耦）

## 背景

在完成 ADR-0012 规划的 11 项 P2 任务（任务回收、早期错误终态、Server 超时、步骤追踪与 Demo 增量渲染）交付后，我们依据知识库内沉淀的工业级 Agent SDK 规范（`01-Rules/AGENT-SDK-PATTERNS.md` 与 `08-Projects/Pi-Agent/ARCHITECTURE.md`）对当前项目进行整体工程架构审视。

用户提问：「当前项目符合我们知识库内提炼的 agent 开发模板吗？有没有做到 pi 那样的水准？」，并进一步指出：「pi 那种设计也值得思考，可以借鉴设计思路，我们这种乱七八糟的层级设置架构很容易导致双向依赖，影响重构和后续扩展升级。」

通过对仓库依赖拓扑（`go list`）和代码职责进行静态物理审计，确认存在以下三大核心架构缺陷：

1. **`worker` 包职责过度混杂（Kitchen Sink 反模式）**：
   - `worker/` 目录同时堆叠了四重不同领域的逻辑：
     - **并发基础设施**：`pool.go`（Goroutine 生命周期、Channel 背压快速失败、`wg.Wait()`、优雅停机取消树、任务状态持久化同步）；
     - **认知决策循环**：`agent.go` 中的 `runAgent`（ReAct 多步循环、Prompt 拼接、`turns` 对话历史）；
     - **具体工具实现与计算引擎**：`defaultTools`（`calculate` 与 `current_time`）以及算术 AST 解析器 `evalArithmetic`；
     - **观测追踪缓存**：`trace.go`（`TraceStep` 契约定义与 `traceBuffer` 有界环形缓冲）。
   - 这导致测试边界严重污染：测试一个纯函数算术表达式需要进 `worker` 包，测试模型循环与工具回调需要与并发池混在一起。
2. **类型归属倒挂导致越层依赖**：
   - `api/handler.go:115-118` 定义对外 JSON 响应 `taskWithTrace`，字段是 `Steps []worker.TraceStep`。
   - `TraceStep`、`KindThink` 等概念本属于 **Agent 认知域与观察契约**，却被错误地定义在负责并发池调度的 `worker` 包中。导致负责 HTTP 协议交互的 `api` 包为了输出一个模型思考步骤，被迫直接依赖底层的 `worker` 并发调度包。
3. **缺少依赖倒置（DIP 缺失），API 强绑定具体结构体**：
   - `api/handler.go:21` 中的 `Handler` 直接强引用了具体的 `pool *worker.Pool` 指针类型。
   - 违反了 Go 核心工程谚语 *“Accept interfaces, return structs”* 与面向对象依赖倒置原则。导致 `api` 层无法与并发池解耦，无法单独做轻量单元测试（必须构造真实的 Pool 和上下文），且未来若引入同步任务执行或不同执行器时必须动及 `api` 核心代码。
4. **领域模型缺失（Missing Domain Model）**：
   - 项目名称为 `agent-api`，然而整个代码库中竟然没有一个包叫 `agent`，核心认知逻辑寄生在并发池工作线程中，丧失了最关键的业务领域边界。

## 双镜头苏格拉底审讯结论（Dual-Lens Socratic Grill）

本轮审讯聚焦于两个核心议题：**系统定位权衡** 与 **分层解耦拓扑**。

### 🎯 产品与用户体验层（UX / Positioning）
- **警惕定位错位与伪需求**：Pi（`earendil-works/pi`）是单用户本地交互式 Coding Agent，核心交互是 TUI/CLI、长对话会话树（Session Tree 分叉与回退）、Steering 实时插队与权限审批拦截。而 `agent-api` 的根本定位是**高并发异步任务执行后端服务**（以 HTTP 提交、Worker Pool 抢占、快速 503 背压、Prometheus 指标、标准库优先为技术卖点）。
- 若在 `agent-api` 中盲目复刻 Pi 的会话树或双向 WebSocket，会颠覆已证明成熟的无状态轮询架构（ADR-0005 与 ADR-0012），背离当前用户主要用于高并发 Go 后端面试防守与展示的核心价值。
- **真正的 UX 价值**：借鉴 Pi 的不是其终端交互形式，而是其**内核最小化、工具分层化、错误成消息（errors-as-messages）**的优雅工程哲学。将工具独立后，开发者接入新工具的认知摩擦大幅下降。

### 🤖 技术与系统工程层（Systems Rigor / Topology）
- **消除循环导入（Import Cycle）隐患**：Go 语言对包间循环依赖零容忍。当前 `api -> worker`、`worker -> store/llm/metrics` 的结构极脆弱。一旦工具执行需要回调存储或 API 上下文，立即引发编译死锁。
- **纯净单向依赖图**：必须重塑依赖拓扑，构建符合分层架构（Layered Architecture）的严格无环有向图（DAG）。

---

## 候选方案与权衡

### 针对分包拓扑（Q1）
- **方案 1-A（推荐）**：**按 Pi 哲学彻底正名，拆出独立 `agent/` 内核与 `agent/tool/`（或 `tool/`）模块**。
  - 抽取 `agent/`：收拢 `TraceStep`、`Kind*` 常量、`traceBuffer`、`Engine`（封装 `runAgent`）；
  - 抽取 `tool/`：定义抽象 `Tool` 接口，独立存放 `calculate`（含 `evalArithmetic`）与 `current_time` 实现；
  - 净化 `worker/`：只保留 `Pool`、队列背压、任务超时与取消树，纯粹作为 Job Concurrency Executor。
- **方案 1-B**：仅把 `TraceStep` 移出到公共 `types/` 包，保留 `worker/agent.go`。
  - *否决理由*：治标不治本，`worker` 依然充斥着非并发逻辑，未能解决领域模型丢失与大杂烩问题。

### 针对 API 依赖解耦（Q2）
- **方案 2-A（推荐）**：**Go 惯用的 Consumer-Defined Interface（消费者定义接口）**。
  - 在 `api/handler.go` 中由使用者自行声明接口：
    ```go
    type TaskRunner interface {
        Enqueue(id string) error
        Trace(id string) []agent.TraceStep
    }
    ```
  - `NewHandler` 接收 `TaskRunner` 接口，不再直接持有 `*worker.Pool`。
- **方案 2-B**：保持 `api.Handler` 直接持有 `*worker.Pool` 指针。
  - *否决理由*：丧失轻量 Mock 能力，维持跨包强耦合。

---

## 决策

**决定采纳方案 1-A 与方案 2-A，启动领域架构正名与分层解耦重构**：

```markdown
                              【重构后架构依赖拓扑】

          ┌───────────────────────────────────────────────────┐
          │                      cmd/main                     │  (装配根节点)
          └──────┬──────────────┬──────────────┬──────────────┘
                 │              │              │
                 ▼              ▼              ▼
            ┌─────────┐    ┌─────────┐    ┌─────────┐
            │   api   │    │ worker  │    │  store  │
            └────┬────┘    └────┬────┘    └─────────┘
                 │ (仅依赖接口)   │ (调度)
                 ▼              ▼
            ┌────────────────────────┐
            │         agent          │  (认知内核：Engine, TraceStep, Buffer)
            └───────────┬────────────┘
                        │
                        ▼
            ┌────────────────────────┐
            │       agent/tool       │  (工具注册表与实现：calculate, time...)
            └───────────┬────────────┘
                        │
                        ▼
            ┌────────────────────────┐
            │          llm           │  (底层模型协议：Chat, ChatWithTools)
            └────────────────────────┘
```

### 1. 契约规约与职责划分

| 包名 | 领域定位 | 核心职责 | 允许依赖的内部包 |
|---|---|---|---|
| `llm/` | 模型协议适配 | OpenAI 兼容 HTTP 通信，提供 `ChatWithTools` | 零内部依赖（纯标准库） |
| `agent/tool/` | 工具系统 | `Tool` 接口、工具参数解析与执行，内置 `calculate` / `current_time` | `llm/` |
| `agent/` | 认知内核 | `TraceStep`、`traceBuffer`、`Engine`（ReAct 循环）、步骤回调 | `llm/`, `agent/tool/` |
| `store/` | 任务状态持久化 | `TaskStore` 接口、内存 `Store`、SQLite `SQLStore` | 零内部依赖 |
| `worker/` | 并发调度器 | `Pool`（Goroutine 池）、任务入队出队、超时控制、状态流转 | `store/`, `agent/`, `metrics/` |
| `api/` | HTTP 接入与网关 | 路由中间件链、JSON 契约、`TaskRunner` 消费者接口、Demo 页面 | `store/`, `agent/`, `metrics/` |
| `main.go` | 组合根 (Composition Root) | 读取配置、依赖注入组装各层、优雅停机管理 | 全部包 |

### 2. 接口倒置规约
在 `api/handler.go` 中声明：
```go
type TaskRunner interface {
    Enqueue(id string) error
    Trace(id string) []agent.TraceStep
}
```
`*worker.Pool` 隐式实现该接口，`api` 层严禁出现 `import "agent-api/worker"`。

---

## 与已学知识的映射

| 设计点 | 知识库规范 / 原则依据 |
|---|---|
| **三层工具与认知分离** | `08-Projects/Pi-Agent/ARCHITECTURE.md` §1（Agent Loop 最小化）、§2（工具三层递进） |
| **Errors-as-messages 坚持** | `01-Rules/AGENT-SDK-PATTERNS.md` §2.2（工具错误绝不打断循环，作为消息回喂模型） |
| **消费者定义接口** | [[03-Languages/GO/GO-STANDARDS]]（Go 接口设计规范：谁消费谁定义，保持接口最小化） |
| **单向依赖 DAG 保证** | Clean Architecture / 依赖倒置原则（高层模块不依赖低层细节） |
| **零破坏性渐进迁移** | RFC 2119 规范约束，保证外部 HTTP JSON 契约与全部 57 项既有测试行为 100% 兼容 |

---

## 后果与收益

- ✅ **包结构清晰正名**：诞生真正的 `agent/` 领域内核与 `tool/` 插件库，架构对齐工业级 Agent 设计规范。
- ✅ **切断跨层类型倒挂**：`api` 不再依赖 `worker`，彻底消灭循环依赖的架构土壤。
- ✅ **API 层可测试性跃迁**：`api.NewHandler` 接入 `TaskRunner` 接口，无需初始化真实 worker pool 即可进行确定性 HTTP 单元测试。
- ✅ **工具扩展具备高内聚性**：新增工具（如文件读取、网络搜索）只需在 `tool` 包内新增实现，无需碰 `worker` 并发代码。
- ⚠️ **重构迁移面**：需要将已有的 `worker/agent.go`、`worker/trace.go` 及相关测试平滑平移至 `agent/` 与 `agent/tool/`，更新全仓 import 路径，确保 57 个测试 0 破坏无感通过。
