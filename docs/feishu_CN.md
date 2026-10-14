# 飞书接入指南

所有 IM 平台统一使用单 owner 配对与显式聊天绑定，单工作区也需绑定。首次运行 `chord-gateway --pair`，微信/飞书/企业微信/钉钉在机器人私聊发送 `/bind "~/workspace" <pairing_code>`；详见[配对与本机重置](./pairing_CN.md)。旧部署升级步骤见[更新日志](../CHANGELOG_CN.md)。 Slack 在 App 私聊使用 `/chord bind "~/workspace" <pairing_code>`；参见 [Slack 接入指南](./slack_CN.md)。

本文档用于指导你在 `chord-gateway` 中配置并跑通 **飞书**（`ims.feishu`）。

- 如果你只想快速跑通最小配置，先看：[快速开始](../QUICKSTART_CN.md)
- 如果你需要字段级配置说明，见：[配置参考](./configuration_CN.md)

## 开始前先准备

建议先准备好：

- 一个本地项目目录，供 Chord 操作，例如 `/path/to/project`
- 本机可用的 `chord` 可执行文件，或明确的 `chord_path`
- 一个你有权限修改的飞书应用
- 一个可以查看 gateway 日志的位置（终端 stderr 或 `<state_dir>/gateway.log`）

对新手最稳妥的首轮流程建议是：

1. 先只配置 **一个工作区**。
2. 只使用私聊或受控测试群进行联调。
3. 前台运行 `--pair`，在机器人私聊使用终端显示的配对码绑定目录，再发送 `/status`。

## 这条接入是什么

- gateway 使用飞书应用凭据（`app_id` / `app_secret`）。
- 入站事件通过 **长连接**（WebSocket）接收：
  - 不需要公网 webhook URL。
  - 不要配置 webhook 模式相关字段（例如 `verification_token`、`encrypt_key` 等；本项目不使用这些字段）。

## 第 0 步：准备飞书应用

在飞书开放平台开发者后台：

1. 创建一个应用（或选择已有应用）。
2. 开启 **机器人（Bot）能力**（否则无法收发消息）。
3. 获取应用凭据：
   - **App ID** → `ims.feishu.app_id`
   - **App Secret** → `ims.feishu.app_secret`

新手检查清单：

- 确认你正在修改的就是将要写入 `config.yaml` 的那个应用。
- 如果你所在组织对应用变更需要审批，请先完成审批再测试事件投递。
- 不要在 IM 聊天、截图或 shell history 中泄露 app secret。

## 第 1 步：配置事件（长连接）

在飞书后台的 **事件订阅** 中：

1. 选择 **使用长连接接收事件**（不要选 webhook / 开发者服务器模式）。
2. 添加事件 `im.message.receive_v1`。它在控制台中显示为 **消息与群组** 分类下的 **接收消息 v2.0**。

补充说明：

