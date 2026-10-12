# 使用指南

本文档说明如何在 IM 聊天中日常使用 `chord-gateway`。

## 第一条消息检查清单

网关启动后：

1. 从已连接的 IM 聊天发送 `/status`。
2. 确认显示的是预期工作区和 IM 绑定。
3. 如果当前绑定已有固定会话，决定继续使用，或发送 `/new` 开始新会话。
4. 发送普通文本请求给 Chord。

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
- 如果 pinned session 已被清理或被占用导致 resume 失败，普通文本消息会自动改用新 session 重试，并提示用户。
- 如果没有 pinned session ID，gateway 会启动一个新 session。
- `/new` 会清除当前绑定的 pin 并启动新 session。
- `/resume <sid>` 会把指定 session ID pin 到当前绑定。
- 进程内会话切换（`session_switched`）会把 pin 指向新的活动会话，之后重新拉起进程会 resume 运行时实际使用的会话。

Session pin 默认持久化到 `<state_dir>/session-pins.json`，也可通过 `session_pins_file` 配置。

## IM 命令

| 命令 | 说明 |
|---|---|
| `/status` | 查看当前 Chord 状态：busy/idle、session、当前角色、phase、最近一次上下文检查点结果、待处理交互 |
| `/cancel` | 取消当前 turn |
| `/allow` | 批准待确认请求 |
| `/deny [reason]` | 拒绝待确认请求；可选原因会转发给 Chord |
| `/answer @<id> <text>` | 按 ID 回答或修正问题；唯一待答题可省略 ID |
| `/handoff <agent> [model_pool]` | 将待处理 Chord handoff 请求的接受结果转发给 Chord；不带参数时由 Chord 使用默认 agent |
| `/handoff-deny <reason>` | 将待处理 Chord handoff 请求的拒绝结果和原因转发给 Chord |
| `/role [编号\|角色名]` | 显示当前主角色与可切换角色；不带参数时展示菜单，带编号或角色名时切换到目标角色 |
| `!<command>` / `！<command>` | 通过绑定工作区向 Chord 发送 `local_shell` 命令，并回传 Chord 返回的 stdout/stderr 结果 |
| `/todos` | 查看当前 todo 列表 |
| `/new` | 优先交给当前 Chord 进程启动新 session；如果没有可用进程，则清除当前 session pin 并启动新的 Chord 进程 |
| `/resume <id>` | 恢复并 pin 指定 session；如果恢复失败，会清除该 pin 并提示用户 |
| `/sessions` | 列出最近 session |
| `/login [platform]` | 查看支持登录续期的平台；指定平台时启动续期流程（例如 `/login wechat`） |
| 其他文本 | 发送新要求给 Chord，待答问题继续保留 |

## 问答交互

问题按 ID 独立保留，可以暂不回答，继续发送普通聊天。`/status` 列出所有待答题；普通文字只作为新要求，不会自动当答案。

- `/answer @<id> 1` 选择选项；多选用 `1,3`，自定义答案用 `/answer @<id> <text>`。只有唯一 pending 题时可省略 ID。
- 飞书选项按钮或多选表单也可答题，卡片与题目 ID 关联。
- `/question-interact <id>` 停用计时，`/question-decline <id>` 拒绝回答；拒绝不撤回必要要求。
- `/question-withdraw <id>` 撤回要求，`/question-replace <old> <new>` 关联替代决策。`/new-task <text>` 开始独立任务，`/question cancel-task` 取消当前任务。

IM 无法可靠检测开始输入，因此 gateway 先请求 Chord 停用本题计时，收到可靠状态更新后才提供回答入口。必要确认本来就无限等待。系统默认结果显示为系统采用，不表示用户同意。

指定的问题 ID 不在本地缓存时，网关会先向 Chord 同步，包括进程重启后恢复的历史问题。同步失败与当前会话不存在该 ID 会分别提示。

提交后 gateway 等待匹配的 `question_receipt`，只有明确接受才显示 `✅ Answer accepted`。失败保留题目；超时或断连提示结果未知，应先检查 `/status`，不能把入队成功当作回答成功。终态通知会标明题目 ID，已回答的飞书卡片保留所选答案。飞书卡片以 Chord 确定终态更新，断连不编造终态。

