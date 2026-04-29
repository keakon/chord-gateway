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

- `workspaces[].path` does not exist.
- WeChat is enabled with multiple workspaces but `ims[].wechat.workspace_id` is not set.
- Feishu has multiple workspaces but `ims[].feishu.chat_bindings` is missing or incomplete.
- A Feishu `chat_bindings` entry points to the wrong workspace ID.

## WeChat issues

### No QR login appears

Check:

- `ims` includes a WeChat entry.
- `base_url` is correct.
- Existing token files are not stale or corrupt. The default path is `<state_dir>/wechat/token.json`; if `ims[].wechat.token_path` is configured, check that custom path instead.
- Gateway logs under the state directory.

To force a new login, stop the gateway and remove the saved WeChat token file (`<state_dir>/wechat/token.json` by default, or `ims[].wechat.token_path` if configured).

### Token expired

In multi-IM mode, send this from another active channel:

```text
/login weixin
```

Then open the returned login link and scan the QR code. When the gateway itself detects an expired WeChat token, it clears the saved token and starts QR login automatically.

## Feishu issues

### Feishu events do not reach the gateway

Check:

- The configured `listen` address is reachable from Feishu.
- The public URL path matches `webhook_path`.
- HTTPS/reverse proxy forwarding is correct.
- Firewall or cloud security groups allow inbound traffic.
- Feishu event subscription is enabled for the app.
- The subscribed event types include `im.message.receive_v1` and `card.action.trigger`.
- The callback URL has passed Feishu URL verification.

### Verification fails

Check that `verification_token` matches the Feishu developer console. If event encryption is enabled, also check `encrypt_key`.

If URL verification fails immediately after saving the callback URL, check:

- the callback path exactly matches `webhook_path`
- the public endpoint is reachable from Feishu
- the configured `verification_token` matches the challenge request token

### Messages are ignored

Check:

- the sender is allowed by `owner_open_id` / `allowed_open_ids` when an allowlist is configured
- the inbound Feishu message is plain text; non-text messages are ignored
- the target chat is routed to the expected workspace

You can obtain the sender `open_id` from the Feishu developer console or event payload and add it to the allowlist.

### Duplicate messages

Feishu may retry callbacks. The gateway deduplicates by `app_id + chat_id + message_id` and stores data in `<state_dir>/dedupe.json`.

## Routing issues

### Wrong workspace receives messages

For Feishu multi-workspace mode, confirm `ims[].feishu.chat_bindings` maps each Feishu chat ID to the intended workspace ID.

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

Logs, tokens, dedupe data, and session pins are stored there. WeChat token state defaults to `<state_dir>/wechat/token.json`; legacy `<state_dir>/token.json` is migrated automatically.

## Still stuck

When reporting an issue, include:

- gateway version or commit
- OS and architecture
- Go version
- Chord version or commit
- sanitized config
- relevant gateway logs
- the IM platform and mode you are using
