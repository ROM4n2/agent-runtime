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

- P1 已交付（可观测性 + Demo + 测试）；HEAD=`406254d`，bootstrap 前工作树 clean。
- 测试基线：`go test ./... -count=1` 全绿（40 个 `func Test`，2026-10-11 复测 exit 0）。
- 最新三项能力（ADR-0010/0011）：SQLite 可插拔持久化（`TaskStore` 接口，内存/SQLite 双实现）、
  任务超时可配置（`task_timeout_seconds`，默认 30s，事故期止血降到 8s）、优雅停机取消树 + `Enqueue` 快速失败。
- 待办 / 已知限制：队列本身不持久（重启丢已入队但未开始的任务，ADR-0004）；轮询有空转请求（ADR-0005，当前体量无影响）；
  `Enqueue` 满时阻塞（缓冲=worker 数），高负载下异步退化为同步（ADR-0004/0007）——改造需先立 ADR。
- 面试向资产在 `docs/interview/`；规范与踩坑沉淀走 vault（`{vault_root}/AGENTS.md` / `scripts/search-vault.py` / `99-Inbox/`）。

## 高频坑（踩过的雷区）

<!-- 每条：现象 → 根因 → 解法 -->

- **探活/指标被限流 429** → 路由混走全链中间件 → 运维路由（`/healthz` `/metrics` `/`）只挂 `Recover→RequestID`，业务路由才走 Limit→Auth。
- **`Task.Error` 泄漏上游响应体** → `Task` 被原样序列化返回调用方 → 失败只存粗粒度分类（`"upstream error"`），错误全文只进 slog，绝不放 `Task`。
- **goroutine 向已关闭 channel 发送 panic** → `Stop` 里 `close(queue)` → 改用 `cancel()`；`Enqueue` 满时阻塞是已知取舍。
- **改 hook 后 git 报 `cannot spawn .githooks/pre-commit`** → 编辑器重加 UTF-8 BOM 弄坏 shebang → `tail -c +4 .githooks/pre-commit > tmp && mv tmp` 去 BOM（安装脚本亦强制无 BOM UTF-8）。
- **`config.yaml` 改了不生效 / 被静默忽略** → 优先级与解析范围误解 → 环境变量 > `config.yaml`；只接受扁平 `key: value`，嵌套或未知 key 直接启动失败（不静默）；文件 gitignored，示例是 `config.yaml.example`。
- **并发写 map 崩溃** → 直接读写 `s.tasks` → 一律走 `Create/Update/Complete/Get` 方法（整段持锁，`Get` 返回副本）；终态只由 `Store.Complete` 裁决。
