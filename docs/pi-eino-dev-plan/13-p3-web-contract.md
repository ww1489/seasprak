# 13 P3 Web 接入契约

状态：2026-10-02 按当前源码同步双 Agent 独立分层。Code Agent 与 Workflow Agent 是同级 SDK 能力，Web 是业务层；细则见 [职责边界](../sdk-scope.md)。旧会话内工作流 target、定义、编译、图、执行及节点状态已退出 Code Agent；独立 Workflow Agent、公开工厂、三类资源路由与独立前端视图已接线，有对应默认测试与 SDK 消费者。以下区分当前 HTTP 白名单、SDK 能力与未来目标；Code 原生预检回调隔离的有界接受与唯一方法见 [09 §checkpoint](09-persistence-and-recovery.md#checkpoint)，不外推 Workflow 原生整图或跨运行恢复。最终文件集的父全仓、真实浏览器/live 与平台结果统一见 [12 当前证据与限制](12-delivery-and-validation.md#当前证据与限制) 和 [P3 验证记录](../p3-verification.md)，P3 未完成，macOS 延期未验。历史网络契约/来源与失败证据保留，未接线能力不能空成功。

## 1. 范围与包边界

- 新增 `cmd/web --web`，保留 `cmd/agentd` 的 help/version 与仅标准库依赖限制。
- 单一本地 Principal，只监听回环地址。网页不开放宿主 shell，也不接受 Go 代码、任意后端实现或容器挂载。
- 前移历史分支、上下文预算、Eino 压缩接线，以及启动时注册的多 Agent/静态工作流。尚未实现的能力不宣称可用。
- P4 保留完整资源/Skills 发现加载及其余上下文条目；P5 保留 Coze 导入、缺参业务问答、热重载、多 generation 生命周期和通用扩展 hooks。前移不等于 P4/P5 整体完成。
- `internal/web` 分别调用 `internal/codeagent` / `internal/workflowagent` 的受控入口，不 import `sdk`；公开 Go 类型仍只在 `sdk/sdk.go`。两类上层互不导入，日志、状态、预算、审批及恢复独立。HTTP/A2UI 不读写 Manager/Store；跨 Agent 编排和工具组合由 Web/业务负责，不新增 SDK 联动框架。两类上层、共享存储与 Web 三类资源路由均已接线；Web 拥有目录、创建去重、名称标签与上传展示，Code 保留附件准入、模型展开及原授权。

## 2. 启动、身份与授权

当前启动方式：`go run ./cmd/web --web --workspace <绝对目录> --state-root <绝对目录> --config <受信配置>`。缺工作区不采用 cwd；启动配置仅引用获准模型、后端及凭据源，不由网页上传。生产启动不读取 `.test_env`。

启动生成随机 bearer，通过受保护本地文件提供，仅输出其路径，不输出秘密值。浏览器只在内存保存凭据，使用带 Authorization 的 fetch（包括 SSE），不使用 URL、localStorage 或原生 EventSource 携带凭据。默认关闭 CORS，验证 Host 和存在时的 Origin，静态页面不内嵌 token。

认证后由服务端注入稳定本地 Principal；token 轮换不改变会话/幂等身份。所有快照、事件、产物、审批及修改入口按所属资源执行同一授权规则：会话检查 sessionId，工作流运行检查 runId；访问其中一个不授予另一个的权限。工作区只允许启动装配授权的范围，客户端字段不扩大授权。

## 3. 请求与回执

路由前缀 `/v1`，JSON 字段 lowerCamelCase。修改操作使用 `Idempotency-Key`，`expectedRevision` 为 JSON 整数；浏览器遇到超出安全整数的值必须拒绝发送而非舍入。拒绝未知字段、多份 JSON 值和超出 4 MiB 的 JSON 请求。附件另设独立限制，不因 multipart 绕过限制。

输入分为两种资源：Code Agent `/v1/sessions/{sid}/inputs` 允许 kind、targetTraceId、普通 targetAgent、content 及公开引用；Workflow Agent `/v1/workflow-runs` 允许受信定义引用、固定版本、结构化 input 与获准工作区，不接收会话 targetAgent、steering 或聊天历史。工作流 schema 校验先于受理和副作用；业务补参不在本轮。text 按用户输入接纳，附件先保存并授权。两种输入均拒绝自填系统角色、provider Extra 和身份/授权字段。

普通 InputReceipt 保留 inputId/traceId/actualKind/targetAgent/state/acceptedCommit；当前 HTTP 的 targetAgent 为最终名称字符串，acceptedCommit 为十进制字符串，不能照搬 SDK 的目标结构或 uint64 JSON。持久 OperationReceipt 保留 operationId/state/target/acceptedCommit/scope，执行结果通过 GET operation 查询；Code HTTP acceptedCommit 为字符串，Workflow HTTP acceptedCommit 为安全整数。202 只证明接纳，不表示执行完成；工作流 POST 创建返回原受理 snapshot（初始 running 的 revision/cursor 等），不是 SDK WorkflowInputReceipt，也不是重试时最新状态。网络 DTO 不直接序列化 SDK Snapshot、FrozenExecution 或内部证据；明确 open 使用 JSON `{}` 且不要求幂等键，不自动 Resume。

幂等作用域为 Principal/资源类型/资源 ID/operationKind/key。会话使用 sessionId，工作流使用独立 runId；创建先持久预分配相应类型身份。同键同内容返回原回执，异内容 409 idempotency_conflict；查重先于 revision 和版本选择。规范化保留数字及数组顺序，对象键顺序不影响等价。两类创建响应丢失均不得另建执行。

### 3.1 审批实例回执例外

一次批准仍仅存在当前运行实例内存。当前 HTTP 回执使用 `scope="instance"` 与 `instanceId`；Workflow SDK 对应字段为 `ReceiptScope`（JSON 名 `receiptScope`），Code SDK `OperationReceipt` 仅有四个原字段，实例 scope/instanceId 由 HTTP 路由投影补充，不能创造 SDK 字段或混用两层字段名。审批 `target` 为原 `interactionId`（iid），`acceptedCommit=0` 不伪装落盘。工作流 pause/cancel/resume 等持久控制回执使用 `scope="durable"`，`target` 为原 `runId`（rid），`acceptedCommit>0`，不携带 instanceId；回执始终保留原 accepted 状态，完成/失败另查 operation。Code 的持久回执也遵循其自身目标身份，不能把两类 target 归一化。

审批响应只接受当前 interaction 的 allowed-once/rejected/cancelled，服务端解析冻结描述和 interrupt target，客户端不得提交新 arguments/env/grant。审批请求携带其看到的 instanceId；不同实例返回 409 state_conflict，不接受旧决定。重复同键查询仅保证该实例内有效。重开丢弃旧批准和临时回执；显式 Resume 按当前策略重新询问。审批事件只能作为临时实例事实，不伪造 durable cursor。实例变更后客户端清除旧审批控件并重取快照。

工作流业务缺参在执行前拒绝，不将 answer 或主模型猜参作为此次静态工作流补参通道。

### 3.2 断连与受理边界

HTTP 请求取消只终止尚未受理的工作或当前查询/订阅。已经持久接纳的工作由所属 Code Agent 或 Workflow Agent 管理，断连不调用 Cancel/Close，不另建重试 Trace 或工作流运行。显式取消返回 cancelling/operation 状态，只有真实停止后提交的终态才可显示停止。

核对可能先接纳再失败，必须保留 operationId。核对从原调用解析内部授权，不允许任意 shell 取证。批准、核对完成均不隐式 Resume。

## 4. P3 路由范围

遵循 06 的路径及 DTO，当前 mounted 能力与完整后续目标分开：

- 会话创建/列表/元信息和显式 open，inputs，trace 查询/cancel/resume，interactions responses，queue continue，reconcile，operations。
- messages 稳定分页、branches 查询/创建/activate、compactions、metadata 名称标签已接线；模型/工具选择是已有 SDK 能力但当前 Web 未挂载，不写成已接通路由。
- capabilities 只列 Code Agent 普通目标；独立 `/v1/workflows` 查询受信启动定义，`POST /v1/workflow-runs` 创建并受理结构化运行；运行详情/open/pause/resume/cancel/interactions/events 均在 `/v1/workflow-runs/{rid}` 下，不进入会话队列。受限附件和产物仍按所属资源授权，未接线工作流附件明确不支持。
- A2UI 增加 `GET /v1/sessions/{sid}/render`（当前投影 JSONL）及 `GET /v1/sessions/{sid}/ui/events`（SSE），不改变 `/events` 的产品事件语义。render 响应头 `X-Session-Cursor` 标记所用一致视图的 cursor，页面随后订阅其后的 UI 事件。
- 附件保存采用 `POST /v1/sessions/{sid}/attachments`，只保存材料，不触发模型；返回成功保存的 artifactId/mimeType/size，文件名仅为显示数据，不作为存储路径。
- 输入 `content` 除 `{"type":"text","text"}` 外可含 `{"type":"attachment","artifactId"}`（每条输入最多 8 个，不可重复，块内字段互斥）。受理时校验附件属于本会话且当前目标模型声明对应模态（图片需 `input_image`），否则 422/404/400 且零写入；Code Agent 的 steering 不接受附件；独立工作流入口本轮不接线附件，明确拒绝而非转换为会话输入。历史只保存 artifactId；构建模型请求时文本附件转为带名称的 `UserInputText`（单个 256 KiB 上限，UTF-8 边界截断），图片转为 base64 `UserInputImage`。附件缺失或校验失败时执行前失败，物理请求为零。

浏览器选择会话只是更换查询/观察目标，不能冒充尚未实现的带 before_switch 协调操作。本次不提供 `/switch` 生命周期能力、`resources/reload`、扩展 `commands/{name}`、动态导入和用户 shell `executions/commands` 可执行路由。能力查询明确缺失；禁用能力不得用空成功响应代替。

错误映射沿用现有错误码：400 invalid_argument；401 unauthenticated；403 permission_denied；404 not_found；409 state_conflict/idempotency_conflict/incompatible_version/incompatible_resume/reconciliation_required；410 resync_required（已确认游标过期）；422 unsupported_capability/budget_exhausted；503 storage_unavailable/resource_unavailable；500 internal_error。内部原始错误不直接序列化。流已开始时无法再改 HTTP 状态，发送有界重同步诊断并关闭；网络不可写时直接关闭。

## 5. cursor 与重放交接

持久 cursor 为带资源类型的版本化不透明位置：会话保留 `base64.RawURLEncoding(JSON([1, sessionId, durableSeq十进制字符串]))`；工作流使用独立格式 `base64.RawURLEncoding(JSON([2, "workflow", runId, durableSeq十进制字符串]))`。无填充，要求重新编码完全相同，最大 512 字节；错类型、错身份或未来位置拒绝。cursor 不是授权或签名，每次验证所属资源访问权限；两类游标不串用。新工作流格式已在快照、路由、SSE 与前端重连接线，有对应默认测试；认证以最终验证记录为准。

- 空 cursor 表示从起始位置 0 重放；合法当前位置返回实时订阅。
- 格式、版本、资源类型或资源身份错配及未来位置返回 400 invalid_argument；确已过期返回 410 resync_required。默认全日志保留，不为 P3 额外裁剪。
- Code snapshot 提供 cursor、durableSeq（十进制字符串）、earliestReplayCursor、instanceId 和临时聚合；Workflow 当前 HTTP snapshot 没有 earliestReplayCursor 或临时聚合字段，不将 SDK 的更多字段原样输出。revision 是安全 JSON 整数（最大 2^53−1），与 durableSeq 不能互换。
- Code Agent snapshot 只含自己的普通 invocations 和 pendingReconciliations，不含 workflowNodes；字段继续白名单、空列表为 `[]`。Workflow HTTP snapshot 白名单为 runId、definitionName、definitionVersion、state、revision、cursor、durableSeq、instanceId、executionStopped、canResume、workflowNodes[]、interactions[]，可选 errorCode/failedNode；仅 completed 时输出 END 选定的 result。节点项只含 nodeExecutionId/nodeId/kind/state；交互只在 paused 且 executionStopped 时展示当前实例 ready 项的 interactionId/nodeExecutionId/question/options/instanceId。SDK 的 ModelAttempts/Observations/Operations/Usage/Transient 及私有 TODO 不因此进入 HTTP；principal/policy/manifest/bindingVersion/toolCallId、参数、中间节点结果、原始错误和内部证据全部不下发。业务组合只公开允许的外层结果/引用，不复制另一资源节点状态。
- query cursor 和 Last-Event-ID 同时出现须逐字相同，否则拒绝。会话按 traceId 过滤仍使用本 Session 全局位置；Workflow 当前不提供节点过滤查询，仍使用本 run 全局位置，不把节点空档当成丢失。
- 所属运行的一次协调操作固定交接 B、临时视图和实时缓冲；离开 mailbox 后分页交付 `(K,B]`，再交接聚合，最后交付 B 后实时事件。不能先快照后订阅而没有缓冲。
- Workflow SSE 先发 `event: ready`，data 为 handoff typed cursor 与 live；每条事实统一 `event: event`，data 保留原 Type（JSON `type`）、runId、occurredAt，以及可选 nodeExecutionId/cursor/eventId/payload，不将产品类型直接作为 SSE dispatcher。持久状态 payload 按白名单投影，临时模型/工具/审批帧仅用于刷新，不带内部正文。持久帧 id 为本运行 typed cursor，临时帧无 id；只读重放结束发 end，慢订阅发 resync，心跳和逐写期限 15 秒。
- 持久事件保留原 eventId/durableSeq；临时事件无 SSE id。重放分页有界，不将历史塞进实时缓冲；慢订阅不反压执行。
- 一条事实产生多条 A2UI 帧时仅最后一帧携带该事实 cursor。断线半组重复投影必须幂等；没有 UI 变化的事实可通过独立 cursor 传输控制帧推进，不伪造 A2UI 组件消息。
- SSE 心跳、逐写期限均 15 秒；不设置长连接总 WriteTimeout。使用完整 SSE 帧解析，覆盖多行 data、跨块 UTF-8 和 CRLF。

临时模型聚合是最新 snapshot 的替换，工具输出是按 call/stream 的有界预览。界面身份来自稳定产品 ID，不用历史数组下标。instanceId 改变时清理旧临时流；终态提交成功后清理，迟到输出不复活旧流。既有 Code Agent 聚合限额集中于 `internal/config`；独立工作流迁移须分别验证每流/总量边界与资源隔离，不能仅凭既有会话测试认定通过。

## 6. Code Agent 的 Eino 摘要接线约束

本节只适用于 Code Agent 历史、分支与压缩，不为独立 Workflow Agent 增加聊天上下文或自动摘要循环。

使用 Eino v0.9.21 `adk/middlewares/summarization`，Agentic 泛型；候选输入为独占副本，TokenCounter/GenModelInput/Finalize 自定义。显式 Summarize 不检查自动阈值，阈值由唯一产品上下文入口判断。

摘要模型调用有独立 purpose，仍计产品逻辑/实际请求及活动预算，不伪装普通业务助手消息。自定义 Finalize 取代默认后处理，必须自行验证标题、H/P/K、工具配对、文件 facts 和最终请求预算；Callback 仅通知，不承担唯一提交。不能同时启用两份自动摘要 middleware，也不能因框架返回摘要就提前替换持久投影。

框架探针只验证接口语义。产品候选提交、分支继承、崩溃恢复仍由后续 Steps 9–12 的真实路径测试认证。

## 7. 固定 A2UI 来源和安全改造

来源仓库 `cloudwego/eino-examples`，固定提交 `a6dbd95ab51fe9896a2bafa2e5a468e3bed01161`（2026-09-29 通过 git ls-remote 核对）。仅复用所需子集，不添加浮动 main 依赖或引入整个示例服务器。

- [消息类型](https://github.com/cloudwego/eino-examples/blob/a6dbd95ab51fe9896a2bafa2e5a468e3bed01161/quickstart/chatwitheino/a2ui/types.go)：beginRendering/surfaceUpdate/dataModelUpdate/deleteSurface，Text/Column/Card/Row。
- [事件转换参考](https://github.com/cloudwego/eino-examples/blob/a6dbd95ab51fe9896a2bafa2e5a468e3bed01161/quickstart/chatwitheino/a2ui/streamer.go)：仅参考 UI 投影，不直接使用其执行 reader/持久化逻辑。
- [前端参考](https://github.com/cloudwego/eino-examples/blob/a6dbd95ab51fe9896a2bafa2e5a468e3bed01161/quickstart/chatwitheino/static/index.html)：保留组件树和数据绑定，替换原始 HTML/错误渲染、无界消息积累和自动 preempt 行为。
- [许可证](https://github.com/cloudwego/eino-examples/blob/a6dbd95ab51fe9896a2bafa2e5a468e3bed01161/LICENSE-APACHE)：Apache-2.0；源文件标注 Copyright 2026 CloudWeGo Authors。复制代码时保留版权、完整许可和修改说明；2026-09-29 固定来源时尚未复制实现，后续会话 A2UI 实现状态以 [P3 验证记录](../p3-verification.md) 为准，不认证新工作流视图。

`interruptRequest` 按示例扩展而非完整标准能力描述；本项目映射产品 interactionId，不公开框架中断地址。页面审批/取消按钮来自受信控制区，不执行模型给出的 action/HTML。服务端转换公开产品事实，不向浏览器泄漏 provider Extra、原始后端错误和秘密；内容使用安全 DOM 文本渲染，不加载浮动 CDN 脚本。

### 7.1 P3 A2UI 呈现协议（2026-09-30 固定，Step 18/19 共用）

本轮 Code surface 的端点：
- `GET /v1/sessions/{sid}/render`：`application/jsonl`，每行一条 A2UI 消息，重建当前完整 surface（首行 beginRendering）。响应头 `X-Session-Cursor` 为所用一致视图的持久 cursor（同 §5 编码）。只读 Browse，不打开 writer。
- `GET /v1/sessions/{sid}/ui/events?cursor=`（或 `Last-Event-ID`，规则同 `/events`）：SSE。`event: ready`（data 为 `{"handoff":"<本资源交接 cursor>","live":true}`，live 按实际订阅状态返回）；`event: a2ui`，data 为一条消息，一条持久事实产生的帧组只有最后一帧带 `id: <cursor>`；持久事实无 UI 变化时发 `event: cursor` + `id`，data `{}`；临时事实（模型快照、工具输出预览）的帧不带 id；`event: resync` / `event: end` / `: heartbeat` 同 `/events`。

消息信封（恰好一个字段）：`beginRendering{surfaceId,root}`、`surfaceUpdate{surfaceId,components[]}`、`dataModelUpdate{surfaceId,contents[{key,valueString}]}`、`deleteSurface{surfaceId}`。不使用示例的 `interruptRequest`，审批改用下述 Approval 组件。

组件 `{id, component}`，component 恰好一个字段：
- Eino 示例子集：`Text{value?,dataKey?,usageHint?}`、`Column{children}`、`Card{children}`、`Row{children}`。
- 产品扩展：`ChatMessage{messageId,role:"user"|"assistant"|"tool"|"summary",status:"final"|"streaming",dataKey}`、`Task{traceId,state,targetAgent,settled}` 只用于 Code Agent；`ToolCall{callId,name,status}` 可表示所属任一资源的受控调用。工作流运行状态使用自己的 DTO 展示，不伪造 Task/traceId，也不因此新增完整 A2UI 组件承诺。`Approval{interactionId,resourceType,resourceId,traceId?,question,options[],instanceId}` 按所属资源关联：Code 使用 sessionId 且可关联原 traceId，Workflow 使用 runId、不填 traceId；question/options 只来自当前实例受信审批视图。工作流的 `workflow:{rid}` DOM 视图、节点与审批已按本运行 DTO 接线；上述完整跨类型 A2UI 组件/字段映射仍是未来能力，不据协议声明记为交付。
- 进度扩展：`Invocation{invocationId,parentCallId,agent,state}` 仅表示 Code Agent 普通子调用；`WorkflowNode{nodeExecutionId,runId,nodeId,kind,state}` 仅表示 Workflow Agent 节点。两者只含本资源的公开身份与状态，不下发正文、参数或内部错误。workflowNodes 不再取自 AgentSession Snapshot。

身份与更新规则：Code surface 为 `session:{sid}`，包含消息、Task、普通 Invocation 和审批；当前独立 WorkflowView 使用 `workflow:{rid}` 视图身份，从本运行 HTTP snapshot/SSE 直接刷新运行、节点、最终结果和审批，不挂入 session 根。工作流 DOM 视图已接线，不代表新增 Workflow A2UI 消息实现。稳定产品 ID、纯文本渲染、临时帧无 id、终态清理流和本资源 cursor 规则均保留；审批仅当前 instanceId，实例变化后丢弃旧控件。后续 `/v1/workflow-runs/{rid}/render` / `ui/events` 若实现须遵循相同安全语义；本轮没有这两类路由，也未交付完整 Workflow A2UI 组件。本文的 WorkflowNode/跨类型 Approval 描述只规定未来映射边界，不把现有 DTO 视图标成未接线，亦不把它标成 A2UI 或真实浏览器认证。全部文本安全渲染，不下发 HTML、provider Extra、principal、grant 或内部错误。

## 8. 状态与验证

历史框架探针及会话 Web 接线的证据见 [P3 验证记录](../p3-verification.md)，不等于后续最终文件集通过。独立工作流工厂、三类资源路由与独立视图已接线，有 SDK 消费者和持久重开、权限/计量/取消、资源隔离的默认测试路径。Code 原生预检回调修正已按 [09 §checkpoint](09-persistence-and-recovery.md#checkpoint) 获 Step9 有界接受：固定 Eino v0.9.21，每次新建且单次使用探针，只认证原目标根；宿主 codec 仍可先执行，不保证任意宿主零执行或整树。Workflow 当前没有原生整图 checkpoint，节点恢复仍复用自己的已提交事实，不借 Code 修正扩张。Blob H1/M1、13.2e清单 publisher、Workflow.Close 安全错误及可写创建父目录同步均经独立复核和父两平台实际证据有界接受，清单通用失败阶段旧 ID/mode 测试补强建议保留；Repair 完整旧 WRITE_THROUGH 替换暂停不变，方法见 [09 §layout](09-persistence-and-recovery.md#layout)。父最终冻结源两平台全仓/显式消费者及原 Step14 目标均已通过，Windows 文件 symlink/junction 实际执行；唯一授权四协议 live 与原样完整 E2E 已各执行一次并通过，没有自动重跑或追加调用，顺序/计数与外部 Node 原生 env-file 桥接安全条件见 [12 当前证据与限制](12-delivery-and-validation.md#当前证据与限制)。修前回调、历史浏览器/配置失败及修后旧 Web 错误成员断言失败保留，不据 unit/build 或最终绿色记整体 P3 完成。macOS 批准延期未验，Gemini 本轮请求零，动态 OS 保护边界不扩大。
