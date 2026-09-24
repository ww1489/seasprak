# 基于 Pi SDK 的桌面 Agent 产品需求文档（PRD）

| 项 | 内容 |
|---|---|
| 文档状态 | 草案，对应已采纳的 Proma 技术路线 |
| 日期 | 2026-09-21 |
| 工作名 | Pi 桌面 Agent（正式产品名待定） |
| 参考实现 | 同仓库 `Proma/`（架构对齐，不是 fork 其品牌与全量功能） |

---

## 1. 背景与问题

Pi（`@earendil-works/pi-coding-agent`）已提供可用的编码 Agent 内核：多模型、工具循环、会话 JSONL、上下文压缩。默认产品形态是终端 TUI，不适合作为桌面工作台交付。

我们需要一款 **本地优先的桌面客户端**：用户在图形界面里对话、确认权限、管理项目与能力，底层仍用 Pi 的 Agent 循环，而不是自研 loop，也不是把 Pi 整仓 fork 进产品。

同目录 Proma 已验证该路径：npm 钉死 Pi SDK、自研 Electron UI、Agent 放独立进程、用 `defineTool` / Skills / MCP 桥扩展能力。

## 2. 目标与非目标

### 2.1 目标

- 提供可安装的桌面应用，用户无需使用 Pi CLI / TUI 即可完成「在本地项目里读改文件、跑命令、多步任务」。
- Agent 内核复用 Pi SDK（`createAgentSession`），产品层只做编排、权限、UI 与能力注入。
- 能力可扩展：内置文件工具、产品工具、工作区 Skills、用户 MCP，且全部经过权限确认。
- 会话与配置默认落在本机产品目录，不依赖云端，不共用 `~/.pi/`。
- 能跟随 Pi 上游：精确版本升级 + 必要时 patch 已发布 dist，不维护一份 Pi 源码 fork。

### 2.2 非目标（明确不做）

- 不实现或嵌入 Pi 终端 TUI、slash 命令 UI、`pi install` 扩展市场。
- 不 fork `earendil-works/pi` 整仓作为 vendor。
- 不从 `pi-ai` + `pi-agent-core` 重写 AgentSession / compact / SessionManager。
- 不把 `pi-protocol` / `pi-server` 作为桌面主通信路径。
- MVP 不做：飞书/钉钉/微信机器人、商业订阅渠道、内嵌浏览器自动化、独立 Chat 模式（与 Agent 分家的纯对话栈）。这些可作为后续版本，不进首发范围。

## 3. 用户与场景

### 3.1 目标用户

- 在本机写代码、改文档、跑脚本的开发者或知识工作者。
- 需要图形界面确认危险操作，而不是把全部权限交给终端 Agent。
- 使用自有模型渠道（API Key），数据留在本地。

### 3.2 核心场景

1. **项目任务**：打开本地项目，让 Agent 阅读代码、修改文件、用 shell 执行命令，过程在界面流式展示，写文件 / bash 需确认。
2. **长会话**：同一任务连续多轮；上下文接近窗口上限时自动压缩，用户可继续，不丢会话。
3. **按项目扩展**：为工作区启用若干 Skills（`SKILL.md`）和 MCP Server，Agent 只使用该工作区显式配置的能力。
4. **可恢复**：关闭应用后可从会话列表恢复上次 Pi session，继续同一条任务。

## 4. 产品范围

### 4.1 MVP（必须交付）

- 桌面壳：Electron + React 图形界面（对话流、工具调用展示、权限弹窗、会话列表、渠道设置）。
- 一条 Agent 主路径：发消息 → 流式回复 → 工具执行 → 结束/报错。
- 模型渠道：至少一个 API Key 渠道（OpenAI 兼容或 Anthropic 兼容即可），可配置 base URL。
- 工作区：绑定本地项目根目录作为 Agent cwd。
- 文件类工具（带权限）：Read / Write / Edit / Bash / Grep / Find / Ls。Windows 无 Git Bash/WSL 时可不提供 Bash，并在界面说明。
- 权限：工具执行前可允许一次 / 拒绝；拒绝须反馈给模型。
- 会话：列表、新建、恢复；消息与 Pi session 持久化在产品数据目录。
- 上下文压缩：使用 Pi 原生 auto-compact 与手动触发（若 UI 提供入口）。
- 打包：Windows / macOS 至少一条可安装路径能跑通 Agent（含 Pi native 依赖）。

