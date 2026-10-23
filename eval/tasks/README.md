# eval/tasks — 检索评测任务集 v1（构造纪律）

本目录是 ADR-0018 决策 1 的**防共谋任务集**：`retrieval_v1.json`（30 条）+ 本文档。
判定器在 `eval/grader.go`（引用命中的唯一实现，禁 LLM-as-Judge）；schema 与解析在
`eval/task.go`；入仓契约测试在 `eval/realset_test.go`（`TestLoadTasks_RealSet`）。

## 1. 纪律：先入仓后跑，禁止跑完挑题

- 任务集**先于评测运行入仓**（本任务时点：检索实现 Task 1-9 已合入分支，但评测尚未开跑）。
- **禁止**根据评测结果增删题目或调整期望 span——那等于让切分器/模型与题目共谋，
  虚高命中率（ADR-0018 决策 1 防共谋纪律）。
- 任何题目/span 变更必须记录在第 6 节核定记录表，并经 orchestrator 走 HITL 合入。
- 三档 scope 必须同时存在：纯语料内直答题会让切分器与题目共谋。

## 2. 语料与 doc_id 口径（评测前必须确认）

- **语料**：仓库 `docs/` 全部文档 + 根目录 `README.md`（ADR-0018 决策 1：语料为仓库自有
  `docs/` + `README`）。
- **doc_id 口径**：`expect_docs` 用**仓库根相对路径**（`docs/adr/00xx-*.md`、`README.md`），
  与 retrieval 的 `doc_id = 相对 corpusDir 的路径`（`retrieval/fingerprint.go` listDocs）对应。
- **因此评测时语料目录必须是仓库根**（`-corpus .`）。若沿用当前 `main.go` 默认值
  `-corpus docs`：doc_id 会变成 `adr/00xx-*.md`（无 `docs/` 前缀）且 `README.md` 不在语料内，
  本题集全部 `expect_docs` 将失配，in-corpus/cross-chunk 题会被批量误判为 wrong-span。
- ⚠️ **HITL 待确认**：`-corpus .` 会索引仓库全部常规文件（`.go`/`go.mod`/`WORKMEMORY/` 等），
  不止 `docs/`+`README`。若要求语料恰好等于"docs/+README"，需在 Task 11/12 前落实
  （实施计划 Fog of War 已登记该问题）。在口径落定前，本题集的 doc_id 前缀不得改动。

## 3. 选材与配比

选材（全部为语料内确定性事实，答案可在原文逐字定位）：

| 来源 | 覆盖内容 |
| --- | --- |
| ADR-0004 | YAGNI 立场、候选方案、条件触发信号、Ladder of Reuse |
| ADR-0010 | SQLite opt-in、modernc 选型、并发写串行化、TaskStore 双实现 |
| ADR-0011 | 优雅停机取消树（cross-chunk 配 ADR-0014） |
| ADR-0013 | Kitchen Sink 四重逻辑、包职责表、TaskRunner 消费者接口 |
| ADR-0014 | cancel API 契约、因果归因、reason 约束、接口归属 |
| ADR-0015 | 补偿接口、启动重灌、崩溃 vs 停机文案区分 |
| ADR-0016 | TraceBuffer 订阅广播、SSE 推送与 WriteTimeout |
| ADR-0017 | 分层落位、引用回链硬契约、哨兵三分类、默认 lexical |
| ADR-0018 | 任务集入仓纪律、eval run 命令、反向判据 |
| README | 配置密钥两种方式、config key 清单、测试命令 |

配比（`realset_test.go` 断言下限：in-corpus ≥40%、cross-chunk ≥30%、out-of-corpus ≥15%）：

| scope | 条数 | 占比 | 说明 |
| --- | --- | --- | --- |
| in-corpus | 13 | 43.3% | 直答，答案在单一文档连续区间 |
| cross-chunk | 10 | 33.3% | 跨 ≥2 文档综合，期望 span 跨文档 |
| out-of-corpus | 7 | 23.3% | 语料外事实；`expect_docs` 列"看似相关"文档 id，`expect_spans` 空数组 |
| **合计** | **30** | — | 规模在 20-50 区间 |

out-of-corpus 题的 `expect_docs` 语义（schema 强制非空）：标注的是**会被误引的文档**，
grader 靠它识别伪造引用（trace 命中该 doc 或回答出现该 doc_id 全串 → wrong-span）。

## 4. span 推定方法（可审计）

### 4.1 span 语义

- span 是**原文 rune 偏移 `[start, end)`（含头不含尾）**，与 `retrieval/chunk.go` 的
  `Chunk.SpanStart/SpanEnd`、`eval/grader.go` 的区间运算**同一坐标系**。
- **不是行号**（ADR-0018 示例里的 `[[120,168]]` 曾被误读为行号；本题集一律字符偏移）。
- 偏移相对**整个文件内容**（与 Rebuild 读入并分块的文本一致）。

### 4.2 推定步骤

