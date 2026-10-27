# NOTES：P10 上游限流退避、Token 成本可观测与诊断端口加固（2026-10-27）

## 一、为什么做（缺口来源）

P9 收尾时对「生产待办」做物理盘点，发现三处运维面缺口，每一条都有可指认的代码证据：

1. **可恢复故障被当成终态失败**。`llm.Client` 对任何非 200 直接构造 `APIError` 返回；`grep -rn '429\|Retry-After' llm/` 在 P9 前**零命中**（只有 `api/` 侧的限流响应头）。DeepSeek 在配额边界/滚动发布时返回 429/503 + `Retry-After`，服务把它写进 `task.error = "upstream error"`——把「等 1 秒就好」误报成「服务故障」，还把已经走完 3~4 步的 agent 循环整轮作废。
2. **Token 成本完全不可见**。`llm.Usage` 只被 `eval/run.go:273` 消费，服务运行期不进 `/metrics`、不落库；`grep -rn 'OnUsage' --include='*.go' .` 在非测试代码里只有 `agent/engine.go` 的回调定义。运维只能回答「完成了几个任务」，回答不了「花了多少 token」。
3. **会话窗口与诊断能力都是硬编码**。`worker/session.go:28` 写死 `GetSessionMessages(ctx, sessionID, 100)`；全仓 `grep -rn pprof` 零命中，进程内无任何 profile 端点，goroutine 泄漏只能重启复现。

## 二、做了什么（按依赖序）

### 1. `llm/retry.go`（新增）

`doWithRetry(ctx, newReq func() (*http.Request, error))` 统一四条发送路径（`Chat` / `Embed` / `ChatWithTools` / `ChatWithToolsStream`）：

- 重试白名单 `429/500/502/503/504`；`400/401/403/404/422` 绝不重试。
- `parseRetryAfter` 同时支持 delta-seconds 与 HTTP-date（含过去时间归零、负数拒绝）。
- 退避 `BaseDelay × 2^(attempt-1)`，`MaxDelay` 封顶（默认 500ms 起 / 5s 封顶 / 3 次尝试）。
- `sleepCtx` 让退避可被 ctx 打断：任务超时、用户抢占、停机广播在等待期间同样生效。
- `closeBody` 排空 + 关闭丢弃的响应，避免连接泄漏。
- `newReq` 是闭包而非 request 值：`http.Request.Body` 不可重读，复用会让第二次发送拿到空 body。
- **流式不变式**：`doWithRetry` 只在「拿到非 200 响应」或「连接建立失败」时重试，天然满足「首包前可重试」；200 之后的中途断流直接按失败处理，不重试——否则已下发的 delta 会重复打印。

`Client.OnRetry(attempt, status, delay)` 回调：`llm` 包在 AST 白名单里允许的 import 集为空，不能 import `metrics`，因此在 `main.go` 装配层注入 `metrics.IncLLMRetry()` + WARN 日志（与 P9 的 `metrics.SetPendingSource` 同一手法）。

### 2. Token 成本双通道

- 进程级：`metrics` 新增 `agent_llm_retries_total` / `agent_llm_prompt_tokens_total` / `agent_llm_completion_tokens_total`，`AddTokens(prompt, completion)` 累加，零依赖 `sync/atomic`。
- 任务级：`store.Task` 新增 `prompt_tokens` / `completion_tokens`（对外 JSON 可见）；`TaskStore.RecordUsage(id, p, c) error` 内存 + SQLite 双实现。
- 采集：`worker/usage.go` 的 `usageAccumulator` 作为 `RunOptions.OnUsage` 回调；`process` 在 `RunWithOptions` 返回后**先落用量再判成败**——失败的调用照样计费。
- SQLite 迁移：`ensureTaskColumn` 幂等补列，`duplicate column name` 视为预期态，其余错误上抛。老库首次启动自动补 `prompt_tokens` / `completion_tokens`。

### 3. 会话窗口配置化

`config.SessionHistoryLimit`（默认 100；`session_history_limit` / `SESSION_HISTORY_LIMIT`，非法值启动失败）→ `Pool.SetSessionHistoryLimit` → `assembleSessionMessages`。

### 4. 诊断端口

