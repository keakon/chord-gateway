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

这些事件提供 IM 控制所需的最小行为：

- 最终 assistant 回复
- 权限确认
- 用户问题
- busy/idle 状态聚合
- 错误报告
- 面向用户的标准通知

## 可选可见事件

可选事件默认关闭。通过 `event_visibility` 启用：

```yaml
event_visibility:
  activity: false
  agent_done: false
  info: false
  toast: false
  tool_result: false
  todos: false
```

| 字段 | 事件类型 | 典型用途 |
|---|---|---|
| `activity` | `activity` | 较低层进度细节 |
| `agent_done` | `agent_done` | 子 agent 完成通知 |
| `info` | `info` | 信息类消息 |
| `toast` | `toast` | 短暂提示消息 |
| `tool_result` | `tool_result` | 工具结果摘要 |
| `todos` | `todos` | Todo 列表更新 |

## 完成通知

`assistant_message` 是发送到 IM 的主要最终回复。

`notification` 是用户提醒的标准事件，包括权限请求、问题请求、blocked 错误和完全停止后的完成通知。

如果 `idle` 事件显示任务完成，但此前没有发送最终 assistant 文本或完成通知，gateway 可以发送兜底完成消息，确保 IM 用户获得明确的结束状态。

## 日志

为便于观测，gateway 日志会包含以下路由阶段：

- `gateway event`
- `gateway routing event`
- `gateway sending notification`

日志详情见 [operations_CN.md](./operations_CN.md)。
