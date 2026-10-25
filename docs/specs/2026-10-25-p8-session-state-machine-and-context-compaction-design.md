# Design Specification: P8 Multi-Round Session State Machine & Context Compaction

**Date**: 2026-10-25  
**Status**: APPROVED  
**Author**: Hermes Agent & User Collaborative Design  
**Target Path**: `docs/specs/2026-10-25-p8-session-state-machine-and-context-compaction-design.md`  

---

## 1. Problem Statement & User Value

- **Why build this**:  
  在 P0–P7 演进中，`agent-runtime` 已经构建了极高吞吐、支持 SQLite WAL 事务队列削峰（10,000+ 任务排队）、支持 SSE Token 级打字机推断与受控混合检索的高性能 Agent 运行时。然而，系统在交互与状态维度上仍处于「单轮任务孤岛（Stateless Task）」模式：
  1. `POST /run` 仅接收单一 `prompt`，无法感知前序对话结论或工具执行结果，用户在多轮交互中被迫手动反复复制上下文；
  2. 随着连续交互轮次增加，Prompt 上下文呈二次方膨胀，若无主动的上下文配给与压缩机制，极易击穿模型的上下文硬上限或耗尽用户 API Token 预算；
  3. `worker.NewPool(3, ...)` 仍硬编码为 3，无法根据物理机核数与服务商 RPM/TPM 弹性配置。
- **Target Scenario & Persona**:  
  希望基于 `agent-runtime` 构建多轮连续推导工作流（如交互式代码排障、递进式知识库问答、长效决策辅助）的系统开发者与后端集成方。
- **Cost of Doing Nothing**:  
  若不做，系统永远只能作为单次离线 Job 执行器，无法承载真实的对话式 Agent 交互；多轮调用必须由调用方在业务层自建消息拼接，缺乏统一的持久化状态机与上下文防爆保障。

---

## 2. User Journey & Core Flow

- **Happy Path (<= 3 steps to value)**:
  1. **开启或指定会话**: 客户端调用 `POST /run` 时携带 `session_id: "sess_001"` 与 `prompt: "检索 0015 号 ADR 的核心结论"`。服务端返回 `202 Accepted`；
  2. **上下文感知推导**: Worker 领单后，从 Store 提取该 Session 的历史消息与系统提示词。若总 Token 接近安全预算（默认 16,000 tokens），自动触发滑动窗口与谱系压缩（保留 System Prompt 与最近 K 轮核心意图），驱动模型执行推导；
  3. **状态递进与多轮对账**: 任务完成后，当前轮的 User 提示词与 Assistant 最终回复自动追加至 Session 历史。客户端第二轮发送 `session_id: "sess_001"` 与 `prompt: "根据上一轮的结论，帮我写一个恢复测试"` 时，模型无缝理解上下文并精准作答。
- **Friction Points & Mitigation**:  
  - *摩擦点 1：同一 Session 并发提交冲突*。缓解策略：在 Store 层通过 SQLite WAL 事务原子追加消息，配合 Session 级更新时间戳与乐观排它，防止乱序。
  - *摩擦点 2：长对话导致 Token 暴涨或 400 Context Overflow*。缓解策略：在 Agent 进入 LLM 循环前执行纯本地无损/微损滑动窗口压缩，确保送入模型的上下文严格受控。
- **Error Feedback & Recovery**:  
  - 会话不存在或已失效时，返回结构化 404 或根据参数自动初始化新会话；
  - 上下文超出物理极限且压缩失败时，安全回退为「保留 System Prompt + 当前 User Prompt」紧急放行，绝不阻断任务。

---

## 3. Architecture & Data Models

### 3.1 Components & Boundaries

1. **`store/` 领域扩展 (Session & Message Store)**:
   - 在 `store` 包扩展会话持久化契约，保持纯 Go 标准库与 `modernc.org/sqlite` 实现：
     ```go
     type Session struct {
         ID        string    `json:"id"`
         CreatedAt time.Time `json:"created_at"`
         UpdatedAt time.Time `json:"updated_at"`
     }

     type SessionMessage struct {
         ID        int64     `json:"id"`
         SessionID string    `json:"session_id"`
         Role      string    `json:"role"`    // "user" | "assistant" | "system"
         Content   string    `json:"content"`
         CreatedAt time.Time `json:"created_at"`
     }

     type SessionStore interface {
         GetOrCreateSession(ctx context.Context, id string) (Session, error)
         AppendSessionMessage(ctx context.Context, msg SessionMessage) error
         GetSessionMessages(ctx context.Context, sessionID string, limit int) ([]SessionMessage, error)
     }
     ```
   - 内存 `Store` 与 SQLite `SQLStore` 双实现，SQLite DDL 新增 `sessions` 与 `session_messages` 表。