### 4.2 第二期

- 工作区 Skills：启用 / 停用、`SKILL.md` 同步、@mention 注入。
- 用户 MCP：stdio / HTTP / SSE，主进程连接后注入为 Agent 工具。
- 受信 `AGENTS.md`：仅已授权项目根，禁止祖先目录自动发现。
- 更完整的渠道：多供应商、OAuth（如 Codex）按需。
- 产品级 `defineTool`（规划、搜索等）按业务需要逐个加，不一次做完 Proma 全量。

### 4.3 以后再说

- 独立 Chat 模式（不走 Agent 循环）。
- IM 桥、内嵌浏览器、云端账号 / 计费。
- SubAgent：Proma 有完整实现，但是产品层自研（见附录 A），不是 Pi 内核能力。本产品 MVP / M4 不做；需要时按附录对齐，不要接 `pi install` 或幻想 Pi 内置 Task 工具。

## 5. 功能需求

### 5.1 对话与流式展示

| ID | 需求 | 优先级 |
|---|---|---|
| F1 | 用户输入文本发送给当前会话 Agent | P0 |
| F2 | 界面实时展示 assistant 文本增量 | P0 |
| F3 | 展示工具调用：名称、参数摘要、结果/错误 | P0 |
| F4 | 运行中可停止当前 Agent 轮次 | P0 |
| F5 | 渲染层只消费产品自研消息协议，不直接绑定 Pi 事件类型 | P0 |

### 5.2 权限

| ID | 需求 | 优先级 |
|---|---|---|
| F6 | 写文件、bash 等危险工具默认需确认 | P0 |
| F7 | 用户拒绝后，模型收到明确失败原因，不得当成功 | P0 |
| F8 | 只读工具（Read/Grep/Find/Ls）策略可配置：默认放行或同样确认 | P1 |

### 5.3 渠道与模型

| ID | 需求 | 优先级 |
|---|---|---|
| F9 | 设置页添加/编辑/删除渠道（名称、协议、base URL、API Key） | P0 |
| F10 | API Key 用系统安全存储或加密落盘，不以明文进 git/日志 | P0 |
| F11 | 会话可选择渠道与模型 | P0 |

### 5.4 工作区与会话

| ID | 需求 | 优先级 |
|---|---|---|
| F12 | 会话绑定一个本地目录作为工作区 | P0 |
| F13 | 会话列表：标题、时间、工作区路径；可新建/打开/继续 | P0 |
| F14 | Agent 崩溃或进程退出后，主界面仍可用，可重新拉起该会话进程 | P0 |

### 5.5 能力拓展（P0 仅文件工具；其余 P1）

| ID | 需求 | 优先级 |
|---|---|---|
| F15 | 内置文件工具全部经产品权限包装后交给 Pi | P0 |
| F16 | 关闭 Pi 对用户磁盘上扩展/Skills/上下文文件的自动扫描 | P0 |
| F17 | 工作区 `skills/` 注入 ResourceLoader；模型通过 Read 使用 SKILL.md | P1 |
| F18 | 工作区 `mcp.json` 中已启用的 server，其工具以 `mcp__{server}__{tool}` 出现 | P1 |
| F19 | 不提供 Pi 官方 `pi install` 作为用户扩展方式 | P0（约束） |

## 6. 体验原则

