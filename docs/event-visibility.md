# Gateway control-plane event visibility

`chord-gateway` reads JSONL events from `chord headless`. Required control-plane events are always subscribed and routed; optional events are controlled by `event_visibility`.

- 中文版: [event-visibility_CN.md](./event-visibility_CN.md)

## Required events

The gateway always subscribes to these events:

- `assistant_message`
- `confirm_request`
- `question_request`
- `idle`
- `error`
- `notification`

These events provide the minimum behavior required for IM control:

- final assistant responses
- permission confirmations
- user questions
- busy/idle state aggregation
- error reporting
- canonical user-facing notifications

## Optional visible events

Optional events are disabled by default. Enable them through `event_visibility`:

```yaml
event_visibility:
  activity: false
  agent_done: false
  info: false
  toast: false
  tool_result: false
  todos: false
```

| Field | Event type | Typical use |
|---|---|---|
| `activity` | `activity` | Lower-level progress details. The gateway records phase state but does not expose phases in long-running reminders. |
| `agent_done` | `agent_done` | Sub-agent completion notifications |
| `info` | `info` | Informational messages |
| `toast` | `toast` | Short transient messages |
| `tool_result` | `tool_result` | Tool result summaries; counts as an internal event for long-running reminders |
| `todos` | `todos` | Full todo list updates; every event is forwarded without deduplication and counts as an internal event for long-running reminders |

## Long-running reminders

While a turn remains busy, the gateway sends a compact reminder every 5 minutes. Any user-visible output resets the next 5-minute reminder window. The reminder does not include low-level phases such as `connecting`. If tracked internal progress occurred, it includes the number of internal events observed since the previous visible output or reminder:

```text
⏳ Still working (4 internal events)
```

Internal-event counts are currently based on gateway-tracked progress events such as `tool_result` and `todos`. When `event_visibility.todos` is enabled, each `todos` event is pushed as the full current todo list without deduplication, even if it is unchanged or empty.

## Completion notifications

`assistant_message` is the primary final response sent to IM.

`notification` is the canonical event for user alerts, including permission requests, question requests, blocked errors, and fully stopped completion.

`idle` events normally do not emit fallback completion messages. If an `idle` event clears a pending question or confirmation, the gateway sends a targeted expiry notification instead of a generic completion message. The gateway also emits the same expiry notification before removing an idle process that still has a pending question or confirmation.

## Logs

For observability, gateway logs include routing stages such as:

- `gateway event`
- `gateway routing event`
- `gateway sending notification`

See [operations.md](./operations.md) for log details.
