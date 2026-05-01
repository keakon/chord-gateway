# Compatibility Policy

Chord Gateway is pre-1.0. This page records the small number of compatibility paths
that are intentionally retained, plus the rule for cleaning them up.

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
