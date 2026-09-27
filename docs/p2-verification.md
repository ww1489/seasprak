# P2 实施记录与验证证据

## 2026-09-27 15:38 范围变更记录：用户 shell 与日志简化

维护者确认：只有用户直接 shell 脱离工具审批、Agent 预算、执行票据、工作区访问限制和持久化执行去重；模型/受控工具全部保留现有保护。用户 shell 仍有初始 cwd、输出、退出码、超时、主动取消及真实退出状态，不自动重试/恢复；来源必须由受信宿主入口建立。日志按 Zero 式短输出直返、超长尽力保存脱敏全文及头尾预览，保存失败不改原结果、不重跑，不新增日志审批/独立票据。

本次只修改需求、设计、验收文档及原 P2 计划。新语义尚未实现，旧 `commands_approval*`、直接恢复与全仓绿测均不能作为新方案认证；下方原审批恢复记录保留为历史事实。Steps 16–17 仍 in_progress，Step 22/23 仍 pending，不新增待办、不提交或推送。旧等待数据的兼容读取与禁止恢复执行纳入 Step 16.1；模型恢复保持原契约。

参考核对为本地 Zero `internal/tui/command_bash.go`、`model.go` 的用户 shell，以及 `internal/tools/spill.go`、`exec_command.go` 的工具日志路径。Zero 用户入口需要 unsafe 模式且有 30 秒超时；日志保存来自另一条工具路径。本方案组合其行为，不声称原样照抄同一链路，也未决定照搬启动开关、超时常量、共享临时目录或七天清理周期。本轮未运行 Go 测试、构建、安全扫描或 live；仅作文档一致性与差异检查，不能替代实施后的必需验证。


记录日期：2026-09-25—2026-09-27；最新复核：2026-09-27。环境：Windows 10.0.26200、Go 1.27.0 windows/amd64。
基线：`feat/p0-p1-runtime` / `0541e3d`。

## 当前状态

### 前置补齐验收结论（2026-09-27，本轮最新）

Steps 3–4、9–10、11 已按已确认范围完成逐项复核与补齐，既有三个待办更新为 completed，未重建待办或修改原计划正文。开始继续 Steps 16–17；P2 整体仍未完成。Workflow 全部 P5、原生流式工具排除和 macOS 延期保持不变。Windows 的两个符号链接子例已取得真实管理员十轮 PASS，不再是未验项。

最终补齐包括真实隐藏工具门禁修复、共享 testkit 文件/进程/独立产物实现、Write/Edit 篡改/复制/版本/跨请求票据矩阵、两接口 Session 取消与结算后票据失效、拒绝/预算身份断言，以及恢复新执行段保留原调用身份。独立审查指出的两项证据边界已由实际审批 Resume 原身份断言和八格 Session 票据生命周期矩阵补齐。产物结果链和完整文件工具语义仍归 Step 16，不能把独立夹具当作该步骤完成。

两次测试基础设施修正不改变生产安全契约：共享审批语义夹具经 mailbox 注入现有手动时钟，独立 `TestApprovalActivityReservationDeadline` 保留一秒预留准时续期/过期拒绝的真实审批路径对照；factory 测试改用既有 `waitResumeCondition` 等待同一个 frame.done（既有五秒挂死防护），不再使用活动测试专用的一秒 wallclock helper。需求未规定真实协议重试必须一秒完成，全部原请求数/工具数/attempt/usage/事件断言保留，生产活动截止、重试及取消语义未改。历史 /429、三次 tool_then_503 和审批预留过期的失败继续保留；具体运行延迟来源未定位，不宣称性能问题已修复。修正后的 factory 完整十场景 race20、CPU 1/2/8（60轮）退出 0，152.574s；取消/退避/活动边界同配置回归退出 0。审批/过期/续期十四项 race20 退出 0；首次重跑受并行删除诊断 import 的暂态编译错误阻断，稳定后原集合通过，不算行为红测。

主流程在稳定版本上最终独立执行：
- Windows：`go test ./... -count=1 -timeout=180s` 退出 0（sessions 13.071s）；`go test -race ./... -count=1 -timeout=180s` 退出 0（99.684s）。
- Windows：Step 11 原集合 `go test -race ./internal/agent/eino ./internal/agent/tools ./internal/sessions -run 'EnhancedTool|EnhancedBatch|ToolInterface|ToolOutput|Approval' -count=10 -timeout=600s` 退出 0（sessions 277.754s）。
- Windows：隐藏工具四格真实 Session 用例 race20 退出 0（70.228s），其余本轮十轮/二十轮矩阵证据见下文。
- Linux / WSL、Go 1.27.0：全仓普通和 race 均退出 0（sessions 12.664s / 85.812s），vet/build 退出 0；审批活动截止对照 race20 退出 0（8.717s）。
- Windows：最终 `gofmt -l .` 无输出，vet/build、独立 `git diff HEAD --check` 均退出 0，仅 Git 换行提示；本轮 go mod verify、固定版本 govulncheck、live 和外部 consumer race10 的实际结果见下文。

未提交或推送。平台认证中 macOS 仍是延期未运行；真实端点未扩大为五协议全部认证。Step 23 的全部故障窗/崩溃矩阵仍未完成。

### 2026-09-27 继续执行：Windows 符号链接实际验证

普通权限重新执行两个符号链接子用例，命令退出 0 但两项均 SKIP，未作为通过证据。维护者随后明确批准管理员授权运行测试，不修改开发者模式、系统策略或目录权限。管理员环境实际执行 `go test -race ./internal/sessions/store/jsonl -run TestBlob -count=10 -timeout=180s -v`，退出 0。首次运行仅取得进程退出码；再次保留详细输出，确认 `TestBlobTamperAndNonRegular/symlink` 与 `TestBlobRejectCheckpointPath/symlink` 各十次 PASS，无 SKIP/FAIL，包耗时 6.486s。由此补齐此前 Windows 两项权限跳过的运行证据；普通权限下仍可能跳过，不改变测试条件。辅助脚本及详细输出保存在仓库外，不进入提交。

同期独立运行 Step 3 原验收集合：`go test -race ./internal/agent ./internal/sessions/state ./internal/sessions -run 'P2Budget|Budget|ExecutionStopped' -count=10 -timeout=600s`，三包退出 0，sessions 34.691s。600s 仅为测试进程总时限，不修改用例内部或生产安全期限。其余前置步骤仍在逐项验收，不据此宣称整个 P2 完成。

当前工作区独立追加验证：
- Windows：`go test -race ./internal/agent/tools ./internal/sessions -run 'P2Prepare|P2Frozen|P2Origin|P2Policy|P2Ticket|P2Resources' -count=10 -timeout=600s`，退出 0，sessions 57.988s。
- Windows：`go test -race ./internal/agent/eino ./internal/agent/tools ./internal/sessions -run 'EnhancedTool|EnhancedBatch|ToolInterface|ToolOutput|Approval' -count=10 -timeout=600s`，退出 0，sessions 281.261s。
- Windows：`go test -race ./internal/sessions/store/... -count=10 -timeout=180s`，三个包退出 0；本次普通权限运行仍不替代上述管理员符号链接专项。
- Linux / WSL、Go 1.27.0：`go test -race ./internal/agent/eino ./internal/agent/tools ./internal/sessions -run ToolInterface -count=10 -timeout=600s`，退出 0；tools 包无命中用例，不作为该包十轮覆盖证据。终端中文代理提示和部分输出显示编码异常，不影响取得的进程退出码。

### 前置步骤逐项复核后的追加动作

Steps 3–4 既有待办已更新为 completed：逐项源码/默认测试复核确认原子预算、执行段隔离、活动预留、只读打开和不可变 blob 的真实接线；此前最新 Windows/Linux 全仓验证、此次预算十轮、两平台存储包十轮及 Windows 管理员符号链接十轮共同补齐当前范围证据。Linux 存储命令 `go test -race ./internal/sessions/store/... -count=10 -timeout=180s` 退出 0（jsonl 11.046s）。另执行 Windows `go test -race ./internal/agent ./internal/sessions/state -run 'Budget|Activity|Ticket|Reconciliation|Claim' -count=20 -timeout=180s`，两包退出 0。macOS 保持已批准延期，未记为 PASS；这不代表 Step 23 的全部故障窗或 P2 已完成。未修改原计划正文或重建待办。

Step 11 的 `internal/agent/eino/tool_variants_test.go` 已增强既有两接口真实 ToolsNode 测试：拒绝和审批不可用观察必须为 denied/none，预算耗尽必须为 failed/none；零 claim 时预算快照保持不变，观察保持原 product/provider 调用身份、参数、Session 和 generation。此次为正确行为补断言，不声称发现生产缺陷。Windows/Linux 均执行 `go test -race ./internal/agent/eino -run TestEnhancedToolRejectsBeforeBackendForSameReasons -count=10 -timeout=120s`，退出 0（1.247s / 1.169s）。

Step 10 尚需共享 testkit 受控文件/进程/产物实现和文件 Write/Edit 版本、复制、篡改矩阵。共享夹具计划放入 `internal/testkit/operations`：原 testkit 被 llm 同包测试引用，直接引入 agent 将产生循环依赖；独立辅助子包用于隔离该依赖，不新增产品层。夹具内容/补丁登记只属于测试私有协议，不冒充公开 ContentRef/PatchRef 产品契约。进程启动后产物保存复用已消费票据的问题归 Step 16 另行补齐，独立 artifact-store 测试不能替代它。

对后续 Steps 16–17 的只读核查已定位实际缺口：read 版本传递/行与字节语义/UTF-8 和续读元数据；edit 精确替换；目录分页和搜索限额；后端原始观察与完整产物的保存顺序、事实保留及引用读取；invocation TODO 持久实现；工具选择门禁和重开后的模型选择、pending 选择与 checkpoint 兼容、原子激活。原计划所列 `P2Builtin|P2Output|P2Command|P2Artifact|P2Selection|P2ModelSwitch` 当前没有命中测试，不能用该正则的退出 0 认证这些步骤。直接命令审批持久恢复已有实现与回归，不重复作为缺口。工具清单检查的可疑分支正在通过真实 Session 红测核实，尚未将静态审查当作漏洞复现。

### 本批真实缺陷修复与验证异常

`selection_hidden_call_test.go` 经真实 CreateAgentSession → Eino → Executor 复现已接纳模型调用绕过本轮清单：两同步接口、有/无下轮开放共四例，修复前隐藏工具实际执行 1 次、claim 为真、预算 1。`tools/execution.go` 最小修复为所有尚未执行的 model 调用检查选择，并用保留原 Turn、携带当前 ExecutionID 的 envelope 查询；direct 和已有观察优先复用不变。主流程已阅读新增默认测试和修改位置。`approval_test.go` 另增强两接口真实恢复断言：新 ExecutionID、原调用/Scope/Turn 不变、一次成功执行；Windows 十轮 race 退出 0（15.460s）。

共享夹具新增四文件：`internal/testkit/operations/{memory.go,process.go,operations_test.go}`、`internal/agent/tools/testkit_operations_test.go`。主流程已阅读全部文件并独立执行新夹具/接线十轮 race（退出 0），再执行 eino/tools/sessions/testkit 四包完整 race（退出 0，sessions 76.579s）。内存夹具的 full 能力仅描述自身无宿主访问的隔离，不认证原生沙箱；List/Search 明确 unsupported，Step 16 尚待补齐。真实 Session 结束后的文件票据失效正在追加验证，独立无状态夹具不作为该生命周期证据。

本批实现过程中一次 `go test -race ./...` 退出 1：`TestP2AttemptChatFactoryRetriesWithoutRepeatingTools/429` 在 `p2_attempt_factory_retry_test.go:170` 报 `execution did not exit`。随后主流程无缓存重跑 `go test -race ./... -count=1 -timeout=180s` 退出 0（sessions 88.059s），Linux 同命令退出 0（sessions 91.505s）；仍保留原失败，正在专项复现并收集根因，不能以重跑绿宣称已修复。尚不将 Steps 9–11 整体关闭。

真实 Session 文件票据生命周期已补测：`testkit_ticket_lifecycle_test.go` 覆盖两同步接口 × Write/Edit × settled 后重放/取消后消费共八格。代理保留真实、尚未消费的请求，取消时 worker 仍阻塞并未宣布停止；用去除调用上下文取消的上下文尝试消费，仍由 Session 返回 context.Canceled。结算后重放返回 state_conflict（已无活跃执行），不是伪造票据的 permission_denied。文件内容/版本不变，原 claim 和工具预算 1 保留；复制后重新读取快照及 Edit 跨真实请求换票据也已补齐。主流程阅读测试后独立 Windows `go test -race ./internal/sessions -run TestTestkitTicketSessionLifecycle -count=10 -timeout=180s` 退出 0（19.263s）；Linux 同前缀专项退出 0（sessions 19.560s），并另跑 `go test -race ./internal/testkit/operations -count=10 -timeout=120s` 退出 0（1.561s），未将无命中包计为覆盖。

追加失败保留：主流程 `go test ./... -count=1 -timeout=180s` 退出 1，`TestCancelPausedApprovalRejectsLateAnswerAndResume` 的共享审批夹具在等待审批前进入 failed；错误包含 `budget_exhausted: activity reservation expired`，已结算活动时间约 1.859s。其余包通过不改变全仓失败结果。同期重试专项 `-race -count=20 '-cpu=1,2,8'` 捕获三次 tool_then_503 的 1 秒退出等待失败，安全计数均为请求 3、工具 1，堆栈显示 mailbox 在可运行的 View 深拷贝路径；尚不足以归因为同一问题或死锁，继续自然收敛诊断，不延长生产期限或改掉失败记录。主流程 live（2.378s）与外部 consumer race 十轮（27.884s）退出 0，均不替代失败的全仓普通测试。

当前补齐代码上主流程 `gofmt -l .` 无输出、`go vet ./...`、`go build ./...`、`go mod verify` 均退出 0。固定版本 `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` 退出 0：0 可达漏洞，另有 1 未调用包级及 1 模块级提示。实现侧曾使用 @latest 工具检查，不能充当固定版本验收依据；主流程以明确固定版本的实际结果为准。

2026-09-27 14:15 后维护者明确选择：workflow_node 的实际接线与验收整体移至 P5，P2 保持拒绝。当前不存在受信工作流绑定登记或节点接纳入口，冻结来源仍只接受 model/direct；此次没有放宽生产校验。此前“workflow_node 是 Step 9 待补阻塞项”的记录按当时范围保留，现在不再作为 P2 完成阻塞。P5 需一并解决绑定/generation、节点身份持久接纳、独立 invocation 归属、审批等待及恢复，不以预留字段或现有基线测试替代验收。

2026-09-27 13:32 维护者确认：macOS 实际运行验证延期，单独保留待验证清单，不再作为当前步骤或 P2 实施完成的阻塞条件。仍须在后续真实 macOS 环境运行 build、普通/race 测试及平台文件语义专项；当前状态是未运行，不是通过。Windows/Linux 的可用环境验证和全部功能、安全、恢复验收要求保持不变。

2026-09-27 已确认的当前 P2 范围：仅 Invokable、EnhancedInvokable 两类同步接口；保留 SDK 实际输出回调，不要求动态百分比或阶段进度，不以输出回调替代原生 Streamable 验收。原生 Streamable、EnhancedStreamable 明确不在本次 P2 范围，保持装配期 `resource_unavailable` 拒绝；不修改 Eino、不维护 fork。其缺失及历史 reader 生命周期限制不再阻塞 Step 12 或 P2 出口；Step 11 仍须按调整后的同步接口与安全要求独立验收，不因范围调整自动标为 complete。模型流式响应、取消/后端收敛/unknown、审批/checkpoint 和其余平台、协议、故障窗验收要求不变。

以下旧记录中的“四接口验收”“Step 12 依赖完整 Step 11 而阻塞”“原计划未修改/尚待批准”等按当时范围保留为历史证据，当前范围以本声明及末尾范围调整记录为准；历史红测和框架限制未被抹去，也未被改记为通过。

2026-09-27 最新：同步路径已经接入持久 Pause、严格显式 Resume、一次审批应答和原调用定向恢复，实现及红绿记录见末尾 Steps 12–14 执行补充。Pause 先受理与业务中断保存 blob 期间晚受理均保留审批目标，暂停回执与 checkpoint/bindings/stopped 同一提交；最新冻结验证单独记录，修复前结果不作当前证明。先前宽范围 race20 曾在三工具逐个审批的第二次恢复中失败，专项 race40 未再复现；后续确认混合取消错误会隐藏独立失败原因，已用可控时钟行为红测修复这一诊断缺陷。诊断修复后的全仓及保留原范围、追加活动/收尾用例的 race20 通过，详见末尾新验证；历史执行失败原因仍未确认，不将其归因于 Pause 或诊断修复。两种同步工具接口共用唯一执行器，SDK 实际输出片段不是原生 reader 流式完成证明；两种原生 Streamable 保持拒绝，原四接口验收是已收窄范围的历史要求，不再作为当前 P2 阻塞。Step 15 可信核对/release、剩余协议、Linux/macOS 和完整 P2 验收仍待实施或认证；第 21 节保留先前 CI 问题及历史边界。

Steps 1–2 已完成依赖/框架边界核实与持久记录专项实现，并通过第 8 节记录的全仓普通/race 验证。按用户后续要求，依赖已升级到第 7 节版本；OpenAI SDK 保留明确兼容例外。流式 Interrupt 探针已补齐，同时保留 reader 内中断重跑 sibling 的原生限制。Steps 3–4 已实现原子调用预算、执行段隔离、活动时间持久预留，以及只读/blob 存储基础，并通过第 11 节的独立 Windows 验证。恢复段来源映射已在 Step 13 同步路径接线，符号链接权限用例和其他平台认证尚待补齐。Steps 5–7 的开发与 Windows 确定性验证已完成：Step 5 的目录/凭据/选项已完成本地验收；Step 6 的有界用量采集与唯一物理请求预算已接线并通过第 13 节的独立 Windows 验收；Step 7 产品 Chat 工厂已实现并通过第 15 节的独立 Windows 全仓验证，真实 endpoint 产品工厂认证仍待 Step 23。Step 8 已补齐 attempt 登记/终态/证据、原子接纳、实时快照和真实工厂受限重试，并通过第 17 节独立 Windows 全仓验证；真实端点及其他平台认证仍待最终验收。Steps 9–10 的 Windows 修复及未完成的平台认证见第 20 节；Step 11 的部分交付与历史流式阻塞见第 21 节，现按顶部已确认范围验收。Steps 12–23 的原计划全范围交付尚未完成；末尾附记记录 Step 11 内容流、Step 12 同步路径暂停与 Step 13 严格显式恢复的局部交付，不代表原计划 Step 23 验收。本记录不是 P2 完成证明。

此前阶段性变更（含 Step 11 同步内容流、Steps 12–13 同步暂停/恢复）已经明确获准提交并推送为 `56c592ae51681676422e0b2b9c5005bac0b95f68`。本轮 Step 14 审批接线及其后的修复、测试和记录尚未提交或推送，没有新的提交授权。原生 Immediate 限制不是由本次修改引入；中间尝试直接接 Immediate 曾破坏 P1 的真实退出等待，已由既有回归发现并修正。以下按实施时间保留历史命令与失败过程，旧版本结果不替代当前验收。

## 2026-09-27 Step 9 Schema 精度及准备回调独立复核

