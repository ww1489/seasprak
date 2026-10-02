# SDK 与业务层的职责边界

## 2026-10-01 维护者确认的分层

SDK 按模型、通用执行和上层 Agent 三层组织，Web 是外侧业务应用。上层保留两类同级能力：`seasprak-code-agent` 负责代码助手会话，`seasprak-workflow-agent` 负责声明式工作流。两者独立运行与持久化，普通会话不再内置工作流调度。

该分层已接入当前源码，不等于完整交付认证。Code Agent 与共享存储位于 `internal/codeagent`、`internal/storage`，独立 `internal/workflowagent` 的创建、打开和受控运行有默认及 SDK 外部消费者测试。原会话内工作流接线及节点状态已从 Code Agent 退出，工作流定义、编译与 Eino 图由独立实现持有；应用目录、创建去重、展示 metadata 与附件上传管理归 Web，Code 保留输入附件准入、模型展开及原授权检查。三类 Web 资源路由与独立视图已接通，最终平台、浏览器和真实模型认证见 [P3 验证记录](p3-verification.md)。原“所有工作流接口原样保留、不物理拆包”的范围说明已被此次决定替代，其他无关公开能力保留。

同一 Go module、唯一公开入口 `github.com/ww1489/seasprak/sdk` 和单生产文件 `sdk/sdk.go` 保持不变。底层通用 `Agent` 不改名。普通 `CreateAgentSession` / `OpenAgentSession` 保留会话语义；独立工作流入口为 `WorkflowAgent`、`WorkflowOptions`、`CreateWorkflowAgent` / `OpenWorkflowAgent`。工作流定义、静态编译与图只有一份实现；SDK 通过类型别名和工厂导出，Web 调用独立上层入口。具体当前签名与网络 DTO 见 [开发方案 06](pi-eino-dev-plan/06-events-and-api.md#sdk) 和 [13](pi-eino-dev-plan/13-p3-web-contract.md)，不把接线完成等同于最终认证。

## SDK 负责什么

- L1 `internal/llm`：模型协议、流式响应、错误归一与实际请求计量，不依赖上层运行对象。
- L2 `internal/agent`：通用执行契约、消息、预算基础、受控工具及 Eino 适配。复用 Eino Agentic 执行路径和 Graph，不另写模型循环。
- Code Agent：显式工作区绑定，会话创建/打开、输入队列、历史/分支、上下文/压缩、模型与工具选择、事件、取消和已有基础暂停/恢复/核对。普通受控子 Agent 的权限、版本、预算和取消约束保留。
- Workflow Agent：独立定义/编译/运行、结构化输入、节点状态与结果、事件、取消/关闭，以及经过真实路径验证的节点级暂停、审批和显式恢复。保留已有静态子流程、条件和并行汇合，不自动增加聊天历史、steering 或压缩能力。
- 共享存储只提供契约和后端，不拥有业务调度。两类上层 Agent 互不导入，各自持有状态、日志写入者、版本引用、审批、恢复与预算；共享后端实现不等于共写一份日志。
- 每个所属运行继续执行冻结参数、授权、票据占用、计量、预算和取消规则。状态先持久提交再发布；失败候选不变成成功结果；未知副作用不能通过盲重试消除。
- 只读浏览和打开不写入、不改变原绑定、不自动执行或恢复。取消/关闭等待真实退出，调用方超时不是停止证明。

## 业务层负责什么

- Web 分别消费两类 Agent。代码会话资源为 `/v1/sessions`；独立工作流定义和运行资源为 `/v1/workflows`、`/v1/workflow-runs`，已通过各自调用链接通，不通过代码会话的工作流 `targetAgent` 执行。
- 多 Agent 角色分工、任务拆解、路由、并行编排、结果聚合及多实例目录/展示管理。
- 通过既有受控工具组合调用独立 Agent，显式传递允许的输入和取消信号，返回匹配原工具调用的结果或业务引用。SDK 不新增专用跨 Agent 调用框架。
- 跨任务审批、补参、补偿、业务重试和外部系统幂等；完整持久子树的进度/恢复调度；跨独立运行或进程的总资源/费用政策。

业务组合不自动共享历史、generation、审批或恢复，也不能把外层工具批准当作内层副作用许可。被调用 Agent 按自身策略校验权限和预算。应用不直接修改内部状态或重造票据，SDK 不承诺跨任意外部副作用恰好执行一次或跨运行原子总预算。

共用 Eino 与存储实现仍不合并两类运行账目。当前日志命名空间分别为 `sessions/<sid>` / `workflow-runs/<rid>`；应用目录、创建去重、名称标签及附件上传展示由 Web 拥有，Code 仅保留附件准入、模型展开和原授权。Workflow 的默认 TODO 后端由自己的 journal 持久化；`Operations.Todos` 非空时宿主完整替换，不镜像默认状态。manifest 固定 `workflow-journal-v1` / `host-injected` 归属，重开切换拒绝；私有 TODO 不进入 HTTP。具体字段、回执作用域与 typed cursor 由开发 06/10/13 定义，不增加跨 Agent 公共框架。

## 迁移顺序与兼容边界

1. 先修订原 PRD 和开发设计的冲突正文及验收条件，区分目标与当前实现。
2. 迁移 Code Agent 和共享存储目录，建立单向依赖检查。
3. 建立独立 Workflow Agent，复用图和工具安全路径，统一模型完整响应校验与并行节点预算。
4. 替换 SDK/Web 接线，移除 Code Agent 内工作流定义、节点状态、执行/恢复分支及旧 `CompileWorkflowTarget` 入口。
5. Web 接通两类独立资源和视图，最后针对最终文件集重新验证。

旧开发数据不要求迁移；不兼容或运行类型不匹配时明确拒绝，不自动删除目录。这个决定不授权删除无关公开能力、用户未提交修改或已有安全子执行修复。动态导入、Coze、热重载和业务补参仍保持原未交付阶段，本轮不启动 P4/P5。

## 当前阻塞和证据

Code 原生检查点预检的生命周期回调缺陷已按批准的 Step9 方法修正，并获独立规格/质量复核及 Windows／实际 Linux 受影响范围普通/race 接受。唯一方法主定义见 [开发方案 09 §checkpoint](pi-eino-dev-plan/09-persistence-and-recovery.md#checkpoint)：固定 Eino v0.9.21，每次新建、单次使用探针，在生命周期回调前停止并检查原目标根状态；宿主 codec 仍可先执行，不能外推任意宿主零执行或整树认证。原拒绝保护、修前失败、宿主 callbacks 和真实恢复执行路径保留，不 fork/patch/升级 Eino，不使用反射/私有回调管理器或第二循环。Workflow 当前没有原生整图 checkpoint，不因 Code 修正扩大其恢复范围。

默认文件访问的有界交付及未闭合写路径见 [09 §layout](pi-eino-dev-plan/09-persistence-and-recovery.md#layout)：Blob H1/M1、13.2e清单 publisher、Workflow.Close 安全错误与可写创建父目录同步均已独立复核并获父两平台实际证据的有界接受；清单通用失败阶段旧 ID/mode 的测试补强建议仍保留。Repair 完整旧 WRITE_THROUGH 替换迁移继续暂停。父最终冻结源 Windows／实际 WSL Linux 全仓普通/race/vet/build 及显式消费者普通/race 已通过，原 Step14 具名父/子目标 actions 均通过，Windows 文件 symlink/junction 实际执行；历史失败/权限 skip 保留，macOS 延期未验，P3 仍未完成。唯一授权四协议 live 与原样完整 E2E 已各执行一次并通过，未自动重跑或追加调用；顺序、请求计数、安全扫描剩余模块告警及外部 Node 原生 env-file 桥接限制统一见 [12 当前证据与限制](pi-eino-dev-plan/12-delivery-and-validation.md#当前证据与限制)，Gemini 本轮请求保持零。没有提交、推送或 PR 授权，具体命令/范围见 [P3 验证记录](p3-verification.md)。
