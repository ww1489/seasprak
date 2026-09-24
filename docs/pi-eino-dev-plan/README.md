# pi 思想 + Eino Agent 底座开发方案

状态：2026-09-24，按已确认方案完整重写。本文档是待实现的技术规格；本轮没有创建产品 Go module 或实现产品功能。

需求基线：[PRD 总纲](../pi-eino-prd/README.md)、[十二章需求](../pi-eino-prd/roadmap.md)、[系统不变量与 38 个系统场景](../pi-eino-prd/system-review.md)。产品行为以 PRD 和用户后续明确决定为准。本方案固定工程选型、接口、数据和实施顺序；源码事实、设计决定、待运行认证分别标明。

## 1. 已确定的技术路线

宿主机 Go 程序提供可嵌入 SDK 和本地 HTTP/SSE 服务。模型采用 Eino/eino-ext 的 Agentic 路径，执行复用 DeepAgent、TypedRunner 与 TurnLoop。Session 必须绑定工作区，默认具备文件、命令、TODO、通用子 Agent。主程序始终在宿主机；shell 可选原生沙箱或本机 Docker 容器沙箱，文件工具与 shell 通过明确映射操作相同工作区/产物。

核心按 L1 模型能力、L2 通用 Agent、L3 应用会话递进；**实际 import 方向为 L3 → L2 → L1**。HTTP 与平台实现由外层装配。AgentSession 协调，SessionManager 保存，Agent 执行，ResourceLoader 加载，ExtensionRegistry 登记，CreateAgentSession 创建，职责不合并为 Host。

## 2. 阅读与实现导航

| 文档 | 要解决的实现问题 |
| --- | --- |
| [01 技术选型与总体架构](01-architecture.md) | 依赖版本、包边界、部署、创建及关闭 |
| [02 数据契约与消息](02-contracts-and-messages.md) | 身份、三类标准消息、产品数据和错误 |
| [03 执行循环与调度](03-runtime-and-scheduling.md) | Eino 接线、输入、轮后处理、取消和收尾 |
| [04 模型接入](04-model-access.md) | 协议、流、思考、缓存、计量与重试 |
| [05 工具与 Operations](05-tools-and-operations.md) | 默认工具、校验授权、并发、结果与产物 |
| [06 事件与外部接口](06-events-and-api.md) | 两条管道、Go SDK、HTTP、SSE、Web 验证 |
| [07 上下文与 Skills](07-context-and-skills.md) | 唯一上下文管道、规范、技能、预览与预算 |
| [08 压缩与摘要](08-compaction.md) | 合法切点、H/P/K、增量摘要、文件事实 |
| [09 会话与恢复](09-persistence-and-recovery.md) | JSONL、历史树、checkpoint、核对及恢复 |
| [10 扩展与 Workflow](10-extensions-and-workflows.md) | 代码登记、generation、子 Agent、动态定义与两种使用方式 |
| [11 安全与沙箱](11-security-and-sandbox.md) | 一次许可、Auto、基于 Zero 的原生后端、容器路径与保护 |
| [12 开发顺序与验收](12-delivery-and-validation.md) | 实施批次、默认值、测试和认证门槛 |
| [逐条需求覆盖表](requirements-coverage.md) | PRD 条款/章节 → 技术落点 → 图/接口 → 验收断言 |

本目录替换原三篇方案，不保留两套互相竞争的实现说明。原始方向文档仍可用于了解背景，不能覆盖这里的技术决定。

## 3. 跨章主定义

| 契约 | 唯一主定义 | 其他章节如何使用 |
| --- | --- | --- |
| 身份、消息、错误与执行结果 | 02 | 使用同名字段，不发明同义 Run/Task 对象 |
| Trace/Turn/输入与队列 | 03 | hooks、恢复和 HTTP 按其状态条件调用 |
| 模型协议、思考、缓存与用量 | 04 | 上下文使用已解析选项；上层不识别厂商事件 |
| 工具完整执行边界 | 05 | Workflow、子 Agent、命令都复用 |
| 事件、hooks 组合与接入 | 06 | 事实先提交再发布；观察订阅不承担保存 |
| 完整请求构建和预算 | 07 | 压缩后重新构造，不多处追加提示词 |
| 摘要候选及文件记录 | 08 | 历史仅保存已校验候选 |
| 提交、分支与恢复资格 | 09 | 取消、审批、核对按一致提交协作 |
| 注册、资源版本和委派 | 10 | 当前 Trace 不读取候选新版本 |
| 授权与真实执行限制 | 11 | 工具可见不等于允许执行 |
| 工程默认值及验收 | 12 | 各章引用，不重复维护数值 |

## 4. 当前证据与限制

已核查本地 pi、Eino、eino-ext、DSH、Zero 和 Coze Studio 源码。规划阶段在 Windows / Go 1.27.0 上运行 Eino 定向测试：普通非抢占 Push、停止后恢复、业务中断恢复、定向 Resume、事件处理错误、工具后 hook、AfterAgent、Agentic 子 Agent 事件；涉及的 `adk` 和 `adk/prebuilt/deep` 两个包通过。命令及用例边界见 [12](12-delivery-and-validation.md#evidence)。

这不是产品适配完成的证明。真实供应商缓存/usage、HTTP/SSE、JSONL 崩溃一致性、基于 Zero 的 Windows helper、Linux/macOS 和容器沙箱、Coze Canvas 兼容均有明确实现与验收任务，尚未认证。默认值是开发者可替换的工程保护值，不是成功率、延迟或成本承诺。

## 5. 本轮确认的部署决定

- Windows 使用 Go 主程序和 Go 辅助 exe，不依赖 Node.js/Cordis。
- 主程序直接运行在宿主机，容器用于 shell 沙箱；不增加容器内主服务或分布式执行平台。
- 保留 workspace-write + ask、Auto 关闭、满足必要保护的 partial 可用、不可用时拒绝执行。
- 前端只交付公开接入契约及测试页，A2UI 预留在外层，CLI/TUI 和完整 UI 不在本轮产品范围。