本次复核范围仅为 `internal/agent/tools/schema.go`、`prepare.go` 与新增默认测试 `schema_precision_test.go`、`prepare_contract_test.go`。主流程已读取全部四个文件并核对生产差异：Schema 使用现有 DecodeNumbers 保留数值约束精度，json.Valid 拒绝尾随内容；prepare、Validate、BeforeCall 在回调实际返回后优先保留 context 错误，不以后台 goroutine 提前结束不合作回调。

实现阶段报告的红测：大整数 minimum/maximum 三个边界用例退出 1；回调超时四个子用例退出 1。主流程未再次回退生产代码复现红测，独立检查了测试确实经过 Executor.Run，并断言 claim、工具预算、配对观察与实际运行次数；业务 Validate 的最终参数、副本隔离以及三类回调 error/panic 也由默认测试覆盖。

主流程在本次代码上独立执行，以下命令均退出 0：

- Windows：`go test -race ./internal/agent/tools -count=1 -timeout=120s`，1.557s。
- Windows：`go test -race ./internal/agent/tools -run P2Prepare -count=10 -timeout=180s`，2.904s。
- Linux（Ubuntu 24.04 / WSL，`GOTOOLCHAIN=go1.27.0`）：同工具包完整 race 命令，1.472s。
- Linux（同环境）：同 P2Prepare 专项 race 十轮命令，2.835s。

Linux 命令伴随既有 localhost 代理映射警告，但运行成功。以上为局部补齐证据；本次尚未重跑最新全仓强制检查及 live，macOS 仍未实跑。后端能力门禁、workflow_node 来源绑定及其余集成缺口保持待验，Steps 9–10 原待办仍为 in_progress，不据此宣称 Step 9 或 P2 完成。

## 2026-09-27 Step 11 两同步接口生命周期补测独立复核

本次仅修改测试：`tool_output_boundary_test.go` 将不合作后端取消参数化为 Invokable/EnhancedInvokable；新增 `tool_interface_lifecycle_test.go`，经真实 CreateAgentSession/SubmitInput 覆盖冻结描述、启动 intent、最终 observation 三处保存失败（两接口共六个子用例），以及 BeforeCall 阻塞期间取消（两接口共两个子用例）。主流程已读取新文件和原文件差异，核实断言包括真实后端次数、预算、claim、取消前后实际观察，以及保存失败时 live manager 和日志回放状态的一致性。存储使用受控内存实现；日志回放不等于磁盘重开认证。

主流程独立验证均退出 0：

- Windows：`go test -race ./internal/sessions -run 'TestToolInterfaceSession(SaveFailures|CancelBeforeClaim)|TestCancelledUncooperativeToolDoesNotStopBeforeReturnOrPublishLateText' -count=10 -timeout=180s`，12.972s。
- Linux（Ubuntu 24.04 / WSL，Go 1.27.0）：`go test -race ./internal/sessions -run TestToolInterfaceSession -count=10 -timeout=180s`，9.468s。
- Linux（同环境）：`go test -race ./internal/sessions -run TestCancelledUncooperativeToolDoesNotStopBeforeReturnOrPublishLateText -count=10 -timeout=180s`，4.806s。

BeforeCall 屏障只证明该钩子期间取消，不证明已进入资源等待队列，也不证明已完成授权但尚未执行原子 claim。这两个真实 Session 精确边界仍缺测试；既有 Executor 层授权重检测试不能替代。此次没有生产改动或新发现的生产缺陷，未重跑全仓强制检查、live 或 macOS，Step 11 仍为 in_progress。

## 2026-09-27 审批期间外部文件版本变化

新增默认测试 `internal/sessions/approval_file_version_test.go`，两同步接口均经真实 CreateAgentSession → SubmitInput → 审批暂停 → RespondInteraction → Resume → FileOperations.Write。受控文件后端消费真实执行票据，实际比较预期版本；等待期间将文件由 v1 更新为 v2，恢复仍携带原 v1，调用一次但写入零次，外部内容不变，持久观察为 failed/none，预算累计一次。该测试证明产品传递版本前置条件并接受后端冲突结果，不把 P2 受控后端当作原生文件系统认证。

Windows 普通专项退出 0；Windows/Linux `go test -race ./internal/sessions -run TestApprovalExternalFileVersionChangeDoesNotOverwrite -count=10 -timeout=180s` 均退出 0，分别 17.493s/16.360s。此前三次编译尝试因并行工作中的函数签名、runtime 导入命名及新增测试尚未完成而退出 1；待对应文件修正后重跑通过，这些编译失败不是本用例的行为红测。

## 2026-09-27 新增持久恢复与精确取消测试的中间复核

新增 `p2_resources_disk_reopen_test.go` 的实现报告包含真实 JSONL 关闭重开、新调度器恢复 unknown 限制、第二会话等待及可信核对原子释放；报告的 Windows 专项普通及 race 十轮通过，主流程尚待独立复验，不据此标记 Step 10 完成。

新增 `p2_transport_disk_resume_test.go` 的初版用例在三个物理请求后让未接纳模型调用失败，再要求公共 Pause 成功。实际返回 `state_conflict: pause did not produce a safe checkpoint`，尚未到达 Resume；这不是预算恢复重置的证据。正在核对安全暂停契约及测试前置条件，既不伪造 checkpoint，也不自动放宽恢复条件。初版红测及未覆盖的公共 Resume 组合场景保留为证据。

`tool_interface_cancel_boundary_test.go` 初版资源等待测试由主流程独立 Windows race 十轮复现失败；进一步打印观察确认实际为 `skipped/none, Executed=false`，而测试要求 cancelled。既有 `docs/paused-cancellation-verification.md` 明确未执行工具使用 skipped/none，因此需按边界精确断言，不将该差异自动定为生产缺陷。授权后用例正在改为先排入 Cancel 再回送授权结果，使取消与 claim 的测试调度保持 FIFO。修改期间一次联合复验因该文件重复声明变量编译失败，退出 1，不计通过；稳定版本仍待复验。

## 2026-09-27 精确取消边界修正后的独立运行证据

主流程复核 `tool_interface_cancel_boundary_test.go` 的稳定版本：资源等待取消精确断言 skipped/none，BeforeCall 取消与授权后 claim 被拒绝精确断言 cancelled/none。授权后测试先执行实际授权闭包，暂缓回复直到真实 Cancel 已入队，再让 Executor 提交 claim；按 Cancel→claim 的 FIFO 顺序执行原生产闭包。测试控制调度时机，但不再重排队列或替换生产授权/取消实现。

共享编译错误消除后，主流程独立执行均退出 0：

- Windows：`go test -race ./internal/sessions -run 'TestToolInterfaceSessionCancel(WhileResourceWaiting|AfterAuthorizationBeforeAtomicClaim|BeforeClaim)$' -count=10 -timeout=180s`，9.625s。
- Linux（Ubuntu 24.04 / WSL，Go 1.27.0）：`go test -race ./internal/sessions -run TestToolInterfaceSessionCancel -count=10 -timeout=180s`，8.870s。

两接口覆盖以上三个边界，断言实际工具次数零、模型次数一、内存与持久工具预算零、无 claim、正确配对观察和真实停止。此次仅补测试，未修改生产取消语义；此前编译失败和错误状态断言记录保留。两个精确边界测试缺口已取得局部证据，Step 11 整体验收仍须等待相邻能力门禁集成及全仓检查，macOS 按维护者要求延期。

## 2026-09-27 磁盘预算与未知效果恢复独立复核

主流程已读取两个新增测试的完整实现，并独立验证最新版本。原“模型失败后 Pause 必须成功”前提不符合恢复设计：失败不保证产生安全 checkpoint，终态不复活。已保留为 `TestP2TransportDiskFailedAttemptRejectsPauseAndResume`，准确断言 Pause=state_conflict、无 checkpoint、三次请求预算不返、真实 Close/Open 状态不变且零执行，以及 Resume=incompatible_resume。初版失败不是预算恢复重置缺陷的证据。

`TestP2TransportDiskRestoredLedgerRejectsFourthRequest` 从真实磁盘关闭重开后的 usage/limits 重建原 ModelCallID 的预算账本，再调用实际 ObservedTransport；第四次请求返回 budget_exhausted，发送零次、新增持久占额零次、账本及磁盘记录不变。这是磁盘回放＋预算＋传输边界集成，不是公共 Resume 成功恢复同一未接纳逻辑调用的证明；后者没有找到合法 checkpoint 前提，仍明确不作认证，也不将原计划扩展成“任意模型失败都必须可恢复”。

`TestP2ResourcesDiskReopenUnknownBlocksUntilDurableReconcile` 经两次真实 JSONL 关闭重开及全新调度器验证 unknown 限制恢复、人工证据不得释放、可信核对与释放同事务落盘后第二会话才启动，并保留原未知观察、终态和预算。原后端/第二后端/可信查询均一次，模型均零次。测试以实际 Acquire 的阻塞堆栈定位资源等待，不使用 sleep 猜测；该观测依赖当前 Go 堆栈格式，不属于公开产品接口。

主流程独立运行均退出 0：

- Windows：`go test -race ./internal/sessions -run '^TestP2(ResourcesDiskReopenUnknownBlocksUntilDurableReconcile|TransportDiskFailedAttemptRejectsPauseAndResume|TransportDiskRestoredLedgerRejectsFourthRequest)$' -count=10 -timeout=180s`，10.837s。
- Linux（Ubuntu 24.04 / WSL，Go 1.27.0）：`go test -race ./internal/sessions -run TestP2TransportDisk -count=10 -timeout=180s`，8.296s。
- Linux（同环境）：`go test -race ./internal/sessions -run TestP2ResourcesDiskReopenUnknownBlocksUntilDurableReconcile -count=10 -timeout=180s`，7.043s。

本次没有生产修改；上述是局部证据，全仓及相邻功能验收尚未收口，原待办不自动改为完成。macOS 继续按批准延期。

## 2026-09-27 后端能力门禁独立复核及全仓检查

主流程复核新增 BackendCapabilityReporter 与 CapabilityHash/ValidateFrozenCapabilities，以及 Executor、会话 policy 和 SDK 别名接线。报告由实际 Files/Process 实例提供；缺失、无运行数据写保护、不支持模式或 none/unavailable 均拒绝。规范化报告摘要和模式纳入冻结 hash；配置、授权、资源等待之后 claim 之前、claim 后启动之前及消费票据时复核。trusted-run 保持受信进程内兼容例外，不宣称有沙箱隔离。

审批恢复时有效但变化的能力报告在 worker 重冻结阶段触发 state_conflict；能力无效触发 resource_unavailable。Resume 的受理本身不代表后端已经获准启动，测试断言零新 claim、零执行。此处没有扩展 Resume 资格预检查，也没有实现 P6 原生沙箱。

主流程在当前代码独立执行以下检查，均退出 0：

- Windows 受影响完整竞态：`go test -race ./internal/agent/tools ./internal/sessions -count=1 -timeout=180s`，sessions 76.695s。
- Windows `gofmt -l .` 无输出；`go vet ./...`、`go build ./...`、`go mod verify` 通过，依赖全部校验通过。
- Windows `go test ./... -count=1 -timeout=180s` 与 `go test -race ./... -count=1 -timeout=180s`，sessions 分别 11.708s/78.296s。
- Linux（Ubuntu 24.04 / WSL，Go 1.27.0）`go test -race ./internal/agent/tools ./internal/sessions -run TestBackendCapabilities -count=10 -timeout=180s`，sessions 19.393s。
- Linux `go test -race ./... -count=1 -timeout=180s`，sessions 75.618s；`go vet ./...`、`go build ./...`、`go test ./... -count=1 -timeout=180s` 均通过。
- Windows `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`：可达漏洞 0，另报一个未调用包级漏洞和一个模块级漏洞，不将其改写成所有依赖无漏洞。
- Windows `go test -tags live ./internal/llm -count=1 -timeout=120s`，3.046s；`.test_env` 由 `git check-ignore .test_env` 确认被忽略，未读取或输出认证内容。该结果不等于五协议全部真实端点认证。
- Windows `go test ./sdk/testdata/consumer -count=1 -timeout=120s`，0.758s。
- `git diff HEAD --check` 通过，仅 LF/CRLF 提示；已列出未跟踪文件，新增清单无禁止提交产物，internal Go 文件冲突标记/私钥/明显 token 模式检查无命中。

macOS 按维护者批准延期，Windows 原有 symlink 权限受限用例仍不计运行通过。以上是当前代码回归及后端门禁证据，不替代逐项需求验收；workflow_node 固定绑定来源仍在补齐，P2 全部步骤尚未完成。本次未提交或推送。

## 1. 依赖固定与兼容检查

`go list -m -json github.com/cloudwego/eino@v0.9.15` 退出码 0，Origin.Hash 为 `ebd616c8291e957684ea6ca99dd54225d04e0438`，与设计一致，未升级 Eino。

以 eino-ext 设计提交 `6752ff8da9b1ea85c8e91b27b0a64fef099f22f9` 解析四个子模块，逐项命令退出码 0：

- `github.com/cloudwego/eino-ext/components/model/agenticopenai@v0.2.3-0.20260820123736-6752ff8da9b1`。
- `github.com/cloudwego/eino-ext/components/model/agenticclaude@v0.1.4-0.20260820123736-6752ff8da9b1`。
- `github.com/cloudwego/eino-ext/components/model/agenticgemini@v0.2.3-0.20260820123736-6752ff8da9b1`。
- `github.com/cloudwego/eino-ext/components/model/agenticdeepseek@v0.1.1-0.20260820123736-6752ff8da9b1`。

四模块通过显式版本 `go get` 固定到 go.mod/go.sum，随后 `go mod tidy`，两项退出码均为 0。依赖图由 Go 最小版本选择更新，未使用 latest 或本地 replace。新增云平台间接依赖来自现成 adapter，并不表示本产品已支持或认证 Bedrock/Vertex/Azure。

`internal/llm/p2_dependency_probe_test.go`：对五协议分别构造实际 adapter，使用注入的 fake RoundTripper，真实调用 Generate 与 Stream；返回合成 HTTP 400，断言每条路径实际传输一次、错误未被当作成功；Responses 断言出站 `store:false` 且无 `previous_response_id`。没有访问网络或读取凭据。

该测试证明构造/接口/注入传输兼容，**不证明产品模型工厂、正常响应接纳、usage 或真实 endpoint 已交付**。

### 后续必须使用的真实 API 和限制

- Chat：`agenticopenai.NewChatModel`、`ChatConfig.HTTPClient`，结束信息在 `ChatResponseMetaExtension.FinishReason`。
- Responses：`NewResponsesModel`、`ResponsesConfig.HTTPClient`；`MaxRetries` 需显式指向 0，`Store` 需显式指向 false，`EnableAutoCache=false`。默认 Store=nil 不等于禁止远端存储；还须禁止调用选项/ExtraFields 重新启用 previous-response 链。结束信息使用 OpenAIExtension.Status/IncompleteDetails/Error。
- Claude：`agenticclaude.New`、原生 API 的 `Config.HTTPClient`；结束信息使用 ClaudeExtension.StopReason。固定版本没有公开关闭 SDK 重试的配置入口，底层 anthropic-sdk-go v1.56.0 默认重试两次。Step 6 必须在实际 transport 前计数并验证连接错误/429/5xx，不能沿用一次模型调用等于一次物理请求的假设。
- Gemini：`genai.NewClient(ClientConfig{HTTPClient: ...})` 后传给 `agenticgemini.Config.Client`；Models/Caches 使用同一 API client，`CreatePrefixCache` 能经过注入 HTTP 客户端，但不走普通模型 callback。缓存辅助请求必须在 transport 计数。结束信息使用 GeminiExtension.FinishReason。
- DeepSeek：`agenticdeepseek.New`、`Config.HTTPClient`，结束信息在 ResponseMetaExtension.FinishReason。
- usage 中整数零值不能证明原始字段存在。上述探针未认证缓存命中、推理明细或私有签名回放。

## 2. 已通过的框架探针

### 恢复点与停止方式

`internal/agent/eino/p2_checkpoint_probe_test.go` 复用已有真实 NewAgent/InputRef 测试基础：

- 父 context 取消不保证保存 checkpoint；本探针路径不写 checkpoint。
- 模型安全点的 Graceful Stop 保存带 runner state 的 checkpoint，工具执行数为 0。
- Immediate Stop 的外层 checkpoint 不等于已保证存在可恢复 runner；产品不能仅凭 blob 存在授权 Resume。
- 构造合法的无 runner checkpoint 后，框架进入 GenInput 而不是 GenResume；探针拒绝该退化路径，模型/工具调用数均为 0。此 guard 目前在测试 harness，产品 Resume 尚未实现。
- 读取过 checkpoint 的自然完成路径会调用可选 Delete；未来产品适配必须仅清理别名，不删除仍被引用的不可变 blob。

### 四类工具接口

`internal/agent/eino/p2_tool_interface_probe_test.go` 使用真实 AgenticToolsNode：

- Invokable、Streamable、EnhancedInvokable、EnhancedStreamable 各跑 Invoke/Stream 两种路径。
- Enhanced 参数确为 ToolArgument.Text；大整数 JSON 与中文原样保留。
- 每条路径执行一次，原 CallID/工具名和文本结果保留。
- 流式 reader 提前 Close 能解除本探针中阻塞的 producer；EOF 先于调用方 Close 可被正常消费。

这些不是产品四类安全 wrapper 认证；授权、预算、后端停止与最终观察结算仍待 Step 11。

### 工具批次业务中断与定向恢复

`internal/agent/eino/p2_interrupt_probe_test.go` 使用真实 NewAgent/TurnLoop，原生工具特意不走产品结果缓存：

- A 已完成，B StatefulInterrupt；正常业务中断和 Graceful 在 ask 前/后的两种次序均可保存并定向恢复。
- 无回答恢复时 B 再次中断；明确回答当前服务端保存 target 才产生 B 的效果。
- A 实际执行始终为 1，B 最终效果为 1，GenInput=1、GenResume=2；后续模型接收原 A/B CallID 的结果。
- 再次中断产生新 target UUID，地址/原调用不变；产品需持久绑定最新 checkpoint 的 target，不能沿用第一次 UUID。
- 父 context 取消可以使阻塞的 B 退出，B 业务效果为 0。

## 3. 初始阻塞：Immediate Stop 不能代替同步工具 context 取消

失败用例：`TestP2FrameworkBatchAbortSafety/immediate_true`。

复现：A 已完成；B 阻塞在 gate/ctx.Done 的 select；调用 `Stop(adk.WithImmediate())`，通过 `TurnContext.Stopped` 确认信号已应用；8 秒内 B 未观察到 context 取消。

失败信息：`blocked B did not receive context cancellation after applied stop`。

普通执行、race 十轮及主执行者独立运行均复现。测试 Cleanup 取消父 context、释放所有 gate，并有界等待 Loop 退出；没有观测到清理超时。Immediate 失败分支之后的 checkpoint 内容/恢复断言未执行到，不作推断。

源码核对：Eino v0.9.15 的 `adk/cancel.go` 中 `sendImmediateInterrupt` 发出图中断与内部信号；`withAbortOnlyCancelContext` 是单独的 context 取消桥接路径。不能从 Immediate 名称推导所有同步工具的 ctx.Done 均会关闭。

处理原则：不升级 Eino、不削弱占用/未知效果规则，不声称 Stop 请求等于实际执行已退出。维护者确认在产品执行适配中分离安全点 Pause 与终止 Cancel，并要求先核对最新版。后续核对、适配和最终结果见第 6 节；第 4 节保留修复前结果，不代表当前测试状态。

## 4. 修复前的验证命令与结果

以下均在本仓库根目录运行，除注明外由主执行者核实。退出码 0 表示命令成功，不扩大为尚未实施能力的证明。

