# Telegram 接入指南

Telegram 通过 Bot API 长轮询连接到本机 Gateway，不需要公网回调地址。当前支持机器人私聊、普通群和 supergroup 的纯文本任务与命令；每个聊天都需由配对 owner 显式绑定。真实账号部署验收尚未完成，发布前仍需完成下文检查。

## 创建机器人并配对

1. 在 Telegram 的 BotFather 创建机器人，取得 bot token，记下机器人 username。群 privacy 保持默认开启；Gateway 不需要机器人管理员权限。
2. 保存最小配置，首次省略 `owner` 和 `chat_bindings`：

   ```yaml
   ims:
     telegram:
       bot_token: your-bot-token
   ```

3. 在前台终端运行：

   ```bash
   chord-gateway --config /path/to/config.yaml --pair
   ```

4. Gateway 验证机器人身份、检查 webhook 并完成连接探测后，终端显示有效期 10 分钟的配对码。在机器人**私聊**发送：

   ```text
   /bind "/path/to/project" <pairing_code>
   ```

   目录必须已存在于 Gateway 主机上。成功后发送 `/status`，再发送一条短文本任务。Gateway 将 owner 和绑定保存到 YAML；后续启动无需 `--pair`。

凭据保留在配置文件中，不要提交到版本库。API 地址固定为 `https://api.telegram.org`，不提供自定义 endpoint 配置。网络环境须允许 Gateway 访问这个地址。

## 群聊与 privacy

先在私聊完成配对，再把机器人加入可信群。由 owner 在群里发送：

```text
/bind@YourBotUsername "/path/to/project"
```

将 `YourBotUsername` 替换为当前机器人 username。默认 privacy 下，用显式带 suffix 的命令操作，并通过**回复机器人的消息**发送普通任务。普通群文本未送达而命令可达通常是 privacy 的预期行为；Gateway 无法处理 Telegram 未投递的消息。

确实需要接收任意群文本时，在 BotFather 关闭 privacy，并按 Telegram 客户端要求重新添加机器人。即使关闭 privacy，Gateway 也只接受配对 owner 的输入。Gateway 不强制任务必须是回复：只要 Telegram 实际投递 owner 的纯文本，且聊天已绑定，就会按公共授权规则接纳。群回复对成员可见，因此只绑定可信群。

群里的 `/status@YourBotUsername`、`/cancel@YourBotUsername`、`/allow@YourBotUsername`、`/answer@YourBotUsername @<id> <text>` 与私聊命令含义相同。自动生成的操作提示会添加当前机器人 suffix。发给其他机器人的命令会被忽略；普通任务里的路径、`@` 和斜杠原样保留。完整命令和会话恢复见[使用指南](./usage_CN.md)。

Telegram 将公共命令中带连字符的名称写为 `/handoff_deny`、`/question_interact`、`/question_withdraw`、`/question_replace`。群里再加 `@YourBotUsername`，例如 `/handoff_deny@YourBotUsername <reason>`。私聊和群聊中的自动提示都会使用这些名称；带机器人后缀的旧连字符写法会被拒绝。
Telegram 将公共命令中带连字符的名称写为 `/handoff_deny`、`/question_interact`、`/question_decline`、`/question_withdraw`、`/question_replace`。群里再加 `@YourBotUsername`，例如 `/handoff_deny@YourBotUsername <reason>`。私聊和群聊中的自动提示都会使用这些名称；带机器人后缀的旧连字符写法会被拒绝。

确认、回答和取消都以 Gateway 收到整批消息时的目标为准，接纳重试不会改指向新请求。执行前目标已变化时，旧操作会被拒绝；请查看 `/status` 后再发送新命令。`/status` 也会显示最近一次输出部分失败及已接受段数。输出不完整时请查看原 Chord 会话，不要为了补输出重新执行任务。

输出使用纯文本，长消息自动分段；代码块不会按 Markdown 渲染。确认和 Question 使用文本命令，不使用按钮。长任务沿用[进度与提醒](./event-visibility_CN.md)，可用 `/status` 查询、`/cancel` 取消。

## 身份、重启与保留期

账号身份以 `getMe` 返回的数字 Bot ID 为准，username 变化不改变 owner 或绑定。同一 Bot 更换 token 后仍使用原账号存储；换成另一个 Bot 会阻断旧 owner，需要核对后在本机重置并重新配对：

```bash
chord-gateway pairing reset --platform telegram --config /path/to/config.yaml
```

