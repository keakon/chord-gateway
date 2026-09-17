# Gateway 控制面事件可见性

`chord-gateway` 从 `chord headless` 读取 JSONL 事件。必需控制面事件始终订阅并路由；可选事件由 `event_visibility` 控制。

- English: [event-visibility.md](./event-visibility.md)

## 必需事件

gateway 始终订阅以下事件：

- `assistant_message`
- `confirm_request`
- `question_request`
- `handoff_request`
- `handoff_cancelled`
- `idle`
- `error`
- `notification`
- `done_completion`
- `local_shell_result`
- `role_change`
- `agent_done`
- `compaction_status`
- `session_switched`
- `background_result`

这些事件提供 IM 控制所需的最小行为：

- 最终 assistant 回复
- 权限确认
- 用户问题
- handoff 请求和响应
- handoff 取消通知
- busy/idle 状态聚合
- 错误报告
- 面向用户的标准通知
- 非 loop Done 完成报告
- 本地 shell 命令结果
- 供 `/status` 跟踪当前主角色
- SubAgent 完成摘要
- 供 `/status` 展示最近一次上下文检查点结果
- 进程内会话切换，使已 pin 的绑定跟随运行时实际使用的会话
- 后台任务完成结果，没有其它投递通道

`session_switched` 只更新状态：触发切换的命令本身已经回复了用户，不再额外发消息。它会把绑定的 session pin 指向新的活动会话，之后重新拉起进程会 resume 运行时实际使用的会话。

`background_result` 会作为聊天消息推送到 IM，因为 Chord 侧没有其它通道：后台任务通常在 turn 进入 idle 之后才完成，之后不会有 `assistant_message` 再总结它。

## 可选可见事件

可选事件默认关闭。通过 `event_visibility` 启用：

```yaml
event_visibility:
  activity: false
  agent_started: false
  agent_notify: false
  context_notice: false
  info: false
  toast: false
  todos: false
```

| 字段 | 事件类型 | 典型用途 |
|---|---|---|
| `activity` | `activity` | 较低层进度细节。gateway 会记录 phase 状态，但长时间提醒不会直接暴露这些 phase。 |
| `agent_started` | `agent_started` | SubAgent 委托开始通知 |
| `agent_notify` | `agent_notify` | 面向 owner 或指定委派工作流的非阻塞更新 |
| `context_notice` | `context_notice` | 持久化的上下文压力提示，作为聊天消息推送。`level` 取值为 `pressure`、`imminent` 或 `warning`。 |
| `info` | `info` | 信息类消息 |
| `toast` | `toast` | 短暂提示消息 |
| `todos` | `todos` | 完整 Todo 列表更新；每个事件都会完整转发且不去重，并会计入长时间提醒的内部事件数 |

`context_notice` 默认关闭，因为它的聊天投递属于产品取舍、而不是正确性要求。一次压缩周期最多在三个递进级别上告警（`pressure`、`imminent`、`warning`）；Chord 每个级别只发一次并抑制重复，所以量是有界的——但这类通知是「发出去就不管」的：当 Chord 撤回某条通知时（例如切换模型后有效阈值变化），已经发到聊天里的那条不会撤回，因此长期开启该开关的聊天可能保留一条过期提示，直到它被翻走。

## 长时间提醒

当一个 turn 仍处于 busy 状态时，gateway 会每 5 分钟发送一次简短提醒。只要有新的用户可见输出，下一次 5 分钟提醒窗口就会重新计时。提醒不会包含 `connecting` 这类低层 phase。如果期间观察到已跟踪的内部进展，会显示自上次用户可见输出或提醒以来的内部事件数：

```text
⏳ Still working (4 internal events)
```

内部事件数目前基于 gateway 已跟踪的进展事件，例如 `todos`。启用 `event_visibility.todos` 后，每个 `todos` 事件都会以完整的当前 todo 列表推送，不做去重，即使列表未变化或为空也会推送。

## 完成通知

`assistant_message` 是发送到 IM 的主要最终回复。

SubAgent 的 `assistant_message` 会标注 agent 类型（缺失时使用 agent ID）和任务 ID。`agent_done` 始终订阅并发送权威完成摘要，因此也能覆盖只有工具调用、没有 assistant 文本的完成轮次。

`notification` 是用户提醒的标准事件，包括权限请求、问题请求、blocked 错误和完全停止后的完成通知。

`idle` 表示全局空闲：主 agent 与所有 SubAgent 都已停止活跃工作。单个 agent 的 idle 状态变化不会作为协议 `idle` 事件暴露，因此只要仍有任何 agent 在工作，gateway 就会保持 busy，不会停止长时间提醒，也不会发送 idle 通知。

全局 `idle` 事件通常不会触发兜底完成消息。如果它清理了待回答问题、待确认请求或待处理 handoff 请求，gateway 会发送针对性的英文失效提示，而不是发送通用完成消息。gateway 在清理仍带有待回答问题、待确认请求或待处理 handoff 请求的空闲进程前，也会发送同样的失效提示。Chord 可能会在配置操作（例如手动切换 model pool）导致静默时附带 `suppress_user_notification: true`；gateway 仍然把进程收口为 idle 并停止提醒，只跳过通用 idle 消息。待处理交互的失效提示优先级更高，不受该字段抑制。

Chord 也可能在未做决策的情况下取消待处理 handoff，例如该请求被新的请求或会话切换取代。gateway 会消费 `handoff_cancelled` 事件，丢弃已失效的待决 handoff，并发送取消通知。之后再用 `/handoff` 或 `/handoff-deny` 会回到常见的「没有待处理 handoff」提示，而不会把回复路由到已取消的请求。`handoff_cancelled` 事件可能迟到，或携带不匹配的 request ID；gateway 只清理该事件指向的待决 handoff，不会误清更新的待决请求。

## 日志

为便于观测，gateway 日志会包含以下路由阶段：

- `gateway event`
- `gateway routing event`
- `gateway sending notification`

日志详情见 [operations_CN.md](./operations_CN.md)。
