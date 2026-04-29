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

- WeChat always routes to one workspace through `ims[].wechat.workspace_id`.
- Feishu uses `ims[].feishu.chat_bindings` to map chat IDs to workspace IDs.
- If there is exactly one workspace, both WeChat `workspace_id` and Feishu `chat_bindings` may be omitted.

### 3.1 WeChat

```yaml
ims:
  - wechat:
      base_url: https://ilinkai.weixin.qq.com
workspaces:
  - id: default
    path: /path/to/project
chord_path: chord
idle_timeout: 30m
```

First run behavior:

- The gateway prints a QR-code URL.
- Scan it with WeChat to log in.
- The token is stored at `<state_dir>/wechat/token.json` for later runs. You can override it with `ims[].wechat.token_path` if you want to manage that secret separately.

### 3.2 Feishu

```yaml
ims:
  - feishu:
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
  - wechat:
      base_url: https://ilinkai.weixin.qq.com
      workspace_id: project-a
  - feishu:
      app_id: cli_xxx
      app_secret: your-app-secret
      verification_token: your-token
      listen: ":8080"
      webhook_path: /feishu/callback
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

## 5. Where state is stored

By default the gateway stores runtime state under:

- macOS: `~/Library/Application Support/chord-gateway`
- Linux: `${XDG_STATE_HOME:-~/.local/state}/chord-gateway`
- Config file: `${XDG_CONFIG_HOME:-~/.config}/chord-gateway/config.yaml`

State includes logs, WeChat token files (`<state_dir>/wechat/token.json` by default), Feishu dedupe data, and session pins. Feishu `app_id`/`app_secret` remain configuration credentials; its short-lived access token is kept in memory and refreshed as needed.

## 6. Next docs

- [Configuration reference](./docs/configuration.md)
- [Usage guide](./docs/usage.md)
- [Operations](./docs/operations.md)
- [Troubleshooting](./docs/troubleshooting.md)
