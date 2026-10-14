# Operations

All IM platforms use one paired owner and explicit chat bindings, including single-workspace deployments. Run `chord-gateway --pair`, then send `/bind "~/workspace" <pairing_code>` in a WeChat/Feishu/WeCom/DingTalk private bot chat. See [pairing and local reset](./pairing.md) and the [upgrade notes](../CHANGELOG.md). Slack uses `/chord bind "~/workspace" <pairing_code>` in the App DM; see the [Slack guide](./slack.md).

This page describes runtime behavior that matters when operating `chord-gateway`.

## Process lifecycle

The gateway starts `chord headless` as a child process for each active binding as needed.

Each spawned Chord process is placed into its own process group. On normal gateway shutdown, the gateway attempts to terminate the whole process group:

1. Close stdin to request graceful exit.
2. Send `SIGTERM` to the process group.
3. After a short grace period, send `SIGKILL` to the process group.

This prevents orphaned Chord processes and child processes from keeping session locks or workspace resources alive.

If the gateway itself is killed with `SIGKILL` (`kill -9`), it cannot run cleanup handlers. Chord's parent-death watcher is expected to detect the parent process change and exit promptly.

A Chord process that crashes (exits while the gateway did not stop it) is respawned automatically after a short delay. If the gateway is shutting down, the pending restart is cancelled instead. A process in a session transfer is also excluded from automatic restart; a successful transfer exits with code `20`. A transfer guard remains effective even if its final events are lost or the process exits unexpectedly.

## Session transfer recovery

Use `/takeover status` in the affected IM chat, or `/status` while its binding is blocked. Querying does not create a new transfer or fall back to a fresh session. For an existing transfer into this chat, a query can finish resuming its paused target after the source has released the session. From the gateway host, use the same workspace and Chord state directory as the source:

```sh
chord takeover status -d /path/to/workspace --session <session-id> --transfer-id <transfer-id> --json
```

The IM notification shows only a short blocker preview. The host command returns a bounded page with `blocker_count`; when `next_offset` is present, query again with `--blocker-offset <next_offset>` to inspect the next page.

If Chord uses a custom state directory, append `--state-dir /path/to/chord-state`; configure the same path as `chord_state_dir` in the gateway. This is separate from the gateway's own state directory.

- A waiting timeout ends observation; the source still holds the session lock and keeps draining. Inspect blockers and let existing work finish. A long-lived job can block indefinitely; stopping it is a separate user action.
- A save failure leaves the source frozen with its lock. Correct the reported storage problem, then retry saving with the same transfer ID:

  ```sh
  chord takeover retry-save -d /path/to/workspace --session <session-id> --transfer-id <transfer-id> --json
  ```

- If the source saved the session but the target could not resume, check the workspace, role/configuration, and lock state before explicitly sending `/resume <session-id>`. A paused checkpoint resumes without a model request until the user explicitly continues or sends new input. Check `/status` for the restored queue's count, owner tasks, previews, and attachment counts before continuing.
- If observation was interrupted, query the existing transfer. Do not assume that a missing reply means the source resumed execution or that the target started. An unavailable coordinator does not guarantee that a target process will start unattended.
- Restarting or stopping the gateway during draining is an external interruption, not a completed natural transfer. The guard remains in place for recovery.

`<state_dir>/session-transfers.json` records blocked bindings before the source acknowledges preparation. It takes precedence over session pins after a restart. Ordinary messages, reminders, old cards, and crash recovery cannot silently reclaim the transferred session. When takeover into this chat reaches `ready_paused` and its session pin is saved, the target binding opens for explicit continuation. A binding that transferred its session out remains blocked until an explicit `/new` or `/resume` succeeds; failed attempts retain the guard. Guard deletion uses a private `session-transfers.json.pending` journal so a failed disk sync cannot reopen the binding after restart. If a storage failure leaves this journal, correct the storage problem and retry explicitly; do not delete either guard file to force an automatic restart while source ownership is uncertain.

Session transfer notifications use the ordered outbound queue. Slow IM delivery does not hold process lifecycle locks; bounded queue backpressure may still delay protocol reading when the queue is full.

## Idle timeout

`idle_timeout` controls how long an idle Chord process may remain alive. The default is `30m`. The same timeout applies even if the process is waiting on a pending question, confirmation, or handoff request. Processes reporting active background jobs are retained until those jobs no longer count as running; `/status` shows the count. Processes with pending local shell commands are also retained until their results arrive. Transfer preparation and draining also prevent idle cleanup.

If a restored input summary may be stale, idle cleanup refreshes it before deciding. A successful status refresh restarts the existing idle timer; once the summary confirms no pending input, cleanup no longer needs a manual `/status`. Failed refreshes preserve the process.

Idle cleanup stops Core and suspends question timing without inventing outcomes. Reconnect and restore the session to answer by its original ID; check `/status` and rely on Chord receipts. Confirmation and handoff requests follow their own cancellation rules.

Use Go duration syntax, for example:

