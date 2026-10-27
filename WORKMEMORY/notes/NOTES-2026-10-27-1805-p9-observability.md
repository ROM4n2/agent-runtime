# NOTES：P9 可观测性与积压查询缺陷加固（2026-10-27）

> 来源：work.log 事件 2026-10-27 18:05（正文超 4KB，按 PROTOCOL §1.4 独立成文）。
> 触发背景：vault `08-Projects/agent-runtime/PREP-MASTER.md` 的面试评估结论——代码已够用，真正该补的是「话术与代码对齐」。

**不做范围**：不加多智能体 SpawnAgent、不做 Overlay 钩子/会话树、不引入 Redis/MQ——均为 ADR-0004 YAGNI 立场的主动取舍，非能力缺口。

## 1. 积压查询全表扫描（真实性能缺陷）

`tasks` 表只有主键索引，`ClaimNext`（每次抢占）与 `PendingCount`（每次 Enqueue + `EnqueueWait` 10ms 轮询）均 `SCAN tasks`。

修复（`store/sql.go`）：

```sql
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status, id)
```

列序 `(status, id)` 同时服务 `WHERE status=?` 与 `ORDER BY id ASC`，使「按 id 升序取最老一条」沿索引游标读取，无需额外排序。

实测（真实库 `EXPLAIN QUERY PLAN`）：

| 查询 | 修复前 | 修复后 |
|---|---|---|
| `ClaimNext` | `SCAN tasks` | `SEARCH tasks USING INDEX idx_tasks_status (status=?)` |
| `PendingCount` | `SCAN tasks` | `SEARCH tasks USING COVERING INDEX idx_tasks_status (status=?)` |

门禁：新增 `store/query_plan_test.go`，用 `EXPLAIN QUERY PLAN` 断言锁死走索引，防后续 DDL 改动静默删索引。TDD 顺序 RED 先证（两测试均以 `SCAN tasks` 失败）→ 加索引 → GREEN。

## 2. `/metrics` 缺积压信号

原先只有 `running`（在飞），无任何 pending 指标，排障时无法区分「消费不足」与「上游卡死」。

修复：`metrics` 包在 `architecture_test.go` AST DAG 白名单里允许的 import 集为**空**，故不能让 metrics 反向 import store；改用 `SetPendingSource` 回调做依赖倒置（`main.go` 装配注入，scrape 时实时读存储）。source 报错时退化为 `-1`（Prometheus 未知约定），且不拖垮整页 scrape。

**DECISION**：用注入回调而非镜像计数器——镜像计数需在 6 处维护增减，必然漂移；scrape-time 读一次存储在 covering index 上代价可忽略。

## 3. `running` gauge 语义错误（既有缺陷，被新增 pending 指标暴露）

`IncSubmitted` 曾同时把 running +1，使其实际等于「已提交未终态」总数。

端到端实测（`worker_concurrency=1`，提交 5 任务）：

| 指标 | 修复前 | 修复后 |
|---|---|---|
| `submitted` | 5 | 5 |
| `running` | **4**（误） | **1** |
| `pending` | 3 | 4 |

修复：新增 `IncClaimed`（抢占成功时 +1，running gauge 唯一入口），与终态 -1 严格成对；`IncRecovered` 不再计入 running（恢复任务回到 pending 排队）。

新增不变式（已写入 README）：`submitted - done - failed = running + pending`。

防膨胀：`worker/pool.go` 因注释涨到 401 行触发 `TestArchitecture_FileLineBudget` 失败（门禁确实在工作），按仓库既有惯例（同 `worker/session.go`）拆分出 `worker/metrics.go`，回到 399 行。

## 4. 变异测试反证（两轮，第二轮才发现测试有洞）

1. 变异「移除抢占侧 +1」→ 捕获（`running=0`）。
2. 变异「精确复现旧缺陷即提交侧 +1」→ **首轮未捕获**。根因：测试直接调 `s.Create` 绕过了 `api` 层 `metrics.IncSubmitted()`，只覆盖了抢占一侧。补上完整提交路径（`IncSubmitted` + `Create` + `Enqueue`）并改用基线差值断言后重做变异 → 捕获（`running 增量 = 4, want 1`）。

**PITFALL**：只测「状态改变的出口」而漏测「计数入口」，变异测试才会揭穿——否则会误以为门禁已生效。

## 5. CI 门禁落地

新增 `.github/workflows/ci.yml`（build / vet / 架构门禁单列一步 / `go test -race` / gofmt）。仓库此前从无 workflow，`architecture_test.go` 从「本地手动跑才生效」升级为「每次 push 强制」。

## 6. gofmt 漂移清理

9 个文件存在旧版注释列表缩进与文件尾空行漂移。经 `git ls-files --eol` 确认 160 文件全 `i/lf w/lf`，**非** CRLF 误报，是真实 gofmt 差异（54 行）；已 `gofmt -w`，行尾未变。

## 验证（物理回执）

- `GOWORK=off go build ./...` / `go vet ./...`：通过。
- `GOWORK=off go test -race ./... -count=1`：**506 passed / 12 包 / 0 数据竞争**（基线 498）。
- `GOWORK=off go test . -run 'TestArchitecture_' -count=1`：2 passed（AST DAG + 体积预算）。
- 端到端：真实服务 + SQLite，实测 `/metrics` 三项自洽、EXPLAIN 走索引 / covering index。

## 产出物与回写

新增：`store/query_plan_test.go`、`metrics/pending_test.go`、`metrics/running_semantics_test.go`、`worker/metrics_semantics_test.go`、`worker/metrics.go`、`.github/workflows/ci.yml`。
修改：`store/sql.go`、`metrics/metrics.go`、`main.go`、`worker/pool.go`、`README.md`、`WORKMEMORY/PROJECT_OVERVIEW.md`。

回写检查：结论已回写 PROJECT_OVERVIEW（P9 条目 + 基线按实测改正 329 func Test / 88 files / 40 prod / 48 test / 19,479 行）与 README；临时产物（探针测试、scratch 下 mtest 服务与 db）未入库。

**本次被纠正**：写文档时凭估算填了派生计数（337 / 86 / 约 19500），下一步实测为（329 / 88 / 19,479）后即改——派生数字必须实测回读，不可估算。
