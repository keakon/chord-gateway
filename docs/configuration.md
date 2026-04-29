# Configuration Reference

This document is the field-by-field reference for `chord-gateway` configuration.

## Top-Level Fields

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `ims` | array | yes | - | IM adapter list. Each item contains exactly one platform block. |
| `workspaces` | array | yes | - | Workspace list. These entries only define workspace identity and path. |
| `chord_path` | string | no | `chord` | Path to the Chord binary. Supports `~` expansion. |
| `idle_timeout` | string | no | `30m` | Idle process timeout (Go duration format). |
| `event_visibility` | object | no | all `false` | Optional control-plane event visibility flags. |
| `session_pins_file` | string | no | `<state_dir>/session-pins.json` | Custom session pin storage path. |

## `ims[]`

Each entry in `ims` must contain exactly one platform object.

Examples:

```yaml
ims:
  - wechat:
      base_url: https://ilinkai.weixin.qq.com
      workspace_id: default

  - feishu:
      app_id: cli_xxx
      app_secret: your-app-secret
      chat_bindings:
        oc_project_a: project-a
        oc_project_b: project-b
```

### WeChat Config (`wechat`)

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `base_url` | string | no | `https://ilinkai.weixin.qq.com` | WeChat iLink API base URL |
| `workspace_id` | string | conditional | first workspace | Workspace that receives all WeChat traffic. Required when more than one workspace exists. |
| `token_path` | string | no | `<state_dir>/wechat/token.json` | Custom path for the persisted WeChat QR-login session token. Supports `~` expansion. |

Notes:

- The QR login API requires `bot_type=3`.
- The gateway hardcodes that fixed protocol constant and does not expose it in config.
- WeChat always routes to a single workspace.
- WeChat QR-login token is runtime state, not static config. By default it is saved as JSON at `<state_dir>/wechat/token.json`; set `token_path` only if you want to manage that secret in a custom location.
- Existing legacy files at `<state_dir>/token.json` and `<state_dir>/sync-buf.json` are migrated automatically to the `wechat/` state subdirectory.

### Feishu Config (`feishu`)

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `app_id` | string | yes | - | Feishu app ID |
| `app_secret` | string | yes | - | Feishu app secret |
| `verification_token` | string | no | - | Webhook verification token |
| `encrypt_key` | string | no | - | Event encryption key |
| `listen` | string | no | `:8080` | HTTP listen address |
| `webhook_path` | string | no | `/feishu/callback` | Webhook path |
| `owner_open_id` | string | no | - | Owner `open_id`; if set, it is part of the effective allowlist |
| `allowed_open_ids` | array | no | `[]` | Additional allowed `open_id`s |
| `chat_bindings` | object | conditional | - | Mapping from Feishu chat ID to workspace ID. Required when more than one workspace exists. |

Feishu access control behavior:

- If neither `owner_open_id` nor `allowed_open_ids` is set, all users are allowed.
- If either field is set, only listed `open_id`s are allowed.
- `owner_open_id` is automatically included in the effective allowlist.

Feishu routing behavior:

- If exactly one workspace exists, `chat_bindings` may be omitted and all Feishu chats route there.
- If multiple workspaces exist, `chat_bindings` is required.
- Each chat ID must map to a valid workspace ID.
- Multiple chat IDs may point to the same workspace.

## `workspaces[]`

Each workspace item:

| Field | Type | Required | Description |
|---|---|---|---|
| `id` | string | yes | Workspace identifier |
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
- `agent_done`
- `info`
- `toast`
- `tool_result`
- `todos`

Essential events are always subscribed and cannot be disabled.

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
- The default log file is `<state_dir>/gateway.log`.
- `session_pins_file`, `ims[].wechat.token_path`, `chord_path`, and `workspaces[].path` support `~` expansion.

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

## Example: Feishu Multi-Workspace

Use this when you want different Feishu chats to route to different workspaces.

```yaml
ims:
  - feishu:
      app_id: cli_xxx
      app_secret: your-app-secret
      verification_token: your-token
      listen: ":8080"
      webhook_path: /feishu/callback
      owner_open_id: ou_owner_xxx
      allowed_open_ids:
        - ou_owner_xxx
      chat_bindings:
        oc_project_a: project-a
        oc_project_b: project-b
workspaces:
  - id: project-a
    path: ~/work/project-a
  - id: project-b
    path: ~/work/project-b
chord_path: chord
idle_timeout: 30m
event_visibility:
  activity: false
  agent_done: false
  info: false
  toast: false
  tool_result: false
  todos: false
```

## Example: WeChat + Feishu Multi-Workspace

Use this when WeChat should control one fixed workspace while Feishu groups map to different workspaces.

```yaml
ims:
  - wechat:
      base_url: https://ilinkai.weixin.qq.com
      workspace_id: project-a
  - feishu:
      app_id: cli_xxx
      app_secret: your-app-secret
      verification_token: your-token
      listen: ":8080"
      webhook_path: /feishu/callback
      owner_open_id: ou_owner_xxx
      allowed_open_ids:
        - ou_owner_xxx
      chat_bindings:
        oc_project_a: project-a
        oc_project_b: project-b
workspaces:
  - id: project-a
    path: ~/work/project-a
  - id: project-b
    path: ~/work/project-b
chord_path: chord
idle_timeout: 30m
event_visibility:
  activity: false
  agent_done: false
  info: false
  toast: false
  tool_result: false
  todos: true
```

In this example:

- all WeChat traffic goes to `project-a`
- Feishu chat `oc_project_a` goes to `project-a`
- Feishu chat `oc_project_b` goes to `project-b`
