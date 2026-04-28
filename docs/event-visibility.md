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
| `activity` | `activity` | Lower-level progress details |
| `agent_done` | `agent_done` | Sub-agent completion notifications |
| `info` | `info` | Informational messages |
| `toast` | `toast` | Short transient messages |
| `tool_result` | `tool_result` | Tool result summaries |
| `todos` | `todos` | Todo list updates |

## Completion notifications

`assistant_message` is the primary final response sent to IM.

`notification` is the canonical event for user alerts, including permission requests, question requests, blocked errors, and fully stopped completion.

If an `idle` event indicates completion but no final assistant text or completion notification was sent, the gateway may send a fallback completion message so the IM user receives a clear end state.

## Logs

For observability, gateway logs include routing stages such as:

- `gateway event`
- `gateway routing event`
- `gateway sending notification`

See [operations.md](./operations.md) for log details.
