# P3 验证记录

## 2026-09-29 实施开始

环境：Windows/amd64，Go 1.27.0，分支 `feat/p0-p1-runtime`。实施前工作区已有 P2 未提交修改及新测试；保留，不混记为 P3 交付。本轮未提交/推送。

范围：已批准 P3 计划的 Step 1 开始执行，先固定契约和框架探针，未跳到后续生产实现。

### 契约与来源

- 新增 `docs/pi-eino-dev-plan/13-p3-web-contract.md`：细化网络 DTO、审批实例回执、cursor、重放交接和 A2UI 适配边界。
- 06/12/requirements-coverage 同步前移范围，保留 P4/P5 未交付条目，不把范围调整当成功验收。
- `git ls-remote https://github.com/cloudwego/eino-examples.git refs/heads/main`：exit 0，来源固定为 `a6dbd95ab51fe9896a2bafa2e5a468e3bed01161`。GitHub 固定提交目录确认许可证为 `LICENSE-APACHE`；最初请求 `/LICENSE` 返回 404，未将该地址当作有效来源。此步骤尚未复制上游实现。

### 待运行和未交付

新增 `internal/agent/eino/p3_summarization_contract_test.go`、`p3_workflow_contract_test.go`，共 8 个顶层测试、13 个子测试，仅认证框架语义，不认证产品压缩/工作流实现。首次图恢复探针因假设内部中断会自动保留节点输入而失败（exit 1）；核对源码后改为断言恢复输入为零值，并通过 StatefulInterrupt 的显式状态保存/读取所需输入，不弱化前驱不重跑断言。摘要验证自定义输入/计数/Finalize/Callback 顺序、候选副本隔离、错误及取消；图验证已完成前节点不重跑、中断节点再次进入、后节点首次执行，以及 checkpoint 读写/损坏和取消错误。

主线程阅读测试及框架 `Summarize` 实现后独立验证：
- Windows `go test ./internal/architecture ./internal/agent/eino -count=1`：exit 0。
- Windows `go test -race ./internal/agent/eino -count=1`：exit 0。
- Windows `gofmt -l .` 无输出、`go vet ./...`、`go build ./...`：均 exit 0。
- Windows `go test ./... ./sdk/testdata/consumer -count=1`：exit 0（sessions 36.201s）。
- Windows `go test -race ./... ./sdk/testdata/consumer -count=1`：exit 0（sessions 168.529s）。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`：exit 0，代码可达漏洞为 0，保留一项未触达包级、一项模块级漏洞提示。PATH 未找到 govulncheck，因此使用固定版本命令。
- WSL Ubuntu-24.04、Go 1.27.0/linux amd64：`go test ./internal/architecture ./internal/agent/eino -count=1` 与 `go test -race ./internal/agent/eino -count=1` 均 exit 0；WSL 输出本机代理配置提示，不影响此离线测试。

框架责任边界：Summarize 仅浅复制状态，调用方须提供独占深复制材料；成功模型调用后框架可在 context 已取消时继续 Finalize，适配须在 Finalize 和提交前检查取消。Callback 返回错误会否决候选，产品 after 通知不能直接用其错误撤销已提交结果。以上是实际行为，不通过放松产品规则绕过。

这些结果对应 Step 1 文件集，不代替后续生产修改验证。

## 批次 A 启动与认证基础

新增 `cmd/web` 与 `internal/web`，工程限额集中在 `internal/config/limits.go`。仅实现入口/受信模型 Catalog 装配、真实回环 HTTP listener、Host/Origin/bearer 验证、严格 JSON helper 和安全错误投影；不创建 Session，不提供业务路由、A2UI 页面、资源级授权或快照 DTO。后者随后续业务步骤完成，不能将 Step 3 的全部目标标为已完成。

Windows 凭据使用当前用户/SYSTEM 保护 DACL 原子创建；Unix 使用 owner-only 权限并校验所有者。已有不安全状态目录拒绝且不自动修改。凭据值不输出，正常停止清理凭据文件和 listener。`docs/p3-web-startup.md` 记录当前入口与尚未交付范围。

主线程新增 `cmd/web/entry_integration_test.go`，在独立进程运行真实 `run --web` 路径：读取启动 URL/凭据路径、认证请求确认未实现路由为404、发停止信号、等待成功退出、断言文件删除和端口可重新绑定。未把 Start helper 测试冒充入口验收。

开发期保留失败：初始测试因实现缺失失败；受信配置拒绝 `.test_env` 的测试红转绿；Catalog 装配测试补齐既有请求计量 context 后验证真实工厂调用一次。主线程两次提前撞上仍在编辑的文件，分别得到 `requestCounter` 未定义、time 引用未完成替换的编译失败（exit 1）；最终停止编辑后重跑以下门禁通过，不隐藏这些中间结果。

最终独立验证：
- Windows `go test -race ./cmd/web ./internal/web -count=1`：exit 0。
- Windows `go vet ./...`、`go build ./...`：exit 0。
- Windows `go test ./... ./sdk/testdata/consumer -count=1`：exit 0（sessions 38.398s）。
- Windows `go test -race ./... ./sdk/testdata/consumer -count=1`：exit 0（sessions 171.146s，consumer 13.444s）。
- Linux（WSL Ubuntu-24.04，实际执行）`go test -race ./cmd/web ./internal/web ./internal/agent/eino -count=1`：exit 0；`go vet ./...`、`go build ./...`：exit 0。
- Linux `go test ./... ./sdk/testdata/consumer -count=1`：exit 0（sessions 32.219s）。
- Linux `go test -race ./... ./sdk/testdata/consumer -count=1`：exit 0（sessions 167.199s，consumer 13.902s）。
- 最终 `gofmt -l .`：无输出、exit 0；固定 `govulncheck@v1.8.0` 全仓：exit 0，保留两项未触达依赖漏洞提示。
- 本轮早先 `go test -tags live ./internal/llm -count=1`：exit 0，43.150s。最终同范围复验 exit 1，32.558s：Gemini stream_false/stream_true 收到503，错误码 resource_unavailable。按维护者已有决定保留外部服务失败，不修改重试/断言，也不将最终 live 标为通过。
- 新增测试/入口/Web文件常见私钥/凭据模式和冲突标记扫描无命中；测试输出未回显凭据。

macOS 无实际运行环境，运行命令未执行；交叉编译不能算运行验证。完整 P3 尚未交付，批次 B–F 待实施；阶段推进需维护者确认 macOS 运行验证的安排。未提交或推送。

## 2026-09-30 批次 B、C 检查点（Steps 4–12）

范围按批准计划实施，未改变 Steps 13–20 与前端方案。每步先写失败测试（编译红灯或断言失败后才实现）。

实现要点（均以当前源码为准）：
- Step 4：`internal/sessions/catalog.go` 会话目录。创建前在 `stateRoot/catalog/creations` 持久登记预分配 sid（`store/jsonl/catalog.go`，writer 锁、原子替换、Sync、未知字段/尾随 JSON/链接拒绝），完成后保存原始初始快照作为回执；重启后 pending 登记复用原 sid。GET 列表/快照只读打开，不写 journal、不调用模型。`ProfileDefault` 在预留前即拒绝（503），不放宽 Start 的现有规则；Web 启动配置新增显式 `profile`，仅接受空或 `memory`。
- Step 5：`CancelTrace`/`ContinueQueued` 带幂等键与可选 expectedRevision；operation 与 trace 状态/hold 释放在同一 commit（`state.AcceptTraceControl/AcceptQueueRelease`，`SetTraceState` 拆出无提交的 `traceTransition`）。worker 取消只在提交后发生；终态后 operation 完成并记录实际终态；重启时补齐“trace 已终态但 operation 未完成”窗口。旧 `Cancel`/`ContinueQueue` 保持不变。SDK 增加两个请求别名。
- Step 6：`internal/web/operations.go` 接通 inputs、trace 查询/取消/恢复/核对、队列、交互回复、operation 查询。类型化 text 块转为真实用户文本；审批按进程 instanceId 校验，回执标 `scope=instance`；核对的 grantRef 由会话层从原冻结调用解析，客户端提交即 400。
- Step 7：`SubscribeFrom` 在一次 mailbox 操作内固定交接点并注册只接收之后事件的实时订阅，历史 `(K,B]` 在 mailbox 外按页读取（`state.EventsRange`）；未来 cursor 拒绝，注册取消无泄漏，溢出 resync_required。
- Step 8：mailbox 内临时聚合（模型快照替换、每流 64 KiB 尾部预览含截断标记、会话 1 MiB 先淘汰最旧、终态清理，限额在 `config/limits.go`）；`/events` SSE 先重放后实时，持久事件带 cursor id、临时事件无 id，15s 心跳、每写 10s 期限、无总写超时；query cursor 与 Last-Event-ID 不一致拒绝；无 writer 时只读重放结束后发送 end，不打开 writer。
- Step 9：`state/history.go` 历史树（Nodes/Branches/Offpath，旧线性日志按提交重建），Fork/Navigate 要求空闲，路径切换不丢弃旁支；`/branches`、`/messages`（以 entry ID 为游标）。捕获模型输入证明分支隔离，重开后路径一致。
- Step 10：`agent/context_budget.go` 以序列化请求 UTF-8 字节为界：字节数为已证上界（软阈值用），字节/8 为下界（硬拒绝用，只拒绝不可能容纳的请求）。在每次模型调用前检查完整请求（instruction、消息、工具 schema），窗口来自 catalog 模型解析值；超限零物理请求。首次实现直接用字节上界做硬拒绝，导致现有 `TestP2AttemptChatFactoryTruncatedToolNeverRuns`（窗口 4096，请求 8018 字节）被误拒并因测试等待而超时 600s；已按上/下界拆分修正，不放宽测试。
- Step 11：`agent/compaction.go`（H/K 切分不拆工具组、旧摘要只作增量输入、六标题校验、拒绝伪造附录标记、确认的 read/write/edit 文件事实合并，denied/unknown/shell 不计）与 `agent/eino/compaction.go`（`summarization.NewTyped` + 显式 `Summarize`、自定义 GenModelInput/Finalize、无工具、生成后复查取消）。
- Step 12：`Session.Compact` 持久受理 operation，mailbox 固定范围，模型调用在 mailbox 外，提交时核对 branch/leaf 并在同一 commit 追加摘要 Entry 与完成 operation；失败将 operation 置 failed 且旧投影不变。`agent.ProjectHistory` 以最新摘要 + FirstKeptID 起原文投影，重开后仍生效。`POST /compactions` 仅接受 manual。

尚未交付（诚实记录）：自动软阈值触发与已认证 overflow 恢复尚未接到执行路径（估算与 `OverSoft` 已实现并测试，但运行中未调用压缩）；工作前缀 P 摘要、分支摘要、审批等待期间登记压缩意图、子调用压缩隔离未实现；`p3-compaction` 待办因此保持进行中。

检查点验证（Windows，实际执行）：
- `gofmt -l .` 无输出；`go vet ./...`、`go build ./...` exit 0；`git diff HEAD --check` 无问题。
- `go test ./... ./sdk/testdata/consumer -count=1` exit 0。
- `go test -race ./... ./sdk/testdata/consumer -count=1` exit 0（sessions 182.426s，consumer 14.341s）。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` exit 0，代码可达漏洞 0，保留包级与模块级各 1 项未触达提示。
- `go test -tags live ./internal/llm -count=1` exit 1（38.036s）：`TestLocalCompatibleModel` 一项 Gemini 返回 HTTP 503（resource_unavailable），同轮其余 Gemini 请求 200。按维护者决定视为服务端问题，未调整重试或断言，不记为通过。
- 57 个未跟踪文件的冲突标记与常见密钥模式扫描无命中；`.test_env` 被 Git 忽略且未跟踪。
- Linux（WSL Ubuntu-24.04，Go 1.27.0 linux/amd64，实际执行）：`go vet ./...`、`go build ./...` exit 0；`go test ./... ./sdk/testdata/consumer -count=1` exit 0；`go test -race ./... ./sdk/testdata/consumer -count=1` exit 0（sessions 176.908s，consumer 14.391s）。
- macOS 仍无运行环境，未执行，不记为通过。

