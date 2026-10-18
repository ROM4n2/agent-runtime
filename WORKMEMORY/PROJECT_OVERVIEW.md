# PROJECT_OVERVIEW — agent-api

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
- 测试基线：`GOWORK=off go test -race ./... -count=1` 全绿（82 个 `func Test`，0 数据竞争）。
- 架构领域分层：
  - `agent/tool`: 独立工具系统与 Tool 注册表（内置纯 AST 算术引擎与时区工具，Errors-As-Messages 规范）；
  - `agent`: 纯粹认知内核（ReAct `Run` 循环、`Chatter` 接口、`TraceStep` 步骤与有界环形缓冲 `TraceBuffer`）；
  - `api`: 纯粹 HTTP 适配器与装配层，依赖倒置使用 `TaskRunner` 消费者接口，有自动化架构防线静态门禁阻断反向依赖；
  - `worker`: 纯粹 Job Concurrency Executor，两阶段优雅停机 + 队列排空 + 细化异常终态归因。
- 生产加固能力：
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
