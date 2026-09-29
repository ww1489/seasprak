# 私有 Eino 上游适配器补丁

## 来源与交付状态

- 上游仓库：<https://github.com/cloudwego/eino-ext>。
- 两个模块的共同源码基线：`3603a39473c3e7b2aa3bfc11216487c94b8c7fd9`。
- Gemini：`components/model/agenticgemini/v0.2.5`，源码目录 `components/model/agenticgemini`。
- Claude：`components/model/agenticclaude/v0.1.7`，源码目录 `components/model/agenticclaude`。
- 2026-09-28 核实两个标签均指向上述基线。补丁来自 `eino-ext-p2-fixes` 隔离副本的未提交修改；该基线 hash 标识原始上游，而不是声称本地修复已经在该提交中。
- **状态口径（2026-09-29）**：下文保留 2026-09-28 的阶段测试记录，不将早期“待验”视为当前缺陷清单。当前源码含后续缓存、流式参数和响应精度恢复补丁；本轮离线收尾结果统一记录于 `docs/p2-verification.md`。macOS 与 live 按维护者要求延期，不计通过。后续修复必须同步私有副本及产品回归测试，不能只修改隔离源码。
- 产品直接引用本仓库 `internal/llm/einoext/agenticgemini` 和 `internal/llm/einoext/agenticclaude`；无独立嵌套模块，无本地路径 `replace`，不发布远程 fork。

## 复制范围与许可证

保留两个包根目录的全部生产 Go 文件，以维持原有构造、转换、流处理、选项、扩展和注册责任。供应商转换仍分别属于各适配器，不进入产品工厂或会话层。额外迁移各包新增的 `protocol_roundtrip_test.go`，并同步审查期间新增的 Gemini `signature_parts_test.go`。

- Gemini 生产文件：`consts.go`、`content_block_extra.go`、`conv.go`、`extension.go`、`function_stream.go`、`message_extra.go`、`model.go`、`option.go`、`register.go`、`response_restore.go`。
- Claude 生产文件：`consts.go`、`content_block_extra.go`、`convertor.go`、`event_convertor.go`、`extension.go`、`message_extra.go`、`model.go`、`option.go`、`register.go`、`utils.go`。
- 排除上游其他测试（包括依赖 mockey 的旧测试和 live 测试）、示例、上游模块 go.mod/go.sum、工作区文件、凭据、二进制及 Git 元数据；没有删除或替换 seasprak 原有产品红测。
- 根 `LICENSE` 原文复制自上游根目录。已核实根 `LICENSE-APACHE` 同为 Apache-2.0 正文；源树没有额外 NOTICE。所有生产文件原有版权与许可头保留，修改文件增加明确的本地修改说明；`NOTICE` 为本项目补充的来源说明。
- 复制后的 Go 文件仅统一 gofmt/换行，除下述协议修复、Gemini 缓存纯投影扩展及其修改说明外，不进行语义改写。

## 相对基线的修改与原因

### Gemini

1. `agenticgemini/conv.go`：`convAgenticFC` 保存供应商 `FunctionCall.ID` 为 `CallID`；`convFunctionToolCall` 和 `convFunctionToolResult` 在下一轮请求中分别回传 `functionCall.id` 与 `functionResponse.id`。没有供应商 ID 时，仅对有名称的新响应调用分配 `gemini_` 前缀 UUID；持久化和下一轮 call/result 回传复用该身份，不改写已有供应商 ID，不放宽产品历史完整性校验。
2. 同文件 `populateStreamingMeta`：相邻独立完整函数调用分配不同的流块索引，避免同名调用被拼接为一个调用；仅签名单独帧沿用前一调用索引。
3. `agenticgemini/content_block_extra.go`：`getThoughtSignature` 同时接受原有 `[]byte` 和 JSON 保存恢复后的 base64 字符串，避免回传时丢失 thought signature。
4. 新增 `agenticgemini/protocol_roundtrip_test.go`：调用真实 Generate/Stream，经本地 HTTP/SSE 服务覆盖有/无 ID、同名跨帧调用、下一轮两种 ID、签名及 JSON 往返、仅签名帧索引；断言实际 HTTP 次数。
5. 同步审查追加修复：`conv.go` 的 `convAgenticCandidate` 将同一响应中独立的签名 part 直接附着于前一个内容块，避免错误使用上一个响应的内容类型或产生多余索引；响应开头的独立签名仍作为上一响应的续帧。新增 `signature_parts_test.go` 覆盖首帧、文本帧后、调用帧后及完全独立签名帧，并分别验证有/无供应商 ID、拼接、JSON、下一轮 HTTP body 和调用次数。先在旧私有副本执行 `go test -mod=readonly ./internal/llm/einoext/agenticgemini -run '^TestOfflineSignatureOnlyPartsHTTPReplay$' -count=1`，退出 1，复现签名丢失、错误附着、多余调用及回传失败后，再同步隔离源码修复。

