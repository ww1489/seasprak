# seasprak

绑定显式工作区的 Go Agent SDK。模型层和通用执行层之上已实现同级的 `seasprak-code-agent`（代码助手会话）与 `seasprak-workflow-agent`（独立工作流），由外侧 Web 业务应用分别消费。两类运行独立持有状态、日志、预算、审批和恢复，复杂跨运行编排与高级子树恢复由业务实现。Code 会话内工作流定义、节点状态及执行分支已退出，定义、编译和 Eino 图由独立 Workflow 实现持有；Web 的代码会话、工作流定义与独立运行路由及视图已接通，并有对应默认测试。唯一公开导入路径是 `github.com/ww1489/seasprak/sdk`。

当前正在收尾基础 Web 与已有功能修复，P3 尚未完成。已实现功能、未验证项及历史结果以 [P3 验证记录](docs/p3-verification.md) 为准，不能把本说明当作完整交付认证。

## 基础接入

宿主提供显式工作区和已配置的模型。普通任务无需自行构造 Eino runner、预算账本或持久状态管理器：

```go
import "github.com/ww1489/seasprak/sdk"

s, err := sdk.CreateAgentSession(ctx, sdk.SessionOptions{
    Workspace: ws, StateRoot: stateRoot,
    Profile: sdk.ProfileMemory, Model: model,
    Instruction: "按用户要求完成任务。",
})
```

`ws` 和 `stateRoot` 是宿主提供的绝对目录，且状态根与工作区分离；`model` 是已装配的 `sdk.Model`。`ProfileMemory` 表示显式裁剪掉默认文件／进程后端；使用真实状态目录时，会话历史仍由 JSONL 持久保存。只有 `StateRoot: "memory"` 使用不跨进程保存的内存存储。

创建成功后，使用 `SubmitInput` 提交输入，使用 `SubscribeEvents` 接收事件、`Snapshot` 查询状态、`Cancel` 停止任务、`Close` 释放会话。已有历史使用 `OpenAgentSession` 打开；打开不会自动执行或恢复任务。

模型配置与凭据解析、业务工具由宿主注入。普通接入无需了解内部身份、审批地址或检查点格式。L1（模型层）与 L2（通用执行层）独立使用能力保留。`WorkflowAgent`、`CreateWorkflowAgent` 和 `OpenWorkflowAgent` 已接入源码，工作流用自己的结构化 `WorkflowInputCommand`、快照与生命周期，不借用 Code Session/Trace/Turn；默认测试及 SDK 消费者覆盖独立执行、重开与受控业务工具组合。业务组合分别校验权限与预算，外层批准不授权内层效果。旧 `CompileWorkflowTarget`、会话内工作流目标和节点状态已退出，旧工作流记录不兼容时明确拒绝，其他普通会话能力保留。最终认证见验证记录。

## 独立工作流接入

通过 `sdk.CreateWorkflowAgent(ctx, sdk.WorkflowOptions{...})` 显式提供 Workspace、StateRoot、Definition、Principal 和可信构建指纹；模型、工具、固定子流程分别从 Models/Tools/Subflows 装配。创建仅保存初始记录，再用 `WorkflowInputCommand{Input: json.RawMessage(...), IdempotencyKey: ..., Principal: ...}` 提交一个 JSON 对象，不传聊天 content、targetAgent、steering 或 follow-up。当前定义字段与可用 SDK 示例见 [开发方案 10](docs/pi-eino-dev-plan/10-extensions-and-workflows.md#registration)。一个运行只受理一次输入，同键重试返回不可变原回执，新执行另建运行。

查询使用该对象的 `Snapshot` / `SubscribeFrom`。打开原 RunID 保留原绑定；可写或只读 `OpenWorkflowAgent` 都不自动执行/Resume。Pause 后依据当前快照回答本运行的 interaction，再由用户显式 Resume；回答本身不继续执行，批准仅当前实例有效。Cancel/Close 等待实际退出，未知效果保持阻止盲重跑。两类分别写 `sessions/<sid>` 与 `workflow-runs/<rid>`；业务受控工具组合只返回匹配父调用的一条结果或引用，外层批准不转移内层授权。

完整职责边界和高级能力的业务归属见 [SDK 与业务层的职责边界](docs/sdk-scope.md)。

## 基础约束

- 同一会话只执行一个顶层任务；忙时的独立输入排队，已有 follow-up 和 steering 行为保留。
- 工具仍经过冻结参数、授权、预算和结果记录。流中断或不完整模型响应不能授权工具执行。
- `Cancel` / `Close` 等待实际执行收尾；调用方超时不等于工具已经停止。已发生的业务副作用不承诺自动回滚。
- 状态与持久事件先提交再发布。只读打开和浏览不写入、不启动模型、不自动恢复。
- 暂停、恢复与核对只在已有条件满足时可用；未知工具效果不能通过重新运行或人工成功标签消除。
- 默认文件／进程后端尚不可用，当前执行须显式使用 `ProfileMemory` 并注入实际允许的工具。工作区绑定和目录权限保护不等于原生沙箱认证。

## 目录

```text
sdk/sdk.go                 唯一公开生产入口
cmd/agentd                 帮助／版本入口，无业务启动
cmd/web                    本机 Web 启动入口
internal/codeagent         代码会话协调与状态，不内置工作流
internal/workflowagent     独立工作流定义、编译、图与运行
internal/storage           共用存储契约及 JSONL/memory 后端
internal/agent             执行契约、预算、Eino 适配与工具
internal/llm               模型协议、配置与计量
internal/web               本机 HTTP、事件与页面适配
internal/errors            公共错误
internal/config            工程限额
```

当前源码具有 `internal/codeagent → internal/agent → internal/llm` 和同级 `internal/workflowagent → internal/agent → internal/llm` 两条受控执行路径，共享存储位于 `internal/storage`。Web 拥有应用目录、创建去重、展示 metadata 和附件上传管理；Code 保留附件准入、模型展开及原授权。代码会话与工作流分别使用 `sessions/<sid>` 和 `workflow-runs/<rid>` 日志，不共享写入者、预算、审批或恢复。业务只导入公开 SDK，Web 服务端调用受控上层入口，不直接写执行状态。实现与默认测试不等于完整 P3 认证；旧开发数据不迁移、不自动清理。页面不另建执行循环。

## 本机页面

页面启动、受信配置和认证方式见 [本机 Web 启动](docs/p3-web-startup.md)。页面是 SDK 能力的外层接入，浏览会话与显式取得控制分别处理；刷新或重连不会自动执行输入或恢复任务。

## 验证

```powershell
gofmt -l .
go vet ./...
go build ./...
go test ./... ./sdk/testdata/consumer -count=1
go test -race ./... ./sdk/testdata/consumer -count=1
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
git diff HEAD --check
```

普通测试使用本地可控模型、传输和临时目录，不调用真实供应商。前端另在 `web/` 执行 typecheck、unit、build 和完整浏览器测试；实际平台验证、当前真实模型授权范围及延期项见验证记录。带 `live` 标签的模型测试与默认套件隔离，`.test_env` 只供获准本机测试，禁止进入源码、夹具、输出或提交。
