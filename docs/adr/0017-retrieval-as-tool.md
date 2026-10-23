# ADR-0017：检索能力以工具形式接入（Hybrid Retrieval as a Tool）

- **日期**：2026-10-22（v2 重写：2026-10-22 dfs-grill 双透镜拷打后修订）
- **状态**：已接受（**未实施**）
- **上游依赖**：ADR-0008（工具调用循环）、ADR-0010（SQLite 持久化）、ADR-0013（分层解耦）、ADR-0014（取消语义）、ADR-0004（YAGNI 立场）
- **下游影响**：`tool.Registry` 接口签名、`agent.Run` 循环、新增 `retrieval` 包、`llm` 包扩展；评测口径由 **ADR-0018** 定义
- **上位规范**：vault `01-Rules/LAYERED-ARCHITECTURE.md`（Pi-Agent 三层蒸馏：依赖方向判据）、`01-Rules/AGENT-EVALUATION-HARNESS.md`（裁剪引用）

## 背景

`agent-engine` 目前只有两个确定性工具（`calc`、`time`），能力边界停留在「能跑通工具调用循环」。
真实 Agent 任务里最常见的一步是「先从自己的知识里找到相关片段，再基于片段回答」，也就是检索。
缺这一层会带来三个具体后果：

1. **能力缺口**：任务只能做无状态计算，无法处理「文档里说了什么」这类问题，工具调用的价值被低估。
2. **招聘口径缺口**：目标岗位族（Agentic AI 基础设施 / AIGC 系统核心）明确要求 RAG 与 Prompt 工程能力，
   而当前仓库的检索命中为零（`grep -ri "embedding|retriev|vector|rerank"` 在源码中无实现）。
3. **可验证性缺口**：即使加了检索，没有对照指标就无法回答「加了之后好在哪」——这会让整块工作退化成
   简历上的形容词。该缺口由 ADR-0018 封口，本 ADR 的 DoD 引用其验收物。

本 ADR v2 由 dfs-grill 双透镜拷打（product-ux / dba-optimizer / sre-resilience 三 seat）后重写，
实证基础：modernc.org/sqlite v1.58.0 模块实测（FTS5 与 vec0 均默认可用、无 CGO）；
DeepSeek 官方 API 无 embeddings 端点（三源实证，其公开面仅 chat/completion）。

## 决策

### 1. 检索是工具，不是服务、不是新项目

在 `agent/tool` 的 `Registry` 中注册 `search_docs`，由 `agent.Run` 的工具调用循环调用。
不新增 HTTP 服务、不新建仓库、不引入 LangChain/LlamaIndex 类框架、不引任何新运行时依赖。

### 2. 分层落位：独立顶层 `retrieval/` 包（Pi 判据应用）

按 vault `LAYERED-ARCHITECTURE.md` 四判据切层：

- **依赖方向单向向上**：`retrieval → llm`（用 `Embedder` 接口）、`retrieval → agent/tool`
  （导出 `SearchTool(idx) tool.Tool` 注册物）；**任何包不得反向依赖 retrieval**；
  `agent` 认知内核保持零检索依赖。
- **底层定原子，上层只加不改**：`tool.Tool`（name/description/parameters）不动；
  检索扩展字段（`mode` / `truncated` / 引用 span）定义在 retrieval 自己的返回类型上。
- **切层依据是知识边界**：retrieval 知道「文档、分块、索引、召回」，不知道循环、队列与 HTTP。
- **可独立使用验证（物理证据）**：`go test ./retrieval/` 的测试树物理不 import
  `agent`/`worker`/`api`（静态断言，非口头约定）；删掉 retrieval 后 `agent+llm+tool` 仍跑
  `calculate`/`current_time`。

索引存储归 retrieval 自持（`*sql.DB` + 全部 DDL），`store` 包保持「任务状态」单一职责，
两者共享 `db_path` 文件、各管各表，由 `main` 装配注入。不把 `doc_chunk` 塞进 `TaskStore` 接口——
那会让 store 从「任务存储」糊成「数据库访问层」，并迫使认知内核知道索引存在。

包结构：

```
retrieval/
  index.go     // Open/Create/Rebuild：DDL、分块事务、corpus_version 内容哈希指纹
  search.go    // 混合召回：FTS5 bm25 + vec0 KNN + RRF 融合；token 预算截断
  embed.go     // Embedder 消费者侧：缓存键、容量上限、并发上限
  tool.go      // SearchTool(idx) tool.Tool：span 引用硬契约 + 哨兵错误
```

### 3. 混合召回：基础设施全做，默认 lexical（禁止静默降级）