6. 2026-09-28 显式前缀资源内部扩展：`model.go` 新增无网络的 `PrefixCacheProjection`，复用 `genInputAndConf` 与 `convAgenticMessages`；`CreatePrefixCache` 改为调用同一投影函数，供产品 L1 使用相同模型/system/tools/toolConfig/contents 计算摘要，未复制供应商转换器、未新增公共 SDK API。`conv.go` 将首条 system 提取条件修正为允许单条 system，避免 system-only 缓存被错误序列化为 conversation。原许可证头完整保留。
7. 产品侧 `gemini_cache.go` 首版只缓存 system/tools/toolConfig，conversation 覆盖数为 0，全部对话与工具配对保留为等价后缀。创建省略 TTL/ExpireTime，遵循供应商默认，仅复用名称合法且返回未来 `ExpireTime` 的句柄；long 降为 short 并记录不能保证长保留。创建失败及未开始输出前的 HTTP 404 使用受同一预算约束的完整请求回退，不重复创建，不在流已输出后回放。相关产品测试位于 `internal/llm/gemini_explicit_cache_test.go`、`gemini_cache_registry_test.go`；默认测试包含真实 HTTP/SSE、工厂跨调用复用、正文等价、失效、隔离、预算、取消和并发去重。生命周期采用供应商 TTL 到期与本地淘汰，不新增后台删除或续期任务；这不代表 live 缓存命中或费用认证。

8. 2026-09-28 Gemini 网关适配：新增 `function_stream.go` 并接入 `Model.Stream`，仅接受有名称的空参数首片，后接单一 `args.arguments` 字符串匿名片段；单流单待完成调用，累计上限 1 MiB，以完整 JSON 对象提交，保留大整数、签名及调用顺序。拒绝孤立匿名片、冲突 ID、非法对象及超限；不把有名称调用的业务 `arguments` 字段当分片，不支持原生 `PartialArgs`。`gemini_identity_stream_test.go` 经真实工厂验证普通/流式 ID 分配、参数合并、JSON 持久化和下一轮相同身份回传。真实网关已通过工具调用接收，但工具结果回传仍 HTTP 400，不能宣称完整 live 通过。

9. 2026-09-28 提交前数值回放修复：`conv.go` 使用 `UseNumber` 解析工具参数和结果；`model.go` 在 Generate/Stream 发起请求前，通过请求级 `HTTPOptions.ExtraBody.contents` 保留精确历史内容，绕过锁定 genai 中间 map 转换的 float64 舍入。原有 HTTP、预算和取消链路不变。`gemini_numeric_replay_test.go` 验证下一轮实际 HTTP JSON 的整数、小数、身份和签名；Windows 模型包回归通过。此项当时仅证明出站历史回放，原始响应精度的后续实现见第 10 项；保留这一历史验证范围，不将当时的调查状态视为当前实现缺失。

10. 后续原始响应精度恢复：新增 `agenticgemini/response_restore.go` 的请求级恢复接口，由 `model.go` 在 Generate/Stream 的响应转换前调用。产品 `internal/llm/gemini_response_restore.go` 从原始响应收集有界 `json.RawMessage` 参数，按响应结构与调用身份校验后恢复精确数值，取消及关闭清理收集状态，不将原参数写入公开 Extra 或诊断。实际接线在 `internal/llm/gemini.go`，回归入口为 `gemini_numeric_response_test.go`、`gemini_numeric_boundary_test.go`、`gemini_response_restore_test.go`；源码接线不代替最终测试结果。

