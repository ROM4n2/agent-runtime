# Agent Runtime

异步任务 + Worker Pool + LLM Agent 的 Go 后端服务。客户端提交 prompt，服务异步调用 DeepSeek：
worker 以 **think → call tool → observe** 的多步认知循环执行 agent（内置 `calculate` / `current_time` / `search_docs` 工具），
通过轮询或 SSE 获取结果。支持用户主动取消、掉电崩溃恢复、FTS5+vec0 RRF 混合知识检索、SQLite WAL 事务持久队列削峰与 SSE 原生打字机流式推断。

## Architecture

```text
客户端                     服务器
  │ POST /run (prompt)     │
  │───────────────────────→│ api: store.Create(prompt) → task_id (202)
  │                         │         ▼
  │                         │   Transactional Queue (SQLite / Memory)
  │                         │   Push-Notify (wakeUp beacon)
  │                         │         ▼
  │                         │  [worker 1][worker 2][worker 3]
  │                         │   tasks.ClaimNext (atomic pending→running)
  │                         │         ▼
  │                         │   runAgent: ChatWithToolsStream ⇄ 工具(calc/time/search_docs)
  │                         │         │      ▼ onDelta
  │                         │         │   TraceBuffer.AppendDelta
  │                         │         ▼
  │                         │   store.Complete → done / failed
  │ GET /tasks/{id} 轮询    │
  │───────────────────────→│ api: store.Get(id) → Task JSON (200)
  │ GET /tasks/{id}/events  │
  │───────────────────────→│ SSE: event: delta (打字机) + step + done
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

`config.yaml` 已被 `.gitignore` 忽略。只支持扁平的 `key: value`，解析到嵌套或未知 key 会直接启动失败，不静默忽略。已支持 10 项配置：`deepseek_api_key`、`api_auth_key`、`model`、`db_path`、`task_timeout_seconds`、`embed_base_url`、`embed_api_key`、`embed_model`、`embed_dim`、`max_queue_size`：

```yaml
deepseek_api_key: "sk-你的key"
api_auth_key: "你的服务端密钥"
model: "deepseek-chat"   # 可选：默认 deepseek-chat，环境变量 MODEL 优先级更高
db_path: ""   # 可选：填 SQLite 文件路径（如 agent-runtime.db）即启用持久化，留空用内存存储
task_timeout_seconds: 30   # 可选：单次任务 LLM 调用超时（正整数，0=默认 30s）
max_queue_size: 10000   # 可选：最大允许排队积压的任务数（超出返回 503），默认 10000
# embed_* 四项为检索功能的向量通道配置（ADR-0017），全部留空 = 纯词法检索（lexical）
# 本地 Ollama 等免密向量引擎无需鉴权时，embed_api_key 可以留空
embed_base_url: "http://localhost:11434/v1"   # OpenAI 兼容 embeddings 端点
embed_api_key: ""                             # 远端引擎需鉴权时填写，本地 Ollama 留空
embed_model: "nomic-embed-text:latest"
embed_dim: 768                               # 向量维度，0=不建向量索引
```

没有配置文件也没设环境变量时，服务会打印错误并以退出码 1 退出。

### CLI 子命令（运维与离线对照）

服务支持两条短路退出的 CLI 子命令，不占用主服务监听端口：

```powershell
# 1. 离线构建/全量重建检索索引（对指定语料目录切分分块并建库）
go run . -index docs

# 2. 自动化对照评测 Harness（ADR-0018：成对受控，单命令跑完 30 题 × 3 次重复并落盘 Markdown/JSON 报表）
# 词法基线组（FTS5 词法召回）：
go run . -eval -group lexical -repeat 3 -template eval-template-v2