## 2026-09-30 批次 D Step 13 检查点

- Step 13：新增 `agent/registry.go`（启动时不可变注册清单；内置 main 不可重定义，保持 `main`/`main-v1` 与空 hash，旧日志和检查点不变；自定义目标校验名称、版本、重名、kind，定义 hash 覆盖指令、模型身份、可委派标记与工作流 hash）和 `sessions/agents.go`。受理前由清单解析 `targetAgent`，与 input/trace 同一提交保存名称/版本/hash；未知目标 422 `unsupported_capability`，零写入、不回退 main。`chat` 显式指向另一目标时不再并入活动 Trace（409）；定向输入省略目标即继承、错配 409。执行、调度、`ContinueQueue(d)` 与 Resume 只解析保存目标，版本或定义变化返回 `incompatible_resume`，队列保持阻塞、不换版。目标自带模型时不被会话默认模型选择改写。注册清单 hash 仅在存在自定义目标时进入 Resume 构建指纹。新增 `GET /v1/sessions/{sid}/capabilities`（只读 Browse，列出目标及明确不提供的能力），SDK 增加 `Capabilities`/`AgentDefinition`/`AgentInfo` 别名。`agent/workflow.go` 先固定声明式工作流数据结构；工作流目标执行在 Step 15/16 前明确返回 `unsupported_capability`，不调用模型。
- 测试 `internal/sessions/agents_test.go`：两个不同 fake 模型的实际调用次数、默认路由、未知目标零写、定向继承/错配、忙时 chat 改目标、排队后 JSONL 重开且实现版本变化拒绝继续（零调用、revision 不变）、非法注册拒绝启动。
- 修复既有缺陷：`TestEventsReplayResumeAndReject` 在 Windows 与 WSL Linux 均约 1/5 概率因“HTTP shutdown did not complete within its deadline”失败。goroutine 转储显示 net/http 将已连接但未发出请求的连接（客户端备用拨号）视为活动，Shutdown 等满 5 秒。新增红测 `TestShutdownClosesUnusedConnections`（原始 TCP 连接不发请求，修复前 5.07s 失败），`server.go` 在 Shutdown 期间通过 ConnState 关闭仍处于 StateNew 的连接；这些连接未处理任何请求。修复后 `go test ./internal/web -count=30` 通过。

检查点验证（Windows，实际执行）：`gofmt -l .` 无输出（首次发现 server.go 需格式化，已 gofmt 后复查）；`go vet ./...`、`go build ./...` exit 0；`go test ./... ./sdk/testdata/consumer -count=1` exit 0（sessions 38.413s）；`go test -race ./... ./sdk/testdata/consumer -count=1` exit 0（sessions 184.932s）。本检查点未重跑 govulncheck、live、Linux 全仓与 macOS。Steps 14–20 未开始。

维护者决定（2026-09-30）：同意 Step 14 新增 invocation 持久记录类型（旧日志可读，新日志不向旧版本兼容）；同意 Step 19 联网获取 BeautifulUI 源码、`npm ci` 固定版本依赖及下载 Playwright 浏览器；**macOS 运行验证延期**，待有实际环境或 CI 后补做 build/test/race 与服务启动/监听/关闭/目录保护测试，在此之前 macOS 一律记为未验证，不记为通过。A2UI 呈现协议已在 13 §7.1 固定，供 Step 18/19 共用。

## 2026-09-30 Step 15 检查点（静态工作流校验与图编译）

- `agent/workflow_validation.go`：`CompileWorkflow(def, WorkflowBindings)` 按 10 §5 顺序校验：头部/格式 `seasprak-workflow/v1`/输入 schema（jsonschema v6，离线）→ 全部原始节点（含未连线节点）的 ID/类型/Extra/字段/字面量/占位符/引用存在性及边端点和条件端口 → 模型/工具/子流程绑定、直接与间接递归、工具与子流程 required 参数 → 修剪不可达孤立节点（记入 `Pruned`）后复查引用 → 唯一 start/end、无环拓扑序、end 可达、每个保留节点可达 end、引用目标必须支配引用节点且字段在其声明输出中。错误均为 `invalid_argument`，定位到节点/边/字段。`ValidateWorkflowInput` 完整 schema 校验，缺参直接拒绝。
- `agent/eino/workflow.go`：`BuildWorkflowGraph(ctx, compiled, WorkflowNodeExecutor, store)` 编译为 Eino compose 图；条件节点用 `NewGraphBranch`，状态 map 在 lambda 间传递，汇合用注册的合并函数，start 节点先做输入 schema 校验。
- 与原执行指令的偏离（已接受）：汇合触发模式使用 `compose.AllPredecessor`，而非指令中的 `AnyPredecessor`。`TestWorkflowGraphTriggerModeForJoins` 证明 `AnyPredecessor` 在“工具节点与分支同时汇入 end”时，工具节点一完成就触发 end，已选分支执行 0 次却报告成功；`AllPredecessor` 下 Eino v0.9.21 把未选分支标记跳过，汇合只执行一次且结果正确。测试保留对 `AnyPredecessor` 错误行为的锁定断言，框架行为变化时会失败以提示重新评估。
- 额外约束：模型/工具节点仅一个 string 输出；`truthy` 条件不得有 Right。
- 未交付：节点级 Interrupt/Resume、nodeExecutionId、授权与预算归 Step 16；子流程由执行器 `RunSubflow` 运行，不递归构建子图。
- 主线程独立复验（Windows）：`gofmt -l internal\agent` 无输出；`go vet ./internal/agent/...` exit 0；`go test ./internal/agent/... ./internal/architecture -count=1` exit 0；`go test -race ./internal/agent/eino -run Workflow -count=3` exit 0。全仓检查待各步骤集成后统一执行。

## 2026-09-30 Step 14 检查点（受控子 Agent 委派）

- 存在 Delegable 目标时，`alignTools` 追加内置 `delegate_task`（`delegate-task-v1`，trusted-run，Effect none），计入 manifest；无此类目标时工具清单与旧 manifest hash 不变，同名应用工具被拒绝。调用方无委派目标时不装配、默认选择也去掉。Eino 自动通用子 Agent 保持关闭。
- 调用链：模型 tool call → 既有执行器（冻结、授权、预算 claim）→ `runDelegateTask`：mailbox 内 `startDelegation` 核对已 claim 的原调用、目标属于调用方 `Delegates`、workflow 目标拒绝、并发 4/嵌套 4 准入，提交 `running` 的 `invocation` 记录（新记录类型，`View.Invocations`，终态不可变）；mailbox 外以父工具 ctx 同步运行子 Agent（`einorun.RunDelegated`，无工具、无 Boundary，唯一输入为任务文本），结果按 UTF-8 截断至 `config.DelegateResultBytes` 后作为唯一父工具结果。子 Agent 用独立 InvocationID/ParentInvocationID，镜像账本每次提交先经 `BudgetLedger.ChargeDelegated` 计入父 Trace 共享总额，不推进父 Turn；子消息不写父分支历史。重开时残留 `running` invocation 置 `failed`，父调用按既有 unknown 规则需核对，不重跑。
- 接受的偏离与限制：(1) 子 Agent 失败/取消时父观察为 `succeeded`、SideEffect `none`，失败状态放在结构化结果 `{"status":"failed|cancelled","code"}` 中；原因是 trusted-run 返回 error 会被记为 SideEffect unknown 并阻塞调度，而子 Agent 无工具、不可能产生副作用，故 `none` 是事实。(2) 子模型尝试不写 `ModelAttempts`（该记录要求父 Turn），只在 invocation 记录调用次数。(3) 子 Agent 当前无工具，嵌套实际 1 层；并发上限仅有单元测试，同批 5 个并行委派的端到端场景未测。(4) Step 13 之后、本步之前以 Delegable 目标创建的会话重开会因 manifest 多出 `delegate_task` 而 `incompatible_version`；该窗口仅存在于本未发布工作区。(5) `Snapshot`/SDK 尚未暴露 `Invocations`；子 Agent 暂停/恢复未交付。
- 新增常量 `config.SubagentConcurrency=4`、`SubagentDepth=4`、`DelegateResultBytes`，与 12 限额表一致。
- 主线程已阅读 `sessions/subagents.go`、`state/invocations.go`、`BudgetLedger.ChargeDelegated`，独立复验（Windows）：`gofmt -l .` 无输出；`go vet ./...`、`go build ./...` exit 0；`go test -race ./internal/sessions -run "Deleg|Subagent|Invocation" -count=5` exit 0；`go test ./... ./sdk/testdata/consumer -count=1` exit 0。

## 2026-09-30 Step 16 检查点（工作流节点执行与恢复）

- 独立入口：`SubmitInput` 以工作流为目标时在提交前 `ValidateWorkflowInput`（接受 `{"input":{...}}` 或文本块 JSON 对象）；缺参/自由文本 `invalid_argument`，零写入、零调用。发往工作流 trace 的 steering/follow_up/chat 返回 `unsupported_capability`。`executeWorkflow` 用 `BuildWorkflowGraph` 与会话节点执行器运行，结束追加 `KindCustom`/`workflow_result` 消息；不调用主模型、不创建 Turn/ModelAttempt/providerCallId/FunctionToolResult。
- 节点身份：新记录 `workflow_node`（`nodeExecutionId = invocation:节点路径:1`），accepted 先提交、completed/failed 后提交且终态不可变；重跑时已 completed 节点直接复用结果。
- 工具节点：新增 `Executor.RunWorkflowNode`（origin `workflow_node`），复用冻结/授权/预算 claim/票据/观察，仅跳过模型 `ToolSelected`；调用由 `BeginWorkflowNode` 与节点 accepted 同一提交登记，执行器经 `agent.WorkflowToolSource` 查找，查不到即 `permission_denied`。模型/direct 来源判定保留并额外要求非工作流调用；子工作流 scope 仅能进入 tool_* 类端口，模型/Turn/boundary 端口拒绝。主线程已阅读执行器分支与查找函数。
- 模型节点经 `ChargeDelegated` 计入共享预算后以单条用户消息调用，不伪造 Turn。启动绑定 `sessions.CompileWorkflowTarget`（SDK 别名导出）。`delegate_task` 可委派到 Delegable 工作流，与独立入口同一 schema。关闭时运行中工作流 trace 转 paused，Resume 走工作流专用校验分支，在原 invocation 下重跑图，已完成节点不重复执行。
- 接受的偏离：需要审批的工作流工具直接 `permission_denied`（节点 failed，工具未启动）；工作流 trace 的 Pause 返回 `unsupported_capability`（仅 Close+Resume 或 Cancel）；可证明未启动的工具尝试以新调用 ID `nodeExecutionId#k` 重试，已 claim 的调用绝不重跑；中断时仍 accepted 的模型节点在 Resume 时会重新调用并重新计费。
- 未解决：Cancel 后已登记未 claim 的工作流调用无观察、显示 pending（不阻塞调度）；子流程无会话层端到端测试；Snapshot/SDK 未暴露 `WorkflowNodes`。
- 子 agent 在 Step 16 期间两次运行 live 测试均因 Gemini `stream_false` 返回 HTTP 503 失败（外部服务）；主线程随后最终复跑 live 通过，见下文 Step 20。

