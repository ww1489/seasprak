# P1 审查缺陷修复与验证记录

记录日期：2026-09-25。基于 `5fd3069` 的当前工作区；本批改动未提交、未推送。此前 SDK 目录修复和用户对开发规范的改动保留，未混作本批新增实现。

## 修复范围与安全边界

- 工具执行在意图及预算保存后、实际调用前再次检查取消。未启动时保存 `cancelled`、`Executed=false`、`SideEffect=none`，保留原调用身份、作用域、claim 和已占预算；返回可由 `errors.Is` 识别的取消错误。观察保存失败时同时保留两个错误。正常工具结果和已启动工具的未知效果处理不变。
- 无工具执行段自然结束时，现有邮箱协调者先检查 steering，再检查 follow-up；继续复用同一 Trace、invocation、generation、上下文和预算。steering 续段在既有 BeforeModel 边界消费，避免 GenInput 和 BeforeModel 在同一边界各消费一条；没有新增模型/工具循环。
- 重开存在 `paused` 执行的会话仍只重建，不自动恢复或放弃。新的执行输入与 ContinueQueue 返回 `reconciliation_required`，不会静默新增输入或解除 hold；同键同请求仍先返回原受理回执。快照仍可查询，未知工具效果不被清除。

暂停处理修复的是“成功受理但永不执行”的接口行为，**不是** P2 的 resume、reconcile 或安全放弃实现。需要继续执行时仍须后续恢复能力；本批没有将暂停任务强行改成终态。

## 正式回归及原始失败证据

### 工具取消窗口

`internal/agent/tools/cancellation_test.go`：

- `TestPersistOrAuthorizeCancelDoesNotStartTool`：授权回调、意图保存、预算保存三个取消点；断言工具调用次数 0、取消错误、原冻结描述与作用域、claim/预算保留，以及真实观察字段。
- `TestCancelObservationSaveFailureKeepsBothErrors`：观察写入失败不伪装成功，不丢失取消错误。

修复前这些测试失败：取消后仍返回 `succeeded`、`Executed=true`，取消错误为空；观察保存失败时只有存储错误。修复后通过。

### 无工具收尾与定向输入

`internal/sessions/steering_regression_test.go`：

- `TestRuntimeNoToolSteeringPrecedesFollowUp`：仅 steering、steering 优先于先受理的 follow-up、连续两条 steering。用通道同步首轮请求，检查每次真实模型请求中的 user 内容、逐项消费、同一 Trace/invocation/generation、累计预算、每轮结束及唯一 settled。
- `TestRuntimeDirectedInputAfterFinalIsRejected`：终态先提交时，后到的 steering/follow-up 返回 `state_conflict`，无状态、事件或模型调用变化。
- `TestRuntimeNoToolSteeringPreservesBudgetLimit`：分别耗尽逻辑模型额度与实际模型请求额度，并交叉验证 steering / follow-up。协调者在续段前检查两类累计预算；预算耗尽时不调用模型、不增加逻辑轮次计数、不消费待处理输入。
- `TestRuntimeCancelDoesNotConsumePendingSteering`：取消不续轮，待处理指令标为 undelivered，重复 Cancel 不重复 settled。

修复前仅 steering 场景模型调用 1 次（预期 2），输入变为 undelivered；带 follow-up 和连续 steering 的模型调用也少于预期。复核追加的预算测试还复现了实际请求额度耗尽后 steering 已消费、逻辑轮次加 1，以及两种额度耗尽时 follow-up 仍被消费。协调者的续段前双预算检查修复这些路径后，全部通过。请求内容断言只比较真实 user 角色，不将框架系统消息当作用户输入。

### 暂停会话显式受阻

`internal/sessions/paused_regression_test.go`：

- `TestRuntimePausedReopenRejectsNewWorkWithoutChangingFacts`：执行中 Close 成功后重开，旧执行保持 paused、旧队列 hold；原回执可重放、异内容幂等冲突优先；新输入和 ContinueQueue 明确失败，缺失目标仍为 not_found，快照和 cursor 完全不变，模型调用 0。

修复前新 prompt/follow-up 被成功受理，ContinueQueue 解除 hold；输入数量从 2 增至 5、cursor 从 10 增至 14，但没有执行。修复后通过。

