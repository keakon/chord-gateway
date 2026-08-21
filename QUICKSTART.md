# Quick Start

Use this guide to connect one project to either WeChat or Feishu. Start with one workspace; add more workspaces only after the first chat works.

## 1. Install Chord and the gateway

Install [Chord](https://github.com/keakon/chord#three-step-setup), complete its setup wizard, and verify it first:

```bash
chord --version
```

Then install the gateway with Go 1.27.0 or later:

```bash
go install github.com/keakon/chord-gateway@latest
chord-gateway --version
```

For prebuilt binaries, source builds, and service setup, see the [Cookbook](./docs/cookbook.md).

## 2. Create one minimal config

Choose one platform below and save its example as `config.yaml`. Replace `/path/to/project` with the project Chord may operate on. Paths may be absolute, start with `~`, use a Windows drive prefix, or use a UNC prefix.

### Option A: WeChat

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

On first run, scan the printed QR-code URL with WeChat. The login token is then stored at `<state_dir>/wechat/token.json` by default.

### Option B: Feishu

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner_open_id: ou_xxx
workspaces:
  default:
    path: /path/to/project
chord_path: chord
idle_timeout: 30m
```

Before starting the gateway, configure the Feishu app to use a long connection, subscribe to `im.message.receive_v1`, add the bot to a private chat or controlled test group, and publish the app version. Add `card.action.trigger` if you want interactive confirmation and question cards. See the [Feishu guide](./docs/feishu.md) for exact permissions and console steps.

Before creating the final config, you can discover your ID safely: start once without `owner_open_id` or `allowed_open_ids`, send a text message, and copy `open_id=ou_xxx` from the `message from non-allowed open_id` log entry. The message is logged without its content and is not processed. Stop the gateway, set that value as `owner_open_id`, then restart. Running the gateway locally does not limit who can message the bot through Feishu.

## 3. Run and verify

```bash
chord-gateway -f config.yaml
```

After startup:

1. Complete the WeChat QR login, or send a text message from the owner Feishu account.
2. Send `/status` and confirm the expected workspace is shown.
3. Send a normal text request to Chord.

For Feishu, verify that messages from accounts outside the allowlist are ignored.

## 4. Add advanced routing later

WeChat always routes to one workspace. Feishu can map different chats to different workspaces with `/bind` and `chat_bindings`. Follow the [Feishu multi-workspace guide](./docs/feishu.md#multi-workspace-routing-with-bind) after the single-workspace setup works.

## 5. State and next steps

By default the gateway stores runtime state under:

- macOS / Linux: `${XDG_STATE_HOME:-~/.local/state}/chord-gateway`
- Config file: `${XDG_CONFIG_HOME:-~/.config}/chord-gateway/config.yaml`

State includes logs, WeChat token files (`<state_dir>/wechat/token.json` by default), Feishu dedupe data, and session pins. Feishu `app_id`/`app_secret` remain configuration credentials; its short-lived access token is kept in memory and refreshed as needed.

- [Configuration reference](./docs/configuration.md)
- [Cookbook](./docs/cookbook.md)
- [IM integration overview](./docs/im.md)
- [Usage guide](./docs/usage.md)
- [Operations](./docs/operations.md)
- [Troubleshooting](./docs/troubleshooting.md)
