# WORKMEMORY INDEX — agent-api

## Configuration

- HOT_RETENTION_EVENTS: 50 # work.log 热层保留事件数；轮转阈值 = 本值 × 1.5

## 文件清单

| 文件 | 职责 |
| --- | --- |
| `PROTOCOL.md` | 协议本体（读一次即可，之后按本索引工作） |
| `PROJECT_OVERVIEW.md` | 60 秒项目 primer（新 agent 第二读） |
| `work.log` | HOT 事件流（append-only，第三读） |
| `archive/` | WARM 层（按日分片，只按下方索引按需加载） |
| `cold/` | COLD 层（按月 digest，仅显式要求时生成） |
| `handoff_*.md` | 跨 agent 交接包 |

## 主题索引（archive/digest 内容导航）

（随首次轮转生成）

## Active handoffs

（无）

## 蒸馏登记（WORKMEMORY → vault）

| 日期 | 主题 | vault 归属 |
| --- | --- | --- |
| 2026-10-23 | 检索类评测的三个口径陷阱（语料污染 / 观测截断 / span 口径） | `vault://99-Inbox/2026-10-23-检索类评测的三个口径陷阱：语料污染、观测截断与-span-口径（agent-runtime-p5-实证）` → 归档目标 `01-Rules/AGENT-EVALUATION-HARNESS.md` 新增 §「检索类评测的口径三关」 |
| 2026-10-23 | 评测数字可疑时先证伪 harness，再证伪能力（三轮作废纪律） | `vault://99-Inbox/2026-10-23-评测数字可疑时先证伪-harness，再证伪能力（三轮作废纪律）` → 归档目标 `01-Rules/AGENT-EVALUATION-HARNESS.md` 新增 §「反向判据的前置条件」 |
