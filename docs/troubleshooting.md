# Troubleshooting

This page covers common `chord-gateway` startup, IM integration, routing, and runtime issues.

## Startup failures

### `load config` errors

Check that the intended config file is being used. Resolution order:

1. `--config` / `-f`
2. `$CHORD_GATEWAY_CONFIG`
3. `$XDG_CONFIG_HOME/chord-gateway/config.yaml`
4. `~/.config/chord-gateway/config.yaml`

Run with an explicit config while debugging:

```bash
chord-gateway -f ./config.yaml
```

### `chord` is not found

The gateway executes `chord_path` directly. Shell aliases are not expanded.

Use one of these approaches:

- Put the Chord binary directory in `PATH`.
- Set an absolute path in `chord_path`.
- Use a `~` path supported by the gateway.

Example:

```yaml
chord_path: ~/go/bin/chord
```

### Workspace validation fails

Common causes:

- `workspaces.<id>.path` does not exist.
- WeChat is enabled with multiple workspaces but `ims.wechat.workspace_id` is not set.
- Feishu has multiple workspaces but `ims.feishu.chat_bindings` is missing or incomplete.
- A Feishu `chat_bindings` entry points to the wrong workspace ID.

## WeChat issues

### No QR login appears

Check:

- `ims` includes a WeChat entry.
- `base_url` is correct.
- Existing token files are not stale or corrupt. The default path is `<state_dir>/wechat/token.json`; if `ims.wechat.token_path` is configured, check that custom path instead.
- Gateway logs under the state directory.

To force a new login, stop the gateway and remove the saved WeChat token file (`<state_dir>/wechat/token.json` by default, or `ims.wechat.token_path` if configured).

### Token expired

When a WeChat token expires, in multi-IM mode send this from another active channel:

```text
/login wechat
```

Then open the returned login link and scan the QR code. When the gateway itself detects an expired WeChat token, it clears the saved token and starts QR login automatically.

Feishu does not need in-chat login or manual renewal; the gateway automatically obtains and refreshes Feishu access tokens from the configured app credentials. If the Feishu connection becomes invalid, check the deployment configuration and Feishu developer console for app credentials, permissions, event subscriptions, and long-connection settings. Do not send or change `app_id` / `app_secret` in an IM chat.

## Feishu issues

### Feishu long connection is not established or no events arrive

Check:

- the gateway process is running and stays online
- the Feishu app uses “Receive events via long connection” in the developer console
- the subscribed event types include `im.message.receive_v1`
- callback configuration includes `card.action.trigger` for interactive cards
- the Feishu app version has been published after changing capabilities, permissions, or event subscriptions
- the machine can make outbound connections to Feishu services

### Long connection configuration fails

Check that the Feishu app is configured to use “Receive events via long connection”, the gateway is already running, and `app_id` / `app_secret` are correct.

### Messages are ignored

Check:

- the sender is allowed by `owner_open_id` / `allowed_open_ids` when an allowlist is configured
- the inbound Feishu message is plain text; non-text messages are ignored
- the target chat is routed to the expected workspace

You can obtain the sender `open_id` from the Feishu developer console or event payload and add it to the allowlist.

### How to bind a Feishu group to a workspace

Recommended steps:

1. Start the gateway with a single workspace and no `chat_bindings`
2. Create the target Feishu group and add the bot to that group
3. Send a plain-text message in the group
4. In that same chat, run:

```text
/bind <workspace_id> <path>
```

5. The gateway updates only Feishu `chat_bindings` and `workspaces` in memory, then writes the same binding/workspace change back to the YAML config file.

`/bind` accepts exactly two arguments. The path must start with `/`, `~`, a Windows drive prefix such as `C:\\` or `C:/`, or a UNC prefix such as `\\\\server\\share`; after expansion it must be accessible and must be a directory. Extra arguments or an inaccessible path return an error and leave the config unchanged.

Example:

```text
/bind project-a ~/work/project-a
```

Manual YAML edits still work, but they require a gateway restart to take effect.

### How to find a Feishu group `chat_id`

Recommended steps:

1. Start the gateway with a single workspace and no `chat_bindings`
2. Create the target Feishu group and add the bot to that group
3. Send a plain-text message in the group
4. Look for a log line like:

```text
msg="feishu: received message" chat_id=oc_xxx open_id=ou_xxx message_id=om_xxx content=hello
```

Here, `chat_id=oc_xxx` is the real `chat_id` for that group.

If you have multiple workspaces and prefer to edit YAML manually, copy it back into:

```yaml
chat_bindings:
  oc_xxx: your-workspace-id
```

### Duplicate messages

Feishu may retry event deliveries. The gateway deduplicates by `app_id + chat_id + message_id` and stores data in `<state_dir>/dedupe.json`.

## Routing issues

### Wrong workspace receives messages

For Feishu multi-workspace mode, confirm `ims.feishu.chat_bindings` maps each Feishu chat ID to the intended workspace ID.

For a single Feishu workspace without `chat_bindings`, all chats use that workspace.

### Session context is not what you expected

Use:

```text
/current
/sessions
```

Then use `/new` to clear the current binding or `/resume <id>` to pin a known session.

## Runtime issues

### Chord does not respond

Check:

- Gateway logs for process start or stdout parsing errors.
- Chord logs and session state.
- Whether a confirmation or question is pending (`/status`).
- Whether the process was cancelled or timed out.

### Late `/answer`, `/allow`, or `/deny` is sent as follow-up context

A pending question or confirmation can expire when Chord becomes idle or when the gateway removes the idle process after `idle_timeout`. In that case, the original request ID is no longer pending.

Expected behavior:

- The gateway sends an expiry notice when it clears a pending question or confirmation.
- A late `/answer` is forwarded as a normal follow-up message, not as the original structured answer.
- A late `/allow` or `/deny` is forwarded only as follow-up context, not as an approval or denial.
- If the gateway still has the expired question or confirmation cached, it includes that context in the follow-up message sent to Chord.

If you need the original operation to proceed, ask Chord to retry or reissue the request so a fresh question or confirmation can be created.

### A task appears stuck

Use `/status` first. If the task should stop, send:

```text
/cancel
```

If the process remains stuck, restart the gateway and check for orphan Chord processes.

### Where are logs and state files?

State directory priority:

1. `$CHORD_GATEWAY_STATE_DIR`
2. `$XDG_STATE_HOME/chord-gateway`
3. `~/.local/state/chord-gateway`

Logs, tokens, dedupe data, and session pins are stored there. WeChat token state defaults to `<state_dir>/wechat/token.json`.

## Still stuck

When reporting an issue, include:

- gateway version or commit
- OS and architecture
- Go version
- Chord version or commit
- sanitized config
- relevant gateway logs
- the IM platform and mode you are using
