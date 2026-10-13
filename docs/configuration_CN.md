# 配置说明

所有 IM 平台统一使用单 owner 配对与显式聊天绑定，单工作区也需绑定。首次运行 `chord-gateway --pair`，在机器人私聊发送 `/bind "~/workspace" <pairing_code>`；详见[配对与本机重置](./pairing_CN.md)。旧部署升级步骤见[更新日志](../CHANGELOG_CN.md)。

本文档提供 `chord-gateway` 的字段级配置说明。

## 顶层字段

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|---|---|---|---|---|
| `ims` | object | 是 | - | IM 适配器映射。支持的 key 为 `wechat` 和 `feishu`。 |
| `workspaces` | object | 否 | - | 以 workspace ID 为 key 的工作区映射。value 只定义工作区路径。 |
| `chord_path` | string | 否 | `chord` | chord 可执行文件路径，支持 `~` 展开。 |
| `chord_state_dir` | string | 否 | Chord 默认状态目录 | managed headless 与 takeover CLI 共用的 Chord 状态目录；TUI 使用自定义 `--state-dir` 时设置相同路径。支持 `~` 展开。 |
| `idle_timeout` | string | 否 | `30m` | 空闲超时（Go duration 格式）。 |
| `event_visibility` | object | 否 | 除 `background_result`（`true`）外全部 `false` | 控制面事件可见性。 |
| `session_pins_file` | string | 否 | `<state_dir>/session-pins.json` | 自定义会话 pin 文件路径。 |

推荐配置结构：

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
    owner: {account_id: bot_example, user_id: user_example}
    chat_bindings:
      user_example: default
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner: {account_id: cli_xxx, user_id: ou_owner_xxx}
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

配置文件中的 `ims` 和 `workspaces` 现在都必须使用上面的 map 结构；旧的 list / sequence 形式已不再支持，`/bind` 也只会读写这种 map 结构。

## `ims`

支持的适配器 key：

- `wechat`
- `feishu`

每种适配器最多允许一个配置。

### 微信配置（`ims.wechat`）

任务型指南：[微信 iLink 接入指南](./wechat_CN.md)

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|---|---|---|---|---|
| `base_url` | string | 否 | `https://ilinkai.weixin.qq.com` | 微信 iLink API 地址 |
| `owner` | object | 配对后 | - | `account_id`, `user_id` |
| `chat_bindings` | object | 绑定后 | - | chat ID → workspace ID |
| `token_path` | string | 否 | `<state_dir>/wechat/token.json` | 自定义微信扫码登录 session token 的持久化路径。支持 `~` 展开。 |

说明：

- 二维码登录接口要求 `bot_type=3`。
- gateway 已将这个固定协议常量硬编码，不再暴露为配置项。
- 微信始终路由到一个 workspace。
- 微信扫码 token 属于运行时状态，不是静态配置。默认以 JSON 保存到 `<state_dir>/wechat/token.json`；只有在需要自定义密钥存储位置时才设置 `token_path`。

### 飞书配置（`ims.feishu`）

任务型指南：[飞书接入指南](./feishu_CN.md)

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|---|---|---|---|---|
| `app_id` | string | 是 | - | 飞书应用 ID |
| `app_secret` | string | 是 | - | 飞书应用密钥 |
| `owner` | object | 配对后 | - | `account_id`, `user_id` |
| `chat_bindings` | object | 条件必填 | - | 从飞书 chat ID 到 workspace ID 的映射。存在多个 workspace 时必须设置。 |

飞书使用长连接模式接收事件：

- 不需要配置公网回调地址。
- 不需要配置 `verification_token`、`encrypt_key`、`listen` 或 `webhook_path`。
- 在飞书开放平台的“事件与回调”中，应选择“使用长连接接收事件”。
- 在事件订阅中订阅 `im.message.receive_v1`。
- 如果希望使用飞书交互卡片处理确认/提问流程，还需要在回调配置中添加 `card.action.trigger`。

### 授权与绑定

两个平台都只接受配对 owner 的输入。首次运行 `chord-gateway --pair`，在机器人私聊发送 `/bind "~/workspace" <pairing_code>`；已有 owner 在新聊天中发送 `/bind "~/workspace"`。所有聊天都必须显式绑定，单工作区也不例外。

命令会注册已存在的目录，并在持久保存 YAML 后启用聊天绑定。目录按真实文件系统身份复用，不会创建或克隆目录；切换目录前必须处理完待执行输入、后台任务和交互。路径和恢复规则见[配对指南](./pairing_CN.md)。手工修改配置需先停服务，再修改并重启。

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

- 微信和飞书都通过 `chat_bindings` 指定聊天到工作区的映射。
- workspace 不包含任何 IM 特定路由字段。

