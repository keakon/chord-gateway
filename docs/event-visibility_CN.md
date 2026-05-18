# Gateway 控制面事件可见性

`chord-gateway` 从 `chord headless` 读取 JSONL 事件。必需控制面事件始终订阅并路由；可选事件由 `event_visibility` 控制。

- English: [event-visibility.md](./event-visibility.md)

## 必需事件

gateway 始终订阅以下事件：

- `assistant_message`
- `confirm_request`
- `question_request`
- `idle`
- `error`
- `notification`
- `done_completion`

这些事件提供 IM 控制所需的最小行为：

- 最终 assistant 回复
- 权限确认
- 用户问题
- busy/idle 状态聚合
- 错误报告
- 面向用户的标准通知
- Done 工具完成状态，以及 gateway 使用的其他工具结果状态

## 可选可见事件

可选事件默认关闭。通过 `event_visibility` 启用：

```yaml
event_visibility:
  activity: false
  agent_done: false
  info: false
  toast: false
  todos: false
```

| 字段 | 事件类型 | 典型用途 |
|---|---|---|
| `activity` | `activity` | 较低层进度细节。gateway 会记录 phase 状态，但长时间提醒不会直接暴露这些 phase。 |
| `agent_done` | `agent_done` | 子 agent 完成通知 |
| `info` | `info` | 信息类消息 |
| `toast` | `toast` | 短暂提示消息 |
| `todos` | `todos` | 完整 Todo 列表更新；每个事件都会完整转发且不去重，并会计入长时间提醒的内部事件数 |

## 长时间提醒

当一个 turn 仍处于 busy 状态时，gateway 会每 5 分钟发送一次简短提醒。只要有新的用户可见输出，下一次 5 分钟提醒窗口就会重新计时。提醒不会包含 `connecting` 这类低层 phase。如果期间观察到已跟踪的内部进展，会显示自上次用户可见输出或提醒以来的内部事件数：

```text
⏳ Still working (4 internal events)
```

内部事件数目前基于 gateway 已跟踪的进展事件，例如 `todos`。启用 `event_visibility.todos` 后，每个 `todos` 事件都会以完整的当前 todo 列表推送，不做去重，即使列表未变化或为空也会推送。

## 完成通知

`assistant_message` 是发送到 IM 的主要最终回复。

`notification` 是用户提醒的标准事件，包括权限请求、问题请求、blocked 错误和完全停止后的完成通知。

`idle` 事件通常不会触发兜底完成消息。如果某个 `idle` 事件清理了待回答问题或待确认请求，gateway 会发送针对性的英文失效提示，而不是发送通用完成消息。gateway 在清理仍带有待回答问题或待确认请求的空闲进程前，也会发送同样的失效提示。

## 日志

为便于观测，gateway 日志会包含以下路由阶段：

- `gateway event`
- `gateway routing event`
- `gateway sending notification`

日志详情见 [operations_CN.md](./operations_CN.md)。
