# PROJECT_OVERVIEW — agent-runtime

> 60 秒项目 primer。新 agent 的第二读（读完 work.log 事件流之前先建立全局认知）。
> 维护规则：bootstrap 时由 agent 从代码库提炼生成初稿；此后每次 WORK_END / 交接后刷新。
> 初稿生成：2026-10-11 21:58（bootstrap 蒸馏）。

## 这是什么项目

异步任务 + Worker Pool + LLM Agent 的 Go 后端服务（面试/AI Agent 项目 Phase 2-3 产物，ADR-0001）。
客户端 `POST /run` 提交 prompt → 服务落库入队（`202 {"task_id"}`）→ 3 个 worker 以
**think → call tool → observe** 多步循环调用 DeepSeek（内置 `calculate` / `current_time` 工具，
`maxAgentSteps=5`）→ 客户端 `GET /tasks/{id}` 轮询取结果。
技术栈：Go 1.26.5 · 标准库为主（`net/http` / `embed` / `httptest` / `sync/atomic`）· slog。
**默认零第三方依赖**；唯一例外是 opt-in 持久化 `modernc.org/sqlite`（纯 Go 无 CGO），仅当 `db_path` 非空才加载。
运行：`go run .`（需 `DEEPSEEK_API_KEY`，或 `config.yaml`），监听 `:8080`；`docker build -t agent-api .` 亦可。

## 当前状态

- P1/P2/P3/P4 完整交付（ADR-0012 轨迹可观测性、ADR-0013 架构解耦、P4 生产级审计缺陷加固全部落地）。
- P5 检索即工具随 PR #1 合入 `master`（b5f8b97，2026-10-23）。
- **P6 生产级对齐与混合检索实证完整落地（2026-10-25）**：
  1. **工业级契约闭环**：`llm.joinEndpoint` 幂等防 `/v1/v1` 404；允许空 `EmbedAPIKey` 支持本地 Ollama 免密嵌入；`ChatWithTools` 透传 `Temperature=0` 与 `Seed`；解析 `llm.Usage` 并在 `agent.RunWithOptions` 支持多步 Token 累加；`config.Model` 9 项配置支持。
  2. **结构熵增治理三件套**：在 `agent/tool` 下沉 `TerminalClassifier` 接口，`retrieval.ErrIndexMissing` 实现协议，`worker/pool.go` 面向接口做 `errors.As` 提取终态，彻底移除 `worker → retrieval` 反向依赖；在不引入外部子包的前提下同包正交拆分 5 个巨型文件（`main.go → cli.go`、`api/handler.go → api/events.go`、`retrieval/search.go → retrieval/vector_search.go`、`retrieval/index.go → retrieval/schema.go`、`eval/run.go → eval/collector.go`）；新增根级 `architecture_test.go`，用 `go/parser` 扫描全仓建立 12 包 AST DAG 白名单与生产 ≤ 400 行 / 测试 ≤ 800 行代码体积预算机械闸。
  3. **版本化 Prompt 与实证评测闭环**：创建 `prompts/` 包（`go:embed` 内嵌 `eval-template-v1`、`eval-template-v2`、`default-v1`），在本地 Ollama `nomic-embed-text:latest`（768 维）与 DeepSeek-V3 下完成 30 题 × 3 重复（每组 90 轮）实跑。实证验证混合检索将跨文档综合题（`cross-chunk`）成功率提升至 100%，平均步数下降至 11.84 步，单轮评测节省 54,608 个 Prompt Context Tokens，端到端延迟仅微增 188ms。
- **评测结论（docs/eval/2026-10-25-*-r3.md）**：
  - Baseline：0.00 [0.00, 0.00]（0/90）；
  - Lexical（v2 提示词）：1.00 [0.00, 1.00]（61/90，none×61, wrong-span×24, right-span-wrong-reasoning×5，总 Token 1042K+72K）；
  - Hybrid（v2 提示词 + nomic-embed-text 768维）：1.00 [0.25, 1.00]（67/90，none×67, wrong-span×20, right-span-wrong-reasoning×3，总 Token 987K+71K）→ **合入**。
- 测试基线：`GOWORK=off go test -race ./... -count=1` 12 包全绿（260 个 `func Test` / 422 RUN，0 数据竞争）。代码规模 71 个 Go 文件（33 生产 / 38 测试，共 15,319 行）。
- 架构领域分层：
  - `prompts`: 版本化系统提示词管理（`go:embed`，提供 `Get`，内嵌反幻觉与 Grounded Citation 铁律）；
  - `agent/tool`: 独立工具系统与 Tool 注册表（内置纯 AST 算术引擎与时区工具，提供 `TerminalClassifier` 协议下沉接口）；
  - `agent`: 纯粹认知内核（ReAct `Run`/`RunWithOptions` 循环、`Chatter` 接口、`TraceBuffer` 环形缓冲）；
  - `api`: 纯粹 HTTP 适配器与装配层，REST CRUD 与 SSE 流式事件按文件正交拆分，有架构防线静态门禁阻断反向依赖；
  - `worker`: 纯粹 Job Concurrency Executor，零 retrieval 依赖，通过 `TerminalClassifier` 提取终态；
  - `retrieval`: 独立顶层检索包，FTS5 词法 + vec0 KNN + RRF 融合，生命周期/DDL/词法/向量正交切分；
  - `eval`: 检索评测 harness（ADR-0018），成对受控实验参数锁定与 API 真实 Token 统计。
