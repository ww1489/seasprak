# Activity 状态复制最小优化执行说明

范围：主线程于 2026-09-29 批准窄读取、仅 Activity 提交的局部复制，以及 Pause 测试契约分离。原 Linux 全仓失败 344842 保留；Windows 344843 早于本轮修改。禁止改变租约、轮询、真实时钟压力、生产 Pause、公共 SDK、日志格式及其他功能；不读凭据、不 live、不提交。

## Step 1: 增加值类型窄读取并接线

1. **前置条件**：已确认续租循环没有 View；首次 beginActivity 只需 limit/Activity，writable 原优先级为 closing → read-only → fault → repair。
2. **目标与输入**：state/activity.go、state/manager.go、sessions/activity.go、coordinator.go；当前 trace ID、Manager 持有的状态。
3. **实施方法**：ActivityState 返回 `(time.Duration, ActivityBudget, bool)`，WriteStatus 在同一 Manager.mu 内先读 fault 再读 repair；仅返回值或原错误，不泄露可变对象。
4. **动作顺序**：先以运行时接口断言写可编译红测和错误优先级对照；实现读取；beginActivity 使用窄读取，缺 trace 返回 state_conflict；writable 保留前两个本地检查，再调用 WriteStatus。
5. **保留约束**：不移动状态所有权、不把 View clone 移出锁；ReserveActivity 仍重新校验；所有时限/采样不变。
6. **失败处理**：缺 trace 不建立租约；fault 优先于 repair；closing/read-only 优先于存储状态。
7. **交付物**：两个仅 internal 的方法、生产接线、零历史序列化和返回值隔离测试。
8. **验收**：ActivityState/WriteStatus 直接调用测试、writable 错误优先级、缺 trace；普通及 race 通过，复制计数为零。

## Step 2: 限定 Activity 候选复制并保留统一提交流程

1. **前置条件**：既有 renew/settle 红测均观察无关历史序列化一次；原子性、lost-ack、多个 active、旧截止和 Snapshot 隔离对照通过。
2. **目标与输入**：state/activity.go、manager.go、activity_commit_contract_test.go；仅 Reserve/Settle 构造的完整 trace 控制记录。
3. **实施方法**：通用 commit 委托同一个私有提交实现并使用完整 clone；Activity 显式选择受限候选。准入检查仅一个已有 trace，版本/ID/ParentID 合法，其他 trace 字段完全相同，无 entries/events/其他 controls/branch updates；非法输入拒绝。候选为 View 值副本和 Traces map 副本；原 applyCommit 解码目标 trace。
4. **动作顺序**：先复现红测，补准入拒绝与通用路径保留复制的测试；新增受限候选构造；共用原 fault/repair/context、宿主校验、applyCommit、Append、fault 和发布顺序；仅 Reserve/Settle 选择局部复制。
5. **保留约束**：Manager.mu 全程持有。审计的可达路径：两个宿主校验无对应 record 时只扫描后返回；resource-hold 校验只构造局部 map；approval 校验无 claim 时返回；applyControl(trace) 只写复制后的 Traces 和候选 Generation；零 entries/events 不触及共享切片；尾部仅写候选 LastSeq/ActiveTrace 并只读扫描 traces。其他记录分支不可达。不得把此结论推广到任意提交或锁外 View。
6. **失败处理**：准入/候选验证失败零 Append、零发布；拒写/lost-ack 保持旧 View 并 fault；成功才发布；不制造事件或停止证明。
7. **交付物**：局部候选准入函数与统一流水线，无第二套持久化实现；旧 View/嵌套切片不变、非法混入拒绝、通用 commit 仍 clone 的默认测试。
8. **验收**：renew/settle 历史复制次数由 1 降为 0；拒写/lost-ack、候选多 active、Snapshot、过期/迟到不可复活全部保持通过；完整 state/sessions 测试和原真实时钟压力继续执行。

## Step 3: 分离消费者恢复与 Pause 等待取消

1. **前置条件**：已证明存储 accepted 不能证明 Pause 已收到回执；rt.call 返回前取消可返回空回执，返回后等待取消保留回执。
2. **目标与输入**：consumer/resume_test.go、sessions/pause_acceptance_boundary_test.go；实际 Pause、工具/模型屏障、测试 context。
3. **实施方法**：消费者以异步 Pause 和公共 Snapshot 确认 accepted 后释放工具，等待完整 Pause 成功，再沿原 Close/Open/Resume 流程。取消契约单独使用测试 context.Done 的直接调用者观察：只有 AgentSession.Pause 直接评估等待 select 的 Done 才通知；runtime.call 或存储/context 内部 Done 均不通知，不计调用次数。
4. **动作顺序**：先补测试验证观察点确在 rt.call 返回并拿到非空回执之后（原存储扣留回执对照时不能通知）；等待该通知再注入 Canceled/DeadlineExceeded；断言非空回执、accepted 操作、执行未被取消、未真实退出；释放工作后确认真实退出。移除消费者固定 80ms 受理假设，保留原恢复身份、调用次数和预算断言。
5. **保留约束**：生产 Pause 不改；测试观察依赖当前函数边界，若边界变化应明确失败，不用存储受理信号替代。仅边界测试使用既有可控时钟夹具；原真实时钟性能测试不变。
6. **失败处理**：所有测试先释放 gate 再 Close；有界等待仅用于挂死防护，不作为成功时序条件；调用方取消不等于工作退出。
7. **交付物**：消费者恢复功能测试及受理前/回执交付前/受理后等待的独立测试，保留非空回执和两类 context 错误断言。
8. **验收**：Windows/Linux 的 Activity/Pause 专项普通/race，完整 sessions/state、SDK consumer 与外层 sdk 普通/race；全部 GOWORK=off、-mod=readonly、串行平台运行。格式与 diff 检查。主线程负责最终串行全仓及其他强制门禁，未运行项不得计通过。
