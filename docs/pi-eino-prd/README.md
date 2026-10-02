# pi-eino：通用 Agent 底座 PRD

| 项目 | 内容 |
| --- | --- |
| 状态 | 2026-10-02 源码同步：Code Agent 与共享存储已迁移，独立 Workflow Agent、公开工厂、Web 三类资源路由及独立视图已接线；有对应默认测试，最终认证见 P3 验证记录，不据文档声明整体通过 |
| 日期 | 2026-09-21 |
| 已确认定位 | 通用 Agent 底座，以编码助手作为首个验证场景 |
| 已有决策来源 | [pi 思想 + Eino DeepAgent 技术方案](../pi-eino-deepagent-design.md) |
| 本轮范围 | 十章完整草案、扩展/工作流/接入补充篇、跨章契约和系统验收；不制定开发方案 |
| 已确认接入边界 | 提供前端可调用的能力与事件接口；先用最小 Web 页面验证，前端实现方案不在产品范围内 |
| 接入落地建议 | Go SDK + HTTP 请求 / SSE 事件流；A2UI 为外层可选映射，契约见第 7 章 |
| 已确认分层原则 | L1 模型 → L2 通用 Agent 执行 → L3 两类同级 Code/Workflow Agent；上层使用下层，两类互不导入，存储实现共用但运行状态独立 |
| 已确认推进方式 | 2026-09-23 的“先章节、后系统拼图、再方案”顺序保留为历史；开发方案已形成，本轮按最终源码同步受影响的原 PRD/开发正文，不改历史 deepagent 设计，不替代最终认证 |
| 模型与环境方向 | 模型接入参考 pi；Windows 原生、Linux、macOS、容器均需支持；具体组合认证清单待定 |
| 模型层能力边界 | 基于 eino-ext 复用实现，并验收 pi 式统一请求/响应、供应商缓存适配与计量；稳定前缀由上下文层配合 |
| 工具与安全参考 | 工具方法论借鉴 pi；安全审核、一次审批和审计参考 DeepSeek Harness；原生三平台沙箱参考 Zero 并按产品契约适配 |

## 文档导航

- [系统闭合检查](system-review.md)：建议先读，汇总契约归属、完整执行轨迹、38 个系统验收场景，以及最新整体评审的收口结果和已确认默认行为。
- [开发方案](../pi-eino-dev-plan/README.md)：接口、工程默认值、需求覆盖及阶段计划的来源；交付事实以源码和本项目验证为准。源码中两类运行与中立 storage 已独立接线，有对应默认测试；实现事实及最终认证分开，具体当前签名/DTO 见开发 06/10/13 和 P3 验证记录。
- [章节路线与决策清单](roadmap.md)：全部章节状态、已确认决定和评审问题。
- [第 1 章：定位、原则与验收目标](01-product-foundation.md)。
- [第 2 章：分层、职责与扩展边界](02-architecture-boundaries.md)。
- [第 3 章：Agent Loop、Trace 与 Turn](03-agent-loop.md)。
- [第 4 章：模型接入与失败边界](04-model-access.md)。
- [第 5 章：工具、权限与副作用](05-tool-system.md)。
- [第 6 章：消息类型与模型边界](06-messages.md)。
- [第 7 章：事件、控制钩子与前端接入](07-events-and-access.md)。
- [第 8 章：上下文工程](08-context-engineering.md)。
- [第 9 章：上下文压缩](09-compaction.md)。
- [第 10 章：会话、分支与执行恢复](10-session-persistence.md)。
- [补充篇：扩展、工作流与接入](11-extensions-workflows-access.md)。
- [补充篇：安全审核、审批审计与沙箱](12-security-sandbox.md)。
- [源码证据与原方案差异](source-evidence.md)：区分已经读到的实现、产品需求和待验证的适配方案。

教程是组织问题的顺序，pi 本地源码是行为参考，Eino 本地源码是实现可行性的依据。本 PRD 描述我们要交付的行为，不逐行翻译 pi，也不把 Eino 示例的全部功能自动纳入首发。

