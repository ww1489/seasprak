# 本机 Web 启动（P3 实施中）

目前交付启动/停止、受信模型装配和本机认证基础；业务路由及 A2UI 页面尚未接入。启动后未知路由返回 404，不能把此入口当作已经完成的 P3 Web 产品。

## 启动

`go run ./cmd/web --web --workspace <工作区绝对路径> --state-root <独立私有目录绝对路径> --config <配置文件绝对路径> [--listen 127.0.0.1:8080]`

不带 `--web` 仅显示帮助；`--help`、`--version` 不启动监听。监听地址必须为回环 IP，不接受非回环地址或通过 DNS 扩大监听范围。允许 `127.0.0.1:0` 由系统分配端口。

工作区必须存在；状态目录不能与工作区重叠。状态目录的父目录必须存在。新状态目录由程序私有创建；已有目录权限不符合要求时拒绝启动，不自动修改其 ACL。Windows 使用当前用户与 SYSTEM 的保护 DACL；Linux/macOS 使用当前用户所有、无组/其他用户权限的目录和凭据文件。实际平台验证情况以 P3 验证记录为准。

## 受信配置

配置是一个 JSON 对象，字段为 `model` 和非空 `generationFingerprint`。`model` 使用已有 `llm.ModelConfig` 字段，包含 Provider、Protocol、Model、Endpoint、Version、AccountScope、CredentialRef/NoCredentials、Parameters、Capabilities；能力与窗口必须如实声明，不能随意填 verified。此配置是受信宿主装配，不是 HTTP 请求 DTO。

当前允许已有五类工厂的协议名：openai-chat、openai-responses、anthropic-messages、gemini-generate-content、deepseek-chat。没有工具后端或插件上传入口。具体模型实际能力仍须按原协议测试认证，服务启动成功不等于供应商可用。

有凭据的配置将 CredentialRef 指向 `env:变量名`，程序从宿主环境解析；配置不保存明文密钥。无凭据仅在显式 NoCredentials 且所选工厂允许时使用。生产入口不自动读取 `.test_env`；该文件仍只用于本地 live 测试。

配置和工作区缺失、工厂/模型校验失败、凭据引用缺失、权限不足或端口占用时启动失败，日志只给固定诊断，不回显模型原始错误或秘密。

## 本机认证与退出

启动输出监听 URL 和本地 bearer 文件路径，不打印 token。每次启动生成新的随机 token；凭据文件在正常关闭后删除。浏览器后续仅在内存使用 bearer，通过 Authorization 请求，禁止将其放入 URL 或持久浏览器存储。

HTTP 同时校验实际监听 authority、存在时的同源 Origin 和 bearer，不允许转发头扩张访问范围，默认无 CORS。认证失败不调用业务 handler。进程关闭停止监听，等待 HTTP 退出；超时强制关闭并返回失败，不虚报正常停止。

当前服务不创建或恢复任何 Session；后续业务路由将负责将已接纳业务与 HTTP 请求生命周期分离。当前原 SDK、cmd/agentd 与已有 P2 改动均保留。

验收证据及未完成项见 [P3 验证记录](p3-verification.md)，协议边界见 [P3 Web 接入契约](pi-eino-dev-plan/13-p3-web-contract.md)。
