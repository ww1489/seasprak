# 13 P3 Web 接入契约

状态：2026-09-29 已批准范围，实施中；本文件不是功能已交付证明。执行顺序和逐步验收以已批准的 P3 计划为准。本章细化 06 的网络契约，不改变 02 的身份、03 的调度、09 的提交或 11 的安全规则。

## 1. 范围与包边界

- 新增 `cmd/web --web`，保留 `cmd/agentd` 的 help/version 与仅标准库依赖限制。
- 单一本地 Principal，只监听回环地址。网页不开放宿主 shell，也不接受 Go 代码、任意后端实现或容器挂载。
- 前移历史分支、上下文预算、Eino 压缩接线，以及启动时注册的多 Agent/静态工作流。尚未实现的能力不宣称可用。
- P4 保留完整资源/Skills 发现加载及其余上下文条目；P5 保留 Coze 导入、缺参业务问答、热重载、多 generation 生命周期和通用扩展 hooks。前移不等于 P4/P5 整体完成。
- `internal/web` 调用 `internal/sessions` 的受控入口，不 import `sdk`；公开 Go 类型仍只从 `sdk/sdk.go` 暴露。HTTP/A2UI 不直接读取 Manager 或写 Store。

## 2. 启动、身份与授权

拟定启动方式：`go run ./cmd/web --web --workspace <绝对目录> --state-root <绝对目录> --config <受信配置>`。缺工作区不采用 cwd；启动配置仅引用获准模型、后端及凭据源，不由网页上传。生产启动不读取 `.test_env`。

启动生成随机 bearer，通过受保护本地文件提供，仅输出其路径，不输出秘密值。浏览器只在内存保存凭据，使用带 Authorization 的 fetch（包括 SSE），不使用 URL、localStorage 或原生 EventSource 携带凭据。默认关闭 CORS，验证 Host 和存在时的 Origin，静态页面不内嵌 token。

认证后由服务端注入稳定本地 Principal；token 轮换不改变会话/幂等身份。所有快照、事件、产物、审批及修改入口执行相同 Session 授权。工作区只允许启动装配授权的范围，客户端字段不扩大授权。

## 3. 请求与回执

路由前缀 `/v1`，JSON 字段 lowerCamelCase。修改操作使用 `Idempotency-Key`，`expectedRevision` 为 JSON 整数；浏览器遇到超出安全整数的值必须拒绝发送而非舍入。拒绝未知字段、多份 JSON 值和超出 4 MiB 的 JSON 请求。附件另设独立限制，不因 multipart 绕过限制。

输入字段仅允许 kind、targetTraceId、targetAgent、content 及契约规定的公开引用。text 块按用户输入接纳，不能把 JSON 字符串本身当作文本；附件必须已保存并获授权。拒绝 system、assistant、FunctionToolResult、provider Extra 和任何自填身份/授权字段。

普通 InputReceipt 保留 inputId/traceId/actualKind/targetAgent/state/acceptedCommit。持久 OperationReceipt 保留 operationId/state/target/acceptedCommit，执行结果通过 GET operation 查询；202 只证明接纳，不表示执行完成。网络 DTO 不直接序列化 SDK Snapshot、FrozenExecution 或内部证据。

幂等作用域为 Principal/sessionId/operationKind/key，规范化摘要保留 JSON 数字及数组顺序，对象键顺序不影响等价。查重先于输入分类、目标选择和 revision 校验。同键同内容重放原回执，同键异内容返回 409 idempotency_conflict。创建先持久预分配 sid，创建响应丢失后不能生成新 sid。

### 3.1 审批实例回执例外

一次批准仍仅存在当前运行实例内存。审批网络回执在普通字段外带 `receiptScope="instance"` 与 `instanceId`；`acceptedCommit=0` 不伪装落盘。其他持久操作使用 `receiptScope="durable"`，不携带 instanceId。

审批响应只接受当前 interaction 的 allowed-once/rejected/cancelled，服务端解析冻结描述和 interrupt target，客户端不得提交新 arguments/env/grant。审批请求携带其看到的 instanceId；不同实例返回 409 state_conflict，不接受旧决定。重复同键查询仅保证该实例内有效。重开丢弃旧批准和临时回执；显式 Resume 按当前策略重新询问。审批事件只能作为临时实例事实，不伪造 durable cursor。实例变更后客户端清除旧审批控件并重取快照。

