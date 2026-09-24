# 章节路线与决策清单

状态：2026-09-23，第 1～12 章已完成本轮收口及默认行为确认。用户明确：先完成全部章节和系统拼图，再制定开发方案。以[源码学习十章](https://dg-ai-notes.pages.dev/modules/)为主线；Eino [Quickstart](https://www.cloudwego.io/zh/docs/eino/quick_start/)是能力参考。

## 推进规则

每章依次完成：阅读教程 → 对照本地 pi 调用路径 → 核对 Eino 能力 → 提炼产品需求 → 写边界及异常场景 → 给出验收条件 → 讨论未决点。只把用户明确选定的事项标为“已确认”。

状态定义：**草案已成稿**表示已有需求、来源、边界和验收；**已确认**仅指用户明确选择的事项；**已实现/已验证运行**需有代码与测试证据。当前第 1～12 章具备完整规格，本轮评审中的收口项及产品默认行为已按用户决定落实；尚未实现产品代码。下一步可制定含验证办法的开发方案，不将文档确认等同于代码已可运行。

## 十章覆盖表

| 教程章节 | PRD 需要回答的问题 | pi 研究入口（相对工作区） | Eino 对照方向 | 交付与状态 |
| --- | --- | --- | --- | --- |
| [M01 开篇](https://dg-ai-notes.pages.dev/modules/ch01-overview) | 通用底座为谁服务？“极简”和“可扩展”怎样验收？ | `pi/packages/*/package.json`、`pi/packages/agent/src/index.ts` | DeepAgent 配置与内置行为 | [定位与原则](01-product-foundation.md)，草案 |
| [M02 三层架构](https://dg-ai-notes.pages.dev/modules/ch02-three-layer-arch) | 模型、Agent、AgentSession 如何递进？SessionManager/ResourceLoader 与注册职责如何区分？ | `pi/packages/agent/src/types.ts`、`pi/packages/coding-agent/src/core/sdk.ts` | `adk/interface.go`、`prebuilt/deep/deep.go`、`turn_loop.go` | [分层与边界](02-architecture-boundaries.md)，名称和单向依赖原则已确认，细节为草案 |
| [M03 Agent Loop](https://dg-ai-notes.pages.dev/modules/ch03-agent-loop) | Session → Trace → Turn 如何组织？steering、外层 follow-up 和统一收尾如何衔接？ | pi agent-loop、pigo 流程图及 loop 源码 | Agentic DeepAgent 内层、TurnLoop 调度、轮后适配 | [执行循环](03-agent-loop.md)，已按双层循环修订 |
| [M04 模型调用](https://dg-ai-notes.pages.dev/modules/ch04-model-call) | 如何统一调用入口、思考映射、流协议、缓存意图与错误识别？新增模型如何不改 Loop？ | pi 的协议、ThinkingLevel 映射、流与 overflow | AgenticModel、eino-ext 适配及必要归一化 | [模型接入](04-model-access.md)，已按文章主题及设计精华补齐契约 |
| [M05 工具系统](https://dg-ai-notes.pages.dev/modules/ch05-tools) | 参数校验、执行、权限、取消、并发、结果及工作流调用如何统一？ | `pi/packages/agent/src/agent-loop.ts`、coding-agent tools/extensions | Tool、ToolsNode、文件 backend、WorkflowAgent | [工具系统](05-tool-system.md)，草案已成稿 |
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
| 工作流与子 Agent | M03、M05、M07、M10 | 动态声明式定义、主 Agent 委派与对话框选择两种使用方式、输入校验、父子任务、HITL 与副作用 |
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
| D-07 | 已确认（本轮用户明确修正） | 默认装配文件读写/搜索、命令执行、TODO 和 general-purpose 子 Agent；基础提示词描述实际能力，应用可覆盖并显式裁剪/替换工具，缺少默认必需后端不静默降配 | M01/M02/M05/M08/M11 |
| D-08 | 完整需求已覆盖，阶段后定 | steering、会话分叉、压缩、工作流恢复均已成稿；首版分期属于系统评审后的开发方案 | 全系统 |
| D-09 | 当前沿用，后续独立评估 | 当前采用已编译 Go 包登记与资源重载；运行时加载全新代码不在本轮范围，不作为当前定稿的待答问题 | 插件补充篇 |
| D-10 | 已确认 | 取消完成后保留原输入/队列并暂停当时旧独立队列的自动启动；无冲突时新 prompt 可正常开始，旧队列显式继续。终态 Trace 的未消费输入重做需新 inputId/traceId；HITL/generation 规则已由 M03/M10 明确 | M03/M07/M10 |
| D-11 | 已确认（用户澄清） | 参考 pi，层级职责清晰、下层不依赖上层、逐层组合；三层映射及接口注入细节见 M02 | M02 |
| D-12 | 接入方向已明确，协议细节留方案 | SDK/HTTP 操作 + SSE 事件复用 AgentSession 支撑 Web 验证；A2UI 为可选外层映射，未要求完整实现时不阻塞底座；具体 DTO/路由/版本在方案定义 | M06～M07 / 接入补充篇 |
| D-13 | 初始工程默认值已确定，效果待测量 | 模型/工具/摘要预算、重试、执行时限、事件窗口与资源保留以[开发方案 12](../pi-eino-dev-plan/12-delivery-and-validation.md#defaults)为主定义；终端用户无需配置，开发者可在代码中调整，不作为延迟/成功率承诺 | M04～M11；开发方案 |
| D-14 | 行为已明确，适配待验证 | 终答附近已接纳的 steering/follow-up 在同 Trace 继续；特殊未知消息缺规则时阻断；手动压缩空闲受理；分叉不回滚文件；不再重复确认已写明行为 | M03、M06、M09～M10 |
| D-15 | 已确认（用户澄清） | 子 Agent 采用开发者代码注册：自行创建实例，通过 ExtensionRegistry.AddSubAgent 登记，再交创建入口装配；不要求终端用户配置，具体 Go 签名后定 | M03 / 扩展补充篇 |
| D-16 | 已确认（用户明确确认职责拆分） | HTTP/SSE 调用 AgentSession；AgentSession 通过 Agent 执行、通过 SessionManager 保存历史；ResourceLoader 加载资源，ExtensionRegistry.AddSubAgent 单独注册，CreateAgentSession 负责装配；新增契约单独标明 | 全文；M02 为术语来源 |
| D-17 | 设计已补充，逐模型映射待认证 | L1 统一模型调用、ThinkingLevel、缓存意图、错误与溢出识别；流成功/失败具有唯一收尾。M08 使用生效参数预算并保持前缀稳定；缓存、显式资源和有状态续接分别定义，默认由代码策略提供 | M04、M06～M09 |
| D-18 | 已确认（用户指定参考） | 工具吸收 pi 方法论：分层定义、五步处理、错误反馈、content/details、Operations 及并发归并；安全/审计参考本地官方 DSH，原生沙箱参考 Zero | M05 / 安全补充篇 |
| D-19 | 产品规则已确认，平台认证待验证 | partial 满足运行数据保护等必需条件后可用并明确展示限制，否则拒绝受限配置；Auto 默认关闭，依赖审核时能力失效暂停而不静默放宽；OS/容器后端与实际强制能力进入技术验证 | 安全补充篇 |
| D-20 | 已确认（本轮用户指定） | 使用 *schema.AgenticMessage / AgenticModel；按 pi 三类标准职责、内部 AgentMessage 与两阶段转换收敛消息设计，普通自定义消息不强制专用 codec | M02、M04、M06、M08、M11 |
| D-21 | 已确认（用户纠正并要求落实） | Trace 表示完整运行/回复过程，Turn 表示一次模型调用及工具执行；同 Trace 包含 steering/follow-up，内部 agent_start/agent_end 按执行段区分，产品 trace.settled 完整收尾；观测 span 不冒用 Trace 语义 | M02～M11 |
| D-22 | 已确认本轮完善范围 | 完善动态工具管理、扩展运行操作、会话生命周期 hooks；固定实现版本与逐 Turn 工具选择分开。独立插件加载机制不在本轮范围，沿用已编译 Go 包，具体接口和适配留后续验证 | M03～M11 |
| D-23 | 已按用户要求完善规格，实现待验证 | 参考 pi/DSH 补齐参数转换后最终校验与冻结、受限模式运行数据保护、会话产物路径、一次许可占用/恢复、Auto 原始授权来源；与框架既有能力的差异显式标注，平台组合仍按 D-19 验证 | M05～M12 |
| D-24 | 已确认并完成本轮收口 | 产品 Session 必须有工作区；共同祖先上的有效压缩摘要可继承；受控核对追加证据后计算恢复资格，不能重跑副作用或复活终态请求 | M01/M02/M05/M07/M08/M09/M10 |
| D-25 | 已确认（2026-09-24 开发方案） | Windows 采用 Go 主程序＋Go 辅助 exe；主程序直接在宿主机运行，容器仅作为可选 shell 沙箱。文件工具和容器 shell 经受信挂载/路径映射操作同一真实工作区及产物；不把容器路径用于宿主同名文件 | M05/M12；开发方案 01/11 |
| D-26 | 已确认（2026-09-24 用户调整） | 工作流为声明式动态运行能力，先兼容指定 Coze Canvas 子集，后续可接其他来源和自有编排。对外只保留主 Agent 子委派和对话框选择独立 Agent；沿用当前 Session，独立输入受理时固定目标与 generation；queued/hold、执行及恢复都不换版，定向输入继承原目标且错配拒绝 | M03/M05/M07/M11 |
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

已阅读原方案、教程十章及各章对应关键本地源码；完成正常/异常/边界行为、源码依据和验收定义，并统一跨章身份、状态和链接。未实现产品代码，未运行真实模型或上游全量测试。文档交付前检查本地链接、源码引用位置、十章覆盖与系统场景归属。
