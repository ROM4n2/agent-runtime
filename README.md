# Agent API

异步任务 + Worker Pool + LLM Agent 的 Go 后端服务。客户端提交 prompt，服务异步调用 DeepSeek：
worker 以 **think → call tool → observe** 的多步循环执行 agent（内置 `calculate` / `current_time` 工具），
通过轮询或 SSE 获取结果。支持用户主动取消与掉电崩溃恢复。

## Architecture

```text
客户端                     服务器
  │ POST /run (prompt)     │
  │───────────────────────→│ api: store.Create(prompt) → task_id (202)
  │                         │         ▼
  │                         │   queue(chan string)
  │                         │         ▼
  │                         │  [worker 1][worker 2][worker 3]
  │                         │   store.Update(running)
  │                         │         ▼
  │                         │   runAgent: llm.ChatWithTools ⇄ 执行工具(calc/time)
  │                         │         ▼
  │                         │   store.Complete → done / failed
  │ GET /tasks/{id} 轮询    │
  │───────────────────────→│ api: store.Get(id) → Task JSON (200)
  │ GET /tasks/{id}/events  │
  │───────────────────────→│ SSE: agent.TraceBuffer 实时步进 + done
  │ POST /tasks/{id}/cancel │
  │───────────────────────→│ api: Pool.Cancel → context 物理中断上游
```

> 中间件链：`Recover → RequestID → 限流(20/s, 突发40) → 鉴权(Bearer)`。
> 生产必须设 `API_AUTH_KEY` 开启鉴权；不设则开发模式放行。
>
> 路由分两组：业务路由（`POST /run`、`GET /tasks/{id}`、`POST /tasks/{id}/cancel`、
> `GET /tasks/{id}/events`）走完整中间件链；
> 运维/演示路由（`GET /healthz`、`GET /metrics`、`GET /`）只走 `Recover → RequestID`，
> 保证探活与指标抓取不被限流或鉴权挡住。

## Quick Start

### 配置密钥

两种配置方式，**环境变量优先级更高**（会覆盖配置文件）：

```powershell
# 方式一：环境变量
$env:DEEPSEEK_API_KEY = "sk-你的key"
$env:API_AUTH_KEY = "你的服务端密钥"   # 可选，不设则开发模式放行鉴权
go run .
```

```powershell
# 方式二：配置文件（推荐本地开发，配一次即可）
Copy-Item config.yaml.example config.yaml   # 然后填真实值
go run .
```

`config.yaml` 已被 `.gitignore` 忽略。只支持扁平的 `key: value`，解析到嵌套或未知 key 会直接启动失败，不静默忽略。已支持的 key：`deepseek_api_key`、`api_auth_key`、`db_path`（可选，留空则用内存存储）：

```yaml
deepseek_api_key: "sk-你的key"
api_auth_key: "你的服务端密钥"
db_path: ""   # 可选：填 SQLite 文件路径（如 agent-api.db）即启用持久化，留空用内存存储
```

没有配置文件也没设环境变量时，服务会打印错误并以退出码 1 退出。

### Docker

```powershell
docker build -t agent-api .
docker run -e DEEPSEEK_API_KEY=sk-你的key -e API_AUTH_KEY=你的服务端密钥 -p 8080:8080 agent-api
```

### 验证

```powershell
# 提交任务
Invoke-RestMethod -Uri http://localhost:8080/run -Method Post `
  -ContentType 'application/json' `
  -Body ([System.Text.Encoding]::UTF8.GetBytes('{"prompt":"用一句话解释goroutine"}'))

# 查询结果（task_id 换成实际返回的值）
Invoke-RestMethod -Uri http://localhost:8080/tasks/1

# 存活探针
Invoke-RestMethod -Uri http://localhost:8080/healthz

# 指标
Invoke-RestMethod -Uri http://localhost:8080/metrics

# 浏览器打开 http://localhost:8080 使用单页 Demo
```

## API

