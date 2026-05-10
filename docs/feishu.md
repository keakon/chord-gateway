# Feishu Guide

This guide walks you through setting up **Feishu** (`ims.feishu`) for `chord-gateway`.

- If you want the minimal config quickly, see [Quickstart](../QUICKSTART.md).
- For field-by-field config docs, see [Configuration reference](./configuration.md).

## Before you start

Prepare these things first:

- A local project directory that Chord may operate on, for example `/path/to/project`
- A working `chord` binary in `PATH`, or a known `chord_path`
- A Feishu app that you can edit in the developer console
- A place to read gateway logs (terminal stderr or `<state_dir>/gateway.log`)

Recommended first-run strategy for beginners:

1. Start with **one workspace**.
2. Start without `owner_open_id` / `allowed_open_ids` so you can confirm the bot works first.
3. After you see the first successful message in logs, tighten access control.

## What this integration is

- The gateway uses a **Feishu app** (`app_id` / `app_secret`).
- Inbound events are received via **long connection** (WebSocket).
  - No public webhook URL is needed.
  - Do **not** configure webhook-mode fields (`verification_token`, `encrypt_key`, etc.).

## Step 0: Create (or pick) a Feishu app

In the Feishu Open Platform developer console:

1. Create an app.
2. Enable the **Bot** capability (your app must have a bot to send/receive messages).
3. Find and copy the app credentials:
   - **App ID** → `ims.feishu.app_id`
   - **App Secret** → `ims.feishu.app_secret`

Beginner checklist:

- Make sure you are editing the **same app** whose `app_id` / `app_secret` you will place into `config.yaml`.
- If your org requires review/approval for app changes, complete that process before testing event delivery.
- Keep the app secret out of IM chats, screenshots, and shell history where possible.

## Step 1: Configure events (long connection)

In the Feishu console under “Events and callbacks”:

1. Select **Receive events via long connection**.
2. In event subscriptions, subscribe to:
   - `im.message.receive_v1`

Notes:

- In group chats, make sure the bot is actually **added to the group**, otherwise it will not receive messages there.
- The gateway currently only processes **text** (including `text` and `post`); images/files are ignored.

## Step 2: Enable interactive cards (recommended)

`chord-gateway` can show interactive cards for confirm/question flows.

In the Feishu console callback configuration, add:

- `card.action.trigger`

Without this callback, card button clicks cannot be delivered to the gateway.

## Step 3: Permissions, scopes, and publishing

Feishu requires permissions (scopes) for both receiving events and sending messages.

### 3.1 Minimal scopes (recommended starting point)

In the Feishu console under **Permissions & Scopes**, grant **at least**:

- **Send messages as bot** (required for gateway replies)
  - typically one of: `im:message` / `im:message:send_as_bot`

- **Receive message events** (required for `im.message.receive_v1`)
  - choose the smallest set that matches your usage:
    - If you only use **DM** with the bot: `im:message.p2p_msg:readonly`
    - If you use **group chats** and plan to @mention the bot: `im:message.group_at_msg:readonly`
    - If you want to receive **all group messages** (more powerful, often treated as sensitive): `im:message.group_msg`

Notes:

- Feishu allows subscribing to `im.message.receive_v1` as long as **any one** of the above “receive message” scopes is granted.
- Tenant policies vary. The console may require admin approval for some scopes.

### 3.2 Publish after any change (easy to miss)

After changing **capabilities / permissions / event subscriptions**, you must **publish a new app version**. Otherwise the gateway may keep receiving nothing even though the console looks correctly configured.

### 3.3 How to recognize missing permissions

Common symptoms:

- The gateway establishes long connection, but never logs `feishu: received message`.
- The gateway logs API errors when sending messages, e.g. `feishu API error: code=... msg=...`.

Fix:

1. Add the missing scope in the console.
2. Publish a new app version.
3. Restart the gateway.


## Step 4: Configure `chord-gateway`

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

A slightly more realistic starter config:

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
workspaces:
  default:
    path: /path/to/project
chord_path: chord
idle_timeout: 30m
```

Start the gateway:

```bash
chord-gateway -f config.yaml
```

What to expect on startup:

- the gateway starts normally without `load config` errors
- Feishu long connection is established
- logs are written to stderr and also to `<state_dir>/gateway.log`

## Step 5: Verify inbound events and obtain `open_id`

1. Send a **plain-text** message to the bot (DM or group).
2. Check gateway logs for a line like:

```text
msg="feishu: received message" chat_id=oc_xxx open_id=ou_xxx message_id=om_xxx content=hello
```

3. Treat this log line as the first success checkpoint:
   - `chat_id=oc_xxx` tells you which Feishu chat the gateway saw
   - `open_id=ou_xxx` tells you who sent the message
4. If you plan to deploy beyond local testing, lock down access:

```yaml
ims:
  feishu:
    owner_open_id: ou_xxx
```

Then restart the gateway.

If you do **not** see the log line, check in this order:

1. Is the app using **long connection**, not webhook mode?
2. Did you subscribe to `im.message.receive_v1`?
3. Did you **publish** after the last console change?
4. Is the bot actually in the DM / group where you sent the message?
5. Did you send **plain text**, not an image/file/sticker?

## Multi-workspace routing with `/bind`

If you have multiple workspaces, configure per-chat routing via `chat_bindings`.

Recommended workflow:

1. Start with one workspace and no `chat_bindings`.
2. Create the target Feishu group chat, add the bot, and send a plain-text message.
3. In that same chat, run:

```text
/bind <workspace_id> <path>
```

Example:

```text
/bind project-a ~/work/project-a
```

4. The gateway updates only `ims.feishu.chat_bindings` and `workspaces`, and writes those sections back to YAML.
5. Re-open `config.yaml` and confirm the expected mapping was written.

Important boundaries:

- `/bind` does not update allowlists or other adapter settings.
- Manual edits outside `/bind` still require a gateway restart.

## Known limitations

- Inbound handling is **text-only**. Non-text messages are ignored.

## Troubleshooting

- Long connection not established / no events: see [Troubleshooting — Feishu](./troubleshooting.md#feishu-issues)