1. 在源文档定位答案所在**段落**（非单句，原因见 4.3）；
2. 取段落首标记与尾标记（各为文中唯一子串，脚本校验计数 == 1）；
3. 用 4.4 的一行式计算 rune 起止：`start = index(首标记)`，`end = index(尾标记) + len(尾标记)`；
4. 人工核对 span 首句确为答案所在（本次已抽核 5 题，见第 7 节）。

### 4.3 IoU 预演：span 为什么必须覆盖段落而非单句

grader 的 `spanScore = |交集| / |并集`，命中阈值 0.5；而检索返回的命中 span 是**分块 span**
（`ChunkSize=512`、`ChunkOverlap=64`，步长 448，rune 计）。若期望 span 只有一句（几十 rune），
包含它的分块与期望 span 的 IoU ≈ 句长/512 < 0.5——**即使检索找对了位置也会被判 wrong-span**。
因此构造纪律要求：span 覆盖答案所在段落（≥约 260 rune，或恰好覆盖一个完整分块），
使"理想命中"（包含答案的全部分块都被召回）下 IoU ≥ 0.5。

本题集每题都做过该预演；第 5 节辅助表的 `idealRecall` 列 = 假设包含答案的全部分块
均被召回时 grader 会给出的 recall（每题的下界，真实 top_k 截断只可能更低）。
构造时要求 `idealRecall ≥ 0.5`，全部 30 题满足（最低 q022 = 0.534）。

### 4.4 复跑命令（一行式，无需入仓脚本）

```bash
# 答案首标记的 rune 起止（[start, end) 含头不含尾）：
python -c "import sys;t=open(sys.argv[1],'rb').read().decode('utf-8');i=t.index(sys.argv[2]);print(i,i+len(sys.argv[2]))" \
  docs/adr/0015-crash-recovery-and-task-rehydration.md "status='failed', error='server restarted unexpectedly'"
# 尾标记同理：end = t.index(尾标记) + len(尾标记)；span = [start, end)
```

注意：读文件必须 `rb` + `decode('utf-8')`——与 Go 侧 `os.ReadFile` 一致（不做 newline 翻译）；
文本模式会把 CRLF 归一成 LF，导致偏移漂移。标记必须全文唯一（`t.count(m) == 1`）。

### 4.5 解析与规模自检

```bash
GOWORK=off go test ./eval/ -run TestLoadTasks_RealSet -count=1
```

该测试断言：LoadTasks 全 schema 校验通过、总数 20-50、三档配比下限、全部 span 满足
`0 <= start < end`。

## 5. 题目清单与核定辅助材料

span 均为 `[start, end)` rune 偏移；`idealRecall` 为 4.3 的理想命中下界。
**本表是人工核定辅助材料，不是核定结论。**

| id | scope | expect_docs | expect_spans | idealRecall |
| --- | --- | --- | --- | --- |
| q001 | in-corpus | 0004 | [930,1405) | 0.928 |
| q002 | in-corpus | 0004 | [220,504) | 0.555 |
| q003 | in-corpus | 0010 | [513,870) | 0.697 |
| q004 | in-corpus | 0010 | [1103,1415) | 0.588 |
| q005 | in-corpus | 0013 | [588,1435) | 0.604 |
| q006 | in-corpus | 0013 | [4607,5464) | 0.597 |
| q007 | in-corpus | 0014 | [914,1201) | 0.561 |
| q008 | in-corpus | 0014 | [510,913) | 0.787 |
| q009 | in-corpus | 0015 | [441,780) | 0.640 |
| q010 | in-corpus | 0015 | [781,1293) | 0.633 |
| q011 | in-corpus | 0016 | [929,1226) | 0.580 |
| q012 | in-corpus | 0017 | [3616,4227) | 0.747 |
| q013 | in-corpus | README | [1450,1926) | 0.698 |
| q014 | cross-chunk | 0004, 0010 | [930,1405), [126,382) | 0.714 |
| q015 | cross-chunk | 0010, 0015 | [881,1415), [1380,1613) | 0.911 |
| q016 | cross-chunk | 0013, 0014 | [4607,5464), [914,1309) | 0.684 |
| q017 | cross-chunk | 0016, 0015 | [302,972), [781,1293) | 0.699 |
| q018 | cross-chunk | 0017, 0018 | [5057,5348), [6880,7193), [892,1178) | 0.575 |
| q019 | cross-chunk | 0017, 0013 | [1251,2019), [4607,5464) | 0.632 |
| q020 | cross-chunk | 0017, 0010 | [1682,2325), [513,870) | 0.747 |
| q021 | cross-chunk | 0011, 0014 | [958,1341), [1366,1750) | 0.846 |
| q022 | cross-chunk | 0018, README | [801,1921), [4428,4824) | 0.534 |
| q023 | cross-chunk | 0004, 0017 | [506,919), [3054,3357) | 0.589 |
| q024 | out-of-corpus | README | [] | — |
| q025 | out-of-corpus | 0018 | [] | — |
| q026 | out-of-corpus | README | [] | — |
| q027 | out-of-corpus | 0018 | [] | — |
| q028 | out-of-corpus | README | [] | — |
| q029 | out-of-corpus | 0010 | [] | — |
| q030 | out-of-corpus | 0017 | [] | — |

