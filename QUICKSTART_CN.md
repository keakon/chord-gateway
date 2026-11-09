# 快速开始

本指南帮助你把一个项目连接到微信、飞书、Telegram、企业微信、钉钉、Discord 或 Slack。配对后即可在聊天中登记目录。

## 1. 安装 Chord 和网关

先按 [Chord 官方安装说明](https://github.com/keakon/chord/blob/main/README_CN.md#三步完成设置)安装 Chord，完成首次配置向导并验证：

```bash
chord --version
```

使用 Homebrew 安装网关：

```bash
brew install keakon/tap/chord-gateway
chord-gateway --version
```

或使用 Go 1.27.0 或更高版本从源码安装：

```bash
go install github.com/keakon/chord-gateway@latest
chord-gateway --version
```

手动下载预编译版本、源码构建和后台服务配置见 [Cookbook](./docs/cookbook_CN.md)。

## 2. 选择平台

将下面一个最小配置保存为 `config.yaml`。

微信：

```yaml
ims:
  wechat: {}
```

飞书：

```yaml
ims:
  feishu:
    app_id: cli_example
    app_secret: your-app-secret
```

飞书应用需要启用长连接、订阅 `im.message.receive_v1` 并发布版本；使用交互卡片时添加 `card.action.trigger`。权限和控制台步骤见[飞书指南](./docs/feishu_CN.md)。

Telegram：

```yaml
ims:
  telegram:
    bot_token: your-bot-token
```

先在 BotFather 创建 Bot 并保留默认群 privacy，详见[Telegram 指南](./docs/telegram_CN.md)。已有 webhook 会阻止启动；Gateway 不会自动删除。

企业微信：

```yaml
ims:
  wecom:
    bot_id: your-bot-id
    secret: your-bot-secret
```

开通智能机器人 **API 模式 → 长连接**，在可用范围中允许 owner 私聊。产品权限和部署验收见[企业微信指南](./docs/wecom_CN.md)。

钉钉企业内部应用机器人：

```yaml
ims:
  dingtalk:
    client_id: your-client-id
    client_secret: your-client-secret
    robot_code: your-robot-code
    corp_id: your-corp-id
```

开启机器人 Stream 接收、申请单聊和群主动发送权限并发布应用，详见[钉钉指南](./docs/dingtalk_CN.md)。真实企业验收仍待完成。

Discord：

```yaml
ims:
  discord:
    bot_token: your-bot-token
    application_id: "123456789012345678"
```

安装机器人时授予 View Channel、Send Messages、Send Messages in Threads，保持 privileged intents 关闭。安装与私有 thread 访问步骤见 [Discord 指南](./docs/discord_CN.md)。

Slack：

```yaml
ims:
  slack:
    app_token: your-app-token
    bot_token: your-bot-token
    app_id: your-app-id
    team_id: your-team-id
```

启用 Socket Mode、安装 Bot scopes、订阅 `message.im` / `app_mention` 并注册 `/chord`。配对前先按 [Slack 指南](./docs/slack_CN.md)完成安装。

不想接 IM 平台？本机使用可运行 `chord-gateway -f config.yaml --console`，它只使用唯一一个 workspace，无需配对与绑定。见 [IM 接入总览](./docs/im_CN.md#本地-console-模式)。

## 3. 在私聊中配对

在前台终端运行：

```bash
chord-gateway -f config.yaml --pair
```

终端会立即显示有效期为 10 分钟的配对码。等待所选平台连接就绪后，使用 gateway 主机上已有的目录完成绑定：

| 平台 | 在机器人私聊中发送 | 验证命令 |
|---|---|---|
| Slack App 私聊 | `/chord bind "<directory>" <pairing_code>` | `/chord status` |
| 其他平台 | `/bind "<directory>" <pairing_code>` | `/status` |

- **微信**：按提示扫码登录。
- **钉钉**：先由同企业员工私聊发送 `hello` 验证身份。保存绑定前，gateway 会发送 `Gateway binding check.`；探测失败或结果未知时，需修复问题后发送新的 `/bind`。
- **企业微信**：主动绑定探测收到平台成功 ACK 后，才能保存绑定。

Gateway 为每个平台登记一位可信控制者（owner），并绑定当前聊天；其他发送者的消息会被忽略。发送第一条任务才会在绑定目录启动 Chord。配对后使用 `chord-gateway -f config.yaml` 普通启动，也可作为后台服务运行。

> **Note:** 每个聊天都需显式绑定，单工作区也不例外。详见[配对与本机重置](./docs/pairing_CN.md)；已有部署升级前还需阅读[更新日志](./CHANGELOG_CN.md)。

## 4. 绑定新聊天或切换目录

配对后的 owner 绑定新聊天时无需配对码。为已有聊天切换目录前，应完成任务和待处理交互。

按目标聊天选择命令：

| 目标 | 绑定命令 |
|---|---|
| Slack 之外的平台机器人私聊 | `/bind "/path/to/project"` |
| 飞书或企业微信群 | `/bind "/path/to/project"` |
| Telegram 群 | `/bind@BotUsername "/path/to/project"` |
| 钉钉群 | `@robot /bind "/path/to/project"` |
| Discord 服务器频道或 thread | `@Bot /bind "/path/to/project"` |
| Slack 聊天根目标 | `/chord bind "/path/to/project"` |
| Slack thread | `@Bot /bind "/path/to/project"` |

群成员可以看到回复。Telegram 默认 privacy 设置下，普通任务需通过回复 Bot 发送。Discord 服务器中的后续命令也需在开头 @Bot。Slack slash 命令始终作用于聊天根目标；Slack 和 Discord 的每个 thread 都需独立绑定。

绑定条件、保存失败恢复、账号变化和本机重置见[配对指南](./docs/pairing_CN.md)。

## 5. 状态位置与后续文档

网关默认使用以下位置：

- macOS / Linux 状态目录：`${XDG_STATE_HOME:-~/.local/state}/chord-gateway`
- 配置文件：`${XDG_CONFIG_HOME:-~/.config}/chord-gateway/config.yaml`

Discord bot token 保留在配置中；配置文件需按敏感信息保护。企业微信使用 bot ID 和 secret 配置，绑定前需主动探测成功，详见[企业微信指南](./docs/wecom_CN.md)。

状态目录包含日志、微信 token（默认 `<state_dir>/wechat/token.json`）和会话固定记录。飞书 `app_id` / `app_secret` 保留在配置中；短期访问 token 只保存在内存中，并按需刷新。

收件队列、去重记录和轮询游标仅存内存，重启不恢复。结果不确定时，先核查 Chord 原会话再决定是否重发。详见[投递与重启语义](./docs/operations_CN.md#消息投递与重启)。

- [配置参考](./docs/configuration_CN.md)
- [Cookbook](./docs/cookbook_CN.md)
- [IM 接入总览](./docs/im_CN.md)
- [使用指南](./docs/usage_CN.md)
- [运维说明](./docs/operations_CN.md)
- [故障排查](./docs/troubleshooting_CN.md)
