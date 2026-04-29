# chord-gateway

[![CI](https://github.com/keakon/chord-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/keakon/chord-gateway/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/keakon/chord-gateway?display_name=release)](https://github.com/keakon/chord-gateway/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/keakon/chord-gateway)](https://go.dev/)
[![License](https://img.shields.io/github/license/keakon/chord-gateway)](./LICENSE)

`chord-gateway` connects IM platforms such as WeChat and Feishu to local `chord headless` processes. It lets you control Chord from chat while keeping the agent process, workspace access, credentials, and state on your own machine or server.

- 中文版: [README_CN.md](./README_CN.md)
- Full documentation: [docs/index.md](./docs/index.md)
- Requires: Go 1.26+ and a working `chord` binary

## Features

- WeChat iLink support with QR-code login and token persistence (`<state_dir>/wechat/token.json` by default)
- Feishu bot support with webhook verification, optional encryption, owner allowlist, and callback dedupe
- Multiple IM adapters can run together
- Per-chat and per-workspace session isolation
- Session pinning and resume commands (`/new`, `/resume`, `/sessions`, `/current`)
- Cross-IM login notification when one channel expires
- Local `chord headless` subprocess lifecycle management and cleanup
- Configurable visibility for optional control-plane events

## Install

Install from source with Go:

```bash
go install github.com/keakon/chord-gateway@latest
```

Or build from a local checkout:

```bash
go build -o chord-gateway .
```

Install Chord separately and make sure `chord` is available in `PATH`, or set `chord_path` in the gateway config. Shell aliases are not supported for `chord_path` because the gateway executes the binary directly.

Verify the installation:

```bash
chord-gateway --version
```

## Quickstart

Create a minimal config and point `workspaces[].path` at the project you want Chord to operate on. Here is a WeChat example:

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

Run the gateway:

```bash
chord-gateway -f config.yaml
```

Only YAML config files are supported (`.yaml` or `.yml`).

After the gateway is running, send `/status` from the connected IM chat to verify the route, then send a normal text message to talk to Chord.

For Feishu setup and multi-workspace routing, see [QUICKSTART.md](./QUICKSTART.md).

## Support scope / known limitations

- Source builds require Go 1.26+ and a separate `chord` binary. `chord-gateway` does not bundle Chord itself.
- CI currently builds these targets: `darwin/amd64`, `darwin/arm64`, `linux/amd64`, `linux/arm64`, and `windows/amd64`.
- WeChat always routes to one workspace. When multiple workspaces exist, set `ims[].wechat.workspace_id` to choose the workspace used by WeChat.
- Feishu multi-workspace routing uses `ims[].feishu.chat_bindings` to map chat IDs to workspace IDs. Inbound Feishu handling currently accepts text messages only; non-text messages are ignored.
- `/login [platform]` is only useful for adapters that support interactive login renewal. Today that flow is available for WeChat, not Feishu.

## Common IM commands

| Command | Description |
|---|---|
| `/status` | Show current Chord state |
| `/cancel` | Cancel the current turn |
| `/allow` / `/deny` | Approve or deny a pending confirmation |
| `/answer <text>` | Answer a pending question |
| `/todos` | Show the current todo list |
| `/new` | Start a fresh session for the current binding |
| `/resume <id>` | Resume and pin a session |
| `/sessions` | List recent sessions |
| `/current` | Show the current binding status and pinned session |
| `/login [platform]` | Start an interactive login flow |

See [docs/usage.md](./docs/usage.md) for full command behavior and session semantics.

## Documentation

- [Quickstart](./QUICKSTART.md)
- [Usage](./docs/usage.md)
- [Configuration reference](./docs/configuration.md)
- [Operations](./docs/operations.md)
- [Event visibility](./docs/event-visibility.md)
- [Permissions & safety](./docs/permissions-and-safety.md)
- [Troubleshooting](./docs/troubleshooting.md)
- [Changelog](./CHANGELOG.md)
- 中文文档入口: [docs/index_CN.md](./docs/index_CN.md)

## Security notes

`chord-gateway` routes IM messages to a local `chord headless` process that can interact with the configured workspace. Treat IM access as control-plane access to that workspace.

For public Feishu deployments, configure webhook verification and an allowlist (`owner_open_id` and/or `allowed_open_ids`). Keep Feishu secrets, WeChat token files (`<state_dir>/wechat/token.json` unless `ims[].wechat.token_path` is set), and the gateway state directory out of version control. See [docs/permissions-and-safety.md](./docs/permissions-and-safety.md) and [SECURITY.md](./SECURITY.md).

## License

MIT License. See [LICENSE](./LICENSE).