- 基线 `go test ./... -count=1 -timeout=120s`：退出码 0。
- `go test -race ./internal/agent/eino -run 'TestP2Framework(StopModes|CheckpointWithoutRunner|CleanExit)' -count=10 -timeout=90s`：退出码 0。
- `go test -race ./internal/agent/eino -run 'TestP2FrameworkTool' -count=10 -timeout=90s`：退出码 0。
- `go test ./internal/llm -run TestP2FrameworkProtocolClients -count=1 -timeout=60s`：退出码 0。
- `go test -race ./internal/llm -run TestP2FrameworkProtocolClients -count=10 -timeout=90s`：退出码 0。
- 批次探针实施者运行 `go test -race ./internal/agent/eino -run '^TestP2FrameworkBatch' -count=10 -v`：退出码 1，十轮均在 Immediate 场景失败，其余批次场景通过。
- 主执行者运行 `go test -race ./internal/agent/eino ./internal/llm -run 'Checkpoint|P2Framework' -count=1 -timeout=90s`：退出码 1，仅上述 Immediate 安全断言失败。
- `gofmt -l .`：退出码 0，无输出。
- `go vet ./...`：退出码 0。
- `go build ./...`：退出码 0。
- `go mod verify`：退出码 0，all modules verified。
- `go test ./... -count=1 -timeout=120s`：退出码 1，仅上述 Immediate 安全断言失败，其余包通过。
- `go test -race ./... -count=1 -timeout=120s`：退出码 1，同一失败；未出现 race 报告，不能称全量 race 通过。
- `go test ./internal/architecture ./sdk ./sdk/testdata/consumer -count=1 -timeout=90s`：退出码 0。
- `govulncheck ./...`：命令不可用，PowerShell CommandNotFoundException。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`：退出码 0，0 个可达漏洞；另有 1 个未调用的模块级漏洞。该默认扫描不等于对测试专用 adapter 路径的完整漏洞认证。
- `go test -tags live ./internal/llm -count=1 -timeout=120s`：退出码 0。
- 为确认不是 skip，另跑 `go test -tags live ./internal/llm -run '^TestLocalCompatibleModel$' -count=1 -v -timeout=120s`：退出码 0，明确 PASS。仍是现有 P1 裸 HTTP live 路径，不是 P2 Factory 或五协议真实认证。
- `git diff HEAD --check`：退出码 0；只有 Git LF/CRLF 转换提示。
- 新增测试检查冲突标记、常见密钥格式、私钥标记：无命中；显式 `synthetic-test-key` 为固定合成测试值，未读取真实凭据。

探针编写初期出现过测试配置缺少 PrepareAgent、空 Input 及中断地址解读错误，已修正测试装配；这些不作为产品行为红测证据。当前保留的失败仅为上述安全能力差异。

## 5. 待验与下一步

- Step 1：同步工具终止适配已通过本地验证；流式工具在业务 Interrupt/取消竞争中的完整 checkpoint 组合未认证。
- Step 2–23：未实施，不以类型/接口上游存在宣称已交付。
- Linux/macOS 运行与 CI 本轮未执行；平台专项不能由 Windows 或交叉编译代替。
- 五协议正常响应、真实凭据认证、产品 usage/transport 预算，以及 P4/P5/P6 集成均待后续步骤。
- 当前全仓验证通过，仍不能标记整个 P2 完成；原生限制以特征测试和以下记录保留。

## 6. 最新稳定版核对与产品适配后的结果

### 最新版仍有同一行为

`go list -m -json github.com/cloudwego/eino@latest` 返回 **v0.9.21**，发布时间 `2026-09-23T08:56:40Z`。`go mod download -json github.com/cloudwego/eino@v0.9.21` 确认提交 `ba04fde8641057055c358d7ab5d3015a9ba825e1`。两命令退出码 0。

使用仓库外临时 modfile，保留当前依赖版本，仅把 Eino 改成 v0.9.21；没有升级项目 go.mod。修复前执行：

`go test -mod=mod -modfile=C:/Users/admin/AppData/Local/Temp/seasprak-p2-eino-v0921.mod -race ./internal/agent/eino -run '^TestP2FrameworkBatchAbortSafety/immediate_true$' -count=3 -timeout=90s`

退出码 1，三轮均为同一错误：`blocked B did not receive context cancellation after applied stop`。这不是仅按更新日志推测。源码差异检查显示 cancel.go/turn_loop.go 有其他变更，但没有消除此复现。

随后新增 `TestP2FrameworkNativeImmediateOutlivesTool` 保留原生行为的确定性特征测试：原生 Stop/Wait 返回 CancelError 时 B 尚未收到 context 取消，随后显式取消父 context 才使 B 退出。测试不恢复该未知执行、不伪造成功结果，Cleanup 仍等待工具取消信号。该特征测试与产品 abort 回归分别验证原生限制和本产品补偿，不把原生问题改写成已修复。

### 产品适配与真实退出屏障

新增 `internal/agent/eino/stop.go` 的 `AbortTurnLoop`：

1. 调用执行段的 cancel 函数，通知模型/工具进行协作式终止。
2. 调用 `loop.Stop(adk.WithSkipCheckpoint())` 关闭新输入派发；**不使用 WithImmediate**，防止图中断在不合作调用仍运行时提前返回。
3. 函数仅发出请求，不宣布已停止；会话继续通过实际执行退出、现有协调器和效果事实确认结束。
4. `sessions.executeSegment` 使用 `context.AfterFunc` 将 Cancel、Close、活动期限导致的 context 取消接到该适配。退出时注销回调，若已启动则等待回调结束，避免遗留取消回调操作已结束段。
5. 终止路径明确不创建新的恢复点；Graceful 暂停和原生定向恢复探针保持不变。未来 ProcessOperations.Stop 仍需实际后端停止证据，当前未实现。

先把仅有框架 Stop 的行为提取到适配函数，原安全断言仍失败（退出码 1）；补 context 取消后该断言通过。第一次接线同时使用 Immediate 时，已有 P1 测试 `TestRuntimeCloseTimeoutRetainsWriterLock`、`TestRuntimeCancelWaitsForToolBeforeStoppedProof`、`TestRuntimeCloseWaitsAndRejectsWrites` 发现了提前释放写锁/返回取消的回归。最终移除 Immediate 图中断，保留协作取消和实际退出等待；这些测试恢复通过。没有删除或放宽 P1 断言。

### 最终验证（适配接线之后）

- `go test -race ./internal/agent/eino ./internal/sessions -run 'P2Framework|Checkpoint|Cancel|Close|UnknownEffect|ExecutionStopped' -count=10 -timeout=120s`：退出码 0。
- `go test -race ./internal/agent/eino -run 'TestP2FrameworkNativeImmediate|TestP2FrameworkBatchAbortSafety' -count=20 -timeout=90s`：退出码 0。
- 同上两类探针使用 v0.9.21 临时 modfile、`-mod=readonly -race -count=20`：退出码 0，证明最新版仍呈现已记录的原生限制且产品补偿可用，不是最新版修复证明。
- `go test ./... -count=1 -timeout=120s`：退出码 0，全仓通过。
- `go test -race ./... -count=1 -timeout=120s`：退出码 0，全仓通过。
- `go test -race ./sdk/testdata/consumer -count=10 -timeout=90s`：退出码 0。
- `go vet ./...`、`go build ./...`、`go mod verify`：各退出码 0。
- `gofmt -l .`：退出码 0，无输出。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`：退出码 0，0 可达漏洞，另有 1 个未调用的模块级漏洞；PATH 中 govulncheck 仍不可用。
- `go test -tags live ./internal/llm -count=1 -v -timeout=120s`：退出码 0，`TestLocalCompatibleModel` 明确 PASS，无 skip；仍不代替五协议产品工厂认证。
- 项目 `go list -m github.com/cloudwego/eino` 仍为 v0.9.15；临时 modfile 和 sum 验证后删除。

Linux/macOS 运行证据仍待补。本次未提交或推送。

## 7. 按用户要求升级依赖（2026-09-25）

本节替代前述旧依赖基线的“当前版本”描述；前述命令作为历史证据保留，不代替新版验证。原 P2 计划未修改。

执行 `go get -u -t ./...`、`go mod tidy`，均退出码 0。当前 Eino v0.9.21；agenticopenai v0.2.4、agenticclaude v0.1.7、agenticgemini v0.2.5、agenticdeepseek v0.1.1；genai v1.71.0、anthropic-sdk-go v1.75.0。其余直接/间接依赖固定在 go.mod/go.sum，无本地 replace。yaml/v4 当前为 v4.0.0-rc.6，是上游预发布版本，不能称所有依赖均为稳定版。

### 明确兼容例外

OpenAI SDK 最新 v3.66.0 与最新 agenticopenai v0.2.4 编译不兼容：CallID 从 string 改成 param.Opt[string]、MCP error 从 string 改成 union，适配器 responses_convertor.go 出现四处类型错误。按用户确认的“最新框架/adapter＋验证兼容 SDK”策略，固定 SDK v3.44.0（适配器自己的 go.mod 声明版本）。`go get github.com/openai/openai-go/v3@v3.44.0` 和 tidy 退出码 0。

再次执行 `go list -m -u`，与 `go list -deps -test ./...` 的实际导入模块集合交叉核对：唯一仍有更新的实际导入模块是上述 OpenAI SDK。完整模块图仍列出未被本项目导入的上游依赖更新；没有为了清空列表而强行引入这些模块。该结论已额外用 GOOS=linux/darwin 的 `go list -deps -test` 核对：三平台实际导入闭包中均只有该 OpenAI SDK 例外；这是依赖解析核对，不是跨平台构建或运行测试，也不是未来版本永久最新保证。

### 新版流式行为核实

- `AgenticToolsNode.Stream` 经 `InternalMergeNamedStreamReaders` 将转换 reader 接到异步 `toStream` 泵。外层 Close 只立即关闭泵输出；泵可能仍在源 recv 中，必须完成该次 recv 才能在发送失败后 defer 关闭源。因此 Close 返回不等于生产者已退出，也不能要求 Close 后第一次 Send 必定被拒绝。
- Close 探针按此源码路径最多允许一次已挂起的预取，并断言下一次 Send 被拒绝且生产者完成；不是无限重试或放宽生产停止证明。`go test -race ./internal/agent/eino -run 'TestP2FrameworkToolStreamClosePropagatesToProducer$' -count=20 -timeout=120s` 退出码 0。
- 最新 agenticclaude v0.1.7 仍未公开 MaxRetries；anthropic-sdk-go v1.75.0 默认重试两次。新增 `TestP2FrameworkClaudeHiddenRetries` 对 HTTP 500 和无响应传输错误实际调用 Generate，均断言 RoundTrip=3；`go test -race ./internal/llm -run TestP2FrameworkClaudeHiddenRetries -count=3 -timeout=60s` 退出码 0。产品预算必须在实际传输层占额，不能只数模型回调，也不能声称响应头能禁用无响应错误重试。
- 原生 reader.Recv 返回 StatefulInterrupt 后，无回答恢复会再次调用已完成 A（A=2）。探针保留此框架限制，不把原生重跑判为产品可接受行为；权限中断必须在打开流之前返回，产品层后续仍需冻结调用、claim 与观察去重。新版未自动解决所有恢复安全问题。

### 升级后重新执行的验证

以下均在项目当前 go.mod 上执行，各退出码 0：

- `go test -race ./internal/llm ./internal/agent/eino ./internal/sessions -count=1 -timeout=120s`。
- `gofmt -l .`：无输出。
- `go vet ./...`、`go build ./...`、`go mod verify`。
- `go test ./... -count=1 -timeout=120s`。
- `go test -race ./... -count=1 -timeout=120s`。
- `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`：No vulnerabilities found；使用 go run 因此前 PATH 无 govulncheck。
- 额外执行 `go run golang.org/x/vuln/cmd/govulncheck@latest -test -show verbose ./internal/llm ./internal/agent/eino`：退出码 0，0 可达漏洞；发现未调用的包级 GO-2026-6443（grpc v1.84.0 的服务器请求处理，修复仅列 v1.85.0 开发版）和模块级 GO-2026-5932（未导入的 x/crypto/openpgp，无修复版）。因此不能笼统宣称整个依赖图无漏洞；当前保留最新稳定 grpc，后续实际协议接线需重新扫描可达性。
- `go test -tags live ./internal/llm -count=1 -v -timeout=120s`：TestLocalCompatibleModel 明确 PASS，无 skip；仍是 P1 live 路径，不是五协议产品工厂认证。
- `git diff HEAD --check`：仅 LF/CRLF 提示，无格式错误。

以上记录的是依赖升级与 Step 1 探针验收时点；Step 2 后续实现仍需新一轮验证。开始 Step 2 后的一次全仓检查退出码 1：当时新增 `TestP2RecordsJournalReplay` 的 operation/observation_revision/reconciliation 分支尚未接入回放，属于正在实现中的红测；gofmt 也列出了三个在途 state 文件。因此不能将前述通过结果称为当前整个工作区已验收。Linux/macOS 运行未执行，不能声称三平台已通过。

## 8. Step 2 持久记录验收与后续实施

Step 2 已实现有限 P2 记录的保存/回放、operation 幂等回执和合法状态转换、观察版本追加和旧无版本观察兼容。沿用唯一 Manager.commit，未新增数据库或通用记录注册器。所有新增类型覆盖保存—重开；另有 JSONL 关闭后重新打开的完整往返测试。

主执行者补充 `TestP2ReconciliationOperationTargetMustMatchCall`，实际红测为期望 state_conflict、返回 nil：reconcile operation 指向别的 Call 仍可提交当前观察。现已在回放校验中绑定 operation.Target 与 CallID，失败候选不产生提交；该回归已通过。

共享契约边界：SaveRecords 使用 session LastSeq CAS，记录不可变；operation 使用独立 revision，重试返回原 accepted 回执，GetOperation 提供当前状态；观察版本按 Call 连续追加，原 ToolRecord.Observation 不覆盖。ModelAttempt/Interaction/Approval 当前为初始事实，Step 8/14 仍需有限的生命周期追加记录，不能用换业务 ID 或覆盖初始记录代替状态转换。核对记录本身不释放 claim、不完成 operation、不自动 Resume。

主执行者独立验证，各退出码 0：

- `go test -race ./internal/sessions/state ./internal/architecture -count=10 -timeout=120s`。
- `go test -race ./internal/sessions -run 'Cancel|Close|UnknownEffect|ExecutionStopped|Recovery' -count=20 -timeout=180s`。
- `go test -race ./internal/agent/eino -run 'Checkpoint|P2Framework' -count=20 -timeout=120s`。
- `gofmt -l .` 无输出；`go vet ./...`、`go build ./...`、`go mod verify`。
- `go test ./... -count=1 -timeout=120s` 和 `go test -race ./... -count=1 -timeout=120s`。

Step 3 原子调用预算/执行段隔离已部分实现，活动时间持久预留仍未交付，不标整个步骤完成，详见第 10 节。

Step 4 的只读子项：先用现有 OpenAgentSession API 复现与活动 writer 争锁，退出码 1（session already has a writer）；新增 JSONL ReadOnly 模式并接入 Open，使用 O_RDONLY、不建立写锁、不 chmod、不创建或修复文件。OpenSessionDir 的现存目录检查也改为不修改权限。读取固定为打开时文件长度内的有效前缀，不纳入后来追加的内容。原写入路径保持单写锁。

只读用例覆盖 writer 存在、Append 拒绝、文件及目录权限/内容/清单不变、缺目录零创建、尾部残缺不修复、重新打开获取新快照。`go test -race ./internal/sessions -run '^TestP2ReadOnly|ReadOnly' -count=10 -timeout=120s`、`go test -race ./internal/sessions/store/jsonl -count=10 -timeout=120s` 均退出 0。不可变 blob 子项随后完成，详见第 9 节；三平台验收尚未完成。

## 9. Step 4 不可变 blob 与只读存储

新增独立可选 `store.CheckpointBlobs`：Put(ctx, sessionID, data) 返回 BlobRef{Hash, Size}，Get(ctx, sessionID, ref) 返回校验后的深拷贝；Store 四方法不变。JSONL 使用 checkpoints/<sha256>.bin：同目录临时写入、File.Sync、os.Root.Link 不覆盖定名、移除临时文件、SyncDir 后才返回引用。重复内容验证而非覆盖；内存后端保持同义校验但不具备跨进程持久性。无 Delete/孤儿自动恢复，框架关联与自定义 Store 的不支持判定仍待 Step 12 接线。

主执行者审查实际实现及故障测试，并独立执行 `go test -race ./internal/sessions/store/... -run 'Blob|P2ReadOnly' -count=20 -timeout=180s`，退出码 0。只读部分另外用 channel 固定“commit 正文已写、换行尚未写”窗口：只读打开不等待写锁，只返回完整有效前缀；写入完成后旧 reader 快照仍不变。`go test -race ./internal/sessions/store/jsonl -run P2ReadOnly -count=20 -timeout=120s` 退出码 0。

平台缺口：两个 blob symlink 用例因 Windows 缺创建权限而 SKIP；主执行者将 skip 限定为 Windows 权限错误，其余 fixture 错误必须失败，并用 verbose 单次确认上述两项确为 SKIP。普通文件损坏、长度错误、目录替代、并发完整读取、Sync/短写失败与只读拒写用例实际通过。尚未实测不支持硬链接的文件系统，代码对此直接失败、无覆盖式回退。

运行环境核对：wsl --list --quiet 退出码 1，提示尚未安装 WSL；docker、gh、govulncheck 不在 PATH。没有安装 OS 组件或申请远端运行。Linux/macOS 仍待真实运行，Windows 的 skipped symlink 用例也不能记为通过。后续全仓验收须在 Step 3 在途修改收敛后重新执行。

## 10. Step 3 部分交付与阻塞项

已审查原子预算实际调用链：BudgetLedger.ClaimTool 在 tool_intent 中携带候选 Usage；Manager.ClaimTool 同 commit 保存 claim、意图和额度，成功后才更新执行缓存并准许后端调用。模型 BeginTurnID 持久原逻辑调用身份及已用请求数，Restore 不再清零；SaveTraceBudget 拒绝持久计数回退。计量当前仍在模型接口调用边界，SDK 隐藏 HTTP 请求的唯一占额接线归 Step 6，不能提前宣称完成。

活动事实、工具查询、轮次边界、steering 和预算回调现有 ExecutionID 门禁；工具持久原调用 scope 与当前执行信封分开。mailbox 收尾改读 View 的 usage/limits，不反向获取 ledger 锁。审查既有测试变更：intent/预算从两次提交改成原子提交，相应占额时点断言更新；授权阶段取消现在不 claim，已提交 claim 后取消仍保留占用，实际 Run=0 与停止证明断言保留。

主执行者独立执行 `go test -race ./internal/agent/... ./internal/sessions/state ./internal/sessions -run 'P2Budget|Budget|ExecutionStopped' -count=10 -timeout=180s`，退出码 0。另已补跑 `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`（退出码 0，无可达漏洞）及现有 `go test -tags live ./internal/llm -count=1 -v -timeout=120s`（退出码 0，TestLocalCompatibleModel 明确 PASS）。这些命令不替代后续活动预留改动的新验证。

尚未完成：活动时间目前仍为 context.WithTimeout，缺少持久实耗、一秒预留、崩溃不确定占用、假时钟及续期故障验收；旧 P1 journal 缺新字段时的保守恢复策略尚待认证；旧执行段晚到观察的版本化核对也不能由“拒绝旧段提交”冒充已交付。已继续实施上述剩余项，后续交付与验证见第 11 节；此处保留当时尚未交付的历史状态。