答题入口的展示确认命令无法发送或被拒绝时，网关会提示原因；发送 `/status` 可重试展示。即使状态快照先于结果事件到达，卡片也会按已确认的结果收口。

答案结果未知时，先查看 `/status`，再由同一用户主动重试同一答案。网关运行期间会保留最多 128 个未确认的问题操作，保留期为 14 天；即使 Chord 子进程重启，重试仍复用原操作 ID 和参数。飞书的新点击可以再次提交，只有同一回调事件的重复投递会被去重。重启网关会清空这份重试缓存，之后再次提交前应先核对恢复后的会话。输入不会自动重放。

历史修订被接受，并不代表 Chord 已开始工作。异步问题保持开放时，不会阻断其他工作期间的提醒或无关告警。

历史补答始终指定 ID，例如 `/answer @q-1 revised text`。即使有新题 pending，也能修正旧题；修正追加独立结果并保留原终态、任务归属，不会默认修改当前无关任务。会话切换后使用所选会话的问题集合。

## 确认交互

当 Chord 请求权限确认时，飞书可以展示带 `Allow` / `Deny` 按钮的交互确认卡片。卡片会展示风险等级、工具名、参数摘要、request ID，以及可用时的 workspace/session 上下文。你也可以直接文字回复：

- `/allow` 批准
- `/deny [reason]` 拒绝；可选原因会转发给 Chord

飞书确认按钮被接受后，gateway 会尽力更新原始卡片为最终状态；如果更新失败，仍会发送文本确认，且不会回滚 Chord 动作。

如果不确定是否有待确认请求，可以先发送 `/status`。

如果待确认请求因为 Chord 进入 idle 或 gateway 清理空闲进程而过期，gateway 会发送一条英文失效提示。之后再发送 `/allow` 或 `/deny` 时，不会被当作批准或拒绝执行；gateway 只会把它作为后续上下文转发给 Chord，并明确说明不能把该消息视为确认结果。

用户可见提示示例：

```text
⚠️ The pending confirmation has expired. Your response was sent as a follow-up message, not as an approval or denial.
```

## Handoff 请求路由

当 Chord 发送 `handoff_request` 时，gateway 会把 handoff plan、可用 agent 和 model pool 发送到 IM 聊天。gateway 不实现 handoff 本身，只把用户回复路由回 Chord 当前待处理的请求：

- `/handoff`：让 Chord 使用默认 agent 接受。
- `/handoff <agent>`：选择 Chord 展示的某个 agent。
- `/handoff <agent> [model_pool]`：同时选择 agent 和 model pool。
- `/handoff-deny <reason>`：拒绝 handoff 请求，并附带原因。

请按 gateway 消息中展示的名称填写 agent 和 model pool。如果当前没有待处理的 Chord handoff 请求，这些命令只会返回提示，不会启动新的 Chord 动作。

如果 Chord 在未做决策的情况下取消了待处理 handoff（例如该请求被新的请求或会话切换取代），gateway 会发送取消通知并丢弃该请求。之后再用 `/handoff` 或 `/handoff-deny` 会返回同样的「没有待处理 handoff」提示，而不会把回复路由到已取消的请求。

## 角色切换

`/role` 切换主 agent 角色（例如从 `builder` 切到 `planner`），不会新建 session，也不会丢弃当前会话上下文。

- `/role` 显示当前角色和 Chord 可切换的角色列表。飞书里用交互卡片呈现，每个角色一个按钮；文本平台显示为编号列表。
- `/role <编号>` 按编号选择角色（选择时会重新拉取最新列表，编号始终对应当前角色）。
- `/role <角色名>` 直接按名称切换。

```text
You: /role
Gateway: 🎭 Current role: builder
1. builder (current)
2. planner
Reply /role <number> or /role <name> to switch.
```

只有 Chord 配置为 main-mode 的角色才会出现在列表里，SubAgent 不会是切换目标。切到当前角色会提示没有变化。如果 Chord 拒绝切换（例如仍有待处理的 handoff 决策），gateway 会原样显示 Chord 的说明。它和 `/handoff` 的区别：handoff 是让另一个角色执行待处理 plan，`/role` 是直接切换当前会话的主角色。