工作流业务缺参在执行前拒绝，不将 answer 或主模型猜参作为此次静态工作流补参通道。

### 3.2 断连与受理边界

HTTP 请求取消只终止尚未受理的工作或当前查询/订阅。已经持久接纳的业务由会话管理，断连不调用 Cancel/Close，不新建重试 Trace。显式取消返回 cancelling/operation 状态，只有真实停止后提交的终态才可显示停止。

核对可能先接纳再失败，必须保留 operationId。核对从原调用解析内部授权，不允许任意 shell 取证。批准、核对完成均不隐式 Resume。

## 4. P3 路由范围

遵循 06 的路径及 DTO，并分批接入真实能力：

- 会话创建/列表/元信息，inputs，trace 查询/cancel/resume，interactions responses，queue continue，reconcile，operations。
- messages 稳定分页、branches 查询/创建/activate、compactions、metadata 名称标签、模型与工具选择。
- capabilities、启动时已注册 workflows、受限附件保存与产物读取、snapshot、events。
- A2UI 增加 `GET /v1/sessions/{sid}/render`（当前投影 JSONL）及 `GET /v1/sessions/{sid}/ui/events`（SSE），不改变 `/events` 的产品事件语义。render 响应头 `X-Session-Cursor` 标记所用一致视图的 cursor，页面随后订阅其后的 UI 事件。
- 附件保存采用 `POST /v1/sessions/{sid}/attachments`，只保存材料，不触发模型；返回成功保存的 artifactId/mimeType/size，文件名仅为显示数据，不作为存储路径。
- 输入 `content` 除 `{"type":"text","text"}` 外可含 `{"type":"attachment","artifactId"}`（每条输入最多 8 个，不可重复，块内字段互斥）。受理时校验附件属于本会话且当前目标模型声明对应模态（图片需 `input_image`），否则 422/404/400 且零写入；工作流目标与 steering 不接受附件。历史只保存 artifactId；构建模型请求时文本附件转为带名称的 `UserInputText`（单个 256 KiB 上限，UTF-8 边界截断），图片转为 base64 `UserInputImage`。附件缺失或校验失败时执行前失败，物理请求为零。

浏览器选择会话只是更换查询/观察目标，不能冒充尚未实现的带 before_switch 协调操作。本次不提供 `/switch` 生命周期能力、`resources/reload`、扩展 `commands/{name}`、动态导入和用户 shell `executions/commands` 可执行路由。能力查询明确缺失；禁用能力不得用空成功响应代替。

错误映射沿用现有错误码：400 invalid_argument；401 unauthenticated；403 permission_denied；404 not_found；409 state_conflict/idempotency_conflict/incompatible_version/incompatible_resume/reconciliation_required；410 resync_required（已确认游标过期）；422 unsupported_capability/budget_exhausted；503 storage_unavailable/resource_unavailable；500 internal_error。内部原始错误不直接序列化。流已开始时无法再改 HTTP 状态，发送有界重同步诊断并关闭；网络不可写时直接关闭。

## 5. cursor 与重放交接

持久网络 cursor 的版本化编码为 `base64.RawURLEncoding(JSON([1, sessionId, durableSeq十进制字符串]))`，无填充，要求重新编码后与输入完全一致，最大 512 字节。它是不透明位置，不是授权能力或签名；每次仍执行 Session 授权。

