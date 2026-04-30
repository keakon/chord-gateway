# Compatibility Policy

Chord Gateway is pre-1.0, but it intentionally keeps a small set of low-cost compatibility paths where removing them could break existing deployments or the current Chord headless contract.

## Kept compatibility paths

- `HandleMessage(imType, chatID, text)` remains as a backward-compatible wrapper around `HandleIncomingMessage`. New code should call `HandleIncomingMessage` with a structured `IncomingMessage`.
- YAML `ims` and `workspaces` sequence forms are still accepted and `/bind` may normalize them to mapping form. New configurations should use mapping form.
- The Chord headless `todos` event is supported. Although Chord also has an internal protocol event named `todos_updated`, the current headless command still exposes `todos` externally.
- `todos` payload parsing accepts both `{"todos":[...]}` and a raw `[...]` array. New producers should send the wrapper form.

## Cleanup rule

Remove one of these paths only after the supported external configuration/protocol version is changed and tests/docs are updated in the same change.
