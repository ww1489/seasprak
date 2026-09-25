# seasprak

绑定显式工作区的 Go Agent SDK。当前实现用于 P0–P1 的受控内存工具与假模型执行，不代表真实模型、审批恢复或沙箱已经交付。

唯一公开导入路径是 `github.com/ww1489/seasprak/sdk`。

```go
import "github.com/ww1489/seasprak/sdk"

s, err := sdk.CreateAgentSession(ctx, sdk.SessionOptions{
    Workspace: ws, StateRoot: "memory", Profile: sdk.ProfileMemory, Model: model,
})
```

## 目录

```text
sdk/sdk.go                 唯一公开入口
cmd/agentd                 帮助/版本进程入口，无业务启动
internal/sessions          会话装配、调度、状态与存储
internal/agent             执行契约、预算、Eino 适配与工具
internal/llm               模型接口与配置
internal/errors            公共错误
internal/config            工程限额
```

旧路径 `session/`、`agent/`、`model/`、`extensions/` 和根目录 `sdk.go` 已删除，请改用 `sdk` 与 `internal` 对应模块。

## 当前行为

- 同一会话只执行一个顶层任务；忙时的独立输入排队。`follow_up` 在当前执行段结束后继续，`steering` 在模型轮次边界逐项消费。
- `Cancel` 传播执行取消并等待收尾。对于没有运行中执行器的 `paused` 任务，已有停止证据，或不存在已占用但未解决的工具调用时，可以安全结束为 `cancelled`；不会重跑模型或工具，也不会自动释放旧队列的 hold。无法确认停止的未知调用仍返回 `reconciliation_required`。
- 执行停止与效果已知分别保存。即使任务已结束，未知工具效果仍会阻止新的执行输入、`ContinueQueue` 和自动调度；原调用、预算及未知结果不会被取消操作清除。当前版本尚未提供公开 `Reconcile`，不能通过人工修改状态解除该限制。
- `Close` 停止受理、等待执行退出后释放写锁；超时返回错误并保留锁。关闭中的非取消任务保留为 `paused`。当前版本尚未提供公开 `Pause`/`Resume` 或持久 checkpoint 关联；新增原生断点接线测试不代表 SDK 恢复能力已经交付。重开不自动执行，仍可查询快照及重放已有幂等回执。
- 状态、输入消费、消息、工具事实和事件先持久化再发布；快照是深拷贝。幂等键按调用方隔离，重开保留原回执。
- JSONL 日志实行单写、严格序号与父链检查；残缺尾行不生效。修复先备份并验证候选，再替换日志。内存存储不跨进程保存。
- 工具必须声明 `Version` 和 JSON Schema。SDK 从定义生成模型工具信息；传入独立 `ToolInfos` 时必须一致。版本相容性依赖开发者维护显式版本，不能自动证明 Go 函数行为未改变。
- 模型适配器必须提供 `Extra["seasprak.finish"]` 为 `stop` 或 `tool_calls` 的完整结束证据。仅流结束、截断或取消不能授权工具执行。真实供应商如何产生该证据仍待适配与认证。
- `SubscribeEvents` 提供持久事件及终止错误；待送缓冲溢出时 `Err()` 返回 `resync_required`。旧 `Subscribe` 便利接口仍可使用。
- `OpenAgentSession` 只打开已有日志，不自动重跑任务。`ReadOnly` 禁止会话写操作，可在缺少模型或能力清单时浏览历史；当前仍使用单写存储句柄，不是多读者并发存储模式。

状态根默认位于 `os.UserConfigDir()/seasprak/state`，与真实工作区分离。Unix 权限位、路径检查以及 Windows 下的刷盘行为都不等于沙箱或掉电认证。当前仅支持显式 `ProfileMemory`；默认文件、命令和子 Agent 后端尚未交付。

## 验证

```powershell
go test ./... -count=1 -timeout 90s
go test -race ./... -count=1 -timeout 120s
go vet ./...
go build ./...
go mod verify
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
```

普通测试使用假模型和临时目录，不调用真实模型。`internal/llm` 中带 `live` 构建标签的测试与普通测试隔离；若仓库根存在 `.test_env`，完成实现前必须运行 `go test -tags live ./internal/llm`。`.test_env` 仅供本机测试，已忽略，不应复制到源码、测试夹具或日志。

暂停任务安全结束的交付范围、回归测试和剩余恢复能力见 [验证记录](docs/paused-cancellation-verification.md)。
