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
| `/deny` | Deny the pending confirmation |
| `/answer <text>` | Answer a pending question; numeric shortcuts are supported |
| `/todos` | Show the current todo list |
| `/new` | Clear the current session pin and start a fresh session |
| `/resume <id>` | Resume and pin a specific session |
| `/sessions` | List recent sessions from the workspace |
| `/current` | Show the current binding status, including workspace, IM/chat binding, active session, and pending interaction |
| `/login [platform]` | Start a login flow, for example `/login weixin` |
| any other text | Send the text to Chord, or answer a pending question if one exists |

Notes:

- `/login weixin` is the documented command name for WeChat login renewal.
- Unrecognized slash commands are forwarded to Chord as normal text.
- `/summary` is not part of the documented gateway command set.

## Question interaction

When Chord sends a `question_request`, the gateway sends a numbered question to the IM chat:

```text
❓ Continue?
  1. yes — Yes, proceed
  2. no — No, stop
Reply /answer 1 / 1,2 / or type your answer
```

You can answer in two ways:

- Use `/answer`, for example `/answer 1` or `/answer 1,3` for a multi-select question.
- Send a plain text message while a question is pending. The gateway treats it as a free-text answer.

Invalid numeric shortcuts are sent as custom text instead of being silently accepted.

## Confirmation interaction

When Chord asks for permission, use:

- `/allow` to approve
- `/deny` to reject

Use `/status` if you are unsure whether a confirmation is pending.

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

In multi-IM mode, the gateway can notify another active IM channel when one platform expires.

Example: if WeChat login expires, send this from Feishu:

```text
/login weixin
```

The gateway replies with a WeChat QR login link. After scanning, it updates the token without requiring a gateway restart.

If Feishu becomes invalid, the gateway cannot renew it through `/login`; update the Feishu configuration instead.

## Notifications

The gateway pushes important control-plane notifications to active IM channels, including:

- confirm required
- question required
- task started
- task completed
- error or blocked state
- tool failure
- long-running phase alert

Optional lower-level events are controlled by `event_visibility`. See [event-visibility.md](./event-visibility.md).