# 混合召回组（本地 Ollama 768 维向量 + FTS5 BM25 + RRF 融合倒数排序）：
$env:EMBED_BASE_URL="http://localhost:11434/v1"; $env:EMBED_MODEL="nomic-embed-text:latest"; $env:EMBED_DIM="768"
go run . -eval -group hybrid -repeat 3 -template eval-template-v2
```

### Docker

```powershell
docker build -t agent-runtime .
docker run -e DEEPSEEK_API_KEY=sk-你的key -e API_AUTH_KEY=你的服务端密钥 -p 8080:8080 agent-runtime
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
| GET | `/tasks/{id}/events` | SSE 实时流式推送 | - | `text/event-stream`：`event: delta` (打字机增量) / `event: step` / `event: done` |
| GET | `/healthz` | 存活探针 | - | `200 ok`（纯文本） |
| GET | `/metrics` | Prometheus 指标 | - | `agent_tasks_*` 文本格式 |
| GET | `/` | 单页 Demo | - | `text/html` 页面 |

Status 取值：`pending` → `running` → `done` / `failed`

并发上限 3 个任务同时执行，超出部分排队（基于 SQLite WAL 事务队列削峰，默认支撑 10,000+ 任务排队）。

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
GOWORK=off go test -race ./... -count=1  # 278 个测试用例 / 442 个测试分支全绿，零 race
go test -bench=. -benchmem ./worker/     # 基准测试
```

- **单测体系**：涵盖 12 个分层包，覆盖 store 状态机、worker 并发与取消树、agent 认知循环、tool 注册表与 AST 算术求解器、retrieval 词法/向量/RRF 融合召回、eval 自动化判题器。
- **静态架构与体积门禁**（`architecture_test.go`）：
  - **AST DAG 白名单**：用 `go/parser` 扫描全包 import 关系，严格锁死单向分层，杜绝跨层反向依赖（如 worker 严禁 import retrieval，api 严禁 import worker）；
  - **代码体积预算（Line Budget）**：强制生产 `.go` 文件 ≤ 400 行、测试文件 ≤ 800 行，多版本迭代防腐化。
- **集成测试**：`api/integration_test.go` 用 `httptest` 端到端验证 `POST /run → 轮询 → done` 与 SSE 流式事件。
- **Benchmark**：
  - `BenchmarkPoolEnqueue` ≈ 1.5 µs/op（3 worker 调度吞吐）
  - `BenchmarkRunAgent` ≈ 84 ns/op（单步 agent 循环开销）

## 检索实证对照（Evaluation Benchmark）

依据 ADR-0018 成对受控设计，使用 30 题独立评测集（包含语料内直答 `in-corpus`、跨文档综合 `cross-chunk` 与语料外拒答 `out-of-corpus`）在 DeepSeek-V3 与 本地 Ollama `nomic-embed-text:latest`（768 维）下执行 N=3 重复实跑（每组 90 轮）：

| 对照组别 | 召回通道 | 任务成功率 (中位数 [P25, P75]) | 引用命中率 (中位数) | 平均步数 | P50 延迟 (ms) | 真实 Token 成本 (p+c) |
|---|---|---|---|---|---|---|
| **Baseline** | 无检索（仅计算/时间） | 0.00 [0.00, 0.00] (0/90) | 0.00 | 2.00 | 4551.5 | 0 + 0 (not-wired) |
| **Lexical** | FTS5 trigram BM25 | 1.00 [0.00, 1.00] (61/90) | 1.00 | 12.72 | 6412.5 | 1042K + 72K (llm-usage) |
| **Hybrid** | FTS5 + vec0 (768维) + RRF | **1.00 [0.25, 1.00] (67/90)** | 1.00 | **11.84** | 6601.0 | **987K + 71K (-55K tokens)** |

> **实证结论**：混合检索使成功率绝对值提升 6.66%（P25 跃升至 0.25），跨文档综合题（如取消与重试联动、并发池与状态机解耦）成功率由 33.3% 跃升至 100%；同时因语义命中更精准，Agent 平均认知步数减少 0.88 步，单轮评测节省 54,608 个 Prompt Context Tokens，端到端延迟仅微增 188ms。

## Project Structure

```
agent-runtime/
├── main.go               # 守护进程入口：配置加载 → 依赖组装 → 优雅停机
├── cli.go                # 运维 CLI：-index 索引构建、-eval 对照评测子命令
├── architecture_test.go  # 根级架构守卫：AST DAG 白名单 + 400 行体积预算门禁
├── api/                  # HTTP 传输层（依赖 agent、metrics、store）
│   ├── handler.go        # REST CRUD：/run、/tasks/{id}、/healthz、/metrics
│   ├── events.go         # SSE 实时事件流与写超时管理
│   ├── middleware.go     # Recover / RequestID / 令牌桶限流 / Bearer 鉴权
│   ├── demo.go / demo.html # go:embed 零依赖离线单页 Demo
├── agent/                # 认知内核（ReAct 循环，支持 context 抢占与 OnUsage 累加）
│   ├── engine.go         # Run / RunWithOptions 多步决策循环
│   ├── trace.go          # 有界 TraceBuffer 环形缓冲与 512B 安全截断
│   └── tool/             # 独立工具注册表与契约
│       ├── tool.go       # Tool、RetryableError、TerminalClassifier 解耦接口
│       ├── registry.go   # 工具注册表（Errors-As-Messages）
│       ├── calculate.go  # AST 递归下降算术求值器（防代码注入）
│       └── time.go       # IANA 时区解析时间工具
├── worker/               # 任务调度执行层（Worker Pool，纯化并发池）
│   └── pool.go           # Push-Notify 事务性抢占并发池、两阶段停机、排队排空、取消树管理
├── retrieval/            # 混合检索即工具（独立领域包，零反向依赖）
│   ├── index.go          # 索引生命周期：Open、Close、CheckFresh、Rebuild
│   ├── schema.go         # SQLite DDL、FTS5 表结构与版本指纹
│   ├── search.go         # FTS5 BM25 词法召回与 RRF 融合排序
│   ├── vector_search.go  # vec0 向量 KNN 检索与距离加权
│   ├── embed.go          # 向量嵌入客户端适配（支持本地 keyless Ollama）
│   └── tool.go           # search_docs 注册工具（Span 引用契约与终态分类）
├── eval/                 # 检索评测 Harness（离线受控实验与数据对账）
│   ├── run.go            # 评测协调器（成对受控参数锁定、索引预检）
│   ├── collector.go      # 运行时无截断全文证据收集器
│   ├── grader.go         # 程序化 Span 覆盖率判定器（禁 LLM-as-Judge）
│   ├── aggregate.go      # 分布指标计算器（中位数、P25、P75）
│   └── report.go         # 结构化 Markdown / JSON 报表渲染与落盘
├── prompts/              # 版本化系统提示词（go:embed 内嵌）
│   ├── prompts.go        # Get(version) 模板管理器
│   ├── default-v1.md     # 生产服务默认提示词
│   ├── eval-template-v1.md # 历史对照空提示词
│   └── eval-template-v2.md # Grounded Citation 铁律与反幻觉拒答规范
├── store/                # 任务状态存储（TaskStore 接口，消除数据竞争）
│   ├── store.go          # 内存原子存储（默认）
│   └── sql.go            # modernc SQLite WAL 持久化、原子 CAS 抢占与崩溃补偿
├── llm/                  # 模型适配层（OpenAI 兼容协议封装）
│   ├── client.go         # ChatWithTools、幂等 joinEndpoint、Usage 解析
│   └── stream.go         # ChatWithToolsStream 原生 SSE 流式解析与工具分块组装
├── metrics/              # 监控度量（sync/atomic 零依赖 Prometheus 格式）
├── config/               # 扁平配置解析器（环境变量 > config.yaml > 默认值）
├── docs/                 # ADR 决策记录、设计规格、实施计划与实测报表
└── Dockerfile            # 生产多阶段镜像构建
```

## Tech Stack

Go 1.24+ · modernc.org/sqlite (纯 Go 无 CGO) · slog · Docker · DeepSeek API · 本地 Ollama (nomic-embed-text) · 以标准库为主（`net/http` / `embed` / `go/parser` / `httptest` / `sync/atomic`）。

## License

MIT
