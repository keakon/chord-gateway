# Compatibility Policy

Chord Gateway is pre-1.0. This page records the small number of compatibility paths
that are intentionally retained, plus the rule for cleaning them up.

## Minimum `chord headless` versions

Gateway runs against whatever `chord headless` build is configured, and newer
gateway features degrade on older builds instead of failing:

- `chord headless` v0.8.0 and later supports the `/role` command and emits the
  `role_change`, `handoff_cancelled`, and `compaction_status` events.
- Builds newer than v0.8.1 additionally emit the `session_switched`,
  `background_result`, and `context_notice` events, and add a monotonic `seq` to
  state-carrying envelopes. Gateway uses `seq` to drop a `status_response`
  snapshot that a newer push has already overtaken; without it, a late snapshot
  can still be applied.

Older builds never send the newer events, and unknown subscription entries are
ignored rather than treated as errors.

## Kept compatibility paths

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

## Cleanup rule

Remove a kept compatibility path only after the supported external configuration /
protocol version is changed and tests/docs are updated in the same change.
