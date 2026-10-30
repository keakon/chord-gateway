# Compatibility Policy

Chord Gateway is pre-1.0. This page records the small number of compatibility paths
that are intentionally retained, plus the rule for cleaning them up.

## Required `chord headless` contract

Use a Chord build that supports all of the following:

- Versioned envelopes with monotonic `seq` and `input_result` consumption receipts.
- The `confirm_resolved` event with `request_id`, `tool_name`, `agent_id`, and an `outcome` of `approved`, `denied`, `timeout`, or `cancelled`. Without this event, expired confirmation controls may remain actionable in the gateway.
- `confirm_rule_intent_result` receipts with `request_id` and per-rule `tool`, `pattern`, and optional `error`. Scoped `/allow` waits for this receipt before reporting whether Chord remembered the offered rules.
- Durable question collections in `status_response.questions`, stable `option_ids`, and `question_request`, `question_updated`, and `question_resolved` events.
- Question commands with `action` / `operation_id`, correlated `question_receipt`, and presentation commands with `version`, `binding_id`, and `supports_interaction`.
- `models` commands with a unique `request_id` echoed by each `models_response`. The gateway ignores uncorrelated or late model responses.
- Background-job counts in `idle` and `status_response`.
- Input-queue projections with complete target identity, `view_version`, `view_version_cas: true`, and `queue_revision`. The gateway includes `expected_view_version` when applying queue changes; stale views return `input_queue_result: conflict`. It can still list a queue when this compare is unavailable, but it will not mutate it. `works` with a per-work `outcome`, a per-input `wait_work_id`, and `restored_held` are what let `/queue` explain why an entry has not started; a build omitting them still lists and edits the queue, but a held entry looks like ordinary pending input.
- `image_result` saved-image summaries with SHA-256 integrity metadata, ordered `image_artifact` chunks with byte offsets and final markers, and `request_id` echoed in errors for failed image-artifact reads. The aggregate `idle` event follows all generated-image chunks.

