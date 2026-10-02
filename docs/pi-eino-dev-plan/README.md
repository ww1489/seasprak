# pi 思想 + Eino Agent 底座开发方案

状态：2026-10-02，按最终源码同步三层 SDK 与两类同级 Agent。Code Agent 与中立存储位于 `internal/codeagent`、`internal/storage`；Code 旧工作流 target、定义/编译/图、节点状态与执行/恢复分支已退出，独立 `internal/workflowagent`、公开工厂、Web 三类资源路由及独立视图已接线，有对应默认测试和 SDK 消费者。当前签名及 DTO 见 06/10/13；最终平台、浏览器和真实模型认证见 [P3 验证记录](../p3-verification.md)，不把接线或文档视为整体 P3 通过。

需求基线：[PRD 总纲](../pi-eino-prd/README.md)、[十二章需求](../pi-eino-prd/roadmap.md)、[系统不变量与 38 个系统场景](../pi-eino-prd/system-review.md)。产品行为以 PRD 和用户后续明确决定为准。本方案固定工程选型、接口、数据和实施顺序；源码事实、设计决定、待运行认证分别标明。

## 1. 已确定的技术路线

宿主机 Go 程序提供可嵌入 SDK 和本地 HTTP/SSE 服务。模型采用 Eino/eino-ext 的 Agentic 路径，Code Agent 执行复用 DeepAgent、TypedRunner 与 TurnLoop，Workflow Agent 复用 Eino Graph。两类运行均必须显式绑定各自工作区；Code Agent 默认具备文件、命令、TODO、通用子 Agent。主程序始终在宿主机；shell 可选原生沙箱或本机 Docker 容器沙箱，文件工具与 shell 通过明确映射操作所属工作区/产物。

L1 为 `internal/llm`；L2 为 `internal/agent`、`internal/agent/eino` 与 `internal/agent/tools`，保留通用 `Agent` 名称。L3 的 Code Agent（当前目录 `internal/codeagent`，已由 `internal/sessions` 机械迁移）和 Workflow Agent（当前目录 `internal/workflowagent`）同级且互不导入，分别管理状态、日志写入者、自身记录、generation、预算、审批、取消和恢复；Code Agent 管理聊天历史/分支，Workflow Agent 管理图运行和节点结果，不自动增加聊天循环。共享 `internal/storage` 契约及 JSONL/memory 后端不拥有调度，不形成跨两类运行的冻结、全树 checkpoint 或预算原子提交承诺。

Code Agent 继续使用 `AgentSession`、`CreateAgentSession` / `OpenAgentSession`；独立工作流已公开 `WorkflowAgent`、`WorkflowOptions`、`CreateWorkflowAgent` / `OpenWorkflowAgent`，有实际受控运行及消费者测试。唯一公开 import 路径仍是 `github.com/ww1489/seasprak/sdk`，且 `sdk/sdk.go` 是同 module 唯一公开生产入口。外部 consumer 只 import `sdk`；仓库内 Web 依赖两类受控入口而不 import `sdk` 或写 manager。Web 分别消费 `/v1/sessions`、工作流定义 `/v1/workflows` 和独立运行 `/v1/workflow-runs`，不再通过 `AgentSession.targetAgent` 执行工作流。

业务可通过现有受控工具组合独立 Agent，SDK 不新增跨 Agent 调用端口或联动调度框架。工具只返回匹配父调用的一条结果或业务引用；外层批准不授权内层效果。普通受控子 Agent 和工作流内静态子流程、条件、汇合、节点级暂停/审批/显式恢复保留；完整子树恢复、跨任务审批、补偿及跨运行总预算归业务。旧开发数据不兼容迁移，不自动清目录；本轮不启动 P4/P5，也不提前交付 Coze、动态加载、热重载或业务补参。

## 2. 阅读与实现导航

2026-09-29 P3 已批准的阶段调整、网络细节和固定 A2UI 来源见 [13 P3 Web 接入契约](13-p3-web-contract.md)；实施证据见 [P3 验证记录](../p3-verification.md)。以下原规划状态和上游验证保留历史口径，不代表当前产品交付状态。

| 文档 | 要解决的实现问题 |
| --- | --- |
| [01 技术选型与总体架构](01-architecture.md) | 依赖版本、包边界、部署、创建及关闭 |
| [02 数据契约与消息](02-contracts-and-messages.md) | 身份、三类标准消息、产品数据和错误 |
| [03 执行循环与调度](03-runtime-and-scheduling.md) | Code Agent 普通输入队列、轮后处理、取消和收尾；工作流控制独立 |
| [04 模型接入](04-model-access.md) | 协议、流、思考、缓存、计量与重试 |
| [05 工具与 Operations](05-tools-and-operations.md) | 默认工具、校验授权、并发、结果与产物 |
| [06 事件与外部接口](06-events-and-api.md) | 两类所属事件/订阅、控制 hooks、Go SDK、HTTP、SSE、Web 验证；审批仅实例临时事实 |
| [07 上下文与 Skills](07-context-and-skills.md) | 唯一上下文管道、规范、技能、预览与预算 |
| [08 压缩与摘要](08-compaction.md) | 合法切点、H/P/K、增量摘要、文件事实 |
| [09 会话与恢复](09-persistence-and-recovery.md) | 共享后端、两类独立日志、Code 历史树、各自恢复点及核对 |
| [10 扩展与 Workflow](10-extensions-and-workflows.md) | 普通子 Agent 注册/版本、独立工作流执行与业务工具组合 |
| [11 安全与沙箱](11-security-and-sandbox.md) | 一次许可、Auto、基于 Zero 的原生后端、容器路径与保护 |
| [12 开发顺序与验收](12-delivery-and-validation.md) | 实施批次、默认值、测试和认证门槛 |
| [逐条需求覆盖表](requirements-coverage.md) | PRD 条款/章节 → 技术落点 → 图/接口 → 验收断言 |

