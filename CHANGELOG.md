# Changelog

All notable changes to `chord-gateway` are documented here.

This project follows a simple human-readable changelog format. Dates use `YYYY-MM-DD`.

- Chinese version: [CHANGELOG_CN.md](./CHANGELOG_CN.md)

## Unreleased

### Breaking changes

- Gateway intake, deduplication and polling cursors are memory-only and are not restored after restart; loss or duplication across restarts is possible. Old deduplication and cursor files are no longer imported. Configuration, login credentials, session pins and transfer guards remain saved; Chord owns its sessions. Reconcile pending input before stopping; see [delivery semantics](./docs/operations.md#message-delivery-and-restarts).

- Feishu and WeChat now use one paired owner and explicit chat bindings, including single-workspace deployments. Removed legacy mode and `access_mode`, `owner_open_id`, `allowed_open_ids`, and WeChat `workspace_id`; configurations containing them ignore those fields with a startup warning. Replaced `/bind <workspace_id> <path>` with `/bind "<existing-directory>"`; first private-chat pairing also requires the terminal code. Old authorization is not migrated automatically: start with `--pair` and pair privately to activate existing `chat_bindings`; starting without `--pair` while bindings are unpaired exits with guidance.
- Upgrading an existing deployment: stop the gateway, finish or resolve outstanding session transfers, and back up YAML and runtime state. The config file can stay unchanged — legacy fields are ignored with warnings and `chat_bindings` activate once an owner is paired. Start with `--pair`, pair privately in a terminal-verified chat, then rebind each chat. Do not delete unresolved transfer state.
- Local stdin/stdout now requires explicit `--console`, exactly one configured workspace, and no `--pair`. Missing WeChat tokens trigger QR login instead of implicitly selecting console mode.
- Removed `/question-decline` (Telegram `/question_decline`) to match Chord, where a question can only be answered or cancelled. Use `/question-withdraw <id>` to cancel a question; the `declined` outcome is no longer shown.
- Automatic session pins are keyed by workspace and route instead of paired identity. Re-binding a chat or moving to a different platform account keeps its session instead of silently starting a new one, and re-pairing to a different owner inherits the previous session: one workspace binding is one conversation, so a second owner can read the first owner's session unless `/new` or a deleted pin is used. Local `pairing reset` is the explicit exception: it clears the platform's automatic pins along with the owner and bindings, so a reset route starts without the old session. Pins written by the identity-scoped layout, and pins in the pre-`v2:` pipe-joined layout that the current key parser no longer addresses, are discarded on load rather than rewritten onto another identity. If you need a previous session after upgrading, list it with `/sessions` and re-pin it explicitly with `/resume <id>`. See [permissions and safety](./docs/permissions-and-safety.md) before sharing a workspace between owners.

### Added

- The gateway logs each configured outbound proxy once at startup as `outbound proxy ...` with credentials stripped, so operators can confirm the resolved value.
- Added optional outbound `proxy_url` globally and per IM (`ims.<platform>.proxy_url`, priority per-IM > global). Empty means direct; environment proxy variables are ignored. Supported schemes: `http`, `https`, `socks5`, `socks5h`. HTTP clients accept all four; WebSocket long-connections (Discord, Slack, Feishu, WeCom, DingTalk) accept `http`, `socks5` and `socks5h` and reject `https` at startup, and normalize `socks5h` to `socks5`. A per-IM `proxy_url` override is not blocked by an incompatible global default: an `https` global proxy is rejected only when a WebSocket platform actually resolves to it.
- `--pair` now prints a single pairing code immediately at startup instead of waiting for each platform to connect. The code works on every active platform and stays valid for the full 10-minute window, so multiple chats and platforms can be paired with it; a successful bind no longer consumes it. DingTalk shows the same startup code, but its bindings still require the same-enterprise identity probe before they authorize. See [pairing](./docs/pairing.md).

- Added Telegram Bot API polling with private/group text, owner pairing, controls, output splitting and throttling. Existing webhooks block startup and are not deleted automatically. See [Telegram](./docs/telegram.md).

- Added WeCom intelligent bot long connections with private/group text, binding probes, background replies and account exclusion. Network failures reconnect with backoff; connection replacement stops contention.
- Added single-workspace Slack Socket Mode with owner DM/channel/thread input, root `/chord` commands, mention-based thread controls, independent bindings, text escaping and throttling. Intake reserves time for ACK and diagnoses unconfirmed acceptance. See [Slack](./docs/slack.md).

- Added DingTalk internal-app Stream text and proactive replies. Each process start requires a private owner identity probe; existing bindings remain. Tokens and verification are memory-only. Temporary reconnects preserve accepted work within the process, and unknown sends are not retried. See [DingTalk](./docs/dingtalk.md).

- Discord bot integration with outbound Gateway connection, private owner pairing, explicit @Bot text input in server channels and existing public/private threads, and process-local message-ID deduplication. Each thread has its own binding. Minimal permissions need no Message Content privileged intent; output suppresses mention notifications and link embeds. Unlocked archived threads can reactivate on send. Native interactions, buttons, attachments, previews, and automatic thread creation remain deferred; real-account deployment checks are documented in the [Discord guide](./docs/discord.md).

- All platforms share bounded in-memory intake, process-local deduplication, authorization checks, per-chat ordering and Chord consumption receipts. Unknown outcomes are not replayed; `/status` reports waiting and isolation reasons.

- Added owner pairing (`--pair`), directory-based `/bind`, and local `pairing reset`. Directory switches preserve existing configuration and wait for outstanding work and session transfers to be resolved. Temporary connection loss preserves accepted owner inputs and replies; account, directory, and process changes prevent stale output from being sent under a new identity. Text and image delivery distinguish explicit rejection from unknown outcomes without automatically resending uncertain messages.

- MCP availability in `/status` and `/mcp status`, with notifications for visible status changes and login instructions for the session working directory on the host. Remote authentication commands are handled locally by the gateway and never forwarded to the model.

- Session takeover between a local TUI and IM on macOS/Linux: `/takeover <session-id> [--timeout <duration>]` waits for existing work to finish without cancelling it, preserves accepted queued input and TUI attachments, and resumes the target paused. `/takeover status` reports progress with bounded blocker details and recovery guidance; waiting timeouts do not stop the source. `/status` shows the restored queue's total count, owner tasks, bounded previews, and attachment counts before explicit continuation. Transfer guards survive gateway restarts and prevent messages, old cards, and reminders from reclaiming a session transferred out until explicit `/new` or `/resume` succeeds. Incoming target bindings open only after paused readiness, a valid queue summary, and a saved pin. Requires Chord takeover support; `chord_state_dir` can select a shared custom Chord state directory.

- `event_visibility.background_result` can now turn off the chat pushes for finished background job results. They remain enabled by default, and the durable result stays in the Chord session when disabled.

### Changed

- WeChat advances an in-memory cursor after each batch is admitted. Same-account renewal preserves progress; missing reply context waits for a fresh owner message. Oversized batches stop that receiver with a diagnostic without skipping the cursor.

- Feishu text/post and authenticated cards use shared in-memory intake and receipts. Topic groups use group authorization; non-Question cards update after command settlement rather than treating intake as completion. Question forms, role cards and image output remain available.

- Feishu text sends now share the common global concurrency and five-minute deadline, with 64 KiB/32-segment limits and visible truncation notices. Explicit HTTP 429 rejections retry only the rejected segment with cancellable waits; partial failures retain the confirmed segment count, and unknown outcomes are never replayed.


- Completion notifications now append a compact summary of the turn that just ended (duration, tool calls, retries), including the `⏳ Chord: Ready for input; background jobs still running: N` variant. Long-running reminders now report the elapsed turn time, a coarse stage such as `running tools` or `retrying`, and the tool and retry counts observed since the work cycle started. Confirmations, handoffs, and cancellation requests preserve the accumulated progress within the same session. Local shell work is tracked separately and does not reset agent progress; completion notifications show any shell commands still running. Transport-level phases such as `connecting` stay hidden.

- Unsupported YAML config keys are now reported in the gateway log with their line and ignored instead of doing nothing silently: typos and removed options such as `event_visibility.activity` are visible at startup while the rest of the file still loads.

- Duplicate explicit YAML keys in the configuration file no longer prevent startup. The first occurrence wins, every later duplicate is reported as a `duplicate key ignored` warning with its line, and the next configuration change rewrites the file without the duplicates.

- Private senders to an unpaired gateway now receive a bounded hint instead of being dropped silently: how to pair, what a malformed `/bind` is missing, or that a pairing code was wrong. When pairing is inactive the hint says to restart with `--pair`. Group chats, internal actions, and duplicate redeliveries stay silent, and hints draw on a separate rate-limit budget from pairing attempts.

- The WeCom setup steps are now a complete followable procedure. The admin console entry point is corrected to `Intelligent Zone → Smart Robot`; "Security and Management → Management Tools" is not the current entry point. Added that personal teams have neither a Workbench nor the web admin console, so both creation entry points are unreachable and an enterprise team or a "Change to Enterprise Account" conversion is required. Added the two permission layers — bot creation permission and API mode management — including that the long-connection scope defaults to empty above 1,000 contacts, which hides the Bot ID and Secret. Clarified that WeCom verification is not a prerequisite and that the creation page's "Available Permissions" is an MCP/CLI capability channel that Gateway does not use. Added that the available scope must contain the owner, how to pick the usage mode, and where to find the bot in the client. The troubleshooting guide gains diagnostics for these four setup blockers.

- The startup pairing prompt and the quickstart, pairing, and IM guides now show `/bind "<directory>" <pairing_code>` instead of `~/workspace`, which rarely exists on a fresh host and made a copied first `/bind` fail with "The directory does not exist or is not accessible." The placeholder matches the runtime hints, still quotes the path, and still requires an existing directory; concrete examples in the cookbook and platform guides are unchanged.

### Fixed

- Console mode no longer demands `--pair` for dormant chat bindings: its pairing remedy is mutually exclusive with `--console`, so the check is skipped there.
- Pairing hint replies are budgeted per private chat instead of per platform, so a spammer in one chat can no longer silence hints for unpaired users elsewhere on the same platform.
- `pairing reset` prints the duplicate-key and unsupported-field warnings it is about to drop from the rewritten config, instead of silently deduplicating the file.
- Discord now logs a bounded, rate-limited diagnostic when a mention carries no usable content, instead of dropping it silently.
- Corrected the Slack guide so its reconnect description matches redelivery: a saturated intake leaves the frame unacknowledged instead of backing off locally.
- Slack no longer drops a submission refusal without a trace: an envelope left unacknowledged for Slack to redeliver — for example when the shared normalization queue is saturated — is now counted as `lost_before_accept` with its bounded reason, like every other intake discard path.
- Documented that WeCom single-chat callbacks only carry a plaintext `from.userid` when the bot creator is an enterprise super admin, and corrected the heartbeat-ACK description to two consecutive misses.
- `/new` reports `Session created` and `/resume` reports `Session resumed`. Both previously answered `Session selected`, which described neither outcome and read as a failure on `/new`.
- `/new` and `/resume` now refuse a busy Chord process before the route-change barrier, replying that `/cancel` comes first, instead of failing inside the replacement with a bare reason. Session-change failures also report distinct reasons — busy, transfer in progress, unresolved inputs — instead of one generic `session_change_failed`.
- Session takeover no longer sends a full status dump for every internal event. A transfer previously produced a burst of near-identical messages whenever Chord's blocking set changed; it now reports that the transfer started, how long it has been waiting and on what, and that the session was saved. Unfinished progress repeats at most once a minute, an unchanged report is dropped, and failures still arrive in full with their recovery action. The reported wait is observed elapsed time, not a completion estimate.
- A takeover or session change that fails after the route-change barrier is raised now settles the route. A failure while persisting the transfer guard previously left that chat refusing every later task with `route_changing` until the gateway restarted, and WeChat also stopped advancing its polling batch at that message.
- `/new` and `/resume <id>` are no longer rejected as an expired control target while a workspace's Chord process is still starting up. Sessions that previously failed with `control_target_expired` right after launching or upgrading the gateway now start normally; a control target that captured a runtime identity and then lost it is still refused. Session-change failures and dropped readiness events are now logged with their cause instead of being reported as a bare rejection.
- Telegram intake now logs every fetched batch, the capture outcome of each update, and its dispatch disposition, so a message the gateway never accepts is distinguishable from one Telegram never delivered. An update that keeps failing to reach a terminal disposition warns once with its `update_id` and then at most once a minute, with an `acceptance_unconfirmed` metric, instead of retrying silently forever. A 409 immediately after a local restart retries briefly instead of exiting; only a persistent conflict stays fatal.
- WeCom answer hints preserve `@request` for copyable replies. Slack now honors per-method `Retry-After` across lookups, identity checks and socket reconnects.
- Partial-send diagnostics retain unknown delivery outcomes, including cancellation after sending; DingTalk distinguishes token acquisition and pacing failures from uncertain message delivery.
- DingTalk now reports the real refusal reason for a message it could not accept. Every refusal was recorded as `capacity`, so a busy target and a closing intake were indistinguishable in the loss metrics; it now reports `target_busy`, `capacity` or `ingress_unavailable` as appropriate.
- Intake capacity checks use a per-chat pending index, avoiding scans of retained completed messages during admission.
- Input that can never be delivered is now settled and reclaimed instead of staying pending forever. Once a chat accumulated enough isolated inputs, every later message was refused as over capacity and only a gateway restart recovered; isolated input now releases its pending slot and its body, and is reclaimed by the same 24-hour window as a delivered message.
- A delivery whose receipt never arrives is now settled after a bounded deadline. The receipt observer reports its own timeout at 30 seconds but never resolved the record, so one unconfirmed delivery could hold its chat's queue barrier and a pending slot until the gateway restarted.
- Bursts above the bounded response queues no longer tear down DingTalk Stream or Feishu long connections: the overflowing frame answers 500 so the loss stays observable — DingTalk robot callbacks are fire-and-forget, so the 500 reports a failure rather than requesting redelivery — while the connection stays up to drain workers. DingTalk also bounds its send-pacing table.
- A late Question receipt for an ingress record already expired after 24 hours no longer marks the process input blocked; it is skipped while the 14-day question operation window drains.
- A receipt that arrives after its delivery was settled no longer blocks the chat. A delivery that outlives the receipt deadline is settled while its receipt is still in flight, and the arriving receipt was then refused; the gateway read that refusal as backpressure and stopped accepting input from that chat until it restarted. A late receipt for a settled or pruned delivery is now dropped instead.
- Gateway metrics keep normalization and transport outcomes apart instead of collapsing them into `other`. The bounded label vocabulary was missing the reasons the shared normalizer and the transport liveness checks produce, so a failed normalization and an unknown refusal were indistinguishable in `/status` and the metrics log.

- WeCom proactive replies and binding probes now specify private/group addressing; assistant and channel callbacks no longer enter user command handling.
- Slow Slack conversation lookups can finish after the ACK wait expires, with a separate bounded admission deadline and process-local redelivery deduplication. Slack ACK hints and DingTalk mention placeholders now follow the English command-reply convention.
- Stopping Telegram during startup or polling now exits successfully while preserving genuine failures.

- Startup configuration warnings reach both stderr and the configured gateway log after resource locks are acquired.

- Session lists now use Chord’s configured storage and repository scope, including custom state directories and worktrees, instead of scanning a legacy workspace directory. Query failures are reported explicitly; requires `chord sessions list --json`.

- Pending durable inputs, blocked queues and uncertain receipts prevent directory switches, idle retirement and session replacement, including takeover into a disconnected binding. Before `/new`, `/resume`, directory switches and idle retirement, the gateway refreshes previously observed restored input summaries so completed work does not leave stale guards. `/status` explains queue blocks even after disconnection; requests are never automatically replayed.

- Requesting cancellation while idle no longer marks the process busy or starts repeated working reminders. Pending local shell commands remain busy across agent idle events and status snapshots, and are excluded from idle-timeout cleanup.

- Slack acknowledges `events_api` messages immediately, before conversation lookups finish, so a slow lookup can no longer miss the 3-second ACK deadline and trigger redelivery; only `/chord` slash commands that accept a response payload wait briefly for a hint, and a plain acceptance stays a bare ACK. Socket liveness is proven by any inbound frame instead of relying on an unguaranteed WebSocket ping interval.

- Gateway Resume replay for Discord now buffers up to 1024 useful events, so a longer transient disconnect no longer turns into a fatal reconnect loop.
- Shutting down the Discord gateway now releases senders waiting on the command rate limiter. `Close` previously waited for them without releasing them, so stopping or restarting the gateway could hang until the limiter next granted a slot.

- WeCom rejects out-of-range `create_time` values like every other platform instead of silently rewriting them to receipt time.
- A WeChat batch item that keeps failing without a terminal outcome no longer blocks the account's polling order silently. The gateway warns with its message ID, disposition, reason and consecutive failure count and records an `acceptance_unconfirmed` metric, repeating at most once a minute.
- WeCom now counts a malformed user text callback in `callback_dropped` with its bounded reason — `invalid_callback` or `source_time_out_of_range` — and warns at most once a minute; events, voice and other unsupported media stay ignored. Failed reconnect attempts also log the bounded transport category instead of leaving an empty log.
- A DingTalk send attempted while its account is unavailable now reports `connection_unavailable` in the shared text-failure categories instead of an unobserved bare error, and an input admission that raced adapter shutdown is refused with a 500 so the loss stays observable instead of being acknowledged without joining the work.
- The startup error for unpaired `chat_bindings` now shows the actual first `/bind "<directory>" <pairing_code>` command, including the pairing code, matching the runtime hints and the guides.
- Corrected the Chinese documentation's proxy anchors, the expired-confirmation description (a text reply is forwarded as an explicitly non-decisive follow-up instead of being rejected), and the permissions guide's platform, credential and incident-response lists, which were missing Telegram and Slack tokens.
- WeCom and DingTalk no longer dispatch an empty or whitespace-only message as an empty task; both reject it as invalid input like Telegram.
- Slack send failures report bounded categories (`invalid_target`, `forbidden`, `credentials`, `api_rejected`, `platform_rejected`) instead of collapsing every explicit rejection into `delivery_failed`, and the DingTalk refusal reply now tells the user to resend, since robot callbacks are fire-and-forget and are not redelivered.
- The startup proxy log reports the effective per-platform value and treats a whitespace-only `proxy_url` as a direct connection instead of reporting it as configured.


## v0.3.3 – 2026-10-12

### Added

- Forward generated images to the source WeChat or Feishu chat in order. Feishu sends a smaller preview for images at or above its upload limit while Chord keeps the original. Image processing limits transient memory, and failed transfers do not flood the chat with chunk errors. WeChat CDN uploads retry transient failures up to three attempts; Feishu uploads only retry an explicit expired-token rejection once; a failed or unconfirmed image delivery never triggers generation again. Requires Chord headless with image summaries and artifact chunks.

- Questions use ID-based collections and support historical revisions while another question is pending. Ordinary chat keeps questions open; answer acceptance waits for a durable Chord receipt. IM answer entries disable timing before opening, and defaults never appear as user consent.

- Idle notifications and `/status` show running background jobs. Processes reporting active background jobs are excluded from idle-timeout cleanup.

- Added IM `/role` command for switching the main agent role (for example `builder` → `planner`) via Chord headless: `/role` lists the current role and the switchable roles (a Feishu button card, or a numbered list on text platforms), while `/role <number>` / `/role <name>` pick the target. The gateway always subscribes to Chord's `role_change` event and tracks the active role for `/status`. Requires a Chord headless build that supports the `role` control command. Switch request failures or response timeouts resolve the clicked Feishu card as unconfirmed and direct users to `/status` before retrying. A Feishu card click is patched in place and does not also send a duplicate chat line.
- Gateway now always subscribes to Chord's `compaction_status` event and surfaces the latest context-checkpoint outcome in `/status` (for example `succeeded`, `skipped` with its reason, or `failed`); outcomes are never pushed as chat messages. A skipped checkpoint that never occupied the compaction slot does not overwrite the outcome of a compaction that is still running.
- Gateway now consumes Chord's `handoff_cancelled` event: when a pending handoff is cancelled without a decision (for example superseded by a new request or session switch), the stale pending request is dropped and the chat receives a cancellation notice. A later `/handoff` or `/handoff-deny` returns the usual no-pending warning instead of routing the reply to the cancelled request.
- Gateway now always subscribes to Chord's `background_result` event and pushes a finished background job's durable result to the chat; Chord has no other delivery channel for it, so it was previously invisible to IM. Durable context-pressure warnings (`context_notice`) are also consumed, but pushing them to a chat is opt-in through the new `event_visibility.context_notice` flag (default `false`), because that delivery is a product decision rather than a correctness requirement.
- Multi-select questions are now interactive Feishu cards: the options become checkboxes inside a form with a `Submit` button, and the checked options are submitted as the answer. Submitting the form with nothing checked shows a hint instead of sending an empty answer. Unrecognized checkbox values are rejected rather than interpreted as selections.

### Changed

- Busy state and reminders for normal inputs now follow Chord consumption receipts and activity, avoiding false busy state for local commands. Activity is always subscribed; remove `event_visibility.activity` from configuration. Disconnects report unconfirmed inputs without replaying them. Requires Chord headless with input receipts.

- Updated the Go toolchain requirement to 1.27.0 and refreshed third-party Go dependencies (`golog` v0.4.1, Lark SDK v3.10.0).
- SubAgent `agent_notify` notifications now include the alert subtype (for example `blocked/stall_resolved`), so a resolved stall is distinguishable from one that is still pending.
- The periodic `performance queue=...` metrics line now also reports the current deepest shard (`max_depth`) and completed queue-capacity waits (`blocked_wait_total`, `blocked_wait_max`, `blocked_slow`), helping diagnose notification backpressure. Ongoing waits and shard producer-lock waits are not included in the duration metrics.
- Feishu question cards no longer fall back to plain text because an option label is long: when any label exceeds the button budget the card switches to numbered buttons (`1`, `2`, …) while the card body still lists every label in full, and option details no longer decide the card form. Only free-answer questions and questions with more than ten options still use the text form.

- Message sends now require an explicit successful platform receipt; Feishu also requires a message ID. Missing receipts, transport failures and server errors are reported as unconfirmed instead of successful, without automatic resending, and a non-JSON response is no longer treated as delivered. Previously such a send could be reported as delivered.

### Fixed

- Message requests no longer follow redirects that could replay a send; unexpected HTTP statuses are reported as unconfirmed. A platform rejection (4xx, except 408/429) is reported as a definite failure instead of an unknown outcome, so a rejected Feishu card still uses its text fallback while an unconfirmed card does not repeat its prompt as text.

- Feishu requests now use a tenant access token instead of an app access token; the token is still refreshed automatically from the configured app credentials, including explicit expired-token rejections returned with ordinary 4xx responses.

- Question cards now settle even when a status snapshot cleared pending first. Timeout notices are no longer repeated on later idle events, and Feishu question submissions keep the original card until the outcome arrives.
- Feishu question buttons use stable option IDs, so repeated or numeric labels select the option clicked. A failed or uncertain submission can be explicitly retried; redelivery is deduplicated by callback event ID. System defaults are shown as system decisions in both cards and text. Failed answer-entry presentation can be retried with `/status`. Answering an ID missing from the local cache automatically synchronizes restored questions; terminal notices identify the question and answered cards retain the selection.

- Input IDs remain unique across Chord process restarts, preventing restored question transactions from colliding. Historical answer acceptance no longer invents busy state. Reminders do not spin while waiting for input, and open asynchronous questions do not suppress reminders during other work.

- Headless JSONL frames now accept up to 16 MiB of JSON, including output expanded by JSON escaping. Malformed lines produce a chat warning and reading continues. Oversized frames and stream failures stop the Chord process within a bounded grace period and report the failure in chat, without replaying inputs or scheduling a crash restart.

- Only a recognized companion `notification` alert is deduplicated after the matching confirmation or handoff prompt is delivered successfully. Stale pending entries no longer hide unrelated alerts, and failed prompt delivery preserves its fallback alert. Alerts without a dedicated prompt, such as a loop-exit decision or a notify-protocol correction, still arrive.
- Gateway now preserves headless idle state transitions for configuration-only model-pool switches while suppressing the generic ready-for-input message. Pending confirmation, question, and handoff expiry notifications continue to be delivered.
- Corrected the Feishu setup guide against current Feishu Open Platform behavior: the event page is **Event Subscriptions** (not “Events and callbacks”), `im.message.receive_v1` appears as **Receive message v2.0**, and receive-message scopes are **per scenario** — using both DMs and group chats requires granting both the DM scope and the group scope, not just any one of them. The guide now also documents the **Batch import** JSON flow and clarifies that `im:message:update` is not required because `im:message` / `im:message:send_as_bot` already cover the card update call.
- Gateway now follows Chord's `session_switched` event. The session pin was only written when a Chord process started, so an in-band switch (handoff plan execution, `/resume <id>`, `/new`) left a pinned binding pointing at the session the switch abandoned, and a later respawn resumed the wrong session.
- Gateway now drops a headless `status_response` snapshot that a newer push overtook: Chord versions state-carrying envelopes with a monotonic `seq`, and the status copy is taken on a different goroutine than the push emit, so a late snapshot must not roll SessionID, busy, or pending interactions back. Requires a Chord headless build that sends `seq`.
- Gateway now cancels a pending crash auto-restart and stops the idle-timeout checker as soon as shutdown begins. A crash that happened just before shutdown used to leave a goroutine waiting out the whole restart delay and log a misleading `auto-restart failed ... chord manager is shutting down` error.

### Breaking changes

- Requires Chord headless with input receipts and durable question collections; older single-question protocols are unsupported. Ordinary chat no longer answers pending questions: use `/answer @<id> <answer>` or a current Feishu answer entry.
- Removed `event_visibility.activity`; delete it from configuration. Activity tracking is always enabled; unknown YAML fields are ignored.

- Removed the IM `/current` command. `/status` already shows the active session, current role, last context checkpoint, and pending interaction.

## v0.3.2

### Added

- Added end-to-end SubAgent event forwarding: completion summaries are now always subscribed and sent to IM, optional `agent_started` / `agent_notify` events expose delegation progress, and SubAgent assistant text is labeled with its agent and task identity.
- Added Chord headless `handoff_request` support: gateway now subscribes by default, displays the full handoff plan with available agents/model pools, and accepts `/handoff <agent> [model_pool]` or `/handoff-deny <reason>` responses.
- Added IM `!` / `！` local shell shortcuts backed by Chord headless `local_shell` commands. Gateway now subscribes to `local_shell_result` by default and renders stdout/stderr results back to IM chats.

### Breaking changes

- Removed Chord headless `tool_result` support from gateway configuration, subscription, state, and IM rendering. Gateway now consumes non-loop Done reports via Chord's `done_completion` event; loop-mode Done exit requests remain `confirm_request` events with `done_report` / `done_reason` fields.

### Changed

- Streamlined the bilingual quickstart around a single-workspace first-run path, linked the official Chord installation instructions, removed repeated Feishu binding procedures from the configuration reference, and clarified IM/model data boundaries and Feishu allowlist requirements.
- Upgraded the documentation stack to Astro 7 and Starlight 0.41, aligned the declared Node.js baseline with CI, and refreshed transitive dependencies so the production dependency audit reports no known vulnerabilities.
- Updated the Go toolchain requirement to 1.26.3 and refreshed third-party Go dependencies.
- Done confirmation rendering now prefers explicit `done_report` / `done_reason` fields and treats plain text during a pending Done confirmation as the deny reason.

### Fixed

- Restricted gateway-managed state directories to `0700` and sensitive persisted state and rotating logs to `0600`, including existing files that were previously created with wider permissions.
- Fixed IM session recovery when pinned Chord sessions are stale or unavailable: `/new` now prefers an existing live process but starts fresh without a stale resume when needed, normal messages retry once in a fresh session after stale/busy pinned-session delivery failures, `/resume <id>` now reports missing/busy sessions instead of falsely appearing successful, and exited Chord processes are no longer reused.

## v0.3.1 – 2026-05-11

### Changed

- Updated Chord integration references from the old `Bash` tool name to the breaking `Shell` tool name and aligned command-execution confirmation risk/summary rendering with current Chord events.
- Improved cross-repo headless contract tests for current Chord config requirements and ready-timeout diagnostics.
- Tightened CI quality checks by keeping the coverage gate at 70.0%, aligning staticcheck execution, and adding a gopls validation job.

### Added

- Added compact gateway build identity output to `chord-gateway --version` (version, short commit, and dirty marker when applicable). Startup logs now include `gateway_version`, `gateway_commit`, `gateway_build_time`, `gateway_vcs_time`, `gateway_dirty`, and `go_version`. Child `chord headless` spawn logs now include the configured `chord_binary` path and mtime for provenance checks.

## v0.3.0 – 2026-05-05

### Breaking changes

- Removed the `HandleMessage(imType, chatID, text)` router entrypoint. Use `HandleIncomingMessage` with a structured `IncomingMessage`.
- Removed YAML `ims` and `workspaces` sequence (list) forms in `config.yaml`. Both must now be mappings keyed by adapter type / workspace id.
- Removed unused fields from the protocol/state model: `ConfirmPayload.TimeoutMS`, `ConfirmPayload.AlreadyAllowed`, `QuestionPayload.TimeoutMS`, `IncomingMessage.ConversationID`, and `ControlState.LastStatusResponseAt`. The fields were never consumed; downstream consumers that relied on them must migrate to the equivalents (e.g. use `WaitStatus` to observe `status_response` arrival rather than polling `LastStatusResponseAt`).
- Renamed the package-private `normalizeIMType` helper to `config.NormalizeIMType` and made it the single source of truth across packages. The duplicate copies in `main` and `config` are gone; downstream code must use `config.NormalizeIMType`.

### Added

- Added a compatibility policy document that records the remaining compatibility surface (the Chord headless `todos` event name) and the cleanup rule.
- Added regression tests covering session-pin write failures and concurrent session-pin updates.
- Added regression tests for idle event rendering and normal-idle pending-confirm cleanup.
- Restored WeChat regression coverage for persisted token / sync state loading, expired-token re-login, custom token paths, `splitText`, and context-aware sleep cancellation.
- Added `SessionLoginNotifier` interface so IM adapters depend on a narrow contract for cross-channel session/login notices instead of the full `*NotificationRouter`.
- Added `ChordProcess.BeginTurn` / `MarkVisibleOutput` methods so the router no longer reaches into `ChordProcess.state` directly when stamping reminder counters.
- Added `ControlState.applyPendingConfirm` / `applyPendingQuestion` / `applyStatusResponse` helpers to keep "incoming pending interaction" handling consistent.

### Changed

- Refactored the router and process implementation into topic-specific files (`router_commands`, `router_format`, `router_feishu_cards`, `router_reminders`, `router_parse`, `process_protocol`, `process_lifecycle`, `process_env`) without changing documented behavior.
- Centralized rune-aware truncation helpers in a single `text.go` (`truncate`, `truncateLine`, `truncateButtonLabel`, `shortID`, `truncateStderrTail`) so all IM-facing string trimming uses one consistent implementation.
- Centralized Feishu helpers (`buildFeishuResolvedCard`, `displaySender`, `updateFeishuCardStatus`, `buildExpired*Followup`) in `router_feishu_helpers.go` and the new `resolveFeishuCard(...)` wrapper, removing seven duplicated `updateFeishuCardStatus + buildFeishuResolvedCard` call sites in router commands.
- Extracted `submitQuestionAnswer` so `/answer` and plain-text auto-redirect share a single answer-dispatch path.
- Extracted `compositeKey` so process keys, Feishu dedupe keys, and Feishu card-handle keys all share one canonical join helper.
- `todos` events now forward the full current todo list on every event when enabled, instead of only surfacing the current in-progress item.
- `/deny` now takes an optional human-readable reason text instead of a platform-internal request ID; the gateway resolves the pending confirmation automatically.
- `/new` now sends the command to chord via stdin rather than killing the process, letting chord manage session lifecycle gracefully.
- `/bind` now rejects rebinds while a session is actively running; cancel the current turn first.
- Feishu cards no longer embed user-facing commands for confirm/question buttons; they dispatch structured internal actions so the IM protocol stays clean.
- Feishu post-format messages are now accepted alongside plain text, enabling rich-text input for all commands.
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
- Updated `github.com/keakon/golog` to v0.3.0.
- Standardized remaining runtime/user-facing messages in non-Chinese docs and IM responses to English.
- `handleChordCommand` now takes an explicit `*IncomingMessage` instead of a variadic, making the "optional message context" behaviour explicit.

### Removed

- Removed unused helpers and dead state: `MultiAdapter.BroadcastText`, `MultiAdapter.Adapters`, `FeishuAdapter.SendInteractive`, the `WechatAdapter.sessionExpired` flag, and the `ControlState.StreamText` / `LastThinkingText` fields.
- Removed raw-array Chord headless `todos` payload support; gateway now only accepts the current wrapper payload shape (`{"todos":[...]}`).
- Removed the `WechatAdapter.sleep` thin wrapper, the `runLongConn`-self-assignment dance in `NewFeishuAdapter`, the `cloneStringMap` helper (replaced with `maps.Clone`), and the now-unused `feishuDedupeKeyFmt`/`feishuCardActionFmt` Sprintf format strings.

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

## v0.2.0 – 2026-04-30

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

## v0.1.0 – 2026-04-29

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