## 2026-09-30 Step 17 检查点（附件与维护查询）

- `POST /v1/sessions/{sid}/attachments?name=`（原始请求体，`Content-Type` 为媒体类型，需 Idempotency-Key；201/重放 200 返回 `{artifactId,mimeType,size,name?}`）与 `GET .../attachments/{aid}`（单 Range，416 带 `Content-Range: bytes */N`，`nosniff`、清洗后的 `Content-Disposition: attachment`、`CSP: sandbox`）。存于 `stateRoot/sessions/<sid>/attachments/<id>.bin|.json`，先内容后记录、均临时文件+Sync+Rename+SyncDir，读取校验长度与 sha256、拒绝 symlink/junction（Windows 真实 junction 用例实际运行）。限额 8 MiB、每会话 64 个，MIME 白名单并对图片嗅探。保存从不调用模型（测试断言模型调用 0、无 trace）。
- `GET /workflows`（仅 name/version/description/inputSchema）；`GET`/`PATCH`/`PUT /metadata`（名称、标签，幂等；存 `metadata.json`，仅显示用、不写 journal），会话列表/详情带 name/labels。未改状态机与 replay。
- 契约差异：06 路由表原写作 `GET /v1/sessions/{sid}/artifacts/{aid}`，实现与 13 §4 一致为 `/attachments/{aid}`；已以 13 为准同步 06。上传未实现 multipart，大小限制作用于整个请求体。附件到模型多模态输入未交付，`inputs` 仍仅接受 text 块。

## 2026-09-30 Step 20 联合验收（全仓强制检查）

全部在 Steps 13–19 集成后的同一工作区执行：
- Windows：`gofmt -l .` 无输出；`go vet ./...`、`go build ./...` exit 0；`go test ./... ./sdk/testdata/consumer -count=1` exit 0（sessions 47.883s）；`go test -race ./... ./sdk/testdata/consumer -count=1` exit 0（sessions 201.525s，consumer 15.714s）；`go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` exit 0，代码可达漏洞 0，保留包级与模块级各 1 项未触达提示；`git diff HEAD --check` exit 0；`go test -tags live ./internal/llm -count=1` exit 0（38.784s）。
- 前端（Node v24.19.0、npm 11.17.0）：`npm ci`（0 vulnerabilities）、`npm run typecheck` exit 0、`npm run test -- --run` 12 通过、`npm run build`、`npm run test:e2e` 2 通过。
- Linux（WSL Ubuntu-24.04，Go 1.27.0 linux/amd64，实际执行）：`go vet ./...`、`go build ./...` 通过；`go test ./... ./sdk/testdata/consumer -count=1` exit 0（sessions 40.070s）；`go test -race ./... ./sdk/testdata/consumer -count=1` exit 0（sessions 190.706s）。Linux 未运行前端与 live。
- 新文件卫生：120 个未跟踪文件扫描冲突标记与常见私钥/API key 模式 0 命中；无 `*.exe`、`*.test`、`bin/`、`.test_env`、`node_modules`、`test-results` 进入未跟踪清单（后两者已在 `.gitignore`）。
- **macOS：未运行**（维护者决定延期）。build/test/race、服务启动/监听/关闭及目录权限保护测试均待有实际环境或 CI 后补做，不记为通过。

更正（2026-09-30 13:57）：上述全仓检查通过只证明已实现部分的现状，**P3 阶段尚未完成**。对照批准计划，Steps 11/12（P 前缀摘要、自动软阈值与 overflow 恢复、审批等待登记压缩意图、分支摘要、子调用压缩隔离）、Step 14（子调用暂停/恢复、带工具子 Agent 的真实嵌套与并发端到端）、Step 16（工作流工具审批询问与节点 Interrupt/Resume 关联）、Step 17（附件多模态转换）、Step 19（前端继续队列/核对/恢复资格/工作流 schema 表单；e2e 必须以真实 Chromium、真实 Go 服务/持久存储与根目录 `.test_env` 中的真实模型验证 HTTP/SSE 与模型/工具计数、审批一次提交、工作流输入、分支/压缩（2026-09-30 用户决定覆盖旧离线假模型条款；天然零模型调用工作流可零调用验证）；桌面/窄屏与明暗主题检查）及 Step 20 相应端到端项仍是计划内必须项。以下为已记录的剩余范围，不视为已交付：子 Agent 与工作流内审批/暂停恢复；子 Agent 模型尝试的独立记录；Snapshot/SDK 暴露 Invocations 与 WorkflowNodes；附件多模态输入；自动软阈值压缩与已认证 overflow 恢复接线（Step 12 记录）；Approval Card/Prompt Bar 因上游依赖 404 以本地组件替代；macOS 运行验证。P4/P5 其余条目（完整 Skills 发现、Coze 导入、补参问答、热重载、通用扩展 hooks）不在本次范围。未提交或推送。

## 2026-09-30 剩余交付 C（工作流审批、暂停与清理）检查点

- 取代 Step 16 的“审批即拒绝”与“Pause unsupported”偏离。独立工作流工具节点遇审批时创建实例内审批问题并停止执行段；`CommitWorkflowStop` 同一提交将节点置非终态 `waiting`（ApprovalWait）、trace 转 paused+ExecutionStopped，调用保持已登记未 claim 无观察。问题绑定停止段的 ExecutionID（非 runner 检查点），仅在节点 waiting、trace 停止、执行 ID 匹配、调用未 claim 且无观察时可答，原主体/有效期/冻结 hash/策略校验照旧。回答只记录决定；显式 Resume 同一提交将节点改回 accepted 后重跑图，已完成节点复用，等待节点以原 toolCallId 经 `ClaimWorkflowApprovedTool` 一次 claim（要求当前执行、无 Turn/providerCallId、由恢复该停止段的执行消费）；拒绝则节点与 trace failed、工具零执行。重开实例后旧问题 `not_found`，Resume 重新询问。委派子工作流仍拒绝审批（无可停止的节点边界）。
- Pause 在下一节点开始前生效（`workflowGate` 在 mailbox 内控制准入并等待运行中节点结束），trace paused、pause 操作 completed；若已无下一节点则工作流正常完成、Pause 返回 `state_conflict`，与模型 Pause 语义一致。Cancel/终态时已登记未 claim 的工作流调用写入 `skipped/none` 观察；已 claim 无观察者仍走核对。`Snapshot.WorkflowNodes`、SDK 别名 `WorkflowNodeRun`、Web `workflowNodes` 白名单 DTO（不含 result/error/toolCallId）。
- 主线程已阅读 `approval_memory.go` 的并列工作流分支与 `state/workflow.go` 的 `CommitWorkflowStop`/`ClaimWorkflowApprovedTool`：模型审批分支未改动。独立复验（Windows）：`go build ./...` exit 0；`go test -race ./internal/sessions -run "Workflow|Approval" -count=2` exit 0；`go test ./internal/sessions/state ./internal/web -count=1` exit 0。`gofmt -l .` 仅列出 A 正在修改的 `internal/agent/compaction_test.go`，待 A 完成后统一处理。子 agent 越出分工修改 `coordinator.go`（段收尾与终态清理的唯一位置）、`recovery.go`、`interactions.go` 与 `session.go` 一行，并改写了断言旧行为的 `TestWorkflowToolApprovalFailsNodeWithoutRunning`。

## 2026-09-30 剩余交付 D（附件多模态）与 E（前端补齐、真实端到端）检查点

- D：输入 `content` 可含 `{"type":"attachment","artifactId"}`（≤8、不可重复）；受理前校验附件属于本会话、目标模型声明所需模态（图片需 `input_image`）、非工作流目标与非 steering，否则零写入、零模型调用。历史仅存 artifactId；构建请求时由 `expandAttachments` 展开为 `UserInputText`（256 KiB，UTF-8 截断）或 base64 `UserInputImage`。测试 `internal/sessions/input_attachments_test.go`（模型实收文本与图片块、journal 无附件内容、五种拒绝零写零调用）与 `internal/web/artifacts_test.go` HTTP 用例；临时移除展开后用例失败（exit 1）以确认其有效，恢复后通过。
- E：前端补齐继续队列、恢复（canResume）、工作流 `inputSchema` 表单、附件上传与引用、分支变更后自动重新 render；核对 UI 与 fork 摘要复选框已实现但分别等待快照 `pendingReconciliations` 与 fork DTO `summarize` 字段，当前不显示。A2UI 新增 `Invocation`/`WorkflowNode` 组件（契约与前端/Go 组件函数已加，快照接线待 B/C）。`web/tests/fake-openai.ts` 是此前使用的假 OpenAI-Chat 服务；以下离线结果仅作历史记录，按 2026-09-30 用户最新决定不作为浏览器真实模型验收证据，当前测试已改用真实模型透明代理。此前 `cmd/web` 经真实产品工厂接入假服务：中文+emoji 跨块 UTF-8 流式、流式中刷新重连无重复、同幂等键重放一次 trace/一次模型请求、切换会话丢弃迟到帧、模型 XSS 文本、附件送达模型、分支变更重渲染均通过，并断言假服务请求次数与零外部请求。e2e 发现并修复 A2UI 投影未识别 Eino 流式快照块类型 `assistant_gen_text` 导致流式中间文本不显示的缺陷。视觉检查：1280×800 与 390×844、明暗主题截图无横向溢出，Tab 可达输入框与发送按钮（新增本地暗色配色与窄屏单列布局）。
- 启动配置新增 `tools` 白名单（安全相关，主线程已阅读 `internal/web/config.go`）：仅允许会话自有后端的 `write_todos`，未知、依赖宿主后端或重复的名称拒绝，有测试覆盖。
- 历史浏览器阻塞（后续独立初验已通过，见下方）：当时手动压缩 e2e 标为 `test.fixme`——`Session.Compact` 的摘要请求未挂 `llm.WithRequestObservation`，真实 openai-chat 传输在发请求前拒绝（"model request rejected"，假服务 0 次请求），操作 failed；该缺陷属 A，已转交 A 修复。审批 e2e 未写：唯一无需宿主后端的内置工具 `write_todos` 不请求审批，按规定未为测试虚构工具，待维护者决定。

## 2026-09-30 Steps 18–19 检查点（A2UI 投影与本机页面）

