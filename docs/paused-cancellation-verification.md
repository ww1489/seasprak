# 暂停任务安全结束：实现与验证

日期：2026-09-25。环境：Windows amd64，Go 1.27.0，Eino v0.9.15。

## 本批交付范围

本批完成“安全结束暂停任务”，并增加真实原生断点恢复的接线测试。**公开 Pause/Resume/Reconcile、不可变 checkpoint blob 存储、产品 checkpoint 关联、恢复兼容校验和核对操作仍未交付**，不能把下述原生测试当作 SDK 已支持执行恢复。

- 既有 `Cancel(ctx, traceID)` 接口保持兼容。
- 没有运行中执行器的暂停任务，在已有持久停止证据，或不存在已占用但未解决的工具调用时，可以结束为 `cancelled`。
- `TraceState.ExecutionStopped` 表示协调者已观察到执行段退出，不表示效果已知。正常执行在执行段返回（进入 TurnLoop 的路径包含 Wait 返回）且不再续执行段后记录；重开将任务标成 paused 不产生该证据。旧暂停任务的安全取消也只保留已有证据，不新增该标志。
- 对旧记录，没有执行停止证据且存在已占用无结果、未知结果或未知效果的调用时，取消仍返回 `reconciliation_required`，不改事实。
- 安全取消复用既有轮次收尾与终态提交路径，保留 Trace/Invocation/调用身份、累计预算和已有结果。未执行工具记为 skipped/none；pending 定向输入变为 undelivered；既有独立队列保持 hold；重复 Cancel 不重复提交 settled。
- 已确认执行停止但效果未知，可以结束任务并保留未知事实。未知效果持续阻止新执行输入、ContinueQueue、自动调度以及当前执行的后续模型轮次。旧幂等回执仍可查询重放。
- 调用方等待超时不等于执行停止。停止证明写入失败时，不暴露证明、不提交 cancelled/settled，Manager 进入存储故障状态。

## 缺陷复现与正式回归

先运行 `go test ./internal/sessions -run 'CancelPaused|PausedReopen' -count=1 -timeout 45s`，退出码 1：纯模型暂停、未占用工具、已保存确定结果、已停止但未知效果四类安全结束路径均因旧的 `reconciliation_required` 分支失败；无停止证据的未知调用继续正确拒绝。

另运行 `go test ./internal/sessions -run '^TestRuntimeUnknownEffectStopsModelAndQueue$' -count=1 -timeout 30s`，退出码 1：旧实现收到未知效果后仍推进模型并完成任务。修复后同组测试均通过。

新增默认测试：

- `internal/sessions/paused_cancellation_test.go`：JSONL Close/Open/Cancel、旧崩溃前缀安全判定、调用身份/结果不变、旧队列 hold、唯一 settled、未知效果跨两次重开保留、无模型/工具重放、取消等待超时、停止证据提交失败不结算。
- `internal/sessions/state/recovery_test.go`：停止证据持久重放、重复确认幂等、提交失败快照不变、正常运行中的 claim 与未知效果区别。
- `sdk/testdata/consumer/recovery_test.go`：独立消费模块只导入公开 SDK，执行 Create→Close→Open→Cancel，检查停止证据、身份/预算、队列 hold、无模型重放和重复取消游标不变。由默认 `sdk` 测试自动复制并运行。
- `internal/agent/eino/checkpoint_regression_test.go`：真实 NewAgent + Executor + PipelineTool + Eino TurnLoop，分别在模型后工具前、工具后下一模型前 Graceful Stop，再重建 Agent/Loop 并走 GenResume。工具缓存未实现 LookupTool，防止把产品缓存误当框架执行栈恢复。断言模型与工具实际次数、GenInput/GenResume 路径、原 InputRef/Turn、独立重建后的累计预算与非空 checkpoint。

## 复核后修正