### Claude

1. `agenticclaude/convertor.go`：Generate 保留 `redacted_thinking` 为 reasoning 类型的内容块，正文及普通签名保持为空，供应商 opaque data 原样存于块级 `Extra["_eino_ext_agentic_claude_redacted_thinking"]`。下一轮还原为原生 redacted thinking。非法类型或冲突的正文/签名元数据显式报错，错误不包含 opaque data。
2. `agenticclaude/event_convertor.go`：Stream 保留 redacted thinking 块及供应商索引，连续两个 opaque 块不合并。
3. 新增 `agenticclaude/protocol_roundtrip_test.go`：覆盖转换、非法元数据、真实 Generate/Stream、相邻 opaque 块、普通 thinking 对照、顺序、JSON 恢复及下一轮请求原样回传；断言两次 HTTP 调用。
4. 隔离源码中的旧 `convertor_test.go` 曾修改“丢弃 redacted thinking”的断言；该旧文件未迁入产品，此私有包由新增离线测试覆盖相应行为。
5. `content_block_extra.go` 提供 `SetToolInfoCacheControl` 和 `SetContentBlockCacheControl`，通过既有 Extra 元数据传递缓存控制，供产品 `anthropic_cache.go` 使用；回归由 `internal/llm/anthropic_cache_test.go` 验证。此内部扩展不等于真实服务缓存命中或计费认证。

两个新增测试文件的 HTTP 客户端仅允许本地测试服务地址，禁止跟随重定向，超时五秒；凭据和协议数据均为合成 fixture，不读取真实凭据。

## 依赖与接线约束

- 沿用产品锁定的 Eino `v0.9.21`，未升级 Eino。
- 将已锁定的 `anthropic-sdk-go v1.75.0`、`aws-sdk-go-v2/config v1.33.6`、`aws-sdk-go-v2/credentials v1.20.6`、`bytedance/sonic v1.15.4`、`go-viper/mapstructure/v2 v2.5.0` 提升为直接依赖；`genai v1.71.0` 与 `eino-contrib/jsonschema v1.0.3` 已是直接依赖。未新增依赖版本，未运行全仓 go mod tidy。
- 原上游两个模块的生产和测试导入已统一切换，go.mod 中相应 require 已移除。2026-09-29 在 `GOPROXY=off GOTOOLCHAIN=local GOWORK=off` 下先审查 `go mod tidy -diff`：仅五项依赖的直接/间接分类及旧 Claude/Gemini 模块四行校验记录有差异；随后运行离线 tidy，同一 diff 检查无输出且 `go mod verify` 通过，依赖版本未改变。源码基线由本文件和 NOTICE 保留，不能以删除旧校验项推断上游已包含补丁。当前产品测试认证本仓库补丁副本。
- 所有生产及测试中的旧包引用均已切换，避免新旧 Claude 类型以相同名字重复注册。本次接线只替换导入及修正证据注释，不削减产品测试断言；工厂行为由独立产品任务实现。
- 产品持久化、复制、流拼接和下一轮请求必须保留完整 ContentBlock 和 Extra；不能因 Reasoning.Text 为空而删除 opaque 块，不能把 opaque data 输出到展示文本或日志。

## 不支持或未证明的边界

- Gemini 修复针对完整函数调用；Vertex 的 `partialArgs` / `willContinue` 增量参数协议仍不支持，本补丁未扩展其能力。
- Gemini 非法 base64 元数据仍沿用上游无可用签名的返回语义；当前测试不宣称损坏元数据可恢复。
- Claude 自定义 Extra 键是既有私有持久化格式；若未来上游引入正式字段，须迁移或兼容已保存的消息。
- 保留上游生产闭包中的其他供应商能力不等于产品已提供相应配置/API或完成兼容认证。当前测试是离线协议测试，不是 live 模型或 Bedrock/Vertex 的真实服务证明。
- 本次接线后的模型层、私有副本和架构测试已在 Windows/amd64 与 WSL Ubuntu 24.04 Linux/amd64 原生 Go 1.27.0 运行；macOS 及产品全仓验收仍待补。