- Step 18：`internal/web/a2ui.go`、`a2ui_projection.go` 按 13 §7.1 从公开 Snapshot/产品事件纯投影；`GET /render`（JSONL + `X-Session-Cursor`，只读 Browse）与 `GET /ui/events`（SSE，一条持久事实的帧组仅末帧带 id，无 UI 变化发 `event: cursor`，临时帧无 id）。cursor/Last-Event-ID 校验抽为 `streamCursor` 与 `/events` 共用。`a2ui_test.go` 覆盖黄金结构、两次 render 稳定、半组重放幂等、流式→最终同 key、终态不复活、审批映射仅 interactionId 及私有字段零泄漏。
- Step 19：`web/` React + TypeScript + Vite，依赖精确版本并提交 lockfile；构建输出 `internal/web/static/`（须随代码提交，干净检出后 `go:embed` 才能编译），关闭 sourcemap，无外部字体/CDN。BeautifulUI（MIT，Copyright (c) 2026 Shane Levine，已在 /license 核对）从站点 View code 获取：Loading State、Streaming Text、Task Rows、Tool Chips、Chat 已集成（移除演示数据、远程视频/外链和脚本计时器）；Approval Card 与 Prompt Bar 依赖的 `atoms/Button`、`primitives/GlideMenu` 在站点返回 404，按规则停止接入并改为本地组件；Thinking 因服务端不下发推理内容未接入。来源、sha256、依赖与本地改动见 `web/THIRD_PARTY_NOTICES.md`。额外依赖 `tailwindcss`、`@tailwindcss/vite`（构建期把组件类名生成 CSS）与 `@types/node`（配置类型检查）。
- 认证边界变更（安全相关）：`auth.go` 在相同 Host/Origin 校验之后，仅对不带 Authorization 的 GET/HEAD `/`、`/index.html`、`/assets/<单段文件名>` 返回嵌入页面；其余路径（含全部 `/v1`）认证不变。`static.go` 拒绝编码/字面路径穿越，index 带严格 CSP（`default-src 'none'`、`script-src 'self'`、`connect-src 'self'`、`frame-ancestors 'none'`）。主线程已阅读两处代码。
- 主线程独立复验（Windows）：`go test ./internal/web ./cmd/web -count=1` exit 0；`go test -race ./internal/web -count=2` exit 0；`npm run typecheck` exit 0；`npm run test -- --run` 12 通过；`npm run build` 后 `internal/web/static` 文件 hash 与提交前一致（构建可复现）；`npm run test:e2e` 2 通过，结束后无残留 web 进程。构建产物扫描：无 `localStorage`；`dangerouslySetInnerHTML` 13 处与 `https://react.dev`、`http://www.w3.org` 均来自 React 运行时库自身的属性处理与报错链接，`web/src` 源码中无 `localStorage`/`sessionStorage`/`innerHTML`/`dangerouslySetInnerHTML`/`eval`。
- 顺带修正 `cmd/web` 帮助文字中已过时的“UI 未实现”，`go test ./cmd/web` exit 0。
- 已知限制：审批无持久事件，`/ui/events` 在 trace/tool 类持久事件后重取快照刷新审批（正确但有开销，审批流无专门 Go 端到端测试）；切换分支后当前流不自动重新 render，需重选会话；此前 e2e 无法注入离线模型、依赖模型输出的流程仅由 Go 测试覆盖；该旧验收方式已被用户最新真实模型浏览器决定覆盖，实际结果见下方独立初验记录。仅 Windows 验证，Linux 待 Step 20，macOS 按维护者决定延期。

## 2026-09-30 22:37–22:40 浏览器真实模型独立初验

### 验收方法与安全边界

- 用户最新批准覆盖旧离线假模型条款：全部浏览器 E2E 使用真实 Chromium、测试临时目录中的真实 `cmd/web` Go 进程与 JSONL 持久存储、根目录 `.test_env` 中的真实模型；天然无模型节点的工作流允许零调用验证。此前“2 通过”及假模型结果仅为历史记录，不替代本轮真实模型证据。
- 已阅读 `AGENTS.md`、`docs/DEVELOPMENT-PLAN-GUIDELINES.md`、验收脚本和适用执行/完成验证技能。检查 `web/tests/live-openai-proxy.ts`：原始请求体及 Go 提供的认证头转发至真实上游，响应状态/头与原始响应字节回传；仅重分块、暂停真实流和记录请求形状/次数，不生成或修改模型输出。代理异常只返回脱敏传输错误，不伪造模型成功响应。本轮无需修改代理。
- 模型凭据仅由测试加载并通过 `SEASPRAK_WEB_E2E_API_KEY` 子进程环境传给 Go；启动 JSON 只含 `CredentialRef`，浏览器不获得模型凭据。浏览器鉴权令牌仅在内存使用；截图在连接成功、令牌输入界面移除后拍摄，trace/video/自动失败截图均关闭。补充仅含计数的验收日志，将注册 Agent 指令断言改为布尔值以防失败时回显含模型配置的完整请求体。未读取配置值到聊天，未输出模型端点或凭据。
- 用户批准的真实外部请求只从本地服务端代理发出。浏览器请求通过路由保护只允许本地 Go 服务同源地址；检查外部请求数组和浏览器运行错误的用例均为空。两个批准计划中对应旧假模型/离线浏览器条款已校正；Go 默认离线测试与生产 Web 不自动读取 `.test_env` 的约束保持不变。

### 实际命令与结果（Windows/amd64）

- `node --version`、`npm --version`、`go version`：exit 0；Node v24.19.0、npm 11.17.0、Go 1.27.0。
- `npm exec playwright -- --version`、`npm exec playwright -- install --list`：exit 0；Playwright 1.63.0，锁定版本 Chromium 已安装。`node --input-type=module -e "import { chromium } from 'playwright'; const browser = await chromium.launch(); console.log('Chromium ' + browser.version()); await browser.close();"`：exit 0，实际 Chromium 版本 153.0.8010.12。依赖/浏览器已可用，因此未执行安装或 `npm ci`。
- `npm run test:e2e`（工作目录 `web/`）：**exit 0，17 passed，0 failed，0 skipped，89.117s**。套件通过 `go build -o <临时可执行文件> ./cmd/web` 构建并启动真实服务；未启用 Playwright 重试，未缩小测试范围。
- 真实模型请求 **17 次**，收到上游响应 **17 次**，其中 **17 次为 2xx**。流式文本/刷新重连/同幂等键/切换会话/注册 Agent/安全文本/附件七个场景各一次；分支历史两次；手动压缩三次前置对话加一次摘要；四种视觉场景各一次。无需模型的认证/静态边界检查不发送模型请求；echo 工作流与审批工具工作流均断言零模型调用。
- 工作流审批实读持久 journal：批准前 `todo_update=0`，明确批准并点击恢复、执行终态后 `todo_update=1`；审批卡片消失，真实模型请求仍为零。手动压缩真实摘要调用一次并显示已激活摘要、页面无 alert；此前两项浏览器阻塞在本轮实际运行中均未复现。
- `npm run typecheck`：exit 0。`npm run test -- --run`：exit 0，4 个测试文件、16 个测试全部通过。
- `git diff HEAD --check`：exit 0，仅有现有工作副本 LF/CRLF 提示。对验收脚本、代理、Playwright 配置、验收记录和两份计划补跑 `git diff --no-index --check -- NUL <文件>`：首次发现本次新增记录末尾空行（exit 3），已删除；复查全部无空白错误诊断，逐文件 exit 1 仅表示相对 NUL 有新增内容，包装命令 exit 0。`web/tests/` 新文件冲突标记、常见私钥/API key 模式扫描无命中；`.test_env` 确认被 Git 忽略。测试结束后查询 `web` 进程为 0，截图目录未进入未跟踪清单。未提交、未推送。

### 截图视觉结论与未验证范围

- 已逐张查看 `web/test-results/visual/live-session-1280x800-light.png`、`live-session-1280x800-dark.png`、`live-session-390x844-light.png`、`live-session-390x844-dark.png`。桌面为侧栏/对话双列，窄屏为单列；明暗配色均生效，消息、任务完成状态、输入区域和能力面板可见，无组件重叠或页面横向溢出。四种场景均通过页面宽度断言与 Tab 可达输入框、发送按钮断言。窄屏因会话列表较长需要纵向滚动才能到达对话和输入区；记录为当前布局行为，不宣称首屏均可见。截图未显示模型端点、模型配置值或凭据。
- 本轮无浏览器失败或生产 blocker，未放松产品断言、未添加盲目重试、未修改 Go 生产代码。主线程同时修改 sessions/agent 恢复逻辑，因此本结果是 **独立初验，不是最终集成验收**；待主线程停止修改后应完整复跑 `npm run test:e2e`。
- 本轮未重跑 `npm run build`（避免写入允许范围之外的嵌入资源）、全仓 `gofmt`/`go vet`/`go build`/普通与 race 测试/`govulncheck`/`go test -tags live ./internal/llm` 或 Linux/macOS 运行验证；这些仍由主线程在最终稳定工作区执行，未运行项不记通过。本轮浏览器套件不新增审批重开实例、子调用递归恢复或完整故障窗覆盖；不得凭 17 项通过宣称完整 P3 交付。

## 2026-09-30 子调用安全恢复与最终集成复验

### 实现范围及默认回归

- 沿用既有 Eino child 执行、共享预算和 Session mailbox，不新增 ReAct 循环。单个根 interrupted child 只有在父 Trace 已持久确认停止、child 没有 claimed/unknown 工具时才可显式恢复。invocation 持久绑定兼容构建、完整 child 模型配置、环境、原历史 leaf 和 consumed input；恢复复核原目标、父模型与 Turn、已接受调用、冻结策略及其他未知效果。旧记录缺少恢复绑定时不授权重跑。
- `CommitChildResume` 将 operation、原 invocation 重新运行、Trace 新执行段、旧未 claim child calls 的取消与 `resumed_execution` 关联同一提交。写入和重放均双向检查完整记录集；缺失整个关联、Trace/invocation/旧调用取消记录、篡改旧参数/scope 均拒绝。`CompleteChildResume` 同一提交保存原 child 结局、原 parent observation、按原 `turn.CallIDs` 顺序的完整工具结果组与唯一 turn_end；普通工具收尾也复用 `toolResultRecords`。提交失败不发布候选。
- child 结果提交后，父执行在同一已受理的恢复 execution 内走正常 Eino 路径；`Consume` 对原 consumed input 保持幂等。模型实收请求回归断言原输入和原 child result 各一次；恢复幂等键重放不新建 invocation/call、不再调用模型。
- 取消与 Close 收尾均经 mailbox；立即取消、已知 child 工具后取消、再次 Close 的 invocation/调用结局和累计计数保留。真正未知 child 效果保留 outcome_unknown 并阻止父模型继续。工具回调仅返回内部信号不能抑制 observation，必须由持久 owner 在父 Close 时确认安全；已批准的抑制向框架返回普通取消，内部信号不写入错误。恢复读取 `FrozenExecution.FinalArguments`，不再次运行参数 hooks；UTF-8 截断与 `truncated:true` 一并持久化。
- `subagents_resume_test.go`、`state/child_resume_test.go`、`tools/interruption_test.go` 将上述行为纳入默认套件。各缺陷均先观察到相应失败再修复；独立复核原五项剩余缺陷后逐项补红测并关闭。子调用压缩已有产品回归证明只使用 child 内存消息副本、共享计费且父历史/Turn/operation 不变，本轮未重新实现压缩器。
- **范围限制保留：**多 interrupted 根 child 的联合恢复不支持；已 claim 子工具或已有嵌套 invocation 的 child 不重跑；子 Agent/委派子工作流的交互审批恢复、独立 child 模型尝试记录仍未交付。没有确定性进程 Kill 覆盖全部新增恢复故障窗；当前事务失败、load-only 提交篡改和公开 Close/Open/Resume 测试不替代该认证。完整 P3 仍未完成。