- 群聊场景下请确认机器人已经 **加入目标群聊**，否则群里发消息不会触发事件。
- gateway 目前只处理 **文本消息**（包括 `text` 和 `post`）；入站的图片/文件等非文本消息会被忽略。生成图片的出站转发见[事件可见性](./event-visibility_CN.md#生成图片)。

部署前值得了解的平台侧限制：

- 长连接仅支持 **企业自建应用**；商店应用必须使用开发者服务器。
- 每个事件必须在 **3 秒** 内处理完成，否则飞书会重推。gateway 会尽快确认并异步处理；被重推的事件在当前进程内按 `message_id` 去重。
- 推送为 **集群模式**：同一个应用运行多个客户端时，每个事件只会被其中 **一个** 客户端收到。
- 单个应用最多保持 **50** 条长连接。

## 第 2 步：启用交互卡片回调（强烈建议）

`chord-gateway` 会在确认/提问流程中使用飞书交互卡片。

在飞书后台的回调配置中添加：

- `card.action.trigger`

如果没有这个回调，卡片按钮点击事件无法投递到 gateway。

## 第 3 步：权限、scope 与发布

飞书应用需要权限（scope）才能“接收事件 + 发送消息”。

### 3.1 最小权限（建议从这里开始）

在飞书后台打开 **权限管理（Permissions & Scopes）**。除了逐项勾选，也可以点击
**批量导入**，直接粘贴下面的 JSON 一键开通。

- **以应用身份发消息**（gateway 回复需要；同时也覆盖把确认/提问卡片置为已解决的那次卡片更新）
  - 以下**任选其一**即可：
    - `im:message`（获取与发送单聊、群组消息）
    - `im:message:send_as_bot`（以应用的身份发消息，范围最小）

- **接收消息事件**（用于订阅 `im.message.receive_v1`）
  - 这几项**不是三选一**：飞书按场景分别判断推送范围，你用到哪种场景就要开哪个：
    - 只用 **私聊**：`im:message.p2p_msg:readonly`（或 `im:message.p2p_msg`）
    - 用 **群聊并 @ 机器人**（仅用户消息）：`im:message.group_at_msg:readonly`（或 `im:message.group_at_msg`）
    - 用 **群聊并 @ 机器人**（含其他机器人消息）：`im:message.group_at_msg.include_bot:readonly`
    - 需要接收 **群内所有用户消息**（属于敏感权限）：`im:message.group_msg`
    - 需要接收 **群内所有消息（含其他机器人消息）**（属于敏感权限）：`im:message.group_msg.include_bot:read`

> 如果私聊和群聊都要用，**两种权限都要开**。只开私聊权限时，群里 @ 机器人会静默收不到任何消息。

仅私聊配置（包含生成图片转发）：

```json
{
  "scopes": {
    "tenant": [
      "im:message:send_as_bot",
      "im:resource",
      "im:message.p2p_msg:readonly"
    ],
    "user": []
  }
}
```

`im:resource` 用于上传生成图片（上传接口接受不超过 10 MB 的图片，且只支持有限格式）；不用图片转发时可以省略。已有应用新增此权限后，需要发布新的应用版本。生图能力在 Chord 中启用，Chord 构建还须支持[兼容策略](./compatibility_CN.md)中的图片传输协议。

如果还要用群聊，往 `tenant` 数组里追加 `im:message.group_at_msg:readonly`
（或 `im:message.group_msg`）。

说明：

- gateway 用 `POST /open-apis/im/v1/messages` 发送文本和交互卡片，用
  `PATCH /open-apis/im/v1/messages/{message_id}` 把确认/提问卡片置为已解决。
  这两个接口都接受 `im:message` 或 `im:message:send_as_bot`，因此**不需要**额外开
  `im:message:update`。
- 不同企业/租户策略可能需要管理员审批。
- 平台限额：消息接口整体允许 1000 次/分钟、50 次/秒，对同一用户或群组约 5 次/秒。文本消息上限 150 KB，卡片/富文本消息上限 30 KB。卡片只能在发送后 14 天内更新，单条消息更新频控 5 次/秒。gateway 会把过长的回复拆成每条不超过 4000 字符的消息。

### 3.2 任何变更后都要发布（最容易遗漏）

能力 / 权限 / 事件订阅变更后，必须 **发布新的应用版本** 才会生效。否则控制台看似已配置正确，但 gateway 仍可能收不到事件。

### 3.3 如何判断权限不足

常见表现：

- 长连接已建立，但始终收不到文本或卡片事件；检查飞书控制台中的订阅与权限。
- 私聊正常但群聊毫无反应：只开了私聊接收权限，没有开对应的群聊接收权限。
- gateway 发送消息时报错，例如：`feishu API error: code=... msg=...`。
- 确认/提问按钮点了有效，但卡片始终不变成“已解决”状态：通常是卡片更新失败（仅支持更新发送后 14 天内的消息）。

处理步骤：

1. 在控制台补齐缺失权限。
2. 发布新版本。
3. 重启 gateway。


## 第 4 步：配置并配对

保存最小配置：

```yaml
ims:
  feishu:
    app_id: cli_example
    app_secret: your-app-secret
```

在前台终端运行 `chord-gateway --config config.yaml --pair`。连接成功后，终端显示 10 分钟有效的配对码。在机器人**私聊**中发送：

```text
/bind "~/work/project-a" <pairing_code>
```

目录必须已存在。成功后 gateway 保存 owner、工作区和聊天绑定；发送 `/status` 验证。之后普通启动即可，不需再加 `--pair`。若收不到事件，检查长连接、订阅、scope 和应用发布状态。

## 多工作区路由与 `/bind`

配对后的 owner 可把机器人加入群聊，并在目标聊天发送 `/bind "~/work/project-b"`。不同聊天可绑定不同目录或共享同一目录；只有 owner 的输入会执行，群成员可以看到回复。单工作区也必须显式绑定。新绑定不会重放先前未绑定的消息。

目录切换需等待工作、输入和交互完成。账号变化、配对重置与保存失败恢复见[配对指南](./pairing_CN.md)。

## 接纳与运行期去重

text/post 与可信卡片回调共用内存收件和 Chord 消费回执。飞书可能重推同一事件，因此按 `message_id`（gateway 的做法）而不是 `event_id` 去重。重复消息或同一回调在当前进程内去重；Gateway 重启后没有去重历史或恢复队列。旧去重文件不再读取或导入。App ID、owner、绑定和 session pin 继续使用原配置。

普通群与话题群都按群授权；非 Question 卡片等待命令终态后更新结果，平台接纳提示不等于执行完成。旧控制目标、过期回调和恢复失败的 pin 不会自动改投新会话。未知结果先核查 `/status` 与 Chord 原会话。见[投递语义](./operations_CN.md#消息投递与重启)。

## 已知限制

- 飞书入站处理目前仅支持 **文本消息**（`text` 和 `post`）；非文本消息会被忽略。

## 官方文档参考

- [使用长连接接收事件](https://open.feishu.cn/document/server-docs/event-subscription-guide/event-subscription-configure-/request-url-configuration-case)
- [接收消息事件（`im.message.receive_v1`）](https://open.feishu.cn/document/server-docs/im-v1/message/events/receive)
- [发送消息](https://open.feishu.cn/document/server-docs/im-v1/message/create)与[更新已发送的消息卡片](https://open.feishu.cn/document/server-docs/im-v1/message-card/patch)
- [上传图片](https://open.feishu.cn/document/server-docs/im-v1/image/create)
- [API 权限列表](https://open.feishu.cn/document/server-docs/application-scope/scope-list)

## 故障排查

- 长连接未建立或收不到事件：见 [故障排查 — 飞书问题](./troubleshooting_CN.md#飞书问题)