全文统一使用以下名称，详细边界见[第 2 章术语表](02-architecture-boundaries.md#23-与-pi-对齐的对象名称与职责)：

| 名称 | 职责 |
| --- | --- |
| `seasprak-code-agent` | L3 Code Agent 产品类型，现归 `internal/codeagent`（原 `internal/sessions`），旧内置工作流 target/执行/状态/恢复已退出 |
| `CreateAgentSession` / `OpenAgentSession` | 保留的 Code Agent 公开创建/打开入口；打开读取原绑定，不自动执行 |
| `AgentSession` | Code Agent 的运行对象，负责其对话、任务控制和产品事件，不管理 Workflow Agent |
| `Agent` | 保留名称的 L2 通用执行对象，在 `internal/agent` 复用 Eino 执行 |
| `SessionManager` | Code Agent 的历史树、分支与持久提交语义所有者 |
| `ResourceLoader` / `ExtensionRegistry` | Code Agent 当前的资源读取/静态登记及未来 ResourceLoader / ExtensionRegistry 契约，包括通用 AddSubAgent 目标；当前普通目标用 SessionOptions.Agents，见开发 10 |
| `seasprak-workflow-agent` | 同级 L3 产品类型，现归独立 `internal/workflowagent`，拥有单份定义、编译、Eino Graph、节点状态与生命周期 |
| `WorkflowAgent` / `WorkflowOptions` / `CreateWorkflowAgent` / `OpenWorkflowAgent` | 已接线的独立公开能力，有对应默认测试与消费者；不作为 AgentSession 的 targetAgent 或内置子 Agent |
| `internal/storage` 与 jsonl/memory | 共享存储契约与后端已机械迁移；共用实现，不共享两类运行的写入者或业务状态 |

沿用 pi 名称的是 Code Agent 的创建、会话控制、执行、历史和资源职责；ExtensionRegistry 和独立 Workflow Agent 是本产品契约，不声称 pi 自带同名 API。唯一公开 import 为 `github.com/ww1489/seasprak/sdk`，唯一公开生产文件为 `sdk/sdk.go`；同 module 不增加公开子包。

## 1. 执行摘要

### 1.1 问题与方案

需要一套可扩展的 Go Agent 底座，让两类同级产品复用模型、通用执行和存储实现，而各自管理运行状态。`seasprak-code-agent` 仍由 CreateAgentSession/OpenAgentSession 创建或打开 AgentSession，复用 Agent 与 SessionManager；`seasprak-workflow-agent` 独立定义、编译并执行 Eino Graph，不由 AgentSession/targetAgent 调度。新增业务工具不修改通用执行循环，也不绑定特定 UI。

L1 `internal/llm` → L2 `internal/agent`（含 eino/tools）由上层使用；L3 Code Agent 位于 `internal/codeagent`（原 sessions），Workflow Agent 位于 `internal/workflowagent`，两类已在源码独立接线且互不导入。共用 `internal/storage` 契约及 jsonl/memory 实现，不共享写入者、历史、审批、恢复、generation 或隐式预算。Web 在外层分别调用；互调用已有受控工具接口由业务组合，被调方独立授权。独立 Go module、唯一公开 `sdk/sdk.go`，不修改或依赖 `pigo/`。目录、公开工厂及 Web 独立资源/视图有对应默认测试与消费者；实现与测试存在不等于完整认证，最终结果见验证记录。

### 1.2 可验证的成功标准（建议验收门槛）

以下是拟定的产品验收条件，不是现有实现的测试结果，也不是已承诺的上线日期。

| 编号 | 成功标准 | 验证方式 |
| --- | --- | --- |
| K-01 | SDK 能在没有终端交互、没有编码工具时完成一次带自定义工具的任务 | 使用可控假模型和一个内存计算工具，检查调用、结果及完成事件 |
| K-02 | 编码配置能完成读文件、修改一个限定文件、运行给定测试命令并报告结果 | 在临时示例仓库中核对 diff、命令退出码及最终报告；不得改动范围外文件 |
| K-03 | Code Agent 增加工具/skill、独立 Workflow 定义/编译均不改 L2 Agent 循环；业务工具组合不新增 SDK 跨 Agent 框架 | 按各自交付阶段验证三个示例及装配边界，不把动态加载标为本轮交付 |
| K-04 | 切换运行实例不丢失会话身份和已提交历史 | 验证正常换实例、构建失败回退、Abort 后继续三种轨迹 |
| K-05 | Code Agent 同一 Session 不会有两个顶层任务同时写历史；Workflow 拥有独立写入者及运行状态，两类不串用审批/恢复/预算 | 并发输入、取消和各自版本约束的确定性验收；每条结果关联原调用，跨类工具包装不合并日志 |

交互验证统一使用最小 Web 页面：通过公开接口提交任务、订阅回复和工具事件、停止任务，并在同一 Session 继续。页面不得绕过接口直接读写内部状态；无头 SDK 测试继续用于验证底座契约。

真实模型的任务成功率、延迟和成本目标在模型及评测集选定后补充；不虚构当前基线。

## 2. 用户体验与功能

### 2.1 用户与核心流程

首要用户是把 Agent 嵌入自己的程序的 Go 开发者；其次是编写工具、skills 和工作流的扩展作者，以及使用编码助手验证底座的开发者。

典型 Code Agent 流程：开发者登记能力 → CreateAgentSession/OpenAgentSession 创建或打开绑定工作区的 AgentSession → 提交任务 → 查看流式回复和工具执行 → 完成或停止 → 从 SessionManager 保留的历史继续。固定路径由业务调用独立 Workflow 目标入口，不借同一 AgentSession 切换 targetAgent；两类组合由业务用已有受控工具实现。开放探索仍由 Code Agent 选择工具或普通受控子 Agent。

| 用户故事 | 需求 | 验收归属 |
| --- | --- | --- |
| 作为应用开发者，我希望直接嵌入或远程调用 Agent，以便接入自己的前端 | SDK 与 HTTP/SSE 分别映射两类公开能力；核心不依赖页面或前端框架 | K-01；第 2 章 |
| 作为扩展作者，我希望只实现业务能力，以便复用底座 | Code Agent 工具、skill、普通子 Agent 通过其注册入口装配；Workflow 在独立产品中定义/编译 | K-03；第 5、8 章及扩展补充篇 |
| 作为编码助手用户，我希望能追问、停止并继续工作 | Code Agent follow-up、steering、Abort 有不同语义，不隐式互换或用于 Workflow 路由 | K-04、K-05；第 3 章 |
| 作为长期任务用户，我希望保存、恢复与分叉对话 | Code Agent Session 独立持久化，不以 Checkpoint 替代；Workflow 保存自身运行/节点状态 | 第 9～10 章及工作流补充篇 |

### 2.2 继承的产品约束

1. L2 继续复用 Eino Agentic DeepAgent/TurnLoop，不另写 ReAct；L3 Code Agent 与 Workflow Agent 同级、互不导入，后者不作为前者的内置子 Agent。业务可用已有受控工具组合两类，不保留 workflow-as-tool 禁令。
2. Code Agent 会话调度使用 Eino TurnLoop；不自写一套 `Runner.Run` 轮询循环替代它。Workflow 独立编译和执行 Eino Graph。
3. Code Agent 普通忙时输入为当前 Trace 的 follow-up，由外层在内层自然停下后消费；steering 在轮次边界消费，两者均不新建 Trace，也不用 preempt 冒充。Workflow 不读取该队列。
4. Code Agent 已受理 Trace（含 queued/hold）固定工具、skill、handler 与普通子 Agent 的 generation；后续 Turn、follow-up、子调用及恢复不升级、不重置总预算。Workflow 自身冻结定义/节点绑定，跨两类不共享 generation 或预算；动态加载、热重载、多 generation 仍属未来阶段。
5. 产品消息允许扩展；模型输入通过投影得到；未知扩展消息的原始数据可保留。两类历史不自动合并。
6. 已编译 Go 包静态登记与 Workflow 静态定义/子流程、条件、并行汇合、已有节点审批和显式恢复保留。Coze Canvas 导入、动态定义与业务补参仍在原未来阶段，本轮不启动 P4/P5；不做 Go `.so` 或可视化编排界面。
7. Code Agent 普通子 Agent 采用开发者代码扩展，经 ExtensionRegistry 的 `AddSubAgent` 装配；仍在父 Trace 权限、预算、generation 与取消范围。独立 Workflow 不经此入口登记；两类互调的授权、参数冻结、票据、预算与恢复分别校验。
8. 模型消息统一采用 `*schema.AgenticMessage` / `model.AgenticModel`；内部消息借鉴 pi 的三类标准职责和可扩展 AgentMessage，先 transformContext，再 convertToLlm。
9. Code Agent 创建必须显式绑定工作区；默认具备文件、命令、TODO 与通用子 Agent，裁剪由开发者明确指定。Workflow 使用自身显式绑定与策略，不继承 Code Agent 装配或授权。
10. Code Agent 概念为 Session → Trace → Turn：Trace 以 agent_start/agent_end 表达内部循环边界，以 trace.settled 表达产品收尾；traceId 关联其状态和恢复。Workflow 使用自己的运行/节点生命周期；共用 storage 实现不共用写入者、历史、审批或恢复。

### 2.3 范围与非目标

本套 PRD 的完整目标包含：执行循环、模型接入、工具、消息与事件、上下文、压缩、会话，以及 skills / 插件 / 工作流扩展。既有开发方案承接工程参数、交付阶段与开发顺序；本次只统一 2026-10-01 架构决定的正文和系统衔接，不更新开发计划或据此认证阶段交付。

底座负责前端接入契约，包括调用操作、状态查询、事件流与交互结果回传。建议首先通过 HTTP/SSE 支撑 Web 验证；A2UI 的适配位置预留在对外接入层，是否实现及协议版本随后按场景决定。HTTP/SSE 承担请求和事件传输，A2UI 承担 UI 描述语义，不将三者视为互相替代的同类接口。

前端框架、组件、布局和正式 UI 产品设计不在范围内；最小 Web 页面仅作为测试工具，随已交付的底座能力增加测试控件。CLI 不再是首期必须交付项。

暂不纳入：MCP 市场、分布式多租户调度、公司级审批 BPM、任意 Go 源码文件的进程内热编译加载、对 pi API/会话格式的完全兼容。

本目录不继承相邻 `PRD-pi-desktop-agent.md` 的 Electron 产品范围；那是另一条使用 Pi SDK 的方案。

## 3. AI 系统需求

### 3.1 模型、工具与能力

- 模型经 Eino / eino-ext 接入，按 pi 的思路提供多供应商请求/响应统一、工具及推理回放、缓存策略和用量归一化；现成组件能力直接复用，缺失处补适配。第 4 章给出协议及缓存矩阵，第 8 章负责稳定前缀；全环境目标和具体认证范围沿用既定约束。
- 编码能力作为首个可装配的能力组合；文件和命令执行能力不能成为所有 Agent 场景的隐式前提。
- 业务 Workflow 必须声明输入和输出；业务直接调用独立 Workflow 时不调用 Code Agent 主模型选路，节点调用只在 Workflow 自身记账。
- skills 是 Code Agent 按需使用的指令与资源，不等于可执行插件；加载 skill 不自动授予工具权限，Workflow 不自动读取 Code Agent 资源/历史。
- Code Agent Session 必须绑定工作区，默认装配文件读写/搜索、命令执行、TODO 与 general-purpose 子 Agent；创建入口校验后端及安全策略，开发者可显式裁剪/替换。Workflow 按自身定义装配节点与策略，不继承这些默认能力或授权。提示词反映实际能力，可由应用覆盖。

### 3.2 评估策略

确定性测试负责验证底座行为：假模型产生可预测的工具调用、错误和分块输出，工具使用临时目录或内存替身。真实模型评估负责验证任务效果，两类结果分别记录。

建立小型固定任务集：解释示例代码、完成限定修改并运行测试、调用自定义非编码工具、以业务直接调用和已有受控工具包装两种方式运行独立 Workflow、加载 skill 后执行任务、在长会话中继续任务。每个用例记录模型配置、输入、期望产物、验收条件和实际事件；两类分别记录运行身份/预算/版本/恢复，不承诺跨运行总额或整树恢复。任务成功以产物及可重复验证的结果为准，不以模型自述“完成”为准。

## 4. 技术规格

### 4.1 所有权边界

| 对象 | 谁负责 | 关键边界 |
| --- | --- | --- |
| Code Agent 创建/打开与依赖装配 | CreateAgentSession / OpenAgentSession | 返回 AgentSession；打开保留原绑定，不自动执行 |
| Code Agent Session、cwd、历史树与分支 | SessionManager | 不随执行实例销毁；经共享 storage 契约独立持久化 |
| Code Agent Trace、输入队列、能力版本 | AgentSession | 同一 Session 至多一个活动/待恢复顶层 Trace；协调 Agent 与 SessionManager，不管理 Workflow |
| 通用执行机制 | L2 Agent / Eino 适配 / DeepAgent / TurnLoop | 名称不变，消费执行策略，不导入任一 L3 |
| Code Agent skills、项目指令及扩展资源 | ResourceLoader / ExtensionRegistry | 读取与登记普通受控子 Agent 等候选能力；不拥有 Workflow 定义/节点 |
| Workflow 定义、编译、图、节点及生命周期 | 独立 Workflow Agent（已接线，有默认测试及消费者） | 自己持有写入者、历史、审批、恢复、generation 与预算，不用 AgentSession/targetAgent |
| 共用记录契约与 jsonl/memory | internal/storage（契约及后端已机械迁移） | 仅共用实现，不共用两类业务状态；本次不迁移或自动删除旧开发数据 |
| HTTP/SSE、可选 A2UI 与跨类工具组合 | 外层业务/接入层 | 分别调用两类公开能力；被调方独立授权，不建 SDK 跨 Agent 框架或总预算/整树恢复 |
| 最小 Web 测试页面 | 底座之外的测试客户端 | 仅通过公开接口验证已交付能力 |

核心分三层：L1 `internal/llm`，L2 `internal/agent`（含 eino/tools），L3 同级 `internal/codeagent` 与 `internal/workflowagent`；同级 `internal/codeagent` / `internal/workflowagent` 与中立 storage、两类工厂及 Web 三类资源路由和独立视图均已接线，有对应默认测试与消费者，最终认证见验证记录。两类互不导入，共用 `internal/storage`，SDK 在外侧同 module 单一 `sdk/sdk.go` 导出；详见[第 2 章](02-architecture-boundaries.md#21-三层核心与对外接入)。

**对原方案的源码复核结果**见[差异记录](source-evidence.md#原方案需要修订或补足的地方)：

- Code Agent 一个 Session 同时最多一个活动 TurnLoop，但整个 Code Session 生命周期可以先后拥有多个 TurnLoop 实例。`Stop()` 后要重新建立执行循环，不能再次启动旧实例。
- 若未来阶段启用动态热重建，失败时由 AgentSession 协调 Agent 的构建错误与旧版本回退；让 `PrepareAgent` 返回错误会结束当前 TurnLoop。本轮静态装配不启动该阶段。
- Code Agent 的中断任务恢复属于原任务的继续，不是允许任意更换工具拓扑的新任务；恢复与未来热重建必须协调。Workflow 使用自身节点/运行恢复，不套用 Code AgentSession。
- 已确认采用 `*schema.AgenticMessage`，主/子 Agent 和执行器统一使用匹配的泛型路径。不能以旧注释认定取消/重试缺失，也不能以实现存在替代供应商和恢复兼容性验证。

### 4.2 集成与安全边界

Go SDK 通过稳定的 Code Agent Session/任务/消息/事件概念，以及独立 Workflow 的运行/节点/事件概念对接应用，Eino 配置集中在各自运行时装配处。Go 包的最终导出签名留待相应章节，避免现在冻结未经验证的接口；两类入口由外层业务分别选择和调用。

工具设计吸收 pi 的分层、五步处理、错误反馈和 Operations。安全按用户要求：策略、审批和审核参考 DSH，原生三平台沙箱参考 Zero 并适配。按调用解析文件效果策略，一次批准与审计独立于执行沙箱，后端缺失不裸跑，full/partial 如实上报；默认已确认为 workspace-write + ask，Auto review 关闭；partial 满足运行数据保护等必需条件后可用并如实标识。详情见 M05 与安全补充篇。模式不隐含网络/读取/任意 Go 插件隔离，凭据不进入普通历史。

## 5. 风险与路线

| 风险/未决事项 | 对需求的影响 | 后续处理 |
| --- | --- | --- |
| 教程 v0.80.2 与本地 pi v0.84.2 的结构差异 | 旧路径或简化分层不能直接作为当前事实 | 每章保留本地代码证据 |
| Eino 注释与实现不一致 | 可能误判选型能力 | 以调用路径、已有测试和必要的验证实验判断 |
| Abort、HITL 与扩展热换交叉 | 丢队列、重复执行、恢复错误拓扑 | 第 3、10 章已给出契约；系统场景统一验收 |
| 同进程 Go 插件与热加载混淆 | 承诺一期无法提供的动态代码加载 | 保留已编译能力静态装配；资源重载/运行时启停仍在原未来阶段，本次不启动 P4/P5 |
| “通用底座”范围失控 | 过早实现大量未来适配层 | 用 K-01 和 K-03 的真实扩展示例检验必要抽象 |

本轮按当前源码同步迁移影响的原需求与开发正文；此前确认的必需工作区、默认基础能力、公共祖先摘要、受控核对、安全默认值和取消后队列保留。[开发方案](../pi-eino-dev-plan/README.md)承接真实接口、工程默认值、支持矩阵与阶段排期，06/10/13 已按当前 SDK/HTTP 细化；本次不改历史 deepagent 设计，不启动 P4/P5。独立工厂、定义/编译/图与 Web 路由/视图已实现，有对应默认测试；最终认证见 [P3 验证记录](../p3-verification.md)，不以文档或局部测试证明 P3 完成。

前端范围、Web 测试方式、单向分层、全环境、必需工作区、默认 DeepAgent 基础能力、取消后队列及安全默认值已确认。原决策表中的产品选择与工程验证已区分；详见[决策清单](roadmap.md#待讨论决策)和[系统闭合检查](system-review.md)。
