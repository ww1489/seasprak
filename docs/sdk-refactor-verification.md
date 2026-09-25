# SDK 目录重构修复验证记录

记录日期：2026-09-24。修复基于目录重构提交 `5fd30694eabe0dffa8089e10017ff585b315c723`，本记录写入时修复尚未提交或推送。

## 当前状态

四项代码修复已落地，Windows 本地验证通过。**Linux/macOS 运行验证待补齐，不视为三平台验收通过。**

维护者已确认当前没有 Linux/macOS 测试环境，暂时保留修改，后续再补跑。当前 Windows 主机运行 `wsl --list --quiet` 退出码为 1，未安装可用 WSL；macOS 运行环境不可用。没有为这些修复触发新的远程 CI，也没有以交叉编译代替运行测试。

## 修复范围

- 外部消费测试复用根模块锁定的 `go.mod` / `go.sum`，避免新消费模块解析依赖图时需要未缓存的间接模块元数据。子进程继续使用 `GOPROXY=off`、`GOSUMDB=off`，增加 `GOWORK=off` 和 `-mod=readonly`。
- 临时消费模块使用 `go mod edit` 设置替换路径，正确处理含空格的路径；本仓库的依赖文件不添加本地替换。
- `sdk/sdk.go` 补齐消息载荷类型别名和 Kind、Status、Source 常量。
- `sdk/sdk.go` 为既有工具适配、执行范围、轮次结束钩子提供薄转发，并导出受控停止错误；不改变内部执行、权限、预算或存储实现。
- 默认外部消费测试实际验证消息构造、工具往返、授权拒绝、受控停止、取消无副作用，以及调用次数、轮次标识和预算计数。

## 已执行验证

环境：Windows 10.0.26200，amd64，Go 1.27.0。以下命令均退出 0：

- `gofmt -l .`：无输出。
- `go vet ./...`
- `go vet ./sdk/testdata/consumer`
- `go build ./...`
- `go mod verify`
- `go test ./sdk -run '^Test(ConsumerModulePreservesDependencyPinsAndPaths|IndependentConsumerUsesOnlySDK)$' -count=1`
- `go test -race ./sdk ./sdk/testdata/consumer -count=1 -timeout 120s`
- `go test ./... -count=1 -timeout 90s`
- `go test -race ./... -count=1 -timeout 120s`
- `go test -race ./sdk/testdata/consumer -count=20 -timeout 120s`
- 全新独立 `GOMODCACHE` 下的 `go test ./... -count=1 -timeout 90s`：通过。Go 在编译准备阶段正常下载依赖，消费测试子进程仍禁止模块联网。
- `go test -tags live ./internal/llm -count=1 -v`：真实模型用例实际通过，未跳过；未输出或复制凭据配置。
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 -show verbose ./...`：0 个可达漏洞。仍有既存依赖层告警 `GO-2026-4514`、`GO-2026-5970`，本次没有升级依赖。
- `git diff HEAD --check`：通过。新增测试文件另行检查了空白、冲突标记及常见敏感内容模式。

新增回归先在修复前失败，分别证明了依赖固定信息缺失、含空格替换路径无法解析和 SDK 缺少公开符号；修复后通过。

## 后续补跑清单

- [ ] 在真实 Linux 环境或对应 CI runner 上，记录修复版本的提交 SHA、系统版本、架构、Go 版本，运行格式、vet、build、普通全仓测试和全仓 race 测试。
- [ ] 在真实 macOS 环境或对应 CI runner 上，记录相同环境信息并运行上述检查。
- [ ] 两个平台都运行 `go test -race ./sdk/testdata/consumer -count=1 -timeout 120s`，使外部消费夹具中的工具执行测试也被竞态检测覆盖。
- [ ] 两个平台都使用新的独立 `GOMODCACHE` 运行 `go test ./... -count=1 -timeout 90s`，验证消费测试不依赖开发机预热的额外缓存。
- [ ] 保存运行链接或命令、退出码及失败诊断；失败项修复并重跑后，再更新本记录的验收状态。

平台测试沿用项目规定的 Go 1.27.0 和固定依赖版本。CI 不运行 live 测试，也不创建、上传或复制 `.test_env`。只有取得修复版本的两平台实际运行证据后，才可勾选对应项目。
