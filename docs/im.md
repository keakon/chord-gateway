# IM Integration Overview

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

## Quick paths

### A) WeChat only (minimal)

1. Create a config with `ims.wechat` and one workspace.
2. Start the gateway.
3. Scan the printed QR-login URL.
4. Send `/status` from the WeChat chat.

Minimal config:

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
workspaces:
  default:
    path: /path/to/project
```

First success checkpoint:

- QR login completes
- `/status` replies in the same WeChat chat

Detailed steps: [WeChat iLink guide](./wechat.md)

### B) Feishu only (minimal)

1. Create a Feishu app and obtain `app_id` / `app_secret`.
2. Enable **long connection** event delivery and subscribe to `im.message.receive_v1`.
3. Start the gateway and send a text message (`text` or `post`) to the bot.

Minimal config:

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
workspaces:
  default:
    path: /path/to/project
```

First success checkpoint:

- gateway logs `feishu: received message`
- you can read `chat_id` and `open_id` from that log line

Detailed steps: [Feishu guide](./feishu.md)

## Routing model (important)

The gateway routes by IM platform:

- **WeChat** routes to a single workspace using `ims.wechat.workspace_id`.
- **Feishu** routes per chat using `ims.feishu.chat_bindings`.

If there is exactly one workspace, both `workspace_id` (WeChat) and `chat_bindings` (Feishu) may be omitted.

## `/bind` in one sentence

`/bind <workspace_id> <path>` (from a Feishu chat) updates **only**:

- `ims.feishu.chat_bindings`
- `workspaces`

…and persists those two sections back to your YAML config file. It does **not** reload other fields (such as allowlists). Manual edits still require a gateway restart.

## Security notes (minimum)

- Treat IM senders as control-plane users for the configured workspace.
- For Feishu beyond local testing, configure `owner_open_id` and/or `allowed_open_ids`.
- Do not commit gateway state or secrets. See: [Permissions & safety](./permissions-and-safety.md)

## Troubleshooting entry points

- WeChat issues: [Troubleshooting](./troubleshooting.md#wechat-issues)
- Feishu issues: [Troubleshooting](./troubleshooting.md#feishu-issues)
