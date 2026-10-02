# 安全审核、审批审计与沙箱

**2026-09-27 最新范围确认，待实现：**用户亲自发起、由受信宿主调用的直接 shell 独立于模型工具。本入口不走人工审批、Agent 预算、执行票据、工作区访问限制或持久化执行去重；以操作系统账户权限执行，保留初始 cwd、输出、退出码、超时与主动取消，不自动重试/恢复。它不承诺保护可信运行数据，也不能由模型参数、普通扩展或导入记录伪造来源调用。本章安全约束仍适用于模型和受控工具，不能以此例外绕过审批或将不可用沙箱降级为裸执行。日志自动保存不新增审批/票据，但仍保留脱敏及产物绑定/内容校验。此决定替代“所有命令均同一安全管道”的旧表述，不表示代码已完成迁移。

状态：完整 PRD 讨论稿，2026-09-24 修订。安全职责采用双来源：DeepSeek Harness 继续作为策略、审批和审核分工的参考；Zero 作为 Go 原生三平台沙箱的主要实现来源。适配到已确认的 pi 命名和 Eino 执行架构。只定义需求，不移植 Cordis、不运行沙箱或修改系统权限。

参考版本：
- `deepseek-ai/deepseek-harness`，commit `ddefc45fbc7f8e46dd73185e68295696d1297887`；DSH 将该实现标为开发者预览，声明尚未经过安全审计。[来源安全说明](../../deepseek-harness/SAFETY.zh.md)
- 本地 `zero/`，commit `99721c762f37cd43ac511007a5f51d1846df959e`。其沙箱位于 Go `internal` 包，采用必要代码抽取/移植和许可保留，不是可供本项目直接导入的公共 SDK。

“参考其设计或源码”不等于本产品已经获得安全认证，也不等于三平台已通过本产品验收。

## 1. 执行摘要

安全相关能力分为四件事：

| 能力 | 回答的问题 | 不等于 |
| --- | --- | --- |
| 执行前策略/安全审核 | 当前完整操作是否符合已有授权和限制？ | 模型任意理由都能批准，或 OS 已被隔离 |
| 一次审批 | 这个具体操作是否获准扩大权限？ | 后续任意操作的长期通行证 |
| 审计记录 | 谁基于什么范围作出决定，实际执行结果是什么？ | 把全部原始秘密和模型推理写进日志 |
| 执行沙箱 | 后端实际阻止哪些文件效果？ | 审批、网络隔离、任意插件隔离或效果可回滚 |

工具管道见 M05：参数转换、最终校验与冻结后进入策略，许可/票据、预算和执行后端准备完备才执行。Code Agent 与独立 Workflow 分别注入策略、审批、记录和恢复入口；共用 L2/后端语义不共享授权、日志或预算，业务封装工具也不是授权转移通道。普通 Code Agent 子委派保留父 Trace 权限/预算/generation/取消范围，Workflow 节点用自己的运行与绑定。Code Agent 与共享存储已迁移，独立 Workflow Agent、工厂和 Web 三类资源路由已接通，并有冻结参数、一次审批、显式恢复和独立预算的默认测试；这不认证真实平台沙箱或完整 P3。安全策略不写进模型循环，Operations/后端强制落实；取消等到实际退出，unknown 仍先核对。

成功标准：没有许可不执行；限制后端不可用不裸跑；审计决定与调用一一对应；部分约束如实显示；批准、恢复和重试不能重复副作用；策略变化不破坏模型上下文来源和缓存前缀。

## 2. 用户体验与功能

### 2.1 沙箱模式与审批策略是两个维度

采用 DSH 的文件效果模式及审批语义：

| 维度 | 值 | 含义 |
| --- | --- | --- |
| sandboxMode | read-only | 限制文件修改；允许执行后端声明的必要 sink 等例外，不承诺所有文件不可读取 |
| sandboxMode | workspace-write | 允许工作区与后端声明的临时区域修改；其他文件效果受约束 |
| sandboxMode | danger-full-access | 明确不使用此进程沙箱；不能展示成“另一种强沙箱” |
| approvalPolicy | ask | 操作需要额外许可时，向已配置审批通道询问；没有应答者不放行 |
| approvalPolicy | never | 所有需要询问的操作直接 rejected；不是“全部允许” |

已确认默认采用 DSH 基础组合的 `workspace-write + ask`，Auto 默认关闭；`read-only + ask` 和 `danger-full-access + never` 是其他明确模式。基础配置可由开发者提供默认值，用户无需填写低层 runner/阈值参数。选择 Full access 是明确改变运行模式，不是缺少沙箱时的回退。