## 11. 活动预留与迟到观察的独立集成核验

已审查 sessions/activity.go、state/activity.go、协调器/执行入口门禁、回放和迟到观察路径。活动记录区分 Settled（单调时钟实耗）、Reserved（当前未结清预留）、Uncertain（崩溃后的保守占用）。每片最多一秒，提交成功才放行新工作；提交耗时计入该片、不延后截止时间。旧到期计时器在续期 Append 时仍有效，在 mailbox 外触发取消；提交失败或迟到成功不能复活已取消执行。实际退出后才停止计时并等待续期收敛后结清，不能以取消信号代替停止证明。

旧已启动 P1 日志没有活动计量时标记 unknown，拒绝取得完整新预算；未启动历史 Trace 可在首次启动初始化。加载仅形成诊断视图，未结清预留转为不确定占用，不自动启动、不计算停机墙钟。原执行身份可证明且已 claim 的迟到结果追加观察版本，重复结果不重复提交；原 ToolRecord、unknown 门禁、预算及当前执行状态不变。

主执行者在交付后独立执行，以下各命令退出码 0：

- `go test -race ./internal/agent/... ./internal/sessions/state ./internal/sessions -run 'P2Budget|Activity|Budget|ExecutionStopped' -count=10 -timeout=180s`。
- `go test -race ./internal/sessions ./internal/sessions/state -run 'Activity|LateObservation|ExecutionStopped' -count=20 -timeout=180s`。
- `go test ./internal/sessions -run '^TestSessionCrash$' -count=10 -timeout=180s`。
- `gofmt -l .`：无输出；`go vet ./...`、`go build ./...`、`go mod verify`。
- `go test ./... -count=1 -timeout=180s`、`go test -race ./... -count=1 -timeout=180s`。
- `go test ./sdk/testdata/consumer -count=1 -timeout=120s`。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`：No vulnerabilities found；仍不替代第 7 节测试依赖漏洞说明。
- `go test -tags live ./internal/llm -count=1 -v -timeout=120s`：TestLocalCompatibleModel 明确 PASS，无 skip；仍不是 P2 五协议产品工厂认证。
- `git diff HEAD --check`；另列出所有新增文件，未见 exe/test/bin 或凭据文件；内部 Go 文件常见密钥/私钥和冲突标记扫描无命中。

剩余边界：恢复后的新执行段与原调用之间的持久来源映射归 Step 13；不能证明来源的迟到信封仍拒绝。当前证据追加不是完整 Reconcile，不释放 claim、不改变有效结果投影、不 Resume。Step 6 尚需实际 HTTP 传输计量，Step 12 尚需 blob 关联及自定义 Store 能力拒绝接线。Steps 3–4 的平台认证仍不完整：两个 Windows symlink 用例曾跳过，Linux/macOS 无运行证据，保留待验，不标整个 P2 完成。

## 12. 模型基础实施与额外平台证据

Step 5 的模型目录、能力声明、凭据请求作用域和有效选项已通过主执行者本地独立验收：`go test -race ./internal/llm -run 'P2Catalog|P2Options|P2Credential|P2FrameworkChatFinish' -count=10 -timeout=180s` 退出码 0；随后 gofmt 无输出、vet/build/modverify、全仓普通/race 均退出码 0。目录以四字段复合 key 保存深拷贝，Bind 不取凭据或联网，每次 Generate/Stream 重新解析并固定当前请求账号/provider/endpoint。已有配置字段类型保持，未装配五协议实际工厂。注入模型要求能力/计量声明但不自称verified。

主执行者补充并实际验证调用边界：Eino 调用选项不能扩大已解析的输出额度、切换到未登记模型、降低必需答案预算或传入不透明供应商选项；失败不得调用底层模型。

新增框架测试 `TestP2FrameworkChatFinishMetadata`，使用最新 agenticopenai 的真实 Generate/Stream 和离线 transport，验证 stop、length 和缺终止标志在 ChatResponseMetaExtension 中保持原义，不能将单纯 EOF 当成功。`go test -race ./internal/llm -run '^TestP2FrameworkChatFinishMetadata$' -count=10 -timeout=120s` 退出码 0。这只是协议适配器依据，不是 Step 7 产品入口认证。

补充 Windows 专属 `TestBlobRejectWindowsCheckpointJunction`：通过实际目录联接尝试将 checkpoints 指向工作区外目录，Put 拒绝、返回零引用、外部目录保持空。`go test -race ./internal/sessions/store/jsonl -run '^TestBlobRejectWindowsCheckpointJunction$' -count=10 -v -timeout=120s` 退出码 0，十轮明确 PASS，无 skip。该证据不替代仍缺权限的文件 symlink 用例，也不替代 Linux/macOS 运行认证。

### Step 6 当前子项（尚未整步交付）

主执行者先写可编译行为红测，实际观察：SDK隐藏重试产生3次真实请求、0次占额观察；缺观察上下文仍发送；usage累计/known-zero未保存；catalog将budget_exhausted错误变成resource_unavailable。最小实现后专项 `go test -race ./internal/llm -run 'P2Transport|P2Usage|P2Redaction' -count=10 -timeout=120s` 退出码0。

现有实现：NewObservedTransport 在每次真正 RoundTrip 前调用请求上下文的 RequestObserver，观察端口负责提交预算而非新增第二预算。真实Claude SDK的3次尝试在限额2时只进入底层transport两次，第三次被拒绝；占额期间取消也不会发送。并发attempt计数隔离，稳定ModelCallID/AttemptID/Purpose随上下文传递。UsageRecord逐字段Value/Known/Source，累计覆盖而非累加，缺失字段不伪装0；缓存与推理子项不重复加入总量。catalog错误包装仅保留允许的错误码并重建公开错误，不透传Message/Refs/Details/Retryable。

以上子项之后主执行者重跑全仓普通/race（count1 timeout180s）、govulncheck@v1.8.0、现有live（count1 timeout120s），均退出码0。Step6仍缺有界read-through JSON/SSE采集器、实际执行层观察端口及去重占额接线，正在继续；上述通过不代表其后在途修改已验收。Step7产品Chat工厂和Step8完整attempt生命周期仍未交付。

## 13. Step 6 接线后的独立核验

主执行者审查 usage_body.go、transport.go、observed_model.go、BudgetLedger.BeforeRequest 和 ValidatedModel.requestContext 的真实接线。有界JSON/SSE采集在原Body读取路径上执行，不预读，不修改Read字节/错误和Close错误；超限/未知格式将统计降为unknown。Close不冒充EOF。usage保留缓存写5分钟/1小时明细和价格版本契约，未设置虚构价格。

明确使用observed transport的模型通过请求上下文调用原BudgetLedger占额，ValidatedModel不再额外占一次；旧受信注入模型保留调用级占额兼容，不因此认证真实HTTP计量。model.transport_reserved事件与预算同commit，含逻辑调用/attempt/物理序号/purpose；该事件仅证明占额，不证明已经发出请求。隐藏重试及缓存辅助请求共享原逻辑调用预算，提交失败不发送。观察声明是受信装配契约，不是沙箱隔离或自动认证。

本次交付后主执行者独立运行，各退出码0：

- `go test -race ./internal/llm ./internal/agent ./internal/agent/eino ./internal/sessions/state ./internal/sessions -run 'P2Transport|P2Usage|P2Redaction' -count=10 -timeout=180s`。
- `gofmt -l .` 无输出；`go vet ./...`、`go build ./...`、`go mod verify`。
- `go test ./... -count=1 -timeout=180s`、`go test -race ./... -count=1 -timeout=180s`。
- `go test ./sdk/testdata/consumer -count=1 -timeout=120s`。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`：No vulnerabilities found。使用之前采用的固定工具版本补齐PATH工具缺失，不安装全局工具。
- `go test -tags live ./internal/llm -count=1 -v -timeout=120s`：TestLocalCompatibleModel明确PASS，无skip；不代表尚未交付的产品协议工厂认证。
- `git diff HEAD --check`、新增文件清单检查，以及内部Go文件常见密钥/私钥/冲突标记扫描均无新增问题。

Linux/macOS运行仍未执行，Windows文件symlink权限缺口仍保留。Step7协议工厂和Step8完整attempt记录/终态仍待实施，当前不能宣称P2全部完成。

## 14. Step7 行为红测：Chat 拒答元数据缺失（2026-09-25）

新增 `internal/llm/p2_openai_chat_test.go`，通过 catalog 请求期凭据、真实 agenticopenai Chat adapter、observed transport 和离线响应复现：Generate/Stream 都丢失 `refusal`，但保留 `finish_reason=stop`。两条路径的真实 HTTP 请求与预算观察次数均为1。该测试目前使用测试内登记的适配器工厂，不是已经交付的产品 Chat 工厂。

主执行者独立运行 `go test ./internal/llm -run '^TestP2OpenAIChat' -count=1`，退出码1，两条路径均因拒答信息丢失失败。当前默认测试集包含此红测，不能沿用第13节的全仓通过结论描述当前工作区。尚未修改生产实现，Step7保持未完成，Step8未开始。

拟调整方法：在已有有界 read-through 响应采集机制内补采集请求隔离的 Chat 拒答元数据；继续由原 adapter 处理请求和内容流，普通结束原因仍来自真实 ResponseMeta。须确认后实施，不手写第二套 Chat SDK；采集不完整时应保守拒绝接纳，不能照搬 usage 缺失仅降统计已知性的处理。当前未重跑全仓验证、live 或其他平台运行。

## 15. Step7 产品 Chat 工厂与独立验收（2026-09-25）

用户确认补充有界拒答采集后，新增 `openai_chat.go`、`chat_collection.go`，扩展既有 transport/usage framing。`Catalog.RegisterOpenAIChat(client, maxResponseBytes)` 显式注册工厂，绑定后每次请求解析凭据，复用 agenticopenai；普通结束原因来自真实 Extension，拒答优先，未知或不完整采集保守失败。关闭流可取消阻塞读取，建流失败关闭响应体。有效输出上限、thinking effort、已认证 retention 参数均通过产品入口测试；无原始用量证据不公开 SDK 合成零值。Chat 底层使用 go-openai v0.1.6，不是 Responses 的 OpenAI SDK v3；401/429/503 离线测试仅一次发送，不增加隐藏重试或重定向。

主执行者阅读工厂/采集实现并独立执行，以下每项退出码均为0：

- `go test -race ./internal/llm ./internal/agent/eino -count=1 -timeout=180s`。
- `gofmt -l .`，无输出；`go vet ./...`；`go build ./...`；`go mod verify`。
- `go test ./... -count=1 -timeout=180s`。
- `go test -race ./... -count=1 -timeout=180s`。
- `go test ./sdk/testdata/consumer -count=1 -timeout=120s`。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`，No vulnerabilities found（默认扫描范围，不替代此前记录的测试依赖图风险）。
- `go test -tags live ./internal/llm -count=1 -timeout=120s`；现有 live 仍是旧裸 HTTP 路径，不构成新工厂真实 endpoint 认证。
- `git diff HEAD --check`，仅 LF/CRLF 提示；新增文件清单退出码0，无禁止提交产物；internal Go 文件冲突标记、常见秘密格式扫描无命中。

第14节行为红测现已转绿。Steps5–7开发与Windows确定性验证完成；Linux/macOS运行、新工厂真实endpoint认证仍待补齐。精确thinking token budget前置拒绝，未实现的缓存资源/有状态续接不宣称支持。Step8完整attempt生命周期、实时观察与同事务接纳尚未交付。原计划文件未改动，没有commit/push。

## 16. Step8 部分实现独立核对（2026-09-25）

请求前登记 attempt/message/stream 身份；新增追加式 ModelAttemptTransition，保留初始记录，accepted 与助手消息、工具调用通过同一次 Manager.commit 保存。transport-observed 模型实际开启流式路径，ValidatedModel 唯一消费并发布不持久化的脱敏 snapshot；工具入口核验 accepted attempt。新增取消/存储错误禁止重试保护。无 attempt 的历史兼容路径保留。

主执行者审阅 `state/attempts.go` 并独立运行 `go test -race ./internal/agent/eino ./internal/sessions ./internal/sessions/state -run 'P2Attempt|StreamBoundary|Validated' -count=10 -timeout=180s`，退出码0。该结果只证明当前专项通过，不代表Step8验收完成；本轮主执行者尚未重新执行全仓检查。

剩余：真实工厂安全瞬态分类与 Retry-After、可控退避和剩余时间约束、overflow 无新投影诊断、配置版本和usage/失败诊断关联、临时消息开始事件等。现有工具后重试用例依赖受控 transient 模型，不作为真实Chat 429/503重试证据。继续按已批准Steps6/8的安全错误归一化与受限重试要求补齐；不转发供应商原始正文/重试标志，不降低取消/存储失败和最大物理次数门禁。待办保持in_progress。

## 17. Step8 补齐与独立 Windows 验收（2026-09-25）

新增受信安全失败分类，仅保留状态/白名单枚举与有界Retry-After；取消和存储等强否决优先。Eino原生ShouldRetry返回Backoff并负责等待，产品只计算100ms指数、0–50% jitter、10s上限及活动余额/deadline约束；超过上限的服务端最短等待拒绝重试，不截短后提前发送。未创建第二模型执行循环。配置版本、usage和安全失败诊断随attempt终态保存；started/snapshot为非持久临时事件，终态后拒绝更新；重开未终态诊断派生自日志，不伪造成功或退出。

主执行者审阅failure.go、retry.go、state/attempts.go，独立运行以下命令，全部退出码0：

- `go test -race ./internal/agent/eino ./internal/sessions ./internal/sessions/state ./internal/llm -run 'P2Attempt|StreamBoundary|Validated|P2Safe|P2OpenAIChatHTTPRetry|P2OpenAIChatOverflow|P2OpenAIChatControlledRead' -count=10 -timeout=180s`。
- `gofmt -l .`无输出；`go vet ./...`；`go build ./...`；`go mod verify`。
- `go test ./... -count=1 -timeout=180s`；`go test -race ./... -count=1 -timeout=180s`。
- `go test ./sdk/testdata/consumer -count=1 -timeout=120s`。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`，默认扫描No vulnerabilities found。
- `go test -tags live ./internal/llm -count=1 -timeout=120s`（旧入口，非新工厂真实端点认证）。
- `git diff HEAD --check`，仅换行提示；internal Go文件冲突标记/常见密钥模式扫描无命中。

真实Chat工厂加离线HTTP测试覆盖429/503重试、持续失败最多3请求、工具后重试仍只执行工具1次、401/overflow不重试、partial不拼入下一次响应。Steps5–8开发及Windows确定性验证通过。Linux/macOS、各真实端点认证继续待验；overflow白名单尚非逐端点认证。精确等待用原生非流式runner虚拟时间测试，真实session流由独立集成测试覆盖；Eino流副本GC finalizer跨synctest环境限制未冒充修复。P4压缩和SDK重连快照仍分别留P4/Step22。下一步Step9，未提交或推送。

## 18. 恢复工作区一致性与上游复用核对（2026-09-26）

本轮恢复时，`internal/agent/operations.go` 等Step10实现仍引用agent.FrozenExecution，但对应 `agent/frozen.go`、tools准备文件、sessions工具执行接线及部分测试已缺失，现有Executor仍是旧准备路径。未认定缺失原因，也未直接覆盖其他改动；经用户确认后恢复原批准设计。首次运行 `go test ./internal/agent/... ./internal/sessions/... -run '^TestP2Prepare|^TestP2Policy|^TestP2Resources' -count=1 -timeout=120s` 退出码1，首个编译错误为undefined FrozenExecution；这不是行为红测，也不是功能验收通过。

模型层当前已包含拒答原因有界保留、当前请求凭据脱敏、原始结束原因与归一化结果分离，以及按endpoint/model/configVersion认证的overflow门禁。本轮独立读源码并运行：

- `go test -race ./internal/llm -count=1 -timeout=180s`，退出码0。
- `go test -race ./internal/llm -run 'P2ChatRefusal|P2ChatOverflow|P2UsageClaudeUpstream' -count=20 -timeout=180s`，退出码0。
- `go test -tags live ./internal/llm -count=1 -v -timeout=120s`，退出码0，TestLocalCompatibleModel明确PASS；仍为旧入口，不作为新Chat工厂真实endpoint认证。

修正开发文档04的过时Claude说明：agenticclaude v0.1.7已有CacheWriteTokens，补采集只为字段存在性/TTL等额外证据；现有真实adapter探针验证开启/关闭补采集均保持SDK数值，缺失与已知零值不同。同步明确Eino已有重试执行/可取消等待/默认退避，项目只通过ShouldRetry进行策略与预算判定，不另建模型重试循环。未修改原P2计划，未升级依赖，不缩减已批准范围。

工具部分恢复后须重新执行受影响race和全仓强制检查；当前模型专项通过不代表全仓已恢复。Windows符号链接权限、Linux/macOS运行与新工厂真实端点认证继续待验。

## 19. 恢复实现的独立审查与新增回归（2026-09-26）

冻结值对象、最终参数准备、策略/票据/资源接线已恢复。主执行者运行 `go test -race ./internal/agent/... ./internal/sessions/... -run 'P2Prepare|P2Frozen|P2Origin|P2Policy|P2Ticket|P2Resources' -count=20 -timeout=180s` 退出码0；标有no tests to run的包不作为专项覆盖证明。

对照实际实现补充四条行为回归，均进入默认测试，按用户要求以行为而非阶段命名：

- `execution_boundary_test.go`：调用方能通过原始BeforeCall切片替换已登记钩子；Started=true/Terminated=false/SideEffect=none被错误结算为succeeded并释放冲突资源。运行 `go test ./internal/agent/tools -run 'TestExecutorCopiesBeforeCallHooks|TestUnterminatedProcessRetainsResources' -count=1 -timeout=60s` 退出码1，两项真实断言失败。
- 最小修复：NewExecutor复用Definition.Clone，进程未确认终止保留outcome_unknown和资源占用，已确认副作用仍保留。第一次扩大回归退出码1，原因是无started证据的票据取消错误被一并改了状态；收窄为实际started且未终止，保留原取消/过期错误语义，未修改旧断言。`go test -race ./internal/agent/tools -run 'TestExecutorCopiesBeforeCallHooks|TestUnterminatedProcessRetainsResources|P2Ticket|P2Resources' -count=20 -timeout=120s` 最终退出码0。
- `tool_generation_test.go`：Create→Close→Open没有发现后端/副作用声明改变；无效schema类型在Create阶段未报错。运行 `go test ./internal/sessions -run 'TestReopenRejectsChangedExecutionDeclaration|TestCreateRejectsInvalidToolSchemaBeforeExecution' -count=1 -timeout=60s` 退出码1，两项都得到错误的nil。正在恢复generation静态描述和创建期单一schema编译/复用，不推进Step11。

另外核对到会话重开仍逐项RestoreHold，未将已有RestoreHolds批量原子能力接入；要求以会话级失败测试补齐，不用scheduler helper绿测替代。Steps9–10仍in_progress。本轮尚未重跑全仓强制检查；既往验证报告不覆盖上述新红测与后续在途修复。没有提交或推送。

## 20. Steps 9–10 修复交付后的独立复验（2026-09-26）

本节更新第 19 节的在途状态；其中失败命令作为历史证据保留。最终实现交付后，主执行者重新读取 `tools/schema.go`、`tools/execution.go`、`sessions/tools.go`、`sessions/manifest.go`、`sessions/tool_execution.go` 及新增回归，确认实现进入实际创建、执行和重开路径，而非仅定义 helper。

