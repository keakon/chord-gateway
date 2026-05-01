# Changelog

All notable changes to `chord-gateway` are documented here.

This project follows a simple human-readable changelog format. Dates use `YYYY-MM-DD`.

- 中文版: [CHANGELOG_CN.md](./CHANGELOG_CN.md)

## Unreleased

### Added

- Added a compatibility policy document that records the currently supported legacy config forms, backward-compatible router entrypoints, and the active headless `todos` event contract.
- Added regression tests covering session-pin write failures and concurrent session-pin updates.

### Changed

- Refactored the router and process implementation into topic-specific files (`router_commands`, `router_format`, `router_feishu_cards`, `router_reminders`, `router_parse`, `process_protocol`, `process_lifecycle`, `process_env`) without changing documented behavior.
- Made session-pin and dedupe persistence use atomic file replacement, and fixed session-pin updates so failed writes do not mutate in-memory state while concurrent updates do not lose pins.
- Clarified Feishu renewal behavior in user docs and cross-IM notifications: Feishu access tokens are refreshed automatically from configured app credentials, `/login feishu` is unsupported, and app credentials must not be sent or changed in IM chats.
- Feishu interactive confirm/question cards now carry richer context, try to update the original card to a resolved state after approval/answer, and fall back to the existing text notifications if card delivery or update fails.
- Plain-text replies to pending Feishu questions now update the original question card when possible, and card updates prefer the stored sent-message ID over callback metadata to avoid patching the wrong message.

## 0.2.0 – 2026-04-30

### Added

- Feishu chat binding updates via `/bind`, limited to in-memory/YAML updates for Feishu `chat_bindings` and `workspaces`.

### Changed

- Feishu now receives events only through long connection mode.
- Long-running busy turns now continue to emit compact reminders every 5 minutes while work is still in progress.
- `todos` events now forward the full current todo list on every event when enabled, instead of only surfacing the current in-progress item.
- `/bind` now rejects malformed input more strictly: unterminated quotes, extra arguments, unsupported path prefixes, inaccessible paths, and non-directory paths all fail without changing the config file.
- Feishu setup, operations, safety, troubleshooting, and usage docs now describe long connection mode and `/bind`-based routing updates.
- Pending question/confirmation expiry is now surfaced to users; late `/answer`, `/allow`, and `/deny` responses are forwarded as follow-up context instead of being treated as structured responses.

### Removed

- Feishu webhook mode and its related config fields (`verification_token`, `encrypt_key`, `listen`, `webhook_path`).

## 0.1.0 – 2026-04-29

### Added

- WeChat iLink mode with QR-code login and token persistence.
- Feishu bot mode.
- Multi-IM mode for running WeChat and Feishu together.
- Cross-IM login notification for expired IM sessions.
- Per-chat and per-workspace session pinning.
- IM session commands: `/new`, `/resume`, `/sessions`, `/current`.
- Feishu owner allowlist with `owner_open_id` and `allowed_open_ids`.
- Feishu event dedupe persisted under the gateway state directory.
- Configurable optional control-plane event visibility.
- Process group cleanup for spawned `chord headless` processes.
- User documentation for usage, operations, safety, troubleshooting, and configuration.

### Changed

- README is now a concise release entry page; detailed behavior moved to `docs/`.
- Final assistant messages are pushed in real time; `/summary` is no longer part of the documented command set.
- Expanded unit-test coverage across router, multi-adapter, WeChat helper, adapter-factory, and config paths; shared Go quality checks now enforce coverage >= 60.0%, `go vet`, and `staticcheck` in pre-commit and CI.
- Added GitHub Actions release workflow for tagged multi-platform binary archives with checksums.
- Fixed Windows build compatibility by isolating Unix process-group syscalls behind platform-specific implementations.
- Made contract blackbox tests build their temporary `chord` binary without VCS stamping for reliable hook and CI execution.

### Security

- Added explicit safety guidance for IM access control, Feishu credential handling, and workspace scoping.