- **本地优先**：会话、工作区配置、Skills、MCP 配置默认在用户数据目录，用文件而非本地数据库。
- **显式能力**：Agent 只能使用产品注入的工具与该工作区启用的 Skills/MCP，不偷偷加载项目外 Pi 扩展。
- **进程隔离**：单个 Agent 卡住或崩溃不得拖死整个窗口；默认一会话一 utility 进程。
- **升级隔离**：Pi 版本变化只允许改 Adapter 与 patch，不得迫使 UI 协议同步大改。

## 7. 架构约束（需求级，非实现清单）

这些是产品必须遵守的技术边界，实现应对齐 Proma，而不是另开一套。

```
图形界面（Renderer）
  → IPC
主进程：会话编排、权限 UI、渠道、工作区
  → Electron utilityProcess（每会话一个）
       → Pi createAgentSession
```

- 依赖：精确版本 `@earendil-works/pi-coding-agent`（传递 `pi-ai`、`pi-agent-core`、`pi-tui`）。`pi-tui` 仅运行时传递依赖，产品代码不 import。
- SDK：`createAgentSession({ noTools: 'builtin', customTools, resourceLoader, sessionManager, modelRuntime })`。压缩与重试用 Pi `SettingsManager` 原生能力。
- 工具注入：文件工具用 Pi 工厂函数；MCP 与产品工具用 `defineTool`。
- ResourceLoader：`noExtensions` / `noSkills` / `noContextFiles`；Skills 只走 `additionalSkillPaths`；`AGENTS.md` 只走已授权 override。
- 打包：Pi 包不得打进 renderer bundle；native（含 `pi-tui/native`）需正确 unpack。Electron 侧需满足 Pi `node >= 22.19`。
- 跟上游：不 fork 源码仓；修 provider 小洞只 patch 已发布包。

## 8. 数据与安全

- 数据根目录：产品自有路径（例如 `~/.{app}/`），与 Pi CLI 的 `~/.pi/` 隔离。
- 持久化：会话元数据 + Pi JSONL；配置为 JSON。
- 权限模型：Pi 本身无沙箱；产品必须在工具 `execute` 前做 `canUseTool`。工作区外路径默认拒绝，除非用户明确授权附加目录（第二期）。
- 日志：禁止打印 API Key、OAuth token、完整文件内容（可截断路径与工具名）。

## 9. 成功标准

MVP 视为达标当且仅当：

1. 用户配置渠道后，在绑定的本地项目中完成一轮「读文件 + 按确认后修改文件」的真实任务。
2. 流式输出与工具卡片在 UI 中可见；拒绝写文件后面模型继续对话且文件未改。
3. 结束并重启应用后，可从会话列表恢复并继续。
4. Agent 子进程被杀掉后，主窗口不退出，可重新开始该会话。
5. 安装包在目标 OS 上冷启动能跑通上述路径（Pi native 无缺失）。

## 10. 里程碑

| 阶段 | 内容 | 对应成功标准 |
|---|---|---|
| M1 | 钉死 Pi SDK、打包 external、utilityProcess 能 `createAgentSession` 并把事件打到主进程 | 进程与依赖 |
| M2 | 渠道 + 权限包装的文件工具 + 自研 UI 协议 + 会话恢复 | 1–4 |
| M3 | 可分发安装包 | 5 |
| M4 | Skills + MCP 桥 | F17–F18 |

## 11. 开放问题

- 正式产品名、包名、数据目录名。
- 首发 OS：仅 Windows，或 Windows + macOS 同时。
- P0 权限粒度：是否允许「本会话始终允许 Read」。
- 首发模型协议：只做一种兼容协议还是 OpenAI + Anthropic 一起。

未关闭前，实现按：Windows 优先、写/bash 必确认、OpenAI 兼容渠道即可开工。

## 附录 A：Proma 的 SubAgent（参考，非本产品范围）

**有，但不是 Pi 自带的。** Pi 官方明确不内置 sub-agent。Proma 用与 MCP/浏览器相同的方式：主进程 `defineTool` 注入协作工具。

