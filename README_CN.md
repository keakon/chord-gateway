# chord-gateway

所有 IM 平台统一使用单 owner 配对与显式聊天绑定，单工作区也需绑定。首次运行 `chord-gateway --pair`，微信/飞书/企业微信/钉钉在机器人私聊发送 `/bind "<directory>" <pairing_code>`；详见[配对与本机重置](./docs/pairing_CN.md)。旧部署升级步骤见[更新日志](./CHANGELOG_CN.md)。 Slack 在 App 私聊使用 `/chord bind "<directory>" <pairing_code>`；参见 [Slack 接入指南](./docs/slack_CN.md)。

[![CI](https://github.com/keakon/chord-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/keakon/chord-gateway/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/keakon/chord-gateway?display_name=release)](https://github.com/keakon/chord-gateway/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/keakon/chord-gateway)](https://go.dev/)
[![License](https://img.shields.io/github/license/keakon/chord-gateway)](./LICENSE)

`chord-gateway` 用于把微信、飞书、Telegram、企业微信、钉钉、Discord、Slack 等 IM 平台连接到本地 `chord headless` 进程。你可以在聊天窗口里远程控制 Chord，同时让 agent 进程、工作区访问、凭据和状态继续保留在自己的机器或服务器上。

- English: [README.md](./README.md)
- 文档站: <https://keakon.github.io/chord-gateway/zh/>
- 完整文档: [docs/index_CN.md](./docs/index_CN.md)
- 运行要求：Go 1.27.0+，以及支持[当前 headless 协议](./docs/compatibility_CN.md)的 `chord` 构建

## 功能特性

- 微信 iLink：扫码登录与 token 持久化（默认 `<state_dir>/wechat/token.json`）
- 飞书机器人：长连接、owner 配对、消息与卡片统一接纳、消费回执核查
- Telegram 机器人：Bot API 长轮询、私聊与群纯文本、有界内存收件
- 企业微信智能机器人 API 长连接，支持私聊、群文本和主动回传后台结果（[接入指南](./docs/wecom_CN.md)）
- 钉钉企业机器人通过 Stream 收件、主动文本回传，并验证私聊身份和绑定目标；[部署验收仍待完成](./docs/dingtalk_CN.md)
- Discord 机器人：私聊，以及服务器文字频道和既有公开/私有 thread 中的 @Bot 文本消息，无需公网入口或 Message Content privileged intent
- Slack：Socket Mode、私聊/频道/thread 独立绑定、统一 `/chord` 命令
- 支持同时运行多个 IM adapter
- 按聊天会话和工作区隔离 Chord session
- 会话 pin 与恢复命令（`/new`、`/resume`、`/sessions`）
- 用 `/takeover` 在 TUI 与 IM 间移交运行中的会话，保留排队输入，新端保持暂停（macOS/Linux，需要 Chord 支持接管）。
- 某个 IM 过期时，可通过其他 IM 发送登录通知（例如提示重新登录）
- 本地 `chord headless` 子进程生命周期管理与清理
- 可配置可选控制面事件的可见性
- 微信、飞书支持将 Chord 生成的图片转发回发起会话；Telegram 图片输出延期。达到或超过飞书上传上限的图片改发缩小预览，原图仍保留在 Chord

## 安装

通过 Go 安装：

```bash
go install github.com/keakon/chord-gateway@latest
```

或在本地源码目录构建：

```bash
go build -o chord-gateway .
```

请先按 [Chord 官方安装说明](https://github.com/keakon/chord/blob/main/README_CN.md#三步完成设置)单独安装 Chord，并确保 `chord` 在 `PATH` 中可执行；也可以在网关配置中设置 `chord_path`。由于网关直接执行二进制文件，`chord_path` 不支持 shell alias。

验证两个程序均可用：

```bash
chord-gateway --version
chord --version
```

## 快速开始

创建一个最小配置，并把 `workspaces.default.path` 指向你希望 Chord 操作的项目目录。workspace 路径必须以 `/`、`~`、Windows 盘符前缀或 UNC 前缀开头。以下是微信示例：

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
workspaces:
  default:
    path: /path/to/project
chord_path: chord
idle_timeout: 30m
```

启动 gateway：

```bash
chord-gateway -f config.yaml --pair
```

仅支持 YAML 配置文件（`.yaml` 或 `.yml`）。

扫码登录后，在机器人私聊发送 `/bind "/path/to/project" <pairing_code>`，使用终端显示的配对码。成功后发送 `/status` 验证，再发送普通文本任务。之后普通启动无需 `--pair`。

提示：每个聊天需显式绑定；飞书入站当前只接受文本消息。

平台配置和多工作区路由请参见 [QUICKSTART_CN.md](./QUICKSTART_CN.md)；企业微信配置与部署验收见[企业微信指南](./docs/wecom_CN.md)，Discord 配置见 [Discord 指南](./docs/discord_CN.md)。

Telegram 的 token、群 privacy 与部署步骤见[接入指南](./docs/telegram_CN.md)。

## 支持范围 / 已知限制

- 源码构建需要 Go 1.27.0+，并且需要单独安装 `chord` 二进制；`chord-gateway` 不会内置 Chord。
- 当前 CI 会构建这些目标：`darwin/amd64`、`darwin/arm64`、`linux/amd64`、`linux/arm64`、`windows/amd64`。
- 微信、飞书、企业微信、钉钉和 Slack 都通过显式 `chat_bindings` 选择工作区；owner 使用对应平台命令入口分别绑定各目标。
- 飞书多 workspace 路由通过 `ims.feishu.chat_bindings` 把 chat ID 映射到 workspace ID。当前飞书入站处理只接受文本消息，非文本消息会被忽略。
- 企业微信首版仅支持文本，暂不支持媒体、卡片或流式回复；真实企业账号验收仍未完成。
- Slack 首版仅支持文本；频道/thread 任务必须由 owner 显式 @Bot。slash 命令作用于根聊天，thread 控制使用 `@Bot /命令`。详见 [Slack 指南](./docs/slack_CN.md)。
- 钉钉仅支持同企业文本，群命令需 @机器人；应用凭据和主动 API 权限须单独配置。
- Discord 服务器频道和 thread 必须显式 @Bot；每个 thread 独立绑定。原生 slash command、按钮、附件和富文本预览延期，确认与答题使用[文本命令](./docs/discord_CN.md#配对与聊天绑定)。
- `/login [platform]` 仅适用于支持交互式登录续期的适配器；目前该流程适用于微信；飞书、Telegram、企业微信、钉钉、Discord 和 Slack 使用配置凭据。

## 常用 IM 命令

| 命令 | 说明 |
|---|---|
| `/bind "<directory>" [pairing_code]` | 绑定已有目录；首次私聊配对需提供终端配对码 |
| `/status` | 查看当前 Chord 状态 |
| `/cancel` | 取消当前 turn |
| `/allow` / `/deny [reason]` | 批准或拒绝待确认请求 |
| `/answer @<id> <text>` | 按 ID 回答或修正问题；支持数字快捷选择 |
| `/role [编号\|角色名]` | 显示当前主角色与可切换角色；不带参数时展示菜单，带编号或角色名时切换到目标角色 |
| `/todos` | 查看当前 todo 列表 |
| `/new` | 为当前绑定启动新 session |
| `/resume <id>` | 恢复并 pin 指定 session |
| `/takeover <id> [--timeout <duration>]` | 将 TUI 会话接管到当前聊天，并保持暂停 |
| `/takeover status` | 查询当前会话的移交状态 |
| `/sessions` | 列出最近 session |
| `/login [platform]` | 查看支持登录续期的平台；指定平台时启动续期流程（例如 `/login wechat`） |

完整命令行为和会话语义见 [docs/usage_CN.md](./docs/usage_CN.md)。

## 文档

- [快速开始](./QUICKSTART_CN.md)
- [Cookbook](./docs/cookbook_CN.md)
- [IM 接入总览](./docs/im_CN.md)
- [钉钉企业机器人](./docs/dingtalk_CN.md)
- [使用指南](./docs/usage_CN.md)
- [配置参考](./docs/configuration_CN.md)
- [运维说明](./docs/operations_CN.md)
- [事件可见性](./docs/event-visibility_CN.md)
- [权限与安全边界](./docs/permissions-and-safety_CN.md)
- [故障排查](./docs/troubleshooting_CN.md)
- [变更日志](./CHANGELOG_CN.md)
- English docs: [docs/index.md](./docs/index.md)

## 安全提示

`chord-gateway` 会把 IM 消息路由到本地 `chord headless` 进程，而该进程可以与配置的工作区交互。请把 IM 访问视为对该工作区的控制面访问。

通过配对登记的 owner 是受信任的本机控制面用户。请勿把 Slack token、Discord bot token、飞书密钥、企业微信 secret、钉钉 client_secret、微信 token 或网关状态目录提交到版本控制。详见[权限与安全](./docs/permissions-and-safety_CN.md)。

## License

MIT License。见 [LICENSE](./LICENSE)。内置 Discord Gateway 代码在 `internal/discordgateway/` 保留上游 Apache-2.0 许可证。

所有平台共用有界内存收件、授权和投递回执。Gateway 重启后不恢复收件队列、去重记录或轮询游标；结果不确定时，先核查 Chord 原会话再决定是否重发。详见[投递与重启语义](./docs/operations_CN.md#消息投递与重启)。