- JSON Schema 由唯一 `CompileSchemas` 使用 jsonschema/v6 编译，默认 Draft 2020-12，支持显式声明的版本；未登记的外部引用由离线 loader 拒绝。会话装配时编译，执行段通过 `WithCompiledSchemas` 复用；独立 Executor 使用同一编译函数回退。创建期无效 schema 返回 `invalid_argument`。
- 工具静态执行声明深拷贝后进入 generation manifest（固定工具配置清单）及其散列。真实 Create→Close→Open 测试覆盖后端、副作用、资源、超时、输出限制变化，返回 `incompatible_version`。旧清单未记录执行声明时保留旧散列和显式 Version 契约，重开不改写清单；不能据此证明旧日志没有记录过的声明相同。
- 实际生效的工具超时写入 `FrozenExecution` 并参与散列，执行使用该冻结值。原始模型调用参数保持不变。第 19 节的钩子切片隔离、进程未确认终止时保留 unknown 与资源限制两项修复仍在，相关回归通过。
- 会话恢复先收集所有未解决调用，单次调用 `RestoreHolds`；只有批量恢复成功后才释放已知完成的占用。新增会话级测试验证失败后既有占用不释放、合法候选也不部分暴露；其 100 次循环覆盖不同 map 遍历顺序，不称为确定性排序屏障。

### 本轮主执行者实际运行结果

以下命令均从仓库根目录执行，各退出码为 0：

