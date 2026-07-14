# Gateway control-plane event visibility

`chord-gateway` reads JSONL events from `chord headless`. Required control-plane events are always subscribed and routed; optional events are controlled by `event_visibility`.

- Chinese version: [event-visibility_CN.md](./event-visibility_CN.md)

## Required events

The gateway always subscribes to these events:

- `assistant_message`
- `confirm_request`
- `question_request`
- `handoff_request`
- `idle`
- `error`
- `notification`
- `done_completion`
- `local_shell_result`
- `agent_done`

These events provide the minimum behavior required for IM control:

- final assistant responses
- permission confirmations
- user questions
- handoff requests and responses
- busy/idle state aggregation
- error reporting
- canonical user-facing notifications
- non-loop Done completion reports
- local shell command results
- SubAgent completion summaries

## Optional visible events

Optional events are disabled by default. Enable them through `event_visibility`:

```yaml
event_visibility:
  activity: false
  agent_started: false
  agent_notify: false
  info: false
  toast: false
  todos: false
```

| Field | Event type | Typical use |
|---|---|---|
| `activity` | `activity` | Lower-level progress details. The gateway records phase state but does not expose phases in long-running reminders. |
| `agent_started` | `agent_started` | SubAgent delegation start notifications |
| `agent_notify` | `agent_notify` | Non-blocking owner or targeted delegated-workstream updates |
| `info` | `info` | Informational messages |
| `toast` | `toast` | Short transient messages |
| `todos` | `todos` | Full todo list updates; every event is forwarded without deduplication and counts as an internal event for long-running reminders |

## Long-running reminders

While a turn remains busy, the gateway sends a compact reminder every 5 minutes. Any user-visible output resets the next 5-minute reminder window. The reminder does not include low-level phases such as `connecting`. If tracked internal progress occurred, it includes the number of internal events observed since the previous visible output or reminder:

```text
⏳ Still working (4 internal events)
```

Internal-event counts are currently based on gateway-tracked progress events such as `todos`. When `event_visibility.todos` is enabled, each `todos` event is pushed as the full current todo list without deduplication, even if it is unchanged or empty.

## Completion notifications

`assistant_message` is the primary final response sent to IM.

SubAgent `assistant_message` events are labeled with their agent type (falling back to agent ID) and task ID. `agent_done` is always subscribed and sends the authoritative completion summary; this also covers tool-only completion turns that have no assistant text.

`notification` is the canonical event for user alerts, including permission requests, question requests, blocked errors, and fully stopped completion.

`idle` represents global idle: the main agent and all SubAgents have stopped active work. Per-agent idle transitions are not exposed as protocol `idle` events, so the gateway keeps the process busy and does not stop reminders or send idle notifications while any agent is still working.

Global `idle` events normally do not emit fallback completion messages. If one clears a pending question, confirmation, or handoff request, the gateway sends a targeted expiry notification instead of a generic completion message. The gateway also emits the same expiry notification before removing an idle process that still has a pending question, confirmation, or handoff request.

## Logs

For observability, gateway logs include routing stages such as:

- `gateway event`
- `gateway routing event`
- `gateway sending notification`

See [operations.md](./operations.md) for log details.
