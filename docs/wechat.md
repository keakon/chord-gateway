# WeChat iLink Guide

This guide walks you through setting up **WeChat iLink** (`ims.wechat`) for `chord-gateway`.

If you want a quick minimal config example, see [Quickstart](../QUICKSTART.md). For field-by-field configuration docs, see [Configuration reference](./configuration.md).

## Before you start

Prepare these things first:

- A local project directory that Chord may operate on
- A working `chord` binary in `PATH`, or a known `chord_path`
- A WeChat account that you can use for QR login
- A place to read gateway logs (terminal stderr or `<state_dir>/gateway.log`)

Recommended first-run strategy for beginners:

1. Start with exactly **one workspace**.
2. Use the default WeChat state paths first; customize them only if you really need to.
3. Confirm `/status` works before adjusting token paths or adding more IM platforms.

## What this integration is

- The gateway integrates with **WeChat iLink Bot API**.
- Login is done via **QR code**: the gateway prints a QR-login URL and you scan it in WeChat.
- Your login session is persisted locally as a **token file**.

## Minimal configuration

With a single workspace you can omit `workspace_id`:

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
workspaces:
  default:
    path: /path/to/project
```

A slightly more realistic starter config:

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

If you have multiple workspaces and still enable WeChat, you must choose which one WeChat uses:

```yaml
ims:
  wechat:
    workspace_id: project-a
workspaces:
  project-a:
    path: ~/work/project-a
  project-b:
    path: ~/work/project-b
```

## First login (QR)

1. Start the gateway:

   ```bash
   chord-gateway -f config.yaml
   ```

2. On first run (or when token is missing/expired), the gateway prints a **QR-login URL**.
3. Open that link and scan the QR code with WeChat.
4. After login, send `/status` from the WeChat chat to confirm the binding.

Treat `/status` as the first success checkpoint. A good first-run outcome is:

- the gateway starts without config errors
- QR login completes successfully
- `/status` replies from the same WeChat chat

If QR login does not appear, check:

1. Is `ims.wechat.base_url` reachable from this machine?
2. Are you reading the correct terminal or `<state_dir>/gateway.log`?
3. Is an old broken token file present at `<state_dir>/wechat/token.json` or your custom `token_path`?

## Runtime state: token and sync cursor

WeChat integration keeps two important runtime state files under the gateway state directory:

- **Token file** (credential-like):
  - default: `<state_dir>/wechat/token.json`
  - override: `ims.wechat.token_path`

- **Sync cursor** (non-credential, but still sensitive metadata):
  - path: `<state_dir>/wechat/sync-buf.json`
  - purpose: stores `get_updates_buf` so the gateway can resume polling without reprocessing old updates.

Operational notes:

- Both files are written with atomic replacement to avoid corruption on crashes.
- Do not commit them to version control.
- If you move deployments, copy both files if you want to preserve continuity.
- Do not point multiple gateway instances at the same WeChat state files unless you fully understand the risk of conflicting token / cursor updates.

## Renew / re-login

### Automatic behavior

When the gateway detects the WeChat session is expired, it:

- clears the saved token
- starts a QR-login flow automatically

### Manual renewal from another IM

In **multi-IM mode** (for example, you also configured Feishu), you can renew WeChat login by sending:

```text
/login wechat
```

The gateway returns a QR-login link. Scan it to refresh the token without restarting.

### Force a fresh login

1. Stop the gateway.
2. Remove the token file:
   - `<state_dir>/wechat/token.json` (or your custom `ims.wechat.token_path`)
3. Start the gateway again.

## Limitations and risk notes

- WeChat routes to **one workspace only**.
- WeChat iLink integration depends on an external iLink API and may be less stable than pure bot platforms.
  - Avoid using your main WeChat account for testing if stability/risk is a concern.

## Troubleshooting

- If no QR login appears: see [Troubleshooting — WeChat](./troubleshooting.md#wechat-issues)
- If token expired: see [Troubleshooting — Token expired](./troubleshooting.md#token-expired)
