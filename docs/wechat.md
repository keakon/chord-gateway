# WeChat iLink Guide

All IM platforms use one paired owner and explicit chat bindings, including single-workspace deployments. Run `chord-gateway --pair`, then send `/bind "~/workspace" <pairing_code>` in a WeChat/Feishu/WeCom/DingTalk private bot chat. See [pairing and local reset](./pairing.md) and the [upgrade notes](../CHANGELOG.md). Slack uses `/chord bind "~/workspace" <pairing_code>` in the App DM; see the [Slack guide](./slack.md).

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

Before first pairing, only the WeChat adapter is needed:

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
workspaces:
  default:
    path: /path/to/project
```

When direct access is blocked, set `proxy_url` globally or under `ims.wechat`. See [configuration](./configuration.md#proxy).

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

Each chat requires explicit binding; see [pairing](./pairing.md).

## First login (QR)

1. Start the gateway:

   ```bash
   chord-gateway -f config.yaml --pair
   ```

2. On first run (or when token is missing/expired), the gateway prints a **QR-login URL**.
3. Open that link and scan the QR code with WeChat.
4. After login, pair in a private bot chat with `/bind "~/workspace" <pairing_code>`, using the terminal code and an existing directory. Then send `/status`.

Treat `/status` as the first success checkpoint. A good first-run outcome is:

- the gateway starts without config errors
- QR login completes successfully
- `/status` replies from the same WeChat chat

If QR login does not appear, check:

1. Is `ims.wechat.base_url` reachable from this machine?
2. Are you reading the correct terminal or `<state_dir>/gateway.log`?
3. Is an old broken token file present at `<state_dir>/wechat/token.json` or your custom `token_path`?

The gateway deduplicates inbound messages by the server-assigned `message_id` (or an item’s `msg_id` when the top-level ID is absent), scoped to the bot account and sender. Numeric and string-encoded uint64 IDs retain their full precision. An outbound `client_id` is not used as an inbound message ID.

## Runtime state and upgrades

The login token stays at its default path or `ims.wechat.token_path`. Intake, deduplication, polling cursors and short-lived reply `context_token` credentials stay in memory; old deduplication and cursor files are not imported. Before upgrading, resolve pending input and back up configuration, tokens, session pins and transfer guards; see [operations](./operations.md#upgrades-and-stopping-the-service).

The in-memory cursor advances only after every supported message in a batch is accepted, duplicated or definitely refused; temporary refusal retains the batch. Restart clears the queue and cursor, so input may be lost or redelivered without cross-restart deduplication. Same-account renewal preserves the cursor within this process; new accounts never inherit old authorization or contexts. Only one instance per account may run on the same host and OS user.

## Waiting for reply context

Reply context starts empty; a fresh owner message or `/status` can establish it. If context expires during the process lifetime, pending tasks wait in their original queue and `/status` shows `reply_context_missing`. New context does not extend the original 24-hour delivery window. Restart does not restore this queue.

Inspect uncertain results in the original Chord session. Lost context can prevent delivery of completed output; Gateway does not redo tasks or resend old results. See [troubleshooting](./troubleshooting.md#wechat-tasks-waiting-or-polling-paused) for oversized polling responses.

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

- Each WeChat chat routes to one workspace at a time. The owner can switch its directory with `/bind`.
- WeChat iLink integration depends on an external iLink API and may be less stable than pure bot platforms.
  - Avoid using your main WeChat account for testing if stability/risk is a concern.

## Troubleshooting

- If no QR login appears: see [Troubleshooting — WeChat](./troubleshooting.md#wechat-issues)
- If token expired: see [Troubleshooting — Token expired](./troubleshooting.md#token-expired)
