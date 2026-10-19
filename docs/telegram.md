# Telegram integration

Telegram connects to the local Gateway through Bot API long polling, with no public callback address. The adapter supports plain text tasks and commands in private chats, groups, and supergroups. Every chat requires an explicit binding by the paired owner. Real-account deployment acceptance remains outstanding; complete the checks below  before release.

## Create a bot and pair

1. Create a bot in Telegram's BotFather, save its token, and note its username. Keep group privacy enabled initially; Gateway does not require bot administrator rights.
2. Save this minimal configuration, omitting `owner` and `chat_bindings` on first setup:

   ```yaml
   ims:
     telegram:
       bot_token: your-bot-token
   ```

3. Run in a foreground terminal:

   ```bash
   chord-gateway --config /path/to/config.yaml --pair
   ```

4. After Gateway verifies the bot, checks its webhook, and probes the connection, the terminal displays a pairing code valid for 10 minutes. Send this in a **private chat** with the bot:

   ```text
   /bind "/path/to/project" <pairing_code>
   ```

   The directory must already exist on the Gateway host. Send `/status`, then a short text task. Gateway saves the owner and binding to YAML; later starts do not need `--pair`.

The credential remains in the configuration file; do not commit it. The API address is fixed to `https://api.telegram.org`, with no configurable endpoint. Gateway must have network access to that address. When direct access is blocked, set `proxy_url` globally or under `ims.telegram`; empty means direct and environment proxy variables are ignored. See [configuration](./configuration.md#proxy).

## Groups and privacy

Pair privately first, then add the bot to a trusted group. The owner sends:

```text
/bind@YourBotUsername "/path/to/project"
```

Replace `YourBotUsername` with the current bot username. With default privacy, use commands with this explicit suffix and **reply to a bot message** to send ordinary tasks. If commands arrive but ordinary group messages do not, privacy is usually working as intended. Gateway cannot process messages Telegram does not deliver.

If you need all group text delivered, disable privacy in BotFather and re-add the bot as required by the Telegram client. Gateway still only accepts the paired owner's input. Gateway does not require a reply: any owner plain text Telegram actually delivers to a bound chat is admitted under the common authorization rules. Replies are visible to group members, so bind trusted groups only.

Group commands such as `/status@YourBotUsername`, `/cancel@YourBotUsername`, `/allow@YourBotUsername`, and `/answer@YourBotUsername @<id> <text>` have the same meaning as private commands. Generated command hints include the current bot suffix. Commands addressed to other bots are ignored; paths, `@`, and slashes in ordinary tasks are preserved. See the [usage guide](./usage.md) for all commands and session recovery.

Telegram uses `/handoff_deny`, `/question_interact`, `/question_withdraw`, and `/question_replace` for commands whose shared names contain a hyphen. Add `@YourBotUsername` in groups, for example `/handoff_deny@YourBotUsername <reason>`. Generated hints use these names in both private chats and groups. Legacy hyphenated forms with a bot suffix, such as `/handoff-deny@YourBotUsername`, are rejected whether Telegram marks only the prefix or the whole command as the command entity.

Confirmations, answers, and cancellation keep the target observed when Gateway receives the batch, including during admission retries. If that target changes before execution, the old operation is refused; inspect `/status` before sending a new command. `/status` also shows the latest partial output failure and the number of accepted segments. Do not resend the task just to recover missing output; inspect the original Chord session.

Output is plain text, split automatically for long messages; code blocks are not rendered as Markdown. Confirmations and Questions use text commands rather than buttons. Long tasks use the common [progress and reminders](./event-visibility.md); query `/status` or use `/cancel`.

## Identity, restart, and retention

The numeric Bot ID returned by `getMe` identifies the account; username changes do not alter its owner or bindings. Rotating the same bot's token keeps its configured owner and bindings. Switching to a different bot blocks the old owner; inspect the deployment, reset locally, and pair again:

```bash
chord-gateway pairing reset --platform telegram --config /path/to/config.yaml
```

Stop the service first. Reset requires typing the platform name at the controlling terminal. See [pairing and local reset](./pairing.md). The account lock excludes other instances for the same bot under the same local OS user; deployments across hosts must also keep only one poller active.

Messages, deduplication and pending offsets stay in memory. Offsets advance only after each item in the current batch has a definite intake result. Restart does not restore local progress: input acknowledged before reaching Chord may be lost, and messages may be redelivered. See [delivery semantics](./operations.md#message-delivery-and-restarts).

Only ordinary text tasks retain a body digest for conflict detection. Control commands and unsupported inputs retain identity-based deduplication evidence without parameter hashes, including pairing codes and answers; duplicate controls never execute again. Parameter-only differences in an otherwise identical control identity are not compared within this process.

Network failures retry with exponential backoff and jitter. Brief successes do not reset the delay; a minute of successful requests does. An explicit Telegram `retry_after` takes precedence and remains cancellable. A failing request warns on its first failure and then at most once a minute, naming the method, the error category and the delay; the token and the request URL never appear. A 409 right after a local restart retries briefly (the previous server-side long poll survives up to its 25s timeout); a persistent 409 means a true second poller and stays fatal. A transport failure and a 409 are counted separately, so flaky upstream never exhausts the conflict window. A method-level rejection from `getMe`, `getWebhookInfo` or `getUpdates` — an `ok:false` answer that is neither a 429 nor a credential, conflict or migration answer — is fatal instead of retried, because it never resolves by itself; a proxy that answers HTTP 200 with its own error body produces exactly this.

Telegram retains unreceived updates for at most **24 hours**; longer outages can lose messages. After at least a week without updates, new update IDs may be randomly reset. Gateway acknowledges current batches rather than relying on a permanent maximum ID. See the official [getUpdates](https://core.telegram.org/bots/api#getupdates) and [Update](https://core.telegram.org/bots/api#update) documentation.

## Troubleshooting

| Symptom or classification | Action |
| --- | --- |
| `getMe` 401 | Verify or regenerate the BotFather token, update configuration, and restart |
| `webhook_conflict` | This bot has a webhook. Stop its previous service and explicitly clear the webhook before starting; Gateway never deletes it automatically or drops pending updates |
| `poller_conflict` / 409 | Stop other polling processes for the bot, including instances on other hosts. A single 409 immediately after a local restart is the stale server-side long poll and retries on its own; only a persistent 409 needs action |
| Group text absent but commands arrive | Check privacy; reply to the bot to send tasks in default mode |
| 403 | Check whether the bot is blocked, removed from the group, or unable to access the target |
| `chat_migrated` | Have the owner explicitly bind the new supergroup; old group authorization is not copied |
| `unknown_send` | Delivery may have occurred; inspect the chat first. Gateway does not automatically resend or rerun the task |
| Bot receives nothing | Run at debug log level. `telegram: received updates` proves Telegram delivered a batch; `telegram: captured update ... reason=` explains how each update was normalized; `telegram: dispatch update ... disposition=` shows where it stopped; `telegram: <method> request failed ... retrying in ...` warns on the first failure and then at most once a minute. An update that never reaches a terminal disposition blocks later updates in order by design and warns once, then at most once a minute with its `update_id`, plus an `acceptance_unconfirmed` metric. No `received updates` line at all means polling never fetched, so compare `getWebhookInfo.pending_update_count` and `getUpdates` with offset `-1` against the running process |
| `method_rejected` | `getMe`, `getWebhookInfo` or `getUpdates` answered `ok:false` with something that is not a 429, credential, conflict or migration answer. Check the proxy and the fixed `https://api.telegram.org` endpoint: a stale proxy answering HTTP 200 with its own error body looks exactly like this and is fatal rather than retried |

## Official references

- [Bots FAQ — broadcasting to users](https://core.telegram.org/bots/faq#my-bot-is-hitting-limits-how-do-i-avoid-this) — avoid more than **one message per second in a single chat**, no more than **20 messages per minute in a group**, and about **30 messages per second** for bulk delivery; exceeding them returns `429`. These are platform limits, not Gateway budgets.
- [sendMessage](https://core.telegram.org/bots/api#sendmessage) — a single text message is limited to 4096 characters after entity parsing, which is the constraint behind Gateway's automatic splitting.
- [getWebhookInfo](https://core.telegram.org/bots/api#getwebhookinfo) — used in the troubleshooting table to compare `pending_update_count`.

## Deployment acceptance

Real-account acceptance has not been performed. Record private pairing, `/status`, a short task, group binding with the suffix, a task sent by replying to the bot, confirmation/Question/cancellation, a task exceeding five minutes, and status in the same binding after restarting Gateway. Record delivery of ordinary messages, explicit commands, and replies to the bot separately with group privacy enabled; default-mode acceptance does not require disabling privacy.

Forum topics, inline keyboards, media input/output, and edit previews are deferred. Topic messages never fall back to the group root binding. Media captions are not text tasks: a photo, document, video, audio, voice, animation, sticker, video note, contact, location, venue, poll, dice, game, invoice, successful payment, story, passport data or web app data update is admitted as unsupported content, even when its caption reads like a task. Channel posts, edited messages, bot senders, anonymous administrators, and `sender_chat` messages are not admitted as tasks. These capabilities require separate feature work.