### 最终离线检查（稳定 Go 源码）

- Windows/amd64、Go 1.27.0：初次 `gofmt -l .` 列出 17 个既有未格式化文件，按强制要求执行 `go fmt ./...` 与 `go fmt ./sdk/testdata/consumer` 后全仓无输出。`go vet ./...`、`go build ./...`、`go test ./... ./sdk/testdata/consumer -count=1` 均 exit 0（最终 sessions 64.241s）。先 `go test -race ./internal/sessions/... ./internal/agent/... -count=1` exit 0，再 `go test -race ./... ./sdk/testdata/consumer -count=1` exit 0（最终 sessions 268.314s、consumer 16.157s）。格式化未作为新增 P2 行为交付。
- 实际 WSL Ubuntu-24.04、Go 1.27.0 linux/amd64：逐命令执行 `go vet ./...`、`go build ./...`、`go test ./... ./sdk/testdata/consumer -count=1`，各 exit 0；`go test -race ./... ./sdk/testdata/consumer -count=1` exit 0。早期与 `npm ci` 并行的 Linux 检查出现 node_modules 路径暂时缺失，混合 WSL UTF-16 诊断/Go UTF-8 输出又造成日志乱码，该轮不作成功依据，依赖稳定后完整复跑。后续仅为日志转码的冗余 build 遇 WSL `Wsl/Service/E_UNEXPECTED`、exit -1；直接重跑完整 `go build ./...` exit 0，保留该宿主失败，不归为产品代码修复。
- 最终 `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` exit 0：代码可达漏洞 0，保留包级与模块级各一项未触达提示。`git diff HEAD --check` exit 0；132 个未跟踪新文件逐个 `git diff --no-index --check -- NUL <文件>` 无空白诊断（包装 exit 0）。前两次新文件检查因 Git 中文路径转义/PowerShell 编码错误未完成，修正 UTF-8 后全范围通过。源码、默认测试、文档和计划的冲突标记/常见私钥及 key 模式扫描无命中；`.test_env` 被忽略且未跟踪，禁止二进制与测试产物未进入候选文件。新增忽略 `web/.vitest/`，保留已有内容。
- 前端：`npm ci` exit 0、0 vulnerabilities；`npm run typecheck` exit 0；`npm run test -- --run` exit 0，4 个文件、16 个测试通过；`npm run build` exit 0，仍生成 `index-BPaYNqk1.css` 与 `index-DTYXDFc0.js`。浏览器结果独立见下方。
- macOS 按维护者批准继续延期，未运行；Linux 本轮未运行浏览器或 live 模型，不将 Windows 证据外推为其运行认证。未提交、未推送。

### 真实模型失败及维护者批准的最后一次复跑

- 稳定源码下第一次 `go test -tags live ./internal/llm -count=1` exit 1（75.411s）：Gemini `stream_false` 返回 HTTP 503，公开错误码 resource_unavailable；该分项同轮其他流式/工具请求收到 200。未调整生产重试与断言，失败保留。
- 首次最终 `npm run test:e2e` exit 1：13 passed、4 failed（13.4m）。认证/静态、流式重分块、重连、幂等、切换会话、注册 Agent、安全文本、附件、零模型工作流、审批明确回复+恢复一次提交、分支及真实压缩均通过；四种视觉场景均在真实上游非 2xx 后没有预期回复而超时。各 worker 脱敏计数合计 17 次请求、17 次上游响应、13 次成功；失败轮未记录具体状态码，不能推定全部为 503，也不能沿用独立初验的视觉截图作为这轮通过证据。
- 维护者明确选择补充仅 HTTP 状态码诊断后，顺序完整重跑一次模型与浏览器验收。测试收尾新增纯数字状态码日志，不输出端点、模型配置或凭据，不改变模型响应、断言、超时或重试策略。该轮已经结束：`go test -tags live ./internal/llm -count=1` exit 1（68.371s），OpenAI Chat/Responses、DeepSeek 与 Anthropic 多个子测试返回 resource_unavailable；`npm run typecheck` exit 0；`npm run test:e2e` exit 1，5 passed、12 failed（31.8m）。各 worker 合计 16 次真实请求、16 次响应，全部 HTTP 403、成功数 0。分支页面测试虽被套件计为通过，其对应真实请求亦为 403，因此不认证成功模型往返。此前成功截图不作为该轮视觉通过证据。失败来源尚未确定，不能仅凭 403 归责账号、模型服务或本机网络。

## 2026-10-01 HTTP 403 专项排查

- 用户确认账号与地址可用，并要求排查此前没有出现的 403。核对 Go live helper、浏览器透明代理与实际 SDK 请求链：OpenAI live 配置和浏览器测试都会在缺少时追加 `/v1`，SDK 随后追加 `/chat/completions`；生产工厂直接使用已配置的 Endpoint，不自动补版本路径。本轮未修改生产请求、`.test_env`、凭据、模型配置、重试或验收断言。
- 前一轮定向诊断已确认目标 origin 与本地配置一致、路径为 `/v1/chat/completions`、`v1_segments=1`、Authorization 与本地凭据一致且只有一个、Go 环境代理未被选中。两次请求均 HTTP 403，响应为 235 字节 JSON，固定类别未命中；不输出原响应、任意头值、URL 或配置值。由这些证据可排除该轮请求漏加/重复加 `/v1` 及认证头丢失，尚不能定位拒绝来源。旧诊断在 RoundTripper 前查询 User-Agent，不能据此认定线上 User-Agent 缺失，现改为仅报告是否显式设置。
- `internal/llm/live_http_diagnostics_test.go` 仅在测试中分类错误，收集上限 16 KiB，超限丢弃；读取保持响应字节不变，Close 后释放缓存。补充 JSON 转义字符串解码分类（含中文），先运行转义用例得到预期 exit 1，再最小修改获得 exit 0；隐私、透明读取、精确边界与超限默认回归均通过。所有协议记录实际 HTTP 状态，失败仅输出固定布尔分类和长度，不保留或输出私密内容。
- 当前复现结果发生变化：同一个 OpenAIChat 定向 live 命令，先以扩大网络权限执行 exit 0（6.792s），再以默认权限执行 exit 0（5.641s）；各包含非流式/流式三步真实对话，共各 6 次请求，文本、工具参数、物理计数和用量断言均通过。本轮没有对产品或连接做修复；扩大权限不是已证实根因，当前成功也不说明历史拒绝已永久解决。由于缺少历史配置快照及原始拒绝原因，不能判定是否存在外部配置/网络/服务时变。
- 当前 403 不可复现，因此未再探测产品不用的无 `/v1` 路由。随后顺序完整复验：`go test -tags live ./internal/llm -count=1 -v` exit 0（65.744s），五个实际协议工厂的非流式/流式三步对话合计 30 次真实请求，全部 HTTP 200、全部断言通过；`npm run typecheck` exit 0；`npm run test:e2e` exit 0，17 passed、0 failed、0 skipped（1.5m），真实 Go 服务/JSONL/Chromium 下 17 次模型请求、17 次上游响应、全部 HTTP 200。天然零模型工作流及审批一次提交保持原断言。已逐张查看本轮新生成的桌面/窄屏、明/暗四张截图，无横向溢出或组件重叠；窄屏仍需纵向滚动经过会话列表，不将其描述为对话首屏可见。
- 结论：当前配置与真实调用已通过完整复验，403 此轮未出现；历史 403 原因仍未确认，不能宣称已修复某个账号/网关/网络缺陷，也不将本轮成功覆盖此前失败。若再次出现，利用固定状态与错误分类进一步定位，仍不输出响应原文或本地配置值。完整 P3 的既有未交付项与 macOS 延期保持不变。未提交、未推送。

### 离线门禁中发现的独立测试时序问题

- 新增诊断后的 Windows `gofmt`/vet/build/全仓普通与 race/govulncheck/diff 均 exit 0（sessions 普通 48.114s、race 257.184s）；Linux vet/build/race exit 0，但全仓普通测试 exit 1：`TestContinueQueuedAtomicAndIdempotent` 报“partially invalid batch changed state”。该离线测试使用 testkit，不接触真实模型，因此与 HTTP 403 没有请求链关系。失败轮保留，不记作全仓通过。
- Linux 定向 100 次复现得到 exit 1。先增加纯状态诊断再重复 30 次，14 次失败均显示 `LastSeq 15→16`、排队 trace 的 hold 仍为 true、实际模型调用仍为 1、此前取消操作 `accepted→completed`。核对 `coordinator.finishActivity` 与 `settleTraceOperations`：trace 终态与取消操作完成分别提交。原测试只等 trace cancelled 就取 revision，因而把正常取消操作的最后一笔提交误算为被拒绝队列命令写入。
- 仅修正 `internal/sessions/control_test.go` 的测试同步：保留取消回执，显式等取消 operation completed 后才采样，再保留原 revision、hold、调用次数与幂等断言；没有修改取消/队列生产逻辑、没有睡眠、没有放松拒绝规则。修正后 Windows 与 Linux 各普通 100 次、race 100 次均 exit 0。
- 后续 Windows 全仓普通测试通过，但 race 又暴露既有 `TestP2PrepareHookDeadlineWaitsForRealExit` 的时序失败（该轮 exit 1，工具意外执行一次）。原测试等待另一个 35ms 计时器，未确认 20ms hook context 的取消已经生效；独立计时器到期不保证取消回调已经运行。核对生产 `runBeforeHooks` 在 hook 返回后检查 `ctx.Err()`，本轮没有修改它。仅在该默认测试中传回真实 hook context，显式等待其 `Done`，然后断言不合作 hook 尚未退出，再释放 hook 并保留 DeadlineExceeded、工具 0 次、无 intent、有 cancelled observation 的全部断言。Windows/Linux 各普通与 race 重复 100 次均 exit 0。最终稳定测试文件的全仓复验结果如下。
- 最终 Windows：`gofmt -l .` 无输出；`go vet ./...`、`go build ./...`、`go test ./... ./sdk/testdata/consumer -count=1`、`go test -race ./... ./sdk/testdata/consumer -count=1` 均 exit 0（sessions 普通 51.788s、race 256.606s）。最终 Linux/WSL Ubuntu-24.04 实际执行相同 vet/build/普通/race 命令均 exit 0（sessions 普通 55.851s、race 249.591s）；WSL 诊断编码混合问题仍保留，逐命令退出码及终态均为 0。两项时序修正后的稳定文件集通过，不用先前成功掩盖中间失败。
- 最终固定 `govulncheck@v1.8.0` exit 0，可达漏洞 0，包级/模块级各一项未触达提示保留；`git diff HEAD --check` exit 0，新诊断、排队测试及验收文档逐文件空白检查完成。文档末尾额外空行曾被新文件检查检出，删除后复查；新增测试的常见密钥模式与冲突标记无命中。`.test_env` 仍忽略且未跟踪，未修改任何值、未提交/推送。macOS 依批准延期；Windows live/浏览器成功不外推至 Linux/macOS。

## 2026-10-01 全自动可见页面点击验收

### 方法、授权变更与失败轮保留

