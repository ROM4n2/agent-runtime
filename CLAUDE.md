# Project Guidelines

## [NOTE] 工程规范与避坑检索

- 核心规范源：`{vault_root}/AGENTS.md`（`{vault_root}` 按 `$WIKI_PATH` → `$OBSIDIAN_VAULT_PATH` → 向上查找 `AGENTS.md` 解析，契约见 `dfs/references/vault-root-resolution.md`）
- 检索避坑规范：调用 `search_vault` MCP 或 `python {vault_root}/scripts/search-vault.py "<关键词>"`
- 发现新踩坑自动沉淀：写草稿至 `{vault_root}/99-Inbox/YYYY-MM-DD-{topic}.md`

## 共享工作记忆 (WORKMEMORY)

- 进入会话先读 WORKMEMORY\INDEX.md → PROJECT_OVERVIEW.md → work.log 尾部 50 行；发现未闭合 WORK_START 先询问用户。
- 工作中：决策/踩坑/交接 MUST 以 ≤4KB 事件追加 WORKMEMORY\work.log（schema 见 WORKMEMORY\PROTOCOL.md）。
- 语义知识检索：调用 `search_vault` MCP 或 `python {vault_root}/scripts/search-vault.py "<关键词>"`；成熟知识沉淀走 `/dfs-save`。
