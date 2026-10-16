# Owner pairing and chat bindings

Register one trusted owner per platform and explicitly bind each chat to an existing local directory. Existing deployments should read the [breaking changes](../CHANGELOG.md) before upgrading.

Slack initial pairing uses `/chord bind "~/workspace" <pairing_code>` in a verified App DM. Later root bindings use `/chord bind ...`; a thread requires `@Bot /bind ...` inside that thread and never inherits its root binding. See the [Slack guide](./slack.md).

## First connection

Configure the adapters you need, without an owner or workspace:

```yaml
ims:
  wechat: {}
  # feishu:
  #   app_id: cli_example
  #   app_secret: your-app-secret
  # wecom:
  #   bot_id: your-bot-id
  #   secret: your-bot-secret
  # discord:
  #   bot_token: your-bot-token
  #   application_id: "123456789012345678"
```

Start in a foreground controlling terminal:

```sh
chord-gateway --config /path/to/config.yaml --pair
```

Complete WeChat QR login if prompted. With `--pair`, the terminal immediately displays a single 10-minute pairing code that works on every active platform, and `/bind` stays valid for the whole window, so several users and chats can be paired before it expires. DingTalk shows the same startup code, but its bindings still require the same-enterprise identity probe before they authorize. In a **private chat** with that bot, send the displayed command, replacing the directory with an existing directory on the gateway host:

```text
/bind "~/workspace" <pairing_code>
```

For WeCom, a fixed proactive binding probe must receive a successful platform ACK first. The gateway then saves the owner, registers the directory, and binds this chat in YAML. The next task starts Chord in that directory. The code is not written to YAML or logs. Keep the terminal and IM history private; they can contain the code.

Configuration writes preserve effective values inherited through YAML aliases and merge keys, including top-level merges. `/bind` and local reset use the same case-insensitive, whitespace-trimmed platform names as configuration loading. Writeback may expand aliases and merges into explicit mappings.

Restart normally afterward, including as a background service. Ordinary startup never opens pairing. `--pair` does not replace an owner.

A temporary connection loss does not revoke already accepted owner inputs or replies for the same account and directory binding. Delivery still depends on the platform transport; unknown outcomes are not automatically retried. Replacing a Chord process rejects its queued output and cancels sends still in progress. Messages already accepted by the IM platform cannot be recalled. Account changes and directory switches invalidate the old authorization.

DingTalk first waits for a same-enterprise employee private `hello` before authorizing bindings. The probe never runs tasks or registers an owner. An authorized `/bind` sends a fixed proactive probe before saving; rejection or an unknown outcome leaves the binding unsaved. See [DingTalk](./dingtalk.md).

## Later bindings

The owner can bind another chat, including a Feishu, WeCom, or DingTalk group (@robot is required for DingTalk), or a Discord channel/thread, without a code:

```text
/bind "/path/to/another workspace"
```

In Discord server channels and threads, send `@Bot /bind "/path/to/another workspace"`; each thread needs its own binding. First pairing always happens in a bot DM.

Other senders cannot issue commands. Replies in a group are visible to its members. WeChat retains its existing personal-chat identifier; this feature does not add group support.

An unbound chat receives guidance and does not execute or replay the input. Existing directories are reused by their real filesystem identity; new directories are registered but never created or cloned. If several workspace IDs name the same directory, resolve that ambiguity locally first. An inaccessible unrelated workspace does not prevent a new binding.

Use absolute paths or `~/`. Double quotes support spaces; inside them, `\"` and `\\` escape quotes and backslashes. Environment variables and command substitutions are not evaluated. Relative paths, bare `~`, and `~other-user` are not accepted.

Binding the same directory is an idempotent operation, including during active work. Repeated or rejected binding commands do not interrupt replies from an existing task in WeChat. To switch directories, finish outstanding work and interactions first. `/cancel` requests cancellation; wait for actual completion. Background jobs, unconfirmed inputs, and session transfers can still block a switch. Before switching an idle restored session, the gateway refreshes its pending-input summary; a failed refresh leaves the old binding intact. An uncertain shutdown pauses the binding until local inspection and restart. A failed configuration write never authorizes a new owner; an uncertain replacement shuts down the gateway and requires checking the file locally.

