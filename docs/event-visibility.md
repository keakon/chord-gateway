# Gateway control-plane event visibility

`chord-gateway` reads JSONL events from `chord headless`. Required control-plane events are always subscribed and routed. `background_result` is subscribed by default and can be disabled; the remaining optional events are opt-in through `event_visibility`.

- Chinese version: [event-visibility_CN.md](./event-visibility_CN.md)

## Default events

The gateway subscribes to these events by default:

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

These events cover the minimum behavior for IM control, except that `background_result` can be turned off:

- final assistant responses
- permission confirmations
- user questions
- question close events, labeled by question ID and showing accepted selections
- handoff requests and responses
- handoff cancellation notices
- busy/idle state aggregation
- error reporting
- canonical user-facing notifications
- non-loop Done completion reports
- local shell command results
- active-role tracking for `/status`
- SubAgent completion summaries
- last context-checkpoint outcome for `/status`
- in-band session switches, so a pinned binding follows the session the runtime actually runs
- session-transfer preparation, save, and release, so a transferred binding stays blocked across restarts
- finished background job results, which have no other delivery channel
- generated-image forwarding through bounded artifact chunks

`session_switched` is state-only: the command that caused the switch already answers the user, so no extra chat message is sent. It re-points the binding's session pin at the newly active session, so later spawns resume the session the runtime actually runs.

