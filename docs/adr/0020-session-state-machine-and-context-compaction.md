# ADR-0020：会话状态机、滑动上下文安全压缩与并发池弹性配置

- **日期**：2026-10-25
- **状态**：已实施（P8 里程碑通过，2026-10-25）
- **上游依赖**：ADR-0002（异步任务与并发池）、ADR-0010（SQLite 持久化）、ADR-0013（分层解耦）、ADR-0017（检索工具）、ADR-0019（事务性任务队列与打字机流式推断）
- **下游影响**：`store.SessionStore` 接口及双实现、`agent.CompactMessages` 纯算法滑动截断、`worker/session.go` 组装解耦、`config.WorkerConcurrency` & `config.MaxContextTokens` 配置项、`api/session_handler.go` REST 路由
- **上位规范**：vault `01-Rules/LAYERED-ARCHITECTURE.md`（同包文件拆分与行数门禁）、`01-Rules/AGENT-HARNESS-ARCHITECTURE.md`（无依赖认知循环）

---

## 背景与痛点

在 P0–P7 的演进过程中，`agent-runtime` 已经建成了包含「ReAct 认知循环、抢占式取消、崩溃恢复、混合知识检索、事务性削峰队列与 SSE 原生打字机」的完整工业级单机运行时。然而，在真实生产场景下，仍存在以下三项核心缺陷：

1. **单轮任务孤岛（Stateless Single-Turn Island）**：
   - 每次提交任务（`POST /run`）仅执行单轮 `prompt -> result`，执行完毕后认知上下文随 goroutine 销毁。
   - 用户无法进行追问、纠错或在同一业务上下文下多步迭代（例如先提问「ADR-0015 的核心机制是什么？」，后追问「基于此机制，任务超时如何兜底？」）。
2. **多轮历史累积引发的上下文爆炸风险（Context Window Explosion）**：
   - 简单的多轮历史追加会导致 Token 消耗随轮次线性甚至二次方剧增。
   - 一旦累积 Token 超过大模型安全窗口（如 16k/32k 限制），会导致直接抛出上游 `400 context_length_exceeded`，使整个会话不可逆瘫痪。
3. **并发池硬编码与配置文件遗留债务**：
   - `main.go` 中长期硬编码 `worker.NewPool(3, ...)`，无法根据服务器 CPU/RAM 或外部 API 配额动态调整 worker 并发度。
   - 前端 Demo 未升级，技术架构与对外体验存在断层。

---

## 决策

### 1. 会话状态机与双层存储持久化（SessionStore）

在 `store` 领域新增会话抽象与存储双实现，彻底解耦会话状态与单次任务：

- **数据模型**：
  ```go
  type Session struct {
      ID        string    `json:"id"`
      CreatedAt time.Time `json:"created_at"`
      UpdatedAt time.Time `json:"updated_at"`
  }

  type SessionMessage struct {
      ID        int64     `json:"id"`
      SessionID string    `json:"session_id"`
      Role      string    `json:"role"` // "user" | "assistant" | "system"
      Content   string    `json:"content"`
      CreatedAt time.Time `json:"created_at"`
  }
  ```
- **核心接口 (`store.SessionStore`)**：
  - `GetOrCreateSession(ctx context.Context, id string) (Session, error)`
  - `AppendSessionMessage(ctx context.Context, msg SessionMessage) error`
  - `GetSessionMessages(ctx context.Context, sessionID string, limit int) ([]SessionMessage, error)`
- **SQLite 生产持久化 (`store/sql_session.go`)**：
  - 新建 `sessions` 表与 `session_messages` 表；
  - 针对会话高频查询建立复合索引：`CREATE INDEX IF NOT EXISTS idx_session_messages_session_id ON session_messages(session_id, id ASC)`；
  - `tasks` 表增加 `session_id TEXT NOT NULL DEFAULT ''` 字段，使异步任务与所属会话具备强关联性；
  - 严格保持 `store/session.go` (130 行) 与 `store/sql_session.go` (160 行) 职责单一，严禁单文件超标。

### 2. 纯算法确定性滑动截断压缩（Context Sliding Window Compactor）

放弃引入额外 LLM 总结的不可控开销方案（避免引入额外网络延迟、递归调用故障与事实幻觉），在 `agent/compactor.go` 实现纯算法、确定性且零额外内存分配的滑动窗口压缩器：