- 生产加固能力：
  - **主动抢占取消**：`POST /tasks/{id}/cancel` + `Pool.inFlight` 句柄表 + `context.WithCancelCause`
    物理中断上游 HTTP 调用；未在飞任务返回 409；取消原因脱敏后落 `task.error`（ADR-0014）。
  - **掉电崩溃恢复**：`TaskStore.RecoverTasks` 启动扫描，pending 按 ID 升序阻塞重灌、
    僵尸 running 标记 `server restarted unexpectedly`；SQL 实现全程事务（ADR-0015）。
  - **SSE 实时步进**：`GET /tasks/{id}/events`，`TraceBuffer` 快照+增量非阻塞广播，
    终态推送 `event: done`，与轮询接口并存；清除 ResponseController 写截止时间（ADR-0016）。
  - Trace 缓存容量淘汰（1000 上限 FIFO 驱逐，消除 OOM 泄漏）；UTF-8 Rune 安全边界截断；
  - 两阶段优雅停机：HTTP 关闭后将 30s 超时预算传递给在飞 Worker，退出时排空队列积压任务标记 `server shutdown`；
  - 队列满 503 时显式下发 `Retry-After: 1` 响应头；
  - SQLite 自增主键消除全表扫描：`INTEGER PRIMARY KEY AUTOINCREMENT` + `LastInsertId()`，写吞吐提速至微秒级；
  - 支持 `TASK_TIMEOUT_SECONDS` 环境变量覆盖配置，传输层 HTTPTimeout 与全局任务超时解耦。

## 高频坑（踩过的雷区）

<!-- 每条：现象 → 根因 → 解法 -->

- **探活/指标被限流 429** → 路由混走全链中间件 → 运维路由（`/healthz` `/metrics` `/`）只挂 `Recover→RequestID`，业务路由才走 Limit→Auth。
- **`Task.Error` 泄漏上游响应体** → `Task` 被原样序列化返回调用方 → 失败只存粗粒度分类（`"task timeout" / "server shutdown" / "max steps exceeded" / "upstream error"`），错误全文只进 slog，绝不放 `Task`。
- **停机时排队任务卡死在 `pending` / 在飞任务瞬间被掐** → 单一 cancel 粗暴扼杀且 Go select 伪随机抛弃积压任务 → 两阶段停机：先关入队、给在飞任务留 30s 停机预算，停机退出前排空 Channel 积压任务标定 `server shutdown` 终态。
- **`slog.Error` 误传 Printf 格式化占位符导致日志索引字段爆炸** → slog 把动态 id 当作 key、err 当作 value → 统一使用 `slog.Error("msg", "task_id", id, "error", err)` 标准键值对。
- **SQLite 高并发写吞吐严重劣化（<480 QPS）** → `SELECT MAX(CAST(id AS INTEGER))` 触发全表扫描且包裹在排他写锁中 → DDL 迁移为 `INTEGER PRIMARY KEY AUTOINCREMENT`，利用 `LastInsertId()` 实现 O(1) 无表扫描插入。
- **Trace 步骤截断出现 `\ufffd` 乱码** → 原始字节切片切在 3 字节汉字中间 → 使用 `unicode/utf8.RuneStart` 向前回退至合法字符边界。
- **改 hook 后 git 报 `cannot spawn .githooks/pre-commit`** → 编辑器重加 UTF-8 BOM 弄坏 shebang → `tail -c +4 .githooks/pre-commit > tmp && mv tmp` 去 BOM（安装脚本亦强制无 BOM UTF-8）。
- **`config.yaml` 改了不生效 / 被静默忽略** → 优先级与解析范围误解 → 环境变量 > `config.yaml`；只接受扁平 `key: value`，嵌套或未知 key 直接启动失败（不静默）；文件 gitignored，示例是 `config.yaml.example`。
- **并发写 map 崩溃** → 直接读写 `s.tasks` → 一律走 `Create/Update/Complete/Get` 方法（整段持锁，`Get` 返回副本）；终态只由 `Store.Complete` 裁决。
- **SSE 连接 30s 被服务器掐断** → `http.Server.WriteTimeout` 同样作用于流式响应 →
  handler 内 `http.NewResponseController(w).SetWriteDeadline(time.Time{})` 清除写截止时间（不支持时忽略）。
- **订阅者慢导致任务执行被拖死** → 持锁同步投递到无缓冲通道 → `TraceBuffer.Append` 对订阅通道
  `select`+`default` 非阻塞投递，慢消费者只丢自己的增量，终态再用快照对账补齐。
- **崩溃恢复重灌 pending 时再次丢任务** → 队列容量只有 worker 数，直接 `Enqueue` 会满即失败 →
  新增 `Pool.EnqueueWait(ctx,id)` 阻塞等待空位，停机/ctx 取消时中止且任务保持 pending 待下次恢复。
