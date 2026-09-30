---
name: P3 Web 具体开发计划
overview: 新增 cmd/web --web 本机服务，复用 Eino 摘要中间件及官方示例 A2UI 子集，前端使用 BeautifulUI 组件和 React/TypeScript 静态构建，完成可靠 HTTP/SSE、分支与压缩，以及启动时注册的真实多 Agent/工作流。保留现有 SDK 和安全边界。
todos:
  - id: p3-contracts
    content: 固定 P3 扩展范围、协议与 Eino/A2UI 复用契约，完成框架探针
    status: completed
  - id: p3-web-bootstrap
    content: 新增 cmd/web --web、受信装配、生命周期与统一本机认证
    status: in_progress
  - id: p3-command-api
    content: 实现只读目录、创建幂等、持久取消/队列回执及现有操作 HTTP 接线
    status: completed
  - id: p3-replay
    content: 实现固定交接点重放、临时聚合与可靠 SSE
    status: completed
  - id: p3-history
    content: 实现历史树、稳定分页与真实分支操作
    status: completed
  - id: p3-compaction
    content: 接通唯一上下文预算、Eino 摘要候选、原子压缩和恢复
    status: in_progress
  - id: p3-static-agents
    content: 实现启动注册、多目标路由、子调用作用域与受控委派
    status: pending
  - id: p3-static-workflows
    content: 实现静态 Eino 工作流校验、受控节点账目及两种入口恢复
    status: pending
  - id: p3-a2ui
    content: 实现受权附件/维护接口、A2UI 投影及 BeautifulUI 组件前端的安全操作
    status: pending
  - id: p3-validation
    content: 完成端到端、故障窗、跨平台和完整仓库验证，记录剩余阶段范围
    status: pending
isProject: false
---

# P3 Web 与核心能力开发计划

## 一、已确认范围

