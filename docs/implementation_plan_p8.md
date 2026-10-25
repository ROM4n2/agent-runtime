# Implementation Plan: P8 Multi-Round Session State Machine & Context Compaction

**Branch**: `feat/p8-session-and-compaction` (isolated from `master`)  
**Spec Reference**: `docs/specs/2026-10-25-p8-session-state-machine-and-context-compaction-design.md`  
**ADR Reference**: `docs/adr/0020-session-state-machine-and-context-compaction.md` (to be authored in Batch 6)  
**Repo Invariants Anchor**: `.agent-context.md` and `AGENTS.md` (all commands `GOWORK=off`, sequential testing, strict line budget <= 400 prod / <= 800 test lines, root AST DAG whitelist)  

---

## [NOTE] Architectural Anti-Bloat Strategy (文件防膨胀与模块解耦战略)

1. **绝对行数预算防护（Line Budget Guard）**：
   - 当前 `worker/pool.go` 已有 **385 行**（逼近 400 行硬上限），**严禁**直接在 `worker/pool.go` 内堆叠会话加载、回写与压缩逻辑；
   - 采用同包职责切分（Intra-package Split）：新建 `worker/session.go`，将会话上下文组装与消息追加逻辑完全内聚在独立文件中；
   - 类似地，`store/` 包新建 `store/session.go` 与 `store/sql_session.go`，`api/` 包新建 `api/session_handler.go`；
   - 根级 `TestArchitecture_FileLineBudget` 门禁全程自动拦截任何 >400 行生产文件。
2. **AST DAG 依赖单向守恒（AST DAG Invariant）**：
   - `store` 保持叶子包地位（内部依赖 `{}`），会话实体与表操作完全自包含；
   - `agent/compactor.go` 仅依赖 `llm.Message` 与标准库，依赖集合完全受限在 `{"agent-runtime/llm"}` 内；
   - `worker` 通过已有白名单依赖 `agent` 与 `store`；
   - `api` 通过已有白名单依赖 `store`；
   - 保持 0 新增非法跨层依赖与 0 新增第三方包。

---

## [NOTE] Active Frontier (Unblocked Tasks)

### Gate 0: Branch Isolation, Baseline Check & Plan Ingestion [Tier: 1-Light] [Mode: AFK]

**Files:**
- Create: `docs/implementation_plan_p8.md`
- Verify: Git working tree clean on `master`, checkout new branch `feat/p8-session-and-compaction`

**Interfaces:**
- Consumes: `master` HEAD (`0e928eb`)
- Produces: Branch `feat/p8-session-and-compaction` with clean baseline

**Subagent Prompt Scaffold:**
> "Verify repo baseline on master: run `GOWORK=off go test -race ./... -count=1` and ensure 12 packages PASS (278 tests / 442 runs). Create and checkout branch `feat/p8-session-and-compaction`. Verify working tree clean."

**Step Breakdown:**
- [ ] Step 1: Run full test suite with `-race` to confirm 100% green baseline.
- [ ] Step 2: Checkout isolated branch `feat/p8-session-and-compaction`.
- [ ] Step 3: Commit plan and spec documents as initial milestone commit.

---

### Task 1: Store Domain Session Persistence & Message Stream (`SessionStore`) [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Create: `store/session.go` (Session models, `SessionStore` interface, memory implementation)
- Create: `store/sql_session.go` (SQLite `sessions` & `session_messages` DDL and queries)
- Create: `store/session_test.go` (Memory store unit tests)
- Create: `store/sql_session_test.go` (SQLite persistent session tests)

**Interfaces:**
- Consumes: `store.TaskStore` baseline, `modernc.org/sqlite`
- Produces:
  ```go
  // store/session.go
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

  type SessionStore interface {
      GetOrCreateSession(ctx context.Context, id string) (Session, error)
      AppendSessionMessage(ctx context.Context, msg SessionMessage) error
      GetSessionMessages(ctx context.Context, sessionID string, limit int) ([]SessionMessage, error)
  }
  ```

**Subagent Prompt Scaffold:**
> "Implement Task 1 of 6: Session persistence and message streaming in `store` package.
> 1. In `store/session.go`: define `Session`, `SessionMessage`, `SessionStore` interface. Implement `SessionStore` on `*Store` (memory).
> 2. In `store/sql_session.go`: implement `SessionStore` on `*SQLStore`. Add schema migrations for `sessions` and `session_messages` (with index on `session_id, id ASC`).
> 3. In `store/session_test.go` and `store/sql_session_test.go`: write tests covering `GetOrCreateSession`, message ordering (FIFO), message retrieval limit, and persistence across database reopen.
> 4. Keep `store/session.go` <= 250 lines and `store/sql_session.go` <= 250 lines!
> 5. Run `GOWORK=off go test -race ./store/... -count=1` and `GOWORK=off go test -v -run TestArchitecture . -count=1`."

