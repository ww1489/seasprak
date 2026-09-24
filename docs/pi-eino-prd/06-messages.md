# 第 6 章：消息类型与模型边界

状态：按消息设计 review 修订。对应 [M06 消息系统](https://dg-ai-notes.pages.dev/modules/ch06-messages/)。采用 pi 的三类标准消息职责与两层消息设计；Eino 模型输入/输出统一使用 `*schema.AgenticMessage`。本文规定语义，不冻结 Go 结构体和 HTTP DTO。

## 1. 执行摘要

一条消息同时服务于 Agent 推理、历史保存和客户端展示，三者需要的信息不同。例如用户直接执行命令后，客户端需要命令、输出和退出码；模型只需要本次执行的有效上下文。不能为了调用模型，提前把原始结构拍平成一段文本。

本章采用两层设计：内部 `AgentMessage` 保留标准消息与应用自定义消息；调用模型时统一转换为 `[]*schema.AgenticMessage`。先在内部消息上选择和调整上下文，再在模型边界转换。SessionManager 保存原始历史，客户端通过接口读取结构化视图。

成功标准：三类标准消息职责清楚；普通扩展无需专用编解码器即可添加消息；转换不修改原历史；工具结果准确配对；隐藏和排除上下文具有独立且可验证的行为。

## 2. 用户体验与功能

### 2.1 用户故事与验收

| ID | 需求 | 验收 |
| --- | --- | --- |
| MSG-01 | 作为接入者，我希望区分用户输入、模型输出和工具结果 | 工具调用属于助手消息，工具结果关联原调用；不能把直接执行命令的记录冒充模型工具结果 |
| MSG-02 | 作为扩展作者，我希望添加业务上下文并保留结构化详情 | 通用自定义消息提供类型标识、content、details 和展示标记；已有 content 无需原扩展参与即可转换 |
| MSG-03 | 作为使用者，我希望切换模型或重新打开会话后历史仍完整 | 上下文处理与转换产生副本；原消息、内容块顺序、必要协议字段和结构化详情保持不变 |
| MSG-04 | 作为接入者，我希望分别决定展示和模型参与性 | 隐藏不自动排除模型；明确排除的消息不进入普通调用或压缩/分支摘要模型 |
| MSG-05 | 作为使用者，我希望工具并发、失败和取消后仍能读懂结果 | 结果通过调用身份关联；拒绝、失败、未知不伪装为成功；不重复执行或重复生成有效结果 |
| MSG-06 | 作为使用者，我希望故障后保留已产生的信息 | 部分回复可标为 incomplete；不执行未完成的工具参数；消息结束不代表整个 Trace 完成 |
| MSG-07 | 作为历史使用者，我希望消息与会话记录职责分开 | 普通消息、摘要、仅供扩展保存的状态各有明确入口；持久化条目不全部进入模型 |
| MSG-08 | 作为开发者，我希望转换错误可定位 | 非法内容块、缺失必需转换规则或工具配对损坏时指出相关消息，阻止本次模型调用 |

### 2.2 边界与非目标

本章不实现前端组件、不复制 pi 的 TypeScript 类型系统，也不建立通用消息版本注册平台。执行状态、审批及恢复分别引用 M03/M05/M10/M12；不在消息中再定义一套 Trace 状态机。只需要保存的扩展状态使用会话条目，不为了持久化而强制制造对话消息。

## 3. AI 系统需求与评测

模型上下文包含选定的消息内容和明确组装的系统指令。扩展 details、审批审计、运行诊断和页面展示配置默认不进入模型；它们也不能自动获得系统指令优先级。供应商适配、响应归一化及前缀缓存归 M04，本章保留它们所需的内容块与不透明字段。

工具结果的截断范围、限制原因与续读方法是模型需要的工作材料，按 M08 放入有界 content，结构化计数和产物状态另存 details。不能因 details 默认不参与投影而让模型误以为已经获得全文。

确定性验收使用假模型捕获请求：

1. 用户输入 → 助手调用工具 → 工具结果 → 助手终答，确认三类消息职责、CallID 配对及两个 Turn 的边界。
2. 文本、推理、多个工具调用与多模态块混合，转换及流合并后顺序正确，原历史不变。
3. 通用自定义消息隐藏但进入模型；命令记录可展示但排除模型；仅持久化条目不进入模型。
4. 停用生成通用自定义消息的扩展后，历史仍可读取，已保存 content 仍可参与新请求；特殊未知类型没有规则时明确处理，不猜测内容。
5. 两个同名工具并发、工具拒绝、参数截断、响应中断，配对不串线且没有虚假成功。
6. transformContext 收到结构化消息；convertToLlm 在其后调用；压缩重新构造同一条管道，不从已拍平文本反推历史。
7. 关闭日志/观测导出时，traceId 与完整运行记录仍存在；审批恢复保留同一 Trace，失败或取消的 agent_end 不解释为成功。

真实模型仅验证所选 Agentic 适配器的协议兼容与多轮效果，不代替上述确定性验收。本轮为文档修订，未执行模型测试。

## 4. 技术契约

### 4.1 第一层：三类标准消息各有职责

pi 的 `Message` 包含以下三类。本产品沿用它们的语义，不另造一套与 Eino 内容块重复的底层模型 SDK。

| 语义类别 | 内容与职责 | Eino Agentic 表达 |
| --- | --- | --- |
| UserMessage | 用户输入及允许注入的上下文，文本与多模态内容 | `Role=user`，使用对应的用户输入 ContentBlocks |
| AssistantMessage | 模型回复，包括文本、推理和工具调用；保留响应来源、usage 与结束信息 | `Role=assistant`，包含 AssistantGenText、Reasoning、FunctionToolCall 等块；元信息按 M04 归一化 |
| ToolResultMessage | 本地工具执行结果，关联调用 ID、工具名、结果与错误信息 | `Role=user`，包含 FunctionToolResult 块，以 CallID 对应 FunctionToolCall |

**三类语义不等于三个 Eino role 值。** `AgenticMessage` 的角色为 system/user/assistant，没有独立 tool 角色。工具结果虽然放在 user 消息内，仍是工具提供的数据，不能当作人类输入或授权。M09 的切点选择与摘要材料序列化也必须识别内容块及原调用关系，不能把 user role 一律当作可独立保留的用户消息。系统指令由受信装配方单独提供，在模型边界形成 system 消息。

本地 `FunctionToolResult` 没有 pi 的 `isError` 字段，工具失败、拒绝和未知状态仍保留在 M05 调用记录及内部结果中，并在送给模型的结果内容中明确表达；不能因为类型切换丢掉失败语义。见 [结果结构](../../eino/schema/agentic_message.go#L372) `[VERIFY: eino/schema/agentic_message.go:372]`。

`AgenticMessage.ContentBlocks` 还可表示供应商服务端工具等能力，是否开放由 M04/M05 决定；不能把服务端已执行的工具调用重新派发给本地执行器。内容块类型更丰富不代表每个 provider 都支持所有能力。

源码依据：[pi 三类 Message](../../pi/packages/ai/src/types.ts#L421) `[VERIFY: pi/packages/ai/src/types.ts:421]`；[Agentic 角色与内容块](../../eino/schema/agentic_message.go#L39) `[VERIFY: eino/schema/agentic_message.go:39]`；[工具结果构造示例](../../eino-ext/components/model/agenticopenai/examples/chat_generate/main.go#L88) `[VERIFY: eino-ext/components/model/agenticopenai/examples/chat_generate/main.go:88]`。

### 4.2 第二层：AgentMessage 保留内部结构

`AgentMessage` 是内部消息集合：标准消息 + 应用自定义消息。标准消息复用 Agentic 内容及元信息；自定义消息按需要保留业务字段。L2 只接受装配好的转换能力，不导入 L3 的 ExtensionRegistry、AgentSession 或存储实现。

| 内部类型 | 为什么需要单独保存 | 默认模型视图 |
| --- | --- | --- |
| 三类标准消息 | 保留输入、模型回复、工具结果及调用关系 | 保留对应 Agentic 内容块 |
| 命令执行记录 | 用户直接执行命令，保存 command/output/exitCode/取消与截断信息；区别于模型发起的工具调用 | 格式化为 user 上下文；明确排除时跳过 |
| 通用 CustomMessage | 保存 customType、content、可选 details、display；content 是已经适合模型读取的用户输入内容块 | 转换为 user；details 留给应用消费，display 不参与转换判断 |
| BranchSummary / CompactionSummary | 保存摘要正文、程序文件附录及来源/覆盖范围；完整文件集合保留在结构化记录中 | 以有明确摘要标识的 user 内容参与上下文；分别标明“另一分支探索”和“主线历史压缩”，只投影允许且有界的附录，详见 M08/M09 |

普通扩展优先使用通用 CustomMessage，不必为每个 customType 注册编码器、解码器和投影器。只有通用结构无法表达的类型才提供专用转换规则；该规则通过现有扩展装配点传入，不增加独立插件平台。客户端没有专用展示方式时可读取通用 content 和结构化详情。

SessionEntry 是历史树节点，分为可形成上下文的消息/摘要、模型与思考级别的历史配置、纯元数据/扩展状态三类职责，详见 [M10 Entry 分类](10-session-persistence.md#411-entry-的三类职责)。配置用于重建当时状态，`CustomEntry`、书签等不进入 AgentMessage 上下文集合；header、队列和 checkpoint 关联也不伪装成对话消息。序列化、分支和文件版本由 M10 统一规定；不要求每类业务消息各自实现完整版本迁移系统。

扩展的 SendMessage 使用上述通用 CustomMessage，SendUserMessage 式入口提交普通输入但保留扩展来源；两者均不代表人类审批。是否触发执行及投递到 steering/follow-up/下一安全输入边界由 [M11 运行操作](11-extensions-workflows-access.md#26-扩展可使用的运行操作) 明确；受理不等于消费，待处理内容不能提前进入 M08/M09。AppendEntry 只追加扩展状态，投影时不将其转换为用户消息。

源码依据：[AgentMessage 扩展集合](../../pi/packages/agent/src/types.ts#L316) `[VERIFY: pi/packages/agent/src/types.ts:316]`；[pi 自定义消息](../../pi/packages/coding-agent/src/core/messages.ts#L28) `[VERIFY: pi/packages/coding-agent/src/core/messages.ts:28]`；[仅持久化与上下文消息的区分](../../pi/packages/coding-agent/src/core/session-manager.ts#L100) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:100]`。

### 4.3 两阶段管道：先处理上下文，再转换消息

| 阶段 | 输入 → 输出 | 职责 |
| --- | --- | --- |
| transformContext | AgentMessage[] → AgentMessage[] | 选择有效历史、过滤、缩减旧工具预览、加入允许的结构化上下文；仍保留业务字段 |
| convertToLlm | AgentMessage[] → []*schema.AgenticMessage | 标准消息保留内容块，自定义消息按明确规则转换或排除；不修改原历史 |
| L1 供应商适配 | Agentic 消息及选项 → 供应商请求 | 处理协议格式、能力约束、缓存标记和必要 metadata；不承担业务消息选择 |

这是职责分离，不要求三套独立框架。预算可在转换后的完整请求上计算；预算不足时交给 M09，从原始结构化历史重建，不能在已经失去业务字段的模型消息上实现全部上下文管理。具体顺序以 M08 为准。

convertToLlm 不访问网络或执行工具；相同选定内容和转换规则应得到相同输出。通用 CustomMessage 即使其生成扩展停用，也可使用已保存 content；真正未知的特殊类型保留原始数据，若必须进入模型且缺少转换规则，则报错，不偷偷丢弃或自动提升为 system。

pi 的默认 coding-agent 转换器将命令记录、通用自定义消息和摘要转换为 user，标准消息直接通过；这是默认转换规则，不是所有应用和供应商的强制限制。见 [转换器](../../pi/packages/coding-agent/src/core/messages.ts#L148) `[VERIFY: pi/packages/coding-agent/src/core/messages.ts:148]`；[先 transform 再 convert](../../pi/packages/agent/src/agent-loop.ts#L288) `[VERIFY: pi/packages/agent/src/agent-loop.ts:288]`。

### 4.4 展示、模型参与和持久化

| 场景 | 客户端展示 | 模型参与 | 保存方式 |
| --- | --- | --- | --- |
| 普通消息或可见自定义消息 | 按接口内容展示 | 是 | 消息条目 |
| 明确排除上下文的命令记录 | 可展示 | 否 | 原始消息仍保存 |
| display=false 的自定义上下文 | 默认隐藏 | 是 | 消息条目 |
| 扩展内部状态 | 不作为对话消息展示 | 否 | 仅持久化条目 |

展示标记不是权限控制。明确排除的消息也不能经压缩或分支摘要重新进入模型。标准工具请求和结果按完整因果关系处理，不允许任意隐藏一端的模型参与性而破坏配对。未确定模型可见内容的内部状态应使用仅持久化入口，不要求所有消息都携带四组合策略字段。

### 4.5 最少关联信息与流式消息

- `messageId` 标识一条产品消息；流式开始、增量和最终结果保持同一身份。
- `sessionId` 标识会话，`traceId` 标识完整运行，`turnId` 标识具体模型/工具轮次；多条已消费输入通过各自 `inputId` 关联同一 Trace。独立 Workflow Agent 的结果进入该对话的产品消息，不伪造父 FunctionToolResult；其无模型工具节点记录产品调用及节点结果，不伪造 Turn/模型工具请求。子委派的外层结果配对原父 `task` 调用。用于授权的原始输入与转换结果保留来源及原文引用，按下节规则区分。排队阶段不伪造 turnId，历史导入可没有执行归属。
- 日志/性能 span 属于可选诊断，不能与产品 traceId 混名；executionId 等内部尝试信息放在运行记录中，不要求每条消息重复保存。traceId 关联运行状态与恢复范围，但不代替 inputId 去重或工具许可。
- 工具配对保留 Eino 的 `FunctionToolCall.CallID` / `FunctionToolResult.CallID`。跨作用域的产品唯一身份及 provider ID 映射集中在 M05 调用记录中维护，不默认重写所有供应商 ID。
- 活动流由一个运行方消费，使用 `ConcatAgenticMessages` 按内容块语义聚合；不能简单拼接字符串或把所有 chunk 都当作新块。
- 模型 attempt 的开始、内容块更新、快照与收尾遵循 M04 的流契约。一次 attempt 内 messageId 保持稳定；有输出的重试 attempt 使用独立候选消息身份，不再向已经 finalized 的失败消息追加内容。建流失败且没有输出时只记录尝试失败，不伪造助手正文。
- partial/snapshot 表示当前完整视图，delta 表示增量；模型 chunks 由运行方聚合，客户端按更新类别覆盖或追加，不能把整份快照再次拼进文本。模型结束不等于 Trace 结束，持久提交仍按 M07/M10。
- 完整消息提交历史；取消/失败时可保留 incomplete 及原因，不宣称崩溃前最后几个 token 必然落盘。无工具助手回复之后仍须处理 steering/follow-up，不能据此提前标记 Trace 完成。

Turn 与 Trace 的定义以 M03 为准：Trace 是一次完整运行，包含多个 Turn；消息结束不等于 Trace 结束。Trace 状态、执行尝试、工具未知结果核对和恢复引用 M03/M05/M07/M10。

#### 原始输入、派生内容与授权来源

用于授权判断的输入记录保留受信接入赋予的来源身份、inputId、原文或受保护可解析引用，以及是否已被当前 Trace 消费。input hook、模板/skill 展开、上下文转换后的模型内容单独关联原输入及转换来源，不覆盖原文。历史导入或扩展自填 source/role 不能自行取得人类身份；消息来源是事实记录，不是任意 payload 可签发的许可。

普通模型仍使用实际消费后的投影视图，不因保留原文就重复发送两份输入。安全审核按 M12 从受信记录重建材料，不把派生 user 文本、摘要、FunctionToolResult 或父 Agent 的自述直接当成人类授权。直接父任务身份来自运行时委派关系，并受父任务及人类限制约束；受保护的原文引用无法取得时，不用哈希/摘要猜测授权内容。

### 4.6 完整数据流与 Eino 接入

```text
SessionManager 选定分支历史 + 已消费输入
  → AgentMessage[]：保留标准内容块与业务结构
  → transformContext：选择、过滤、缩减、加入上下文
  → convertToLlm：生成 []*schema.AgenticMessage
  → 组装 system / 工具选项，预算与能力校验
  → L1 AgenticModel / eino-ext agentic* 适配器
  → Agentic 输出流 → 内部助手消息 / 工具结果 → 保存与发布
```

模型接口选用 `model.AgenticModel`，主/子 Agent、Runner、TurnLoop 及 middleware 使用匹配的 `*schema.AgenticMessage` 泛型路径。工具通过调用选项传入；不把旧 `BaseChatModel` 或 `adk.Agent` 别名当作相同类型直接接入。详细模型能力与兼容性验证归 M04，子 Agent 注册归 M11。

源码依据：[AgenticModel](../../eino/components/model/interface.go#L105) `[VERIFY: eino/components/model/interface.go:105]`；[DeepAgent 泛型配置](../../eino/adk/prebuilt/deep/deep.go#L40) `[VERIFY: eino/adk/prebuilt/deep/deep.go:40]`；[流聚合](../../eino/schema/agentic_message.go#L901) `[VERIFY: eino/schema/agentic_message.go:901]`。

## 5. 风险与跨章约束

Agentic 类型能承载更丰富的内容，但适配器仍可能不支持某些块或丢失协议字段，需要按 M04 逐路径验证。AgenticResponseMeta 不提供一个适用于所有供应商的统一结束原因，L1 仍需归一化。

普通自定义消息的历史回放不依赖原扩展，不代表正在等待恢复的执行可以卸载所需代码；checkpoint 依赖的工具与转换规则仍按 M10/M11 固定。持久化应保留原始结构与未知数据，具体文件格式和演进策略集中放在 M10。

前端只消费公开数据与事件；本章不决定渲染实现。跨章验收见 [系统闭合检查](system-review.md)。
