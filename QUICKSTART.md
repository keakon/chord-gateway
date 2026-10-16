# Quick Start

All IM platforms use one paired owner and explicit chat bindings, including single-workspace deployments. Run `chord-gateway --pair`, then send `/bind "<directory>" <pairing_code>` in a WeChat/Feishu/WeCom/DingTalk private bot chat. See [pairing and local reset](./docs/pairing.md) and the [upgrade notes](./CHANGELOG.md). Slack uses `/chord bind "<directory>" <pairing_code>` in the App DM; see the [Slack guide](./docs/slack.md).

Use this guide to connect one project to WeChat, Feishu, Telegram, WeCom, DingTalk, Discord, or Slack. You can register directories from the chat after pairing.

## 1. Install Chord and the gateway

Install [Chord](https://github.com/keakon/chord#three-step-setup), complete its setup wizard, and verify it first:

```bash
chord --version
```

Then install the gateway with Go 1.27.0 or later:

```bash
go install github.com/keakon/chord-gateway@latest
chord-gateway --version
```

For prebuilt binaries, source builds, and service setup, see the [Cookbook](./docs/cookbook.md).

## 2. Choose a platform

Save one of these minimal configurations as `config.yaml`.

For WeChat:

```yaml
ims:
  wechat: {}
```

For Feishu:

```yaml
ims:
  feishu:
    app_id: cli_example
    app_secret: your-app-secret
```

For Feishu, enable long connection mode, subscribe to `im.message.receive_v1`, add `card.action.trigger` for interactive cards, and publish the app. See the [Feishu guide](./docs/feishu.md) for permissions and console steps.

Telegram:

```yaml
ims:
  telegram:
    bot_token: your-bot-token
```

Create the bot in BotFather and keep default group privacy enabled; see the [Telegram guide](./docs/telegram.md). Existing webhooks block startup and are never automatically deleted.

For WeCom:

```yaml
ims:
  wecom:
    bot_id: your-bot-id
    secret: your-bot-secret
```

Enable the intelligent bot **API mode → long connection**, allow private chat in its available scope, and follow the [WeCom guide](./docs/wecom.md) for product access and deployment acceptance.

For a DingTalk enterprise internal-app robot:

```yaml
ims:
  dingtalk:
    client_id: your-client-id
    client_secret: your-client-secret
    robot_code: your-robot-code
    corp_id: your-corp-id
```

Enable Stream robot reception, grant private/group proactive sending permissions, and publish the app. See [DingTalk](./docs/dingtalk.md); real-enterprise acceptance remains pending.

For Discord:

```yaml
ims:
  discord:
    bot_token: your-bot-token
    application_id: "123456789012345678"
```

Install the bot with View Channel, Send Messages, and Send Messages in Threads. Keep privileged intents disabled; see the [Discord guide](./docs/discord.md) for installation and private-thread access.

For Slack:

```yaml
ims:
  slack:
    app_token: your-app-token
    bot_token: your-bot-token
    app_id: your-app-id
    team_id: your-team-id
```

Enable Socket Mode, install the Bot scopes, subscribe to `message.im` / `app_mention`, and register `/chord`. Follow the [Slack guide](./docs/slack.md) before pairing.

No IM platform needed? For local use, `chord-gateway -f config.yaml --console` runs against exactly one workspace with no pairing or binding. See the [IM integration overview](./docs/im.md#local-console-mode).

## 3. Pair in a private chat

Run in a foreground terminal:

```bash
chord-gateway -f config.yaml --pair
```

Complete WeChat QR login if prompted. The terminal displays a single pairing code immediately at startup; each platform must still become ready before its `/bind` is authorized, and for the first DingTalk connection an employee must first privately send `hello` to verify the enterprise identity. In a private bot chat on a platform other than Slack, send:

```text
/bind "<directory>" <pairing_code>
```

Use an existing directory on the gateway host. The code expires after 10 minutes. WeCom first requires a successful proactive binding probe. The gateway saves the owner and binding; send a task to start Chord there. Other senders are ignored. After pairing, restart normally with `chord-gateway -f config.yaml`, including as a background service.

Slack App DM pairing uses `/chord bind "<directory>" <pairing_code>` and verification uses `/chord status`. Each channel and thread needs a separate binding; use `@Bot /bind ...` inside a thread, because slash commands always address the channel root.

## 4. Bind another chat or directory

The owner can bind another chat or an idle existing chat without a code: use `/bind "/path/to/project"` on WeChat/Feishu/Telegram/WeCom/DingTalk, `/chord bind "/path/to/project"` in a Slack root, or `@Bot /bind "/path/to/project"` inside a Slack thread. Paired chats use explicit routes on all platforms. Feishu, Telegram, WeCom, and DingTalk groups (DingTalk requires @robot) can be bound by the owner; group members can see replies. For Telegram groups use `/bind@BotUsername` and reply to the bot for ordinary tasks with default privacy. Finish active work and interactions before switching directories. DingTalk sends `Gateway binding check.` before saving a binding; a failed or unknown probe leaves it unsaved until a new `/bind`. Discord server channels and threads use `@Bot /bind "/path/to/project"`; prefix later commands with the bot mention.

See [pairing and local reset](./docs/pairing.md) for failed-write recovery, account changes, and reset.

## 5. State and next steps

By default the gateway stores runtime state under:

- macOS / Linux: `${XDG_STATE_HOME:-~/.local/state}/chord-gateway`
- Config file: `${XDG_CONFIG_HOME:-~/.config}/chord-gateway/config.yaml`

Discord bot tokens remain in configuration; protect the configuration as a secret. WeCom setup uses bot ID and secret and requires a successful proactive binding probe; see the [WeCom guide](./docs/wecom.md).

State includes logs, WeChat token files (`<state_dir>/wechat/token.json` by default), and session pins. Feishu `app_id`/`app_secret` remain configuration credentials; its short-lived access token is kept in memory and refreshed as needed. Inbound intake, deduplication and polling cursors are memory-only and are not restored after restart.

- [Configuration reference](./docs/configuration.md)
- [Cookbook](./docs/cookbook.md)
- [IM integration overview](./docs/im.md)
- [Usage guide](./docs/usage.md)
- [Operations](./docs/operations.md)
- [Troubleshooting](./docs/troubleshooting.md)

All platforms share bounded in-memory intake, authorization and delivery receipts. Gateway does not restore its inbox, deduplication or polling cursors after restart. Check the original Chord session before resending an uncertain task. See [delivery semantics](./docs/operations.md#message-delivery-and-restarts).
