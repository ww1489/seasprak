# 第 4 章：模型接入、能力与失败边界

状态：按 M04 文章小标题及设计精华复核后的 PRD 讨论稿。对应教程 [M04 模型调用](https://dg-ai-notes.pages.dev/modules/ch04-model-call)。用户已明确模型接入方向“参考 pi”，因此本章按多供应商、多协议及可替换模型设计；具体首批认证矩阵待确认。本章不制定实现步骤、开发排期或上线承诺。

## 1. 执行摘要

### 1.1 问题、方案与成功标准

通用底座需要在更换模型后保留相同的任务、工具与事件语义，同时承认不同模型在工具调用、上下文窗口、图片、推理参数和计量上的差异。只把 endpoint 与 model ID 改成配置，无法保证这些行为兼容。

拟定方案：L1 基于 Eino/eino-ext 实现统一模型请求/响应、供应商兼容和前缀缓存策略；L2 的 Agent 负责模型调用、重试边界和工具派发，消费 L1 已归一化的结果；L3 的应用装配方提供模型配置、凭据和默认策略，AgentSession 确定 Trace 模型基线及逐 Turn 选择策略，SessionManager 保存记录。**eino-ext 是复用的实现基础，pi 的统一响应和供应商优化语义仍是产品需要验收的能力。**

建议验收门槛：

- 两个不同协议的假模型适配器完成相同的文本、工具往返和取消用例，运行核心无需按供应商分支。
- 所有已启用模型都有可查询的能力记录；不支持的必需能力在发送模型请求前失败。
- 重试轨迹中，已完成的工具调用执行次数保持为 1；每次模型尝试均可追溯。
- 凭据缺失、鉴权失败、流中断、上下文超限及额度耗尽均产生可区分的结果，不留下永远运行中的请求。
- 支持的供应商缓存路径有请求证据与缓存读写计量；连续模型步骤保持合法稳定前缀，缓存不可用时不丢历史、不重放工具。

### 1.2 教程、pi 与 Eino 的依据

教程强调统一调用入口、供应商翻译与统一流式结果。这里借鉴其职责划分，行为结论以本地代码为准。

| 已验证事实 | 本产品取舍 |
| --- | --- |
| pi 的模型描述包含 `api`、`provider`、`baseUrl`、输入模态、上下文窗口、最大输出和推理映射。见 [Model](../../pi/packages/ai/src/types.ts#L821)。[VERIFY: pi/packages/ai/src/types.ts:821] | 区分模型身份、协议和能力；配置模型不等于证明能力 |
| 本地 pi 的 `stream`/`streamSimple` 先尝试内置 provider 路径，再查 API 注册项，包含环境凭据注入。见 [兼容入口](../../pi/packages/ai/src/compat.ts#L250)。[VERIFY: pi/packages/ai/src/compat.ts:250] | 参考可替换 provider 思想，不复制全部分支及认证实现 |
| pi 的流事件覆盖文本、thinking、工具参数增量与终止；usage 可有 reasoning 明细。见 [事件](../../pi/packages/ai/src/types.ts#L535)、[usage](../../pi/packages/ai/src/types.ts#L382)。[VERIFY: pi/packages/ai/src/types.ts:535] [VERIFY: pi/packages/ai/src/types.ts:382] | 区分供应商公开输出与私有数据，不要求每个模型都能输出 thinking 或详细 usage |
| Eino `AgenticModel` 是 `BaseModel[*schema.AgenticMessage]`，提供 Generate/Stream；工具通过 `model.WithTools` 调用选项传入。见 [接口](../../eino/components/model/interface.go#L105)。[VERIFY: eino/components/model/interface.go:105] | 已确认采用 Agentic 消息及 Typed 执行路径，复用 eino-ext 的 agentic* 组件，不另造模型 SDK |
| agenticopenai 提供 AgenticModel，对底层 Chat 协议客户端的复用在组件内部完成。见 [构造](../../eino-ext/components/model/agenticopenai/chat_model.go#L141)、[Agentic 客户端](../../eino-ext/libs/acl/openai/agentic_client.go#L49)。[VERIFY: eino-ext/components/model/agenticopenai/chat_model.go:141] [VERIFY: eino-ext/libs/acl/openai/agentic_client.go:49] | 供应商适配层拥有协议细节；不同模型请求通过调用选项提供工具，不能依赖共享实例上的可变绑定 |
| DeepAgent 将 `ModelRetryConfig` 和 `ModelFailoverConfig` 传给 ChatModelAgent；重试包裹模型调用。见 [装配](../../eino/adk/prebuilt/deep/deep.go#L151)、[重试](../../eino/adk/retry_chatmodel.go#L339)。[VERIFY: eino/adk/prebuilt/deep/deep.go:151] [VERIFY: eino/adk/retry_chatmodel.go:339] | 优先使用已有模型重试能力；不能据此承诺自动重放整个任务是安全的 |

### 1.3 从文章吸收的设计方法

- **协议优先**：以 Eino AgenticModel 的输入/输出和本章的流生命周期约束接入实现，不要求继承统一 Provider 基类，也不另造并行模型 SDK。
- **统一语义与映射**：上层表达思考强度、输出限制和缓存意图，L1 根据模型/API 能力映射参数；保留厂商差异，不把统一理解为只取所有模型的能力交集。
- **变化留在适配器内**：消息格式、SSE/SDK 流、推理方言与缓存机制由 L1 处理。新增模型配置或协议适配不修改 Agent Loop、会话和前端协议。

| 教程主题 | 本章对应契约 |
| --- | --- |
| 四类供应商差异；统一入口、事件协议、翻译器 | 4.1.1、4.3～4.5 |
| 调用已有模型；接入新模型；完整调用链 | 4.1.1～4.1.2 |
| 思考方言、统一档位及映射 | 4.1.3 |
| 流式解析、StreamFunction 契约及错误收尾 | 4.2～4.4 |
| 缓存语义与供应商实现 | 4.5 |

文章中的三层是模型接入内部的职责划分，不是新增三层产品架构。教程版本中的固定档位数量、预算数字及缓存 TTL 不直接成为产品保证；取舍以本地源码和适配认证为依据。

## 2. 用户体验与功能

### 2.1 用户故事与验收

以下 `MODEL-*` 均为拟定产品要求，不表示上游已经全部实现。

| 编号 | 用户故事 | 验收条件 |
| --- | --- | --- |
| MODEL-01 | 作为应用开发者，我希望注册不同模型，以便按任务选择服务 | 同时登记两个 provider/协议配置；列出身份和能力；新增适配器无需修改请求调度代码 |
| MODEL-02 | 作为接入者，我希望知道哪些配置可用，以免执行中才发现缺项 | 缺少必填模型名、无效 endpoint、需要凭据却未解析或工具能力不足时，请求发送数为 0；明确声明无凭据的本地服务除外；错误指出配置项但不含密钥 |
| MODEL-03 | 作为用户，我希望切换模型后继续当前会话 | 用户选择的新默认模型在下一独立 Trace 生效；当前 Trace 沿用已装配策略，审批恢复采用 checkpoint 当时有效模型。开发者的逐 Turn 模型策略按 4.1 单独处理 |
| MODEL-04 | 作为用户，我希望临时模型故障可以恢复且不重复改文件 | 仅重试当前 Turn 内的模型生成；已提交工具结果继续作为上下文；达到次数或时间预算后有明确失败结果 |
| MODEL-05 | 作为前端接入者，我希望正确显示流式输出与重试 | 每个增量标明 `turnId` 与 `attempt`；失败尝试的半段文本不得与成功尝试拼成一条最终回复 |
| MODEL-06 | 作为使用者，我希望取消立即阻止后续模型尝试 | 取消退避中的请求后，后续模型调用数为 0；已接收输出保留为取消轨迹；HTTP/SSE 断开本身不触发取消 |
| MODEL-07 | 作为运营或调试者，我希望看到真实消耗 | usage 区分已知、估算、未知；不把缺失值当零；同一次尝试的累计 usage 只计一次，子 Agent 消耗可归属 |
| MODEL-08 | 作为扩展作者，我希望模型配置与工具集合彼此隔离 | 两个 Session 使用同一底层服务但不同工具集时，模型各自只能获得本请求的定义；结果不能串入另一 Session |
| MODEL-09 | 作为接入者，我希望兼容问题明确可诊断 | 不支持的图片、结构化输出或推理参数应拒绝或报告明确降级；不得静默丢失必需内容；保留脱敏错误分类和请求关联信息 |
| MODEL-10 | 作为部署者，我希望多种运行环境具有同一协议行为 | Windows 原生、Linux、macOS、容器均运行同一假服务契约集；路径、代理与证书配置由应用装配方提供，核心不读取前端连接状态 |
| MODEL-11 | 作为底座开发者，我希望 Agent 不必了解每家供应商的响应格式 | L1 统一文本/公开推理/工具调用流、最终内容、终止原因和 usage；L2/M07 无需按供应商解析原始事件 |
| MODEL-12 | 作为使用者，我希望连续任务调用能利用供应商缓存 | L1 对已认证的模型/API 启用适用默认策略，M08 保持前缀稳定；终端用户无需设置缓存断点或缓存键；开发者可替换策略 |
| MODEL-13 | 作为调试者，我希望知道缓存是否真正生效 | 分别记录采用的策略、读/写 token、统计可用性及失效原因；不得把配置开启、0 和未报告混淆 |
| MODEL-14 | 作为用户，我希望缓存优化不影响分叉、恢复及切换模型的正确性 | 缓存未命中或句柄过期不丢上下文；不可串用其他账号/模型/分支的显式缓存或续接 ID；有界回退只重做模型请求 |
| MODEL-15 | 作为应用开发者，我希望用相同思考级别调用不同模型 | 模型声明支持档位及映射；记录请求值、生效值与调整原因；输出预算合规；不要求终端用户填写厂商参数 |
| MODEL-16 | 作为运行时开发者，我希望所有模型尝试具有相同的生命周期 | 建流失败、中途错误、取消与正常完成都只有一次尝试收尾；增量、快照和完整结果不混用，不因回调与 reader 同时报错产生双终态 |
| MODEL-17 | 作为使用者，我希望真实上下文溢出能进入正确恢复流程 | 显式错误与经认证的静默溢出按 4.2.1 识别；限流、普通输出截断和证据不足的空输出不误触发压缩 |

### 2.2 模型能力矩阵

模型记录至少区分以下能力；每项标注“已验证 / 声明但未验证 / 不支持”，以及验证的适配器版本和模型配置版本。不能从供应商品牌或 API 兼容宣传直接推导结论。

| 能力 | 产品要求 |
| --- | --- |
| 文本与流式文本 | 开放式 Agent 的基本能力；记录是否真正支持流式，非流式适配须显式标识 |
| 工具调用 | 使用 DeepAgent 工具循环的模型必须通过工具名、参数、调用 ID 与结果续接用例 |
| 多工具调用 | 描述是否支持一次产生多个调用；模型可以生成多调用不意味着应用装配方提供的执行策略允许并发 |
| 上下文和输出上限 | 记录已知窗口、最大输出及估算来源；未知时不得宣称预算精确，按第 8～9 章处理 |
| 图片/其他模态 | 按输入与输出分别声明；编码基础场景不强制图片；不支持的内容由投影策略明确拒绝或转换 |
| 推理控制与公开推理内容 | 记录可用 ThinkingLevel、映射及与输出预算的关系，规则见 4.1.3；参数支持不代表必须展示推理内容，只转发服务明确提供且允许展示的部分 |
| usage、缓存与费用 | 分别记录输入总量、未缓存输入、缓存读/写及推理明细的可用性；不同来源的包含关系先归一化；价格版本与费用估算不能代替账单 |
| 供应商缓存与续接 | 区分隐式前缀复用、显式断点、显式缓存资源和有状态响应续接；记录已认证 API/endpoint/模型与适配器版本，不以支持其中一种代表全支持 |
| 结构化输出、服务端工具 | 可选能力；未认证的服务端副作用工具不进入普通可重试模型路径 |

以下只是本地适配候选依据，不是完整产品认证结果：

| 协议/部署家族 | 已读代码证据 | 本产品边界 |
| --- | --- | --- |
| OpenAI Chat Completions / 兼容服务 | [agenticopenai Chat 构造](../../eino-ext/components/model/agenticopenai/chat_model.go#L141)。[VERIFY: eino-ext/components/model/agenticopenai/chat_model.go:141] | 每个兼容 endpoint 单独认证；底层复用旧协议客户端不改变对外 Agentic 契约 |
| Anthropic | [agenticclaude 构造](../../eino-ext/components/model/agenticclaude/model.go#L216)。[VERIFY: eino-ext/components/model/agenticclaude/model.go:216] | 接入协议、凭据及内容块支持分别认证 |
| Gemini | [agenticgemini 构造](../../eino-ext/components/model/agenticgemini/model.go#L112)。[VERIFY: eino-ext/components/model/agenticgemini/model.go:112] | SDK 依赖及缓存资源管理保持在 L1 |
| 本地模型服务 | 兼容 Chat endpoint 优先验证 agenticopenai；原 [Ollama 组件](../../eino-ext/components/model/ollama/chatmodel.go#L77) 是旧消息路径。[VERIFY: eino-ext/components/model/ollama/chatmodel.go:77] | 原组件不能直接接入 Agentic 执行器；需要时在 L1 提供明确的兼容适配，并验证工具及内容保真 |
| OpenAI Responses | [Responses 构造](../../eino-ext/components/model/agenticopenai/responses_model.go#L165)。[VERIFY: eino-ext/components/model/agenticopenai/responses_model.go:165] | 原生符合 AgenticModel；缓存、续接和服务端工具仍分别认证 |

DeepSeek 使用原生 [agenticdeepseek](../../eino-ext/components/model/agenticdeepseek/model.go#L98) 作为候选，仍按具体 endpoint 验证缓存计量、推理和工具调用。[VERIFY: eino-ext/components/model/agenticdeepseek/model.go:98] 不再用旧 Message 适配器已具备某能力来推断 Agentic 路径也完整保留了它。

### 2.3 非目标

本章不承诺复制 pi 的全部模型目录、OAuth 登录流程和订阅账号接入，不按所有供应商同时完成来定义最小验证闭环。系统不自动寻找或扫描用户其他应用的凭据，不实现模型训练平台、供应商计费平台或多供应商竞价路由。

## 3. AI 系统要求与评测

### 3.1 确定性场景

使用可控假模型、假 HTTP 服务及内存工具验证，不依赖真实 API 费用。所有断言均为将来的验收要求，本轮未执行实现测试。

| 场景 | 输入及故障注入 | 必须断言 |
| --- | --- | --- |
| M-E01 基本互换 | 两协议模型都返回“调用计算工具 → 答案” | 产品结果等价，调用归属完整，模型层不依赖 AgentSession、SessionManager 或 SessionStore |
| M-E02 参数不兼容 | 无图片能力模型收到必需图片；不支持推理参数 | 明确拒绝，或按明确可选策略报告降级；不能悄悄删除内容 |
| M-E03 工具之后重试 | 工具写入计数器 1 次；下一模型步骤先 429 再成功 | 计数器为 1；两次模型 attempt；一个最终接纳的回复 |
| M-E04 流中断 | attempt 1 输出文本及半个工具参数后断流；attempt 2 成功 | 未完成参数不执行；attempt 1 标记失败；最终回复不含两次输出拼接 |
| M-E05 预算与取消 | 持续 503；或在退避时取消 | 调用不超过配置上限；取消后没有新尝试；恰好一个 Trace 终态 |
| M-E06 切换与恢复 | Trace A 等待审批时用户选择默认模型 B，再恢复 A | A 采用 checkpoint 当时有效模型及原 generation；下一独立 Trace 才使用用户选定的 B |
| M-E07 计量 | 累计 usage 分三块上报；另一次完全不报 usage | 第一项按最终累计值记账；第二项标记未知；失败尝试不从总消耗中消失 |
| M-E08 凭据隔离 | 假服务返回含授权片段的错误体 | 普通事件、Session 和 Web 响应均无密钥；错误仍可定位到配置与 attempt |
| M-E09 响应语义 | 相同内容经不同厂商事件、分块和 usage 顺序返回 | 文本/公开推理/工具块的关联正确，最终内容等价；没有缓存写字段时保持未知，不按块重复累计 |
| M-E10 缓存请求 | Claude 断点、OpenAI 参数、Gemini 显式句柄、DeepSeek 隐式路径分别捕获请求 | 只传该 endpoint 支持的缓存字段，未重复启用互相冲突的策略；L2 没有供应商分支 |
| M-E11 前缀复用 | 同资源/工具连续执行三次模型步骤，仅追加新输入和工具结果 | 合法稳定区域保持一致，缓存作用域不因 executionId/attempt 改变；真实命中用供应商计量另验 |
| M-E12 缓存计量 | 输入总量 1000，其中缓存读 600、写 100、未缓存 300；其他响应缺写入字段 | 输入合计为 1000，不是 1700；缺失的写计量仍未知；缓存写不是模型输出 token |
| M-E13 失效与分支 | 缓存过期、分叉、压缩、改工具定义和切换模型 | 显式句柄只在输入等价且作用域兼容时复用；回退恢复完整请求，不续上错误分支，不重跑已完成工具 |
| M-E14 缓存策略关闭 | 开发者禁用主动缓存优化；使用支持自动服务端缓存的模型 | 不再发主动标记/创建显式资源；不假称可以关闭供应商隐式缓存；基本调用与取消正常 |
| M-E15 思考映射 | 两个协议处理同一档位；请求不支持的高/低档及 off；思考与答案共用输出上限 | 原生参数正确，回退值/原因可查；off 不偷偷开启；思考预算不挤占必需答案空间或突破上限 |
| M-E16 尝试收尾 | Stream 建立失败、Recv 中途失败、取消、异常 EOF、已认证的正常 EOF | 每次 attempt 一个终态；无重试文本拼接、无虚假完成；reader 释放，失败原因保留 |
| M-E17 块与快照 | 文本/推理/多个工具参数块交错，随后输出完整结果或断流 | 块索引和调用身份稳定；快照覆盖不重复追加，最终聚合一致；不完整参数不执行 |
| M-E18 溢出判别 | 显式超限、成功但输入超窗、输入满窗且 length/零输出；混入 429、低输出上限、未知 usage | 仅有足够证据的超限进入 M09；限流和普通截断不压缩；缺数据不填零推断 |
| M-E19 配置与协议扩展 | 已有协议新增模型；代码登记新协议的假 AgenticModel；引用未登记协议 | 前两者不改 Loop；配置/协议身份分开；未登记或必需能力缺失时网络调用为零 |
| M-E20 缓存意图 | none/short/long 映射到不同 endpoint，包含 long 不支持和仅隐式缓存 | 请求值与实际策略可查；不盲传字段，none 不承诺关闭服务端隐式缓存；long 不隐式开启服务端存储 |
| M-E21 Agentic 缓存明细 | 同一 Claude usage 样本经完整/流式响应转换，分别启用及不启用写入明细补采集 | 总量不重复累加；补采集值与原响应一致，无法取得 cacheWrite 时明确未知，不调用旧消息 getter |

真实模型评测复用限定代码修改、工具调用、长会话继续和非编码工具场景；先记录模型/适配器/提示词版本及基线，再讨论成功率和成本阈值。对同一 provider 的不同 model ID 分别报告，不能用假模型通过率代替任务效果。

### 3.2 计量与预算

Trace 总预算、单个模型调用重试上限及超时由底座提供开发者默认策略，终端用户无需配置。steering、follow-up、内部重试和恢复都归属同一 Trace，不重置总预算；主 Agent、子 Agent、工作流与自动压缩的模型消耗均计入。手动会话维护消耗归 operationId。默认值在开发验证时确定。

本地 general-purpose 子 Agent 的构造传入了 `ModelFailoverConfig`，该构造处没有传入主 Agent 的 `ModelRetryConfig`。见 [子 Agent 配置](../../eino/adk/prebuilt/deep/task_tool.go#L90)。[VERIFY: eino/adk/prebuilt/deep/task_tool.go:90] 因而“主 Agent 配置了重试”不能作为子 Agent 重试一致性的验收依据；产品需要对各执行来源分别验证预算和失败行为。

Eino `AgenticResponseMeta.TokenUsage` 可以缺失；结束原因分布在 OpenAI/Claude/Gemini 等协议扩展及其他 Extension 中，不存在一个可对所有模型直接读取的公共 FinishReason。见 [AgenticResponseMeta](../../eino/schema/agentic_message.go#L85)、[TokenUsage](../../eino/schema/message.go#L535)。[VERIFY: eino/schema/agentic_message.go:85] [VERIFY: eino/schema/message.go:535] L1 负责归一化并保留可用性来源，不能把默认零误报为供应商确认的零消耗。

## 4. 技术契约

### 4.1 配置、快照与分层

以下是语义字段，不冻结 Go 类型、配置文件格式或 HTTP URL。

| 对象 | 最少内容及约束 |
| --- | --- |
| 模型配置 | `provider`、`protocol`、`model`、endpoint、凭据引用、能力记录、模型参数、配置版本；凭据值不进入可序列化快照 |
| Trace 模型基线 | 初始模型、能力/凭据引用、可用模型选择策略和总预算；工具/skill generation 固定；子 Agent 的模型差异显式列出 |
| Turn 内模型调用 | traceId/turnId、模型 attempt、本轮实际生效配置版本、请求模型与响应模型、终止原因；可选观测 span 单独记录 |
| 消耗记录 | attempt 归属、已报告 usage、估算及其算法版本、费用估算版本；不得重复累加同一累计值 |

L1 接受已解析的凭据与本轮请求选项，不依赖 AgentSession、SessionManager、队列或 HTTP。CreateAgentSession 注入配置及选择策略，AgentSession 为 Trace 选择基线；L2 的 prepareNextTurn 可按已装配策略为下一 Turn 选择已获准模型/思考参数，默认沿用现有配置。每次改变都记录生效轮次，并重验消息兼容、上下文预算和缓存作用域；不得修改正在消费的流、扩大权限或加载新的工具/skill generation。选择失败保留当前有效配置并报告未生效，不能半途混用两个模型的流。

扩展的模型/思考选择入口使用同样规则：明确请求“下一独立 Trace 默认值”或“当前 Trace 下一 Turn 的获准选择”，不以一个含糊 setModel 操作覆盖两种作用域。选择请求和实际生效位置可查询，失败保留原有效配置；新 provider/模型适配实现仍通过现有登记与装配契约提供，不因运行操作而加载新代码。

从历史继续时，SessionManager 按 [M10 的选定路径重建规则](10-session-persistence.md#441-从选定路径重建上下文与配置) 提供历史模型/思考状态。新的独立 Trace 优先采用用户明确指定或已登记待生效的配置，否则以路径值为候选、缺失项用应用初始值；AgentSession 重新验证可用性与权限，保存实际生效配置。历史重建先于消息压缩筛选，响应模型别名不替代配置记录；resume 始终采用 checkpoint 当时有效配置。

凭据轮换不属于工具拓扑升级：当前模型调用不改认证，后续调用可经同一引用取新凭据，但不借轮换静默更换账号或模型。用户选择的默认配置在下一 Trace 生效；代码策略在获准配置内的轮后选择按上述规则验证。明确撤销则禁止后续调用，不能以旧快照绕过撤销。

#### 4.1.1 统一入口、协议与适配器

模型接入分三项职责：按配置选择实现，遵守统一输入/输出契约，在实现内部翻译供应商协议。provider 表示服务来源，protocol 表示实际 API 方言，model 表示该服务中的模型身份；同一服务可能有多个协议，多个服务也可复用同一协议。不能只按供应商品牌判断参数或路由。

复用 `model.AgenticModel` 的 Generate/Stream，输入为 `[]*schema.AgenticMessage`，工具等共性配置使用 `model.Option`。思考和缓存语义由 L1 的代码策略映射为组件实际支持的配置/选项；不假设 Eino 已提供统一 ThinkingLevel 选项。协议实现可在创建入口登记构造函数，也可注入已构造模型；不为此新增独立插件框架。

```text
本轮模型配置 + 已选消息/工具 + 统一调用意图
  → L1 解析默认值、思考映射及缓存策略，提供预算所需的生效选项
  → M08 完整请求预算与能力校验
  → model.AgenticModel.Generate / Stream
  → 适配器构建请求、使用 SDK 或解析原始协议、转换 Agentic 内容
  → L2 统一消费、聚合快照并记录 attempt 结果
  → M06 保存消息，M07 发布产品事件，M03 决定下一 Turn
```

统一策略提供类似 pi streamSimple 的便利：省略参数时使用开发者默认值，调用方不逐家拼推理字段；底层仍保留 AgenticModel 接口。L1 不决定业务历史选择或工具是否执行，不在归一化后擅自扩大输出预算。最终请求使用的模型及有效参数须与 M08 校验时一致。

#### 4.1.2 调用模型与接入新模型

| 场景 | 所需工作 | 验收边界 |
| --- | --- | --- |
| 调用已有模型 | 选择配置，传入消息/工具及统一语义选项，使用 Generate 或 Stream | 上层不解析厂商事件；非流式模型明确声明，不能伪造逐 token 流 |
| 已有协议新增模型或 endpoint | 新增模型配置、能力记录及必要映射，复用已登记适配器 | 不因换模型名就要求新写一个 Provider；兼容 endpoint 仍单独认证 |
| 新增协议 | 实现/复用符合 AgenticModel 的组件，补请求、响应、错误与语义映射；在代码装配处登记，再添加模型配置 | 不修改 Agent Loop、Session 或客户端；协议缺失、重复登记或配置不完整在调用前报错 |

适配器内部遵循五步：取得客户端/凭据 → 构建供应商请求 → 发起调用 → 解析并归一化响应 → 结束本次调用并释放资源。客户端可安全复用，不要求每次新建。已有 SDK 能解析就复用；需要原始 SSE 时使用明确的帧/数据解析规则，不按网络分块边界直接 JSON 解码。自定义传输仍返回同一 Agentic 内容及错误契约。

#### 4.1.3 ThinkingLevel 与模型映射

统一思考意图使用 off、minimal、low、medium、high、xhigh、max；不是每个模型都必须支持全部档位。未指定表示采用装配默认值，与显式 off 不同。每个模型的能力记录给出可用档位及映射；自适应思考、token 预算和 effort 字符串都在 L1 翻译，底层可以复用组件现有选项而非重新发送 HTTP。

- 支持请求档位时直接使用。默认对不支持的正向思考档位参考 pi 的顺序，在已认证的正向档位中先找不低于请求的最近档，再向下找；记录 requested/effective 和原因。全部不支持、必需精确能力无法满足或显式 off 无法兑现时，在请求前拒绝，不默默变成无思考或开启思考。
- 映射因 provider/protocol/model/config 版本而异。例如 budget 模型映射为预算，effort 模型映射为强度，自适应模型映射为其模式。max 是模型允许的最高已认证强度，不是无限 token。
- 共用响应上限的模型必须同时容纳思考及答案；记录实际输出上限和思考预算，给答案保留空间。有效值不得超过模型、调用方和 Trace 的限制。M08 使用这些有效值预算；无法同时满足必需限制时拒绝，不套用全局固定 token 表或在校验后扩大 maxTokens。
- 模型切换后重新解析映射和预算。请求值、生效值、原生参数摘要与策略版本用于诊断，不把厂商参数要求转交终端用户。

本地 pi 已包含 max，教程的“五级”不是当前穷尽列表；参考其方法而非冻结旧数字。见 [档位](../../pi/packages/ai/src/types.ts#L83) `[VERIFY: pi/packages/ai/src/types.ts:83]`、[映射与 clamp](../../pi/packages/ai/src/models.ts#L900) `[VERIFY: pi/packages/ai/src/models.ts:900]`、[思考与输出预算](../../pi/packages/ai/src/api/simple-options.ts#L75) `[VERIFY: pi/packages/ai/src/api/simple-options.ts:75]`。上述显式 off/必需能力校验是本产品边界，不声称完全复制 pi 的默认回退。

### 4.2 重试与失败分类

重试单位是尚未被接纳的模型调用 attempt，不是整次用户请求、执行尝试或包含已执行工具的 Turn。一个 Turn 的模型生成可以有多次网络尝试；只有被接纳的模型输出才能进入工具派发，不能因重试重新执行整轮工具。重试权限由执行策略决定，消耗计入原 Trace；观测记录不作为重试依据。

| 类别 | 拟定行为 |
| --- | --- |
| 参数错误、鉴权失败、必需能力缺失 | 不自动重试；返回可定位错误 |
| 限流、可恢复服务故障、短暂连接失败 | 在次数、退避总时长和 Trace 预算内重试；遵守可解析且在预算内的 Retry-After |
| 流中断 | 关闭旧流，保留失败 attempt 记录；在模型没有服务端副作用的已认证路径内允许有界重试 |
| 上下文超限 | 转交第 9 章压缩恢复规则；不得由普通重试无修改地无限重发 |
| 用户取消、审批暂停 | 不作为网络错误重试；按第 3 章生命周期处理 |
| 工具执行错误、结果未知 | 交第 5 章处理；模型重试器不得重新调用工具 |

Eino 的 `ShouldRetry` 能检查完整或部分流输出；其流式路径在判定前已有事件输出，因此产品消费者必须识别尝试边界。见 [Retry 配置](../../eino/adk/retry_chatmodel.go#L228)、[流判定](../../eino/adk/retry_chatmodel.go#L587)。[VERIFY: eino/adk/retry_chatmodel.go:228] [VERIFY: eino/adk/retry_chatmodel.go:587] 内部 HTTP 客户端重试与 ADK 重试须共享可观测总上限，不允许叠加后突破产品预算。

自动跨模型 failover 默认不启用；若显式配置，候选模型、能力下限、凭据、费用上限及投影规则必须预先确定，并记录实际选择。它不能回到旧步骤重放已经发生的工具副作用。

#### 4.2.1 上下文溢出的识别

L1 将供应商证据归一化为上下文超限或其他错误，L2 再按 M09 请求压缩恢复。不能只列出“超限后压缩”而让每个调用者自行猜测错误文本。

| 证据 | 识别规则 |
| --- | --- |
| 明确超限错误 | 优先使用已认证的错误码/结构；必要时使用按 endpoint 验证的错误文本模式。400/413 状态本身不足以证明 token 超窗 |
| 成功响应但输入超窗 | 当前模型窗口已知、当前请求 inputTotal 可信且口径完整，实际输入超过窗口时识别异常；未知或陈旧 usage 不参与判断 |
| length 且零输出、输入接近满窗 | 仅对经过样本认证的协议/模型使用该组合信号及阈值；同时排除调用方输出限制过小等原因，不将 length 单独当溢出 |
| 限流、服务不可用、普通输出截断 | 优先按真实错误处理；文本中出现“too many tokens”不覆盖明确的限流分类 |
| 缺少窗口/用量、空输出但原因不明 | 不伪造超限证据；按异常响应/普通截断处理并保存诊断，不自动无限压缩 |

输入统计使用归一后的 inputTotal，不能重复加缓存 token。空文本但存在工具调用、推理或已声明的其他输出不等于空响应。确认为异常的必需输出缺失不能被当作正常完成；也不能仅因此断言是上下文溢出。检测规则、依据和已知限制作为适配能力记录，不能保证识别所有服务端静默截断。

pi 同时处理错误模式、成功但用量超窗及满窗零输出，并排除限流模式，见 [overflow](../../pi/packages/ai/src/utils/overflow.ts#L134) `[VERIFY: pi/packages/ai/src/utils/overflow.ts:134]`。本产品采用认证后的检测规则；M09 统一限制恢复次数，不由普通模型重试器再独立发起压缩。

### 4.3 流、消息与客户端

模型流拥有唯一负责消费和关闭的运行方；外部 HTTP/SSE 消费产品事件，不直接持有供应商 reader。客户端断开后后端仍按 Trace 预算执行，可按第 7 章查询与重连。

文本与工具参数增量是临时输出，不是已提交历史。模型完成并通过接纳检查后，AgentSession 委托 SessionManager 提交完整消息，再发布对应产品事件；失败、取消和被重试替代的输出按第 6 章保留诊断状态，不投影为成功助理回复。事件持久游标 `durableSeq`、瞬态 `chunkSeq` 的规则由[第 7 章](07-events-and-access.md)定义。

finish reason 统一区分正常结束、请求工具、长度截断、服务端拒绝、取消及模型错误，并保留脱敏原始值。`length` 不自动等于任务成功；建议保守拒绝执行被标为截断响应中的所有工具调用，即使部分 JSON 恰好能解析也不据此认定意图完整。产品明确反馈截断原因，按预算决定重新生成或失败，不执行半完成参数。

#### 4.3.1 一次模型 attempt 的流契约

保留 Eino 的 Go 契约：Generate/Stream 可直接返回 error，reader.Recv 也可中途返回 error。L1 归一化原因，L2 负责每次 attempt 的唯一聚合与收尾；不要求底层吞掉 error 或额外模拟一套 TypeScript 事件流。

| 阶段 | 必须满足的语义 |
| --- | --- |
| 尝试开始 | 完成前置配置校验后开始 attempt；此时不代表已连接或已得到内容。失败发生在建流阶段也能关联该尝试 |
| 内容更新 | 文本、推理、工具参数按内容块类型和块身份更新；块索引属于当前消息/attempt，不能把同名工具或不同 attempt 合并 |
| 部分快照 | 由单一聚合者从 Agentic chunks 生成当前消息视图；delta 是追加，snapshot 是覆盖，消费者不能把快照再次当增量追加 |
| 正常流结束 | 按该适配器的终止协议校验完整内容、结束原因和 usage；全量结果须与同次尝试的聚合视图一致 |
| 建流失败 / 中途失败 / 取消 | 保留已观察到的部分内容及真实原因，分别归一为 error/aborted；不执行残缺参数，不和后续成功 attempt 拼接 |
| 释放与收尾 | 已开始的 attempt 恰好一个结束结果，并释放 reader/底层连接；回调和 reader 重复报告同一错误不产生双终态 |

EOF 仅表示 reader 已耗尽，不能普遍代表成功。只有适配器明确声明以 EOF 正常结束、且内容完整性检查通过时，才按该协议归一化为正常结果；其他缺少必要终止信息的断流按失败处理。块完成不等于模型调用完成；模型调用完成不等于 Turn 或 Trace 完成，工具派发仍遵循 M03/M05。

pi 的中间事件携带 partial，done/error 携带最终消息；本产品复用 Agentic 内容块和 ConcatAgenticMessages 达到相同的状态一致性，不要求每个 chunk 自带全量快照。默认只发布允许展示的公开推理；签名及私有协议字段仅用于保真回放。历史提交、重放游标仍由 M06/M07 定义，不在模型层重复实现。

源码依据：[pi 流协议](../../pi/packages/ai/src/types.ts#L528) `[VERIFY: pi/packages/ai/src/types.ts:528]`；[Eino 返回 error](../../eino/components/model/interface.go#L36) `[VERIFY: eino/components/model/interface.go:36]`；[Recv](../../eino/schema/stream.go#L195) `[VERIFY: eino/schema/stream.go:195]`；[块聚合](../../eino/schema/agentic_message.go#L901) `[VERIFY: eino/schema/agentic_message.go:901]`。

### 4.4 对齐 pi 的统一模型响应语义

响应归一化属于 L1。L2 给生成结果增加 turnId/attempt、控制是否重试或派发工具；M06 定义产品历史；M07 定义对外事件。后三者不再自己识别 Anthropic、OpenAI 或 Gemini 的原始事件名。

| 统一语义 | 必须保留和校验 | Eino 复用与产品补足 |
| --- | --- | --- |
| 文本与多模态内容 | 内容类别、块或调用关联、增量/完整内容边界 | 使用 AgenticMessage.ContentBlocks、StreamReader 与 ConcatAgenticMessages；不能把完整片段按 delta 重复追加 |
| 公开推理与回放数据 | 服务实际返回的公开推理内容；签名/opaque metadata 按原协议保留 | Reasoning 块、Signature、协议扩展与 Extra 按能力映射；签名不作为正文展示，不向不兼容模型原样回放 |
| 工具调用 | 多调用身份、名称、完整参数、参数增量及完成状态 | 复用 FunctionToolCall / FunctionToolResult，按 CallID 配对；本地调用、服务端工具和 MCP 块分别识别，M05 再校验与执行 |
| 终止与错误 | 正常终答、工具请求、截断、拒绝、取消、传输/服务错误，附原始原因 | L1 统一供应商原因；L2 决定任务推进，不能把 EOF 普遍等同于成功 |
| 用量与缓存 | 输入/输出总量、缓存读、缓存写、推理明细、已知或未知、累积或增量语义 | L1 返回归一结果，由 AgentSession 协调 SessionManager 保存；各来源字段的包含关系不能直接相加 |
| 响应来源 | 模型/API/endpoint 配置版本、供应商 response ID、适配器/策略版本 | response ID 是标识，是否可用于服务端续接须另行认证 |

pi 的 AssistantMessageEvent 区分文本、thinking、工具参数的 start/delta/end 和 done/error；Usage 区分 input/output/cacheRead/cacheWrite。[流事件](../../pi/packages/ai/src/types.ts#L535) `[VERIFY: pi/packages/ai/src/types.ts:535]`；[Usage](../../pi/packages/ai/src/types.ts#L382) `[VERIFY: pi/packages/ai/src/types.ts:382]`。这是本产品对齐的响应语义，不要求 Go 层照抄所有 TypeScript 类型或事件名。

Eino AgenticMessage 已提供有序 ContentBlocks、推理签名、流块索引、ResponseMeta 与 Extra，但不能据此假定所有适配器都保留完整缓存细项或统一结束原因。[AgenticMessage](../../eino/schema/agentic_message.go#L71) `[VERIFY: eino/schema/agentic_message.go:71]`；[ContentBlock](../../eino/schema/agentic_message.go#L103) `[VERIFY: eino/schema/agentic_message.go:103]`。优先复用这些类型，仅在 L1 补缺失的语义映射。

如果某个适配器已经丢弃必需的内容块顺序、签名或用量细项，不能在 L2 凭空恢复；应在 L1 使用其响应扩展点补取，或对该组件做限定适配，并通过协议样本验证。无法保真的能力标为未支持或未认证，不以字段名称相同宣称与 pi 等价。

请求侧同样需对齐：system/developer 角色、工具结果合并及 ID、schema 方言、最大输出/推理参数、流式 usage 和媒体表示都由 L1 按协议能力转换。允许的规范化要有明确定义；不能把必需内容静默丢弃后称为“兼容”。

### 4.5 供应商前缀缓存与续接契约

#### 4.5.1 缓存类型不能混用

- **前缀缓存**：复用已处理的输入前缀，后续回答仍重新生成；不代替本地历史。
- **显式缓存资源**：先创建服务端缓存对象，再以句柄引用等价的输入前缀；需要内容匹配、生命周期与失败回退。
- **服务端会话续接**：通过 previous response/interaction ID 让服务端补全先前对话；涉及存储与因果链，是独立的有状态调用模式。
- **最终答案缓存、Eino checkpoint**：分别是应用结果复用与执行恢复，不属于本章前缀优化。

开发者提供默认策略，用户无需配置缓存键/断点/TTL。普通前缀优化按已认证能力启用；创建计费缓存资源或启用服务端会话续接必须由应用装配策略明确选择，不因组件开关名字含 Cache 就自动启用。

统一缓存意图沿用 none/short/long，未指定时采用代码默认的 short；它表示主动前缀优化的保留倾向，不是跨供应商统一的 TTL 保证。

| 意图 | 默认处理 |
| --- | --- |
| none | 不增加主动缓存标记或主动创建资源；不能承诺关闭供应商隐式缓存 |
| short | 使用已认证的短期/常规前缀策略；仅隐式缓存的服务继续依靠稳定输入，不发伪造字段 |
| long | 使用已认证的较长保留策略；不支持时回退 short 并记录原因，不能自动开启 store/有状态续接 |
| endpoint 没有对应能力 | 不传不支持的选项，记录不支持及实际策略；没有主动能力时可保持隐式缓存或不采用主动优化 |

请求意图、生效策略和调整原因可查询；开发者可替换映射，终端用户无需选择每家厂商的参数。显式计费缓存资源与服务端会话续接仍受独立装配策略约束，none/short/long 不越过这些开关。Bedrock 的 cachePoint 可作为另一协议机制的参考，不因此要求首批加入所有供应商。

#### 4.5.2 本地实现对照

核查基于 source-evidence 中记录的本地 pi / eino-ext commit。以下为已读源码事实，尚未运行真实供应商验证；所列选项不代表适用于所有代理、模型或最新 API。

| 路径 | pi 参考行为 | 本地 eino-ext 能力 | 产品需要补足 |
| --- | --- | --- | --- |
| Claude Messages | system、末尾常驻工具，以及转换后末条消息为 user 时的末块采用缓存标记（可含 tool_result） | agenticclaude 支持顶层 CacheControl，也能从内容块和工具读取显式缓存标记 | 自动策略与 pi 多位置标记不是同一方案；选择并验证断点、限制和 TTL，不能直接套用旧 claude 组件的 WithAutoCacheControl |
| OpenAI 兼容 Chat | 按兼容能力处理缓存字段 | agenticopenai 支持 ExtraFields，并通过 Agentic 客户端复用底层协议调用 | 核实缓存参数与 usage 在完整 Agentic 路径中的保留；不从兼容品牌推断支持 |
| OpenAI Responses | 通用路径完整 input 回放、缓存键取自 sessionId，store:false | agenticopenai 有缓存键/保留选项；EnableAutoCache 另会用 PreviousResponseID、截去已续接历史，并可能置 Store=true | 使用已选 Agentic 接口；前缀缓存与服务端有状态续接仍分别认证 |
| Gemini GenerateContent | 当前读取 cachedContentTokenCount；所读 buildParams 路径未创建显式缓存对象 | agenticgemini 提供 CreatePrefixCache、WithCachedContentName 和缓存 token 读取 | 明确 system/tools/messages 覆盖范围及合法后缀；管理模型/资源版本、过期与重建 |
| DeepSeek | 作为供应商兼容路径的一种参考，具体采集按其协议验证 | agenticdeepseek 使用 Agentic 客户端；旧 deepseek 组件已有 PromptCacheHitTokens 映射，不代表新路径自动覆盖全部原生字段 | 新路径必须验证 hit/miss 字段是否保留；缺失则在 L1 补取或标为未知，不虚构命中量或创建 API |

源码证据：

- pi Claude 系统标记、用户消息断点和末尾工具标记：[system](../../pi/packages/ai/src/api/anthropic-messages.ts#L973) `[VERIFY: pi/packages/ai/src/api/anthropic-messages.ts:973]`；[历史](../../pi/packages/ai/src/api/anthropic-messages.ts#L1295) `[VERIFY: pi/packages/ai/src/api/anthropic-messages.ts:1295]`；[工具](../../pi/packages/ai/src/api/anthropic-messages.ts#L1360) `[VERIFY: pi/packages/ai/src/api/anthropic-messages.ts:1360]`。
- pi Responses 请求：[buildParams](../../pi/packages/ai/src/api/openai-responses.ts#L262) `[VERIFY: pi/packages/ai/src/api/openai-responses.ts:262]`。pi Gemini：[usage](../../pi/packages/ai/src/api/google-generative-ai.ts#L227) `[VERIFY: pi/packages/ai/src/api/google-generative-ai.ts:227]`；[buildParams](../../pi/packages/ai/src/api/google-generative-ai.ts#L359) `[VERIFY: pi/packages/ai/src/api/google-generative-ai.ts:359]`。
- pi 兼容 Chat 路径按 compat 映射缓存字段和 Anthropic 风格标记：[请求选项](../../pi/packages/ai/src/api/openai-completions.ts#L767) `[VERIFY: pi/packages/ai/src/api/openai-completions.ts:767]`；[标记转换](../../pi/packages/ai/src/api/openai-completions.ts#L1015) `[VERIFY: pi/packages/ai/src/api/openai-completions.ts:1015]`。
- eino-ext agenticclaude：[自动配置](../../eino-ext/components/model/agenticclaude/model.go#L126) `[VERIFY: eino-ext/components/model/agenticclaude/model.go:126]`；[请求写入](../../eino-ext/components/model/agenticclaude/model.go#L489) `[VERIFY: eino-ext/components/model/agenticclaude/model.go:489]`；[消息断点](../../eino-ext/components/model/agenticclaude/convertor.go#L198) `[VERIFY: eino-ext/components/model/agenticclaude/convertor.go:198]`；[工具断点](../../eino-ext/components/model/agenticclaude/convertor.go#L541) `[VERIFY: eino-ext/components/model/agenticclaude/convertor.go:541]`；[usage](../../eino-ext/components/model/agenticclaude/convertor.go#L1318) `[VERIFY: eino-ext/components/model/agenticclaude/convertor.go:1318]`。
- eino-ext agenticopenai：[额外字段](../../eino-ext/components/model/agenticopenai/chat_model.go#L114) `[VERIFY: eino-ext/components/model/agenticopenai/chat_model.go:114]`；[调用选项传递](../../eino-ext/components/model/agenticopenai/chat_model.go#L233) `[VERIFY: eino-ext/components/model/agenticopenai/chat_model.go:233]`。
- eino-ext Responses：[缓存键](../../eino-ext/components/model/agenticopenai/option.go#L97) `[VERIFY: eino-ext/components/model/agenticopenai/option.go:97]`；[有状态续接](../../eino-ext/components/model/agenticopenai/responses_model.go#L631) `[VERIFY: eino-ext/components/model/agenticopenai/responses_model.go:631]`。
- eino-ext agenticgemini：[创建](../../eino-ext/components/model/agenticgemini/model.go#L194) `[VERIFY: eino-ext/components/model/agenticgemini/model.go:194]`；[引用](../../eino-ext/components/model/agenticgemini/conv.go#L919) `[VERIFY: eino-ext/components/model/agenticgemini/conv.go:919]`；[usage](../../eino-ext/components/model/agenticgemini/conv.go#L391) `[VERIFY: eino-ext/components/model/agenticgemini/conv.go:391]`。agenticdeepseek：[客户端构造](../../eino-ext/components/model/agenticdeepseek/model.go#L138) `[VERIFY: eino-ext/components/model/agenticdeepseek/model.go:138]`；旧 deepseek [usage 参考](../../eino-ext/components/model/deepseek/deepseek.go#L906) `[VERIFY: eino-ext/components/model/deepseek/deepseek.go:906]`。

服务端规则以相应 API 的官方文档为准：Claude 支持自动缓存和显式断点，其前缀涉及 tools/system/messages；修改前面的内容会影响后续复用。[Claude 缓存文档](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)

OpenAI 当前文档按模型区分缓存策略与字段；本地源码中的 prompt_cache_retention 不能被当作所有后续模型唯一参数，新增选项必须单独验证。[OpenAI 缓存文档](https://developers.openai.com/api/docs/guides/prompt-caching)

Gemini GenerateContent 的显式缓存资源与隐式缓存分别定义，不能将它们直接推广到其他 Gemini API。[GenerateContent 缓存文档](https://ai.google.dev/gemini-api/docs/generate-content/caching?hl=en) DeepSeek 的缓存由服务自动处理并返回命中相关用量，不需要照搬 Claude 标记。[DeepSeek 缓存文档](https://api-docs.deepseek.com/guides/kv_cache/)

#### 4.5.3 L1/L2/L3 的联合约束

1. AgentSession 提供稳定、不含秘密的不透明缓存作用域与模型/资源版本；L1 无需理解或查询 Session 对象。缓存作用域不以每次新建的 traceId、executionId、attempt 或时间戳作为唯一键，否则会人为破坏复用。
2. M08 负责内容与顺序稳定。L1 在完成协议序列化后选择合法断点/参数；只装饰本次请求副本，不把 cache_control 或缓存句柄写成用户话语，不改变产品历史。
3. L1 内同一请求只采用已验证的缓存策略组合；优先复用 eino-ext 开关/选项，缺失时使用明确限定的适配或扩展，不在 L2 按供应商品牌插入字段。
4. 显式缓存句柄绑定 provider/endpoint、账号权限范围、模型、缓存策略版本和被缓存内容摘要。只有输入等价且作用域兼容才可引用；generation 变更要检查相关内容是否改变，不假定一个字符串缓存键能证明内容相等。
5. 分叉与压缩必须以实际选定的历史构造请求。普通前缀匹配可继续复用合法共同前缀；有状态续接 ID 仅在属于正确因果链时使用，不能把另一分支未选中的内容重新引入。具体版本信息由上层传值，不反向依赖。
6. 缓存未命中属于正常结果；无合法缓存句柄时发送完整等价请求，或在预算内重建已获准的资源。回退只重做无副作用的模型请求，不重放已完成工具；无法重建完整上下文时明确报错，不只发送后缀继续。
7. 开发者禁用主动优化时不注入主动标记/创建缓存资源；供应商可能仍自动缓存，必须如实表述。缓存键不是访问控制，不能代替账号/会话的权限隔离。
8. TTL、最小缓存长度、价格和命中率按模型/API 能力验证，不冻结未经实测的全局数字。默认策略由代码提供，用户只需使用 Agent；性能指标可观测，但不承诺固定命中率或节省比例。

#### 4.5.4 缓存用量的统一口径

产品至少区分 inputTotal、uncachedInput、cacheRead、cacheWrite、outputTotal 和各字段的已知/未知来源。厂商有保留时长细分时额外保存，reasoning 通常是 output 的子项，不能再次加总。缓存写入费用及显式缓存资源存储费用也不能混入输出 token。

只有供应商语义明确、字段完整且互不重叠时，才校验 `inputTotal = uncachedInput + cacheRead + cacheWrite`；缺字段不自行填零或通过未经证明的减法推断。响应中的 PromptTokens 已经包含缓存输入时，不能再把 CachedTokens 加到总输入。

原基线组件将 InputTokens + CacheReadInputTokens + CacheCreationInputTokens 合成 PromptTokens，并把缓存读保存为 CachedTokens；当时核查的缓存写明细缺口是历史事实，不能直接用于判断升级后的能力。2026-09-25 依赖更新后的 agenticclaude v0.1.7 已在 Generate/Stream 的 `PromptTokenDetails.CacheWriteTokens` 保留缓存写，并提供 `GetCacheCreationInputTokens`。产品测试 `internal/llm/p2_usage_upstream_test.go` 实际调用 adapter，确认正数缓存写可直接读取，开启/关闭补采集不改变原 SDK 数值；该测试不是完整产品 Claude 工厂或真实 endpoint 认证。

应优先复用现成字段与 getter。当前 getter 只在缓存写大于零时返回存在标记，缺失与明确返回零都表现为 `(0, false)`；因此不能用它独自满足“known/unknown”的字段存在性契约。仅对仍缺失的 presence、TTL 等必需证据，通过响应扩展点或有界 read-through 采集补取，不能重复实现整套协议解析。无法取得证据时 cacheWrite 为未知，uncachedInput 也不能仅靠 PromptTokens - CachedTokens 推出，因为差值仍可能包含缓存写。PromptTokens 可作为适配器报告的总量，但产品对其 known 状态仍按所需原始字段完整性判定，不能再把读/写重复加到总输入。

所有用量按 attempt 保存，区分累积报告与增量报告。指标至少可区分“策略未启用/不支持”“请求已携带优化参数”“供应商确认读取/写入”“统计未知”；最终是否命中以供应商响应为依据。

## 5. 风险与未决决策

| 决策/风险 | 当前建议 | 对完整系统的影响 |
| --- | --- | --- |
| 首批供应商、协议、模型及认证范围 | 按 pi 的可扩展方式，先选有限且不同协议的认证矩阵；数量与名单待确认 | 决定真实评测、兼容承诺及凭据交互，不能从“参考 pi”推定全部 OAuth 都必须首发 |
| 重试次数、超时与费用默认值 | 有界且可查询；默认值待模型矩阵后确认 | 必须与第 3 章 Trace 预算、第 9 章压缩一致 |
| Agentic 模型基线 | 已确认采用 *schema.AgenticMessage / AgenticModel | 验证主/子 Agent、取消、重试、工具结果、缓存与恢复；旧消息组件须明确适配，不能混用泛型 |
| 本地模型和无凭据服务 | 允许明确声明无凭据的配置，不把空密钥普遍视作有效 | 需要独立工具能力测试 |
| 请求重试是否产生重复计费 | 记录每次 attempt 与未知消耗，不承诺账单恰好一次 | 请求结果去重与供应商实际计费是不同保证 |
| 全环境支持 | Windows 原生、Linux、macOS、容器均为目标 | 本轮只有源码核查，实际系统尚未做跨平台验证 |
| 统一语义与适配认证 | 各认证模型覆盖 MODEL-11～17、M-E09～21，包括思考映射、流生命周期、溢出识别和缓存计量 | 能力声明与实际测试分别记录；模型接入不等于所有明细均已保留，缺失能力明确为未知/未支持 |
| 有状态续接及显式缓存资源 | 与普通前缀优化分别选择，当前不因 EnableAutoCache 名称自动启用 | 需确认远端存储、内容等价、过期回退和分支规则；不改变本地 Session 为历史来源的原则 |

本章与[执行生命周期](03-agent-loop.md)、[工具系统](05-tool-system.md)、[消息](06-messages.md)、[上下文](08-context-engineering.md)、[压缩](09-compaction.md)和[会话持久化](10-session-persistence.md)共同闭合。只有全套 PRD 的未决项完成归类、契约冲突处理后，才制定开发方案。
