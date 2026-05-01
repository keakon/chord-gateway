# 配置说明

本文档提供 `chord-gateway` 的字段级配置说明。

## 顶层字段

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|---|---|---|---|---|
| `ims` | object | 是 | - | IM 适配器映射。支持的 key 为 `wechat` 和 `feishu`。 |
| `workspaces` | object | 是 | - | 以 workspace ID 为 key 的工作区映射。value 只定义工作区路径。 |
| `chord_path` | string | 否 | `chord` | chord 可执行文件路径，支持 `~` 展开。 |
| `idle_timeout` | string | 否 | `30m` | 空闲超时（Go duration 格式）。 |
| `event_visibility` | object | 否 | 全部 `false` | 可选控制面事件可见性。 |
| `session_pins_file` | string | 否 | `<state_dir>/session-pins.json` | 自定义会话 pin 文件路径。 |

推荐配置结构：

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
    workspace_id: default
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    chat_bindings:
      oc_project_a: project-a
      oc_project_b: project-b
workspaces:
  default:
    path: ~/project
  project-a:
    path: ~/work/project-a
  project-b:
    path: ~/work/project-b
```

为了兼容已有配置，加载时仍接受旧的 list 形式；但文档推荐格式和 `/bind` 写回格式都已改为上面的 map 结构。

## `ims`

支持的适配器 key：

- `wechat`
- `feishu`

每种适配器最多允许一个配置。

### 微信配置（`ims.wechat`）

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|---|---|---|---|---|
| `base_url` | string | 否 | `https://ilinkai.weixin.qq.com` | 微信 iLink API 地址 |
| `workspace_id` | string | 条件必填 | 第一个/唯一 workspace | 微信消息进入的 workspace。存在多个 workspace 时必须设置。 |
| `token_path` | string | 否 | `<state_dir>/wechat/token.json` | 自定义微信扫码登录 session token 的持久化路径。支持 `~` 展开。 |

说明：

- 二维码登录接口要求 `bot_type=3`。
- gateway 已将这个固定协议常量硬编码，不再暴露为配置项。
- 微信始终路由到一个 workspace。
- 微信扫码 token 属于运行时状态，不是静态配置。默认以 JSON 保存到 `<state_dir>/wechat/token.json`；只有在需要自定义密钥存储位置时才设置 `token_path`。

### 飞书配置（`ims.feishu`）

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|---|---|---|---|---|
| `app_id` | string | 是 | - | 飞书应用 ID |
| `app_secret` | string | 是 | - | 飞书应用密钥 |
| `owner_open_id` | string | 否 | - | owner open_id；如果设置，会自动加入有效 allowlist |
| `allowed_open_ids` | array | 否 | `[]` | 额外允许的 open_id 列表 |
| `chat_bindings` | object | 条件必填 | - | 从飞书 chat ID 到 workspace ID 的映射。存在多个 workspace 时必须设置。 |

飞书使用长连接模式接收事件：

- 不需要配置公网回调地址。
- 不需要配置 `verification_token`、`encrypt_key`、`listen` 或 `webhook_path`。
- 在飞书开放平台的“事件与回调”中，应选择“使用长连接接收事件”。
- 在事件订阅中订阅 `im.message.receive_v1`。
- 如果希望使用飞书交互卡片处理确认/提问流程，还需要在回调配置中添加 `card.action.trigger`。

飞书访问控制行为：

- 如果 `owner_open_id` 和 `allowed_open_ids` 都未设置，则默认允许所有用户——不做任何过滤。
- 只要设置了其中任意一个字段，就只处理在 allowlist 中的用户的消息和命令，其他用户的消息会被静默忽略。
- `owner_open_id` 会自动加入最终 allowlist。

如何获取 `open_id`：

1. 先不设置 `owner_open_id` 和 `allowed_open_ids` 启动 gateway（默认允许所有用户）。
2. 从飞书聊天中发送一条纯文本消息。
3. 在 gateway 日志中查找类似下面的记录：

```text
msg="feishu: received message" chat_id=oc_xxx open_id=ou_xxx message_id=om_xxx content=hello
```

