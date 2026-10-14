# Slack 接入指南

通过 Socket Mode 接入一个 Slack workspace，并使用 Bot Web API 回传结果。支持已有 App 私聊、公开/私有频道和独立 thread；每个目标分别绑定目录并保留会话。本页说明安装流程；自动协议测试不能替代你自己的 Slack workspace 验收。

## 1. 创建并安装 App

在 [Slack API](https://api.slack.com/apps) 创建 Slack App，启用 **Socket Mode**，创建授予 `connections:write` 的 app-level token。在 **App Home** 启用 **Messages tab**，允许用户发送消息和 slash commands。

在 **OAuth & Permissions** 添加以下 **Bot Token Scopes**：

| Scope | 用途 |
| --- | --- |
| `chat:write` | 向已绑定目标发送回复 |
| `im:history` | 接收 `message.im` |
| `im:read` | 核验已有 Bot 私聊 |
| `app_mentions:read` | 接收频道/thread 中的 mention |
| `channels:read`、`groups:read` | 核验公开/私有频道及归档、共享状态 |
| `users:read` | 通过 `bots.info` 核验 Bot 所属 App |
| `commands` | 接收 `/chord` |

在 **Event Subscriptions** 订阅 `message.im` 与 `app_mention`。不要订阅 `message.channels` 或 `message.groups`：频道和 thread 输入必须显式 @ 此 Bot。

在 **Slash Commands** 创建唯一的 `/chord`，Usage Hint 填写 `<command> [arguments]`。关闭 **Escape channels, users, and links sent to your app**，保留路径、引号和原始参数。Socket Mode 接收命令无需公网 Request URL。同 workspace 中若有其他 App 定义了 `/chord`，请移除冲突定义，不要更改本项目命令名。

安装 App 到 workspace，保存 **Bot User OAuth Token**、App ID 和 workspace/team ID。必须使用 Bot 安装 token，不使用用户 token。修改 scopes 后重新安装。将 Bot 邀请到每个目标频道，包括私有频道。

首版不申请 `chat:write.public`、`im:write`、`channels:history`、`groups:history` 或文件权限；也不会替你创建私聊、加入频道或解除归档。

## 2. 配置并在私聊配对

保存 `config.yaml`：

```yaml
ims:
  slack:
    app_token: your-app-token
    bot_token: your-bot-token
    app_id: your-app-id
    team_id: your-team-id
```

四项均必填，token 和 ID 必须来自同一安装范围。妥善保管两个 token 与网关状态目录。启动时核验 Bot 的 team、user、App 身份和 Socket Mode hello，身份不一致不会进入 ready。同一操作系统用户下，一个账号/App 只能有一个本机接收实例；跨主机也应保持单个活跃实例。

在前台终端启动：

```sh
chord-gateway -f config.yaml --pair
```

通过 App 的 Messages tab 打开已有私聊。终端显示配对码后发送：

```text
/chord bind "/path/to/project" <pairing_code>
/chord status
```

目录必须存在于网关主机。配对码有效期为 10 分钟；只有经过核验的一对一 Bot 私聊可以登记 owner，私聊里的 thread 不能首配。配对后可在私聊发送普通文本任务。作为服务运行时正常重启，无需 `--pair`。

Slack 根聊天的命令入口统一为 `/chord <command> [arguments]`，裸 `/bind` 不是已注册的 Slack 命令。参数仍遵循 Gateway 既有语法，例如 `/chord answer @<question-id> <answer>`。

## 3. 绑定频道和 thread

配对后的 owner 可在已邀请 Bot 的公开/私有频道发送：

```text
/chord bind "/path/to/project"
```

随后用 `@Bot 你的任务` 发起工作。频道和 thread 只接受 owner 显式 @ 此 Bot 的消息。频道成员可以看到回复，请只绑定受信任的频道。共享频道、Slack Connect、外部 team 用户和多人私聊不在首版范围。

slash command 始终操作**频道根目标**，即使在查看 thread 时发送也一样。绑定和控制独立 thread 时，请**在该 thread 内**发送以下消息，并选择实际的 Bot mention：

```text
@Bot /bind "/path/to/project"
@Bot 你的任务
@Bot /status
@Bot /allow
@Bot /answer @<question-id> <answer>
```

每个 thread 需独立绑定，不继承频道根或其他 thread 的绑定。回复、确认、Question 和后台结果均返回原 thread；后续消息也必须 @Bot。网关提示会显示 Bot 名称并提醒你从 Slack 的 `@` 菜单选择该 Bot；仅粘贴名称或字面量 `<@用户ID>` 不等于实际 mention。

普通消息和 thread 文本命令中的 `&`、`<`、`>` 会还原为用户输入的字符；`/chord` 参数保持原样。

## 4. 输出与恢复

回复使用纯文本，模型输出中的 mention 和链接控制语法会转义，关闭自动链接预览，长回复按序分段。同一频道的根消息和全部 thread 共享每秒一条的预算；遇到 `429`，当前安装实例对应 API 方法按 Slack `Retry-After` 暂停，覆盖会话查询、身份核验与连接建立。其他方法不受影响；冷却状态跨 Socket 重连保留，等待遵守取消信号与请求期限。Socket 重连重新核验身份；内存接纳短暂繁忙时退避重试。同进程重连保留已接纳输入，进程重启清空；结果未确认的发送不自动重发。

首版支持文本命令和文本输出。文件、生成图片转发、按钮、modal、编辑预览、Agent View、Enterprise 多团队安装和 Marketplace 分发延期。长时间任务使用 Bot API 回传，不依赖会过期的 slash `response_url`。

需要本机重置时，先停止网关：

```sh
chord-gateway pairing reset --platform slack -f config.yaml
```

在终端输入 `slack` 确认，再以 `--pair` 启动。处理未知操作或更换账号前，阅读[配对说明](./pairing_CN.md)与[投递语义](./operations_CN.md#消息投递与重启)。ACK 或 `Accepted`（已接收）仅确认收件，不表示 Chord 执行成功。同一 slash 命令重投时，已经过期、隔离、丢弃或结果未知的收件会返回相应状态；结果未知时先核查会话和 `/chord status`，不要直接重复操作。

Socket 收件最多等待 2 秒确认接纳，为 Slack 的 3 秒 ACK 截止时间预留发送时间。聊天查询与排队另有 30 秒总期限，断线时取消。未收到 ACK 不会取消接纳，也不证明命令失败：命令仍可能执行，同一消息重投会在当前进程内去重。手动重试前先查看 `/chord status` 和原会话。ACK 提示统一使用英文，与其他命令回复一致。

## 安装检查与排障

验证私聊配对、`/chord status`、文本任务、确认和 Question；将 Bot 邀请到一个公开频道和一个私有频道并分别绑定；在同频道绑定两条 thread，检查命令、结果和长回复分段留在各自 thread。最后重启网关，验证会话恢复和长时间后台结果。这些检查需要真实 Slack workspace 和凭据，不能由本机测试通过推定。

- **无法识别 `/chord`**：检查命令注册、安装 workspace 和冲突定义，不要改用裸 `/bind`。
- **未 ready / 身份不匹配**：检查四项配置和 Socket Mode。`link_disabled` 需重新启用 Socket Mode；撤销或失效的 token 需修正凭据。
- **`missing_scope`**：补齐权限并重新安装 App，仅修改控制台不会更新已安装 token。启动身份核验缺少 `users:read` 等必需权限时会明确报错并停止重试；修正安装权限后重启网关。
- **`not_in_channel`、`channel_not_found`、`is_archived`**：邀请 Bot、核验频道访问或在本机解除归档。网关不更改频道成员，也不改投私聊。
- **消息被忽略**：确认发送者是 owner，目标已独立绑定，频道/thread 包含实际 Bot mention。共享频道和未核验私聊会被拒绝。
- **发送或执行结果未知**：重试前检查原聊天、会话和本机 ingress 元数据。结果未确认的输出不会自动重发。