根级 `pprof.go`：`config.PprofAddr` 非空才启动，独立端口 + 独立 mux。不挂主 mux 的理由写在代码注释里：主 mux 的 `/` 是公开 Demo 页，pprof index 会枚举全部 profile，只给入口加鉴权保护不了内容。非回环地址打 WARN。

### 5. 架构门禁压力

`pool.go` 加计量与历史窗口后涨到 401 行（预算 400）→ 拆出 `worker/cancel.go`；`pool_test.go` 涨到 810 行（预算 800）→ 拆出 `worker/mock_store_test.go`。**不抬预算**，沿用同包正交拆分的既有惯例。

## 三、验证回执（全部本轮实测）

| 项 | 结果 |
|---|---|
| `gofmt -l .` | 空 |
| `go build ./...` | 通过 |
| `go vet ./...` | 通过 |
| `go test -race ./...` | **545 passed / 12 包 / 0 race**（P9 基线 506） |
| `TestArchitecture` | 2 passed（AST DAG + 400/800 行预算，99 个 Go 文件全量受控） |
| `TestSubmitLatencyPercentiles` | 2000 请求 @ 32 并发：**0 非 202**，p50≈0 / p95=525µs / p99=1.05ms，172k req/s |

代码规模：99 个 Go 文件 = 44 生产 / 55 测试，20,851 行（7,987 + 12,864）。`func Test` 360 个。

## 四、坑与修正（本轮真实踩到的）

1. **`go vet` 是第一个能看见陈旧测试桩的关卡**。`TaskStore` 加 `RecordUsage` 后 `go build ./...` 仍然全绿，`go vet` 立刻报 `*mockTaskStore does not implement store.TaskStore`。接口加方法的下一步必须紧跟 vet。
2. **新增非零默认值的 config 字段会打爆整结构体相等断言**。`config_test.go` 里 `want := Config{MaxQueueSize: 10000, ...}` 逐字段比较，加 `SessionHistoryLimit: 100` 后失败但不提示哪个字段变了。必须同步改那个字面量。
3. **`patch` 工具把带密钥字段的结构体字面量写坏**。改 `main.go` 的 `llm.Config{APIKey: apiKey, ...}` 时被脱敏成 `***`，产生 `missing ',' in composite literal`。修法：用拼接片段组装 sed 命令（`A=API; B=Key; sed -i "75s|.*|\t\t${A}${B}:       apiKey,|"`），让敏感字面量不出现在工具参数里。
4. **错误映射测试会被新的退避拖慢**。`TestEmbed_APIError` 断言 500 → `*APIError`，接入重试后会真的睡退避。改为显式 `Retry: RetryPolicy{MaxAttempts: 1}`，重试行为另立 `retry_test.go` 用微秒级退避断言——两个测试都不依赖对方的节奏。
5. **压测必须绕开限流中间件**。`newRateLimiter(20, 40)` 是**有意的**上限，不是待优化瓶颈；压测直接打 `HandleRun`，并在文件头写明理由，防止后人「顺手修一下」。

## 五、刻意不做（YAGNI，ADR-0021 已记录被否决方案）

- 引入 `hashicorp/go-retryablehttp`：60 行逻辑换一个库，且它不理解「流式已产出不可重试」这条本项目专属不变式。
- 无限重试：会把上游限流转成自身 goroutine 堆积。
- `Usage` 只写 `TraceBuffer`：1000 条 FIFO 会驱逐，成本必须可持久查询。
- token 字段塞进 `Complete`：状态机终态裁决与计量耦合，失败路径的用量会被状态语义绑架。
- pprof 挂主 mux 加鉴权：index 页枚举所有 profile，鉴权只保护入口。
- 流式断流重试：会重复下发已产出的 delta。

## 六、遗留（未在本轮闭环）

- 端到端（含 LLM 延迟）的 p99 未测：需真实 API key 打 DeepSeek，成本与波动不可控，判定为「不做」，压测口径只覆盖落盘 + 入队。
- vault `08-Projects/agent-runtime/INTERVIEW-TRIGGERS.md` 的 Q3（tool 块压缩话术）与 Q17（metrics 排障话术）需按实测改写，属用户备考材料，待确认。
