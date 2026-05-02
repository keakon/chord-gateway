# Changelog

All notable changes to `chord-gateway` are documented here.

This project follows a simple human-readable changelog format. Dates use `YYYY-MM-DD`.

- Chinese version: [CHANGELOG_CN.md](./CHANGELOG_CN.md)

## Unreleased

### Breaking changes

- Removed the `HandleMessage(imType, chatID, text)` router entrypoint. Use `HandleIncomingMessage` with a structured `IncomingMessage`.
- Removed YAML `ims` and `workspaces` sequence (list) forms in `config.yaml`. Both must now be mappings keyed by adapter type / workspace id.

### Added

- Added a compatibility policy document that records the remaining compatibility surface (the Chord headless `todos` event name) and the cleanup rule.
- Added regression tests covering session-pin write failures and concurrent session-pin updates.
- Added regression tests for idle event rendering and normal-idle pending-confirm cleanup.
- Restored WeChat regression coverage for persisted token / sync state loading, expired-token re-login, custom token paths, `splitText`, and context-aware sleep cancellation.

### Changed

- Refactored the router and process implementation into topic-specific files (`router_commands`, `router_format`, `router_feishu_cards`, `router_reminders`, `router_parse`, `process_protocol`, `process_lifecycle`, `process_env`)
- `todos` events now forward the full current todo list on every event when enabled, instead of only surfacing the current in-progress item.
- `/deny` now takes an optional human-readable reason text instead of a platform-internal request ID; the gateway resolves the pending confirmation automatically.
- `/new` now sends the command to chord via stdin rather than killing the process, letting chord manage session lifecycle gracefully.
- `/bind` now rejects rebinds while a session is actively running; cancel the current turn first.
- Feishu cards no longer embed user-facing commands for confirm/question buttons; they dispatch structured internal actions so the IM protocol stays clean.
- Feishu post-format messages are now accepted alongside plain text, enabling rich-text input for all commands. without changing documented behavior.
- Made `ChordManager` the router's single source of configuration truth, so `/bind` updates no longer need to keep separate router and manager config copies in sync.
- Made session-pin and dedupe persistence use atomic file replacement, and fixed session-pin updates so failed writes do not mutate in-memory state while concurrent updates do not lose pins.
- Clarified Feishu renewal behavior in user docs and cross-IM notifications: Feishu access tokens are refreshed automatically from configured app credentials, `/login feishu` is unsupported, and app credentials must not be sent or changed in IM chats.
- `/status` now waits for the next `status_response` envelope via a buffered channel instead of polling `LastStatusResponseAt`, so the IM message goroutine is no longer blocked for up to 10 seconds.
- `truncate` and `splitText` now operate on runes, so notifications containing multi-byte characters (Chinese, emoji) are no longer split mid-character.
- Hardened concurrency around `ChordManager.cfg` and `WechatAdapter.token` using `atomic.Pointer` so `/bind`-driven config updates and WeChat token refreshes no longer race with reads.
- Refactored Feishu HTTP send / update helpers to share a single `doFeishuJSONRequest` path that handles access-token retry uniformly.
- Feishu interactive confirm/question cards now carry richer context, try to update the original card to a resolved state after approval/answer, and fall back to the existing text notifications if card delivery or update fails.
- Plain-text replies to pending Feishu questions now update the original question card when possible, and card updates prefer the stored sent-message ID over callback metadata to avoid patching the wrong message.
- Gateway logging now uses `github.com/keakon/golog/log` directly for log records and `github.com/keakon/golog` for file rotation. Rotated logs are no longer gzip-compressed.
- Chord `idle` envelopes are now rendered by the gateway as the user-visible ready notification instead of relying on a separate headless `notification` envelope.
- Updated `github.com/keakon/golog` to v0.2.0.
- Standardized remaining runtime/user-facing messages in non-Chinese docs and IM responses to English.

### Removed

- Removed unused helpers and dead state: `MultiAdapter.BroadcastText`, `MultiAdapter.Adapters`, `FeishuAdapter.SendInteractive`, the `WechatAdapter.sessionExpired` flag, and the `ControlState.StreamText` / `LastThinkingText` fields.
- Removed raw-array Chord headless `todos` payload support; gateway now only accepts the current wrapper payload shape (`{"todos":[...]}`).

### Fixed

- `pins.Set` failures (e.g. `/new`, `/resume`, `/bind`) are now logged at warn level instead of being silently dropped.
- Restored nil guards around router config / process-manager access so `HandleIncomingMessage` and session respawn paths now fail with user-visible errors instead of panicking when the router is constructed without a manager or active config.
- Removed a debug-time auto `status` command that was being piggy-backed onto every plain `send`, reducing duplicate stdin commands.
- `truncateLine` now also truncates by rune, so tool-argument summaries no longer split Chinese or emoji mid-character; added regression tests to keep UTF-8 output valid.
- Corrected the `Config.UnmarshalYAML` comment to match current behavior: only map-based `ims` / `workspaces` forms are supported.
- Fixed dedupe persistence so expired entries removed during lookups are marked dirty and successful commits do not trigger redundant cleanup rewrites.
- Suppressed long-running `⏳ Still working` reminders while Chord is waiting for a pending confirmation or question.
- Fixed normal Chord `idle` handling so stale pending confirmations are cleared without being reported as expired; expiry notifications are now reserved for gateway idle-timeout shutdowns.
- Fixed `/bind` and `/resume` busy checks so they inspect existing processes without accidentally spawning new Chord processes.
- Fixed dedupe cleanup persistence so failed writes keep the store dirty and can be retried by the next cleanup tick.

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
