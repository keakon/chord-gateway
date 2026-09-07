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
2. Use a private chat or controlled test group for setup.
3. Start once without an allowlist to discover your ID from the rejected-message audit log; messages are not processed until you configure the allowlist and restart.

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

In the Feishu console under **Event Subscriptions**:

1. Select **Use long connection to receive events** (do not use the webhook / developer-server mode).
2. Add the event `im.message.receive_v1`. In the console it is listed as **Receive message v2.0** under the **Messages & Groups** category.

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

In the Feishu console open **Permissions & Scopes**. Instead of ticking permissions
one by one, click **Batch import** and paste the JSON below.

- **Send messages as the app** (required for gateway replies, and also covers the
  card update used to resolve confirmation/question cards). Grant **any one** of:
  - `im:message` — get and send messages in p2p chats and groups
  - `im:message:send_as_bot` — send messages as the app (smallest option)

- **Receive message events** (required for `im.message.receive_v1`). These are
  **not** alternatives — Feishu decides what to push per scenario, so grant one
  scope for **each** scenario you use:
  - **DM with the bot**: `im:message.p2p_msg:readonly`
  - **Group chats, @ the bot** (user messages only): `im:message.group_at_msg:readonly`
  - **Group chats, @ the bot** (including messages from other bots): `im:message.group_at_msg.include_bot:readonly`
  - **All group messages** (more powerful, treated as sensitive): `im:message.group_msg`

> If you use both DMs and group chats, grant **both** the DM scope and the group
> scope. A DM-only app silently receives nothing in group chats.

Smallest DM-only setup:

```json
{
  "scopes": {
    "tenant": [
      "im:message:send_as_bot",
      "im:message.p2p_msg:readonly"
    ],
    "user": []
  }
}
```

If you also use group chats, add `im:message.group_at_msg:readonly` (or
`im:message.group_msg`) to the `tenant` array.

Notes:

- The gateway sends text and interactive cards with `POST /open-apis/im/v1/messages`,
  and resolves a confirmation/question card with `PATCH /open-apis/im/v1/messages/{message_id}`.
  Both accept `im:message` or `im:message:send_as_bot`, so the dedicated
  `im:message:update` scope is **not** required.
- Tenant policies vary. The console may require admin approval for some scopes.

### 3.2 Publish after any change (easy to miss)

After changing **capabilities / permissions / event subscriptions**, you must **publish a new app version**. Otherwise the gateway may keep receiving nothing even though the console looks correctly configured.

### 3.3 How to recognize missing permissions

Common symptoms:

- The gateway establishes long connection, but never logs `feishu: received message`.
- DMs work but group chats produce nothing: the DM receive scope was granted without the matching group scope.
- The gateway logs API errors when sending messages, e.g. `feishu API error: code=... msg=...`.
- Confirmation/question buttons work, but the card never changes to the resolved state: usually a card update failure (only messages sent within 14 days can be updated).

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
    owner_open_id: ou_xxx
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
    owner_open_id: ou_xxx
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

If you do not know your `open_id`, first omit `owner_open_id`, start the gateway, and send a **text message** (`text` or `post`) to the bot. The message will not be processed.

1. Check gateway logs for a line like:

```text
msg="feishu: message from non-allowed open_id, ignoring" open_id=ou_xxx chat_id=oc_xxx
```

2. Treat this content-free audit log as the first success checkpoint:
   - `chat_id=oc_xxx` tells you which Feishu chat the gateway saw
   - `open_id=ou_xxx` tells you who sent the message
3. Stop the gateway and configure the owner (localhost does not restrict who can message the bot):

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
5. Did you send a **text message** (`text` or `post`), not an image/file/sticker?

## Multi-workspace routing with `/bind`

If you have multiple workspaces, configure per-chat routing via `chat_bindings`.

Recommended workflow:

1. Start with one workspace and no `chat_bindings`.
2. Create the target Feishu group chat, add the bot, and send a text message.
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

- Inbound handling currently accepts only **text messages** (`text` and `post`). Non-text messages are ignored.

## Troubleshooting

- Long connection not established / no events: see [Troubleshooting — Feishu](./troubleshooting.md#feishu-issues)
