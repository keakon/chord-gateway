# Slack guide

Connect one Slack workspace through Socket Mode and send results through the Bot Web API. Supported targets are an existing App DM, public/private channels, and individual threads. Each target has its own binding and session. This guide describes setup; automated protocol tests do not replace acceptance in your own Slack workspace.

## 1. Create and install the App

Create a Slack App at [Slack API](https://api.slack.com/apps) and enable **Socket Mode**. Create an app-level token with `connections:write`. In **App Home**, enable the **Messages tab** and allow users to send messages and slash commands.

Add these **Bot Token Scopes** under **OAuth & Permissions**:

| Scope | Purpose |
| --- | --- |
| `chat:write` | Send replies to bound targets |
| `im:history` | Receive `message.im` |
| `im:read` | Verify an existing Bot DM |
| `app_mentions:read` | Receive channel/thread mentions |
| `channels:read`, `groups:read` | Verify public/private channels, archive and sharing state |
| `users:read` | Verify the Bot's App identity using `bots.info` |
| `commands` | Receive `/chord` |

Under **Event Subscriptions**, subscribe to `message.im` and `app_mention`. Do not subscribe to `message.channels` or `message.groups`: channel and thread input requires an explicit mention of this Bot.

Under **Slash Commands**, create exactly `/chord` with Usage Hint `<command> [arguments]`. Disable **Escape channels, users, and links sent to your app** so paths, quotes and arguments remain unchanged. Socket Mode delivers commands without a public Request URL. Remove conflicting `/chord` definitions from other Apps in the workspace rather than renaming this command.

Install the App into the workspace and copy its **Bot User OAuth Token**, App ID and workspace/team ID. Use a Bot installation token, not a user token. Reinstall after changing scopes. Invite the Bot to every target channel, including private channels.

The first version does not request `chat:write.public`, `im:write`, `channels:history`, `groups:history` or file scopes. It does not create DMs, join channels or unarchive them for you.

## 2. Configure and pair privately

Save `config.yaml`:

```yaml
ims:
  slack:
    app_token: your-app-token
    bot_token: your-bot-token
    app_id: your-app-id
    team_id: your-team-id
```

All four fields are required. Tokens and IDs must belong to the same installation. Keep both tokens and gateway state private. Startup verifies the Bot's team, user and App identities and the Socket Mode hello; mismatched credentials never become ready. Only one local receiver for an account/App may run under the same OS user; use a single active instance across hosts as well. Socket Mode allows an app up to ten concurrent WebSocket connections; Gateway opens one per instance, so keep a single receiver per account/App.

Start in a foreground terminal:

```sh
chord-gateway -f config.yaml --pair
```

When direct access to Slack is blocked, set `proxy_url` globally or under `ims.slack` (`http`, `socks5`/`socks5h`; no `https` for Socket Mode). See [configuration](./configuration.md#proxy).

Open the App's existing DM through the Messages tab. After the terminal shows the code, send:

```text
/chord bind "/path/to/project" <pairing_code>
/chord status
```

The directory must exist on the gateway host. The code expires after 10 minutes. Only a verified one-to-one Bot DM can establish the owner; a DM thread cannot. After pairing, send a normal text task in that DM. Restart normally without `--pair` for service operation.

Slack uses `/chord <command> [arguments]` for generated root-chat instructions. A bare `/bind` is not a registered Slack command. Existing Gateway command argument rules still apply, including `/chord answer @<question-id> <answer>`.

## 3. Bind channels and threads

The paired owner can bind an invited public/private channel with:

```text
/chord bind "/path/to/project"
```

Then send `@Bot your task`. Only owner messages explicitly mentioning this Bot are accepted in channels and threads. Group members can see replies, so bind trusted channels only. Shared channels, Slack Connect, external-team users and multi-person DMs are outside this version's scope.

A slash command always addresses the **channel root**, even when issued while viewing a thread. To bind and control an individual thread, send these messages **inside that thread**, selecting the real Bot mention:

```text
@Bot /bind "/path/to/project"
@Bot your task
@Bot /status
@Bot /allow
@Bot /answer @<question-id> <answer>
```

Every thread needs its own binding; it does not inherit the channel root or another thread's binding. Replies, confirmations, questions and background results stay in the originating thread. Follow-up thread messages also require `@Bot`. Generated instructions show the Bot name and ask you to select it from Slack's `@` menu; pasting a name or literal `<@user-id>` is not an actual mention.

The characters `&`, `<` and `>` in normal messages and thread text commands are restored to the user's input. `/chord` arguments are preserved unchanged.

## 4. Output and recovery

Replies are plain text. Mentions and links in model output are escaped, automatic unfurling is disabled, and long replies are split into ordered messages. Root and thread output in the same channel share a one-message-per-second budget. A `429` pauses the installation's affected API method for Slack's `Retry-After`, including conversation lookups, identity checks and connection opening. Other methods remain available; cooldowns survive socket reconnects and waits honor cancellation and request deadlines. Socket reconnects reverify identity. When intake is momentarily saturated an envelope is left unacknowledged and Slack redelivers it; the gateway never blocks the socket reader on a held snapshot. Accepted input survives socket reconnects within this process, but is lost on process restart. Uncertain sends are not replayed.

Socket Mode `interactive` payloads (block actions, dialog submissions, message menus) are acknowledged and then discarded, because no interaction surface is registered yet; Slack therefore never redelivers them. The first version supports text commands and text output. Files, generated-image forwarding, buttons, modals, editing previews, Agent View, Enterprise multi-team installs and Marketplace distribution are deferred. Long-running results use the Bot API rather than an expiring slash `response_url`.

For local reset, stop the gateway first:

```sh
chord-gateway pairing reset --platform slack -f config.yaml
```

Confirm `slack` in the terminal, then restart with `--pair`. See [pairing](./pairing.md) and [delivery semantics](./operations.md#message-delivery-and-restarts) before resolving unknown operations or changing account identity. An ACK or “accepted” response confirms receipt, not successful Chord execution. Redelivery of the same slash command reports expired, quarantined or unknown outcomes as appropriate. For unknown outcomes, inspect the session and `/chord status` before repeating an operation.

Socket intake waits up to two seconds for acceptance, reserving the remainder of Slack’s three-second ACK deadline for writing. Conversation lookup and queue wait have a separate 30-second limit, cancelled on disconnect. Missing an ACK does not cancel admission or prove the command failed: it may still execute, and redelivery is deduplicated within this process. Check `/chord status` and the original session before manually retrying. ACK hints use English, consistent with other command replies.

## Official references

- [Socket Mode](https://api.slack.com/apis/socket-mode) — no public Request URL, the app-level token (`xapp-`), up to ten concurrent WebSocket connections per app, per-event acknowledgement through `envelope_id`, and why Socket Mode apps are not eligible for Marketplace listing.
- [Rate limits](https://api.slack.com/apis/rate-limits) — Web API tiers (Tier 1 `1+`/min, Tier 2 `20+`/min, Tier 3 `50+`/min, Tier 4 `100+`/min), the special tier where `chat.postMessage` allows roughly one message per second per channel, and the `429` + `Retry-After` contract. Other methods for the same workspace remain available during a cooldown.
- [Events API](https://api.slack.com/apis/events-api) — the three-second acknowledgement window, the three retries (immediately, after 1 minute, after 5 minutes), and the rule that more than 95% failed deliveries within 60 minutes temporarily disables event subscriptions. Gateway's two-second intake budget exists to answer inside that window.
- [Bot token scopes](https://api.slack.com/scopes) — official reference for the scopes listed in step 1.

## Installation checks and troubleshooting

Verify DM pairing, `/chord status`, a text task, confirmation and Question handling. Invite the Bot to one public and one private channel; bind each. Bind two threads in the same channel and check that commands, results and long-message segments remain in their respective threads. Finally restart the gateway and verify restored sessions and long-running background results. These checks require a real Slack workspace and credentials; they are not implied by a passing local test suite.

- **Unrecognized `/chord`**: check command registration, workspace installation and conflicting definitions. Do not replace it with bare `/bind`.
- **Not ready / identity mismatch**: check all four config fields and that Socket Mode is enabled. `link_disabled` requires re-enabling Socket Mode; revoked or invalid tokens require correcting credentials.
- **`missing_scope`**: add the listed scope and reinstall the App. A console change alone does not update an installed token. Missing required identity scopes such as `users:read` stop startup retries with an explicit error; restart the gateway after correcting the installation permissions.
- **`not_in_channel`, `channel_not_found`, `is_archived`**: invite the Bot, verify channel access or unarchive locally. Gateway does not change channel membership or fall back to a DM.
- **Ignored input**: confirm the sender is the paired owner, the target has its own binding, and channel/thread text contains an actual Bot mention. Shared channels and unverified DMs are rejected.
- **Unknown send or execution outcome**: inspect the original chat/session and local ingress metadata before retrying. Uncertain output is not automatically resent.