- 验收源码为 `28f9678`，开始时实际 Git 工作区干净。用户要求无需人工登录或文件选择；已更新外部执行计划。所有临时辅助程序、可执行文件、状态、观察文件及截图均置于系统临时目录，不加入仓库；生产逻辑与 `.test_env` 未修改，不提交、不推送。
- 先复用 `loadOpenAI` / `LiveOpenAIProxy` 及真实 `cmd/web` 启动配置创建独立 workspace/state，安装会话自有 `write_todos` 和一次审批、reviewer、tool-less Delegable helper、echo-flow、approved-todo-flow、default 模型绑定的 model-flow。配置只保存环境凭据引用，模型密钥仅进入子进程环境。真实代理不生成回答，仅重分块或暂停真实输出；模型端点按现有 helper 归一化，不重复添加 `/v1`。
- Cursor 侧边栏实际点击合成错误令牌后显示“令牌无效或已过期，请重新输入。”；会话与模型请求均为 0。全自动私密登录尝试被工具明确拒绝：`DOM.setFileInputFiles` 不允许。刷新工具目录也没有从本地秘密文件填充的能力。临时文件读取控件已移除，未绕过鉴权、未新增认证路由、未将真实令牌输出。
- 用户明确批准改用独立真实 Chromium 全自动点击，且随后要求观看。早期无头轮被停止，未作为最终结果；第一次可见轮复用旧状态使“未授权工作区后目录仍为空”的基线断言失败，停止并保留失败，不归为产品拒绝失效。随后重新创建全新临时状态，以 `headless:false`、`slowMo:120` 在可见窗口从零执行。停止旧服务不作为优雅停机或恢复认证。
- 业务操作均由真实页面控件触发，包括原生 `setInputFiles` 上传；没有后台业务 POST 代替点击。只读 GET、journal 和代理请求体仅在本地程序内提取状态/次数/布尔值。浏览器令牌仅在本地测试进程与页面内存流转；截图均在登录表单移除后采集，无原始 provider 响应、请求或凭据输出。

### 命令与实际结果

- 临时启动程序由 `node --experimental-transform-types --disable-warning=ExperimentalWarning <临时启动脚本>` 运行；真实 `go build -o <临时 web.exe> ./cmd/web` exit 0，启动成功。最初仅使用 Node strip-only 模式时不支持既有 TS parameter property，发生启动失败；添加 Node 自带的类型转换选项后运行，无产品源码改动。
- 可见完整点击轮：`node --experimental-transform-types --disable-warning=ExperimentalWarning <临时 clicks 脚本>`，exit 1，19 项中 13 通过、6 失败。保留每项脱敏记录；没有把这轮描述为全部通过。
- 五项失败来自本次临时脚本的定位/同步假设：委派结果选择器把“工具结果”和“助手”均算为助手；批准/拒绝后错误地要求审批卡片在显式 Resume 前立刻消失；取消脚本寻找了不存在的“进行中/执行中”任务标签，实际为“运行中”；第二次主题检查只 blur 而未重新定位 Tab 起点。只修正临时脚本，实际再点击完整受影响场景，保留子调用、TODO、取消终态、hold 和请求数断言。
- 定向可见复验：通过环境 `CURSOR_CASES` 选择上述委派、批准、拒绝、取消/队列、四种布局整组，运行同一临时点击脚本，exit 0，5 项全部通过。委派实证所有消息角色有标记 2 处，真正助手回答仅 1 处，child invocation completed 1 次，模型请求恰 3 次；不是通过删除调用计数断言掩盖重复执行。
- 按每个场景最后有效结果汇总：**19 项全部执行，18 项通过，1 项产品行为失败（分支摘要）**。这是一次完整可见轮加有原因的定向复验汇总，不是一次全绿套件。全新状态中的真实模型请求累计 **26 次，响应 26 次，全部 HTTP 200**。其中包含第一次错误队列测试释放后正常运行的排队请求；分支诊断不产生模型请求。本轮未出现 403/503，不能据此宣布历史 403 根因已解决。

### 已通过的页面行为

- 空工作区创建禁用、未授权工作区拒绝且零模型调用、正确路径创建、列表刷新/选择、能力与注册目标展示；令牌不进入 URL、localStorage 或 sessionStorage。
- 中文/emoji 对话、真实生成中窗口、最终回答与任务终态、Enter 发送、Shift+Enter 换行而不提交；快速双击仅 1 input/1 trace/1 真实模型请求。
- 真实流中刷新、自动重新认证并重选会话，没有重复模型请求；切换到另一个空会话不受旧流污染，再选原会话可见结果。
- reviewer 真实请求；通过页面要求 `delegate_task` 到 helper，真实 child completed、父工具结果与最终回答可见，主调用/子调用/父后续共 3 次真实模型请求。
- echo 必填字段拒绝且零 trace/零请求、结构化输出与返回对话；model-flow 的真实模型节点 completed 及结果可见，恰 1 次模型请求。
- 审批批准前 TODO 提交 0；批准本身不会自动运行，明确 Resume 后 TODO 恰 1；拒绝并 Resume 后 failed、TODO 0；等待审批时点击取消后 cancelled、TODO 0。三种工具工作流均零模型调用。
- 真实第一流固定窗口、第二独立任务 queued；点击取消第一条后真实 cancelled，第二条 hold；点击继续队列后原任务只执行 1 次，两任务合计 2 次真实模型请求。
- 页面拒绝不支持的二进制 MIME；文本附件上传本身零模型调用、移除、重新上传、发送后待发送列表清空。请求提示不包含附件独有标记，本地只读检查真实模型请求确含该标记，回答也包含它，恰 1 次请求。
- 不带摘要的消息起点分叉与切回 main：第二轮历史在分叉后消失，切回恢复，分支操作本身零模型请求。手动压缩在三轮真实对话后另调用真实模型 1 次，已激活摘要可见。
- 真实模型返回合成 HTML/script 原文，以普通文本显示；无新增 img/script DOM、未执行合成脚本、无浏览器外部请求。每项记录的浏览器外部请求及 pageerror 均为 0。

### 分支摘要失败与安全结果

- 两轮正常真实对话后，选择历史消息起点、勾选“为离开的分支生成摘要”并点击创建分支。原可见轮失败；随后用同一历史与新分支名再次通过页面复现：HTTP **400 / invalid_argument**，额外真实模型请求 **0**，页面显示“请求参数无效。”。
- 只读比较证明 revision、完整 messages 投影及 branches 投影全部未改变，仍选中 main；没有成功创建分支或激活伪摘要。失败截图已查看，旧两轮历史完整保留。
- 源码核对：`internal/sessions/branches.go` 的 `changeBranch` 直接将 `rt.opts.Model` 传入 `GenerateBranchSummary`，而真实观察型传输要求请求身份及计量观察器；`internal/sessions/compaction.go` 的手动压缩通过 `chargedModel` 装配 `WithRequestObservation`，分支路径没有同样装配。该缺口与发请求前的 invalid_argument/零物理请求吻合。本轮按测试范围保留缺陷，未修改生产实现，也未弱化模型认证或计量要求。

### 视觉、证据位置与未覆盖范围

- 已逐张查看本轮 `click-screenshots/cursor-click-{1280,390}-{light,dark}.png`：1280×800 双列与 390×844 单列，明暗主题有效，完成任务、中文/emoji echo 消息、输入框、能力与分支区域无重叠，无横向溢出；四组均实际 Tab 到达输入框与发送按钮。窄屏长会话列表在对话前，需要纵向滚动，不能称为对话首屏可见。布局图使用天然零模型 echo 的真实页面输出，不将其描述为新增视觉模型请求。最终视口恢复桌面明色。
- 本机证据根为 `C:\Users\admin\AppData\Local\Temp\seasprak-cursor-p3-EzW3BK\state`：`click-results.json` 保留完整轮失败，`click-followup-results.json` 保留定向复验，`branch-summary-diagnostics.json` 仅含状态码与历史未变布尔值，`cursor-observations.json` 仅含脱敏状态/次数，截图在 `click-screenshots/`。文件均未进入 Git。
- 服务保留在 `http://127.0.0.1:52616/`；可见 Chromium 保留在已成功压缩的会话供观看，侧边栏同步为新地址但仍为未登录页面。**Cursor 侧边栏的登录后点击未认证**，不能把独立 Chromium 成果冒充侧边栏完成。
- 元数据编辑、模型/工具选择、主动暂停无本轮页面入口；精确同幂等键 API 重放、未知效果核对的合法故障前置状态、自动压缩/overflow、进程 Kill/重启及 child 故障窗未认证；无人工介入需求的前提下不虚造前置记录。多 interrupted 根 child 联合恢复、委派审批等原有未交付限制保留。图片模型能力不在本轮声明中，未认证图片端到端。上传错误后的“重试上传”需要真实可恢复上传故障，未注入该故障，也未认证此按钮。
- 本轮仅 Windows 页面测试；Linux/macOS 不外推，完整 P3 仍未完成。没有实现改动或提交，本轮未重新运行全仓 gofmt/vet/build 普通与 race/govulncheck/live 五协议及前端 typecheck/build/unit/E2E 既有套件；此前结果保持历史属性，不称为本轮新验证。本轮实际新构建、可见点击、只读事实及文档 diff 检查独立记录。

## 2026-10-01 分支摘要修复及重新验证

### 修改范围与默认回归

- 维护者明确要求修复上一轮唯一产品失败。`internal/sessions/branches.go` 在既有 `changeBranch` 固定后缀之后，复用手动压缩已有的 `chargedModel` 与独立 `agent.NewBudget`、新请求身份，再调用原 Eino `GenerateBranchSummary`。生产差异仅此装配；没有修改 SDK/API、前端、认证、重试、摘要验证、空闲条件或原子提交路径，没有添加执行循环。
- 新增默认 `internal/sessions/branch_summary_transport_test.go`，通过真实 OpenAIChat 工厂和本地 RoundTripper 复现，完全离线。未修复时定向命令 exit 1：Fork 报 invalid_argument、物理摘要请求 0；取消/摘要校验场景没有进入实际传输，预算拒绝也未返回预期预算错误。最小修复后，首轮仅预算测试的错误提取断言失败；Eino 包装错误使直接类型断言无法取得 code，改为标准 `errors.As`，未改变预期错误码或产品逻辑。
- 最终定向 `go test ./internal/sessions -run 'TestBranchSummary|TestIdleCompactUsesMeteredObservedTransport' -count=1` exit 0（0.997s）。回归实证 Fork 与 Navigate 各 1 次摘要请求、仅独有后缀、无工具、非流式；摘要与选定分支共同生效，既有 Trace/Turn/ModelAttempt 不变，同分支零额外请求，Close/Open 不重新请求。无效摘要和调用中取消各实际请求 1 次、预算拒绝 0 次，错误类别及完整历史未变均断言。

### 本轮离线和前端命令

