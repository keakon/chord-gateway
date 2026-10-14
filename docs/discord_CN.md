# Discord 机器人接入

通过机器人私聊、服务器文字频道或既有公开/私有 thread 控制 Chord。Discord 使用向外建立的 Gateway WebSocket，无需公网入口。服务器频道与 thread 中的输入必须来自配对 owner，并显式 @Bot。首版不支持群 DM、语音频道或向论坛根频道发帖。

Discord 真实账号部署验收仍未完成；自动协议测试通过不代表已完成无人值守部署验收。

## 创建并安装机器人

1. 在 [Discord Developer Portal](https://discord.com/developers/applications) 创建应用与 Bot，取得机器人 token 和 application ID。
2. 创建 scope 为 `bot` 的服务器安装链接，授予 **View Channel**、**Send Messages**、**Send Messages in Threads**。检查频道权限覆盖；使用私有 thread 时先邀请机器人加入。
3. 保持 Message Content、Server Members、Presence privileged intents 关闭。Gateway 只使用 `GUILDS`、`GUILD_MESSAGES`、`DIRECT_MESSAGES`，首版通过 DM 和显式 mention 获取正文。详见 [Discord Gateway intents](https://docs.discord.com/developers/events/gateway#message-content-intent)。

网关不读取历史消息，无需 Read Message History、Administrator 或 Manage Threads。

保存以下配置，两项凭据均必填：

```yaml
ims:
  discord:
    bot_token: your-bot-token
    application_id: "123456789012345678"
```

token 属于敏感凭据。application ID 标识应用；配对写入的 `owner.account_id` 则是认证后的 Bot user ID。更换机器人账号后必须本机重置，不能继承原有授权。

## 配对与聊天绑定

在前台终端运行 `chord-gateway -f config.yaml --pair`。在 Discord 隐私设置中允许共享服务器的机器人私聊，打开 Bot DM 并发送：

```text
/bind "/path/to/project" <pairing_code>
```

目录必须已存在于网关主机。私聊配对后，同一 owner 分别绑定服务器文字频道或可访问的 thread：

```text
@Bot /bind "/path/to/project"
@Bot /status
@Bot Explain the project structure
```

请在 Discord 中选择真实的机器人 mention。DM 中直接发送命令，无需 @Bot。频道与 thread 中的 `/allow`、`/deny`、`/answer`、`/cancel` 等命令需在开头 @Bot；仅在正文后面提及机器人时，整条消息作为普通任务，正文中的 `/bind` 不会升级为命令。其他用户、机器人、webhook、消息编辑与删除事件不会创建任务。

每个 thread 独立绑定和维护会话，不继承父频道路由。已有论坛帖子 thread 在可访问时可以使用，但网关不会创建 thread 或论坛帖子。回复对频道/thread 成员可见，只绑定可信空间。详见[配对与本机重置](./pairing_CN.md)。

## 输出与连接行为

回复以转义文本发送，长输出按最多 1900 个 UTF-16 code units 分段。首版不提供 Markdown/代码围栏富文本、embed、附件、交互组件或原生 slash command。输出中的用户、角色、全员 mention 不会触发通知，链接 embed 预览关闭。

向未锁定的归档 thread 发送结果时，Discord 可能自动恢复活跃状态。锁定、删除或无法访问时明确报错，结果不会转发至父频道。详见 [Discord thread 规则](https://docs.discord.com/developers/topics/threads)。

断线期间平台标记为不可用，并尝试恢复连接。已接纳输入使用公共内存收件及消息 ID 去重；Gateway Resume 本身不代表跨重启可恢复。API 限流等待可取消；网络超时与服务端错误保留发送结果未确认状态，不自动重发。

Resume 期间，`RESUMED` 前到达的候选消息会保留当时的正文与命令目标，待同一会话恢复就绪后进入公共收件。临时重放缓冲最多保留 128 个有效事件；容量溢出时连接以 `replay_capacity_exceeded` 明确停止，不静默跳过消息，也不继续自动重连。管理员应先在本机核查原 Chord 会话与当前收件状态，再决定重启，不要盲目重发历史消息。进程重启不恢复这个缓冲或已接纳队列。

## 故障排查与部署验证

| 现象 | 检查项 |
| --- | --- |
| 无法打开 Bot DM 或私聊投递失败 | 允许共享服务器私聊，检查 Discord 隐私设置。 |
| 服务器消息被忽略 | 使用配对 owner 显式 @Bot；命令需在开头提及机器人，并先绑定频道/thread。 |
| `401` 或认证失败 | 本机更新失效 token，确认 application ID 与机器人属于同一应用。 |
| `403` 或缺少访问权限 | 检查频道权限覆盖、View Channel、Send Messages、Send Messages in Threads，以及私有 thread 访问权限。 |
| Gateway close `4014` | 检查部署版本只请求上述 intents，不用开启 privileged intents 来绕过不匹配。 |
| 归档 thread 恢复活跃 | 向未锁定的归档 thread 发送消息时属于预期行为。 |
| 锁定 thread 无法接收结果 | 请 Discord 管理员自行解锁；网关不申请 Manage Threads，也不改投其他聊天。 |
| `sharding_not_supported` | 首版仅支持单 shard，需使用不要求多 shard 的机器人部署。 |
| `replay_capacity_exceeded` | Resume 完成前到达的有效事件超过 128 个，连接明确停止。管理员先检查原 Chord 会话、输入队列与本机收件记录，再决定重启；不要盲目重放历史命令或任务。 |

正式使用前，验证私聊配对、频道绑定、公开/私有 thread 分别绑定、`/status`、`/allow`、`/answer`、`/cancel`、超过 15 分钟的任务、未锁定归档 thread 中的后台结果，以及锁定 thread 的明确失败。自动协议测试不能替代真实账号验收。

重置前停止服务：`chord-gateway pairing reset --platform discord -f config.yaml`。重置清除 Discord owner、绑定和自动 pin，保留凭据、工作区与其他平台；随后用 `--pair` 重新配对。投递语义见[运维说明](./operations_CN.md)。

WebSocket 单条消息限制为 16 MiB，以容纳较大的 READY 并限制资源使用；超限会记录错误并断开连接。这是客户端防护预算，不是平台发送限额。
