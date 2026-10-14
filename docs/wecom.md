# WeCom Integration Guide

The WeCom intelligent bot API connects to Gateway over WebSocket, supporting private and group text, existing control commands, and background text results. No public callback URL is required. Completed tasks use proactive messages without depending on the original callback lifetime. Cards, streaming replies, and media are outside this release.

## Enable the bot

Create an intelligent bot in WeCom, select **API mode → long connection**, obtain its bot ID and secret, and configure its available scope so the owner can message it privately and add it to the intended groups. If this option is unavailable, ask the administrator to enable the capability and classify deployment as `product_mode_unavailable`. [Official WeCom entry](https://open.work.weixin.qq.com/help?doc_id=21657)

This integration requires the intelligent bot API long connection. Enterprise applications, ordinary group webhook bots, and HTTP/XML callbacks use separate product interfaces.

## Configure and pair

```yaml
ims:
  wecom:
    bot_id: your-bot-id
    secret: your-bot-secret
workspaces: {}
```

The endpoint is fixed at `wss://openws.work.weixin.qq.com`. An `endpoint` YAML field is ignored with an unsupported-field warning; it cannot override this address. Protect the secret in YAML and ensure the project directory already exists.

```bash
chord-gateway -f config.yaml --pair
```

After authentication and ingress preparation, the foreground terminal displays a pairing code. Send privately to the bot:

```text
/bind "/path/to/project" <pairing_code>
```

Gateway first proactively sends the fixed text `Gateway binding check.` to that chat. Only a successful platform ACK allows saving the owner and binding. If the probe fails, the binding remains unchanged; check scope, proactive send permission, and quota, then send `/bind` again.

After pairing, send `/status` followed by a task. The owner can send `/bind "/path/to/project"` in a group containing the bot; both the probe and subsequent replies go to that group. Other members cannot execute commands or approve requests, but everyone in the group can see replies. Each chat explicitly binds to one workspace. Gateway writes encoded `chat_bindings`; no manual chat ID construction is needed. See [usage](./usage.md) and [pairing and reset](./pairing.md).

Rotating the secret preserves the owner if the bot ID stays the same. Changing the bot ID blocks the old pairing: stop the service, run `chord-gateway -f config.yaml pairing reset --platform wecom` in a local controlling terminal, type `wecom` to confirm, then pair again. Reset clears the owner, bindings, and automatic session pins while preserving workspaces and session files. A local account lock is acquired before dialing and excludes duplicate instances even across state directories.

## Replies and failures

Replies use proactive `aibot_send_msg` with an explicit private/group type, avoiding the platform’s group-first interpretation of private targets. Gateway renders safe text, escaping Markdown controls and removing notification semantics; it does not generate links, images, or cards. Segments have a conservative 1024 UTF-8 byte budget after rendering, preserve Unicode boundaries, and are limited to 32 per output with a truncation notice. The original Chord session retains the full result.

Sends are spaced at least four seconds per chat, with an account limit of two per second and burst one. Each segment waits for the preceding successful ACK. An output has a five-minute total deadline. These are project budgets and throttles; actual permissions and quotas require testing in the target enterprise.

| Diagnostic | Action |
| --- | --- |
| `auth_failed` | Check bot ID, secret, and product mode; authentication rejection stops retries. |
| Binding probe not accepted | Check target, scope, send permission, and quota, then send `/bind` again. |
| `platform_rejected` | Retain the numeric code for diagnosis; Gateway does not guess rate-limit mappings or blindly retry. |
| `unknown_send` | A network interruption or ACK timeout leaves the outcome unknown. Check the chat and original session; output is not resent automatically. Partial failure records accepted segment count. |
| `identity_mismatch` | The callback bot ID differs from the configured account. The connection stops; check the bot credentials and deployment before restarting. |
| `ingress_prepare_failed` | Check ingress service availability and the paired account. After changing bot ID, perform the local reset above before pairing again. |
| `duplicate_connection` | Another connection replaced this instance. Automatic reconnection stops; close the other deployment and restart. |
| `acceptance_unconfirmed` | This delivery attempt did not confirm acceptance; an earlier delivery of the same business message may already be committed. Inspect the original Chord session before sending anything again. |
| `callback_dropped` | Connection preparation overflow or disconnect left a delivery unsubmitted to shared ingress. This does not prove the business message was never accepted, and redelivery is not guaranteed. Inspect the original session. |
| `control_target_unavailable` | Business state was busy at receipt, preventing capture of the original control target. The message is rejected within this process and cannot execute against a later state. Check `/status`. |

Ordinary network failures reconnect with base delays of 1/2/4/8/16/30 seconds and ±20% jitter. Backoff resets only after 60 seconds continuously ready. JSON heartbeats start after subscription authentication and continue every 30 seconds during ingress preparation; no successful ACK within ten seconds closes the old connection. Preparation buffers at most 64 callbacks and 4 MiB of raw frames. Overflow drops and counts new deliveries while retaining buffered messages and the connection; dropped input is not automatically replayed. Sends fail while disconnected. Old ACKs cannot affect a new connection, and old sends are never replayed.

Only ordinary user text becomes commands or tasks. Messages carrying `compat_info` from assistants, channels, or unknown sources are ignored. Ordinary messages need no `create_time`; Gateway uses reception time when absent and accepts the [official SDK’s optional timestamp](https://github.com/WecomTeam/aibot-node-sdk/blob/main/src/types/message.ts) when present. See the [official long-connection protocol](https://developer.work.weixin.qq.com/document/path/101463).

WeCom deduplicates business msgids within the current process, clearing them on restart. Reception does not prove execution; unknown outcomes are not replayed on reconnect. Check `/status` and the original Chord session. See [delivery semantics](./operations.md#message-delivery-and-restarts).

## Deployment acceptance

Automated tests cover simulated WebSocket authentication, ACK correlation, shutdown, command authorization, binding probes, process-local business deduplication, and text boundaries. These tests do not establish real enterprise acceptance; real-account checks remain pending. In the target enterprise verify authentication; private probe, pairing, and `/status`; owner group binding and rejection of other members; a 1024-byte Chinese/emoji sample and three-segment output for display, order, and no notifications; a task lasting over ten minutes and background text results; and cessation of reconnect contention after another connection replaces this one. Retain error codes for permission or budget failures; do not mark them as passing. A missing long-connection product capability is a deployment `product_mode_unavailable` failure; a rejected proactive binding probe requires checking the target type and platform error code. Classify only explicit permission rejections as `proactive_send_denied`, not addressing errors, rate limits, or network timeouts. These deployment classifications are separate from the transport diagnostics above.