先停服务；命令需要在控制终端输入平台名称确认。重置细节见[配对与本机重置](./pairing_CN.md)。同一 OS 用户下，同一 Bot 的本机实例受账号锁保护；跨主机部署也必须保持一个 poller。

消息、去重和待确认 offset 只保存在内存。当前批次逐条确定处理后才推进 offset；重启不恢复本地进度，可能丢失已确认但未交给 Chord 的输入，或收到重复消息。详见[投递语义](./operations_CN.md#消息投递与重启)。

只有普通文本任务保存用于冲突检测的正文摘要。控制命令和不支持的输入只保留身份去重证据，不保存配对码、回答等参数的摘要；重复控制不会再次执行。身份相同、仅参数不同的控制输入不做当前进程内参数比较。

网络故障使用带抖动的指数退避，短暂成功不会重置等待时间，持续成功一分钟后才重置。Telegram 明确返回的 `retry_after` 优先，等待可以取消。本地重启后紧接着的 409 会短暂重试（上一次服务端长轮询在其 25s 超时前仍残留）；持续 409 说明真有第二个 poller，仍为致命错误。

Telegram 服务器最多保留未接收 update **24 小时**，离线超过保留期可能丢失消息。至少一周没有新 update 后，新的 update ID 可能随机重置，因此 Gateway 按当前批次确认，不依赖永久最大 ID。详见官方 [getUpdates](https://core.telegram.org/bots/api#getupdates) 和 [Update](https://core.telegram.org/bots/api#update) 文档。

## 排障

| 现象或分类 | 处理 |
| --- | --- |
| `getMe` 401 | 核对或重新生成 BotFather token，更新配置后重启 |
| `webhook_conflict` | 当前 Bot 已配置 webhook。停用原服务，显式清除 webhook 后再启动；Gateway 不会自动删除 webhook，也不会丢弃待收消息 |
| `poller_conflict` / 409 | 停止同一 Bot 的其他长轮询进程，包括其他主机上的实例。本地重启后紧接着的一次 409 是残留的服务端长轮询，会自行重试；只有持续 409 才需要处理 |
| 群普通文本未送达、命令可达 | 检查 privacy；默认模式用回复 Bot 的方式发送任务 |
| 403 | 检查机器人是否被屏蔽、被移出群或目标不可访问 |
| `chat_migrated` | 由 owner 到新 supergroup 显式重新绑定；不会复制旧群授权 |
| `unknown_send` | 请求可能已经送达，先查看聊天；Gateway 不会自动重发或重新执行任务 |
| Bot 完全收不到消息 | 以 debug 级别启动日志。`telegram: received updates` 说明 Telegram 已投递批次；`telegram: captured update ... reason=` 说明每条 update 如何被归一化；`telegram: dispatch update ... disposition=` 显示它停在哪一步。若完全没有 `received updates`，说明轮询从未拉取，请把 `getWebhookInfo.pending_update_count` 与 offset `-1` 的 `getUpdates` 结果对照正在运行的进程核对 |
| Bot 完全收不到消息 | 以 debug 级别启动日志。`telegram: received updates` 说明 Telegram 已投递批次；`telegram: captured update ... reason=` 说明每条 update 如何被归一化；`telegram: dispatch update ... disposition=` 显示它停在哪一步。始终无法进入终态的 update 会按序阻塞后续 update（设计如此），首次告警一次、之后最多每分钟一次并带 `update_id`，同时记录 `acceptance_unconfirmed` 指标。若完全没有 `received updates`，说明轮询从未拉取，请把 `getWebhookInfo.pending_update_count` 与 offset `-1` 的 `getUpdates` 结果对照正在运行的进程核对 |

## 部署验收

真实账号验收尚未完成。部署后依次记录：私聊配对、`/status`、短文本任务、群带 suffix 绑定、回复 Bot 的任务、确认/Question/取消、超过 5 分钟的任务，以及重启后在原绑定查询状态。群 privacy 测试分别记录普通消息、显式命令和回复 Bot 三种送达情况；默认模式通过不要求关闭 privacy。

本轮不支持 forum topic、inline keyboard、媒体输入/输出和编辑预览。Topic 输入不会落到群根绑定；媒体 caption 不会变成文本任务；channel post、编辑消息、机器人、匿名管理员和 `sender_chat` 代发均不作为任务接纳。这些能力需要单独功能实现。