- **词法**：SQLite **FTS5**（`modernc.org/sqlite` 内置，实测无 CGO，`bm25()` 可用），
  外接索引 `content=doc_chunk` 避免 text 双份存储。不手写倒排表——「避免 CGO 所以手写」
  的前提经实测不成立。中文语料插入原文（**修订见下**）。
  > **修订注记（2026-10-22，Task 4 实施实证，机制失效）**：原决策「插入前预切成空格分隔
  > bigram」经 modernc.org/sqlite v1.58.0 实测**产生空索引**——trigram 分词器要求 run 内
  > ≥3 字符且以空格为分隔符，bigram 预切把每个 2 字 run 隔开后反而摧毁全部查询（含 3+ 字），
  > 比「影响召回质量」严重一个量级（CJK 语料结构性不可检索）。现路径：Rebuild 插原文，
  > 3+ 字子串查询可命中；`chunk.go` 的 `bigramSplit` 保留但**未接入生产路径**，
  > `TestFTS_BigramSplitProducesEmptyIndex`（`retrieval/index_test.go`）固化该实证防误改回。
  > **残留缺口**：2 字 CJK 查询零命中（trigram 硬限制），Task 6 工具层需显式处理
  > （schema 提示或查询侧 fallback），并在 ADR-0018 评测中单独观察。
  > **Trigger**：modernc trigram 行为变化，或换 CJK 分词策略（unicode61 + 自建词典）时重估本决策。
- **向量**：`llm` 包新增 `Embedder` 接口（OpenAI 兼容 `/v1/embeddings`，独立
  `EmbedBaseURL`/`EmbedAPIKey`/`EmbedModel`/`EmbedDim` 配置，**默认空**）；`llm.Client`
  实现之。**DeepSeek 无 embeddings 端点**（官方公开面仅 chat/completion），因此向量通道
  默认关闭：无凭证时 `mode="lexical"` 显式运行，结果与 trace 中如实标注；
  有凭证时 `mode="hybrid"`。**不允许**把降级结果伪装成全量检索。
- **向量存储**：`sqlite-vec` 虚拟表 `vec0`（驱动内置，`_ "modernc.org/sqlite/vec"` 引入），
  **禁止**手写 `vector BLOB` + Go 全量 cosine（1536 维万行 = 60MB/次查询，P95 与 GC 失控）。
- **融合**：RRF（Reciprocal Rank Fusion），权重可配；词法权重默认不高于向量。
- 检索命中率的对照口径见 ADR-0018：lexical 是主基线，hybrid 是有凭证时的增开组。

### 4. 引用回链是硬契约

工具返回 `{chunk_id, doc_id, span:[start,end], score, mode}`；`span` 缺失即实现错误
（返回参数错误而非空结果）。上层回答必须能引用到 `doc_id + span`。
`mode` 与 `truncated` 必达 trace，供审计与评测（ADR-0018 逐次记录 mode）。

### 5. 上下文预算与取消语义

- `Tool` 接口升级：`Execute(ctx context.Context, args json.RawMessage) (string, error)`
  （ADR-0014 取消语义必须 ctx 到底；search_docs 必然发 HTTP，无 ctx 则不可取消）。
- 错误可分类：错误实现 `Retryable() bool`；`agent.Run` 分支决策——
  retryable：同 step 内重试一次（指数退避 200ms/400ms）后仍失败，错误串喂回模型；
  non-retryable：立即喂回模型并 trace 标 `kind=error`。
  哨兵三分类：`ErrIndexMissing`（不可重试）/ `ErrCorpusStale`（不可重试）/
  `ErrEmbeddingUnavailable`（可重试，退化为 lexical 一次）。
- **检索子预算独立于会话预算**：单次 search_docs 的内嵌 deadline = 剩余任务预算的 50%；
  多个 embedding 请求并发且有并发上限；截断只丢尾部低分片段，`truncated=true` 必达调用方，
  被截片段入 trace 可审计。会话侧预算沿用 ADR 既有策略，两者不同池。
- embedding 缓存：键 = 规范化文本 hash + 模型 + 维度；落 `embed_cache` 表（带 TTL 与容量上限），
  不用进程内 map（OOM 与并发问题）。

### 6. 索引生命周期：幂等、指纹、分块事务

- 索引构建是幂等命令（`cmd/index` 或 `-index` 子命令）；`corpus_version` = 全部源文档
  内容的 SHA-256 聚合指纹（**不用 mtime**：git checkout / 跨机 mtime 漂移会漏判陈旧）。
- 指纹不符时**如实报错**（`ErrCorpusStale`，提示重建），不返回陈旧结果；
  检查点在工具调用时（每次 search 前比对，廉价），启动时仅提示。
- 重建 = 分块事务（每 N 行一提交），**禁止**全量 DELETE+INSERT 单事务——
  embedding 调用持 SQLite 写锁分钟级会阻塞任务写入（历史 <480 QPS 教训同源）。
  指纹相同则跳过重建；崩溃半成品靠指纹不符触发下次重建。