**Step Breakdown:**
- [ ] Step 1: Write failing tests in `store/session_test.go` and `store/sql_session_test.go` (RED).
- [ ] Step 2: Implement `store/session.go` (memory) and `store/sql_session.go` (SQLite) (GREEN).
- [ ] Step 3: Verify FIFO message ordering and transaction safety under concurrent writes.
- [ ] Step 4: Verify architecture gates (<= 400 lines per file). Commit with message `feat(store): add SessionStore interface and memory/sqlite implementations`.

---

### Task 2: Pure Algorithmic Context Compactor & Sliding Window (`agent.CompactMessages`) [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Create: `agent/compactor.go` (deterministic sliding window context compactor)
- Create: `agent/compactor_test.go` (comprehensive boundary and budget tests)

**Interfaces:**
- Consumes: `llm.Message`
- Produces:
  ```go
  // agent/compactor.go
  type CompactionConfig struct {
      MaxContextTokens     int     // 安全 Token 上限，默认 16000
      CompactionRatio      float64 // 截断保留比例，默认 0.5
      EstimateTokenPerChar float64 // 字符估算比例，默认 0.5 (中文+英文混合加权)
  }

  func DefaultCompactionConfig() CompactionConfig

  // EstimateTokens 快速无内存分配地估算一批消息的总 Token 消耗
  func EstimateTokens(msgs []llm.Message, ratio float64) int

  // CompactMessages 对消息历史执行安全滑动窗口压缩：
  // 1. 如果估算总 Token <= MaxContextTokens，原样返回切片；
  // 2. 如果超限：绝对保留开头的 System 消息（若有）与末尾最新的一轮 User 消息；
  // 3. 对中间的历史消息进行折半滑动剔除，并在截断点插入一条包含省略标记的系统级消息。
  func CompactMessages(msgs []llm.Message, cfg CompactionConfig) []llm.Message
  ```

**Subagent Prompt Scaffold:**
> "Implement Task 2 of 6: Context budget estimation and deterministic sliding window compaction in `agent` package.
> 1. Create `agent/compactor.go`: implement `CompactionConfig`, `DefaultCompactionConfig()`, `EstimateTokens()`, and `CompactMessages()`.
> 2. Ensure zero external dependencies. Pure stdlib + `llm.Message`.
> 3. In `agent/compactor_test.go`: test:
>    - Messages within budget return untouched.
>    - Messages exceeding budget preserve initial System message and trailing User message.
>    - Middle messages truncated with placeholder text.
>    - Empty messages and single message edge cases handled gracefully without panic.
> 4. Keep `agent/compactor.go` <= 200 lines and `agent/compactor_test.go` <= 400 lines.
> 5. Run `GOWORK=off go test -race ./agent/... -count=1` and architecture gate."

**Step Breakdown:**
- [ ] Step 1: Write failing tests in `agent/compactor_test.go` (RED).
- [ ] Step 2: Implement `agent/compactor.go` (GREEN).
- [ ] Step 3: Run mutation probe (e.g. disable truncation threshold check -> ensure test fails).
- [ ] Step 4: Verify architecture gate and commit with message `feat(agent): implement deterministic context sliding window compaction`.

---

### Task 3: Config Layer Worker Concurrency & Context Token Budget [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Modify: `config/config.go` (add `WorkerConcurrency` and `MaxContextTokens`)
- Modify: `config.yaml.example` (document new configuration keys)
- Modify: `config/config_test.go` (test parsing, env override, and defaults)

**Interfaces:**
- Consumes: `os.Getenv`, `config.yaml`
- Produces:
  ```go
  type Config struct {
      // ... existing 10 fields ...
      WorkerConcurrency int // 默认 3，可配
      MaxContextTokens  int // 默认 16000，可配
  }
  ```