## 本次验证

2026-09-28，Windows/amd64，Go 1.27.0，从 seasprak 仓库根设置 `GOWORK=off`：

- `go test -mod=readonly ./internal/llm/einoext/... -count=1`：通过。
- `go test -mod=readonly -race ./internal/llm/einoext/... -count=1`：通过。
- `go vet -mod=readonly ./internal/llm/einoext/...`：通过。
- `go build -mod=readonly ./internal/llm/einoext/...`：通过。

接线后再次验证：2026-09-28，Windows/amd64 和 WSL Ubuntu 24.04 Linux/amd64 均使用原生 Go 1.27.0、`GOWORK=off`：

- `go test -mod=readonly ./internal/llm/... -count=1`：两平台退出 0，包含产品模型层及两个私有包。
- `go test -mod=readonly -race ./internal/llm/... -count=1`：两平台退出 0。
- `go test -mod=readonly ./internal/architecture -count=1`：两平台退出 0，未修改架构规则。
- 传输接线测试：Windows `go test -mod=readonly ./internal/agent/eino -run 'Transport|Retry' -count=1`、Linux `go test -mod=readonly ./internal/agent/eino -run Transport -count=1`：退出 0。
- `go vet -mod=readonly ./internal/llm/...`、`go build -mod=readonly ./internal/llm/...`：两平台退出 0。
- Windows 额外执行 `go test -mod=readonly ./internal/llm -run '^TestP2(FixedAdapter|Factory)' -count=1 -v`：退出 0，确认原协议红测及真实产品工厂回传测试实际执行；未删除或削减断言。
- Windows `gofmt -l .` 无输出；`git diff HEAD --check` 退出 0。
- 全产品 `*.go` 中无两个旧上游 import，go.mod 已移除旧 require；历史文档和 go.sum 中保留来源/校验记录。

上述证据不代表产品全仓验收；全仓静态检查、构建、普通/race 测试、安全扫描、存在本地凭据时的 live 测试及 macOS 运行证据须由主线程统一执行并报告。独立审查结论与最终交付验收由主线程汇总，状态保持待验。

## Gemini 显式缓存子项验证（2026-09-28）

- 先运行 `go test ./internal/llm -run TestGeminiExplicitCache -count=1`，退出 1，全部显式调用在 `ResolveOptions` 门禁失败；再实现 opt-in、纯投影和 registry。额外红测证明创建者取消曾错误取消独立等待者，修复后等待者改走自身预算下完整请求。
- Windows 与 WSL Linux 原生 Go 1.27.0：`go test ./internal/llm/... -count=1`、`go test -race ./internal/llm/... -count=1`、`go vet ./internal/llm/...`、`go build ./internal/llm/...`、`go test ./internal/architecture -count=1` 均通过。
- 修改 Go 文件的 `gofmt -l` 无输出；跟踪文件定向 `git diff HEAD --check` 通过，新缓存文件检查无冲突标记或真实凭据。
- 本段仅为离线子项证据；整仓/live/macOS 与独立审查仍由主线程汇总，不能据此宣称全部 Step20 或 Gemini 协议认证完成。

## 切回上游的条件

1. 上游已发布包含本文件完整补丁清单的等效版本，核实发布 tag/commit 和依赖兼容性；不再仅以早期四项协议修复为准。
2. 使用正式上游版本在 `GOWORK=off` 下通过此处离线协议测试及 seasprak 产品回归；至少覆盖 ID、流块边界、thought signature、opaque thinking、匿名工具参数分片、精确数值出站与响应恢复、显式缓存投影和 Claude 缓存元数据。持久化及下一轮实际 HTTP 请求必须保持这些契约，不能只验证适配器内存转换。
3. 对 Claude 旧 Extra 键和任何注册类型的历史数据给出兼容迁移验证，不直接丢弃旧消息元数据。
4. 完成产品全量普通/race、静态、安全、live（适用时）及 Linux/Windows/macOS 运行验证后，统一替换导入、移除此私有源码和不再需要的直接依赖，更新来源记录。禁止仅以“上游已修复”或上游测试通过作为切回依据。
