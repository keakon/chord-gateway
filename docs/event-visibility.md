# Gateway control-plane event visibility

`chord-gateway` reads JSONL events from `chord headless`. Required control-plane events are always subscribed and routed; optional events are controlled by `event_visibility`.

- Chinese version: [event-visibility_CN.md](./event-visibility_CN.md)

## Required events

The gateway always subscribes to these events:

- `activity`
- `assistant_message`
- `confirm_request`
- `question_request`
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
- `background_result`

These events provide the minimum behavior required for IM control:

- final assistant responses
- permission confirmations
- user questions
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
- finished background job results, which have no other delivery channel

`session_switched` is state-only: the command that caused the switch already answers the user, so no extra chat message is sent. It does re-point the binding's session pin at the newly active session, so later spawns resume the session the runtime actually runs.

`background_result` is pushed to IM as a chat message, because Chord has no other channel for it: a background job usually finishes after the turn went idle, so no later `assistant_message` summarizes it.

## Optional visible events

Optional events are disabled by default. Enable them through `event_visibility`:

```yaml
event_visibility:
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
| `context_notice` | `context_notice` | Durable context-pressure warnings, pushed as chat messages. `level` is one of `pressure`, `imminent` or `warning`. |
| `info` | `info` | Informational messages |
| `toast` | `toast` | Short transient messages |
| `todos` | `todos` | Full todo list updates; every event is forwarded without deduplication and counts as an internal event for long-running reminders |

`context_notice` is opt-in because its chat delivery is a product decision rather than a correctness requirement. A pressure cycle keeps at most one durable notice at a time: `level` is the level at which that row was written, and later escalations in the same cycle are not pushed to the chat; they stay inside Chord, injected into the model's request context and recorded in its diagnostics. The notice is fire-and-forget: when Chord retracts one (for example after a model switch moved the effective threshold), nothing is pushed to withdraw the message already sent, so a chat that keeps the warnings enabled can hold a stale one until it scrolls away; a later re-crossing can then push a fresh notice.

## Long-running reminders

While a turn remains busy, the gateway sends a compact reminder every 5 minutes. Any user-visible output resets the next 5-minute reminder window. The reminder does not include low-level phases such as `connecting`. If tracked internal progress occurred, it includes the number of internal events observed since the previous visible output or reminder:

```text
⏳ Still working (4 internal events)
```

Internal-event counts are currently based on gateway-tracked progress events such as `todos`. When `event_visibility.todos` is enabled, each `todos` event is pushed as the full current todo list without deduplication, even if it is unchanged or empty.

## Completion notifications

`assistant_message` is the primary final response sent to IM.

SubAgent `assistant_message` events are labeled with their agent type (falling back to agent ID) and task ID. `agent_done` is always subscribed and sends the authoritative completion summary; this also covers tool-only completion turns that have no assistant text.

`notification` is the canonical event for user alerts, including blocked errors, fully stopped completion, and the requests that have no prompt of their own (loop-exit decisions and notify-protocol corrections, all reported with Chord's `user_input_required` reason). Only a recognized companion `user_input_required` alert for a matching confirmation or handoff is dropped after its prompt has been delivered successfully. Other alerts remain visible even if the pending cache still contains an earlier request.

`idle` and `status_response` carry `running_jobs`, the number of active background jobs whose completion will notify the current session. Idle notifications and `/status` show a nonzero count; the gateway keeps such processes out of idle-timeout cleanup. Completion wakes an agent, and its next `idle` updates the count. Services and watchers may run indefinitely. A job stopped without a completion notification leaves the count when the stop is requested: tool-driven stops are reflected in the active work's next `idle`, while `session_switched` clears the gateway's count for the previous session.

`idle` represents agent idle: the main agent and all SubAgents have stopped active work. Per-agent idle transitions are not exposed as protocol `idle` events, so the gateway keeps the process busy and does not stop reminders or send idle notifications while any agent is still working.

Global `idle` events normally do not emit fallback completion messages. If one clears a pending question, confirmation, or handoff request, the gateway sends a targeted expiry notification instead of a generic completion message. The gateway also emits the same expiry notification before removing an idle process that still has a pending question, confirmation, or handoff request. Chord may include `suppress_user_notification: true` for configuration-only quiescence, such as a manual model-pool switch; the gateway still records the process as idle and stops reminders, but skips the generic idle message. Pending-interaction expiry notifications still take precedence.

Chord may also cancel a pending handoff without a decision, for example when a new request or session switch supersedes it. The gateway consumes the `handoff_cancelled` event, drops the stale pending handoff, and sends a cancellation notice. A later `/handoff` or `/handoff-deny` then falls back to the usual no-pending warning instead of routing the reply to the cancelled request. A `handoff_cancelled` event may arrive late or without a matching request ID; the gateway only clears the pending handoff it targets and leaves a newer pending request untouched.

## Logs

For observability, gateway logs include routing stages such as:

- `gateway event`
- `gateway routing event`
- `gateway sending notification`

See [operations.md](./operations.md) for log details.

## Input receipts

Normal messages and late replies carry correlation IDs. Writing to the process does not mean work started. Gateway follows `input_result` receipts and always-subscribed `activity` to update busy state and reminders. `handled` means a local command was consumed, `queued` means input was queued, `started` means a turn was created, and `rejected` means input was rejected. Receipts other than `started` never clear existing active work. Disconnects report unconfirmed inputs; check the session before resending requests with side effects. Requires Chord headless with input receipts.