| Method | Path | 说明 | Request Body | Response |
|--------|------|------|-------------|----------|
| POST | `/run` | 提交任务 | `{"prompt":"..."}` | `202 {"task_id":"1"}` |
| GET | `/tasks/{id}` | 查询状态（轮询） | - | `{"id":"1","status":"done","prompt":"...","result":"...","error":""}` |
| POST | `/tasks/{id}/cancel` | 主动取消在飞任务 | `{"reason":"user_aborted"}`（可选） | `202 {"id":"1","status":"canceling"}`；任务不在飞返回 `409` |
| GET | `/tasks/{id}/events` | SSE 实时步进 | - | `text/event-stream`：`event: step` / `event: done` |
| GET | `/healthz` | 存活探针 | - | `200 ok`（纯文本） |
| GET | `/metrics` | Prometheus 指标 | - | `agent_tasks_*` 文本格式 |
| GET | `/` | 单页 Demo | - | `text/html` 页面 |

Status 取值：`pending` → `running` → `done` / `failed`

并发上限 3 个任务同时执行，超出部分排队等待。

## Observability

`GET /metrics` 以 Prometheus 文本格式暴露进程内计数（`sync/atomic` 实现，零第三方依赖）：

| 指标 | 类型 | 含义 |
|------|------|------|
| `agent_tasks_submitted` | counter | 累计提交任务数 |
| `agent_tasks_running` | gauge | 当前在飞任务数（提交 +1，终态 -1） |
| `agent_tasks_done` | counter | 累计成功完成数 |
| `agent_tasks_failed` | counter | 累计失败数 |
| `agent_tasks_recovered` | counter | 崩溃恢复重灌的 pending 任务数（同时补回 running gauge） |

`GET /healthz` 返回 `200 ok`，仅表示进程存活，不涉及任务状态。

指标只暴露聚合计数，不含任务内容或上游响应体。

## Demo

启动后浏览器打开 http://localhost:8080 即可提交 prompt，页面会轮询展示状态演进
（`pending → running → done`）与最终结果。页面是零依赖单页，通过 `go:embed` 内嵌进二进制，
无需外部文件、可离线访问。

Token 框填一次就会存进浏览器 `localStorage`，下次打开自动回填，不必每次手输。
填的值必须与服务端 `api_auth_key` 一致，否则 `/run` 会返回 401。

## Testing

```powershell
go test ./... -count=1                 # 25 个测试全绿
go test -bench=. -benchmem ./worker/   # 基准测试
```

- **单测**：store 状态机、worker 并发上限、agent 工具调用循环、llm 请求/响应解析、metrics 计数。
- **集成测试**：`api/integration_test.go` 用 `httptest` 端到端跑通 `POST /run → 轮询 → done`。
- **Benchmark**：
  - `BenchmarkPoolEnqueue` ≈ 1.5 µs/op（3 worker 调度吞吐）
  - `BenchmarkRunAgent` ≈ 84 ns/op（单步 agent 循环开销）

## Project Structure

```
agent-api/
├── main.go            # 入口：环境变量 → 组装依赖 → 启动服务
├── api/               # HTTP 层
│   ├── handler.go     # 路由注册：/run、/tasks/{id}、/healthz、/metrics、/
│   ├── middleware.go  # Recover / RequestID / 限流 / 鉴权
│   ├── demo.go        # go:embed 单页 Demo
│   └── demo.html
├── config/            # 配置加载：环境变量 > config.yaml（扁平解析，零依赖）
├── store/             # 任务存储：TaskStore 接口；内存 map + Mutex（默认）或 SQLite 持久化（配 db_path）
├── worker/            # Worker Pool + agent 工具调用循环
├── llm/               # LLM 客户端：封装 DeepSeek API 调用
├── metrics/           # 进程内计数 + Prometheus 文本格式（零依赖）
├── docs/              # 项目文档、ADR、实施计划
└── Dockerfile         # 多阶段构建
```

## Tech Stack

Go 1.26 · slog · Docker · DeepSeek API · 以标准库为主（`net/http` / `embed` / `httptest` / `sync/atomic`）；持久化可选引入 `modernc.org/sqlite`（纯 Go，无 CGO）作为唯一第三方运行时依赖，默认不启用。

## License

MIT