## 真实子进程崩溃窗口

`internal/sessions/recovery_test.go` 与 `recovery_helpers_test.go` 的 `TestSessionCrash` 使用正式会话、Eino 假模型及 JSONL；测试包装 Store 的 Append，不向生产添加故障钩子。每个窗口在 Append 前和持久成功后、调用者得到回执之前分别阻塞，父进程通过管道确认后 Kill/Wait，再 Open 同一会话，共 10 个子案例：

- `input.accepted_before/after`：未持久受理不凭空生成任务；已受理输入保留原身份与 pending 状态、原回执，重开为 queued+hold，不自动执行。
- `assistant_before/after`：已消费输入不重消费，助手与其工具调用原子接纳；未接纳工具实际调用为 0。
- `tool_intent_before/after`：冻结调用、claim 状态正确；保存意图后尚未实际执行的次数为 0。
- `tool_observation_before/after`：测试工具将实际执行计数保存并同步到独立临时文件；观察未保存时保留未决记录，已保存时保留原结果，不靠重跑修补。
- `trace.settled_before/after`：终态未提交时不伪造完成，已提交时保留唯一原 settled；重复重开和原请求幂等重放不再次执行。

这些用例提供**业务 Append 边界的进程中断证据**，不是断电安全、文件系统缓存丢失、每个底层写字节/Sync 内部位置的认证，也不覆盖 P2 审批或 checkpoint。现有 JSONL 短写、Sync 失败、残缺尾及跨进程锁测试仍单独保留。

## Windows 验证记录

环境：Windows 10.0.26200 / amd64 / Go 1.27.0。以下命令已运行并退出 0：

- `gofmt -l .`：首次发现新 steering 测试需要格式化；执行 `go fmt ./...` 后复查无输出。
- `go vet ./...`
- `go build ./...`
- `go mod verify`
- `go test ./internal/sessions/... -count=1 -timeout 90s`
- `go test -race ./internal/sessions/... -count=1 -timeout 90s`
- `go test -race ./internal/agent/tools ./internal/sessions ./internal/sessions/state -count=10 -timeout 120s`
- `go test ./internal/sessions -run '^TestSessionCrash$' -count=1 -timeout 120s`
- `go test -race ./internal/sessions -run '^TestSessionCrash$' -count=1 -timeout 120s`
- `go test -race ./internal/sessions -run '^TestSessionCrash$' -count=10 -timeout 120s`：10 个业务窗口重复 10 轮，共 100 次子进程终止/重开通过。
- `go test ./... -count=1 -timeout 90s`
- `go test -race ./... -count=1 -timeout 120s`
- `go test -race ./sdk/testdata/consumer -count=10 -timeout 120s`
- `go vet ./sdk/testdata/consumer`
- `go test -tags live ./internal/llm -count=1 -v`：live 用例实际执行并通过，未跳过；只证明该测试现有断言，不等于供应商完整认证。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 -show verbose ./...`：0 个可达漏洞；既有依赖层告警 `GO-2026-4514` 和 `GO-2026-5970` 仍在，本批未升级依赖。
- `git diff HEAD --check`：无差异卫生错误；Git 提示部分工作副本未来会进行 LF/CRLF 转换。
- 新测试另行检查冲突标记、尾空格及常见秘密模式；未读取或复制 `.test_env`，该文件由 Git 忽略。

## 未验证及阶段判断

Linux/macOS 缺少本次实际运行环境，按维护者已确认安排延期，**不计通过**。未提交、未推送，也没有为本批获取新的远程 CI 运行证据。

本批限定范围的复核追加发现“实际模型请求额度耗尽仍消费续段输入”，已通过正式红测确认并修复，再次复核未发现该范围内新的确定缺陷。完整复核不能保证不存在其他未覆盖问题。

本记录只覆盖上述修复与明确列出的验收场景，不能替代整个 P1 的逐条出口复核。P2 的恢复/审批/真实工具、P3 HTTP/SSE 和平台沙箱仍不属于已交付能力。上一轮审查中未独立证实的其他存储、只读元数据及角色校验建议，不因本批测试通过而视为已修复。
