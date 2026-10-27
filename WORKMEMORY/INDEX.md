# WORKMEMORY INDEX — agent-runtime

> **本文件是 HOT 索引，MUST 保持极小**——只放：配置 / 文件清单 / Active handoffs / 已归档 handoffs。
> 主题索引与蒸馏登记**不在这里** ⇒ 见 `INDEX-topics.md`（WARM 层，`grep` 命中才加载）。
> 理由：热层每个会话都要读，主题表会随 WARM 分片持续膨胀；把它留在热层等于让每个新会话
> 为历史买单（2026-10-17 从 DeLector 实例回写：热层 65 行 → 28 行）。

## Configuration

- HOT_RETENTION_EVENTS: 50 # work.log 热层保留事件数；轮转阈值 = 本值 × 1.5

## 文件清单

| 文件 | 职责 |
| --- | --- |
| `PROTOCOL.md` | 协议本体（读一次即可，之后按本索引工作） |
| `PROJECT_OVERVIEW.md` | 60 秒项目 primer（新 agent 第二读） |
| `work.log` | HOT 事件流（append-only，第三读） |
| `INDEX-topics.md` | **WARM 索引**：主题索引（archive/digest 导航）+ 蒸馏登记（→ vault）；按需加载 |
| `archive/` | WARM 层事件分片（按日 `work-YYYY-MM-DD.log`，只按 `INDEX-topics.md` 索引加载） |
| `notes/` | 超限事件正文（`NOTES-<日期>-<时间>-<slug>.md`，事件 >4KB 时外置；首次使用时创建） |
| `cold/` | COLD 层（按月 digest，仅用户显式要求时生成） |
| `handoff_*.md` | 跨 agent 交接包：活跃的放顶层，closed 的 MUST 移入 `archive/` |

## Active handoffs

（无）

## 已归档 handoffs

- `archive/handoff-agent-api.md` — 旧仓库 `agent-api`（2026-08-23，改名前的 M1–M3 阶段）交接包；2026-10-27 归档并加过期标注，仅保留面试/八股历史上下文。**本地文件（gitignored，含本机路径与个人信息，不入库）**。当前权威锚点见根级 `ARCHITECTURE.md`。
