# Permissions & Safety

All IM platforms use one paired owner and explicit chat bindings, including single-workspace deployments. Run `chord-gateway --pair`, then send `/bind "<directory>" <pairing_code>` in a WeChat/Feishu/WeCom/DingTalk private bot chat. See [pairing and local reset](./pairing.md) and the [upgrade notes](../CHANGELOG.md). Slack uses `/chord bind "<directory>" <pairing_code>` in the App DM; see the [Slack guide](./slack.md).

`chord-gateway` is a remote control plane for a local `chord headless` process. Anyone who can send accepted IM messages to the gateway may influence what Chord does in the configured workspace.

## Security boundary

The gateway is not a multi-tenant sandbox.

Treat an allowed IM sender as someone who can interact with Chord in the configured workspace. Depending on Chord configuration and permissions, that may include reading files, editing files, running tools, or requesting model actions.

Workspace files and the Chord process stay on the gateway host, but chat messages and replies still pass through the selected IM platform. Data sent to model providers is governed by the Chord configuration.

Session takeover uses the same accepted IM sender and workspace routing rules. There is no separate takeover permission switch or source-user approval prompt. It is limited to the same OS user and canonical workspace on macOS/Linux, between TUI and gateway-managed headless. Local endpoint identity checks prevent accidental cross-runtime routing; they do not sandbox a malicious process running under the same OS user.

## Recommended deployment practices

- Use a dedicated machine, user account, container, or VM for gateway deployments when possible.
- Point `workspaces.<id>.path` only at projects that should be controllable from IM.
- Do not point a workspace at your whole home directory.
- Keep Chord permissions and tool approvals conservative.
- Monitor logs for unexpected senders, chats, commands, or workspace routing.

## IM access control

WeChat, Feishu, WeCom, DingTalk, Discord, and Slack accept only the paired owner's input, and each chat requires an explicit binding. Register the owner through foreground `--pair` and a private bot chat. Ordinary startup never opens pairing. Running locally does not prevent external users from messaging the bot. The pairing code stays valid for the whole 10-minute window and is not consumed by a single bind, so it can pair several chats and platforms; keep the terminal and the code private.

The owner has local control equivalent to the gateway process, including existing shell commands; pairing is not a directory sandbox.

Automatic session pins are keyed by workspace and route, not by owner. One workspace binding is one conversation: re-binding the same chat, moving it to a different platform account, or re-pairing to a different owner all continue the previous session, so a second owner can read what the first owner left behind. Use `/new`, delete the pin, or `pairing reset` to isolate. Do not share a workspace binding between owners you would not share a terminal with. Bind only trusted groups because replies are visible to members. Account changes block the old identity; stop the service and reset locally before reauthorization. See [pairing and reset](./pairing.md).

Feishu card callbacks require verified app, user, chat, request and binding targets; plain text cannot impersonate a trusted card action. Accepted card parameters, form values and card tokens stay in memory and are not persisted in intake records. A gateway restart makes pending card controls unavailable; check `/status` and use a newly presented interaction. Retained input bodies and ingress backups remain sensitive local state.

## Credential handling

Treat these as sensitive:

- Discord `bot_token`
- Feishu `app_secret`
- WeCom `secret`
- DingTalk `client_secret`
- WeChat token files (`<state_dir>/wechat/token.json` by default, or `ims.wechat.token_path`)
- legacy WeChat cursor and deduplication files (no longer imported; they are inert and may be deleted)
- Gateway state directory
- Chord provider credentials and auth files

Do not commit secrets or state directories to version control. Keep the gateway state directory readable only by the gateway user when possible, because session pins and sync state may reveal chat/session identifiers even when they are not direct credentials.

Short-lived WeChat `context_token` credentials stay in memory, scoped to account and chat, and never enter ingress records, ordinary logs or diagnostics. Only the current owner or a verified initial pairing flow may prepare context; initial pairing context permits only its bounded flow reply, not ordinary background sends. Account replacement clears old contexts immediately, and old output cannot use the new account. Accepted bodies stay in memory and clear on terminal settlement; restart drops the queue.

Same-account polling is exclusive for one OS user on one host even with separate configurations or state directories. The deployment operator must ensure exclusivity across users or hosts. Login renewal does not revoke accepted inputs; account replacement or local reset isolates old inputs under authorization rules.

## Multi-workspace safety

For Feishu multi-workspace mode, configure `ims.feishu.chat_bindings` so each Feishu chat ID maps to the intended workspace.

WeChat supports private chat only. Each chat binds to one directory at a time and can switch with `/bind`. Use Feishu, WeCom, DingTalk, or Discord to control separate directories from different chats. Discord threads require individual bindings and existing access; grant only View Channel, Send Messages, and Send Messages in Threads. Output mentions are disabled. Channels and threads expose replies to members.

## DingTalk identity boundary

A same-enterprise private callback validates configured enterprise/robot identity before pairing, but grants no owner rights; only the terminal code can pair an owner. Staff IDs come from authenticated enterprise callbacks, and a group mention grants no additional authority. Unbound chats cannot request arbitrary proactive sends; only an authorized binding probe can target a candidate chat. Reset revokes identity evidence. Keep the application secret and ingress database private. See [DingTalk](./dingtalk.md).

## Incident response

If you suspect unauthorized access:

1. Stop the gateway.
2. Revoke or rotate affected Feishu app secrets, WeCom bot secrets, DingTalk client secrets, and Discord bot tokens.
3. Remove or rotate WeChat token files (`<state_dir>/wechat/token.json` by default, or `ims.wechat.token_path`).
4. Review gateway logs and Chord session history.
5. Review changes in configured workspaces.
6. Reset locally, pair a trusted owner again, and verify chat bindings before restarting the service.

To report vulnerabilities in the project, follow [SECURITY.md](../SECURITY.md).

All platforms share bounded in-memory intake, authorization and delivery receipts. Gateway does not restore its inbox, deduplication or polling cursors after restart. Check the original Chord session before resending an uncertain task. See [delivery semantics](./operations.md#message-delivery-and-restarts).

## WeCom security boundary

Treat the WeCom `secret` as a credential. The temporary pre-binding send permit only sends the fixed `Gateway binding check.` to the verified candidate; it cannot execute tasks or enable ordinary unbound output. Only owner input executes in groups, where replies are visible to all members. Model output is rendered as safe text without generated notifications, links, images, or cards. Rotating the same bot secret preserves the owner; a changed bot ID requires stopping the service and running `chord-gateway -f config.yaml pairing reset --platform wecom` in a local controlling terminal, with `wecom` typed to confirm.

## Slack

Treat both Slack app and Bot tokens as secrets. Grant only the scopes in the [Slack guide](./slack.md); file access and full channel history are not required. Only owner input is accepted, and channel/thread text requires an explicit Bot mention. Channel replies are visible to members. Shared channels, Slack Connect and multi-person DMs are unsupported. An ACK is not evidence of successful execution.
