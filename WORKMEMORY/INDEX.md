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

（初始为空；`/dfs-save` 沉淀后回写此表，vault 侧引用用 `vault://` 前缀）
