# 使用指南

本文档说明如何在 IM 聊天中日常使用 `chord-gateway`。

## 路由模型

gateway 按 IM 会话和工作区隔离 Chord session。

会话绑定键为：

```text
workspaceID | imType | chatID
```

示例：

- `default|wechat|user123`
- `backend|feishu|oc_xxx`

不同聊天会话拥有不同上下文；不同工作区也拥有不同上下文。

## Session pin 机制

gateway 不使用 `chord headless --continue`，而是为每个绑定保存一个 pinned Chord session ID。

- 如果当前绑定已有 pinned session ID，gateway 会用 `--resume <sid>` 启动 Chord。
- 如果没有 pinned session ID，gateway 会启动一个新 session。
- `/new` 会清除当前绑定的 pin 并启动新 session。
- `/resume <sid>` 会把指定 session ID pin 到当前绑定。

Session pin 默认持久化到 `<state_dir>/session-pins.json`，也可通过 `session_pins_file` 配置。

## IM 命令

| 命令 | 说明 |
|---|---|
| `/status` | 查看当前 Chord 状态：busy/idle、phase、待处理交互 |
| `/cancel` | 取消当前 turn |
| `/allow` | 批准待确认请求 |
| `/deny [reason]` | 拒绝待确认请求；可选原因会转发给 Chord |
| `/answer <text>` | 回答待处理问题；支持数字快捷选择 |
| `/todos` | 查看当前 todo 列表 |
| `/new` | 清除当前 session pin 并启动新 session |
| `/resume <id>` | 恢复并 pin 指定 session |
| `/sessions` | 列出最近 session |
| `/current` | 查看当前聊天 pin 的 session |
| `/login [platform]` | 启动登录流程，例如 `/login wechat` |
| 其他文本 | 发送给 Chord；如果当前有待回答问题，则作为问题答案 |

## 问题交互

当 Chord 发送 `question_request` 时，gateway 会把问题和选项发送到 IM 聊天。飞书中，受支持的单选问题会展示为带选项按钮的交互卡片；用户可以点击按钮，也可以直接文字回复。单选且选项数不超过 10 的问题，只有在选项文本和可展示正文都足够短时才会使用按钮卡片。多选、自由回答、选项过多，或会让卡片正文过长的选项/详情，会回退到文本形式。

飞书按钮点击被接受后，gateway 会尝试把原卡片更新为已处理状态。如果更新失败，仍会发送文本确认，且不会回滚已经提交给 Chord 的审批或回答。

```text
❓ Continue?
  1. yes — Yes, proceed
  2. no — No, stop
Reply /answer 1 / 1,2 / or type your answer
```

回答方式包括：

- 飞书出现交互问题卡片时，点击选项按钮。
- 使用 `/answer`，例如 `/answer 1`；多选问题可用 `/answer 1,3`。
- 在问题待处理时直接发送普通文本，gateway 会把它作为自由文本答案。

无效的数字快捷输入不会被静默接受，而是作为自定义文本发送。

如果待回答问题因为 Chord 进入 idle 或 gateway 清理空闲进程而过期，gateway 会发送一条英文失效提示。之后再发送 `/answer` 时，不会继续作为原来的结构化回答提交，因为原 request ID 已不再处于 pending 状态；gateway 会把它作为普通后续消息转发给 Chord，并在仍能找到时附带已过期的问题内容。

用户可见提示示例：

```text
⚠️ The pending question has expired. Your response was sent as a follow-up message, not as a structured answer.
```

## 确认交互

当 Chord 请求权限确认时，飞书可以展示带 `Allow` / `Deny` 按钮的交互确认卡片。卡片会展示风险等级、工具名、参数摘要、request ID，以及可用时的 workspace/session 上下文。你也可以直接文字回复：

- `/allow` 批准
- `/deny [reason]` 拒绝；可选原因会转发给 Chord

如果不确定是否有待确认请求，可以先发送 `/status`。

如果待确认请求因为 Chord 进入 idle 或 gateway 清理空闲进程而过期，gateway 会发送一条英文失效提示。之后再发送 `/allow` 或 `/deny` 时，不会被当作批准或拒绝执行；gateway 只会把它作为后续上下文转发给 Chord，并明确说明不能把该消息视为确认结果。

用户可见提示示例：

```text
⚠️ The pending confirmation has expired. Your response was sent as a follow-up message, not as an approval or denial.
```

## Session 示例

列出最近 session：

```text
You: /sessions
Gateway: 📋 Recent sessions:
  • 2026-04-14-abc123 - "Analyze project structure" (2 hours ago)
  • 2026-04-14-def456 - "Help with Go code" (5 hours ago)
```

恢复 session：

```text
You: /resume 2026-04-14-abc123
Gateway: ✅ Resumed session 2026-04-14-abc123
```

查看当前 session：

```text
You: /current
Gateway: 📍 Current session: 2026-04-14-abc123
```

启动新 session：

```text
You: /new
Gateway: 🆕 Started new session
```

## 多 IM 登录

在多 IM 模式下，如果某个平台登录过期或连接失效，gateway 可以通过其他活跃 IM 发送通知。

示例：如果微信登录过期，可以在飞书中发送：

```text
/login wechat
```

gateway 会返回微信二维码登录链接。扫码后 token 会自动更新，无需重启 gateway。登录成功或失败结果也会通过其他 IM 通知。

注意：

- `/login` 只用于支持交互式登录续期的平台；当前文档化的续期流程是 `/login wechat`。
- 飞书不需要、也不支持在会话中通过 `/login feishu` 登录或续期。飞书 access token 会由 gateway 使用已配置的应用凭证自动获取和刷新。
- 如果收到飞书连接或配置失效通知，请在部署配置或飞书开放平台中检查应用凭证、权限、事件订阅和长连接设置；不要在 IM 会话中发送或修改 `app_id` / `app_secret`。
- 跨 IM 通知需要至少另一个 IM 仍然可用，并且 gateway 能找到该 IM 的聊天 ID。飞书建议配置 `chat_bindings`，或先在目标聊天中发送过消息。

## 通知

gateway 会向活跃 IM 推送重要控制面通知，包括：

- 需要确认
- 需要回答问题
- 任务开始
- 任务完成
- 错误或 blocked 状态
- 工具失败
- 任务长时间仍在处理时，每 5 分钟发送一次提醒

长时间提醒不会直接暴露 `connecting` 这类低层 phase。只要有新的用户可见输出，下一次 5 分钟提醒窗口就会重新计时。如果当前提醒窗口内观察到内部进展事件，提醒会附带简短计数，例如：

```text
⏳ Still working (4 internal events)
```

内部事件数目前来自 gateway 已跟踪的进展事件，例如 `tool_result` 和 `todos`；每次用户可见输出或提醒后会重置。启用 `event_visibility.todos` 后，每个 `todos` 事件都会以完整的当前 todo 列表推送，不做去重。

可选的低层事件由 `event_visibility` 控制。详见 [event-visibility_CN.md](./event-visibility_CN.md)。
