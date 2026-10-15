# 钉钉

企业内部应用机器人通过钉钉 Stream 接收消息，通过 OpenAPI 主动发送文本回复。首版支持单企业、单机器人、同企业员工私聊，以及群内 @机器人文本。配对后的 owner 可以使用现有命令、文本权限确认、Question 和后台任务结果。

适配器实现已可用于部署测试；尚未完成真实企业账号验收。协议测试通过不代表生产部署已验收。微信与飞书也共用内存收件。

- English: [dingtalk.md](./dingtalk.md)
- 通用 owner、绑定与重置规则：[配对与本机重置](./pairing_CN.md)

## 创建并发布应用

1. 在[钉钉开发者平台](https://open-dev.dingtalk.com/)创建企业内部应用，添加机器人能力，选择 **Stream** 接收模式，把预期 owner 纳入应用可用范围。
2. 申请官方[单聊主动发送](https://open.dingtalk.com/document/orgapp/chatbots-send-one-on-one-chat-messages-in-batches)和[群主动发送](https://open.dingtalk.com/document/orgapp/the-robot-sends-a-group-message)接口页列出的企业内机器人权限；发布应用版本，并确保目标员工和群在可用范围。只有 Stream 凭据不代表具有主动发送权限。
3. 将应用 AppKey/AppSecret 填入 `client_id`/`client_secret`，机器人编码填入 `robot_code`，企业 ID 填入 `corp_id`。四项必填；机器人编码从控制台配置复制，不假定它等于 client ID。

```yaml
ims:
  dingtalk:
    client_id: your-client-id
    client_secret: your-client-secret
    robot_code: your-robot-code
    corp_id: your-corp-id
```

无需公网入站端口或 webhook。Gateway 需要能向钉钉发起 HTTPS 与安全 WebSocket 连接。保护配置凭据；同一应用只能有一个活跃接收实例，跨主机也需保证单活。

## 先验证身份，再配对

在前台终端运行：

```sh
chord-gateway --config /path/to/config.yaml --pair
```

首次连接后，Gateway 等待同企业员工私聊验证。使用配置企业内的员工账号给机器人私聊发送 `hello`。这条消息只核对发送者的企业与员工身份、机器人所属企业和编码，不执行任务，也不登记 owner;它与启动配对码相互独立。群消息不能完成验证,首条回调不能覆盖配置身份。

钉钉同样可用启动配对码执行 `/bind`,但绑定只有在身份验证完成后才会被授权。随后在机器人私聊发送：

```text
/bind "/path/to/project" <pairing_code>
```

目录必须已存在于 Gateway 主机。保存 owner 与绑定前，Gateway 会主动向该员工发送固定文本 `Gateway binding check.`，只有平台明确接纳探测才完成绑定。权限错误、员工不可达或发送结果未知时不会保存绑定；修复部署后重新发送 `/bind`。探测成功表示平台接纳，不证明员工已读。

配对后的 owner 可以把机器人加入可信企业群，发送 `@机器人 /bind "/path/to/project"`。群主动探测同样通过后才保存绑定。后续 owner 群任务与命令需要 @机器人；其他群成员不能控制 Chord，但能看到回复。同一目标与目录的幂等绑定不重复探测。

绑定后可用 `/status` 查看状态，发送文本任务，用 `/allow` 或 `/deny` 处理权限请求，用 `/answer @<id> <text>` 回答 Question。后续正常启动无需 `--pair`。

群聊示例中的 `@robot`（@机器人）表示从钉钉的提及菜单选择实际机器人，不是输入固定名称。[官方 Stream 示例](https://open-dingtalk.github.io/developerpedia/docs/learn/stream/protocol/) 用 `isInAtList` 和 `atUsers` 表示提及，正文已不含机器人前缀。Gateway 去除开头空白，保留任务正文中的字面量提及。

## 状态、发送与恢复

身份验证结果只保存在当前进程内。每次启动后，owner 需先私聊发送 `hello` 完成同企业身份验证，再开放普通接纳和发送；同进程重连复用已验证身份并重新握手。原有 owner 与绑定仍在配置中，无需再次配对；修改配置不能授予新 owner 权限。

Stream 临时断线只暂停新消息接纳与配对。本次运行已完成账号激活后，只要原账号、绑定与运行实例的授权仍有效，已接纳任务继续交付，后台结果仍通过 HTTP 主动发送；停止服务或撤销授权会中止相关工作。进程重启后需重新私聊验证并激活账号。

本机重置前先停服务：

```sh
chord-gateway pairing reset --platform dingtalk --config /path/to/config.yaml
```

在终端输入 `dingtalk` 确认。重置按[公共规则](./pairing_CN.md#本机重置与恢复)清理 owner、绑定与自动 pin。重新运行 `--pair`，再次私聊发送 `hello`。

所有回复，包括后台结果，均通过主动 `sampleText` 发送；Gateway 不保存或使用 `sessionWebhook`。群会话 ID 直接来自已认证的回调。Access token 只保存在内存，提前刷新并合并并发请求；HTTP 重定向不会转发凭据或 token。

文本按 Unicode 边界拆分，每段最多 1024 UTF-8 字节，同一目标至少间隔 3 秒，账号每秒最多 2 次。这是项目预算，不是钉钉官方硬限额。明确的 HTTP 429 可在原发送期限内最多重试 3 次；网络错误与 5xx 结果未知，不重发。部分发送成功后，不会从第一段重新发送整个结果。

机器人回调是 fire-and-forget：500 或未 ACK **不保证平台重投**。协议成功响应表示 Gateway 接纳或拒绝，不表示 Chord 已执行。不要自动重发结果不确定的命令；先查看 `/status` 与[本机收件状态](./operations_CN.md#消息投递与重启)。详见[官方 Stream 协议](https://open-dingtalk.github.io/developerpedia/docs/learn/stream/protocol/)。

连接失败会记录脱敏类别、重试次数和下次等待时间，同类连续故障的重试日志每分钟最多一条；成功连接也会记录日志。发送指标中的限流等待包含本地账号/目标节流及 HTTP 429 退避，取消等待时记录实际已等待时间。

## 范围与排障

首版不支持外部联系人、跨企业、自定义群 webhook、EVENT 订阅、HTTP 回调、卡片、图片、音频或富文本输入。权限确认与 Question 使用文本命令。群内 @机器人仅触发消息投递，不授予 owner 权限。

| 错误类别 | 处理方式 |
| --- | --- |
| `auth_failed` | 核对 AppKey/AppSecret 与应用发布状态。Stream 注册认证失败停止重连；主动 API 401 清理 token 并终止当前发送，不重发，后续新发送重新取 token。 |
| `tenant_mismatch` | 核对 `corp_id`/`robot_code`；身份不符会停止重连。 |
| `sender_identity_unsupported` | 使用同企业、具有员工 ID 的员工私聊。 |
| `proactive_send_denied` | 开通主动 API 权限、修正可用范围并重新发布，再发送新的 `/bind`。 |
| `target_unreachable` | 核对员工可用范围和机器人是否已入群，再发送新的 `/bind`。 |
| `rate_limited` | 等待平台额度恢复；Gateway 重试次数有上限。 |
| `token_unavailable` | 消息发送前获取 token 失败；检查网络与 API 可用性。这不表示消息投递结果未知。 |
| `unknown_send` | 检查聊天与本机状态，不自动重发原消息。 |

## 部署验收清单

真实账号仍待验收：发布应用；私聊 `hello` 验证；私聊配对并观察主动探测；群绑定并观察主动探测；验证 `/status`、任务、权限确认、Question 与取消。让后台任务运行至首条回调 webhook 过期后至少一分钟，验证主动回传。重启后重新私聊验证身份，并确认原绑定可用，再执行 reset 确认需要新的私聊验证。只保存脱敏错误类别与结果，不保存回调正文、webhook URL、凭据或 token。
