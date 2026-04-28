# Changelog

All notable changes to `chord-gateway` are documented here.

This project follows a simple human-readable changelog format. Dates use `YYYY-MM-DD`.

- 中文版: [CHANGELOG_CN.md](./CHANGELOG_CN.md)

## 0.1.0 – 2026-04-29

### Added

- WeChat iLink mode with QR-code login and token persistence.
- Feishu bot mode with webhook handling.
- Multi-IM mode for running WeChat and Feishu together.
- Cross-IM login notification for expired IM sessions.
- Per-chat and per-workspace session pinning.
- IM session commands: `/new`, `/resume`, `/sessions`, `/current`.
- Feishu owner allowlist with `owner_open_id` and `allowed_open_ids`.
- Feishu callback dedupe persisted under the gateway state directory.
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

- Added explicit safety guidance for IM access control, Feishu webhook verification, credential handling, and workspace scoping.
