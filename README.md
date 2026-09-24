# seasprak

绑定显式工作区的 Go Agent SDK。当前实现用于 P0–P1 的受控内存工具与假模型执行，不代表真实模型、审批恢复或沙箱已经交付。

## 当前行为

- 同一会话只执行一个顶层任务；忙时的独立输入排队。`follow_up` 在当前执行段结束后继续，`steering` 在模型轮次边界逐项消费。
- `Cancel` 传播执行取消并等待收尾。`Close` 停止受理、等待执行退出后释放写锁；超时返回错误并保留锁。关闭中的非取消任务保留为 `paused`，当前版本不支持恢复执行。
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

普通测试使用假模型和临时目录，不调用真实模型。`model/live` 受 `live` 构建标签隔离；本轮修复没有执行它。`.test_env` 仅供本机测试，已忽略，不应复制到源码、测试夹具或日志。
