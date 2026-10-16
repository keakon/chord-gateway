# Troubleshooting

All IM platforms use one paired owner and explicit chat bindings, including single-workspace deployments. Run `chord-gateway --pair`, then send `/bind "~/workspace" <pairing_code>` in a WeChat/Feishu/WeCom/DingTalk private bot chat. See [pairing and local reset](./pairing.md) and the [upgrade notes](../CHANGELOG.md). Slack uses `/chord bind "~/workspace" <pairing_code>` in the App DM; see the [Slack guide](./slack.md).

This page covers common `chord-gateway` startup, IM integration, routing, and runtime issues.

## Startup failures

### `load config` errors

Check that the intended config file is being used. Resolution order:

1. `--config` / `-f`
2. `$CHORD_GATEWAY_CONFIG`
3. `$XDG_CONFIG_HOME/chord-gateway/config.yaml`
4. `~/.config/chord-gateway/config.yaml`

Run with an explicit config while debugging:

```bash
chord-gateway -f ./config.yaml
```

### `chord` is not found

The gateway executes `chord_path` directly. Shell aliases are not expanded.

Use one of these approaches:

- Put the Chord binary directory in `PATH`.
- Set an absolute path in `chord_path`.
- Use a `~` path supported by the gateway.

Example:

```yaml
chord_path: ~/go/bin/chord
```

### Workspace validation fails

Common causes:

- `workspaces.<id>.path` does not exist.
- All platforms select workspaces through explicit `chat_bindings`; use the platform command entry point to bind each target.
- The Feishu chat is not listed in `ims.feishu.chat_bindings`; this also applies with one workspace.
- A Feishu `chat_bindings` entry points to the wrong workspace ID.

## WeChat issues

If you have not completed the WeChat setup workflow yet, start with: [WeChat iLink guide](./wechat.md)

### No QR login appears

Check:

- `ims` includes a WeChat entry.
- `base_url` is correct.
- Existing token files are not stale or corrupt. The default path is `<state_dir>/wechat/token.json`; if `ims.wechat.token_path` is configured, check that custom path instead.
- Gateway logs under the state directory.

To force a new login, stop the gateway and remove the saved WeChat token file (`<state_dir>/wechat/token.json` by default, or `ims.wechat.token_path` if configured).

### Token expired

When a WeChat token expires, in multi-IM mode send this from another active channel:

```text
/login wechat
```

Then open the returned login link and scan the QR code. When the gateway itself detects an expired WeChat token, it clears the saved token and starts QR login automatically.

Feishu does not need in-chat login or manual renewal; the gateway automatically obtains and refreshes Feishu access tokens from the configured app credentials. If the Feishu connection becomes invalid, check the deployment configuration and Feishu developer console for app credentials, permissions, event subscriptions, and long-connection settings. Do not send or change `app_id` / `app_secret` in an IM chat.

### WeChat tasks waiting or polling paused

`reply_context_missing` means input in this process has not been delivered; a fresh owner `/status` can establish context. Process restart clears the queue without restoring old tasks. Temporary capacity refusal retains the current polling batch until intake can advance its in-memory cursor.

`batch_too_large` means one response exceeds the client budget of 4 MiB or 1,024 items. The WeChat receive loop stops without advancing its cursor; other platforms may keep running. Logs report `IM adapter exited with error type=wechat`. Restart may encounter the same oversized server batch; `/login wechat` does not restart an exited loop. Inspect the platform backlog and original Chord session before acting; do not skip an opaque cursor or blindly resend old tasks.

Unknown input may already have executed. Check `/status` and the original Chord session queue, execution history and results before deciding to resend or restart. Restart cannot cancel work already executed.

## Discord issues