- Windows/amd64、Go 1.27.0：`gofmt -l .` 无输出；`go vet ./...`、`go build ./...`、`go test ./... ./sdk/testdata/consumer -count=1` 均 exit 0（sessions 60.950s）。先运行 `go test -race ./internal/sessions -count=1` exit 0（281.476s），再运行 `go test -race ./... ./sdk/testdata/consumer -count=1` exit 0（sessions 249.612s）。
- 实际 WSL Ubuntu-24.04、Go 1.27.0 linux/amd64：用 `&&` 串联 `go vet ./...`、`go build ./...`、`go test ./... ./sdk/testdata/consumer -count=1`、`go test -race ./... ./sdk/testdata/consumer -count=1`，完整命令 exit 0。WSL 宿主提示与 Go 输出编码混合导致显示乱码，保留该限制；随后通过 Node 接收原始 stdout，实际补跑 `go test -race ./internal/sessions -run TestBranchSummary -count=10` exit 0（39.872s），版本与摘要包结果清晰可读。macOS 依维护者此前批准延期，未运行、不记通过。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` exit 0，可达漏洞 0，仍有包级、模块级各 1 项未触达提示。`npm run typecheck`、`npm run test -- --run`、`npm run build` 均 exit 0，前端单元 4 文件/16 测试，构建产物内容未改变。
- 既有 `.test_env` 已忽略且未跟踪，未读取任何值到输出或修改。`git diff HEAD --check` 无空白诊断；新增测试逐文件 `git diff --no-index --check -- NUL ...` 无空白诊断（no-index exit 1 表示新增文件与 NUL 存在差异，不误记为空白缺陷）。新测试的秘密模式与冲突标记扫描无命中。未提交、未推送。

### 真实浏览器和保留的中间失败

- `web/tests/e2e.spec.ts` 新增永久真实模型分支摘要回归：完整共享第一轮后才分叉，独有第二轮进入真实非流式摘要、共同历史标记不进入摘要请求；摘要激活和分支切换后主历史恢复、无额外模型请求、无外部浏览器请求或 pageerror 均断言。
- 首次完整 `npm run test:e2e` exit 1：17 passed、1 failed；新增分支摘要测试通过，失败为既有流中刷新场景的最终文本断言，两个 worker 合计 20 次请求/20 次响应、全 HTTP 200。维护者明确批准保留失败并完整复跑一次，未修改该刷新测试、产品源码、凭据或断言；末次 `npm run test:e2e` exit 0，18 passed、0 failed/skip，20 次请求/20 次响应、全 HTTP 200。原刷新失败原因未确证，不能仅据复跑成功宣布根因已修复。
- 从当前源码另构建真实 cmd/web，在全新临时 state 的可见 Chromium 中实际登录、创建会话、完成两轮真实对话、选第一轮助手消息、勾摘要并创建分支。临时诊断脚本最初将标准 Fetch 的 `response.status` 属性当成函数，失败两轮均模型请求 0；采集固定 TypeError/脚本行号后仅修正临时脚本，不修改产品。
- 最终可见定向场景通过：Fork HTTP 200、实际摘要请求恰 1 次/HTTP 200、revision 前进、分支选中且摘要可见，既有 traces 不变；切回 main 的完整 messages 与此前逐字相同，再切回摘要分支零额外请求。两轮对话与摘要合计 3 次真实模型请求，外部浏览器请求及 pageerror 均 0。已查看成功截图，摘要及两条完成任务、分支当前状态可见。
- 保留当前修复版服务 `http://127.0.0.1:53379/` 和成功摘要页面的可见 Chromium。脱敏结果为 `C:\Users\admin\AppData\Local\Temp\seasprak-cursor-p3-bajVh0\state\branch-summary-fixed-final-results.json`，截图为同目录 `click-screenshots/branch-summary-fixed.png`；旧失败和临时脚本诊断均分别保留，不加入 Git。此处认证的是独立 Chromium，Cursor 侧边栏私密自动输入限制未改变。

### 完整 live 仍未通过及后续授权

- 首次 `go test -tags live ./internal/llm -count=1` exit 1（44.902s），Gemini 非流式中途及流式首请求 HTTP 503。按维护者批准仅完整复跑一次，`go test -tags live ./internal/llm -count=1 -v` 仍 exit 1（62.110s）：OpenAI Chat/Responses、DeepSeek、Anthropic 各非流式/流式三步对话通过；Gemini 非流式三步 HTTP 200，但流式首请求 HTTP 503、resource_unavailable。复跑实际共 28 次请求，27 次 200、1 次 503，未弱化断言、未追加 SDK 重试。
- 分支摘要缺陷已通过默认与实际模型/页面验证，完整 live 门禁仍失败，完整 P3 的既有未交付/未认证项保持不变。维护者随后明确要求将 Gemini 503 作为独立问题继续诊断，并禁止盲重跑；诊断与该分支修复区分，不以定向诊断成功代替完整 live 通过。

### Gemini 503 独立诊断结果

