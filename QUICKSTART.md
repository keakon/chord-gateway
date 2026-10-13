# Quick Start

All IM platforms use one paired owner and explicit chat bindings, including single-workspace deployments. Run `chord-gateway --pair`, then send `/bind "~/workspace" <pairing_code>` in a private chat with the bot. See [pairing and local reset](./docs/pairing.md) and the [upgrade notes](./CHANGELOG.md).

Use this guide to connect one project to either WeChat or Feishu. You can register directories from the chat after pairing.

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

## 3. Pair in a private chat

Run in a foreground terminal:

```bash
chord-gateway -f config.yaml --pair
```

Complete WeChat QR login if prompted. After the platform connects, the terminal displays its pairing code. In a private chat with the bot, send:

```text
/bind "~/workspace" <pairing_code>
```

Use an existing directory on the gateway host. The code expires after 10 minutes. The gateway saves the owner and binding; send a task to start Chord there. Other senders are ignored. After pairing, restart normally with `chord-gateway -f config.yaml`, including as a background service.

## 4. Bind another chat or directory

The owner can send `/bind "/path/to/project"` in another chat or in an idle existing chat, without a code. Paired chats use explicit routes on both platforms. Feishu groups can be bound by the owner; group members can see replies. Finish active work and interactions before switching directories.

See [pairing and local reset](./docs/pairing.md) for failed-write recovery, account changes, and reset.

## 5. State and next steps

By default the gateway stores runtime state under:

- macOS / Linux: `${XDG_STATE_HOME:-~/.local/state}/chord-gateway`
- Config file: `${XDG_CONFIG_HOME:-~/.config}/chord-gateway/config.yaml`

State includes logs, WeChat token files (`<state_dir>/wechat/token.json` by default), Feishu dedupe data, and session pins. Feishu `app_id`/`app_secret` remain configuration credentials; its short-lived access token is kept in memory and refreshed as needed.

- [Configuration reference](./docs/configuration.md)
- [Cookbook](./docs/cookbook.md)
- [IM integration overview](./docs/im.md)
- [Usage guide](./docs/usage.md)
- [Operations](./docs/operations.md)
- [Troubleshooting](./docs/troubleshooting.md)