- **无堆逃逸 Token 估算 (`EstimateTokens`)**：
  - 基于中英文混排比例（默认 0.5 Token/Rune），对消息内容和工具调用参数执行极速扫描，避免字符串序列化开销。
- **轮次原子保全与折半截断 (`CompactMessages`)**：
  - **预算守卫**：估算总 Token $\le$ `MaxContextTokens` 时，原样放行，零额外耗时；
  - **首尾强锚定**：绝对保留开头的 System 提示词（若存在）与末尾最新 1 轮消息（最后一条 User 提示及当前上下文）；
  - **轮次边界感知**：按 User 消息划分对话轮次，严禁腰斩 Assistant 的 ToolCall 与 ToolResult 配对结构；
  - **折叠注记**：中间历史按比例折半截断，在截断处插入标准系统通知：
    `[系统提示：为避免上下文超限，更早的 N 轮历史对话已自动折叠]`。

### 3. Worker 多轮拼装与历史回写解耦（Anti-Bloat Architecture）

为遵守架构行数门禁（`worker/pool.go` 已有 385 行，硬上限 400 行），会话相关逻辑严禁直接堆叠在 `pool.go`：

- **同包切分 (`worker/session.go`)**：
  - 实现 `assembleSessionMessages`：拉取会话历史 $\rightarrow$ 组装系统提示与当前 Prompt $\rightarrow$ 经过 `agent.CompactMessages` 压缩 $\rightarrow$ 注入 `agent.RunOptions.HistoryMessages`；
  - 实现 `recordSessionTurn`：任务成功完成时，原子写回当前轮的 User 提示词与 Assistant 回复；
  - 严格防范静默吞错（Red Card #4）：对 `AppendSessionMessage` 的错误进行显式捕获与包装，通过 `slog.Warn` 暴露。
- **并发时序护栏 (TOCTOU 防护)**：
  - 在 `worker/pool.go:process` 中，必须先登记 `inFlight.Store(id, cancelCause)`，再更新数据库 `tasks.Update(id, StatusRunning)`，杜绝外部取消检查与在飞登记的竞态失效。

### 4. 并发度与上下文窗口弹性配置（Config Layer）

在 `config/config.go` 引入两项工业级配置，彻底消除代码硬编码：

- `worker_concurrency`: 后台并发执行任务的 Worker 数量，正整数，默认 `3`，环境变量 `WORKER_CONCURRENCY` 优先级最高。
- `max_context_tokens`: 多轮会话上下文安全 Token 预算，正整数，默认 `16000`，环境变量 `MAX_CONTEXT_TOKENS` 优先级最高。
- 完善 `config.yaml.example` 说明并同步更新未知配置校验白名单。

### 5. 会话 REST 路由与参数绑定（API Layer）

在 `api/session_handler.go` 实现会话查询路由，保持 `api/handler.go` 紧凑（242 行）：

- `POST /run`：请求体扩展可选字段 `session_id`。若传入则绑定会话落库入队，响应体回传 `{"task_id": "...", "session_id": "..."}`。
- `GET /sessions/{id}`：获取指定会话的创建与更新元数据（若 Store 不支持会话则响应 `501 Not Implemented`）。
- `GET /sessions/{id}/messages?limit=N`：分页按时间升序返回历史消息（空时返回 `[]` 而非 `null`）。
- 两条新路由无缝继承统一中间件链（`Recover` + `RequestID` + `Limit` + `Auth`）。

---

## 验证与收益

1. **测试与竞态验证**：
   - 全仓 12 个 Go 包全量通过 `GOWORK=off go test -race ./... -count=1`（321 个测试函数，498 次运行用例，0 data race）；
   - 并发多轮会话回写（`TestSession_MultiRound`）、上下文溢出压缩（`TestSession_CompactionApplied`）、取消抢占时序（`TestPool_CancelInFlightTask`）均通过高并发压力测试。
2. **架构门禁与体积守卫**：
   - `TestArchitecture_FileLineBudget` 全绿：全仓 83 个 Go 文件，生产代码全部 $\le 400$ 行（`worker/session.go` 76 行，`worker/pool.go` 395 行，`store/sql_session.go` 160 行，`api/session_handler.go` 80 行）；
   - `TestArchitecture_DAGImports` 全绿：无任何非法跨层依赖与反向引用。