- 核对现有 `internal/llm/gemini.go`：生产工厂显式设置 Google SDK `Attempts=1`，Eino 适配调用 SDK `GenerateContentStream`；未增加第二条流式协议实现。通过系统 TEMP 的 Go overlay 加入一次性诊断测试，复用现有配置读取及真实 Catalog，仅重复原验收首个无工具文本输入，不修改仓库 Gemini 源码、测试超时、模型或凭据。
- 第一次定向诊断使用原 40 秒等待上限，真实物理请求/观察计数各 1。固定布尔核对：configured_origin、official_host、POST、标准 `/v1beta/models/<配置模型>:streamGenerateContent` 路径、`alt=sse`、credential_matches 均 true，认证头 1，环境代理 false；40 秒内未获得 HTTP 响应，无法分类 503 原因。诊断测试 exit 0 只表示采集完成，实际模型调用没有通过，不能记为 live 成功。
- 随后零模型、零凭据公开 HEAD 探测实际完成 DNS/TCP/TLS，TLS 验证成功，约 1.190 秒收到官方根入口 HTTP 404。它只证明该时刻公开入口可达，不证明生成服务健康或账号授权。
- 维护者明确批准最后一次连接阶段诊断，仅临时程序等待上限增至 120 秒，并使用标准 `httptrace`；生产与验收仍保持原上限。实际 DNS 9ms、TCP 10ms、TLS 验证及连接完成 860ms、请求写出 861ms、首响应字节 10.230s，收到 HTTP 200/SSE，模型错误 none，真实物理请求/观察计数各 1。诊断命令 exit 0（10.406s）。当前单次流式调用可成功；没有因为设置 120 秒才等待超过 40 秒，不将其描述为超时修改修复了故障。
- 官方 [Gemini 排错文档](https://ai.google.dev/gemini-api/docs/troubleshooting) 将 503 UNAVAILABLE 列为可重试的服务错误，但本轮没有捕获到历史 503 的具体固定原因分类，不能推定为负载、地域、账号或网络的某一根因。完整验收中 503、后续无响应超时、最后单次 200 都保留，表现有时变；没有修改 SDK 重试、降低断言、换模型或盲重跑。
- **最终状态：**原分支摘要失败已修复并实测通过；Gemini 503 的具体根因仍未确认，最后单次成功不能替代五协议三步工具对话完整 live 验收。最后完整 live 命令仍 exit 1，之后未再次整套重跑。macOS 延期、Cursor 侧边栏私密自动输入限制以及完整 P3 未交付项保持不变；没有提交或推送。

### 2026-10-01 维护者再次授权确认 Gemini 失败原因

- 维护者要求“确定下gemini503的问题”。先核对完整 live 的 `acceptanceConversation` 与此前 TEMP 诊断：原验收先执行非流式文本/工具调用/工具结果三步，再执行流式三步；此前定向只覆盖流式首条无工具文本。没有将此前单次成功外推为完整工具对话通过。
- 在系统 TEMP 新建 `seasprak-gemini-503-investigation-20261001_test.go` 和对应 Go overlay，虚拟加入 `internal/llm/live_gemini_503_investigation_test.go`，仓库没有创建这个测试文件。复用原配置解析、Catalog、生产 Gemini 工厂和原验收函数，保持 40 秒上限、每次观察预算 1、原内容/工具参数/用量/真实调用次数断言，无自动重试或并发。请求只输出结构和内存哈希相等布尔值，不输出请求内容、哈希、配置模型/地址或凭据；错误体最多 16KiB 读透采集，Close 时输出固定分类并清除。
- 离线 `go test -overlay <TEMP overlay> -tags live ./internal/llm -run '^TestGemini503DiagnosticControls$' -count=1 -v` 首次 exit 0（0.137s）；加入 Google 配额细分后再次 exit 0（0.143s）。合成 503/429/403/400、转义消息及未知详情均验证固定分类，私密标记不进入输出结构；16KiB 边界/溢出、原字节保持和 Close 清除均验证。这两条命令真实模型请求 0，不记为 live 模型通过。
- 一次原顺序定向 `go test -overlay <TEMP overlay> -tags live ./internal/llm -run '^TestGemini503Investigation$' -count=1 -v` exit 1（19.688s）。实际 4 次物理请求，响应依次为 **429、200、200、429**：非流式首条文本失败；流式首条文本及工具调用各通过完整断言，工具结果续答收到 429。每个成功请求观察/物理计数各 1；首个流式与非流式请求体在内存规范化比较完全相同。所有请求标准路径、同配置 origin、认证一致、认证头 1、token 上限一致，环境代理 false；TLS 验证和官方 SNI 均 true，HTTP/2，无网络超时。两个 429 固定 JSON status 为 `RESOURCE_EXHAUSTED`，不是 503/UNAVAILABLE，也没有捕获权限、非法密钥、地域或参数错误分类。
- 为区分本轮实际 429 的分钟/每日/请求/token 配额，在仍不超过本轮总 6 请求边界内仅追加一笔原非流式首请求；没有再跑整组。`go test -overlay <TEMP overlay> -tags live ./internal/llm -run '^TestGeminiQuotaDiagnostic$' -count=1 -v` exit 1（1.231s），物理请求 1、HTTP 429。DNS 8ms、TCP 10ms、TLS 验证和请求写出 705ms、首字节 1.075s；Google `google.rpc.QuotaFailure` 固定分类为 **PerDay=true、RequestQuota=true、FreeTier=true、Violations=1**，PerMinute/TokenQuota=false。同时有 RetryInfo，提示延迟在一分钟内，但不能据此否定明确的每日配额违规或保证短等后恢复。未输出 quotaId/metric/dimensions、错误原文或配置值。
- 本轮合计 **5 次真实模型请求，2 次 HTTP 200、3 次 HTTP 429、0 次 503**，到此停止网络请求。可以确定当前上游拒绝原因是免费层每日请求配额超限；错误在官方 TLS HTTP 边界已存在，不是本项目摘要逻辑生成。实际流式文本/工具调用成功，也未支持固定认证、路由或工具请求格式错误的假设。没有账号控制台的实际用量/额度证据，不能计算还剩多少配额、推断全部消耗来源或保证所有请求都持续拒绝。
- 对照官方 [配额说明](https://ai.google.dev/gemini-api/docs/rate-limits)：限额按项目而非 API key 计算，每日请求配额在太平洋时间午夜重置；项目实际限额需在 AI Studio 查看。换同项目 key 不能增加项目配额；可等待重置或由维护者确认计费/额度，不擅自创建 key、切换模型或启用计费。对照 [排错说明](https://ai.google.dev/gemini-api/docs/troubleshooting)：503 UNAVAILABLE 是服务暂不可用类错误，429 RESOURCE_EXHAUSTED 是另一类错误。**当前 429 已确诊，历史 503 的具体根因仍未捕获，不能用每日配额结果倒推历史 503 是同一原因。**需配额恢复后才适合再次有界采集 503；本轮不盲重跑。
- 本轮只修改该验收文档，所有诊断程序与 overlay 留在 TEMP；Gemini 生产代码、默认验收、凭据、模型配置、40 秒上限、SDK Attempts=1、先前分支摘要修复均未改变。完整五协议 live 没有重跑，最后完整结果仍 exit 1；Windows/Linux 全仓离线、前端/browser 与 macOS 延期仍保持前一轮历史属性，不冒充本轮新验证。服务和可见窗口保留，无提交或推送。

### 2026-10-01 已确认当前 Gemini 每日额度为 20 次

- 维护者进一步要求“确定下每天具体多少次”，明确授权再次定向取证。公开文档的项目动态额度不能代替实际额度；旧响应已按安全策略清除，不从累计请求次数推算。只新增一次原非流式首请求，不重跑整套或循环重试。
- TEMP 新增每日整数解析测试，通过 overlay 与既有诊断共同加载。Google 官方 `google/rpc/error_details.proto` 定义 `QuotaFailure.Violation.quota_value` 为发生违规时实际执行的额度，不是已用请求数。解析只接受 429/RESOURCE_EXHAUSTED、当前配置模型、免费层每日项目/模型请求违规；优先读结构化 quotaValue，并与唯一匹配该 metric/模型的消息 limit 整数核对。分钟/token/其他模型、重复或冲突数字、溢出、无数字均返回 unknown，输出只有整数与固定布尔值。
- 先以空实现运行 `go test -overlay <TEMP daily-limit overlay> -tags live ./internal/llm -run '^TestGeminiDailyLimitParser$' -count=1 -v`，exit 1（0.150s），正常数字和结构化额度用例失败，证明测试能检出未解析。实现后运行 `go test -overlay <TEMP daily-limit overlay> -tags live ./internal/llm -run '^(TestGeminiDailyLimitParser|TestGemini503DiagnosticControls)$' -count=1 -v`，exit 0（0.138s）；22 个整数/范围/歧义子场景及既有脱敏读透控制全部通过，真实模型请求 0。
- 按新授权单次 `go test -overlay <TEMP daily-limit overlay> -tags live ./internal/llm -run '^TestGeminiQuotaDiagnostic$' -count=1 -v`，exit 1（1.440s），实际物理请求恰 1、HTTP 429，官方 TLS 验证与 SNI 均 true；错误仍为免费层每日请求配额违规。安全提取结果 **Known=true、RequestsPerDay=20、MatchedDailyQuota=true、FromStructured=true、FromMessage=true**，即 Google 结构化额度与消息中的上限数字共同确认 **当前配置模型在该项目免费层每日上限为 20 次请求**。未输出模型名、地址、凭据、quota dimensions 或错误原文；完成后停止请求。
- 20 次是当前项目/当前模型的实际免费层上限，不是所有 Gemini 模型的通用上限，也不是 20 次完整工具对话。原三步文本/工具调用/工具结果续答每步各发请求，非流式和流式合计一轮验收需 6 次。官方配额按项目而非 API key 计算，太平洋时间午夜重置；2026-10-01 对应北京时间 15:00。配额恢复不保证历史 503 一定消失；此次 429 及每日上限不能解释历史 503 的具体根因。
- 本轮只追加文档，TEMP 诊断未加入 Git，未修改生产代码、默认验收、凭据、模型、计费、等待上限或重试次数；未操作既有服务/可见窗口，未提交推送。完整 live 未重跑，最后完整验收仍失败；单次采集取得额度数字不记为模型请求或完整门禁通过。

### 2026-10-01 新模型在 15 次上限内复验通过

- 维护者自行更新 `.test_env` 中的 Gemini 模型配置，说明新模型额度为每日 15 次，要求再次尝试且不得超过 15 次。原模型的 20 次额度是旧配置的历史证据，不外推给新模型。本轮通过既有 `loadEnv` 在测试运行时读取维护者更新后的配置，不读取任何配置值到输出或回滚修改。
- 严格只运行一次 TEMP `TestGemini503Investigation`：复用原 `acceptanceConversation` 和生产 Catalog/Gemini 工厂，先非流式三步再流式三步；HTTP 边界最多发送 6 次，超过 6 在发送前拒绝，每逻辑调用观察预算 1、SDK Attempts=1、40 秒上限保持。没有自动重试、再次配额探测、其他模型调用或浏览器请求；实际总量 **6 次，小于维护者 15 次上限**，结束即停止，没有为了用完剩余授权额度继续请求。
- 先运行零模型请求的脱敏/解析控制：`go test -overlay <TEMP daily-limit overlay> -tags live ./internal/llm -run '^(TestGeminiDailyLimitParser|TestGemini503DiagnosticControls)$' -count=1` exit 0（0.142s）。随后真实组 `go test -overlay <TEMP daily-limit overlay> -tags live ./internal/llm -run '^TestGemini503Investigation$' -count=1 -v` exit 0（5.212s）：非流式与流式均完成文本、工具调用、工具结果续答三步，原回答/工具名与参数/历史回放/用量合法性/实际调用次数断言均通过。
- 真实物理请求 **6、HTTP 200 为 6、HTTP 429/503 为 0、无响应为 0**；每个调用 observed=1/physical=1，认证头 1、认证一致、标准路径及 origin 均匹配，环境代理 false，TLS 证书验证与官方 SNI 均 true。首个非流式/流式请求体规范化比较相等。输入用量均已知，输出用量均未知；原未知用量不能伪造及按声明能力校验的断言原样通过，不将未知输出用量描述为已测量或已认证该能力。
- 此结果证明维护者更新后的当前配置通过 Gemini 非流式/流式三步工具对话；新模型的每日 15 次来自维护者说明，本轮没有触发配额错误，所以没有再次核验其官方额度，也不能算出账号当日剩余次数。**切换模型后本轮成功不证明旧模型 503 的具体根因已修复**，历史失败及旧模型的每日 20 次违规记录全部保留。
- 五协议完整 `go test -tags live ./internal/llm` 通常需 30 次实际请求，本轮遵守用户次数限制不运行、不记通过；最后完整 live 结果仍保持此前 exit 1 的历史状态。本轮只追加文档，生产/原验收/本地配置/重试不修改，未操作既有服务或可见窗口、未提交推送；Windows/Linux 离线、前端/browser 及 macOS 延期没有本轮新验证，不将该 Gemini 定向通过冒充全套或 P3 完成。

### 2026-10-01 新配置完整五协议 live 实测通过

- 维护者继续要求使用剩余 9 次机会尽量完成，并在额度范围澄清中明确选择：**仅 Gemini 最多 9 次，其他已配置模型正常测试，运行完整五协议套件**。额度不足“默认算过”仅接受为维护者批准的豁免、未验证，不能伪造实测结果；本次全部原测试实际通过，无须使用此豁免。
- 从仓库根仅执行一次原完整命令 `go test -tags live ./internal/llm -count=1 -v`，**exit 0（32.175s），无失败或跳过**。没有使用 TEMP overlay、定向 -run、修改配置/断言、增加重试或延长原 40 秒上限；特别排除诊断 overlay 的额外真实请求入口，避免超过本轮 Gemini 限额。此命令包含该包默认离线测试及全部 live 测试，不代表本轮重跑全仓测试。
- OpenAI Chat、OpenAI Responses、DeepSeek Chat、Anthropic Messages、Gemini GenerateContent 五个协议工厂，非流式/流式共 10 个真实对话子测试全部通过；各模式均实际完成无工具文本、`lookup(q=ping)` 工具调用、工具结果续答三步。原精确回答、工具名/参数、结果回放、终止状态、用量合法性与实际调用次数断言保持原样通过，不能只凭 HTTP 200 判定。
- 逐笔核验原真实传输日志：**共 30 次物理请求、30 次 HTTP 200**；五协议各 6 次，其中 **Gemini 仅 6 次≤本轮授权 9 次**，其余协议合计 24 次。每笔观察/物理计数均为 1，没有 429、503、无响应或额外自动重试。本轮未使用的 3 次授权不继续消耗；与前一轮新配置定向组相加，本会话两轮新配置 Gemini 共 12 次，这只是本会话调用计数，不能据此断言项目实际余额或再次核验维护者所述每日 15 次上限。
- 四个其他协议的输入/输出用量均已知；Gemini 6 笔输入用量已知、输出用量未知。原能力声明及未知用量不得伪造的断言均通过，未知输出用量仍未认证。Responses、DeepSeek、Anthropic 继续使用此前授权的同源网关派生配置，认证的是实际经过的协议工厂路径，不冒充三个独立供应商账号/模型或所有上游能力均已认证。
- **当前新配置的完整五协议 live 门禁已实测通过**；此前完整失败、旧配置每日 20 次超限及历史 503 具体根因未定的证据全部保留。新配置成功不能证明旧模型 503 根因已修复，也不能认证所有模型或宣布完整 P3 已交付。
- 本次仅追加该验收文档，生产代码、原验收、本地配置、预算、超时与重试均未新增修改；`.test_env` 仍被 Git 忽略且未跟踪，未输出其值。文档更新后 `git diff HEAD --check` exit 0，无空白诊断；工作区仍仅既有四个文件。Windows/Linux 全仓离线及前端/browser 维持此前实测的历史属性，macOS 延期、Cursor 侧边栏未认证及 P3 既有未交付范围不变。完整命令结束后已停止追加模型请求，既有服务/可见窗口保留，无提交或推送。

### 2026-10-01 维护者授权提交与推送前复核

- 维护者于 12:59 明确要求执行当前差异的 commit-and-push。复核待提交范围仅为 `internal/sessions/branches.go`、新增 `internal/sessions/branch_summary_transport_test.go`、`web/tests/e2e.spec.ts` 和本文件，现有分支 `feat/p0-p1-runtime` 跟踪 `origin/feat/p0-p1-runtime`。不纳入本地配置、临时诊断、截图、状态或可执行产物，不改生产行为，也不创建 PR 或强推。
- Windows/amd64、Go 1.27.0 提交前重新运行：`gofmt -l .` exit 0、未格式化文件 0；`go vet ./...`、`go build ./...` 均 exit 0；`go test ./... ./sdk/testdata/consumer -count=1` exit 0（sessions 49.775s）；`go test -race ./... ./sdk/testdata/consumer -count=1` exit 0（sessions 233.171s）。本次没有新的并发修改，受影响 sessions 的先行 race 已在前述修复轮完成。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` exit 0，可达漏洞 0，包级和模块级各 1 项未触达提示继续保留。`npm --prefix web run typecheck`、`npm --prefix web run test -- --run`、`npm --prefix web run build` 均 exit 0，前端单元 4 文件/16 项通过，重新构建没有新增产物差异。
- 保留刚完成的原完整 `go test -tags live ./internal/llm -count=1 -v` exit 0（32.175s）作为当前源码的真实模型证据，随后产品代码与验收测试没有变动；本次提交动作不再运行 live 或真实 E2E，实际新增模型请求 0，不消耗剩余授权。此前真实浏览器与实际 WSL Linux 验证保持历史属性，macOS 继续按维护者已有批准延期，不记通过；旧模型 503 根因未定、Gemini 输出用量未知及完整 P3 未交付范围不变。
- 提交前只更新本验收记录并继续核对 diff、秘密/冲突/禁止产物、`.test_env` 忽略且未跟踪，以及暂存精确四文件范围。历史段落中的未提交/未推送描述是各当时轮次的事实，不表示维护者本次授权的提交动作被取消；实际 Git 提交和远端 SHA 结果以完成该动作后的报告为准。