如果切换请求失败或等待响应超时，gateway 会提示结果尚未确认，并尝试更新已点击的飞书卡片，避免停留在 Processing。未收到响应不代表切换一定失败；重试前先发送 `/status` 确认当前角色。飞书卡片点击只在原卡片上更新结果，不再额外发一条聊天消息；文本平台仍会发消息。

需要支持 headless `role` 控制命令的 Chord 版本才能列出并切换角色；旧版 Chord 下该命令会提示无法加载角色列表。

## Chord local shell 快捷入口

以 `!` 或全角 `！` 开头的消息会作为 Chord `local_shell` headless 命令发送，而不是作为普通聊天文本：

```text
You: !pwd
Gateway: ✅ Local shell: pwd
/path/to/workspace
```

gateway 转发命令并展示 Chord 返回的结果；执行语义和可用性属于 Chord headless。本地 shell 与 agent turn 分别计时，agent 摘要只统计观察到的 agent 工作。Shell 请求从发出到结果返回期间保持 busy；长时间运行的 shell 命令会收到提醒，也不会因空闲超时而被回收。请把它视为绑定工作区中的本地命令执行能力：只在可信 IM 聊天中使用，避免在命令文本或输出中暴露敏感信息。

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
Gateway: 🔄 Resuming session 2026-04-14-abc123

# 如果 session 不存在或被占用：
Gateway: ❌ Failed to resume session 2026-04-14-abc123. It may not exist or may be busy.
```

启动新 session：

```text
You: /new
Gateway: 🆕 /new sent to chord process.
# 如果当前没有可用 Chord 进程：
Gateway: 🆕 New session started.
```

## 多 IM 登录

在多 IM 模式下，如果某个平台登录过期或连接失效，gateway 可以通过其他活跃 IM 发送通知。

示例：如果微信登录过期，可以在飞书中发送：

```text
/login wechat
```

gateway 会返回微信二维码登录链接。扫码后 token 会自动更新，无需重启 gateway。登录成功或失败结果也会通过其他 IM 通知。

注意：

- 不带平台参数的 `/login` 只会显示用法和支持登录续期的平台，不会启动任何登录流程。
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
- 后台任务结果：后台任务结束时推送，通常在 turn 已进入 idle 之后；可用 `event_visibility.background_result: false` 关闭
- 错误或 blocked 状态
- 工具失败
- 生成图片：Chord 产出后转发到来源会话
- 上下文压力提示：仅在启用 `event_visibility.context_notice` 时推送
- 任务长时间仍在处理时，每 5 分钟发送一次提醒

确认或 handoff 的提示成功投递后，只有对应请求的已识别伴随 `notification` 提醒会被去重。即使网关缓存仍保留旧请求，无关告警也会照常送达；提示投递失败时，仍会发送伴随提醒。持久化问题没有伴随的重复提醒，也不会屏蔽通知。本身没有专用提示的提醒（例如 loop 需要用户决策）仍会照常送达。

长时间提醒不会直接暴露 `connecting` 这类低层 phase，但会显示已运行的 turn 时长、粗粒度阶段，以及 turn 开始以来观察到的工具与重试次数。只要有新的用户可见输出，下一次 5 分钟提醒窗口就会重新计时。例如：

```text
⏳ Still working (12m03s · running tools · 7 tools · 1 retry · 4 internal events)
```

重试数统计 Chord 在该 turn 内宣告的每次重试决策（包括回退切换），是压力信号而非精确的失败次数。内部事件数目前来自 gateway 已跟踪的进展事件，例如 `todos`；每次用户可见输出或提醒后会重置。启用 `event_visibility.todos` 后，每个 `todos` 事件都会以完整的当前 todo 列表推送，不做去重。

完成通知也会附带刚结束 turn 的紧凑摘要：时长、主 agent 与 SubAgent 报告的工具调用数、以及观察到的重试次数，例如 `✅ Chord: Ready for input (1m42s · 12 tools)`；`⏳ Chord: Ready for input; background jobs still running: N` 变体同样会附加该摘要。同一会话内的确认、交接和取消操作会保留当前工作周期内已经累计的进度。空闲时请求取消不会启动工作周期或提醒。本地 shell 的结果不会重置 agent 进度；若 agent 空闲时仍有 shell 命令未返回，完成通知会显示其数量，并继续 shell 提醒。

可选的低层事件由 `event_visibility` 控制。详见 [event-visibility_CN.md](./event-visibility_CN.md)。