- `go test -race ./internal/agent/... ./internal/sessions/... -run 'P2Prepare|P2Frozen|P2Origin|P2Policy|P2Ticket|P2Resources|ExecutorCopies|UnterminatedProcess|ReopenRejectsChanged|CreateRejectsInvalid|ResourceHolds|ToolSchema|ExecutionDeclaration|CompileSchemas|LegacyManifest' -count=20 -timeout=240s`。标为 no tests to run 的包不算专项覆盖。
- `gofmt -l .`：无输出；`go vet ./...`；`go build ./...`；`go mod verify`：all modules verified。
- `go test ./... -count=1 -timeout=180s`。
- `go test -race ./... -count=1 -timeout=180s`。
- `go test -race ./sdk/testdata/consumer -count=10 -timeout=120s`；全仓命令包含架构测试，未改 SDK consumer 断言。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`：No vulnerabilities found。使用固定工具版本补齐 PATH 缺失；不替代第 7 节记录的测试依赖图风险说明。
- `go test -tags live ./internal/llm -count=1 -v -timeout=120s`：TestLocalCompatibleModel 明确 PASS，无 skip；未直接读取凭据文件。仍是旧裸 HTTP 路径，不是新 Chat 工厂或其他四协议的真实端点认证。
- `git diff HEAD --check`：退出码 0，仅 LF/CRLF 提示；`git ls-files --others --exclude-standard`：退出码 0，新增清单无 exe/test/bin 或凭据文件。internal Go 文件及 docs 的冲突标记、私钥和常见密钥模式扫描无命中；`git check-ignore -q -- .test_env`：退出码 0，确认凭据文件仍被忽略。

### 验收边界

Steps 9–10 本次恢复及遗漏修复的 Windows 验证通过；完整跨平台验收仍未完成，既有待办继续标为 in_progress 并记录缺口。Linux/macOS 实际 build/test 本轮未运行，Windows 两个文件符号链接权限用例的既有跳过也未补齐，跳过不记为通过。请维护者提供相应运行环境或 CI 验证安排。

真实原生/Docker 沙箱开发与认证属于 P6，不是本次恢复遗漏，也不是 Steps 9–10 已提供的能力。P2 当前仅提供受控后端契约、注入和确定性验证，缺后端仍拒绝执行；审批 ask 的持久交互、四类工具包装、Pause/Resume、Reconcile 仍留后续步骤。本轮只做交付复核和记录同步，未启动 Step 11、未修改批准计划正文、未提交或推送。

## 21. Step 11 部分交付、流式契约阻塞及 CI 状态（2026-09-26）

阶段性提交 `b7a0fd36416f86b7f252cc305de00f22ce051155` 已推送，但不代表 P2 完成。[该提交的 CI](https://github.com/ww1489/seasprak/actions/runs/36228657710) 报告 macOS `go test ./...` 退出码 1；Linux/Windows 矩阵测试取消，Linux race 任务成功。当前只有公开任务状态，没有可核实的 macOS 失败断言；按维护者指示，将该故障留到 Step 23 处理。不得把被取消的测试或 macOS 失败记为通过，也不得用本机 Windows 结果替代三平台运行证据。

本轮未提交的 Step 11 工作：`Definition.ToolInterface` 作为静态 generation 声明进入 manifest；空值与显式 `invokable` 等价，重开时修改接口被拒绝。`NewPipelineToolForInterface` 的 `invokable` 与 `enhanced-invokable` 都调用既有 `Executor.Run`，增强参数使用经真实框架探针确认的 `ToolArgument.Text`。真实 Eino ToolsNode 和生产会话测试覆盖原始 CallID、大整数/中文参数、一次 claim、持久观察、预算与审批不可用、观察提交失败和重复工具批次。当前 `streamable` 与 `enhanced-streamable` 在创建期返回 `resource_unavailable`；这是明确不可用，不是能力对等。审批 ask 尚无 Step 14 的持久交互，不能把普通错误结果伪装成可恢复的 StatefulInterrupt。

阻塞来源：Eino v0.9.21 的 `schema.StreamReader.Close` 不提供可挂接、可等待的结束回调；`schema.Pipe` 的 writer 要到下一次 Send 才可能观察到 reader 已关闭。`InternalMergeNamedStreamReaders` 走异步 `toStream` 转发，外层 Close 返回仍可能滞留一次 Recv/预取。因此包装器既无法从外层 Close 返回证明后端已退出，也不能把提前 Close 判定为成功或释放仍可能生效的资源。现有框架探针还验证：在流 reader 的 `Recv` 中返回 `StatefulInterrupt`，恢复可能重跑已成功的 sibling；权限中断必须在交付 reader 前完成，且持久审批/恢复尚未交付。同步执行并只返回一块结果虽安全，但没有真实流式 progress，不能据此声称满足原 Step 11。直接返回异步 producer 会丢失同步 Close 后端结算保证。实施前须确认流式生命周期与进度契约或获准调整框架边界，不放宽副作用、权限与取消断言。

本轮主执行者从仓库根目录独立运行，以下命令退出码均为 0，未修改原计划正文：

- `go test -race ./internal/agent/eino ./internal/agent/tools ./internal/sessions -run 'ToolInterface|EnhancedTool|EnhancedBatch|ToolStream|P2FrameworkToolInterfaces|ReopenPreservesDefaultInterface|ReopenRejectsChangedToolInterface|CreateRejectsUnsettledStreamInterface' -count=10 -timeout=180s`；`internal/agent/tools` 在该过滤器下为 no tests to run，不计作专项覆盖。
- `gofmt -l .` 无输出；`go vet ./...`、`go build ./...`、`go mod verify`（all modules verified）。
- `go test ./... -count=1 -timeout=180s`、`go test -race ./... -count=1 -timeout=180s`；当前默认测试集通过，不覆盖尚未实现的两种生产流式接口。
- `go test ./internal/architecture ./sdk ./sdk/testdata/consumer -count=1 -timeout=120s`。
- `govulncheck ./...`：未运行成功，命令不在 PATH；改用固定版 `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`，退出码 0，报告 No vulnerabilities found（默认扫描范围）。
- `go test -tags live ./internal/llm -count=1 -timeout=120s` 退出码 0；额外运行 `go test -tags live ./internal/llm -run '^TestLocalCompatibleModel$' -count=1 -v -timeout=120s`，明确 PASS 而非 SKIP。仍为旧入口，不构成五协议产品 Factory 的真实端点认证。
- `git diff HEAD --check` 退出码 0，仅 LF/CRLF 提示；`git ls-files --others --exclude-standard` 仅列出两个 Step 11 测试文件。未提交或推送本轮改动。

结论：同步两类接口为局部交付；Step 11 仍 in_progress，两类流式接口、四接口统一表驱动测试及审批恢复/关闭结算均待实现。Step 12 以完整 Step 11 为前置，现不推进。三平台运行、Windows 文件符号链接权限用例及新产品工厂真实 endpoint 认证均保持待验。

### 仓库外 Eino 补丁可行性实验（同日）

维护者选择保留真正的流式进度及提前 Close 后的后端结算，并允许先验证、修补框架。以 Eino v0.9.21（提交 `ba04fde8641057055c358d7ab5d3015a9ba825e1`）在仓库外临时克隆进行实验，项目 `go.mod`、`go.sum`、原计划及发布构建均未改动；仅用临时 Go workspace 覆盖框架来观察本项目探针，不作为可发布依赖。

- 原始 `schema` 合并 reader 的关闭等待行为红测：`go test ./schema -run '^TestMergedStreamCloseWaitsForActiveConverter$' -count=1 -timeout=90s`，退出码 1，断言为 `Close returned while the stream converter was still active`。首次编译错误是测试对 synctest 签名的误用，修正后才得到上述行为红测。
- 临时尝试给异步转发增加停止通知与 join，并给转换 reader 加关闭通知：定向 `go test -race ./schema -run '^TestMergedStreamClose' -count=20 -timeout=90s` 退出码 0；真实 AgenticToolsNode 的受控进度/提前关闭测试 `go test -race ./compose -run '^TestAgenticToolsNodeCloseSignalsAndJoinsToolProducer$' -count=10 -timeout=60s` 退出码 0。项目原有 pre-reader Interrupt 和 Cancel 的限定探针在该临时覆盖下通过三轮，但这些都不足以认证通用框架流。
- 原有项目外层 Close 探针通过临时覆盖执行 `go test ./internal/agent/eino -run '^TestP2FrameworkToolStreamClosePropagatesToProducer$' -count=1 -timeout=12s`，超时退出非零：旧探针的 producer 特意等待 Close 返回后才继续发送，新的同步等待语义与之形成互等；若采用新契约须用协作式停止的生产者重写该特征测试，而非删掉断言。
- 更关键的是，临时补丁 `go test -race ./schema ./compose -count=1 -timeout=180s` 退出码 1；`go test -race ./compose -run '^TestDAG$' -count=10 -timeout=90s` 再次退出码 1，报 `parentStreamReader.close` 与 `parentStreamReader.peek` 对子 reader 状态的并发读写。未经修改的 Eino v0.9.21 独立对照工作树执行同一个 `TestDAG -race -count=10` 退出码 0。原因是补丁从关闭线程直接调用源 reader 的 Close，与仍在 Recv 的异步泵竞争。仅靠添加回调和等待不足以安全修复：停止请求必须与有状态 reader 的 Close 分离，先通知所有受影响生产者协作停止，再等待读泵与后端结算，最后安全清理；不合作后端仍不得伪造退出。

因此临时补丁已判定不合格，**未接入项目**，也未创建/推送框架提交或 PR。下一步需对框架的停止请求、reader 并发访问和转发 join 进行独立安全设计及完整 Eino/产品 race 验证；补丁交付还需要可固定、可获取的发布模块版本，不能把临时本地 replace 留在发布构建。Step 11 和依赖步骤继续阻塞。

维护者随后明确不希望修改 Eino 或维护 fork，当前不再推进框架补丁。本节的原始红测、Close 互等及 race 复现可在后续整理成官方 issue；本轮不提交 issue、PR、commit 或推送。保持原四类接口的完整验收时，当前产品不能仅用已有 Eino API 证明提前 Close 通知、后端收敛和真实进度同时成立；同步执行后返回单块 reader 不能充当流式能力。可行的阶段性收窄是继续拒绝两种流式接口，只在已验证的同步接口上实施 Pause/Resume/审批，并明确变更 Step 12 的前置验收范围；须先得到维护者对该依赖调整的确认，Step 11 仍不得标为完成。

## 22. 已确认的 SDK 工具输出流实施方式（2026-09-26，待验收）


维护者随后确认借鉴 Pi 的部分结果回调方式：只向 SDK 调用方逐段返回工具实际产生的内容，不增加百分比、阶段提示或进度条。Eino 继续通过两类同步工具接口等待最终结果；不修改框架，不启用两类原生 Streamable 接口。以下是本轮执行补充，不修改原 P2 计划正文，也不将原生四接口验收改记为通过。

1. **前置条件：**保留第 21 节两类同步包装器与已验证的 claim/观察、资源保留、取消等待、订阅隔离路径；新输出接线先红测再实现。
2. **目标与输入：**`agent/operations.go` 已有 ProcessOperations 的 ProgressSink 参数，但 Executor 当前传 nil；`tools/definition.go` 旧 Run 没有输出回调；`sessions/events.go` 已有有界独立订阅；`sessions/execution.go` 为唯一执行事实入口；`sdk/sdk.go` 为唯一公开生产文件。
3. **实施方法：**沿用唯一 Executor.Run 和原 Eino 调用链。新增类型化的实际输出片段及调用期 sink；旧 Definition.Run 原签名保留，增加可选的带输出回调执行函数，两者不得同时登记。受控进程回调适配到同一 sink；不创建第二执行器、另一事实库或新的工具 reader。
4. **动作顺序：**先验证原输出无法到达订阅；已提交 claim 后建立绑定 CallID/ExecutionID 的输出入口；后端调用期间串行受理实际文本片段，经原 mailbox 核验活动调用后发送临时 `tool.output.delta` 事件；后端返回时先关闭输出入口并结清已经受理的发布，再按原路径持久保存观察、返回最终结果。自定义工具新回调能力进入 generation 静态声明。公开必要别名并由仅 import sdk 的消费者实测。
5. **保留约束：**片段不是完成、启动或审批证明；SDK 不从输出推断副作用。输出按同调用单调序号发送，UTF-8 边界完整，单片有界；内容来自受信工具/后端准备的可公开文本，禁止在事件中附带原参数、票据、环境内容或不透明内容引用。原始完整日志及其产物持久化仍属 Step 16，本次临时流不承诺断线重放。关闭订阅仅停止查看，取消执行仍显式调用 Cancel。
6. **失败处理：**慢订阅/已关闭订阅不阻塞后端或改变执行结局；终态后、取消后与旧 ExecutionID 的晚到片段不得发布。取消超时不证明退出，未确认终止的后端继续保留 unknown/资源限制；最终保存失败不重执行。无输出的旧工具保持原行为，不伪造内容片段。
7. **交付物：**受控进程和受信自定义工具的真实内容流、同一最终结果管道、SDK 可用的类型别名和行为测试；新测试使用行为名称，不再添加 p2_ 前缀。本节当前只记录获准方法，不是实现完成证明。
8. **验收：**测试必须在后端完成前从订阅读到真实片段，并同时确认此时无最终观察/后续模型调用；覆盖中文和 emoji、并发片段序号、关闭/溢出订阅、晚到输出、取消不合作工具、观察提交失败和重开 declaration 变化。受影响包先 race 再执行全仓强制检查；原生流式两接口仍测试明确拒绝。通过本轮安全接线后，后续恢复能力以同步工具路径实施并单列原生流式未完成项。

## 附记：SDK 工具实际输出接线及本地验收（2026-09-26）

第 22 节规定的产品层输出方式已接入。受控进程通过 `ProcessProgress.Text`、受信自定义工具通过 `RunWithOutput` 在执行期间发送实际文本；同一 `Executor.Run` 负责授权、原子 claim、最终观察和模型结果。SDK 订阅收到临时 `tool.output.delta`；载荷 `toolCallId` 为产品调用 ID，不是可能在其他 Turn 重复的供应商 ID。空流名归一为 `output`，每片上限 50 KiB，按 UTF-8 边界分片。`ContentRef` 和后端 Sequence 不作为公开内容或可信序号。输出不写 journal，不回放，也不进入模型消息；仍要求受信后端自行准备可公开文本，SDK 不具备自动识别未知秘密的能力。

已提交 claim 的单次调用持有输出入口，绑定执行上下文、ExecutionID、产品调用 ID 和临时流 ID；并发写串行。调用返回或 panic 被转为失败结果后，先关闭入口并结清已受理发布，再保存观察。取消后即使生产者传入 `context.Background()` 也不能发布。订阅关闭/溢出不改变工具结果，未确认终止和保存失败保留既有 unknown/资源占用规则。原生两种 Eino Streamable 接口仍创建期拒绝，审批与持久 Pause/Resume 尚未由本内容流实现；Step 11 原四接口验收仍未完成。

行为红测：`TestProcessOutputHasSinkBeforeBackendCompletes` 首次失败，原断言为 `claimed process received nil progress sink`；`TestToolOutputIdentifiesProductCall` 在初始接线后失败，原载荷用了 provider-local-id 而不是产品调用 ID，两处已改为现有同一调用链。`TestRunWithOutputClosesBeforeFailedObservationAndDoesNotRerun` 的重复 race 测试最初超时于共享资源 scheduler；随后隔离该测试的 scheduler，并在测试中断言保存失败后实际保留 hold。旧执行输出在新段运行期间的测试初版比较全局 durable 序号，因新模型请求本身提交事实而误报；现改为检查旧片段被拒绝且新段的流位置未被污染。

本轮主执行者独立复核：

- `go test -race ./internal/agent ./internal/agent/tools ./internal/agent/eino ./internal/sessions ./sdk ./sdk/testdata/consumer -run 'TestRunWithOutput|TestProcessOutput|TestToolOutput|TestSessionSyncTools|TestCancelledUncooperative|TestSlowOrClosed|TestTerminalAndOldExecution|TestSDKConsumer|TestToolCallback|TestSessionRejectsConflicting|TestEnhancedToolUses' -count=20 -timeout=180s`：退出码 0；其中 `internal/agent` 和 `sdk` 在该过滤器下没有用例运行，不计作专项覆盖，其余工具、Eino、会话和外部消费者执行了对应测试。
- `gofmt -l .` 无输出；`go vet ./...`、`go build ./...`、`go mod verify` 均退出码 0。
- `go test ./... -count=1 -timeout=180s` 和 `go test -race ./... -count=1 -timeout=180s` 均退出码 0；默认全仓用例覆盖上述新增测试。`go test ./internal/architecture ./sdk ./sdk/testdata/consumer -count=1 -timeout=120s` 退出码 0。
- PATH 未安装全局 `govulncheck`；固定版 `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` 退出码 0，报告 `No vulnerabilities found`（默认可达范围）。`go test -tags live ./internal/llm -count=1 -v -timeout=120s` 退出码 0，旧入口 `TestLocalCompatibleModel` 明确 PASS；该测试自己读取本地配置，不代表 P2 五协议产品工厂真实认证。
- `git diff HEAD --check` 退出码 0，仅 Git 的 LF/CRLF 提示；`git check-ignore -q -- .test_env` 确认忽略。新增文件清单没有二进制构建产物、凭据文件或 `p2_` 阶段前缀；Go 和本记录扫描冲突标记、私钥标记及常见令牌形式无命中。差异检查不覆盖未跟踪文件，已另行扫描新增 Go 文件。

Linux/macOS 本轮没有实际运行，既有 macOS CI 失败的具体断言仍未取得，Windows 文件符号链接权限跳过项仍待验；这些不记为通过。旧 `p2_` 测试文件是此前已登记的框架/产品行为用例，不是本次内容流产生的临时废弃物；需在旧测试归类收敛时按行为重命名，本轮不批量移动以免改变无关测试。原 Step 11 原生四接口、审批恢复和原计划 Steps 12–23 仍不能标记完成。本轮未改原计划、未提交或推送。

## Step 12：在已验证同步接口上接入持久安全暂停（执行补充，同步路径本地验收见下文）

1. **前置条件：**维护者在 Step 11 两种同步接口及 SDK 内容流已交付、原生两种流式接口仍不可用的前提下要求继续。此项仅作为同步路径条件性交付，不修改原计划 Step 12 对完整 Step 11 的前置条件，也不提前宣布原四接口验收完成。
2. **目标与输入：**现有 `sessions/execution.go` 的 `TurnLoop` 未装配 checkpoint Store；`sessions/coordinator.go` 有 Cancel/Close 的真实退出屏障；`sessions/state/records.go` 已定义 CheckpointRef 和有限记录；`sessions/store/blobs.go` 有可选不可变 blob 能力；Eino v0.9.21 的 `TurnLoopConfig`、Stop、Wait 及 gob 封装由适配层核实。
3. **实施方法：**先写生产会话可编译的行为红测，分别证明当前没有安全 Pause、没有产品 checkpoint 关联。产品 key→blob 关联和 operation 归 sessions；Eino Store/codec 适配归 `internal/agent/eino`；跨包端口使用 `internal/agent` 执行层类型。复用已有 mailbox、`Manager.commit`、`CheckpointBlobs`、框架 `WithGraceful`，不新建 ReAct、独立日志或第二个调度器。
4. **动作顺序：**先验证存储能力和活动身份；在 mailbox 内受理 Pause operation；执行段登记可安全 Stop 的 loop，受理后请求 Graceful；worker 在 mailbox 外等待实际执行结束并核验 `CheckpointAttempted`、`CheckpointErr`、runner state、原输入、未处理及 late 项；blob 先同步保存；完成证据和历史进度再经唯一 Manager 原子关联 checkpointRef、paused 状态及 operation 结果。不凭 Eino Store.Set 单独宣称已暂停或可恢复。
5. **保留约束：**Pause 不取消执行段 context，不调用 Immediate，不制造工具结果/`turn_end`/`trace.settled`；Cancel、Close、活动超限继续走原终止路径；已 claim 而尚有未知效果不宣称安全。原生流式接口仍拒绝，Resume 与审批另按原计划后续步骤验收。
6. **失败处理：**不合作后端仍运行时不提前确认退出；请求等待超时不当作安全点。blob 成功、关联失败只留不可恢复孤儿；无 runner state、错误、输入身份不符、旧片段或历史进度漂移均不标可 Resume。存储失败不重跑工具；重开会话只恢复可浏览的状态，不自动执行。
7. **交付物：**明确的 Pause 受理与可查询结果、受保护 checkpoint blob 引用和真实安全暂停生命周期。未实现的 Resume 不导出或冒称可用；新测试按行为命名，不加 `p2_`。
8. **验收：**会话及 Eino 行为测试覆盖模型后/工具后、并发 Cancel、无安全点、持久失败/重开、已确认和未知效果、实际工具调用次数及没有自动继续；并发变更先跑受影响包 race，再执行仓库强制格式、vet、build、普通/race 全套、漏洞、live、差异与新文件检查。Windows 通过不能代替 Linux/macOS 运行或此前 macOS CI 故障定位。

### 同步路径局部交付及独立收口（2026-09-26）

新增 `agent/checkpoint.go` 的窄 blob 端口、`agent/eino/checkpoint.go` 的 Eino key→不可变 blob 适配及 v0.9.21 gob 校验、`sessions/checkpoint.go` 的存储接线、`sessions/pause.go` 的 Pause/GetOperation、`state/pause.go` 的原子关联。调用链为 Pause → mailbox 持久受理 → Graceful 请求 → 原 worker 等待真实退出及校验 → 活动计量结清 → mailbox 内单次提交 checkpoint/paused/operation。两种同步 wrapper 都进入实际测试。Pause 不给运行中的外部进程伪造 Terminated，不调用 Immediate 或取消 frame；Cancel 仍按原执行退出屏障优先处理。有效 checkpoint 与暂停状态关联并不等于严格 Resume 已交付；该阶段 Step 13 仍需验证构建、环境、历史进度、来源关系及恢复入口，后续实施见下一节。

独立收口新增三条可编译行为红测：已受理 follow-up 使 Pause 错误返回 state_conflict，checkpoint 丢失原模型配置版本/选择 revision，AgentSession 无 GetOperation 查询。修复后待处理输入保持原 inputId/traceId/state=pending，不自动消费；原模型尝试的 ModelConfigVersion、原 TurnID 和准备时选择 revision 写入 checkpoint；GetOperation 可在等待超时后及重开后查询同一 operation。取消与已受理 Pause 竞争的新增测试连续 race 20 轮通过，确认工具尚未退出时无 stopped/settled 或 checkpoint，取消后不保存安全暂停点。

另复现工具包重复测试挂起：`go test ./internal/agent/tools -count=20 -timeout=60s` 退出码 1，堆栈位于 `ResourceScheduler.Acquire`，卡住的测试在不同运行间变化；单例/两例过滤重复通过不代表完整包通过。加入临时诊断并用 panic/预算两例重复，5 秒超时再次复现：空 SessionID 的不同调用共用 `\x00prod-1` hold ID，而调度域使用可复用的 Executor 地址；后续异常调用 Retain 冲突后遗留 transient 占用，同地址被新执行器再次使用时等待该占用。临时诊断现已移除。新增 `standalone_identity_test.go` 将两种冲突固定为行为红测：两独立未知调用只有 1 个 hold；重用地址的新调用执行 0 次并超时。最小修复给独立 Executor 分配生命周期唯一 ID，同时用于没有工作区绑定时的调度域和没有 SessionID 时的 hold 所有者；显式会话/工作区仍使用共享 scheduler，未知效果的资源保留规则不变。修复后的完整工具包普通/race 各重复 20 轮通过，未通过降低锁约束或全局重置换取绿测。

本轮收口后主执行者运行：

- `go test ./internal/agent/tools -count=20 -timeout=120s` 与 `go test -race ./internal/agent/tools -count=20 -timeout=90s`：退出码 0。
- `go test -race ./internal/agent/tools ./internal/agent/eino ./internal/sessions ./internal/sessions/state -run 'Pause|Checkpoint|Cancel|Close|UnknownEffect|Standalone|Resources' -count=20 -timeout=180s`：退出码 0。清理新增未使用字段后，另执行 `go test -race ./internal/sessions ./internal/agent/eino ./internal/sessions/state -run 'Pause|Checkpoint|Cancel|Close|UnknownEffect|SessionCanQuery' -count=10 -timeout=180s`，退出码 0。
- `gofmt -l .` 无输出；`go vet ./...`、`go build ./...`、`go mod verify` 均退出码 0。
- `go test ./... -count=1 -timeout=180s`、`go test -race ./... -count=1 -timeout=180s`：退出码 0。
- `go test ./internal/architecture ./sdk ./sdk/testdata/consumer -count=1 -timeout=120s`：退出码 0。
- 固定回退 `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`：退出码 0，`No vulnerabilities found`；本机 PATH 仍没有直接 govulncheck 命令。
- `go test -tags live ./internal/llm -count=1 -timeout=120s`：退出码 0；仍只认证当前旧 live 路径，不扩大为 P2 五协议产品认证。

Linux/macOS 运行证据、Windows 文件符号链接跳过项和既有 macOS CI 失败尚待最终验收，不将跳过记为通过。本轮不实施 Step 13，不改原计划正文、依赖或上游 Eino，不提交或推送。Step 12 在获准同步路径上局部交付，原 Step 11 四接口与 Steps 12–13 全验收待办继续保持 in_progress。

## Step 13：同步路径的严格显式 Resume（2026-09-26，执行补充）

1. **前置条件：**维护者要求继续执行已批准计划；延续 Step 12 在已验证同步接口上的条件性范围。先重新阅读开发规划执行规范，确认当前 `feat/p0-p1-runtime` 工作区及未提交改动，受影响会话/Eino/state 包的初始完整 race 均通过。不修改原计划正文或重复创建待办，不把原 Step 11 的两类流式与四接口验收标完成。
2. **目标与输入：**`sessions/execution.go` 的原 TurnLoop、`coordinator.go` 的唯一 mailbox、`state/operations.go` 的幂等契约、`state/pause.go` 的原暂停关联、不可变 blob、静态 generation manifest 和现有 `agent/eino/scope.go`；权威恢复设计为 `09-persistence-and-recovery.md`。框架前置核验确认 v0.9.21 原生 GenResume/ResumeParams 不重跑模型后已接纳响应或工具后已完成批次；这不替代产品测试。
3. **实施方法：**先写可编译的默认行为红测，再接原生 GenResume，不构造第二个 ReAct、不重放 prompt、不改 Eino/依赖。新增 `sessions/resume.go`/`resume_validation.go` 与 `state/resume.go`；既有 Executor 继续用原 Call 账目复用结果。恢复校验与受理在同一 mailbox 视图上运行，Manager.commit 保存 operation、新执行段来源记录和已消费 checkpoint 状态，之后才启动 worker。
4. **动作顺序：**ResumeCommand 包含 traceId、expectedRevision、idempotencyKey，Principal 取受信 Options；同键先查询原 operation 回执。复核 paused/stopped/非终态、当前权限、unknown、原输入/Turn/调用/预算、blob/runner envelope、工作区/后端、generation/main target、模型版本/原 selection、history leaf/commit，拒绝 checkpoint 后新历史及未支持执行进度。仅接纳明确支持的未消费输入及控制变化；审批决定、核对合并、扩展/工作流状态仍不支持。一次 commit 受理后换 ExecutionID、恢复原 Trace/Invocation/Turn/Call 与 BudgetLedger；绑定已关联的 blob，GenResume 使用保存的 InputRef 和服务端 main target。GenInput 和 GenResume 都建立共享的执行 scope 上下文，后续工具读取真正的当前 Turn；恢复分支禁止 fresh GenInput 回退。
5. **保留约束：**原 call.Scope/FrozenExecution 不改写。活动事实仍严格按新 ExecutionID 准入；只有恢复记录明确关联的原调用可供当前段授权和 ticket 校验。已完成结果由既有 Executor/Eino 复用，不重复 claim、预算或 Run；未完成原 Turn 不再次 BeginTurn。Open 不启动 worker；已受理但中断的恢复点不会因重开再次消费。当前 SDK 仍只有 `sdk/sdk.go` 一份公开生产文件。
6. **失败处理：**不兼容或已消费旧点为 incompatible_resume，unknown 为 reconciliation_required，权限撤销为 permission_denied，旧 expectedRevision 为 state_conflict，同键异内容为 idempotency_conflict。关联/Append 失败不启动工作；已提交但确认响应丢失后，重开能查询 accepted 原回执、保持无停止证明及旧点不可复用。等待、取消或 Close 不伪造实际退出。旧输出拒绝；晚到观察只有原执行或持久 resumed_execution→checkpoint→原 call 的关联可追加版本证据，不能覆盖原结果、重置预算或自动恢复。
7. **交付物：**SDK 可声明 ResumeCommand/OperationReceipt/OperationStatus/ResumeEligibility；Snapshot 新增 session commit Revision 和逐 Trace 的 Resume 恢复资格，查询不写 journal、不执行模型/工具、不公开 blob 或 Eino target。`GenerationFingerprint` 非空是受信宿主对应用/SDK build 以及模型、工具、hooks、后端实现版本的明确兼容担保，配合 Go/OS/架构/Eino/codec、当前模型配置和函数/后端存在性摘要；不是自动识别同声明不同闭包。缺声明、缺模型 Configuration.Version 的旧注入路径可执行、Pause、浏览，但严格 Resume 拒绝。
8. **验收：**默认 `resume*_test.go` 实际调用生产 Resume；普通/增强同步接口模型后和工具后恢复、磁盘关闭重开、幂等/并发、提交前后响应失败、重复 Pause、当前模型/实现/环境/权限/历史/unknown/no-runner 门禁、旧片段与晚到结果来源、取消不合作工具、新模型工具轮次及 pending follow-up 新工具段。外部 `sdk/testdata/consumer/resume_test.go` 仅 import sdk 完成受控调用→Pause→关闭重开→显式 Resume→查询/幂等，模型与工具次数、原调用身份和预算同时验证。Windows 证据不代替其他平台。

### 红绿测试、独立审查与故障记录

- 初始可编译会话红测显示没有显式 Resume；暂停元数据红测显示 BuildCompatibility 仅为 `go1.27.0`、memory manifest hash 为空且未保存原 InputRef。补齐受信兼容性声明、manifest 和输入关联后，模型后/工具后真实恢复以及预算/身份断言转绿。
- Snapshot 红测分别发现缺恢复资格和缺 SDK 可用于 expectedRevision 的 session commit revision；补为已提交视图的派生查询，未保存另一份可恢复布尔。外部 consumer 最初为导出别名缺失的编译红测，补齐别名后真实产品恢复通过。
- 缺 Run 实现被错误受理的红测、晚到恢复结果无持久来源映射的红测、重复 Pause 丢失原模型版本的红测均已修复。新增 resumed_execution 有限记录，与 operation/trace 一次提交并按明确分支回放。
- 独立只读审查定位授权器固定旧 TurnID：同实例/磁盘重开两条新工具轮次测试真实失败 permission_denied。初次直接覆写当前 Turn 的修复使旧 `TestP2PolicyAuthorizationRequiresCommittedMatchingDescriptor` 失败，伪造 fallback scope 被授权。最终改为先校验固定段、再读边界注入的实际 scope，仅空 Turn 才补当前值；旧测试保留原拒绝断言。
- 新增 pending follow-up 工具段测试随后真实失败 `state_conflict: tool call was not accepted`，工具只执行 1 次而应为 2。原因是新输入 GenInput 没在模型/工具共同上下文预先创建 mutable scope，模型 before hook 更新的 Turn 对工具不可见，工具使用了原 frame 的旧 fallback Turn。给 GenInputResult.RunCtx 接入既有 WithExecutionScope 后转绿；未放宽事实 ExecutionID 检查。
- 父执行者另写行为红测：模型配置在 attempt 登记和 Pause 之间从 v1 改为 v2，原恢复校验错误返回 nil；现明确比较当前 Configuration.Version 与 checkpoint 原 ModelConfigVersion，拒绝不增加模型/工具调用、不受理 Resume operation，Snapshot 恢复资格为 false。
- 最初广范围 `go test -race ./internal/sessions ./internal/sessions/state ./internal/agent/eino -run 'Resume|Pause|Cancel|Close|UnknownEffect|SessionCanQuery|Operation' -count=20 -timeout=240s` 退出码 1，触发总时长上限。相同范围单轮 `-v` 实测 16.651 秒；当时超时显示的磁盘恢复用例单独 race20 通过，30.831 秒。相同广范围改为 `-timeout=600s` 后通过，sessions 255.787 秒。此为验证时长不足，不修改测试断言或生产等待逻辑；随后新增授权/跟随输入用例的真实错误另行红测修复，先前 snapshot 的通过结果不替代最终验证。

- 最终冻结前的广范围 race20 还发现旧 `TestP2PolicySessionChangedAfterFreezeAndCancelledHook/cancelled-hook` 的时序缺口：20ms 调用方等待可在 mailbox 受理取消之前到期，随后释放 hook 时工具仍合法执行，测试实际报 Run=1/trace=completed。现在只调整测试驱动：先发真实 Cancel、等待 `frame.ctx.Done()` 确认受理，再用测试私有可控 deadline 注入调用方 `DeadlineExceeded`；未改生产取消路径，保留未退出、Run=0、claim=0、预算=0、cancelled 原观察与结果配对断言。该旧测试 race100 实测通过，18.738 秒；最终宽范围继续按原条件重新验证。

### 验收边界

本节仅交付获准的静态 main Agent、同步两接口 Graceful Pause 恢复。原生两种 Streamable、业务审批 checkpoint/定向应答、核对结果合并、扩展/工作流及跨 generation 的兼容恢复未交付；缺声明的旧 checkpoint 明确不可 Resume。受理启动窗的全套子进程 kill/crash 及内层 runner 损坏专项仍需 Step 23 补齐，现有提交响应丢失注入不冒称进程 kill 认证。Linux/macOS、Windows 符号链接权限跳过及旧 macOS CI 故障仍待实际运行；live 仍是旧入口，不能当五协议产品工厂认证。本轮不提交、不推送、不改原计划正文、依赖或上游 Eino；既有 Steps 12–13 与 Step 11 待办保持 in_progress，尚不推进后续完整审批/核对验收。

### 最终冻结版本验证结果

以下为全部上述修复及旧测试受理屏障调整后，主执行者从仓库根目录实际运行的检查；均退出码 0：

- `gofmt -l .` 无输出；`go vet ./...`、`go build ./...`、`go mod verify`（all modules verified）。
- `go test ./... -count=1 -timeout=240s`；`go test -race ./... -count=1 -timeout=240s`（最终 sessions 35.345 秒）。
- `go test -race ./internal/llm ./internal/agent/... ./internal/sessions/... -count=1 -timeout=240s`。
- `go test -race ./internal/sessions ./internal/sessions/state ./internal/agent/eino -run 'Resume|Pause|Cancel|Close|UnknownEffect|SessionCanQuery|Operation|TestP2PolicyAuthorizationRequires' -count=20 -timeout=600s`：三个包全部通过，最终 sessions 307.329 秒，包含新 follow-up 工具段和原伪造 scope/取消安全断言。早先宽范围的超时和旧测试时序失败记录仍保留；未缩小范围冒称通过。
- `go test -race ./internal/sessions -run '^TestP2PolicySessionChangedAfterFreezeAndCancelledHook$' -count=100 -timeout=120s`：18.738 秒，通过。
- `go test ./internal/architecture ./sdk ./sdk/testdata/consumer -count=1 -timeout=120s`；`go test -race ./sdk/testdata/consumer -count=10 -timeout=120s`（22.167 秒）。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`：No vulnerabilities found，仍使用获准固定工具版本，不改变项目依赖。
- `go test -tags live ./internal/llm -count=1 -timeout=120s`；另 `go test -tags live ./internal/llm -run '^TestLocalCompatibleModel$' -count=1 -v -timeout=40s` 明确 PASS，而非 skip；测试自行读取配置，未查看/输出凭据内容，不扩大认证范围。
- `git diff HEAD --check`：通过，仅 LF/CRLF 提示；本记录追加时曾发现末尾空白行，已修正。`git ls-files --others --exclude-standard` 新增清单只有必要 Go/test 文件，无 exe/test/bin/凭据。sessions、SDK 与本记录的冲突/私钥/常见令牌形式扫描无命中；`.test_env` 仍被 Git 忽略。

最终独立只读复审未发现本轮指定范围内的剩余阻断问题；父执行者验证 GenInput 共享 scope、当前轮次授权、旧 fallback 拒绝及可控取消屏障均实际进入默认调用链。此结论只覆盖已测试的 Windows 同步路径；原计划完整 Step 11、Steps 12–13、P2 三平台与真实五协议认证仍未完成。Linux/macOS build/test/race 本轮未运行，请维护者提供实际环境或 CI 安排，不能将交叉编译视为认证。

## Step 14：同步路径的一次审批与定向恢复（2026-09-27，执行补充）

