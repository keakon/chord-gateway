# Operations

All IM platforms use one paired owner and explicit chat bindings, including single-workspace deployments. Run `chord-gateway --pair`, then send `/bind "~/workspace" <pairing_code>` in a private chat with the bot. See [pairing and local reset](./pairing.md) and the [upgrade notes](../CHANGELOG.md).

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
- WeChat sync cursor (`<state_dir>/wechat/sync-buf.json`)
- Feishu dedupe store
- session pin store
- session transfer guards (`session-transfers.json`)

The gateway writes WeChat token/sync state, Feishu dedupe state, session-pin state, and transfer guards with atomic file replacement so crashes or abrupt restarts do not leave truncated state files behind.

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
- `performance queue=...` – queue metrics logged every 5 minutes for outbound notification shards and Feishu inbound dispatch, including queue `depth`, `max_depth`, `full`, `closed`, `blocked`, and how long blocked enqueues waited (`blocked_wait_total`, `blocked_wait_total_delta`, `blocked_wait_max`, `blocked_slow`, `blocked_slow_delta`). The `*_total` and `blocked_slow` fields are process-lifetime counters; use their `_delta` fields to identify recent provider stalls. `max_depth` is the deepest shard at the time of the sample, not a historical maximum.

Wait durations are recorded only when a blocked queue-capacity wait ends, either by enqueueing or by dispatcher shutdown. They exclude time waiting for the shard producer lock and do not measure end-to-end delivery latency. An ongoing stall may therefore leave wait-duration deltas at zero; also inspect `depth`, `max_depth`, `blocked`, and `processed_delta`. `blocked_wait_max` is the lifetime maximum of completed waits, and `blocked_slow` counts completed waits of at least one second. Feishu inbound dispatch does not wait on a full queue, so its wait metrics remain zero. Backpressure can also reflect traffic bursts, not only a stalled provider.

Useful fields include:

- event type
- workspace ID
- IM type
- chat ID
- session ID
- busy/idle state
- phase
- last outcome

## Feishu async dispatch and dedupe

Feishu event handling uses long connection mode plus async dispatch:

- The gateway maintains a long-lived Feishu websocket connection.
- Incoming events are routed to `chord headless` through an in-process bounded queue.
- Duplicate deliveries are filtered by a lightweight dedupe store.

For chat messages, the dedupe key is based on:

```text
app_id + chat_id + message_id
```

Deduplication data is persisted to:

```text
<state_dir>/dedupe.json
```

Card callbacks use `app_id + card + event_id` instead. Redelivery of one callback is ignored, while a new click with the same answer can retry an uncertain submission. See [Question interaction](./usage.md#question-interaction) for operation retry behavior.

The current dedupe TTL is 24 hours.

## Workspace routing

Both platforms select workspaces through explicit `chat_bindings`; the paired owner uses `/bind "~/workspace"` in each chat.

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
- `session_transfer_preparing`
- `session_transfer_status`
- `session_transfer_saved`
- `session_transfer_released`
- `background_result`
- `image_result`

Optional events are controlled by `event_visibility`. `background_result` is subscribed by default and can be turned off with `event_visibility.background_result: false`. See [event-visibility.md](./event-visibility.md) for what each event does and which ones are state-only.

If a Feishu card send is unconfirmed, the gateway sends a diagnostic instead of repeating its prompt as text. Check the chat first; use `/status` to reopen a missing question entry, or `/role` to request a role menu. Message requests do not follow redirects; unexpected HTTP statuses are unconfirmed. Explicitly rejected cards still use the text fallback. `/status` shows Chord state and can reopen question entries; it does not query platform delivery receipts.

Generated image delivery requires a Chord configuration with image generation enabled or an authorized native image tool. No separate gateway image-generation setting is required. Feishu bot permissions must include image upload (`im:resource`) and bot message sending (`im:message:send_as_bot`). A failed platform upload or unconfirmed send is reported without automatic resending or image regeneration. If a send is unconfirmed, check the chat before taking further action; the original remains available in Chord.
