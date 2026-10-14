# 使用指南

所有 IM 平台统一使用单 owner 配对与显式聊天绑定，单工作区也需绑定。首次运行 `chord-gateway --pair`，微信/飞书/企业微信/钉钉在机器人私聊发送 `/bind "~/workspace" <pairing_code>`；详见[配对与本机重置](./pairing_CN.md)。旧部署升级步骤见[更新日志](../CHANGELOG_CN.md)。 Slack 在 App 私聊使用 `/chord bind "~/workspace" <pairing_code>`；参见 [Slack 接入指南](./slack_CN.md)。

本文档说明如何在 IM 聊天中日常使用 `chord-gateway`。

Discord 服务器频道和 thread 中，下列命令需在开头带真实 Bot mention，例如 `@Bot /status`、`@Bot /allow`、`@Bot /answer @<id> <answer>`；DM 中直接发送。原生 slash command 与按钮延期，每个 thread 需独立绑定，详见 [Discord 指南](./discord_CN.md)。

## 第一条消息检查清单

网关启动后：

1. 从已连接的 IM 聊天发送 `/status`。
2. 确认显示的是预期工作区和 IM 绑定。
3. 如果当前绑定已有固定会话，决定继续使用，或发送 `/new` 开始新会话。
4. 发送普通文本请求给 Chord。

钉钉需先完成私聊身份验证与主动绑定探测；只有配对 owner 可发命令，群输入需 @机器人。权限确认用 `/allow`/`/deny`，Question 用 `/answer`，回复与后台结果仅支持文本，详见[钉钉指南](./dingtalk_CN.md)。

## 路由模型

聊天通过 `chat_bindings` 选择工作区。Gateway 按工作区、IM 平台和聊天隔离运行会话；自动恢复 pin 使用同样的键，更换平台账号也会延续原会话。键以编码形式保存，不应手工拼接。即使使用同一工作区，不同聊天仍保留各自的会话上下文。

## Session pin 机制

gateway 不使用 `chord headless --continue`，而是为每个绑定保存一个 pinned Chord session ID。

- 如果当前绑定已有 pinned session ID，gateway 会用 `--resume <sid>` 启动 Chord。
- 飞书的 pinned session 恢复失败时，已接纳输入会隔离供核查，并保留原目标，不自动换新 session 重试。先查看 `/status` 和原会话，再显式使用 `/new` 或 `/resume`。
- 如果没有 pinned session ID，gateway 会启动一个新 session。
- `/new` 会启动新 session；运行中的绑定会保留原 pin，直到 Chord 确认会话已切换。
- `/resume <sid>` 会把指定 session ID pin 到当前绑定。
- 进程内会话切换（`session_switched`）会把 pin 指向新的活动会话，之后重新拉起进程会 resume 运行时实际使用的会话。

Session pin 默认持久化到 `<state_dir>/session-pins.json`，也可通过 `session_pins_file` 配置。pin 按工作区和路由记录，因此在同一路由上更换平台账号时会延续原会话。本机 `pairing reset` 会连同 owner 和绑定一起清除该平台的自动 pin,重置后的路由从新会话开始。

进行中或已经完成的会话移交优先于 pin：普通消息不能重新拉起该绑定，也不会改用新会话。接管到当前聊天的会话确认 `ready_paused` 且 pin 保存成功后，目标绑定会开放输入，等待用户明确继续。已经把会话移交出去的绑定继续阻断，直到显式 `/new` 或 `/resume` 成功；失败时保留阻断。

`/new` 或 `/resume` 替换空闲的恢复会话前，gateway 会刷新此前非空的输入摘要。刷新失败或输入结果仍不确定时，保留原会话和 pin。旧进程断线后，`/takeover` 也不能绕过这项保护。


进程忙碌时 `/new` 与 `/resume` 会被拒绝；先发送 `/cancel`，等待当前 turn 结束。
## Slack 命令入口

下表展示 Gateway 命令语法。Slack 根聊天使用 `/chord status`、`/chord allow`、`/chord answer @<id> <text>`，其他命令同样加 `/chord` 包装；thread 内使用 `@Bot /status`、`@Bot /allow`、`@Bot /answer @<id> <text>`。slash 始终作用于频道根目标，各 thread 需独立绑定。频道/thread 任务必须显式 @Bot。详见 [Slack 指南](./slack_CN.md)。

## IM 命令