The `session_transfer_*` events are required and cannot be disabled by `event_visibility`. Preparation closes new input admission and records a persistent guard before acknowledging the source. Status events report draining, blocker changes, saving, and save failures without clearing the source pin. Save/release update transfer status and conditionally clear the source pin. Existing accepted input remains part of the source session; new messages and reminders stay blocked. Old card callbacks cannot target a replacement binding instance. Use `/takeover status`, or `/status` while blocked, to inspect progress and blockers. Missing final events do not permit automatic restart; see [operations.md](./operations.md#session-transfer-recovery).

Paused readiness and status snapshots also carry the restored queue summary. `/status` shows the pause state and total pending count, then up to eight entries with owner task, a short single-line preview, and attachment count; remaining entries are counted separately. These summaries do not start work. The full content and attachments remain in Chord. Missing queue data is not reported as zero. The takeover target's binding stays blocked until Chord confirms paused readiness with a valid summary. See [compatibility.md](./compatibility.md#session-takeover-capability) for the headless contract.

`background_result` is pushed to IM as a chat message, because Chord has no other channel for it: a background job usually finishes after the turn went idle, so no later `assistant_message` summarizes it. Set `event_visibility.background_result: false` to stop these pushes in a noisy chat; the durable result stays in the Chord session.

## Optional visible events

Optional events are disabled by default and enabled through `event_visibility`. `background_result` is the exception: it is enabled by default, and only an explicit `false` turns it off.

```yaml
event_visibility:
  background_result: true
  agent_started: false
  agent_notify: false
  context_notice: false
  info: false
  toast: false
  todos: false
```

| Field | Event type | Typical use |
|---|---|---|
| `agent_started` | `agent_started` | SubAgent delegation start notifications |
| `agent_notify` | `agent_notify` | Non-blocking owner or targeted delegated-workstream updates |
| `background_result` | `background_result` | Finished background job results. Enabled by default; set `false` to disable the chat pushes. |
| `context_notice` | `context_notice` | Durable context-pressure warnings, pushed as chat messages. `level` is one of `pressure`, `imminent` or `warning`. |
| `info` | `info` | Informational messages |
| `toast` | `toast` | Short transient messages |
| `todos` | `todos` | Full todo list updates; every event is forwarded without deduplication and counts as an internal event for long-running reminders |

`context_notice` is opt-in because its chat delivery is a product decision rather than a correctness requirement. A pressure cycle keeps at most one durable notice at a time: `level` is the level at which that row was written, and later escalations in the same cycle are not pushed to the chat; they stay inside Chord, injected into the model's request context and recorded in its diagnostics. The notice is fire-and-forget: when Chord retracts one (for example after a model switch moved the effective threshold), nothing is pushed to withdraw the message already sent, so a chat that keeps the warnings enabled can hold a stale one until it scrolls away; a later re-crossing can then push a fresh notice.

The gateway preserves `session_id` and `message_index` from `background_result` and `context_notice` alongside their content. They identify the source session and durable transcript row; the active session still follows `ready` and `session_switched`.

Status queries include IDs of questions whose outcomes the gateway has not observed, so Core returns those decisions even when they fall outside the bounded history page. Complete historical browsing uses Core's question cursor; an omitted historical record does not imply a terminal outcome.

A session switch rebuilds the question collection. Historical replies belong to their original session and task. Disconnects retain known questions with an unknown outcome; resynchronize against Core snapshots and receipts instead of inventing outcomes.

## Long-running reminders

While a turn remains busy, the gateway sends a reminder every 5 minutes. Any user-visible output resets the next 5-minute reminder window. The reminder reports how long the current turn has been running, the tool calls and model retries observed since it started, and the tracked internal events since the previous visible output or reminder. It also names a coarse stage when the current activity is meaningful (`preparing`, `receiving reply`, `running tools`, `checkpointing context`, `retrying`, or `cooling down`); transport-level phases such as `connecting` stay hidden:

```text
⏳ Still working (12m03s · running tools · 7 tools · 1 retry · 4 internal events)
```

A retry counts every retry decision Chord announced during the turn, including fallback switches, so it is a pressure signal rather than an exact failure count. The summary covers the work observed since it started until global idle. Within the same session, confirmations, handoffs, and cancellation requests preserve that start time and the accumulated counts. An idle cancellation request does not start work or reminders. Queued messages start counting when Chord reports active work; time spent idle between work cycles is excluded.

Local shell commands are tracked separately: their results do not reset an active agent turn, and they remain busy until all requested shell commands have returned. When only shell work remains, reminders show its elapsed time and the `running local shell` stage.

Internal-event counts are currently based on gateway-tracked progress events such as `todos` and reset after each user-visible output or reminder. When `event_visibility.todos` is enabled, each `todos` event is pushed as the full current todo list without deduplication, even if it is unchanged or empty.

## Completion notifications

`assistant_message` is the primary final response sent to IM.

SubAgent `assistant_message` events are labeled with their agent type (falling back to agent ID) and task ID. `agent_done` is always subscribed and sends the authoritative completion summary; this also covers tool-only completion turns that have no assistant text.

`notification` is the canonical event for user alerts, including blocked errors, fully stopped completion, and the requests that have no prompt of their own (loop-exit decisions and notify-protocol corrections, all reported with Chord's `user_input_required` reason). Only a recognized companion `user_input_required` alert for a matching confirmation or handoff is dropped after its prompt has been delivered successfully. Other alerts remain visible even if the pending cache still contains an earlier request. Durable questions have no companion alert, so open questions never suppress these notifications.

`idle` and `status_response` carry `running_jobs`, the number of active background jobs whose completion will notify the current session. Idle notifications and `/status` show a nonzero count; the gateway keeps such processes out of idle-timeout cleanup. Completion wakes an agent, and its next `idle` updates the count. Services and watchers may run indefinitely. A job stopped without a completion notification leaves the count when the stop is requested: tool-driven stops are reflected in the active work's next `idle`, while `session_switched` clears the gateway's count for the previous session. When the gateway tracked a turn before this `idle`, `✅ Chord: Ready for input` (or the background-jobs variant) appends a compact summary of it: duration, tool calls reported across the main agent and SubAgents, and retry decisions. A turn with nothing measurable keeps the plain confirmation.

If local shell commands are still pending when agents become idle, the completion notification also reports `local shell commands still running: N`; shell reminders continue.

`idle` represents agent idle: the main agent and all SubAgents have stopped active work. Per-agent idle transitions are not exposed as protocol `idle` events, so the gateway keeps the process busy and does not stop reminders or send idle notifications while any agent is still working.

`idle` and `idle_timeout` do not close questions. Questions settle through durable ID-based outcomes; historical revisions use an explicit ID and retain the original outcome. `question_updated` pushes timer, interaction, requirement, and revision changes instead of silently updating a cache. A status snapshot can also restore missing answer entries or close a known open question; caching a snapshot does not consume a later revision notification.

Chord may also cancel a pending handoff without a decision, for example when a new request or session switch supersedes it. The gateway consumes the `handoff_cancelled` event, drops the stale pending handoff, and sends a cancellation notice. A later `/handoff` or `/handoff-deny` then falls back to the usual no-pending warning instead of routing the reply to the cancelled request. A `handoff_cancelled` event may arrive late or without a matching request ID; the gateway only clears the pending handoff it targets and leaves a newer pending request untouched.

Gateway JSONL parsing and stream failures produce a separate chat notice. Malformed lines are skipped; a failed stream stops the process before notification delivery, and the notice includes any inputs with no consumption receipt. These gateway-side failures do not wait for a Chord `notification` event.

## Logs

For observability, gateway logs include routing stages such as:

- `gateway event`
- `gateway routing event`
- `gateway sending notification`

See [operations.md](./operations.md) for log details.

## Input receipts

Normal messages and late replies carry correlation IDs. Writing to the process does not mean work started. Gateway follows `input_result` receipts and always-subscribed `activity` to update busy state and reminders. `handled` means a local command was consumed, `queued` means input was queued, `started` means a turn was created, and `rejected` means input was rejected. Receipts other than `started` never clear existing active work. Disconnects report unconfirmed inputs; check the session before resending requests with side effects. Requires Chord headless with input receipts.

## Generated images

The gateway subscribes to `image_result` and receives bounded `image_artifact` chunks. Images go to the source chat in order: WeChat uploads encrypted media; Feishu uploads message images with a tenant access token. All returned images are forwarded. Feishu images at or above 10 MB use a reduced PNG preview preserving transparency; originals remain in the Chord session.

This is outbound image delivery. The gateway accepts text input from IM; it does not upload incoming images or files to Chord. Attachments already accepted through TUI remain part of the transferred session and are restored by Chord.

When Chord finishes processing, remaining incomplete transfers are discarded and reported. Session switches and process shutdown also discard partial transfers. Corrupt or incomplete data is never uploaded. Each failed transfer is reported once; its remaining chunks are discarded without repeated notices.

Image processing is serialized across chats to limit transient memory, and queued originals stay on disk. Waiting for image-processing capacity is limited to 40 seconds; a timeout is reported as a delivery failure while the original remains in Chord. Previews preserve transparency and use a faster resize that may soften fine detail; originals are unchanged. Concurrent agents can deliver images independently.

WeChat CDN uploads retry network errors, 5xx responses and missing upload receipts up to three attempts total, using the same upload URL and ciphertext; 4xx and redirects are not retried. Feishu uploads do not retry transient failures; only an explicit expired-token response (HTTP 200 or an ordinary 4xx, excluding 408/429) triggers one refresh and retry. Feishu image processing, token waits, upload and final sending share a two-minute budget that ends when the adapter disconnects. Preview processing checks cancellation between decoding, resizing and encoding; a codec call already running finishes before cancellation is observed.

An exhausted upload, a platform rejection or an unconfirmed send is reported without replaying the send or regenerating the image. Check the chat first when delivery is unconfirmed, or view the saved original in Chord. A confirmed image or delivery notice resets the long-running reminder window. The gateway does not import local paths from Chord or expose original image bytes in `/status`.

### MCP status

`mcp_status_changed` is subscribed by default. It updates the complete MCP view and notifies changes to the visible status, separate from agent replies. Repeated identical notifications and intermediate operations are suppressed; stable problems on other servers remain visible. `/status` reads the same cached view. Login URLs and credentials are not part of these events; browser authorization happens in the session's working directory on the Chord host. A changed view replaces the previous visibility scope, and delayed snapshots cannot restore older MCP state.

Authentication and connection are reported separately. A connected server can still require sign-in. Automatic credential refresh is shown as an operation; it does not by itself require a host login.
