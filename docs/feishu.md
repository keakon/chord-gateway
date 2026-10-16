# Feishu Guide

All IM platforms use one paired owner and explicit chat bindings, including single-workspace deployments. Run `chord-gateway --pair`, then send `/bind "~/workspace" <pairing_code>` in a WeChat/Feishu/WeCom/DingTalk private bot chat. See [pairing and local reset](./pairing.md) and the [upgrade notes](../CHANGELOG.md). Slack uses `/chord bind "~/workspace" <pairing_code>` in the App DM; see the [Slack guide](./slack.md).

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
3. Run with `--pair` in the foreground, bind a directory in a private bot chat using the terminal code, then send `/status`.

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
- The gateway currently only processes **text** (including `text` and `post`); inbound images/files are ignored. Generated images are delivered outbound as described in [Event visibility](./event-visibility.md#generated-images).

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

DM-only setup, including generated-image delivery:

```json
{
  "scopes": {
    "tenant": [
      "im:message:send_as_bot",
      "im:resource",
      "im:message.p2p_msg:readonly"
    ],
    "user": []
  }
}
```

The `im:resource` scope permits uploading generated images. You may omit it if you do not use image delivery. After adding this permission to an existing app, publish a new app version. Image generation must be enabled in Chord, and the Chord build must support the image delivery contract in [Compatibility](./compatibility.md).

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

- The gateway establishes long connection, but receives no text or card events; check subscriptions and scopes in the Feishu console.
- DMs work but group chats produce nothing: the DM receive scope was granted without the matching group scope.
- The gateway logs API errors when sending messages, e.g. `feishu API error: code=... msg=...`.
- Confirmation/question buttons work, but the card never changes to the resolved state: usually a card update failure (only messages sent within 14 days can be updated).

Fix:

1. Add the missing scope in the console.
2. Publish a new app version.
3. Restart the gateway.


## Step 4: Configure and pair

Save a minimal configuration:

```yaml
ims:
  feishu:
    app_id: cli_example
    app_secret: your-app-secret
```

When direct access is blocked, set `proxy_url` globally or under `ims.feishu`. See [configuration](./configuration.md#proxy).

Run `chord-gateway --config config.yaml --pair` in a foreground terminal. After connection, the terminal displays a pairing code valid for 10 minutes. In a **private chat** with the bot, send:

```text
/bind "~/work/project-a" <pairing_code>
```

The directory must already exist. The gateway saves the owner, workspace, and chat binding; send `/status` to verify. Later starts need no `--pair`. If no events arrive, check long connection, subscriptions, scopes, and app publication.

## Multi-workspace routing and `/bind`

The paired owner can add the bot to a group and send `/bind "~/work/project-b"` there. Chats may use different directories or share one directory. Only owner input executes; group members can see replies. Even a single workspace requires explicit binding. Binding never replays previously unbound input.

Finish pending work, inputs, and interactions before switching directories. See [pairing](./pairing.md) for account changes, reset, and failed-write recovery.

## Intake and process-local deduplication

Text/post and authenticated cards share in-memory intake and Chord consumption receipts. Messages and callbacks are deduplicated within this process; Gateway restart has no deduplication history or recovery queue. Old deduplication files are not read or imported. App ID, owner, bindings and session pins retain their configuration.

Ordinary and topic groups use group authorization. Non-Question cards update after the command reaches its terminal outcome; platform acceptance is not completed execution. Stale control targets, expired callbacks and failed pin restoration never silently redirect input to a new session. Inspect `/status` and the original Chord session for uncertain results. See [delivery semantics](./operations.md#message-delivery-and-restarts).

## Known limitations

- Inbound handling currently accepts only **text messages** (`text` and `post`). Non-text messages are ignored.

## Troubleshooting

- Long connection not established / no events: see [Troubleshooting — Feishu](./troubleshooting.md#feishu-issues)
