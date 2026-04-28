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

- `workspaces[].path` 不存在。
- 配置了多个 workspace 并启用了微信，但没有设置 `wechat_workspace_id`。
- 飞书配置了多个 workspace，但某些条目缺少 `im_chat_id`。
- 飞书多 workspace 中复用了相同的 `im_chat_id`。

## 微信问题

### 没有出现二维码登录

检查：

- `im.type` 是否为 `wechat`，或 `ims` 中是否存在 WeChat 条目。
- `base_url` 是否正确。
- 已保存的 token 文件是否过期或损坏。
- 状态目录下的 gateway 日志。

如果需要强制重新登录，停止 gateway 后删除状态目录中的微信 token 文件。

### Token 过期

在多 IM 模式下，可以从另一个活跃渠道发送：

```text
/login weixin
```

然后打开返回的登录链接并扫码。

## 飞书问题

### 飞书事件没有到达 gateway

检查：

- 配置的 `listen` 地址是否能被飞书访问。
- 公网 URL 路径是否匹配 `webhook_path`。
- HTTPS / 反向代理转发是否正确。
- 防火墙或云安全组是否允许入站流量。
- 飞书应用是否启用了事件订阅。
- 订阅的事件类型是否包含 `im.message.receive_v1` 和 `card.action.trigger`。
- 回调 URL 是否已经通过飞书 URL 校验。

### 校验失败

检查 `verification_token` 是否与飞书开发者后台一致。如果启用了事件加密，也要检查 `encrypt_key`。

如果在保存回调 URL 后立即校验失败，再检查：

- 回调路径是否与 `webhook_path` 完全一致
- 公网端点是否能被飞书访问
- 配置的 `verification_token` 是否与 challenge 请求中的 token 一致

### 消息被忽略

检查：

- 如果配置了 allowlist，发送者是否被 `owner_open_id` / `allowed_open_ids` 允许
- 飞书入站消息是否为纯文本；非文本消息会被忽略
- 目标聊天是否路由到了预期 workspace

可以从飞书开发者后台或事件 payload 中获取发送者 `open_id`，并加入 allowlist。

### 重复消息

飞书可能重试回调。gateway 会按 `app_id + chat_id + message_id` 去重，并把数据存储在 `<state_dir>/dedupe.json`。

## 路由问题

### 消息进入了错误 workspace

飞书多 workspace 模式下，请确认每个 workspace 都配置了预期的 `im_chat_id`，且每个值唯一。

飞书单 workspace 且未设置 `im_chat_id` 时，所有聊天会使用第一个 workspace。

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

日志、token、去重数据和 session pin 都存储在这里。

## 仍然无法解决

提交问题时请提供：

- gateway 版本或 commit
- OS 和架构
- Go 版本
- Chord 版本或 commit
- 脱敏后的配置
- 相关 gateway 日志
- 使用的 IM 平台和模式