- 首版只做**全量重建**；文档须写明：重建耗时量级、期间检索不可用窗口、触发条件。
- 启动期/提交期预检：索引缺失时 `-index` 命令与启动日志给出结构化摘要
  （处理/跳过/失败文件数）；任务执行中发现索引缺失归独立终态分类
  （`"index missing"`），不得压成 `upstream error`（那会让「提示先建索引」永远到不了客户端）。

### 7. Prompt 版本化

检索结果注入的回答模板放 `prompts/`，文件名带版本（如 `search_answer_v1.md`），
模板头部注明用途与改动理由；变体对照结论进 ADR-0018 评测报告，不在代码里硬编码多套措辞。

### 8. 完成的定义（DoD，硬门槛）

在 ADR-0018 定义的任务集上产出**对照表**：`无检索 vs lexical（vs hybrid，有凭证时）` 的
任务成功率 / 引用命中率 / 首错归因分布 / P95 延迟。**没有数字就不算完成**，
本 ADR 的实施以该表格为验收物，不以「代码能跑」为验收物。

## 四类已知缺陷的应答（dfs-grill Stage 2.5 强制自检）

| #   | 缺陷类              | 本设计的应答                                                                                                                                                 |
| --- | ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| 1   | 错误分类宽度        | 哨兵三分类 + `Retryable()` 循环层分支落地 + 终态分类独立映射（决策 5/6）；`task.Error` 仍只存脱敏粗粒度分类，错误全文只进 slog                               |
| 2   | 取消上下文 teardown | `Execute(ctx, ...)` 到底；retrieval 内嵌 deadline；`store.Complete` 无 ctx 参数（取消路径的终态写不依赖 ctx，规避「已取消 ctx 导致 UPDATE 永不发出」类缺陷） |
| 3   | 进度语义            | 评测指标是**绝对值 + N=3 重复分布**，禁止单次成功率下结论（ADR-0018 决策 3）                                                                                 |
| 4   | 一规则两实现        | corpus_version 指纹只有一处实现（index.go）；引用命中判定只有一处（ADR-0018 的程序判定器）；BM25/RRF 权重只有配置一处，代码不写死第二份                      |

## 候选方案

- **A（采用）**：作为 `tool.Registry` 的一个工具，独立 `retrieval` 包，FTS5+vec0 内置能力，
  默认 lexical 可演进 hybrid，span 引用硬契约。
- **B**：独立 RAG 服务（单独进程/单独仓库，agent 通过 HTTP 调）。
  缺点：多一套部署与协议，与 ADR-0004 的 YAGNI 冲突；面试叙事上会变成「又一个 demo 服务」，
  与现有运行时割裂。
- **C**：放在防御门禁工程（另一仓库，Python 单机工具）里做。
  缺点：与「LLM 用于结构化工程任务」的岗位口径关联弱，Python 单机 CLI 撑不起「系统」叙事。

## 后果

- ✅ 工具循环从「两个玩具工具」升级为「带知识面的 Agent」，trace 显示检索步骤、mode 与命中片段。
- ✅ 分层经 Pi 判据验证：retrieval 可独立测试、可整体替换，agent 内核不受检索演进牵动。
- ✅ 零新运行时依赖：FTS5/vec0 均在既有 modernc.org/sqlite 内。
- ✅ 评测口径（ADR-0018）先于实现锁定，杜绝「代码能跑价值为零」的假完成。
- ⚠️ embedding 依赖外部凭证：默认 lexical 意味着混合能力开箱不可用——
  这是**如实的能力边界**，不是缺陷；有凭证时按 ADR-0018 增开 hybrid 对照。
- ⚠️ 中文语料受 trigram 分词器硬限制：2 字查询零命中（bigram 预切路径已实证失效并修订，见决策 3 修订注记）；3+ 字子串检索可用，短查询策略在 Task 6 工具层显式处理。
- ⚠️ 全量重建策略在语料增长后有重建耗时问题，首版可接受，文档须写明限制。
- ⚠️ 若评测显示「加检索后无提升」，按反向判据收口：不合入该能力，
  转而把时间投回调度/取消/恢复的深度（ADR-0018 定义判定阈值）。

## 非目标

- 不做分布式检索，不引入 Kafka / Redis / 向量数据库服务（沿用 ADR-0004）。
- 不做 Web 前端与多模态检索。
- 不做重排模型（rerank）的独立服务化；首版以融合分数排序。
- 不引入任何 Agent 编排框架；不做 rerank 独立服务。
- 不做增量索引（首版全量重建，限制已写明）。

## 后续

- **ADR-0018（已写）**：评测 harness 形态、任务集来源、指标定义、判定器与报表格式。
- 具体表结构字段类型、命令参数、包内 API 签名在实施时由 `docs/implementation_plan_*.md` 细化。
- 附带待办（grill 发现，建议独立小任务）：`main.go` 的 `deepseek-flash` 模型名已于
  2026-07-24 退役为 `deepseek-v4-flash`，需更新。