- 空 cursor 表示从起始位置 0 重放；合法当前位置返回实时订阅。
- 格式/版本/Session 错配或未来位置返回 400 invalid_argument；确已过期返回 410 resync_required。默认全日志保留，不为 P3 额外裁剪。
- snapshot 提供 cursor、durableSeq（十进制字符串）、earliestReplayCursor、instanceId 和临时聚合。revision 与 durableSeq 不能互换。
- snapshot 另含三个白名单列表（空时为 `[]`）：`workflowNodes`（nodeExecutionId/traceId/nodeId/kind/state）、`invocations`（invocationId/parentInvocationId/traceId/targetAgent/state）、`pendingReconciliations`（traceId/invocationId/toolCallId/observationId/observationVersion，字段名与 reconcile 请求一致，只列出 Reconcile 当前会受理的调用）。结果、错误、参数和子调用正文不进入快照。
- query cursor 和 Last-Event-ID 同时出现须逐字相同，否则拒绝。按 traceId 过滤仍使用 Session 全局位置，不把其他 Trace 的空档当成丢失。
- Session 的一次 mailbox 操作固定交接 B、临时视图和实时缓冲；离开 mailbox 后分页交付 `(K,B]`，再交接聚合，最后交付 B 后实时事件。不能先快照后订阅而没有缓冲。
- 持久事件保留原 eventId/durableSeq；临时事件无 SSE id。重放分页有界，不将历史塞进实时缓冲；慢订阅不反压执行。
- 一条事实产生多条 A2UI 帧时仅最后一帧携带该事实 cursor。断线半组重复投影必须幂等；没有 UI 变化的事实可通过独立 cursor 传输控制帧推进，不伪造 A2UI 组件消息。
- SSE 心跳、逐写期限均 15 秒；不设置长连接总 WriteTimeout。使用完整 SSE 帧解析，覆盖多行 data、跨块 UTF-8 和 CRLF。

临时模型聚合是最新 snapshot 的替换，工具输出是按 call/stream 的有界预览。界面身份来自稳定产品 ID，不用历史数组下标。instanceId 改变时清理旧临时流；终态提交成功后清理，迟到输出不复活旧流。具体聚合上限在 Step 8 通过 config 集中实现，并补每流/总量边界测试后才能交付。

## 6. Eino 摘要接线约束

使用 Eino v0.9.21 `adk/middlewares/summarization`，Agentic 泛型；候选输入为独占副本，TokenCounter/GenModelInput/Finalize 自定义。显式 Summarize 不检查自动阈值，阈值由唯一产品上下文入口判断。

摘要模型调用有独立 purpose，仍计产品逻辑/实际请求及活动预算，不伪装普通业务助手消息。自定义 Finalize 取代默认后处理，必须自行验证标题、H/P/K、工具配对、文件 facts 和最终请求预算；Callback 仅通知，不承担唯一提交。不能同时启用两份自动摘要 middleware，也不能因框架返回摘要就提前替换持久投影。

框架探针只验证接口语义。产品候选提交、分支继承、崩溃恢复仍由后续 Steps 9–12 的真实路径测试认证。

## 7. 固定 A2UI 来源和安全改造

来源仓库 `cloudwego/eino-examples`，固定提交 `a6dbd95ab51fe9896a2bafa2e5a468e3bed01161`（2026-09-29 通过 git ls-remote 核对）。仅复用所需子集，不添加浮动 main 依赖或引入整个示例服务器。