- 新增 `cmd/web`，通过 `--web` 启动接口与页面；`cmd/agentd` 继续只提供 help/version。
- 单用户本机服务，默认只监听回环地址；工作区、状态目录、模型与工具由受信启动配置装配。
- 前后端采用 Eino 官方示例的 **A2UI v0.8 子集**，复用消息结构与数据绑定语义；不宣称实现完整 A2UI 标准。页面使用 [BeautifulUI](https://www.beautifului.dev/) 的实际组件源码作本地适配，不再复用 Eino 示例页面的视觉组件实现，也不只模仿 BeautifulUI 外观。
- 前端使用 **React + TypeScript + Vite** 构建静态资源，由 `internal/web` 通过 Go `embed` 同源提供；运行 `cmd/web --web` 不依赖 Node 服务、Next.js 或外部 CDN。BeautifulUI 只负责视图与交互外观，A2UI 负责受限呈现协议，Session 层继续拥有所有业务事实和权限判断。
- 压缩直接复用当前锁定 Eino `v0.9.21` 的 `summarization.NewTyped`、`TokenCounter`、`GenModelInput`、`Finalize`、`Summarize`，不另写摘要生成引擎。
- 前移真实分支导航、压缩以及多 Agent/工作流基础。压缩按原设计保留手动、自动阈值及已认证溢出恢复入口，统一使用同一适配服务；不能因使用中间件就省略持久化、预算和恢复断言。
- 多 Agent/工作流采用**启动时注册、完整参数执行**；不实现 Coze 动态导入、业务补参问答、运行中 reload。这些及完整扩展生命周期仍归 P5。
- **不开放网页宿主 shell**，不提供对应可执行路由。模型或工作流只能调用受信装配且经过现有安全管道的工具；不因此认证 P6 原生沙箱。
- 不引入第二个会话状态机、第二套 ReAct 循环、浏览器直连模型或 HTTP handler 直接写 Store。

### 阶段边界与已有证据

P2 的嵌套检查点和 Snapshot 优化仍在当前工作区；不得覆盖、回滚或顺手重构。上一轮 Windows 普通/race、静态检查和最终 live 测试已有通过记录，但偶发租约失败根因、macOS 验证及提交推送状态仍须独立记录。Gemini 两次 503 与随后通过保留，按维护者决定不继续调查。以上不是 P3 的验收证据。

本计划通过后，先同步阶段覆盖关系：P4/P5 被前移的条目逐项标记，不能把整个 P4/P5 自动标为完成。暂不实施完整 Skills 发现系统、通用扩展 hooks 平台、原生沙箱、远程多用户或工作流编排界面。

## 二、依据与已核实修改点

- 执行规则：[DEVELOPMENT-PLAN-GUIDELINES.md](D:/Code/owner_agents/seasprak/docs/DEVELOPMENT-PLAN-GUIDELINES.md)。
- 接口、事件与安全主定义：[06-events-and-api.md](D:/Code/owner_agents/seasprak/docs/pi-eino-dev-plan/06-events-and-api.md)。
- 上下文、压缩、历史和工作流：[07-context-and-skills.md](D:/Code/owner_agents/seasprak/docs/pi-eino-dev-plan/07-context-and-skills.md)、[08-compaction.md](D:/Code/owner_agents/seasprak/docs/pi-eino-dev-plan/08-compaction.md)、[09-persistence-and-recovery.md](D:/Code/owner_agents/seasprak/docs/pi-eino-dev-plan/09-persistence-and-recovery.md)、[10-extensions-and-workflows.md](D:/Code/owner_agents/seasprak/docs/pi-eino-dev-plan/10-extensions-and-workflows.md)。
- 当前 [events.go](D:/Code/owner_agents/seasprak/internal/sessions/events.go) 只提供新事件订阅；[state/events.go](D:/Code/owner_agents/seasprak/internal/sessions/state/events.go) 没有固定上界分页重放。需要补交接能力，不能在 handler 中先快照后订阅冒充无遗漏。
- 当前 [state/replay.go](D:/Code/owner_agents/seasprak/internal/sessions/state/replay.go) 以线性消息视图为主，存在 parent 字段不等于已实现历史树。
- 当前 [execution.go](D:/Code/owner_agents/seasprak/internal/sessions/execution.go) 固定创建主 Agent，并直接转换历史；[runtime.go](D:/Code/owner_agents/seasprak/internal/agent/eino/runtime.go) 尚未接入产品压缩适配及受控子 Agent。
- 当前 [approval_memory.go](D:/Code/owner_agents/seasprak/internal/sessions/approval_memory.go) 将一次许可保存在运行实例内存；P3 不将许可写进日志或检查点。
- SDK 唯一入口：[sdk.go](D:/Code/owner_agents/seasprak/sdk/sdk.go)。内部 Web 包调用同一 `internal/sessions` 入口，不反向 import SDK。
- 上游参考：[摘要中间件](https://www.cloudwego.io/docs/eino/core_modules/eino_adk/eino_adk_chatmodelagentmiddleware/middleware_summarization)、[A2UI 官方示例说明](https://www.cloudwego.io/docs/eino/quick_start/chapter_10_a2ui_protocol)、[消息结构](https://github.com/cloudwego/eino-examples/blob/main/quickstart/chatwitheino/a2ui/types.go)、[示例数据绑定参考](https://github.com/cloudwego/eino-examples/blob/main/quickstart/chatwitheino/static/index.html)。保留已固定的 Eino 示例来源提交与 Apache 许可；页面视觉组件改用 BeautifulUI，不依赖浮动 main。
- BeautifulUI 来源：[组件展示与 View code](https://www.beautifului.dev/)、[MIT 许可证](https://www.beautifului.dev/license)。2026-09-29 已检查 Approval Card 的 View code：使用 React hooks、TypeScript，并引用站内 `@/components/atoms/Button`、`@/components/primitives/GlideMenu`；不是已核实的独立 npm 组件包。MIT 版权为 `Copyright (c) 2026 Shane Levine`。其他组件的完整依赖闭包须在 Step 19.1 按源码逐一核实，不能推定全部仅依赖 React。
- 本次修订只改变前端组件来源、构建与验收方式；Steps 1–17 的业务范围和既有待办 ID、状态保持不变。Step 1 的已完成框架证据继续保留，BeautifulUI 来源与适配验证属于尚未完成的 Step 19。

## 三、组织与执行规则

**拟新增包与文件职责：**
- [cmd/web/main.go](D:/Code/owner_agents/seasprak/cmd/web/main.go)：参数、受信装配、启动和退出。
- [internal/web](D:/Code/owner_agents/seasprak/internal/web)：服务、认证、DTO、路由、SSE、A2UI 投影及嵌入静态文件；初期同包分文件，不拆 controller/service/repository 多层。
- 拟新增 `web/`：React/TypeScript 前端源码、BeautifulUI 本地组件及许可证、固定版本 npm 工具链与浏览器测试；不承载 Go SDK 或服务端业务。Vite 输出到 `internal/web/static/`，后者只存可复现发布资源，不手工维护第二套页面实现。
- [internal/sessions](D:/Code/owner_agents/seasprak/internal/sessions)：继续拥有受理、调度、历史维护、会话目录及事件交接。
- [internal/sessions/state](D:/Code/owner_agents/seasprak/internal/sessions/state)：继续拥有一致提交、回放、幂等与版本验证，不依赖 Eino 或具体存储。
- [internal/agent](D:/Code/owner_agents/seasprak/internal/agent)：上下文、摘要候选、执行目标与节点/子调用端口；Eino 实现留在 [internal/agent/eino](D:/Code/owner_agents/seasprak/internal/agent/eino)。

所有后文“新增”文件均为拟交付，不表示已存在。每步先写对应包默认套件中的失败测试，再实现，再执行指定包完整测试；涉及并发先执行受影响包 race。每个批次另跑文末完整检查。测试证明状态、错误码及实际模型/工具/存储次数，不仅检查文字。

本次只制定计划，不编辑代码、运行测试、提交或推送。实施时也不自动提交，提交须另行明确授权。

## 批次 A：固定契约与可启动服务

### Step 1: 固定 P3 契约和上游复用边界
- **前置：** 本计划获批；保留未提交 P2 变更。
- **目标与输入：** 上述设计文档、`go.mod`、`internal/architecture`、锁定版 Eino 源码、A2UI 示例。
- **方法：** 以当前代码为基线，先做小范围框架适配测试，禁止通过依赖升级或削弱规则规避缺口。
- **顺序：** ①同步 cmd/web、A2UI 和前移范围；②固定 HTTP 路由/DTO、审批实例回执例外、游标和稳定 ID；③记录 A2UI 来源提交和 Apache 许可；④在 `internal/agent/eino` 新增摘要/图恢复探针，实际验证 Agentic 泛型、自定义 Finalize、显式 Summarize、取消和图节点恢复。
- **约束：** 摘要 Callback 不承担落库；默认摘要重试/failover 不与产品预算重复叠加；示例服务器不替换产品执行路径。
- **失败处理：** 探针失败形成精确缺口和方案，再确认适配方式；不带着未验证框架语义进入依赖步骤。
- **产物：** 更新后的范围/协议说明、固定来源记录、框架契约测试。
- **验收：** `go test ./internal/architecture ./internal/agent/eino -count=1`；证明摘要不会提前替换持久历史、节点已有结果恢复不重复执行。上游测试不能代替项目测试。

### Step 2: 新增安全的 cmd/web 启动与停止
- **前置：** Step 1。
- **目标与输入：** 新增 `cmd/web/main.go`、`internal/web/server.go`、`config.go`；复用现有 LLM Catalog、会话 Options 和错误码。
- **方法：** 标准库 `flag`、`net/http`、`embed`；入口仅装配。启动用 `go run ./cmd/web --web --workspace <显式目录> --state-root <目录> --config <受信配置>`。
- **顺序：** ①帮助/版本不启动服务；②校验工作区、受保护状态目录、回环监听和启动配置；③解析获准模型/工具引用，密钥仅由宿主凭据源解析；④启动 HTTP；⑤进程退出时停止接纳，按既有 Close 语义安置会话并释放订阅。
- **约束：** 不自动读取 `.test_env` 作为 Web 配置；不默认使用 cwd；不开放任意后端/Go 插件/挂载上传；不把 `ProfileDefault` 尚不可执行的能力标记可用。
- **失败处理：** 缺配置、监听失败、凭据引用缺失时脱敏退出；启动未完成不暴露半就绪服务；关闭超时如实报告。
- **产物：** 可运行的独立入口、清晰配置错误和生命周期管理。
- **验收：** `go test ./cmd/web ./internal/web ./internal/architecture -count=1`；子进程验证未带 `--web` 不监听、帮助无副作用、非回环拒绝、端口占用和关闭释放。保留 agentd 原架构测试。

### Step 3: 建立统一认证、授权和网络 DTO
- **前置：** Step 2。
- **目标与输入：** 新增 `internal/web/auth.go`、`dto.go`、`errors.go`；现有产品错误与公开消息投影。
- **方法：** 启动生成随机 bearer，存入受保护的本地凭据文件，仅提示路径；浏览器手工输入后只保留内存。统一中间件映射单一本地 Principal。
- **顺序：** ①验证 token、Host 和存在时的 Origin；②统一 Session/附件/事件访问授权；③严格 lowerCamelCase DTO、未知字段拒绝、单次 JSON 4 MiB 限制；④用白名单映射快照和错误。
- **约束：** CORS 默认关闭；秘密不进 URL、localStorage、日志或页面源码；不接受 body Principal、system/assistant/tool-result、provider Extra、授权引用或 Eino interrupt 地址。
- **失败处理：** 认证失败不读取会话内容；沿用 400/401/403/404/409/410/422/503/500 及现有产品码；响应已受理的执行错误留在 operation/trace。
- **产物：** 所有路由共享的访问边界和稳定网络投影。
- **验收：** `go test ./internal/web -count=1`；缺/错 token、DNS rebinding Host、跨源请求、未知字段、超限、私有元数据泄漏、工具 HTML 文本输入；各拒绝路径模型/工具次数均为零。

## 批次 B：会话目录、HTTP 命令与事件可靠性

### Step 4: 实现只读会话目录和创建幂等
- **前置：** Steps 1–3。
- **目标与输入：** `internal/sessions/create.go`、`open.go`、`store` 接口与 jsonl/memory 后端；新增 `internal/sessions/catalog.go` 及相应后端登记文件。
- **方法：** 目录索引可重建，不作为会话事实；创建幂等使用持久登记，不使用 HTTP 内存 map 代替。
- **顺序：** ①只读枚举获准 stateRoot 下 header；②创建前按 Principal/key 保存规范化摘要和预分配 sid；③用固定 sid 创建；④完成登记并返回原回执；⑤进程重启核对 pending 登记与已存在会话，绝不创建第二个 sid。
- **约束：** GET 列表/浏览不调用可写 Open，不修改工作区绑定、不修复日志、不恢复执行；目录中损坏会话返回不可用元信息。
- **失败处理：** 同键异内容冲突；创建/Sync/登记/响应间失败后以原 sid 核对；索引缺失可重建，幂等记录不作为可随意删除的缓存。
- **产物：** POST/GET sessions、会话元信息查询及创建幂等。
- **验收：** `go test ./internal/sessions/... ./internal/web -count=1`；独立进程重试、两个并发创建、响应丢失、陈旧目录、只读浏览的存储写入与执行次数为零。

### Step 5: 补齐持久取消与继续队列操作
- **前置：** Step 4。
- **目标与输入：** `internal/sessions/coordinator.go`、`state/operations.go`、`state/inputs.go`；新增命令请求类型并在 `sdk/sdk.go` 别名导出。
- **方法：** 新增带 key/revision 的受理入口；保留现有 `Cancel(ctx,string)`、`ContinueQueue(ctx,string)` 兼容行为，复用其内部状态转移，不复制执行逻辑。
- **顺序：** ①在 mailbox 内先查重、验证目标和版本；②同 commit 保存 accepted operation 与取消意图/选中队列 hold 变化；③提交后才取消 worker 或调度；④真实退出/生效后保存最终结果。
- **约束：** 批量 continue 全部验证后原子提交；不抢占；HTTP 断连不取消已接纳 Trace；取消受理不等于已停止。
- **失败处理：** 提交失败零调度；重复 key 返回原操作；已终态重复取消返回已有状态，不再产生执行；响应丢失可查询原 operation。
- **产物：** HTTP 可用的 durable OperationReceipt 及 SDK 兼容入口。
- **验收：** `go test -race ./internal/sessions ./internal/sessions/state -count=1`；取消与自然结束竞争、批量部分非法、版本冲突、提交失败和重启回执；断言一次 settled 和实际启动次数。

### Step 6: 接通输入、审批、恢复、核对与选择路由
- **前置：** Steps 3–5。
- **目标与输入：** `session.go`、`interactions.go`、`resume.go`、`reconcile.go`、`selection.go`；新增 `internal/web/routes.go`、`inputs.go`、`operations.go`。
- **方法：** handler 只做 DTO/授权和调用；正文转换在产品输入接纳路径完成；复用已有持久幂等摘要规范化。
- **顺序：** ①将类型化 text 块转换为真实用户消息而非 JSON 文本；②映射现有 SubmitInput/Resume/选择/GetOperation；③审批从路由 interactionId 查保存目标；④核对从原调用解析 grant 等内部字段；⑤将已受理核对与取证完成分离，202 返回真实回执。
- **约束：** 审批仍为实例内许可，网络回执标明实例作用域，`acceptedCommit=0` 不伪装持久提交；旧实例应答不可恢复批准。工作流缺参直接拒绝，尚不提供 answer 补参。
- **失败处理：** 已有 receipt 的取证错误写 operation，不丢受理事实；同键对象键顺序等价，数组顺序保留；重新连接不会自动重发新输入或 Resume。
- **产物：** inputs、trace 查询/取消/恢复、交互回复、队列、核对、operation、模型/工具选择路由。
- **验收：** `go test ./internal/sessions/... ./internal/web ./sdk/... -count=1`；实际 HTTP 请求、进程重开、输入类别/目标错误、审批过期/重问、原工具调用不重跑及失败响应脱敏。

### Step 7: 实现固定交接点的重放订阅
- **前置：** Step 5；与 Step 6 可在契约确定后独立实施。
- **目标与输入：** `internal/sessions/events.go`、`state/events.go`、`coordinator.go`；新增 `event_replay.go` 及测试，SDK 保留旧订阅入口。
- **方法：** Session 层新增带 context/cursor 的重放入口，一次 mailbox 操作固定交接点 B、临时视图和有界实时订阅；网络不得直接访问 Manager。
- **顺序：** ①验证 K；②登记仅接收 B 后持久事件的实时缓冲；③离开 mailbox 后分页读取 `(K,B]`；④发送交接视图；⑤交付排队实时事件。游标是事件 durableSeq，不是 commit revision。
- **约束：** 重放不改变全局发布 cursor；每页限制条数/字节；不得将全部历史放入实时队列；保留原 eventId。
- **失败处理：** 注册途中取消必须清理订阅；重放中溢出返回 resync_required 并关闭；未来/异会话/损坏 cursor 拒绝；默认日志全保留但声明 earliestReplayCursor。
- **产物：** 可取消、无交接遗漏的 Session 重放接口。
- **验收：** `go test -race ./internal/sessions ./internal/sessions/state -count=1`；通道屏障覆盖固定 B 前后提交、多页写入、取消与关闭、无事件 commit、旧 cursor 和订阅泄漏；正常消费者不受慢消费者影响。

### Step 8: 补齐临时聚合与可靠 SSE 传输
- **前置：** Step 7。
- **目标与输入：** `events.go`、`execution.go`、`internal/config/limits.go`；新增 `internal/web/cursor.go`、`sse.go`。
- **方法：** mailbox 保存最新公开模型 snapshot 与有界工具输出预览；HTTP 仅编码。采用版本化且绑定 Session 的不透明 cursor。
- **顺序：** ①模型 snapshot 替换而非重复追加；②按工具调用/stream 保存带截断标记的输出；③终态提交成功后清理；④SSE 先重放、交接快照、再实时；⑤15 秒心跳及每次写入期限，不设长连接总 WriteTimeout。
- **约束：** 聚合有每流及会话总量上限，值在 config/协议说明中统一确定并有边界测试；临时事件无 SSE id；trace 过滤使用全局 cursor；Last-Event-ID 与 query 不一致拒绝；重启重置临时显示。
- **失败处理：** 写失败/EOF 只关闭订阅，不取消任务；无法发送 resync 帧时直接关闭，客户端重新取快照；迟到 chunk 不复活终态。
- **产物：** `/snapshot` 和 `/events` 的一致临时视图、可靠重连及流量限制。
- **验收：** `go test -race ./internal/sessions ./internal/web -count=1`；真实慢客户端、交接时 finalized、UTF-8 边界、多路工具输出、重启及断连，实际模型/工具调用次数不增加。

## 批次 C：历史分支与 Eino 压缩接线

### Step 9: 实现历史树及分支操作
- **前置：** Steps 1、5。
- **目标与输入：** `state/replay.go`、`types.go`、`messages.go`、`inputs.go`、Store BranchUpdates；新增 `state/history.go`、`state/branches.go`、`sessions/branches.go`。
- **方法：** 基于已有稳定 Entry ID/parent 保存完整索引，按选定祖先路径构建视图，不再用全日志消息数组充当当前分支。
- **顺序：** ①兼容重建旧线性历史；②保存命名 branch head/active leaf；③只读查询与稳定分页；④空闲条件检查后 Fork/Navigate；⑤从旧位置受理新输入时同 commit 建立新分支；⑥先恢复路径配置再投影消息。
- **约束：** 浏览零写入；导航不回滚工作区文件；活动/待恢复/队列/未决写效果冲突时拒绝；消息游标不用数组下标。
- **失败处理：** 非祖先/不存在目标拒绝；提交失败原游标不变；旧分支后缀保持可选回；坏路径不静默忽略。
- **产物：** ListMessages/ListBranches/ForkBranch/NavigateBranch 与 HTTP 路由。
- **验收：** `go test ./internal/sessions/... ./sdk/... -count=1`；捕获实际模型输入证明无跨分支消息；重开身份一致、旧格式可读、分页稳定和失败原子性。

### Step 10: 固定唯一上下文与完整请求预算
- **前置：** Step 9。
- **目标与输入：** `internal/agent/projection.go`、`ports.go`、`sessions/execution.go`、Eino model 边界及各 LLM 最终请求构造；新增 `context.go`、`context_budget.go`。
- **方法：** 实际使用 ScopeSnapshot，区分历史投影 revision 与无关控制提交；复用现有选项解析和模型调用端口。
- **顺序：** ①固定 branch/leaf/invocation/generation/selection；②按路径取得已消费消息；③转换副本，保留完整工具组；④组装 system/schema/媒体与输出预留；⑤计算有来源版本的保守估算；⑥最终 payload 扩展后复核。
- **约束：** 不使用统一四字符/token 假设；未知必需媒体成本拒绝；pending 不入模型；动态 ID 不污染稳定 system；Skills 发现不在此步扩张，但已有资源版本引用不可丢失。
- **失败处理：** 固定内容超窗直接返回预算错误；转换/配对/引用错误时物理请求为零；模型变小后重算，不能复用过期 usage 基线。
- **产物：** 所有普通/子调用/摘要共享的上下文预算契约与投影范围。
- **验收：** `go test ./internal/agent/... ./internal/llm/... ./internal/sessions -count=1`；fake transport 捕获最终请求，验证中文/emoji/schema/输出预留/媒体/无关 revision 变化。

### Step 11: 通过 Eino 中间件生成并验证摘要候选
- **前置：** Steps 1、10。
- **目标与输入：** 新增 `internal/agent/compaction.go`、`file_facts.go`、`internal/agent/eino/compaction.go`；现有工具观察与 LLM 计量。
- **方法：** `NewTyped[*schema.AgenticMessage]` 配自定义 TokenCounter/GenModelInput/Finalize，显式 Summarize 处理候选副本；产品不另写生成循环。
- **顺序：** ①按合法工具组划分 H（旧历史）、P（当前请求已完成前缀）、K（保留原文）；②主摘要与必要前缀依次生成；③验证设计规定标题/来源范围；④从确认的工具 FileFact 合并文件附录；⑤重建完整请求并复核预算。
- **约束：** 摘要通过有 purpose 的模型计量端口，不伪造普通助手/Turn；禁业务工具；默认 failover 关闭、重试受原预算限制；Finalize 自定义后显式负责保留要求，不假定上游默认处理仍生效。
- **失败处理：** 无新增范围返回 no_op；任一摘要部分失败不返回可激活半候选；unknown/denied 不算成功文件修改；不从 shell 文本猜文件事实。
- **产物：** 可测试的摘要候选与确定性文件附录。
- **验收：** `go test ./internal/agent/... ./internal/llm/... -count=1`；首次/增量/H 为空/工具切点/标题缺失/预算超限/模型失败/用量计数，必须实际调用新增适配函数。

### Step 12: 提交压缩、自动触发与跨分支恢复
- **前置：** Steps 9–11。
- **目标与输入：** 新增 `sessions/compaction.go`、`state/compaction.go`；修改 execution 安全边界、resume 校验及历史投影。
- **方法：** mailbox 固定范围，模型调用在 mailbox 外，候选返回后验证 leaf/projection/generation，再同 commit 保存摘要 Entry、details、投影关联和事件。
- **顺序：** ①手动入口持久受理；②软阈值和获认证的 overflow 回调同一服务；③等待审批仅登记意图，恢复原批次后处理；④重建共同祖先摘要及近期原文；⑤显式请求的分支摘要仅取旧路径独有后缀。
- **约束：** 不再额外挂第二个自动压缩 middleware；按原 30 分钟/调用预算和压缩次数限制计量；一个 overflow 恢复必须有新的已提交投影；子调用只修改自己范围。
- **失败处理：** 取消/导航/相关历史改变使候选失效；提交失败保留旧投影；旧投影仍满足硬预算才允许继续；提交成功后取消不撤销事实。
- **产物：** 真实 Compact 操作、自动安全边界接线、分支继承与恢复。
- **验收：** `go test -race ./internal/sessions/... ./internal/agent/eino -count=1`；候选生成/提交前后进程中断、连续压缩、共同祖先/兄弟隔离、取消及审批等待，断言无半摘要和无副作用重跑。

## 批次 D：启动时注册的多 Agent 与工作流

### Step 13: 固定启动注册清单与真实目标路由
- **前置：** Steps 1、9、10。
- **目标与输入：** `sessions/options.go`、manifest/create/execution/resume、`state/inputs.go`；新增 `agent/registry.go`、`sessions/agents.go`，SDK 别名。
- **方法：** 启动构建不可变注册清单，包含 main、自定义受控 Agent 和工作流定义/绑定的名称、版本与 hash；不实现运行中 reload。
- **顺序：** ①校验重复名称、schema、恢复声明；②受理前从注册清单解析 target；③与 input/trace 原子保存目标版本；④执行/排队/Resume 只解析保存版本；⑤capabilities 列出真实可执行目标。
- **约束：** main 保持现有默认；定向输入省略目标继承，显式错配冲突；未知目标不回退 main；HTTP 不上传可执行 Go 实现。
- **失败处理：** 重启缺同版本实现时 incompatible_resume，历史仍可浏览；不得使用同名新版替代；注册失败不启动半套服务。
- **产物：** 启动多目标装配、能力查询、实际执行选择。
- **验收：** `go test ./internal/sessions/... ./internal/agent/eino ./sdk/... -count=1`；两个不同 fake Agent 的实际调用计数、忙时 prompt 队列、目标省略/错配、重开版本变化拒绝。

### Step 14: 建立父子调用作用域与受控委派
- **前置：** Steps 10、13。
- **目标与输入：** `agent/budget.go`、执行端口、sessions execution/CommitFact/取消；新增 `agent/invocation.go`、`sessions/invocations.go`、`state/invocations.go`、`eino/subagents.go`。
- **方法：** 复用 Eino TypedAgent/Agentic 执行，产品 task 工具先进入既有受控工具管道；不能直接开启框架自动 task 后假定已受控。
- **顺序：** ①持久父调用与子 invocation；②分离每次模型调用计数槽位，共享总预算；③显式传允许上下文；④子事实按自身 scope 提交；⑤子结果合并为唯一父工具结果；⑥递归取消、暂停和恢复。
- **约束：** 默认并发/嵌套均按现有工程值 4；子不消费父 steering、不读取父私有历史；委派不持父写锁；不合作受信 Go 实例不得声称可强杀/已沙箱认证。
- **失败处理：** 中断保持原父子身份；controlled stop 归一为正常内部结束，真正错误保留；unknown 效果不重跑；权限撤销即时生效。
- **产物：** 可用的子 Agent 委派及父子预算/恢复隔离。
- **验收：** `go test -race ./internal/agent/... ./internal/sessions/... -count=1`；并发子请求预算、父取消、嵌套边界、子压缩隔离、恢复实际工具次数不增加。

### Step 15: 校验静态工作流并编译 Eino 图
- **前置：** Steps 1、13。
- **目标与输入：** 新增 `agent/workflow.go`、`workflow_validation.go`、`eino/workflow.go`；启动配置绑定。
- **方法：** 启动时加载统一声明式定义或已编译受信包装，不读取 Coze 原始导出、不支持浏览器动态导入。统一定义编译到锁定版 Eino compose 图。
- **顺序：** ①验证全部原始节点，包括孤立节点；②校验唯一 ID、类型、引用、分支路径、唯一开始/结束、无环与固定版本子流程依赖；③解析本地模型/工具绑定；④完整输入 schema 校验后才执行；⑤登记 WorkflowAgent。
- **约束：** 首批开始/结束、字面量/输出引用、模型、条件、受控工具及固定子流程；代码/任意 HTTP/循环/批处理/平台变量拒绝；缺参不调用主模型猜测。
- **失败处理：** 定位节点/边/字段的安全诊断；未知节点即使未连线也拒绝；非法定义不出现在可选清单。
- **产物：** 真正可执行的静态 WorkflowAgent、定义/schema 查询。
- **验收：** `go test ./internal/agent/... -count=1`；非法类型/拓扑/绑定全部零模型与零工具调用；有效分支不读取未执行路径的输出。

### Step 16: 工作流节点接入许可、持久结果及恢复
- **前置：** Steps 14、15。
- **目标与输入：** `sessions/tool_execution.go`、`execution.go`、`agent/tools/execution.go`、审批恢复；新增 `state/workflow.go`。
- **方法：** 新增真正 workflow_node 受信路径；由固定定义解析节点与绑定，持久分配 nodeExecutionId→toolCallId，复用冻结/授权/预算/claim/票据/结果核对。
- **顺序：** ①保存节点接纳事实；②通过完整安全管道执行；③结果保存后回填 Eino 节点；④实现 Interrupt/Resume 产品关联；⑤同一 WorkflowAgent 接通独立 target 与父 task 两种入口。
- **约束：** 无模型节点不伪造 Turn、providerCallId 或 FunctionToolResult；独立工作流不调用主模型路由；缺业务参数装配/受理时拒绝，工具审批仍可正常询问。
- **失败处理：** 部分节点成功后失败保留进度；已有结果直接复用；claim 后效果未知先核对；旧许可不随检查点恢复。
- **产物：** 安全可恢复的工作流节点与两种执行方式。
- **验收：** `go test -race ./internal/agent/... ./internal/sessions/... ./sdk/... -count=1`；审批/取消/节点结果提交窗口、进程重启、两个入口 schema 一致，断言已完成节点实际执行一次、独立入口主模型次数为零。

## 批次 E：BeautifulUI 页面、A2UI 适配与完整 Web 接线

### Step 17: 补齐附件、历史和维护查询接口
- **前置：** Steps 6、9、12、13、16。
- **目标与输入：** 现有产物后端端口；新增 Session 授权产物查询与 `internal/web/artifacts.go`、`maintenance.go`。
- **方法：** 用户附件/下载采用 Session 授权，不伪造模型工具执行票据；ID 解析到受控对象，不当作路径。
- **顺序：** ①定义受限附件保存请求并固定独立大小/数量限额；②校验 MIME/引用归属，成功保存才发布 artifactId；③接入获准模型多模态转换；④提供授权 range 读取；⑤接线 branches/compactions/capabilities/workflows、名称标签及稳定消息分页。
- **约束：** 失败不返回可读引用；不暴露 stateRoot/检查点/秘密；附件不触发模型；工作流定义仅显示获准信息；浏览器换会话只换观察目标，不自动取消、Resume 或更改绑定。
- **失败处理：** 内容缺失/变化明确错误；不支持模态提前拒绝；无后端时能力查询标明不可用，不能返回假成功；下载失败不重跑工具。
- **产物：** 页面所需真实查询和维护 API；不提供宿主 shell、热重载、动态工作流导入和业务补参入口。
- **验收：** `go test ./internal/sessions/... ./internal/web -count=1`；跨 Session 引用、路径穿越、非法 range、UTF-8/二进制边界、超限、内容变更及维护冲突。

### Step 18: 实现产品事件到 A2UI 的纯投影
- **前置：** Steps 7、8、17。
- **目标与输入：** 新增 `internal/web/a2ui.go`、`a2ui_projection.go`；固定来源的 Eino 示例消息结构。
- **方法：** 复用 beginRendering/surfaceUpdate/dataModelUpdate/deleteSurface 与 Text/Column/Card/Row；输入只接公开 Snapshot/产品事件，不直接消费 Eino reader。前端通过本地白名单渲染适配器将这些节点与已定义的产品扩展映射到 React/BeautifulUI 组件；BeautifulUI 组件名、JSX、CSS 或回调代码不进入服务端协议，不根据模型文本自动选择可执行控件。
- **顺序：** ①以 Session/entry/message/call 稳定 ID 生成组件；②历史和重连生成可覆盖的 surface；③模型临时快照更新数据绑定，最终消息替换临时内容；④审批扩展使用产品 interactionId，不暴露 Eino interruptId；⑤业务完成只依据持久 settled。
- **约束：** UI 不是业务真相或审批权威；保持原 `/events` 产品事件契约，另设 A2UI 呈现订阅端点，共用同一重放入口；一条 durable 事件映射多个 UI 帧时只在完整帧组末推进 cursor，帧组重放幂等。
- **失败处理：** 未知组件/非法结构拒绝；投影失败只影响显示；断线半组从原 cursor 重放不会重复卡片；临时事件不推进持久 cursor。
- **产物：** 可重建、可重放的 A2UI 服务端投影及子集兼容说明。
- **验收：** `go test ./internal/web -count=1`；黄金消息结构、完整/半组重放、稳定 ID、终态不复活、审批映射和私有字段零泄漏。

### Step 19: 使用 BeautifulUI 组件实现页面并接入 A2UI 与产品操作
- **前置：** Step 18；BeautifulUI MIT 许可与 Approval Card 的 React/TypeScript 用法已核实，其他所选组件须通过下述源码依赖检查。
- **目标与输入：** 拟新增 `web/package.json`、`package-lock.json`、`tsconfig.json`、`vite.config.ts`、`index.html`；`web/src/main.tsx`、`App.tsx`、`styles.css`；`web/src/components/beautifului/`；`web/src/a2ui/{protocol.ts,store.ts,renderer.tsx}`；`web/src/api/{client.ts,sse.ts}`；`web/src/features/{sessions,chat,approvals,operations,history,workflows}/`。新增 `web/THIRD_PARTY_NOTICES.md`、`web/licenses/beautifului-MIT.txt`、组件与协议 `*.test.ts(x)`、`web/tests/*.spec.ts`。Go 在 `internal/web/static.go` 嵌入 `internal/web/static/` 构建产物并保留默认套件中的资源/路由测试。以上均为拟新增路径，不表示已落地。
- **方法：** 按需复制 BeautifulUI 实际源码及其必要原子组件到本地，保留许可后改造成由 props 驱动的受控组件；用 React 文本渲染、独立 API 控制层和 A2UI 白名单适配器接线。采用 Vite 静态构建，不引入 Next.js 服务端。每个子步骤先写失败测试，禁止把网站演示定时器、模拟请求或默认问题带入真实业务。
- **顺序：**
  1. **19.1 来源与依赖固定：** 在网站 View code 逐一读取 Chat、Prompt Bar、Streaming Text、Loading State、Thinking、Tool Chips、Task Rows、Approval Card 和所需 Code Block；追踪每个 import 的 Button、GlideMenu、样式、字体等依赖。来源清单记录组件名称、URL、获取日期、原始源码 SHA-256、许可、依赖与本地改动；有稳定上游提交则同时记录。保存 MIT 原文与版权，Eino Apache 声明单独保留。依赖源码无法取得或许可不清时停止该组件接入并报告，不能编造依赖或静默改为仅仿外观。
  2. **19.2 构建与组件边界：** 先验证源码可用及依赖兼容，再在 `package.json` 固定 React、React DOM、TypeScript、Vite、Vitest、Testing Library、Playwright 的精确版本，提交 lockfile，并记录兼容的 Node/npm 版本。仅增加实际所需依赖，不预设 Tailwind 或动画库。建立 `typecheck`、`test`、`build`、`test:e2e` 脚本，Vite 输出到 `../internal/web/static`；此目录仅为前端构建产物，构建前不得清理其他 Go 文件。发布资源纳入版本管理以保证干净检出可直接 `go build`，CI 重新构建并检查一致性；不提交 node_modules、测试报告、浏览器缓存或含本机路径的 source map。打包第三方声明，服务运行期不加载远程字体、脚本或演示图片。
  3. **19.3 协议适配：** `protocol.ts` 定义 Step 18 子集，`store.ts` 按 surface/组件稳定 ID 覆盖更新，`renderer.tsx` 只渲染固定组件注册表。Text/Column/Row/Card 映射到安全文本与布局；已定义的消息、工具、任务、审批扩展映射到 BeautifulUI 视图。未知节点显示安全错误且无可执行动作，不动态 import/eval。先以协议样例测试完整帧组、半组重放、重复帧、删除 surface、临时消息替换及迟到事件，再接 fetch SSE；只有完整持久帧组才更新 cursor。
  4. **19.4 组件与页面接线：** Chat 提供会话对话布局，Prompt Bar 提供输入与获准模型选择，Streaming Text 展示实际服务端文本，Loading State 展示真实等待，Tool Chips 展示受权工具调用，Task Rows 展示队列与 operation。Thinking 只展示公开进度/阶段摘要，不显示私有推理或编造思考过程。Approval Card 改为明确的批准/拒绝确认，仅绑定服务端 interactionId；移除演示问题、选中自动推进并授权、Skip 默认许可和本地“提交即成功”状态。Code Block 如使用仅安全展示公开代码/差异，不提供执行。页面中文文案，复用组件配色、间距和交互样式，支持明暗主题、窄屏、键盘焦点、可访问标签及减少动画偏好。
  5. **19.5 产品控制与网络恢复：** API client 统一注入仅内存 bearer；会话/Agent/工作流选择仅来自 capabilities，工作流提供完整 schema 参数表单。接入取消、继续、核对、分支、压缩、审批和恢复资格显示；操作由受信 controls 发起，不由模型文本生成授权。重连先取快照再按契约 cursor 订阅，响应不确定时沿用原幂等 key 查询/重试；换会话取消旧观察连接并隔离迟到响应，不自动 Resume 或取消业务。Prompt Bar 中尚未实现的语音、任意 @ 资源、斜杠命令入口隐藏，不展示假能力；不引入工作流编辑器或业务补参问答。
  6. **19.6 默认与浏览器验证（2026-09-30 用户最新决定）：** Vitest/Testing Library 继续使用默认离线样例覆盖受控组件和协议适配；所有 Playwright 浏览器 E2E 必须使用真实 Chromium、本机真实 `cmd/web` Go 服务及持久存储、根目录 `.test_env` 配置的真实模型。禁止假模型服务或模拟模型输出，天然无模型节点的工作流可以零调用验收。测试只通过子进程环境把凭据传给 Go，启动 JSON 保存引用，配置值不得出现在聊天/日志/截图。真实外部模型请求只由用户批准的服务端透明代理转发；代理只计数及重分块/暂停真实字节，不生成或修改模型输出。浏览器只访问本机服务，模型与工具实际计数由测试端断言。对桌面/窄屏、明暗主题做浏览器检查，验证必要组件确实来自已记录源码且无演示数据。Go 资源测试检查嵌入首页、带 hash 资源、MIME、无目录遍历和无秘密；静态资源服务不能削弱 API 认证。
- **约束：** 不从 CDN 加载浮动脚本，不用 `dangerouslySetInnerHTML` 直接呈现模型/工具内容；首版文本安全渲染，代码同样转义。链接仅允许经验证的安全协议。浏览器不直连模型，不存 token 到 URL/localStorage；UI store 只是显示缓存，不成为第二个业务状态机。动画完成、文本打字结束或组件本地 sent 状态均不等于业务完成。无 shell 操作入口。
- **失败处理：** 缺源码/依赖/许可时停止对应接入，记录原因；构建失败不得用空白占位页交付。JSON/SSE 错误显式重同步，不吞异常；浏览器中止只影响观察。取消受理后显示 cancelling，持久终态到达才显示 stopped；审批过期/旧实例/重复点击显示真实回执并刷新，不自动批准。无权限或缺能力按钮禁用/隐藏但服务端仍复核。提交失败保留用户输入，重试不生成新的业务请求身份。
- **产物：** 基于 BeautifulUI 真实组件的本机 React 页面、A2UI 适配层、完整源码/许可清单、可复现嵌入产物与自动化测试；包含前移功能真实操作路径，不声称覆盖 BeautifulUI 全部组件。
- **验收：** 从 `web/` 执行 `npm ci`、`npm run typecheck`、`npm run test -- --run`、`npm run build`、`npm run test:e2e`，全部通过并记录工具版本。浏览器首次需按锁定 Playwright 版本准备对应浏览器，准备阶段可下载依赖；浏览器测试与运行时只访问本机服务，用户批准的真实模型外部请求仅由服务端代理转发。用例覆盖跨网络块 UTF-8/SSE、完整/半组重连、响应丢失、切换会话迟到帧、XSS/危险链接、审批重问与一次实际提交、工作流输入、分支/压缩、键盘和窄屏。根目录执行 `go test ./internal/web ./cmd/web -count=1`，静态资源测试继续进入 `go test ./...`；前端测试不能代替完整 Go 验证。

## 批次 F：联合验收与交付记录

### Step 20: 执行故障、平台与端到端验收
- **前置：** Steps 1–19 各自验收通过；已知失败不得只靠重跑消除记录。
- **目标与输入：** `internal/web`、Session/状态/SDK consumer 测试、架构测试、`web/` 前端与浏览器测试、BeautifulUI 来源/许可清单；新增 P3 验证记录并更新 requirements-coverage。
- **方法：** 默认测试使用 testkit、临时目录与可控 transport；真实多进程与实际浏览器补充验证，不能用 fixture 证明沙箱或真实供应商能力。
- **顺序：** ①创建/accepted/响应丢失与重启；②SSE 重放交接、慢客户端、临时恢复；③取消/审批/核对；④分支与压缩提交窗口；⑤独立 Agent/子委派/工作流恢复；⑥Windows/Linux/macOS 实际运行；⑦完整仓库检查与 live；⑧逐条记录范围及残余缺口。
- **约束：** 临时流、网络响应和 A2UI 完成标记都不替代持久事实；浏览器/SDK 走同一业务路径；原 SDK 消费者保持兼容，批准的新增类型仍集中 sdk.go。
- **失败处理：** 缺平台/工具/服务时记录未运行、退出码和输出摘要并交维护者决定；不可将 cross-build 记作 runtime PASS；不自动提交或推送。
- **产物：** P3 可运行版本、启动使用说明、A2UI 子集和 HTTP 契约、可追溯验证记录、重新划分后的 P4/P5 剩余清单。
- **验收：** 下列命令全部执行并记录结果，新增文件另检查秘密、冲突标记及禁止产物。

## 四、完整验证门槛

从仓库根目录执行：
- `gofmt -l .`：无输出，否则格式化后复查。
- `go vet ./...`。
- `go build ./...`。
- `go test ./...`。
- 并发修改先运行受影响包 `go test -race`，再执行 `go test -race ./...`。
- `govulncheck ./...`；未安装时用已固定版本 `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`，记录替代命令。
- `git diff HEAD --check`，并扫描新增文件。
- `.test_env` 存在时执行 `go test -tags live ./internal/llm -count=1`，只由测试读取；缺失记录跳过。
- SDK 外部 consumer 若其目录不被默认 `./...` 纳入，按现有装配方式单独运行完整 consumer 套件，不能漏报。
- 前端在 `web/` 执行 `npm ci`、`npm run typecheck`、`npm run test -- --run`、`npm run build`、`npm run test:e2e`；记录锁定 Node/npm/浏览器版本，检查重新构建后 `internal/web/static/` 与交付资源一致，新增/删除资源也必须纳入检查。前端单元测试离线运行，浏览器 E2E 采用 Step 19.6 的真实模型规则，仅允许服务端代理的获准模型外部请求；审查 BeautifulUI 源码来源、依赖许可及发布包中的版权声明。
- 实际浏览器端到端测试及三平台服务启动/监听/关闭/目录保护测试；工具或平台缺失时保留未验收状态。

## 五、实施依赖与阶段出口

- A → B 提供安全、可运行的基础 HTTP/SSE；此时可演示当前 main，但不宣布扩展版 P3 完成。
- C 构建历史/压缩；D 构建静态多目标/工作流。共享的上下文、版本、invocation 与预算契约先固定，不能让不同实现各自定义。
- E 在真实接口之上实现 A2UI 协议适配和 BeautifulUI 组件页面；F 统一验收。
- 完整 P3 出口：通过 `cmd/web --web` 启动后，用户能创建/浏览会话、选择已注册 Agent 或工作流、提交任务、观察与重连、审批/取消/恢复/核对、导航分支和压缩；这些动作均有实际执行与持久恢复证据。
- P4 剩余包括完整资源/Skills 发现加载及未覆盖的上下文工程；P5 剩余包括动态导入/Coze 子集、补参、热重载/多 generation 生命周期与通用扩展 hooks。阶段重划以逐条覆盖为准，不将前移部分冒充整阶段完成。