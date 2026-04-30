# 故障排查

本文档覆盖 `chord-gateway` 启动、IM 集成、路由和运行时常见问题。

## 启动失败

### `load config` 错误

先确认 gateway 使用的是预期配置文件。解析顺序：

1. `--config` / `-f`
2. `$CHORD_GATEWAY_CONFIG`
3. `$XDG_CONFIG_HOME/chord-gateway/config.yaml`
4. `~/.config/chord-gateway/config.yaml`

调试时建议显式指定配置：

```bash
chord-gateway -f ./config.yaml
```

### 找不到 `chord`

gateway 会直接执行 `chord_path`。shell alias 不会展开。

可使用以下方式之一：

- 把 Chord 二进制所在目录加入 `PATH`。
- 在 `chord_path` 中配置绝对路径。
- 使用 gateway 支持的 `~` 路径。

示例：

```yaml
chord_path: ~/go/bin/chord
```

### Workspace 校验失败

常见原因：

- `workspaces.<id>.path` 不存在。
- 配置了多个 workspace 并启用了微信，但没有设置 `ims.wechat.workspace_id`。
- 飞书配置了多个 workspace，但缺少或漏配了 `ims.feishu.chat_bindings`。
- 飞书 `chat_bindings` 中的某个映射指向了错误的 workspace ID。

## 微信问题

### 没有出现二维码登录

检查：

- `ims` 中存在 WeChat 条目。
- `base_url` 是否正确。
- 已保存的 token 文件是否过期或损坏。默认路径是 `<state_dir>/wechat/token.json`；如果配置了 `ims.wechat.token_path`，请检查该自定义路径。
- 状态目录下的 gateway 日志。

如果需要强制重新登录，停止 gateway 后删除微信 token 文件（默认 `<state_dir>/wechat/token.json`；如配置了 `ims.wechat.token_path`，则删除该自定义路径）。

### Token 过期

微信 token 过期时，在多 IM 模式下可以从另一个活跃渠道发送：

```text
/login wechat
```

然后打开返回的登录链接并扫码。当 gateway 自身检测到微信 token 过期时，会自动清理已保存 token 并启动二维码登录。

飞书不需要在会话中登录或手动续期；gateway 会使用配置中的应用凭证自动获取和刷新飞书 access token。如果飞书连接失效，请检查部署配置和飞书开放平台中的应用凭证、权限、事件订阅与长连接设置，不要在 IM 会话中发送或修改 `app_id` / `app_secret`。

## 飞书问题

### 飞书长连接未建立或收不到事件

检查：

- gateway 是否已启动并保持运行。
- 飞书应用是否在“事件与回调”中选择了“使用长连接接收事件”。
- 订阅的事件类型是否包含 `im.message.receive_v1`。
- 回调配置中是否添加了 `card.action.trigger` 用于卡片交互。
- 飞书应用是否已发布最新版本。
- 机器是否可以主动访问飞书公网服务。

### 长连接配置失败

检查飞书开放平台中事件订阅方式是否已切换为“使用长连接接收事件”，并确认 gateway 已启动、`app_id`/`app_secret` 正确。

### 消息被忽略

检查：

- 如果配置了 allowlist，发送者是否被 `owner_open_id` / `allowed_open_ids` 允许
- 飞书入站消息是否为纯文本；非文本消息会被忽略
- 目标聊天是否路由到了预期 workspace

可以从飞书开发者后台或事件 payload 中获取发送者 `open_id`，并加入 allowlist。

### 如何把飞书群绑定到 workspace

推荐步骤：

1. 先用单 workspace 配置启动 gateway，不填写 `chat_bindings`
2. 在飞书中创建目标群聊并把机器人加入该群
3. 在群里发送一条纯文本消息
4. 直接在该聊天中执行：

```text
/bind <workspace_id> <path>
```

5. gateway 只会立即更新内存中的飞书 `chat_bindings` 和 `workspaces`，并把对应的绑定/workspace 变更写回 YAML 配置文件

`/bind` 只接受两个参数。路径必须以 `/`、`~`、Windows 盘符前缀（如 `C:\\` 或 `C:/`）或 UNC 前缀（如 `\\\\server\\share`）开头；展开后必须可访问且必须是目录。多余参数或不可访问路径都会返回错误，且不会修改配置文件。

示例：

```text
/bind project-a ~/work/project-a
```

手工编辑 YAML 仍然可行，但修改后仍需要重启 gateway 才会生效。

### 如何找到飞书群聊 `chat_id`

推荐步骤：

1. 先用单 workspace 配置启动 gateway，不填写 `chat_bindings`
2. 在飞书中创建目标群聊并把机器人加入该群
3. 在群里发送一条纯文本消息
4. 在 gateway 日志中查找：

```text
msg="feishu: received message" chat_id=oc_xxx open_id=ou_xxx message_id=om_xxx content=hello
```

其中 `chat_id=oc_xxx` 就是这个群的真实 `chat_id`。

如果有多个 workspace，且你更希望手工编辑 YAML，再把它写回：

```yaml
chat_bindings:
  oc_xxx: your-workspace-id
```

### 重复消息

飞书可能重试事件投递。gateway 会按 `app_id + chat_id + message_id` 去重，并把数据存储在 `<state_dir>/dedupe.json`。

## 路由问题

### 消息进入了错误 workspace

飞书多 workspace 模式下，请确认 `ims.feishu.chat_bindings` 把每个飞书 chat ID 映射到预期 workspace ID。

飞书单 workspace 且未设置 `chat_bindings` 时，所有聊天会使用该 workspace。

### Session 上下文不是预期内容

使用：

```text
/current
/sessions
```

然后用 `/new` 清除当前绑定，或用 `/resume <id>` pin 到已知 session。

## 运行时问题

### Chord 没有回复

检查：

- gateway 日志中是否有进程启动或 stdout 解析错误。
- Chord 日志和 session 状态。
- 是否有待确认或待回答问题（`/status`）。
- 进程是否被取消或超时。

### 迟到的 `/answer`、`/allow` 或 `/deny` 被作为后续上下文发送

当 Chord 进入 idle，或 gateway 在 `idle_timeout` 后清理空闲进程时，待回答问题或待确认请求可能过期。此时原 request ID 已不再处于 pending 状态。

预期行为：

- gateway 清理待回答问题或待确认请求时，会发送英文失效提示。
- 迟到的 `/answer` 会作为普通后续消息转发，而不是作为原结构化回答提交。
- 迟到的 `/allow` 或 `/deny` 只会作为后续上下文转发，不会被当作批准或拒绝执行。
- 如果 gateway 仍缓存有已过期的问题或确认内容，会把这些上下文一起发送给 Chord。

如果仍需继续原操作，请让 Chord 重新发起请求，以生成新的待回答问题或待确认请求。

### 任务看起来卡住

先发送 `/status`。如果应该停止任务，发送：

```text
/cancel
```

如果进程仍卡住，重启 gateway 并检查是否有孤儿 Chord 进程。

### 日志和状态文件在哪里？

状态目录优先级：

1. `$CHORD_GATEWAY_STATE_DIR`
2. `$XDG_STATE_HOME/chord-gateway`
3. `~/.local/state/chord-gateway`

日志、token、去重数据和 session pin 都存储在这里。微信 token 默认位于 `<state_dir>/wechat/token.json`。

## 仍然无法解决

提交问题时请提供：

- gateway 版本或 commit
- OS 和架构
- Go 版本
- Chord 版本或 commit
- 脱敏后的配置
- 相关 gateway 日志
- 使用的 IM 平台和模式
