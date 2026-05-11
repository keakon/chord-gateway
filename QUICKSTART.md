# Quick Start

This guide helps you get `chord-gateway` running quickly with either WeChat iLink or Feishu.

## 1. Prerequisites

- Go 1.26+
- A working `chord` binary available in `PATH`, or configured through `chord_path`
- At least one IM platform configuration:
  - WeChat iLink (personal WeChat QR login)
  - Feishu app credentials (`app_id`, `app_secret`)

## 2. Install

Install with Go:

```bash
go install github.com/keakon/chord-gateway@latest
```

Or build from a local checkout:

```bash
go build -o chord-gateway .
```

## 3. Create a minimal config

Point `workspaces.default.path` at the project you want Chord to operate on.
Workspace paths must start with `/`, `~`, a Windows drive prefix (for example `C:\` or `C:/`), or a UNC prefix (for example `\\server\share`). `~` is expanded before use.

Important routing rules:

- WeChat always routes to one workspace through `ims.wechat.workspace_id`.
- Feishu uses `ims.feishu.chat_bindings` to map chat IDs to workspace IDs.
- If there is exactly one workspace, both WeChat `workspace_id` and Feishu `chat_bindings` may be omitted.

### 3.1 WeChat

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
workspaces:
  default:
    path: /path/to/project
chord_path: chord
idle_timeout: 30m
```

First run behavior:

- The gateway prints a QR-code URL.
- Scan it with WeChat to log in.
- The token is stored at `<state_dir>/wechat/token.json` for later runs. You can override it with `ims.wechat.token_path` if you want to manage that secret separately.

### 3.2 Feishu

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
workspaces:
  default:
    path: /path/to/project
chord_path: chord
idle_timeout: 30m
```

Feishu setup notes:

- In the Feishu developer console, set event subscription mode to “Receive events via long connection”.
- Subscribe to `im.message.receive_v1` in event subscriptions for inbound messages.
- Add `card.action.trigger` in callback configuration for interactive confirm/question cards.
- If you want different Feishu groups to route to different workspaces, start with a single workspace and no `chat_bindings`.
- Then create the target group, add the bot to it, send a text message (`text` or `post`), and run `/bind <workspace_id> <path>` in that same chat. If the path contains spaces, wrap it in double quotes, for example `/bind project-a "~/work/project a"`.
- `/bind` updates only Feishu `chat_bindings` and `workspaces` in the running gateway state, then persists the same two sections back to the YAML config file. It is not a general config reload path. Manual edits to the config file still require a gateway restart to take effect.
- If you prefer editing YAML yourself, you can also read the real group `chat_id` from the gateway log and fill in `chat_bindings` manually.
- For public deployments, also configure `owner_open_id` and/or `allowed_open_ids`. When neither is set, all users are allowed; when set, only listed `open_id`s can send messages and commands. To discover your `open_id`, send a message and check the gateway log for `open_id=ou_xxx` in the `feishu: received message` line.
- Inbound Feishu messages must be text messages (`text` or `post`). Non-text messages are ignored.
- No public callback URL, `verification_token`, `encrypt_key`, `listen`, or `webhook_path` is required.

Feishu checklist before you send the first message:

1. `app_id` and `app_secret` are valid, and the gateway can obtain an app access token at startup.
2. The app is configured for long connection mode in the Feishu developer console.
3. Event subscription is enabled and includes `im.message.receive_v1`.
4. Callback configuration includes `card.action.trigger` for interactive cards.
5. If you need a specific group to route to a specific workspace, create that group first, add the bot, send a text message (`text` or `post`), and run `/bind <workspace_id> <path>` in that same chat.
6. If you use `owner_open_id` or `allowed_open_ids`, your sender `open_id` is present in the effective allowlist. (To discover your `open_id`, send a message and check the gateway log for `open_id=ou_xxx`.)
7. Send a text message (`text` or `post`) and then `/status` from the target Feishu chat.

### 3.3 WeChat + Feishu with multiple workspaces

Use this when WeChat should stay on one fixed workspace while different Feishu groups map to different workspaces. The recommended rollout is:

1. Start with the single-workspace Feishu setup.
2. In each target Feishu chat, run `/bind <workspace_id> <path>` to create or update its binding.
3. Keep log-based `chat_id` discovery only as a fallback when you want to edit YAML manually.

Example final config:

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
    workspace_id: project-a
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
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
```

In this layout:

- all WeChat traffic goes to `project-a`
- Feishu chat `oc_project_a` goes to `project-a`
- Feishu chat `oc_project_b` goes to `project-b`

Recommended workflow to bind a Feishu group to a workspace:

1. Temporarily keep only one workspace and remove `chat_bindings`
2. Start the gateway
3. Create the target Feishu group and add the bot
4. Send a text message (`text` or `post`) in the group
5. In that same chat, send `/bind <workspace_id> <path>`
6. The gateway updates only Feishu `chat_bindings` and `workspaces` in memory, then writes the same two sections back to `config.yaml`.

If you prefer to edit YAML manually, you can still discover the real `chat_id` from a log line like:

```text
msg="feishu: received message" chat_id=oc_xxx open_id=ou_xxx ...
```

Then copy `chat_id=oc_xxx` back into `chat_bindings`.

Manual edits to the config file still require a gateway restart to take effect.

## 4. Run

```bash
chord-gateway -f config.yaml
```

After startup:

1. Send `/status` from the connected IM chat.
2. Confirm the gateway can resolve your workspace and reach `chord headless`.
3. Send a normal text request to start working with Chord.

## 5. Where state is stored

By default the gateway stores runtime state under:

- macOS / Linux: `${XDG_STATE_HOME:-~/.local/state}/chord-gateway`
- Config file: `${XDG_CONFIG_HOME:-~/.config}/chord-gateway/config.yaml`

State includes logs, WeChat token files (`<state_dir>/wechat/token.json` by default), Feishu dedupe data, and session pins. Feishu `app_id`/`app_secret` remain configuration credentials; its short-lived access token is kept in memory and refreshed as needed.

## 6. Next docs

- [Configuration reference](./docs/configuration.md)
- [IM integration overview](./docs/im.md)
- [Usage guide](./docs/usage.md)
- [Operations](./docs/operations.md)
- [Troubleshooting](./docs/troubleshooting.md)