独立复核发现：旧 paused 在“没有未解决调用”路径上能够安全取消，但原实现会额外写入 `ExecutionStopped=true`，把安全判定误当成本进程已观察到执行退出。未知调用的前置阻挡仍有效，因此这不是已证实的未知效果放行，但会污染后续恢复所依赖的事实语义。

先为 `TestRuntimeCancelPausedCrashPrefix` 增加“取消前后停止证据必须不变”的断言；运行 `go test ./internal/sessions -run '^TestRuntimeCancelPausedCrashPrefix$' -count=1 -timeout 30s`，退出码 1，unclaimed/known_result 两例按预期失败。随后移除 `cancelInterrupted` 中的证明写入。活执行段仍在实际退出后保存证明，安全结束能力不回退。

同一组测试新增 `stopped_claim_no_result`：从已提交停止证明、但缺工具结果的内存日志前缀重建 Manager，再经 Start/Cancel 收尾。原 claim 保留，追加 outcome_unknown/unknown，未知效果继续阻挡新输入，唯一 settled。这是日志前缀重放测试，不是新增真实进程 kill 窗口认证。

原生恢复测试已使用真实 `agent.InputRef`，并在暂停后通过 `NewBudget` + `Restore` 创建独立预算账本；检查原输入三个字段、恢复累计值及旧账本不再变化。清理路径会释放通道、取消并有界等待 Loop 退出，超时显式报错。该证据仍不代表 JSONL checkpoint 恢复或活动时间累计已交付。

## 验证结果

以下命令在本批生产代码修改后执行，退出码均为 0：

- `gofmt -l .`：无输出。
- `go vet ./...`。
- `go build ./...`。
- `go test ./... -count=1 -timeout 120s`。
- `go test -race ./... -count=1 -timeout 120s`。
- `go test -race ./internal/sessions ./internal/sessions/state -count=10 -timeout 120s`。
- `go test -race ./internal/sessions ./internal/sessions/state -run 'CancelPaused|PausedReopen|UnknownEffect|ExecutionStopped|UnresolvedEffects|StoppedConfirmation|StoppedProof' -count=10 -timeout 120s`。
- `go test -race ./internal/agent/eino -run Checkpoint -count=20 -timeout 90s`。
- `go test -race ./internal/sessions -run '^TestSessionCrash$' -count=10 -timeout 120s`：既有十种进程中断/重开窗口重复验证；不是掉电或每个 Sync 内部字节认证。
- `go test -race ./sdk/testdata/consumer -count=10 -timeout 120s`。
- `go test ./sdk -count=1 -timeout 120s`。
- `go mod verify`：all modules verified。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`，并运行 `-show verbose`：0 个可达漏洞。仍有未调用依赖项 `GO-2026-4514`（jsonparser v1.1.1）及 `GO-2026-5970`（x/text v0.14.0）；本批未改依赖版本。
- `go test -tags live ./internal/llm -count=1 -v`：`TestLocalCompatibleModel` 实际通过，未跳过。仅证明本项目当前 live 断言通过，不代表完整供应商能力认证。
- `git diff HEAD --check`：通过；Git 的 LF→CRLF 提示不是失败。

新增 Go 文件已额外检查冲突标记、尾空格及常见凭据模式，没有命中。`.test_env` 仍由 Git 忽略，未读取/输出其内容。未提交或推送。

## 仍需完成的范围

1. 原生 checkpoint 测试不是 JSONL 执行恢复：尚无产品关联、兼容性检查或公开 Resume。
2. 尚无可信只读 Reconcile；有未知效果的任务即使安全结束，后续工作仍会被阻挡。不能通过改日志、清 claim 或人工填写“未执行”解除限制。
3. 旧暂停任务没有有效恢复点时，仍不能自动恢复执行；可以安全结束的任务不会重跑原提示词。
4. Linux/macOS 实际运行验证按维护者此前同意延期，未执行，不记为通过。当前仅有 Windows 运行证据。
5. Sonic 在 Go 1.27 上提示回退到 encoding/json；这是现有依赖的兼容提示，未改依赖或宣称其优化路径得到认证。
