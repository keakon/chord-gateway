# Operations

This page describes runtime behavior that matters when operating `chord-gateway`.

## Process lifecycle

The gateway starts `chord headless` as a child process for each active binding as needed.

Each spawned Chord process is placed into its own process group. On normal gateway shutdown, the gateway attempts to terminate the whole process group:

1. Close stdin to request graceful exit.
2. Send `SIGTERM` to the process group.
3. After a short grace period, send `SIGKILL` to the process group.

This prevents orphaned Chord processes and child processes from keeping session locks or workspace resources alive.

If the gateway itself is killed with `SIGKILL` (`kill -9`), it cannot run cleanup handlers. Chord's parent-death watcher is expected to detect the parent process change and exit promptly.

## Idle timeout

`idle_timeout` controls how long an idle Chord process may remain alive. The default is `30m`. The same timeout applies even if the process is waiting on a pending question or confirmation.

If the gateway removes an idle process while a question or confirmation is still pending, it first records the expired interaction, sends an expiry notification to the IM chat, and then terminates the process. A later `/answer`, `/allow`, or `/deny` cannot use the old structured request ID; the router forwards it as follow-up context instead.

Use Go duration syntax, for example:

```yaml
idle_timeout: 30m
```

## State directory

Runtime state is stored in this priority order:

1. `$CHORD_GATEWAY_STATE_DIR`
2. `$XDG_STATE_HOME/chord-gateway`
3. `~/.local/state/chord-gateway`

State data includes:

- logs
- WeChat token files (`<state_dir>/wechat/token.json` by default, or `ims.wechat.token_path`)
- WeChat sync cursor (`<state_dir>/wechat/sync-buf.json`)
- Feishu dedupe store
- session pin store

The gateway writes WeChat token/sync state, Feishu dedupe state, and session-pin state with atomic file replacement so crashes or abrupt restarts do not leave truncated state files behind.

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

The dedupe key is based on:

```text
app_id + chat_id + message_id
```

Deduplication data is persisted to:

```text
<state_dir>/dedupe.json
```

The current TTL is 24 hours.

## Workspace routing

WeChat always routes to one workspace. If multiple workspaces are configured and WeChat is enabled, set `ims.wechat.workspace_id` to choose the workspace used by WeChat.

Feishu supports multiple workspaces through `ims.feishu.chat_bindings`, which maps Feishu `chat_id` values to workspace IDs.

For a single Feishu workspace, `chat_bindings` may be omitted and all chats use that workspace. For multiple Feishu workspaces, configure `chat_bindings` for the chats you want to route.

## Event contract

The gateway runs `chord headless` and reads JSONL events from stdout. It subscribes to the required control-plane event set by default:

- `idle`
- `assistant_message`
- `confirm_request`
- `question_request`
- `error`
- `notification`

Optional events are controlled by `event_visibility`. See [event-visibility.md](./event-visibility.md).
