# 第 7 章：事件、控制钩子与前端接入

状态：完整讨论稿。对应教程 [M07 事件驱动](https://dg-ai-notes.pages.dev/modules/ch07-event-driven)。前端实现不纳入产品范围、使用最小 Web 页面验证已确认；工作区、默认基础能力、安全与取消后队列已获确认；HTTP/SSE 具体路径和 DTO 仍是可评审建议。状态机引用[执行状态契约](03-agent-loop.md)，消息结构引用[第 6 章](06-messages.md)。

对象名称与[第 2 章的职责划分](02-architecture-boundaries.md#23-与-pi-对齐的对象名称与职责)一致：AgentSession 协调会话操作和事件发布，SessionManager 负责持久提交；HTTP/SSE 仅映射这些公开能力。

## 1. 执行摘要

### 1.1 问题与方案

如果页面连接拥有任务生命周期，刷新页面可能丢任务或重复执行；如果观察者和权限判断使用同一类回调，慢客户端可能阻塞执行，权限检查失败也可能被误当作日志错误忽略。

Agent 产生通用执行事件，AgentSession 协调 SessionManager 提交状态和历史，保存成功后由 AgentSession 发布产品事实，最外层将操作与事件映射到 HTTP/SSE。观察订阅、执行控制钩子、必须完成的内部持久化分别定义责任；A2UI 按需读取同一套消息和事件并回传普通操作。

### 1.2 成功标准

| 编号 | 可验证目标 |
| --- | --- |
| EVT-K1 | 任务正常、失败、取消、等待用户输入和故障中断均能用公开状态及事件解释；不把一次模型消息结束当成整个任务完成 |
| EVT-K2 | 断开并重新连接后，客户端状态与查询快照一致，不重复提交任务、不重复展示终态消息 |
| EVT-K3 | 观察订阅者失败或停止消费不会改变任务结果；权限钩子失败不会放行工具 |
| EVT-K4 | 相同幂等请求重试多次只产生一次业务操作；相同键但不同内容被明确拒绝 |
| EVT-K5 | 无需页面、HTTP 或 SSE 依赖即可运行 L2 Agent；HTTP 与 Go SDK 的同一输入得到相同 AgentSession 语义 |
| EVT-K6 | SDK 观察订阅可独立注销；Session 关闭后不保留监听器；注销与关闭不会留下重复事件投递 |
| EVT-K7 | 扩展控制点能区分通知、拦截和链式转换；处理器顺序、短路、超时和错误行为可预测 |
| EVT-K8 | 接入者能区分内核事件、会话事件和扩展独占控制点；压缩、重试、队列和模型选择均有可查询的状态/事件映射 |

## 2. 用户体验与功能

用户包括 SDK 集成者、前端接入者、扩展作者及用 Web 页面联调的开发者。正常路径：提交带幂等键的输入 → 得到 inputId/traceId → 订阅 agent/turn/message/tool 事件 → 查询或收到 Trace 终态 → 在同一 Session 提交后续任务。SSE 只负责通知，不承担提交命令或确认副作用。

### 2.1 需求与验收

| ID | 用户故事与要求 | 验收及异常行为 |
| --- | --- | --- |
| EVT-01 | 作为接入者，我希望读取统一事件，以便替换客户端而不改变执行核心 | 事件带版本、类型和关联身份；自定义事件使用命名空间；Eino 事件及 SDK 内部对象不直接成为永久公开协议 |
| EVT-02 | 作为使用者，我希望知道任务是真的结束还是暂时等待 | 完成、失败、取消事件在消息与任务状态提交后产生；`waiting_input` 带可回答请求引用；崩溃不确定为 `paused`；一条 assistant 消息结束不是 Trace 结束 |
| EVT-03 | 作为客户端作者，我希望可安全处理重放与并发工具事件 | 同一 Session 的持久事件有递增 durableSeq；重复发送使用同一事件 ID；同名工具按调用身份区分；不同工具之间不承诺完成顺序 |
| EVT-04 | 作为使用者，我希望刷新页面后继续观察，以便任务不受浏览器影响 | 关闭连接不取消任务；可从游标重连；游标过期或临时流丢失时明确重同步，并返回当前任务/消息快照，不能静默跳过缺口 |
| EVT-05 | 作为集成者，我希望失败的观察者不会使任务失败 | 同时挂一个正常订阅者和一个抛错/慢订阅者；任务继续，正常订阅者可看到最终事实，故障订阅有诊断；队列不无限增长 |
| EVT-06 | 作为策略作者，我希望工具执行前能明确拒绝，以便权限规则可靠生效 | pre-execute 可返回 allow/deny/cancel/ask；ask 仅在 allowed-once 后继续；拒绝或钩子异常时执行为零；订阅 tool.started 不具备拦截能力 |
| EVT-07 | 作为网络调用者，我希望提交、取消和恢复可重试，以便请求超时不会重复动作 | 同幂等键同请求内容返回原结果，跨重启仍有效至声明的保留边界；同键异请求冲突；返回 accepted 之前必须保存输入或命令事实 |
| EVT-08 | 作为前端接入者，我希望有操作、状态查询与事件入口，以便自行实现界面 | SDK 和 HTTP 覆盖 Session、输入、Trace、取消、恢复、历史、分支及能力查询；权限拒绝、无效状态和版本不兼容具有结构化错误 |
| EVT-09 | 作为测试者，我希望用最小 Web 页面验证公开接口 | 完成提交、流式观察、停止、继续、断连重连、等待输入应答和读取历史；页面不能直接调用内部存储或更改运行对象 |
| EVT-10 | 作为协议适配作者，我希望按需增加 A2UI，以便结构化交互复用同一底座 | 映射器放在最外层；动作带 Session/Trace/交互请求身份，经过同一校验和权限；未知或过期动作拒绝，不直接执行模型给出的任意命令 |
| EVT-11 | 作为 SDK 使用者，我希望暂时观察一个 Session 后安全停止观察 | `Subscribe` 返回注销句柄；注销后不再投递新事件；Session 关闭释放所有订阅和临时流；在途回调完成后不得重新注册自身 |
| EVT-12 | 作为扩展作者，我希望知道 hook 修改结果如何组合 | 通知 hook 只观察；拦截 hook 可短路；转换 hook 按注册顺序接收上一个结果；异常、超时和拒绝分别按 fail-closed/fail-safe 规则处理 |

### 2.2 关键异常轨迹

**提交响应丢失：**服务端已保存 inputId=i1 与 traceId，响应丢失后相同键重试返回原归属。对于 follow-up，原归属仍是当前 Trace，不创建第二个 Trace；同键不同内容拒绝。

**客户端在工具运行时掉线：**原 Trace 继续。重连恢复持久事实和当前消息快照；临时增量可有缺口。进程崩溃则查询 paused 及已知结果，不能把最后一段文本当作完整回复；日志不可用也不影响 traceId。

**等待审批期间重复回复：**第一次有效回复已绑定对应交互请求并受理；同键重试返回该结果；另一个相互冲突的回答被拒绝。过期请求或旧执行尝试的无效交互不能用于批准新任务的工具调用。

### 2.3 非目标

不规定前端框架、布局或组件，不提供 UI 工作流设计器；不实现分布式消息中间件、全局跨会话事件顺序或所有 token 永久重放；不把一台本地服务的接口直接等同于完整多租户公网服务。

## 3. AI 系统需求与评测

### 3.1 事件代表观察事实

文本增量、工具进度和模型自述不构成业务验收结果。成功必须由任务终态、已提交产物及工具实际结果支撑；工具失败后模型可以继续处理，因此 `tool.finished(outcome=failed)` 不自动等于 `agent_end(state=failed)`。

模型可能一次产生多个工具调用，也可能委派子 Agent。所有消息和工具事件携带所属执行作用域；子 Agent 输出不会默认冒充顶层最终回复。模型 usage 和延迟以可获得的供应商数据及执行计时记录，缺失时标明未知，不通过流式 chunk 数量估算为确定 token 用量。

#### 3.1.1 事件源的四层嵌套

参考文章的模型调用路径可归纳为四层，产品事件名称可以不同，但因果层次保持一致；无模型节点和直接调用按下文的来源关联，不强行套入 Turn/助手消息层：

```text
Trace：agent_start → agent_end（底层循环边界）→ trace.settled（产品完整收尾）
└── Turn：turn_start → turn_end
    └── Message：message.started → message.delta/snapshot → message.finalized
        └── Tool execution：tool.requested → tool.started/progress → tool.finished
```

一轮可能没有工具层；一条助手消息可能包含多个工具调用层。`tool.requested` 是按来源接纳的调用事实，`tool.started` 才表示执行后端已经启动；控制 hook 在二者之间运行。被拒绝的调用不产生 started，但必须有明确的 finished(denied) 或对应控制结果。事件消费者按 origin 和实际作用域关联：model 使用 traceId/invocationId/turnId/messageId/toolCallId，workflow_node 使用 traceId/invocationId/nodeExecutionId/toolCallId，direct 使用实际 invocation 或 operationId 与 toolCallId；后两类不伪造 Turn 或助手工具请求，不用到达顺序猜测父子关系。

pi 的底层循环遵循这条嵌套顺序：首轮发出 agent/turn 起始事件，消息流产生 start/update/end，工具批次完成后发 turn_end，外层 follow-up 结束后才发 agent_end；无工具轮次仍然有 turn_end。coding-agent 还会在 retry/compaction/queue 都处理完后发 agent_settled。本产品分别表达为内部 `agent_end` 和产品 `trace.settled`，不能把底层边界直接当作最终收尾。见[循环源码](../../pi/packages/agent/src/agent-loop.ts#L95) `[VERIFY: pi/packages/agent/src/agent-loop.ts:95]`、[轮后与收尾](../../pi/packages/agent/src/agent-loop.ts#L224) `[VERIFY: pi/packages/agent/src/agent-loop.ts:224]`、[settled](../../pi/packages/coding-agent/src/core/agent-session.ts#L607) `[VERIFY: pi/packages/coding-agent/src/core/agent-session.ts:607]`。

### 3.2 验收矩阵

| 场景 | 可观察断言 |
| --- | --- |
| 文本 → 工具 → 文本 | assistant 中间消息结束后 Trace 仍运行；工具请求、结果、最终答案及终态顺序可追溯 |
| 同名工具并发 | 每个调用独立开始/结束；全局展示顺序由接收/提交序号决定，不误配结果 |
| 取消与结果同时到达 | 只形成一个确定终态，已完成副作用如实保留；重复取消返回现状 |
| SSE 断开、慢消费、重复重放 | 任务继续；客户端重同步后与查询一致；持久事件按 ID 去重 |
| 输入已提交但响应丢失 | 重试不会新增输入、任务或工具副作用 |
| 等待输入后恢复、恢复前扩展更新 | 继续原 Trace，创建新执行尝试，使用原 Trace generation；旧审批答案不能串到新交互 |
| 存储提交失败 | 不发布虚假的持久成功事实；停止推进需要该提交的执行步骤，公开错误可查询 |
| 权限钩子异常与观察者异常 | 前者按控制点策略阻止或暂停相应操作；后者只影响该订阅，不改变业务决策 |
| 内核/会话/扩展事件分层 | `turn_*`、`message_*`、`tool_execution_*` 是执行事实；`trace.state_changed`、队列/压缩/重试是产品状态；`tool_call`、`context`、`input` 等控制点不从观察订阅泄漏 |

以上以假模型、受控工具及故障注入验证，不调用付费模型。最小 Web 页面用于确认同一公开契约可由真实浏览器消费，不用于证明模型推理质量。

## 4. 技术契约

### 4.1 分层及所有权

| 位置 | 负责什么 | 不负责什么 |
| --- | --- | --- |
| L1 模型能力 | 供应商流与模型调用观测 | 产品 Trace 状态、SSE 游标 |
| L2 Agent | 消费 Eino 输出流，产生通用消息/工具执行事实；执行注入的控制钩子 | HTTP 状态码、客户端连接、AgentSession、SessionManager、ResourceLoader、ExtensionRegistry 的具体实现及产品存储实现 |
| L3 AgentSession | 协调当前 Session 的串行操作、身份关联、命令去重和状态变更；SessionManager 保存成功后发布持久事实；管理订阅 | 直接实现存储后端、前端组件树和网络连接对象 |
| L3 SessionManager | 管理历史与分支，校验并通过 SessionStore 保存输入、消息、任务记录及关联持久事件 | 决定请求调度、执行模型/工具、管理客户端订阅；SessionStore 只负责存储适配 |
| 外侧接入 | HTTP 映射、SSE 编码、连接授权、可选 A2UI 映射 | 重写任务状态机、直接操纵 Eino Runner 或历史文件 |

Eino `MessageStream` 有独占消费约束。建议由 Agent 统一消费并报告通用输出，AgentSession 聚合产品消息快照并分发事件；持久事实仍须先经 SessionManager 提交，再发送给客户端和观测器。不能让多个 SSE 连接竞争读取同一个框架流。多个观察者收到同一份不可变内容视图。

### 4.2 观察订阅、控制钩子与提交

| 通道 | 时机和返回语义 | 失败处理 |
| --- | --- | --- |
| 观察订阅 | 接收已经发生的事实；返回值不参与下一步决策 | 隔离该订阅的错误，记录诊断；慢订阅超出有界缓存后断开并要求重同步；不拖住工具和模型 |
| 控制钩子 | 在输入处理、上下文准备、工具执行前等明确位置等待结果 | 需要返回决定；权限检查异常或超时不能放行；上下文变换失败不能静默继续；具体行为按对应章节定义 |
| 会话内部提交 | AgentSession 协调，SessionManager 经 SessionStore 保存输入、消息、任务状态及其持久事件 | AgentSession 必须取得提交成功结果再确认受理/发布事实；不能放入可丢失的普通异步观察订阅 |

执行内控制钩子按 Trace generation 中确定的顺序运行；空闲会话维护在操作受理时固定 active generation 及处理器顺序，单次操作不混入 reload 的候选。观察者不能修改共享事件对象、悄悄写会话历史或通过返回值授权工具；需要操作时显式调用 AgentSession 的操作入口。扩展日志可异步，产品历史提交不可依赖日志是否送达。

#### 4.2.1 两条管道与事件层次

事件来源分成三层：

1. **内核事实事件**：`agent_start/end`、`turn_start/end`、`message.started/delta/snapshot/finalized`、`tool.requested/started/progress/finished`，描述已经发生或正在发生的执行事实。
2. **会话产品事件**：`trace.state_changed`、`input.*`、压缩、重试、队列、模型/扩展版本变化，描述 AgentSession 和 SessionManager 的产品状态。
3. **扩展控制点**：输入改写、运行前准备、上下文转换、工具调用前拦截、工具结果改写、模型请求前后处理。它们由控制链主动触发，不能假设会从观察订阅收到。

`Subscribe` 是产品层只读观察管道：监听器返回值丢弃，慢处理不能改变 Agent 决策。注册返回注销句柄；注销操作幂等。Session 关闭时注销全部监听器、停止临时事件投递并释放连接关联；正在执行的回调允许完成，但完成后不得再收到新事件。观察监听器的异步错误进入订阅诊断，不传播到 Agent。L2 内部 Agent 的事件 sink 可以等待处理器完成，这是执行内部同步屏障；不能把两种订阅混为一个“不等待”规则。

控制 hook 是同步决策管道：AgentSession/Agent 等待结果，并按以下三种形态组合：

| 形态 | 例子 | 多处理器组合 | 默认错误行为 |
| --- | --- | --- | --- |
| 通知 | turn/message 生命周期、模型结果观察 | 按注册顺序等待处理完成，但忽略返回值 | 记录扩展错误并继续；不可替代持久提交 |
| 拦截 | 工具调用前、会话切换/分叉前、敏感操作前 | 第一个明确 deny/cancel/block 短路；allow 不覆盖更严格的拒绝 | 权限、审批和安全设施异常 fail-closed；普通业务错误按对应阶段返回 |
| 链式转换 | input、before-agent-start、context、工具参数准备、tool-result | 后一个处理器接收前一个成功处理器的输出；没有返回值表示保持当前值 | 转换失败不静默使用半成品；上下文/输入转换阻止本次调用 |

处理器快照在 Trace 开始时固定，按 generation 中的注册顺序运行；运行中新增处理器只形成候选版本，不插入当前链。hook 的 timeout 使用请求取消信号，超时结果必须带阶段、扩展身份和是否已执行副作用。控制 hook 不直接取得 SessionStore 写权限；需要持久事实时调用 AgentSession/SessionManager 的受控入口，避免 hook 绕过提交顺序。

通知型扩展处理器按注册顺序串行等待，但一般忽略返回值；会话 before 类事件可以读取 cancel 结果并短路。普通通知异常隔离并发出 `extension.error`；工具调用前拦截是安全例外，处理器异常默认 fail-closed，由工具阶段转换为拒绝/失败结果。工具进度更新可合并或延迟投递，但在 `tool.finished` 前必须收敛；生命周期开始/结束事件不能被静默丢弃。

#### 4.2.2 控制点与最小上下文

| 控制点 | 触发时机 | 可修改/返回 | 不允许 |
| --- | --- | --- | --- |
| `input` | 输入进入消息处理、模板/skill 展开前，原始受理输入已保存 | 派生文本、图片及允许的输入类别；handled/transform/continue；多个处理器按链式转换并保留来源关联 | 改写原始人类输入或其来源身份、赋予派生文本新授权、绕过 inputId/Trace 归属 |
| `before_agent_start` | Trace 首个模型循环开始前 | 系统指令覆盖、结构化注入消息 | 将未经授权文本提升为 system；直接写历史 |
| `context` / `transformContext` | 模型请求准备时、convertToLlm 前 | AgentMessage 选择、缩减、注入、替换副本 | 修改原历史、绕过 M08 预算、访问网络偷偷改变结果 |
| `prepareArguments` / 参数转换 | 工具准备阶段，最终 schema/业务校验之前 | 返回候选参数；按顺序组合，最后再规范化/校验 | 改变调用/工具身份、执行工具、把转换结果当作许可 |
| `tool_call` / pre-execute | 最终参数/执行描述已校验冻结，实际启动之前 | 对只读描述提出 allow/deny/cancel/ask；强制安全策略仍独立执行 | 返回/原地改写参数或环境；用 allow 跳过策略/审计；审批未完成时放行 |
| `tool_result` / post-execute | 工具执行已经结束后 | content、details、isError、usage 的模型投影视图；多个处理器累积修改 | 改写原始副作用事实、将失败变成功、触发第二次执行 |
| `before_provider_request` / `after_provider_response` | L1 发送前/收到响应后 | 供应商请求扩展字段或脱敏诊断；请求 payload 按链式替换 | 在 L2 按品牌分支；将密钥或完整敏感响应写入普通事件 |

工具参数转换与授权使用不同阶段，顺序以 M05/M12 为准；普通扩展无法靠处理器排序把强制授权放到最后一次参数变更之前。需要改变已冻结描述时，原候选停止并重新校验授权，不能在 execute 前静默修补。

控制上下文最小包含：`traceId/invocationId`、generation、取消信号、当前模型能力摘要（适用时）、工作区/安全范围、只读历史查询和受控操作入口。有模型轮次时带 turnId，无模型工作流节点用 nodeExecutionId/toolCallId 关联，不借用父 Turn。扩展可按 M11 请求发送消息、保存私有状态、选择模型/工具、abort 或 compaction；空闲会话事件允许没有 traceId/turnId。不能直接操作 reader、SessionStore、HTTP 连接或共享可变工具列表。TUI/RPC 专属 UI 不进入 L2 核心；Web 接入通过普通 interaction/AgentSession 操作表达。

#### 4.2.3 会话生命周期 hooks

这些名称参照 pi 的扩展层命名，是控制/通知管道的阶段名；不要求公共 SSE 原样暴露它们。对外事实继续使用 session.branch_changed、context.compacted 等产品事件。具体 Go 方法签名在开发方案确定。

| 阶段 | 触发与上下文 | 返回值及提交规则 |
| --- | --- | --- |
| session_start | 新建、打开、切换或资源重载后的扩展上下文已完成激活；给出原因和恢复所需只读视图 | 通知，不自动执行旧输入；checkpoint resume 不新建 Session，不靠这个通知重放扩展业务 |
| session_before_switch | 离开当前 Session 去新建/打开另一会话前，目标已通过基础校验 | 继续/取消；取消不改变当前绑定。目标就绪后才提交切换并通知，不因加载失败遗失原会话 |
| session_before_fork | M10 同 Session 新建分支前，已固定原头、目标节点和合法来源范围 | 继续/取消；可定制本次已请求的分支摘要重点或返回候选，不改写目标、共同前缀或权限 |
| session_before_tree | 切换已有分支/活动游标前；纯只读浏览不触发 | 继续/取消；可定制本次已请求的分支摘要；一次操作按 fork 或 tree 分类，不重复执行两组 before hooks |
| session_tree | 同会话分叉/导航及可选摘要已一致提交后 | 通知，携带操作类别和前后引用；不会因同会话分叉再发 session_start |
| session_before_compact | M09 已固定触发原因、投影版本、来源 H/P/K 与保留边界，生成默认摘要之前 | 继续默认生成、取消或返回替代摘要候选；可依序追加摘要重点，不能自行扩大材料范围 |
| session_compact / session_compact_failed | 压缩提交完成，或取消/生成/校验/提交失败之后 | 通知，明确 committed、cancelled 或具体失败原因；不把候选生成完当成已提交 |
| session_shutdown | 当前会话扩展上下文正常关闭或被替换，相关执行已按 M03 停止/安置后 | 有界清理，返回值不能否决关闭；错误隔离，必要持久状态不依赖本通知；断连和暂停不触发关闭 |

统一顺序是：校验命令与该操作的安全边界 → 固定操作身份、来源与 handler 快照 → before 链 → 校验最终候选并重新核对提交前置条件 → SessionManager 提交相关状态 → 发布 durable 事实和 after 通知。Session 绑定切换由应用会话协调入口完成，不能让 SessionManager 调度新任务或将候选目标视为已经切换。切换/分叉/导航的 before hook 不能绕过 M10 对活动 Trace、待消费输入/扩展写操作或副作用冲突的限制；自动压缩则按 M09 在当前 Trace 的安全边界进行，尚未消费的输入仍留在队列，不进入固定材料。

组合规则：按已固定顺序等待；任一 cancel 短路；摘要重点按顺序累积。一次操作最多接受一个替代候选，后续处理器可继续校验/取消，第二个替代候选返回冲突而非静默覆盖。before 异常、超时、非法结果取消本次维护操作并保留原状态；after 通知错误只记录 extension.error，不将已提交成功改成失败或自动重做。

替代摘要仅改变允许的生成结果，正文/范围/预算、文件 facts/details 与提交仍由 M09 校验和生成；扩展不能伪造已修改文件。自动压缩被取消或失败时，原投影满足硬预算才可继续，否则按 M09 阻止模型请求。手动压缩与导航取消仅结束该维护操作，不表示取消整个 Trace。

会话维护 before hooks 只读已固定范围并返回决定/候选；除请求 abort 外，不同时提交消息、私有条目或另一项维护操作来改变该范围。资源重载时仍被旧 Trace/checkpoint 引用的上下文不能提前关闭。会话 before hooks 不得否决明确的用户 abort 或权限撤销。生命周期 handler 也遵循 M11 的受控操作和不重入规则：不能在 before_switch 内再次同步 switch，也不能在 before_compact 内等待另一次 compact。提交后的通知可能因崩溃未送达；它们不承担唯一持久化或外部副作用恰好一次保证，恢复依据提交记录。

### 4.3 事件封装建议

| 字段 | 约束 |
| --- | --- |
| `schemaVersion / type` | 版本化产品事件；扩展类型使用命名空间 |
| `sessionId / traceId / turnId` | 会话、完整运行与模型/工具轮次；Agent 执行相关事件必须关联 traceId，排队阶段无 turnId，无模型工作流节点以 invocation/nodeExecutionId/toolCallId 关联而不伪造 Turn；会话维护或独立 direct 操作以 operationId 关联，可无 traceId |
| 诊断关联（可选） | 观测系统的 span 或供应商请求 ID 使用单独字段，不冒用产品 traceId；日志导出不决定 Trace 状态 |
| `executionId` | 执行诊断和恢复记录按需提供，标识一次执行尝试；普通消息事件不强制重复携带 |
| `messageId / toolCallId / inputId` | 按事件种类提供，不能把供应商 call ID 直接当作全局产品身份 |
| `invocationId / parentToolCallId` | 区分顶层与子执行；父子关联由运行时装配建立，不能假定 Eino RunPath 就是完整业务调用树 |
| `eventId / durableSeq` | 持久事件使用；同 Session 单调递增，重放保持原值；具体编码对客户端不透明 |
| `streamId / chunkSeq` | 临时消息流使用；在该流内递增，重同步后可创建新 streamId；不是可永久恢复的历史游标 |
| `attempt / blockIndex` | 模型更新按适用事件携带尝试与块身份；delta 与 snapshot 由事件类型区分，不跨尝试拼接，不将重连流身份当作新的模型调用 |
| `occurredAt / payload` | 时间与版本化事件内容；时间不用于排序；凭据、未授权文件内容不进入普通事件 |

公开协议保留新字段兼容空间：客户端可以忽略不认识的可选字段；遇到未知事件类型可记录并继续读取，但若状态版本或关键快照无法解释，应明确报告协议不兼容，不能推断任务成功。

### 4.4 事件族及持久性

下表是建议的产品名称，后续接口审定可调整命名，不改变语义。

| 事件族 | 语义 | 保存要求 |
| --- | --- | --- |
| `input.accepted / input.consumed / input.cancelled` | 输入受理、真正被任务消费或显式撤回 | 持久；被接受不等于已经进入模型 |
| `trace.state_changed` | Trace 排队、运行、等待交互、暂停、取消中及终态变化 | 持久；状态以 M03 为准，queued 尚未产生 agent_start |
| `agent_start / agent_end` | 底层 Agent 循环的开始与结束；agent_end 只表示当前执行段不再产生内核循环事件 | 已开始的执行段各一次，以 executionId 区分；扩展/内部 runner 可以观察，但不能直接将其作为产品完成 |
| `trace.settled` | AgentSession 在 retry、compaction、queue、审批恢复及持久提交均处理完成后发布的完整 Trace 收尾 | 已开始的 Trace 最多一次；携带 completed/failed/cancelled 终态；waiting_input/paused 按 M03 保持未完成，只发布状态变化 |
| `turn_start / turn_end` | 一次模型调用及其工具执行的边界，关联 traceId/turnId | 持久；有工具与无工具路径都结束本轮，错误/取消附原因；审批等待不提前结束未完成轮次，恢复不重复 start |
| `message.started / message.delta / message.snapshot` | 消息开始、增量和按需推送的当前完整视图；delta 追加，snapshot 覆盖 | 可为临时事件；查询快照也返回同一聚合内容，不将完整视图伪装为增量 |
| `message.finalized` | 完整或 incomplete 的消息已提交，附状态与原因 | 持久；不承诺每个 token 均已保存 |
| `tool.requested / tool.started / tool.finished` | 完整调用被接受、实际执行开始、本次执行观察结束 | 持久关键事实；许可占用不等于 started，缺少 started 不证明未执行；finished 可带 outcome_unknown；拒绝可直接由 requested 到 finished(denied) |
| `tool.state_changed` | 执行未知、停止待确认或核对结果已提交 | 持久；核对记录关联原调用，不代表再次执行或第二条工具结果消息 |
| `tool.progress` | 命令输出/进度更新 | 临时；最终结果摘要或附件引用进入消息 |
| `interaction.requested / interaction.resolved` | 可回答的澄清、确认或审批请求 | 持久；关联 interactionId、traceId、工具调用/作用域及有效期 |
| `approval.asked / approval.decided / security.review_decided` | DSH 式一次审批及启用后的自动审核决定；后者为本产品最小审计增强 | 关联冻结执行描述；决定先提交，再按 M12 占用许可和启动；占用/核对属于原调用记录，不强制新增公共事件族 |
| `security.policy_changed` | 已提交的常驻安全模式/策略变更 | 持久；模型状态快照按 M08 追加；不能暗自扩大旧批准 |
| `context.compacted / session.branch_changed / extension.generation_changed` | 上下文、历史分支或能力版本的已提交变化 | 持久，详情引用对应记录；generation 变化不改正在执行的 Trace |
| `diagnostic` | 重试、订阅故障等诊断 | 按诊断保留策略；不能代替终态事实 |
| `queue.changed` | steering/follow-up/独立 Trace 队列的加入、消费、撤回与冲突 | 可持久也可临时；至少保留 inputId、类别、Trace 和去向 |
| `compaction.started / compaction.finished / compaction.failed` | 压缩候选开始、提交成功或失败/取消 | 持久关键状态；finished 只表示 compaction 提交，不表示 Trace 完成；failed 不激活半成品 |
| `retry.started / retry.finished` | 模型 attempt 的重试开始与成功/失败收尾 | 持久诊断；关联 turnId/attempt/reason，不产生第二个 Trace |
| `model.changed / extension.generation_changed` | 当前 Trace 允许的下一 Turn 模型策略或候选 generation 变化 | 持久；旧 Turn 不回写，新 generation 不能影响活动 Trace |
| `tools.selection_changed` | 下一 Turn 的工具选择已验证并提交，关联 Trace/invocation、原/新集合及生效位置 | 持久；登记或受理选择请求不冒充已生效；不代表权限已授予 |
| `extension.error` | hook 被隔离、短路或导致 fail-closed 的错误 | 持久诊断；带扩展身份、阶段、错误类别和是否阻止操作 |

Trace 是产品完整运行对象，多个输入和 Turn 可以归属同一 traceId。观测适配可另行记录耗时、attempt 和 span；采样或导出失败不删除 Trace、不改变恢复资格。`agent_end` 是内部边界，`trace.settled` 是产品完成事件；客户端不能用前者替代后者。

模型 attempt 的结束遵循 M04：Stream 建立失败、Recv 错误、取消和正常结束汇总成一次尝试结果，可在诊断或查询快照中表达，不直接变成产品 trace.settled。部分消息是否 incomplete 及成功尝试的接纳由 M06/M03 决定；同一更新不同时作为 delta 与 snapshot 重复累计。公共事件不直接透传厂商 SSE 名称，也不要求复制 pi 的全部模型流事件名。

Agentic 事件使用 `TypedAgentEvent[*schema.AgenticMessage]`，从 `AgenticRole` 与 ContentBlocks 识别输出；不能沿用旧事件的 `Role/ToolName` 非空假设，也不能只见 user role 就当作人类消息。见 [事件变体](../../eino/adk/interface.go#L66) `[VERIFY: eino/adk/interface.go:66]`、[Agentic 事件构造](../../eino/adk/interface.go#L291) `[VERIFY: eino/adk/interface.go:291]`。

中间文本与工具进度可以合并或丢弃后重同步；任务状态、交互请求和终态消息不能当作可丢弃的 token 增量。持久事件保留窗口与会话历史保留不是同一概念：即使游标过期，仍可查询尚在保留期内的会话历史和当前状态。

一次审批的内部审计与客户端 interaction 通过同一 approvalId/interactionId 映射，不成为两次独立提问。审批者身份由受信接入确定，客户端回传决定和对应引用，不回传可替换原操作的“新参数”；状态、有效期及冻结描述不匹配时拒绝。sandbox mode、enforcement、设施失败/拒绝分类放入原工具的执行详情与结果。Auto 拒绝的必要理由可给客户端，但 reviewer 推理不进入普通 SSE；完整规则见[安全补充篇](12-security-sandbox.md)。

### 4.5 顺序与提交边界

同一 Session 的已提交事实有唯一顺序；不同 Session 无全局顺序保证。并发工具按各自调用维护因果顺序，不要求 A 工具先请求就先结束。至少保证：

1. 输入 accepted 发生在实际 consumed 之前；消费关系绑定确定 Trace，不能因重试重复消费。
2. 调用事实先于其工具结局：model 先提交助手工具请求；workflow_node/direct 先提交节点或受信入口调用事实，不伪造模型请求。执行被拒绝时不存在“实际开始执行”的事实。
3. 某消息 finalized 之后不再产生该消息的新 delta；若存在重发，客户端按流和消息身份忽略旧增量。
4. 产品 `trace.settled` 之前，已产生的终态消息、工具观察及 Trace 终态必须提交。自然完成还要原子确定 steering/follow-up 已无可消费输入；错误/取消不自动出队，未决副作用不得写成 completed。
5. 网络上允许重复送达；业务命令和历史写入通过身份与去重保证不会重复生效。内部执行段事件若保存，按 executionId 与事件身份区分；产品 trace.settled 按 traceId 保证一条收尾事实，重放使用原事件身份。网络传输不承诺 exactly-once。

模型内部重试、自动压缩续执行、follow-up 或审批恢复保持同一 Trace；不会提前或重复发送产品 `trace.settled`。AgentSession 汇总内部 `agent_end`、框架退出结果和 SessionManager 提交，完成后发布 settled。这里保留 pi 的两层边界：底层 agent_end 与 coding-agent 的 agent_settled 分开，产品事件名采用 `trace.settled`，不再让客户端猜测底层事件是否已完成所有续处理。

### 4.6 HTTP 操作语义

路径为可评审建议，具体错误码与 DTO 命名尚未冻结。创建入口通过 CreateAgentSession 组装对象；当前会话的操作、查询与订阅统一通过 AgentSession，历史与分支处理由其委托 SessionManager。网络层不另实现一套会话调度或直接写存储。

| 建议入口 | 语义 |
| --- | --- |
| `POST /v1/sessions`、`GET /v1/sessions/{sid}` | 创建时必须提供并验证工作区/执行环境；返回默认已装配能力及状态，不泄露模型凭据 |
| `POST /v1/sessions/{sid}/inputs` | 按 M03 区分 chat、prompt、steering、follow-up。新 chat/prompt 可选择 Agent，缺省为主 Agent；定向输入必须指定原 Trace，省略 Agent 时继承、显式错配拒绝，不创建新任务。返回 inputId、实际类别、绑定目标/版本和持久 traceId。改选另一 Agent 的新请求不能隐式变成旧 Trace 的 follow-up |
| `GET /v1/sessions/{sid}/traces/{traceId}` | 查询完整 Trace、当前 Turn、待答交互、未消费输入和公开结果；内部执行尝试仅作必要详情 |
| `POST /v1/sessions/{sid}/traces/{traceId}/cancel` | 取消指定 Trace；未启动时直接 cancelled，已启动按 M03 等待执行停止后收尾；重复操作返回现状 |
| `POST /v1/sessions/{sid}/traces/{traceId}/resume` | 恢复符合条件的原 Trace；保留 traceId、targetAgent、未完成 turnId 和 generation，采用 checkpoint 当时有效的模型/上下文版本；新执行段可发内部边界事件，不创建新 Trace 或提前发布 trace.settled |
| `POST /v1/sessions/{sid}/queue/continue` | 显式恢复所选 queued 项的自动调度；返回恢复范围和顺序，不抢占活动 Trace，不带入已终态 Trace 的旧 follow-up |
| `POST /v1/sessions/{sid}/traces/{traceId}/reconcile` | 对已有 invocation/toolCall 发起受控查询或提交核对材料；返回 operationId 和受理结果，不代表效果已确认或 Trace 已恢复 |
| `GET /v1/sessions/{sid}/operations/{operationId}` | 查询维护/核对操作的状态、可公开证据引用、结论及恢复资格；按会话权限过滤 |
| `GET /v1/sessions/{sid}/messages`、分支查询/操作入口 | 查询分页历史和目标分支；分支变更须满足第 10 章的空闲与冲突要求 |
| `GET /v1/sessions/{sid}/snapshot` | 返回一致的公开快照、持久游标和当前消息/工具状态，供首次连接及重同步 |
| `GET /v1/sessions/{sid}/events` | SSE；接受服务端声明支持的游标与过滤方式 |
| 能力查询与可选 Agent 清单 | 列出已装配能力、可独立选择的 Agent 及输入 schema；选择后的执行仍进入 AgentSession 任务调度与权限检查，不能旁路并发写同一 Session |

修改类请求带幂等键，作用域至少含调用者、Session（创建时为调用者作用域）及操作种类，并保存请求内容摘要和受理结果。同键同内容返回原结果；同键异内容为冲突。建议任务相关键至少保留到任务与其恢复窗口结束；服务端声明具体保留边界，超期不得暗示仍有历史去重保证。

异步操作受理不等于执行成功。请求级取消与任务取消是两个动作：提交请求超时、浏览器页面关闭、SSE 断连不能自动取消已受理任务。Abort 后按 M03 保留旧记录并暂停当时独立队列的自动启动；取消完成且无冲突时，新 prompt 可正常开始。旧队列只有显式继续才恢复，已终态 Trace 的未消费输入重做时需新 inputId/traceId。

结构化错误包含稳定错误类别、可读说明、相关身份与是否可重试。无效参数、权限拒绝、无效状态、幂等冲突、版本不兼容、游标失效分别表达。服务端内部栈和密钥不返回普通客户端。

#### 核对操作的语义

这是针对未决工具效果/许可占用的受控操作，不是要求 Session 空闲的分叉或压缩，也不是再次执行工具。AgentSession 可在目标 Trace 为 paused、正在收敛取消或已终态但仍有未决效果时受理；原操作仍可能继续生效时只能保留查询/材料，不能以瞬时观察提前作“未执行”的最终结论。

请求最少关联原 sessionId/traceId/invocationId/toolCallId、相关许可/观察记录和幂等键，并明确是发起已装配的只读查询，还是提交人工材料/产物引用。材料来源、提交者和查询实现身份由服务端记录；客户端的“已执行/未执行”字段只是待核实陈述，不直接改写工具状态或释放许可。不可传任意 shell 命令作为核对函数。

处理顺序：检查调用者权限、目标和当前观察版本 → 保存受理记录 → 收集/核验来源与证据 → 判断可确认效果及仍未知部分 → SessionManager 追加核对事实 → 发布 tool.state_changed 并返回冲突限制与 canResume 的最新判断。重复提交去重；旧观察已经被更新时重新核验，相互矛盾的材料保留诊断，不用后到请求覆盖已确认结果。

核对过程中不新建业务 Trace/Turn、不调用主模型、不重跑原副作用，也不修改正在消费的模型请求。核对结果只在后续合法视图/投影体现。只有 M12 允许的受信未执行证据可解除一次许可占用，人工材料不能直接替代它；效果已确认也不自动重新放行其他审批。

核对提交与 resume 是两个动作：结果已知、冲突已解除且存在兼容 checkpoint 时，调用者才能显式恢复；恢复必须承接已有结果而不重跑原调用。没有兼容 checkpoint 时返回不可恢复原因，允许明确结束非终态原 Trace 后另起 prompt；cancelled/failed 只更新效果事实，不复活。一次核对成功也不会自动启动 M03 中暂停的旧队列。

### 4.7 SSE 断连、缺口和重同步

持久事件的 SSE `id` 使用不透明持久游标；客户端通过 `Last-Event-ID` 或等价显式参数重连。临时 delta 不推进持久游标，它自带 streamId/chunkSeq。客户端不能把最近一个持久 ID 当作之后所有 token 的唯一 ID。

服务端声明最早可重放游标。游标有效时发送缺失持久事实并接入实时流，保证重放与实时交接无遗漏；允许边界重复。临时 delta 不保证重放，服务器在新连接提供当前消息聚合快照及流位置，客户端以快照修复显示。

游标过期、进程重启丢失临时流或慢连接发生缺口时，明确返回 `resync_required`。客户端获取一致快照和其游标，再订阅该游标之后的事实。快照覆盖之前的局部视图；快照获取和新订阅之间发生的持久事实不能遗漏。此流程是行为契约，不限定使用哪种缓存或数据库。

连接心跳只检测连接存活，不表示任务进展。订阅可按 traceId 过滤，游标仍使用 Session 提交顺序；不能把其他 Trace 的过滤结果误判为缺口。SSE 关闭或框架 reader EOF 不代表 agent_end，更不代表 `trace.settled(completed)`。

### 4.8 控制钩子与观察订阅验收

| 场景 | 必须观察到的结果 |
| --- | --- |
| 两个观察订阅，一个慢、一个抛错 | Agent、工具和另一订阅继续；错误关联订阅句柄；慢订阅达到上限后注销或要求重同步 |
| 注销后重新订阅 | 旧句柄不再收到新事件；新句柄从声明的快照/游标开始，不重复注册旧处理器 |
| 两个 context 转换器 | 第二个看到第一个的输出；任一转换失败不发送模型；原历史不变 |
| 两个工具拦截器 | 任一明确 block/deny 短路；没有实际 started；拒绝原因和调用身份持久保存 |
| tool_result 后处理器 | 后一个看到前一个的 content/details；原始工具事实保留；模型只收到最终投影视图 |
| hook 超时、普通 hook 异常、tool_call 异常 | 控制点按阶段规则继续/阻止；tool_call 默认 fail-closed；所有结果含阶段和扩展身份 |
| input/before_agent_start/context 控制点 | 观察订阅收不到控制事件；只有装配的控制链可修改；Trace generation 固定 |
| 压缩/重试/队列/模型切换 | 事件与 traceId/turnId/inputId 对应；内部段按 executionId 区分，Trace 完整收尾仅一条 trace.settled 持久事实 |
| 核对查询、人工材料、重复/冲突材料与 resume | 受理不等于确认；只在核对事实提交后更新工具视图，未知效果不被覆盖，恢复不重复副作用，旧队列不自动启动 |

### 4.9 安全与 Web 验证边界

本地验证建议默认仅监听回环地址；浏览器跨源访问遵循明确的来源和调用凭据策略，不能默认允许任意网页调用本地命令工具。若开放远程接入，HTTP 操作、快照、附件与 SSE 必须使用一致的调用者/Session 授权。SDK 由嵌入程序建立信任边界，不能自动沿用网络层身份。

最小 Web 页面只验证已公开的输入、事件、查询、取消、恢复及历史能力。它需要能显示真实状态和结构化错误，但不规定控件、框架或布局。A2UI 未启用时这些能力仍然完整；启用后模型生成的 UI 动作只是候选输入，仍由 AgentSession 验证作用域和有效性，并按注入的策略检查权限。协议版本选择列为未决项，不虚构已完成完整兼容实现。

### 4.10 已核实的证据与教程差异

| 事实 | 本地证据 | 对本产品的含义 |
| --- | --- | --- |
| pi 低层 Agent 会 await listener，产品 AgentSession 的外部订阅直接调用 | [低层派发](../../pi/packages/agent/src/agent.ts#L588) `[VERIFY: pi/packages/agent/src/agent.ts:588]`；[产品派发](../../pi/packages/coding-agent/src/core/agent-session.ts#L568) `[VERIFY: pi/packages/coding-agent/src/core/agent-session.ts:568]` | 不能把“订阅永不阻塞”概括为所有 pi API 的事实；本产品显式约定隔离 |
| pi 产品先发外部事件，再处理 message_end 持久化 | [事件处理](../../pi/packages/coding-agent/src/core/agent-session.ts#L644) `[VERIFY: pi/packages/coding-agent/src/core/agent-session.ts:644]` | 本产品对 durable 事实要求先提交后发布，是自己的产品保证 |
| pi 工具调用拦截读取返回值；异常向外冒泡 | [emitToolCall](../../pi/packages/coding-agent/src/core/extensions/runner.ts#L932) `[VERIFY: pi/packages/coding-agent/src/core/extensions/runner.ts:932]` | 观察通道不能承担权限决定 |
| 本地 pi 在 prepareToolCall 之前发 tool_execution_start；被拦调用也有该事件 | [顺序执行](../../pi/packages/agent/src/agent-loop.ts#L444) `[VERIFY: pi/packages/agent/src/agent-loop.ts:444]`；[拦截](../../pi/packages/agent/src/agent-loop.ts#L619) `[VERIFY: pi/packages/agent/src/agent-loop.ts:619]` | 不采纳教程“被拦截不会产生工具事件”的概括；本产品分别定义 requested 与实际 started |
| Eino AgentEvent 包含输出、动作与错误；MessageStream 是独占可读取对象 | [事件](../../eino/adk/interface.go#L419) `[VERIFY: eino/adk/interface.go:419]`；[流约束](../../eino/adk/interface.go#L459) `[VERIFY: eino/adk/interface.go:459]` | 运行时转换后分发；不将 reader 直接暴露给多个客户端 |
| OnAgentEvents 返回错误可以结束 TurnLoop | [回调契约](../../eino/adk/turn_loop.go#L594) `[VERIFY: eino/adk/turn_loop.go:594]` | 外部观察者错误不能直接作为该回调错误返回 |
| Eino middleware 可包装工具并在模型前重写状态 | [工具包装](../../eino/adk/handler.go#L183) `[VERIFY: eino/adk/handler.go:183]`；[状态重写](../../eino/adk/handler.go#L160) `[VERIFY: eino/adk/handler.go:160]` | 控制钩子可以薄适配，仍须补产品顺序、权限和错误语义 |
| pi 观察订阅返回注销函数；扩展上下文是只读 SessionManager + 受控操作 | [subscribe](../../pi/packages/coding-agent/src/core/agent-session.ts#L826) `[VERIFY: pi/packages/coding-agent/src/core/agent-session.ts:826]`；[ExtensionContext](../../pi/packages/coding-agent/src/core/extensions/types.ts#L307) `[VERIFY: pi/packages/coding-agent/src/core/extensions/types.ts:307]` | 产品要求注销幂等、Session 关闭释放订阅；控制 hook 不直接写 SessionStore |
| pi 的控制点有工具前拦截、工具后转换、context 链、input 转换和 provider 前后处理 | [tool_result](../../pi/packages/coding-agent/src/core/extensions/runner.ts#L877) `[VERIFY: pi/packages/coding-agent/src/core/extensions/runner.ts:877]`；[context](../../pi/packages/coding-agent/src/core/extensions/runner.ts#L984) `[VERIFY: pi/packages/coding-agent/src/core/extensions/runner.ts:984]`；[input](../../pi/packages/coding-agent/src/core/extensions/runner.ts#L1196) `[VERIFY: pi/packages/coding-agent/src/core/extensions/runner.ts:1196]` | M07 以窄契约吸收阶段和组合规则，不复制扩展 API 全量重载 |

## 5. 风险与未决项

本章完成接入和事件需求拼图，不据此提前确定开发排期。

| 编号 | 选择/风险 | 建议默认值与影响 |
| --- | --- | --- |
| EVT-D1 | HTTP/SSE 与其他传输的首个实现 | 建议 HTTP + SSE；保持 SDK 契约可用，A2UI 为外层可选映射，用户尚未确认具体协议版本 |
| EVT-D2 | 持久事件保留窗口与订阅缓存大小 | 必须有限且可查询；具体数值待运行环境确定，过期通过快照恢复，不承诺无限 token 回放 |
| EVT-D3 | 控制钩子超时与可取消性 | 权限超时不放行；上下文失败不继续；具体超时数值与工具策略统一确认 |
| EVT-D4 | 本地测试服务是否需要远程开放 | 建议先只开放回环地址；远程身份和多用户隔离需求需另行确认，不能由增加一个 HTTP 入口推导出来 |
| EVT-D5 | 发布事件与历史提交的一致性 | 必须满足 EVT-02/07；实现方案需验证崩溃窗口，不用“事件驱动”四字代替可靠提交设计 |
| EVT-D6 | A2UI 适配范围 | 当前只承诺适配位置和动作校验边界；选定版本与具体交互场景后再确定兼容验收，不影响基础 Web 验证 |

章节评审通过条件：每个公开操作能对应 Trace 状态与执行尝试规则，每种事件有持久性与错误语义，断连、重复请求、等待输入和取消都能通过公开接口完整解释；L1/L2 不出现网络或页面依赖。
