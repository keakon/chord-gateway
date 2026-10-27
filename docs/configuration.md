# Configuration Reference

All IM platforms use one paired owner and explicit chat bindings, including single-workspace deployments. Run `chord-gateway --pair`, then send `/bind "<directory>" <pairing_code>` in a WeChat/Feishu/Telegram/WeCom/DingTalk/Discord private bot chat. See [pairing and local reset](./pairing.md) and the [upgrade notes](../CHANGELOG.md). Slack uses `/chord bind "<directory>" <pairing_code>` in the App DM; see the [Slack guide](./slack.md).

This document is the field-by-field reference for `chord-gateway` configuration.

## Top-Level Fields

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `ims` | object | yes | - | IM adapter map. Available adapters are `wechat`, `feishu`, `telegram`, `wecom`, `dingtalk`, `discord`, and `slack`. |
| `workspaces` | object | no | - | Workspace map keyed by workspace ID. Values define only the workspace path. |
| `chord_path` | string | no | `chord` | Path to the Chord binary. Supports `~` expansion. |
| `chord_state_dir` | string | no | Chord's default state directory | Chord state directory shared by managed headless and takeover CLI; set it to match a TUI using custom `--state-dir`. Supports `~` expansion. |
| `idle_timeout` | string | no | `30m` | Idle process timeout (Go duration format). |
| `event_visibility` | object | no | all `false` except `background_result` (`true`) | Control-plane event visibility flags. |
| `session_pins_file` | string | no | `<state_dir>/session-pins.json` | Custom session pin storage path. |
| `proxy_url` | string | no | - | Optional outbound proxy applied to all IM traffic. Empty means direct. See [Proxy](#proxy). |

Preferred config shape:

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
    owner: {account_id: bot_example, user_id: user_example}
    chat_bindings:
      user_example: default
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner: {account_id: cli_xxx, user_id: ou_owner_xxx}
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
- `telegram`
- `wecom`
- `dingtalk`
- `discord`
- `slack`

At most one config per adapter type is allowed.

### WeChat Config (`ims.wechat`)

Task guide: [WeChat iLink guide](./wechat.md)

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `base_url` | string | no | `https://ilinkai.weixin.qq.com` | WeChat iLink API base URL |
| `owner` | object | after pairing | - | `account_id`, `user_id` |
| `chat_bindings` | object | after binding | - | chat ID → workspace ID |
| `token_path` | string | no | `<state_dir>/wechat/token.json` | Custom path for the persisted WeChat QR-login session token. Supports `~` expansion. |
| `proxy_url` | string | no | - | Per-IM proxy override for this adapter. See [Proxy](#proxy). |

Notes:

- The QR login API requires `bot_type=3`.
- The gateway hardcodes that fixed protocol constant and does not expose it in config.
- Each WeChat chat routes through its explicit binding.
- WeChat QR-login token is runtime state, not static config. By default it is saved as JSON at `<state_dir>/wechat/token.json`; set `token_path` only if you want to manage that secret in a custom location.

### Feishu Config (`ims.feishu`)

Task guide: [Feishu guide](./feishu.md)

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `app_id` | string | yes | - | Feishu app ID |
| `app_secret` | string | yes | - | Feishu app secret |
| `owner` | object | after pairing | - | `account_id`, `user_id` |
| `chat_bindings` | object | conditional | - | Mapping from Feishu chat ID to workspace ID. Required for each chat before use. |
| `proxy_url` | string | no | - | Per-IM proxy override for this adapter. See [Proxy](#proxy). |

Feishu uses long connection mode to receive events:

- No public callback URL is required.
- Do not configure `verification_token`, `encrypt_key`, `listen`, or `webhook_path`.
- In the Feishu developer console under “Events and callbacks”, select “Receive events via long connection”.
- Subscribe to `im.message.receive_v1` under event subscriptions.
- Add `card.action.trigger` under callback configuration if you want interactive Feishu cards for confirm/question flows.

### Telegram configuration (`ims.telegram`)

Task guide: [Telegram integration](./telegram.md)

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `bot_token` | string | yes | BotFather-issued token; custom API endpoints are not configurable |
| `owner` | object | after pairing | Numeric Bot ID as `account_id` and numeric owner user ID as `user_id`, both stored as strings |
| `chat_bindings` | object | after binding | Numeric chat ID string → workspace ID; retain the minus sign for groups |
| `proxy_url` | string | no | Per-IM proxy override for this adapter. See [Proxy](#proxy). |

Uses Bot API long polling. An existing webhook blocks startup and is never automatically deleted. Start with only `bot_token`; foreground `--pair` and private `/bind` save authorization.

### WeCom Config (`ims.wecom`)

See the [WeCom guide](./wecom.md). Requires the intelligent bot API long connection. The endpoint is fixed at `wss://openws.work.weixin.qq.com`. An `endpoint` YAML field is unsupported: it is ignored with a warning and cannot override this address.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `bot_id` | string | yes | Intelligent bot ID |
| `secret` | string | yes | Intelligent bot secret |
| `owner` | object | after pairing | `account_id` and `user_id`, written by pairing |
| `chat_bindings` | object | after binding | Encoded chat ID → workspace ID, written by `/bind` |
| `proxy_url` | string | no | Per-IM proxy override for this adapter. See [Proxy](#proxy). |

Initial pairing and later binds require a successful fixed-message probe ACK before saving. Secret rotation preserves the owner when the bot ID stays the same. A changed bot ID requires stopping the service and running `chord-gateway -f config.yaml pairing reset --platform wecom` in a local controlling terminal; type `wecom` to confirm, then pair again.

### DingTalk Config (`ims.dingtalk`)

Deployment guide: [DingTalk](./dingtalk.md). All four identity/credential fields are required; the robot code is never inferred.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `client_id` | string | yes | Enterprise internal-app AppKey |
| `client_secret` | string | yes | Enterprise internal-app AppSecret |
| `robot_code` | string | yes | Robot code from the console |
| `corp_id` | string | yes | Enterprise ID |
| `owner` | object | after pairing | `account_id`, `user_id` |
| `chat_bindings` | object | after binding | Gateway-encoded target ID → workspace ID, saved by `/bind` |
| `proxy_url` | string | no | Per-IM proxy override for this adapter. See [Proxy](#proxy). |

The first Stream connection needs an employee private `hello` to verify identity before a DingTalk binding is authorized. Candidate targets receive a proactive probe before binding. Webhook, API host, and Stream endpoint overrides are not configurable. Access tokens stay in memory.

### Discord Config (`ims.discord`)

Task guide: [Discord bot guide](./discord.md)

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `bot_token` | string | yes | - | Discord bot token; protect as a secret |
| `application_id` | string | yes | - | Application ID belonging to the authenticated bot |
| `owner` | object | after pairing | - | `account_id` is the authenticated bot user ID; `user_id` is the paired owner |
| `chat_bindings` | object | after binding | - | DM, text channel, or thread ID → workspace ID |
| `proxy_url` | string | no | - | Per-IM proxy override for this adapter. See [Proxy](#proxy). |

Discord uses bot DMs and explicit @Bot messages in server channels/threads. There is no `message_content` switch and no privileged intent requirement. Each thread needs its own binding and access permission; no parent-channel fallback is used. Install with View Channel, Send Messages, and Send Messages in Threads.

### Slack Config (`ims.slack`)

Task guide: [Slack guide](./slack.md)

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `app_token` | string | yes | App-level token with `connections:write` |
| `bot_token` | string | yes | Bot installation token |
| `app_id` | string | yes | App ID verified against the Bot and Socket Mode hello |
| `team_id` | string | yes | Workspace/team ID verified by `auth.test` |
| `owner` | object | after pairing | Verified account identity and owner user ID |
| `chat_bindings` | object | after binding | Root channel ID or encoded thread target → workspace ID |
| `proxy_url` | string | no | Per-IM proxy override for this adapter. See [Proxy](#proxy). |

Use `/chord bind ...` in root chats and `@Bot /bind ...` inside a thread. Each root and thread requires its own binding. All four credentials are required; endpoints are fixed and cannot be overridden in YAML. See the Slack guide for scopes, installation and supported targets.

### Authorization and binding

All platforms accept only the paired owner's input. Start with `chord-gateway --pair`: WeChat/Feishu/Telegram/WeCom/DingTalk/Discord private pairing uses `/bind "<directory>" <pairing_code>`; Slack App DM pairing uses `/chord bind "<directory>" <pairing_code>`. Later bindings omit the code. Slack roots use `/chord bind ...` and threads use `@Bot /bind ...` inside that thread. All chats require explicit binding, including single-workspace deployments.

Binding registers an existing directory and enables the route only after saving YAML. Directories are reused by filesystem identity, never created or cloned. Resolve pending inputs, background work, and interactions before switching directories. See [pairing](./pairing.md) for path and recovery rules. Stop the service before manual configuration edits, then restart.

## Proxy

Optional outbound proxy for IM API and long-connection traffic:

```yaml
proxy_url: "http://127.0.0.1:8080"
ims:
  telegram:
    bot_token: your-bot-token
    proxy_url: "socks5h://127.0.0.1:1080"
```

- Top-level `proxy_url` applies to all IMs; `ims.<platform>.proxy_url` overrides it for that platform.
- Priority: per-IM > global. Empty means direct; environment proxy variables are ignored.
- Supported schemes: `http`, `https`, `socks5`, `socks5h` (`socks5` and `socks5h` behave identically in Go).
- WebSocket long-connections (Discord Gateway, Slack/Feishu/WeCom/DingTalk sockets) **do not support an `https` proxy**. A platform's *effective* proxy is its per-IM `proxy_url` when set, otherwise the global one; an `https` effective proxy is rejected at startup when a WebSocket platform resolves to it. Use `http`, `socks5`, or `socks5h` for those platforms.
- WebSocket long-connections normalize `socks5h` to `socks5` automatically; either spelling works.
- The scheme must be explicit; credentials in the URL are never logged.

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

- All supported platforms define `chat_bindings`.
- Workspaces do not contain IM-specific routing fields.

This keeps workspace definitions stable while each IM adapter declares how it maps incoming chats to workspaces.

## `event_visibility`

Control-plane event flags:

- `agent_started`
- `agent_notify`
- `background_result`
- `context_notice`
- `info`
- `toast`
- `todos`

Essential events are always subscribed and cannot be disabled. This includes `agent_done`, so tool-only SubAgent completions always surface their summary.

Most flags are opt-in and default to `false`. `background_result` is the exception: finished background job results are pushed to the chat by default, and setting `background_result: false` turns those pushes off; the result stays in the Chord session.

`agent_started` and `agent_notify` are opt-in to avoid noisy orchestration updates. `done_completion` is always subscribed for non-loop Done reports. `todos` is subscribed, counted, and forwarded only when enabled. `activity` is always subscribed to track active work, but long-running reminders do not expose low-level phases. `context_notice` is opt-in because pushing durable context-pressure warnings to a chat is a product decision rather than a correctness requirement; see [event-visibility.md](./event-visibility.md) for the volume and retraction caveats.

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
- `session_pins_file`, `ims.wechat.token_path`, `chord_path`, `chord_state_dir`, and `workspaces.<id>.path` support `~` expansion.
- The gateway does not watch for external config file changes. Config is loaded at startup.
- Unsupported or removed YAML keys, including fields inherited through anchors and merges, are reported in the gateway log with their source line and ignored; the file still loads.
- `/bind` updates the selected platform owner and `chat_bindings` only after durable YAML writeback. Local `pairing reset --platform <platform>` removes that platform identity and bindings. Other config changes still require a restart. `/bind` requires an existing directory path and may normalize YAML formatting while preserving common comments.

State data includes:

- logs
- WeChat token files (`<state_dir>/wechat/token.json` by default)
- session pins (`<state_dir>/session-pins.json` by default)
- session transfer guards (`<state_dir>/session-transfers.json`)

`chord_state_dir` refers to Chord's state, not the gateway's state directory above. Leave it unset for Chord's normal default; when the source TUI uses a custom directory, set the same path so takeover queries and target resume find the same session and runtime. It does not change IM or workspace authorization.

## Session pin behavior

The gateway stores one pinned Chord session ID per binding, keyed by:

```text
platform + account + owner + workspace + chat (encoded)
```

That means different chats and different IMs keep separate pinned sessions, even when they point at the same workspace.

If `session_pins_file` is not set, the store defaults to `<state_dir>/session-pins.json`.

A transfer guard takes precedence over the pin. After a transfer starts, neither ordinary input nor automatic restart can resume that pin or fall back to a fresh session. The guard survives gateway restarts. When takeover into this chat reaches `ready_paused` and its session pin is saved, the target binding opens for explicit continuation. A binding that transferred its session out remains blocked until an explicit `/new` or `/resume` succeeds, or until `/takeover <session-id>` supersedes an outgoing transfer that already finished (`released` or `saved`); failed attempts retain the guard. See [usage.md](./usage.md#session-takeover).

## Feishu binding workflow

For the task-oriented `/bind` procedure and manual `chat_id` discovery, see [Feishu guide — Multi-workspace routing](./feishu.md#multi-workspace-routing-with-bind). This reference only defines the `chat_bindings` field and its constraints above.

## Example: Feishu Multi-Workspace

Use this when you want different Feishu chats to route to different workspaces.

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner: {account_id: cli_xxx, user_id: ou_owner_xxx}
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
  background_result: true
  agent_started: false
  agent_notify: false
  context_notice: false
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
    owner: {account_id: bot_example, user_id: user_example}
    chat_bindings:
      user_example: project-a
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner: {account_id: cli_xxx, user_id: ou_owner_xxx}
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
  background_result: true
  agent_started: false
  agent_notify: false
  context_notice: false
  info: false
  toast: false
  todos: true
```

In this example:

- the bound WeChat chat `user_example` goes to `project-a`
- Feishu chat `oc_project_a` goes to `project-a`
- Feishu chat `oc_project_b` goes to `project-b`

All platforms share bounded in-memory intake, authorization and delivery receipts. Gateway does not restore its inbox, deduplication or polling cursors after restart. Check the original Chord session before resending an uncertain task. See [delivery semantics](./operations.md#message-delivery-and-restarts).

Account locks use verified stable account identities and cover local instances under the same OS user. Cross-host deployments still require a single active instance.