Telegram 使用下划线形式的 `/handoff_deny` 和 `/question_interact`、`/question_decline`、`/question_withdraw`、`/question_replace`。群里再添加 `@YourBotUsername`，按自动生成的提示操作即可，详见 [Telegram 指南](./telegram_CN.md#群聊与-privacy)。

| 命令 | 说明 |
|---|---|
| `/bind "<directory>" [pairing_code]` | 绑定已有目录；首次私聊配对需提供终端配对码 |
| `/status` | 查看当前 Chord 状态：busy/idle、session、当前角色、phase、最近一次上下文检查点结果、待处理交互及恢复的排队输入 |
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
| `/takeover <id> [--timeout <duration>]` | 将运行中的 TUI 会话接管到当前聊天，并保持暂停 |
| `/takeover status` | 查询当前绑定的移交状态，不发起新的接管 |
| `/sessions` | 列出最近 session |
| `/login [platform]` | 查看支持登录续期的平台；指定平台时启动续期流程（例如 `/login wechat`） |
| 其他文本 | 发送新要求给 Chord，待答问题继续保留 |

`/sessions` 在绑定目录中调用 `chord sessions list --json --limit 10`，并传递 `chord_state_dir`。自定义会话根目录和仓库 worktree 的共享会话由 Chord 解析。结果按最近活动时间排序，并标出使用中的会话；查询失败与空列表会分别提示。需要使用提供此命令的 Chord，gateway 不再扫描旧目录。

## 会话接管

接管支持同一台 macOS 或 Linux 主机、同一 OS 用户、同一规范化工作区下，TUI 与 gateway 管理的 headless 双向移交。需要支持 takeover CLI 和 managed-headless 标志的 Chord 构建；较早的 Chord 仍可用于普通聊天绑定，但不能接管。普通 headless、ACP、其他工作区及 Windows 不在支持范围内。离线会话请使用 `/resume <id>`。

在当前绑定没有存活 Chord 进程的 IM 聊天中，接管运行中的 TUI 会话：

```text
/takeover <session-id> --timeout 30s
/takeover status
```

如果聊天已经绑定了存活进程，需要先显式处理该绑定。从 gateway 移交到新 TUI 时，在 gateway 所在主机的同一工作区运行：

```sh
chord -d /path/to/workspace --takeover <session-id> --takeover-timeout 30s
```

工作区身份由 Chord 确认，因此 linked worktree 可以在所属主仓库内移交，同时保留会话保存的执行目录。预检拒绝不会阻断普通聊天；若准备在源端冻结前失败，状态查询只有确认仍是同一个存活源进程后才重新开放路由。结果不明时，绑定继续保持阻断。

源端停止接纳新的执行，等待已经开始的请求、工具和后台工作结束并保存结果，不会取消这些操作，也不会派发下一次模型请求。已经接纳的排队输入，包括通过 TUI 提交的附件，随会话保留。gateway 从 IM 接收文本；接管不增加入站图片或文件上传能力，生成图片仍可转发到微信或飞书，企业微信和 Discord 暂不支持媒体输出。服务进程或等待输入的工具可能让移交持续等待；状态查询会显示具体阻塞项。

接管通知优先保留错误、超时和恢复操作提示，显示阻塞项总数和最多三条简短预览，并在省略时标明数量。完整详情请使用[宿主机状态查询命令](./operations_CN.md#会话移交恢复)。

`--timeout` 只限制观察时间。`0` 表示立即返回移交 ID 和当前状态，不指定时持续观察。超时或断线后，源端继续自然收口并持有会话锁。用 `/takeover status` 查询原移交；绑定被阻断时，`/status` 也会显示移交状态。再次发起接管会返回 `transfer_busy`，没有撤销移交命令。

目标端恢复同一个会话并保持暂停。用 `/status` 核对暂停状态和排队输入总数：最多展示八项，每项包含所属任务、简短的单行预览及附件数，并提示还有多少项未展示。完整正文及附件保留在 Chord 会话中。核对后明确发送继续指令或新的普通消息，才会开始模型工作；接管本身不会创建 turn。移交期间，普通消息、旧卡回调和提醒不能拉起替代进程，也不能把工作发给另一个绑定实例。

保存失败时，源端保留会话锁，执行继续冻结。如果会话已经保存但目标加载失败，不会自动改用新会话：先查询状态，确认源端释放锁后显式 `/resume <id>`。如果要放弃当前聊天绑定，可用 `/new` 启动独立会话。移交阻断在 gateway 重启后仍保留。接管到当前聊天的会话确认 `ready_paused` 且 pin 保存成功后，目标绑定会开放输入，等待用户明确继续。已经把会话移交出去的绑定继续阻断，直到显式 `/new` 或 `/resume` 成功；失败时保留阻断。终端查询与保存重试见[运维说明](./operations_CN.md#会话移交恢复)。

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

答案结果未知时，先查看 `/status`，再由同一用户主动重试同一答案。网关运行期间会保留最多 128 个未确认的问题操作，保留期为 14 天。在原会话的运行实例未变化时，主动重试同一操作会复用原操作 ID 和参数。公共收件会核对运行实例、会话代次、请求及当前版本；运行实例或会话变化后，应先核查原问题再提交。飞书和微信均已使用这项公共目标校验。飞书的新点击可以再次提交，只有同一回调事件的重复投递会被去重。重启网关会清空这份重试缓存，之后再次提交前应先核对恢复后的会话。输入不会自动重放。

历史修订被接受，并不代表 Chord 已开始工作。异步问题保持开放时，不会阻断其他工作期间的提醒或无关告警。

历史补答始终指定 ID，例如 `/answer @q-1 revised text`。即使有新题 pending，也能修正旧题；修正追加独立结果并保留原终态、任务归属，不会默认修改当前无关任务。会话切换后使用所选会话的问题集合。

## 确认交互

当 Chord 请求权限确认时，飞书可以展示带 `Allow` / `Deny` 按钮的交互确认卡片。卡片会展示风险等级、工具名、参数摘要、request ID，以及可用时的 workspace/session 上下文。你也可以直接文字回复：

- `/allow` 批准
- `/deny [reason]` 拒绝；可选原因会转发给 Chord

飞书批准或拒绝请求提交后，原卡片会显示请求已提交，尚未确认 Chord 接受；请通过 `/status` 核查。卡片更新失败不会回滚提交；平台明确拒绝更新时可改用文本提示，更新结果未知时不重复发送。

如果不确定是否有待确认请求，可以先发送 `/status`。

飞书上的过期确认和失效控制会被拒绝，不再作为模型上下文转发；旧卡片不能作用于替换后的运行实例或请求。先查看 `/status`；需要新任务时明确发送任务内容。原操作仍需确认时，请让 Chord 重新发起确认。

## Handoff 请求路由

当 Chord 发送 `handoff_request` 时，gateway 会把 handoff plan、可用 agent 和 model pool 发送到 IM 聊天。gateway 不实现 handoff 本身，只把用户回复路由回 Chord 当前待处理的请求：

- `/handoff`：让 Chord 使用默认 agent 接受。
- `/handoff <agent>`：选择 Chord 展示的某个 agent。
- `/handoff <agent> [model_pool]`：同时选择 agent 和 model pool。
- `/handoff-deny <reason>`：拒绝 handoff 请求，并附带原因。

请按 gateway 消息中展示的名称填写 agent 和 model pool。如果当前没有待处理的 Chord handoff 请求，这些命令只会返回提示，不会启动新的 Chord 动作。

如果 Chord 在未做决策的情况下取消了待处理 handoff（例如该请求被新的请求或会话切换取代），gateway 会发送取消通知并丢弃该请求。之后再用 `/handoff` 或 `/handoff-deny` 会返回同样的「没有待处理 handoff」提示，而不会把回复路由到已取消的请求。

## MCP 登录与可用状态

`/status` 和 `/mcp status` 展示不可用的 MCP 服务及宿主机登录命令。认证、连接和工具目录分别显示：凭证已保存不代表连接已成功。可见状态变化会以 MCP 通知推送一次，中间操作进度可通过 `/status` 查看。「全部可用」只作为故障恢复后的确认推送；新会话一开始就全部可用时不会推送。

在运行 Chord 的宿主机上，先进入该会话的工作目录，再执行 `chord mcp login <server>` 或 `chord mcp logout <server>`；角色私有服务加上 `--agent <definition>`。重连操作在本地 TUI 完成。IM 中的 `/mcp login`、`logout`、`reauth`、`reconnect` 和 `cancel` 只返回宿主机操作指引，不会发送给模型。原有 `/mcp enable` 和 `/mcp disable` 仍可使用。

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
Gateway: Session created: 20261015145318085
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
- 生成图片：Chord 产出后转发到来源微信或飞书会话
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

公共收件与重启语义见[运维说明](./operations_CN.md#消息投递与重启)。公共链路对文本命令和已认证卡片操作使用同一授权与冻结目标校验。主动重试结果未知的 Question 操作时，仅在用户、运行实例、会话、请求和参数匹配，且当前目标版本通过校验时复用原操作 ID 和命令参数（包括原版本），不能因此放行无关命令；同一消息或回调的重投仍会去重。卡片更新未送达不撤销已提交操作。

所有平台共享内存收件；去重只在当前进程内有效，重启不重放旧队列。

微信在运行期缺少短期回复上下文时，当前内存队列中的任务保持 `received` 和原 FIFO，尚不交付给 Chord。已授权 owner 在原聊天发送新鲜的 `/status` 可建立上下文并唤醒交付，查询本身不会调用模型；普通新任务排在旧任务后。`/status` 显示等待原因 `reply_context_missing`、输入年龄及隔离数量。等待超过原 24 小时窗口会隔离，新消息不延长这个窗口。已消费任务不会因回复失败再次执行；结果未知时先在宿主机核对原会话。

在公共内存收件链路中，最多四个优先控制请求可以越过普通队列；随后按普通队列原顺序处理，包括排在任务前的 shell、模型和角色设置命令。`/cancel` 在接收时捕获 Gateway 当前观察到的进程、会话和活动工作，发送前若发现目标变化则拒绝。现有 Chord 协议取消的是它处理命令时的当前活动工作，无法原子地限定为 Gateway 先前观察到的 turn；“请求已提交”不表示取消已经完成，也不会删除尚未交付的收件。

## 企业微信命令与输出

企业微信私聊和群文本支持本页现有控制命令，以文本进行确认与问答；只有 owner 可执行。绑定需固定主动探测成功，群回复对所有成员可见。首版只发送最终文本和后台文本结果，卡片、媒体及流式回复尚未支持。失败时先查已接受段数与原会话；结果未知不会自动重发。见[企业微信指南](./wecom_CN.md)。
