# 运维说明

本文档说明运行和运维 `chord-gateway` 时需要关注的行为。

## 进程生命周期

gateway 会按需为每个活跃绑定启动 `chord headless` 子进程。

每个 Chord 子进程会被放入独立进程组。gateway 正常关闭时，会尝试终止整个进程组：

1. 关闭 stdin，请求进程优雅退出。
2. 向进程组发送 `SIGTERM`。
3. 等待一小段宽限时间后，向进程组发送 `SIGKILL`。

这样可以避免孤儿 Chord 进程或其子进程继续持有 session lock 或工作区资源。

如果 gateway 自身被 `SIGKILL`（`kill -9`）杀死，则无法执行清理 handler。此时依赖 Chord 的 parent-death watcher 检测父进程变化并尽快退出。

## 空闲超时

`idle_timeout` 控制空闲 Chord 进程可保留多久。默认值为 `30m`。

使用 Go duration 语法，例如：

```yaml
idle_timeout: 30m
```

## 状态目录

运行态状态按以下优先级存储：

1. `$CHORD_GATEWAY_STATE_DIR`
2. `$XDG_STATE_HOME/chord-gateway`
3. `~/.local/state/chord-gateway`

状态数据包括：

- 日志
- 微信 token 文件（默认 `<state_dir>/wechat/token.json`，或 `ims[].wechat.token_path`）
- 飞书去重存储
- session pin 存储

## 配置文件解析

gateway 按以下优先级加载配置：

1. `--config` / `-f`
2. `$CHORD_GATEWAY_CONFIG`
3. `$XDG_CONFIG_HOME/chord-gateway/config.yaml`
4. `~/.config/chord-gateway/config.yaml`

## 日志

gateway 会把结构化日志写入 stderr，并写入状态目录下的滚动日志文件。

gateway 会记录关键路由阶段：

- `gateway event` – 从 `chord headless` stdout 解析出的原始事件
- `gateway routing event` – router 对某个绑定的处理决策
- `gateway sending notification` – 尝试向 IM 发送通知

常用字段包括：

- 事件类型
- workspace ID
- IM 类型
- chat ID
- session ID
- busy/idle 状态
- phase
- last outcome

## 飞书异步分发和去重

飞书 webhook 处理采用快速 ACK + 异步分发：

- webhook handler 校验并解析回调，然后尽快返回 `200 OK`。
- 实际路由到 `chord headless` 的操作通过进程内有界队列完成。
- 重复投递会由轻量去重存储过滤。

去重 key 基于：

```text
app_id + chat_id + message_id
```

去重数据持久化到：

```text
<state_dir>/dedupe.json
```

当前 TTL 为 24 小时。

## 工作区路由

微信始终路由到一个工作区。如果配置了多个工作区且启用了微信，需要通过 `ims[].wechat.workspace_id` 指定微信使用哪个工作区。

飞书支持多个工作区，可通过 `ims[].feishu.chat_bindings` 把飞书 `chat_id` 映射到 workspace ID。

飞书单工作区时可省略 `chat_bindings`，所有聊天使用该工作区。飞书多工作区时，请为需要路由的聊天配置 `chat_bindings`。

## 事件协议

gateway 运行 `chord headless` 并从 stdout 读取 JSONL 事件。默认订阅必需控制面事件：

- `idle`
- `assistant_message`
- `confirm_request`
- `question_request`
- `error`
- `notification`

可选事件由 `event_visibility` 控制。详见 [event-visibility_CN.md](./event-visibility_CN.md)。
