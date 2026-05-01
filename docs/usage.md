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
- If no session ID is pinned, the gateway starts a fresh session.
- `/new` clears the current binding's pin and starts fresh.
- `/resume <sid>` pins the specified session ID for the current binding.

Session pins are persisted in `<state_dir>/session-pins.json` unless `session_pins_file` is configured.

## IM commands

| Command | Description |
|---|---|
| `/status` | Show current Chord state: busy/idle, phase, and pending interaction |
| `/cancel` | Cancel the current turn |
| `/allow` | Approve the pending confirmation |
| `/deny [reason]` | Deny the pending confirmation; optional reason text is forwarded to Chord |
| `/answer <text>` | Answer a pending question; numeric shortcuts are supported |
| `/todos` | Show the current todo list |
| `/new` | Clear the current session pin and start a fresh session |
| `/resume <id>` | Resume and pin a specific session |
| `/sessions` | List recent sessions from the workspace |
| `/current` | Show the current binding status, including workspace, IM/chat binding, active session, and pending interaction |
| `/login [platform]` | Start a login flow, for example `/login wechat` |
| any other text | Send the text to Chord, or answer a pending question if one exists |

Notes:

- `/login wechat` is the documented command name for WeChat login renewal.
- Unrecognized slash commands are forwarded to Chord as normal text.
- `/summary` is not part of the documented gateway command set.

## Question interaction

When Chord sends a `question_request`, the gateway sends a numbered question to the IM chat. In Feishu, supported single-select questions are shown as interactive cards with option buttons; users can click an option or reply with text. Single-select questions with up to 10 options use button cards only when the option text and rendered body stay short enough. Multi-select, free-answer, too many options, or option/detail content that would make the card too long fall back to the text form.

After a Feishu button click is accepted, the gateway tries to update the original card to show the resolved state. If that update fails, the text confirmation is still sent and the Chord action is not rolled back.

When a Feishu question is still pending, users can also send a plain-text reply instead of clicking a button. The gateway submits that text as a free-text answer and, if the card update succeeds, relies on the updated card state instead of sending an extra duplicate `💬 Answered` text confirmation.

```text
❓ Continue?
  1. yes — Yes, proceed
  2. no — No, stop
Reply /answer 1 / 1,2 / or type your answer
```

You can answer in these ways:

- Click a Feishu option button when an interactive question card is shown.
- Use `/answer`, for example `/answer 1` or `/answer 1,3` for a multi-select question.
- Send a plain text message while a question is pending. The gateway treats it as a free-text answer.

Invalid numeric shortcuts are sent as custom text instead of being silently accepted.

If the pending question expires because Chord becomes idle or the gateway removes the idle process, the gateway sends an expiry notice. A later `/answer` is not sent as the original structured answer because the request ID is no longer pending. Instead, the gateway forwards it to Chord as a normal follow-up message, including the expired question when it is still available.

The user-facing notice is in English, for example:

```text
⚠️ The pending question has expired. Your response was sent as a follow-up message, not as a structured answer.
```

## Confirmation interaction

When Chord asks for permission, Feishu can show an interactive confirmation card with `Allow` and `Deny` buttons. The card includes the risk level, tool name, argument summary, request ID, and workspace/session context when available. You can also reply with text:

- `/allow` to approve
- `/deny [reason]` to reject, optionally including a reason

After a Feishu confirmation button is accepted, the gateway tries to update the original card to show the resolved state. If that update fails, the text confirmation is still sent and the Chord action is not rolled back.

Use `/status` if you are unsure whether a confirmation is pending.

If the pending confirmation expires because Chord becomes idle or the gateway removes the idle process, the gateway sends an expiry notice. A later `/allow` or `/deny` is not sent as an approval or denial. Instead, the gateway forwards it to Chord as follow-up context that explicitly says it must not be treated as confirmation.

The user-facing notice is in English, for example:

```text
⚠️ The pending confirmation has expired. Your response was sent as a follow-up message, not as an approval or denial.
```

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
```

Check the current binding:

```text
You: /current
Gateway: Workspace: default
IM: feishu
Chat: oc_xxx
Session: 2026-04-14-abc123
State: idle
```

Start fresh:

```text
You: /new
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
- error or blocked state
- tool failure
- long-running reminders every 5 minutes while a turn is still busy

Long-running reminders intentionally do not expose low-level phases such as `connecting`. Any user-visible output resets the next 5-minute reminder window. When internal progress events were observed in the current reminder window, the reminder includes a compact count, for example:

```text
⏳ Still working (4 internal events)
```

The internal-event count currently comes from gateway-tracked progress events such as `tool_result` and `todos`; it is reset after each user-visible output or reminder. If `event_visibility.todos` is enabled, every `todos` event is sent as the full current todo list without deduplication.

Optional lower-level events are controlled by `event_visibility`. See [event-visibility.md](./event-visibility.md).