2. **`agent/` 谱系压缩器 (Context Compactor)**:
   - 新增 `agent/compactor.go`：
     ```go
     type CompactionConfig struct {
         MaxContextTokens    int     // 上下文安全预算，默认 16000
         CompactionRatio     float64 // 压缩保留比例，默认 0.5
         EstimateTokenPerChar float64 // 字符/Token 估算比率（中文/英文混合估算）
     }

     // CompactMessages 保持 System 提示词不变，对过长历史消息执行滑动截断与摘要占位
     func CompactMessages(msgs []llm.Message, cfg CompactionConfig) []llm.Message
     ```
3. **`worker/` 调度器与并发配置**:
   - `config/config.go` 新增 `WorkerConcurrency int`（默认 `3`，环境变量 `WORKER_CONCURRENCY` 覆盖）；
   - `worker.Pool` 构造参数支持动态并发度；
   - `worker.process` 检测到任务绑定的 `SessionID` 时，拉取并压缩历史消息喂入 `agent.RunWithOptions`，并在任务成功终态时将本轮问答原子回写进 `SessionStore`。
4. **`api/` 会话路由与 REST 接口**:
   - `POST /run` 请求体支持 `session_id`（可选；若留空则保持无状态 Task，向下兼容）；
   - 新增 `GET /sessions/{id}`（查询会话元数据）与 `GET /sessions/{id}/messages`（查询会话历史）。

---

## 4. Edge Cases & Resilience

- **历史消息体积过大导致 OOM**:  
  `GetSessionMessages` 强制设置拉取条数上限（如默认 `limit = 100`），避免单次查询打满内存；压缩器就地操作切片指针，杜绝深拷贝内存暴涨。
- **并发写入同一会话**:  
  SQLite `INSERT INTO session_messages` 依赖自增主键保证消息插入严格按物理时间有序；内存 Store 使用 `sync.Mutex` 全程保护。
- **停机与事务一致性**:  
  任务执行失败（如模型超限、超时）时，当前轮的错误回复不污染 Session 主历史，保证下一轮重试依然基于干净的成功上下文。
- **向后兼容性（100% Backward Compatible）**:  
  凡是不带 `session_id` 的调用，完全沿用 P0–P7 现有的独立 Task 机制，已有 278 个单测无需重构。

---

## 5. Test Strategy

- **Unit Tests**:
  - `store/session_test.go`: 内存与 SQLite 双实现下 `GetOrCreateSession`、`AppendSessionMessage`、FIFO 排序与持久化重启验证；
  - `agent/compactor_test.go`: 验证消息超出预算时，System Prompt 与最新 User Prompt 绝对保留，中间冗余轮次按比例折半截断；
  - `config/config_test.go`: 验证 `worker_concurrency` 与 `max_context_tokens` 的文件配置与环境变量覆盖；
  - `worker/pool_test.go`: 验证可配 Worker 并发度与 Session 消息回写时序。
- **Integration Scenarios**:
  - `api/session_integration_test.go`: 构造端到端连续 3 轮问答（问答 1 → 问答 2 引用问答 1 → 问答 3 验证压缩生效），验证会话状态贯穿完整 HTTP/Store/Worker 链路。
- **Verification Commands**:
  - `GOWORK=off go test -race ./... -count=1`（12 个包全绿，0 race）；
  - `GOWORK=off go test -v -run TestArchitecture . -count=1`（AST DAG 白名单与 400 行预算零突破）。

---

## 6. Fog of War & Scope Boundaries

- **Fog of War (Not Yet Specified)**:
  - 自动调用轻量 LLM 进行「递归摘要压缩（Recursive Summarization）」在长文中质量最佳，但本次暂不引入额外的异步摘要调用，优先采用极速、确定性高、零额外 Token 开销的「结构化滑动窗口截断（Sliding Window with Anchor Preservation）」，避免在主执行链中引入额外的网络依赖。
- **Out of Scope (Explicit Non-Goals)**:
  - **前端 UI 重构（`api/demo.html`）**: 用户明确要求「前端最后做」，本次 P8 阶段专注于纯服务端状态机、存储与 API 契约，不改动前端页面；
  - **MCP (Model Context Protocol) 平台生态**: Option C 规划的外部 stdio 进程工具沙箱留待后续 P9 阶段，本次不碰外部进程管控。

---

## Self-Review Checklist

- [x] Zero "TODO" or "TBD" placeholders.
- [x] No ambiguous assumptions or hidden scope creep.
- [x] The Ladder of Reuse checked: preferred stdlib (`sync`, `time`, `net/http`) + `modernc.org/sqlite` without third-party frameworks.
- [x] Strict RFC 2119 discipline followed across all layers.