这样 workspace 定义保持稳定，而每个 IM 自己声明“收到消息时该进入哪个 workspace”。

## `event_visibility`

控制面事件开关：

- `agent_started`
- `agent_notify`
- `background_result`
- `context_notice`
- `info`
- `toast`
- `todos`

核心事件始终订阅，无法关闭。其中包括 `agent_done`，因此只有工具调用、没有 assistant 文本的 SubAgent 完成轮次也一定会展示摘要。

大部分开关默认关闭。`background_result` 是例外：后台任务完成结果默认推送到聊天，设置 `background_result: false` 可关闭推送；结果仍保留在 Chord 会话中。

`agent_started` 与 `agent_notify` 为避免编排更新刷屏而默认关闭。`done_completion` 始终订阅，用于非 loop Done 完成报告。`todos` 仅在启用后订阅、计数和转发。`activity` 始终订阅，用于跟踪实际工作状态，但长时间提醒不会直接暴露低层 phase。`context_notice` 默认关闭，因为把持久化的上下文压力提示推到聊天里属于产品取舍、而不是正确性要求；推送量与「撤回不生效」的说明见 [event-visibility_CN.md](./event-visibility_CN.md)。

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
- `session_pins_file`、`ims.wechat.token_path`、`chord_path`、`chord_state_dir` 和 `workspaces.<id>.path` 支持 `~` 展开。
- gateway 不会监听外部配置文件变更；配置只在启动时加载。
- 配置中不支持或已移除的 YAML 字段（包括通过锚点和合并继承的字段）会带源字段行号记录到 gateway 日志并被忽略；文件其余部分仍会正常加载。
- `/bind` 是当前内置的唯一会立即更新内存中的飞书 `chat_bindings`、并把绑定同步写回 YAML 配置文件的入口；其他配置变更仍需要重启。`/bind` 要求路径是已存在的目录，并可能在保留常见注释的同时规范化 YAML 格式。

状态目录包含：

- 日志
- 微信 token 文件（默认 `<state_dir>/wechat/token.json`）
- 飞书去重存储（`<state_dir>/dedupe.json`）
- 会话 pin（默认 `<state_dir>/session-pins.json`）
- 会话移交阻断（`<state_dir>/session-transfers.json`）

`chord_state_dir` 指向 Chord 自己的状态目录，与上面的 gateway 状态目录不同。不设置时沿用 Chord 默认值；如果源 TUI 使用自定义目录，应填写相同路径，让接管查询与目标恢复访问同一份会话和运行时记录。它不会改变 IM 或工作区授权。

## Session pin 行为

gateway 会按绑定存储一个 pin 的 Chord session ID，绑定键为：

```text
platform + account + owner + workspace + chat (encoded)
```

这意味着即使指向同一个 workspace，不同群聊和不同 IM 也会保留各自独立的 pinned session。

如果未设置 `session_pins_file`，默认路径为 `<state_dir>/session-pins.json`。

移交阻断优先于 pin。移交开始后，普通输入和自动重启都不能恢复该 pin，也不会自动改用新会话。阻断在 gateway 重启后仍然保留。接管到当前聊天的会话确认 `ready_paused` 且 pin 保存成功后，目标绑定会开放输入，等待用户明确继续。已经把会话移交出去的绑定继续阻断，直到显式 `/new` 或 `/resume` 成功；失败时保留阻断。详见[会话接管](./usage_CN.md#会话接管)。

## 飞书绑定流程

`/bind` 的任务型操作步骤见[飞书指南 — 多工作区路由](./feishu_CN.md#多工作区路由与-bind)。本参考页只说明上文 `chat_bindings` 字段的含义与约束。

## 示例：飞书多工作区

当你希望不同飞书群聊进入不同 workspace 时，可使用：

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner: {account_id: cli_xxx, user_id: ou_owner_xxx}
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
  background_result: true
  agent_started: false
  agent_notify: false
  context_notice: false
  info: false
  toast: false
  todos: false
```

## 示例：微信 + 飞书多工作区

当你希望微信固定一个 workspace，而飞书不同群绑定到不同 workspace 时，可使用：

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
    owner: {account_id: bot_example, user_id: user_example}
    chat_bindings:
      user_example: project-a
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner: {account_id: cli_xxx, user_id: ou_owner_xxx}
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
  background_result: true
  agent_started: false
  agent_notify: false
  context_notice: false
  info: false
  toast: false
  todos: true
```

在这个示例中：

- 已绑定的微信消息都进入 `project-a`
- 飞书群 `oc_project_a` 进入 `project-a`
- 飞书群 `oc_project_b` 进入 `project-b`
