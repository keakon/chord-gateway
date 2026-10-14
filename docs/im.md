# IM Integration Overview

All IM platforms use one paired owner and explicit chat bindings, including single-workspace deployments. Run `chord-gateway --pair`, then send `/bind "~/workspace" <pairing_code>` in a private chat with the bot. See [pairing and local reset](./pairing.md) and the [upgrade notes](../CHANGELOG.md).

This page is a task-oriented entry point for connecting IM platforms to `chord-gateway`.

If you are looking for field-by-field configuration documentation, see:

- [Configuration reference](./configuration.md)

## What IM platforms are supported?

`chord-gateway` currently supports:

- **WeChat iLink** (`ims.wechat`) — personal WeChat QR login via the iLink Bot API.
- **Feishu** (`ims.feishu`) — Feishu app credentials + long-connection event delivery.

## Which one should you use?

### Choose WeChat iLink when

- You specifically need **personal WeChat** as the chat surface.
- You are OK with **single-workspace routing** (all WeChat traffic must go to one workspace).

Start here: [WeChat iLink guide](./wechat.md)

### Choose Feishu when

- You want **multi-workspace routing** (different Feishu group chats map to different workspaces).
- You need a more “bot-friendly” platform for interactive confirm/question flows.

Start here: [Feishu guide](./feishu.md)

## Local console mode

To use Chord locally without connecting any IM platform, start with `--console`:

```bash
chord-gateway -f config.yaml --console
```

- `--console` cannot be combined with `--pair` and requires **exactly one** workspace. The `ims` section is ignored and no platform credentials are used.
- No pairing or chat binding is needed: the single workspace is used directly, and `/bind` is not available (it prints usage instead).
- Each non-empty line on stdin is handled as one command or task, for example `/status`; replies are printed to stdout as `[console] <text>`.
- Console input is a local control plane and bypasses the paired-owner check; run it only on a trusted host.

See [pairing](./pairing.md) and the [configuration reference](./configuration.md).

## Connection and routing

Configure `ims.wechat: {}`, or Feishu `app_id` / `app_secret` with long connection and event subscriptions. Run `chord-gateway --pair`. Complete WeChat QR login if prompted; the terminal displays a code after each platform connects. Send `/bind "~/workspace" <pairing_code>` in a private bot chat, then `/status` to verify.

The paired owner uses `/bind "~/another workspace"` to explicitly bind more chats. Both platforms route through `chat_bindings`, with no single-workspace fallback. Directories must exist; routes become active only after configuration is saved. Stop the service before editing other configuration, then restart.

The owner is a local control-plane user. Bind only trusted groups because replies are visible to their members. Never commit credentials or runtime state. See [pairing](./pairing.md), [WeChat](./wechat.md), [Feishu](./feishu.md), and [permissions and safety](./permissions-and-safety.md).

## Troubleshooting entry points

- WeChat issues: [Troubleshooting](./troubleshooting.md#wechat-issues)
- Feishu issues: [Troubleshooting](./troubleshooting.md#feishu-issues)