1. **前置条件：**维护者在阶段性提交 `56c592a` 后要求继续已批准计划。重新阅读开发规划执行规范，确认工作区干净，并先复验受影响完整调用链 race。延续静态 main Agent、两类同步工具的条件性范围，不修改原计划正文，不重复创建待办，不把原生两类 Streamable 或三平台认证记为完成。
2. **目标与输入：**现有 `state/records.go` 的 immutable Interaction/Approval、operation 幂等记录、原子预算、持久恢复来源及真实 Eino StatefulInterrupt 探针；权威审批、应答、恢复定义为设计 05、06、09、11。当前 ask 会保存 denied 并返回 resource_unavailable，实际审批入口尚缺。
3. **实施方法：**先写默认行为红测，再沿唯一 mailbox/Manager.commit/Executor.Run 接线。不改旧 Authorize 签名，不建第二许可服务、事实库或 ReAct。初始交互/审批仍不可变，新增有限 `approval_binding`、`approval_decision`、`approval_claim` 记录；框架地址仅在内部 binding 中保存，不作为 SDK 应答参数或公开事件内容。
4. **动作顺序：**最终描述已冻结后持久 asked，返回窄执行层 ApprovalWait，由同步 Eino wrapper 转成原生 StatefulInterrupt；Wait、blob、实际退出与原调用地址校验后，一事务关联 bindings/checkpoint/stopped。RespondInteraction 使用 Options 中的受信 Principal、expectedRevision 和幂等键，只保存 allowed-once/rejected/cancelled 决定，不自动执行。显式 Resume 从保存映射构造 Targets；未答 sibling 再中断，已有结果复用。启动前当前权限及 24 小时有效期再检查，许可占用、原调用 intent、预算同一提交。
5. **保留约束：**保持原 Trace/Invocation/Turn/Call/FrozenExecution，只有执行段变化；不放宽 matchesExecution。审批等待期间无活动 worker、工具超时或工具预算占用。SDK 保持唯一 `sdk/sdk.go`，增加必要别名及一致快照；公开交互 TargetRef 始终为空，实际冻结描述供授权查询。缺受信身份、兼容声明或 blob 能力的旧注入仍明确拒绝审批，不假造许可。
6. **失败处理：**拒绝、取消、过期、策略改变、旧版本和错误绑定均零迟到执行；必要 Append 失败不露出新许可/预算。取消不推断忽略 context 的 hook 已退出。只读核对/release 未实现，已占用许可不自动返还。Pause 与审批合流时，operation 完成、绑定、checkpoint 和 stopped 仍同一提交；批准后的工具观察保存失败仍不重新执行。
7. **交付物：**新增 `agent/approval.go`、`state/approvals.go`、`sessions/interactions.go`，接入已有工具、策略、Loop 和恢复路径。SDK 的 InteractionResponse、RespondInteraction 及 Interactions/Approvals/FrozenExecutions 快照供外部消费者实际关闭重开、批准和显式恢复。workflow 补参 answer 明确 unsupported；审批 Scope 与许可状态来自已提交事实的派生查询，不另存许可布尔。
8. **验收：**默认测试覆盖两类同步真实等待、原调用批准恢复、三工具逐个批准跨三次恢复、当前策略/有效期/取消、失败提交和损坏回放。外部 consumer 只通过 sdk 产品入口关闭重开→决定→显式恢复，核对实际模型/工具次数、预算、原身份和私密目标。高风险 race20、全仓普通/race、格式/vet/build/漏洞/live 及新增文件检查须在最终修复后重新确认；Linux/macOS 和完整故障窗另列待验。

### 红测及审查修复记录

- 现有 `ClaimTool` 实际接受了声明需审批但无决定的调用，红测收到 nil；补为必须走专用批准占用，并把三类事实原子化。 asked、绑定、决定及 claim 保存失败的视图均保持不变。
- 会话两种同步接口最初均因 resource_unavailable 终结；接入 ApprovalWait/原生中断后真实安全等待，模型 1、工具 0、预算 0，无伪造结果或 turn_end。保存批准后最初仍因旧 Resume 不支持 InteractionIDs 而拒绝；添加严格绑定和保存决定合并后，恢复原调用转绿。
- SDK consumer 最初收到 paused 但快照没有 Interactions；补齐派生快照和应答入口后，真实磁盘重开不执行，批准不执行，显式恢复工具 1 次、模型总计 2 次，幂等重试返回原回执。
- 独立状态复核建议已变为三条真实红测：恢复来源只追一层导致第二次未答重中断失败；跨会话 operation 仍能回放决定；删掉 claim 提交的预算 trace 仍能重开。分别修为有循环检测、逐层核对 CallID 的持久来源链、operation.SessionID 绑定、提交前整批校验原 call intent/预算/许可确实同事务；三条转绿。
- 三工具逐个回答的两接口产品测试验证计数依次 1/0/0、1/1/0、1/1/1，预算 1、2、3，模型总计 2，原 call Scope 不改写；初始 interaction/approval 保持不可变。
- 显式 Pause 先受理时，旧代码成功暂停却丢 InteractionIDs 和 targets；新增屏障红测后改为从 CancelError 的可信上下文捕获目标，并由 CommitApprovalPause 原子完成关联及回执。
- 再次只读复审定位反向窗口：真实业务中断已确定退出原因、blob 保存仍阻塞时受理 Pause，旧分支将等待点错误收敛为失败。Put 通道屏障红测真实收到 state_conflict；最小修复按实际 InterruptError 处理业务等待，不用稍后到达的 Pause 意图改写框架退出原因。两种次序均转绿，后续必须重新验收冻结版本。
- 取消后派生 Approval 状态仍显示 allowed-once 的红测已修复；未占用许可显示 cancelled，已占用许可仍保持 claimed，不回滚原决定或返还预算。

中间版本审批完整调用链 `go test -race ./internal/sessions ./internal/sessions/state ./sdk/testdata/consumer -run 'Approval|ApprovesOriginal' -count=20 -timeout=240s` 退出码 0，sessions 128.849 秒、state 20.789 秒、consumer 21.004 秒。此结果发生在最后晚到 Pause 红测修复之前，不替代最终冻结验证。Step 15 Reconcile、审批 no-start/release、原生流式、扩展/workflow 与跨平台均未交付，本节不作为完整 Step 14/P2 完成证明。本轮原计划和完整待办继续未完成，后续改动未提交或推送。

### 最新版本复验与历史失败（2026-09-27）

修复晚到 Pause 后重新读取先前后台宽范围结果：`go test -race ./internal/sessions ./internal/sessions/state ./internal/agent/eino -run 'Approval|Resume|Pause|Cancel|Close|UnknownEffect|SessionCanQuery|Operation|TestP2PolicyAuthorizationRequires' -count=20 -timeout=600s` 退出码 1，sessions 464.737 秒。失败为 `TestApprovalPartialAnswersKeepUnansweredOriginalCallsWaiting/invokable` 在第二个工具 b 恢复后 trace=failed、ToolExecutions=1、公开 Error 为空。该进程启动早于最后 Pause 修复，记录作为真实中间失败，不推断是 Pause 所致，也不当作通过。为保留可核查证据，测试在失败时额外记录当前执行段的活动租约原因与三个实际工具计数，未降低任何状态、预算或原身份断言。当前同一用例 `go test -race ./internal/sessions -run '^TestApprovalPartialAnswersKeepUnansweredOriginalCallsWaiting$' -count=40 -timeout=240s` 退出码 0，186.025 秒；尚不足以认定旧失败的根因已经定位或修复。

最新生产修复后主执行者已经实际完成，以下命令均退出码 0：

- `gofmt -l .`：首次发现仅新 `approval_pause_test.go` 未格式化，`go fmt ./...` 后重查无输出；`go vet ./...`、`go build ./...`、`go mod verify`（all modules verified）。
- `go test -race ./internal/llm ./internal/agent/... ./internal/sessions/... -count=1 -timeout=240s`：完整受影响调用链通过，sessions 41.346 秒；先于全仓运行。
- `go test ./... -count=1 -timeout=240s`：全仓通过，sessions 7.966 秒。
- `go test -race ./... -count=1 -timeout=240s`：全仓通过，sessions 41.045 秒。
- `go test ./internal/architecture ./sdk ./sdk/testdata/consumer -count=1 -timeout=120s`；`go test -race ./sdk/testdata/consumer -count=10 -timeout=180s`（23.866 秒）。外部消费者仍实际经过关闭重开、批准、显式恢复和查询，Open/决定零执行、最终实际调用一次。
- `go test ./internal/sessions ./internal/sessions/store/jsonl -run '^(TestSessionCrash|TestCommitCrashSubprocess)$' -count=10 -timeout=300s`：sessions 11.692 秒、jsonl 0.607 秒。旧 SessionCrash 的 input.accepted、assistant、tool_intent、tool_observation、trace.settled 各 before/after 窗口分别重复 10 次（合计 100 个子案例），torn-tail 真子进程窗口重复 10 次；不冒充新增审批、Resume 或 Reconcile 提交窗的完整 kill 认证。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`：No vulnerabilities found；固定工具版本，不修改项目依赖。
- `go test -tags live ./internal/llm -count=1 -v -timeout=120s`：`TestLocalCompatibleModel` 明确 PASS（1.03 秒），完整带标签包退出码 0。配置仅由测试内部加载，未读取或打印认证值；这是现有旧 HTTP 入口，不代表五协议产品工厂 live 认证。

最终 Pause 窄复审只读结果已返回：两条先/晚受理用例 `go test -race ./internal/sessions -run '^TestPauseDuringApproval(Preparation|CheckpointSave)PreservesAnswerableCheckpoint$' -count=20` 通过（24.493 秒）；状态、会话、Eino 的审批范围 race20，以及取消覆盖 Pause、blob/关联故障、真实工具退出和 Close 范围 race20 均通过。复核确认条件删除仍保留 Cancel/Close/context 优先、原输入/可信 target/blob 校验，以及 Pause operation 与关联/停止证明同提交。此复审仅为限定同步路径结论，不替代父执行者全仓和宽范围复验。

最终完整高风险命令 `go test -race ./internal/sessions ./internal/sessions/state ./internal/agent/eino -run 'Approval|Resume|Pause|Cancel|Close|UnknownEffect|SessionCanQuery|Operation|TestP2PolicyAuthorizationRequires' -count=20 -timeout=900s` 退出码 0：sessions 456.052 秒、state 22.784 秒、Eino 2.561 秒。900 秒为根据中间实测 464.737 秒预留的总运行上限，不缩减测试范围或改变产品等待逻辑。此为最后 Pause 生产修复及测试失败诊断补充后的新验证，完整范围含两条 Pause 次序用例、多轮部分应答、取消/关闭、原恢复身份和既有伪造 scope 门禁。旧中间失败没有再次出现；旧日志缺少活动租约原因，不能因此宣称历史根因已修复。

差异卫生首次 `git diff HEAD --check` 退出码 2，仅报告本记录 EOF 多余空行；去除该空行后退出码 0，仅有 Git 的 LF/CRLF 提示。另逐个用 `git diff --no-index --check -- NUL <new-file>` 检查 9 个新增 Go 文件，无空白错误输出；该形式因新文件与 NUL 不同返回 1，属于 diff 差异状态，不当作命令 0，未发现返回 2 的空白错误。新增清单只有上述 Go/test 文件，无 `.test_env`、exe、test 或 bin 产物；sessions、agent、SDK 与本记录扫描冲突/私钥/常见令牌形式均无命中。`.test_env` 存在且仍被 Git 忽略，`go.mod`/`go.sum` 本轮没有差异，Eino 仍固定 v0.9.21，无 replace。

完整 Step 14/15 和 P2 仍保持未完成：原生两类 Streamable、可信 Reconcile/no-start/release、完整审批与恢复子进程故障窗、其余协议/选择/受控内建能力尚待相应步骤交付。Linux/macOS 的 build/test/race 本轮未运行（无可用实际环境，退出码不适用）；Windows 文件符号链接权限跳过项、此前 macOS CI 失败细节及五协议产品工厂 live 认证仍待验，请维护者安排实际环境或 CI 并提供相关失败日志。交叉编译或配置文件存在不计作运行通过。本轮不修改原计划、不新建待办、不改依赖或上游 Eino、不提交或推送。

### Step 14 后续：保留混合取消错误的独立失败原因（2026-09-27）

1. **前置条件：**最新 Pause/审批冻结版本的全仓及完整高风险 race20 已通过；历史部分应答失败仍未归因。进一步只读检查确认 `errors.Is(runErr, context.Canceled)` 对整个 joined error 返回 true，会隐藏其他独立失败，不把这个诊断缺陷当作历史失败的已知取消来源。
2. **目标与输入：**`sessions/runSegment` 把执行退出与 `endActivity` 原因通过 errors.Join 合并；`coordinator.go/segmentFinished` 决定状态并保存错误；已有 manualActivityClock、activitySession 和 memory Manager 可直接复现，不需网络、真实等待或新生产接口。
3. **实施方法：**先新增默认 `execution_error_test.go`，以真实活动时钟推进与生产 worker 收尾为行为红测，另从真实 Manager 状态调用同一 segmentFinished 检查纯取消/包装/合并错误。只改变错误是否持久保存的判定，不新建日志、状态缓存或执行循环。
4. **动作顺序：**模型实际进入后将可控时钟推进一秒，确认已取消但模型未退出且没有停止证明；释放模型后原路径保存 failed 和结算，检查预算错误与重开。新增私有 `cancellationOnly` 递归检查单层 Unwrap 和 joined error 的每个原因；只有所有叶子都是 context.Canceled 才抑制错误，独立预算/DeadlineExceeded 原因保留原完整诊断。
5. **保留约束：**不改 worker 生命周期、活动预留/续订、Cancel/Close、终态优先级、预算、原审批 claim 或工具启动规则。普通纯取消（包括包装和多取消合并）仍不写失败诊断；有独立失败时取消终态仍为 cancelled，不复活执行。仅使用现有 budget_exhausted 错误码。
6. **失败处理：**未退出模型仍无 ExecutionStopped；一秒预留到期仍保守停止，不因总额还有两秒就继续派请求。保存/重开不能自动执行或返额。新诊断帮助下次归因，但旧日志缺少取消来源/期限，历史原因保持未确认。
7. **交付物：**一个内部判定函数、收尾路径一处条件替换、默认行为回归及更新后的验证记录；SDK/依赖/原计划不改，不提交或推送。
8. **验收：**首轮普通测试真实失败：活动到期后 failed/Error 为空，另外三个混合错误子案例同样丢失诊断；修复后相同两条行为测试转绿，`-race -count=100` 退出码 0（13.555 秒）。断言实际模型一次、工具预算零、原传输预算一次、实际退出后 stopped、结算一秒及重开保留错误；纯取消用例保持空错误，混合错误用例保留诊断且状态仍 cancelled。

由于收尾判定新增生产改动，上节 456.052 秒的宽范围通过只保留为修改前证据。本次重新冻结后的实际检查均退出码 0：

- `gofmt -l .` 无输出；`go vet ./...`、`go build ./...`、`go mod verify`（all modules verified）。
- `go test -race ./internal/llm ./internal/agent/... ./internal/sessions/... -count=1 -timeout=240s`：sessions 40.880 秒，先于全仓运行。
- `go test ./... -count=1 -timeout=240s`：sessions 7.916 秒；`go test -race ./... -count=1 -timeout=240s`：sessions 41.373 秒。
- `go test ./internal/architecture ./sdk ./sdk/testdata/consumer -count=1 -timeout=120s`；`go test -race ./sdk/testdata/consumer -count=10 -timeout=180s`（24.427 秒）。
- `go test ./internal/sessions ./internal/sessions/store/jsonl -run '^(TestSessionCrash|TestCommitCrashSubprocess)$' -count=10 -timeout=300s`：sessions 11.668 秒、jsonl 0.575 秒；仍是原十个 before/after 窗口各 10 次及 torn-tail 10 次，不扩大新增故障窗认证。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`：No vulnerabilities found。
- `go test -tags live ./internal/llm -count=1 -timeout=120s`；另 `go test -tags live ./internal/llm -run '^TestLocalCompatibleModel$' -count=1 -v -timeout=40s` 明确 PASS（0.96 秒），不扩大旧入口认证范围，不查看或输出本地配置。

最终命令 `go test -race ./internal/sessions ./internal/sessions/state ./internal/agent/eino -run 'Approval|Resume|Pause|Cancel|Close|UnknownEffect|SessionCanQuery|Operation|TestP2PolicyAuthorizationRequires|Activity|ExecutionFinalization' -count=20 -timeout=900s` 退出码 0：sessions 477.625 秒、state 23.793 秒、Eino 2.607 秒。保留原完整高风险过滤器，只追加活动与收尾诊断用例；这次结果包含本节诊断生产修复，不沿用之前冻结通过。

本轮唯一追加的未跟踪文件 `internal/sessions/execution_error_test.go` 通过单独 no-index 空白检查（返回 1 为新内容差异，无空白错误）；`git diff HEAD --check` 退出码 0，仅有 LF/CRLF 提示。新增文件及变更包/本记录秘密、私钥与冲突模式扫描无命中，凭据仍忽略，依赖没有差异。原生流式、Reconcile/release、跨平台及完整 P2 状态仍未完成；平台实际环境仍按上节请求维护者安排，历史部分应答失败保持未归因。本次未提交或推送。

### 当前审批批次的提交前复验（2026-09-27）

维护者随后明确授权当前差异提交并推送，同时要求继续所有剩余步骤。提交前完整受影响 race 通过（sessions 40.758 秒）；格式无输出，vet/build/mod verify、固定 govulncheck 和旧 live 包均退出码 0。首次全仓普通测试退出码 1：`TestStandaloneDomainSurvivesExecutorAddressReuse` 的测试挂起保护只有 100ms，本次启动前返回 `permission_denied: execution ticket is expired`、calls=0；同次全仓 race 通过。该用例测试独立调度域，不验证百毫秒启动性能；仅将其测试调用期限改为 5 秒，不改生产票据期限、取消或授权规则，保留实际执行一次和旧未知 hold 仍在的全部断言。

调整后 `go test -race ./internal/agent/tools -count=20 -timeout=180s` 退出码 0（6.109 秒）；重新执行 `go test ./... -count=1 -timeout=240s` 和 `go test -race ./... -count=1 -timeout=240s` 均退出码 0（sessions 12.452 秒、42.475 秒）。sessions/agent/SDK 与新增文件秘密和冲突模式扫描无命中，原 `.test_env` 不进入暂存。上节高风险 race20 和 crash10 的生产版本未改变，这次只改测试保护期限；其结果不扩大为 P2 或三平台完成。实际提交/推送结果另由 Git 核对，不修改原计划文件。

## 协议工厂与 Step15 后续回归（2026-09-27，本批仍未收口 P2）

本批新增 Anthropic Messages 和 Gemini GenerateContent 基础 Catalog 工厂，复用固定 Eino adapter、请求期凭据和 observed transport；修正红测中的 Close、AsError、schema 类型、认证头和建流阻塞假设。Anthropic 将 end_turn/tool_use 分别归一为 stop/tool_calls；增加实际缺失 message_stop 的行为红测，确认先失败后在既有有界采集器中补终态证据。Gemini 固定一次 SDK 尝试，工具 CallID 不能保真时在请求前返回 unsupported_capability；服务端工具、额外 provider route、显式缓存资源仍不可用。两协议的 thinking/off、完整缓存、usage presence、关闭后读取收敛和执行层端到端契约未全部验收，不标 Steps19–20 完成。Responses 缺 usage 流测试先失败，随后清除中间 SDK 块的合成 usage，只在最终块提交有原始证据的数值。

