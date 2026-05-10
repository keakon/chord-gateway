# 快速开始

本文档帮助你快速运行 `chord-gateway`，并完成微信 iLink 或飞书接入。

## 1. 前置条件

- Go 1.26+
- 本机可用 `chord` 可执行文件，或通过 `chord_path` 配置其路径
- 至少准备一种 IM 平台配置：
  - 微信 iLink（个人微信二维码登录）
  - 飞书应用凭据（`app_id`、`app_secret`）

## 2. 安装

通过 Go 安装：

```bash
go install github.com/keakon/chord-gateway@latest
```

或在本地源码目录构建：

```bash
go build -o chord-gateway .
```

## 3. 创建最小配置

把 `workspaces.default.path` 指向你希望 Chord 操作的项目目录。

关键路由规则：

- 微信始终通过 `ims.wechat.workspace_id` 路由到一个工作区。
- 飞书通过 `ims.feishu.chat_bindings` 把 chat ID 映射到 workspace ID。
- 如果只有一个 workspace，微信 `workspace_id` 和飞书 `chat_bindings` 都可以省略。

### 3.1 微信

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
workspaces:
  default:
    path: /path/to/project
chord_path: chord
idle_timeout: 30m
```

首次运行行为：

- gateway 会打印二维码登录 URL。
- 用微信扫码登录。
- token 会保存到 `<state_dir>/wechat/token.json`，后续运行可复用。如需单独管理该密钥，可通过 `ims.wechat.token_path` 覆盖路径。

### 3.2 飞书

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

飞书配置说明：

- 在飞书开发者后台将事件订阅方式设置为“使用长连接接收事件”。
- 在事件订阅里至少订阅 `im.message.receive_v1`（接收入站消息）。
- 在回调配置里添加 `card.action.trigger`（处理确认/提问卡片交互）。
- 如果你要让不同飞书群路由到不同 workspace，推荐先只配置一个 workspace 启动 gateway，不填写 `chat_bindings`。
- 然后在飞书中创建目标群聊、把机器人加入该群，发送一条纯文本消息，并在该聊天中执行 `/bind <workspace_id> <path>`。如果路径包含空格，请用英文双引号包裹，例如 `/bind project-a "~/work/project a"`。
- `/bind` 只会立即更新运行中 gateway 的飞书 `chat_bindings` 和 `workspaces`，并把这两个部分的相同变更写回 YAML 配置文件；它不是通用配置重载入口。手工修改配置文件仍需要重启 gateway 才会生效。
- 如果你更希望自己编辑 YAML，也可以从 gateway 日志里的 `chat_id=...` 提取真实群聊 `chat_id` 后，再回填到 `chat_bindings`。
- 长期使用建议配置 `owner_open_id` 和/或 `allowed_open_ids`。未设置时允许所有用户；设置后只处理 allowlist 中用户的消息和命令。要获取你的 `open_id`，先发一条消息，然后在 gateway 日志的 `feishu: received message` 行中找到 `open_id=ou_xxx`。
- 飞书入站消息必须是纯文本；非文本消息会被忽略。
- 不需要公网回调地址，也不需要配置 `verification_token`、`encrypt_key`、`listen` 或 `webhook_path`。

首次发消息前的飞书检查清单：

1. `app_id` 和 `app_secret` 有效，gateway 启动时能成功获取 app access token。
2. 飞书开发者后台已将事件订阅方式切换为“使用长连接接收事件”。
3. 已启用事件订阅，并包含 `im.message.receive_v1`。
4. 已在回调配置中添加 `card.action.trigger`。
5. 如果需要把某个群路由到特定 workspace，请先创建目标群、添加机器人、发送一条纯文本消息，然后直接在该聊天中执行 `/bind <workspace_id> <path>`。
6. 如果使用了 `owner_open_id` 或 `allowed_open_ids`，发送者的 `open_id` 已在最终 allowlist 中。（要获取 `open_id`，先发一条消息，然后在 gateway 日志的 `feishu: received message` 行中找到 `open_id=ou_xxx`。）
7. 在目标飞书聊天中先发送一条纯文本消息，再发送 `/status`。

### 3.3 微信 + 飞书 + 多工作区

如果你希望“微信固定一个 workspace，飞书不同群绑定不同 workspace”，推荐先分两步做：

1. 先按单 workspace 方式跑通飞书。
2. 再在每个目标飞书聊天中执行 `/bind <workspace_id> <path>` 完成绑定或更新绑定。
3. 如果你更希望手工编辑 YAML，再把日志里发现的 `chat_id` 写入 `chat_bindings`。

示例最终配置：

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
    workspace_id: project-a
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    chat_bindings:
      oc_project_a: project-a
      oc_project_b: project-b
workspaces:
  project-a:
    path: ~/work/project-a
  project-b:
    path: ~/work/project-b
chord_path: chord
idle_timeout: 30m
```

这个配置下：

- 所有微信消息都进入 `project-a`
- 飞书群 `oc_project_a` 进入 `project-a`
- 飞书群 `oc_project_b` 进入 `project-b`

推荐绑定飞书群到 workspace 的流程：

1. 暂时只保留一个 workspace，并移除 `chat_bindings`
2. 启动 gateway
3. 在飞书中创建目标群并添加机器人
4. 在群里发送一条纯文本消息
5. 在该聊天中执行 `/bind <workspace_id> <path>`
6. gateway 只会立即更新内存中的飞书 `chat_bindings` 和 `workspaces`，并把这两个部分的相同变更写回 `config.yaml`

如果你更喜欢手工编辑 YAML，也可以继续通过日志发现真实 `chat_id`：

```text
msg="feishu: received message" chat_id=oc_xxx open_id=ou_xxx ...
```

然后把 `chat_id=oc_xxx` 写回 `chat_bindings`。

手工修改配置文件后，仍需要重启 gateway 才会生效。

## 4. 启动

```bash
chord-gateway -f config.yaml
```

启动后：

1. 在已连接的 IM 聊天中发送 `/status`
2. 确认 gateway 能正确解析到预期 workspace，并成功连接 `chord headless`
3. 再发送普通文本开始与 Chord 交互

## 5. 状态文件位置

默认情况下，gateway 会把运行时状态存储到：

- macOS: `~/Library/Application Support/chord-gateway`
- Linux: `${XDG_STATE_HOME:-~/.local/state}/chord-gateway`
- 配置文件: `${XDG_CONFIG_HOME:-~/.config}/chord-gateway/config.yaml`

状态内容包括日志、微信 token 文件（默认 `<state_dir>/wechat/token.json`）、飞书去重数据和 session pin。飞书 `app_id`/`app_secret` 仍属于配置凭据；短期 access token 只保存在内存中并按需刷新。

## 6. 下一步文档

- [配置参考](./docs/configuration_CN.md)
- [IM 接入总览](./docs/im_CN.md)
- [使用指南](./docs/usage_CN.md)
- [运维说明](./docs/operations_CN.md)
- [故障排查](./docs/troubleshooting_CN.md)