```yaml
idle_timeout: 30m
```

## State directory

Runtime state is stored in this priority order:

1. `$CHORD_GATEWAY_STATE_DIR`
2. `$XDG_STATE_HOME/chord-gateway`
3. `~/.local/state/chord-gateway`

For beginners, this means:

- if you do not set anything, state usually lands under `~/.local/state/chord-gateway`
- if you cannot find logs or WeChat token files, check this directory first
- if you run multiple gateway instances, give them separate state directories unless you intentionally want shared state

State data includes:

- logs
- WeChat token files (`<state_dir>/wechat/token.json` by default, or `ims.wechat.token_path`)
- session pin store
- session transfer guards (`session-transfers.json`)

Gateway atomically saves login tokens, session pins and transfer guards. Input queues, deduplication and polling progress stay in memory.

## Upgrades and stopping the service

Check `/status` for pending input and inspect uncertain results in the original Chord session before stopping. Restart clears Gateway's in-memory queue and is not a substitute for reconciliation. Back up configuration, credentials, session pins and transfer guards before upgrading; Chord owns its own saved sessions.

Old `dedupe.json`, WeChat `sync-buf.json` and development `im-ingress.db` files are no longer read, imported or updated, and are not automatically deleted. Intake starts empty; old messages redelivered by a platform may execute again. Before rolling back, reconcile session outcomes instead of relying on stale cursors to prevent duplicates.

One account permits one instance per host and OS user; enforce exclusivity across users or hosts yourself. Same-account WeChat renewal preserves the cursor within this process; changing accounts requires local reset and pairing again.

## Config file resolution

The gateway loads config in this priority order:

1. `--config` / `-f`
2. `$CHORD_GATEWAY_CONFIG`
3. `$XDG_CONFIG_HOME/chord-gateway/config.yaml`
4. `~/.config/chord-gateway/config.yaml`

## Logs

The gateway writes golog-formatted key-value logs to stderr and to a rotating log file under the state directory.
By default, the log file is `<state_dir>/gateway.log`; set `CHORD_GATEWAY_LOG_FILE` to override it.
Log files rotate at 10 MiB and keep 3 backups. Rotated logs are not gzip-compressed.

The gateway logs important routing stages:

- `chord-gateway starting` – startup metadata including `gateway_version`, `gateway_commit`, `gateway_build_time`, `gateway_vcs_time`, `gateway_dirty`, and `go_version`
- `chord process spawned` – child Chord process metadata including `chord_binary` and `chord_binary_mtime`
- `gateway event` – raw event parsed from `chord headless` stdout
- `gateway routing event` – router-side handling decision for a binding
- `gateway sending notification` – outbound IM send attempt
- `performance queue=...` – queue metrics logged every 5 minutes for outbound notification shards, including queue `depth`, `max_depth`, `full`, `closed`, `blocked`, and how long blocked enqueues waited (`blocked_wait_total`, `blocked_wait_total_delta`, `blocked_wait_max`, `blocked_slow`, `blocked_slow_delta`). The `*_total` and `blocked_slow` fields are process-lifetime counters; use their `_delta` fields to identify recent provider stalls. `max_depth` is the deepest shard at the time of the sample, not a historical maximum.

Wait durations are recorded only when a blocked queue-capacity wait ends, either by enqueueing or by dispatcher shutdown. They exclude time waiting for the shard producer lock and do not measure end-to-end delivery latency. An ongoing stall may therefore leave wait-duration deltas at zero; also inspect `depth`, `max_depth`, `blocked`, and `processed_delta`. `blocked_wait_max` is the lifetime maximum of completed waits, and `blocked_slow` counts completed waits of at least one second. Backpressure can also reflect traffic bursts, not only a stalled provider.

Useful fields include:

- event type
- workspace ID
- IM type
- chat ID
- session ID
- busy/idle state
- phase
- last outcome

## Discord connection and delivery

The application and bot identities are verified before connecting; a local account lock excludes competing receivers. Temporary disconnections may Resume; process restarts do not restore Gateway's queue. Each WebSocket message has a 16 MiB client resource limit; exceeding it interrupts the connection with a diagnostic. Resume buffers at most 128 useful events; inspect the original session after overflow.

Only bot DMs and explicit @Bot channel/thread text are accepted, without the privileged Message Content intent. Each thread binds independently and keeps its replies; sending may reactivate an unlocked archived thread. See [Discord](./discord.md).

## Feishu intake and deduplication

Text/post messages and authenticated cards use in-memory intake. `message:<message_id>` and `card:<event_id>` are scoped by App ID and deduplicated within this process. A successful platform response indicates acceptance or definite refusal, not completed Chord execution; a failed card update does not undo a submitted operation.

Text shares eight concurrent sends, a five-minute deadline and a 64 KiB / 32-segment limit per output; control replies have 15 seconds. Explicit HTTP 429 retries only the rejected segment; unknown outcomes stop sending, with partial failures visible in `/status`. When delivery remains unconfirmed, cancellation does not replace the `unknown_send` diagnostic; confirmed segment counts cover only platform-accepted segments. See [Feishu](./feishu.md).

