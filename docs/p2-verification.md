# P2 实施记录与验证证据

## 2026-09-29 嵌套 Extra 与 Snapshot 优化（进行中）

基线 `a8fa57fef3d544482e2e4998d5671b17058e645e`，执行前工作区干净。本轮不读取 `.test_env`、不运行 live、不处理 macOS、不提交推送。

已确认 Eino v0.9.21 `compose/graph_run.go` 在写 checkpoint 前调用 `deepCopyState`；其内部序列化切片分支保存元素类型并以 `reflect.SliceOf` 重建，导致接口中的 `json.RawMessage` 恢复为 `[]byte`。直接 gob 类型矩阵通过，但真实恢复链四个扩展位置的严格命名类型断言失败。维护者已明确选择有限支持：普通嵌套 JSON 树及 `json.Number` 为恢复合同，RawMessage 的恢复类型限制单列，不改框架编码器、不声称其端到端命名类型保真。原始字节内容仍须验证。

旧标量夹具已用 Go build overlay 将生产 `checkpoint.go` 替换为修复前源码后重新独占捕获；主线程核对 overlay 文件与 HEAD 对应 blob 哈希均为 `6abe582b065913b3bbbeaccc029e00c83665bdd0`，当前 `TestCheckpointLegacyScalarResume` 普通及 race 均通过。夹具不由默认测试重写，来源和兼容范围见 `internal/agent/eino/testdata/README.md`。本证据仅认证旧模型后、工具前标量 checkpoint，不推广到全部历史格式。为不改变旧回归的流式覆盖，测试模型在没有新增 decorate/inspect 回调时仍调用原 Stream 实现；完整 Checkpoint 专项 race3 通过。

Snapshot 结构性红测 `TestSnapshotOwnedProjectionDoesNotSerializeAgain` 在普通历史和宿主消息两分支均复现每条重复序列化一次。新增 `PublicMessageOwned` 与新旧投影等价性测试，原 `PublicMessage` 仍复制后调用共享脱敏逻辑。完整嵌套夹具先采集双平台 before，再仅将 `snapshotMessages` 两处切换到 owned 入口。`runtime.snapshot` 明确消费独占 View；所有恢复校验及 ctx.Err 检查仍先于展示投影。通用 View/commit 深复制、Store.Load/blob Get 和公开字段规则均未改动。

新增 `TestCheckpointExtraIndependentProcessReopen`：独立 writer 真实 Pause 保存磁盘并退出，再由全新 reader Open/Snapshot 零执行、显式 Resume。模型后和工具后两暂停点均断言跨进程工具合计一次、原模型不重跑、四处嵌套树与 json.Number 精确恢复；没有测试注册或编码预热。主线程 Windows 普通及 race 精确专项分别 exit 0。恢复完整 `privateReplayFixture`，不再将嵌套扩展转为标量或清除 Extension；默认隔离测试覆盖具体私有值与公开投影。

新增 `TestSnapshotMessagesConcurrentIsolation` 验证并发修改旧快照消息不影响新快照或 Manager，并验证已取消请求与关闭后查询；结构性红测接线后转绿。Windows `go test -mod=readonly [-race] ./internal/sessions ./internal/agent -run 'Snapshot|PublicMessage|CheckpointExtra' -count=1 -timeout=180s` 均 exit 0。后续整仓与性能结果见本节补充。

### 同夹具性能对照与验收状态

先恢复完整嵌套 Extra 并通过默认测试，再在相同机器、Go 1.27.0、GOWORK=off、GOMAXPROCS=4 下，执行 `go test -mod=readonly ./internal/sessions -run '^$' -bench '^BenchmarkAgentSessionSnapshot$' -benchmem -benchtime=1s -count=5 -timeout=15m`。Windows before/after 与 Linux before/after 四次均 exit 0，每次 20 组各五轮；不使用旧标量夹具作收益对照。

独立复核四份结果：各组 ns/op、B/op、allocs/op 中位数均下降。32/128 页 completed/running：Windows 耗时下降 39.45%–41.61%，Linux 40.93%–43.91%，分配字节下降 52.73%–58.78%；paused 耗时 Windows 下降 26.32%–27.37%，Linux 22.20%–25.99%，分配下降 10.13%–11.49%。Windows 20 组及 Linux 19 组的 after 最慢轮次仍快于 before 最快轮次；Linux 一页 paused 存在单次波动，中位数下降 11.88%，没有稳定退化证据。各组 Load/blob Get/Append 次数保持一致。renewal 是同步续租组合，不称纯查询延迟；B/op 是累计分配量，不是常驻内存。

Linux 恢复专项 race3、vet/build、完整普通及 race（均含 `./sdk/testdata/consumer`、`-mod=readonly -count=1`）全部 exit 0。Windows vet/build及完整普通通过；首次全仓 race exit 1，`TestSDKConsumerReadSnapshotThroughSession/bytes/conflict-false` 和 `conflict-true` 报 `budget_exhausted: activity reservation expired`。随后同用例独立 race10 exit 0（18.411s），未改代码、租约或断言；专项不代替全仓，首次失败原因仍未确定。Windows 随后原范围全仓 race 复验 exit 0（consumer 12.874s），`go mod verify` 和固定版 `govulncheck@v1.8.0` exit 0，扫描仍提示一项包级和一项模块级未触达漏洞。格式无输出、diffcheck及新增测试常见凭据/冲突扫描通过。首次偶发租约失败仍保留，未定位根因，不把重跑通过称为修复；整体稳定性收口待维护者决定是否继续调查。macOS/live按授权未执行，未提交推送。


## 2026-09-29 P2 离线收尾（双平台门禁通过）

基线 `53fadff1c9874380396cc2ec25c1900b3653840e`。执行前 17 项工作区修改标记均为索引 LF/工作区 CRLF，`git diff HEAD --exit-code` 与暂存区差异均为 0；不把这些标记计为未提交生产修复。本轮仅离线收尾，不处理 macOS、不运行 live、不读取 `.test_env`、不提交推送。最终稳定代码已按 Linux→Windows 串行完成下列门禁，不沿用上一轮结果冒充本轮通过。

### 当前方案与历史记录的关系

2026-09-28 22:01 维护者要求“先直接按照 pi 的设计来吧”，22:04 在持久待提交命令方案说明后要求“开始执行”。因此以下 20:06 的“不采用新增记录”是已被后续决定替代的历史方案，不是当前实现要求。已提交的执行说明见 `docs/superpowers/plans/2026-09-28-pi-host-command-context.md`：shell 结果以不可变 `host_command_result` 保存，不推进正在恢复的模型历史；原 Trace 终态后、下一独立输入前消费；审批等待和已答复未恢复期间拒绝新命令，同 Trace 暂停/恢复不消费。当前回归见 `commands_checkpoint_test.go`、`commands_continuation_test.go`、`commands_boundary_gap_test.go` 和 `state/host_commands*_test.go`。

Gemini 原始响应精度恢复、Activity 窄读取与受限候选复制已在当前基线中实现并有产品测试，历史“尚在调查”不代表当前缺失。通用 View 与 commit 的完整复制仍保留；本轮先测 Snapshot，不擅自扩展浅复制或改变恢复资格缓存。PowerShell 原生命令退出码已有测试，本轮按风险复验，不预先宣称存在缺陷。

### PowerShell 复验与新增回归

新增 `internal/sessions/commands_shell_windows_test.go`，Windows PowerShell 5.1.26100.9444 与 PowerShell 7.6.6 各 7 个真实进程用例：显式非零、原生非零、cmdlet 非终止/终止错误、原生成功后 cmdlet 错误、成功及 UTF-8。每项验证 Started/Terminated、准确退出码、输出、执行一次、模型零调用，以及内存和关闭重开后的持久结果与返回结果一致。原专项 `TestP2CommandShellExplicitShellExitAndOutput` 也复验通过，没有复现原生退出码被归一为 1，不修改生产 shell 包装。

Windows `go test ./internal/sessions -run '^TestP2CommandShell(ExplicitShellExitAndOutput|WindowsPowerShellResults)$' -count=10 -timeout=180s`：exit 0（61.567s）；对应 `-race`：exit 0（65.788s）。catch 后成功、前错后对、原生非零与 cmdlet 错误并存的优先级没有新增合同，不把本次覆盖推广到这些未定义组合。

### Snapshot 基准口径

新增 `internal/sessions/snapshot_benchmark_test.go`：真实完成的四个 Trace，1/8/32/128 页（每页 50 KiB 已接受助手文本），覆盖完成态、宿主命令排序、实际 Pause 生成的 checkpoint、运行态及计时器驱动续租竞争，共 20 组。使用内存存储和测试时钟；测量不是磁盘后端性能或真实时钟租约压力认证，不替代现有真实时钟测试。`renewal` 是查询与续租的组合耗时，包含同步，不是纯 Snapshot 延迟。基准准备不计时，记录 ns/op、B/op、allocs/op、Load/blob Get/Append 次数。

默认 `TestSnapshotBenchmarkReadOnlyIsolation` 验证两次快照之间的深层隔离、私有信息不公开但内部回放保留、命令展示排序/去重、查询零模型/工具/Append。暂停态每次查询实际 Load 和 blob Get 各一次；其他纯查询为零。续租组合每次恰好一次独立 Activity Append，模型和工具调用不增加。

夹具初版把嵌套 `map[string]any` 放在接口类型 Extra 中，真实暂停返回 `gob: type not registered for interface: map[string]interface {}`；最终夹具使用可编码的标量私有 Extra 和签名，没有修改生产编码器。该基准不认证任意嵌套扩展的暂停兼容性，后续如需承诺此能力，应另建契约与回归，不能将夹具调整称生产缺陷修复。

两平台基准均固定 `GOMAXPROCS=4`、`GOWORK=off`，命令为 `go test -mod=readonly ./internal/sessions -run '^$' -bench '^BenchmarkAgentSessionSnapshot$' -benchmem -benchtime=1s -count=5 -timeout=15m`。Windows 完整 100 次采样 exit 0（282.751s）：50 KiB 完成态 0.854–0.884 ms/op；6.25 MiB 完成态 39.46–42.17 ms/op、约 96–98 MB 分配/op；同体量暂停态 52.29–55.79 ms/op、约 202 MB 分配/op。B/op 是累计分配量，不是常驻内存。

Windows 对照 `go test -mod=readonly ./internal/sessions/state -run '^$' -bench '^BenchmarkActivityLedgerStages$/pages_(1|8|32|128)$/View$' -benchmem -benchtime=1s -count=5 -timeout=300s`：exit 0；128 页 View 38.28–39.40 ms/op。此 View 夹具以合成事件为主，不能用它与真实消息快照的数值相减作为各阶段耗时分解。Linux 最终可读汇总确认 20 组各 5 次、共 100 次采样，exit 0：50 KiB 完成态 0.976–1.050 ms/op；6.25 MiB 完成态 45.16–51.04 ms/op、约 95.7–98.0 MB 分配/op；同体量暂停态 65.61–72.61 ms/op、约 201.8–202.0 MB 分配/op。初次 Linux 基准链及后续重采样虽 exit 0，但输出转码异常；最终显式设置输出编码并统计恰好 100 个样本，才采用上述数值。Linux View 对照首轮基准链 exit 0，但其数字输出未作为本节定量依据。不同夹具与平台不用于宣称因果或优化幅度。

本轮暂不改 Snapshot 生产逻辑：现有测量证明大历史成本增加，但没有容器预分配前后收益证据；不为达成“优化”而改变状态所有权、复制或恢复校验合同。

### 最终离线门禁（2026-09-29）

平台使用原生 Go 1.27.0、`GOWORK=off`，所有最终测试明确 `-count=1`，不使用测试结果缓存。Linux 使用 WSL Ubuntu 24.04，完成后才运行 Windows。

- 两平台受影响包 race：`go test -mod=readonly -race ./internal/sessions ./internal/sessions/state -run 'Snapshot|Activity|CommandShell' -count=1 -timeout=300s` 均 exit 0。Linux sessions 27.360s；Windows sessions 38.695s。
- 两平台 `go vet -mod=readonly ./...`、`go build -mod=readonly ./...` 均 exit 0。
- 两平台 `go test -mod=readonly ./... ./sdk/testdata/consumer -count=1 -timeout=600s` 均 exit 0。Linux sessions 31.093s、consumer 3.406s；Windows sessions 37.800s、consumer 3.168s。
- 两平台 `go test -mod=readonly -race ./... ./sdk/testdata/consumer -count=1 -timeout=900s` 均 exit 0。Linux sessions 159.128s、consumer 13.537s；Windows sessions 172.860s、consumer 14.257s。显式包含 testdata consumer，不以外层 SDK 的子进程测试代替 consumer race。
- 第一轮 Linux 门禁命令因嵌套 shell 引号将正则的 `|` 解析为管道，报 `Activity: not found` / `CommandShell: not found`、exit 127；改为 WSL 直接传参执行专项后通过。这是命令构造失败，不是产品断言失败，历史结果保留。
- 格式与差异：`gofmt -l .` 无输出；`git diff HEAD --check` exit 0；两份新增测试各自 `git diff --no-index --check -- NUL <file>` 无空白错误。新增文件常见凭据模式、私钥头、冲突标记扫描无匹配，未跟踪文件仅两份 Go 测试，无构建产物。
- 维护者另行批准对基线 17 个 CRLF 文件仅运行 gofmt；规范化 Git 差异没有新增生产逻辑修改。本轮新增两份测试，修改依赖元数据和三份说明，不实施未经证明有收益的性能优化。
- 本轮离线收尾完成；不宣称 macOS、live、任意嵌套 Extra 恢复或磁盘 Snapshot 性能已认证。未提交推送。

### 依赖与私有补丁维护