- [消息类型](https://github.com/cloudwego/eino-examples/blob/a6dbd95ab51fe9896a2bafa2e5a468e3bed01161/quickstart/chatwitheino/a2ui/types.go)：beginRendering/surfaceUpdate/dataModelUpdate/deleteSurface，Text/Column/Card/Row。
- [事件转换参考](https://github.com/cloudwego/eino-examples/blob/a6dbd95ab51fe9896a2bafa2e5a468e3bed01161/quickstart/chatwitheino/a2ui/streamer.go)：仅参考 UI 投影，不直接使用其执行 reader/持久化逻辑。
- [前端参考](https://github.com/cloudwego/eino-examples/blob/a6dbd95ab51fe9896a2bafa2e5a468e3bed01161/quickstart/chatwitheino/static/index.html)：保留组件树和数据绑定，替换原始 HTML/错误渲染、无界消息积累和自动 preempt 行为。
- [许可证](https://github.com/cloudwego/eino-examples/blob/a6dbd95ab51fe9896a2bafa2e5a468e3bed01161/LICENSE-APACHE)：Apache-2.0；源文件标注 Copyright 2026 CloudWeGo Authors。复制代码时保留版权、完整许可和修改说明；本步骤仅固定来源，尚未复制实现。

`interruptRequest` 按示例扩展而非完整标准能力描述；本项目映射产品 interactionId，不公开框架中断地址。页面审批/取消按钮来自受信控制区，不执行模型给出的 action/HTML。服务端转换公开产品事实，不向浏览器泄漏 provider Extra、原始后端错误和秘密；内容使用安全 DOM 文本渲染，不加载浮动 CDN 脚本。

### 7.1 P3 A2UI 呈现协议（2026-09-30 固定，Step 18/19 共用）

端点：
- `GET /v1/sessions/{sid}/render`：`application/jsonl`，每行一条 A2UI 消息，重建当前完整 surface（首行 beginRendering）。响应头 `X-Session-Cursor` 为所用一致视图的持久 cursor（同 §5 编码）。只读 Browse，不打开 writer。
- `GET /v1/sessions/{sid}/ui/events?cursor=`（或 `Last-Event-ID`，规则同 `/events`）：SSE。`event: ready`（data `{"handoff","live"}`）；`event: a2ui`，data 为一条消息，一条持久事实产生的帧组只有最后一帧带 `id: <cursor>`；持久事实无 UI 变化时发 `event: cursor` + `id`，data `{}`；临时事实（模型快照、工具输出预览）的帧不带 id；`event: resync` / `event: end` / `: heartbeat` 同 `/events`。

消息信封（恰好一个字段）：`beginRendering{surfaceId,root}`、`surfaceUpdate{surfaceId,components[]}`、`dataModelUpdate{surfaceId,contents[{key,valueString}]}`、`deleteSurface{surfaceId}`。不使用示例的 `interruptRequest`，审批改用下述 Approval 组件。

组件 `{id, component}`，component 恰好一个字段：
- Eino 示例子集：`Text{value?,dataKey?,usageHint?}`、`Column{children}`、`Card{children}`、`Row{children}`。
- 产品扩展：`ChatMessage{messageId,role:"user"|"assistant"|"tool"|"summary",status:"final"|"streaming",dataKey}`、`ToolCall{callId,name,status}`、`Task{traceId,state,targetAgent,settled}`、`Approval{interactionId,traceId,question,options[],instanceId}`。
- 进度扩展（2026-09-30 增补，Step 19）：`Invocation{invocationId,parentCallId,agent,state}` 表示一次子 Agent 调用，state 取会话 invocation 状态（running/interrupted/completed/failed/cancelled）；`WorkflowNode{nodeExecutionId,traceId,nodeId,kind:"model"|"tool",state}` 表示一次工作流节点执行，state 取节点记录状态（accepted/waiting/completed/failed）。两者只含身份、名称和状态，不下发子调用结果、节点输出、错误正文或参数。服务端仅在 Snapshot 公开对应事实（`Invocations`/`WorkflowNodes`）后投影；未投影时客户端不得推断。

身份与更新规则：单一 surface `session:{sid}`；根 `root` 为 Column，children 依当前分支消息顺序后接 Task，再接 Invocation 与 WorkflowNode（按产品 ID 排序），最后是 Approval；组件 ID 为 `msg:{messageId}`、`call:{callId}`、`task:{traceId}`、`inv:{invocationId}`、`node:{nodeExecutionId}`、`approval:{interactionId}`，来自稳定产品 ID，不用数组下标。消息正文只经数据绑定 `text:{messageId}` 下发；流式快照更新同一 key（最终消息 ID 等于 attempt 的 MessageID），终态消息以 status final 覆盖。新增组件时 surfaceUpdate 同时下发完整的 root children（覆盖式、幂等）。审批来自当前实例的运行时状态，只作为临时帧（无 id）或 render 内容出现；Invocation 与 WorkflowNode 没有独立事件，render 时来自快照，`/ui/events` 在 trace/tool 类持久事件后重取快照、有变化时以临时帧（无 id）更新；实例变化后客户端丢弃旧 Approval。全部文本按纯文本渲染；不下发 HTML、provider Extra、principal、generation、grant 或内部错误。

## 8. 状态与验证

Step 1 的契约与框架探针已落地，包级验证见记录；Step 2/3 开始实施，尚未完成交付验收。HTTP/SSE、A2UI、分支、压缩和多 Agent/工作流仍不能声明已交付。框架探针、完整检查及平台证据统一记录在 [P3 验证记录](../p3-verification.md)；未执行或失败不记为通过。