## Workspace routing

All platforms select workspaces through explicit `chat_bindings`; use the platform command entry point to bind each target.

Feishu supports multiple workspaces through `ims.feishu.chat_bindings`, which maps Feishu `chat_id` values to workspace IDs.

Every Feishu chat requires a `chat_bindings` entry, even with one workspace. An unbound group receives binding guidance; use `/bind` before sending tasks.

## Event contract

The gateway runs `chord headless` and reads JSONL events from stdout. It subscribes to the default control-plane event set:

- `activity`
- `assistant_message`
- `confirm_request`
- `question_request`
- `question_updated`
- `question_resolved`
- `handoff_request`
- `handoff_cancelled`
- `idle`
- `error`
- `notification`
- `done_completion`
- `local_shell_result`
- `role_change`
- `agent_done`
- `compaction_status`
- `session_switched`
- `mcp_status_changed`
- `input_queue_changed`
- `session_transfer_preparing`
- `session_transfer_status`
- `session_transfer_saved`
- `session_transfer_released`
- `background_result`
- `image_result`

Optional events are controlled by `event_visibility`. `background_result` is subscribed by default and can be turned off with `event_visibility.background_result: false`. See [event-visibility.md](./event-visibility.md) for what each event does and which ones are state-only.

If a Feishu card send is unconfirmed, the gateway sends a diagnostic instead of repeating its prompt as text. Check the chat first; use `/status` to reopen a missing question entry, or `/role` to request a role menu. Message requests do not follow redirects; unexpected HTTP statuses are unconfirmed. Explicitly rejected cards still use the text fallback. `/status` shows Chord state and can reopen question entries; it does not query platform delivery receipts.

Generated image delivery to WeChat or Feishu requires a Chord configuration with image generation enabled or an authorized native image tool. No separate gateway image-generation setting is required. Feishu bot permissions must include image upload (`im:resource`) and bot message sending (`im:message:send_as_bot`). A failed platform upload or unconfirmed send is reported without automatic resending or image regeneration. If a send is unconfirmed, check the chat before taking further action; the original remains available in Chord.

## Slack operations

Reconnect rechecks Bot/App identity. Invalid credentials, identity mismatch or disabled Socket Mode stop the connection until corrected locally. Intake waits up to two seconds, leaving time to send an ACK. Confirmed acceptance, duplicates and definite refusals receive one ACK; unconfirmed intake records a diagnostic. Temporary disconnection preserves the current process queue; restart clears it. See [Slack](./slack.md).

## Message delivery and restarts

Gateway integrates IM transport with the Chord control plane. Its inbox, deduplication, polling cursors, temporary reply contexts and delivery receipts live only in memory; Chord owns session and task state. Gateway does not restore intake after restart and does not guarantee loss-free or duplicate-free delivery across restarts.

A platform ACK confirms in-memory acceptance or definite refusal; `consumed` means Chord accepted input, not that the task succeeded. Exiting after ACK but before Chord acceptance can lose input; losing confirmation after Chord acceptance can cause redelivery. Platforms differ in redelivery behavior: withholding ACK is not a universal retry guarantee, and a Chord request ID is not a cross-restart idempotency key.

Within one process, intake preserves per-chat order, freezes control targets and rechecks authorization. `unknown` means execution may already have occurred and is never automatically resent; inspect `/status` and the original Chord session. Quarantine on the current route pauses delivery; explicit switching never authorizes old input for a new session. A missing IM reply does not prove that an operation failed to execute.

Defaults allow 64 pending records per chat, 1,024 / 16 MiB per account and 4,096 / 64 MiB globally, including unknown and quarantined records. An authorized `/status` has one extra count slot but still obeys byte limits. Polling cannot skip earlier unaccepted input; wait for capacity. All records also share a 100,000-record and approximately 64 MiB serialized-size budget; actual heap usage includes indexes and other overhead. Terminal records clear their bodies; deduplication lasts 24 hours and is reclaimed on subsequent intake. Unresolved records are never silently evicted. Inputs beyond their original 24-hour delivery window are quarantined.

`/status` shows waiting, isolation and send failures; runtime metrics count acceptance and receipt outcomes. Control replies share 15 seconds across queueing and sending; congestion may drop a reply without undoing the operation. There is no offline `ingress` database maintenance CLI. Reconcile pending input before stopping; after checking outcomes, restarting clears in-memory guards but cannot cancel or undo work already handed to Chord.

Configuration, login credentials, session pins and transfer guards retain their own persistence. If a binding configuration write is uncertain, stop and compare YAML and Chord sessions before acting; do not delete transfer guards to force progress.

## WeCom operation and reconnects

The intelligent bot API long connection deduplicates business msgids within the process and opens intake after authentication. Network failures reconnect with backoff; authentication rejection or connection replacement stops reconnecting. Background results do not depend on the original callback lifetime. See [WeCom](./wecom.md).
