# 源码证据与原方案差异

本文件记录已核实的事实及其对 PRD 的影响。**源码事实、产品决策、设计建议是三类不同信息**；本轮只做文档与静态核查，没有实现 Agent，也没有运行上游测试或真实模型评测。

## 版本与资料基线

| 来源 | 本轮使用版本/位置 | 说明 |
| --- | --- | --- |
| pi | `5cd93f688aaab89dbb6dfa4aca535f21796ae185`；包版本 0.84.2 | 本地参考源码；核查时工作区干净 |
| Eino | `ebd616c8291e957684ea6ca99dd54225d04e0438` | 本地执行框架源码；存在既有未跟踪 `docs/`，未改动 |
| eino-ext | `6752ff8da9b1ea85c8e91b27b0a64fef099f22f9` | 模型及集成研究入口；核查时工作区干净 |
| DeepSeek Harness | `ddefc45fbc7f8e46dd73185e68295696d1297887`；本地 `deepseek-harness/`，origin 为 deepseek-ai/deepseek-harness | 用户指定的安全审核/审批/审计职责参考；本轮只读；证据在安全补充篇 |
| Zero | `99721c762f37cd43ac511007a5f51d1846df959e`；本地 `zero/` | 用户指定的 Go 原生三平台沙箱实现参考；`internal/sandbox` 需抽取移植，不是公共 SDK |
| Coze Studio | `fefb05ff27be1da939612fbf9faf5db62583b8ae`；本地 `coze-studio/` | Canvas JSON → schema → Eino compile 的格式语义参考；不引入整个后端 |
| 原设计 | [pi-eino-deepagent-design.md](../pi-eino-deepagent-design.md) | 前序技术方向记录；入口与分层已按用户澄清更新，源码差异单列如下 |
| 教程 | [源码学习目录](https://dg-ai-notes.pages.dev/modules/) | 十章均已对照研究；原文标明基于 pi v0.80.2，实际行为按各章本地证据核查 |
| Eino 官方 | [Quickstart](https://www.cloudwego.io/zh/docs/eino/quick_start/) | 有模型、Runner、Session、Tool、Middleware、Callback、Interrupt、Graph Tool、Skill、A2UI、TurnLoop 章节 |

教程网页可访问；网页检索无法读取的内容通过其公开[源码仓库](https://github.com/buchidonggua/dg-ai-notes/tree/main/pi-agent/web/src/content/modules)补读 MDX 原文。本 PRD 不采纳教程的热度、性能或市场比较数字。以下保留前两章建立的基础证据，M03～M10 与补充篇各有本章新增证据表；不把全部引用重复复制到本文件。

下文源码链接指向本地 checkout；`[VERIFY: ...]` 中的路径相对 `D:/Code/owner_agents`。行号以本轮工作树为准，升级后须重新核查。

## pi 证据

### P-01：依赖方向比包数量更重要

coding-agent 同时依赖 agent-core、ai、tui 等包；agent-core 依赖 ai；ai 也依赖 telemetry，因此不能笼统称其“不依赖任何 pi 包”。

- [coding-agent 依赖](../../pi/packages/coding-agent/package.json#L46) `[VERIFY: pi/packages/coding-agent/package.json:46]`
- [agent-core 依赖](../../pi/packages/agent/package.json#L38) `[VERIFY: pi/packages/agent/package.json:38]`
- [ai 的 telemetry 依赖](../../pi/packages/ai/package.json#L65) `[VERIFY: pi/packages/ai/package.json:65]`

PRD 推导：允许上层直接使用底层公共模型/工具类型；禁止把具体 UI 或业务逻辑反向写进执行核心。

### P-02：可运行 SDK 的装配路径

`createAgentSession` 接收工作区、模型运行时、资源加载器、会话和工具配置；在内部先建立低层 Agent，再建立 AgentSession。编码默认工具在装配处选择。

- [createAgentSession](../../pi/packages/coding-agent/src/core/sdk.ts#L171) `[VERIFY: pi/packages/coding-agent/src/core/sdk.ts:171]`
- [默认工具](../../pi/packages/coding-agent/src/core/sdk.ts#L254) `[VERIFY: pi/packages/coding-agent/src/core/sdk.ts:254]`
- [Agent 构建](../../pi/packages/coding-agent/src/core/sdk.ts#L304) `[VERIFY: pi/packages/coding-agent/src/core/sdk.ts:304]`
- [AgentSession 构建](../../pi/packages/coding-agent/src/core/sdk.ts#L386) `[VERIFY: pi/packages/coding-agent/src/core/sdk.ts:386]`

PRD 推导：编码能力是 SDK 的首个装配场景，而不是所有宿主都必须接受的能力集合。

### P-03：本地 agent-core 的职责已扩展，部分新接口尚未落地

当前 agent-core 导出 harness、session、skills、compaction 与工具；`AgentTool` 接口提供工具执行能力。不能写成“agent-core 包完全不含 read/bash”。同时，新 `AgentHarness.prompt` 等执行入口仍返回 unavailable；当前 coding-agent SDK 不能被描述为已全面迁移新 harness。

- [agent-core 导出](../../pi/packages/agent/src/index.ts#L46) `[VERIFY: pi/packages/agent/src/index.ts:46]`
- [createReadTool](../../pi/packages/agent/src/harness/tools/read.ts#L45) `[VERIFY: pi/packages/agent/src/harness/tools/read.ts:45]`
- [AgentTool](../../pi/packages/agent/src/types.ts#L386) `[VERIFY: pi/packages/agent/src/types.ts:386]`
- [AgentHarness 未实现入口](../../pi/packages/agent/src/harness/agent-harness.ts#L365) `[VERIFY: pi/packages/agent/src/harness/agent-harness.ts:365]`

PRD 推导：区分低层 Agent 的职责与整个 npm 包的内容；用已实现调用路径推导行为，用新接口作为演进参考。

### P-04：消息扩展和模型投影

`AgentMessage` 通过 TypeScript 类型合并开放扩展；模型调用前有 `transformContext` 和 `convertToLlm`。coding-agent 的 custom 消息会投影为 user，展示标志本身不控制是否发送给模型。

- [AgentMessage](../../pi/packages/agent/src/types.ts#L325) `[VERIFY: pi/packages/agent/src/types.ts:325]`
- [模型前转换](../../pi/packages/agent/src/agent-loop.ts#L290) `[VERIFY: pi/packages/agent/src/agent-loop.ts:290]`
- [coding-agent 消息投影](../../pi/packages/coding-agent/src/core/messages.ts#L148) `[VERIFY: pi/packages/coding-agent/src/core/messages.ts:148]`

PRD 推导：Go 的 role/codec registry 是本产品设计，不是对 pi 原生运行时 API 的照搬；分别定义保存、显示和模型投影。

### P-05：扩展注册与运行职责分开

扩展 API 将工具、命令、事件处理器和消息渲染器分别登记，并把执行动作交给 runtime。

- [createExtensionAPI](../../pi/packages/coding-agent/src/core/extensions/loader.ts#L252) `[VERIFY: pi/packages/coding-agent/src/core/extensions/loader.ts:252]`
- [工具注册](../../pi/packages/coding-agent/src/core/extensions/loader.ts#L267) `[VERIFY: pi/packages/coding-agent/src/core/extensions/loader.ts:267]`
- [命令注册](../../pi/packages/coding-agent/src/core/extensions/loader.ts#L276) `[VERIFY: pi/packages/coding-agent/src/core/extensions/loader.ts:276]`

PRD 推导：ExtensionRegistry 负责声明与版本，AgentSession 负责会话内的执行协调，Agent 实际执行；不能声称 pi 自带本方案的 Workflow Registry。

### P-06：steering / follow-up 的真实消费边界

pi 低层循环首次进入和每个完整 turn 后检查 steering，并不要求刚完成的 turn 一定调用了工具；follow-up 在内层循环将结束时检查，仍处于同一次低层 Run。

- [初次 steering](../../pi/packages/agent/src/agent-loop.ts#L167) `[VERIFY: pi/packages/agent/src/agent-loop.ts:167]`
- [turn 后 steering](../../pi/packages/agent/src/agent-loop.ts#L259) `[VERIFY: pi/packages/agent/src/agent-loop.ts:259]`
- [follow-up](../../pi/packages/agent/src/agent-loop.ts#L263) `[VERIFY: pi/packages/agent/src/agent-loop.ts:263]`

PRD 推导：首轮前和无工具 Turn 结束后也需检查 steering；follow-up 在同一次完整运行的外层继续。M03 以 Trace 表达此边界，Eino 可能需要多个框架执行段，但对外保持同一 traceId，不能把 follow-up 改成另一完整运行。

### P-07：两套会话实现不能混成一个现状

coding-agent 的 SessionManager 使用带父子关系的 JSONL 历史，并沿当前分支构建上下文；agent-core 同时存在新的 session repo 抽象和 JSONL 后端。新接口存在不等于当前产品路径已经使用它。

- [SessionManager](../../pi/packages/coding-agent/src/core/session-manager.ts#L855) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:855]`
- [buildSessionContext](../../pi/packages/coding-agent/src/core/session-manager.ts#L461) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:461]`
- [新 SessionStorage](../../pi/packages/agent/src/harness/session/types.ts#L290) `[VERIFY: pi/packages/agent/src/harness/session/types.ts:290]`

PRD 推导：产品 Session 的历史语义独立设计，读取当前实现时必须说明使用哪一条路径。M10 再细化持久化与分叉。

### P-08：名称与职责边界

本地 pi coding-agent 通过 `createAgentSession` 组合资源加载器、会话历史管理器和 Agent，然后返回 AgentSession；AgentSession 持有 Agent、SessionManager 与资源加载器。SessionManager 的职责是历史树和持久化，ResourceLoader 暴露资源读取与 reload。各组件分担职责，不能把名称相近的会话对象全部解释为全局管理器。

- [创建与资源装配](../../pi/packages/coding-agent/src/core/sdk.ts#L171) `[VERIFY: pi/packages/coding-agent/src/core/sdk.ts:171]`
- [AgentSession 的组合成员](../../pi/packages/coding-agent/src/core/agent-session.ts#L310) `[VERIFY: pi/packages/coding-agent/src/core/agent-session.ts:310]`
- [SessionManager](../../pi/packages/coding-agent/src/core/session-manager.ts#L855) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:855]`
- [ResourceLoader](../../pi/packages/coding-agent/src/core/resource-loader.ts#L40) `[VERIFY: pi/packages/coding-agent/src/core/resource-loader.ts:40]`

pi 的 `AgentSessionRuntime` 持有当前 AgentSession 和 cwd 相关服务，并在 switch/new/fork 操作中替换当前组合；它不等于底层模型执行循环。

- [AgentSessionRuntime](../../pi/packages/coding-agent/src/core/agent-session-runtime.ts#L74) `[VERIFY: pi/packages/coding-agent/src/core/agent-session-runtime.ts:74]`
- [切换会话](../../pi/packages/coding-agent/src/core/agent-session-runtime.ts#L196) `[VERIFY: pi/packages/coding-agent/src/core/agent-session-runtime.ts:196]`

本产品采用上述核心名称与职责语义，`CreateAgentSession` 是 Go 风格的创建入口名称。Agent 内部仍使用 Eino；ExtensionRegistry、AddSubAgent、SessionStore 及更强的提交/事件保证均是产品新增或适配契约，不伪称 pi 已有同名实现。统一术语见[第 2 章](02-architecture-boundaries.md#23-与-pi-对齐的对象名称与职责)。

### P-09：Trace、轮后顺序与完整收尾

教程用 Trace 表示 agent_start 到 agent_end 的完整运行，包含多个 Turn；这里采用这个运行概念，不将其定义成日志追踪。pi 的原生事件表达运行边界，并不要求照抄一个 Trace 类。

- [运行开始](../../pi/packages/agent/src/agent-loop.ts#L109) `[VERIFY: pi/packages/agent/src/agent-loop.ts:109]`；[轮后顺序](../../pi/packages/agent/src/agent-loop.ts#L224) `[VERIFY: pi/packages/agent/src/agent-loop.ts:224]`；[外层 follow-up 及结束](../../pi/packages/agent/src/agent-loop.ts#L262) `[VERIFY: pi/packages/agent/src/agent-loop.ts:262]`。
- [pigo 无工具路径](../../pigo/internal/runtime/loop.go#L228) `[VERIFY: pigo/internal/runtime/loop.go:228]`：流程图省略了这一支的 turn_end/afterTurn，实际源码仍会调用。
- [pigo 全 terminate](../../pigo/internal/runtime/loop.go#L255) `[VERIFY: pigo/internal/runtime/loop.go:255]` 直接收尾；pi 批次聚合后仍进入轮后及队列判断。使用配图的双层结构，不将不同实现的细节拼成同一套事实。
- [coding-agent 结束后重试标记](../../pi/packages/coding-agent/src/core/agent-session.ts#L648) `[VERIFY: pi/packages/coding-agent/src/core/agent-session.ts:648]`；[settled](../../pi/packages/coding-agent/src/core/agent-session.ts#L607) `[VERIFY: pi/packages/coding-agent/src/core/agent-session.ts:607]`。产品完整回复的结束须汇总内部续执行，不能透传任一次底层结束。

M03 的 Trace 可跨内部重试、自动压缩和审批恢复；M07 用 executionId 区分内部 agent_start/agent_end，仅在完整终态提交后发布产品 trace.settled。这是面向完整回复的产品适配，不声称一个 Trace 与底层每一次 agentLoop 一一对应。

### P-10：模型层统一语义与流契约

M04 不只要求供应商格式转换，还要求统一调用语义。pi 的共性是协议与函数契约，而非 Provider 继承体系；教程中档位数和具体 token/TTL 数字不能直接当作当前全模型保证。

- [思考档位](../../pi/packages/ai/src/types.ts#L83) `[VERIFY: pi/packages/ai/src/types.ts:83]`；[支持集与 clamp](../../pi/packages/ai/src/models.ts#L900) `[VERIFY: pi/packages/ai/src/models.ts:900]`：本地已有 max，映射先查支持集，再向上/向下查找。
- [共享输出空间](../../pi/packages/ai/src/api/simple-options.ts#L75) `[VERIFY: pi/packages/ai/src/api/simple-options.ts:75]`：思考预算与模型输出上限相互约束；M04 解析有效参数，M08 完成完整请求预算。
- [StreamFunction 错误约束](../../pi/packages/ai/src/types.ts#L324) `[VERIFY: pi/packages/ai/src/types.ts:324]`；[事件终止协议](../../pi/packages/ai/src/types.ts#L528) `[VERIFY: pi/packages/ai/src/types.ts:528]`：中间更新与最终成功/错误消息有不同语义，不是所有事件都携带 partial。
- [调用入口](../../pi/packages/ai/src/compat.ts#L242) `[VERIFY: pi/packages/ai/src/compat.ts:242]`：未注册 API 的解析可抛错；“永不抛出”不能泛化为所有配置/路由阶段都无异常。Eino 保留 Go error，由产品统一收尾。
- [溢出检测](../../pi/packages/ai/src/utils/overflow.ts#L134) `[VERIFY: pi/packages/ai/src/utils/overflow.ts:134]`：结合错误证据、可信用量/窗口及 length 零输出，并排除限流；不是见到空文本就压缩。

产品的显式 off/必需能力约束、每次 attempt 收尾及经过认证的检测规则见 M04。模型组件是否实现某个字段、是否提供所需扩展点，仍需按实际调用路径验证。

### P-11：压缩分区、摘要更新与文件集合

M09 以当前 coding-agent 路径为参考，不将教程伪代码当作产品完整算法。

- [有效切点及工作起点](../../pi/packages/coding-agent/src/core/compaction/compaction.ts#L308) `[VERIFY: pi/packages/coding-agent/src/core/compaction/compaction.ts:308]` 与 [cut point](../../pi/packages/coding-agent/src/core/compaction/compaction.ts#L403) `[VERIFY: pi/packages/coding-agent/src/core/compaction/compaction.ts:403]`：切点是第一条保留记录；排除工具结果，工作前缀可能跨多个模型/工具 Turn。
- [首次/更新模板](../../pi/packages/coding-agent/src/core/compaction/compaction.ts#L467) `[VERIFY: pi/packages/coding-agent/src/core/compaction/compaction.ts:467]`、[材料构造](../../pi/packages/coding-agent/src/core/compaction/compaction.ts#L643) `[VERIFY: pi/packages/coding-agent/src/core/compaction/compaction.ts:643]`：历史作为待总结材料，previousSummary 参与更新，不继续业务对话。
- [前缀范围](../../pi/packages/coding-agent/src/core/compaction/compaction.ts#L773) `[VERIFY: pi/packages/coding-agent/src/core/compaction/compaction.ts:773]` 包含起始用户式消息；[当前组合代码](../../pi/packages/coding-agent/src/core/compaction/compaction.ts#L873) `[VERIFY: pi/packages/coding-agent/src/core/compaction/compaction.ts:873]` 顺序生成两个部分，不是教程的 Promise.all。产品另要求没有新主历史时也保留上一份有效摘要。
- [文件集合继承](../../pi/packages/coding-agent/src/core/compaction/compaction.ts#L42) `[VERIFY: pi/packages/coding-agent/src/core/compaction/compaction.ts:42]` 从旧 details 取数据；[提取与列表计算](../../pi/packages/coding-agent/src/core/compaction/utils.ts#L29) `[VERIFY: pi/packages/coding-agent/src/core/compaction/utils.ts:29]` 仅按 toolCall.name/path 提取 read/write/edit，不验证成功。readFiles 最终指只读未修改，modifiedFiles 是写入/编辑并集。
- [附录与 details](../../pi/packages/coding-agent/src/core/compaction/compaction.ts#L935) `[VERIFY: pi/packages/coding-agent/src/core/compaction/compaction.ts:935]` 由代码生成；[会话重建](../../pi/packages/coding-agent/src/core/session-manager.ts#L410) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:410]` 使用最新摘要、包含 firstKept 的原文及压缩记录之后的消息。

产品按 M05 实际执行事实补足失败/未知文件效果，按 M06 过滤模型可见范围；不宣称 pi 文件列表等价于已验证的工作区修改或当前 Git diff。文件清单不交给模型自由重写，Eino 现有 Summarization 不自动提供这些产品保证。

### P-12：会话树、配置重建与 JSONL 的实际边界

- [九类 Entry 与文件 header](../../pi/packages/coding-agent/src/core/session-manager.ts#L32) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:32]` 可归为上下文、历史配置、纯元数据三类职责；header 不在 parent 链中，children 属于派生展示结构。
- [branch / resetLeaf / branchWithSummary](../../pi/packages/coding-agent/src/core/session-manager.ts#L1360) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:1360]` 移动游标并追加；带摘要时接收现成摘要，后续新消息是摘要的后代，不是与摘要同父的兄弟。[createBranchedSession](../../pi/packages/coding-agent/src/core/session-manager.ts#L1413) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:1413]` 则复制选定路径为新会话。
- [getSessionContextSettings](../../pi/packages/coding-agent/src/core/session-manager.ts#L362) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:362]` 沿完整路径覆盖式读取模型/思考状态，包含 assistant 的 provider/model；教程“assistant 没有模型来源”的说法不适用于本地版本。[buildSessionContext](../../pi/packages/coding-agent/src/core/session-manager.ts#L461) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:461]` 将配置提取与压缩后的消息选择分开。
- [默认目录](../../pi/packages/coding-agent/src/core/session-manager.ts#L474) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:474]` 位于用户级 `~/.pi/agent/sessions/<encoded-cwd>/`；[_persist](../../pi/packages/coding-agent/src/core/session-manager.ts#L1015) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:1015]` 的首条 assistant 前延迟写入及 `"wx"` 独占创建，不提供产品受理即持久或多记录原子提交保证。
- [历史格式迁移](../../pi/packages/coding-agent/src/core/session-manager.ts#L230) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:230]` 与 [_rewriteFile](../../pi/packages/coding-agent/src/core/session-manager.ts#L979) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:979]` 表明逻辑追加历史不等于文件从不重写；[branch](../../pi/packages/coding-agent/src/core/session-manager.ts#L1360) 仅移动内存游标，不能据此声称回退后未追加便重启也会保留当前位置。

产品 M10 按所选祖先路径继承有效 CompactionEntry，创建时的 branchId 不排除公共祖先摘要，兄弟分支独有后缀不自动进入；核对事实另经受控操作提交，是否可恢复仍受 checkpoint/许可条件约束。

产品 M10 补充持久活动游标、分支头引用、受理与事件提交、格式诊断和原件保护；这些不是上述 pi API 已提供的保证。配置重建使用明确生效记录，保留 assistant 请求/响应来源，避免响应别名误改下一次默认模型；新 Trace 与 checkpoint 的取值按 M04 分开。两套 pi 会话实现的关系仍以 P-07 为准，不要求增加第二套产品会话层。

### P-13：扩展运行操作与会话生命周期

pi 的 [ExtensionAPI](../../pi/packages/coding-agent/src/core/extensions/types.ts#L1214) `[VERIFY: pi/packages/coding-agent/src/core/extensions/types.ts:1214]` 同时暴露注册方法、发送消息、追加私有条目、工具/模型选择；[loader 委托](../../pi/packages/coding-agent/src/core/extensions/loader.ts#L252) `[VERIFY: pi/packages/coding-agent/src/core/extensions/loader.ts:252]` 将登记和运行操作分开。[会话 before 结果](../../pi/packages/coding-agent/src/core/extensions/types.ts#L1124) `[VERIFY: pi/packages/coding-agent/src/core/extensions/types.ts:1124]` 包含取消与 compaction/tree 摘要候选。

产品 M11 采用扩展上下文的窄操作入口，M07 定义 before/after 顺序；输入、工具选择、私有状态仍遵守 M03/M05/M10 的作用域和持久化。pi 的运行时刷新与同名覆盖不直接成为产品规则：产品明确替换形成新 generation，固定实现内的选择可在下一 Turn 生效。同会话分叉与 pi 复制新会话的 fork 分开，不扩展插件加载机制。

## Eino 证据

### E-01：DeepAgent 是可配置装配，不是完整产品外壳

`deep.New` 通过 TypedConfig 设置模型、指令、工具、子 Agent、backend、shell、handlers 等，再建立 ChatModelAgent。默认会建立 TODO 能力；有默认或显式子 Agent 时建立 task 工具；文件/执行工具依赖相应配置。

- [TypedConfig](../../eino/adk/prebuilt/deep/deep.go#L44) `[VERIFY: eino/adk/prebuilt/deep/deep.go:44]`
- [task 条件及装配](../../eino/adk/prebuilt/deep/deep.go#L130) `[VERIFY: eino/adk/prebuilt/deep/deep.go:130]`
- [ChatModelAgent 构建](../../eino/adk/prebuilt/deep/deep.go#L151) `[VERIFY: eino/adk/prebuilt/deep/deep.go:151]`
- [内置 middleware](../../eino/adk/prebuilt/deep/deep.go#L209) `[VERIFY: eino/adk/prebuilt/deep/deep.go:209]`

PRD 推导：不需要重写推理循环，但仍需产品 Session、扩展装配、交互和权限策略。用户已确认产品默认装配文件、命令、TODO 与 general-purpose 子 Agent，并要求 Session 绑定工作区；CreateAgentSession 需提供相应 Backend/Shell，不能把产品默认选择说成 Eino 零配置即可使用文件和命令。

### E-02：默认子 Agent 继承配置

general-purpose 子 Agent 构造时接收主 Agent 的工具、middleware 和 handlers；工作流等显式子 Agent 可以通过 agent tool 包装。

- [默认子 Agent 配置](../../eino/adk/prebuilt/deep/task_tool.go#L85) `[VERIFY: eino/adk/prebuilt/deep/task_tool.go:85]`
- [AgentTool 包装](../../eino/adk/prebuilt/deep/task_tool.go#L114) `[VERIFY: eino/adk/prebuilt/deep/task_tool.go:114]`

推导出的产品要求：有状态的 steering、历史写入或权限 handler 需要区分主/子执行作用域；共享 handler 对象不应等于共享主任务输入队列。此项是基于继承关系识别的适配风险，不是已复现的产品缺陷。

### E-03：输入规划、准备实例和恢复

TurnLoop 的 `GenInput` 返回 Consumed / Remaining；`PrepareAgent` 在规划后调用。该回调返回错误会使 loop 退出。恢复路径也经过实例准备。Checkpoint 保存 runner 状态和输入 bookkeeping，要求输入类型可进行 gob 编码。

- [配置契约](../../eino/adk/turn_loop.go#L562) `[VERIFY: eino/adk/turn_loop.go:562]`
- [checkpoint 与 gob](../../eino/adk/turn_loop.go#L613) `[VERIFY: eino/adk/turn_loop.go:613]`
- [Consumed / Remaining](../../eino/adk/turn_loop.go#L636) `[VERIFY: eino/adk/turn_loop.go:636]`
- [planTurn 调用](../../eino/adk/turn_loop.go#L1679) `[VERIFY: eino/adk/turn_loop.go:1679]`
- [PrepareAgent 错误分支](../../eino/adk/turn_loop.go#L1697) `[VERIFY: eino/adk/turn_loop.go:1697]`

PRD 推导：AgentSession 协调输入去重和候选构建；新独立输入受理前构建失败可保留旧可用版本，受理时将目标、generation 及依赖引用一致保存。PrepareAgent 只重建已受理版本，失败则明确失败/不兼容，不静默换版；queued/hold、恢复旧任务同样保留原版本。Checkpoint 的产品用途是恢复执行，不能替代用户历史树。

### E-04：Stop 结束的是整个循环实例

Run 通过 `sync.Once` 启动；Stop 关闭缓冲；已停止的 loop 不再正常接受输入。退出结果含未处理及中断输入，另外有 late items 的接管机制。

- [退出状态](../../eino/adk/turn_loop.go#L794) `[VERIFY: eino/adk/turn_loop.go:794]`
- [runOnce](../../eino/adk/turn_loop.go#L1335) `[VERIFY: eino/adk/turn_loop.go:1335]`
- [Push 契约](../../eino/adk/turn_loop.go#L1354) `[VERIFY: eino/adk/turn_loop.go:1354]`
- [Stop](../../eino/adk/turn_loop.go#L1500) `[VERIFY: eino/adk/turn_loop.go:1500]`
- [关闭缓冲](../../eino/adk/turn_loop.go#L1554) `[VERIFY: eino/adk/turn_loop.go:1554]`

PRD 推导：长期对象是 Session；TurnLoop 是可更换实例。同一时刻仍只有一个顶层执行协调者。

### E-05：AgenticMessage 的旧注释不足以说明当前能力

`interface.go` 的注释仍说 AgenticMessage cancel/retry 尚未接线，但 `buildAgenticReActRunFunc` 已接入 retry/failover/cancel 与 graph interrupt，且仓库包含相应测试。这里只确认实现和测试存在，未运行测试，也不宣称所有 provider 路径已完全兼容。

- [旧注释](../../eino/adk/interface.go#L451) `[VERIFY: eino/adk/interface.go:451]`
- [Agentic ReAct 装配](../../eino/adk/chatmodel.go#L1246) `[VERIFY: eino/adk/chatmodel.go:1246]`
- [取消上下文](../../eino/adk/chatmodel.go#L1273) `[VERIFY: eino/adk/chatmodel.go:1273]`
- [重试测试](../../eino/adk/agentic_test.go#L1419) `[VERIFY: eino/adk/agentic_test.go:1419]`
- [工具后取消测试](../../eino/adk/agentic_react_test.go#L512) `[VERIFY: eino/adk/agentic_react_test.go:512]`

PRD 决策（已由用户确认）：采用 `*schema.AgenticMessage` / `model.AgenticModel`，替换旧稿的 Message 基线。这里依据本地实现说明接入点，不将用户选型表述为所有官方文档已经统一迁移，也不声称 provider 兼容性测试已通过。

- [Agentic 角色、内容块与响应元信息](../../eino/schema/agentic_message.go#L39) `[VERIFY: eino/schema/agentic_message.go:39]`：角色为 system/user/assistant，工具结果使用 FunctionToolResult 块，不能继续按独立 tool role 解析。
- [AgenticModel](../../eino/components/model/interface.go#L105) `[VERIFY: eino/components/model/interface.go:105]`：工具使用调用选项，不能混用旧 ToolCallingChatModel 接口。
- [DeepAgent 泛型配置](../../eino/adk/prebuilt/deep/deep.go#L40) `[VERIFY: eino/adk/prebuilt/deep/deep.go:40]`；[NewTyped](../../eino/adk/prebuilt/deep/deep.go#L116) `[VERIFY: eino/adk/prebuilt/deep/deep.go:116]`：主/子 Agent 消息类型一致。
- [Agentic 事件](../../eino/adk/interface.go#L66) `[VERIFY: eino/adk/interface.go:66]`：使用 AgenticRole 与内容块，Role/ToolName 不承载该路径的输出角色/工具信息。
- 消息默认转换与上下文两阶段规则见 M06；Agentic 适配器、供应商缓存和旧接口兼容边界见 M04。旧组件的能力不能未经验证直接归给新路径。

### E-06：延迟替换 Agent 不等于冻结所有内容

Skill middleware 通过 backend 读取内容，skill 加载工具调用时执行 `Get`。模型前 state 重写也允许调整 ToolInfos；因此“工具列表绝对不能在 Run 中变化”不是精确的框架事实。

- [SkillBackend 契约](../../eino/adk/middlewares/skill/skill.go#L66) `[VERIFY: eino/adk/middlewares/skill/skill.go:66]`
- [skill 加载与 Get](../../eino/adk/middlewares/skill/skill.go#L419) `[VERIFY: eino/adk/middlewares/skill/skill.go:419]`
- [模型前状态](../../eino/adk/chatmodel.go#L220) `[VERIFY: eino/adk/chatmodel.go:220]`
- [state rewrite hook](../../eino/adk/handler.go#L160) `[VERIFY: eino/adk/handler.go:160]`

PRD 推导：延迟生效是产品一致性选择；必须同时约束工具定义、实现、权限和 skill 内容的版本，不只延迟调用 `deep.New`。

### E-07：工作流执行与恢复是两层契约

Workflow 基于依赖和字段映射，不支持循环，Compile 返回 Runnable；工具接口可封装调用。Agent 和 ResumableAgent 的契约不同，工作流作为可恢复子 Agent 时要显式适配恢复。

- [Workflow 定义](../../eino/compose/workflow.go#L44) `[VERIFY: eino/compose/workflow.go:44]`
- [Compile](../../eino/compose/workflow.go#L82) `[VERIFY: eino/compose/workflow.go:82]`
- [工具接口](../../eino/components/tool/interface.go#L32) `[VERIFY: eino/components/tool/interface.go:32]`
- [ResumableAgent](../../eino/adk/interface.go#L481) `[VERIFY: eino/adk/interface.go:481]`
- [AgentTool 恢复路径](../../eino/adk/agent_tool.go#L198) `[VERIFY: eino/adk/agent_tool.go:198]`

PRD 推导：一期以声明式定义运行时编译为 Eino 图，再适配为 `WorkflowAgent`；工具名、schema、业务错误、事件和权限仍由产品定义。仅实现 Run 不能据此承诺 HITL 恢复。Eino 可工具封装是源码事实，产品使用方式只保留主 Agent 委派和对话框选择独立 Agent。

- [Coze 执行入口](../../coze-studio/backend/domain/workflow/service/executable_impl.go#L79) `[VERIFY: coze-studio/backend/domain/workflow/service/executable_impl.go:79]`
- [Canvas 转换](../../coze-studio/backend/domain/workflow/internal/canvas/adaptor/to_schema.go#L66) `[VERIFY: coze-studio/backend/domain/workflow/internal/canvas/adaptor/to_schema.go:66]`
- [图装配](../../coze-studio/backend/domain/workflow/internal/compose/workflow.go#L82) `[VERIFY: coze-studio/backend/domain/workflow/internal/compose/workflow.go:82]`

Coze 的转换先调用 PruneIsolatedNodes（to_schema.go:73），该函数会剔除无入边的非开始/结束节点，并对不存在的边目标 panic。[修剪实现](../../coze-studio/backend/domain/workflow/internal/canvas/adaptor/to_schema.go#L396) `[VERIFY: coze-studio/backend/domain/workflow/internal/canvas/adaptor/to_schema.go:396]`。这是上游处理方式，不是本产品的输入校验契约：产品先检查全部原始节点/边和执行语义，再规范化并校验类型、引用、拓扑和本地绑定；孤立未知节点不能通过修剪避开拒绝。

### E-08：Agentic 流错误与缓存写明细

- [AgenticModel 底层接口](../../eino/components/model/interface.go#L36) `[VERIFY: eino/components/model/interface.go:36]` 与 [Recv](../../eino/schema/stream.go#L195) `[VERIFY: eino/schema/stream.go:195]` 均可返回 error；[agenticclaude 建流](../../eino-ext/components/model/agenticclaude/model.go#L359) `[VERIFY: eino-ext/components/model/agenticclaude/model.go:359]` 及事件转换还可能在不同阶段失败。统一语义不需要改变这些签名。
- [内容块合并](../../eino/schema/agentic_message.go#L901) `[VERIFY: eino/schema/agentic_message.go:901]` 按 StreamingMeta.Index 聚合；不能把原始块都当新文本，也不能将已聚合快照再次追加为 delta。
- agenticclaude 的[完整 usage](../../eino-ext/components/model/agenticclaude/convertor.go#L1317) `[VERIFY: eino-ext/components/model/agenticclaude/convertor.go:1317]` 与[流式 usage](../../eino-ext/components/model/agenticclaude/convertor.go#L1285) `[VERIFY: eino-ext/components/model/agenticclaude/convertor.go:1285]` 合并了未缓存/缓存读/缓存写输入，只单列缓存读。当前转换没有单独保留 cacheWrite，旧 claude 的 Message getter 不是本路径的读取 API。

M04 要求在丢失前补采集写入明细，或明确标记未知；不得仅通过 PromptTokens - CachedTokens 推断未缓存输入。这里是源码静态事实和后续验收要求，尚未执行适配测试。

### E-09：动态工具已有框架入口，产品仍需约束选择与恢复

- [TypedChatModelAgentState](../../eino/adk/chatmodel.go#L216) `[VERIFY: eino/adk/chatmodel.go:216]` 包含 ToolInfos / DeferredToolInfos；[工具修改说明](../../eino/adk/chatmodel.go#L386) `[VERIFY: eino/adk/chatmodel.go:386]` 区分 BeforeAgent 调整实际工具与 BeforeModelRewriteState 调整后续模型可见定义。
- [toolsearch.NewTyped](../../eino/adk/middlewares/dynamictool/toolsearch/toolsearch.go#L50) `[VERIFY: eino/adk/middlewares/dynamictool/toolsearch/toolsearch.go:50]` 支持 Agentic 泛型；[模型前选择](../../eino/adk/middlewares/dynamictool/toolsearch/toolsearch.go#L277) `[VERIFY: eino/adk/middlewares/dynamictool/toolsearch/toolsearch.go:277]` 区分原生延迟检索与根据 tool_search 结果开放工具。
- [TypedChatModelAgentMiddleware](../../eino/adk/handler.go#L139) `[VERIFY: eino/adk/handler.go:139]` 提供模型/工具执行 hooks；AfterAgent 只覆盖成功结束，不能用它替代产品所有错误/取消/会话关闭通知。

产品 M05/M11 固定 generation 内实现并记录 Turn/invocation 工具选择；模型可见性不替代执行授权。M07 会话生命周期在 L3 协调，M10 保存选择及扩展状态的恢复关联；不声称 Eino 原生提供这些产品提交保证。

## 安全适配的补充证据

| 已核对事实 | 本产品需要补足的规则 |
| --- | --- |
| pi [prepareToolCall](../../pi/packages/agent/src/agent-loop.ts#L600) `[VERIFY: pi/packages/agent/src/agent-loop.ts:600]` 在规范化/校验后调用 beforeToolCall；[ToolCallEventResult](../../pi/packages/coding-agent/src/core/extensions/types.ts#L1087) `[VERIFY: pi/packages/coding-agent/src/core/extensions/types.ts:1087]` 允许原地修改 input | 参数转换集中在准备阶段，所有转换后最终校验并冻结；授权 hook 只读，强制安全策略不因普通 allow 被跳过 |
| DSH [approval.request](../../deepseek-harness/packages/interaction/user-approval/src/index.ts#L208) `[VERIFY: deepseek-harness/packages/interaction/user-approval/src/index.ts:208]` 在返回决定前记录 asked/decided；[escalation](../../deepseek-harness/packages/sandbox/sandbox/src/escalation.ts#L153) `[VERIFY: deepseek-harness/packages/sandbox/sandbox/src/escalation.ts:153]` 将允许的 mode 返回给该调用 | 产品另行保存许可占用、执行意图与启动/结果事实，跨 checkpoint 恢复不重复使用；这些不是 DSH Promise 自动提供的保证 |
| DSH [bwrap / Landlock profile](../../deepseek-harness/packages/sandbox/sandbox-local/src/profiles.ts#L16) `[VERIFY: deepseek-harness/packages/sandbox/sandbox-local/src/profiles.ts:16]` 分别使用私有 /tmp 与宿主 /tmp；[writableRoots](../../deepseek-harness/packages/sandbox/sandbox/src/roots.ts#L43) `[VERIFY: deepseek-harness/packages/sandbox/sandbox/src/roots.ts:43]` 包含平台临时目录 | 产品明确会话产物根、后端私有临时区和真实路径映射；运行数据写保护与跨 Session 临时范围须按本产品要求调整并验证 |
| DSH [文件写检查](../../deepseek-harness/packages/fs/fs-sandbox/src/index.ts#L122) `[VERIFY: deepseek-harness/packages/fs/fs-sandbox/src/index.ts:122]` 在执行时重新解析目标并检查可写根 | 采用同一真实目标执行，增加可信运行数据布局检查；不将路径检查说成任意 Go 代码隔离或 TOCTOU 已完全消除 |
| DSH Auto [来源识别](../../deepseek-harness/packages/experimental/auto-review/src/index.ts#L175) `[VERIFY: deepseek-harness/packages/experimental/auto-review/src/index.ts:175]` 使用接入来源与直接父会话身份；[分类](../../deepseek-harness/packages/experimental/auto-review/src/index.ts#L222) `[VERIFY: deepseek-harness/packages/experimental/auto-review/src/index.ts:222]` 区分指令与事实 | 以产品 SDK/HTTP 受信身份、不可变原始受理输入和委派记录映射；派生 user 文本、导入 source 字段、摘要不能授予许可 |

M12 是上述安全语义的唯一规格，M05/M06/M07/M10 分别落实执行、来源、事件和保存。冻结描述、运行数据保护、会话产物布局及占用恢复是产品增强，尚未运行平台沙箱或故障注入测试；不据静态源码宣称安全认证。

## 原方案需要修订或补足的地方

原方案保留为前序方向记录，并已在文首标明最新范围与分层以 PRD 为准。用户本次明确的前端接口、Web 验证和下层不依赖上层原则已同步；其他源码修订建议的细节仍按章节讨论，不将设计建议冒充已确认决定。

| 原表述/缺口 | 本轮核查 | 新 PRD 的处理 |
| --- | --- | --- |
| 一个 Session 一个长期 TurnLoop，同时 Abort 用 Stop | Stop 后同一实例不可再次启动；E-04 | 改为同时最多一个活动实例，Session 生命周期可跨多个实例 |
| AgenticMessage 取消/重试未接线 | 注释与实际装配/测试不一致；E-05 | 按用户确认采用 AgenticMessage；更新主/子 Agent、模型和事件契约，后续验证兼容性 |
| reload 失败保留旧 Agent | PrepareAgent 报错会退出；E-03 | 候选构建及旧版保留发生在新独立输入受理前；受理后 PrepareAgent 只重建固定版本，失败明确报告，不静默换版 |
| 仅延迟 deep.New 即可冻结旧能力 | skill 调用时可能读取新内容；E-06 | 任务绑定能力及内容版本 |
| 所有 PrepareAgent 都可以热换 | 恢复也经过同一入口；E-03 | 恢复原任务必须验证原扩展版本/拓扑 |
| steering 只在工具结果后消费 | pi 的消费边界更广；P-06 | M03 讨论初次进入、无工具终答附近和子 Agent 作用域 |
| `/workflow` 可独立 Invoke 并先 ensureAgentFresh | 可能绕过唯一换版本入口和同 Session 串行约束 | 旧 `/workflow` 退出当前产品入口；独立执行经 SubmitInput 指定目标 Agent，仍由 AgentSession 串行协调 |
| 同进程插件可加载/卸载 | 不等于 Go 动态源码加载 | 一期明确为已编译注册项启停和资源重载 |
| 原方案以 CLI/REPL 为主要入口 | 用户明确只定义前端接入能力，先用 Web 页面测试 | 提供公开操作和事件接口；页面仅用于验证，CLI 非首期必交付项 |
| 包列表未明确依赖方向，加载器示例直接调用旧 Host | 用户要求参考 pi 分层，下层不依赖上层 | ResourceLoader 读取候选、ExtensionRegistry 登记、AgentSession 协调激活，职责及依赖明确 |
| 旧 Host/AgentHost 混合组装、控制、存储和注册 | 用户要求与 pi 名称及职责对齐；P-02、P-08 | 拆为 CreateAgentSession、AgentSession、Agent、SessionManager、ResourceLoader，新增注册契约单独标注 |

## 完整章节新增的关键核查

| 新发现 | 详细依据所在章节 | 需要的产品契约 |
| --- | --- | --- |
| pi 本地对截断响应中的工具调用不直接执行 | [M03](03-agent-loop.md) | 不因 JSON 部分可解析就派发工具 |
| Eino InferTool 的解码不是完整 JSON Schema 校验，ToolsNode 普通错误也不自动等价于模型反馈 | [M05](05-tool-system.md) | 校验、错误分类与策略由产品补足 |
| pi 产品通知和落盘顺序不等于提交后发布 | [M07](07-events-and-access.md) | durable 事件是本产品的更强保证 |
| Eino AgentsMD 注释与 state 回写路径有差异 | [M08](08-context-engineering.md) | 规范注入、摘要和用户历史需组合验证 |
| pi read 保留完整头部行、bash 保留尾部片段、grep 独立裁剪单行；续读提示进入正文 | [M08 截断](08-context-engineering.md#42-工具输出截断说明与续读) | 不能只裁总字节或只在 details 记录截断；不照搬依赖 bash 的超长行回退 |
| pi system prompt 有基础替换/追加/资源顺序，skill 按实际加载能力进入索引；分支摘要只取旧路径独有后缀 | [M08 组装与摘要](08-context-engineering.md#43-系统提示词的组装结构) | 复用明确来源及按需加载；不将 Current date、编码人设、固定 2048 token 当通用强制规则 |
| Eino 默认摘要不自动保留 pi 式近期原始历史 | [M09](09-compaction.md) | 保留区、摘要来源及投影提交单独定义 |
| Eino checkpoint 不是任意崩溃的最新任务快照 | [M10](10-session-persistence.md) | paused/recovery_required、工具副作用核对和兼容恢复 |
| DeepAgent task 的委派输入是描述字符串 | [补充篇](11-extensions-workflows-access.md) | 工作流子 Agent 仍需显式参数转换与校验 |
| pi 的模型层还承担统一响应、供应商前缀缓存与缓存用量归一化；eino-ext 已有部分能力但语义不全相同 | [M04 响应与缓存](04-model-access.md#44-对齐-pi-的统一模型响应语义) | L1 补缺失适配，M08 保持稳定前缀；有状态续接与缓存分别选择 |
| pi 工具为描述/执行/应用三种角色，规范化、校验、控制与结果后处理分开，底层 Operations 可替换 | [M05 方法论](05-tool-system.md#13-从-pi-提炼的方法论) | 不复制 TUI、宽泛强转或“任何错误永不传播”的教学概括；保留执行事实 |
| DSH 审批失败默认不放行，沙箱按调用解析，文件模式与实际 full/partial 分开 | [安全补充篇](12-security-sandbox.md) | 继承词汇与职责方法，映射 Eino 等待/恢复及产品提交；不把文件写限制称作网络/读取隔离 |
| DSH Auto 是默认关闭的实验能力，allow 后 Full access，未覆盖任意进程内操作 | [Auto 对照](12-security-sandbox.md#24-自动安全审核独立可选能力) | 不宣称自动审核就是 OS 沙箱；产品默认关闭，依赖审核时失效暂停，不自动降级 Full access；实际能力另行验证 |
| Zero 已有 Linux helper+bwrap、macOS Seatbelt、Windows token/ACL/WFP，默认可 degraded 直接执行 | [安全补充篇 4.2](12-security-sandbox.md#42-平台矩阵) | 抽取移植底层实现；拒绝 auto degraded；Job/控制通道和运行数据保护按产品契约补足并逐平台认证 |

## 本轮核查的限度

已完成全部章节的关键定义、调用路径和部分测试源码静态核对；没有以此代替后续行为测试。模型兼容性、实际恢复语义、资源冻结和持久化一致性仍需实现验证；全套 PRD 的待定项及验证责任见[系统闭合检查](system-review.md)。初稿核对时未编写产品实现或开发方案。

2026-09-24 文档调整：补充 Coze Canvas 格式语义与 Zero 三平台沙箱源码事实，并同步开发方案。本文件仍只记录静态核对，不表示工作流兼容或三平台沙箱已经实现。