源码中独立 sandbox-policy 服务的默认值是 read-only，而 shipped base profile 将它装配为 workspace-write；本产品参照的是基础应用组合，不能混淆库默认和产品默认。[策略配置](../../deepseek-harness/packages/sandbox/sandbox-policy/src/index.ts#L110) `[VERIFY: deepseek-harness/packages/sandbox/sandbox-policy/src/index.ts:110]`；[基础组合](../../deepseek-harness/packages/bundle/base/cordis.patch.yml#L211) `[VERIFY: deepseek-harness/packages/bundle/base/cordis.patch.yml:211]`。

### 2.2 一次审批

作为用户，我希望只在当前策略不足以执行一个具体动作时决策，能知道动作、目标、范围及原因。

- **SEC-01**：执行前决定区分 allow、deny、cancel、ask。ask 的结果采用 `allowed-once / rejected / cancelled / unavailable`；只有 allowed-once 可以继续。
- **SEC-02**：审批关联 approvalId、所属产品的调用身份（Code 为 toolCallId/traceId/invocationId，Workflow 为运行/节点调用身份）和 4.1.1 冻结执行描述的引用/摘要；原始参数、转换结果和实际执行值可追溯。前端展示同一份描述的授权视图，不另维护一份可漂移的参数。
- **SEC-03**：应答者缺失、异常、返回非法值，或审核/必要审计不可用时，未决操作不能执行。ask 等待期间取消则撤回请求，迟到批准不生效。
- **SEC-04**：同一批准最多应用一次，绑定原冻结调用；参数、资源目标、环境或许可范围变化后重新判断。一次问答、应答者、有效期、响应去重与消费状态只属于当前运行实例内存，关闭、重开或重启后失效；旧记录、checkpoint 或模型声称“之前已批准”均不能恢复批准。显式 Resume 重新复核当前策略，原调用尚未占用且仍需审批时重新询问，已有占用或结果按 2.2.1 核对/复用。

DSH 的工具前置决定是 allow/deny/cancel/ask；审批服务再把 ask 归一为闭合结果，二者不是同一个枚举。[PreToolDecision](../../deepseek-harness/packages/core/tools/src/index.ts#L589) `[VERIFY: deepseek-harness/packages/core/tools/src/index.ts:589]`。

DSH 在 request 中先记录 `approval/asked`，取得结果后记录 `approval/decided`，最后返回；两次必要保存任一失败则不返回可用许可。它要求请求位于已开启的 turn 内以满足自身日志提交规则；这是上游事实，本产品不采用其持久审批决定或 session 文件格式。本产品按 [M10 4.3.2/4.4](10-session-persistence.md#432-审批许可与授权材料的恢复) 在当前实例内存管理问答、响应去重、有效期及消费，持久保存原冻结调用、执行意图/占用、预算、真实启动/结果和恢复定位；审批决定、应答者及内存消费状态不写入 JSONL、持久事件、checkpoint 或持久操作回执。Auto 最小决定元数据仍按 3.1/3.2 独立保留。[request](../../deepseek-harness/packages/interaction/user-approval/src/index.ts#L208) `[VERIFY: deepseek-harness/packages/interaction/user-approval/src/index.ts:208]`；[结果词汇](../../deepseek-harness/packages/interaction/user-approval/src/types.ts#L32) `[VERIFY: deepseek-harness/packages/interaction/user-approval/src/types.ts:32]`。

#### 2.2.1 一次许可的占用、启动与恢复

**SEC-15**：当前实例内存批准表示允许原冻结调用，持久执行意图/占用记录表示该调用已取得一次执行位置，实际启动和结果另记；持久占用不保存决定、应答者或内存消费状态，也不能成为重开后的批准。沿用 M10 调用记录与预算提交，不新增任务状态机、审计方案或分布式事务承诺。本节许可占用针对需要 allowed-once 的调用；常驻策略或 Auto 直接 allow 不制造人工审批或 approvalId，仍遵守调用去重、必要审计及执行意图/结果的提交规则。

执行顺序为：当前实例内存受理/去重 asked/decided → 复核原冻结描述、取消、有效期与当前策略 → 串行校验/消费内存批准，一致提交原调用执行意图/占用及预算 → 调用执行器 → 记录真实启动事实和结果。只有意图/占用及预算提交成功的执行者可进入执行器；重复回复、恢复或新 executionId 不重用同一执行位置，提交失败不执行。占用不是 tool.started，不能提前宣称进程已经运行。执行器/runner 已启动也不一定证明目标操作已开始；工具事件按 M07 表达已知事实，核对另看受信的目标操作启动证据。

| 恢复时已有事实 | 可采取的动作 |
| --- | --- |
| 当前实例未批准、已拒绝/取消/过期/撤销，或回复无效 | 不执行；缺少有效内存响应不能推断获批 |
| 当前实例有效批准且尚未占用 | 描述及当前策略仍有效、原调用允许继续时，串行消费并提交执行意图/占用与预算后才执行；不授权别的调用 |
| 已关闭、重开或重启，原调用尚无执行意图/占用 | 原批准与响应去重状态失效；显式 Resume 重新复核策略，仍需审批时为原冻结调用重新询问，不从旧 asked/decided 或 checkpoint 取许可 |
| 已占用，受信执行器明确确认尚未开始实际操作 | 先提交未执行证据及解除占用，再复核当前策略；需要审批时重新询问，不恢复已消费批准 |
| 已占用，但是否启动/是否产生效果无法确认 | 保留占用，标记待核对，不因批准丢失、缺少 started/结果或新 executionId 再次执行 |
| 已启动，结果未提交 | 按 M05 outcome_unknown 核对；没有可靠证据不重做 |
| 结果已提交或已核对确认执行过 | 直接复用已有结果，不为已完成动作重新审批或执行；业务失败也不恢复原执行位置或批准 |

“受信执行器确认未执行”不能只靠工具输出中出现某句 runner 错误、某个退出码或查询不到 PID；这些可作诊断，不能单独解除占用。进程启动与本地提交之间无法共同原子完成，遇到不确定状态选择暂停/核对。checkpoint 保存执行栈及原调用恢复定位，不携带审批决定；同一调用跨内部 executionId 恢复时仍关联原执行意图/占用，显式 Resume 重新复核策略，旧格式审批记录只供历史兼容读取而不授予许可。

### 2.3 单次扩大沙箱许可

参考 DSH 的 `sandbox_permissions + justification`：二者同时存在、理由非空，目标模式必须相对本次解析的当前模式严格扩大；重复当前模式不应触发扩大权限审批。许可只属于这次调用，不能改变会话的常驻模式。

正常流程：操作按常驻策略尝试 → 返回已识别的约束拒绝 → 模型或用户提出同一操作的最小必要扩大范围 → 一次批准 → 新调用使用显式许可 → 记录真实结果。失败后不能先裸跑再补问。

**SEC-05**：原操作可能已经完成部分修改时，不因检测到拒绝就认定“完全没执行”；是否重试先按 M05 核对副作用及幂等性。这里是本产品对已有 unknown 契约的衔接，不声称 DSH 升级 helper 自动证明原操作无效果。

DSH helper 验证参数搭配、扩大关系与一次批准，但本身不查历史证明“确为刚失败的同一命令”。因此本产品若在界面承诺“原操作的重试”，必须校验原调用引用、参数与目标，不只依赖提示词说明。[参数](../../deepseek-harness/packages/sandbox/sandbox/src/escalation.ts#L51) `[VERIFY: deepseek-harness/packages/sandbox/sandbox/src/escalation.ts:51]`；[扩大许可](../../deepseek-harness/packages/sandbox/sandbox/src/escalation.ts#L153) `[VERIFY: deepseek-harness/packages/sandbox/sandbox/src/escalation.ts:153]`。

### 2.4 自动安全审核（独立可选能力）

DSH 的 Auto review 是实验插件，默认不启用，审核的是即将执行的一个完整工具操作。它使用当前 Agent 的模型，把固定审核规则、cwd 环境、带来源的项目限制、筛选后的授权历史及完整待执行动作组成独立请求。

| 风险 | DSH 参考判断 | 产品吸收要求 |
| --- | --- | --- |
| low | 常规项目内读写、测试/构建，以及确有事实证明由本 Session 创建对象的精确清理 | 不额外机械询问；依旧服从实际模式和硬限制 |
| medium | 删除既有对象、生产操作、对外写入/发送、权限或系统变更等 | 需当前人类或直接父任务对动作、目标、范围的明确授权；不能把历史工具记录当授权 |
| high | 秘密/私有信息向不可信边界外泄等硬拒绝效果 | 拒绝；不能由模型解释或普通批准降低风险等级 |

**SEC-06**：审核输入区分 human-instruction、direct-parent-instruction、constraint、checkpoint 和 fact。项目文件只能收窄，摘要不能变成原始授权，助手自述/推理/工具返回不能作为人类许可。独立审核结果采用闭合结构，缺字段、非法组合、超时、流失败或解释冲突均不放行。

**SEC-07**：各自调用在完整参数冻结后、execute 前审核。Code Agent 普通子委派内每次工具仍检查父范围与实际策略；独立 Workflow 每个节点调用使用自己的策略/审批/预算，不能凭业务工具外层 allow/批准或父 Trace 取得内层许可。外部子进程任意系统调用仍靠执行后端约束，不等于注册工具都可观察。

需要完整保留参考的差异：DSH Auto 模式实际组合为 `danger-full-access + never`，allow 后没有后续人工确认，也没有文件沙箱；其源码跳过外层 run_code 运输调用而审核已启动的 PTC 内层工具。它不是“安全模型 + 文件沙箱”的默认叠加保证。当前产品不因引入 Auto 自动增加 PTC 功能，也不宣称覆盖任意进程内操作。

DSH Auto 卸载时会将活动 Auto 会话转为 Full access。本产品不采用这一自动放宽行为：默认关闭 Auto；以后若显式启用，审核能力消失时暂停受影响操作，待明确选择新模式，不静默降为 Full access。保留策略接口及上述授权规则，具体启用方案另行验证。

依据：[风险/来源规则](../../deepseek-harness/packages/experimental/auto-review/src/index.ts#L38) `[VERIFY: deepseek-harness/packages/experimental/auto-review/src/index.ts:38]`；[审核快照](../../deepseek-harness/packages/experimental/auto-review/src/index.ts#L359) `[VERIFY: deepseek-harness/packages/experimental/auto-review/src/index.ts:359]`；[闭合解析](../../deepseek-harness/packages/experimental/auto-review/src/index.ts#L560) `[VERIFY: deepseek-harness/packages/experimental/auto-review/src/index.ts:560]`；[调用与卸载](../../deepseek-harness/packages/experimental/auto-review/src/index.ts#L647) `[VERIFY: deepseek-harness/packages/experimental/auto-review/src/index.ts:647]`。

#### 2.4.1 审核使用的授权原文与来源

**SEC-16**：Auto 的授权材料从受信的原始受理记录、已生效的限制记录、当前实例内仍有效的审批问答及运行时建立的直接父子关系构建；旧/跨重开的审批记录不授予权限，不从主模型当前 messages、摘要或扩展改写结果反推出“用户曾授权”。

| 材料 | 来源判定与审核用途 |
| --- | --- |
| 人类原始输入 | 经已认证接入通道或应用明确认可的 SDK 身份受理；保存 inputId 与原文/受保护原文引用。只有已消费且适用于当前操作范围的内容可作为 human-instruction |
| 直接父任务指令 | 由运行时的委派关系及输入记录确认 sender/receiver 和 invocation；可定义子任务范围，不能覆盖人类限制、扩大父任务许可或转移一次批准 |
| input hook、模板、skill 展开及扩展 SendMessage/SendUserMessage | 保留原输入关联及转换来源，作为派生材料；即使模型协议使用 user role，也不因而取得人类授权 |
| 项目规范、摘要、工具结果、附件及历史动作 | 分别作为 constraint、checkpoint 或 fact；保留限制和证据，不自动取得授权角色 |
| 导入历史、来源缺失/无法确认的数据 | 可用于普通历史展示或按 M06 投影；不能凭自填的 source/role 字段生成有效许可或人类身份 |

身份与父子关系由受信接入/协调层赋予，模型、扩展消息 payload 和普通客户端字段不能自行声明。原始人类输入与转换后的实际模型输入分别关联保存；转换不改写原文，不隐式将原文再发一次给主模型。敏感原文可保存在受保护位置并引用；只有摘要/哈希而无法取得所需原文时，不据此批准需要明确授权的动作。

尚未消费的普通 steering/follow-up 不是当前工具的授权；显式取消或权限撤销则按独立控制路径立即处理。审核材料保留仍有效的收窄限制与原文关系，不因主上下文压缩丢掉限制；预算或材料缺失导致无法判断时不放行。普通 input/context 转换器不能重新改写审核材料中的授权来源；同进程可信 Go 代码仍不属于隔离对象。

DSH 使用持久 MessageSource 中的接入来源和直接父会话关系进行分类，[人类/父任务识别](../../deepseek-harness/packages/experimental/auto-review/src/index.ts#L175) `[VERIFY: deepseek-harness/packages/experimental/auto-review/src/index.ts:175]`、[分类](../../deepseek-harness/packages/experimental/auto-review/src/index.ts#L222) `[VERIFY: deepseek-harness/packages/experimental/auto-review/src/index.ts:222]`。本产品不照搬其 Web rpcId 判据，而以已定义的 SDK/HTTP 身份和委派记录映射同一职责。

## 3. AI 系统需求与审计

### 3.1 审核输出与模型反馈

人工审批、自动审核、工具实际结果是不同记录。模型侧只需要足够纠正行为的结构化失败反馈，客户端可查看来源与批准/拒绝说明；不要把 reviewer 的推理全文或未经处理的敏感输入注入主模型。

DSH Auto 对主模型返回固定拒绝文本，可选理由作为结构化错误供 UI 使用，risk/reviewer prompt/推理/原始回答不持久化；本产品不因此宣称 DSH 已提供完整自动审核账本。为满足可追溯要求，建议另保存最小决策元数据：调用引用、审查规则/模型版本、allow/deny/unavailable、原因类别、取消与耗时/用量可用性；不保存推理过程。[拒绝构造](../../deepseek-harness/packages/experimental/auto-review/src/index.ts#L634) `[VERIFY: deepseek-harness/packages/experimental/auto-review/src/index.ts:634]`。

审核模型调用纳入所属运行预算及取消：Code Agent 普通子调用仍受父 Trace，Workflow 节点审核计入自己运行，两类不共用总额；跨运行预算由业务管理。审核不带业务工具、不执行待审动作，材料不足、超时或取消不放行。DSH 当前没有专门的审核重试/压缩和小输出预算层，本产品继续按 M04/M08 有界失败，不无限重试或截掉关键授权后放行。工程默认值沿用开发方案，不另造数值。

### 3.2 最小审计链

| 记录 | 最少内容 | 保存/模型可见性 |
| --- | --- | --- |
| 策略解析 | 原调用身份、冻结描述引用、workspace、常驻模式、单次执行范围、策略版本及等待/恢复定位 | 可追溯；不含凭据值，定位或单次范围不等于可恢复的批准 |
| 本实例内存 approval asked/decided | 同一 approvalId、原冻结调用引用、理由、应答者、闭合结果、有效期、响应去重和消费状态 | 仅当前运行实例内存；不写入 JSONL、持久事件、checkpoint 或持久操作回执，关闭/重开失效，默认不进入模型 |
| 原调用执行意图/占用与预算 | 原调用身份、执行关联、预算使用及受信未执行/结果事实；审批引用只定位原调用，不携带决定 | 必须在实际启动前一致提交；拒绝并发重用及崩溃后盲重跑，不用 started 的缺失推断可重用，也不把持久占用当作批准 |
| 自动审核决定（若启用） | 完整动作引用及策略/模型版本、决定类别 | 本产品的最小增强；不把 reviewer 原文存为授权 |
| 实际沙箱事实 | 后端、有效 mode、full/partial、错误类别、启动是否已确认 | 跟随执行记录保存；partial 必须可见 |
| 工具结局 | 原始成功/失败/拒绝/取消/未知及产物状态 | 与 M05/M07/M10 同一调用链，后处理不能伪造 |
| 模式变更 | 发起来源、前后值、生效边界 | Code 由 SessionManager 持久化，Workflow 由自身记录入口持久化；各自恢复时不能因缺省值无声扩大许可 |

**SEC-08**：Code Agent AgentSession/SessionManager 与 Workflow 自身协调/提交入口分别完成本运行必要审计后再执行/发布；另一类决定或工具包装结果不能替代本调用记录。观察者失败不改决定，审批/必要保存失败不执行；未决中断保留可查原因，恢复不伪造允许。

**SEC-09**：策略变化向模型提供带来源的完整当前状态快照，追加在保留历史之后，不每次重写稳定 system 前缀。该快照用于解释已生效策略，权限判断仍使用受信策略值；与 M08 缓存及不可变资源版本共同适配。DSH approval/sandbox-policy 的上下文贡献采用该思路。[审批上下文](../../deepseek-harness/packages/interaction/user-approval/src/index.ts#L143) `[VERIFY: deepseek-harness/packages/interaction/user-approval/src/index.ts:143]`；[策略解析](../../deepseek-harness/packages/sandbox/sandbox-policy/src/index.ts#L164) `[VERIFY: deepseek-harness/packages/sandbox/sandbox-policy/src/index.ts:164]`。

## 4. 技术规格

### 4.1 策略、执行器与后端分工

| 角色 | 本产品职责 | 依赖约束 |
| --- | --- | --- |
| 策略解析/审批能力（各自应用注入） | 解析本运行常驻模式/单次授权与应答者，产生结构化决定 | Code/Workflow 分别协调，向 L2 工具提供窄接口；外层工具批准不转移 |
| Code Agent AgentSession / SessionManager | 自身任务等待/恢复、审计提交与历史关联 | 不管理 Workflow，底层不反向导入 |
| Workflow 自身运行/节点协调者（已接线） | 自身策略、节点审批/恢复、预算与调用事实提交 | 不用 Code Agent 历史、批准或 checkpoint；共用 storage 不共用业务状态 |
| 工具 / Operations 执行器 | 将规范化动作和明确策略送入正确环境，负责实际启动、取消、结果分类 | 不直接通过 HTTP 或 UI 决策 |
| SandboxProvider | 在所在环境把 argv 和文件效果策略变成受约束执行方式，返回能力事实 | 不认识模型意图、会话树或批准按钮 |
| 直接文件后端 | 对受信读写方法落实同一文件效果策略，返回结构化拒绝 | 与进程后端共享规则；不是任意 Go 代码沙箱 |

原生进程后端参考 Zero 的 `PermissionProfile → SandboxManager → CommandPlan → execution.Preparer` 装配思路，把平台中立请求变成可强制的 OS 命令。[执行适配](../../zero/internal/execution/runner.go#L15) `[VERIFY: zero/internal/execution/runner.go:15]`；[命令计划](../../zero/internal/sandbox/runner.go#L103) `[VERIFY: zero/internal/sandbox/runner.go:103]`。本产品仍使用冻结描述和可信票据契约：`CommandContext` 返回尚未启动的 `exec.Cmd`，不等于已经完成审批、原子占用并可信确认目标启动。后续工作流代码/命令节点必须经过该受控进程后端；仅有 Zero 沙箱不代表任意导入代码已可安全执行。

DSH 的按调用策略是 `{mode, workspaceRoot, sessionId?}`，优先级为有效单次覆盖 → 会话策略 → 部署默认；解析后的值随调用传递，不修改共享 provider 全局模式。因此两个会话不同 root、不同许可可以共用后端而不串线。[策略](../../deepseek-harness/packages/sandbox/sandbox-policy/src/index.ts#L164) `[VERIFY: deepseek-harness/packages/sandbox/sandbox-policy/src/index.ts:164]`。

本产品的已解析执行环境还需明确会话产物根、临时映射和运行数据保护布局；这些由受信装配方提供，不让模型通过工具参数自行扩大或取消。具体字段/接口在开发方案定义，SandboxProvider 与直接文件后端只接收解析结果，不反向读取 SessionManager。

DSH 扩大许可 helper 通过窄 approver 接口调用审批，并不依赖 Agent 包；这种依赖方向适合本产品已确认分层。[接口](../../deepseek-harness/packages/sandbox/sandbox/src/escalation.ts#L102) `[VERIFY: deepseek-harness/packages/sandbox/sandbox/src/escalation.ts:102]`。

#### 4.1.1 参数转换完成后才冻结和授权

**SEC-12**：统一采用“参数转换 → 最终规范化/校验 → 冻结执行描述 → 只读决策/强制安全策略 → 必要审批与审计/许可占用 → 实际执行”的顺序。转换放在 M05 prepareArguments 阶段；M07 tool_call 读取冻结描述，只能提出 allow/deny/cancel/ask，不能再返回或原地修改参数。

冻结描述至少关联工具实现/schema/generation、调用身份、最终参数、规范化资源目标和已声明的业务前置条件；进程调用还包含实际 argv、cwd、执行环境/后端、工作区与临时目录映射、申请的许可范围，以及适用的 env/stdin 内容引用。秘密值不进入普通日志或审批展示。执行器消费同一冻结副本，不能重新取共享可变参数或全局最新默认值。

所有业务转换完成后必须执行最终校验；提前做过的校验不能代替它。普通扩展返回 allow 仅表示该处理器不阻止，不能跳过强制策略、沙箱准备或必要审计；deny/cancel 保留阻止效力。冻结后发现任何影响行为的参数、目标、实现或环境变化，停止该候选并重新校验授权，旧许可不得挪用。等待后或启动前重新核对当前策略、取消和声明的资源前提；这不承诺冻结整个业务工作区或消除全部文件系统竞争。

pi 的 prepareToolCall 先规范化/校验，再执行 beforeToolCall，[顺序](../../pi/packages/agent/src/agent-loop.ts#L600) `[VERIFY: pi/packages/agent/src/agent-loop.ts:600]`；其 ToolCallEventResult 注释允许原地改 input，[扩展类型](../../pi/packages/coding-agent/src/core/extensions/types.ts#L1087) `[VERIFY: pi/packages/coding-agent/src/core/extensions/types.ts:1087]`。本产品借鉴阶段职责，并明确增加最后一次校验、冻结及只读授权要求，不将 pi 的可变输入直接作为授权保证。

#### 4.1.2 可信运行数据的写保护

**SEC-13**：两类各自日志、策略、原冻结调用/执行意图/占用、预算、启动/结果记录、checkpoint 及可信关联元数据属于运行数据；当前实例内存审批问答不新增持久决定文件。工作区文件和模型工具产物属于业务数据。受限模式下运行数据目录不能由注册文件工具或模型命令直接修改；Code 正常保存由受信 SessionManager/checkpoint Store 完成，Workflow 由自身受信记录/节点 checkpoint 保存者完成。共用 storage 契约/后端不共用写入者或授权状态。

应用装配时确定运行数据根、工作区根和临时可写根，按实际执行环境解析真实路径并检查重叠/别名。默认将运行数据放在业务可写范围之外；确有包含关系时，只有后端能实际落实该子目录的写禁止才可启用受限执行。直接文件后端和进程后端都需验证，不能只在工具 description 中隐藏路径或只做字符串前缀比较；符号链接/junction、硬链接和挂载映射的边界进入平台验收。

运行数据保护作为该部署必须满足的一项能力单独核查；仅返回 generic full/partial 标签不足以证明它。配置会暴露运行数据、或后端无法满足这项保护时拒绝该受限执行配置，不悄悄改用 Full access。允许 partial 的平台选择不等于允许模型写调用执行意图/占用或预算账目；无法满足时需选择满足要求的执行环境，具体方案在开发阶段验证。

Full access 明确关闭该进程文件沙箱，独立目录只能减少误写，不能据此保证同用户进程或可信 Go 扩展无法修改运行数据；Auto 使用 Full access 时同样如此。不得将这些场景标成运行数据已隔离。发现记录被外部改变、引用不一致或来源无法确认时，保留诊断并停止用它授予许可/自动恢复；不虚构可以检测任意篡改的能力，也不在本轮引入签名日志或独立安全服务。

#### 4.1.3 临时目录与跨工具产物

**SEC-14**：区分两类临时空间：供同一 Session 的文件工具、shell 与后续 Turn 共同访问的“会话产物目录”，以及后端仅供单次进程使用的私有临时区。共享的是该 Session 内同一真实目录，不是全局共享 /tmp。

会话产物目录按 Session 与执行环境分配独立根，文件后端、进程 cwd/环境变量及挂载映射使用同一解析结果；路径命名相同不足以证明是同一文件。只授予当前调用实际需要且符合 mode 的临时写入范围，不照搬宿主整个 /tmp 或用户 TEMP 为所有 Session 的可写根。read-only 不因配置临时目录就自动获得业务写权限。产物引用按 Session 校验，防止错误绑定或跨会话取用；这不代表文件效果沙箱已提供全部文件的读取隔离，额外的读取限制仍须独立声明。

后端私有临时区可随进程消失，不能把其中路径直接返回为可由其他工具持续读取的产物；需要保留时，由受控产物接口在结束前导出到会话产物目录或 M10 artifact 存储，保存成功才返回引用。工具产物与可信运行记录不混用目录或类型；续读仍检查 Session/环境归属。目录清理需确认无活动调用或有效保留引用，恢复时目录/产物已丢失就明确报告不可用，不读同名替代文件。

DSH 的 bwrap 对 /tmp 使用私有 tmpfs，而 Landlock 授予宿主 /tmp 写入，[profile](../../deepseek-harness/packages/sandbox/sandbox-local/src/profiles.ts#L16) `[VERIFY: deepseek-harness/packages/sandbox/sandbox-local/src/profiles.ts:16]`；文件后端/Seatbelt 的 writableRoots 还包含平台 temp，[根集合](../../deepseek-harness/packages/sandbox/sandbox/src/roots.ts#L43) `[VERIFY: deepseek-harness/packages/sandbox/sandbox/src/roots.ts:43]`。Zero 的 `SandboxRuntime` 按工作区分配 cache/data/temp，并加入宿主共享临时根，不是按 Session/调用隔离的产物目录，也不能当作 `stateRoot`。[运行时目录](../../zero/internal/sandbox/runtime_state.go#L29) `[VERIFY: zero/internal/sandbox/runtime_state.go:29]`。本产品需按上述产物语义调整后端授权和映射，不能声称原样复制 DSH 或 Zero 的临时目录已保证跨工具可见性与跨 Session 隔离。

### 4.2 平台矩阵

| 环境 | 上游实现事实 | 产品目标与必须保留的限制 |
| --- | --- | --- |
| Linux | Zero 默认查找 `zero-linux-sandbox` 与 `bwrap`，只读绑定宿主 `/`、指定写根可写 bind、私有 PID/proc；Landlock 是显式 helper 路径，不是已完成的自动回退链。DSH 另有 bwrap→Landlock 探测顺序，作职责参考 | 后端不可用或无法保护运行数据则拒绝受限配置，不裸跑。旧 Landlock ABI 可为 partial；不把文件约束说成网络约束。Zero 默认 `NetworkDeny` 时另建 network namespace，这是上游能力，不自动改写本产品网络默认 |
| macOS | Zero 使用 sandbox-exec/Seatbelt，`deny default` 后允许读根/写根，并可 `(deny network*)`。DSH profile 主要限制 file-write* | sandbox-exec 不可用时不能裸跑。Zero 的网络 deny 是上游事实；本产品默认原生保持环境既有网络，实际生效值单独报告。读取和同用户进程交互不由写限制保证 |
| Windows 原生 | Zero 已有 command-runner/setup helper、`CreateRestrictedToken`、capability ACL；管理员 setup 后可装 WFP 出站阻止。未 setup 时文件限制可走 unelevated，纯网络需求则 degraded。目标进程直接 `CreateProcessAsUser`，当前没有 Job Object 启动时序 | 初始化状态与实际文件/网络能力分别报告，不把未 setup 与 unelevated 当成互斥等级。DSH 的 Everyone/hard-link 缺口不能直接当作 Zero 当前事实，但仍需验收硬链接、junction 和运行数据保护。Job Object、挂起启动和可信控制通道是本产品待补足项，不能标成 Zero 已实现 |
| 容器 shell | 已确认主程序在宿主机运行，shell 可选 Docker 沙箱；工作区和产物经受信挂载/路径映射共用真实文件 | 文件工具与 shell 的资源身份必须一致；未映射容器路径不能替换为宿主同名路径，私有临时产物先导出。DSH/Zero 都不是完整容器编排器，挂载/网络/凭据另验 |

源码：Zero [平台选择](../../zero/internal/sandbox/manager.go#L179) `[VERIFY: zero/internal/sandbox/manager.go:179]`；[命令计划](../../zero/internal/sandbox/runner.go#L228) `[VERIFY: zero/internal/sandbox/runner.go:228]`；[Windows 启动](../../zero/internal/sandbox/windows_process_windows.go#L59) `[VERIFY: zero/internal/sandbox/windows_process_windows.go:59]`。DSH [runner 链](../../deepseek-harness/packages/sandbox/sandbox-local/src/index.ts#L159) `[VERIFY: deepseek-harness/packages/sandbox/sandbox-local/src/index.ts:159]`；[bwrap/Landlock/Seatbelt profiles](../../deepseek-harness/packages/sandbox/sandbox-local/src/profiles.ts#L16) `[VERIFY: deepseek-harness/packages/sandbox/sandbox-local/src/profiles.ts:16]`。

**SEC-10**：运行模式和实际 enforcement 分开报告。full 仅表示后端落实其承诺的文件效果范围，partial 表示只覆盖部分；none/unavailable 不得冒充 full。已确认默认允许满足运行数据保护等必需条件的 partial，并明确展示其限制；条件未满足则拒绝该受限配置，不自动改用 Full access。应用可通过代码要求更高最低级别，具体平台能力仍需实测。

### 4.3 启动失败、沙箱拒绝与普通工具失败

DSH 多候选会功能探测并缓存选择，单候选不一定预探测；真正执行仍可能失败。Zero 默认 `auto` 偏好在后端不可用且无显式 deny 时生成 `degraded` 直接执行计划，这是产品明确不采纳的行为。[Zero 降级](../../zero/internal/sandbox/manager.go#L229) `[VERIFY: zero/internal/sandbox/manager.go:229]`；[DSH 选择](../../deepseek-harness/packages/sandbox/sandbox-local/src/index.ts#L495) `[VERIFY: deepseek-harness/packages/sandbox/sandbox-local/src/index.ts:495]`。故“找到可执行文件”、Windows setup marker 或“选中 runner”不能作为已经约束的证据。

- **sandbox unavailable / runner failed**：后端缺失、启动或 profile 失败；有足够证据时说明命令未开始。限制模式必须拒绝，不执行原始裸 argv。
- **sandbox denied**：沙箱生效但操作受到阻止；shell 的判断可能来自 stderr 方言，因此保留证据，不保证从一段日志能精确证明所有效果。
- **ordinary failure**：例如程序退出码非零，不能仅因报 permission denied 就跨平台认定是沙箱。
- **unknown**：无法证明启动/完成范围或副作用，进入 M05 的核对流程，不能据此自动提权重试。

**SEC-11**：先检查 runner 故障证据再判断拒绝，保留原始错误类别；日志截断和应用伪造相似错误可能造成误判，应允许人工诊断。仅有 stderr/退出码分类不足以证明未执行，解除许可占用按 2.2.1 的受信启动事实判断；业务命令开始后不能换一个 runner 从头重试。使用 argv 列表，不拼接跨 shell 执行字符串；后端返回的 argv 必须由配套进程能力在同一环境启动。[runner 诊断](../../deepseek-harness/packages/sandbox/sandbox/src/diagnostics.ts#L65) `[VERIFY: deepseek-harness/packages/sandbox/sandbox/src/diagnostics.ts:65]`；[接口](../../deepseek-harness/packages/sandbox/sandbox/src/index.ts#L95) `[VERIFY: deepseek-harness/packages/sandbox/sandbox/src/index.ts:95]`。

直接文件后端对 write/edit 执行前重新解析真实路径并检查 writable roots，reads 保持本地访问语义；它是受信代码中的路径检查，不是内核隔离，仍有缩小但未消除的 TOCTOU。[文件检查](../../deepseek-harness/packages/fs/fs-sandbox/src/index.ts#L122) `[VERIFY: deepseek-harness/packages/fs/fs-sandbox/src/index.ts:122]`。本产品继承其诚实范围，不把所有进程内插件都标为已沙箱化。

### 4.4 和既定系统的连接

审核/批准与工具状态不同：被拒不产生实际 started；若审批通道无法提供许可，工具保持未执行；整个 Trace 可按普通拒绝反馈继续解释。沙箱基础设施失效、必要审计失败或副作用未知则按 M03/M05 停止或暂停，不能用模型自行纠错绕过。

DSH 人工审批等待进程内 Promise；本产品已有 Eino Interrupt/Resume 的节点/工具等待要求，Code 绑定自身 interactionId/Trace/checkpoint，Workflow 绑定自身运行/节点/恢复定位/checkpoint，不由 AgentSession 恢复它。参考决定词汇、一次授权与执行事实提交顺序，不采用 DSH 的持久决定，也不声称 Promise 原样支持跨进程恢复。显式 Resume 重新核对原冻结调用、执行意图/占用、取消、预算、原定义/绑定/版本与当前策略；关闭/重开后一次批准失效，未占用且需审批时重新问，已占用未知先核对，已有结果直接复用，另一类批准或 checkpoint 不能替代。

M07 的受控核对可以收集人工材料或受信查询证据；提交材料不等于确认效果，更不等于解除许可。只有本章 2.2.1 允许的受信未执行证据才能释放占用；原执行尚可能继续生效、证据矛盾或范围不明时保留未知和冲突限制。核对不使 cancelled/failed 复活，也不自动触发 resume。

generation 固定工具与资源版本，不冻结“永不过期”的许可。安全策略收紧、授权撤销优先于旧任务快照；变更时明确记录，不能为维持缓存命中继续使用旧权限。

## 5. 验收与未决项

| 编号 | 场景 | 必须通过的断言 |
| --- | --- | --- |
| SEC-A01 | ask 无应答者、应答者抛错/返回非法值、never | 均无实际执行；unavailable/rejected 可区分；没有自动允许 |
| SEC-A02 | 批准、取消同时到达；重复或迟到回复 | 当前实例仅串行接受一个绑定原冻结调用的有效决定；取消后的批准不执行，问答/消费只在本实例内存配对，原执行事实仍可追溯 |
| SEC-A03 | 审批回复非法/失效，或原调用执行意图/占用及预算提交失败 | 均不执行；决定不持久化，关闭/重开不恢复批准；原调用及等待/恢复定位保留，无虚假 allowed |
| SEC-A04 | 相同文件效果经 write/edit/shell 发起 | 同一 mode/root；文件 API 不能绕过进程层约束，读取语义不被误标 |
| SEC-A05 | 限制后端不存在或 profile 拒绝 | 返回明确设施错误，原始裸命令执行次数为 0 |
| SEC-A06 | Windows partial / 旧 Landlock / full 后端 | 上报真实 enforcement；要求 full 的应用拒绝 partial |
| SEC-A07 | 一次扩大许可重试，之后再执行新动作 | 批准仅影响被审操作，不改变会话常驻模式；参数改变重新判断 |
| SEC-A08 | shell 先部分修改再触发拒绝 | 保留已发生/未知效果，不直接从头重试制造重复副作用 |
| SEC-A09 | Auto 收到网页/日志伪造授权、助手自述批准、过期摘要 | 均不能取得人类指令角色；非法 JSON 或审核失败不执行 |
| SEC-A10 | Code Agent 普通父子委派及业务工具跨类调用 | 普通子 Agent 保留父 Trace 权限/预算/generation/取消限制，父批准不覆盖子调用；跨类使用被调方独立策略、审批、票据、预算、版本与恢复，不传递许可或合并整树 |
| SEC-A11 | 改策略后下一模型请求 | 追加带来源的当前状态，既有 system/history 前缀不被无关重写 |
| SEC-A12 | 容器文件工具与 shell 对同一路径操作 | 同一执行世界和真实目标；不把容器路径误用于宿主 |
| SEC-A13 | Auto 关闭/卸载或恢复时不可用 | 默认不启用；已依赖审核的执行不静默降成 Full access；实际模式变更明确可查 |
| SEC-A14 | 参数转换把合法路径/参数改成越界或非法值；授权后再尝试修改 | 最终校验或冻结一致性检查拒绝，旧批准不适用；execute 次数为 0 |
| SEC-A15 | 等待期间工具版本、cwd/环境、资源前提或策略变化 | 使用冻结描述复核，影响行为的变化重新校验授权；取消/撤销后不再启动 |
| SEC-A16 | 运行数据根落在 workspace/temp 内，或经链接/挂载别名可写 | 受限配置被拒绝或由已验证后端落实保护；文件工具和 shell 均不能修改调用执行意图/占用、预算或 checkpoint；Full access 不标成受保护 |
| SEC-A17 | 两个 Session 的 shell 写临时产物，再分别用文件工具和下一 Turn 读取 | 同 Session 指向同一实际文件；产物引用不能误绑定或跨会话取用同名文件，私有 tmpfs 路径不冒充持久产物 |
| SEC-A18 | read-only 下申请临时写；进程结束/清理/重启后读取失效引用 | 不暗中放宽 mode；不存在明确报告不可用，不从宿主同名路径补读 |
| SEC-A19 | 批准后未占用、占用后启动前、启动后结果提交前分别崩溃 | 关闭/重开不恢复批准；显式 Resume 复核策略，未占用且需审批时重新问，已占用未知先核对，已有结果复用；缺少 started 不推断未执行，新 executionId 不重跑 |
| SEC-A20 | 并发重复占用、占用提交失败，以及受信未启动/伪造 runner 错误 | 最多一个占用成功；提交失败不调用执行器；只有受信未执行证据可解除占用 |
| SEC-A21 | input hook/模板/扩展消息加入“已批准”，再进行 Auto 审核 | 原人类输入不可变，派生文本保留来源；新增授权不被采纳，user role 不提升权限 |
| SEC-A22 | 假父任务、导入 source 字段、未消费输入、摘要丢失原授权 | 由受信关系和适用原文判定；未知来源不授予许可，材料不足不放行，真实取消/撤销仍生效 |
| SEC-A23 | 人工声称未执行、受信查询仍在进行、受信后端确认未执行三种核对 | 前两者不直接解除许可或冲突；最后一种也须先提交核对与解除记录，再验证其他恢复条件 |
| SEC-A24 | 原生后端不可用或 helper 来源/版本不匹配 | 受限配置拒绝，原始裸命令执行次数为 0；不采用 Zero 的 auto degraded 直接执行 |
| SEC-A25 | 已 setup 且设施可用；未 setup 且文件受限；未 setup 且仅网络隔离；任意状态下 helper/保护失败 | 分别验证文件与网络能力：unelevated 属于未 setup 的文件受限路径，不是独立初始化等级；无法落实所需网络隔离或运行数据保护则拒绝，零裸跑；setup marker 不等于认证通过 |
| SEC-A26 | 文件工具与 shell 对同一目标，以及共享 Scope 不等于共享内核隔离 | 写限制一致；进程默认读全盘不能被误标为文件工具已隔离读取；Zero `Evaluate` 通过不等于已进入 OS 沙箱 |

已确认：策略、审批和审核参考 DSH；原生三平台执行参考 Zero 并按本产品契约适配。默认 workspace-write + ask、Auto 关闭；满足运行数据保护等必需条件后允许 partial 并如实标识，未满足时拒绝受限配置；审核能力失效不自动放宽。各 OS/容器后端、保护条件的实现与认证属于后续技术验证（D-19），不再作为未确认的产品默认行为，也不要求终端用户调 runner 参数。

本篇的冻结描述、许可占用、运行数据保护和会话产物目录是结合 pi/DSH/Zero 与 M05/M10 后补充的产品要求，不声称上游已经整体实现。Windows Job/控制通道、Landlock 自动回退和容器等选择仍需验证，尤其不能以平台目标或 Zero 源码存在替代 SEC-A16/17 的结果。

本篇未执行沙箱测试、未修改主机 ACL、未调用真实审核模型。上述验收需在后续开发方案和隔离测试环境中实现；本次仅补齐 PRD 与证据。
