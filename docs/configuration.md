# Configuration Reference

This document is the field-by-field reference for `chord-gateway` configuration.

## Top-Level Fields

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `ims` | object | yes | - | IM adapter map. Supported keys are `wechat` and `feishu`. |
| `workspaces` | object | yes | - | Workspace map keyed by workspace ID. Values define only the workspace path. |
| `chord_path` | string | no | `chord` | Path to the Chord binary. Supports `~` expansion. |
| `idle_timeout` | string | no | `30m` | Idle process timeout (Go duration format). |
| `event_visibility` | object | no | all `false` | Optional control-plane event visibility flags. |
| `session_pins_file` | string | no | `<state_dir>/session-pins.json` | Custom session pin storage path. |

Preferred config shape:

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
    workspace_id: default
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    chat_bindings:
      oc_project_a: project-a
      oc_project_b: project-b
workspaces:
  default:
    path: ~/project
  project-a:
    path: ~/work/project-a
  project-b:
    path: ~/work/project-b
```

`ims` and `workspaces` must now use the map form shown above; the old list / sequence forms are no longer supported, and `/bind` only reads and writes this map form.

## `ims`

Supported adapter keys:

- `wechat`
- `feishu`

At most one config per adapter type is allowed.

### WeChat Config (`ims.wechat`)

Task guide: [WeChat iLink guide](./wechat.md)

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `base_url` | string | no | `https://ilinkai.weixin.qq.com` | WeChat iLink API base URL |
| `workspace_id` | string | conditional | first/only workspace | Workspace that receives all WeChat traffic. Required when more than one workspace exists. |
| `token_path` | string | no | `<state_dir>/wechat/token.json` | Custom path for the persisted WeChat QR-login session token. Supports `~` expansion. |

Notes:

- The QR login API requires `bot_type=3`.
- The gateway hardcodes that fixed protocol constant and does not expose it in config.
- WeChat always routes to a single workspace.
- WeChat QR-login token is runtime state, not static config. By default it is saved as JSON at `<state_dir>/wechat/token.json`; set `token_path` only if you want to manage that secret in a custom location.

### Feishu Config (`ims.feishu`)

Task guide: [Feishu guide](./feishu.md)

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `app_id` | string | yes | - | Feishu app ID |
| `app_secret` | string | yes | - | Feishu app secret |
| `owner_open_id` | string | conditional | - | Owner `open_id`; required unless `allowed_open_ids` is non-empty |
| `allowed_open_ids` | array | no | `[]` | Additional allowed `open_id`s |
| `chat_bindings` | object | conditional | - | Mapping from Feishu chat ID to workspace ID. Required when more than one workspace exists. |

Feishu uses long connection mode to receive events:

- No public callback URL is required.
- Do not configure `verification_token`, `encrypt_key`, `listen`, or `webhook_path`.
- In the Feishu developer console under “Events and callbacks”, select “Receive events via long connection”.
- Subscribe to `im.message.receive_v1` under event subscriptions.
- Add `card.action.trigger` under callback configuration if you want interactive Feishu cards for confirm/question flows.

Feishu access control behavior:

- If neither `owner_open_id` nor `allowed_open_ids` is set, all messages and commands are ignored.
- Only messages and commands from listed `open_id`s are processed; all others are ignored after an audit log records only their `open_id` and `chat_id`.
- `owner_open_id` is automatically included in the effective allowlist.

How to discover your `open_id` during controlled setup:

1. Use a private chat or controlled test group, then start the gateway without `owner_open_id` or `allowed_open_ids`. No messages will be processed in this state.
2. Send a text message (`text` or `post`) from the Feishu chat.
3. Check the gateway log for a line like:

```text
msg="feishu: message from non-allowed open_id, ignoring" open_id=ou_xxx chat_id=oc_xxx
```

4. The audit log intentionally omits the untrusted message content. Copy the `open_id=ou_xxx` value and set it as `owner_open_id` or add it to `allowed_open_ids`.
5. Restart the gateway for the allowlist change to take effect. (`/bind` only updates Feishu `chat_bindings` and `workspaces`; it does not reload allowlist fields.)

