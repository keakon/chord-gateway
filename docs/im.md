# IM Integration Overview

All IM platforms use one paired owner and explicit chat bindings, including single-workspace deployments. Run `chord-gateway --pair`, then send `/bind "<directory>" <pairing_code>` in a WeChat/Feishu/Telegram/WeCom/DingTalk/Discord private bot chat. See [pairing and local reset](./pairing.md) and the [upgrade notes](../CHANGELOG.md). Slack uses `/chord bind "<directory>" <pairing_code>` in the App DM; see the [Slack guide](./slack.md).

This page is a task-oriented entry point for connecting IM platforms to `chord-gateway`.

If you are looking for field-by-field configuration documentation, see:

- [Configuration reference](./configuration.md)

## What IM platforms are supported?

`chord-gateway` currently supports:

- **WeChat iLink** (`ims.wechat`) — personal WeChat QR login via the iLink Bot API.
- **Feishu** (`ims.feishu`) — Feishu app credentials + long-connection event delivery.
- **Telegram** (`ims.telegram`) — BotFather token + Bot API long polling for private and ordinary group text.

- **WeCom** (`ims.wecom`) — intelligent bot API long connection for private/group text and background results.
- **DingTalk** (`ims.dingtalk`) — enterprise internal-app robot, Stream reception and proactive text replies; real-enterprise acceptance remains pending.
- **Discord** (`ims.discord`) — bot DMs and explicit @Bot messages in server text channels and existing threads.
- **Slack** (`ims.slack`) — single-workspace Socket Mode for DMs, channels and threads.

WeCom, DingTalk, and Discord implementations and automated tests do not complete deployment acceptance. Real-account acceptance remains pending; follow the [WeCom checklist](./wecom.md#deployment-acceptance), [DingTalk deployment checklist](./dingtalk.md#deployment-acceptance-checklist), or [Discord deployment checks](./discord.md#troubleshooting-and-deployment-checks) before deployment.

## Which one should you use?

### Choose WeChat iLink when

- You specifically need **personal WeChat** as the chat surface.
- You want private bot chats; this integration does not add WeChat group support.

Start here: [WeChat iLink guide](./wechat.md)

### Choose Feishu when

- You want **multi-workspace routing** (different Feishu group chats map to different workspaces).
- You need a more “bot-friendly” platform for interactive confirm/question flows.

Start here: [Feishu guide](./feishu.md)

### Choose Telegram when

- You need private or group Telegram chats and text commands for confirmations and questions.
- You want deployment without a public callback, sending tasks by replying to the bot with default privacy.

Start here: [Telegram integration](./telegram.md)

### Choose WeCom when

Your organization has enabled the intelligent bot API long connection and you want to control Chord through WeCom private or group chats. This release uses text confirmations and questions and proactively returns long-task and background results.

Start here: [WeCom integration guide](./wecom.md)

### Choose DingTalk when

- Your team uses one DingTalk enterprise and can configure an internal-app robot and proactive API permissions.
- You use employee private chats or group @robot text for tasks, confirmations, and Questions.

Start here: [DingTalk guide](./dingtalk.md). The first version is same-enterprise text only, with deployment acceptance still pending.

### Choose Discord when

- You want separate server channels or threads for different projects.
- You prefer text commands and an outbound connection without a public endpoint.

Start here: [Discord guide](./discord.md)

### Choose Slack

Use Slack when you want separate DM, channel and thread sessions with Socket Mode and a single `/chord` entry point. Channel/thread input requires an owner mention. Start with the [Slack guide](./slack.md).

## Local console mode

To use Chord locally without connecting any IM platform, start with `--console`:

```bash
chord-gateway -f config.yaml --console
```

- `--console` cannot be combined with `--pair` and requires **exactly one** workspace. The `ims` section is not connected and no platform credentials are used.
- No pairing or chat binding is needed: the single workspace is used directly, and `/bind` is not available (it prints usage instead).
- Each non-empty line on stdin is handled as one command or task, for example `/status`; replies are printed to stdout as `[console] <text>`.
- Console input is a local control plane and bypasses the paired-owner check; run it only on a trusted host.

See [pairing](./pairing.md) and the [configuration reference](./configuration.md).

## Connection and routing

Configure the platform you need: `ims.wechat: {}` for WeChat; Feishu `app_id` / `app_secret` with long connection and event subscriptions; WeCom `bot_id` / `secret` for the intelligent bot API long connection; or Discord `bot_token` / `application_id` after installing the bot. Run `chord-gateway --pair`. The terminal displays one pairing code immediately at startup; WeChat still needs QR login and each platform must become ready before its `/bind` is authorized. Send `/bind "<directory>" <pairing_code>` in a private bot chat, then `/status` to verify. WeCom requires the fixed proactive binding probe to be accepted before saving.

DingTalk needs four application/enterprise identity fields. On first connection, privately send `hello` to verify identity; this happens independently of the startup code, and binding targets receive a proactive probe before saving.

The paired owner uses `/bind "~/another workspace"` to explicitly bind more chats. All supported platforms route through `chat_bindings`, with no single-workspace fallback. Directories must exist; routes become active only after configuration is saved. Stop the service before editing other configuration, then restart.

The owner is a local control-plane user. Bind only trusted groups because replies are visible to their members. Never commit credentials or runtime state. See [pairing](./pairing.md), [WeChat](./wechat.md), [Feishu](./feishu.md), [WeCom](./wecom.md), [DingTalk](./dingtalk.md), [Discord](./discord.md), and [permissions and safety](./permissions-and-safety.md).

Configure `ims.telegram.bot_token` for Telegram; group binding uses `/bind@BotUsername`. The server retains unreceived updates for only 24 hours; review [retention and acceptance](./telegram.md) before deployment.

## Troubleshooting entry points

- WeChat issues: [Troubleshooting](./troubleshooting.md#wechat-issues)
- Telegram: [Troubleshooting](./telegram.md#troubleshooting)
- Feishu issues: [Troubleshooting](./troubleshooting.md#feishu-issues)
- [WeCom troubleshooting](./wecom.md#replies-and-failures)
- DingTalk issues: [Limits and troubleshooting](./dingtalk.md#limits-and-troubleshooting)
- Discord issues: [Discord guide](./discord.md#troubleshooting-and-deployment-checks)

- Slack: [Installation checks and troubleshooting](./slack.md#installation-checks-and-troubleshooting)
