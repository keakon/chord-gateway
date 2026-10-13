# Usage

This page covers day-to-day interaction with `chord-gateway` from IM chats.

## First message checklist

After the gateway starts:

1. Send `/status` from the connected IM chat.
2. Confirm the reported workspace and IM binding are the ones you expect.
3. If a session is already pinned, decide whether to continue it or send `/new`.
4. Send a normal text request to Chord.

## Routing model

The gateway isolates Chord sessions by IM channel and workspace.

A session binding is keyed by:

```text
workspaceID | imType | chatID
```

Examples:

- `default|wechat|user123`
- `backend|feishu|oc_xxx`

Different chats keep different session context. Different workspaces also keep different session context.

## Session pinning

The gateway does not use `chord headless --continue`. Instead, it stores a pinned Chord session ID for each binding.

- If a binding has a pinned session ID, the gateway starts Chord with `--resume <sid>`.
- If resume fails because the pinned session was removed or is busy, a normal text message is retried in a fresh session and the user is notified.
- If no session ID is pinned, the gateway starts a fresh session.
- `/new` starts fresh. A live binding retains its pin until Chord confirms the session switch.
- `/resume <sid>` pins the specified session ID for the current binding.
- An in-band session switch (`session_switched`) re-points the pin at the newly active session, so later spawns resume the session the runtime actually runs.

Session pins are persisted in `<state_dir>/session-pins.json` unless `session_pins_file` is configured.

A pending or completed session transfer takes precedence over a pin. Ordinary messages cannot restart that binding or fall back to a fresh session. When takeover into this chat reaches `ready_paused` and its session pin is saved, the target binding opens for explicit continuation. A binding that transferred its session out remains blocked until an explicit `/new` or `/resume` succeeds; failed attempts retain the guard.

## IM commands

| Command | Description |
|---|---|
| `/status` | Show current Chord state: busy/idle, session, current role, phase, last context checkpoint, pending interaction, and restored queued input |
| `/cancel` | Cancel the current turn |
| `/allow` | Approve the pending confirmation |
| `/deny [reason]` | Deny the pending confirmation; optional reason text is forwarded to Chord |
| `/answer @<id> <text>` | Answer or revise an explicit question ID; numeric shortcuts are supported |
| `/handoff <agent> [model_pool]` | Forward an acceptance for a pending Chord handoff request; omit arguments to let Chord use its default agent |
| `/handoff-deny <reason>` | Forward a rejection for a pending Chord handoff request with a reason |
| `/role [number\|name]` | Show the current main-agent role and the switchable roles, or switch to a target role by list number or name |
| `!<command>` / `！<command>` | Send a Chord `local_shell` command through the bound workspace and return Chord's stdout/stderr result |
| `/todos` | Show the current todo list |
| `/new` | Ask the current Chord process to start a new session; if no process is available, clear the current session pin and start a fresh Chord process |
| `/resume <id>` | Resume and pin a specific session; if the session cannot be resumed, the pin is cleared and the user is notified |
| `/takeover <id> [--timeout <duration>]` | Take over a running TUI session into this chat and leave it paused |
| `/takeover status` | Query the current binding's transfer without starting a new request |
| `/sessions` | List recent sessions from the workspace |
| `/login [platform]` | Show supported login-renewal platforms, or start one when a platform is provided (for example `/login wechat`) |
| any other text | Send new instructions to Chord; pending questions remain available |

Notes:

- `/login wechat` is the documented command name for WeChat login renewal.
- `!` and `！` are gateway shortcuts for Chord's `local_shell` headless command, not normal Chord chat messages. Use them only in trusted chats because Chord may execute the command in the local workspace environment.
- Unrecognized slash commands are forwarded to Chord as normal text.
- `/summary` is not part of the documented gateway command set.

## Session takeover

Takeover moves a running session between TUI and gateway-managed headless on the same macOS or Linux host, under the same OS user and canonical workspace. It requires a Chord build with the takeover CLI and managed-headless flags. An older Chord can still serve ordinary chat bindings, but cannot perform takeover. Ordinary headless, ACP, another workspace, and Windows are outside this feature's scope. For an offline session, use `/resume <id>`.

To move a TUI session into an IM chat whose current binding has no live Chord process:

```text
/takeover <session-id> --timeout 30s
/takeover status
```