You can also find `open_id` values through the Feishu admin console or the [Feishu Open Platform API](https://open.feishu.cn/document/server-docs/authentication/access-token/tenant_access_token).

Feishu routing behavior:

- If exactly one workspace exists, `chat_bindings` may be omitted and all Feishu chats route there.
- If multiple workspaces exist, `chat_bindings` is required.
- The keys in `chat_bindings` are the real Feishu `chat_id` values for chats (for example a group chat like `oc_xxx`), and the values are workspace IDs you define under `workspaces`.
- Each chat ID must map to a valid workspace ID.
- Multiple chat IDs may point to the same workspace.
- The recommended workflow is to start with a single workspace and no `chat_bindings`, then run `/bind <workspace_id> <path>` from the target Feishu chat. If the path contains spaces, wrap it in double quotes, for example `/bind project-a "~/work/project a"`.
- `/bind` accepts exactly two arguments. The path must start with `/`, `~`, a Windows drive prefix such as `C:\\` or `C:/`, or a UNC prefix such as `\\\\server\\share`; after expansion it must be accessible and must be a directory. Otherwise the command fails without changing the config file.
- `/bind` updates only Feishu `chat_bindings` and `workspaces` in the running gateway state, then persists the same two sections back to the YAML config file. It is not a general config reload path. Common YAML comments are preserved, but the file may be re-encoded with normalized formatting.
- Manual edits to the config file still require a gateway restart to take effect.
- If you prefer manual YAML edits, you can still discover the real `chat_id` from the gateway log and write `chat_bindings` yourself.

## `workspaces`

`workspaces` is a map keyed by workspace ID:

```yaml
workspaces:
  project-a:
    path: ~/work/project-a
  project-b:
    path: ~/work/project-b
```

Each workspace value supports:

| Field | Type | Required | Description |
|---|---|---|---|
| `path` | string | yes | Absolute or `~` path to project |

Validation rules:

- Workspace IDs must be non-empty and unique.
- Workspace paths are expanded from `~` before use.
- Routing is configured under each IM adapter, not inside workspace entries.

## Routing Model

The gateway uses an IM-owned routing model:

- WeChat defines one `workspace_id`.
- Feishu defines `chat_bindings`.
- Workspaces do not contain IM-specific routing fields.

This keeps workspace definitions stable while each IM adapter declares how it maps incoming chats to workspaces.

## `event_visibility`

Optional flags (boolean):

- `activity`
- `agent_started`
- `agent_notify`
- `info`
- `toast`
- `todos`

Essential events are always subscribed and cannot be disabled. This includes `agent_done`, so tool-only SubAgent completions always surface their summary.

`agent_started` and `agent_notify` are opt-in to avoid noisy orchestration updates. `done_completion` is always subscribed for non-loop Done reports. `todos` is subscribed, counted, and forwarded only when enabled. `activity` updates phase state for status/debugging, but long-running reminders do not expose low-level phases.

## Path and State Resolution

Only YAML config files are supported (`.yaml` or `.yml`).

Config file priority:

1. `--config` / `-f`
2. `CHORD_GATEWAY_CONFIG`
3. `$XDG_CONFIG_HOME/chord-gateway/config.yaml`
4. `~/.config/chord-gateway/config.yaml`

State directory priority:

1. `CHORD_GATEWAY_STATE_DIR`
2. `$XDG_STATE_HOME/chord-gateway`
3. `~/.local/state/chord-gateway`

Additional path behavior:

- `CHORD_GATEWAY_LOG_FILE` overrides the default log file location.
- The default log file is `<state_dir>/gateway.log`; it rotates at 10 MiB, keeps 3 backups, and does not gzip-compress rotated logs.
- `session_pins_file`, `ims.wechat.token_path`, `chord_path`, and `workspaces.<id>.path` support `~` expansion.
- The gateway does not watch for external config file changes. Config is loaded at startup.
- `/bind` is the only built-in path that updates Feishu `chat_bindings` and `workspaces` in memory and writes those same sections back to the YAML config file immediately. Other config changes still require a restart. `/bind` requires an existing directory path and may normalize YAML formatting while preserving common comments.

State data includes:

- logs
- WeChat token files (`<state_dir>/wechat/token.json` by default)
- Feishu dedupe store (`<state_dir>/dedupe.json`)
- session pins (`<state_dir>/session-pins.json` by default)

## Session pin behavior

The gateway stores one pinned Chord session ID per binding, keyed by:

```text
workspaceID | imType | chatID
```

That means different chats and different IMs keep separate pinned sessions, even when they point at the same workspace.

If `session_pins_file` is not set, the store defaults to `<state_dir>/session-pins.json`.

## Feishu binding workflow

For the task-oriented `/bind` procedure and manual `chat_id` discovery, see [Feishu guide — Multi-workspace routing](./feishu.md#multi-workspace-routing-with-bind). This reference only defines the `chat_bindings` field and its constraints above.

## Example: Feishu Multi-Workspace

Use this when you want different Feishu chats to route to different workspaces.

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner_open_id: ou_owner_xxx
    allowed_open_ids:
      - ou_owner_xxx
    chat_bindings:
      oc_project_a: project-a
      oc_project_b: project-b
workspaces:
  project-a:
    path: ~/work/project-a
  project-b:
    path: ~/work/project-b
chord_path: chord
idle_timeout: 30m
event_visibility:
  activity: false
  agent_started: false
  agent_notify: false
  info: false
  toast: false
  todos: false
```

## Example: WeChat + Feishu Multi-Workspace

Use this when WeChat should control one fixed workspace while Feishu groups map to different workspaces.

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
    workspace_id: project-a
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner_open_id: ou_owner_xxx
    allowed_open_ids:
      - ou_owner_xxx
    chat_bindings:
      oc_project_a: project-a
      oc_project_b: project-b
workspaces:
  project-a:
    path: ~/work/project-a
  project-b:
    path: ~/work/project-b
chord_path: chord
idle_timeout: 30m
event_visibility:
  activity: false
  agent_started: false
  agent_notify: false
  info: false
  toast: false
  todos: true
```

In this example:

- all WeChat traffic goes to `project-a`
- Feishu chat `oc_project_a` goes to `project-a`
- Feishu chat `oc_project_b` goes to `project-b`