4. 记录其中的 `open_id=ou_xxx`，将其设置为 `owner_open_id` 或添加到 `allowed_open_ids`。
5. 重启 gateway 使 allowlist 变更生效。（`/bind` 只更新飞书 `chat_bindings` 和 `workspaces`，不会重载 allowlist 字段。）

你也可以通过飞书管理后台或[飞书开放平台 API](https://open.feishu.cn/document/server-docs/authentication/access-token/tenant_access_token) 获取 `open_id`。

飞书路由行为：

- 如果只有一个 workspace，可以省略 `chat_bindings`，所有飞书聊天都进入该 workspace。
- 如果有多个 workspace，必须配置 `chat_bindings`。
- `chat_bindings` 的 key 是飞书聊天的真实 `chat_id`（例如群聊 `oc_xxx`），value 是你在 `workspaces` 下定义的 workspace ID。
- 每个 chat ID 都必须映射到一个有效的 workspace ID。
- 多个 chat ID 可以映射到同一个 workspace。
- 推荐先用单 workspace 启动 gateway，不填写 `chat_bindings`，然后直接在目标飞书聊天中执行 `/bind <workspace_id> <path>`。如果路径包含空格，请用英文双引号包裹，例如 `/bind project-a "~/work/project a"`。
- `/bind` 只接受两个参数。路径必须以 `/`、`~`、Windows 盘符前缀（如 `C:\\` 或 `C:/`）或 UNC 前缀（如 `\\\\server\\share`）开头；展开后必须可访问且必须是目录。否则命令会失败，且不会修改配置文件。
- `/bind` 只会立即更新运行中 gateway 的飞书 `chat_bindings` 和 `workspaces`，并把这两个部分的相同变更写回 YAML 配置文件；它不是通用配置重载入口。常见 YAML 注释会保留，但文件格式可能被重新编码为规范化格式。
- 手工修改配置文件仍需要重启 gateway 才会生效。
- 如果你更喜欢手工编辑 YAML，也可以继续从 gateway 日志中提取真实 `chat_id`，再自己填写 `chat_bindings`。

## `workspaces`

`workspaces` 是一个以 workspace ID 为 key 的映射：

```yaml
workspaces:
  project-a:
    path: ~/work/project-a
  project-b:
    path: ~/work/project-b
```

每个 workspace value 支持：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `path` | string | 是 | 项目目录（绝对路径或 `~`） |

校验规则：

- workspace ID 必须非空且唯一。
- workspace 路径会在使用前展开 `~`。
- 路由规则统一配置在各自 IM 适配器下，不放在 workspace 条目里。

## 路由模型

gateway 使用“IM 自带路由”模型：

- 微信通过 `workspace_id` 指定唯一 workspace。
- 飞书通过 `chat_bindings` 指定 chat 到 workspace 的映射。
- workspace 不包含任何 IM 特定路由字段。

这样 workspace 定义保持稳定，而每个 IM 自己声明“收到消息时该进入哪个 workspace”。

## `event_visibility`

可选布尔开关：

- `activity`
- `agent_done`
- `info`
- `toast`
- `tool_result`
- `todos`

核心事件始终订阅，无法关闭。

启用 `tool_result` 和 `todos` 后，它们也会作为 5 分钟长时间提醒中的内部进展事件计数来源。`activity` 会更新状态/调试用 phase，但长时间提醒不会直接暴露这些低层 phase。

## 配置与状态目录解析

配置文件优先级：

1. `--config` / `-f`
2. `CHORD_GATEWAY_CONFIG`
3. `$XDG_CONFIG_HOME/chord-gateway/config.yaml`
4. `~/.config/chord-gateway/config.yaml`

状态目录优先级：

1. `CHORD_GATEWAY_STATE_DIR`
2. `$XDG_STATE_HOME/chord-gateway`
3. `~/.local/state/chord-gateway`

额外路径行为：

- `CHORD_GATEWAY_LOG_FILE` 可以覆盖默认日志文件位置。
- 默认日志文件为 `<state_dir>/gateway.log`；达到 10 MiB 后轮转，保留 3 个备份，轮转后的日志不会 gzip 压缩。
- `session_pins_file`、`ims.wechat.token_path`、`chord_path` 和 `workspaces.<id>.path` 支持 `~` 展开。
- gateway 不会监听外部配置文件变更；配置只在启动时加载。
- `/bind` 是当前内置的唯一会立即更新内存中的飞书 `chat_bindings` 和 `workspaces`、并把这两个部分同步写回 YAML 配置文件的入口；其他配置变更仍需要重启。`/bind` 要求路径是已存在的目录，并可能在保留常见注释的同时规范化 YAML 格式。

状态目录包含：

- 日志
- 微信 token 文件（默认 `<state_dir>/wechat/token.json`）
- 飞书去重存储（`<state_dir>/dedupe.json`）
- 会话 pin（默认 `<state_dir>/session-pins.json`）

## Session pin 行为

gateway 会按绑定存储一个 pin 的 Chord session ID，绑定键为：

```text
workspaceID | imType | chatID
```

这意味着即使指向同一个 workspace，不同群聊和不同 IM 也会保留各自独立的 pinned session。

如果未设置 `session_pins_file`，默认路径为 `<state_dir>/session-pins.json`。

## 示例：用 `/bind` 绑定飞书聊天到 workspace

推荐流程：

1. 先用单 workspace 配置启动 gateway，不要填写 `chat_bindings`。
2. 在飞书中创建目标群聊，并把应用机器人添加到该群。
3. 在这个群里发送一条纯文本消息。
4. 直接在该聊天中执行 `/bind <workspace_id> <path>`。
5. gateway 只会立即更新内存中的飞书 `chat_bindings` 和 `workspaces`，并把对应的绑定/workspace 变更写回 YAML 配置文件。

`/bind` 只接受两个参数。路径必须以 `/`、`~`、Windows 盘符前缀（如 `C:\\` 或 `C:/`）或 UNC 前缀（如 `\\\\server\\share`）开头；展开后必须可访问且必须是目录。常见 YAML 注释会保留，但文件格式可能被重新编码为规范化格式。

示例命令：

```text
/bind project-a ~/work/project-a
```

手工编辑 YAML 仍然支持，但修改后仍需要重启 gateway 才会生效。

## 示例：发现飞书群聊 `chat_id`

当你更希望手工填写 `chat_bindings` 时，推荐按下面的顺序操作：

1. 先用单 workspace 配置启动 gateway，不要填写 `chat_bindings`。
2. 在飞书中创建目标群聊，并把应用机器人添加到该群。
3. 在这个群里发送一条纯文本消息（如果企业权限只允许群里 `@` 机器人消息，请使用 `@机器人 你的消息`）。
4. 在 gateway 日志中查找类似下面的记录：

```text
msg="feishu: received message" chat_id=oc_xxx open_id=ou_xxx message_id=om_xxx content=hello
```

5. 记录其中的 `chat_id=oc_xxx`，并把它填回配置：

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    chat_bindings:
      oc_xxx: project-a
workspaces:
  project-a:
    path: ~/work/project-a
```

其中：

- `oc_xxx` 是飞书群聊的真实 `chat_id`
- `project-a` 是你自己在 `workspaces` 下定义的 workspace ID

如果是新的群聊：

- 单 workspace 且 `chat_bindings` 为空时，新群会自动进入这个唯一 workspace
- 多 workspace 时，新群如果没有写入 `chat_bindings`，gateway 会回复未绑定 workspace 的错误，并在日志中标明对应 `chat_id`

## 示例：飞书多工作区

当你希望不同飞书群聊进入不同 workspace 时，可使用：

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner_open_id: ou_owner_xxx
    allowed_open_ids:
      - ou_owner_xxx
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
event_visibility:
  activity: false
  agent_done: false
  info: false
  toast: false
  tool_result: false
  todos: false
```

## 示例：微信 + 飞书多工作区

当你希望微信固定一个 workspace，而飞书不同群绑定到不同 workspace 时，可使用：

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
    workspace_id: project-a
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner_open_id: ou_owner_xxx
    allowed_open_ids:
      - ou_owner_xxx
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
event_visibility:
  activity: false
  agent_done: false
  info: false
  toast: false
  tool_result: false
  todos: true
```

在这个示例中：

- 所有微信消息都进入 `project-a`
- 飞书群 `oc_project_a` 进入 `project-a`
- 飞书群 `oc_project_b` 进入 `project-b`
