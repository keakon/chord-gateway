# chord-gateway

All IM platforms use one paired owner and explicit chat bindings, including single-workspace deployments. Run `chord-gateway --pair`, then send `/bind "<directory>" <pairing_code>` in a WeChat/Feishu/WeCom/DingTalk private bot chat. See [pairing and local reset](./docs/pairing.md) and the [upgrade notes](./CHANGELOG.md). Slack uses `/chord bind "<directory>" <pairing_code>` in the App DM; see the [Slack guide](./docs/slack.md).

[![CI](https://github.com/keakon/chord-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/keakon/chord-gateway/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/keakon/chord-gateway?display_name=release)](https://github.com/keakon/chord-gateway/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/keakon/chord-gateway)](https://go.dev/)
[![License](https://img.shields.io/github/license/keakon/chord-gateway)](./LICENSE)

`chord-gateway` connects IM platforms such as WeChat, Feishu, Telegram, WeCom, DingTalk, Discord, and Slack to local `chord headless` processes. It lets you control Chord from chat while keeping the agent process, workspace access, credentials, and state on your own machine or server.

- Chinese version: [README_CN.md](./README_CN.md)
- Documentation site: <https://keakon.github.io/chord-gateway/>
- Full documentation: [docs/index.md](./docs/index.md)
- Requires: Go 1.27.0+ and a `chord` build supporting the [current headless contract](./docs/compatibility.md)

## Features

- WeChat iLink support with QR-code login and token persistence (`<state_dir>/wechat/token.json` by default)
- Feishu bot support with long-lived connections, owner pairing, shared message/card intake, and execution-receipt checks
- Telegram bots: Bot API polling, private/group plain text, and bounded in-memory intake
- WeCom intelligent bot API long connection for private/group text and proactive background replies ([setup guide](./docs/wecom.md))
- DingTalk enterprise robot support through Stream, proactive text replies, private identity verification, and binding probes; [deployment acceptance remains pending](./docs/dingtalk.md)
- Discord bot DMs and explicit @Bot messages in server text channels and existing public/private threads; no public endpoint or Message Content privileged intent
- Slack Socket Mode with separate DM/channel/thread bindings and a unified `/chord` command
- Multiple IM adapters can run together
- Per-chat and per-workspace session isolation
- Session pinning and resume commands (`/new`, `/resume`, `/sessions`)
- Move a running session between TUI and IM with `/takeover`, preserving queued input and leaving the target paused (macOS/Linux; requires Chord takeover support).
- Cross-IM login notification (for example, when one channel expires, use another channel to prompt re-login)
- Local `chord headless` subprocess lifecycle management and cleanup
- Configurable visibility for optional control-plane events
- WeChat and Feishu forward generated images to the originating chat; Telegram image output is deferred. Images at or above Feishu's upload limit are sent as a reduced preview while Chord keeps the original

## Install

Install from source with Go:

```bash
go install github.com/keakon/chord-gateway@latest
```

Or build from a local checkout:

```bash
go build -o chord-gateway .
```

Install [Chord](https://github.com/keakon/chord#three-step-setup) separately and make sure `chord` is available in `PATH`, or set `chord_path` in the gateway config. Shell aliases are not supported for `chord_path` because the gateway executes the binary directly.

Verify the installation:

```bash
chord-gateway --version
chord --version
```

## Quickstart

Create a minimal config and point `workspaces.default.path` at the project you want Chord to operate on. Workspace paths must start with `/`, `~`, a Windows drive prefix, or a UNC prefix. Here is a WeChat example:

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

Run the gateway:

```bash
chord-gateway -f config.yaml --pair
```

Only YAML config files are supported (`.yaml` or `.yml`).

After QR login, send `/bind "/path/to/project" <pairing_code>` in a private bot chat using the code shown in the terminal. Send `/status` to verify, then a normal text task. Later starts need no `--pair`.

Notes: Each chat requires explicit binding; Feishu inbound handling currently accepts text messages only. See “Support scope / known limitations” for details.

For platform setup and multi-workspace routing, see [QUICKSTART.md](./QUICKSTART.md), the [WeCom guide](./docs/wecom.md), and the [Discord guide](./docs/discord.md).

See the [Telegram guide](./docs/telegram.md) for token setup, group privacy, and deployment.

## Support scope / known limitations

- Source builds require Go 1.27.0+ and a separate `chord` binary. `chord-gateway` does not bundle Chord itself.
- CI currently builds these targets: `darwin/amd64`, `darwin/arm64`, `linux/amd64`, `linux/arm64`, and `windows/amd64`.
- All platforms select workspaces through explicit `chat_bindings`; the owner binds WeChat/Feishu/WeCom/DingTalk chats with `/bind "<directory>"`, Slack roots with `/chord bind "<directory>"`, and Slack threads with `@Bot /bind "<directory>"`.
- Feishu multi-workspace routing uses `ims.feishu.chat_bindings` to map chat IDs to workspace IDs. Inbound Feishu handling currently accepts text messages only; non-text messages are ignored.
- WeCom supports text only in this release; media, cards, and streaming replies are unsupported. Real enterprise-account acceptance  remain pending.
- Slack currently supports text only. Channel/thread tasks require an explicit owner mention; slash commands address the chat root, while thread controls use `@Bot /command`. See the [Slack guide](./docs/slack.md).
- DingTalk supports same-enterprise text only; group commands require @robot. Its credentials and proactive API permissions must be configured separately.
- Discord requires explicit @Bot in server channels and threads; each thread has its own binding. Native slash commands, buttons, attachments, and rich-text previews are deferred. Use [text commands](./docs/discord.md#pair-and-bind-chats) for approvals and answers.
- `/login [platform]` is only useful for adapters that support interactive login renewal. Today that flow is available for WeChat; Feishu, Telegram, WeCom, DingTalk, Discord, and Slack use configured credentials.

## Common IM commands

| Command | Description |
|---|---|
| `/bind "<directory>" [pairing_code]` | Bind an existing directory; first private pairing requires the terminal code |
| `/status` | Show current Chord state |
| `/cancel` | Cancel the current turn |
| `/allow` / `/deny [reason]` | Approve or deny a pending confirmation |
| `/answer @<id> <text>` | Answer or revise a question by ID; numeric shortcuts are supported |
| `/role [number\|name]` | Show the current main-agent role and the switchable roles, or switch to a target role by list number or name |
| `/todos` | Show the current todo list |
| `/new` | Start a fresh session for the current binding |
| `/resume <id>` | Resume and pin a session |
| `/takeover <id> [--timeout <duration>]` | Take over a TUI session into this chat, paused |
| `/takeover status` | Query the current session transfer |
| `/sessions` | List recent sessions |
| `/login [platform]` | Show supported login-renewal platforms, or start one when a platform is provided (for example `/login wechat`) |

See [docs/usage.md](./docs/usage.md) for full command behavior and session semantics.

## Documentation

- [Quickstart](./QUICKSTART.md)
- [Cookbook](./docs/cookbook.md)
- [IM integration overview](./docs/im.md)
- [DingTalk enterprise robot](./docs/dingtalk.md)
- [Usage](./docs/usage.md)
- [Configuration reference](./docs/configuration.md)
- [Operations](./docs/operations.md)
- [Event visibility](./docs/event-visibility.md)
- [Permissions & safety](./docs/permissions-and-safety.md)
- [Troubleshooting](./docs/troubleshooting.md)
- [Changelog](./CHANGELOG.md)
- Chinese docs: [docs/index_CN.md](./docs/index_CN.md)

## Security notes

`chord-gateway` routes IM messages to a local `chord headless` process that can interact with the configured workspace. Treat IM access as control-plane access to that workspace.

The paired owner is a trusted local control-plane user. Keep Slack, Discord, and Telegram bot tokens, Feishu, WeCom, and DingTalk secrets, WeChat tokens, and gateway state out of version control. See [permissions and safety](./docs/permissions-and-safety.md).

## License

MIT License. See [LICENSE](./LICENSE). The bundled Discord Gateway code retains its upstream Apache-2.0 license under `internal/discordgateway/`.

All platforms share bounded in-memory intake, authorization and delivery receipts. Gateway does not restore its inbox, deduplication or polling cursors after restart. Check the original Chord session before resending an uncertain task. See [delivery semantics](./docs/operations.md#message-delivery-and-restarts).
