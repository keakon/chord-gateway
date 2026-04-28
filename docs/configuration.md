# Configuration Reference

This document is the field-by-field reference for `chord-gateway` configuration.

## Top-Level Fields

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `im` | object | no | - | Single-IM mode (backward compatible). |
| `ims` | array | no | - | Multi-IM mode. If non-empty, takes precedence over `im`. |
| `workspaces` | array | yes | - | Workspace list for routing. |
| `chord_path` | string | no | `chord` | Path to the Chord binary. Supports `~` expansion. |
| `idle_timeout` | string | no | `30m` | Idle process timeout (Go duration format). |
| `wechat_workspace_id` | string | no | first workspace | Optional workspace ID that receives all WeChat traffic. Required when WeChat is enabled alongside multiple workspaces. |
| `event_visibility` | object | no | all `false` | Optional control-plane event visibility flags. |
| `session_pins_file` | string | no | `<state_dir>/session-pins.json` | Custom session pin storage path. |

## `im` / `ims[]`

### Common

| Field | Type | Required | Description |
|---|---|---|---|
| `type` | string | yes | `wechat` or `feishu` |
| `wechat` | object | conditional | Required when `type=wechat` |
| `feishu` | object | conditional | Required when `type=feishu` |

### WeChat Config (`wechat`)

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `base_url` | string | no | `https://ilinkai.weixin.qq.com` | WeChat iLink API base URL |
| `bot_type` | string | no | `3` | iLink bot type |

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

Feishu access control behavior:

- If neither `owner_open_id` nor `allowed_open_ids` is set, all users are allowed.
- If either field is set, only listed `open_id`s are allowed.
- `owner_open_id` is automatically included in the effective allowlist.

## Workspace Routing Rules

Each workspace item:

| Field | Type | Required | Description |
|---|---|---|---|
| `id` | string | yes | Workspace identifier |
| `path` | string | yes | Absolute or `~` path to project |
| `im_chat_id` | string | conditional | Feishu/other IM chat binding key |

Validation rules:

- WeChat always routes to a single workspace.
- If WeChat is enabled and more than one workspace exists, `wechat_workspace_id` is required to choose which workspace receives WeChat traffic.
- Feishu with multiple workspaces requires non-empty and unique `im_chat_id` values.
- Feishu with one workspace can omit `im_chat_id`; all chats route to that single workspace.
- `im_chat_id` is ignored for WeChat routing.

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
- `session_pins_file`, `chord_path`, and `workspaces[].path` support `~` expansion.

State data includes:

- logs
- WeChat token files
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
im:
  type: feishu
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    verification_token: your-token
    listen: ":8080"
    webhook_path: /feishu/callback
    owner_open_id: ou_owner_xxx
    allowed_open_ids:
      - ou_owner_xxx
workspaces:
  - id: project-a
    path: ~/work/project-a
    im_chat_id: oc_project_a
  - id: project-b
    path: ~/work/project-b
    im_chat_id: oc_project_b
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
  - type: wechat
    wechat:
      base_url: https://ilinkai.weixin.qq.com
      bot_type: "3"
  - type: feishu
    feishu:
      app_id: cli_xxx
      app_secret: your-app-secret
      verification_token: your-token
      listen: ":8080"
      webhook_path: /feishu/callback
      owner_open_id: ou_owner_xxx
      allowed_open_ids:
        - ou_owner_xxx
wechat_workspace_id: project-a
workspaces:
  - id: project-a
    path: ~/work/project-a
    im_chat_id: oc_project_a
  - id: project-b
    path: ~/work/project-b
    im_chat_id: oc_project_b
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
