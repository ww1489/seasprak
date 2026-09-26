# P2 实施记录与验证证据

记录日期：2026-09-25—2026-09-26；最新复核：2026-09-26。环境：Windows 10.0.26200、Go 1.27.0 windows/amd64。
基线：`feat/p0-p1-runtime` / `0541e3d`。

## 当前状态

2026-09-26 最新：Steps 9–10 的缺失实现、四项新增回归及会话资源批量恢复接线均已修复。最终实现交付后，主执行者重新核对调用链并独立执行专项 race 二十轮及全仓强制检查，退出码均为 0；默认测试集已无第 19 节记录的失败。详情见第 20 节。Linux/macOS 运行证据仍缺，Steps 9–10 待办保留 in_progress 并注明平台待验；Step 11 本轮未启动。模型层复验及设计依据修正见第 18 节。

Steps 1–2 已完成依赖/框架边界核实与持久记录专项实现，并通过第 8 节记录的全仓普通/race 验证。按用户后续要求，依赖已升级到第 7 节版本；OpenAI SDK 保留明确兼容例外。流式 Interrupt 探针已补齐，同时保留 reader 内中断重跑 sibling 的原生限制。Steps 3–4 已实现原子调用预算、执行段隔离、活动时间持久预留，以及只读/blob 存储基础，并通过第 11 节的独立 Windows 验证。恢复段来源映射仍需 Step 13 接线，符号链接权限用例和其他平台认证尚待补齐。Steps 5–7 的开发与 Windows 确定性验证已完成：Step 5 的目录/凭据/选项已完成本地验收；Step 6 的有界用量采集与唯一物理请求预算已接线并通过第 13 节的独立 Windows 验收；Step 7 产品 Chat 工厂已实现并通过第 15 节的独立 Windows 全仓验证，真实 endpoint 产品工厂认证仍待 Step 23。Step 8 已补齐 attempt 登记/终态/证据、原子接纳、实时快照和真实工厂受限重试，并通过第17节独立Windows全仓验证；真实端点及其他平台认证仍待最终验收。Steps 9–10 实现已恢复，新增回归修复及最终 Windows 独立复验通过，平台认证仍待补齐；Steps 11–23 尚未实施，本记录不是 P2 完成证明。

当前变更包括依赖、框架探针、产品终止适配、持久记录及在途预算/存储工作，没有提交或推送。原生 Immediate 限制不是由本次修改引入；中间尝试直接接 Immediate 曾破坏 P1 的真实退出等待，已由既有回归发现并修正。以下按实施时间保留历史命令与失败过程，旧版本结果不替代当前验收。

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
