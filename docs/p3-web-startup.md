# 本机 Web 启动（P3 收尾中）

当前源码已提供本机页面、会话创建与只读浏览、输入和事件、取消／继续队列，以及已有的历史、分支、压缩、附件和条件恢复入口。页面是现有会话能力的适配层，不另建执行循环。最终集成与平台验收仍未完成，不能把服务可启动等同于 P3 已交付。

Code Agent 与共享存储位于 `internal/codeagent`、`internal/storage`；独立 Workflow 工厂及受控执行已接入，Code 会话内旧工作流与节点状态已退出。应用目录、创建去重、展示 metadata 和附件上传管理归 Web，Code 保留输入附件准入、模型展开及原授权。服务端 `/v1/sessions`、`/v1/workflows`、`/v1/workflow-runs` 与前端独立视图已接通，嵌入页面资源已更新，并有对应默认测试路径。最终真实浏览器认证仍未验证，不将默认测试或资源构建当作整体 Step5/P3 通过；最终结果见 [P3 验证记录](p3-verification.md)。跨运行编排和受控工具组合属于业务；具体边界见 [职责说明](sdk-scope.md)。

## 启动

`go run ./cmd/web --web --workspace <工作区绝对路径> --state-root <独立私有目录绝对路径> --config <配置文件绝对路径> [--listen 127.0.0.1:8080]`

不带 `--web` 仅显示帮助；`--help`、`--version` 不启动监听。监听地址必须为回环 IP，不接受非回环地址或通过 DNS 扩大监听范围。允许 `127.0.0.1:0` 由系统分配端口。

工作区必须存在；状态目录不能与工作区重叠。状态目录的父目录必须存在。新状态目录由程序私有创建；已有目录权限不符合要求时拒绝启动，不自动修改其 ACL。Windows 使用当前用户与 SYSTEM 的保护 DACL；Unix 校验当前用户所有和 owner-only 权限。实际平台验证情况以 P3 验证记录为准，macOS 当前延期且未认证。

## 受信配置

配置是 JSON 对象，必需 `model` 和非空 `generationFingerprint`。当前可执行配置还须显式设置 `profile: "memory"`；省略 profile 保持默认文件／进程后端不可用，不能创建可写执行会话。这里的 memory 是裁剪后端能力，不代表状态目录中的 JSONL 历史不持久。

`model` 使用已有模型配置字段，包括 Provider、Protocol、Model、Endpoint、Version、AccountScope、CredentialRef/NoCredentials、Parameters、Capabilities；能力与窗口必须如实声明，不能随意填 verified。此配置是受信宿主装配，不是 HTTP 请求 DTO。

现有可选字段：

- `tools`：允许启用 `write_todos`；Code 使用自己的 TODO 后端，Workflow 默认使用本运行日志中的独立 TODO 后端，不装配宿主文件或进程后端。
- `approvalTools`：已启用 tools 的子集，要求一次操作批准；不能自行启用工具。
- `agents` / `workflows`：不可变的两份启动清单。agents 只注册普通 Code 执行目标；workflows 直接声明独立定义，按精确名称和版本冻结，模型绑定为 `default`，不接受旧 `delegable` 字段。静态校验先于状态根创建，改变工作流不会改变普通 Code 清单指纹。`GET /v1/workflows` 只返回名称、版本、说明和输入 schema；`POST /v1/workflow-runs` 创建独立运行，不进入 Code 清单或队列。旧会话内工作流 target 与查询路由已退出，动态导入/热重载与高级恢复不因此交付。

当前模型协议工厂为 openai-chat、openai-responses、anthropic-messages、gemini-generate-content、deepseek-chat。启动只装配所配置的一个基础模型，没有 Web 模型／工具选择路由、动态配置上传或插件入口；SDK 已有选择 API 不代表页面已经接线。服务启动成功不等于供应商可用。

有凭据的配置将 CredentialRef 指向 `env:变量名`，程序从宿主环境解析；配置不保存明文密钥。无凭据仅在显式 NoCredentials 且所选工厂允许时使用。生产入口不自动读取 `.test_env`；该文件只用于获准本机真实模型测试。

配置和工作区缺失、工厂／模型校验失败、凭据引用缺失、权限不足或端口占用时启动失败，日志只给固定诊断，不回显模型原始错误或秘密。

以下仅展示真实字段形状，模型名、endpoint、账号范围及能力/窗口值必须替换为受信宿主的实际配置；`declared` 是装配方声明而非本项目认证，示例不发起模型请求。根字段为 lowerCamelCase；嵌入的 ModelConfig/Capabilities/Parameters 使用现有 Go 导出字段名：

```json
{
  "model": {
    "Provider": "openai",
    "Protocol": "openai-chat",
    "Model": "approved-model",
    "Endpoint": "https://model.example.invalid/v1",
    "Version": "config-v1",
    "AccountScope": "local-account",
    "CredentialRef": "env:MODEL_API_KEY",
    "Capabilities": {
      "Items": {
        "text": {"Status": "declared"},
        "text_stream": {"Status": "declared"},
        "tools": {"Status": "declared"},
        "multiple_tools": {"Status": "declared"},
        "context_window": {"Status": "declared"},
        "output_limit": {"Status": "declared"}
      },
      "ContextWindowTokens": 32768,
      "MaxOutputTokens": 1024
    },
    "Parameters": {
      "MaxOutputTokens": 1024,
      "ConservativeContextWindow": 32768,
      "MinAnswerTokens": 128,
      "PolicyVersion": "local-v1"
    }
  },
  "generationFingerprint": "trusted-build-v1",
  "profile": "memory",
  "tools": ["write_todos"],
  "approvalTools": ["write_todos"],
  "agents": [{
    "name": "reviewer", "version": "v1", "instruction": "检查受信委派结果。",
    "delegable": true, "tools": ["write_todos"]
  }],
  "workflows": [{
    "name": "echo", "version": "v1", "source": "local",
    "formatVersion": "seasprak-workflow/v1",
    "inputSchema": {
      "type": "object", "properties": {"text": {"type": "string"}},
      "required": ["text"], "additionalProperties": false
    },
    "nodes": [
      {"id": "start", "type": "start"},
      {"id": "end", "type": "end", "inputs": {"text": {"ref": {"node": "start", "field": "text"}}}}
    ],
    "edges": [{"from": "start", "to": "end"}]
  }]
}
```

