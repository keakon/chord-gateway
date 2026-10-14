# IM 接入总览

所有 IM 平台统一使用单 owner 配对与显式聊天绑定，单工作区也需绑定。首次运行 `chord-gateway --pair`，微信/飞书/企业微信/钉钉在机器人私聊发送 `/bind "~/workspace" <pairing_code>`；详见[配对与本机重置](./pairing_CN.md)。旧部署升级步骤见[更新日志](../CHANGELOG_CN.md)。 Slack 在 App 私聊使用 `/chord bind "~/workspace" <pairing_code>`；参见 [Slack 接入指南](./slack_CN.md)。

本文档是 `chord-gateway` 的“任务型入口页”，用于指导你把 IM 平台接入 gateway。

如果你需要字段级配置参考，请直接看：

- [配置参考](./configuration_CN.md)

## 当前支持哪些 IM 平台？

`chord-gateway` 当前支持：

- **微信 iLink**（`ims.wechat`）— 通过 iLink Bot API 做个人微信扫码登录。
- **飞书**（`ims.feishu`）— 通过飞书应用凭据 + 长连接接收事件。
- **Telegram**（`ims.telegram`）— BotFather token + Bot API 长轮询，支持私聊与普通群文本。

- **企业微信**（`ims.wecom`）— 智能机器人 API 长连接，支持私聊、群文本及后台结果。
- **钉钉**（`ims.dingtalk`）— 企业内部应用机器人，Stream 收件与主动文本回传；真实企业验收待完成。
- **Discord**（`ims.discord`）— Bot DM，以及服务器文字频道和既有 thread 中显式 @Bot 的消息。
- **Slack**（`ims.slack`）— 单 workspace Socket Mode，支持私聊、频道和 thread。

企业微信、钉钉与 Discord 的实现及自动测试不等于部署验收通过。真实账号验收仍待完成，部署前请按[企业微信验收步骤](./wecom_CN.md#部署验收)、[钉钉部署验收清单](./dingtalk_CN.md#部署验收清单)或 [Discord 部署验证](./discord_CN.md#故障排查与部署验证)核查。

## 如何选择平台？

### 适合选微信 iLink 的场景

- 你明确需要 **个人微信** 作为聊天入口。
- 你需要机器人私聊入口；此接入不增加微信群能力。

从这里开始：[微信 iLink 指南](./wechat_CN.md)

### 适合选飞书的场景

- 你需要 **多工作区路由**（不同飞书群聊进入不同 workspace）。
- 你希望确认/提问交互尽可能使用更友好的交互卡片体验。

从这里开始：[飞书接入指南](./feishu_CN.md)

## 本地 console 模式

不接任何 IM 平台、只在本机使用 Chord 时，可用 `--console` 启动：

```bash
chord-gateway -f config.yaml --console
```

- `--console` 不能与 `--pair` 同时使用，且要求**恰好一个** workspace；`ims` 配置会被忽略，也不使用任何平台凭据。
- 不需要配对或聊天绑定：直接使用唯一的 workspace，`/bind` 不可用（会打印用法说明）。
- stdin 上每一行非空输入都作为一条命令或任务处理（例如 `/status`）；回复以 `[console] <文本>` 打印到 stdout。
- console 输入属于本机控制面，不经过 owner 校验；请只在可信主机上使用。

另见[配对](./pairing_CN.md)与[配置参考](./configuration_CN.md)。
### 适合选 Telegram 的场景

- 你需要 Telegram 私聊或群聊入口，使用文本命令处理确认和提问。
- 你希望无需公网回调即可部署，并使用默认 privacy 下回复 Bot 的任务方式。

从这里开始：[Telegram 接入指南](./telegram_CN.md)

### 适合选企业微信的场景

你的组织已开通智能机器人 API 长连接，希望从企业微信私聊或群聊控制 Chord。首版使用文本确认与问答，支持主动回传长任务和后台结果。

从这里开始：[企业微信接入指南](./wecom_CN.md)

### 适合选钉钉的场景

- 团队使用同一钉钉企业，能配置内部应用机器人与主动消息权限。
- 使用员工私聊或群内 @机器人，通过文本完成任务、确认和 Question。

从这里开始：[钉钉接入指南](./dingtalk_CN.md)。首版仅同企业文本，部署验收仍待完成。

### 适合选 Discord 的场景

- 希望用独立服务器频道或 thread 管理不同项目。
- 接受文本命令，希望无需公网入口。

从这里开始：[Discord 指南](./discord_CN.md)

### 适合选 Slack 的场景

如果需要通过 Socket Mode 接入，并在私聊、频道和 thread 保持独立会话，可选择 Slack。根命令入口统一为 `/chord`；频道/thread 任务需 owner 显式 @Bot。先阅读 [Slack 接入指南](./slack_CN.md)。


## 接入与路由

选择需要的平台：微信配置 `ims.wechat: {}`；飞书填写 `app_id` / `app_secret` 并完成长连接和事件订阅；企业微信填写 `bot_id` / `secret` 并开通智能机器人 API 长连接；Discord 安装 Bot 后填写 `bot_token` / `application_id`。运行 `chord-gateway --pair`；微信先扫码登录，微信、飞书、企业微信和 Discord 连接就绪后终端显示配对码。在机器人私聊发送 `/bind "~/workspace" <pairing_code>`，再用 `/status` 验证。企业微信需固定主动绑定探测成功后才保存。

钉钉配置四项应用/企业身份字段，首次私聊 `hello` 验证完成后才显示配对码；保存绑定前先主动探测目标。

配对后的 owner 用 `/bind "~/another workspace"` 显式绑定其他聊天。所有已实现平台都通过 `chat_bindings` 路由，单工作区也不隐式绑定。目录必须已存在，配置保存成功后才启用路由。其他配置的手工改动需停服务后修改并重启。

owner 是本机控制面用户。只在可信群聊中绑定，回复对群成员可见。不要提交凭据或运行状态。详见[配对](./pairing_CN.md)、[微信](./wechat_CN.md)、[飞书](./feishu_CN.md)、[企业微信](./wecom_CN.md)、[钉钉](./dingtalk_CN.md)、[Discord](./discord_CN.md)和[权限与安全](./permissions-and-safety_CN.md)。

Telegram 配置 `ims.telegram.bot_token`；群绑定需 `/bind@BotUsername`。服务器只保留未接收更新 24 小时，部署前核对[保留期与验收](./telegram_CN.md)。

## 故障排查入口

- 微信问题：[故障排查](./troubleshooting_CN.md#微信问题)
- Telegram：[排障](./telegram_CN.md#排障)
- 飞书问题：[故障排查](./troubleshooting_CN.md#飞书问题)
- [企业微信排障](./wecom_CN.md#回复和故障处理)
- 钉钉问题：[钉钉范围与排障](./dingtalk_CN.md#范围与排障)
- Discord 问题：[Discord 指南](./discord_CN.md#故障排查与部署验证)

- Slack：[安装检查与排障](./slack_CN.md#安装检查与排障)
