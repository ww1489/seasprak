# 章节路线与决策清单

状态：2026-10-02，按当前源码同步第 1～12 章的三层、同级双产品决定；Code Agent 与共享存储位于 `internal/codeagent`、`internal/storage`，独立 `internal/workflowagent`、公开工厂、Web 三类资源路由及独立视图已接线，有对应默认测试和 SDK 消费者。2026-09-23 的章节评审及先需求、后方案顺序保留为历史；最终认证见 [P3 验证记录](../p3-verification.md)，不据目录、文档或局部测试声明整体 P3 通过，也不启动 P4/P5。以[源码学习十章](https://dg-ai-notes.pages.dev/modules/)为主线；Eino [Quickstart](https://www.cloudwego.io/zh/docs/eino/quick_start/)是能力参考。

## 推进规则

每章依次完成：阅读教程 → 对照本地 pi 调用路径 → 核对 Eino 能力 → 提炼产品需求 → 写边界及异常场景 → 给出验收条件 → 讨论未决点。只把用户明确选定的事项标为“已确认”。

状态定义：**草案已成稿**表示已有需求、来源、边界和验收；**已确认**仅指用户明确选择的事项；**已实现/已验证运行**需有本项目代码与测试证据。第 1～12 章具备规格，开发方案已形成；2026-10-01 的架构决定是本次正文修订依据，交付状态仍按当前源码与本项目验证判断，不将文档确认等同于迁移完成、P3 完成或代码已可运行。

## 十章覆盖表

| 教程章节 | PRD 需要回答的问题 | pi 研究入口（相对工作区） | Eino 对照方向 | 交付与状态 |
| --- | --- | --- | --- | --- |
| [M01 开篇](https://dg-ai-notes.pages.dev/modules/ch01-overview) | 通用底座为谁服务？“极简”和“可扩展”怎样验收？ | `pi/packages/*/package.json`、`pi/packages/agent/src/index.ts` | DeepAgent 配置与内置行为 | [定位与原则](01-product-foundation.md)，草案 |
| [M02 三层架构](https://dg-ai-notes.pages.dev/modules/ch02-three-layer-arch) | 模型、通用 Agent、同级 Code/Workflow 产品如何组合？各自状态与中立存储怎样隔离？ | `pi/packages/agent/src/types.ts`、`pi/packages/coding-agent/src/core/sdk.ts` | `adk/interface.go`、`prebuilt/deep/deep.go`、`turn_loop.go` | [分层与边界](02-architecture-boundaries.md)，三层双产品目标已确认，Code Agent 与共享存储已迁移，独立 Workflow Agent、工厂、Web 三类资源路由及独立视图已接通，有默认测试；最终认证见验证记录 |
| [M03 Agent Loop](https://dg-ai-notes.pages.dev/modules/ch03-agent-loop) | Code 的 Session → Trace → Turn 如何组织？steering、外层 follow-up 和统一收尾如何衔接？Workflow 如何保留独立节点生命周期？ | pi agent-loop、pigo 流程图及 loop 源码 | Agentic DeepAgent 内层、TurnLoop 调度、轮后适配 | [执行循环](03-agent-loop.md)，保留 Code 双层循环，Workflow 不入 AgentSession 队列 |
| [M04 模型调用](https://dg-ai-notes.pages.dev/modules/ch04-model-call) | 如何统一调用入口、思考映射、流协议、缓存意图与错误识别？新增模型如何不改 Loop？ | pi 的协议、ThinkingLevel 映射、流与 overflow | AgenticModel、eino-ext 适配及必要归一化 | [模型接入](04-model-access.md)，已按文章主题及设计精华补齐契约 |
| [M05 工具系统](https://dg-ai-notes.pages.dev/modules/ch05-tools) | 参数校验、执行、权限、取消、并发、结果及业务组合调用如何遵循相同契约且保持两类独立所有权？ | `pi/packages/agent/src/agent-loop.ts`、coding-agent tools/extensions | Tool、ToolsNode、文件 backend、Eino Graph/恢复；WorkflowAgent 是本产品批准目标而非上游既有产品 API | [工具系统](05-tool-system.md)，草案已成稿 |
| [M06 消息系统](https://dg-ai-notes.pages.dev/modules/ch06-messages) | 三类标准消息与内部扩展如何分工？上下文处理和模型转换如何分开？ | `pi/packages/agent/src/types.ts`、coding-agent 消息转换 | `*schema.AgenticMessage`、TypedAgentInput、内容块 | [消息契约](06-messages.md)，已按 review 收敛 |
| [M07 事件驱动](https://dg-ai-notes.pages.dev/modules/ch07-event-driven) | 客户端订阅什么？事件顺序、错误和审批如何表示？ | AgentEvent、Agent 订阅、extension 事件 | AgentEvent、OnAgentEvents、Callback/诊断 | [事件和接入](07-events-and-access.md)，草案已成稿 |
| [M08 上下文工程](https://dg-ai-notes.pages.dev/modules/ch08-context-engineering) | 已消费输入、工具预览、指令和按需 skill 如何组成模型输入？压缩与分支摘要如何互补？ | system-prompt、skills、resource-loader、truncate、branch-summarization | Skill、AgentsMD、Reduction 与 Agentic 消息投影 | [上下文工程](08-context-engineering.md)，已补截断/续读、组装结构与四项机制契约 |
| [M09 上下文压缩](https://dg-ai-notes.pages.dev/modules/ch09-compaction) | 怎样选择合法切点，生成首次/增量/前缀摘要，并保留可验证的文件记录？ | compaction、utils 文件集合、SessionManager 重建 | Agentic Summarization 输入/Finalize 与产品提交 | [压缩](09-compaction.md)，已补摘要策略、文件事实累积和重建验收 |
| [M10 会话管理](https://dg-ai-notes.pages.dev/modules/ch10-session) | Entry 如何分类，回退/追加怎样形成分支，如何重建历史配置并兼容 JSONL？Checkpoint 如何关联？ | coding-agent SessionManager、路径状态提取；harness/session 为演进参考 | CheckPointStore、TurnLoop 恢复、产品 JSONL Store | [会话持久化](10-session-persistence.md)，已补 Entry 分类、分支操作、配置重建、版本与事件去重边界 |

各章正文包含实际文件/符号/行号证据，区分框架事实与本产品建议；所有完整执行轨迹在[系统闭合检查](system-review.md)汇合。

## 横跨章节的必要补充

用户要求的 skills / 插件 / 声明式工作流 / 延迟热换并不全部对应独立教程章节，已集中于[扩展、工作流与接入补充篇](11-extensions-workflows-access.md)，并引用各章的唯一契约：

| 主题 | 所需前置章节 | 必须形成的决策 |
| --- | --- | --- |
| 扩展与 ExtensionRegistry | M02、M05、M06、M07、M09、M10 | 能力登记/显式替换、固定版本内工具选择、受控运行操作、私有状态和会话生命周期；沿用 Go 包加载方式 |
| skills 和资源重载 | M05、M08 | 搜索路径、覆盖规则、可信来源、按需加载与生效时机 |
| 工作流与子 Agent | M03、M05、M07、M10 | 普通 Code 内部受控委派与父 Trace 约束；同级 Workflow 的定义/编译/静态子流程/节点审批恢复；业务直接调用或已有工具组合。Coze/动态定义仍属未来阶段 |
| 热换与中断恢复 | M03、M08、M10 | 运行版本固定、构建失败回退、恢复兼容、队列归属 |
| 前端接入契约与 Web 验证 | M02、M03、M06、M07、M10 | HTTP 操作、SSE 事件、断连/状态查询、A2UI 按需适配；不设计正式前端 |
| 安全审核、审批审计与沙箱 | M02、M03、M05、M07、M08、M10 | [独立补充篇](12-security-sandbox.md)：DSH 式按调用策略、单次许可、审计；Zero 三平台原生沙箱适配与失败处理 |

同一个规则只在一个主文档定义，其余引用它；避免十章各写一套不一致的状态机。

## 待讨论决策

| 编号 | 状态 | 问题/当前建议 | 最晚讨论章节 |
| --- | --- | --- | --- |
| D-01 | 已确认（本次用户回复） | 通用底座，编码助手为首个验证场景 | M01 |
| D-02 | 已确认（用户澄清） | 底座留前端接入接口，前端实现不在产品范围内；先用最小 Web 页面测试；CLI 不作为首期必交付项 | M01；契约在 M06～M07 细化 |
| D-03 | 已确认（用户本轮要求） | 先完成所有章节与系统拼图，再制定开发方案；不逐章提前进入实现 | 全系统 |
| D-04 | 继承原方案 | DeepAgent、独立 Go module、不改 pigo、开放产品消息 | M02 记录，不重复选型 |
| D-05 | 工程选型已细化，实际组合待认证 | 模型参考 pi；Windows 原生、Linux、macOS 均需要；主程序在宿主机运行，shell 可选 Docker 沙箱。首批协议/后端在开发方案 01/04/11 固定，各 endpoint/模型和 OS/backend 组合按 12 认证 | M04～M05；开发方案 |
| D-06 | 已确认，2026-09-24 补充实现来源 | 策略/审批/审核按 DeepSeek Harness 方法；原生沙箱按 Zero 适配；默认 workspace-write + ask、Auto 关闭；读取/网络与文件写限制分开，partial 的接纳按 D-19；拒绝 Zero auto degraded | M05 / 安全补充篇 |
| D-07 | 已确认（本轮用户明确修正） | Code Agent 默认装配文件读写/搜索、命令执行、TODO 和 general-purpose 子 Agent；基础提示词描述实际能力，应用可覆盖并显式裁剪/替换工具，缺少默认必需后端不静默降配。Workflow 按自身定义和策略装配，不继承 Code 默认能力 | M01/M02/M05/M08/M11 |
| D-08 | 完整需求已覆盖，阶段后定 | steering、会话分叉、压缩、工作流恢复均已成稿；首版分期属于系统评审后的开发方案 | 全系统 |
| D-09 | 当前沿用，后续独立评估 | 当前保留已编译 Go 包静态登记；资源重载、运行时启停及加载全新代码不在本轮范围，不作为当前交付能力 | 插件补充篇 |
| D-10 | 已确认 | 取消完成后保留原输入/队列并暂停当时旧独立队列的自动启动；无冲突时新 prompt 可正常开始，旧队列显式继续。终态 Trace 的未消费输入重做需新 inputId/traceId；HITL/generation 规则已由 M03/M10 明确 | M03/M07/M10 |
| D-11 | 已确认（2026-10-01 架构决定） | L1 internal/llm；L2 internal/agent（含 eino/tools）；L3 同级 internal/codeagent（由 internal/sessions 迁移）与 internal/workflowagent，均依赖 L2/L1 及中立 internal/storage，不互相 import，不反向依赖；Code Agent 与共享存储已迁移，独立 Workflow Agent、工厂、Web 三类资源路由及独立视图已接通，有默认测试；最终认证见验证记录 | M02 |
| D-12 | 接入方向及路由域已明确，协议细节留方案 | 内部 Web 经受控 L3 接口，不 import sdk、不直接操作管理器/Store；外部消费者只 import github.com/ww1489/seasprak/sdk。Code `/v1/sessions`、Workflow 定义 `/v1/workflows`、Workflow 运行 `/v1/workflow-runs` 分域；Workflow 目标能力已接线且有对应默认测试；最终认证见验证记录。互调用已有受控工具接口由业务组合；A2UI 为可选外层映射，不替代被调方校验/授权/恢复；具体子路径/DTO/版本在方案定义 | M06～M07 / 接入补充篇 |
| D-13 | 初始工程默认值已确定，效果待测量 | 模型/工具/摘要预算、重试、执行时限、事件窗口与资源保留以[开发方案 12](../pi-eino-dev-plan/12-delivery-and-validation.md#defaults)为主定义；普通 Code 子调用仍归父 Trace，Workflow 自己记账，跨两类不共享隐式总额；终端用户无需配置，开发者可在代码中调整，不作为延迟/成功率承诺 | M04～M11；开发方案 |
| D-14 | 行为已明确，适配待验证 | 终答附近已接纳的 steering/follow-up 在同 Trace 继续；特殊未知消息缺规则时阻断；手动压缩空闲受理；分叉不回滚文件；不再重复确认已写明行为 | M03、M06、M09～M10 |
| D-15 | 已确认（2026-10-01 明确范围） | 普通 Code 子 Agent 采用开发者代码注册：自行创建实例，通过 ExtensionRegistry.AddSubAgent 登记，再交创建入口装配；保留 general-purpose/专家委派及父 Trace 权限/预算/generation/取消约束，不以此注册 Workflow；不要求终端用户配置，具体 Go 签名后定 | M03 / 扩展补充篇 |
| D-16 | 已确认（2026-10-01 双产品职责） | Code 保留 Agent、AgentSession、CreateAgentSession/OpenAgentSession 及其 SessionManager/ResourceLoader/ExtensionRegistry；Workflow 独立定义/编译/Graph/节点状态/生命周期，WorkflowAgent/WorkflowOptions/CreateWorkflowAgent/OpenWorkflowAgent 已公开接线，有对应默认测试及消费者，最终认证见验证记录。唯一公开 import github.com/ww1489/seasprak/sdk，唯一生产入口 sdk/sdk.go，同 module 不新公开子包；cmd/agentd 仅 help/version，不 import sdk/internal；共用 storage 不共享日志写入者、历史、审批、恢复、generation/预算，旧开发数据不迁移不自动删 | 全文；M02 为术语来源 |
| D-17 | 设计已补充，逐模型映射待认证 | L1 统一模型调用、ThinkingLevel、缓存意图、错误与溢出识别；流成功/失败具有唯一收尾。M08 使用生效参数预算并保持前缀稳定；缓存、显式资源和有状态续接分别定义，默认由代码策略提供 | M04、M06～M09 |
| D-18 | 已确认（用户指定参考） | 工具吸收 pi 方法论：分层定义、五步处理、错误反馈、content/details、Operations 及并发归并；安全/审计参考本地官方 DSH，原生沙箱参考 Zero | M05 / 安全补充篇 |
| D-19 | 产品规则已确认，平台认证待验证 | partial 满足运行数据保护等必需条件后可用并明确展示限制，否则拒绝受限配置；Auto 默认关闭，依赖审核时能力失效暂停而不静默放宽；OS/容器后端与实际强制能力进入技术验证 | 安全补充篇 |
| D-20 | 已确认（本轮用户指定） | 使用 *schema.AgenticMessage / AgenticModel；按 pi 三类标准职责、内部 AgentMessage 与两阶段转换收敛消息设计，普通自定义消息不强制专用 codec | M02、M04、M06、M08、M11 |
| D-21 | 已确认（2026-10-01 明确范围） | Code Trace 表示完整运行/回复过程，Turn 表示一次模型调用及工具执行；同 Trace 包含 steering/follow-up，内部 agent_start/agent_end 按执行段区分，产品 trace.settled 完整收尾；观测 span 不冒用 Trace。Workflow 保留自己的运行/节点身份与生命周期，不伪造 Code Session/Trace/Turn 或继承其恢复范围 | M02～M11 |
| D-22 | 已确认本轮保留范围，未来动态能力另行验证 | 保留固定实现内的逐 Turn 工具选择、扩展运行操作和会话生命周期 hooks；独立动态插件加载、运行时启停、资源热重载及多 generation 不在本轮，静态已编译 Go 包登记保留，具体接口和适配留后续验证 | M03～M11 |
| D-23 | 已按当前审批契约统一，执行安全仍需验证 | 参考 pi/DSH 保留最终校验/冻结、运行数据保护、产物路径、占用/恢复和 Auto 原文来源；本产品问答、响应去重、有效期及消费仅存当前实例内存，关闭/重开失效，不采用 DSH 持久 decided。原冻结调用、执行意图/占用、预算、真实启动/结果与恢复定位持久保存；显式 Resume 复核策略，未占用且需审批时重新问，已占用未知先核对，已有结果复用；Auto 最小决定元数据独立保留，平台组合仍按 D-19 验证 | M05～M12 |
| D-24 | 已确认并完成原评审收口 | Code 的 AgentSession 必须有工作区；共同祖先上的有效压缩摘要可继承；Code/Workflow 各自在受控核对追加证据后计算自己的恢复资格，不能重跑副作用、转移票据或复活终态请求 | M01/M02/M05/M07/M08/M09/M10 |
| D-25 | 已确认（2026-09-24 开发方案） | Windows 采用 Go 主程序＋Go 辅助 exe；主程序直接在宿主机运行，容器仅作为可选 shell 沙箱。文件工具和容器 shell 经受信挂载/路径映射操作同一真实工作区及产物；不把容器路径用于宿主同名文件 | M05/M12；开发方案 01/11 |
| D-26 | 已确认（2026-10-01 替代 2026-09-24 入口决定） | 同级 Code/Workflow 分别公开调用，Workflow 不内置为 Code 子 Agent、不经 AgentSession/targetAgent。业务可用已有受控工具互调，旧 workflow-as-tool 禁令取消，被调方独立校验/授权/冻结/票据/预算/版本/恢复；不新增 SDK 专用跨 Agent 框架或共享总额/整树恢复。静态子流程、条件/并行汇合、已有节点暂停审批显式恢复保留；高级跨任务编排/审批/补偿、业务幂等、完整持久子树恢复、跨运行总预算归业务。Coze、动态加载/热重载/补参仍属原未来阶段，不启动 P4/P5 | M03/M05/M07/M11 |
| D-27 | 已确认（2026-09-24 用户调整） | 原生三平台沙箱以 Zero 已有实现为主要移植来源；产品一次许可、拒绝降级、运行数据保护和 Windows Job/控制通道仍按本产品契约适配并认证 | M12；开发方案 11 |

前端范围、Web 测试方式、依赖方向、全环境、工具方法和 DSH/Zero 安全参考路线已明确。后续集中制定工程参数、实现办法及适配认证矩阵；参考一个安全方案不等于授权当前任务运行任何待开发的权限模式。

## 每章完成标准

- 至少一个用户故事、一条正常轨迹、一条异常或边界轨迹。
- 能明确指出 Eino 复用部分与产品需要新增部分。
- 代码事实有文件、符号及行号；设计建议明确标注，不伪装为上游能力。
- 需求能写成通过/失败条件；未知事项有编号和归属。
- 与已确认基线冲突时，说明证据和建议变更，不静默改变决策。
- 新增能力应说明所属层级；需要前端交互时给出操作/事件契约，通过最小 Web 页面验收，不扩展为正式前端设计。

## 本轮验证记录

前序章节研究已阅读原方案、教程十章及对应关键本地源码，形成正常/异常/边界行为、源码依据和验收定义。2026-10-01 本次只重读和检索 PRD/历史文档并修订架构推导，核对术语、图表、编号、链接和跨章归属；没有重新读取源码或运行测试、构建、安装、真实模型，也没有提交。批准目标的交付和 P3 状态须另按本项目实现与验证证据判断；已确认的 `checkpoint_validation` 全局 callback 缺陷仍阻塞 P3，责任见 [V-02](system-review.md#61-后续技术验证清单)。审批持久化的前轮跨章文字冲突，本次已按当前实例内存契约统一：一次问答/响应去重/有效期/消费不跨重开，checkpoint 只定位原冻结调用；持久执行意图/占用、预算、真实结果与核对仍保留，Auto 最小决定元数据独立保留，DSH 持久决定不作为产品许可。收口见 [系统检查](system-review.md#6-已闭合的设计冲突与剩余问题)，文档统一不代替产品验证。