Older single-question protocols and builds without input receipts are unsupported; the gateway does not translate them. Pin the Chord build tested with your deployment and follow the [Chord headless documentation](https://github.com/keakon/chord/blob/main/docs/headless.md) when upgrading. A build accepting unknown subscription names does not establish protocol compatibility.

After upgrading the gateway, send `/status` to refresh the question collection and answer entries. Use those entries rather than question buttons issued by an older gateway.

## Session takeover capability

Session takeover additionally requires a Chord build with `chord takeover preflight/start/status/retry-save --json`, `--gateway-managed`, `--gateway-instance`, `--gateway-transfer-id`, transfer preparation/save/release events, and a correlated `ready_paused` response. A successful managed transfer uses exit code `20`. The source and target must run on macOS or Linux under the same OS user and canonical workspace; use the same Chord state directory on both sides.

`ready_paused` (or `ready` with `state: ready_paused`) and `status_response` must include a `paused_inputs` summary:

```json
{
  "paused": true,
  "pending_input_count": 1,
  "inputs": [
    {
      "input_id": "input-1",
      "owner_task_id": "task-1",
      "preview": "Review the draft",
      "attachment_count": 1
    }
  ]
}
```

The summary reports the total pending count separately from its bounded entries. The entry count must equal `min(pending_input_count, 8)`; counts cannot be negative. Input and owner IDs must be nonempty, at most 128 bytes, and contain no whitespace or control characters; input IDs must be unique. The gateway displays up to eight entries with owner task, a single-line preview of up to 160 Unicode characters, and attachment count; IDs are also bounded for display. Full input content and attachment data or paths stay in the Chord session, outside takeover CLI status and the gateway's transfer store. A takeover target must return a valid summary with `paused: true` before the binding opens. If an ordinary status response omits the summary, the gateway retains the last confirmed summary for that session, including its directory-switch guard. Only an explicit empty summary or a session change can clear previously known pending inputs.

The gateway checks the binary's headless flags before requesting takeover. A build without managed-headless support can still run ordinary chat bindings if it meets the contract above, but `/takeover` reports the missing capability. There is no alternate takeover protocol or unsafe fallback. See [usage.md](./usage.md#session-takeover).

Takeover workspace identity comes from the Chord `takeover preflight` response; `-d` separately carries the execution directory. Gateway does not infer Git roots. The `ready`, `ready_paused`, `status_response`, and `session_switched` payloads carry `workspace` to validate source and restored target identity. CLI state `preflight` proves that the start request was not admitted. State `prepare_failed` proves that this transfer did not freeze source execution; the gateway reopens preparation routing only while the same source instance is still alive.

## Kept compatibility paths

- Automatic session pins are keyed by workspace and route, so changing the platform account on an existing route keeps its session. Local `pairing reset` clears that platform's automatic pins together with the owner and bindings, so a reset route starts without the old session. Pins written by a release that scoped keys to the paired identity, and pins in the pre-`v2:` pipe-joined layout the current parser no longer addresses, are discarded on load rather than rewritten; see the [upgrade notes](../CHANGELOG.md). Re-pairing to a different owner inherits the previous session by design for single-user deployments; use `/new` or delete the pin for isolation.
- Unknown YAML fields are ignored, but each one is reported in the gateway log with its line, so a typo or a removed option is noticed. The removed `event_visibility.activity` key therefore has no effect; activity tracking is always enabled.
- Duplicate YAML keys keep the first occurrence: each later duplicate is reported as a `duplicate key ignored` warning with its line instead of failing startup, and the next configuration change rewrites the file without the duplicates. That rewrite also normalizes YAML formatting to two-space indentation, so the first change may reflow the file even when nothing is duplicated.
- The Chord headless `todos` event is supported. Although Chord also emits an internal
  protocol event named `todos_updated`, the current headless command still surfaces
  `todos` externally — gateway accepts it as-is.

## Removed paths

The following compatibility paths existed in earlier releases and have been removed:

- `HandleMessage(imType, chatID, text)` — removed. Use `HandleIncomingMessage` with a
  structured `IncomingMessage`.
- YAML `ims` and `workspaces` sequence forms — removed. Use mapping form keyed by
  adapter type / workspace id. `/bind` only operates on mapping form.
- Raw-array Chord headless `todos` payloads (`[...]`) — removed. Current Chord emits
  wrapper payloads (`{"todos":[...]}`), and gateway only accepts that form.
- Question `decline` action and `declined` outcome — removed to match Chord, where a
  question can only be answered or cancelled. Cancel with `/question-withdraw <id>`.
- File-based intake deduplication and polling cursors — removed without migration.
  Intake, deduplication and cursors are memory-only; old files are no longer imported.
- `https` proxy for WebSocket long-connections (Discord, Slack, Feishu, WeCom,
  DingTalk sockets) — rejected at startup. Use `http`, `socks5` or `socks5h`.
- `input_queue` `gate` mode and `restored_next_inputs_blocked` — removed. Current
  Chord exposes no pausable queue gate and reports `restored_held` plus a per-input
  `wait_work_id` instead. `/queue pause` and `/queue resume` are gone with the gate;
  `/queue` explains each held entry's reason.

## Cleanup rule

Remove a kept compatibility path only after the supported external configuration /
protocol version is changed and tests/docs are updated in the same change.

## Session listing

Session listing requires `chord sessions list --json --limit 10`, returning schema version 1, a canonical workspace and a sessions array of IDs, RFC 3339 last-activity timestamps and lock flags. An unsupported command produces an explicit query error, with no legacy directory fallback.

## Chat bindings

All supported platforms require owner pairing and explicit chat bindings, including single-workspace deployments. Old authorization fields (`owner_open_id`, `allowed_open_ids`, `workspace_id`, `access_mode`) are ignored with a startup warning instead of failing startup; see the [upgrade notes](../CHANGELOG.md). With `chat_bindings` but no owner, `chat_bindings` stay dormant until pairing completes (`--pair`), or startup exits with pairing guidance.

## Cross-repository contract tests

The full test suite exercises a real Chord CLI and, for TUI takeover, a Chord test binary containing `TestTakeoverProcessRoundTrip`. Both must come from a compatible Chord revision. CI clones Chord's remote `main`; push the required Chord protocol and fixture changes there before running Gateway CI. A local Chord commit does not update that checkout. Missing capabilities must fail the contract tests, not be skipped or emulated.

To test a specific source checkout locally:

```bash
CHORD_GATEWAY_CONTRACT_CHORD_REPO=../chord go test ./...
```

An explicit `CHORD_GATEWAY_CONTRACT_CHORD_BIN` takes precedence, followed by an explicit `CHORD_GATEWAY_CONTRACT_CHORD_REPO`. With neither set, tests use `chord` from PATH, or build the adjacent checkout when no binary is installed. `CHORD_GATEWAY_CONTRACT_CHORD_HELPER_BIN` can select a prebuilt TUI fixture; otherwise it is built from the selected source checkout. Keep the CLI and fixture revisions aligned. `go test -short ./...` omits these cross-repository tests and does not establish protocol compatibility.