If the chat already owns a process, explicitly handle that binding first. To move the gateway session to a new TUI, run Chord from the same workspace on the gateway host:

```sh
chord -d /path/to/workspace --takeover <session-id> --takeover-timeout 30s
```

Chord provides the canonical workspace identity, so a linked worktree can transfer within its parent repository while retaining the saved execution directory. A rejected preflight leaves ordinary chat available. When preparation fails before the source freezes, a status query reopens routing only after confirming the same live source; uncertain outcomes keep the binding blocked.

The source stops admitting new execution and waits for requests, tools, and background work that already started to finish and save their results. It does not cancel those operations or start the next model request. Accepted queued input, including attachments submitted through TUI, stays with the session. The gateway accepts text from IM; takeover does not add inbound image or file uploads. Generated-image forwarding to IM remains available. A service or tool waiting for input can keep the transfer pending; status reports the blockers.

Transfer notifications prioritize errors, timeouts, and recovery actions. They show the total blocker count and at most three short previews, with the omitted count when needed. For full details, use the [host status command](./operations.md#session-transfer-recovery).

`--timeout` limits observation only. `0` returns the transfer ID and current state immediately; omitting it keeps observing. Expiry or disconnection leaves the source draining with its session lock. Query the existing transfer with `/takeover status`; `/status` also shows transfer status while the binding is blocked. A repeated start returns `transfer_busy`. There is no cancel-transfer command.

The target resumes the same session in a paused state. Use `/status` to check the pause state and total queued-input count. It shows up to eight entries with their owner task, a short single-line preview, and attachment count, and reports how many additional entries remain. Full input content and attachments stay in the Chord session. Then explicitly send a continuation or a new normal message to begin model work. Taking over does not itself start a turn. During the transfer, ordinary messages, old card callbacks, and reminders cannot create a replacement process or send work into another binding instance.

If saving fails, the source keeps its lock and execution stays frozen. If the session is saved but the target fails to load, there is no fresh-session fallback: query status and explicitly `/resume <id>` once the source has released the lock. `/new` starts a separate session when you want to abandon this chat binding. Transfer guards survive gateway restarts. When takeover into this chat reaches `ready_paused` and its session pin is saved, the target binding opens for explicit continuation. A binding that transferred its session out remains blocked until an explicit `/new` or `/resume` succeeds; failed attempts retain the guard. See [operations.md](./operations.md#session-transfer-recovery) for terminal queries and save retries.

## Question interaction

Questions remain available by ID. You can answer later and continue ordinary chat. `/status` lists all pending questions; ordinary text supplies new instructions and is never automatically treated as an answer.

- `/answer @<id> 1` selects an option; use `1,3` for multiple choices or `/answer @<id> <text>` for a custom answer. Omit the ID only when exactly one question is pending.
- Feishu option buttons and multi-select forms answer their associated question ID.
- `/question-interact <id>` disables timing; `/question-decline <id>` declines without withdrawing a required decision.
- `/question-withdraw <id>` withdraws a requirement; `/question-replace <old> <new>` associates a replacement. `/new-task <text>` starts an independent task; `/question cancel-task` cancels the current task.

IM cannot reliably detect text entry, so the gateway asks Chord to disable timing before offering an answer entry and waits for the committed update. Required decisions already wait indefinitely. System defaults are labeled as adoption by the system and do not mean user consent.

If an explicit question ID is missing from the local cache, the gateway synchronizes with Chord before answering, including historical questions restored after a process restart. Synchronization failures and IDs absent from the current session are reported separately.

After submitting, the gateway waits for a correlated `question_receipt` and reports `✅ Answer accepted` only on explicit acceptance. Failure retains the question. Timeouts or disconnects leave the outcome unknown; check `/status` before retrying. Queue admission is not acceptance. Terminal notices include the question ID, and answered Feishu cards retain the accepted selection. Feishu cards follow Chord's terminal outcome, and disconnects do not invent outcomes.

If the presentation command cannot be sent or is rejected, the gateway reports the problem; send `/status` to retry presentation. Status synchronization also closes cards when it observes the result before the corresponding push.

For an unknown answer outcome, check `/status` and explicitly retry the same answer as the same user. While the gateway remains running, it retains up to 128 uncertain question operations for 14 days and reuses their original operation IDs and parameters, including across Chord process restarts. A new Feishu click is accepted; only redelivery of the same callback event is deduplicated. Restarting the gateway clears this retry cache: check the restored session before submitting again. Inputs are never replayed automatically.

An accepted historical revision does not by itself mean Chord started work. Open asynchronous questions do not suppress reminders or unrelated alerts while Chord continues other work.

Always target historical revisions by ID, for example `/answer @q-1 revised text`. A new pending question does not prevent correcting an older one. Revisions append a separate result, retain the original outcome and task, and do not change an unrelated current task. Session switches select a different question collection.

## Confirmation interaction

When Chord asks for permission, Feishu can show an interactive confirmation card with `Allow` and `Deny` buttons. The card includes the risk level, tool name, argument summary, request ID, and workspace/session context when available. You can also reply with text:

- `/allow` to approve
- `/deny [reason]` to reject, optionally including a reason that is forwarded to Chord

Both commands only apply to the currently pending confirmation. If no confirmation is pending, the gateway warns that there is nothing to respond to. If you supply a request ID that does not match the current pending confirmation, the gateway rejects the action as a mismatch.

After a Feishu confirmation button is accepted, the gateway tries to update the original card to show the resolved state. If that update fails, the text confirmation is still sent and the Chord action is not rolled back.

Use `/status` if you are unsure whether a confirmation is pending.

If the pending confirmation expires because Chord becomes idle or the gateway removes the idle process, the gateway sends an expiry notice. A later `/allow` or `/deny` is not sent as an approval or denial. Instead, the gateway forwards it to Chord as follow-up context that explicitly says it must not be treated as confirmation.

The user-facing notice is in English, for example:

```text
⚠️ The pending confirmation has expired. Your response was sent as a follow-up message, not as an approval or denial.
```

## Handoff request routing

When Chord sends a `handoff_request`, the gateway posts the handoff plan, available agents, and model pools to the IM chat. The gateway does not implement handoff itself; it only routes the user's response back to the pending Chord request:

- `/handoff` to let Chord accept with its default agent.
- `/handoff <agent>` to choose one of the agents shown by Chord.
- `/handoff <agent> [model_pool]` to choose both an agent and a model pool.
- `/handoff-deny <reason>` to reject the handoff request.

Use the agent and model pool names exactly as shown in the gateway message. If no Chord handoff request is pending, these commands only return a warning and do not start a new Chord action.

If Chord cancels a pending handoff without a decision (for example because a new request or session switch supersedes it), the gateway sends a cancellation notice and drops the request. A later `/handoff` or `/handoff-deny` then returns the same no-pending warning instead of routing to the cancelled request.

## Role switching

`/role` switches the main agent role (for example `builder` → `planner`) without starting a new session or discarding the conversation context.

- `/role` shows the current role and the roles Chord offers. In Feishu the list is an interactive card with one button per role; on text platforms it is a numbered list.
- `/role <number>` picks a role from the numbered list. The gateway re-fetches the list from Chord when you select, so the number resolves against the current roles.
- `/role <name>` switches by role name.

```text
You: /role
Gateway: 🎭 Current role: builder
1. builder (current)
2. planner
Reply /role <number> or /role <name> to switch.
```

Only the roles Chord configures as main-mode agents are offered; SubAgents are never switch targets. Switching to the current role is reported as a no-op. If Chord rejects the switch (for example while a handoff decision is still pending), the gateway shows Chord's message. The difference from `/handoff`: handoff hands a pending plan to another role for execution, while `/role` changes the active role of the ongoing session directly.

If the switch request fails or its response times out, the gateway reports an unconfirmed result and attempts to resolve the clicked Feishu card instead of leaving it in Processing. A missing response does not prove the switch failed; send `/status` to check the current role before retrying. A Feishu card click is patched in place and does not also send a duplicate chat line; text platforms still get a message.

Requires a Chord headless build that supports the `role` control command; with an older Chord the command reports that the role list cannot be loaded.

## Chord local shell shortcut

Messages that start with `!` or the full-width `！` are sent to Chord as `local_shell` headless commands instead of normal chat text:

```text
You: !pwd
Gateway: ✅ Local shell: pwd
/path/to/workspace
```

The gateway forwards the command and displays Chord's result. Shell commands have their own timer; agent turn summaries report only observed agent work. A shell request keeps the process busy until its result arrives; long-running shell commands receive reminders and are excluded from idle-timeout cleanup. Execution semantics and availability belong to Chord headless. Treat this as local command execution in the bound workspace environment: use it only in trusted IM chats and avoid sending secrets in command text or output.

## Session examples

List recent sessions:

```text
You: /sessions
Gateway: 📂 Recent sessions:
  • 2026-04-14-abc123 (2026-04-14 17:30)
  • 2026-04-14-def456 (2026-04-14 15:02)
```

Resume a session:

```text
You: /resume 2026-04-14-abc123
Gateway: 🔄 Resuming session 2026-04-14-abc123

# If the session no longer exists or is busy:
Gateway: ❌ Failed to resume session 2026-04-14-abc123. It may not exist or may be busy.
```

Start fresh:

```text
You: /new
Gateway: 🆕 /new sent to chord process.
# If no Chord process is currently usable:
Gateway: 🆕 New session started.
```

## Multi-IM login

In multi-IM mode, the gateway can notify another active IM channel when one platform expires or becomes unavailable.

Example: if WeChat login expires, send this from Feishu:

```text
/login wechat
```

The gateway replies with a WeChat QR login link. After scanning, it updates the token without requiring a gateway restart. Login success or failure is also reported through another IM channel.

Notes:

- `/login` without a platform only shows usage and supported login-renewal platforms; it does not start any login flow.
- `/login` is only for platforms that support interactive login renewal; the documented renewal flow today is `/login wechat`.
- Feishu does not need and does not support in-chat login or renewal through `/login feishu`. The gateway automatically obtains and refreshes Feishu access tokens from the configured app credentials.
- If Feishu connection/configuration becomes invalid, check the deployment configuration and Feishu developer console for app credentials, permissions, event subscriptions, and long-connection settings; do not send or change `app_id` / `app_secret` in an IM chat.
- Cross-IM notification requires at least one other IM channel to remain available, and the gateway must know that channel's chat ID. For Feishu, configure `chat_bindings` or send a message once in the target chat.

## Notifications

The gateway pushes important control-plane notifications to active IM channels, including:

- confirm required
- question required
- task started
- task completed
- background job results, sent when a background job finishes — usually after the turn already went idle; disable with `event_visibility.background_result: false`
- error or blocked state
- tool failure
- generated images, forwarded to the source chat as Chord produces them
- context-pressure warnings, only when `event_visibility.context_notice` is enabled
- long-running reminders every 5 minutes while a turn is still busy

After a confirmation or handoff prompt is delivered successfully, its recognized companion `notification` alert is dropped. Other alerts remain visible even while an earlier confirmation or handoff remains in the gateway cache. Failed prompt delivery leaves the companion alert available. Durable questions have no companion alert and do not suppress notifications. Alerts that have no prompt of their own, such as a loop-exit decision, still arrive.

Long-running reminders intentionally do not expose low-level phases such as `connecting`; they do report the elapsed turn time, a coarse stage, and the tool and retry counts observed since the turn started. Any user-visible output resets the next 5-minute reminder window. For example:
```text
⏳ Still working (12m03s · running tools · 7 tools · 1 retry · 4 internal events)
```

A retry counts every retry decision Chord announced during the turn, including fallback switches, so it is a pressure signal rather than an exact failure count. The internal-event count currently comes from gateway-tracked progress events such as `todos`; it is reset after each user-visible output or reminder. If `event_visibility.todos` is enabled, every `todos` event is sent as the full current todo list without deduplication.

Completion notifications also carry a compact summary of the turn that just ended: its duration, the tool calls reported across the main agent and SubAgents, and the retry decisions observed, for example `✅ Chord: Ready for input (1m42s · 12 tools)`. The same summary is appended to the `⏳ Chord: Ready for input; background jobs still running: N` variant. Within the same session, confirmations, handoffs, and cancellation requests preserve the progress accumulated during the active work cycle. An idle cancellation request does not start a work cycle or reminders. Local shell results do not reset agent progress; if shell commands are still pending when agents become idle, the completion notification reports their count and shell reminders continue.

Optional lower-level events are controlled by `event_visibility`. See [event-visibility.md](./event-visibility.md).