## 6. 人工核定记录表（2026-10-23 已核定）

| 题目 id | 核定人 | 日期 | 结论 | 修正内容 |
| --- | --- | --- | --- | --- |
| （全体 30 条） | 用户（HITL 授权） | 2026-10-23 | 核准（机器流程替代人工抽查） | 无 |

> 核定方式说明（ADR-0018 决策 1 要求人工核定 ≥10 条）：2026-10-23 HITL 关口，用户选择
> 「信任既定流程直接核准」——替代证据链：①CPE 5 题逐字 spot-check（q001/q009/q012/q018/q022，
> span 首句与源文档逐字相符）；②30 题理想召回 IoU 预演全部 ≥0.5（最低 q022=0.534）；
> ③orchestrator 独立复核 q001/q012 的 span 回读内容与配比计数（13/10/7）。已知残留风险：
> 字符偏移对文档重新格式化敏感（重点题 q014/q022/q018/q023/q002/q007 余量较薄，见 §7），
> 语料文档任何 reflow 后必须重跑 span 推定一行式（§4.4）。

规则：每条被核定的题目登记一行；修正 span 必须写原值→新值；未修正填"否"。
核定完成前，本题集标记为**草案**，不得用于 Task 12 对照评测。

| 题目 id | 核定人 | 核定日期 | 是否修正 span | 修正后 span / 说明 |
| --- | --- | --- | --- | --- |
| q001 |  |  |  |  |
| q002 |  |  |  |  |
| q003 |  |  |  |  |
| q004 |  |  |  |  |
| q005 |  |  |  |  |
| q006 |  |  |  |  |
| q007 |  |  |  |  |
| q008 |  |  |  |  |
| q009 |  |  |  |  |
| q010 |  |  |  |  |
| q011 |  |  |  |  |
| q012 |  |  |  |  |
| q013 |  |  |  |  |
| q014 |  |  |  |  |
| q015 |  |  |  |  |
| q016 |  |  |  |  |
| q017 |  |  |  |  |
| q018 |  |  |  |  |
| q019 |  |  |  |  |
| q020 |  |  |  |  |
| q021 |  |  |  |  |
| q022 |  |  |  |  |
| q023 |  |  |  |  |
| q024 |  |  |  |  |
| q025 |  |  |  |  |
| q026 |  |  |  |  |
| q027 |  |  |  |  |
| q028 |  |  |  |  |
| q029 |  |  |  |  |
| q030 |  |  |  |  |

## 7. 已知局限与重点核定提示

1. **字符偏移的行/字符推定误差风险**（ADR-0018 后果节明言"期望 span 的核定本身带人工误差"）：
   span 为 rune 偏移，语料文档任何重新格式化（增删行、改标点）都会使全部 span 漂移。
   修改语料文档必须同步重推 span 并在第 6 节记录。
2. **余量较薄、建议优先核定**：
   - q014：ADR-0010 want IoU 恰为 0.500（阈值线上，靠 0004 want 0.928 兜底）；
   - q022：ADR-0018 want 0.457（span 1120 rune 超 1024 上限），靠 README want 0.610 兜底；
   - q018：三 want 均值 0.575，为 cross-chunk 中最薄；
   - q023：0017 want 0.372；q002/q007 在 0.555/0.561。
3. **程序判定的固有近似**（禁 LLM 裁判的代价，ADR-0018 决策 2）：
   - in/cross 题要求最终回答包含 `expect_docs` 完整 doc_id 字符串（故 prompt 统一带
     "请引用 docs/ 下具体文档的路径作为依据"）；模型若只写"ADR-0015"不带路径，
     会被记 `right-span-wrong-reasoning`。
   - out-of-corpus 题反过来：回答中出现 expect_docs 全串即判伪造引用；模型若以
     "docs/adr/0018-....md 未提及该数值"的方式否证引用，同样会被记 wrong-span。
4. **span 跨块边界**：部分 span 横跨分块边界（如 q005/q010），理想 IoU 由包含它的
   最佳单块决定；预演已覆盖该情形，但 top_k 截断下若该块未进召回集合仍会失分——
   这是检索侧被测的行为，不是题目缺陷。
5. 起草时工作区存在未提交修改（`docs/interview/m2-llm-timeout.md` 等，非本题集选材）；
   语料内容以磁盘为准，git 跟踪状态不影响 span 推定。

## 8. 防挑题声明

本题集 30 条的题目、prompt、expect_docs、expect_spans 在**任何评测运行之前**一次成型，
全部 span 由第 4 节方法程序化推定并经 IoU 预演（`idealRecall ≥ 0.5`）。本次已由起草方
抽核 5 题（q001/q009/q012/q018/q022，span 首句与源文档逐字相符）；按 ADR-0018 要求，
用户人工抽查 ≥10 条并登记第 6 节后，本题集方可标记为已核定。**跑完评测再改题 = 作弊。**