本目录替换原三篇方案，不保留两套互相竞争的实现说明。原始方向文档仍可用于了解背景，不能覆盖这里的技术决定。

## 3. 跨章主定义

| 契约 | 唯一主定义 | 其他章节如何使用 |
| --- | --- | --- |
| 身份、消息、错误与执行结果 | 02 | 两类作用域分别归属；复用字段语义，不合并可变运行对象 |
| Code Agent Trace/Turn/输入与队列 | 03 | 普通 hooks、恢复和 HTTP 按其状态条件调用；工作流不入该队列 |
| 模型协议、思考、缓存与用量 | 04 | 上下文使用已解析选项；上层不识别厂商事件 |
| 工具完整执行边界 | 05 | Workflow、子 Agent、命令都复用 |
| 事件、hooks 组合与接入 | 06 | 事实先提交再发布；观察订阅不承担保存 |
| 完整请求构建和预算 | 07 | 压缩后重新构造，不多处追加提示词 |
| 摘要候选及文件记录 | 08 | 历史仅保存已校验候选 |
| 提交、Code 分支与所属恢复资格 | 09 | 取消、审批、核对仅在各自日志内一致提交；不承诺跨两类运行事务 |
| 普通注册与各自资源版本 | 10 | Code 普通委派与 Workflow 定义/节点绑定分开；活动运行不读取候选新版本 |
| 授权与真实执行限制 | 11 | 工具可见不等于允许执行 |
| 工程默认值及验收 | 12 | 各章引用，不重复维护数值 |

## 4. 当前证据与限制

已核查本地 pi、Eino、eino-ext、DSH、Zero 和 Coze Studio 源码。规划阶段在 Windows / Go 1.27.0 上运行 Eino 定向测试：普通非抢占 Push、停止后恢复、业务中断恢复、定向 Resume、事件处理错误、工具后 hook、AfterAgent、Agentic 子 Agent 事件；涉及的 `adk` 和 `adk/prebuilt/deep` 两个包通过。命令及用例边界见 [12](12-delivery-and-validation.md#evidence)。

上述规划阶段测试不证明产品适配完成。后续 P2/P3 的产品路径证据、失败和未验证项以各自验证记录为准；HTTP/SSE、JSONL、供应商缓存/usage 等不能一概沿用早期“尚未实现”状态，也不能把历史测试视为本次独立分层迁移认证。Code 原生预检的生命周期回调隔离已按批准 Step9 获有界接受，唯一方法与限制见 [09 §checkpoint](09-persistence-and-recovery.md#checkpoint)：固定 Eino v0.9.21、每次新建且单次使用探针、原目标根资格，宿主 codec 仍可先执行，不认证任意宿主零执行或整树。当前文件访问已接受与待验范围见 [09 §layout](09-persistence-and-recovery.md#layout)；Blob H1/M1、13.2e清单 publisher、Workflow.Close 安全错误与可写创建父目录同步均已独立复核并获父两平台实际证据的有界接受，清单通用失败阶段旧 ID/mode 测试补强建议保留，Repair 完整旧 WRITE_THROUGH 替换继续暂停。父最终冻结源 Windows／实际 WSL Linux 全仓与显式消费者及原 Step14 目标均已通过，Windows 文件 symlink/junction 实际执行，历史失败/权限 skip 保留；macOS 延期未验，P3 仍未完成。唯一授权四协议 live 与原样完整 E2E 已各执行一次并通过，未自动重跑或追加调用，顺序/计数、安全扫描剩余模块告警与外部 Node 原生 env-file 桥接限制见 [12 当前证据与限制](12-delivery-and-validation.md#当前证据与限制)，Gemini 本轮请求零；Coze、动态加载、热重载和业务补参仍为未来阶段。默认值是开发者可替换的工程保护值，不是成功率、延迟或成本承诺。

## 5. 本轮确认的部署决定

- Windows 使用 Go 主程序和 Go 辅助 exe，不依赖 Node.js/Cordis。
- 主程序直接运行在宿主机，容器用于 shell 沙箱；不增加容器内主服务或分布式执行平台。
- 保留 workspace-write + ask、Auto 关闭、满足必要保护的 partial 可用、不可用时拒绝执行。
- 前端交付公开接入契约及测试页；2026-09-29 已批准 P3 采用 Eino 示例 A2UI 子集，替换原仅预留决定，仍位于外层。CLI/TUI 和完整通用 UI 不在本轮产品范围。
