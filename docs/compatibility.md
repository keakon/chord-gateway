# Compatibility Policy

Chord Gateway is pre-1.0. This page records the small number of compatibility paths
that are intentionally retained, plus the rule for cleaning them up.

## Required `chord headless` contract

Use a Chord build that supports all of the following:

- Versioned envelopes with monotonic `seq` and `input_result` consumption receipts.
- Durable question collections in `status_response.questions`, stable `option_ids`, and `question_request`, `question_updated`, and `question_resolved` events.
- Question commands with `action` / `operation_id`, correlated `question_receipt`, and presentation commands with `version`, `binding_id`, and `supports_interaction`.
- Background-job counts in `idle` and `status_response`.

Older single-question protocols and builds without input receipts are unsupported; the gateway does not translate them. Pin the Chord build tested with your deployment and follow the [Chord headless documentation](https://github.com/keakon/chord/blob/main/docs/headless.md) when upgrading. A build accepting unknown subscription names does not establish protocol compatibility.

After upgrading the gateway, send `/status` to refresh the question collection and answer entries. Use those entries rather than question buttons issued by an older gateway.

## Kept compatibility paths

- Session pins still accept the earlier `workspaceID|imType|chatID` storage keys when loading an existing state directory. New keys use the encoded form.
- Unknown YAML fields are ignored. The removed `event_visibility.activity` key therefore has no effect; activity tracking is always enabled.
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
