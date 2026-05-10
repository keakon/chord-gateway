# 飞书接入指南

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

1. 先只配 **一个 workspace**。
2. 先不要配置 `owner_open_id` / `allowed_open_ids`，先确认机器人能跑通。
3. 等你在日志里看到第一条成功收消息记录后，再收紧访问控制。

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

在飞书后台的“事件与回调”中：

1. 选择 **使用长连接接收事件**。
2. 在事件订阅中至少订阅：
   - `im.message.receive_v1`

补充说明：

- 群聊场景下请确认机器人已经 **加入目标群聊**，否则群里发消息不会触发事件。
- gateway 目前只处理 **文本消息**（包括 `text` 和 `post`）；图片/文件等非文本消息会被忽略。

## 第 2 步：启用交互卡片回调（强烈建议）

`chord-gateway` 会在确认/提问流程中使用飞书交互卡片。

在飞书后台的回调配置中添加：

- `card.action.trigger`

如果没有这个回调，卡片按钮点击事件无法投递到 gateway。

## 第 3 步：权限、scope 与发布

飞书应用需要权限（scope）才能“接收事件 + 发送消息”。

### 3.1 最小权限（建议从这里开始）

在飞书后台 **权限管理（Permissions & Scopes）** 中，建议至少开启：

- **以机器人身份发消息**（用于 gateway 回复）
  - 通常对应 `im:message` / `im:message:send_as_bot` 这类权限项（具体名称以控制台为准）

- **接收消息事件**（用于订阅 `im.message.receive_v1`）
  - 按你的使用场景选择最小集合：
    - 只用 **私聊**：`im:message.p2p_msg:readonly`
    - 用 **群聊并 @ 机器人**：`im:message.group_at_msg:readonly`
    - 需要接收 **群内所有消息**（能力更强，通常属于敏感权限）：`im:message.group_msg`

说明：

- 飞书允许在开通“接收消息”相关权限中的**任意一个**后订阅 `im.message.receive_v1`。
- 不同企业/租户策略可能需要管理员审批。

### 3.2 任何变更后都要发布（最容易遗漏）

能力 / 权限 / 事件订阅变更后，必须 **发布新的应用版本** 才会生效。否则控制台看似已配置正确，但 gateway 仍可能收不到事件。

### 3.3 如何判断权限不足

常见表现：

- 长连接已建立，但始终没有 `feishu: received message` 日志。
- gateway 发送消息时报错，例如：`feishu API error: code=... msg=...`。

处理步骤：

1. 在控制台补齐缺失权限。
2. 发布新版本。
3. 重启 gateway。


## 第 4 步：配置 `chord-gateway`

最小配置示例：

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
workspaces:
  default:
    path: /path/to/project
```

更接近日常使用的 starter 配置示例：

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
workspaces:
  default:
    path: /path/to/project
chord_path: chord
idle_timeout: 30m
```

启动 gateway：

```bash
chord-gateway -f config.yaml
```

启动后你应该重点观察：

- gateway 能正常启动，没有 `load config` 错误
- 飞书长连接建立成功
- 日志同时写到 stderr 和 `<state_dir>/gateway.log`

## 第 5 步：验证入站事件并获取 `open_id`

1. 给机器人发送一条 **纯文本** 消息（私聊或群聊）。
2. 在 gateway 日志中查找类似记录：

```text
msg="feishu: received message" chat_id=oc_xxx open_id=ou_xxx message_id=om_xxx content=hello
```

3. 把这条日志视为第一阶段成功标志：
   - `chat_id=oc_xxx` 表示 gateway 识别到了哪个飞书聊天
   - `open_id=ou_xxx` 表示是谁发来的消息
4. 如果不是本地自用测试，建议尽快收敛访问控制：

```yaml
ims:
  feishu:
    owner_open_id: ou_xxx
```

然后重启 gateway。

如果你 **没有** 看到这条日志，按下面顺序检查：

1. 应用是否配置成 **长连接**，而不是 webhook？
2. 是否订阅了 `im.message.receive_v1`？
3. 最近一次控制台改动后是否已经 **发布版本**？
4. 机器人是否真的在你发消息的私聊/群聊里？
5. 你发的是不是 **纯文本**，而不是图片/文件/表情？

## 多工作区路由与 `/bind`

当存在多个 workspace 时，飞书通过 `chat_bindings` 做“群聊 → 工作区”映射。

推荐流程：

1. 先用单 workspace 配置启动 gateway，不填写 `chat_bindings`。
2. 在飞书中创建目标群聊，把机器人加入该群，并发送一条纯文本消息。
3. 在该聊天中执行：

```text
/bind <workspace_id> <path>
```

示例：

```text
/bind project-a ~/work/project-a
```

4. gateway 只会更新 `ims.feishu.chat_bindings` 和 `workspaces`，并把这两段写回 YAML。
5. 重新打开 `config.yaml`，确认映射已经按预期写入。

边界说明：

- `/bind` 不会更新 allowlist 等其他适配器设置。
- `/bind` 之外的手工配置修改仍需要重启 gateway。

## 已知限制

- 飞书入站处理目前仅支持 **文本消息**；非文本消息会被忽略。

## 故障排查

- 长连接未建立或收不到事件：见 [故障排查 — 飞书问题](./troubleshooting_CN.md#飞书问题)