**Subagent Prompt Scaffold:**
> "Implement Task 3 of 6: Add `WorkerConcurrency` (default 3) and `MaxContextTokens` (default 16000) to `config/config.go`.
> 1. Parse `worker_concurrency` and `max_context_tokens` from YAML file.
> 2. Support `WORKER_CONCURRENCY` and `MAX_CONTEXT_TOKENS` environment variable overrides.
> 3. Enforce valid defaults if <= 0: WorkerConcurrency=3, MaxContextTokens=16000.
> 4. Document both in `config.yaml.example`.
> 5. Add unit tests in `config/config_test.go`.
> 6. Keep `config/config.go` <= 250 lines (currently 186 lines).
> 7. Run `GOWORK=off go test -race ./config/... -count=1` and architecture gate."

**Step Breakdown:**
- [ ] Step 1: Write failing tests in `config/config_test.go` (RED).
- [ ] Step 2: Update `config/config.go` and `config.yaml.example` (GREEN).
- [ ] Step 3: Run mutation probe and verify `-race` tests pass.
- [ ] Step 4: Commit with message `feat(config): add worker_concurrency and max_context_tokens configuration`.

---

### Task 4: Worker Dispatcher Session Association & History Backfill (`worker/session.go`) [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Create: `worker/session.go` (separate file to prevent `worker/pool.go` from exceeding 400 lines!)
- Modify: `worker/pool.go` (accept dynamic concurrency, mount session store)
- Modify: `main.go` (wire `conf.WorkerConcurrency` and `s` as `SessionStore`)
- Create: `worker/session_test.go` (session-aware execution and history backfill tests)

**Interfaces:**
- Consumes: `store.SessionStore`, `agent.CompactMessages`, `config.Config`
- Produces:
  ```go
  // worker/session.go
  // assembleSessionMessages loads history messages for sessionID, appends current prompt,
  // and executes CompactMessages against the context budget.
  func (p *Pool) assembleSessionMessages(ctx context.Context, sessionID, prompt, sysPrompt string) ([]llm.Message, error)

  // recordSessionTurn atomically records the successful user prompt and assistant reply into the session store.
  func (p *Pool) recordSessionTurn(ctx context.Context, sessionID, prompt, reply string) error
  ```

**Subagent Prompt Scaffold:**
> "Implement Task 4 of 6: Session-aware execution and history recording in `worker` package.
> 1. Anti-bloat rule: DO NOT add heavy logic to `worker/pool.go` (it is 385 lines!). Create `worker/session.go` to handle session loading, prompt message construction, context compaction, and recording.
> 2. In `worker/pool.go`: update `NewPool(concurrency int, ...)` to use dynamic concurrency. Add `SetSessionStore(ss store.SessionStore)`.
> 3. In `worker/pool.go:process(id string)`: if task has `SessionID != ""` and session store is configured, call `assembleSessionMessages` and feed into `agent.RunWithOptions`. On successful completion, call `recordSessionTurn`.
> 4. In `main.go`: pass `conf.WorkerConcurrency` to `NewPool`, call `p.SetSessionStore(s.(store.SessionStore))`.
> 5. In `worker/session_test.go`: test end-to-end multi-round execution where task 2 receives context from task 1.
> 6. Verify line budget: `worker/pool.go` <= 400 lines! `worker/session.go` <= 250 lines!
> 7. Run `GOWORK=off go test -race ./worker/... -count=1` and architecture gate."

**Step Breakdown:**
- [ ] Step 1: Write failing tests in `worker/session_test.go` (RED).
- [ ] Step 2: Implement `worker/session.go` and adapt `worker/pool.go` (GREEN).
- [ ] Step 3: Verify line count `wc -l worker/pool.go worker/session.go` (must both be <= 400 lines).
- [ ] Step 4: Run full `-race` suite and commit with message `feat(worker): integrate session-aware context loading and history recording`.

---

### Task 5: API Layer Session Endpoints & Multi-Round Task Submission (`api/session_handler.go`) [Tier: 2-Standard] [Mode: AFK] [Role: TDD Builder]

**Files:**
- Create: `api/session_handler.go` (separate file to keep `api/handler.go` <= 400 lines)
- Modify: `api/handler.go` (register `/sessions/{id}`, `/sessions/{id}/messages`, accept `session_id` in `HandleRun`)
- Create: `api/session_test.go` (REST session query and multi-round submission tests)

**Interfaces:**
- Consumes: `store.SessionStore`, `store.TaskStore`
- Produces:
  - `POST /run`: accepts `{"prompt": "...", "session_id": "optional_id"}`
  - `GET /sessions/{id}`: returns `Session` JSON
  - `GET /sessions/{id}/messages`: returns `[]SessionMessage` JSON

