# 第三方来源说明

## Beautiful UI（MIT）

- 来源：https://www.beautifului.dev/（首页各组件“View code”内嵌源码）
- 许可证：MIT，Copyright (c) 2026 Shane Levine；2026-09-30 在 https://www.beautifului.dev/license 核对，全文见 [licenses/beautifului-MIT.txt](licenses/beautifului-MIT.txt)。
- 获取日期：2026-09-30。sha256 为站点页面内嵌的原始 TSX 源码（UTF-8）摘要；原始源码未提交，仅记录摘要。
- 公共修改：删除 `"use client"`、示例数据、脚本化动画计时器和远程资源（视频、来源链接与图标）；文字改为调用方传入的纯文本并以 React 文本节点渲染；标签改为中文。

| 组件 | 原始 sha256 | 依赖 | 状态与本地修改 |
| --- | --- | --- | --- |
| Loading State | `eb1bdcddcd2dbdfe1232b3bf3d7dfd0912ef9b6e0fb1a9b095cb423778e8a8d1` | react | 已集成：`src/components/bui/LoadingState.tsx`。删除 Surfer 变体及远程视频 URL、Dots/Orbit 模式。 |
| Streaming Text | `8b17569fbc9c75aef1f82a5310f2402209cc8ab1b13f65fdd71251cb1e1f14d3` | react | 已集成：`src/components/bui/StreamingText.tsx`。仅保留正文段落与流式光标；删除逐词计时、来源 chip、外链、操作按钮和追问。 |
| Task Rows | `7a81c2a264528405826b8ac9db9e92d4c14ef6fa3fcdb39010022df3cf4c0920` | react | 已集成：`src/components/bui/TaskRow.tsx`。保留 SpinnerRing/Badge/状态胶囊，单行渲染产品 Task，可注入受信取消按钮；删除展开明细与脚本序列。 |
| Tool Chips | `dc06a0ec805825622bf622f0f56cc8305f79751fe7fcb55727a59869b443c9c8` | react, react-dom (createPortal) | 已集成：`src/components/bui/ToolChip.tsx`。保留行与 chip 外观，展开区显示有界纯文本输出预览；删除 diff chip 与 portal 预览（因此不再依赖 react-dom portal）。 |
| Chat | `1b8ac6cd342a5ccab92c0e0265d7431a35703e23496bd85c4cf64d883e6cc7f5` | react | 已集成：`src/components/bui/Chat.tsx`。拆分为用户气泡、助手段落和输入框；输入框改为 textarea，处理输入法组合；删除标签页、图标按钮和脚本化回复。 |
| Thinking | `3d73617406120fad93fd15c67054a59debb9e43ccd385a21e2f48a7fb4036e24` | react | 未集成：服务端按安全规则不向浏览器下发推理内容，没有可展示的数据。 |
| Prompt Bar | `1f2ff02adae3f1fe80b00fd0ef1c54c3558e8638502c1b7d71b1d0000dfceb5a` | react, `glimm`（npm）, `@/components/primitives/GlideMenu` | 未集成：`GlideMenu` 源码在站点未发布（`/components/primitives/GlideMenu` 返回 404），无法获取；改用上面 Chat 的输入框加原生 `<select>` 选择目标 Agent。 |
| Approval Card | `5e90ba19b79f4d385a34e9cf703b15e819c72df93d70a72c8c01aa85d500ca95` | react, `@/components/atoms/Button`, `@/components/primitives/GlideMenu` | 未集成：`Button` 与 `GlideMenu` 源码均无法获取（404）；改用本地简单组件 `src/components/ApprovalCard.tsx`（非 Beautiful UI 代码）。 |

设计令牌：`src/styles.css` 中的颜色、圆角、阴影和关键帧取自站点样式表 `/_next/static/css/916c3f25c8795666.css`（2026-09-30 获取），仅保留浅色主题；站点 Web 字体替换为系统字体，不从任何 CDN 加载。

## CloudWeGo eino-examples（Apache-2.0）

- 来源：https://github.com/cloudwego/eino-examples ，固定提交 `a6dbd95ab51fe9896a2bafa2e5a468e3bed01161`，文件 `quickstart/chatwitheino/a2ui/types.go`。
- 许可证：Apache-2.0，Copyright 2026 CloudWeGo Authors（https://github.com/cloudwego/eino-examples/blob/a6dbd95ab51fe9896a2bafa2e5a468e3bed01161/LICENSE-APACHE）。
- 使用方式：`internal/web/a2ui.go` 参照其 A2UI 消息信封与 Text/Column/Card/Row 组件结构重新编写（未逐字复制）；修改为去掉 `interruptRequest`，增加 ChatMessage/ToolCall/Task/Approval 产品组件，全部类型改为按字段构造的非导出网络 DTO。`streamer.go` 与 `static/index.html` 仅作为参考，未复制代码。

## npm 依赖

运行时只打包 `react` 与 `react-dom`（MIT）。其余为构建和测试工具，版本固定在 `package.json` / `package-lock.json`，不进入 `internal/web/static` 以外的产物；Tailwind CSS（MIT）仅在构建时生成 CSS。
