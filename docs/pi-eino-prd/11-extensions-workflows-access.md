# 补充篇：扩展、工作流与前端接入

状态：完整需求草案。本篇承接十章主线以外、原方案已明确要求的插件、工作流和热更新能力，并落实前端只通过接口接入的边界。协议及扩展的最终代码接口在整套 PRD 闭合后制定。

## 1. 执行摘要

底座的价值在于增加业务能力时不修改模型循环。ResourceLoader 负责发现与读取，ExtensionRegistry 负责登记，CreateAgentSession 负责初始装配，AgentSession 负责运行中的激活时机；这些对象的职责按[第 2 章](02-architecture-boundaries.md#23-与-pi-对齐的对象名称与职责)区分。

能力由 L3 注册和选择，L2 接受已装配的工具、必要的消息转换规则和 handlers；HTTP/SSE、可选 A2UI 映射位于最外侧。一个 Trace 固定一个 generation，覆盖各 Turn、执行尝试、子调用和恢复所需资源。普通 CustomMessage 使用统一 content/details，不要求逐类型 codec。

本篇成功标准：扩展可登记能力、按明确边界请求运行操作并参与会话生命周期；同一 Trace 内可选择已固定版本的工具，新增或替换实现不改半途执行；声明式工作流可动态加载，同一流程在两种 Agent 使用方式下行为一致；Web 客户端通过 AgentSession 操作会话，不直接改变执行状态或历史文件。

### 1.1 参考依据

- pi 将注册方法与执行动作分开：工具/命令注册写入扩展对象，运行操作委托 runtime。[扩展 API](../../pi/packages/coding-agent/src/core/extensions/loader.ts#L252) `[VERIFY: pi/packages/coding-agent/src/core/extensions/loader.ts:252]`。
- pi 的扩展运行操作包括发送消息、追加私有条目和工具/模型选择；会话 before hooks 返回取消或候选结果。[运行操作](../../pi/packages/coding-agent/src/core/extensions/types.ts#L1325) `[VERIFY: pi/packages/coding-agent/src/core/extensions/types.ts:1325]`；[会话 hook 结果](../../pi/packages/coding-agent/src/core/extensions/types.ts#L1124) `[VERIFY: pi/packages/coding-agent/src/core/extensions/types.ts:1124]`。这些是职责参考，产品生效/持久化边界仍按本 PRD。
- Eino 区分运行前调整实际工具与模型前调整 ToolInfos，后者支持逐次模型调用的工具选择；toolsearch 提供 Agentic 泛型入口。[工具调整](../../eino/adk/chatmodel.go#L386) `[VERIFY: eino/adk/chatmodel.go:386]`；[toolsearch](../../eino/adk/middlewares/dynamictool/toolsearch/toolsearch.go#L34) `[VERIFY: eino/adk/middlewares/dynamictool/toolsearch/toolsearch.go:34]`。
- Eino DeepAgent 的默认子 Agent 会继承工具和 handlers；显式子 Agent 通过 AgentTool 调用。[装配](../../eino/adk/prebuilt/deep/task_tool.go#L85) `[VERIFY: eino/adk/prebuilt/deep/task_tool.go:85]`。
- DeepAgent `task` 工具使用 `subagent_type` 与 `description`，随后包装为 request 字符串，不天然接受每条工作流的强类型入参。[task 输入](../../eino/adk/prebuilt/deep/task_tool.go#L151) `[VERIFY: eino/adk/prebuilt/deep/task_tool.go:151]`。
- Eino Workflow 编译成 Runnable；工具内部组合中断需要显式转交其状态，而不是将 Interrupt 当作普通错误文本吞掉。[Workflow](../../eino/compose/workflow.go#L82) `[VERIFY: eino/compose/workflow.go:82]`；[CompositeInterrupt](../../eino/components/tool/interrupt.go#L100) `[VERIFY: eino/components/tool/interrupt.go:100]`。
- Coze Studio 将 Canvas JSON 反序列化后转换为 schema，再创建并编译 Eino Workflow 执行，不需要为每条业务流生成 Go 源码。本产品复用该格式语义，不引入整个 Coze 后端。[执行入口](../../coze-studio/backend/domain/workflow/service/executable_impl.go#L79) `[VERIFY: coze-studio/backend/domain/workflow/service/executable_impl.go:79]`；[Canvas 转换](../../coze-studio/backend/domain/workflow/internal/canvas/adaptor/to_schema.go#L66) `[VERIFY: coze-studio/backend/domain/workflow/internal/canvas/adaptor/to_schema.go:66]`；[图装配](../../coze-studio/backend/domain/workflow/internal/compose/workflow.go#L82) `[VERIFY: coze-studio/backend/domain/workflow/internal/compose/workflow.go:82]`。
- Eino Quickstart 将 A2UI 放在业务 UI 协议层；[A2UI 官方说明](https://a2ui.org/)也将其作为可由不同客户端渲染的 UI 描述机制。传输与渲染职责不属于模型循环。[Eino Quickstart](https://www.cloudwego.io/zh/docs/eino/quick_start/)

## 2. 用户体验与功能

### 2.1 插件作者与使用者

作为扩展作者，我希望提供工具、命令能力、消息类型、hooks、子 Agent 或声明式工作流，并知道可访问的上下文和生命周期；作为用户，我希望知道哪些扩展已加载、来自何处、是否已经生效。

| 能力 | 注册信息至少包含 | 归属 |
| --- | --- | --- |
| 工具 | 稳定名称、描述、schema、执行能力、作用域/副作用说明 | M05 |
| 自定义消息 | 通用 customType、模型可用 content、应用 details 与 display；只有特殊结构才登记转换规则 | M06 |
| 观察订阅 / 控制 hook | 事件或阶段、作用范围、错误行为 | M07；两类不能混为同一个 API |
| skill | 名称、描述、可加载位置/资源 ID、加载工具、正文与引用基址、来源版本；自动可见清单与实际加载能力一致 | M08；未装配加载器时不宣称可自动使用 |
| 子 Agent | 名称、用途、输入边界、工具范围、恢复能力 | 本篇与 M03 |
| 工作流 | 名称、描述、来源/格式版本、输入/输出 schema、节点与资源绑定、恢复与副作用声明 | 本篇 |
| 应用命令 | 参数/结果、执行权限与会话影响 | 由 AgentSession 协调调用；不要求前端使用 slash 文本 |

- **EXT-01**：一期可执行插件采用已编译 Go 包注册。注册新 Go 代码需要构建；运行中可重载配置、skill 资源及启停已存在能力。
- **EXT-02**：注册期检查重复名称、缺依赖、非法 schema 和保留名称冲突。普通注册默认拒绝重名并标明来源；显式工具替换按 2.5 核对目标和权限并形成候选版本，不靠加载顺序覆盖。资源文件覆盖规则仍由 M08 定义。
- **EXT-03**：ResourceLoader 返回候选资源与诊断，ExtensionRegistry 校验并登记能力；CreateAgentSession 装配初始版本，AgentSession 决定运行期间的激活时机。加载器和注册表不导入 AgentSession，不在加载/登记时提交任务或启动外部副作用。
- **EXT-04**：可查询 active generation、pending generation、各扩展来源与加载错误；“登记成功”和“已对当前任务生效”必须区分。
- **EXT-05**：同进程扩展使用宿主信任模型；业务权限控制工具调用，但不宣称能隔离任意 Go 插件代码。进程外插件协议与市场不自动纳入当前范围。

### 2.2 工作流的两种使用方式

作为用户，我希望主 Agent 能委派已装配的工作流，也能在对话框中选择该工作流作为独立 Agent 直接执行，并获得相同的参数校验和权限约束。

工作流保存为声明式数据，运行时由本项目节点执行器编译为 Eino 可执行图，再统一适配为 `WorkflowAgent`。不提供第三种 workflow-as-tool 产品入口；`task` 只是子委派的实现机制，不是单独的业务工具使用方式。

| 使用方式 | 调用者/身份 | 输入与结果 |
| --- | --- | --- |
| 主 Agent 的子 Agent | 主 Agent 通过 task 委派，产生子 invocation | 委派描述转换为工作流参数后再次校验；不能跳过缺参确认；摘要回到原父调用 |
| 对话框选择的独立 Agent | 用户为下一条新请求指定目标 Agent，创建顶层 Trace | 沿用当前 Session；跳过主模型路由；内部模型节点照常计数；结果直接进入该对话，不伪造父工具消息 |

- **WF-01**：同一工作流共享一个规范化输入/输出契约，不能两种使用方式各有一套字段意义；缺参或非法参数在产生副作用前拒绝或提出结构化补参交互。
- **WF-02**：独立选择与普通任务服从同 Session 的单写入者约束；在主任务活动时排队，不能旁路 Invoke 同时写历史。更换目标后的新请求不能被隐式映射为旧 Trace 的 follow-up。
- **WF-03**：工作流失败需提供失败节点、错误类别、已完成步骤和可确认副作用；不把其中一步失败折成“整体成功”。
- **WF-04**：子 Agent 方式返回的模型摘要和产品详细结果引用同一执行身份。富 `workflowResult` 不再伪造第二条未对应请求的 tool result。独立方式没有父工具请求，不生成虚假 FunctionToolResult。
- **WF-05**：支持中断的工作流必须向上保留中断点和恢复状态，等待用户后恢复到正确位置；没有经过恢复验证的工作流只声明不可恢复。采用 Eino 不等于任意图可恢复。
- **WF-06**：工具/节点副作用需显式定义可重试性与幂等键；恢复不能从头再次报销、发送或写账。系统不承诺跨任意第三方 API 的恰好一次语义。

独立 Workflow Agent 使用 Trace 记录完整执行，但不会自动获得开放式 Agent 的自由文本续轮能力。未声明支持 steering/follow-up 时按 M03 明确拒绝；独立 prompt 可排队等待当前 Trace 完成。作为子调用时，顶层两类输入仍由主 Agent 的循环消费。

自然语言首次输入通过声明的字段映射或获准的参数提取步骤转换；首次输入适配与运行中的自由文本干预是不同能力。工作流只读取显式输入及允许的上下文，不自动把全部聊天历史注入所有节点。选择变化不影响已经受理、排队或正在恢复的任务。

### 2.2.1 动态定义、来源适配与节点范围

作为扩展作者和接入方，我希望加载声明式工作流后即可运行，不必为每条业务流程重新编译 Go；新增节点执行器代码仍需重新构建。当前不提供可视化编排界面。

- **WF-07**：工作流定义与可执行实例分离。定义保存标识、版本、输入输出、节点配置、字段引用、控制依赖、分支和资源引用，不包含 Go 函数或运行实例。编辑器布局不参与执行语义。校验通过后，使用宿主已编译的可信节点执行器构造 Eino 图。
- **WF-08**：首个来源适配器读取指定版本的 Coze Canvas JSON。其他使用 Eino 的平台后续增加适配器，不假定共享数据格式。当前承诺指定 Canvas 子集；原始导出包需用真实文件单独认证，不能把 Canvas JSON 跑通写成任意 Coze 导出包可直接运行。
- **WF-09**：首批支持开始/结束、字面量与节点输出引用、基础大模型节点、条件分支，以及本地固定版本的子工作流引用。本项目已登记工具可作为受控节点；Coze 插件引用只有显式绑定后才可执行。循环/批处理、代码、HTTP、知识库和平台全局变量列为后续能力。未知节点、未知执行语义或缺资源绑定给出节点级诊断并阻止激活。先检查全部原始节点与边，再做允许的孤立节点修剪；未连线未知节点也不得被静默丢弃。节点 ID 重复、缺少或重复开始/结束、悬空引用、字段类型不相容、非法分支/必需输入缺失、首批图或子流程依赖成环均给出节点/边/字段级诊断并拒绝激活。
- **WF-10**：模型、工具、子流程和凭据使用本地受信绑定；外部文件中的资源 ID、代码或权限字段不直接取得宿主能力。导入、校验和编译成功后形成候选 generation，供下一次独立输入受理时选定；失败保留旧版。受理时一致保存目标及定义、子流程依赖、节点执行器与资源绑定版本，排队、执行和恢复均不重新取最新版。

现有 Go 代码登记方式可以保留，但不再是加载工作流的唯一方式。不嵌入整个 Coze 服务平台，也不在运行时编译任意 Go 源码。

### 2.3 热更新

作为用户，我希望运行中增加 skill 或启用已编译插件时，当前任务稳定执行，新任务再使用更新。

**EXT-06**：generation 整体验证；在新独立输入受理时选定并保存，整个 Trace 固定原版本，包括排队（含暂停自动启动的队列）、steering、follow-up、子 Agent 和恢复。只有随后受理的独立 Trace 可以采用新 generation；排队转执行、ContinueQueue、重启与内层自然停下消费 follow-up 均不构成升级边界。

**EXT-07**：版本冻结覆盖实际工具实现、参数 schema、handlers、skill 正文/引用资源、系统/项目指令快照，以及工作流定义、子流程依赖、节点执行器与资源绑定版本。不能只冻结工具名字、Agent 指针或工作流名称。在固定版本内选择本轮工具子集属于执行状态，按 2.5 与 M05 在 Turn 边界生效，不构成热更新；能力实现整体启停/替换仍走候选 generation。版本固定只针对指令和能力资源，不冻结正在读改的业务工作区文件。

**EXT-08**：新版本构建失败时保留旧版本，并明确报告未生效；如果初次启动就无可用版本则返回不可执行。被已受理 Trace（包括 queued）或 checkpoint 引用的版本需要保留或能按清单重建；缺失时阻止执行/恢复，不能换成同名新版。

**EXT-09**：正常 reload 不取消当前任务。用户另行明确撤销权限/紧急禁用能力时，必须中止或拒绝相应后续操作；不能借“旧版本继续有效”无视权限撤销。

扩展声明执行的是工具还是直接进程/文件能力，受控效果统一经 M05 的 Operations 后端与[DSH 式安全接口](12-security-sandbox.md)处理。已编译 Go 插件仍是信任代码；不能通过把它命名为“安全扩展”就宣称其任意系统调用都经过审核或沙箱。工作流和子 Agent 内层工具仍有独立审批/审计身份。

Eino skill backend 的 `Get` 是执行时读取入口，因此单纯延迟构建 `deep.NewTyped` 不保证旧内容。[Backend](../../eino/adk/middlewares/skill/skill.go#L66) `[VERIFY: eino/adk/middlewares/skill/skill.go:66]`；[读取](../../eino/adk/middlewares/skill/skill.go#L419) `[VERIFY: eino/adk/middlewares/skill/skill.go:419]`。

### 2.4 开发者通过代码添加子 Agent

已确认的使用方式是：开发者实现或创建一个符合执行接口的 Agent，再通过类似 `AddSubAgent(agent)` 的方法注册。终端用户使用已装配能力，不需要维护子 Agent 定义或新增能力的配置文件；当前也不提供可视化工作流编排界面。声明式工作流由 ResourceLoader 加载、适配并登记后，同样以 Agent 契约使用。

调用形态示意，名称和签名不视为已经实现的 API：

```go
// API 形态示意：登记能力，然后组装会话，最后发起对话。
// customAgent 由开发者创建；省略错误处理及其他初始化选项。
extensions := NewExtensionRegistry()
err := extensions.AddSubAgent(customAgent)
session, err := CreateAgentSession(ctx, SessionOptions{Extensions: extensions})
err = session.Prompt(ctx, "使用已注册能力完成任务")
```

最终 Go 签名在开发方案阶段确定。`AddSubAgent` 归本产品的 ExtensionRegistry；对话操作归 AgentSession，不在会话控制器上混入全部注册方法。

子 Agent 复用 `adk.TypedAgent[*schema.AgenticMessage]`；有中断恢复需求时满足对应的 `TypedResumableAgent[*schema.AgenticMessage]` 及产品恢复约束。主/子 Agent 的消息类型必须一致，不将旧 `adk.Agent` 别名直接混入 Agentic DeepAgent。pi 的 ExtensionAPI 提供工具等注册机制，本篇引用其职责划分；`ExtensionRegistry.AddSubAgent` 是本产品拟定接口，不作为 pi 现有 API 引用。Eino 在构造配置中接收同类型的 `SubAgents` 列表。[Agent 接口](../../eino/adk/interface.go#L453) `[VERIFY: eino/adk/interface.go:453]`；[可恢复接口](../../eino/adk/interface.go#L481) `[VERIFY: eino/adk/interface.go:481]`；[SubAgents 配置](../../eino/adk/prebuilt/deep/deep.go#L60) `[VERIFY: eino/adk/prebuilt/deep/deep.go:60]`。

- **EXT-10**：开发者向 ExtensionRegistry 注册子 Agent 的名称、用途与执行实例；创建函数将登记结果装配给 Agent，不要求修改循环或生成终端用户配置。
- **EXT-11**：声明式工作流经来源适配、校验和编译后，由 `WorkflowAgent` 适配成相同 Agent 契约登记。同一实例可用于主 Agent 委派，也可作为对话框可选的独立 Agent。不另建用户侧工作流配置系统，也不把工作流再作为第三种业务工具入口暴露。
- **EXT-12**：AddSubAgent 成功不立即改变活动 Trace。创建前登记由 CreateAgentSession 装配；运行中登记形成候选，在下一独立输入受理时选定。已受理 queued 项及旧 Trace 的后续 Turn、follow-up 和恢复继续使用原版本。

注册的层级关系是上层提供实现、下层按接口执行。子 Agent 实现不依赖 AgentSession 或 ExtensionRegistry；测试页面调用 AgentSession 使用已装配能力，不负责创建代码实例或编辑执行限制。

### 2.5 动态工具：登记、替换与选择

扩展登记的是可装配能力，运行时选择的是本轮可用集合。以下名称仅表达 API 职责，最终 Go 签名后定。

| 操作 | 负责入口 | 生效边界 |
| --- | --- | --- |
| RegisterTool / 登记新工具 | ExtensionRegistry 接收实现或已有执行器创建的实例，校验名称、schema 和来源 | 创建前直接装配；运行中形成候选 generation，下一独立输入受理时选定 |
| ReplaceTool / 显式替换工具 | 登记期明确目标名称、被替换来源/版本和新实现；仅应用允许替换的工具可替换 | 同样形成候选 generation；保留旧 Trace 所需实现，不能把不同来源重名当作替换 |
| GetAllTools / GetActiveTools | 扩展运行上下文只读查询 | 分别返回当前 generation 的获准候选清单和当前 Turn 生效选择；pending generation 另列，不能混成已可用 |
| SetActiveTools / 选择工具集合 | 扩展提交作用域明确的选择请求，AgentSession 协调 L2 应用 | 首次模型请求前或当前工具批次结算后的下一 Turn 生效；未知名称、越权或来自其他 generation 的名称拒绝，原集合保留 |

**EXT-13**：模型本轮使用的工具选择在调用前固定，模型重试、工具批次及审批恢复沿用同一选择；普通启停不改变已经接纳的工具调用。当前明确的权限撤销仍立即约束后续实际执行，不能用历史选择绕过。工具选择的完整规则与 Eino 适配以 [M05](05-tool-system.md#动态工具选择与实现版本) 为准。

**EXT-14**：支持“先搜索，再开放工具”：从当前 generation 已登记且获准的工具中返回候选，再为后续模型请求开放。搜索结果不注册新代码、不扩大权限；同批次中其他调用不能借搜索结果立即使用尚未对本轮开放的工具。供应商原生延迟工具机制只在 M04 已认证时启用。

**EXT-15**：工具选择按 Trace/invocation 隔离，并保存生效 Turn、选择内容和来源；两个 Session、父子 Agent 不能互改集合。多个选择请求由会话协调者按受理顺序处理，每次以最新候选结果继续校验；后请求替代尚未生效的选择时注明关系，未生效或失败可查询；具体状态引用和 checkpoint 关联由 M10 规定。

例如分析模式只开放 read/search，确认后申请开放同一 generation 中的 edit/write；下一 Turn 同时更新本轮携带的工具定义清单、工具说明和预算，单个工具的 schema 版本不变，实际写入仍走 M05 的授权。此时不需要编译或更换 generation。若需要加入新的执行逻辑，则继续采用 EXT-01 的已编译 Go 包方式。

### 2.6 扩展可使用的运行操作

ExtensionRegistry 负责登记；运行中的动作由注入的扩展上下文请求，交给 AgentSession 协调。扩展上下文是窄能力的集合，不是另一套会话控制器，不要求下层依赖 AgentSession 的具体类型。

| 操作组 | 扩展能做什么 | 明确边界 |
| --- | --- | --- |
| 读取上下文 | 查当前 Session、分支、可用身份、generation、模型/思考状态、工具选择、上下文用量及只读历史 | 返回作用域内视图；空闲/会话操作没有 traceId/turnId 时允许缺省，不伪造身份 |
| 发送自定义消息 | 提交 M06 CustomMessage 的 content/details/display，选择是否触发执行及投递时机 | 记录扩展来源；不冒充真实用户授权，不直接追加到正在发送的模型请求 |
| 提交输入 | 提交新的独立 prompt，或定向 steering/follow-up | 沿用 M03 输入类别、inputId 去重和消费边界；忙时不隐式猜测类别，终态 Trace 不复活 |
| 保存扩展状态 | 顶层追加带扩展命名空间/customType 的 CustomEntry；子调用保存带 invocationId 的关联状态；读取相应记录恢复 | 按 M10 的选定路径/提交位置重建；不进入模型，不直接取得 SessionStore |
| 选择模型/思考级别 | 设置下一独立 Trace 的默认值，或按当前 Trace 已装配策略请求下一 Turn 的获准值 | 两种作用域必须区分；按 M04 校验并保存实际生效位置，不覆盖正在调用的模型或旧 checkpoint |
| 选择工具 | 查询候选/生效集合并请求 SetActiveTools | 遵循 2.5，不直接修改共享工具 map，不绕过工具授权 |
| 请求控制 | 请求 abort；请求当前 Trace 在下一安全边界压缩，或空闲时手动压缩 | abort 沿用 M03；压缩由现有 M09 管道处理，不能在回调中同步执行第二套循环 |
| 会话命令 | 创建/打开/切换会话、同会话分叉/导航、名称或已有元数据操作 | 由专门命令上下文提供；创建必须绑定工作区，维护先满足 M10 空闲/队列/副作用限制，再走 M07 生命周期 |
| 继续队列/核对效果 | 显式恢复所选 queued 项的调度，或对未决调用发起只读查询/提交材料 | 由应用授权的受控入口协调，核对允许针对 paused/终态未决效果；不重跑原调用，不凭人工标签恢复执行或解除一次许可 |

**EXT-16**：发送自定义消息默认不触发模型：活动执行中，投递到该 invocation 下一安全模型输入边界；空闲时可持久追加供下一次输入使用。有明确触发意图时，空闲创建独立 Trace，忙时明确选择 steering 或 follow-up。投递与原始用户输入分开标识；尚未消费的模型可见消息不参与当前调用或摘要。Trace 结束仍未投递的消息记录原因并保留原归属，需要调用方明确另行投递，不自动转入其他 Trace。M06 的 display 与模型可见性规则保持不变。

**EXT-17**：写操作返回可查询的受理结果及身份；只有受理记录已保存才报告 accepted，只有最终提交完成才报告“已保存/已生效”。hook 中提交后不等待必须在当前 hook 退出后才能完成的执行；顺序由协调者保证，不重入 Runner。数据转换与会话维护 before hook 通过返回值贡献消息/候选，除请求 abort 外不另发同作用域写操作或维护命令，以免改变已固定材料；需要保存的独立状态由发起命令先完成提交，或在操作提交后处理。hook 失败不回滚此前独立确认受理的操作，未提交的转换候选则丢弃。

**EXT-18**：ExtensionContext 提供当前作用域的读取和上述受控运行操作；ExtensionCommandContext 在应用命令执行时额外提供会话维护能力。需要空闲的命令在忙时返回 conflict，不能在当前工具/hook 内等待自己结束。M07 核对是针对未决调用的独立受控操作，按其专门状态规则受理；这不允许 hook 递归执行自身或直接改审批/结果。子 Agent 上下文不取得父队列消费权或主会话写入权；跨主/子作用域的消息经已声明的委派返回或父级入口处理。

**EXT-19**：扩展状态使用 M10 追加条目及恢复规则，带扩展来源、作用域和必要的数据版本。压缩不丢这些条目；恢复 checkpoint 时读取其关联提交位置的状态，不取文件最后值或其他分支值。内存缓存可从持久记录重建；不要求每个 customType 新建 codec、数据库或状态管理框架。

命令登记至少包含名称、参数/结果契约、所需能力和是否要求空闲。命令调用先校验、去重，再执行处理器；SDK 与 HTTP 使用相同语义。支持 slash 文本时它只是接入映射，扩展主动发送的普通文本默认不自动解析为管理命令。

参数改写使用 M07 prepareArguments 阶段，全部转换后由 M05 最终校验并冻结；tool_call 阶段只读描述、返回决定，普通 allow 不越过 M12 强制策略和许可占用。扩展输入转换保留原输入及来源，不产生新的人类授权；即使与安全审核共用模型或 handler 装配，也不得用主模型的派生上下文替换审核所需的受信原文。

### 2.7 会话生命周期扩展

会话控制由 L3 AgentSession 及其应用协调入口触发，模型/工具阶段由 L2 适配 Eino；处理器不能通过反向依赖接管会话。完整事件、组合和提交顺序统一定义在 [M07 会话生命周期 hooks](07-events-and-access.md#423-会话生命周期-hooks)。

**EXT-20**：支持会话就绪通知、切换前检查、分叉/导航前取消或摘要定制、压缩前取消/候选替换，以及提交后的结果通知和关闭清理。这里的 fork 是 M10 同 Session 分支；与 pi 复制为新会话的 fork 语义差异需明确，不因此创建新 Session 或 Trace。

**EXT-21**：before hook 获得已校验的目标、来源范围和必要状态，返回继续、取消或该阶段允许的候选；取消/异常不提交目标变更。替代摘要仍满足 M09 的来源、结构、预算、文件事实与一致提交规则。after 通知在提交后发生，返回值不能追溯撤销已提交事实。

**EXT-22**：hooks 不得阻止明确的用户取消或权限撤销。关闭通知用于清理，关键状态必须已走持久入口；崩溃不保证触发关闭 hook。SSE 断开、Trace 完成或 waiting_input 都不等于 Session 关闭。

## 3. AI 系统需求

DeepAgent 的主模型负责选择工具和委派；用户在对话框中明确选择独立 Workflow Agent 时跳过该选路。Workflow 固定其节点依赖。工作流描述应明确适用条件和必填参数，不以“有已注册流程就永远优先”为由强迫不匹配任务进入业务流程。用户选择独立 Agent 后，沿用当前 Session；每次新 Trace 记录目标 Agent，界面改选不影响已受理请求。

保留已确认的 general-purpose 子 Agent。它和专家 Agent 使用父 Trace 的权限、预算、generation 和取消范围，子 Agent 的可用能力不高于父任务许可。父任务的 steering 和历史提交权不能被子执行共享消费。

已确认默认装配工作区内文件读写/搜索、命令执行、write_todos 与 general-purpose 子 Agent；提示词说明实际基础能力并允许应用替换，不将编码设为唯一任务类型。CreateAgentSession 负责装配与校验相应后端，缺失不静默裁剪；开发者可显式定制。能力清单及实际可注册子 Agent 名称来自当前 generation，模型编造的名称返回明确错误。

评估必须分别验证任务路由质量与适配执行正确性：可控模型选定工作流验证参数/事件/恢复；真实模型任务集验证是否选择正确流程。子 Agent 的内部细节默认不全部注入主对话，主任务拿到可验证摘要与产物引用。

## 4. 技术规格

### 4.1 扩展生命周期与层级

ResourceLoader 加载/发现 → 原始数据校验/来源适配 → ExtensionRegistry 校验与编译候选 → 新独立输入受理时由 AgentSession 原子选定已验证版本并保存引用 → 串行执行固定版本 → 任务与 checkpoint 释放引用后可回收。初次启动由 CreateAgentSession 完成装配。具体资源释放和版本保留实现后续制定；已受理排队项也持有版本引用，旧任务不能读到已卸载的半套实现。

L2 定义执行需要的窄契约，L3 插件实现这些契约。CreateAgentSession 和 AgentSession 分别承担初始化、运行中的协同；ResourceLoader 读取资源，ExtensionRegistry 登记能力，Agent 只消费执行配置。框架 hooks 经 L2 适配到产品阶段；保存由 SessionManager 承担，权限由应用提供策略实现，不形成反向 import。

事件扩展采用 M07 的两条管道：观察订阅只读、可注销、返回值不改变执行决定；扩展控制 hook 对应 pi.on 的职责，在固定阶段等待决定或转换结果。每个 Trace 固定处理器快照和顺序；空闲会话维护操作在受理时固定其快照，单次操作不混用版本。handler 使用 2.6 的受控操作入口，不直接写 SessionStore；具体事件、组合和上下文边界以 M07 的 4.2.1～4.2.3 为唯一来源。

### 4.2 前端接入操作面

下表定义能力，尚不是冻结的 REST 路由或 Go 签名：

| 能力组 | 操作 | 必须表达的语义 |
| --- | --- | --- |
| 会话 | 创建、列表、详情、历史、分支读取/切换 | 活动分支、工作区、版本、可继续状态 |
| Trace 与输入 | 提交、查询、取消、继续独立运行队列 | inputId 去重、traceId 归属；follow-up 不是新 Trace；取消后的旧队列暂停，新 prompt 不顺带启动它 |
| 当前 Trace 干预 | steer、follow-up、交互回复、恢复 | traceId/interactionId；不能用普通消息代替授权 |
| 未决效果 | 发起核对、提交材料、查询结论/恢复资格 | operationId 关联原 invocation/toolCall；核对不等于重试，许可和 checkpoint 条件仍独立检查 |
| 能力 | 列出已装配能力、可独立选择的 Agent、输入 schema、active/pending 与加载结果 | 注册/校验/登记更新通过开发者 SDK 完成；当前不要求前端提供子 Agent 创建或编排界面 |
| 工作流 | 查询定义、导入诊断、执行结果与节点状态 | 输入经 SubmitInput 指定目标 Agent；校验输入、顶层排队、执行身份与节点结果 |
| 事件 | 建立订阅、携带游标、恢复快照 | Session 范围，durableSeq、瞬态 chunk 与重放边界 |

HTTP/SSE 是目前落地建议。SSE 的 `id` / `Last-Event-ID` 只提供游标传输机制，服务端仍需实现 M07 的恢复语义；浏览器重连不会自动提供应用持久化保证。[HTML SSE 标准](https://html.spec.whatwg.org/multipage/server-sent-events.html)

### 4.3 断连、重复请求与版本

- 已受理任务由 AgentSession 持有并协调 SessionManager 保存；断开请求或订阅不能取消它。未完成受理的客户端重试使用原 inputId 查询/重发，避免重复输入或创建第二个 Trace。
- 订阅以 M07 的快照与 durableSeq 消除“查状态后再监听”之间的空档；允许重复投递但必须可去重。不承诺永久重放全部 token。
- 无效/过期游标要求客户端重新同步快照；慢客户端不能无限阻塞模型或吞掉关键状态。策略按 M07，页面只展示结果。
- API/事件 envelope 有版本，未知可选扩展事件可忽略但需可诊断；不识别的关键动作或消息模式必须拒绝，不能猜参数。
- 本地测试接口仍验证输入和访问范围，拒绝向任意第三方网页开放工具执行；公开远程多用户部署的身份与租户管理留到独立需求，不由本篇暗自扩展。

### 4.4 A2UI 边界

保持产品事件为核心通用语义；A2UI adapter 可以把适合富交互的结果映射为 UI 描述，把客户端动作映射为已有应用操作。移除 adapter 后文本与结构化结果仍能使用。

完整 A2UI 支持不是当前用户已要求的必交付实现。若选用，需要在开发方案前确定版本、支持的组件/动作集合和未知组件回退行为；所有动作经 AgentSession 的操作校验和注入的权限策略，不能由 UI 文本定义任意工具执行。测试页面不规定 React、Vue 或其他前端框架。

### 4.5 验收

| 编号 | 场景 | 通过条件 |
| --- | --- | --- |
| EXT-A01 | 注册计算工具、自定义消息和观察者 | 无需改核心循环；错误与未知消息按对应契约处理 |
| EXT-A02 | 注册重名工具/子 Agent 或非法 workflow schema | 候选版本不激活，错误指出名称及来源 |
| EXT-A03 | 任务执行时编辑 skill 与其引用文件并 reload | 旧任务看到旧内容，新任务看到新内容；原工作区业务文件仍实时可读写 |
| EXT-A04 | 等待交互时更新并停用插件，随后恢复 | 有旧版本则按原权限约束继续；缺失则明确不兼容，不调用替代实现 |
| EXT-A05 | 代码创建自定义 Agent 或 Workflow 适配 Agent，再调用 AddSubAgent | 主 Agent 可委派；无需用户填写子 Agent 清单或上限；重名/非法实例拒绝；运行中添加按既有版本边界生效 |
| WF-A01 | 同一短流程分别由主 Agent 委派和对话框选择独立 Agent 触发 | 同样 schema/结果；独立方式主模型选路次数为 0；独立结果无虚假父工具消息 |
| WF-A02 | 长流程通过 task 委派但字段缺失 | 在副作用前返回缺参或等待补参；自然语言 description 不绕过 schema |
| WF-A03 | 工作流第 2 步确认，确认后第 3 步失败 | 第 1 步不重放；等待/恢复/失败可关联同一 Trace 和节点 |
| WF-A04 | 导入指定版本 Coze Canvas 子集；另有孤立未知节点、悬空边/字段引用、类型错误、缺少开始/结束、成环或缺绑定样本 | 合法子集可供两种使用方式选择；非法样本在修剪/激活前诊断，含字段级原因，不因删孤立节点而放行 |
| WF-A05 | 活动和排队任务期间导入新定义或编译失败，再重启并继续队列 | 旧任务及已受理 queued 项保持原定义与绑定；引用不被回收；更新失败保留旧版；随后受理的独立 Trace 才可采用新 generation |
| WF-A06 | 主任务运行中改选独立 Agent 并发送新输入 | 新请求排队为独立 Trace，不误投为旧 Trace 的 follow-up；已受理目标不因界面改选变化 |
| WF-A07 | 无模型的“开始 → 本地工具节点 → 结束”流程，分别子委派和独立触发，并注入拒绝/恢复 | 节点通过绑定而非模型可见性校验；授权、预算、去重均生效；不伪造节点 Turn/模型工具请求；已有结果不重复执行 |
| WF-A08 | 向声明支持续输入的非主 Agent Trace 提交定向输入，分别省略、匹配、错配 targetAgent | 省略时继承原目标，匹配时按原 Trace 受理，错配时拒绝且不另建 Trace；未支持续输入的工作流仍拒绝 |
| API-A01 | 最小 Web 页面选择 Agent、提交、观察工具、取消、继续 | 仅公开接口；同一会话语义与 SDK 一致 |
| API-A02 | 断连重连、重复提交、慢消费者 | 无重复输入/Trace；durable 状态可恢复；无无限内存增长承诺漏洞 |
| EVT-A01 | 注册观察订阅后注销、关闭 Session、再次订阅 | 注销幂等；旧句柄不再收到新事件；关闭释放监听器和临时流；新订阅从快照/游标开始 |
| EVT-A02 | 注册两个 context/tool_result/input handler | 按顺序链式传递；转换失败阻止模型；tool_call block 短路；原始工具事实与历史不被改写 |
| EVT-A03 | hook 超时、普通异常、tool_call 异常 | 阶段和扩展身份可诊断；普通通知异常隔离；安全拦截异常 fail-closed；工具事实和 Trace 收尾不重复 |
| EXT-A06 | 同一 Trace 从只读切到读写，切换期间模型重试或等待审批 | 下一 Turn 才更新选择；当前请求/批次/恢复保留原选择，授权仍逐次验证 |
| EXT-A07 | 新工具登记、误用重名注册、明确替换及替换旧版本不匹配 | 普通重名和版本不匹配拒绝；明确替换成功只更新候选 generation，旧 Trace 使用原实现 |
| EXT-A08 | 搜索获准工具、搜索越权/未知工具、同批次试图调用新发现工具 | 只返回允许候选；普通搜索在后续 Turn 开放，同批次越界调用不执行；原生延迟机制单独认证 |
| EXT-A09 | 两个 Session 与父子 invocation 分别切换工具 | 生效集合、待生效选择及事件互不串线，子 Agent 不扩大父任务许可 |
| EXT-A10 | hook 发送 follow-up/自定义消息，随后重放相同请求 | 按身份只受理一次；无同步重入，未消费内容不进入当前模型/摘要；扩展来源不冒充人类授权 |
| EXT-A11 | 扩展状态跨分支、压缩及 checkpoint 恢复 | 状态从正确路径/提交位置重建，CustomEntry 不进入模型；旧版本缺失明确拒绝恢复 |
| EXT-A12 | 运行操作登记后失败、同 hook 内调用要求空闲的会话命令 | 已受理操作可查而不被假装回滚；命令返回 conflict，不出现自等待 |
| EVT-A04 | 切换/分叉/导航 before hook 取消或抛错 | 原 Session/游标/分支保持，结果明确；无虚假 after 成功通知 |
| EVT-A05 | 两个 before_compact handler 返回不同替代候选 | 返回候选冲突，原投影保留；取消/无效候选按预算规则处理，不静默选择一个 |
| EVT-A06 | 扩展返回合法/越界摘要，再在提交时注入失败 | 合法候选经 M09 校验后提交；越界或写入失败不更新摘要、文件 details 或活动游标 |
| EVT-A07 | before hook 期间 reload；提交后通知失败 | 一次操作使用同一处理器快照；通知失败只报告扩展错误，已提交操作仍成功 |
| EVT-A08 | 用户取消、权限撤销、断连、正常关闭与进程崩溃 | hook 不否决取消/撤销；断连不关闭 Session；清理有界，持久状态不依赖关闭回调 |
| API-A03 | 将富结果适配为 A2UI（若启用） | 原产品事件无需改动；动作仍经 AgentSession 校验；禁用 adapter 不影响执行 |

## 5. 风险与系统闭合

本篇把扩展和接入纳入完整系统，不提前划分开发迭代。必需工作区、默认基础能力、安全组合和取消后队列已确认；A2UI 仍为可选外层能力，具体协议集合、版本保留及实现参数由后续开发方案细化，见[路线决策清单](roadmap.md)。

开发方案前必须把 EXT-A03/04、WF-A03、API-A02 与 M03/M07/M10 的同一轨迹合并审查，确认谁负责状态、谁负责保存、谁负责恢复。不能分别通过“能调用工具”“能存 checkpoint”“能发 SSE”就宣布完整系统可用。