实现：[Proma/apps/electron/src/main/lib/agent-collaboration-tools.ts](Proma/apps/electron/src/main/lib/agent-collaboration-tools.ts)。教程里仍写「Claude Agent SDK」，代码已迁到 Pi customTools。

机制：

- 工具名形如 `mcp__collaboration__delegate_agent`（历史 MCP 命名，实际是 in-process `defineTool`）。
- 套件：`list_available_agent_models`、`delegate_agent`、`delegate_agents`（最多 50）、`wait_for_delegations`、`list_delegations`、`get_delegation_results`，以及停止类工具。
- `startDelegation` 会 **新建一条真实 Proma Agent 会话**，走 headless runner（再开一个 `createAgentSession` / utilityProcess），不是 Pi 进程内嵌套 loop。
- 角色：`explore` / `research` / `implement` / `review` / `custom`。
- 约束：必须有工作区；`triggeredBy === 'delegation'` 的子会话 **不再注入协作工具**（禁止套娃）；父会话同时运行上限 50；Pi 重放同一 `toolCallId` 做幂等，避免重复生子会话。
- UI：右侧打开子会话，消息流里展示委派过程；系统提示写明「独立并行探索或对抗审查才用 collaboration」。

若本产品以后要做 SubAgent：复制这套「产品工具 → 新会话」，不要改 Pi 内核。

## 附录 B：与 MiniMax SubAgent 的差别（参考，不采纳其路线）

两边都不是 Pi 内核能力。Proma 在 SDK 上叠一层协作工具；MiniMax 在 vendor 的 Pi 外面包了一整套任务运行时。

| 维度 | Proma | MiniMax |
|---|---|---|
| Pi 接入 | npm 钉死 `pi-coding-agent`，`createAgentSession` | source-vendor `pi-mono@0.79.1`，自研 `PiTurnRunner` 直接 `new Agent()`（`pi-agent-core`） |
| 委派工具 | `mcp__collaboration__delegate_agent` 等 | 产品工具 `task` / `task_append` / `task_query` / `task_output` / `task_stop` |
| 子会话 | 真实可见 Proma 会话，再开 utilityProcess | `sessionKind=task`、默认 hidden；同一进程 `runTurn` |
| 角色 | prompt 角色：explore / research / implement / review / custom | 硬天花板：explore / worker / verifier，另可指向 `mavis` 或自定义 Agent |
| 套娃 | 子会话不再注入协作工具 | 所有 task 子会话关掉 `task`/`task_append`；explore/verifier 再禁写工具 |
| 执行模型 | 创建后独立跑，父用 wait/list 收结果 | 默认前台阻塞父 Turn；可后台 + 完成后唤醒父会话；可 `task_append` 续写/转向 |
| 额外能力 | 阻塞事件冒泡到父会话（ask_user / 权限） | Goal verifier 子 Agent、code review 的 hidden-subagent 模式、Plugin Hook 子 Agent 生命周期、任务行可恢复 |

实现入口：MiniMax [`packages/agent-tools/src/desktop/local-task.ts`](minimax-code/packages/agent-tools/src/desktop/local-task.ts)、[`packages/local-runtime/src/api/local-task-runner.ts`](minimax-code/packages/local-runtime/src/api/local-task-runner.ts)、角色天花板 [`canonical-tool-policy.ts`](minimax-code/packages/agent-tools/src/desktop/canonical-tool-policy.ts)。

MiniMax **SubAgent 产品设计**确实更完整（见计划里「为什么感觉更好」）：`task` 语义、前台默认、`task_append`、硬角色天花板、hidden 子会话、任务可恢复。这些是成品体验，不是 Pi 内核优势。

本产品不跟 MiniMax 的 **Pi 接法**：不 vendor Pi，不自研 `PiTurnRunner`。以后要 SubAgent，优先「MiniMax 的工具 UX + Proma 的 `createAgentSession` 新会话」，不要整套抄 Task 运行时。MVP / M4 仍不做。