**Subagent Prompt Scaffold:**
> "Implement Task 5 of 6: API layer session endpoints and `POST /run` session parameter.
> 1. Anti-bloat rule: DO NOT bloat `api/handler.go` (it is 228 lines). Create `api/session_handler.go` for `HandleGetSession` and `HandleGetSessionMessages`.
> 2. In `api/handler.go`: update `HandleRun` payload struct to parse `SessionID string json:\"session_id,omitempty\"`. If non-empty, bind to created task.
> 3. Register routes: `GET /sessions/{id}` and `GET /sessions/{id}/messages` wrapped with standard middleware.
> 4. In `api/session_test.go`: test creating tasks under a session, fetching session metadata, and fetching ordered session messages.
> 5. Verify line budget: `api/handler.go` <= 400 lines, `api/session_handler.go` <= 200 lines.
> 6. Run `GOWORK=off go test -race ./api/... -count=1` and architecture gate."

**Step Breakdown:**
- [ ] Step 1: Write failing tests in `api/session_test.go` (RED).
- [ ] Step 2: Implement `api/session_handler.go` and update `api/handler.go` (GREEN).
- [ ] Step 3: Run mutation probe and verify `-race` tests.
- [ ] Step 4: Verify architecture gate and commit with message `feat(api): add session REST endpoints and session_id binding in HandleRun`.

---

### Batch 6: Architectural Documentation, ADR-0020, Interview Manuals & WORKMEMORY Refresh [Tier: 1-Light] [Mode: AFK]

**Files:**
- Create: `docs/adr/0020-session-state-machine-and-context-compaction.md`
- Modify: `README.md` (document multi-round session API, compaction, concurrency config)
- Modify: `docs/interview/pitch-3min.md`, `interview-answers.md`, `project-qa.md` (add P8 session state machine & compaction Q&A)
- Modify: `.agent-context.md` (update test baseline counts, package responsibility map)
- Modify: `WORKMEMORY/PROJECT_OVERVIEW.md` & `WORKMEMORY/work.log` (append WORK_END note)

**Interfaces:**
- Consumes: Task 1–5 implementations and test receipts
- Produces: 100% physically aligned documentation and interview assets with 0 privacy leaks

**Subagent Prompt Scaffold:**
> "Complete Batch 6: Author ADR-0020, update README.md, interview manuals, .agent-context.md, and WORKMEMORY.
> 1. Author `docs/adr/0020-session-state-machine-and-context-compaction.md` with status Implemented.
> 2. Update `README.md` with session endpoints, config parameters, and updated test counts.
> 3. Update interview pitch and Q&A documents reflecting P8 session state machine and compaction trade-offs.
> 4. Update `.agent-context.md` and `WORKMEMORY/PROJECT_OVERVIEW.md` with new test counts and file inventory.
> 5. Append WORK_END note to `WORKMEMORY/work.log`.
> 6. Run `git grep -n -E 'Obsidian|Haoyu|[CDcd]:[/\\\\]'` to verify 0 privacy leaks."

**Step Breakdown:**
- [ ] Step 1: Write ADR-0020.
- [ ] Step 2: Update README, interview manuals, and context anchors.
- [ ] Step 3: Verify zero privacy path leaks.
- [ ] Step 4: Commit with message `docs(p8): record ADR-0020, update architecture docs, interview assets, and workmemory`.

---

## [NOTE] Deferred Gap Register

| Gap | Why Deferred | Trigger That Forces Revisiting | Owner |
|---|---|---|---|
| **Frontend Demo Multi-Session UI (`api/demo.html`)** | 用户明确要求「前端最后做」，先夯实纯服务端状态机与压缩算法 | 服务端会话接口全部稳定交付后，独立提需求启动 UI 改造 | User / Orchestrator |
| **Model Context Protocol (MCP) Stdio Client** | Option C 规划；外部子进程沙箱与 stdio 协议复杂度高，需先有稳定会话上下文承载工具调用 | P8 会话状态机稳定合入 master 之后，作为 P9 独立里程碑启动 | Orchestrator |
| **LLM 异步递归摘要压缩 (Recursive Summarization)** | 增加额外的异步 LLM 调用会引入额外延迟与 Token 开销，滑动窗口截断已能保证 100% 安全防爆 | 业务场景出现超过 50 轮超长会话且对早期细节有强召回诉求 | Orchestrator |

---

## [NOTE] Deviation Table (post-execution, filled before merge)

| plan text | what was actually built | why |
| --- | --- | --- |
| *(待执行后据实填写；无偏差填「无偏差」)* | | |
