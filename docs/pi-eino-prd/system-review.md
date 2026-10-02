# 系统闭合检查：技术拼图、契约与评审入口

状态：2026-10-02，覆盖第 1～12 章的系统级评审与当前分层实现状态。**本文件不是开发方案。** 2026-09-23 的先章节、后系统拼图、再开发方案顺序保留为历史。Code Agent 与 Workflow Agent 已同级独立，Code 旧内置工作流已退出，公开工厂、Web 三类资源路由及独立视图已接通，有对应默认测试及 SDK 消费者；具体签名/DTO 见开发 06/10/13，最终认证见 [P3 验证记录](../p3-verification.md)，不据文档或局部测试声明整体 P3 通过，不启动 P4/P5。

十章主线、[扩展/工作流/接入补充篇](11-extensions-workflows-access.md)及[安全与沙箱补充篇](12-security-sandbox.md)已经成稿；批准的默认行为及架构已写入正文。文档覆盖不代替本项目源码、故障注入、真实模型与平台运行证据；旧开发数据不迁移且不自动删除。

## 1. 已确认目标与约束

1. 通用 Agent 底座，编码助手作为首个验证场景。
2. 前端实现不在产品范围内；底座暴露接入能力，使用最小 Web 页面做验证。HTTP/SSE 是建议的首个映射，A2UI 保留外层适配位置。
3. 三层职责：L1 `internal/llm` 模型；L2 `internal/agent`（含 eino/tools）通用执行，`Agent` 不改名；L3 同级 `seasprak-code-agent` / `seasprak-workflow-agent`，目标分别为 `internal/codeagent`（由 `internal/sessions` 迁移）和 `internal/workflowagent`。两类不互相导入，接入在最外侧，下面不依赖上面。
4. 同一独立 Go module；唯一公开 import 为 `github.com/ww1489/seasprak/sdk`，`sdk/sdk.go` 为唯一生产入口文件，不新增公开子包。沿用 Eino Agentic DeepAgent + TurnLoop，不另写 ReAct，不修改或依赖 pigo；`cmd/agentd` 仍仅 help/version，不导入 sdk/internal。
5. 模型能力参考 pi；运行环境覆盖 Windows 原生、Linux、macOS、容器。具体模型及环境组合的支持状态要经矩阵验证，不把存在源码等同于认证。
6. 先需求、后系统评审和开发方案的原顺序保留；本次只修订文档，不启动产品开发、P4/P5，也不认证已有阶段完成。
7. 普通 Code Agent 的 general-purpose/专家子 Agent 由开发者创建并通过 AddSubAgent 注册，保留父 Trace 的权限、预算、generation 与取消范围；Workflow Agent 不登记为其内置子 Agent。终端用户无需配置执行上限，工程默认引用[开发方案 12](../pi-eino-dev-plan/12-delivery-and-validation.md#defaults)。
8. Code Agent 保留 CreateAgentSession/OpenAgentSession、AgentSession、SessionManager、ResourceLoader 和 ExtensionRegistry；Workflow Agent 独立拥有定义、编译、Eino Graph、节点状态与生命周期，WorkflowAgent/WorkflowOptions/CreateWorkflowAgent/OpenWorkflowAgent 已公开接线，有对应默认测试及消费者，真实签名见开发 06/10，最终认证见验证记录。共用 `internal/storage` 契约及 jsonl/memory 实现，不共享写入者、历史、审批、恢复、generation 或隐式预算。业务分别调用，互调用已有受控工具接口组合，被调方独立校验、授权、冻结、管理票据/预算/版本/恢复；SDK 不新增专用跨 Agent 框架。详见[统一术语](02-architecture-boundaries.md#23-与-pi-对齐的对象名称与职责)。
9. 工具方法吸收 pi 的分层、五步处理和 Operations；安全审核、审批审计参考 DeepSeek Harness，原生三平台沙箱参考 Zero。平台实际约束和参考实现未覆盖的部分必须标明。
10. Code Agent 的 AgentSession 必须绑定工作区，默认装配文件读写/搜索、命令执行、TODO 和 general-purpose 子 Agent，开发者可显式裁剪或替换；Workflow 按自身定义与策略装配，不自动获得 Code 历史或能力。Workflow 内部静态子流程、条件/并行汇合及已有节点级暂停审批/显式恢复保留；Coze、动态加载、热重载、业务补参仍在原未来阶段。高级跨任务编排/审批/补偿、业务幂等、完整持久子树恢复、跨运行总预算归业务。
11. 默认 workspace-write + ask、Auto 关闭；满足运行数据保护等必需条件后允许 partial 并如实标识；未满足时拒绝受限配置，不自动放宽。
12. 取消完成后保留旧记录并暂停当时独立队列的自动启动；无冲突时新 prompt 可开始，旧队列须显式继续。公共祖先摘要可继承，未决效果通过受控核对更新事实与恢复资格。

## 2. 每块拼图有唯一规格来源

| 拼图 | 主文档 | 对外提供 | 依赖其他章节 |
| --- | --- | --- | --- |
| 定位和用户验收 | [M01](01-product-foundation.md) | 目标、范围、成功标准 | 全部 |
| 层级和依赖 | [M02](02-architecture-boundaries.md) | L1/L2/L3 责任、接口注入规则 | 每章遵循 |
| Agent Loop 与 Trace | [M03](03-agent-loop.md) | 双层循环、轮后钩子、Trace 状态、输入和统一收尾 | M04、M05、M07、M10 |
| 模型接入 | [M04](04-model-access.md) | 统一入口、思考/缓存映射、流 attempt 契约、usage、溢出识别与预算 | M06、M07、M08、M09 |
| 工具执行 | [M05](05-tool-system.md) | 校验、权限、并发、结果与未知副作用 | M03、M06、M10 |
| 消息语义 | [M06](06-messages.md) | 产品身份/类型、序列化、模型投影子步骤 | M08、M10 |
| 事件和接入 | [M07](07-events-and-access.md) | 发布顺序、游标、快照、HTTP/SSE 行为 | M03、M06、M10 |
| 模型前上下文 | [M08](08-context-engineering.md) | 已消费输入边界、截断续读、提示词组装、skill 加载、摘要分工、资源快照与总预算 | M03、M04、M05、M06、M09 |
| 上下文压缩 | [M09](09-compaction.md) | 合法切点、首次/更新/前缀摘要、确定性文件集合、候选提交与重建 | M03、M04、M05、M06、M08、M10 |
| 会话和恢复 | [M10](10-session-persistence.md) | Entry 分类、树操作、路径配置重建、JSONL/版本、提交与 checkpoint 关联 | M03、M04、M06、M07、M09 |
| 扩展、工作流 | [补充篇](11-extensions-workflows-access.md) | Code 扩展登记/替换、运行操作与生命周期；独立 Workflow 定义/编译/节点状态、各自 generation；业务直接调用或受控工具组合，动态来源仍属未来阶段 | M03、M04、M05、M06、M07、M08、M09、M10 |
| 安全审核与沙箱 | [安全补充篇](12-security-sandbox.md) | 冻结执行描述、原始授权来源、许可占用/恢复、运行数据保护、临时产物映射及平台约束 | M03、M05、M06、M07、M08、M10、M11 |

不同章节互相引用表示行为协作，不表示 Go 包允许反向依赖。M07 拥有接入协议及 hook 组合/生命周期顺序，M05 拥有动态工具执行语义，补充篇定义扩展/工作流的登记和运行操作面；M08 拥有完整上下文排序，M06 只负责其中的消息投影。

## 3. 一条任务经过系统的完整轨迹

下表是 Code Agent 的逻辑因果顺序，不规定函数签名、线程模型或文件布局；Code Agent 实现已从 `internal/sessions` 迁移到 `internal/codeagent`。表内目标 Agent/子调用只指普通 Code Agent，不包括 Workflow Agent。

| 步骤 | 负责方 | 产物/约束 |
| --- | --- | --- |
| 0. 装配 | CreateAgentSession + ResourceLoader / ExtensionRegistry | 取得资源与注册结果，创建 Agent 和 AgentSession，注入 SessionManager；创建函数不持续调度任务 |
| 1. 受理并绑定版本 | 外层校验 → AgentSession → SessionManager | 新独立输入一致保存 inputId、traceId、目标 Agent、generation 与依赖引用后 accepted；同键重试返回原绑定；定向输入继承原目标，错配拒绝 |
| 2. 排队 | AgentSession → Agent 的 TurnLoop 适配 | steering 与 follow-up 归原 Trace；独立 queued 项保留受理时版本，包括 hold 项，不因 reload/重启/继续队列换版 |
| 3. 执行前复核 | AgentSession 使用受理时快照 | 复核原版本可用性、当前权限和执行条件，不重新选最新版；初始模型/代码策略按 M04，resume 恢复 checkpoint 配置 |
| 4. 消费输入 | AgentSession 协调，SessionManager 提交 | 仅一次写入已消费用户消息；固定分支与执行身份 |
| 5. 准备上下文 | L2 + L3 注入的数据 | M08：结构化历史 → transformContext 及允许的规范/skill 贡献 → convertToLlm → 完整请求预算/校验 |
| 6. 模型调用 | L1 / L2 | 一个 Turn 的模型生成可有多个 attempt；只有被接纳的完整响应才可派发工具 |
| 7. 工具或委派 | L2 执行 + L3 策略 | 参数转换→最终校验/冻结→授权/必要许可占用→实际执行→结果；同名调用不混淆，子调用不消费主 steering |
| 8. 持久化与观察 | SessionManager 提交 → AgentSession 发布 M07 事件 | 关键事实先提交再发布；瞬态 chunk 不冒充持久记录 |
| 9. 轮后与内外循环 | L2 通用控制钩子 + AgentSession 队列 | turn_end → prepareNextTurn → shouldStopAfterTurn → steering；内层自然停下再查 follow-up；同 traceId 继续 |
| 10. 暂停或完整收尾 | AgentSession 协调 Agent 与 SessionManager | waiting_input/paused 保持原 Trace；自然收尾前检查两类输入及结果，终态提交后只发一次产品 trace.settled；内部 agent_end 不作为最终完成 |
| 11. 客户端重连 | 外层按 sessionId 路由到 AgentSession | 从 SessionManager 的持久记录与当前运行快照恢复视图；重连不重跑请求 |

独立 Workflow 的轨迹为：业务通过其入口选择并校验定义/输入 → Workflow 自己冻结版本、预算和节点能力 → 自己编译/运行 Eino Graph → 提交节点结果、未知效果及自身事件 → 在已有节点边界暂停/审批 → 用自身票据与 checkpoint 显式恢复。静态子流程、条件/并行汇合继续属于这条轨迹。Code↔Workflow 的业务工具只显式交换获准输入和结果，被调方仍走自身受理/授权/恢复；不经过 AgentSession/targetAgent、不共用父 Trace 总额或整树恢复。

持久化失败、中断、取消、热更新和压缩可能发生在不同步骤；以下场景专门验证交叉点。热更新/Coze/动态定义/补参属于原未来阶段，不由本次修订启动。

## 4. 跨章节不变量

| 编号 | 必须始终成立的规则 | 归属 |
| --- | --- | --- |
| SYS-01 | 各自运行同时最多一个顶层执行/日志写入所有者；Code 等待恢复也不让其他任务旁路写分支，Workflow 自己协调节点提交；共享 storage 不共享写入者或日志 | M03/M10 |
| SYS-02 | Code 的 Session、Trace、Turn、消息和工具身份不混用；Workflow 使用自身运行/节点身份，不借 Code 身份或恢复票据；各自输入去重，span/executionId 只承担诊断/内部执行职责，业务关联不合并运行 | M03/M06/M07/M10 |
| SYS-03 | Code 新独立输入受理时固定 generation 和父 Trace 预算范围并保留引用，覆盖 queued/hold、运行、steering、follow-up、普通子调用与恢复；仅随后受理的新 Trace 可升级。Workflow 自己冻结定义/节点能力版本与预算；业务工具版本不替代被调方冻结，两类不共享 generation/隐式总额；逐 Turn 选择按 M04/M05，动态重载仍属未来阶段 | M03/M04/M05/补充篇 |
| SYS-04 | 历史是真实记录，模型上下文是投影；压缩/截断/过滤不删除原记录或扩大权限 | M06/M08/M09/M10 |
| SYS-05 | 已受理输入有可恢复去向；pending 与 consumed 区分；重复网络请求不能重复执行 | M03/M07/M10 |
| SYS-06 | 每个完整工具调用都有关联结果或显式 unknown；结果未知不能自动重试；取消不等于回滚 | M05/M06/M10 |
| SYS-07 | 模型重试只作用于模型 attempt，不重放已有工具；失败 attempt 不拼入成功回复 | M04/M06 |
| SYS-08 | durable 事件代表已提交事实；瞬态 delta 可以缺失但必须可重同步；页面断连不取消任务 | M07/M10 |
| SYS-09 | Code waiting_input 与 Workflow 节点等待均须有各自可用的 checkpoint、关联记录及许可条件才宣称可恢复；旧版本缺失明确拒绝；业务关联不承诺完整持久子树恢复 | M03/M09/M10 |
| SYS-10 | 暂停、失败、取消、正常终答、请求业务验收各有独立含义，不由一条流结束信号替代 | M03/M05/M07 |
| SYS-11 | 每个模型请求由同一份所属运行的上下文管道计算总预算，包含 system、工具 schema、媒体和输出预留；不能多套 middleware 无限压缩。Code 普通子调用仍归父 Trace，Workflow 自身节点记账，不形成跨类共享总额；工程值见开发方案 12 defaults | M04/M08/M09 |
| SYS-12 | L1/L2 不依赖 Code/Workflow 的运行对象、管理器或接入；同级两类不互相 import、不依赖 Web；各自状态不依赖 Eino/具体 storage 后端，storage 不反向依赖管理器。A2UI 只属外层，SDK 不新增公开子包或专用跨 Agent 框架 | M02/补充篇 |
| SYS-13 | 供应商缓存编码与响应归一化归 L1，前缀内容稳定归 M08；缓存优化不得改变分支/恢复语义，不以缓存键替代权限或上下文等价判断 | M02/M04/M08/M10 |
| SYS-14 | 批准不代替沙箱，沙箱不代替审计；限制后端不可用不裸跑，partial 不标 full，必要审计未提交不发放许可 | M05/安全补充篇 |
| SYS-15 | 历史消息和模型/思考状态来自同一选定 parent 路径；压缩不丢前缀配置，文件末行不决定活动游标。新 Trace 按 M04 验证所选配置，resume 采用 checkpoint 的有效配置 | M04/M06/M09/M10 |
| SYS-16 | Code model 调用绑定原 Turn/invocation，普通子委派仍受父 Trace 约束；Workflow 节点调用绑定自身定义/节点/能力；direct 绑定受信入口。两类逐次独立授权，业务工具不转移许可；普通模型工具选择仅后续 Turn 生效，恢复不扩大权限或开放全集 | M03/M04/M05/M08/M10/M11 |
| SYS-17 | 扩展运行操作沿用受理/消费/提交规则，不重入循环；生命周期 before 只能取消或贡献允许候选，after 不撤销提交。用户取消及权限撤销不受 hook 否决 | M03/M06/M07/M09/M10/M11 |
| SYS-18 | 最后一次参数转换后校验并冻结同一执行描述，授权后改变则重新判断；一次问答/去重/有效期/消费仅存当前实例内存，关闭/重开失效；原调用执行意图/占用与预算先提交，缺启动记录不等于未执行，checkpoint 不携带决定。审核原文来自受信受理/委派记录，派生 user 文本不新增许可 | M05/M06/M07/M10/安全补充篇 |
| SYS-19 | 受限配置保护运行数据，业务产物与可信状态分开；会话产物跨文件工具/shell 指向同一真实文件，后端私有临时路径不当作持久引用 | M05/M08/M10/安全补充篇 |

取消有两个维度：执行是否真实停止、外部副作用是否已知。Code 由 AgentSession/SessionManager 提交 cancelled 与 unknown，Workflow 由自身生命周期提交；各自在核对前阻止冲突动作。已受理取消要等待本作用域执行及受控子执行退出，业务工具不自动提供跨运行取消确认；未受理取消但不能安全继续时为 paused。各自终态均不通过 resume 复活。

## 5. 端到端验收矩阵

以下是后续实现必须通过的系统验收定义，本轮尚未执行。用受控模型验证协议与故障行为，再用真实模型验证任务效果。

| 场景 | 涉及章节 | 核心断言 |
| --- | --- | --- |
| SYS-A01：显式工作区、裁剪基础工具的无前端 SDK 自定义任务 | 1/2/3/4/5 | 工作区绑定仍有效，裁剪由开发者明确指定；通用底座独立工作，无反向依赖 |
| SYS-A02：Web 提交编码任务，读改限定文件并跑测试 | 3/4/5/6/7/10 | 文件 diff、退出码、结果一致；接入层通过 AgentSession 使用已装配能力 |
| SYS-A03：提交响应丢失且客户端重试 | 3/7/10 | 一次 input 消费、稳定 traceId；follow-up 不因重试创建新 Trace |
| SYS-A04：工具执行中提交 steer 与两个 follow-up | 3/5/6/7 | steer 在下一 Turn 生效，follow-up 在内层自然停下后生效；均属同 Trace；内部执行边界按 executionId 区分，最终仅一条产品 trace.settled |
| SYS-A05：终答与 steer/follow-up 同时到达 | 3/7/10 | 先受理则同 Trace 继续，终态先提交则拒绝定向输入；无丢失和双终态 |
| SYS-A06：未来阶段运行中 reload，后续 follow-up 与新 prompt 使用 skill | 3/8/11 | Code 当前 Trace 的 follow-up 仍用旧资源，下一独立 Trace 才升级；Workflow 自己冻结版本，另一类更新不替换本运行；本次不启动该阶段 |
| SYS-A07：等待确认时未来阶段 reload、断连、重启再回答 | 3/5/7/10/11 | 各自恢复原 generation/投影与调用定位，不恢复批准；本实例问答去重，重开旧回复失效，显式 Resume 复核策略，未占用且需审批时重新问，已占用未知先核对，已有结果复用；跨类审批/恢复错配拒绝，不把重载写为本轮交付 |
| SYS-A08：写工具结束后模型先失败再重试 | 4/5/6/7 | 一次写入、两个模型 attempt；失败文本不污染最终答案 |
| SYS-A09：外部操作完成但结果保存前崩溃 | 3/5/10 | unknown/recovery_required，先核对；不凭 checkpoint 重做副作用 |
| SYS-A10：长会话压缩，保存后崩溃再恢复 | 5/6/8/9/10 | 原历史完整，摘要正文与文件 details 一致提交；从 firstKeptEntryId 本身恢复，工具配对、旧目标与累计文件事实保留 |
| SYS-A11：模型窗口由大切小、只有规范/schema 已超限 | 4/8/9 | 正确分项，拒绝无效请求；不无限摘要历史或静默丢要求 |
| SYS-A12：对话分叉后继续编码 | 5/6/9/10 | 分支投影隔离；实际文件未回滚；新任务重新读取真实状态 |
| SYS-A13：慢 SSE 客户端、事件重放窗口过期 | 7/10 | 任务继续；客户端用快照恢复；无无限队列和虚假完成 |
| SYS-A14：业务直接调用与受控工具包装调用同一独立 Workflow | 3/5/7/10/11 | 被调方均独立校验输入、授权、冻结参数/版本/预算并记录节点状态；不经 Code task/targetAgent，不共用历史/写入者/审批票据/恢复游标/generation/隐式总额；已有节点暂停审批可显式恢复；忙时类型切换不误投，取消真实退出、unknown 不盲重放 |
| SYS-A15：扩展缺失或旧 generation 无法重建 | 6/8/10/11 | 历史仍可读；必须投影或恢复时明确报错，不猜测替代实现 |
| SYS-A16：Windows/Linux/macOS/容器的同一能力集 | 2/4/5/7/10 | 分平台验证路径、命令、取消、存储和接入；不以单平台通过冒称全平台 |
| SYS-A17：开发者注册普通 Code 自定义子 Agent | 3/5/11 | 创建实例后调用 ExtensionRegistry.AddSubAgent，经 CreateAgentSession 装配；AgentSession 控制对话、主 Agent 委派，无需改核心或用户配置；general-purpose/专家受父 Trace 权限/预算/generation/取消约束；独立 Workflow 不由 AddSubAgent 登记 |
| SYS-A18：连续模型调用的前缀缓存与响应统一 | 2/4/6/8/10 | 稳定前缀、厂商专属字段、正确用量口径；缓存过期/模型切换/分叉后无错误续接，回退不重跑工具 |
| SYS-A19：DSH 式策略、审批与 Zero 适配沙箱 | 3/5/7/8/10/安全补充篇 | fs/shell 同一策略；限制失效不裸跑；不采用 Zero auto degraded；原调用意图/占用、预算及结果可恢复，一次批准不跨关闭/重开，DSH 持久 decided 不作为产品许可；Auto 最小决定元数据独立保留，缺失不误放行；平台能力如实上报 |
| SYS-A20：逐 Turn 思考/模型切换与输出预算 | 3/4/8 | 请求档位与生效档位可查；本轮预算对应实际请求参数，不突破上限，也不重复计算共享思考/答案空间 |
| SYS-A21：模型流失败及溢出判别 | 3/4/6/7/9 | 建流/Recv/取消各有唯一 attempt 结果；快照不重复追加；限流及无证据空输出不压缩；真实超限恢复有界且不重跑工具 |
| SYS-A22：排队输入、摘要与上下文重建 | 3/6/8/9 | 已受理但未消费的 steering/follow-up 不进入当前模型或摘要；消费后只追加一次 |
| SYS-A23：超长文件/日志/搜索与 skill 按需读取 | 5/6/8/11 | 头尾与单行策略正确；模型看见实际续读方法；无 bash 环境仍有已装配的有界加载能力；缺资源时不假称已读 |
| SYS-A24：连续压缩的文件清单与失败写入 | 5/6/9/10 | 上次 details + 新范围的已确认效果累积；拒绝/未知不伪造修改，跨分支不串线，程序附录不丢记录或泄露排除内容 |
| SYS-A25：长工作前缀与增量摘要 | 3/6/8/9 | M03 Turn 不被重定义；H/P/K 分区不重叠；无新增 H 仍保留旧摘要；任一必需摘要失败不发布半成品，合计预算与 usage 可查 |
| SYS-A26：分支、配置变更、压缩与重启 | 4/6/9/10 | 回退游标可恢复，新消息父节点正确；历史配置按完整选定路径提取；待生效默认不影响原 checkpoint；元数据不进入模型 |
| SYS-A27：格式兼容与恢复事件重放 | 3/7/10 | 不支持的格式不追加，诊断/只读/拒绝保留原件；本次不迁移旧开发数据、不自动删除。通用未来格式演进的转换失败亦保留原件；各自日志不串用游标，不同执行段不被 traceId 错误去重，暂停无 settled，终态仅一条持久事实 |
| SYS-A28：工具搜索、读写模式切换、重试和恢复 | 3/4/5/8/10/11 | 普通工具在下一 Turn 开放，说明/schema/预算一致；同批调用与恢复绑定旧选择，权限撤销仍生效，父子不串线 |
| SYS-A29：扩展发送输入和保存状态后崩溃 | 3/6/7/10/11 | 受理身份去重，未消费消息不入模型；私有状态按正确路径/提交位置恢复；无自等待、无第二次 Runner 重入 |
| SYS-A30：扩展取消导航、替代压缩及提交后通知失败 | 7/9/10/11 | before 取消不改游标，多个候选冲突，文件 details 由事实生成；after 失败不回滚成功，取消/撤销不被扩展阻止 |
| SYS-A31：参数改写与授权后执行描述变化 | 5/7/11/安全补充篇 | 最后转换后校验，tool_call 只读，普通 allow 不跳过强制策略；参数/环境变化使旧许可不适用 |
| SYS-A32：批准/占用/启动/结果之间崩溃 | 3/5/7/10/安全补充篇 | 本实例串行消费批准，原调用意图/占用及预算提交最多一方成功；重开批准失效，显式 Resume 复核策略，未占用且需审批时重新问，已占用未知先核对，已有结果复用；新 executionId 不重跑，受信未执行证据解除占用先提交且不恢复旧批准 |
| SYS-A33：可信运行数据和跨工具临时产物 | 5/8/10/安全补充篇 | 受限配置不能直接写调用执行意图/占用、预算或 checkpoint；同 Session 文件/shell 读到同一产物，跨 Session 与私有 tmpfs 不混淆 |
| SYS-A34：输入改写、压缩和导入后的 Auto 审核 | 6/7/8/9/10/11/安全补充篇 | 只从受信原文与委派关系取授权；派生内容、摘要、导入来源标签不扩大权限，材料不足不放行 |
| SYS-A35：必需工作区与默认 DeepAgent 基础能力 | 1/2/5/8/10/11 | 缺少工作区拒绝；默认文件、命令、TODO 和通用子 Agent 可实际调用；缺后端明确失败，默认装配不替代授权 |
| SYS-A36：共同压缩祖先后的分叉 | 6/8/9/10 | 新分支继承公共 C1 及其合法文件 facts；另一分支后续 C2 不进入；不因 branchId 不同重复摘要 |
| SYS-A37：unknown 的查询/提交/核对/恢复 | 3/5/7/10/12 | paused 可受理核对；材料不直接授予许可；事实追加后才判断 canResume；有兼容 checkpoint 复用结果，无则明确结束后新开，终态不复活 |
| SYS-A38：取消后旧队列、新 prompt 与重启 | 1/3/7/10/11 | 旧输入/queued 项可查但不自动执行；无冲突时新 prompt 可运行；显式继续所选旧队列按顺序调度，不夹带旧 follow-up |

真实模型评测至少覆盖编码修复、非编码工具、独立 Workflow 的业务直接调用/受控工具包装、长会话保持约束四类任务；分别记录产品类型、自身身份、model/config/generation/环境及客观产物。业务工具调用不合并两类预算、历史或恢复；成功率与成本阈值在取得基线后决定，本轮不报告未测量数字。

## 6. 已闭合的设计冲突与剩余问题

本轮已在文档层解决：

- TurnLoop 永久停止与长期 Session 的矛盾：AgentSession 协调 Agent 重建执行实例，SessionManager 的历史保持独立。
- 原方案仅冻结 Agent 指针的问题：Trace 固定资源 generation，包含 follow-up 和恢复。
- 工具 unknown 和任务终态混淆：副作用状态与 Trace 状态分开。
- pi 外部事件与本产品落盘承诺的差异：产品新增提交后发布保证；内部执行段可多次开始/结束，完整 Trace 的 trace.settled 仅一次。
- 历史模型状态与新任务模型选择的混淆：M10 从完整路径重建状态，M04 决定新 Trace 的有效配置，checkpoint 恢复使用原配置。
- Eino 默认摘要与保留近期历史的差异：M09 明确保留区及提交契约。
- 多章节分别排序上下文的问题：M08 为唯一完整管道定义。
- 子 Agent 的 RunPath 与唯一身份混淆：使用产品 invocationId，并保留父调用关联。
- 旧 Host 统管多种职责的歧义：Code 按 pi 拆分创建、会话控制、执行、历史和资源加载，产品扩展登记单独标注；独立 Workflow 不由该会话控制器管理。
- 旧工作流内置子 Agent、AgentSession/targetAgent 统一执行及 workflow-as-tool 禁令：替换为同级双产品分别调用与业务既有受控工具组合，被调方独立授权/冻结/票据/预算/版本/恢复，SDK 不新增专用跨 Agent 框架、共享总额或整树恢复。

需要产品决定的事项与需要后续技术验证的事项分开。前轮发现的审批持久化文字冲突，本次已按维护者确认的当前实例内存契约统一：M10 4.3.2/4.4 为基线，M05/M07/M12 及系统验收明确一次问答、有效期、去重和消费状态仅存当前实例内存；关闭、重开或重启不复活批准，checkpoint 只定位原冻结调用，不携带决定。持久执行意图/占用、预算、真实启动/结果及恢复定位仍按原调用去重，显式 Resume 复核当前策略；尚未占用且仍需审批时重新问，已占用但效果未知先核对，已有结果直接复用。DSH 的持久 asked/decided 保留为上游事实，不作本产品许可推导；Auto 最小决定元数据独立保留。本次不新增审计方案，也不改变参数冻结、原调用身份、未知效果核对或安全恢复规则。

| 类型 | 待完成事项 | 是否阻止开发方案定稿 |
| --- | --- | --- |
| 产品默认 | 必需工作区、DeepAgent 基础能力、workspace-write + ask、Auto 关闭、符合必要条件的 partial、取消后队列均已确认（D-06/07/10/19/24） | 已写入正文，不再作为待答产品问题；平台能力仍须验证 |
| 支持范围 | 首批模型/协议认证名单、各平台 shell/容器执行后端（D-05） | 作为开发方案的支持矩阵明确；参考 pi 和全环境目标不再重选 |
| 接入细节 | SDK/HTTP/SSE 的 DTO、子路径和游标实现；路由域已确定为 `/v1/sessions`、`/v1/workflows`、`/v1/workflow-runs`，内部 Web 经受控 L3 接口而非 import sdk；A2UI 仍为可选外层映射（D-12） | 由开发方案细化，Workflow 路由/能力已接线且有默认测试，真实 DTO 见开发 06/13，最终认证见验证记录，未要求完整 A2UI 时不阻塞底座 |
| 已闭合行为 | 同 Trace steering/follow-up、未知消息、空闲手动压缩、公共祖先摘要和受控核对已明确 | 适配仍需技术验证，不重复要求用户确认已定行为 |
| 参数 | 开发者代码默认预算、超时、重试，以及事件保留、资源上限与版本保留（D-13） | 终端用户无需配置执行上限；默认值及开发者调整方式在后续方案与验证中明确 |
| 技术验证 | hook/恢复、动态工具选择、存储提交、跨平台取消、DSH 式审批审计与 Zero 适配沙箱（V-01～07） | 开发方案必须列验证办法与失败退路，不以静态读码宣称已实现 |
| 模型兼容验证 | 统一入口、思考映射、流生命周期、溢出识别与缓存请求/统计/回退（D-17、M-E09～21） | 每个认证模型/API 单独验证；Agentic 缓存写明细需补采集或明确未知，不能引用旧组件 getter 代替 |

### 6.1 后续技术验证清单

- **V-01**：验证 Agentic DeepAgent/TurnLoop 的内外循环适配：有/无工具轮次的轮后钩子、停止时不读队列、终答与两类输入竞争、同 Trace 的外层续执行；不重入 Runner 或用 preempt 冒充 steering。
- **V-02**：分别验证 Eino 中断/恢复与各自 generation/投影/交互记录一致；普通 Code 子 Agent handler 不串线，Workflow 已有节点审批/显式恢复及静态子流程/条件/并行汇合保留，跨类票据和 checkpoint 错配拒绝，不以此保证完整持久子树恢复。Code 原生预检生命周期回调隔离已按批准 Step9 获生产规格/质量及 Windows／实际 Linux 受影响范围普通/race 有界接受；唯一方法见 [开发 09 §checkpoint](../pi-eino-dev-plan/09-persistence-and-recovery.md#checkpoint)，固定 Eino v0.9.21、每次新建且单次使用探针，只认证原目标根，宿主 codec 仍可先执行。修前失败保留，Workflow 没有原生整图 checkpoint；该有限结果不认证任意宿主零执行、整树或整体 P3，父最终冻结源两平台全仓结果见 [开发12 当前证据与限制](../pi-eino-dev-plan/12-delivery-and-validation.md#当前证据与限制)。
- **V-03**：验证中立 storage 的 jsonl/memory 契约及各自提交边界；历史/节点、输入去重、任务状态、durable 事件和 checkpoint 故障后可核对，两类日志/写入者/审批/恢复不串线，旧开发数据不迁移不自动删。分别验证核对材料/事实、许可占用与启动/结果之间崩溃、已有结果及可信原文承接；不把共用后端当作跨 Agent 原子事务。
- **V-04**：在全部目标环境验证文件路径、shell、后代进程取消、编码/换行和目录持久化，说明容器与宿主路径映射。
- **V-05**：验证 DSH 策略与 Zero 原生实现在 Go/Eino 的落地：最终参数校验/冻结、必要审计与占用、受限 argv/文件后端、full/partial/设施错误及一次许可恢复；逐平台验证运行数据写保护、会话产物映射与私有临时区，Auto 原文来源用受控输入验证。不得以静态参考或 Zero 源码存在充当沙箱运行测试。
- **V-06**：验证 Agentic 泛型路径一致，按内容块识别工具结果；检查两阶段消息转换、普通扩展停用后回放，以及观测导出不可用时产品 traceId/状态仍完整、恢复时执行段事件按 executionId 区分，产品 trace.settled 不提前或重复提交。

- **V-07**：验证 Agentic 动态工具路径的实际执行清单与模型可见清单、普通/原生延迟检索、说明与预算、retry/resume 和工具选择提交一致；验证扩展操作不重入/自等待、会话 hooks 的候选冲突和提交边界。不将 Eino 存在 middleware 等同于产品恢复契约已实现。

这份清单只指出实现可行性需要验证什么，不选择具体锁、数据库、包签名或开发顺序。

## 7. 进入开发方案的条件

PRD 先达到四项条件：各章覆盖正常/异常/边界；每个跨章契约有唯一归属；产品关键未决事项已确认或明确接受建议；支持矩阵和技术验证责任已明确。满足后再制定开发方案，包括模块接口、存储布局、实现选择、验证原型和任务拆分。

当前状态：**原评审 R-01～03 与 Q-01～03 的默认行为保留，同级双产品及独立状态边界已在源码接线。** Code Agent 与中立存储已迁移；独立 Workflow Agent、公开工厂、Web 三类资源路由与独立视图已接通，Code 旧内置工作流已退出，有对应默认测试与 SDK 消费者。真实签名/DTO 见开发 06/10/13，最终认证见 [P3 验证记录](../p3-verification.md)，不据文档或局部测试声明整体通过。后续框架/平台证据仍需按实际范围认证，Code 原生预检生命周期回调隔离已按 [开发 09 §checkpoint](../pi-eino-dev-plan/09-persistence-and-recovery.md#checkpoint) 获 Step9 有界接受，修前失败及 codec/单次探针/固定版本/根资格限制保持；父最终冻结源两平台全仓结果见 [开发12 当前证据与限制](../pi-eino-dev-plan/12-delivery-and-validation.md#当前证据与限制)，P3 仍未完成。

## 8. 本轮整体评审的收口结果

用户已明确工作区必需，并纠正默认能力组合为 DeepAgent 的基础能力默认装配；其余摘要继承、效果核对、安全默认值及取消后队列按建议确认。本节记录最终规则，原先“工作区可选”“默认不带基础工具”的建议不再适用。

### 8.1 第 1～12 章覆盖结论

| 章节 | 当前规则与状态 |
| --- | --- |
| M01 定位 | 通用 Go 底座、Code 必需工作区/默认基础能力、同级独立 Workflow 已明确；正式前端不在范围 |
| M02 分层 | L1 internal/llm、L2 internal/agent、同级 L3 codeagent/workflowagent 及中立 storage 为批准目标；两类互不导入、状态独立，Agent 与 Code 入口保留；Code Agent 与共享存储已迁移，独立 Workflow Agent、工厂、Web 三类资源路由及独立视图已接通，有对应默认测试；最终认证见验证记录 |
| M03 Loop | Code 同 Trace 输入/Turn 与旧队列/核对恢复闭合；Workflow 使用独立节点生命周期，不经 AgentSession/targetAgent |
| M04 模型 | Agentic、统一流/usage/缓存/重试契约由两类分别装配与记账；真实服务认证按本项目证据判断 |
| M05 工具 | 各自冻结/授权/票据/预算/未知效果/结果承接；普通 Code 子委派父约束与业务跨类独立策略分开 |
| M06 消息 | 三类标准语义与派生来源保留，两类历史不自动合并，工具包装只配对调用方结果 |
| M07 事件/接入 | 外层分别调用，两类事件/游标/审批/恢复身份独立；原 Code 路由不扩为 Workflow，P3 不由文档认证 |
| M08 上下文 | Code Session 保留绑定/资源/祖先路径，Workflow 只取显式获准输入，不自动读取 Code 历史/skill |
| M09 压缩 | Code 公共祖先摘要及合法文件事实可继承，普通子投影隔离且受父预算；不管理 Workflow 节点树 |
| M10 会话 | Code 稳定绑定/历史树/核对/checkpoint；Workflow 自己提交节点与恢复；共 storage 不共日志/写入者，旧开发数据不迁移不自动删 |
| M11 扩展 | Code Registry 保留普通委派；独立 Workflow 定义/编译/静态节点能力与业务工具组合；Coze/动态加载/热重载/补参仍属未来，不启动 P4/P5 |
| M12 安全 | 两类各自策略/审批/票据/预算与节点恢复；workspace-write + ask、Auto 关闭、必要保护下 partial 保留；核对不直接释放许可 |

### 8.2 三项技术语义的处理结果

| 编号 | 已采用规则 | 规格来源与验收 |
| --- | --- | --- |
| R-01 | Code AgentSession 必须显式绑定工作区，缺失拒绝；默认基础能力由 Code 创建入口装配。裁剪工具后仍保留绑定；Workflow 按自身定义与策略装配，独立 L1/L2 不反向依赖任一产品运行对象 | [M01](01-product-foundation.md)、[M02](02-architecture-boundaries.md)、[M08](08-context-engineering.md)、[M10](10-session-persistence.md)；SYS-A35 / SESSION-A21 |
| R-02 | 按当前 leaf 的祖先路径和覆盖关系选择 CompactionEntry；公共 C1 可继承，另一分支独有 C2 不自动使用；branchId 不代替路径判断 | [M09 重建](09-compaction.md#411-重建与-eino-适配)、[M10 分支](10-session-persistence.md#44-分支压缩与权限的关联)；SYS-A36 / CMP-A23 / SESSION-A22 |
| R-03 | Code 由 AgentSession/SessionManager 协调并提交核对；Workflow 由自身受控入口/节点记录独立提交，使用各自调用身份、票据与 checkpoint。追加材料和可确认事实后才计算自身恢复资格；核对不重跑工具、不自动 resume，不凭人工标签解除许可，终态不复活 | [M05 核对](05-tool-system.md#核对未知执行效果)、[M07 操作](07-events-and-access.md#核对操作的语义)、[M10 提交](10-session-persistence.md#433-核对事实的提交与恢复资格)、[M12](12-security-sandbox.md)；SYS-A37 / T-E18 / SESSION-A23 / SEC-A23 |

### 8.3 已确认的默认行为

| 编号 | 最终决定 | 状态 |
| --- | --- | --- |
| Q-01 / D-07 | Code 默认具备文件读写/搜索、命令执行、TODO 和 general-purpose 子 Agent；AgentSession 必需工作区。基础提示词说明实际能力，应用可显式覆盖、裁剪或替换；Workflow 按自身定义和独立策略装配，不自动继承 Code 基础能力 | 用户明确修正并确认，已落正文 |
| Q-02 / D-06、D-19 | workspace-write + ask，Auto 关闭；partial 满足运行数据保护等必需条件后可用并标识限制；不满足则拒绝受限配置，不自动 Full access | 用户确认按建议，平台实现仍待验证 |
| Q-03 / D-10 | 取消完成后保留旧记录并暂停当时旧独立队列的自动启动；无冲突时新 prompt 可开始，旧队列显式继续；原 Trace 未消费输入重做使用新身份关联旧记录 | 用户确认按建议，已落 M03/M07 |

### 8.4 后续开发方案需要确定的内容

以下是工程方案及验证责任，不再作为要求用户重选的产品默认行为：

- 按既定模型/全环境方向给出协议、endpoint、shell、容器和平台认证矩阵；不以 Eino 接口或源码存在宣称某组合已经受支持。
- 明确 L1/L2/L3 包和窄接口、默认后端装配、工具裁剪方式、Go 包注册入口；不引入未要求的动态代码加载平台。
- 明确历史/许可/核对/事件/checkpoint 的提交和恢复办法，验证旧资源与已有工具结果可承接；不承诺外部副作用恰好一次。
- 沿用[开发方案 12 defaults](../pi-eino-dev-plan/12-delivery-and-validation.md#defaults) 中预算、超时、取消确认、事件窗口、资源版本/产物保留的工程默认策略，由两类独立装配和记账；终端用户无需填写底层 runner 或运行上限，本章不另造数值。
- SDK/HTTP/SSE 对 Code 与 Workflow 分别映射自身公开操作，细化各自 DTO、路由、游标和 Web 验证方式；不复用 AgentSession/targetAgent 执行 Workflow。唯一公开 import/生产文件约束不变；A2UI 按后续明确场景选择，不阻塞当前底座。
- Go 编译注册保持现有范围；新代码运行时加载、完整 OAuth/供应商目录、正式前端和远程多用户服务没有因此自动纳入。

### 8.5 进入下一阶段的判断

本轮提出的三项技术收口和三项产品确认均已落实到文档；开发方案已形成，当前产品目录、Workflow 工厂与 Web 独立资源/视图已有实际调用链及默认测试；本轮只同步受影响正文，不改历史设计或认证父流程的最终结果。后续实现仍须将 V-01～07 及 38 条系统验收作为验证入口。前轮审批持久化文字冲突，本次已按第 6 节的当前实例内存契约统一；V-02 的 Code 原生预检生命周期回调隔离已获有界接受，方法与 codec/单次探针/固定版本/原根限制见 [开发 09 §checkpoint](../pi-eino-dev-plan/09-persistence-and-recovery.md#checkpoint)。修前失败保留，Workflow 无原生整图 checkpoint；文档修订不扩大恢复认证，Blob H1/M1、13.2e清单 publisher、Workflow.Close 安全错误及可写创建父目录同步均经独立复核和父两平台实际证据有界接受，清单通用失败阶段旧 ID/mode 测试补强建议保留；Repair 完整旧 WRITE_THROUGH 暂停不变，方法见 [开发 09 §layout](../pi-eino-dev-plan/09-persistence-and-recovery.md#layout)。父最终冻结源两平台全仓与显式消费者及原 Step14 目标均已通过，唯一授权四协议 live 与原样完整 E2E 已各执行一次并通过，无自动重跑或追加调用；顺序、计数、安全剩余模块告警与外部 Node 原生 env-file 桥接条件见 [开发 12 当前证据与限制](../pi-eino-dev-plan/12-delivery-and-validation.md#当前证据与限制)。P3、macOS 与其它开放项仍按父验证记录保留，不因最终绿色完成。

最关键的验证仍是 Eino 内外循环及恢复适配、历史/许可/核对/事件/checkpoint 的关联提交，以及真实沙箱与临时产物映射。[开发方案](../pi-eino-dev-plan/README.md)于 2026-09-24 完整重写，按十二个技术章节明确选型、接口、数据、架构/时序和逐条需求覆盖。部分历史 Eino 定向测试已通过，具体证据及适用边界见[验证记录](../pi-eino-dev-plan/12-delivery-and-validation.md#evidence)，不将上游探针推导为当前产品认证。双产品工厂、独立节点执行与 Web 资源/视图现已有实际调用链和默认测试；完整平台、浏览器、真实模型和基础恢复认证见父验证记录，原生沙箱、动态加载等未来项仍不提前交付。本子项仅同步文档，没有产品代码改动。
