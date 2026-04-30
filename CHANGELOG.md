# Changelog

All notable changes to `chord-gateway` are documented here.

This project follows a simple human-readable changelog format. Dates use `YYYY-MM-DD`.

- 中文版: [CHANGELOG_CN.md](./CHANGELOG_CN.md)

## Unreleased

### Added

- Improved Feishu interactive cards for confirmations and questions, including risk/context display, safer callback validation, message-card update handles, resolved-card status updates, and broader unit-test coverage.

### Changed

- Clarified Feishu renewal behavior in user docs and cross-IM notifications: Feishu access tokens are refreshed automatically from configured app credentials, `/login feishu` is unsupported, and app credentials must not be sent or changed in IM chats.

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
