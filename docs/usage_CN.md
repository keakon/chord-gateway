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
| `/deny` | 拒绝待确认请求 |
| `/answer <text>` | 回答待处理问题；支持数字快捷选择 |
| `/todos` | 查看当前 todo 列表 |
| `/new` | 清除当前 session pin 并启动新 session |
| `/resume <id>` | 恢复并 pin 指定 session |
| `/sessions` | 列出最近 session |
| `/current` | 查看当前聊天 pin 的 session |
| `/login [platform]` | 启动登录流程，例如 `/login weixin` |
| 其他文本 | 发送给 Chord；如果当前有待回答问题，则作为问题答案 |

## 问题交互

当 Chord 发送 `question_request` 时，gateway 会把问题和选项发送到 IM 聊天：

```text
❓ Continue?
  1. yes — Yes, proceed
  2. no — No, stop
Reply /answer 1 / 1,2 / or type your answer
```

回答方式有两种：

- 使用 `/answer`，例如 `/answer 1`；多选问题可用 `/answer 1,3`。
- 在问题待处理时直接发送普通文本，gateway 会把它作为自由文本答案。

无效的数字快捷输入不会被静默接受，而是作为自定义文本发送。

## 确认交互

当 Chord 请求权限确认时：

- `/allow` 批准
- `/deny` 拒绝

如果不确定是否有待确认请求，可以先发送 `/status`。

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

在多 IM 模式下，如果某个平台登录过期，gateway 可以通过其他活跃 IM 通知你重新登录。

示例：如果微信登录过期，可以在飞书中发送：

```text
/login weixin
```

gateway 会返回微信二维码登录链接。扫码后 token 会自动更新，无需重启 gateway。

## 通知

gateway 会向活跃 IM 推送重要控制面通知，包括：

- 需要确认
- 需要回答问题
- 任务开始
- 任务完成
- 错误或 blocked 状态
- 工具失败
- 长时间 phase 提醒

可选的低层事件由 `event_visibility` 控制。详见 [event-visibility_CN.md](./event-visibility_CN.md)。
