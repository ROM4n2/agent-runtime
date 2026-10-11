# ADR-0012 审讯证据索引（P2-A SSE 方案否决依据）

本目录保存 `/dfs-grill` 双镜审讯（2026-10-11）的原始视角记录，用作
`docs/adr/0012-next-step-observation-transport-decoupling.md` 的证据备份。

被审讯方案：**P2-A —— 增加 SSE 流式推送 + 实时 Agent Trace 观察器**。
审讯结论：**否决全量 SSE，改采「观测与传输解耦」的窄口径 trace**（ADR-0012 方案 A）。

| # | 视角 | 产物 | 覆盖 |
|---|------|------|------|
| 1 | 🎯 产品与用户体验架构师 | [`01-product-ux-interrogation.md`](01-product-ux-interrogation.md) | 12 条：信号真实性、方案错位、失败体验、可演示性 |
| 2 | 🤖 后端并发工程师 | [`02-backend-concurrency-grill.md`](02-backend-concurrency-grill.md) | 12 条：连接/goroutine 生命周期、取消树、背压、静默错误 |
| 3 | 🛡️ SRE / 可靠性工程师 | [`03-sre-resilience-grill.md`](03-sre-resilience-grill.md) | 12 条：超时、并发上界、停机、多实例事件分裂、粘性会话 |
| 4 | 🎨 前端与状态机架构师 | 见 ADR-0012 正文「前端与状态机层」 | 10 条：状态机一致性、EventSource 重连、渲染成本、幂等去重 |

> 第 4 席记录随本 ADR 正文一并定稿（原始输出：子代理 `deleg_f260a2e9/task-3` 转录日志）。

**证据核验说明**：全部 `path:line` 引用均已对照 `master`（HEAD `6a66f2e`，含 ADR-0010/0011）
逐行复核，非基于子代理自述。