Check DM privacy, explicit @Bot from the paired owner, individual thread binding, and channel/thread permissions. `403` usually requires inspecting permission overrides or private-thread access; `4014` requires checking requested intents. Unlocked archived threads may reactivate on send; locked or deleted threads fail without redirecting output. See the [Discord troubleshooting guide](./discord.md#troubleshooting-and-deployment-checks).

## Feishu issues

If you have not completed the Feishu console setup yet, start with: [Feishu guide](./feishu.md)

### Feishu long connection is not established or no events arrive

Check:

- the gateway process is running and stays online
- the Feishu app uses “Receive events via long connection” in the developer console
- the subscribed event types include `im.message.receive_v1`
- callback configuration includes `card.action.trigger` for interactive cards
- the Feishu app version has been published after changing capabilities, permissions, or event subscriptions
- the machine can make outbound connections to Feishu services

### Long connection configuration fails

Check that the Feishu app is configured to use “Receive events via long connection”, the gateway is already running, and `app_id` / `app_secret` are correct.

### Messages are ignored or the chat is unbound

Check that the sender is the paired owner for the current bot account, the chat is explicitly bound, and the message is text (`text` or `post`). First pairing requires foreground `--pair` and the terminal code in a private bot chat. Ordinary startup never opens pairing.

To bind a group, complete private pairing, add the bot to a trusted group, and have the owner send `/bind "~/work/project-a"` there. The directory must exist. After success, inspect YAML `chat_bindings` for the chat ID and workspace mapping. Earlier unbound messages are not replayed. If switching is blocked, inspect `/status` for pending inputs, background work, interactions, and transfers. A failed status refresh preserves the old binding; retry after restoring connectivity.

See [pairing and local reset](./pairing.md) for account changes or authorization recovery. Do not copy identity from untrusted message logs to bypass pairing.

### Duplicate messages

Feishu may redeliver events. Gateway deduplicates the App ID, chat and business message/card identity within the current process, with no deduplication history after restart.

## DingTalk issues

The first connection waits for same-enterprise private `hello`; a group message cannot unlock pairing. If `Gateway binding check.` fails, do not manually save the target: fix proactive API permissions/application publication (`proactive_send_denied`) or employee scope/group membership (`target_unreachable`), then issue a new `/bind`. Stream registration authentication or tenant mismatch stops reconnects; a proactive API authentication failure only fails the current send and clears its token. Unknown sends and callback 500 responses do not justify automatic replay. See [DingTalk error categories](./dingtalk.md#limits-and-troubleshooting).

## Routing issues

### Wrong workspace receives messages

For Feishu multi-workspace mode, confirm `ims.feishu.chat_bindings` maps each Feishu chat ID to the intended workspace ID.

Feishu never chooses a default workspace for an unbound chat. Ask the owner to bind the chat, even with one workspace.

### Session context is not what you expected

Use:

```text
/status
/sessions
```

Then use `/new` to clear the current binding or `/resume <id>` to pin a known session.

On WeChat and Feishu, if a pinned session is missing, busy or cannot be restored, accepted input is isolated rather than resent in a fresh session. Check `/status` and the original Chord session, then inspect local ingress records if needed. Explicit `/new` or `/resume` selects a target for future work; it does not replay the isolated request. Send a new task only after checking whether the original request ran.

### Session change is refused

`/new` and `/resume` refuse a busy Chord process before replacing the route: send `/cancel` first, then retry. Distinct reasons name the cause:

- `session_change_busy` — a turn is still running.
- `session_transferred` — a transfer guard is active.
- `undelivered_inputs` — restored or queued inputs are unresolved.

A `control_target_expired` refusal right after startup or upgrade is a stale control snapshot: the gateway narrowed this guard so a process that is still starting (no runtime identity yet) no longer trips it. If it persists, check `/status`, restart, and inspect the gateway log for dropped readiness events.

## Runtime issues

### Chord does not respond

Check:

- Gateway logs for process start or stdout parsing errors.
- Chord logs and session state.
- Whether a confirmation or question is pending (`/status`).
- Whether the process was cancelled or timed out.

Headless JSONL frame bodies may contain up to 16 MiB of JSON, excluding the LF or CRLF line ending. This includes shell results that grow during JSON escaping. A malformed JSON line sends a chat warning and reading continues. An oversized frame or a stream read failure stops the Chord process after a two-second graceful-exit window, then sends a chat notice with the error and any unconfirmed-input count. This stop does not schedule a crash restart. Check the session before retrying; inputs are not replayed automatically.

### Late answers and confirmations

A question that Chord has closed stays addressable by ID in its session, whatever its terminal outcome (including `no_response`): `/answer @<id> <answer>` appends a historical revision, and the original outcome and task stay unchanged. Plain text is always a new instruction, never an answer to an older question. Idle and idle-timeout cleanup do not close questions, and a timeout is announced once, not on every later idle event.

If the process disconnects before an outcome arrives, the gateway keeps the known question without inventing an outcome; check `/status` and the session before explicitly retrying the same answer. The gateway reuses the uncertain operation while its retry cache remains available; see [Question interaction](./usage.md#question-interaction) for cache limits and restart behavior. If the answer entry could not open, `/status` retries presentation before displaying it.

On Feishu, expired `/allow` and `/deny` controls are rejected and never become model follow-up context. Check `/status`; ask Chord to issue a new confirmation if the operation still needs approval, or send an explicit new task.

### A task appears stuck

Use `/status` first. If the task should stop, send:

```text
/cancel
```

If the process remains stuck, restart the gateway and check for orphan Chord processes.

### Generated images do not arrive

Images are forwarded only when Chord produces them: the model must actually generate an image, and the Chord build must support `image_result` summaries and `image_artifact` chunks (see [Compatibility](./compatibility.md)). The gateway subscribes to `image_result` itself, so no gateway setting enables or disables forwarding.

Check:

- Chord configuration enables image generation, or the model has an authorized server-side image tool.
- A Feishu app needs the `im:resource` scope to upload images; after adding it, publish a new app version.
- A Feishu image at or above the platform limit is sent as a reduced PNG preview with a short note; the original stays in the Chord session.
- The chat reports `image delivery failed` or an unconfirmed notice, and the gateway log records the failing artifact reference.
- A reported failure never regenerates the image and never resends automatically. Open the saved original in the Chord session instead.

WeChat image forwarding has protocol tests, but real-device decryption and display have not yet been verified. When validating a deployment, generate a small image and confirm that it opens in the WeChat client; a successful API receipt alone does not prove that the image renders.

### Intake is empty after restart

This is expected: Gateway keeps intake, deduplication and polling cursors in memory. Old inbox files are not read or upgraded. Chord sessions, configuration, credentials and transfer guards retain their own persistence; see [delivery and restarts](./operations.md#message-delivery-and-restarts).

### A common-ingress input remains received

Check `/status` for a waiting reason and the original input age. An adapter may need a local delivery prerequisite such as a valid reply context or an available account. The common queue preserves order while waiting and isolates inputs after the original 24-hour window; sending another message does not reset that age. WeChat uses this path: the authorized owner can send a fresh `/status` in the original chat to establish context and wake the original queue. New ordinary tasks join behind it. Feishu intake starts empty on process restart.

### Where are logs and state files?

State directory priority:

1. `$CHORD_GATEWAY_STATE_DIR`
2. `$XDG_STATE_HOME/chord-gateway`
3. `~/.local/state/chord-gateway`

Logs, tokens, dedupe data, and session pins are stored there. WeChat token state defaults to `<state_dir>/wechat/token.json`.

## Still stuck

When reporting an issue, include:

- gateway version or commit
- OS and architecture
- Go version
- Chord version or commit
- sanitized config
- relevant gateway logs
- the IM platform and mode you are using

All platforms share bounded in-memory intake, authorization and delivery receipts. Gateway does not restore its inbox, deduplication or polling cursors after restart. Check the original Chord session before resending an uncertain task. See [delivery semantics](./operations.md#message-delivery-and-restarts).

## WeCom issues

Rule out four setup blockers first. The admin console entry point is **Intelligent Zone → Smart Robot**; "Security and Management → Management Tools" is not the current entry point. A personal team has no Workbench and cannot sign in to the web admin console, so neither entry point is reachable — use an enterprise team or convert it with "Change to Enterprise Account". If the creation page shows no Bot ID or Secret, the API mode management scope does not include your account; enterprises above 1,000 contacts default to empty and need an administrator to enable it. If the bot is missing in the client or `Gateway binding check.` never arrives, verify that the available scope contains you. WeCom verification is not a prerequisite: long connections need no domain, ICP filing, or public IP, so unverified enterprises can connect. The "Available Permissions" panel on the creation page is the MCP/CLI capability channel, which Gateway does not use.

For authentication failures, verify the intelligent bot API long-connection mode and bot ID/secret. For binding probe failures, check scope, target, send permission, and quota, then send `/bind` again. `duplicate_connection` means another deployment replaced the connection; stop it before restarting. `unknown_send` does not prove output was undelivered; inspect the chat and original session. After changing bot ID, stop the service and run `chord-gateway -f config.yaml pairing reset --platform wecom` in a local controlling terminal, type `wecom` to confirm, then pair again. An `endpoint` field is ignored with a warning and cannot select another connection URL. See the [WeCom guide](./wecom.md) for diagnostics and pending real-account acceptance.

## Slack issues

See [Slack installation checks and troubleshooting](./slack.md#installation-checks-and-troubleshooting) for `/chord` conflicts, scope reinstall, identity errors, channel access and unknown sends. Slash commands always target the root; control a thread using a Bot mention inside that thread, with its own binding.

## Telegram issues

See [Telegram troubleshooting](./telegram.md#troubleshooting) for credentials, webhook/poller conflicts, privacy, group migration, and uncertain sends. Group binding uses `/bind@BotUsername`; topics and media are unsupported.
