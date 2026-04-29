# 配置说明

本文档提供 `chord-gateway` 的字段级配置说明。

## 顶层字段

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|---|---|---|---|---|
| `ims` | array | 是 | - | IM 适配器列表。每个条目只包含一个平台块。 |
| `workspaces` | array | 是 | - | 工作区列表。这里只定义工作区本身，不放 IM 路由字段。 |
| `chord_path` | string | 否 | `chord` | chord 可执行文件路径，支持 `~` 展开。 |
| `idle_timeout` | string | 否 | `30m` | 空闲超时（Go duration 格式）。 |
| `event_visibility` | object | 否 | 全部 `false` | 可选控制面事件可见性。 |
| `session_pins_file` | string | 否 | `<state_dir>/session-pins.json` | 自定义会话 pin 文件路径。 |

## `ims[]`

`ims` 中的每个条目必须只包含一个平台对象。

示例：

```yaml
ims:
  - wechat:
      base_url: https://ilinkai.weixin.qq.com
      workspace_id: default

  - feishu:
      app_id: cli_xxx
      app_secret: your-app-secret
      chat_bindings:
        oc_project_a: project-a
        oc_project_b: project-b
```

### 微信配置（`wechat`）

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|---|---|---|---|---|
| `base_url` | string | 否 | `https://ilinkai.weixin.qq.com` | 微信 iLink API 地址 |
| `workspace_id` | string | 条件必填 | 第一个 workspace | 微信消息进入的 workspace。存在多个 workspace 时必须设置。 |
| `token_path` | string | 否 | `<state_dir>/wechat/token.json` | 自定义微信扫码登录 session token 的持久化路径。支持 `~` 展开。 |

说明：

- 二维码登录接口要求 `bot_type=3`。
- gateway 已将这个固定协议常量硬编码，不再暴露为配置项。
- 微信始终路由到一个 workspace。
- 微信扫码 token 属于运行时状态，不是静态配置。默认以 JSON 保存到 `<state_dir>/wechat/token.json`；只有在需要自定义密钥存储位置时才设置 `token_path`。
- 旧版本文件 `<state_dir>/token.json` 和 `<state_dir>/sync-buf.json` 会自动迁移到 `wechat/` 状态子目录。

### 飞书配置（`feishu`）

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|---|---|---|---|---|
| `app_id` | string | 是 | - | 飞书应用 ID |
| `app_secret` | string | 是 | - | 飞书应用密钥 |
| `verification_token` | string | 否 | - | Webhook 校验 token |
| `encrypt_key` | string | 否 | - | 事件加密 key |
| `listen` | string | 否 | `:8080` | HTTP 监听地址 |
| `webhook_path` | string | 否 | `/feishu/callback` | Webhook 路径 |
| `owner_open_id` | string | 否 | - | owner open_id；如果设置，会自动加入有效 allowlist |
| `allowed_open_ids` | array | 否 | `[]` | 额外允许的 open_id 列表 |
| `chat_bindings` | object | 条件必填 | - | 从飞书 chat ID 到 workspace ID 的映射。存在多个 workspace 时必须设置。 |

飞书访问控制行为：

- 如果 `owner_open_id` 和 `allowed_open_ids` 都未设置，则默认允许所有用户。
- 只要设置了其中任意一个字段，就只允许在 allowlist 中的 `open_id`。
- `owner_open_id` 会自动加入最终 allowlist。

飞书路由行为：

- 如果只有一个 workspace，可以省略 `chat_bindings`，所有飞书聊天都进入该 workspace。
- 如果有多个 workspace，必须配置 `chat_bindings`。
- 每个 chat ID 都必须映射到一个有效的 workspace ID。
- 多个 chat ID 可以映射到同一个 workspace。

## `workspaces[]`

每个 workspace 条目：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `id` | string | 是 | 工作区标识 |
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
- 默认日志文件为 `<state_dir>/gateway.log`。
- `session_pins_file`、`ims[].wechat.token_path`、`chord_path` 和 `workspaces[].path` 支持 `~` 展开。

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

## 示例：飞书多工作区

当你希望不同飞书群聊进入不同 workspace 时，可使用：

```yaml
ims:
  - feishu:
      app_id: cli_xxx
      app_secret: your-app-secret
      verification_token: your-token
      listen: ":8080"
      webhook_path: /feishu/callback
      owner_open_id: ou_owner_xxx
      allowed_open_ids:
        - ou_owner_xxx
      chat_bindings:
        oc_project_a: project-a
        oc_project_b: project-b
workspaces:
  - id: project-a
    path: ~/work/project-a
  - id: project-b
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
  - wechat:
      base_url: https://ilinkai.weixin.qq.com
      workspace_id: project-a
  - feishu:
      app_id: cli_xxx
      app_secret: your-app-secret
      verification_token: your-token
      listen: ":8080"
      webhook_path: /feishu/callback
      owner_open_id: ou_owner_xxx
      allowed_open_ids:
        - ou_owner_xxx
      chat_bindings:
        oc_project_a: project-a
        oc_project_b: project-b
workspaces:
  - id: project-a
    path: ~/work/project-a
  - id: project-b
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
