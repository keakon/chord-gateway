# chord-gateway

[![CI](https://github.com/keakon/chord-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/keakon/chord-gateway/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/keakon/chord-gateway?display_name=release)](https://github.com/keakon/chord-gateway/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/keakon/chord-gateway)](https://go.dev/)
[![License](https://img.shields.io/github/license/keakon/chord-gateway)](./LICENSE)

`chord-gateway` connects IM platforms such as WeChat and Feishu to local `chord headless` processes. It lets you control Chord from chat while keeping the agent process, workspace access, credentials, and state on your own machine or server.

- Chinese version: [README_CN.md](./README_CN.md)
- Full documentation: [docs/index.md](./docs/index.md)
- Requires: Go 1.26+ and a working `chord` binary

## Features

- WeChat iLink support with QR-code login and token persistence (`<state_dir>/wechat/token.json` by default)
- Feishu bot support with long-lived connection mode, owner allowlist, and event deduplication
- Multiple IM adapters can run together
- Per-chat and per-workspace session isolation
- Session pinning and resume commands (`/new`, `/resume`, `/sessions`, `/current`)
- Cross-IM login notification (for example, when one channel expires, use another channel to prompt re-login)
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

## Build identity

`chord-gateway --version` prints a compact build identity. A normal local `go build` includes Go VCS fallback data when buildvcs is enabled, for example:

```text
chord-gateway version dev 8da87da152e2 dirty
```

Startup logs include the gateway build fields emitted on every launch (`gateway_version`, `gateway_commit`, `gateway_build_time`, `gateway_vcs_time`, `gateway_dirty`, and `go_version`). When a `chord headless` child process is spawned, the gateway logs the configured `chord_binary` path and its `chord_binary_mtime` to help distinguish gateway-version issues from child Chord binary issues.

For release builds, inject exact build metadata with ldflags:

```bash
go build -o chord-gateway \
  -ldflags "\
    -X github.com/keakon/chord-gateway/internal/buildinfo.Version=v0.1.0 \
    -X github.com/keakon/chord-gateway/internal/buildinfo.Commit=$(git rev-parse HEAD) \
    -X github.com/keakon/chord-gateway/internal/buildinfo.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
    -X github.com/keakon/chord-gateway/internal/buildinfo.Dirty=false" \
  .
```

Plain `go build -o chord-gateway .` remains supported; `gateway_build_time` will be `unknown` unless injected, while `gateway_commit`, `gateway_vcs_time`, and `gateway_dirty` come from Go build info when available.

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
chord-gateway -f config.yaml
```

Only YAML config files are supported (`.yaml` or `.yml`).

After the gateway is running, send `/status` from the connected IM chat to verify the route, then send a normal text message to talk to Chord.

Notes: WeChat routes to a single workspace; Feishu inbound handling currently accepts text messages only. See “Support scope / known limitations” for details.

For Feishu setup and multi-workspace routing, see [QUICKSTART.md](./QUICKSTART.md).

## Support scope / known limitations

- Source builds require Go 1.26+ and a separate `chord` binary. `chord-gateway` does not bundle Chord itself.
- CI currently builds these targets: `darwin/amd64`, `darwin/arm64`, `linux/amd64`, `linux/arm64`, and `windows/amd64`.
- WeChat always routes to one workspace. When multiple workspaces exist, set `ims.wechat.workspace_id` to choose the workspace used by WeChat.
- Feishu multi-workspace routing uses `ims.feishu.chat_bindings` to map chat IDs to workspace IDs. Inbound Feishu handling currently accepts text messages only; non-text messages are ignored.
- `/login [platform]` is only useful for adapters that support interactive login renewal. Today that flow is available for WeChat, not Feishu.

## Common IM commands

| Command | Description |
|---|---|
| `/status` | Show current Chord state |
| `/cancel` | Cancel the current turn |
| `/allow` / `/deny [reason]` | Approve or deny a pending confirmation |
| `/answer <text>` | Answer a pending question; numeric shortcuts are supported |
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
- Chinese docs: [docs/index_CN.md](./docs/index_CN.md)

## Security notes

`chord-gateway` routes IM messages to a local `chord headless` process that can interact with the configured workspace. Treat IM access as control-plane access to that workspace.

For public Feishu deployments, configure an allowlist (`owner_open_id` and/or `allowed_open_ids`). Keep Feishu secrets, WeChat token files (`<state_dir>/wechat/token.json` unless `ims.wechat.token_path` is set), and the gateway state directory out of version control. See [docs/permissions-and-safety.md](./docs/permissions-and-safety.md) and [SECURITY.md](./SECURITY.md).

## License

MIT License. See [LICENSE](./LICENSE).
