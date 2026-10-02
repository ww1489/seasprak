# 补充篇：扩展、工作流与前端接入

状态：完整需求草案。本篇承接十章主线以外、原方案已明确要求的插件、工作流和热更新能力，并落实前端只通过接口接入的边界。协议及扩展的最终代码接口在整套 PRD 闭合后制定。

## 1. 执行摘要

底座的价值在于增加业务能力时不修改模型循环。Code Agent 的 ResourceLoader 发现与读取，ExtensionRegistry 登记，CreateAgentSession 初始装配，AgentSession 协调运行激活；独立 Workflow Agent 自己拥有定义、编译、Eino Graph、节点状态及生命周期。职责按[第 2 章](02-architecture-boundaries.md#23-与-pi-对齐的对象名称与职责)区分，独立 Workflow API、定义/编译/图所有者及 Web 三类资源路由已接通，有对应默认测试；具体签名、DTO 和最终认证分别见 [开发方案 06](../pi-eino-dev-plan/06-events-and-api.md#sdk)、[13](../pi-eino-dev-plan/13-p3-web-contract.md) 与 [P3 验证记录](../p3-verification.md)。

两类 L3 分别注册和选择自身能力，L2 接受已装配的工具、必要的消息转换规则和 handlers；HTTP/SSE、可选 A2UI 映射位于最外侧。Code Agent 一个 Trace 固定一个 generation，覆盖普通受控子调用与恢复；Workflow 的定义/绑定在自身运行内冻结，不与 Code Agent 共用 generation。普通 CustomMessage 使用统一 content/details，不要求逐类型 codec。

本篇成功标准：Code Agent 扩展通过明确入口登记、受控操作及会话生命周期进入系统；Workflow 静态图独立执行并保留子流程、条件、并行汇合和节点级暂停/显式恢复；业务直接调用或通过已有受控工具组合两类，Web 分别调用公开能力，不直接改状态或历史。动态定义、Coze 导入、热重载、多 generation 和补参仍在原未来阶段，本轮不启动 P4/P5，也不证明 P3 完成。

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
| 子 Agent | 名称、用途、输入边界、工具范围、恢复能力 | Code Agent 内部普通受控委派；本篇与 M03 |
| 工作流 | 名称、描述、来源/格式版本、输入/输出 schema、节点与资源绑定、恢复与副作用声明 | 独立 Workflow Agent 拥有，不经 Code Agent 的 ExtensionRegistry/AddSubAgent |
| 应用命令 | 参数/结果、执行权限与会话影响 | Code Agent 命令由 AgentSession 协调；Workflow 操作由其自身入口校验；不要求 slash 文本 |

- **EXT-01**：可执行插件采用已编译 Go 包静态注册，新增 Go 代码需要构建；配置/skill 重载、运行时启停已有能力仍保留在原未来阶段，本次不启动 P4/P5，也不将其描述为已交付。
- **EXT-02**：注册期检查重复名称、缺依赖、非法 schema 和保留名称冲突。普通注册默认拒绝重名并标明来源；显式工具替换按 2.5 核对目标和权限并形成候选版本，不靠加载顺序覆盖。资源文件覆盖规则仍由 M08 定义。
- **EXT-03**：Code Agent ResourceLoader 返回候选资源与诊断，ExtensionRegistry 校验并登记工具、skill、handler 和普通子 Agent；CreateAgentSession 初始装配，AgentSession 决定运行激活。Workflow 自身校验/编译定义和绑定，不由这些 Code Agent 对象管理。加载/登记不启动任务或副作用，不反向导入运行控制器。
- **EXT-04**：可查询 active generation、pending generation、各扩展来源与加载错误；“登记成功”和“已对当前任务生效”必须区分。
- **EXT-05**：同进程扩展使用宿主信任模型；业务权限控制工具调用，但不宣称能隔离任意 Go 插件代码。进程外插件协议与市场不自动纳入当前范围。

### 2.2 工作流的两种使用方式

作为用户，我希望由业务/Web 直接调用独立 Workflow Agent，也能让业务把对另一类 Agent 的调用包装为已有受控工具进行组合。两类是同级产品，互调不要求在 SDK 内登记为父子 Agent。

`seasprak-workflow-agent` 现由 `internal/workflowagent` 独立拥有单份定义、校验、编译、Eino Graph、节点状态和生命周期；`WorkflowAgent`、`WorkflowOptions`、`CreateWorkflowAgent`、`OpenWorkflowAgent` 已从唯一 SDK 入口接通，有对应默认测试及消费者，具体签名见开发 06/10，最终认证见验证记录。`seasprak-code-agent` 现由 `internal/codeagent` 承接原 sessions 的普通会话职责，旧内置工作流 target、节点执行/状态/恢复已退出；其运行对象仍是 AgentSession，`CreateAgentSession` / `OpenAgentSession` 保留。两类共用 storage 契约及 jsonl/memory，不共享历史写入者、审批、恢复、generation 或隐式预算。

| 使用方式 | 调用者/身份 | 输入与结果 |
| --- | --- | --- |
| 业务直接调用独立 Agent | Web/业务分别调用所选类型的公开入口；Workflow 使用自己的运行身份 | 跳过 Code Agent 主模型选路，按工作流输入/节点绑定校验；结果与节点状态属于 Workflow 自身，不写入 Code Agent 历史 |
| 业务经已有受控工具组合 | 业务工具从调用方已有工具接口调用被调方公开能力；可双向组合 | 仅显式参数和获准结果/引用跨边界；被调方独立校验、授权、预算和恢复。外层模型工具结果只配对原调用，不伪造被调方的父 task/invocation |

原 workflow-as-tool 禁令取消；允许业务工具封装，但 SDK 不新增专用跨 Agent 框架。Workflow 不作为 Code Agent 的内置子 Agent，也不沿用 AgentSession/targetAgent 执行。高级跨任务编排、审批/补偿、业务幂等、完整持久子树恢复和跨运行总预算由业务管理。

- **WF-01**：两种方式使用同一工作流规范化输入/输出契约；非法/缺失必需参数在副作用前拒绝。业务补参/参数提取仍在原未来阶段，不能借自然语言或工具包装跳过校验。
- **WF-02**：每类独立保存运行、队列与写入所有权；Code Agent 的单 Session 串行规则不把 Workflow 纳入同一写入者。界面改选类型由业务路由到另一公开入口，不将新请求作为旧 Trace 的 follow-up。
- **WF-03**：失败提供节点、错误类别、已完成步骤和可确认副作用；不将局部失败写成整体成功。
- **WF-04**：Workflow 详细结果引用其自身运行身份；业务需要摘要时显式生成调用方结果视图。直接调用不生成虚假 FunctionToolResult，工具封装只返回一个配对原工具请求的结果；被调方日志和历史不自动合并。
- **WF-05**：保留 Workflow 内部静态子流程、条件分支、并行汇合和已有节点级暂停审批/显式恢复；恢复由 Workflow 独立验证原节点状态、版本、checkpoint 与当前权限，不能凭 Code Agent 的 checkpoint 恢复整树。
- **WF-06**：各节点/工具仍声明副作用及可重试性，使用原调用身份和已有结果防止盲重放。跨任务业务幂等与补偿由业务实现；未知效果先核对，不承诺任意第三方恰好一次或跨 Agent 事务。

Workflow 不自动具有开放式 Agent 的 steering/follow-up 语义，也不读取 Code Agent 队列。未支持的自由文本续输入明确拒绝；已有节点审批/交互按自己的恢复入口处理。暂停、恢复与取消不转移给另一类运行对象；取消须等待实际执行退出，工具包装返回或超时不能证明被调方已停止。

工作流只读取显式输入及获准上下文，不自动注入另一类的聊天历史、审批或授权原文。业务工具不是授权转移通道，被调方使用独立策略；外层 allow、批准或冻结参数不能代替内层调用的校验、参数冻结、票据、预算及授权。

### 2.2.1 动态定义、来源适配与节点范围

静态定义、编译、静态子流程、条件/并行汇合及已有节点暂停审批/显式恢复归独立 Workflow Agent。下列 Coze 来源、运行时动态加载、补参与多 generation/热重载要求保留在原未来阶段（主要 P5；资源/Skills 仍按 P4），本轮不启动，也不作为已经交付的 API。新增节点执行器代码仍需重新构建；不提供可视化编排界面。

- **WF-07**：工作流定义与可执行实例分离。定义保存标识、版本、输入输出、节点配置、字段引用、控制依赖、分支和资源引用，不包含 Go 函数或运行实例。编辑器布局不参与执行语义。校验通过后，使用宿主已编译的可信节点执行器构造 Eino 图。
- **WF-08**：首个来源适配器读取指定版本的 Coze Canvas JSON。其他使用 Eino 的平台后续增加适配器，不假定共享数据格式。当前承诺指定 Canvas 子集；原始导出包需用真实文件单独认证，不能把 Canvas JSON 跑通写成任意 Coze 导出包可直接运行。
- **WF-09**：首批支持开始/结束、字面量与节点输出引用、基础大模型节点、条件分支，以及本地固定版本的子工作流引用。本项目已登记工具可作为受控节点；Coze 插件引用只有显式绑定后才可执行。循环/批处理、代码、HTTP、知识库和平台全局变量列为后续能力。未知节点、未知执行语义或缺资源绑定给出节点级诊断并阻止激活。先检查全部原始节点与边，再做允许的孤立节点修剪；未连线未知节点也不得被静默丢弃。节点 ID 重复、缺少或重复开始/结束、悬空引用、字段类型不相容、非法分支/必需输入缺失、首批图或子流程依赖成环均给出节点/边/字段级诊断并拒绝激活。
- **WF-10**：未来 Workflow 导入的模型、工具、子流程和凭据使用本地受信绑定，外部 ID/代码/权限字段不直接取得宿主能力。Workflow 自身导入、校验、编译形成候选 generation，在自己的下一独立运行受理时选定，失败保留旧版；保存定义、依赖、节点执行器与绑定版本，排队、执行、恢复不取最新版。Code Agent 注册/受理或业务工具版本不替它选版。

现有 Go 代码登记方式可以保留，但不再是加载工作流的唯一方式。不嵌入整个 Coze 服务平台，也不在运行时编译任意 Go 源码。

### 2.3 热更新

作为用户，我希望运行中增加 skill 或启用已编译插件时，当前任务稳定执行，新任务再使用更新。

**EXT-06**：Code Agent generation 整体验证，在新独立输入受理时选定并保存；整个 Trace 的排队、steering、follow-up、普通子 Agent 和恢复固定原版本，ContinueQueue/重启不升级。Workflow 自己在独立运行受理时冻结版本；业务工具调用不继承调用方 generation。动态热更新与多 generation 管理仍在原未来阶段。

**EXT-07**：Code Agent 冻结实际工具实现、schema、handlers、普通子 Agent、skill 正文/引用资源及系统/项目指令。Workflow 自己冻结定义、子流程依赖、节点执行器及资源绑定，双方不串用引用。业务封装工具只固定自己的实现与参数，不替被调方选择/冻结版本。当前 generation 内选择本轮工具子集不构成热更新；不冻结正在读改的业务工作区文件。

**EXT-08**：新版本构建失败时保留旧版本，并明确报告未生效；如果初次启动就无可用版本则返回不可执行。被已受理 Trace（包括 queued）或 checkpoint 引用的版本需要保留或能按清单重建；缺失时阻止执行/恢复，不能换成同名新版。

**EXT-09**：正常 reload 不取消当前任务。用户另行明确撤销权限/紧急禁用能力时，必须中止或拒绝相应后续操作；不能借“旧版本继续有效”无视权限撤销。

扩展声明执行的是工具还是直接进程/文件能力，受控效果统一经 M05 的 Operations 后端与[DSH 式安全接口](12-security-sandbox.md)处理。已编译 Go 插件仍是信任代码；不能通过把它命名为“安全扩展”就宣称其任意系统调用都经过审核或沙箱。工作流和子 Agent 内层工具仍有独立审批/审计身份。

Eino skill backend 的 `Get` 是执行时读取入口，因此单纯延迟构建 `deep.NewTyped` 不保证旧内容。[Backend](../../eino/adk/middlewares/skill/skill.go#L66) `[VERIFY: eino/adk/middlewares/skill/skill.go:66]`；[读取](../../eino/adk/middlewares/skill/skill.go#L419) `[VERIFY: eino/adk/middlewares/skill/skill.go:419]`。

### 2.4 开发者通过代码添加子 Agent

本节仅为 Code Agent 普通 general-purpose/专家子 Agent 的代码登记方式：开发者实现或创建符合执行接口的 Agent，通过类似 `AddSubAgent(agent)` 的方法注册。终端用户不维护子 Agent 定义或执行限制配置。独立 Workflow 自己加载/编译定义，不经 Code Agent ResourceLoader、ExtensionRegistry 或 AddSubAgent；业务封装工具也不建立 SDK 内置父子关系。

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
- **EXT-11**：Workflow 在独立 `internal/workflowagent` 目标包校验/编译 Eino Graph，并由自身生命周期运行；不经 AddSubAgent，不复用 AgentSession/targetAgent。业务可直接调用或用已有受控工具接口封装，两类互不导入，SDK 不新增专用跨 Agent 框架。
- **EXT-12**：AddSubAgent 成功不立即改变活动 Trace。创建前登记由 CreateAgentSession 装配；运行中登记/候选热激活仍属原未来阶段，届时在下一独立输入受理时选定。已受理 queued 项及旧 Trace 的后续 Turn、follow-up 和恢复继续使用原版本；普通静态子 Agent 委派保留。

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

本节及 2.5/2.7 的扩展运行操作、会话 hooks 属于 Code Agent；Workflow 独立管理其运行/节点生命周期，不通过这些入口修改定义、状态或恢复。ExtensionRegistry 负责登记，运行动作由注入的扩展上下文请求并交 AgentSession 协调；该窄上下文不成为另一会话控制器，也不要求下层依赖其具体类型。

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

DeepAgent 主模型选择 Code Agent 工具与普通受控委派。业务/Web 明确选择 Workflow 时调用独立目标入口，Code Agent 主模型选路为 0；Workflow 固定节点依赖，并保留静态子流程、条件、并行汇合及节点级暂停/显式恢复。描述应明确适用条件和必填参数，不强迫不匹配任务进入流程。界面选择只决定业务路由，不把 Workflow 放入当前 Session 或 targetAgent，也不改变已受理运行。

保留 general-purpose/专家子 Agent 的父 Trace 权限、预算、generation 与取消约束；子调用不消费父队列或直接写主历史。这些是 Code Agent 内部委派，不用于独立 Workflow 的运行归属。跨两类受控工具包装只传显式输入与获准结果，被调方独立策略、预算与恢复，不自动继承父 Trace。

已确认默认装配工作区内文件读写/搜索、命令执行、write_todos 与 general-purpose 子 Agent；提示词说明实际基础能力并允许应用替换，不将编码设为唯一任务类型。CreateAgentSession 负责装配与校验相应后端，缺失不静默裁剪；开发者可显式定制。能力清单及实际可注册子 Agent 名称来自当前 generation，模型编造的名称返回明确错误。

评估分别验证业务路由与被调方执行：可控请求经 Workflow 直接入口或业务受控工具触发，检查自身参数、授权、节点事件/恢复；真实模型只评估其选用已装配业务工具的合理性，不将 Workflow 当成 Code task 子 Agent。普通 Code 子委派的内部细节不默认全部注入主对话，父任务取得可验证摘要/产物引用并保留父 Trace 约束。

## 4. 技术规格

### 4.1 扩展生命周期与层级

Code Agent 生命周期：ResourceLoader 读取 → 原始数据/来源校验 → ExtensionRegistry 验证候选 → AgentSession 在新独立输入受理时原子选版并保存 → 串行执行 → 无旧 Trace/checkpoint 引用后按策略回收。初次由 CreateAgentSession 装配，queued 项也保留引用。动态资源与热重载仍在原未来阶段。

Workflow 生命周期独立：自身定义/绑定校验 → 编译 Eino Graph → 自身受理、节点执行与提交 → 自身节点暂停/审批及显式恢复。其定义、记录、写入者、generation、预算、审批和恢复不交给 Code Agent。未来动态候选按 Workflow 自己的边界激活。

L2 定义执行窄契约，两类 L3 分别注入配置、上下文、策略和提交端口，不互相导入或持有对方管理器。Code Agent 由 SessionManager 保存，Workflow 由自身节点状态协调保存；共用 storage 契约/jsonl/memory，不共用业务日志。业务工具组合不新增 SDK 框架，也不拥有被调方授权。

事件扩展采用 M07 的两条管道：观察订阅只读、可注销、返回值不改变执行决定；扩展控制 hook 对应 pi.on 的职责，在固定阶段等待决定或转换结果。每个 Trace 固定处理器快照和顺序；空闲会话维护操作在受理时固定其快照，单次操作不混用版本。handler 使用 2.6 的受控操作入口，不直接写 SessionStore；具体事件、组合和上下文边界以 M07 的 4.2.1～4.2.3 为唯一来源。

### 4.2 前端接入操作面

下表的会话/Trace/扩展命令属于 Code Agent；Workflow 操作单独路由自己的目标公开能力。内部 Web 使用受控 L3 接口，不导入 sdk、不直接操作管理器或 Store；外部消费者只导入 `github.com/ww1489/seasprak/sdk`。Web 不共用两类审批、恢复、generation、预算或日志写入者；业务工具不转移授权。路由域已接通为 Code `/v1/sessions`、Workflow 定义 `/v1/workflows` 和 Workflow 运行 `/v1/workflow-runs`；真实子路径、HTTP 白名单 DTO、typed cursor 与 Go 签名见开发方案 06/10/13，均有对应默认测试与消费者，最终认证见验证记录。详见 [M07 HTTP 操作语义](07-events-and-access.md#46-http-操作语义)：

| 能力组 | 操作 | 必须表达的语义 |
| --- | --- | --- |
| 会话 | 创建、列表、详情、历史、分支读取/切换 | 活动分支、工作区、版本、可继续状态 |
| Trace 与输入 | 提交、查询、取消、继续独立运行队列 | inputId 去重、traceId 归属；follow-up 不是新 Trace；取消后的旧队列暂停，新 prompt 不顺带启动它 |
| 当前 Trace 干预 | steer、follow-up、交互回复、恢复 | traceId/interactionId；不能用普通消息代替授权 |
| 未决效果 | 发起核对、提交材料、查询结论/恢复资格 | operationId 关联原 invocation/toolCall；核对不等于重试，许可和 checkpoint 条件仍独立检查 |
| 能力 | 分别列出 Code Agent 能力/输入 schema 与 Workflow 定义/节点绑定/诊断 | 各自版本与范围分开；注册/导入由开发者 SDK 完成，不要求前端创建子 Agent 或编排界面；动态来源仍为未来 |
| 工作流 | 业务分别调用独立 Workflow 的创建/打开、执行、节点状态、审批及显式恢复目标能力 | 不经 Code Agent SubmitInput/targetAgent；使用 Workflow 自身身份、schema、策略与写入者，工作流创建/打开及结构化运行已接线，有对应默认测试，最终认证见验证记录 |
| 事件 | 各自订阅、游标与恢复快照 | 按产品类型及自身运行范围过滤；不把两类 durableSeq、审批或恢复混作 Code Agent Session |

HTTP/SSE 是目前落地建议。SSE 的 `id` / `Last-Event-ID` 只提供游标传输机制，服务端仍需实现 M07 的恢复语义；浏览器重连不会自动提供应用持久化保证。[HTML SSE 标准](https://html.spec.whatwg.org/multipage/server-sent-events.html)

### 4.3 断连、重复请求与版本

- 已受理任务由所属产品运行对象持有：Code 由 AgentSession/SessionManager 保存，Workflow 由自身生命周期独立提交。断开请求/订阅不取消它；重试在自身受理作用域去重，不借另一类 inputId/票据启动或恢复。业务跨运行幂等归业务，工具超时不证明被调方已停止。
- 各自订阅使用 M07 快照与所属日志的 durableSeq 消除“查状态后再监听”空档；允许重复投递但可去重，两类不共享游标，不承诺永久重放全部 token。
- 无效/过期游标要求客户端重新同步快照；慢客户端不能无限阻塞模型或吞掉关键状态。策略按 M07，页面只展示结果。
- API/事件 envelope 有版本，未知可选扩展事件可忽略但需可诊断；不识别的关键动作或消息模式必须拒绝，不能猜参数。
- 本地测试接口仍验证输入和访问范围，拒绝向任意第三方网页开放工具执行；公开远程多用户部署的身份与租户管理留到独立需求，不由本篇暗自扩展。

### 4.4 A2UI 边界

保持产品事件为核心通用语义；A2UI adapter 可以把适合富交互的结果映射为 UI 描述，把客户端动作映射为已有应用操作。移除 adapter 后文本与结构化结果仍能使用。

完整 A2UI 支持不是当前必交付实现；选用时明确版本、组件/动作集合及未知组件回退。外层按产品类型分别映射，动作经 Code AgentSession 或 Workflow 自身入口验证身份、票据、有效性与独立策略，另一类批准不能放行；UI 文本不能定义任意工具执行。测试页面不规定 React、Vue 或其他框架。

### 4.5 验收

| 编号 | 场景 | 通过条件 |
| --- | --- | --- |
| EXT-A01 | 注册计算工具、自定义消息和观察者 | 无需改核心循环；错误与未知消息按对应契约处理 |
| EXT-A02 | Code 注册重名工具/普通子 Agent，或 Workflow 校验非法 schema | Code Registry/Workflow 自己分别拒绝候选，诊断指出名称和来源；不经 Code 统一 generation 激活 Workflow |
| EXT-A03 | 未来阶段任务执行时编辑 skill 与其引用文件并 reload | 旧任务看到旧内容，新任务看到新内容；原工作区业务文件仍实时可读写；本轮不启动资源热重载阶段 |
| EXT-A04 | 未来阶段等待交互时更新并停用插件，随后恢复 | 有旧版本则按原权限约束继续；缺失则明确不兼容，不调用替代实现；当前权限撤销仍生效 |
| EXT-A05 | 代码创建 Code Agent 普通自定义/专家 Agent 后 AddSubAgent | 父模型可委派、父 Trace 权限/预算/generation/取消约束不变；非法/重名拒绝，运行登记按原版本边界生效；不登记独立 Workflow |
| WF-A01 | 同一短流程业务直接调用或受控工具包装触发 | 同样 Workflow schema/结果与独立授权；直接方式 Code Agent 主模型为 0；包装只配对外层原工具请求，不合并历史或伪造父 task |
| WF-A02 | 业务直接/工具调用 Workflow 字段缺失或外层已批准 | 副作用前拒绝非法/缺参；补参仍属未来能力，自然语言或外层批准不绕过内层 schema/授权/冻结/票据 |
| WF-A03 | Workflow 第 2 步节点确认，恢复后第 3 步失败 | 静态子流程/条件/并行与节点审批显式恢复保持；第 1 步已确认效果不重放；等待、恢复、失败关联 Workflow 自身运行/节点，unknown 先核对 |
| WF-A04 | 未来导入指定 Coze Canvas 子集，混入孤立未知节点、悬空引用、类型错误、成环或缺绑定 | 原未来阶段单独验收：合法子集由 Workflow 自身编译；非法样本修剪/激活前诊断，不把本轮独立架构视为 Coze 交付 |
| WF-A05 | 未来 Workflow 动态定义更新/编译失败及自身排队/恢复 | 自身已受理运行固定定义/绑定，更新失败保留旧版；不与 Code Agent generation 共用，新独立运行才可选新版本；本轮不启动该阶段 |
| WF-A06 | Code Agent 活动时界面改选 Workflow 并提交 | 外层调用 Workflow 自身入口，Code Agent 原 Trace/队列/恢复目标不变，不误投 follow-up 或共享写入者 |
| WF-A07 | 无模型的“开始 → 本地工具节点 → 结束”直接/包装触发并注入拒绝与恢复 | 节点按自身固定绑定校验，独立授权/预算/去重；不伪造 Turn/provider 请求，已有结果不重复执行，外层批准不放行内层 |
| WF-A08 | Workflow 输入/审批误发 AgentSession 或 targetAgent，或使用另一类恢复票据 | 明确拒绝错类型/错身份，不另建 Code Agent Trace；正确 Workflow 节点恢复仅用自身已保存条件，未支持自由续输入明确拒绝 |
| API-A01 | 最小 Web 分别选择 Code/Workflow、提交、观察、取消与继续/恢复 | 只调用所选类型公开能力；Code Agent 同会话语义与 SDK 一致，Workflow 自身节点/运行语义独立，targetAgent 不跨类型 |
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
| API-A03 | 将富结果适配为 A2UI（若启用） | 原产品事件无需改动；动作经所选类型的 Code AgentSession 或 Workflow 自身入口校验身份、票据和独立策略，错类型/跨类批准拒绝；禁用 adapter 不影响执行 |

## 5. 风险与系统闭合

本篇把扩展和接入纳入完整系统，不提前划分开发迭代。必需工作区、默认基础能力、安全组合和取消后队列已确认；A2UI 仍为可选外层能力，具体协议集合、版本保留及实现参数由后续开发方案细化，见[路线决策清单](roadmap.md)。

开发方案前必须把 EXT-A03/04、WF-A03、API-A02 与 M03/M07/M10 的同一轨迹合并审查，确认谁负责状态、谁负责保存、谁负责恢复。不能分别通过“能调用工具”“能存 checkpoint”“能发 SSE”就宣布完整系统可用。