If a directory switch completes while `/takeover` is still checking the source, that pending request is discarded. Once a transfer has been recorded, resolve it before switching directories. A directory switch that has already begun rejects a new transfer out of the old process, including while waiting for old replies to finish.

## Configuration and routing

`owner` contains `account_id` and `user_id`; all supported platforms use `chat_bindings` to map chat IDs to workspace IDs. Before first pairing, omit the owner, workspaces, and bindings. Empty workspaces and bindings may be omitted or written as `{}` or `null`. An unpaired platform only accepts valid pairing commands during a foreground `--pair` window. A private sender without a valid pairing command receives a bounded hint instead of silence — how to pair, what a malformed `/bind` is missing, or that a code was wrong; when pairing is inactive the hint says to restart with `--pair`. Group chats, internal actions, and duplicate redeliveries stay silent, and hints draw on a separate rate-limit budget from pairing attempts. Unbound chats cannot execute tasks or fall back to the only workspace.

Old authorization fields and mode switches have been removed without automatic migration; see the [upgrade notes](../CHANGELOG.md). Local stdin/stdout uses explicit `--console`, requires exactly one workspace, connects to no IM platform, and cannot be combined with `--pair`.

## Local reset and recovery

Stop the gateway, then run:

```sh
chord-gateway pairing reset --platform feishu --config /path/to/config.yaml
chord-gateway pairing reset --platform wechat --config /path/to/config.yaml
chord-gateway pairing reset --platform wecom --config /path/to/config.yaml
chord-gateway pairing reset --platform dingtalk --config /path/to/config.yaml
chord-gateway pairing reset --platform discord --config /path/to/config.yaml
chord-gateway pairing reset --platform slack --config /path/to/config.yaml
chord-gateway pairing reset --platform telegram --config /path/to/config.yaml
```

Use the command for the intended platform only. Type the platform name at the controlling terminal to confirm. There is no remote reset or noninteractive `--yes`.

Reset clears that platform's owner, chat bindings, and automatic session pins. It keeps workspaces, Chord session files, and other platforms unchanged. Outstanding transfer guards or unreadable pin/transfer state block reset. If pin cleanup succeeds but configuration replacement fails, the old owner remains but automatic resume pins may already be gone. Retry after correcting the local problem. An uncertain replacement is never reported as a successful reset.

DingTalk reset also revokes local enterprise identity evidence; the next connection needs a private `hello`. Matching ordinary restarts reuse the evidence after a new authenticated handshake.

Start with `--pair` to register again. Even the same owner starts without the old automatic pins. This does not isolate local historical files: the owner is a trusted local control-plane user, with access equivalent to the gateway process, including existing shell commands.

A bot account change does not grant access to the new account. Old paired activity is cancelled and blocked; inspect the deployment and reset locally. Changing owner fields manually is not a substitute for reset.

Stop the service before editing identity or routing. Runtime transactions reject changed file bytes rather than merging external edits. Resource locks cover configuration, state, pin storage, and custom WeChat token storage. They use the OS user's cache directory and do not depend on `CHORD_GATEWAY_STATE_DIR`. Do not run an older gateway binary concurrently: it does not participate in this lock protocol. Configuration changes require the filesystem's locking and durable replacement support.

Old binding output cannot be newly authorized for a replacement binding. Messages already accepted by an IM platform cannot be withdrawn and may arrive later. Login notices contain connection status only and paired recipients come from explicit current chat bindings.

If a switch is blocked by queued inputs, storage failure or an uncertain result, use `/status` for the reason and inspect the original session on the host. Cancelling active work alone does not resolve these states. Idle cleanup and process replacement preserve the same input guards.

All platforms share bounded in-memory intake, authorization and delivery receipts. Gateway does not restore its inbox, deduplication or polling cursors after restart. Check the original Chord session before resending an uncertain task. See [delivery semantics](./operations.md#message-delivery-and-restarts).
