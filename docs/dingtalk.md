# DingTalk

Connect an enterprise internal-app robot to Chord through DingTalk Stream and proactive OpenAPI text messages. The first version supports one enterprise and one robot, employee private chats, and group text that @mentions the robot. The paired owner can use existing commands, permission confirmations, Questions, and background results through text.

The adapter implementation is available for deployment testing. Real-enterprise acceptance has not been performed. Protocol tests do not establish production readiness. WeChat and Feishu also use shared in-memory intake.

- Chinese version: [dingtalk_CN.md](./dingtalk_CN.md)
- Common owner, binding, and reset rules: [pairing](./pairing.md)

## Create and publish the application

1. Create an enterprise internal application in the [DingTalk developer console](https://open-dev.dingtalk.com/), add robot capability, and select **Stream** receive mode. Include the intended owner in the application's available scope.
2. Grant the enterprise robot permissions listed on the official [private proactive sending](https://open.dingtalk.com/document/orgapp/chatbots-send-one-on-one-chat-messages-in-batches) and [group proactive sending](https://open.dingtalk.com/document/orgapp/the-robot-sends-a-group-message) API pages. Publish the application version with the intended employees and groups in scope. Stream credentials alone do not grant proactive sending permission.
3. Record the application's AppKey/AppSecret as `client_id`/`client_secret`, its robot code as `robot_code`, and the enterprise ID as `corp_id`. All four fields are required. Copy the robot code from its configuration; do not assume it equals the client ID.

```yaml
ims:
  dingtalk:
    client_id: your-client-id
    client_secret: your-client-secret
    robot_code: your-robot-code
    corp_id: your-corp-id
```

No inbound public port or webhook is required. The gateway needs outbound HTTPS and secure WebSocket access to DingTalk. Keep configuration credentials private and run only one active receiver for this application, including across hosts.

## Verify identity, then pair

Start in a foreground terminal:

```sh
chord-gateway --config /path/to/config.yaml --pair
```

The first connection waits for an employee private-chat identity probe. Send `hello` privately to the robot from the configured enterprise. This message verifies the sender's enterprise and staff identity and the robot's enterprise/code; it does not run a task or register an owner; it is independent of the startup pairing code. A group message cannot complete this check. Configuration identity is never inferred from the first callback.

The startup pairing code is valid for `/bind` on DingTalk too, but a DingTalk binding is authorized only after this identity check. In a private chat, send:

```text
/bind "/path/to/project" <pairing_code>
```

Use an existing directory on the gateway host. Before saving the owner and binding, Gateway sends the fixed proactive message `Gateway binding check.` to that employee. The bind succeeds only when DingTalk explicitly accepts the probe. A permission error, unreachable employee, or unknown send outcome leaves the binding unsaved; correct the deployment and issue a new `/bind`. A successful probe shows platform acceptance, not proof that the employee read it.

The paired owner can add the robot to a trusted enterprise group and send `@robot /bind "/path/to/project"`. The group receives the same proactive probe before the binding is saved. Subsequent owner group tasks and commands must @mention the robot. Other group members cannot control Chord, but they can see replies. An identical existing binding does not send another probe.

After binding, send `/status`, a text task, `/allow` or `/deny` for a pending permission request, and `/answer @<id> <text>` for a Question. Ordinary restarts need no `--pair`.

In group examples, `@robot` means selecting your actual robot from DingTalk’s mention menu, not typing a fixed name. The [official Stream example](https://open-dingtalk.github.io/developerpedia/docs/learn/stream/protocol/) represents the mention through `isInAtList` and `atUsers`, with the command text already free of the robot prefix. Gateway removes leading whitespace and preserves literal mentions in task text.

## State, sending, and recovery

Identity verification stays in memory. After each process start, the owner must send a private `hello` to verify the enterprise identity before ordinary intake and sending open. Reconnects within that process reuse the verified identity after a fresh handshake. Existing owners and bindings remain configured without pairing again; editing configuration cannot grant a new owner.

A temporary Stream outage pauses new input admission and pairing. Once this process has activated the account, accepted tasks and proactive HTTP replies continue under their existing account, binding, and runtime authorization. Shutdown or authorization revocation still stops that work. Restart requires a new private identity probe and account activation.

Stop the service before a local reset:

```sh
chord-gateway pairing reset --platform dingtalk --config /path/to/config.yaml
```

Confirm `dingtalk` in the terminal. Reset clears owner/bindings/pins according to the [shared reset rules](./pairing.md#local-reset-and-recovery). Start with `--pair` and send `hello` again.

Replies use proactive `sampleText`, including background results; Gateway never stores or uses `sessionWebhook`. Group conversation IDs come directly from authenticated callbacks. Access tokens are kept in memory, refreshed ahead of expiry, and shared by concurrent sends. Credentials and tokens are never forwarded through HTTP redirects.

Text is split at Unicode boundaries into segments of at most 1024 UTF-8 bytes, with at least 3 seconds between sends to one target and at most 2 sends per second per account. These are project budgets, not official DingTalk hard limits. An explicit HTTP 429 may retry up to 3 times within the original send deadline. Network errors and 5xx responses have unknown outcomes and are not resent. A partially sent output is not restarted from its first segment. When direct access is blocked, set `proxy_url` globally or under `ims.dingtalk` (`http`, `socks5`/`socks5h`; no `https` for Stream sockets). See [configuration](./configuration.md#proxy).

Robot callbacks are fire-and-forget: a 500 response or missing acknowledgment does **not** guarantee redelivery. An acknowledgment confirms gateway acceptance or rejection, not Chord execution. Do not resend uncertain commands automatically; inspect `/status` and [local ingress state](./operations.md#message-delivery-and-restarts). See the [official Stream protocol](https://open-dingtalk.github.io/developerpedia/docs/learn/stream/protocol/).

Connection failures report a sanitized category, retry attempt, and next delay. Retry logs for the same continuing failure are limited to once per minute; successful connections are also logged. Send rate-wait metrics include local account/target pacing and HTTP 429 backoff, and record elapsed time when a wait is canceled.

## Limits and troubleshooting

External contacts, cross-enterprise use, custom group webhooks, EVENT subscriptions, HTTP callbacks, cards, images, audio, and rich-text input are outside the first version. Permission and Question interactions use text commands. A group's robot mention only enables message delivery; it does not grant owner rights.

| Category | Action |
| --- | --- |
| `auth_failed` | Check AppKey/AppSecret and application publication. Stream registration authentication failures stop reconnects; a proactive API 401 clears the token and fails this send without a retry, while a later new send obtains a token again. |
| `tenant_mismatch` | Correct `corp_id`/`robot_code`; identity mismatches stop reconnect attempts. |
| `sender_identity_unsupported` | Use a same-enterprise employee private chat with a staff ID. |
| `proactive_send_denied` | Grant proactive API permissions, update available scope, and publish again before a new `/bind`. |
| `target_unreachable` | Check employee availability and group robot membership before a new `/bind`. |
| `rate_limited` | Wait for platform limits to recover; Gateway's retry budget is bounded. |
| `token_unavailable` | Token acquisition failed before sending the message. Check connectivity and API availability; this does not imply an uncertain message delivery. |
| `unknown_send` | Inspect chat and local state; do not automatically resend the original message. |

## Deployment acceptance checklist

Real-account testing remains to be completed: publish the application; verify `hello`; pair privately and observe the proactive probe; bind a group and observe its probe; exercise `/status`, tasks, confirmations, Questions, and cancellation. Keep a background task running beyond the initial callback's webhook expiration plus one minute and verify proactive delivery. Restart, send a fresh private identity probe and check the existing bindings; then reset and verify a new private probe is required. Record sanitized error categories and results, without callback bodies, webhook URLs, credentials, or tokens.
