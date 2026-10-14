# Discord bot integration

Control Chord through a bot DM, a server text channel, or an existing public/private thread. Discord connects over an outbound Gateway WebSocket, so you do not need a public endpoint. Server and thread messages must explicitly mention the bot and come from the paired owner. Group DMs, voice channels, and posting to forum roots are outside this release.

Deployment checks with a real Discord account remain pending. Automated protocol tests alone do not establish readiness for unattended deployment.

## Create and install the bot

1. Create an application in the [Discord Developer Portal](https://discord.com/developers/applications), add a bot, and copy its bot token and application ID.
2. Create a guild-install link with the `bot` scope. Grant **View Channel**, **Send Messages**, and **Send Messages in Threads**. Check channel permission overrides and invite the bot to private threads you intend to use.
3. Keep Message Content, Server Members, and Presence privileged intents disabled. Gateway uses `GUILDS`, `GUILD_MESSAGES`, and `DIRECT_MESSAGES`; DM text and explicit bot mentions are sufficient for this release. See [Discord Gateway intents](https://docs.discord.com/developers/events/gateway#message-content-intent).

The gateway does not read message history. You do not need Read Message History, Administrator, or Manage Threads.

Save this configuration; both credential fields are required:

```yaml
ims:
  discord:
    bot_token: your-bot-token
    application_id: "123456789012345678"
```

Treat the token as a secret. The application ID identifies the application; pairing records the authenticated bot user ID separately as `owner.account_id`. A different bot account requires a local reset rather than inheriting existing authorization.

## Pair and bind chats

Run `chord-gateway -f config.yaml --pair` in a foreground terminal. Allow DMs from the bot's shared server in your Discord privacy settings, open the bot DM, and send:

```text
/bind "/path/to/project" <pairing_code>
```

The directory must already exist on the gateway host. After private pairing, the same owner can bind each server text channel or accessible thread separately:

```text
@Bot /bind "/path/to/project"
@Bot /status
@Bot Explain the project structure
```

Select the actual bot mention in Discord. In DMs, send commands directly without a mention. In channels and threads, put the bot mention at the start when invoking commands such as `/allow`, `/deny`, `/answer`, or `/cancel`. Mentioning the bot later in the body submits ordinary task text; an embedded `/bind` is not promoted into a command. Other users, bots, webhooks, edits, and deletion events cannot start tasks.

Each thread has its own binding and session; it does not inherit its parent's route. Existing forum-post threads are usable when accessible, but the gateway does not create threads or forum posts. Replies are visible to channel/thread members, so bind trusted spaces. See [pairing and local reset](./pairing.md).

## Output and connection behavior

Replies use escaped text and split long output into segments of at most 1900 UTF-16 code units. Markdown, code fences, embeds, attachments, interactive components, and native slash commands are not provided in this release. Output mentions cannot notify users, roles, or everyone, and link embeds are suppressed.

Sending a result to an unlocked archived thread may automatically reactivate it. Locked, deleted, or inaccessible threads report failure; results are never redirected to the parent channel. See [Discord's thread rules](https://docs.discord.com/developers/topics/threads).

Connection loss marks the platform unavailable while reconnecting. Already accepted inputs use the shared in-memory ingress and message-ID deduplication; Gateway Resume alone is not a cross-restart delivery guarantee. API rate-limit waits can be cancelled. Network timeouts and server errors leave send outcomes unconfirmed and are not automatically resent.

During Resume, candidate message events arriving before `RESUMED` retain their original text and command target, then enter the common ingress after the same session is ready. The temporary replay buffer holds at most 128 useful events. Overflow stops the connection with `replay_capacity_exceeded` instead of silently skipping messages or continuing automatic reconnects. Inspect the original Chord session and current intake state on the host before restarting; do not blindly resend earlier messages. Process restart clears both the replay buffer and intake queue.

## Troubleshooting and deployment checks

| Symptom | Check |
| --- | --- |
| Bot DM cannot be opened or delivered | Allow DMs for the shared server and check Discord privacy settings. |
| A server message is ignored | Send from the paired owner and explicitly mention this bot; commands need the leading mention. Bind the channel/thread first. |
| `401` or authentication failure | Rotate an invalid bot token locally and ensure the application ID belongs to this bot. |
| `403` or missing access | Check channel overrides, View Channel, Send Messages, Send Messages in Threads, and private-thread access. |
| Gateway close `4014` | Confirm the deployed binary requests only the documented intents; do not enable privileged intents to work around a mismatch. |
| Archived thread becomes active | Expected when sending to an unlocked archived thread. |
| Locked thread cannot receive results | Ask a Discord administrator to unlock it; the gateway never requests Manage Threads or redirects replies. |
| `sharding_not_supported` | This release supports one shard only; choose a bot deployment that does not require multiple shards. |
| `replay_capacity_exceeded` | More than 128 useful events arrived before Resume completed. The connection stops; inspect the original Chord session, its input queue, and local ingress records before restarting. Do not blindly replay earlier commands or tasks. |

Before relying on a deployment, verify private pairing, channel binding, separate public/private thread binding, `/status`, `/allow`, `/answer`, `/cancel`, a task lasting more than 15 minutes, a background result in an unlocked archived thread, and a clear failure in a locked thread. Automated protocol tests do not replace this real-account check.

Stop the service before resetting: `chord-gateway pairing reset --platform discord -f config.yaml`. Reset removes Discord's owner, bindings, and automatic pins while retaining credentials, workspaces, and other platforms. Restart with `--pair`; see [operations](./operations.md) for delivery and restart semantics.

Each WebSocket message is limited to 16 MiB to accommodate large READY payloads while bounding resource use. Exceeding it reports an error and interrupts the connection. This is a client budget, not a platform sending limit.
