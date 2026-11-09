# Quick Start

Use this guide to connect one project to WeChat, Feishu, Telegram, WeCom, DingTalk, Discord, or Slack. You can register directories from the chat after pairing.

## 1. Install Chord and the gateway

Install [Chord](https://github.com/keakon/chord#three-step-setup), complete its setup wizard, and verify it first:

```bash
chord --version
```

Install the gateway with Homebrew:

```bash
brew install keakon/tap/chord-gateway
chord-gateway --version
```

Or install from source with Go 1.27.0 or later:

```bash
go install github.com/keakon/chord-gateway@latest
chord-gateway --version
```

For manual binary downloads, source builds, and service setup, see the [Cookbook](./docs/cookbook.md).

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

The terminal immediately displays a pairing code valid for 10 minutes. Wait for your platform to connect, then use an existing directory on the gateway host:

| Platform | Command in the private bot chat | Verify |
|---|---|---|
| Slack App DM | `/chord bind "<directory>" <pairing_code>` | `/chord status` |
| Other platforms | `/bind "<directory>" <pairing_code>` | `/status` |

- **WeChat:** complete QR login when prompted.
- **DingTalk:** a same-enterprise employee must first send a private `hello` to verify identity. Before saving a binding, the gateway sends `Gateway binding check.`; if the probe fails or its result is unknown, fix the problem and send a new `/bind`.
- **WeCom:** the proactive binding probe must receive a successful platform ACK before the binding is saved.

The gateway saves one trusted owner per platform and binds the chat. Other senders are ignored. Send a task to start Chord in the bound directory. After pairing, restart normally with `chord-gateway -f config.yaml`, including as a background service.

> **Note:** Every chat needs an explicit binding, even with only one workspace. See [pairing and local reset](./docs/pairing.md); existing deployments should also read the [upgrade notes](./CHANGELOG.md).

## 4. Bind another chat or directory

Once paired, the owner can bind another chat without a code. Finish active work and pending interactions before switching an existing chat to another directory.

Use the command for the target chat:

| Target | Binding command |
|---|---|
| Private bot chat except Slack | `/bind "/path/to/project"` |
| Feishu or WeCom group | `/bind "/path/to/project"` |
| Telegram group | `/bind@BotUsername "/path/to/project"` |
| DingTalk group | `@robot /bind "/path/to/project"` |
| Discord server channel or thread | `@Bot /bind "/path/to/project"` |
| Slack chat root | `/chord bind "/path/to/project"` |
| Slack thread | `@Bot /bind "/path/to/project"` |

Group members can see replies. With Telegram's default privacy settings, reply to the bot for ordinary tasks. Discord server commands also need the bot mention. Slack slash commands always address the chat root; each Slack or Discord thread needs its own binding.

See [pairing and local reset](./docs/pairing.md) for binding conditions, failed-write recovery, account changes, and reset.

## 5. State and next steps

By default the gateway stores runtime state under:

- macOS / Linux: `${XDG_STATE_HOME:-~/.local/state}/chord-gateway`
- Config file: `${XDG_CONFIG_HOME:-~/.config}/chord-gateway/config.yaml`

Discord bot tokens remain in configuration; protect the configuration as a secret. WeCom setup uses bot ID and secret and requires a successful proactive binding probe; see the [WeCom guide](./docs/wecom.md).

State includes logs, WeChat token files (`<state_dir>/wechat/token.json` by default), and session pins. Feishu `app_id`/`app_secret` remain configuration credentials; its short-lived access token is kept in memory and refreshed as needed.

The inbox, deduplication records, and polling cursors are memory-only and are not restored after restart. Check the original Chord session before resending an uncertain task. See [delivery semantics](./docs/operations.md#message-delivery-and-restarts).

- [Configuration reference](./docs/configuration.md)
- [Cookbook](./docs/cookbook.md)
- [IM integration overview](./docs/im.md)
- [Usage guide](./docs/usage.md)
- [Operations](./docs/operations.md)
- [Troubleshooting](./docs/troubleshooting.md)
