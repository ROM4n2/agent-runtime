# WORKMEMORY 协议（跨 Agent 共享工作记忆）
> protocol-version: 1

> 本文件是协议本体，所有能读写文件的 AI agent（Claude Code / Codex / OpenCode / Antigravity / Gemini CLI / CodeBuddy / …）一律遵守。
> 协议铁律的规范层见 vault：`D:\Obsidian\Coding/01-Rules/CROSS-AGENT-MEMORY.md`。

---

## 1. 铁律（MUST）

1. **读序**：每次会话开始处理用户请求前，按序读取：
   `INDEX.md` → `PROJECT_OVERVIEW.md` → `work.log` 最后 50 行。
2. **发现未闭合 WORK_START 必须先询问用户**：「上一任务 '<X>' 未收尾，继续还是重开？」——禁止自行默默继续或忽略。
3. **追加制**：`work.log` 只允许追加（append-only），禁止读改写历史事件；唯一例外是 §5 轮转。
4. **事件 ≤ 4KB**：单事件超限时正文写入独立文件（`NOTES-<slug>.md`）并在事件体里只留一行链接。
5. **UTF-8 写入**：所有 WORKMEMORY 文件一律 UTF-8；Windows 下禁止依赖控制台默认编码（GBK 陷阱）。
6. **禁止敏感信息**：work.log / handoff / NOTES 禁止 API key、token、密码字面量（引用名称即可）；pre-commit 密钥扫描兜底。
7. **协议靠约定维持**：某 agent 漏写事件不是灾难——下一个 agent 读 tail 发现缺口时补一条 `NOTE` 说明即可，不回改历史。

## 2. 事件 Schema（work.log）

```markdown
### YYYY-MM-DD HH:MM | <agent-id> | <EVENT_TYPE>
<body>
```

- `<agent-id>` = `<model-id>__<harness>`（如 `claude-sonnet-4-6__claude-code`、`gpt-5-codex__codex-cli`、`gemini-2-5-pro__antigravity`）。**不确定自己的 model-id 时问用户，禁止猜**——错误身份会永久污染日志。
- 事件头下一行 MUST 附能力声明一行：`Caps: filesystem-read, filesystem-write, shell-exec, web-fetch …`（vendor-neutral 通用词，不写各家工具名）。
- `EVENT_TYPE` 最小集：

| 类型 | 用途 |
| --- | --- |
| `WORK_START` | 开始一段有明确目标的任务；收尾时必须有配对 `WORK_END`。推荐附「不做范围」一行（本任务明确排除的功能/重构/顺手修改），对抗 scope creep |
| `WORK_END` | 任务收尾：做了什么、验证方式（测试命令+结果）、产出物路径。推荐做「回写检查」：稳定结论已回写（vault/模块文档/INDEX）？临时产物（日志/真实数据/凭据/截图）未入库？可选附一行「本次被纠正: <1 行>」（ADR-0004 §4.1-7 措辞，零额外步骤）。 |
| `DECISION` | 架构/实现决策，MUST 含 why 一句话 |
| `PITFALL` | 踩坑：现象 + 根因 + 解法各一句；成熟后走 vault 蒸馏 |
| `HANDOFF` | 交接包已写：指向 `handoff_<topic>.md` |
| `HANDOFF_RECEIVED` | 接收方确认收到并响应 |
| `NOTE` | 其他一切（并行声明、补充、勘误） |

### 标准追加命令（Windows Git Bash 模板）

```bash
cat >> WORKMEMORY/work.log <<'EOF'

### 2026-09-04 15:00 | claude-sonnet-4-6__claude-code | NOTE
Caps: filesystem-read, filesystem-write, shell-exec
<事件正文>
EOF
```

追加前先读 tail 确认无他方未闭合 `WORK_START`；若有，先追加 `NOTE` 声明并行再开工。

## 3. 读序与 60 秒上手

新会话的固定动作：

1. 读 `INDEX.md`（文件清单 + 主题索引 + Active handoffs）——小文件，先看这里找旧工作。
2. 读 `PROJECT_OVERVIEW.md`（项目 primer，60 秒建立全局认知）。
3. `tail -n 50 WORKMEMORY/work.log`（最近事件流）；**顺手 `grep -c '^### ' WORKMEMORY/work.log`，若计数 > `HOT_RETENTION_EVENTS × 1.5` 就先按 §5 轮转再做别的**（把轮转变成读序的第一步，别等有人想起来才做）。
4. 查旧主题：`grep -i "<关键词>" WORKMEMORY/INDEX.md` → 按索引只加载命中的 archive 文件，禁止通读全部历史。

## 4. 交接包（handoff_*.md）