Step15 新增回归覆盖 LookupTool/FinishTools 使用核对结果、残留未知和相互矛盾的 no-start/confirmed 证据、查询期间 Trace 状态变化，以及 no-start 后的迟到确认事实。原观察保持不可变，最新核对和其后的不同观察保留未知门禁；迟到真实 CommitFact 路径恢复资源限制，进一步核对允许基于最新已确认观察继续处理。原子 release 的独立持久记录、完整崩溃窗口和已有工具结果消息的恢复投影仍待验收，不能据这些测试标记整个 Step15 完成。默认模型选择测试改为等待持久 completed 状态；尚未证明完整下一 Turn/审批/重开选择契约。

本批最终 Windows 验证：

- `gofmt -l .`：退出码0、无输出；此前按项目要求执行 `go fmt ./...`，并单独格式化 `sdk/testdata/consumer`。
- `go vet ./...`、`go build ./...`、`go mod verify`：均退出码0，依赖完整性通过。
- `go test -race ./internal/llm ./internal/agent/... ./internal/sessions/... -count=1 -timeout=180s`：退出码0，先于全仓竞态验证。
- `go test ./... -count=1 -timeout=180s`：最终退出码0。
- `go test -race ./... -count=1 -timeout=180s`：最终退出码0。
- `go test ./sdk ./sdk/testdata/consumer ./internal/architecture -count=1 -timeout=120s`：退出码0。
- `go test -race ./internal/sessions ./internal/sessions/state -run 'Reconcile|Reconciliation|LateConfirmedEvidence|UnknownEffect|Unresolved' -count=20 -timeout=180s`：最终退出码0，包含真实迟到 CommitFact 入口。本批未新增或重跑全部崩溃进程窗口，不挪用旧证据作为新窗口证明。
- `govulncheck ./...`：命令不可用，PowerShell CommandNotFoundException；改用既定 `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`，退出码0、0个可达漏洞；另报告1个未调用的包级漏洞和1个模块级漏洞，不宣称依赖图完全无漏洞。
- `go test -tags live ./internal/llm -count=1 -timeout=120s`：退出码0，仍非五协议产品工厂的完整真实端点认证。只通过测试加载本地配置，未读取凭据内容。
- `git diff HEAD --check`：退出码0，仅LF/CRLF提示。列出的新增文件无禁止提交产物；internal Go文件的冲突标记/常见密钥/私钥模式扫描无命中。`.test_env`仍被忽略。

Linux/macOS 本批未运行。`wsl --list --quiet` 提示未安装 Linux 子系统；docker/gh 不在 PATH。平台及全部剩余功能仍待完成，原计划文件未改，本批未提交或推送。

## Step15 原子释放与协议 thinking 校验（2026-09-27，续批）

新增不可变 `resource_hold_release` 控制记录，与新观察、核对结论和 operation 完成同一次 Append。回放也校验事务成员和受信 no-start/停止证明；不返还预算或 claim。查询期间新增 ExecutionID 一致性复核。TrustedNoStart 与 ConfirmedEffects 同时出现保留冲突；迟到确认使已有释放失效并恢复 hold。旧日志只有 no-start 观察而无释放记录时保守保留 hold，本批没有迁移旧记录。新增行为红测先复现缺记录、观察误释放、ExecutionID 变化及矛盾证据误释放，再转绿；另覆盖 Append 失败视图不变、同事务回放和历史不改。完整进程崩溃窗口及已有工具结果消息恢复投影仍未收口。

Anthropic 工厂新增 `1024 <= thinking budget < max_tokens` 校验，Generate/Stream 调用时缩小输出上限也不能绕过；非法配置实际请求和占额均为零。合法 fixture 使用 1024/2048 并验证 1024/1025 边界。Gemini 显式 off 仅在已有 scoped verified 声明且精确模型为 gemini-2.5-flash/flash-lite 时通过固定 adapter 发送零 thinkingBudget；未认证模型或 off 映射为 minimal 等不能兑现配置前置拒绝。未指定 thinking 与显式 off 保持区别。本批未新增 endpoint 认证，显式缓存和完整协议契约仍未完成。

实际 Windows 验证（均退出码0）：

- `gofmt -l .`：无输出。
- `go vet ./...`、`go build ./...`、`go mod verify`：通过。
- `go test -race ./internal/llm ./internal/agent/... ./internal/sessions/... -count=1 -timeout=180s`：最终协议改动后再次通过，先于最终全仓 race。
- `go test ./... -count=1 -timeout=180s`、`go test -race ./... -count=1 -timeout=180s`：最终改动后通过。
- `go test -race ./internal/sessions ./internal/sessions/state -run 'Reconcile|Reconciliation|ResourceHoldRelease|LateConfirmedEvidence|UnknownEffect|Unresolved' -count=20 -timeout=180s`：通过。
- `go test ./internal/sessions ./internal/sessions/store/jsonl -run 'TestSessionCrash|TestCommitCrashKeepsPrefix|TestRuntimeCancelPausedCrashPrefix' -count=10 -timeout=180s`：通过，仅证明现有命中用例，不代表所有新增故障窗已覆盖。
- `go test ./sdk ./sdk/testdata/consumer ./internal/architecture -count=1 -timeout=120s`：通过。
- `go test -tags live ./internal/llm -count=1 -timeout=120s`：最终协议改动后通过，仍不代表五协议产品工厂认证。
- 安全检查沿用固定 `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`；0个可达漏洞，仍报告未调用的包级和模块级漏洞。

新增文件清单未见禁止产物，internal Go 文件的冲突标记和常见密钥模式检查无命中；`.test_env`仍被忽略。Linux/macOS 实际运行、五协议完整统一契约、选择/直接命令/SDK闭环等仍未完成。未修改原计划、未提交或推送，不将本批结果标为P2完成。

## 直接命令事务、模型选择恢复与 usage presence（2026-09-27 续批）

直接命令受理现在将 operation、输入和 Trace 同事务提交；command 结果、operation 终态和输入消费同事务提交。零 ExpectedRevision 不随最新日志版本改变幂等摘要；确认丢失后重开不重新执行。默认测试先复现孤立 operation、结果部分可见、输入 undelivered、重复请求冲突及 unknown 仍受理新工作，再转绿。另通过真实 ExecuteCommand 红测修复普通参数误用 schema 规范化：required 数组保持原序，大整数精度保留，尾随 JSON 请求前拒绝。直接命令完整审批/Resume 仍未交付；当前并无与其相容的 Eino checkpoint，不能改为 fresh-start 来规避。

模型选择幂等重放不再替换进程内实例或回退最新默认。SelectNextTurnModel 在终态检查前查询原回执，同键异内容仍冲突。恢复校验只允许核对通过的 next_trace 默认选择控制记录，原 Trace/Turn 的模型实例和 revision 用于恢复；当前 Trace pending 选择、未知记录、新历史继续拒绝。真实审批测试覆盖 A 为初始模型、Trace 默认选择或 Turn 激活选择三类：决定不执行，显式恢复 A、工具一次，新独立 Trace B。磁盘重开后的动态模型实例重新绑定及多工具部分审批组合尚待完成。

Anthropic/Gemini/DeepSeek 复用最终 read-through 统计投影 usage；中间块不重复累计，缺失字段及溢出不使用 SDK 合成零值。Anthropic 保留 message_start 输入和 message_delta 输出；统计溢出后继续有界识别后续 message_stop，终态自身超限或不完整仍拒绝。工厂行为红测及分帧/溢出测试已进入默认套件。完整工具与显式缓存等未交付能力不因这些测试标为支持。

本批实际 Windows 验证：

- `gofmt -l .` 无输出；`go vet ./...`、`go build ./...` 退出码0。
- `go test -race ./internal/llm ./internal/agent/... ./internal/sessions/... -count=1 -timeout=180s` 最终退出码0。
- `go test ./... -count=1 -timeout=180s` 最终退出码0。
- `go test -race ./internal/sessions -run 'ExecuteCommand|Selection|DefaultModel|NextTurnModel|Resume.*Model|Model.*Resume' -count=20 -timeout=240s` 首次退出码1：command fixture 共享全局调度域，unknown 占用污染后续轮次。count=2复现，排除unknown的对照通过；仅给fixture独立临时工作区和调度器，同fixture重开仍复用，新增Close后unknown hold仍在断言。最终组合race20退出码0（179.150秒），未加长原等待期限或修改生产释放规则。
- `go test -race ./... -count=1 -timeout=180s` 一轮退出码1：TestRuntimeConsumesConsecutiveSteeringAtSeparateBoundaries 遇到 activity reservation expired。当时其他验证并发运行；尚未证明并发负载就是根因。该用例单独 `-race -count=20 -timeout=180s` 退出码0，随后全仓同命令独立复跑退出码0（sessions 70.856秒）。保留这次间歇失败，未宣称根因已修复，未削弱活动预算限制。
- `go test -race ./sdk/testdata/consumer -count=10 -timeout=180s` 退出码0。
- `go test -tags live ./internal/llm -count=1 -timeout=120s` 退出码0，仍是现有live入口，不代表五协议工厂真实认证。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` 退出码0、0个可达漏洞，仍报告未调用的包级和模块级漏洞。

新增文件清单未见禁止产物，internal Go文件冲突及常见秘密模式检查无命中，`.test_env`仍忽略。最新Git状态未复现启动快照中的反斜杠重复文件。Linux/macOS 未运行；本批未补全新进程崩溃窗口。原计划未修改，未提交或推送，P2保持未完成。

### 活动预留旧截止校验的追加修复

调查确认“预留过期但总预算仍有余额时停止”符合既有安全契约，不放宽该行为。另用假时钟和阻塞Append确定性复现独立漏洞：续期在500ms提交，旧watchdog未获调度，1000/1100ms才返回成功时会错误放行新请求。红测实际TransportRequests从1增到2，退出码1。现保存并复核旧预留截止时间，迟到提交仍保留事实用于退出结算，但取消执行、拒绝新请求；999999999ns准时成功边界保持通过。模型未退出前不宣称ExecutionStopped。此修复不被用作前述steering间歇失败根因的证明。

`go test -race ./internal/sessions -run '^TestActivity' -count=20` 退出码0。最终主流程再次执行受影响sessions/state全包race、`go test -race ./... -count=1 -timeout=180s`（sessions 63.733秒）、`go test ./... -count=1 -timeout=180s`，全部退出码0。最终代码格式无输出，vet/build通过；`go mod verify`退出码0、all modules verified；固定govulncheck再次退出码0（0可达漏洞但仍有未调用漏洞），现有live再次退出码0。`git -c core.safecrlf=false diff HEAD --check`在修正文档末尾多余空行后通过。上述仍不能替代三平台、五协议完整认证和所有剩余P2交付。

## Step 4 定名与绑定覆盖补齐（2026-09-27）

新增 `internal/sessions/store/jsonl/blobs_publication_test.go`，复用现有 SyncFile 屏障，未修改生产代码或新增注入点。覆盖初次不存在检查之后的同内容/同长度异内容目标竞争、真实 Link 非 EEXIST 失败，以及 journal 路径被替换后的绑定拒绝；检查错误码、空失败引用、内容和文件身份不被覆盖、临时名清理及无覆盖 fallback。journal 替换 fixture 在 Windows 先关闭旧句柄、改名再持有原文件，并用 SameFile 确认身份；这证明绑定检查，不宣称普通 Windows 进程可以直接改名被锁定的 journal。

主流程阅读全部新增测试后独立执行：
- Windows：`go test -race ./internal/sessions/store/jsonl -run 'BlobLink|BlobRejectReplacedJournal' -count=10 -timeout=120s`，退出0。
- Linux：经 WSL、`GOTOOLCHAIN=go1.27.0` 执行 `go test -race ./internal/sessions/store/jsonl -run Blob -count=10 -timeout=120s`，退出0，包含全部新增用例。此前带管道符的选择器被跨壳解析成命令，报 `BlobRejectReplacedJournal: command not found`，不计为测试结果；改为更广的 Blob 选择器后实际测试通过。

本次属于已有正确行为的覆盖补充，不声称生产缺陷红绿修复。Windows 原有两个 symlink 子测试权限跳过、macOS 未验，以及 Steps 3–4 其他待核事项仍保留，原待办不提前完成。其他包仍在并行补齐中，待稳定后再进行整仓最终验证。未提交或推送。

## Steps 3–4、9–11 重新核验：Linux 运行证据（2026-09-27）

维护者要求先核验并补齐这些前置步骤，再更新既有待办并继续后续步骤。本轮逐条覆盖核验仍进行中，以下通过结果不能单独证明每项验收条件均已满足，不提前修改为 completed。

WSL 的 ext4.vhdx 挂载权限故障已修复，普通用户启动及停止后重启均退出0。安装 Ubuntu Go 引导包和 build-essential，实际项目命令显式设置 `GOTOOLCHAIN=go1.27.0`；`go version` 为 `go1.27.0 linux/amd64`。代码位于 `/mnt/d/Code/owner_agents/seasprak`，测试临时目录采用 Linux 默认临时目录。未修改代理设置，localhost/NAT 提示仍存在，但本次工具链及模块下载成功。

以下命令通过 `wsl -d Ubuntu-24.04 --cd /mnt/d/Code/owner_agents/seasprak -- env GOTOOLCHAIN=go1.27.0` 实际运行，退出码均为0：
- `go test ./internal/sessions/store/... -count=1 -timeout=180s`。
- `go test -race ./internal/agent ./internal/agent/tools ./internal/agent/eino ./internal/sessions/state ./internal/sessions/store/... ./internal/sessions -count=1 -timeout=180s`（sessions 71.371秒）。
- `go test ./... -count=1 -timeout=180s`。
- `go test -race ./... -count=1 -timeout=180s`（sessions 68.336秒）。
- `go vet ./...`；`go build ./...`。

这是 Linux 实际运行而非交叉编译；不等同 macOS 运行证据，也不替代尚在核查的逐项故障/安全验收。此前“WSL 无法启动”的记录为历史状态，本节覆盖该环境阻塞的当前状态。未提交或推送。

Windows 专项复核：Step 3 原命令 `go test -race ./internal/agent ./internal/sessions/state ./internal/sessions -run 'P2Budget|Budget|ExecutionStopped' -count=10 -timeout=180s` 退出0。Step 11 的 `EnhancedTool|EnhancedBatch|ToolInterface|ToolOutput|Approval` 三包十轮组合首次使用180秒进程总时限，sessions退出1（test timed out）；保持同一测试集合、十轮次数、用例内部期限及全部生产安全限制，仅将进程总时限改为300秒，三个包退出0，sessions耗时208.316秒。本次组合已包含新增direct审批测试；保留首次超时证据，不将其改写为通过，不据此声称修复生产死锁。逐项覆盖审计尚未返回，三个既有待办继续in_progress。

## 直接命令持久审批恢复：实现及主流程验证（2026-09-27）

维护者批准完整 direct 审批恢复契约后，Step 16.1 已接入现有调用链：ApprovalWait 不提交命令终态；worker 退出后唯一 coordinator 原子保存 DirectResumeBinding、审批绑定和 paused/ExecutionStopped；决定只记录，显式 Resume 使用新 ExecutionID、原调用身份，经同一 Executor 和一次 claim 执行。完成复用 SaveCommand，等待取消结算原 operation。模型 checkpoint 校验保持独立，不制造 Turn/blob/FunctionToolResult。

新增 commands_approval、commands_approval_fault、commands_approval_race 默认测试。实现阶段红测分别复现等待被错误结算，以及取消后原 operation 仍 running；修复后审批/Open/拒绝/撤权/变化描述路径后端次数为0，成功恢复累计1，unknown 不重执行。JSONL 实测关闭重开；五处追加失败和确认丢失使用内存故障注入，不能冒充操作系统强杀或掉电试验。

主流程阅读新增恢复实现、审批/claim/回放/取消接线和测试断言后，独立运行以下检查，全部退出码0：
- `gofmt -l .` 无输出；`go vet ./...`；`go build ./...`。
- `go test -race ./internal/sessions ./internal/sessions/state ./internal/agent/tools -count=1 -timeout=180s`（无名称过滤，三个包实际运行；sessions 67.020秒）。
- `go test ./... -count=1 -timeout=180s`。
- `go test -race ./... -count=1 -timeout=180s`（sessions 67.249秒）。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`：0可达漏洞，另有1个未调用包级、1个模块级漏洞；使用固定版本代替未安装到 PATH 的裸命令。
- `go test -tags live ./internal/llm -count=1 -timeout=120s`：通过，仍不等于五协议完整真实认证。仅确认 `.test_env` 存在且被忽略，未读取其内容。
- `git diff HEAD --check`：通过，仅LF/CRLF提示；五个新增Go文件已核对，命令相关文件冲突标记/常见秘密模式搜索无命中，无禁止产物。

平台限制更新：WSL 已登记 Ubuntu-24.04，但启动挂载 ext4.vhdx 报 `Wsl/Service/CreateInstance/MountDisk/HCS/E_ACCESSDENIED`，沙箱外重试仍失败，Linux运行未执行；macOS运行未执行。保留三平台及新进程故障窗口未验收状态。此记录证明本地 Windows 检查通过，不将 Step 16 全部产物功能或 P2 整体标为完成。未提交或推送。

## P2 同步工具接口范围调整（2026-09-27）

维护者已明确授权先同步设计、开发文档与原计划，再继续后续步骤。仓库设计、需求、开发验收文档及外部 `p2_详细开发计划_6daf332f.plan.md` 已同步：包括目标、Step 11 方法和验收、Step 12 前置、待办描述及批次顺序，未自动完成待办。

- 本次 P2 仅要求 Invokable、EnhancedInvokable 两类同步接口，继续共享既有执行管道。已核对 `internal/sessions/tools.go` 的 `alignTools` 和 `internal/sessions/tool_interface_test.go`：`invokable`（空值等价）与 `enhanced-invokable` 为同步接口；`streamable`、`enhanced-streamable` 在装配期返回 `resource_unavailable`。`internal/agent/eino/tool_variants_test.go` 覆盖真实 ToolsNode 的 Invoke/Stream 调度模式；该 Stream 调度模式不等于产品支持原生 Streamable 工具。本轮先核对源码和测试定义，再运行 `go test -race ./internal/agent/eino ./internal/agent/tools ./internal/sessions -run 'EnhancedTool|EnhancedBatch|ToolInterface|ToolOutput|Approval' -count=10 -timeout=180s`，三个包均退出码0（sessions 157.336秒）；仅证明命中测试通过，不代表全部 Step 11 场景已验收。
- 保留已有 SDK 实际输出回调及入口关闭、已受理输出结清和晚到拒绝要求；不要求动态百分比、阶段提示或其他工具进度。实际输出回调不是原生 reader 生命周期或 Streamable 验收证明，不删除现有输出能力。
- 原生 Streamable、EnhancedStreamable 明确不在本次 P2 范围，保持装配拒绝；不修改 Eino、不维护 fork。其未交付及历史 reader Close/Recv、Interrupt sibling 重跑等限制不再阻塞 Step 12 或 P2 出口。Step 12 的工具前置按同步两接口和既有安全要求验收；Step 11 不因本次文档范围调整自动标为 complete。
- 模型流式响应、取消、实际后端收敛、unknown 效果与资源占用、审批/checkpoint、预算和防止重复执行等约束均不改变。其余平台运行、协议认证、故障窗和未交付功能仍需逐项验收，范围调整不代表 P2 完成。
- 第 21 节及后续历史记录中的原四接口验收、阻塞结论、临时框架补丁红测和“原计划未修改/待批准”原文保留，表示当时范围和证据；当前范围以顶部声明和本节为准。框架缺口没有被宣称修复，历史失败没有被改写为通过。
