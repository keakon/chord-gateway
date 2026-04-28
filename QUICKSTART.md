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

Point `workspaces[].path` at the project you want Chord to operate on.

Important routing rules:

- WeChat always routes to one workspace.
- Feishu with multiple workspaces requires a unique `im_chat_id` on every workspace.
- If WeChat is enabled alongside multiple workspaces, set `wechat_workspace_id` to choose the workspace used by WeChat.

### 3.1 WeChat

```yaml
im:
  type: wechat
  wechat:
    base_url: https://ilinkai.weixin.qq.com
    bot_type: "3"
workspaces:
  - id: default
    path: /path/to/project
chord_path: chord
idle_timeout: 30m
```

First run behavior:

- The gateway prints a QR-code URL.
- Scan it with WeChat to log in.
- The token is stored under the gateway state directory for later runs.

### 3.2 Feishu

```yaml
im:
  type: feishu
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    verification_token: your-token
    listen: ":8080"
    webhook_path: /feishu/callback
workspaces:
  - id: default
    path: /path/to/project
chord_path: chord
idle_timeout: 30m
```

Feishu setup notes:

- Expose the configured `listen + webhook_path` as a public HTTPS URL.
- Configure that URL in the Feishu developer console event subscription settings.
- Subscribe at least to `im.message.receive_v1` for inbound messages and `card.action.trigger` for interactive confirm/question actions.
- If Feishu event encryption is enabled, also set `encrypt_key`.
- For public deployments, also configure `owner_open_id` and/or `allowed_open_ids`.
- Inbound Feishu messages must be plain text. Non-text messages are ignored.

Feishu checklist before you send the first message:

1. `app_id` and `app_secret` are valid, and the gateway can obtain an app access token at startup.
2. The public callback URL resolves to `listen + webhook_path` and responds to Feishu URL verification.
3. `verification_token` matches the value configured in the Feishu developer console.
4. If event encryption is enabled in Feishu, `encrypt_key` is set to the same value in the gateway config.
5. Event subscription is enabled and includes `im.message.receive_v1` and `card.action.trigger`.
6. If you use `owner_open_id` or `allowed_open_ids`, your sender `open_id` is present in the effective allowlist.
7. Send a plain text message and then `/status` from the target Feishu chat.

### 3.3 WeChat + Feishu with multiple workspaces

Use this when WeChat should stay on one fixed workspace while Feishu groups map to different workspaces.

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
```

In this layout:

- all WeChat traffic goes to `project-a`
- Feishu chat `oc_project_a` goes to `project-a`
- Feishu chat `oc_project_b` goes to `project-b`

## 4. Run

```bash
chord-gateway -f config.yaml
```

After startup:

1. Send `/status` from the connected IM chat.
2. Confirm the gateway can resolve your workspace and reach `chord headless`.
3. Send a normal text request to start working with Chord.

Config file resolution priority:

1. `--config` / `-f`
2. `CHORD_GATEWAY_CONFIG`
3. `$XDG_CONFIG_HOME/chord-gateway/config.yaml`
4. `~/.config/chord-gateway/config.yaml`

Runtime state directory priority:

1. `CHORD_GATEWAY_STATE_DIR`
2. `$XDG_STATE_HOME/chord-gateway`
3. `~/.local/state/chord-gateway`

The state directory stores logs, WeChat token files, Feishu dedupe data, and session pins. The default log file is `<state_dir>/gateway.log`.

## 5. Common IM commands

- `/status` – show current Chord state
- `/cancel` – cancel the current turn
- `/allow`, `/deny` – approve or reject a pending confirmation
- `/answer <text>` – answer a pending question
- `/todos` – show the current todo list
- `/new` – clear the current binding's session pin and start fresh
- `/resume <id>` – resume and pin a specific session
- `/sessions` – list recent sessions from the workspace
- `/current` – show the current binding status and pinned session
- `/login [platform]` – start a login flow, for example `/login weixin`

## 6. Next docs

- [README](./README.md) – release overview
- [Usage](./docs/usage.md) – session behavior and IM commands
- [Configuration reference](./docs/configuration.md) – all config fields
- [Operations](./docs/operations.md) – logs, state files, cleanup, routing
- [Permissions & Safety](./docs/permissions-and-safety.md)
- [Troubleshooting](./docs/troubleshooting.md)