```markdown
---
to: <目标 agent-id 或 "any">
from: <本 agent-id>
created: YYYY-MM-DD HH:MM
role: IMPLEMENT | REVIEW | INSPECT | TEST | VERIFY | FIX（可组合）
required_capability: <如 image-input / android-build；无则省略>
status: open
---
# Handoff: <主题>

## 任务上下文
## 已完成（含证据：测试命令与结果）
## 未完成 / 下一步
## 风险与坑
```

接收方读完：`status` 改 `closed` + 正文追加响应节 + work.log 记 `HANDOFF_RECEIVED`。
发送方：写包 + work.log 记 `HANDOFF` + 更新 `INDEX.md` 的 Active handoffs 节。

## 5. 分层与轮转（HOT / WARM / COLD）

- **HOT**：`work.log`，保留最近 `HOT_RETENTION_EVENTS` 条（配置行在 INDEX.md，默认 50）。
- **轮转触发**：事件数超 `HOT_RETENTION_EVENTS × 1.5` 时，**下一个开始工作的 agent** 顺手执行：最老事件移入 `archive/work-YYYY-MM-DD.log`（按事件日期分片），并更新 INDEX 主题索引。
- **WARM**：`archive/`，只经 INDEX 索引按需加载。
- **COLD**：`cold/digest-YYYY-MM.md`，仅在用户显式要求（"把上月总结成 digest"）时由 agent 蒸馏生成，并同步刷新 PROJECT_OVERVIEW.md。

### 5.1 并发守卫（轮转 = 移动，不是改写）

轮转是 `work.log` 唯一被允许的 read-modify-write 操作（§1.3 追加制的唯一例外），也是并发风险最高的一步——2026-09-26 事件已实证：另一并行会话同时在写同一 `work.log`，写方一度用**过期快照整文件覆盖**，抹掉对方 19 行，最终靠 `git restore --source=HEAD` 才复原。故轮转 MUST 遵守：

- **轮转前**：立即重读 `work.log` 尾部（`tail -n $(…)` 或整文件），**禁止**使用会话早期或缓存的快照作为基准。
- **轮转中**：只做「移出最老事件 + 保留最新 `HOT_RETENTION_EVENTS` 条」，**禁止**顺手改写 / 美化 / 去重 / 重排任何事件正文；移出的分片只做重定位，不改字节。
- **轮转后**：复核两点——(a) 末条事件与轮转前一致（`### ` 末条头不变）；(b) 「轮转前 union 事件集合 == 轮转后 union 事件集合」，逐 `### ` 头对账：

  ```bash
  # 轮转前先存基线
  grep -h '^### ' WORKMEMORY/work.log WORKMEMORY/archive/*.log | sort | uniq -c > /tmp/events.before
  # …执行轮转…
  grep -h '^### ' WORKMEMORY/work.log WORKMEMORY/archive/*.log | sort | uniq -c > /tmp/events.after
  diff /tmp/events.before /tmp/events.after   # 无输出 = 事件集合守恒（不丢、不增、不改）
  ```

- **若发现写入期间有他方追加**（基线里没有的事件出现，或末条与基线不符）⇒ **以最新状态整体重做轮转**，绝不允许丢事件。
- 一句话：**轮转的产物是「移动」，不是「改写」**——事件正文在整个生命周期内只追加、不修改。

## 6. 与 vault 的蒸馏联动

- WORKMEMORY 只存**状态与轨迹**；vault（D:\Obsidian\Coding）只存**规范与结论**。
- 会话收尾（`/dfs-save` 或 agent 主动判定）时：work.log 中成熟的 PITFALL/DECISION → 按 vault 摄取流程沉淀（`save_inbox_draft` 或直写 99-Inbox）→ 在 INDEX.md 蒸馏登记节标注 `vault://<vault 路径>`（用 `vault://` 前缀说明性文字，不写 Obsidian wikilink，避免跨库断链）。
- 反向：vault 规范更新若影响本项目工作方式，当前 agent 记一条 `NOTE` 指向 vault 路径。

## 7. 多机 / 云同步附录（默认不启用）

若 WORKMEMORY 所在项目目录被同步盘（Dropbox/iCloud/OneDrive）管理：改用 per-session 分文件
`sessions/YYYY-MM-DDTHH-MM__<model-id>__<harness>.log`，每会话写自己的文件，协议其余不变。
（git 仓库场景直接随 git 走，无需此模式。）

## 8. 原生记忆的关系

本协议**不替代**任何 agent 的原生记忆（Claude auto-memory、Codex sqlite、CodeBuddy memery 照常运行）——它是各 agent **共同的写入层**。原生记忆是私有层，WORKMEMORY 是公约层。
