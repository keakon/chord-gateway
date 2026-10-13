# IM 接入总览

所有 IM 平台统一使用单 owner 配对与显式聊天绑定，单工作区也需绑定。首次运行 `chord-gateway --pair`，在机器人私聊发送 `/bind "~/workspace" <pairing_code>`；详见[配对与本机重置](./pairing_CN.md)。旧部署升级步骤见[更新日志](../CHANGELOG_CN.md)。

本文档是 `chord-gateway` 的“任务型入口页”，用于指导你把 IM 平台接入 gateway。

如果你需要字段级配置参考，请直接看：

- [配置参考](./configuration_CN.md)

## 当前支持哪些 IM 平台？

`chord-gateway` 当前支持：

- **微信 iLink**（`ims.wechat`）— 通过 iLink Bot API 做个人微信扫码登录。
- **飞书**（`ims.feishu`）— 通过飞书应用凭据 + 长连接接收事件。

## 该选微信还是飞书？

### 适合选微信 iLink 的场景

- 你明确需要 **个人微信** 作为聊天入口。
- 你接受 **单工作区路由**（所有微信消息必须进入同一个 workspace）。

从这里开始：[微信 iLink 指南](./wechat_CN.md)

### 适合选飞书的场景

- 你需要 **多工作区路由**（不同飞书群聊进入不同 workspace）。
- 你希望确认/提问交互尽可能使用更友好的交互卡片体验。

从这里开始：[飞书接入指南](./feishu_CN.md)

## 接入与路由

配置 `ims.wechat: {}`，或配置飞书的 `app_id` / `app_secret` 并完成长连接和事件订阅。运行 `chord-gateway --pair`；微信先扫码登录，平台连接后终端显示配对码。在机器人私聊发送 `/bind "~/workspace" <pairing_code>`，再用 `/status` 验证。

配对后的 owner 用 `/bind "~/another workspace"` 显式绑定其他聊天。两个平台都通过 `chat_bindings` 路由，单工作区也不隐式绑定。目录必须已存在，配置保存成功后才启用路由。其他配置的手工改动需停服务后修改并重启。

owner 是本机控制面用户。只在可信群聊中绑定，回复对群成员可见。不要提交凭据或运行状态。详见[配对](./pairing_CN.md)、[微信](./wechat_CN.md)、[飞书](./feishu_CN.md)和[权限与安全](./permissions-and-safety_CN.md)。

## 故障排查入口

- 微信问题：[故障排查](./troubleshooting_CN.md#微信问题)
- 飞书问题：[故障排查](./troubleshooting_CN.md#飞书问题)
