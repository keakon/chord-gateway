# 配置说明

本文档提供 `chord-gateway` 的字段级配置说明。

## 顶层字段

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|---|---|---|---|---|
| `im` | object | 否 | - | 单 IM 模式（向后兼容）。 |
| `ims` | array | 否 | - | 多 IM 模式。非空时优先于 `im`。 |
| `workspaces` | array | 是 | - | 工作区路由列表。 |
| `chord_path` | string | 否 | `chord` | chord 可执行文件路径，支持 `~` 展开。 |
| `idle_timeout` | string | 否 | `30m` | 空闲超时（Go duration 格式）。 |
| `wechat_workspace_id` | string | 否 | 第一个 workspace | 可选的微信工作区 ID。启用微信且存在多个 workspace 时必须设置。 |
| `event_visibility` | object | 否 | 全部 `false` | 可选控制面事件可见性。 |
| `session_pins_file` | string | 否 | `<state_dir>/session-pins.json` | 自定义会话 pin 文件路径。 |

## `im` / `ims[]`

### 通用字段

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `type` | string | 是 | `wechat` 或 `feishu` |
| `wechat` | object | 条件必填 | 当 `type=wechat` 时需要 |
| `feishu` | object | 条件必填 | 当 `type=feishu` 时需要 |

### 微信配置（`wechat`）

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|---|---|---|---|---|
| `base_url` | string | 否 | `https://ilinkai.weixin.qq.com` | 微信 iLink API 地址 |
| `bot_type` | string | 否 | `3` | iLink bot 类型 |

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

飞书访问控制行为：

- 如果 `owner_open_id` 和 `allowed_open_ids` 都未设置，则默认允许所有用户。
- 只要设置了其中任意一个字段，就只允许在 allowlist 中的 `open_id`。
- `owner_open_id` 会自动加入最终 allowlist。

## 工作区路由规则

每个 workspace 条目：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `id` | string | 是 | 工作区标识 |
| `path` | string | 是 | 项目目录（绝对路径或 `~`） |
| `im_chat_id` | string | 条件必填 | 飞书/其他 IM 的聊天绑定键 |

校验规则：

- 微信始终路由到一个工作区。
- 如果启用了微信且存在多个 workspace，必须通过 `wechat_workspace_id` 指定微信使用的 workspace。
- 飞书多工作区时，`im_chat_id` 必须非空且唯一。
- 飞书单工作区时，可省略 `im_chat_id`，所有飞书聊天都进入该 workspace。
- `im_chat_id` 对微信路由无效。

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
- `session_pins_file`、`chord_path` 和 `workspaces[].path` 支持 `~` 展开。

状态目录包含：

- 日志
- 微信 token 文件
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
im:
  type: feishu
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    verification_token: your-token
    listen: ":8080"
    webhook_path: /feishu/callback
    owner_open_id: ou_owner_xxx
    allowed_open_ids:
      - ou_owner_xxx
workspaces:
  - id: project-a
    path: ~/work/project-a
    im_chat_id: oc_project_a
  - id: project-b
    path: ~/work/project-b
    im_chat_id: oc_project_b
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
  - type: wechat
    wechat:
      base_url: https://ilinkai.weixin.qq.com
      bot_type: "3"
  - type: feishu
    feishu:
      app_id: cli_xxx
      app_secret: your-app-secret
      verification_token: your-token
      listen: ":8080"
      webhook_path: /feishu/callback
      owner_open_id: ou_owner_xxx
      allowed_open_ids:
        - ou_owner_xxx
wechat_workspace_id: project-a
workspaces:
  - id: project-a
    path: ~/work/project-a
    im_chat_id: oc_project_a
  - id: project-b
    path: ~/work/project-b
    im_chat_id: oc_project_b
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
