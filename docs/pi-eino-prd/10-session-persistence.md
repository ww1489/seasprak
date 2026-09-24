# 第 10 章：会话、历史分支与执行恢复

状态：已按系统评审及用户确认完成本轮规格收口，持久化与恢复实现仍待验证。对应 [M10 会话管理](https://dg-ai-notes.pages.dev/modules/ch10-session/)。本章负责持久化承诺、分支和恢复；模型投影见 M06/M08，模型选择见 M04，运行状态见 M03，事件游标见 M07。

命名遵循[第 2 章的职责划分](02-architecture-boundaries.md#23-与-pi-对齐的对象名称与职责)：AgentSession 控制当前会话的执行，SessionManager 管理历史、分支与持久提交，SessionStore 是本产品提供给 SessionManager 的存储适配接口。后两者不是两个同义的会话管理对象。

## 1. 执行摘要

用户需要在进程重启后继续对话、在历史位置尝试另一条路径，并知道中断前到底执行到了哪里。会话恢复不能简单等同于重新发送最后一句话，也不能把 Eino checkpoint 当作用户长期对话档案。

沿用原方案的本地 JSONL 消息树方向，由 SessionManager 管理历史结构并通过 SessionStore 契约持久化会话；存储介质与历史结构分开。完整需求包括树状历史、活动分支、输入去重、任务状态、执行恢复关联和数据损坏诊断，不要求建立数据库产品或兼容 pi 文件格式。请求、事件与 checkpoint 的关联提交是本产品补充的要求，不据此宣称 pi 的同名对象已经提供这些保证。

成功标准：已受理输入与已提交消息在重启后可找回；分叉不改旧分支；恢复不会重放已确认副作用；未知扩展数据可保留；数据损坏不会被伪装成正常历史。

### 1.1 源码证据

| 已验证事实 | 本地证据 | 本产品采用/调整 |
| --- | --- | --- |
| pi coding-agent 的 entry 带 id/parentId，模型与摘要等也是 entry | [Entry 定义](../../pi/packages/coding-agent/src/core/session-manager.ts#L46) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:46]` | 借鉴不可变历史与派生状态，不照搬版本号 |
| buildSessionContext 沿选定分支取模型/思考状态，再经 compaction-aware entries 投影 | [投影](../../pi/packages/coding-agent/src/core/session-manager.ts#L461) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:461]` | 历史树与模型输入分开 |
| branch 移动 leaf，branchWithSummary 接收已生成摘要并追加 | [branch](../../pi/packages/coding-agent/src/core/session-manager.ts#L1360) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:1360]`；[摘要分支](../../pi/packages/coding-agent/src/core/session-manager.ts#L1381) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:1381]` | 教程的摘要生成伪代码不能当作存储层调用模型的依据 |
| pi 的状态提取同时读取 model_change 和 assistant 的 provider/model | [状态提取](../../pi/packages/coding-agent/src/core/session-manager.ts#L362) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:362]` | 教程中“assistant 不含模型来源”的描述不适用于本地版本；产品区分生效配置与响应来源 |
| header 不在历史树中，children 由读取时派生 | [文件与树类型](../../pi/packages/coding-agent/src/core/session-manager.ts#L144) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:144]` | 持久节点只认父节点，索引可重建 |
| 首条 assistant 前 pi 可以延迟落盘 | [_persist](../../pi/packages/coding-agent/src/core/session-manager.ts#L1015) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:1015]` | 本产品受理即需可恢复，不能照搬该策略 |
| pi 解析器会跳过无法解析的 JSON 行 | [parseSessionEntries](../../pi/packages/coding-agent/src/core/session-manager.ts#L299) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:299]` | 本产品要求报告损坏位置，不能静默掩盖中间历史缺失 |
| TurnLoop checkpoint 保存 runner bytes、未处理/中断输入，并用 gob 编码 | [checkpoint 数据](../../eino/adk/turn_loop.go#L942) `[VERIFY: eino/adk/turn_loop.go:942]` | 与产品 JSONL 分开，恢复入口需验证版本 |
| 仅特定停止/业务中断路径会尝试保存 checkpoint | [cleanup](../../eino/adk/turn_loop.go#L1998) `[VERIFY: eino/adk/turn_loop.go:1998]` | 任意崩溃后可 Resume 不是上游保证 |

### 1.2 采用的设计方法

- 分别决定“存在哪里”和“历史长什么样”：本地 JSONL 是首个后端，父节点关系定义历史树；更换存储不改变分支语义。
- 用追加记录和移动游标支持回退，保留旧尝试；树只存 parentId，children、按 ID 查找和展示顺序由读取时派生。
- 模型与思考级别的生效变更也成为历史节点，从选定路径重建当时状态；不拿全文件最后一条变更代表所有分支。
- 历史树在模型边界投影为线性消息，压缩改变投影视图而不删除原记录。会话历史与框架 checkpoint 分别负责长期记录和中断执行。

pi 的 coding-agent SessionManager 与 agent-core 的 session 存储实现并行存在，不能假设它们共享同一个可替换后端。本产品只定义一套 SessionManager → SessionStore 契约，详见[两套实现的证据](source-evidence.md#p-07两套会话实现不能混成一个现状)。

## 2. 用户体验与功能

### 2.1 会话使用

作为用户，我希望创建、列出、打开会话，查看完整历史与当前任务状态，并在同一工作区中继续；作为应用开发者，我希望内存测试后端与持久后端具有相同的历史操作语义，并明确区分是否承诺重启恢复。

- **SESSION-01**：Session 有稳定 sessionId、创建时必需且经验证的 cwd/工作区与执行环境绑定、格式版本、创建信息和活动分支；缺失工作区拒绝创建，不隐式使用进程 cwd。打开已有会话读取其绑定，执行前重新验证可用性，不自动运行工具或恢复任务。
- **SESSION-02**：列表/详情能区分空闲、排队、活动、等待用户、暂停及历史终态。Session 状态由 Trace 和队列派生，不把 TurnLoop 实例是否存在当作唯一真相。
- **SESSION-03**：同一会话只有一个 AgentSession 协调写入，所有持久提交经其 SessionManager；第二个进程争用同一个本地会话时，拒绝写入或只读打开，不能让两个活动 AgentSession 同时追加而假装串行。
- **SESSION-04**：inputId、输入类别、目标 Agent、generation/定义与依赖引用、目标分支、内容摘要与 traceId 映射在受理时一致保存，可恢复重建；queued/hold 项也保留原版本，重启或继续队列不重新选版。follow-up/steering 从原 Trace 继承目标，省略 targetAgent 不默认主 Agent，显式错配拒绝且不另建任务。同幂等请求返回原受理结果和版本，不因 reload 再选版；异内容或显式不同目标拒绝。

### 2.2 历史树、活动分支与分叉

产品历史 entry 至少有稳定 entryId、parentId、类型、时间与内容版本。parentId 指向同 Session 已存在的节点，null 表示虚拟根；header 不充当根消息。已提交节点不改父节点或内容，不复用 entryId。

沿用 pi 的 leafId 表示当前追加游标；回退后该节点可以已有子节点，“leaf”不要求它是整棵树的叶子。本产品的 branchId 只是稳定的分支头引用（branchId → headEntryId），不维护第二套历史树。保存活动分支与游标后才确认切换成功，重启不能用文件最后一行猜测当前位置。

作为用户，我希望从历史某点尝试新方向，并能再回到旧分支。

- **SESSION-05**：分叉保留共同前缀并创建新分支身份；旧分支及摘要记录不被修改或删除。
- **SESSION-06**：模型只看到所选分支的合法上下文；其他分支内容只有经明确选择的分支摘要才进入。模型配置可回溯，凭据/当前权限不随历史自动回滚。
- **SESSION-07**：同 Session 存在任何非终态顶层 Trace、待消费输入/未提交的扩展写操作或未解除的副作用冲突限制时，切换/分叉需先处理这些事项；建议返回 conflict，用户明确取消/清理队列并完成必要核对后再切换。独立读取历史始终可用。
- **SESSION-08**：一个工具批次尚未有完整结果时，不允许把其内部节点作为可继续执行的分叉点；拒绝并返回最近的合法边界。历史浏览仍可查看这些中间记录。
- **SESSION-09**：对话分叉不回滚仓库文件、命令或外部 API 副作用。新分支继续时按当前真实工作区执行，必要时重新读取状态，不能依据旧对话假定文件未改变。

#### 2.2.1 追加、回退与分支摘要

| 操作 | 最小行为 | 保留什么 |
| --- | --- | --- |
| 追加 | 新 entry.parentId = 当前 leafId；提交后游标指向新 entry，更新相应分支头 | 所有旧节点原样保留 |
| 回退/切换 | 在 SESSION-07/08 允许的边界，将游标移到指定节点或虚拟根并保存；若从历史位置开启新尝试，登记指向该位置的新分支引用 | 原分支头仍可选回；只移动游标不调用模型 |
| 回退后追加 | 新 entry 以游标为 parent，提交后推进新分支头；树的分叉由这次追加自然形成 | 共同前缀只存一份，旧后缀不删除 |
| 带摘要分支 | AgentSession 协调生成离开路径的摘要；SessionManager 将摘要追加在选定节点下，再在摘要下追加新消息 | 摘要记录来源路径/旧头与 details；是否带摘要由调用方明确选择 |

会话切换、同会话分叉/导航在基础校验后进入 M07 的对应 before hook；取消或候选失败不改变已保存游标。通过后重新校验提交前置条件，再将游标、分支引用及必要摘要一致提交，成功后才发结果通知。一次创建分支不重复派发导航 before hook；同会话分叉不创建新 Session。

新分支身份最迟在受理指向它的输入时一并保存，避免已受理 inputId 在消费时被改归另一分支；独立只读浏览不创建分支。普通回退不必生成摘要。带摘要操作使用 M09 的生成和失败规则，候选失败时不提交半个分支切换；SessionManager 只接受已生成的摘要。新消息必须以摘要节点为祖先，不能把摘要和新消息写成兄弟节点，否则沿新路径重建时读不到摘要。

示例（省略时间、版本与运行身份；ToolResult 是产品语义，Eino 中仍使用 M06 的 user-role FunctionToolResult 内容块）：

```text
e1 model_change(A)
└─ e2 用户：检查 auth.ts
   ├─ e3 assistant：调用 read
   │  └─ e4 ToolResult：文件内容
   │     └─ e5 assistant：分析结论        ← 旧分支头
   └─ e6 用户：改看 hash 函数            ← 回退到 e2 后追加
      └─ e7 assistant：新分析            ← 新分支头
```

选择 e7 时只取 e1 → e2 → e6 → e7；旧分支 e3～e5 仍可查看。若明确携带旧分支摘要，新路径改为 e2 → 摘要节点 → e6 → e7。回退本身没有重发 e2，也没有重新执行 e3 的 read。连续用户消息是否需要协议归一化由 M04/M06 处理，存储层不擅自合并原记录。

pi 的 createBranchedSession 将选定路径复制成另一个 sessionId/文件，这是“复制为新会话”，与同一会话内回退不同。本章先定义同会话分叉，不额外要求实现会话克隆功能。[源码](../../pi/packages/coding-agent/src/core/session-manager.ts#L1413) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:1413]`。

### 2.3 恢复的三个不同动作

| 操作 | 恢复什么 | 是否执行 |
| --- | --- | --- |
| 打开会话 | 历史、分支、队列与任务记录 | 只读取，不自动恢复工具 |
| 继续对话 | 无活动 Trace 时从合法历史创建新 Trace；活动中的追加输入按 M03 分类 | 新 Trace 按 4.4.1 选择并验证配置；follow-up 延续原 Trace |
| 恢复中断执行 | checkpoint 中的原执行状态、未完成 Turn 与指定交互 | 保留 traceId/未完成 turnId 与原 targetAgent，创建内部 executionId；验证原 generation、工作流定义/绑定、当时有效模型和投影 |

- **SESSION-10**：只有存在完整、兼容且状态允许的 checkpoint 才标记 `canResume=true`；AgentSession 必须同时检查 Eino checkpoint Store 的成功结果和 SessionManager 保存的产品关联记录。
- **SESSION-11**：进程崩溃时处于 running 的任务，重启后进入 paused/recovery_required，列出最后提交位置及未决工具。不能默认重放请求，也不能因流断了标成完成。
- **SESSION-12**：显式 cancelled 的任务终态不自动复活；旧 checkpoint 不得因为恰好仍存在而被新任务选中。历史重试创建新 Trace 并关联原运行。
- **SESSION-13**：重复、迟到或跨 Trace 的恢复回复拒绝或返回已处理结果；不允许借一个过期 interactionId 启动第二次执行。

## 3. AI 系统需求

SessionManager 和 SessionStore 本身都不调用模型。SessionManager 管理普通会话历史、压缩摘要和分支摘要的记录及来源关系，通过 SessionStore 保存；摘要生成由上下文能力提供，AgentSession 协调其执行和提交边界，并保留来源范围、模型、质量检查结果及版本。ResourceLoader 与 ExtensionRegistry 仅提供候选资源和注册声明，不承担恢复任务的调度。

存储任何文本并不自动授予其指令地位。未知 role 原样保存，按 M06 处理显示与模型投影；恢复旧历史不能让未知扩展数据冒充 system 指令。

评估分两类：存储/恢复行为通过固定记录、故障注入和可控工具判断；摘要是否保留目标、约束、未完成任务等信息由 M09 的语义验收判断。不能以模型生成摘要“看起来合理”替代恢复正确性。

## 4. 技术规格

### 4.1 四类数据的所有权

| 数据 | 语义所有者 | 实际保存者 | 内容与边界 |
| --- | --- | --- | --- |
| 历史树 | L3 SessionManager 管理历史、分支和上下文来源；AgentSession 协调运行中的提交时机 | SessionManager 经 SessionStore 提交 | 最终化产品消息、摘要、配置变更、分支关系；不逐 token 追加一条对话消息 |
| 受理与执行记录 | L3 AgentSession 决定输入受理、Trace 状态及执行尝试记录和交互；SessionManager 管理其与历史、活动分支的关联 | SessionManager 经 SessionStore 提交 | inputId、Trace 状态及执行尝试记录、交互、工具结果状态、活动分支；SessionStore 不作调度或状态迁移决策 |
| 可重放产品事件 | AgentSession 定义并发布产品事实；SessionManager 管理持久事实与状态提交的关联顺序 | SessionManager 经 SessionStore 提交记录与游标，保存成功后 AgentSession 发布 | M07 定义的 durableSeq 与产品状态变化；可由同一提交记录派生，不强制另一数据库 |
| 执行 checkpoint | Eino 定义不透明执行状态；Agent 适配框架结果；AgentSession 判断当前请求是否可恢复 | Eino checkpoint Store 保存执行数据；SessionManager 经 SessionStore 保存产品关联元数据 | opaque runner bytes、格式/框架版本、关联 Trace、targetAgent、generation、工作流定义/绑定、分支/投影版本；不作为 UI 历史 |

这里定义的是数据承诺，不要求四套独立服务。SessionManager 保有历史树与提交语义，SessionStore 只适配底层存储；它们不接管 AgentSession 的任务队列、模型调用和状态迁移决策。JSONL 首先作为 SessionStore 的本地实现；checkpoint 可另存不透明数据，二者通过明确 ID 关联。最终文件布局、原子提交方式、索引与接口签名在整套 PRD 评审后制定。

#### 4.1.1 Entry 的三类职责

借鉴 pi 的九种 Entry 所承担的三类职责；下表是语义分类，不要求九套 Go 对象或独立服务。Entry 是历史记录，AgentMessage 是执行上下文，二者不等同。

| 分类 | pi 对应类型 | 本产品用途与模型边界 |
| --- | --- | --- |
| 可形成上下文 | SessionMessageEntry、CustomMessageEntry | 保存已消费消息、最终化 assistant/工具观察以及扩展注入内容；按 M06/M08 投影，CustomMessage.details 不自动进入模型 |
| 可形成上下文 | CompactionEntry、BranchSummaryEntry | 保存摘要正文、来源范围与 details；按 M09 生成摘要消息，与真实对话来源区分 |
| 历史配置状态 | ModelChangeEntry、ThinkingLevelChangeEntry | 记录当前路径已生效的模型配置引用/版本、思考级别及生效位置；用于状态重建，不伪造一条用户消息 |
| 纯元数据/扩展状态 | SessionInfoEntry、LabelEntry、CustomEntry | 会话名称、书签目标或扩展私有状态；可用于查询或扩展恢复，不自动进入模型 |

名称/书签按功能需要采用，列出类型不要求开发正式会话管理前端。CustomEntry 与 CustomMessage 必须区分：前者只保存状态，后者允许 content 参与上下文；两者都不因持久化而取得更高指令权限。

受理队列、Trace 状态、活动游标、持久事件和 checkpoint 关联使用 4.1 的运行/控制记录表达，不必全都伪装成树中的消息节点。控制记录不会因模型不可见而丢失，也不能因写入同一日志就推进历史游标。配置节点仅记录已经生效的选择；“下次 Trace 使用 B”仍是待生效设置，不能提前改写正在执行的历史状态。

#### 4.1.2 本地 JSONL 的最小契约

- 本地后端以一个 Session 的 JSONL 主日志保存 header 与记录，UTF-8 编码，一行一个完整 JSON 对象。header 保存文件格式版本、sessionId、创建信息和工作区绑定；历史 entry 与运行/控制记录有可区分的类型，header 不参与 parent 链。checkpoint/大附件可由记录引用，不要求塞入同一文件。
- 历史节点只保存 parentId；ID 索引、children 和分支展示结构可由已提交记录重建。时间戳供展示/诊断，文件追加顺序供读取，二者都不替代选定 parent 路径与已保存的活动游标。
- 普通写入追加记录；多行提交仍须满足 4.2，不能把“每行是完整 JSON”当成跨记录原子性。pi 使用的 `openSync("wx")` 只防止覆盖已有文件，不保证随后多次写入共同成功。[首次写入](../../pi/packages/coding-agent/src/core/session-manager.ts#L1015) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:1015]`。
- 存储目录由应用装配决定，与 cwd 绑定和权限检查分开；日志、审批/许可、checkpoint 及可信关联元数据的根必须满足 M12 的运行数据写保护。受限模式默认放在业务/临时可写根之外，重叠或别名需由真实后端落实保护，否则拒绝配置；不要求在用户代码仓库内创建会话日志。pi 默认目录是 `~/.pi/agent/sessions/<encoded-cwd>/`，并非项目内 `.pi/sessions`。[目录规则](../../pi/packages/coding-agent/src/core/session-manager.ts#L474) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:474]`。
- 内存后端可用于测试/临时会话，遵循同样的树、投影和去重语义，但必须声明不承诺进程重启恢复；提供本章持久受理承诺的服务必须装配持久后端。不能复制 pi 首条 assistant 前延迟落盘的策略后仍宣称输入受理即已保存。

逻辑历史追加不等于文件永远不能重写。版本转换或修复可生成经过校验的新文件，但不得借此丢掉旧分支或静默覆盖原件；4.5 规定兼容与损坏边界。这里只约束可恢复的记录格式与行为，不冻结目录命名、锁实现、索引格式或事务算法。

### 4.2 提交与恢复一致性

1. AgentSession 协调受理，SessionManager 保存原始输入内容/受保护引用、受信来源和归属后才返回成功响应；转换结果关联原输入而不覆盖它。用户消息只在实际消费时进入模型可见历史，pending 可查询但不作为当前工具的新授权。
2. SessionManager 保存关键状态、最终消息及关联持久事件成功后，AgentSession 才能发布对应 durable 事件；瞬态文本可以更早显示，但不能声称已持久化。
3. 历史更新、活动分支头、Trace 状态与事件游标必须能作为一致提交恢复。若实现使用多条记录，需要明确提交标记或等价机制，恢复时不能暴露半条状态。
4. 返回“保存成功”意味着按选定存储契约完成提交；进程级崩溃必须能读回最后确认提交。操作系统掉电的持久化边界、flush/sync 策略由开发方案明确，不能以普通 append 返回值冒称所有故障都无损。
5. 相同作用域的 inputId、messageId、toolCallId/执行身份重放不重复生效，不以消息文字相同作为去重依据。产品 `trace.settled` 按 traceId 保证最多一个已提交收尾事实，重放仍使用原事件身份；内部 `agent_start/agent_end` 若保存则按 executionId 与事件身份区分，不能只按 traceId 抹掉后续执行段。重试、压缩续执行、follow-up 和审批恢复不创建第二个 Trace，但可产生真实的新执行段事件。
6. 存储失败后停止推进副作用并报告 storage_unavailable；已发生的工具操作按已知/未知结果保存或在恢复后核对，不能继续向前端发布虚假 durable 完成。
7. 单次许可占用和执行意图必须在调用执行器前一致提交，不能等 tool.started/finished 才标记已使用。原子占用拒绝并发重用；占用、启动和结果分开记录，不承诺本地日志与外部效果共同原子化。

### 4.3 checkpoint 关联与兼容

每个可恢复执行的产品记录至少关联 sessionId、branchId、traceId、内部 executionId、targetAgent、未完成 turnId（如有）、checkpointId、generation、工作流定义/子流程/节点执行器与资源绑定版本（如适用）、当时生效的模型配置、本 invocation/Turn 的工具选择、扩展状态提交引用、projectionRevision、框架/序列化版本和待答交互。traceId 是必需的运行归属，不能因日志未采样而缺失；观测 span 是独立可选数据。仅凭 traceId 不足以授权恢复，仍须验证状态、checkpoint 和当前权限。

历史的标准内容使用 Agentic 消息表示，保留块顺序、CallID、响应及协议 metadata；文件版本集中由本章管理。普通 CustomMessage 保留通用 content/details，扩展停用后仍可读取与转换；真正未知的特殊结构保留原始数据，是否可用于模型按 M06 判定。不要求每种 customType 自建编解码与版本平台。

只有当 Eino checkpoint Store 写入成功、SessionManager 确认对应历史和交互状态可恢复时，AgentSession 才对外发布稳定的 waiting_input/canResume。若 checkpoint 与产品记录写入顺序间崩溃，AgentSession 在打开会话时通过各自接口交叉核对；不匹配则 paused，不自动执行。

Eino 退出结构单独提供 `CheckpointAttempted`、`CheckpointErr` 和未处理输入；判断不能只看 `ExitReason`。[退出状态](../../eino/adk/turn_loop.go#L794) `[VERIFY: eino/adk/turn_loop.go:794]`。

AgentSession 恢复原 Trace 的 targetAgent、generation、checkpoint 对应的轮次和模型配置，并通过 Agent 执行兼容 checkpoint；不创建新 Trace、不重复已提交的 Turn/工具事实，也不因界面改选更换原执行者。新执行段可有新的内部 agent_start/agent_end；waiting_input/paused 尚未完成整个 Trace，不发布 trace.settled，最终终态提交后才发布一次。旧资源不能重建则返回 incompatible_resume，可浏览历史或明确另起 Trace，不能静默升级。已撤销权限不会因旧批准而恢复。

工作区路径或工具环境变化时重新验证：在 Windows 保存的 checkpoint 不承诺能直接在 Linux 恢复进程执行。历史仍可读取；选择不同业务工作区时创建或切换到明确绑定该工作区的 Session，不静默改写旧绑定。原 checkpoint 只有工作区/执行环境及其他恢复条件兼容时才可继续。

#### 4.3.1 工具选择与扩展状态的恢复

工具清单保存或引用当时的 generation、invocationId、生效 Turn、普通/延迟工具集合及选择来源；有已受理的待生效请求时一并保留归属和状态。resume 先恢复 checkpoint 对应的生效集合，处理完未完成调用后才考虑后续请求，不把当前全局工具清单或 pending generation 混入旧执行。调用身份仍绑定原实现与原选择，当前权限撤销照常生效。

顶层扩展通过 M11 受控入口提交 CustomEntry，记录扩展命名空间/customType、作用域、必要版本及数据，按选定 parent 路径顺序交给扩展恢复自身状态。普通打开使用当前路径，checkpoint 恢复使用其关联提交位置；压缩仅影响模型投影，不能跳过恢复所需私有状态。子调用私有状态使用 4.1 的执行关联记录，带 invocationId 和提交引用，由协调者保存，不推进主会话游标或要求另一套会话树；两种作用域都不另建状态数据库。

扩展操作沿用 M07 命令幂等契约并包含扩展与作用域身份，受理记录和最终提交状态可区分。回调内的模型可见消息/私有状态写入只能由协调者在合法边界应用；waiting_input 时禁止旁路改写 checkpoint 所对应历史，可受理为待处理操作。取消或执行结束时未应用请求有明确去向，不静默迁到其他 Trace/分支。关键状态不依赖 session_shutdown 才首次保存。

#### 4.3.2 审批许可与授权材料的恢复

恢复同时读取原 approvalId、冻结执行描述、决定有效性、许可占用/执行意图及可确认的启动/结果事实；处理规则以 [M12 许可恢复表](12-security-sandbox.md#221-一次许可的占用启动与恢复) 为唯一来源。已批准未占用可在重新验证后继续原调用；已占用且启动情况不明则待核对，不能因为新 executionId 或 checkpoint 仍在 pre-execute 就再次启动。仅受信未执行证据允许先提交解除占用；已执行的业务失败不使批准复活。

身份关联使用原逻辑调用，内部 executionId 的变化只记录恢复关系，不创建另一份许可。审批响应、占用和核对均去重并保存；外部查询确认了结果时追加事实，不改写旧观察或重复执行。记录不一致、运行数据保护异常或无法确认来源时停止自动恢复/授予许可并保留诊断。

原人类输入、派生内容、直接父子委派及限制记录按 M06/M12 保留各自来源，普通压缩不删除授权所需引用。安全审核需要时从受保护存储解析原文，不把 SessionEntry 的 user role、导入标签或摘要当作可信身份；敏感原文不广播到普通事件。

#### 4.3.3 核对事实的提交与恢复资格

AgentSession 为核对分配 operationId，关联原 invocation/toolCall、观察记录版本、材料/产物引用、受信来源或人工提交者，以及可确认效果和仍未知部分。SessionManager 串行追加受理及核对事实，保留原 unknown 观察；并发或迟到的矛盾材料不能覆盖已提交事实，也不能只凭客户端结论解除限制。

核对属于受控执行记录操作，允许目标处于 paused 或已终态但仍有未决效果，不受“普通分叉/手动压缩必须空闲”的规则阻断；它不旁路启动第二个业务执行者、不推进另一条历史分支，也不改当前正在消费的模型输入。操作状态、可公开结论与 tool.state_changed 均在对应提交成功后可见。

结果提交后分别判断：是否仍有未决效果/冲突、许可是否允许使用、是否存在兼容 checkpoint、适配器能否把已有调用结果交给恢复点而不重复执行。全部条件满足才标记 canResume，仍需显式 resume；无兼容 checkpoint 则明确不可恢复，原 Trace 可按状态被明确结束后另起新 prompt。核对不把 cancelled/failed 改成 running，不自动继续旧队列；解除许可占用继续遵守 M12 的受信证据要求。

### 4.4 分支、压缩与权限的关联

压缩摘要记录保存创建时的分支及来源范围，不删除原消息；是否适用取决于记录是否位于当前 leaf 的祖先路径，以及覆盖关系和保留边界是否仍合法。B 从共同前缀的压缩记录之后分叉时可继承该摘要；A 分叉后独有的摘要不能自动用于 B。branchId 是分支头引用/来源信息，不是拒绝公共祖先摘要的依据。新有效投影提交后更新对应 projectionRevision；waiting_input 中不能旁路改写 checkpoint 所对应的模型历史。

CompactionEntry 保存 M09 的摘要正文/程序附录、firstKeptEntryId（包含该记录）、上一有效摘要引用及文件 details/来源范围，随摘要作为一个一致候选提交；不从摘要文字反推文件清单。重建只采用当前分支最新生效摘要及合法保留区，不重复展开已覆盖原文。文件集合表示有证据的历史操作而非当前文件内容或 Git diff；失败/未知调用的原记录仍可查询，核对后的更正通过追加事实表达。外部导入或未知格式的 details 不自动成为已确认修改清单。

恢复模型配置不等于恢复旧凭据或权限。模型版本不可用时明确报告，允许用户选择新任务的替代配置；不能无声降级。密钥、浏览器连接、进程句柄和活动文件描述符不进入 JSONL 或通用序列化 payload。

安全参考 DSH 的 asked/decided 配对和策略日志，但使用本产品提交格式。SessionManager 保存审批引用、冻结执行描述、许可占用/消费与执行意图、实际沙箱 mode/enforcement 与策略变更；缺失 decided 的请求不是已批准。Eino checkpoint 不代替审批记录，恢复不得使已消费许可再次生效；未知工具效果仍先核对。Auto 最小决定元数据的保存按安全补充篇，不保存 reviewer 推理。

#### 4.4.1 从选定路径重建上下文与配置

1. 以已保存或明确指定的 leafId 为起点，沿 parentId 回溯到虚拟根，再反转为根到当前节点的路径。验证 ID 唯一、父节点存在、无环且不跨 Session；指定节点失效时报告错误，不静默换成文件最后一条。
2. 在**完整选定路径**上依次提取模型/思考级别的生效变更，后者覆盖前者；不读取兄弟分支的状态。配置提取先于消息压缩选择，因此被摘要覆盖区域里的配置仍然有效。无历史值时保留“未记录”，由应用初始配置兜底，不编造历史值。
3. 按 M09 选择当前路径最新有效 CompactionEntry：摘要消息 + 从 firstKeptEntryId 本身起到压缩节点前的合法保留区 + 压缩节点后的记录；无压缩则使用完整路径。只将可形成上下文的 Entry 转为 AgentMessage，其他记录仍可查询。
4. 将上述上下文交给 M08 管道，执行 M06 的 transformContext / convertToLlm，最终输出 `[]*schema.AgenticMessage`。配置、书签、原始审批审计和私有 details 不因同在 JSONL 中就进入模型。

本地 pi 还从 assistant 的 provider/model 推断模型状态，教程对此有简化。本产品以明确的**生效配置记录**作为配置重建依据，assistant 仍保存实际请求/响应模型来源；供应商返回的模型别名、路由信息或子调用来源不能擅自变成下一次主 Agent 的默认模型。这是对 M04 请求配置与响应来源区分的延续，不要求复制 pi 的字段或解析器。

| 使用场景 | 配置取值规则 |
| --- | --- |
| 浏览历史/切换游标 | 返回所选路径的历史模型与思考级别；只读浏览不改变活动执行 |
| 继续为新的独立 Trace | 用户明确指定或已登记待生效的新配置优先；否则使用路径重建值作为默认候选，缺失项使用应用初始值。AgentSession 按 M04 验证当前模型可用性、消息兼容、预算、凭据及权限，记录实际生效配置后执行 |
| 活动 Trace 的 follow-up/逐 Turn 选择 | 遵循原 Trace 已装配策略；获准的 prepareNextTurn 选择在相应位置记录生效，不读取下一 Trace 的待生效设置 |
| 恢复原 checkpoint | 使用 checkpoint 关联的有效模型、思考参数、generation 和投影；不被新默认值或任意历史节点的配置替换 |

例如旧分支在 e5 之后切到模型 B/高思考级别，回到 e2 后不应继承这两项；从 B 所在路径继续则能恢复相应历史状态，即使那条配置记录早于最新 firstKeptEntryId。以上规则不恢复旧密钥或权限，也不改变 M04“用户新默认值在下一独立 Trace 生效”的边界。

### 4.5 损坏、未知类型与外部产物

#### 4.5.1 格式版本与兼容

| 文件格式 | 读取与写入行为 |
| --- | --- |
| 当前支持版本 | 校验 header、记录结构和树关系后正常打开；写入仍受单写者与提交规则限制 |
| 明确支持的旧版本 | 使用已定义的兼容读取规则；写入前转换为当前格式或明确只读，不直接混写新旧格式 |
| 未支持的旧版/未知未来版本 | 返回 incompatible_format；能安全辨认的元信息与原始文件可供查看/导出，不猜测字段含义或按当前版本追加 |

版本在文件层集中管理，内容类型可带必要版本；新增可选字段应原样保留。未知记录按 M06 与下节处理，不能仅因 header 可读就宣称模型上下文完整。

转换/修复先生成候选并验证 ID、parent 链、活动游标、firstKeptEntryId、摘要 details 和运行关联；如确需更换身份必须同步映射全部引用。成功后才切换使用，失败保留原文件与可诊断原因。首个版本只需声明实际支持的格式和拒绝规则，不虚构尚不存在的旧版迁移器，也不新建迁移平台。[pi 历史迁移](../../pi/packages/coding-agent/src/core/session-manager.ts#L230) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:230]` 与 [_rewriteFile](../../pi/packages/coding-agent/src/core/session-manager.ts#L979) `[VERIFY: pi/packages/coding-agent/src/core/session-manager.ts:979]` 仅作为格式演进参考，不照搬其覆盖写入策略。

#### 4.5.2 损坏与外部数据

- 未完成尾行与中间损坏分别报告。可读取已确认完整前缀；任何修复不得静默覆盖原文件。中间断链、重复 ID 异内容、非法 parent 或未知未来格式时禁止按正常状态继续写。
- 未知扩展记录保留 type/version/raw payload；可展示“暂不支持”，按 M06 决定是否阻止模型执行；不能为了能打开而删掉数据。
- 工具 unknown 的核对经 M07 受控操作追加关联原调用的 reconciliation 记录，不改写原始观察、不新增第二次工具执行。当前视图可反映已确认效果，模型投影仍只产生一个有效配对结果；已取消/失败请求不因此复活。材料、可确认结论与恢复资格按下节分开保存。
- 大工具输出可作为 artifact 存储并由消息引用。引用需检查工作区/会话访问边界、执行环境及资源可用性；过期引用不能伪装成空结果。artifact 内容与可信授权/checkpoint 元数据分开存放；会话产物根及后端私有临时区按 M12 映射，私有临时路径须先导出保存，不依据路径同名恢复。具体保留期限由产品策略配置，不在此预设云存储服务。
- 用户导出包含格式版本、工作区信息的必要部分和所选历史，默认排除秘密配置。删除会话、跨工作区或跨后端迁移如后续纳入范围，需要独立描述，不借“追加历史”承诺永远不能删除个人数据。

### 4.6 验收场景

| 编号 | 场景 | 通过条件 |
| --- | --- | --- |
| SESSION-A01 | 输入受理成功后立刻杀进程并重启 | 输入和请求仍可查询；不重复创建或自动执行未核对副作用 |
| SESSION-A02 | 分叉 A/B，切回 A，再重启 | 两条分支都存在，活动分支正确，模型输入没有 B 的私有后缀 |
| SESSION-A03 | 修改文件后回到修改前的对话节点 | 文件不自动回滚；新任务读取真实文件状态 |
| SESSION-A04 | 一批工具只有部分结果时尝试分叉 | 拒绝不可继续边界，并保留所有历史供查看 |
| SESSION-A05 | HITL 暂停、登记新扩展、重启并恢复 | 使用匹配旧版本/投影恢复；版本不存在则明确阻止 |
| SESSION-A06 | 外部操作已完成但结果提交前崩溃 | 恢复显示 unknown 并要求核对，不无条件再次执行 |
| SESSION-A07 | 在输入/历史/checkpoint/事件写入间注入失败 | 不发布不存在的 durable 成功；可诊断不完整提交 |
| SESSION-A08 | 尾行不完整、文件中间损坏、未知 role | 三者有不同诊断；原始数据保留，不静默丢条目 |
| SESSION-A09 | 第二个进程打开同一会话并写入 | 只有一个写入者成功，另一个被拒绝或只读 |
| SESSION-A10 | Windows/Linux/macOS/容器分别保存与重启 | 各环境受支持路径下会话可恢复；跨环境 checkpoint 不被误判为兼容 |
| SESSION-A11 | 混合消息、摘要、配置与纯元数据 Entry | 按三类职责重建；CustomEntry 不进入模型，CustomMessage 仅投影允许内容，header/控制记录不成为消息 |
| SESSION-A12 | 回退到祖先后未追加就重启，再追加新输入 | 恢复已保存游标；新节点 parent 指向该游标，旧分支头仍可返回，不按文件末行选位置 |
| SESSION-A13 | 带摘要分叉后继续，再模拟摘要生成失败 | 成功时新消息位于摘要之后且能读到摘要；失败不提交半个切换，普通回退不强制调用模型 |
| SESSION-A14 | 分支 A/B 分别切模型与思考级别，再压缩/重建 | 只取所选路径最后生效状态；压缩之前的配置仍有效，响应模型别名不覆盖生效配置 |
| SESSION-A15 | 历史模型 A、待生效默认 B、原 checkpoint C | 浏览呈现 A，新独立 Trace 验证后使用 B；恢复使用 C，缺配置/权限时明确失败，不静默替换 |
| SESSION-A16 | 支持旧格式、未来格式、转换中失败 | 旧格式按声明读取/转换；未来格式不追加，转换失败保留原件，成功后历史身份和摘要/活动游标引用一致 |
| SESSION-A17 | 相同逻辑记录分别使用内存与持久后端 | 进程内树/投影/去重一致；仅持久后端承诺受理成功后重启可读，能力标识无误导 |
| SESSION-A18 | 同 Trace 多次内部执行、暂停恢复与事件重放 | 各 executionId 的真实边界可区分；未完成时无 settled，最终 trace.settled 仅一条持久事实，重复送达不重复生效 |
| SESSION-A19 | 单次许可占用提交后、目标操作启动前后分别中断 | 新 executionId 不重复占用/执行；无法确认启动则待核对，受信未执行证据解除占用也先提交 |
| SESSION-A20 | 授权原文引用失效、运行数据目录暴露、artifact 私有临时路径丢失 | 不从摘要补许可、不在有保护缺陷的受限配置继续执行、不从宿主同名文件恢复产物 |
| SESSION-A21 | 缺少工作区创建；显式裁剪工具后创建；打开已有会话 | 缺失拒绝；裁剪不免除绑定；打开读取原绑定且不自动执行 |
| SESSION-A22 | 分支 B 继承共同压缩祖先，A 有独有新压缩记录 | B 使用共同祖先摘要与合法保留区，不因 branchId 不同丢弃，也不带入 A 后缀 |
| SESSION-A23 | paused/终态目标提交核对，随后查询恢复资格 | 材料和结论分开，追加事实不重跑；只有兼容恢复才 canResume，终态保持，冲突或未知不被人工标签清除 |

## 5. 风险与系统闭合

会话树本身不解决副作用幂等、跨文件事务或事件发布一致性。本章要求这些接口之间能共同恢复，不假定 JSONL 自动提供事务，也不自动引入分布式“恰好一次”执行承诺。

必须和 M03/M04/M05/M06/M07/M09 联合评审输入提交点、历史配置与新 Trace 的选择、工具 unknown、摘要与 checkpoint 版本、事件游标。对会话树与工具副作用的恢复演练属于后续开发方案的验证前提；本轮仅完成规格。总体评审见 [系统闭合检查](system-review.md)。
