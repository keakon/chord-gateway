# Owner pairing and chat bindings

Register one trusted owner per platform and explicitly bind each chat to an existing local directory. Existing deployments should read the [breaking changes](../CHANGELOG.md) before upgrading.

## First connection

Configure one or both adapters, without an owner or workspace:

```yaml
ims:
  wechat: {}
  # feishu:
  #   app_id: cli_example
  #   app_secret: your-app-secret
```

Start in a foreground controlling terminal:

```sh
chord-gateway --config /path/to/config.yaml --pair
```

Complete WeChat QR login if prompted. Once a platform connects, the terminal displays its own 10-minute pairing code. In a **private chat** with that bot, send the displayed command, replacing the directory with an existing directory on the gateway host:

```text
/bind "~/workspace" <pairing_code>
```

The gateway saves the owner, registers the directory, and binds this chat in YAML. The next task starts Chord in that directory. The code is not written to YAML or logs. Keep the terminal and IM history private; they can contain the code.

Configuration writes preserve effective values inherited through YAML aliases and merge keys, including top-level merges. `/bind` and local reset use the same case-insensitive, whitespace-trimmed platform names as configuration loading. Writeback may expand aliases and merges into explicit mappings.

Restart normally afterward, including as a background service. Ordinary startup never opens pairing. `--pair` does not replace an owner.

A temporary connection loss does not revoke already accepted owner inputs or replies for the same account and directory binding. Delivery still depends on the platform HTTP API; unknown outcomes are not automatically retried. Replacing a Chord process rejects its queued output and cancels sends still in progress. Messages already accepted by the IM platform cannot be recalled. Account changes and directory switches invalidate the old authorization.

## Later bindings

The owner can bind another chat, including a Feishu group, without a code:

```text
/bind "/path/to/another workspace"
```

Other senders cannot issue commands. Replies in a group are visible to its members. WeChat retains its existing personal-chat identifier; this feature does not add group support.

An unbound chat receives guidance and does not execute or replay the input. Existing directories are reused by their real filesystem identity; new directories are registered but never created or cloned. If several workspace IDs name the same directory, resolve that ambiguity locally first. An inaccessible unrelated workspace does not prevent a new binding.

Use absolute paths or `~/`. Double quotes support spaces; inside them, `\"` and `\\` escape quotes and backslashes. Environment variables and command substitutions are not evaluated. Relative paths, bare `~`, and `~other-user` are not accepted.

Binding the same directory is an idempotent operation, including during active work. Repeated or rejected binding commands do not interrupt replies from an existing task in WeChat. To switch directories, finish outstanding work and interactions first. `/cancel` requests cancellation; wait for actual completion. Background jobs, unconfirmed inputs, and session transfers can still block a switch. Before switching an idle restored session, the gateway refreshes its pending-input summary; a failed refresh leaves the old binding intact. An uncertain shutdown pauses the binding until local inspection and restart. A failed configuration write never authorizes a new owner; an uncertain replacement shuts down the gateway and requires checking the file locally.

If a directory switch completes while `/takeover` is still checking the source, that pending request is discarded. Once a transfer has been recorded, resolve it before switching directories. A directory switch that has already begun rejects a new transfer out of the old process, including while waiting for old replies to finish.

## Configuration and routing

`owner` contains `account_id` and `user_id`; both platforms use `chat_bindings` to map chat IDs to workspace IDs. Before first pairing, omit the owner, workspaces, and bindings. Empty workspaces and bindings may be omitted or written as `{}` or `null`. An unpaired platform only accepts valid pairing commands during a foreground `--pair` window. Unbound chats cannot execute tasks or fall back to the only workspace.

Old authorization fields and mode switches have been removed without automatic migration; see the [upgrade notes](../CHANGELOG.md). Local stdin/stdout uses explicit `--console`, requires exactly one workspace, connects to no IM platform, and cannot be combined with `--pair`.

## Local reset and recovery

Stop the gateway, then run:

```sh
chord-gateway pairing reset --platform feishu --config /path/to/config.yaml
chord-gateway pairing reset --platform wechat --config /path/to/config.yaml
```

Use the command for the intended platform only. Type the platform name at the controlling terminal to confirm. There is no remote reset or noninteractive `--yes`.

Reset clears that platform's owner, chat bindings, and automatic session pins. It keeps workspaces, Chord session files, and the other platform unchanged. Outstanding transfer guards or unreadable pin/transfer state block reset. If pin cleanup succeeds but configuration replacement fails, the old owner remains but automatic resume pins may already be gone. Retry after correcting the local problem. An uncertain replacement is never reported as a successful reset.

Start with `--pair` to register again. Even the same owner starts without the old automatic pins. This does not isolate local historical files: the owner is a trusted local control-plane user, with access equivalent to the gateway process, including existing shell commands.

A bot account change does not grant access to the new account. Old paired activity is cancelled and blocked; inspect the deployment and reset locally. Changing owner fields manually is not a substitute for reset.

Stop the service before editing identity or routing. Runtime transactions reject changed file bytes rather than merging external edits. Resource locks cover configuration, state, pin storage, and custom WeChat token storage. They use the OS user's cache directory and do not depend on `CHORD_GATEWAY_STATE_DIR`. Do not run an older gateway binary concurrently: it does not participate in this lock protocol. Configuration changes require the filesystem's locking and durable replacement support.

Old binding output cannot be newly authorized for a replacement binding. Messages already accepted by an IM platform cannot be withdrawn and may arrive later. Login notices contain connection status only and paired recipients come from explicit current chat bindings.

If a switch is blocked by queued inputs, storage failure or an uncertain result, use `/status` for the reason and inspect the original session on the host. Cancelling active work alone does not resolve these states. Idle cleanup and process replacement preserve the same input guards.
