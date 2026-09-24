# 第 9 章：上下文压缩

状态：按 M09 摘要策略与文件跟踪 review 修订的 PRD；行为和验收已定义，尚未完成运行实现验证。对应教程 [M09 上下文压缩](https://dg-ai-notes.pages.dev/modules/ch09-compaction/)。

本章只定义压缩行为、摘要质量与一致性；调用前预算归[第 8 章](08-context-engineering.md)，历史树和 checkpoint 恢复归[第 10 章](10-session-persistence.md)，任务状态归[第 3 章](03-agent-loop.md)。

## 1. 执行摘要

### 1.1 问题与方案

长任务需要腾出模型窗口，但直接删除旧消息会丢失用户约束、未完成工作和工具证据；直接把 Eino 的新 Messages 当作产品历史，会破坏回看、分叉及恢复。

建议在安全边界对旧历史生成有来源的结构化摘要，保留近期完整消息与活动任务事实，再以追加压缩记录的方式改变模型投影。原始历史继续存在，摘要失败不会成为删除历史的理由。

### 1.2 成功标准

- 压缩前后用户可回看的原始消息、entryId 与工具结果不变；新投影能追溯其输入范围和摘要版本。
- 每个最终投影都通过工具调用关联检查；当前未闭合调用、人工交互和任务输入不得消失。
- 摘要失败、取消、持久化失败和提交冲突均不激活半成品投影。
- 压缩及恢复不重放已完成的副作用工具；真实模型质量通过固定事实核查集评估。

### 1.3 本章的压缩流程

压缩分为：固定历史范围 → 从近期向前选择合法保留起点 → 生成首次/增量摘要及必要的工作前缀摘要 → 由代码累积文件事实 → 校验完整输入预算 → 原子提交并重建投影。默认复用 Eino 的模型与摘要扩展点，不为压缩建立新的 Agent 循环。

三项设计原则：保留最新且合法的消息组合；使用固定结构和增量更新减少关键信息遗漏；将文件清单等可确定的信息交给代码保存。原始历史仍是事实来源，摘要及清单都是有范围的派生结果。

## 2. 用户体验与功能

### 2.1 角色与故事

| 角色故事 | 功能结果 | 验收 |
| --- | --- | --- |
| 作为长任务用户，我希望对话变长后仍能继续且保留原要求 | 自动压缩保留目标、约束、决策、完成证据和后续任务 | CMP-A01、A02、A10 |
| 作为应用开发者，我希望压缩失败不会损坏会话 | 候选验证通过且持久提交后才替换投影；可查询失败原因 | CMP-A03、A05、A06 |
| 作为需要恢复任务的用户，我希望压缩不会导致命令重复执行 | 压缩版本、checkpoint、工具结果按同一执行边界协调 | CMP-A07、A08 |
| 作为探索方案的用户，我希望分叉时按需带走已有结论 | 可选择生成来源明确的分支摘要，原分支内容保留 | CMP-A09 |

### 2.2 功能需求

| 编号 | 需求及边界 |
| --- | --- |
| CMP-01 | 支持预算触发、明确的模型上下文溢出恢复，以及用户显式请求；触发原因分别记录，不把所有模型错误当作压缩信号。 |
| CMP-02 | 压缩只在无模型输出正在追加、相关工具结果已结算的安全边界执行；同 Session 的压缩提交与顶层历史写入由同一 AgentSession 顺序协调，委托 SessionManager 实际提交。 |
| CMP-03 | 自动压缩属于当前 Trace 的内部阶段，事件保留 traceId 与执行作用域；压缩及续执行不新建 Trace、不提前发布 trace.settled，内部执行段事件按 M03/M07 处理。手动压缩使用 operationId，仅在无活动/待恢复 Trace 且队列为空时受理，忙时 conflict。 |
| CMP-04 | 沿当前合法分支从最新向前累计近期保留目标，再选择有效消息组边界；firstKeptEntryId 是第一条保留记录且包含自身。assistant 工具调用及其结果不得跨边界拆散；Agentic 内容块语义而非 user role 决定合法性，详见 3.2。 |
| CMP-05 | 当前用户输入之后的多轮执行历史过长时，可摘要已完成的工作前缀并保留近期后缀；不得拆开单次模型调用的工具因果组。前缀摘要保留原始请求、早期进展和后缀所需上下文；M03 的 Turn 定义不变，详见 3.3。 |
| CMP-06 | 默认主摘要采用六部分结构，区分目标、约束、进展、决策、下一步和关键上下文；文件清单由代码根据工具事实独立累计并附加，不依赖模型列全文件。生成规则见 3.1，文件证据与累积见 3.4。 |
| CMP-07 | 后续压缩使用上一份生效摘要与本次新覆盖范围进行更新，保留目标/约束，迁移进展状态并更新下一步；不重新摘要已覆盖原文，不拼接全部旧摘要。没有新增可压缩内容时不重复调用模型。 |
| CMP-08 | 摘要调用不提供业务工具；模型错误、取消、截断、toolCall、空摘要、缺必需结构或超预算的候选不提交。首次/更新/前缀的模型与策略版本、各次 usage 分别记录；结构校验不等于自动证明摘要语义完整。 |
| CMP-09 | 压缩不修改产品消息原文；通过不可变 compaction 记录表达原历史范围、保留起点及新摘要。激活前完成结构校验、最终预算检查和持久化确认。 |
| CMP-10 | 阈值压缩失败且原模型请求仍满足硬预算时可用原投影继续并报告失败；已经溢出或硬预算不满足时停止本次模型调用，返回 `context_budget_exceeded` 或有界摘要错误。 |
| CMP-11 | 对同一模型调用边界，自动溢出恢复建议最多一次“压缩并重试模型”；不得借机重跑历史工具。仍失败则明确结束/请求用户选择，不陷入无限压缩重试。 |
| CMP-12 | `waiting_input` 的运行状态与 checkpoint 不做后台改写；resume 先恢复原版本，完成该交互及关联工具结算后才允许下一安全边界压缩。 |
| CMP-13 | 子 Agent 压缩只改变其自身 invocationId 的投影；返回主 Agent 的结果摘要是委派输出，不自动覆盖主历史或成为新的全局事实。 |
| CMP-14 | 分支摘要仅覆盖被离开路径上相对公共祖先的独有部分，携带原分支和目标分支来源；生成失败时不声称已带入结论，分支切换规则以第 10 章为准。 |
| CMP-15 | 扩展可经 M07 session_before_compact 取消、补充摘要重点或提供替代候选；输入范围与保留边界已由产品固定，候选仍满足本章结构、预算、usage 和不执行业务工具的规则。文件集合/附录由代码从原始工具事实生成，不接受扩展自行宣称的修改清单。 |
| CMP-16 | 扩展请求活动 Trace 压缩时，仅登记该 invocation 的安全边界请求；工具批次或审批等待中不执行，不重入当前 compact。无可压缩内容返回无操作结果；请求不强制增加 Turn。空闲手动请求仍按 CMP-03 校验。 |

服务端溢出信号由 M04 的识别契约提供，包含来源与判别依据；本章只负责受控压缩恢复，不再次独立匹配厂商错误。限流、普通输出截断、证据不足的空输出不因“重试可能有用”就自动压缩。预算触发和用户手动压缩仍可独立成立，须记录对应原因。

扩展候选与默认摘要使用同一校验和提交路径；多个替代候选冲突、hook 取消/异常均不激活半成品，是否可用原投影继续按 CMP-10 的硬预算判断。分叉/导航的候选摘要同样遵循 CMP-14 与 M10 的目标范围，不能扩大到别的分支。完成通知在持久提交后触发；失败通知携带阶段，不调用第二套独立压缩器。

### 2.3 正常、异常与边界轨迹

**正常**：一轮工具全部结算 → 下一模型调用预算超软阈值 → 固定历史叶子与保留边界 → 摘要旧部分 → 验证并提交 compaction → 构造“摘要 + 近期消息 + 冻结规范” → 同一任务继续。

**异常**：摘要模型超时 → 记录失败但不提交 compaction → 原投影仍在硬预算内则继续；原投影已超限则报告预算不足。原始历史、完成工具结果和输入队列均保留。

**边界**：摘要生成期间收到 follow-up → 仍留在输入队列，不混入已经固定的摘要范围 → 摘要提交后仍等待第 3 章规定的消费边界。收到 Abort 时停止生成；如果 compaction 已完成持久提交则保留该合法记录，否则丢弃候选。

非目标：保证摘要无语义损失、删除旧历史以节省存储、长期跨 Session 记忆、自动证明代码正确、用摘要替代工具执行记录或 checkpoint。

## 3. AI 系统需求

### 3.1 摘要输入与输出

摘要输入是明确界定的历史材料，不能作为新的执行任务。当前用户输入、AgentSession 已固定的任务约束和工具真实结果独立保留关键元数据，不能仅依赖模型自由复述。安全规则与任务权限不得由摘要推断或扩大。压缩保留授权原文和限制的持久来源引用，但摘要自身仍是 checkpoint/fact；Auto 按 M12 回到可解析的受信原文判断，引用缺失或预算不足时不能靠摘要补出许可。

默认主摘要是有固定小标题的 Markdown，不要求转换成一套复杂 JSON schema；标题可本地化，但以下语义字段必须齐全，无内容时明确写“无”或“未知”。

| 部分 | 必须保留什么 |
| --- | --- |
| Goal | 用户目标及已明确的范围变更 |
| Constraints & Preferences | 已确认限制、偏好和不能遗漏的约定 |
| Progress | 分列 Done、In Progress、Blocked；完成项带证据，解除的阻塞更新状态 |
| Key Decisions | 有效决策及简要理由，注明后续纠正或被替代的结论 |
| Next Steps | 按当前进度更新的下一步，未执行计划不写成成果 |
| Critical Context | 后续必须参考的路径、符号、错误、产物、未决副作用和必要引用 |

文件“被写入”不等于修改正确，命令“已结束”不等于测试通过。文件清单附录由 3.4 的程序逻辑产生，与这六部分模型正文分开。开发者可替换提示词模板或增加摘要重点，但仍须满足来源、必需字段、预算和不执行业务工具的约束；不要求终端用户配置摘要规则。

#### 3.1.1 摘要模型实际收到什么

复用 M06 的消息可见性及 Agentic 转换规则，将选定历史序列化为有明确角色/调用标识的待总结材料，再以独立的摘要指令请求模型。用户输入、助手文本、工具名称/参数、结果/错误以及必要引用分别标记；不能把旧工具调用当作本次待执行指令。Agentic 的 user role 可能装着 FunctionToolResult，序列化必须保留其“工具结果”身份。

对超大工具正文可生成有截断提示的摘要材料，保留退出状态、关键信息和产物引用；材料自身经过预算，不能原样重发已经溢出的请求。必需的多模态内容若无法用文本和引用表达，使用已获准且具备相应能力的摘要模型或保留必要原内容，不能静默删除。排除模型的消息不能通过摘要正文或文件附录重新进入上下文。

默认使用当前作用域已生效的模型及 M04 调用能力，摘要不继承业务工具或服务端副作用工具。独立模型/更大窗口 fallback 由装配策略明确选择，不能自动换服务；发生多次模型尝试时仍计入本次压缩和原 Trace/维护操作预算。模型、模板、策略及来源范围在一次候选生成期间固定。

#### 3.1.2 首次摘要与增量更新

| 模式 | 输入 | 输出及更新规则 |
| --- | --- | --- |
| 首次 | 本次选中的旧历史，无 previousSummary | 生成六部分主摘要，保留近期原文作为独立后缀 |
| 增量 | 当前分支上一份生效摘要 + 自其保留起点以来本次新纳入压缩的历史 | 生成一份更新后的摘要；不再次输入上一份已覆盖的全部原文 |
| 没有新增范围 | 已有摘要，未产生新的可压缩历史 | 不再次调用摘要模型，不追加重复 compaction 记录 |

增量更新保留仍有效的目标、约束与决策，结合新证据把 In Progress 移到 Done、解除 Blocked、更新 Next Steps。只有新证据或用户明确变更支持时才删除/替换旧结论；精确路径、符号和关键错误不随意改写。上一份摘要从已提交记录读取，不以模型自己记忆或缓存答案作为来源；文件 metadata 按 3.4 独立合并。

模型正文结构、调用失败和预算可做确定性校验；摘要是否遗漏语义通过评测集验证，不宣称存在自动“事实正确性证明”。不合格候选按有界策略重试或失败，旧投影保持；不存在偷偷截断摘要后直接发布的后备路径。

摘要后仍需使用的 skill 保留名称、generation、内容哈希及加载状态；关键指令由同版本快照重新注入或保留正文，不能让自由摘要改变其规范。活动 skill 正文装不下时按第 8 章报预算问题，不将随意截半的文本标作完整规范。

分支摘要使用不同的目的说明，明确它是另一分支的探索材料，保留尝试、发现、失败原因及可查文件/产物，不能伪装成目标分支已执行的事实。它与普通压缩共享消息转换和模型调用/错误处理，分别设置来源范围及有界输入/输出预算；具体对照见 [M08](08-context-engineering.md#45-compaction-与分支摘要各自保留什么)。目标分支的必需输入优先保留，不能为了插入分支摘要将其丢弃。

### 3.2 合法切点与近期保留区

`firstKeptEntryId` 指第一条保留的 SessionEntry，区间从它开始（包含自身），不是最后一条被摘要记录。来源使用当前分支的有效上下文：最新生效摘要、上次保留的原文和后续已消费消息；不将所有历史分支或已覆盖原文重新展开压缩。

默认选择方法：

1. 由内部 AgentMessage 和调用关系判断消息语义；排除工具结果作为独立切点，禁止将同一助手工具批次与对应结果拆到两侧。单靠 AgenticMessage.Role=user 无法判定用户输入。
2. 从最新消息向前累计，达到开发者设定的近期保留目标后，在该位置或更靠近末尾处选择最近的合法起点。元数据条目不冒充模型消息；必要关联记录随所属历史保留。
3. 用户输入或助手消息可成为候选起点，但仍须完整工具组校验；应用自定义消息只有投影语义和依赖关系明确时才参与切点判断。
4. 切点落在当前输入之后的连续工作中时使用 3.3 的前缀摘要。当前输入原文/仍有效限制需有可定位的保留来源，不能因切到助手消息而丢失任务由来。
5. 使用合并摘要、文件附录、保留消息以及 M08 的全部指令/工具重新预算。近期目标是选点依据，不是绝对的字节/token 承诺；合法性与必需内容优先。没有可压缩范围或仍无法放入窗口时明确反馈，不丢未结算调用强行压缩。

例如切点为 e30，则 e30 开始的近期记录保留，e30 之前本次选中的范围参与摘要；记录中保存稳定 entryId 与覆盖范围，而非仅保存易漂移的数组下标。窗口大小、近期保留目标和摘要输出上限分别计算，不能用同一个参数控制三者。

### 3.3 长工作历史与前缀摘要

pi 压缩代码的 turnPrefix 从用户式输入处回溯，可能涵盖多次模型/工具轮次；它与 M03“一次模型调用及其工具执行”的 Turn 不是同一粒度。本产品称其为**工作前缀**，不新增一个公开 Turn 类型，也不切开单个工具因果组。

```text
旧摘要（如有）
更早的完整历史 H   → 首次/更新后的主摘要
当前输入 U + 已完成的早期工作 P → 工作前缀摘要
切点 C + 近期工作 K             → 保留原文（含 C）
最终输入：主摘要 + 工作前缀摘要 + 程序生成附录 + C..K
```

H 在 U 之前结束，前缀从 U 开始到 C 之前，保留区从 C 开始，三个范围不重叠。当前输入原文与必要约束通过输入记录/必要内容保留供恢复和验证；前缀摘要默认含 Original Request、Early Progress、Context for Suffix，说明正在做什么、早期结果以及近期后缀需要的背景。

上图适用于 U 仍在本次未摘要原文范围的情况。U 已被前次压缩覆盖时，不回头重新展开旧消息；使用 previousSummary 中的请求背景及原输入引用，新增前缀从当前有效的未覆盖范围开始。多次切分同一长工作历史时，来源范围仍须可去重，不把同一段原文反复当作新增材料。

主摘要有新增 H 时按 3.1 更新；H 为空但有 previousSummary 时直接保留旧摘要，不用“没有历史”占位将其覆盖。需要时分别生成主摘要和前缀摘要，再合成一个压缩候选；各次模型调用单独计量、合并后统一预算。并行与否留给实现，默认不要求并行。任何必需部分失败、被取消或被截断，都不激活只有另一半的摘要。

### 3.4 文件记录由代码累积

编码场景沿用 pi 的文件清单思路：模型总结工作内容，代码维护有证据的文件记录。复用 M05 已有工具调用与执行结果，不依赖解析自然语言摘要，也不为压缩增加工作区监控、全量 Git 扫描或新的审计服务。

| 来源/状态 | 文件记录规则 |
| --- | --- |
| 上一份当前分支生效 compaction 的受信 details | 继承其 readFiles/modifiedFiles 及来源关系；未知格式或外部导入摘要不能直接升级为已确认文件事实 |
| 本次 H 和 P 覆盖的文件工具记录 | 用工具种类、规范化资源路径和执行结果提取 read/write/edit；保留调用身份以追查 |
| 已确认读取或写入/编辑效果 | 纳入相应集合；写入成功不表示业务正确或测试通过 |
| 工具被拒、未启动、确认未生效 | 不因模型提出路径就记为成功读取/修改 |
| 工具失败但已确认部分修改 | 已确认的单项效果仍计入 modifiedFiles，其余失败/未知按原调用记录保留 |
| 副作用未知 | 保留 M05 原调用和未决状态，不擅自放入已确认 modifiedFiles；核对结果以后按关联事实更新 |
| shell、子 Agent、自定义工具 | 仅合并能够关联到该来源范围的结构化文件效果；没有可靠路径信息就标明覆盖有限，不从命令字符串猜文件，也不重执行来探测 |

本地 pi 从助手 toolCall 的 name/path 提取文件，不检查工具结果是否成功；本产品按上表结合执行事实，这是明确补足。对通用非编码 Agent，文件 tracking 可为空，不要求凭空推断文件或将核心绑定在 read/edit/write 这三个工具名上。

累计与发布规则：

- 合并来源是“上一份受信 metadata + 本次新覆盖范围”，不是只扫描本次消息，也不是从旧摘要中的 XML 文本反推事实。保留区中的调用尚未被替换，后续纳入压缩时再合并，不能跨分支汇总全 Session 文件。
- 规范化身份包含工作区/执行环境与路径；按平台路径规则去重，不能统一转小写导致 Linux 两个不同文件合并。显示用稳定路径，原调用提供证据引用。
- modifiedFiles 合并已确认写入/编辑；readFiles 表示只读且未进入 modifiedFiles 的文件，采用 pi 的最终清单语义。两组稳定排序，既读又改只显示在 modifiedFiles。原工具记录仍可查询全部读取行为。
- 文件清单记录所选历史范围内发生过的操作，不保证文件当前内容、当前 Git diff 或修改仍未被撤销。恢复/编辑时仍按 M05 验证真实工作区。
- 模型正文与代码附录在逻辑上分开；模型不得作为文件清单的权威生成者。程序在正文通过校验后生成 read-files/modified-files 分隔块或等价结构，并与 details 一致保存。附录格式标记由代码管理，不重复追加模型输出的同名标签。
- 输出给模型前沿用 M06 的可见性和权限规则；排除内容的路径不能借文件附录泄露。清单过大时完整集合仍保存在 metadata/受控引用中，模型附录给出有界清单及可查入口，并纳入总预算，不能静默丢失累计记录或无界扩充提示词。

例子：第一次确认 read(a)、read(b)、edit(a)，得到 readFiles=[b]、modifiedFiles=[a]；第二次确认 write(c)，并收到一次被拒绝的 edit(d)，则新清单为 readFiles=[b]、modifiedFiles=[a,c]，d 只保留在拒绝记录中。文件名不靠模型重述，连续压缩不会丢掉 a。

### 3.5 预算和质量评估

压缩触发由第 8 章对完整请求计算，建议软阈值小于硬预算；近期保留目标、摘要模型输出上限和主模型回复预留分别由代码策略给出，结合当前模型窗口与 M04 有效思考/输出配置计算，不要求用户配置。摘要请求自身也须预算，不能把已超限全部历史原样送往同一小窗口模型。

摘要材料（含 previousSummary 和指令）仍过大时，允许开发者装配的有界分段摘要或获准的更大窗口模型，分别记录额外调用与费用；如果没有可用策略，明确失败。摘要模型自身不能再次触发无限递归压缩。跨供应商 fallback 必须符合当前模型接入和数据范围配置，不默默把内容发送给新服务。

确定性测试使用受控摘要结果验证事务边界、工具成对和失败回退。真实模型评测用固定长对话及标注事实表，核查约束召回、任务状态准确、路径/错误准确、证据引用与续作表现。建议最低发布条件：必守约束与未完成任务无遗漏、无新增成功声明；整体事实保留率及成本指标由系统闭合检查中的评测集确认，不声称已经达到。

## 4. 技术规格

### 4.1 压缩对象与提交过程

| 对象 | 产品契约 |
| --- | --- |
| 压缩候选 | 固定 Session/分支、基准叶子 entryId、traceId/executionId/invocationId、generation、输入投影版本、覆盖范围、保留起点、摘要模型/策略版本 |
| 已提交记录 | 不可变摘要正文及程序附录、details 文件集合与证据范围、firstKeptEntryId（包含）、上一压缩记录引用、估算前后大小、分次及合计 usage、模型/模板版本与原因；字段布局后定，自身是新的 entryId |
| 新模型投影 | 当前分支最新生效摘要（含前缀和允许的附录）+ 从 firstKeptEntryId 起的保留原文 + 此后消息；按 entryId/inputId 去重，M08 再组装规范/skill，不复制成用户原始输入 |
| 进度事件 | `compaction.started / finished / failed`，关联同一 operationId、traceId 和分支；完成只表示压缩已提交，不代表业务任务成功 |

建议提交顺序：AgentSession 固定稳定边界，从 SessionManager 取得历史 → 生成候选 → 校验结构与预算 → AgentSession 确认分支叶子/任务取消状态仍允许提交 → SessionManager 持久追加记录 → AgentSession 激活新投影并对外发送完成事件。若持久写入失败，保留旧投影；若写入成功后崩溃，恢复从已提交记录重建，不再重复提交同一操作。

具体数据库事务、日志格式和 Eino hook 选择留给后续开发方案；本 PRD 要求可观察结果满足上述原子边界。Eino 内部 state 可以包含压缩后的 Messages，SessionManager 必须通过本产品的 SessionStore 存储接口独立保存原始历史。SessionManager 负责历史树与上下文来源，SessionStore 只表达存储后端契约，二者不互为别名；完整职责见[第 2 章](02-architecture-boundaries.md)。

#### 4.1.1 重建与 Eino 适配

SessionManager 只管理记录和分支，不调用摘要模型。重建时选择当前 leaf 的祖先路径上最新有效 CompactionEntry，用其中的摘要替代其已覆盖范围；保留 firstKeptEntryId 本身到该压缩记录之前的合法原文，再追加压缩记录之后的消息。旧摘要被新摘要更新后不重复拼接，原记录仍可回看。

共同祖先上的 CompactionEntry 可以被后续分支继承，前提是该记录、覆盖关系和 firstKeptEntryId 在所选路径中仍合法。记录的创建 branchId 不必等于当前分支头引用；不得为了分支 ID 不同而丢弃公共摘要。分叉之后另一分支新增的压缩记录不在本路径，不能自动借用；文件 details 也按同样来源继承，不汇总其他分支的独有操作。

例如旧记录 e1～e6、firstKept=e5，新压缩记录 e7、新消息 e8，则投影为“e7 的摘要 + e5 + e6 + e8”，不是“摘要 + e1～e6”，也不是从 e6 才开始。分支或保留起点无法解析时明确报告损坏/不兼容，不猜测数组位置继续。模型看到带来源说明的 CompactionSummary，经 M06 转为 Agentic 输入；details 不自动全文泄露。

执行层使用 summarization 的 TypedConfig/GenModelInput/Finalize 等 Agentic 扩展点实现相应行为：输入构造选定材料及首次/更新指令，Finalize 组合摘要与保留区，产品协调完成候选验证和提交。默认 Finalize 的“system + 摘要”不是本产品契约；Callback 是观察点，不能据其存在推断持久提交已原子完成。具体接线留待开发方案，不能同时启用另一套会删改历史的独立压缩器。

### 4.2 与执行、恢复、版本的协同

1. 运行内压缩不创建新的顶层任务，不重新调用已经完成的工具；模型溢出重试只重做尚未成功的模型请求。
2. 正在运行工具或等待 interactionId 回复时，不改写其消息位置、参数或关联关系；人工交互并非可供压缩丢弃的旧聊天。
3. checkpoint 必须能确定其绑定的上下文投影版本与 generation。恢复旧 checkpoint 不直接拼入更晚的压缩状态；需要由第 10 章判定一致恢复边界。
4. 新 Trace 从当前合法 Session 投影开始；resume 保持原 traceId、创建新 executionId，恢复原 generation 及 checkpoint 当时有效的模型/上下文版本。同 Trace 的 follow-up 使用已提交的新投影，不新建完整运行。
5. 生成摘要期间的输入受理/消费、Abort、分叉和热更新由 AgentSession 串行协调；已受理但未消费的输入不参与摘要，新 generation 不在摘要完成时顺便激活。模型请求与摘要来源都按 M08 的 inputId 消费边界去重。

自动压缩可在 M03 规定的模型/工具安全边界准备，也可为溢出后的内部续执行服务；不将教程“只在完整对话结束后”当作唯一触发时机。其完成事件仅表示候选已提交，最终 Trace 完成由 M07 的 trace.settled 决定；内部 agent_end 不作为摘要已持久化或整个回复已完成的证据。

### 4.3 pi 与 Eino 实际依据

| 已核查事实 | 证据 | 本产品适配要求 |
| --- | --- | --- |
| pi 优先使用最近可信 usage 加后续消息估算，按窗口减 reserveTokens 判断 | [估算与阈值](../../pi/packages/coding-agent/src/core/compaction/compaction.ts#L202) `[VERIFY: pi/packages/coding-agent/src/core/compaction/compaction.ts:202]` | 借鉴余量思想，最终还要覆盖系统/工具及新模型基线 |
| pi 从后向前累计近期消息，寻找有效保留起点，并判断是否拆分输入之后的工作历史 | [切点](../../pi/packages/coding-agent/src/core/compaction/compaction.ts#L403) `[VERIFY: pi/packages/coding-agent/src/core/compaction/compaction.ts:403]`；[准备](../../pi/packages/coding-agent/src/core/compaction/compaction.ts#L736) `[VERIFY: pi/packages/coding-agent/src/core/compaction/compaction.ts:736]` | 工具组不可拆，工作前缀不等同于 M03 的一个 Turn |
| pi 默认采用固定六部分模板，增量规则更新进展和后续步骤 | [首次模板](../../pi/packages/coding-agent/src/core/compaction/compaction.ts#L467) `[VERIFY: pi/packages/coding-agent/src/core/compaction/compaction.ts:467]`；[更新规则](../../pi/packages/coding-agent/src/core/compaction/compaction.ts#L500) `[VERIFY: pi/packages/coding-agent/src/core/compaction/compaction.ts:500]` | 模板默认固定，语义正确性仍需评测 |
| pi 工作前缀包含输入起点，当前实现顺序生成主摘要和前缀摘要 | [分区](../../pi/packages/coding-agent/src/core/compaction/compaction.ts#L773) `[VERIFY: pi/packages/coding-agent/src/core/compaction/compaction.ts:773]`；[组合](../../pi/packages/coding-agent/src/core/compaction/compaction.ts#L873) `[VERIFY: pi/packages/coding-agent/src/core/compaction/compaction.ts:873]` | 不照搬教程的 user 排除在前缀之外、Promise.all 并行描述；旧摘要无新增 H 时仍保留 |
| pi 用 previous details 和工具参数累积文件集合，代码生成附录并保存 details | [继承](../../pi/packages/coding-agent/src/core/compaction/compaction.ts#L42) `[VERIFY: pi/packages/coding-agent/src/core/compaction/compaction.ts:42]`；[提取/去重](../../pi/packages/coding-agent/src/core/compaction/utils.ts#L29) `[VERIFY: pi/packages/coding-agent/src/core/compaction/utils.ts:29]`；[附录与记录](../../pi/packages/coding-agent/src/core/compaction/compaction.ts#L935) `[VERIFY: pi/packages/coding-agent/src/core/compaction/compaction.ts:935]` | 借鉴确定性累积；本产品额外关联真实执行结果，不能把调用参数当作成功变更 |
| pi 将历史序列化为角色标识文本并限制工具结果长度，防止摘要调用变成原对话续执行 | [序列化](../../pi/packages/coding-agent/src/core/compaction/utils.ts#L109) `[VERIFY: pi/packages/coding-agent/src/core/compaction/utils.ts:109]` | Agentic 内容块按语义处理，不凭 user role 猜用户输入；截断仍保留关键证据和引用 |
| pi 根据是否存在旧摘要选择增量提示，并拒绝摘要返回 toolCall | [摘要生成](../../pi/packages/coding-agent/src/core/compaction/compaction.ts#L643) `[VERIFY: pi/packages/coding-agent/src/core/compaction/compaction.ts:643]` | 摘要是受限模型任务，不具备业务执行权 |
| pi 追加 compaction 条目，以 firstKeptEntryId 构造活动上下文；原条目不在该操作中删除 | [记录](../../pi/packages/coding-agent/src/core/session-manager.ts#L1097) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:1097]`；[投影](../../pi/packages/coding-agent/src/core/session-manager.ts#L441) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:441]` | 产品历史与当前模型记忆分离 |
| pi 对一次溢出恢复设置上限，并处理保留消息携带的旧 usage 导致重复压缩 | [一次恢复](../../pi/packages/coding-agent/src/core/agent-session.ts#L2090) `[VERIFY: pi/packages/coding-agent/src/core/agent-session.ts:2090]`；[陈旧 usage](../../pi/packages/coding-agent/src/core/agent-session.ts#L2133) `[VERIFY: pi/packages/coding-agent/src/core/agent-session.ts:2133]` | 恢复要有终点；预算不能跨投影直接复用 |
| Eino summarization 可注入 TokenCounter、GenModelInput、Finalize、Callback，摘要错误向外传播 | [生成和回调](../../eino/adk/middlewares/summarization/summarization.go#L281) `[VERIFY: eino/adk/middlewares/summarization/summarization.go:281]`；[自动入口](../../eino/adk/middlewares/summarization/summarization.go#L335) `[VERIFY: eino/adk/middlewares/summarization/summarization.go:335]` | 存在适配点，但没有自动实现产品提交/失败回退契约 |
| Eino 默认 Finalizer 返回 system 消息与一个处理后的摘要，没有 pi 式近期原始消息保留区 | [默认整理](../../eino/adk/middlewares/summarization/summarization.go#L634) `[VERIFY: eino/adk/middlewares/summarization/summarization.go:634]` | 需要产品定义的保留规则，不能把默认值描述为完整 pi compaction |
| Eino PreserveSkills 根据技能调用和结果重建保留消息，并按数量/预算截断正文 | [技能保留](../../eino/adk/middlewares/summarization/finalizer_builder.go#L186) `[VERIFY: eino/adk/middlewares/summarization/finalizer_builder.go:186]`；[裁剪](../../eino/adk/middlewares/summarization/finalizer_builder.go#L309) `[VERIFY: eino/adk/middlewares/summarization/finalizer_builder.go:309]` | 可复用识别思路，但完整性及同版本保证由产品补足 |
| Eino reduction 会先处理旧工具内容并替换 state.Messages | [清理结果](../../eino/adk/middlewares/reduction/reduction.go#L1134) `[VERIFY: eino/adk/middlewares/reduction/reduction.go:1134]` | 与语义摘要统一排序；清理运行状态不等于允许删产品历史 |

这些是静态源码核查结论，尚未运行组合实验；尤其不能把 Finalize/Callback 可用直接等同于 SessionManager 通过 SessionStore 提交产品历史时，与 checkpoint 的一致性已经解决。

### 4.4 可检验验收

- **CMP-A01**：包含用户约束、失败测试、完成修改和未执行计划的长历史，摘要投影能分别表达这些事实；原消息哈希不变。
- **CMP-A02**：保留边界落在含两个工具结果的轮次内时，最终投影不出现孤立调用/孤立结果，当前请求仍完整可定位。
- **CMP-A03**：摘要超时、空内容、toolCall 和过大输出四类候选均不能提交；原投影和输入队列保留。
- **CMP-A04**：阈值失败但低于硬上限时使用旧投影继续；超过硬上限时不继续发送相同超限请求。
- **CMP-A05**：压缩持久写入失败时无完成事件、新投影不激活；写入后激活前崩溃，恢复仅使用一条已提交记录。
- **CMP-A06**：摘要候选生成期间取消任务或历史叶子变化，候选不能提交到错误分支；已合法提交记录不被反向删除。
- **CMP-A07**：等待人工交互时发起压缩，不改写待恢复 checkpoint；resume 后工具执行次数与无压缩场景一致。
- **CMP-A08**：同一边界连续两次返回上下文溢出，最多自动压缩并重试一次；此前完成的副作用工具不重复调用。
- **CMP-A09**：两个分支有公共祖先，分支摘要仅覆盖离开路径的独有信息，并标识其来源；目标分支历史保持原样。
- **CMP-A10**：连续三次压缩，关键未完成任务、用户约束及有效产物引用仍可恢复；旧摘要不无限叠加。
- **CMP-A11**：父任务和子 Agent 分别触发压缩，历史范围、事件 invocationId、预算和 generation 不串用。
- **CMP-A12**：恢复旧 checkpoint 时检测上下文版本不一致，不能把新摘要和旧工具状态直接拼装后运行。
- **CMP-A13**：仅由 M04 识别为溢出的服务端响应触发溢出恢复；相同批次中的限流、普通 length、未知 usage/空输出不误进入该路径。M08 预算触发与手动压缩按各自原因记录，计数不被重复归零。
- **CMP-A14**：压缩期间已受理但未消费的 follow-up 不进入摘要；之后只在 M03 边界消费一次，投影重建不重复追加。

- **CMP-A15**：没有旧摘要时生成六部分正文；第二次只输入上一份生效摘要及新覆盖范围，完成项迁移、阻塞解除、目标约束保留，旧原文和旧附录不重复堆叠。
- **CMP-A16**：firstKept=e5 的历史按“新摘要 + e5 起的原文 + 后续记录”重建；无新增范围不再次调模型，无法解析边界不猜测继续。
- **CMP-A17**：同一用户输入触发多个 Turn，切点落在后续助手消息；主历史、工作前缀、保留后缀范围无重叠，工具批次两侧均闭合。首次前缀包含原始输入；连续压缩不重新展开已覆盖的 U。只有前缀需摘要且已有旧摘要时，旧摘要不得被丢弃。
- **CMP-A18**：主摘要成功而前缀失败/取消/截断，候选不提交；两次模型消耗可归属，合并后的全部内容仍经过预算。
- **CMP-A19**：read(a)、edit(a)、read(b) 后压缩，再 write(c)、deny edit(d) 后二次压缩，readFiles=[b]、modifiedFiles=[a,c]；metadata 与程序附录一致，d 不伪造成功；平台路径身份不混淆。
- **CMP-A20**：工具失败但确认部分写入、效果未知及后续核对，文件事实按证据更新；命令/子工具没有结构化路径时不虚构完整清单，不重执行探测效果。
- **CMP-A21**：从分支 A 切到 B 或压缩子 Agent，旧清单只按所选来源传递；排除模型的记录不能经文件附录泄露；超大清单保留完整受控 metadata 并给出有界模型视图。
- **CMP-A22**：压缩进度与内部 agent_end 到达后仍有后续工作时，不提前发布 trace.settled；候选落盘成功后才标记 compaction.finished，重复交付不产生第二份记录。
- **CMP-A23**：共同前缀已有压缩 C1，分叉后 A 又生成 C2；选择 B 继承 C1 及合法文件 facts，不读取 A 独有 C2，也不为继承 C1 再调用摘要模型。

## 5. 风险与待定项

| 项目 | 风险与处理要求 |
| --- | --- |
| 语义损失 | 摘要可能遗漏限制或把计划写成成果；保留关键原文及可回查证据，并做事实表评测 |
| 摘要也超窗口 | 需要有界分段或获准模型；没有合适能力时明确失败，不无限重试 |
| checkpoint 与历史不同步 | 是全系统一致性问题；必须通过 CMP-A05、A07、A12 后才能宣称恢复完整 |
| 保留 skill 与预算冲突 | 不能把被截断指令当全文；使用同版本可重载资源，仍不适配则显式报告 |
| 待定参数 | 软阈值、近期保留量、摘要最大输出、重试超时、是否开启分段和独立摘要模型需全章评审 |
| 阶段范围 | 手动/自动压缩、分支摘要及子 Agent 压缩均纳入完整产品拼图；交付阶段及具体算法实现留待开发方案 |