`workflows` 直接承载当前 WorkflowDefinition，没有 delegable、outputSchema 或顶层 bindings；输出由 end.inputs 选择。上例 echo 是纯图，无模型/工具节点，运行不会为了路由调用 Code 主模型；若配置 model 节点，其 model 字段引用 `"default"`，仍在工作流自己的预算和授权范围内执行。`write_todos` 工具节点使用本运行默认 journal TODO 后端；宿主 SDK 注入 Todos 会完整替换默认后端并固定 manifest 归属，不能在重开时切换。

## 页面认证与会话控制

1. 启动输出监听 URL 和本地 bearer 文件路径，不打印 token。打开该 URL，由本机用户提供 bearer；页面只在内存保存，不写入 URL 或持久浏览器存储。
2. 选择已有会话只浏览和订阅已公开事实，不自动取得写控制。通过页面显式“打开会话控制”后才启用修改操作；HTTP 对应 `POST /v1/sessions/{sid}/open`。
3. 创建会话须提交获准工作区，模型引用保持 `default`。发送输入后，页面通过现有快照和事件展示真实状态。
4. 请求结果不确定时保留原请求的幂等键、正文和 revision，允许明确重试或放弃；刷新和重连不自动发送输入、批准或恢复任务。
5. 恢复只在后端公开资格允许时由用户显式触发；未知效果继续走现有核对规则。复杂子树恢复尚未交付，不因新增按钮而获得能力。

## 独立工作流创建、历史和控制

- 页面分别列出 Code 会话与 Workflow 运行。`GET /v1/workflows` 读取允许定义；表单只支持当前声明的简单 string/number/integer/boolean/enum 字段，遇未支持 schema 明确禁用，不猜参或借主模型补参。
- 用 `Idempotency-Key` 和 JSON `{workspace, workflow, version, input}` 调用 `POST /v1/workflow-runs`，input 为一个对象。202 返回原受理 snapshot（不是 SDK 输入回执），同键重试仍为原版本/游标；执行结果另查本 runId 的最新 snapshot。Code 继续走 `/v1/sessions` 和 inputs，普通 targetAgent 不包含工作流。
- `GET /v1/workflow-runs`、`/{rid}`、`/{rid}/snapshot` 与 `/{rid}/events` 为只读列表/历史/观察；历史浏览不取得 writer、不执行节点。显式 `POST /v1/workflow-runs/{rid}/open` 发送 `{}` 且无需 Idempotency-Key，只取得 writer，不自动 Resume。
- pause/resume/cancel 带原幂等键和安全整数 expectedRevision；cancel 可带 reason。暂停且执行已停止后，使用当前 interactionId、instanceId、revision 回答 allowed-once/rejected/cancelled，再由用户单独显式 Resume。HTTP 审批 receipt 的 scope=instance、target=interactionId、acceptedCommit=0；pause/cancel/resume 的 scope=durable、target=runId、acceptedCommit>0。两者不归一化，完成状态查 `/{rid}/operations/{oid}`；重启后旧批准无效。
- 工作流 SSE 使用独立 typed cursor、固定 `event` dispatcher；浏览器校验 runId/instanceId 和安全整数，不借用 Code cursor/审批/取消。请求响应不确定时在页面内存保留原请求与键，导航/重连不改写请求或自动重送，明确重试仍用原正文；不将 bearer 或业务请求持久保存到浏览器存储。

HTTP 校验实际监听 authority、存在时的同源 Origin 和 bearer，不允许转发头扩张访问范围，默认无 CORS。静态页面可加载不等于业务 API 获得授权；认证失败不调用业务 handler。两类 SDK/HTTP 资源按自身 ID 授权，分别写 `sessions/<sid>` / `workflow-runs/<rid>`。业务可用既有受控工具组合，日志/预算/审批/恢复仍独立；外层批准不授权内层，SDK 不提供跨运行总账、原子事务或整树恢复。未知效果阻止盲重跑；当前 Workflow 无公开核对路由，不能用 Code 的核对入口释放它的调用。

## 页面资源与退出

前端源码在 `web/`，实际 Go 服务嵌入 `internal/web/static/`。修改页面后须在 `web/` 运行 `npm run typecheck`、`npm run test -- --run`、`npm run build`，并重新构建服务；只改 React 源码不会更新已构建服务。完整浏览器验收另行执行，单元测试和资源构建不能替代。

每次服务启动生成新 token 和实例身份；旧审批许可不跨重启保存。正常关闭后删除凭据文件，停止监听并等待实际退出；超时返回失败，不虚报正常停止。打开已有会话不自动 Resume。

当前证据和未解决问题见 [P3 验证记录](p3-verification.md)，网络语义见 [P3 Web 接入契约](pi-eino-dev-plan/13-p3-web-contract.md)。