- `GOWORK=off go mod verify`：exit 0。
- `GOPROXY=off GOTOOLCHAIN=local GOWORK=off go mod tidy -diff` 首次 exit 1，仅 5 项直接依赖分类和旧 Claude/Gemini 模块 4 行校验记录有差异；审查后运行离线 `go mod tidy`，再次 `tidy -diff` 无输出、exit 0，`go mod verify` exit 0。没有改变模块版本。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 -show verbose ./...`：exit 0，代码可达漏洞 0；包级 `GO-2026-6443`（gRPC）和模块级 `GO-2026-5932`（OpenPGP）仍有未触达提示，未为消除提示盲目升级。扫描访问公共漏洞数据库，但没有真实模型请求。
- Windows `go test -mod=readonly ./internal/llm/... ./internal/architecture -count=1 -timeout=180s` 及对应 `-race`：exit 0；不代替最终全仓。
- `internal/llm/einoext/PATCH_NOTES.md` 补齐响应恢复、流式参数文件、Claude 缓存扩展及切回上游的完整回归条件；早期测试证据保留，当前最终结果见本节后续更新。

## 2026-09-28 20:06 审批期间拒绝宿主命令方案获准

维护者明确批准：等待工具审批期间，SDK拒绝新的宿主ExecuteCommand，并保护已答复但尚未恢复的间隙；状态和历史查询、审批答复及取消仍可用。不采用新增host_command控制记录或延后注入上下文方案，不放宽checkpoint检查。正在以真实Session测试实现，并核查命令先启动、随后进入审批的竞争；本项尚未完成。

已完成独立专项：Windows读取 `go test -mod=readonly -race ./internal/agent/tools -run '^TestRead' -count=10 -timeout=120s` exit0（4.639s）；Windows PowerShell退出码race10 exit0（11.615s）；Linux文件发现race10 exit0。进程输出四包race10首次因并行Gemini编辑中usage_body.go未用变量编译失败，随后同命令重跑exit0（agent2.746s/tools9.115s/eino11.960s/state48.016s）：`go test -mod=readonly -race ./internal/agent ./internal/agent/tools ./internal/agent/eino ./internal/sessions/state -run 'ProcessModel|ProcessLog|SaveOutputLogRejectsUnpublishableReference|HistoricalProcessFinishTools|ToolOutputProjection' -count=10 -timeout=180s`。不能把第一次编译失败记为测试断言失败或PASS；专项不代替最终全仓。历史超大固定引用仍保留未解决边界，Gemini响应修复仍待交接。未提交、未追加live。

## 2026-09-28 19:46 审查修复接续，提交验收尚未完成

维护者要求核对历史任务，已完成的结束，未完成的继续，并修复剩余问题。历史记录确认此前修复已进行，早期交接摘要的“尚未修复”不再代表当前状态。文件发现最终模型输出限额修复已完成；读取错误正文隔离和命令恢复调查曾因服务503中断，现接续；Gemini出站数值回放已有修复，原始响应精度仍待修。进程输出限额专项已通过，但历史状态直接重建及超大artifact元信息仍需核查，不据局部成功宣称可提交。

主线程当前复验：`GOWORK=off go test -mod=readonly ./internal/llm/... ./internal/sessions ./internal/agent/tools -run 'TestGeminiNumeric|TestP2CommandShellExplicitShellExitAndOutput|TestDiscoveryModelContent|Test.*Command.*Checkpoint' -count=1 -timeout=120s`，exit1。Gemini原始响应普通/流式均将9007199254740993变为9007199254740992；独立宿主命令使审批暂停任务恢复检查返回incompatible_resume（checkpoint history projection differs）。PowerShell退出码和文件发现专项通过。另独立读取编码用例exit1，因安全正文修复尚未接齐本地编码诊断。以上失败是当前未完成项，不覆盖为历史PASS。

执行顺序：保留已完成修复与红测；按原调用路径区分本地安全诊断和后端错误、恢复Gemini数值保真、隔离宿主命令历史与模型恢复点、补齐进程输出最终表示边界。各项先验证缺陷再最小修复，保持权限/预算/取消及历史事实，随后独立复审并进行Windows/Linux全仓普通、race、静态构建和提交检查。此前Gemini live复测例外与macOS延期继续保留，未追加live请求；本轮尚未提交。

## 2026-09-28 17:19 维护者批准Gemini本轮免复测，P2按限定范围收口

维护者明确表示Gemini若为地域问题本次可不再测，采用此前通过的记录。最新定向请求已明确返回官方地域不支持，因此停止追加Gemini请求，保留此前Generate及Stream完整工具往返成功证据作为本轮验收依据。17:13完整live的Stream末次resource_unavailable没有HTTP状态证据，其根因仍未确定；不将其追溯认定为地域问题、不删除失败记录，也不声称最新完整live退出0。此为维护者批准的复测例外，不是把SKIP计作PASS。

原plan Steps1–23均按已批准范围completed：实现和确定性验收完成；Windows/Linux最终全仓普通/race、静态构建及52子案例各10轮真实Kill/reopen已通过；四工厂最新完整live通过，Gemini采用已记录分项成功证据并附本次访问限制。macOS运行继续按此前批准延期，不能宣称三平台全部认证。P4/P5/P6能力保持原边界。本次仅更新记录和状态，不发网络请求、不改生产代码、不提交或推送。最近验证命令与exit code见本文件后续原始记录。

## 2026-09-28 流式定向诊断再次遇到地域拒绝

完整live后仅追加一次Gemini Stream诊断，首请求HTTP400且LocationUnsupported=true，exit1（1.838s），实际1次即停止。仅新增测试专用EOF/timeout/canceled布尔分类，不输出错误正文。此次未到达上一轮末次回传失败位置，因此不能认定上一轮错误也是地域限制。未改生产、代理或凭据，未继续重试，Step23仍in_progress，等待官方服务访问条件恢复或明确延期授权。

## 2026-09-28 17:13 获准完整live一轮：Gemini流式末次回传失败

维护者明确授权五协议30次完整测试。执行 `GOWORK=off go test -mod=readonly -tags live ./internal/llm -count=1 -timeout=300s`，exit1（33.636s）。本轮未筛选子测试、未跳过协议，未追加重跑。唯一失败为 `TestLocalCompatibleModel/GeminiGenerateContent/stream_true`：流式文本与工具调用各HTTP200且每次observed=1/physical=1，第三次工具结果回传在invoke返回 `resource_unavailable`。本轮该失败没有HTTP状态结构日志，不能推断为429、503或地域限制。Gemini Generate三次HTTP200完整通过，另外四工厂均无失败且各自完整六次请求断言通过；此前地域拒绝未在本轮已收到的响应中出现。计划正常请求数30；末次失败未完成请求计数断言，因此不把30次全部成功或末次服务接收情况写成已证实。Step23仍in_progress，仅剩live未通过；不改生产逻辑或削弱验收以取得绿色结果。

## 2026-09-28 最终live按维护者要求减量，单请求遇地域限制停止

维护者选择继续实际验证但要求减少请求，因此只补当前Gemini配置的Generate完整往返，预算最多3次，不重复其他工厂与已通过的Stream。执行 `go test -mod=readonly -tags live ./internal/llm -run '^TestLocalCompatibleModel$/^GeminiGenerateContent$/^stream_false$' -count=1 -timeout=120s -v` exit1（0.922s）；第一请求HTTP400，固定安全分类 `LocationUnsupported=true`、`InvalidAPIKey=false`。实际仅1次请求，未自动重试，未继续工具步骤。该结果是当前访问地域不受支持的服务拒绝，不能作为模型/工具实现失败的证明，也不能算验收成功。没有修改地址、代理、凭据或生产代码。Step23离线已通过，最终live仍阻塞；等待维护者解决官方服务访问地域限制或明确批准延期，缩减请求授权不是延期授权。

## 2026-09-28 17:01 最终离线验收完成，live综合门禁待维护者决定

最终故障矩阵由主线程独立运行：`go test -mod=readonly -race ./internal/sessions ./internal/sessions/store/jsonl -run '^(TestSessionCrash|TestProjectionCrash|TestRecoveryReconcileCrash|TestRecoveryApprovalCrash|TestBlobCrashSubprocess)$' -count=10 -timeout=600s`。Windows sessions194.923s/jsonl14.370s、Linux sessions216.473s/jsonl13.008s，均exit0。共52子案例：基础12、审批/恢复18、日志投影4、Reconcile4、底层blob14，各平台每案例10轮；此前Linux中间态编译失败已由此稳定代码结果替代，不删除失败历史。

故障证据映射：基础覆盖attempt/助手接纳/原观察/工具意图/结算；审批覆盖内存asked/decided、Resume受理、checkpoint关联、claim、后端进入/返回、观察提交、恢复operation结束、实际停止证明及最终结算；投影覆盖日志保存和引用提交前后；Reconcile覆盖受理与取证后的结论/新观察/release事务前后；blob覆盖同步/发布边界、部分journal尾部与定名冲突。仅证明进程终止恢复，不声称断电持久性；后端为受控合成实现，不前移P6。迟到结果与取消竞争使用已有确定性屏障/race20证据，不将其冒充进程死亡后还能发送回调。

最终稳定代码Windows/Linux `go vet -mod=readonly ./...`、`go build -mod=readonly ./...`、`go test -mod=readonly ./... ./sdk/testdata/consumer -count=1 -timeout=600s`、`go test -mod=readonly -race ./... ./sdk/testdata/consumer -count=1 -timeout=900s` 全部exit0。Windows sessions普通27.781s/race137.946s；Linux普通25.566s/race136.010s。gofmt无输出、go mod verify、git diff HEAD --check通过；固定govulncheck v1.8.0 exit0，代码可达漏洞0，依赖仍有未调用包级1/模块级1提示。CI普通及race命令补入sdk/testdata/consumer，因Go默认./...不遍历testdata；此修改不代表远端CI或macOS已运行。

Steps1–22保持completed。Step23实现及离线故障验收完成，但整体暂不completed：此前全包live曾失败，后来按维护者请求仅补测Gemini流式通过，且普通/流式来自不同模型配置；最终完整live未获成功证据。本轮遵循减少请求要求没有新增网络请求。需维护者选择允许一次全包live（正常30次模型请求，无测试内自动重试）或明确批准以现有分项证据结束本轮并延期完整live；不能擅自把限流要求解释成免验。macOS仍按既有批准延期、不记PASS。未提交或推送。

## 2026-09-28 Step23 新增故障窗口实施中

本批继续复用真实Session/Eino/JSONL与父进程Kill/Wait，未新增生产恢复状态机。主线程新增 `TestProjectionCrash` 四窗口：日志保存前/后、投影引用提交前/后；断言原观察先持久、日志孤儿不自动发布引用、已提交投影保留原效果、Open/Close模型调用0且原受控执行和保存不重跑。该测试双平台race10退出0（14.612s、16.568s）。夹具最初遗漏state目录、能力声明与必需日志redactor，均表现为配置拒绝/不进入保存窗口，修正仅限测试，未削弱生产门禁。

新增 `TestRecoveryReconcileCrash`（accepted/result各before/after）和 `TestBlobCrashSubprocess`（14个底层存储窗口）已进入独立整合验收：Windows与投影测试合并race10退出0（sessions36.977s、jsonl14.600s）；Linux jsonl race10退出0（13.121s），sessions该轮编译遇到同时扩展中的审批测试中间态（未定义helper/未用变量）exit1，不能记为Linux整组合格，将在编辑结束后重跑。审批测试仍在扩展checkpoint关联、claim、目标启动及恢复退出窗口。此段是进行中记录，不代表Step23完成；不把底层blob存储测试冒充Session恢复认证。

## 2026-09-28 Steps18–21 实现验收完成；Step23 补充模型尝试真实崩溃窗口

逐条对照原计划Steps18–21：Responses完整输入/store=false、Claude签名/缓存/usage、Gemini opt-in缓存生命周期和DeepSeek/五协议一致性均有产品测试及已有双平台证据。本轮额外执行 Windows/Linux `go test -mod=readonly -race ./internal/llm ./internal/agent/eino ./internal/sessions -run 'Responses|Anthropic|Gemini|DeepSeek|Protocol|Factory' -count=10 -timeout=180s` 全部exit0（Windows sessions62.641s，Linux61.073s）。Windows命令还包含两个私有包目录，但该正则未选中其测试，不能记为本轮私有包专项；随后全仓套件实际运行了这些包。原plan对应Steps18–21聚合TODO已completed，认证范围仍保留同源网关与Gemini不同模型配置的限制。

Step23发现现有TestSessionCrash只含五类业务窗口，未拦截model_attempt初始提交。先新增屏障识别红测exit1，再扩展既有crashStore及真实Kill/Wait/reopen路径；现为六窗口×before/after=12子案例。新增窗口断言逻辑预算已占额但物理请求/工具效果均0，尝试记录提交前0、提交后1，原模型调用身份存在，重开不执行且原受理回执幂等。初次新增用例错误套用了旧窗口预算预期，失败后根据实际提交顺序修正测试预期，未改生产预算或放宽已有窗口断言。Windows/Linux `go test -mod=readonly ./internal/sessions -run '^TestSessionCrash$' -count=10 -timeout=180s` 分别exit0（13.426s、12.791s）。

新增测试后再次执行Windows/Linux全仓普通/race（均含sdk/testdata/consumer）全部exit0；Windows gofmt无输出、vet/build及diffcheck通过。本轮未发送新的live请求，遵循维护者减少请求的要求；此前分项live证据继续保留，不声称本轮完整live通过。Step23仍in_progress：需继续核对并补齐审批内存问答/claim与目标启动、blob关联、Resume执行段和Reconcile迟到结果的真实进程故障窗证据；已有普通故障注入、Close/Open与race20不自动等同这些Kill窗口全覆盖。macOS按批准延期。当前仅测试代码变化，无生产代码改动、无提交。

## 2026-09-28 15:45 按维护者要求仅复验新模型流式：三次请求全部通过

维护者因请求数量与限流更换模型，明确只测此前未过部分。仅执行 `go test -mod=readonly -tags live ./internal/llm -run '^TestLocalCompatibleModel$/^GeminiGenerateContent$/^stream_true$' -count=1 -timeout=120s -v`，exit0（3.634s）。流式文本→工具调用→结果回传恰好3次HTTP200；精确答案、工具名称/参数、调用身份及每次observed=1/physical=1断言通过，无自动重试，未测试普通模式或其他工厂。本模型输入usage存在，输出usage缺失，按现有能力声明保留unknown，未伪造输出计数或削弱断言。

修正live测试父级请求总数检查：按实际选中的子测试累计，每个完整conversation必须3次；全选仍必须6次，避免只选流式时被硬编码6误判。只改测试计数，不改生产预算或请求策略。此前普通模式在前一个配置模型通过、本次流式在新配置模型通过；这两份证据不能合并声称同一模型的Generate/Stream均已认证，也不声称本轮全包live通过。Gemini流式基础往返子项已通过；Steps18–21/23整体状态仍取决于剩余原计划验收。没有追加网络测试、未读取或输出配置值。

## 2026-09-28 15:40 Gemini 官方专属配置鉴权成功，普通往返通过，流式受503/429阻塞

维护者更新配置后，定向命令 `go test -mod=readonly -tags live ./internal/llm -run '^TestLocalCompatibleModel$/GeminiGenerateContent$' -count=1 -timeout=150s -v` 首轮exit1（25.472s）：普通Generate文本→工具调用→结果回传三次HTTP200，名称/参数/身份/usage/每次单请求断言全部通过；流式首条文本请求HTTP503。一次独立完整复验exit1（20.540s）：普通文本200、工具请求503；流式文本200、工具请求429。遇429后停止继续请求，未增加生产重试或降低断言。官方根地址与凭据已可用；此前API_KEY_INVALID不再是当前阻塞。普通工具往返证据不能代替流式完整往返，尚未获得同一轮六次成功的Gemini完整live验收。当前阻塞为服务可用性/限流或配额，429具体原因未进一步分类；待服务恢复或额度确认后重测。原兼容网关工具回传400仍保留历史，不能据官方模型成功反推其确切根因。未修改生产代码或配置文件、未输出配置值；Steps18–21/23仍in_progress。

## 2026-09-28 15:37 官方根地址修正后，Gemini 返回 API_KEY_INVALID

维护者再次保存配置后，live测试内部确认专属配置、官方域名和根路径均匹配；普通/流式均在首条文本请求返回HTTP400。仅提取官方错误details.reason的固定分类，两路均为 `API_KEY_INVALID`，不是此前的路径404，也尚未进入工具回传。定向命令 `go test -mod=readonly -tags live ./internal/llm -run '^TestLocalCompatibleModel$/GeminiGenerateContent$' -count=1 -timeout=150s -v` exit1（0.681s；补充安全错误分类后0.633s）。`go test -mod=readonly ./internal/llm -run '^TestLiveGemini' -count=1 -timeout=120s` exit0（0.136s），验证只输出固定布尔分类且不保留私有错误正文。未改生产代码或.test_env，未认证新模型；需有效官方凭据再验。Steps18–21/23继续未完成。

## 2026-09-28 15:34 新增 Gemini 专属配置复验：官方域名已命中，基础路径待修正

维护者保存专属配置后，定向 live 测试确认不再派生原网关配置。测试内部仅输出布尔结构：专属配置存在、官方域名匹配，但基础路径非根路径；未输出配置值。普通/流式均在第一条文本请求返回 HTTP404，未进入工具调用阶段。命令 `go test -mod=readonly -tags live ./internal/llm -run '^TestLocalCompatibleModel$/GeminiGenerateContent$' -count=1 -timeout=150s -v` 两次exit1（0.764s、0.707s）。当前 SDK 使用官方服务根地址并自行追加版本及模型路由，已建议维护者修正基础路径后再测。此结果既不能证明新模型工具回传失败，也不能证明此前兼容网关400已解决。原配置文件未读取到工具上下文、未改写；新增诊断仅为live测试日志，不修改生产路由。

## 2026-09-28 Gemini 可选身份与兼容网关分片修复，回传仍待定位

已接通私有 Eino Gemini 适配器的流分片处理。锁定 genai v1.71.0 的 FunctionCall.ID 为可选字段；新响应有名称但无 ID 时分配 UUID，原供应商 ID 保真，JSON 持久化及 call/result 下一轮回传保持同一身份，历史缺身份仍拒绝。仅合并明确的“有名称空参数首片→匿名 args.arguments 字符串片段”，单调用缓存上限 1 MiB，使用完整 JSON 对象和 UseNumber 保持大整数。没有实现原生 PartialArgs/willContinue。

此前新增真实工厂红测因 invalid_argument 失败；接线后通过。迁移旧测试中“可选 ID 必须由供应商提供”的错误预期，保留不完整持久历史拒绝。新增测试覆盖孤立匿名片、冲突 ID、错键、非字符串、超限、截断、非对象、尾随 JSON、业务 arguments 字段、同名独立调用和签名；真实 HTTP/SSE 工厂额外验证无终帧/截断均拒绝且仅一次请求。

验证：Windows `go test -mod=readonly ./internal/llm/... -count=1 -timeout=120s` exit0；Windows/Linux `go test -mod=readonly -race ./internal/llm/... -count=10 -timeout=180s` 均exit0。最新稳定代码 Windows/Linux `go test -mod=readonly ./... ./sdk/testdata/consumer -count=1 -timeout=600s` 和相应 `-race ... -timeout=900s` 全部exit0；Windows gofmt无输出、vet/build/modverify/diffcheck通过。`go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` exit0：代码可达0，仍有未调用包级1/模块级1提示。此前高风险审批/恢复/核对/资源测试双平台race20串行已结束exit0（1888.147s）；该长任务早于本补丁启动，不替代当前完整回归。

正式 live 全包 `go test -mod=readonly -tags live ./internal/llm -count=1 -timeout=300s -v` exit1（33.070s）。随后五工厂定向完整复验exit1（30.350s）：OpenAIChat、OpenAIResponses、DeepSeekChat、AnthropicMessages各6次完整往返通过；Gemini普通/流式均通过文本、工具名称/精确参数/非空ID及usage断言，但第三次工具结果回传HTTP400，安全分类仅表明网关报告必需字段错误，不能确定缺哪个字段或认定供应商根因。隔离实验分别省略可选wire ID、包装response.output仍同样400，两项临时请求改写均已撤销，未进入生产。诊断已收敛为固定计数/布尔，不再记录任意参数键或凭据引用。

Steps18–21/23继续in_progress；剩余live阻塞从工具接收前移为结果回传。需要网关脱敏验证详情或可运行的Gemini工具往返请求样例才能进一步定位；不以重试、虚构结果、跳过验收或把其他协议成功冒充Gemini成功。macOS仍按批准延期。

## 2026-09-28 维护者授权从现有配置派生四协议live并发现Gemini兼容缺口

维护者明确允许根据.test_env已有信息自行完善四协议配置再测试。实现测试专用 `resolveLiveConnection`：协议专属三项齐全时优先；专属配置完全缺失时仅复用OPENAI原模型、原凭据、同一origin，调整SDK要求的路径根。任一专属字段存在但不完整则拒绝，不把其他端点的凭据补入；拒绝含userinfo/query/fragment/转义路径的URL。配置只在live测试进程内派生，不工具读取、不打印或复制密钥到文件、不改.test_env、不猜其他域名/模型。默认离线测试使用合成值验证隔离及拒绝边界。

真实工厂测试 `go test -mod=readonly -tags live ./internal/llm -run '^TestLocalCompatibleModel$' -count=1 -timeout=300s -v` exit1（28.756s）。OpenAIChat、OpenAIResponses、DeepSeekChat、AnthropicMessages各Generate/Stream完整文本→工具→答案、6次物理请求及usage断言通过；这认证现有兼容网关/原模型的这四个工厂基础往返，不认证各品牌官方模型、原生推理或缓存命中。Gemini文本连通，但工具验收失败，不能标五协议live通过。

新增仅输出结构计数的有界Gemini诊断（不输出响应正文、工具参数值、供应商错误、签名、模型或认证字段）。独立Gemini复验exit1：HTTP200普通响应含1个有name/args对象的functionCall但无ID；流式响应含10个无ID functionCall片段，仅1片有name，均为args对象，无partialArgs/willContinue，STOP存在。当前严格身份/完整调用契约拒绝，尚未确定无ID生成与匿名片段合并的安全兼容策略；未拼造调用身份、未修改生产门禁或跳过断言。此问题替代之前“Gemini缺配置”的阻塞原因。

离线llm完整普通exit0（1.597s），新配置/诊断及验收负控race20 exit0（2.186s），llm vet及diffcheck通过。双平台高风险race20另在执行中，待收结果；Step18–21仍in_progress，当前live缺口收窄为Gemini工具身份与流片段兼容性。历史四协议缺配置记录仅为此前状态。

## 2026-09-28 14:06 缓存档位按 pi 策略对齐并完成回归

按维护者确认，缓存策略统一采用 pi 的语义：`none` 禁用产品主动缓存字段；`short` 不强制写入具体时长，使用供应商短期默认；`long` 仅在能力已验证时发送协议对应的长期保留值。当前实际映射为 Claude short省略TTL、long=`1h`；OpenAI Chat/Responses short省略保留字段、long=`24h`；Gemini显式缓存仍独立opt-in，普通请求不因short/long自动创建资源。所有显式Gemini资源依赖供应商TTL到期回收，不新增主动DELETE或后台续期。

先改测试期望并运行缓存专项，旧实现按预期失败：Anthropic仍发送`ttl:5m`、OpenAI Chat/Responses仍发送`in_memory`。随后仅修改对应L1请求装饰逻辑及测试断言，未改Eino核心或增加第二套缓存管道。`go test -mod=readonly -race ./internal/llm ./internal/agent/eino ./internal/sessions -run 'Cache|TestP2OpenAIChatOptions|TestP2ModelSwitchActualRequestOptionsCacheAndInventory' -count=10 -timeout=300s` exit0（4.072s/1.230s/6.638s）。

最新完整验证串行通过：Windows普通、Windows race、Linux缓存race10、Linux普通、Linux race、漏洞扫描和live命令均exit0；普通/race全仓包含sdk消费者，live为OpenAIChat通过、其余四协议因缺配置SKIP。`go vet`、`go build`、`go mod verify`、`gofmt -l .`和`git diff HEAD --check`均通过；漏洞扫描报告代码可达漏洞0，但依赖树存在未调用漏洞提示。此项问题已记录，历史主动DELETE缺口说明改为“按维护者批准不属于P2出口”。

## 2026-09-28 pi/Zero缓存保留与自动到期核对，修正主动删除范围

维护者明确指出应按缓存档位保留、到期自动删除，要求对照pi与Zero。按dg-piagent源码兜底路径检查本地pi `5cd93f688`、Zero `99721c76`，未升级依赖或修改参考仓库。

pi的CacheRetention为none/short/long：Anthropic短档省略ttl使用服务默认、long且兼容支持时发送ttl=1h，none不注入断点（packages/ai/src/api/anthropic-messages.ts:50–74）；Responses long且能力支持时发送prompt_cache_retention=24h，short省略使用默认（openai-responses.ts:58–85）。这是请求策略，实际缓存过期由供应商管理，不是pi后台定时DELETE。所查Google GenerateContent路径未创建显式缓存、未设置其TTL，仅解析cachedContentTokenCount，不能推导pi为Gemini也实现同样分档资源管理。

Zero的Anthropic cacheControl只有type=ephemeral字段，没有ttl或统一none/short/long策略（internal/providers/anthropic/types.go:32–44）；OpenAI传prompt_cache_key，没有检出prompt_cache_retention字段；Gemini解析缓存token统计，没有检出cachedContents资源创建/删除管理。其当前provider实现同样不能作为主动远端删除的必要性证据。

Google GenerateContent官方文档明确：显式缓存TTL到期自动删除，未指定TTL默认1小时；手动DELETE是另一个可选操作。来源：https://ai.google.dev/gemini-api/docs/generate-content/caching （本次已读取，页面2026-09-11更新）。不能把其他Gemini API的隐式缓存文档混用为此资源契约，也不把TTL到期等同全部供应商的精确物理清除承诺。

修正此前将主动DELETE列为P2必须实现的过度要求：按维护者指定方向，P2使用供应商TTL自动回收，保留本地到期检查、远端404/失效后的完整回退、预算和句柄隔离，不新增删除调度或后台服务。当前Gemini仍采用供应商默认TTL，long无保证时明确降级，不擅自新增未经确认的统一时长映射。开发04、需求覆盖及原plan剩余说明同步；历史记录保留，不将未实现的DELETE写成已实现。仅文档/范围澄清，无生产代码变化；四协议live仍缺配置，18–21与23不因此直接完成。

## 2026-09-28 13:55 双平台崩溃矩阵十轮通过

Windows与Ubuntu-24.04依次执行 `GOWORK=off go test -mod=readonly ./internal/sessions -run '^TestSessionCrash$' -count=10 -timeout=900s`，完整TestSessionCrash所含故障窗口各重复10轮，串行命令exit0，总耗时37.090s。未缩减该测试的子用例；此项补齐本轮最新代码的崩溃重复验收，不替代其他并发专项或真实服务认证。

Step23仍进行中，剩余高风险竞态矩阵证据复核与外部认证条件。Gemini主动DELETE的回收策略正在向维护者说明，尚未批准延期，原plan和设计边界不擅自改变。

## 2026-09-28 13:49 最新Windows/Linux全仓普通与竞态通过

最新稳定代码串行四条门禁全部exit0，总耗时368.397s：Windows `GOWORK=off go test -mod=readonly ./... -count=1 -timeout=600s` → Windows `go test -mod=readonly -race ./... -count=1 -timeout=900s` → Ubuntu-24.04原生同两条命令。每条后均检查非零即退出，末尾exit0，不从混合编码输出猜测测试结果或包级耗时。另Linux原生 `go build ./...`、`go vet ./...` 及文档更新后的 `git diff HEAD --check` exit0。SDK的testdata消费者已另行完整普通和双平台race10认证，不依赖 `./...` 通配符隐式覆盖testdata。

当前原plan Steps1–17和Step22已完成；18–21与23保留in_progress，具体剩余为Gemini主动远端删除生命周期、四协议真实服务配置/认证，以及最终高风险/崩溃故障矩阵证据复核。macOS按已批准延期，不计通过。此次全仓绿测不抹去上述缺口，不宣布P2整体完成；没有提交或推送。

## 2026-09-28 13:47 五协议会话集成通过及最新静态/live门禁

`TestP2ProtocolFactorySessionCalculationAndDiskReplay` 双平台 `-race -count=10 -timeout=300s` 串行退出0（总体113.768s；终端混合编码，包耗时不另行推断）。五协议均真实发起HTTP，初始任务物理/逻辑模型各2次、工具1次、2个accepted attempt，产品call ID与provider call ID分离；真实工具计算结果进入下一请求，公开答案为5。Claude opaque/Gemini签名及内部来源摘要留在磁盘历史，公开快照与持久事件无该私有数据；Open零HTTP/零工具，显式第二次输入仅新增1次HTTP并保留私有回放。没有新生产协议补丁。

原plan Step22更新completed；当前公开SDK交付与兼容已由普通SDK/架构、双平台完整consumer race10、快照/核对race20及五协议会话集成验证。18–21仍in_progress，明确保留Gemini主动远端资源删除生命周期与四协议live未认证；没有把供应商TTL或本地失效冒充主动DELETE。开发04补当前缓存范围和旧私有历史无来源标记时的兼容拒绝；开发06补公开快照/核对定义；requirements-coverage补已验调用链索引及已实现shell/日志状态。

最新稳定代码静态链全部exit0：`gofmt -l .`无输出、`go vet ./...`、`go build ./...`、`go mod verify`（all modules verified）、`git diff HEAD --check`、固定 `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`。漏洞扫描可达0，仍有未调用的包级1/模块级1提示，不宣称依赖完全无漏洞。新增文件清单已核对，无exe/test产物；代码/文档等文本冲突标记、常见密钥模式检查无匹配；`.test_env`受Git忽略，未读取值。

`GOWORK=off go test -mod=readonly -tags live ./internal/llm -count=1 -timeout=300s -v` exit0（8.401s）：OpenAIChat Generate/Stream共6个真实请求通过；OpenAIResponses/DeepSeek/Anthropic/Gemini分别缺配置SKIP，不记认证通过。Windows/Linux最新全仓普通/race链仍在执行，结果另记，不用之前全仓证据代替。

## 2026-09-28 13:42 Steps16–17完成与SDK双平台专项收口

原plan的 `p2-builtins-selection` 已更新completed：文件发现/精确编辑/TODO/search_tools及Step17逐项选择契约已有独立功能和双平台race20证据，本轮相关完整包继续通过。Windows tools/eino/sessions/state/consumer普通exit0（0.765/0.625/19.851/0.300/2.485s）；Linux同组exit0（0.998/0.572/18.744/0.196/3.206s）。最终全仓门禁由Step23跟踪，不再让其他步骤未完成无限阻塞16–17状态。

SDK/consumer/architecture普通exit0（5.936/2.593/0.305s）；完整consumer race10 Windows128.500s、Linux121.855s均exit0。Linux快照/核对专项race20两包exit0（1.563s/24.909s）。首次WSL bash嵌套引号使正则管道被错误拆分exit1，改直接--exec参数后专项通过；后续第二次WSL启动出现E_UNEXPECTED（exit4294967295），不计测试失败或PASS，未重启WSL，重试后上述完整包通过。SDK剩余前置为五协议真实Session集成验收；本步无新增SDK功能缺口。

五协议续派任务实际因执行服务额度中断且未创建集成文件，状态交接确认无写入/运行任务。主线程接手新增 `internal/sessions/protocol_factory_integration_test.go`，五实际HTTP工厂→Session→Executor→calculate(2,3)→答案5，再Close/Open及显式新输入回放。初始Claude/Gemini fixture误用匿名/fixture provider被请求前拒绝，修正为显式合成凭据与正确provider（不改生产门禁），普通exit0（0.876s）；双平台race10运行中。此处属于测试夹具修正，不虚构为生产缺陷修复。

## 2026-09-28 13:31 SDK公开核对闭环与快照竞态复验

主线程新增 `sdk/testdata/consumer/reconcile_pipeline_test.go`：从真实模型工具执行错误生成unknown，通过公开Snapshot取得call/observation身份与版本，调用Reconcile→GetOperation→Snapshot。断言陈旧revision返回state_conflict、零查询/零提交；受信只读查询一次、新观察version2链接version1、原call/原观察不被覆盖；重复幂等请求不再次查询；关闭再Open从磁盘恢复两个版本和completed operation，不自动执行工具/模型。未伪造内部manager状态，也没有增加另一条核对执行路径。

普通定向 `go test -mod=readonly ./sdk/testdata/consumer -run '^TestSDKConsumerReconcileUnknownThroughSnapshot$' -count=1 -timeout=120s` exit0（0.237s）。Windows快照/核对专项 `go test -mod=readonly -race ./internal/sessions ./sdk/testdata/consumer -run 'TestSnapshotAttemptAndObservationProjection|TestSDKConsumerSnapshot|TestSDKConsumerReconcile' -count=20 -timeout=300s` 两包exit0（1.614s/20.104s）。相关完整包、Linux同组复验仍在运行；此处不以专项绿测宣称Step22或全仓已完成。消费者文件本身仅引用标准库和sdk，复用同包已有注入模型fixture；真实HTTP工厂审批链由独立factory_approval_test覆盖。

## 2026-09-28 13:21 Step17功能验收交接及快照红测最小修复

Step17逐项交接确认默认模型下一Trace、下一Turn原子激活、隐藏工具拒绝、superseded/幂等、原审批快照恢复、pending磁盘恢复、撤销前复核和真实请求有效选项/缓存隔离均有证据；双平台相关race20通过（sessions658.742s/685.931s）。无原请求任意重绑不属于现合同缺口，缺原实例按incompatible_resume拒绝，合法精确请求重放绑定已有验证；P4/P5能力不前移。仅新增selection_boundary_contract_test.go，无生产修复。合并16–17待办待最新集成门禁后关闭，不将Step22编译红测当Step17新缺陷。

主线程接手SDK中断后的实现：新增ModelAttemptView/ObservationView及单文件SDK别名；同一View投影attempt合并终态，观察仅公开ID/version/关联及状态/效果/退出等事实，不公开原正文/后端错误/内部证据引用。加入Snapshot字段及构造接线，修复尚未实现字段导致的编译阻断。此前默认consumer红测已独立复现，现 `GOWORK=off go test -mod=readonly ./internal/sessions ./sdk/testdata/consumer -run 'TestSnapshotAttemptAndObservationProjection|TestSDKConsumerSnapshotIncludesAttemptAndObservationViews' -count=1 -timeout=120s` exit0（0.147s/0.237s）。这仅为最小红绿，真实Reconcile消费者闭环、扩大race和最终门禁仍待补，不宣布Step22完成。

## 2026-09-28 13:08 五协议独立验证通过，SDK修复执行中断

主线程 `GOWORK=off go test -mod=readonly -race ./internal/llm -run P2ProtocolContract -count=10 -timeout=180s` exit0（4.001s），随后agent/eino完整普通exit0（0.746s）。重复的协议交接通知不视为新一轮证据；真实Session集成另行继续。

SDK快照修复执行服务因账户额度不足返回403并中断，属于开发执行环境问题，不是产品模型工厂测试失败；未重试该服务。主线程核对快照字段仍未加入，并独立执行 `go test -mod=readonly ./sdk/testdata/consumer -run '^TestSDKConsumerSnapshotIncludesAttemptAndObservationViews$' -count=1 -timeout=120s` exit1（0.257s），仍为attempts0/observations0。主线程接手此最小快照投影修复，保留Step22进行中，不以任务退出替代验收或删除红测。

## 2026-09-28 13:06 五协议统一契约与私有回放门禁交接

交接五实际Catalog工厂Generate/Stream统一calculate工具→答案、单/双工具、缺usage及取消矩阵，双平台llm普通/race和矩阵race10通过。默认红测修复跨模型私有签名静默接受、Claude正向思考映射none仍发送两项缺陷。私有回放来源摘要绑定provider/protocol/model/configVersion/endpoint/accountScope，输出附幂等Extra键，输入不兼容或无来源时请求前unsupported_capability且HTTP0；普通文本/无签名公开推理不受限制。这是兼容来源标记，不是权限证明。

旧历史含私有签名但无来源标记将明确拒绝，不自动迁移；需在后续文档收尾明确该兼容边界。主线程已读replay_compatibility.go，并启动独立P2ProtocolContract race10后agent/eino完整普通测试，结果待收。交接计算仍位于L1测试，不能替代实际Executor执行，因此续派仅新增sessions五工厂真实HTTP→Session→受控工具→第二轮模型集成，验证持久身份/预算/私有标记保存与公开投影隔离，不修改其他任务正在处理的快照及selection生产区域。

协议整组仍in_progress，待上述全链和最终稳定代码门禁完成再关闭；四协议真实凭据缺失继续未认证。

## 2026-09-28 13:04 SDK消费者验收发现公开快照真实缺口

新增消费者测试通过实际工厂审批/关闭重开/显式恢复、事件提交顺序与溢出、消息脱敏副本和用户shell输出限额；交接双平台相应专项race10通过。但完整consumer普通/race10 exit1：真实模型2次/工具1次后公开Snapshot的ModelAttempts为0、Observations为0，缺原Step22要求的请求尝试终态及Reconcile所需观察ID/version，保留默认红测不跳过。主线程已读红测与state类型，不能用之前完整套件绿测宣称现公开查询链完整。

已确认按原Step22扩展最小快照投影及必要sdk别名：同一已提交View构造隔离map，尝试视图合并AttemptResults终态而非直接暴露初始started；观察提供核对所需身份/版本及安全内容，排除内部checkpoint/target/私有provider记录。续派先使红测转绿，再补实际Reconcile→GetOperation→Snapshot的只读取证、版本追加、CAS及不重跑断言。无新增公开cursor订阅API；按cursor网络重连属于后续入口能力，不顺带扩展。

Step22保持in_progress。完整全仓检查等当前修改稳定再统一执行；此轮未提交推送、不读取凭据。

## 2026-09-28 12:49 原计划逐项收尾继续

重新读取Steps17–22原验收定义，继续三个互不重叠工作范围：选择/恢复对照验收（sessions）；五实际工厂同一计算工具任务及私有签名跨协议拒绝契约（llm）；SDK公开别名、快照与真实消费者验收（sdk/tests）。先核对原需求是否真实未交付，再默认红测及最小修复，不将P4 skill/P5子Agent的完整能力前移为P2新功能。主线程保持负责集成、文档及最终门禁。

原待办ID保持，Step22从pending更新in_progress；16–17搜索已验但模型选择整体验收仍在收尾；18–21说明更新为Gemini显式缓存最小路径已验、统一契约验收中。四协议真实凭据缺失仍记未认证，用户尚未确认延期，不擅自记通过。当前不再在文件持续编辑时启动最终全仓测试，待交接稳定后串行运行。

## 2026-09-28 12:39 Gemini显式缓存最小路径交接

已交接明确opt-in的system/tools/toolConfig前缀缓存，conversation覆盖数0、完整对话作为后缀；共享PrefixCacheProjection与创建转换，无公共TTL配置，供应商默认有效期、合法名称/ExpireTime校验、工厂闭包作用域registry、同键在途去重、过期清理，以及预算约束内完整请求回退。流已输出后不回放；创建者取消不传播为独立等待者的取消。交接模型层Windows/Linux普通/race及架构检查通过，尚未视为整个Step20完成。

主线程已读gemini_cache.go实际投影摘要、身份隔离、registry等待及异常回退路径；将128条工程上限移到internal/config.GeminiCacheRegistryEntries，模型层仅保留常量引用，语义不变。独立执行 `GOWORK=off go test -mod=readonly -race ./internal/llm/... -run 'Gemini.*Cache|PrefixCache' -count=10 -timeout=180s` exit0（llm 2.870s），随后架构测试exit0（0.248s）。两个私有适配器包在该筛选下均no tests to run，不将其退出0记为私有包专项覆盖；通过证据为产品llm中的缓存测试和架构检查。

明确剩余边界：无远端DELETE、后台续期或主动提前清理，远端资源依赖供应商TTL；不缓存conversation前缀。设计中的资源删除要求仍需核对落实或维护者明确缩减，不能把本版最小路径称完整生命周期。真实Gemini端点未配置，最小缓存长度、命中及费用未认证；需最终全仓/live及文档更新，不关闭协议待办。

## 2026-09-28 12:33 Eino普通搜索持久选择闭环交接

search_tools已复用Eino toolsearch.NewTyped生成的匹配工具，通过现有mailbox/acceptActiveTools提交pending，下一Turn原子激活；不安装上游按历史自动开放的中间件。交接真实Session测试目标执行1次、同批抢跑0次；重开保留原Turn与pending，不开放额外工具；声明级策略与后端可用性检查不替代实际参数授权。报告Windows/Linux专项普通及race20通过，无新增公开字段。

主线程已读search_tools.go实际匹配、产品callID绑定、pending合并及既有selection提交路径，独立执行 `GOWORK=off go test -mod=readonly -race ./internal/sessions ./internal/agent/eino ./sdk/testdata/consumer -run 'P2SelectionSearchTools|SDKConsumerSearchTools' -count=20 -timeout=300s` exit0（sessions80.362s、eino1.235s、consumer26.224s）。工具搜索选择及恢复专项二十轮竞态通过，不替代模型选择恢复整体验收。原计划既有16–17待办说明已更新为实现交接完成、独立及整体验收中，status仍in_progress。模型重绑/跨配置与checkpoint整体验收仍需核对；skill索引和父子invocation完整能力应按既定P4/P5边界核实，不以本次普通搜索测试证明，也不擅自前移实现。

## 2026-09-28 12:18 真实工厂 live 验收交接与主线程复验

live_test.go已改走Catalog→RegisterFactory→Bind→预算观察→Generate/Stream；新增默认离线live_acceptance_test.go断言空/错误文本、缺结束标记、错误工具参数不能通过，并验证工具结果调用身份。主线程已读live入口并独立执行 `GOWORK=off go test -mod=readonly -tags live ./internal/llm -count=1 -timeout=300s -v` exit0（6.363s）。交接真实OpenAIChat两路径各完成pong文本、一次无副作用lookup、工具结果后pong，总6次物理请求，usage与SDK数值一致。

配置可验证范围仅OpenAIChat；OpenAIResponses/DeepSeekChat/AnthropicMessages/GeminiGenerateContent均因缺各自MODEL/BASE_URL/API_KEY配置跳过，不能记五工厂live完成。未读取或输出配置值、未写凭据文件、未提升Capability为Verified。后四协议需要维护者本地补配置或明确延期验收；其他离线实现继续推进，原Step23保持进行中。

## 2026-09-28 12:17 Gemini 隐式缓存门禁修复及显式资源继续实施

已交接options.go最小修复：Gemini未ExplicitCacheResource时不因short/long能力声明启用ActiveCache，保持完整输入的默认隐式路径，原因gemini_explicit_cache_not_requested。新增真实HTTP两轮Generate/Stream测试。主线程独立 `GOWORK=off go test -mod=readonly -race ./internal/llm -run 'TestGeminiImplicit|TestGeminiCache|TestResolveOptions' -count=10 -timeout=180s` exit0（1.551s），其中实际新测试名TestGeminiCacheDefaultRemainsImplicit被命中。此时显式资源仍未交付，Step20不关闭。

主线程确认私有适配器允许增加纯投影helper，创建与实际payload摘要共享现有转换逻辑，更新私有PATCH_NOTES，不新增公开SDK字段。最小TTL策略为供应商默认（不发送臆造固定TTL），按有效返回ExpireTime管理复用；缺失有效句柄/到期信息不得当作可靠可复用资源。无长保留配置时long降级short并明确记录原因，不冒称长TTL保证；none禁止主动创建。继续闭包registry、严格前缀/后缀等价、作用域隔离、辅助请求预算及完整回退的红测和实现，待最终接线验证。

## 2026-09-28 12:14 Linux全仓运行遇到新增功能实施中间态

WSL全仓串行命令exit1，普通测试未通过，后续`&&`连接的race未执行。输出含新增gemini_cache_test.go调用不存在的schema.AssistantAgenticMessage导致llm编译失败，以及新增TestP2SelectionSearchToolsNextTurn等待模型请求超时（10.02s）。两处位于仍在实施的Gemini缓存和工具搜索工作范围，不能将本次当稳定代码Linux最终验收，亦不将Windows此前通过扩展到新代码。输出存在编码显示异常，已核对实际新增测试文件；不推断其他未可核实结论。

主线程暂不并发修改这两个正在交付的范围，等待各自完成红绿与交接后再串行跑Linux全仓普通/race；不通过重跑覆盖本次失败记录。后续全仓验收应在生产改动与测试均稳定后启动，避免再以编辑中工作树采集最终门禁。

## 2026-09-28 12:09 继续剩余功能并同步原计划待办

已更新原计划frontmatter中既有待办说明（不改计划正文、不新建重复TODO）：14–15保持completed；16–17明确Step16文件工具/TODO/用户shell已实现且双平台专项、Windows全仓回归通过，Step17工具搜索闭环仍实施中；18–21列明Responses/Claude/DeepSeek已验子项和Gemini显式缓存/统一契约缺口；Step23改in_progress，保留Linux最终全仓/live等待。

继续委派普通search_tools固定generation候选→下一Turn选择的真实闭环，以及Gemini opt-in显式缓存生命周期，分别独占sessions/agent与llm Gemini区域；不放宽当轮可见清单、权限或物理请求预算。主线程启动WSL Linux全仓普通后串行race，结果待收。当前主线程 `GOWORK=off go test -mod=readonly -tags live ./internal/llm -count=1 -timeout=300s` exit0（2.703s），但源码核对现有TestLocalCompatibleModel仅直接HTTP状态检查，未走Catalog、未断言回答，不可作为五工厂真实认证。已安排补真实工厂live验收，缺凭据协议明确skip，不查看或输出.test_env内容，不以已有绿色结果掩盖验收不足。

## 2026-09-28 12:07 最新静态、依赖与固定漏洞门禁通过

主线程串行检查exit0：`gofmt -l .`无输出；`GOWORK=off go vet ./...`、`go build ./...`通过；`go mod verify`输出all modules verified；`git diff HEAD --check`通过，仅LF/CRLF转换提示；`go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`可达符号漏洞0，仍提示当前代码未调用的导入包级1项、依赖模块级1项漏洞。以上为增量事件优化后的最新工作树验证，不将未调用提示隐去，也不以diff检查替代新文件卫生检查。live、Linux最终全仓及未实现功能仍待完成；未提交推送。

## 2026-09-28 12:05 主线程串行专项与Windows全仓普通/race通过

主线程同一串行命令全部exit0（总412.295s）：`GOWORK=off go test -mod=readonly -race ./internal/sessions ./internal/sessions/state -run 'Activity|Reservation|TestFileDiscovery|CommittedEvent|Subscription' -count=20 -timeout=300s`（sessions250.767s/state2.004s）；随后 `go test -mod=readonly ./... -count=1 -timeout=300s`（sessions20.876s）；再 `go test -mod=readonly -race ./... -count=1 -timeout=300s`（sessions115.462s）。本轮包含增量事件读取、TODO、shell和活动正常结束竞态修复后的集成代码，替代此前这些路径的待收集状态。

这证明当前Windows完整套件通过，不消除历史真实租约迟到的所有可能因素，不替代Linux全仓、最新静态/漏洞/live及未实现工具搜索和协议缓存能力的验收。下一步刷新剩余验证及功能状态，不能用全仓绿测关闭整个P2。

## 2026-09-28 11:57 增量事件发布优化交接，主线程串行复验启动

已交付Manager.EventsAfter：同锁按durable序号二分定位已提交后缀、仅深拷贝事件并返回已提交cursor；publishCommitted仅替换读取来源，保留发布/推进逻辑。主线程已读方法和实际调用点。测试覆盖空/重复/超前cursor、重开、Payload及序号/Scope隔离、无订阅推进、新订阅不补旧历史、顺序/去重/慢订阅溢出及Append失败不发布。

默认分配红绿证据：空邮箱处理在1/256条历史时从73/847次分配降到3/3次。相同128页历史race基准Windows约523.50ms→5.87µs，Linux563.42ms→13.13µs；这些为测量样本，不是延迟保证。真实278KB流程模型2次/Search1次不变。交接双平台事件/活动/文件发现普通及race20、sessions/state全包普通/race、SDK文件发现race20通过。

主线程开始串行独立 `GOWORK=off go test -mod=readonly -race ./internal/sessions ./internal/sessions/state -run 'Activity|Reservation|TestFileDiscovery|CommittedEvent|Subscription' -count=20 -timeout=300s`，成功后执行全仓普通及全仓race，结果待收，不提前记通过。保持提交候选全量复制、其他View读取、租约和预算规则；显式View及提交前仍有约23–27ms样本成本，大历史/存储/调度仍可能真实迟到，不把历史11.4ms实例宣称已完全根治。

## 2026-09-28 11:30 续租迟到分段证据与增量事件读取优化启动

性能调查已证明全量JSON复制和View持锁竞争有显著成本，但未完全归因原始11.4ms迟到。原记录触发至处理采样283.7643ms包含调度/邮箱/采样前检查，采样至Append返回225.728ms包含候选复制等，不可统称排队或存储写入。新增真实会话诊断保持模型2次/Search1次；轮询View显著放大等待。约6.57MB合成历史race续租Windows505.57ms/Linux600.81ms，并行View读时约1.222s/1.436s；候选复制为主要测得成本，不能把合成基准冒充原实例计时。

主线程核实publishCommitted每次均调用Manager.View复制整个会话，即使无订阅者。已确认最小内部优化范围并继续实施：state提供只深拷贝已提交事件后缀的游标读取，publishCommitted使用该快照；保留顺序、cursor、新订阅行为、隔离及失败Append不发布。先默认行为测试和同规模前后基准，再双平台普通/race；不改Manager.commit候选原子性、不改租约或预算、不新增SDK API。全量commit复制及外部View轮询仍是剩余成本，不预先宣称真实迟到全部解决。

本轮调查仅新增activity_latency_diagnostics_test.go与state/activity_latency_benchmark_test.go，交接双平台诊断/基准/vet退出0；优化尚未交付。WSL本轮已恢复可运行，未重启。原计划与未完成状态保持，不以测量完成替代产品修复完成。

## 2026-09-28 11:12 活动续租与正常结束竞态修复交接

已确定性复现正常结束在续租closed/epoch检查后、独立allowed检查前关闭有效租约，导致尚未到期也报budget_exhausted。修复activity.go同一临界区检查closed/epoch/截止，并在提交前遇closed直接退出；未扩大预算或跨过旧截止继续授权。交接红测固定500ms正常结束、1s旧截止，修复前exit1，修复后双平台race20 exit0，Settled500ms/Reserved0/Revision2。主线程已读实际锁与提交路径，独立执行 `GOWORK=off go test -mod=readonly -race ./internal/sessions ./internal/sessions/state -run 'Activity|Reservation|TestFileDiscovery' -count=20 -timeout=300s` exit0（sessions 205.471s、state 1.747s）。确认该范围二十轮竞态复验通过，不替代真实续租迟到调查或最终全仓验证。

交接双平台活动/文件发现普通及race20、完整sessions普通/race、SDK文件发现race20通过，原主线程失败命令重跑也通过；这些不能覆盖另一类真实迟到。真实记录显示旧截止1.6221973s，续租触发1.1241206s、mailbox采样1.4078849s、追加返回1.6336129s，已晚约11.4ms，当前安全规则必须取消。没有证据指向UTC丢单调时间；正常结束竞态不是原始所有失败的唯一已证根因。

已续派单独性能因果调查，分段测量mailbox排队、视图复制、提交与Append以及诊断开销，不放宽期限、不以重跑绿代替解决。崩溃计数不足仍保留待自然失败诊断的历史限制；整组步骤及最终验收暂不关闭。

## 2026-09-28 11:02 崩溃窗口计数失败诊断交接（根因未定）

已确认原trace.settled_after模型1次/工具0次计数在父进程Kill和重开之前采集，不能解释成重开后丢失；trace.settled窗口也包含执行失败终态。可控租约过期实验能产生相同1/0及activity reservation expired，但原自然失败未保存Trace.Error/租约元数据，不能认定同根因。仅在recovery_test.go原计数失败分支补执行元数据诊断，保留2/1断言，不修改活动或shell实现。主线程已检查新增诊断范围。

交接Windows目标窗口普通/race各20轮exit0，最终诊断改动后普通1轮exit0；这是重跑证据，不表示原故障已根治。Linux本轮未运行：WSL返回Wsl/Service/E_UNEXPECTED，独立wsl true探测exit4294967295，未重启服务以免影响并行活动诊断。后续需收集新增自然失败元数据，并在WSL恢复后补验证；不把此前Linux其他范围通过替代本轮。

shell补查尚未证实的边界：管道io.Copy若返回非ErrClosed读取错误，OutputIncomplete未置true；与进程错误并存时读取错误可能被遮蔽。当前没有受支持实际路径红测，保留未验证事项，不为假设增加接口或宣称修复。活动租约诊断仍负责活动代码，避免重复修改。

## 2026-09-28 10:53 shell 输出证据修复交接及 TODO/安全独立结果

shell交接已用真实辅助进程TCP握手红测证明：输出管道被WaitDelay关闭时，Cmd.Wait可能优先返回ExitError或取消错误，原先仅检查ErrWaitDelay会漏报OutputIncomplete；原100ms启动时间假设在负载下也不可靠。修复collectHostShellOutput显式管理合并管道，分别记录EOF/强制关闭与进程Wait事实，保留真实退出码、超时/取消，不宣称后代全部终止。交接Windows/Linux shell普通及race各20轮通过，主线程已读实现，独立Windows `GOWORK=off go test -mod=readonly -race ./internal/sessions -run '^TestP2CommandShell' -count=20 -timeout=240s` exit0（66.576s），确认shell专项二十轮竞态复验通过，不替代仍待完成的全仓门禁。

交接完整sessions仍有失败：新活动续租测试等待超时、mailbox停在activityRenewalContextGate.Err，以及TestSessionCrash/trace.settled_after模型/工具次数少于预期；不按较早全仓绿测关闭这些问题。活动路径由原诊断继续处理，另续派崩溃窗口因果取证，避免并行修改同一活动实现或放宽调用次数断言。

主线程TODO四包独立race10 exit0：tools1.260s、sessions55.544s、state1.555s、consumer30.489s。固定 `GOWORK=off go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` exit0：可达符号漏洞0；另报告导入包级1项、依赖模块级1项当前代码未调用的漏洞，不表述为依赖完全无漏洞。最终稳定代码全仓门禁/live及未解故障仍待完成。

## 2026-09-28 10:51 TODO 持久化交接与独立验收启动

默认sessionTodos仅在未注入Todos时装配，宿主注入原样保留、不双写；按invocation覆盖最新列表、按callID保存不可变回执。主线程已阅读sessions/todos.go与state/todos.go：mailbox外消费票据，mailbox内复核策略/取消/claim，现有Manager.commit追加todo_update后发布；同一持久事实重建成功观察及原tool.finished，避免效果提交与结果记录之间崩溃导致重跑。恢复指纹区分session-journal-v1和host-injected，未新增公开Snapshot字段。invokeTodos保留产品错误码，明确拒绝与Append效果未知分离，取消本身不证明无副作用。

交接报告Windows全仓普通/race及Linux四个受影响包普通/race通过，另保留此前Pause等待超时失败，不据随后绿测认定其根因已解决；活动租约和shell专项调查仍在运行。主线程已启动四包 `GOWORK=off go test -mod=readonly -race ./internal/agent/tools ./internal/sessions ./internal/sessions/state ./sdk/testdata/consumer -run Todos -count=10 -timeout=240s` 独立验收，结果待收。交接使用govulncheck@latest，不能替代既定固定版本证据；主线程另启动 `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`。不据交接关闭Step16或整个P2，live及最终稳定代码全仓门禁仍待完成。

## 2026-09-28 10:42 文件发现独立竞态复验失败：活动租约待诊断

主线程 `GOWORK=off go test -mod=readonly -race ./internal/sessions ./sdk/testdata/consumer -run 'TestFileDiscovery|TestSDKConsumerFileDiscoveryThroughSession' -count=10 -timeout=180s` exit1。sessions 95.254s，`TestFileDiscoverySessionGrepModesAndLimits/content` 在4.32s时失败：trace错误含context canceled与budget_exhausted: activity reservation expired，模型检查observed=1/model=2/problem=nil，配置ActivityBudget30m、Activity.Settled约3.432s；公开consumer包19.571s通过。不能把本轮主线程验收记为通过，也不能仅凭输出归因为文件发现逻辑错误或既往审批时钟缺陷。

已续派先检查activity.go租约续租/到期epoch、真实时间与单调时间、mailbox调度证据，再确定性红测及最小修复；不扩大预算、不加sleep、不削弱断言。交接中的两平台通过记录作为此前执行事实保留，不覆盖本次失败；Step16仍未关闭。

## 2026-09-28 10:40 文件发现 Session/SDK 端到端交接

新增 `internal/sessions/file_discovery_session_test.go` 与 `sdk/testdata/consumer/file_discovery_session_test.go`，未修改生产实现。交接报告真实Session多轮ls→glob→grep由上一轮结果生成下一轮参数，模型4次/List1次/Search2次；grep模式、offset、命中/字符/字节限额由下一轮模型核验；权限拒绝三工具各后端0次。外部consumer仅导入公开sdk及Eino模型类型（并非仅stdlib+sdk），模型3次/List1次/Search1次。50KiB断言针对结构化文件发现JSON正文，不宣称既有通用输出包装后的总字节数也在此限额内。

两平台普通及race10交接通过。主线程已阅读真实模型包装及公开后端接线，独立启动 `GOWORK=off go test -mod=readonly -race ./internal/sessions ./sdk/testdata/consumer -run 'TestFileDiscovery|TestSDKConsumerFileDiscoveryThroughSession' -count=10 -timeout=180s`，结果待收集，不先记主线程通过。

上游路径限制取得运行证据：固定Eino v0.9.21内存后端写入root/a.go、root/sub/b.go后，Windows路径清理为反斜杠而前缀检查拼接正斜杠，ls/glob/grep均0条；Linux均2条；精确Write→Edit→Read两平台成功。探针在上述默认测试中，不以静态推测作为复用限制。该证据支持只复用其精确编辑、而文件发现另用已有doublestar/标准库实现的选择；不扩大为对其他Eino后端的判断。Step16仍等待TODO持久化及shell失败修复和集成门禁，不关闭整组。

## 2026-09-28 10:28 文件工具/DeepSeek交接及全仓门禁结果

文件工具子项交接：公开精确编辑old_string/new_string/replace_all；List新增Limit、Search新增模式与简单截取字段；目录500/glob1000/grep100条及50KiB完整JSON，grep每行500个Unicode字符，稳定排序并提示截断。测试后端精确编辑复用Eino内存后端，glob复用已依赖doublestar、grep标准库regexp；上游路径层Windows分隔符问题为未直接复用的理由，主线程还需审查证据。交接报告Windows/Linux tools/testkit普通/race及SDK契约专项通过；尚缺新增Session文件发现端到端验收，不能关闭整个Step16。

DeepSeek子项交接：固定SDK通过ACL WithExtraFields发送原生thinking.type enabled/disabled，未指定不发送，不使用reasoning_effort替代；非法映射/独立token预算拒绝零HTTP。报告两平台定向普通/race通过；其较早完整llm失败发生于主线程Claude红绿过程中，主线程最新两次全仓运行的llm包已通过，不保留为当前Claude阻塞。仍需真实端点认证。主线程已审查deepseek.go实际ACL选项接线并独立执行 `GOWORK=off go test -mod=readonly -race ./internal/llm -run TestP2DeepSeek -count=10 -timeout=180s`，exit0（1.534s）。

最新Windows `GOWORK=off go test -mod=readonly ./... -count=1 -timeout=300s` exit1，仅 `TestP2CommandShellInheritedOutputCannotOutliveTimeout` 在commands_shell_wait_test.go:23失败（未报告继承输出截断，0.66s）；sessions包49.145s。命令使用成功后才运行race的串联，因此全仓race未执行，不记通过。已单独安排先调查实际Windows进程/Wait/输出事实，再最小修复，禁止凭重跑或放宽断言消除失败。

`GOWORK=off go vet ./...`、`go build ./...`、`go mod verify`、`gofmt -l .` 串联exit0，依赖输出all modules verified，格式无输出。TODO持久化首次交接仅调查未改代码：主线程已确认nil时装配Session持久后端、非nil完全保留宿主归属不双写；暂不新增公开Snapshot字段，内部journal与重开验收；后端切换纳入恢复兼容性。继续实施，不重复等待维护者确认已可由最小方案决定的接线。

## 2026-09-28 10:26 新编辑契约的 Session 集成回归

主线程Windows全仓普通 `GOWORK=off go test -mod=readonly ./... -count=1 -timeout=300s` exit1（sessions 62.824s）：`TestTestkitTicketSessionLifecycle` 的 edit夹具仍提交patchRef，未到后端；取消夹具仍把原始context.Canceled预期为failed，而当前文件错误投影已保留cancelled。读取新schema及既有取消测试确认后，迁移该Session夹具为old_string/new_string，分别断言取消为cancelled、非取消拒绝为failed，原票据、零写入、预算、持久事实和重放拒绝断言全部保留。

专项 `go test -mod=readonly -race ./internal/sessions -run '^TestTestkitTicketSessionLifecycle$' -count=10 -timeout=180s` exit0（16.983s）。另Responses执行接线增加外部旧cache scope覆盖断言后，专项race10 exit0（1.208s）；`git diff HEAD --check` exit0，仅现有CRLF转换警告。全仓普通/race及vet/build/mod verify/格式检查已重新启动，不能沿用此前全仓通过结论；其他并行实现尚未全部交接。

## 2026-09-28 10:20 Responses 可信缓存作用域接线（验证中）

按模型设计的 L3 可信 Session 作用域要求，先复现主动缓存在无作用域时仍发送1次HTTP的红测，再补内部 WithSessionCacheScope 与结构化身份元组SHA-256路由键，包含 Session/provider/protocol/endpoint/account/model/config/policy，不含凭据，不随 turn/attempt 改变。仅在已认证 ActiveCache 时注入上游 WithResponsesPromptCacheKey，仍完整 input、store=false、EnableAutoCache=false；none 不注入缓存键/保留参数。无可信作用域时请求前 invalid_argument、HTTP与观察均0。

`agent/eino/validated_model.go` 从实际执行 ScopeFromContext 的 SessionID 注入，覆盖而不信任外部遗留 context 值。真实 ValidatedModel→Catalog→Responses HTTP 红绿测试验证同Session跨Turn同键、跨Session异键、每次占额1。另有Generate/Stream原生请求测试及provider/endpoint/account/model/config/policy隔离测试；夹具曾误将短保留值写作in-memory，已按固定SDK实际wire值in_memory纠正，不将此夹具失败记产品缺陷。双平台专项race10已通过：Windows `GOWORK=off go test -mod=readonly -race ./internal/llm ./internal/agent/eino -run 'TestResponses.*Cache|TestSessionPromptCache|TestAnthropic' -count=10 -timeout=180s` exit0（llm 2.099s、eino 1.234s）；WSL Ubuntu-24.04同参数原生执行exit0，输出编码异常。Windows全仓普通复验仍在运行；尚不关闭Step18整个验收。

## 2026-09-28 10:14 Claude 显式缓存首轮红绿证据

新增 `TestAnthropicExplicitCacheBoundaries` 经真实工厂 Generate/Stream 捕获请求体，先复现 none 仍发送5/6处调用方旧标记、short/long同时出现顶层自动缓存的问题（行为红测exit1；此前夹具类型编译失败及并行DeepSeek文件未完成导致的编译失败不作为行为证据）。修复采用私有适配器公开 SetContentBlockCacheControl/SetToolInfoCacheControl，复制消息/块/工具/Extra后清旧标记，再在前导system末合法文本块、末工具、末user合法块布置统一TTL的最多三个断点；关闭顶层自动缓存，none清除主动标记，tool_result保持原调用关联。原始历史和工具定义不变，每次HTTP和计数观察均1。

主线程验证：`GOWORK=off go test -mod=readonly ./internal/llm -run '^TestAnthropic' -count=1 -timeout=120s` exit0；Windows `go test -mod=readonly -race ./internal/llm -run '^TestAnthropic|^TestP2FactoryClaude' -count=10 -timeout=180s` exit0（2.131s）；WSL Ubuntu-24.04 原生 `env GOWORK=off go test -mod=readonly -race ./internal/llm -run TestAnthropic -count=10 -timeout=180s` exit0（输出编码异常，不从乱码推断耗时）。这些是定向证据，尚未执行最终全仓门禁及live，不据此关闭整个协议组。

## 2026-09-28 10:05 工具复用与有界输出方案调整（已批准，实施中）

用户确认优先复用当前 Eino v0.9.21 能力，完善剩余事项。此前把本项目尚未接入的能力笼统描述为框架缺失，并将 ls 复杂分页作为基础完成条件，混淆了上游事实与产品设计；本次明确纠正。Pi 的 ls/find/grep 采用数量及字节限额和截断提示，Zero 的目录/搜索工具也不提供目录游标分页；Eino ls 直接返回后端路径，内存后端排序，glob 工具排序，grep 在取得匹配后排序并应用 offset/head_limit，不能据此宣称后端按页扫描。

当前批准的差异：ls/glob 采用稳定排序、数量及字节上限、明确截断原因和缩小查询提示，不实现复杂游标/目录快照分页；grep 优先复用 Eino 模式及简单结果截取。

### Step 19.1：Claude 显式缓存断点接线（本轮执行细化）

- 前提及输入：现有 `anthropic.go` 仅配置顶层自动 CacheControl；私有 `agenticclaude.SetContentBlockCacheControl/SetToolInfoCacheControl` 已支持请求副本标记。沿用已解析的 ActiveCache/CacheIntent 及能力门禁，不新增公开选项。
- 方法与顺序：先以真实工厂 HTTP 请求红测覆盖 Generate/Stream；在模型适配器调用前复制消息、块、工具及需要修改的 Extra，清除旧缓存标记，只在启用策略时标记前导 system 的末个合法文本块、末个工具、末个 user 的合法内容块（包含 tool_result）。全部采用已验证 short=5m/long=1h，同次最多三个断点，不混用顶层自动缓存，不改变历史、私有签名或工具顺序。
- 失败及约束：none 或未认证主动缓存策略不保留调用方旧主动标记；不以缓存作为授权，不额外重试/发请求。无合法块不伪造块；无效内容沿既有验证拒绝。修改范围仅 Claude 产品适配器、独立 helper 和测试，不修改框架协议转换。
- 交付及验收：默认测试断言断点位置/数量/TTL、关闭策略、合并工具结果、原对象未修改和 HTTP/预算观察均一次；受影响模型包 Windows/Linux 普通及 race 后，再做全仓门禁。未通过前不关闭 Step19。

read 的版本绑定行/字节续读保持不变。公开精确编辑参数及 replace_all 复用上游语义，仍经过本项目受控端口、版本校验和执行记录。write_todos 必须接入现有任务调用范围与 journal，Eino AddSessionValue 本身不作为本项目落盘证据。原计划正文保留，以本节及现行工具设计记录批准差异；未完成项仍保持进行中，不因缩减分页要求直接标完成。

执行顺序与验收：先以默认测试复现文件工具契约/有界输出缺口，再接通受控后端并验证错误码、权限拒绝零调用及真实效果；TODO 通过既有 mailbox/state commit 接线，验证调用隔离、提交失败及只读重开；并行补 DeepSeek 原生 thinking 选项的真实 HTTP 契约。其后继续工具选择与其余缓存协议项，逐项审查接线并更新原待办。最终仍须全仓普通/race、构建/静态检查、漏洞、live 与跨平台证据；macOS 继续按批准延期，不记通过。当前尚无本轮实现通过结论。

## 2026-09-28 09:49 Steps14–21当前状态核对

本节为当前状态，后续旧日期记录保留历史，不将已解决缺陷继续列为当前阻塞。Step14按新批准的运行期审批契约完成实现与双平台专项验证；单调时间修复已由主线程race20通过。Step15版本观察、只读取证、原子release及unknown限制已实现；主线程 `GOWORK=off go test -mod=readonly -race ./internal/sessions ./internal/sessions/state -run 'Reconcile|Reconciliation|UnknownEffect|Unresolved' -count=20 -timeout=300s` 退出0（60.006s/1.449s）。结合原双平台受影响包证据，原p2-approval-reconcile待办更新为completed。完整子进程故障矩阵、最终全仓/live/漏洞门禁归Step23，不重复当作各前序步骤尚未实现。

Step16：read/用户shell/日志已实现并验证；ls分页排序、glob/grep模式限额、公开精确edit契约、invocation TODO持久事实仍待完成。List/Search公开扩展已获准，但尚未实现，属于实施排期遗漏而非继续等待授权。

Step17：模型/工具选择及有键重绑已有实现；search_tools下轮开放闭环和无原请求模型恢复绑定仍有缺口，不能关闭。Step18：Responses完整回放及加密推理补偿通过，可信Session缓存键接线及私有必需数据缺失边界仍待完成/核对。Step19：Claude基本协议和redacted回放补丁完成，显式缓存断点及数量/TTL验证尚缺。Step20：Gemini工具ID/签名往返已修复并接入默认依赖，显式缓存创建、复用、失效重建及辅助请求预算尚缺。Step21：DeepSeek仍拒绝thinking控制，五工厂统一工具任务与跨协议私有数据兼容性契约尚未完成。macOS按已批准延期，不阻塞这些步骤。

## 2026-09-28 09:42 审批时间误判根因与修复交接

Linux真实时钟诊断在PartialAnswers失败复现中捕获墙钟回拨约1.079s/2.759s，而单调时间分别前进约72ms/104ms。运行期审批创建时UTC()剥离单调时间，导致“签发时间之前”检查误判expired，虽然实际仍有近24h期限。最小修复保留运行期单调时间，仅在公开快照副本转换UTC，不改变24h期限或用假时钟掩盖原测试。确定性测试先红后绿验证保留单调分量、签发/到期边界及快照不改内部期限。

原始扩大race中的ModelSelection/turn失败未捕获时钟样本，不能断言其历史实例已精确归因；该路径使用相同有效期逻辑。交接报告Windows/Linux受影响普通/race及两原测试二十轮通过。主线程已读新测试与快照转换，并独立执行 `GOWORK=off go test -mod=readonly -race ./internal/sessions -run 'TestApprovalRuntimeDeadlineRetainsMonotonicTime|TestApprovalPartialAnswersKeepUnansweredOriginalCallsWaiting|TestApprovalResumeUsesCommittedModelSelection|TestApprovalClaimRechecksExpiryAndPolicyAfterAuthorization' -count=20 -timeout=300s`，退出0（包耗时158.631s）。审批相关四项二十轮竞态复验通过；完整门禁仍待执行，不标P2完成。

## 2026-09-28 09:27 全仓普通通过与公开投影交接

主线程 `GOWORK=off go test -mod=readonly ./... -count=1 -timeout=300s` 退出0（26.011s），包括正式私有适配器及产品原协议红测所在llm包。此为当前一次Windows全仓普通证据，不替代race/live/完整功能验收。

公开投影修复已交接：事件保存展示投影、Snapshot深拷贝消息切片，剥离私有签名和供应商扩展，内部历史及ConvertToLLM保留原数据；主线程已读PublicMessage并独立执行 `GOWORK=off go test -mod=readonly -race ./internal/agent ./internal/sessions -run 'TestPublicMessage|TestSessionPublicMessage' -count=10 -timeout=180s`，退出0（agent 1.177s，sessions 3.397s），公开投影十轮竞态复验通过。交接报告该专项Windows/Linux通过，但Linux扩大race出现ApprovalPartialAnswers的enhanced-invokable及ApprovalResumeUsesCommittedModelSelection/turn过期失败；已安排先诊断/复现再最小修复，不先放宽期限或凭重跑绿推断根因。旧已写事件正文未迁移，保持历史原样。全仓收口仍阻塞，P2待办不关闭。

## 2026-09-28 09:26 默认依赖接线完成交接

生产与测试已统一引用仓库私有Gemini/Claude适配器，go.mod旧两模块require移除，版本未升级，go.sum历史校验保留。主线程独立全仓*.go搜索确认零旧适配器导入；测试注释区分私有补丁认证与原始上游版本，未削减协议断言。

交接报告GOWORK=off下Windows/Linux Go1.27模型层及私有包普通/race、architecture、模型层vet/build通过。主线程已启动 `GOWORK=off go test -mod=readonly ./... -count=1 -timeout=300s` 全仓普通复验，结果待收集；并行公开投影任务尚未交接，最终稳定代码仍须完整门禁，不据此标P2完成。macOS按既定决定延期。

## 2026-09-28 09:23 Gemini能力门禁与空载荷修复

追加核查通过真实Catalog工厂Generate/Stream确认：未声明CapTools、DeferredTools、ToolSearchTool、混合server-tools及Vertex路由均unsupported_capability且零实际HTTP/观察请求。既有Catalog门禁足够，无须恢复普通函数工具一概拒绝。

新可控模型测试先复现Type=FunctionToolCall但payload=nil被Generate/Stream误判成功；normalizeGeminiFinish补完整性检查后返回invalid_argument，底层调用次数保留为1。该红绿测试是内部消息完整性证据，不冒称HTTP供应商夹具。主线程确认工厂已引用私有适配器，并独立执行 `GOWORK=off go test -mod=readonly -race ./internal/llm -run 'Test(P2Factory|Gemini|Anthropic)' -count=10 -timeout=180s` 退出0（包耗时1.966s）。这证明该产品工厂与能力门禁测试集合在默认依赖图下通过十轮竞态验证，不依赖临时工作区。公开投影与全仓验证仍未完成。

## 2026-09-28 09:20 私有补丁副本迁入交接

已迁入internal/llm/einoext两个供应商适配器最小生产闭包、离线HTTP测试及LICENSE/NOTICE/PATCH_NOTES，共24文件。Gemini最新同帧签名修复已在私有副本先复现旧行为红测再同步，不需重复复制。产品现有版本提升五项直接依赖，无版本升级、无嵌套模块或本地replace。主线程已读补丁来源/差异/切回上游说明，并独立执行 `GOWORK=off go test -mod=readonly -race ./internal/llm/einoext/... -count=10 -timeout=180s` 退出0：Claude 1.243s，Gemini 1.632s。此结果验证正式私有包在产品默认依赖图下通过，不依赖临时工作区；仍不替代工厂统一接线后的整体验收。

旧生产和测试import仍需统一切换并移除两上游module的过渡require，已安排精确import修改，防止Claude新旧包重复类型注册。正式产品回放/架构与默认依赖图验证待接线后进行；公开数据投影和Gemini能力门禁追加核查并行进行，不标P2完成。

## 2026-09-28 09:19 Gemini产品工厂交接

最新隔离签名补丁主线程Windows显式生产文件+两离线测试文件 `go test -race ... -run TestOffline -count=10 -timeout=180s` 退出0（1.752s），不是上游完整套件验收。产品工厂接线报告Gemini/Claude两轮真实HTTP及JSON回放在临时GOWORK下Windows/Linux普通/race10通过；GOWORK=off回放仍失败，尚未正式接入私有副本。

主线程审查gemini.go确认响应与历史ID校验、不伪造ID及STOP工具结束归一化；进一步要求核查移除旧一概拒绝后DeferredTools/ToolSearchTool、未声明工具仍请求前拒绝，以及缺工具payload的响应完整性。临时工作区引入额外模块和传递版本变化，最终必须按产品默认依赖图复验；此时不能宣布默认产品已交付。公开SDK最终事件/Snapshot的私有数据投影修复仍在进行。

## 2026-09-28 09:17 同帧签名修复交接

隔离Gemini副本conv.go新增同响应signature-only附着当前最后有效块，新增signature_parts_test.go真实Stream→Concat→JSON→下一次HTTP往返三类打包红绿覆盖。主线程已读实际分支，正独立运行Windows离线race十轮；产品复制任务仍运行中，待其交接后必须同步conv.go、新测试及PATCH_NOTES，不能把旧私有副本当最新补丁。

本轮Linux独立复验尝试在WSL启动阶段返回Wsl/Service/E_UNEXPECTED，Go测试未启动，工具未显示明确数值退出码；不记Linux最新签名补丁race通过，不以此前补丁版本的Linux绿测替代。未重启WSL以免打断并行验证。公开投影修复和产品工厂接线仍在进行。

## 2026-09-28 09:10 补丁独立审查与Linux复验

Linux实际 `GOWORK=/mnt/d/Code/owner_agents/eino-ext-p2-fixes/p2-validation.work go test -race ./internal/llm -run TestP2FixedAdapter -count=10 -timeout=180s` 退出0；仍是临时工作区依赖证据，不能代表正式默认依赖。WSL输出存在编码显示问题，不据乱码推断额外通过项。

独立审查发现两项验收阻塞：Claude块级Extra在内部持久化及模型回放可保留，但message.finalized与Snapshot目前使用完整消息，私有opaque可能被公开；Gemini同一响应parts中的functionCall后signature-only分块可能丢签名或错索引，现有独立帧测试未覆盖。已分别安排真实Session公开投影红测及完整HTTP同帧签名往返红测后修复；内部回放必须保真，不删除私有记录。正式补丁副本尚在准备，Gemini后续修复需同步后才验收。不宣称现有十轮绿测覆盖这些新发现。

## 2026-09-28 09:08 补丁正式交付方式确认

维护者选择将两个适配器的必要源码作为仓库内私有补丁副本，保留许可证与准确来源，由产品直接引用；不发布远程fork。原因是依赖模块的replace不会自动传递给外部SDK消费者，临时GOWORK绿测不构成可分发修复。开始在internal/llm下隔离供应商适配责任，复制最小生产闭包并记录基线及后续回归上游条件，不修改Eino核心，不用本地绝对replace。

另已核实WSL Ubuntu-24.04可直接运行go1.27.0，已启动Linux实际产品固定协议race十轮，不沿用此前“只有Go1.22”的环境报告。Gemini产品工厂仍有旧的函数工具拒绝逻辑，单独执行先红后绿的产品工厂接线验证；正式副本与工厂尚未整合，不宣称默认依赖或全仓已通过。

## 2026-09-28 08:53 上游补丁与产品红测独立验证

隔离eino-ext副本基线3603a39473c3e7b2aa3bfc11216487c94b8c7fd9，匹配Gemini v0.2.5及Claude v0.1.7。补丁保留真实调用ID/请求回传/独立流式索引，以及Claude redacted块Extra和JSON回放；没有改Eino核心或产品go.mod/sum。主线程审查转换差异后，通过副本内临时p2-validation.work显式GOWORK注入两模块，执行产品原测试 `go test -race ./internal/llm -run TestP2FixedAdapter -count=10 -timeout=180s` 退出0（包1.442s）。历史红测未删除，首次在补丁依赖下转绿。

此结果仅证明临时开发工作区下定向契约通过，不代表默认依赖已修复；go.work还可能影响模块版本选择，正式接入须核对依赖图并固定可复现来源。独立审查正在核查流式合并、空Reasoning/Extra持久化与公开投影不泄漏。正式依赖集成、完整门禁和Linux race仍待完成，macOS继续延期不记PASS；未提交推送或发布PR。

## 2026-09-28 08:31 最小上游适配补丁获准

维护者批准以效果优先修复Gemini/Claude适配器，允许必要的临时fork作为此前“不维护fork”的限定例外。优先修复eino-ext转换层并复用原SDK，不修改Eino核心，不伪造供应商ID，不丢弃必需私有回放数据。仅从转换后结果无法恢复的数据，可能通过修复接收与发送两端解决；此前“薄适配绝对无法恢复”的表述过强，保留历史调查但不视为所有方法均已排除。

实施先在隔离上游副本验证真实红测与补丁，不修改Go模块缓存。记录基线、许可证、补丁和切回上游条件；最终依赖必须可复现且无本地绝对路径replace。未经另外明确授权不提交、推送或发布PR。当前仅批准方法，不代表已修复或P2完成；原计划文件不修改。

## 2026-09-28 08:03 两项范围阻塞调查结论

Gemini/Claude专项调查完成但未修改代码。固定 `agenticgemini v0.2.5` 已丢弃供应商 FunctionCall.ID、回放ID及跨帧独立调用身份；固定 `agenticclaude v0.1.7` 已在适配器转换阶段丢弃 `redacted_thinking`，流式路径也无分支。当前产品工厂已对Gemini函数工具内容请求前拒绝；不能在internal/llm伪造供应商ID、拼造redacted内容或修改Eino/fork。两个固定适配器红测继续保留，协议步骤不能宣称完成；需要升级依赖、上游未发布修复或范围调整才能继续。

内建工具调查确认生产路径已有 `write_file`/`edit_file`/`write_todos`/`ls`/`glob`/`grep` 接线，但真实验收缺口需要扩展SDK别名暴露的 `ListRequest`（分页上限）和 `SearchRequest`（glob/grep模式及匹配限额）契约；当前testkit List/Search明确unsupported，且文件操作错误码投影不足以断言版本冲突/模式错误。因用户规则要求发现公开契约变化先停止，任务未自行修改。需确认公开字段迁移后才能补真实后端和消费者测试。



主线程执行 `go test ./... -count=1 -timeout=300s` 退出1（34.485s）。除internal/llm固定协议红测外，cmd、agent、eino、tools、architecture、sessions、state、store、testkit、sdk均通过。失败仍为Gemini v0.2.5工具调用ID/跨帧独立调用丢失，以及Claude v0.1.7 redacted_thinking生成/流式/回放丢失；协议修复任务已启动，不能以局部绿测或删除红测收口。
## 2026-09-28 Open审批创建时机修复交接

主线程复核生产接线：Start不再重建审批，prepareApprovalResume只在显式Resume受理后的原GenResume回调调用；安全暂停后的bindApprovalCheckpoint只绑定已有问题，不创建历史问题。跨实例应按Open→Resume→新询问→Respond→Resume使用。tool_intent分支在同一mailbox内先checkToolPolicy再claimRuntimeApproval，未发现跨mailbox的有效期窗口；新增过期/撤销屏障测试覆盖先前授权失效后零intent、零额度和零执行。

实现报告Open可写/只读两分支先红后绿、Windows/Linux受影响普通/race通过。主线程独立执行三项关键测试 `go test -race ./internal/sessions -run 'TestApprovalDiskOpenDoesNotAskOrWrite|TestApprovalClaimRechecksExpiryAndPolicyAfterAuthorization|TestApprovalReopenExplicitResumeWithoutAnswerWaitsAgain' -count=20 -timeout=240s` 退出0（包耗时51.766s），覆盖打开零审批、授权后过期/撤销以及重开显式恢复重新询问。该专项通过不替代完整审批/P2验收。

## 2026-09-28 审批迁移交接与独立审查缺口

审批请求/决定/回执已移至sessions运行实例内存，持久claim仍保存执行意图和预算，checkpoint仅保存原调用中断目标。交接报告Windows/Linux受影响包通过，但主线程审查发现session.go初始化直接restoreApprovalRequests，会在Open时创建新待审批项，与已确认的“Open不询问，仅显式Resume后按需重新询问”不符。已要求先补磁盘Open/read-only快照零新审批红测，再迁移创建时机，保留原GenResume与防重跑约束。原审批待办继续in_progress，不能据局部绿测关闭。

同时要求核查一次许可有效期在原子claim所在mailbox边界的检查，防止此前策略检查与真正claim之间过期。主线程已启动现有审批专项race独立复验，后续修复仍需重跑。全仓Gemini/Claude缺口尚未解决，macOS按已批准延期，不作为本轮需重复确认的事项；最终漏洞检查使用已固定v1.8.0，不因PATH缺工具跳过。

## 2026-09-28 公开读取后端迁移验证交接

完整快照契约注释及05/06迁移说明已补充。新增SDK外部消费者通过CreateAgentSession→SubmitInput→模型read_file→注入后端验证行/byte真实续读与版本冲突；主线程已审查测试，确认正常路径模型3次、Read/Open各2次，冲突路径Read2次/Open1次，持久观察检查完整JSON及state_conflict，而非仅直接调用fixture接口。消费者未导入internal；模型夹具因公开模型接口签名依赖Eino类型，仍有Eino导入，不宣称整个消费者只有sdk一个第三方依赖。

实现报告Windows/Linux定向普通/race十轮及独立consumer测试通过；主线程独立执行 `go test -race ./sdk/testdata/consumer -run TestSDKConsumerReadSnapshotThroughSession -count=10 -timeout=120s` 退出0（包耗时38.941s），公开读取后端迁移的十轮竞态复验通过。实际宿主旧后端仍须迁移，macOS未认证，Step16其他文件工具及TODO能力尚未验收，不关闭原待办。

## 2026-09-28 文件读取契约迁移获准

维护者在明确得知旧后端预裁剪会导致重复截取、ReadRequest新增Mode会影响无字段名结构体字面量后，选择允许调整读取契约，要求现有注入后端迁移为完整快照语义。这是独立于ExecuteCommand的限定兼容性例外：内建read_file发送lines/bytes，FileOperations.Read返回完整、版本稳定的不可变快照引用，由工具层投影范围；后端不得按Offset/Limit先裁剪该快照。须保留授权/版本检查，旧后端迁移和公开消费者验证仍待完成，不能把批准当验收通过。

字节模式2000行限额及末尾无换行计数已有实现交接，主线程独立 `go test -race ./internal/agent/tools ./internal/testkit/operations -run Read -count=10 -timeout=120s` 退出0，两包耗时分别4.755s、1.384s。该结果覆盖读取双限额及续页回归，不替代尚待完成的公开SDK消费者迁移验证或Step16其他能力验收。

## 2026-09-28 读取实现交接与需求复核

读取范围实现已接入 Executor 的 Read→受控快照 Open→有界投影路径；主线程读取 read.go 后发现 bytes 分支虽统计换行却未执行2000行上限，尚不满足 Step16 的行/字节双限额。已要求先补真实 Executor 超过2000短行且小于50KiB的红测，再修复并验证 nextRead 版本、偏移及无丢失续读；Step16不关闭。交接报告中的 Windows/Linux 局部通过不能替代这一缺口验收。报告曾使用 govulncheck@latest，不纳入最终固定工具证据，最终仍需按v1.8.0执行。

Responses 裸上游探针已改为v0.2.4已知行为刻画，保留单次HTTP、item身份、完成状态和请求约束；产品完整回放断言不变。主线程独立 `go test -race ./internal/llm -run 'TestP2FixedAdapterResponses|TestResponsesReasoning|TestP2OpenAIResponses' -count=10 -timeout=120s` 退出0（2.007s）。Include属于私有推理回放字段，不能直接将缓存参数的能力门禁套用于它。后续只读核对确认 Step18 的能力限制针对缓存键/保留参数，不要求 encrypted_content 独立 opt-in；保留工厂显式 Include、store=false 和完整回放，不新增公开能力常量。先前“无条件 Include 违反门禁”的判断撤回。官方字段允许 nullable，服务端未返回签名不能一概判为错误；对于确实必需但漏返的私有回放数据，当前 Generate/Stream 尚缺明确条件与拒绝接纳的验收证据，单列待核实，不据此宣称完整 Responses 协议已认证。

## 2026-09-28 Responses 推理回放独立复验

Responses 实现交接后，主线程审查 responses.go/transport.go/usage_body.go 的实际接线：使用请求私有采集器原字节透传，真实 item ID 关联 reasoning，完整终态后补一次私有签名；store=false、关闭自动状态链保持。主线程独立 `go test -race ./internal/llm -run 'TestP2FixedAdapterResponsesEncryptedContentReplay|TestResponsesReasoning|TestP2OpenAIResponses' -count=10 -timeout=120s` 退出0（包耗时2.159s）。这是产品定向回归，不是全仓或五协议认证。

审查另发现新增裸上游 done-only 探针被刻意留作默认失败：本产品薄适配已修复的缺陷，应保留历史红证据，并将裸上游探针改为固定版本行为刻画，同时保留产品工厂完整回放的强断言；已安排修正，不删除尚未修复的 Gemini/Claude 产品缺口红测。请求 include 的能力边界继续核查。实现报告中的全仓/live 检查失败，不构成通过证据；审批迁移稳定后须重新验收。

## 2026-09-28 审批生命周期再次确认与实施恢复

维护者在本轮执行前明确选择：一次性审批仅存当前运行实例内存，关闭后失效，不随会话历史恢复；工具执行意图、原子预算与调用 claim、结果和 unknown 限制仍须持久保存。此确认取代附带旧计划 Step14 的审批决定持久恢复要求，不修改原计划文件。尚未执行的调用显式恢复时重新判断权限并按需询问；已完成结果复用，未知效果不得因重新审批而重跑；Open 不执行也不重新弹出历史审批。用户 ExecuteCommand 独立免审批不变。本会话可复用授权、永久授权规则不因本次调整自动纳入实现。

已重新打开原 p2-approval-reconcile 待办进行迁移与回归，p2-builtins-selection 保持进行中；不新增重复待办。实际 Git 分支 feat/p0-p1-runtime，现有未提交修改保留。本轮先复用已存在的运行期审批与文件读取测试，确认红测后实施，尚无本轮通过证据，不宣称 P2 完成。未提交、未推送。

本轮协议复验 `go test ./internal/llm -run 'TestP2FixedAdapter|TestResponsesReasoning' -count=1 -timeout=60s` 退出 1（1.032s）：responses_reasoning.go 五处引用 UsageCollector.responses，但该字段尚未接入，属于当前工作区编译失败，测试未执行。已安排补齐 Responses 推理回放接线并验证真实 HTTP 路径，不能将编译修复代替协议行为验收。

## 2026-09-27 18:25 Linux 模型重绑复验失败

主线程 Linux `go test -race ./internal/sessions -run P2ModelSwitch -count=10 -timeout=600s` 退出1：TestP2ModelSwitchDiskRebindAcceptedModelPreservesBuild/A 在 selection_rebind_test.go:165 复合断言失败（完成状态、模型/工具次数、checkpoint相等）。当前输出没有逐字段信息，不能推断重复执行或活动续期是原因。已安排保留原断言/等待限制，先补安全诊断并Linux专项复现，再按证据修复。此前Windows定向通过不覆盖此次失败，Step17继续未完成。

## 2026-09-27 18:22 回执重放修复交接

实现报告同键回执回归已修复：无效候选只使可选进程内重绑不生效，不改变历史回执；后续Resume仍拒绝缺失有效模型。默认next_trace与下一轮next_turn分别按原摘要校验并支持重绑，不替换实例或较新的默认选择。主线程已核对两个FindOperation分支均返回原receipt/err，不再返回重绑错误，并启动Linux `go test -race ./internal/sessions -run P2ModelSwitch -count=10 -timeout=600s` 独立复验，结果待收集。

新增组合断言覆盖原Scope/FrozenCall/checkpoint/调用集合；报告A→B最终usage为3/3/2，默认B为2/2/1，定向Windows普通及race十轮通过。无键或缺原请求历史仍缺恢复绑定，Step17不关闭；固定adapter协议红测仍未修，不能称全仓通过。

## 2026-09-27 18:07 独立复核后续

独立复核通过pending首次重开内容校验及有键next-turn B重绑路径，但确认新增重绑失败会截断原历史回执，违反同键同内容重放契约。已安排先红测再修复：重复请求仍返回原回执，不合格实例不绑定，Resume继续明确拒绝；不改变已有实例、默认选择或持久状态。补实际组合的完整Scope/FrozenCall/调用数量和usage断言，以及默认B选择的重开路径。默认选择、无幂等键/缺原请求恢复不能借next-turn测试宣称通过；无键公共重绑契约仍未批准。

## 2026-09-27 18:04 选择恢复修复交接（待独立复核）

实现报告：首次重开前修改pending名称/版本/模型身份的三项真实JSONL红测已修复，选择内容重算原operation摘要，工具版本对照受信generation定义，不仅比较同源View。A→B实际执行并审批暂停的磁盘恢复，通过保持Options.Model=A并重放原SelectNextTurnModel及幂等键重新绑定受信B实例；仅恢复缺失的进程内绑定，不追加revision、不替换已有实例、不放宽build指纹。报告选择普通/race各十轮退出0，主线程已安排原审查者独立复核，不能据报告直接关闭Step17。

无幂等键或无法重放原请求的历史选择仍缺模型重绑路径，新增按operation ID绑定入口尚未批准；这是明确未完成边界，不称所有跨模型重开均可恢复。全仓协议默认红测仍在，完整门禁未通过。

## 2026-09-27 17:59 固定协议适配器真实红测

新增默认测试 internal/llm/p2_fixed_adapter_protocol_test.go，真实固定依赖+本地HTTP夹具共5个测试/10个叶用例，8失败、2对照通过。Gemini同步/流式响应和回放ID丢失、跨帧两个独立调用合并为1；Responses仅output_item.done携带的encrypted_content未保留/回放（同步及added阶段对照通过）；Claude redacted_thinking同步/流式丢失且未回放，普通thinking签名对照成立。离线 `go test -mod=readonly ./internal/llm -run '^TestP2FixedAdapter' -count=1 -timeout=45s -v` 退出1；不是产品五协议交付绿测，当前默认套件含明确未解决红测。

本地上游副本未发现修复，不代表远端无修复。主线程正在只查询三个适配器远端latest元数据，不修改go.mod/sum、不改模块源码、不引入fork或伪造供应商ID。查明可用版本与兼容性后再确定修复方法，不能通过删除失败断言或改为skip宣称P2完成。

## 2026-09-27 17:51 五协议剩余交付核查

只读源码调查确认五协议工厂已存在，但 Steps18–21 未完成：Gemini 固定adapter未保留工具调用/结果ID且跨帧边界需探针，显式缓存生命周期未接线；Responses 加密推理的请求include与流式done回放待补；Claude 显式缓存断点、redacted_thinking回放待补；DeepSeek thinking控制字段和私有签名兼容性、五工厂工具往返统一契约待验证。已有usage presence等覆盖不重复列为缺失。

仅运行 go test -list 的调查结果不是测试通过证据，原计划协议筛选存在未命中。已安排使用固定真实依赖与本地HTTP夹具先复现上游转换缺陷；不改模块缓存、不擅自升级、不修改Eino或维护fork。Gemini缺供应商ID继续明确拒绝，不自行生成供应商身份。修复方法必须保留完整回放、传输计量及既定依赖边界，必要范围变更另行确认。

## 2026-09-27 17:49 用户 shell 日志整合交接

用户 shell 现已复用 PrepareOutputLog/SaveOutputLog，新增 CommandResult.Truncated/Artifact/LogError；实际退出状态先确定，再处理日志，最后提交一条 command 历史，不创建模型调用、审批、预算、票据或投影事实。主线程已读取实际 ExecuteCommand 接线，确认产物绑定使用 Session/host/历史ID，历史提交失败会清除返回引用，保留真实执行结果；不因此重跑。

交付报告的 Windows/Linux 定向普通、race 及 SDK/架构测试退出0；此前真实红测为长输出未截断、未脱敏、零保存及无独立日志错误。完整日志仍依赖注入后端，没有本地持久产物实现；长输出缺脱敏器时隐藏正文并报日志错误。主线程四包十轮日志/用户shell/消费者竞态复验已完成：`go test -race ./internal/agent/tools ./internal/sessions ./internal/testkit/operations ./sdk/testdata/consumer -run 'TestP2Command(Log|Shell)|TestProcessLog|TestProcessOutputIsSaved|TestOutputArtifacts|TestConsumer.*Command' -count=10 -timeout=600s` 退出0，四包均命中测试，耗时分别2.458s、260.271s、1.177s、1.808s；不得将定向测试视为最终全仓验收。其他 Step16 文件语义、TODO 持久化和 Step17 审查缺口仍待完成。

## 2026-09-27 17:47 选择事务独立审查

只读审查确认 BeginTurnID 同ID调用在计数前返回，选择激活、Turn快照和usage通过同一commit提交，执行侧只在成功后更新。但发现两项待修：①checkpoint pending例外只比较同源日志/View，首次Manager创建前篡改pending内容但保留原operation摘要的情况缺少重新核对；②原Options.Model=A，实际切B后暂停，重开时单一Options.Model回退与原build指纹无法同时恢复B。后者是基线已有缺口，不称本次回归。已安排先加真实磁盘失败测试，再最小修复；涉及新增受信模型解析公共配置时须先确认，不以放宽build检查解决。Step17保持进行中。审查定向四包普通测试退出0，不替代尚缺测试或全仓门禁。

## 2026-09-27 17:40 恢复实施

维护者确认余额已恢复。已恢复用户 shell 日志整合及选择事务只读审查，先核对中断前部分文件，不重建原待办。此前 403 保留为历史中断记录，不代表当前仍阻塞，也不代表未完成项目已验收。

SDK 外部文件后端新增默认消费者测试，实际实现 FileOperations 并调用 Write/Edit；初次编译失败明确为缺少公开 sdk.FileEffect。现已在唯一公开生产文件 sdk/sdk.go 补类型别名。随后定向 race 首次重跑遇到并行日志提取中 logNotSaved 重复声明；日志帮助函数提取完成后再次执行 `go test -race ./sdk/testdata/consumer -run TestConsumerCanImplementFileOperations -count=10` 退出 0（1.153s）。这是公开类型可实现性的编译与调用证据，不代替受控文件后端认证，不标 Step22 完成。

## 2026-09-27 17:05 执行中断与恢复交接

用户 shell 日志整合及选择激活/恢复的独立审查均因执行服务返回 HTTP 403 / INSUFFICIENT_BALANCE 中断。这是开发服务不可用，不是项目模型测试失败；两项不能标记完成，不反复重试、不自行更换账号或服务。已有工作区改动保留，下一次继续须先核对中断任务是否留下部分修改，再完成用户 shell 日志接线、选择独立审查和稳定代码全仓门禁。P2 仍未完成，本轮未提交推送。

隐藏工具复用调查已复核测试差异：通过手动时钟隔离无关活动续期，原全局序号、调用/预算/身份断言均保留，并增加完整 View 相等断言。确定性续期实验仅新增一条 trace 记录，能使原全局序号断言失败；这不能证明原 Linux 偶发失败必然同因。报告的 Windows/Linux `go test -race ./internal/sessions -run '^(TestSelectionHidden|TestActivity)' -count=20` 均退出 0（108.747s/104.499s），不替代主线程全仓验收。

主线程 Linux 选择专项 `-run P2Selection -race -count=10` 退出 0；日志专项四包 `-run ProcessLog -race -count=10` 退出 0，但仅 tools/sessions 命中测试，state/testkit 未命中不记通过。模型进程日志已接线的结果不等于用户 shell 日志整合已完成。

## 2026-09-27 16:05 继续实施：ExecuteCommand 契约迁移已批准

维护者明确选择直接修改现有 ExecuteCommand 为用户 shell，不新增 ExecuteShell、不保留任意登记工具调用入口；旧请求字段及持久化操作回执可按新语义迁移。这是对先前 SDK 源兼容约束的限定例外，模型工具与其他公开 API 不变。实现及整体验证仍在进行，不能据此标 Step 16/22 完成。

日志预览已取得两个真实红测：超长单行返回 Truncated=false；尾部按字节丢弃前行后 StartLine/EndLine 仍使用旧位置。最小修复后 `go test ./internal/agent/tools -run TestPreview -count=1` 退出 0；新增头尾合并预览的初始失败为缺少新函数的编译红，不算既有行为红。`go test -race ./internal/agent/tools -run TestPreview -count=10` 退出 0（1.173s）。PreviewHeadTail 保留短输出原文，长输出的标记、头尾共同受行/字节限额约束；尚需接入最终输出链路，函数测试不替代产品验收。

外部 SDK 新增 `TestConsumerExecuteCommandIsUserShell`：只 import sdk，以工作区外的显式 cwd 两次调用新 CommandRequest，断言各执行一次、真实退出、模型调用 0、历史已保存，并显式使用公开 CommandResult。首次两次编译失败来自迁移中残留 directBuildFingerprint；清理该旧分支后 Windows 普通和 race10 均退出 0（race 1.532s），Linux 实际 race10 退出 0（1.191s）。Windows `go test -race ./internal/sessions -run 'TestP2CommandShell|TestP2CommandLegacy' -count=10 -timeout=180s` 退出 0（16.120s）；Linux `-run TestP2Command -count=10` 退出 0。以上是迁移中定向证据，仍需稳定代码全仓门禁；其他并行 Step16/17 红测尚未解决前不宣称全仓通过。

另以真实 Executor 红测复现 read_file 的 version 参数未传入 FileOperations.Read（实际为空），按读版本优先、兼容旧 ExpectedVersion 的最小修复后 `go test -race ./internal/agent/tools -run TestBuiltinReadFile -count=10` 退出 0（1.382s）。读取的完整模式/续读元数据验收仍未完成。

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
