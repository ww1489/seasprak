# Pi Host Command Context Implementation Plan

**目标：** 让用户主动提交的 `!command` / `!!command` 在模型正常运行期间可执行，并像 Pi 一样延后进入模型上下文，同时保护审批检查点的一致性。

**架构：** shell 进程仍由 `sessions.ExecuteCommand` 执行。命令结果先以不可变 `host_command_result` 控制记录持久保存，不追加当前 Eino 检查点使用的普通消息；原 Trace 最终终态后、下一独立输入前，在现有 mailbox 内按结果 CommitSeq 消费当前分支结果。审批暂停、普通暂停及同 Trace 续段均不消费。审批等待及已答复未 Resume 期间继续拒绝新命令。

**全局约束：** 不放宽 checkpoint 校验；不重新执行已完成 shell；不在 mailbox 等待外部进程；保留 `!` 进入上下文、`!!` 排除上下文的语义；不读取 `.test_env`；完成前运行普通、race、vet、build 和 Linux 专项。

## Task 1：建立失败回归

**文件：** `internal/sessions/commands_checkpoint_test.go`、对应 state 测试。

- 增加命令在模型运行期间完成时的屏障测试：shell 实际启动并完成，模型仍未结束；断言命令事实可恢复、当前审批/检查点不被伪造为可恢复，命令不重复执行。
- 增加 `!!` 的上下文排除断言，确认仍可持久读取但不进入 `agent.ConvertToLLM` 的模型输入。
- 增加取消、重新打开和重复恢复断言，要求命令结果只提交一次。
- 先运行受影响测试，确认新断言因当前直接 `AppendMessage` 或恢复路径缺少待提交事实而失败。

## Task 2：增加最小可恢复待提交命令事实

**文件：** `internal/sessions/state` 的现有记录/回放文件及新增同职责测试；`internal/sessions/commands.go`。

- 复用现有 `ControlRecords`/事件记录链，增加版本化的待提交 host-command 事实，不新增 Store 接口。
- 记录命令 ID、作用域、结果、`excludeFromContext` 标志和提交状态；身份冲突、重复提交和非法作用域必须拒绝。
- shell 结束后先提交待提交事实，再由现有 mailbox 完成模型历史提交；保存失败不重跑 shell，并保留结果与明确错误。
- 审批等待期间的新命令继续在 mailbox 内原子拒绝。

## Task 3：接入终态消费边界和严格恢复校验

**文件：** `internal/sessions/commands_context.go`、`coordinator.go`、`recovery.go`、`resume_validation.go`、`commands_resume.go` 及对应测试。

- 原 Trace 最终终态后，在下一独立输入前消费当前 session/branch 的待提交命令事实；`!` 生成含完整命令及实际输出的 user 模型消息，`!!` 只保留持久事实并排除模型输入；旧非 shell 命令投影保持不变。
- 按用户要求对齐 Pi 的消费时机：审批暂停、普通暂停、答复及 Resume 后同一 Trace 继续期间保持待提交，不向恢复中的模型请求插入命令。此结论已核对 Pi `_runAgentPrompt` 的 finally 与新 prompt 前 flush 调用。严格恢复旧 checkpoint，禁止重新绑定 LeafID。
- 一次原子 commit 写入 `host_command_consumed` 标记和所有应进入模型的普通 Entry，按原结果 CommitSeq 排序并完整匹配同 session/branch/ID 的不可变事实；不追加第二个 `message.finalized` 事件。重复消费不写入，Snapshot 按原事件顺序合并且 ID 唯一。
- 保存或消费被拒绝、写入成功但确认丢失时均不发布候选状态、不继续启动模型。重开按实际 durable 记录恢复，既不自动消费也不启动 shell；已保存的结果及日志引用保留。下一独立输入前消费失败则不受理该输入。
- 命令先启动后进入审批时，审批期间不接收新命令；已启动命令完成后只保存事实，直到原 Trace 最终结束才消费。Cancel/失败终态也保留真实结果，Close 造成暂停则仍不消费。
- 恢复校验只接受严格验证的 `host_command_result` 控制提交：无 Entry/分支变更、唯一匹配的展示事件、正确版本和身份，并与已回放不可变事实相等；消费标记不得成为 checkpoint 后置变更白名单。

## Task 4：验证

- Windows：受影响 `sessions`、`sessions/state`、SDK consumer 的普通和 race 测试。
- Linux/WSL：同范围普通和 race 测试。
- 稳定后运行 `gofmt -l .`、`go vet ./...`、`go build ./...`、`go test ./...`、`go test -race ./...`、`govulncheck ./...`，并检查 `git diff HEAD --check` 及新文件敏感内容。
- live、macOS 按既有批准例外记录，不修改凭据，不提交生成物。
