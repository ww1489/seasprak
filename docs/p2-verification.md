# P2 实施记录与验证证据

记录日期：2026-09-25—2026-09-26；最新复核：2026-09-26。环境：Windows 10.0.26200、Go 1.27.0 windows/amd64。
基线：`feat/p0-p1-runtime` / `0541e3d`。

## 当前状态

2026-09-26 最新：Steps 9–10 的 Windows 修复和独立复验见第 20 节，Linux/macOS 运行证据仍缺。Step 11 已部分实施：普通与增强型同步工具接口进入同一执行器，新增行为测试和本轮 Windows 验证通过；两种原生流式接口仍明确拒绝。同步接口上的 SDK 实际输出片段接线及边界见末尾附记，不能以此标记原 Step 11 四接口验收完成。审批中断和原生 reader 提前 Close 后的后端结算仍未满足；维护者随后要求在已验证同步路径继续，Step 12 的条件性交付及 Step 13 的显式恢复接线见末尾执行补充。阶段性提交后的 CI 失败及本轮结论见第 21 节；先前章节保留发生时的历史状态。

Steps 1–2 已完成依赖/框架边界核实与持久记录专项实现，并通过第 8 节记录的全仓普通/race 验证。按用户后续要求，依赖已升级到第 7 节版本；OpenAI SDK 保留明确兼容例外。流式 Interrupt 探针已补齐，同时保留 reader 内中断重跑 sibling 的原生限制。Steps 3–4 已实现原子调用预算、执行段隔离、活动时间持久预留，以及只读/blob 存储基础，并通过第 11 节的独立 Windows 验证。恢复段来源映射已在 Step 13 同步路径接线，符号链接权限用例和其他平台认证尚待补齐。Steps 5–7 的开发与 Windows 确定性验证已完成：Step 5 的目录/凭据/选项已完成本地验收；Step 6 的有界用量采集与唯一物理请求预算已接线并通过第 13 节的独立 Windows 验收；Step 7 产品 Chat 工厂已实现并通过第 15 节的独立 Windows 全仓验证，真实 endpoint 产品工厂认证仍待 Step 23。Step 8 已补齐 attempt 登记/终态/证据、原子接纳、实时快照和真实工厂受限重试，并通过第 17 节独立 Windows 全仓验证；真实端点及其他平台认证仍待最终验收。Steps 9–10 的 Windows 修复及未完成的平台认证见第 20 节；Step 11 的部分交付与阻塞见第 21 节。Steps 12–23 的原计划全范围交付尚未完成；末尾附记记录 Step 11 内容流、Step 12 同步路径暂停与 Step 13 严格显式恢复的局部交付，不代表原计划 Step 23 验收。本记录不是 P2 完成证明。

此前各阶段的变更已包含在阶段性提交中；本轮 Step 11 同步内容流、Steps 12–13 同步暂停/恢复改动及验证记录尚未提交或推送。原生 Immediate 限制不是由本次修改引入；中间尝试直接接 Immediate 曾破坏 P1 的真实退出等待，已由既有回归发现并修正。以下按实施时间保留历史命令与失败过程，旧版本结果不替代当前验收。

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
