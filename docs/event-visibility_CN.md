# Gateway 控制面事件可见性

所有 IM 平台统一使用单 owner 配对与显式聊天绑定，单工作区也需绑定。首次运行 `chord-gateway --pair`，微信/飞书/企业微信/钉钉在机器人私聊发送 `/bind "~/workspace" <pairing_code>`；详见[配对与本机重置](./pairing_CN.md)。旧部署升级步骤见[更新日志](../CHANGELOG_CN.md)。 Slack 在 App 私聊使用 `/chord bind "~/workspace" <pairing_code>`；参见 [Slack 接入指南](./slack_CN.md)。

`chord-gateway` 从 `chord headless` 读取 JSONL 事件。必需控制面事件始终订阅并路由。`background_result` 默认订阅、可显式关闭；其余可选事件通过 `event_visibility` 选择启用。

- English: [event-visibility.md](./event-visibility.md)

## 默认事件

gateway 默认订阅以下事件：

- `activity`
- `assistant_message`
- `confirm_request`
- `question_request`
- `question_updated`
- `question_resolved`
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
- `mcp_status_changed`
- `input_queue_changed`
- `session_transfer_preparing`
- `session_transfer_status`
- `session_transfer_saved`
- `session_transfer_released`
- `background_result`
- `image_result`

这些事件覆盖 IM 控制所需的最小行为，其中 `background_result` 可以关闭：

- 最终 assistant 回复
- 权限确认
- 用户问题
- 问题关闭事件，通知标明题目 ID 和已接受的选择
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
- 会话移交准备、保存与释放，保证重启后已经移交的绑定继续被阻断
- 后台任务完成结果，没有其它投递通道
- 通过有界分块转发生成图片

`session_switched` 只更新状态：触发切换的命令本身已经回复了用户，不再额外发消息。它会把绑定的 session pin 指向新的活动会话，之后重新拉起进程会 resume 运行时实际使用的会话。

`session_transfer_*` 事件属于必需控制面事件，不能通过 `event_visibility` 关闭。准备事件会关闭新输入入口，并在向源端确认之前持久化阻断；状态事件会推送等待收口、阻塞项变化、保存中及保存失败的进度，保留源会话 pin；保存和释放事件更新移交状态，并按源身份条件清除 pin。既有已接纳输入仍属于源会话，新消息与提醒继续被阻断；旧卡回调不能发送到替代绑定实例。用 `/takeover status` 查询进度和阻塞项，绑定被阻断时也可用 `/status`。最终事件丢失不会允许自动重启，详见[运维说明](./operations_CN.md#会话移交恢复)。

会话移交在聊天中按阶段汇报，而不是每个事件一条消息。Chord 会在阻塞项集合变化时重新发布状态，因此 gateway 只汇报三件事：移交已开始、已等待多久以及在等什么、会话已保存。未完成的进度最快每分钟一条，内容未变化的报告直接丢弃。失败一定完整送达，因为恢复依赖错误码与下一步动作。等待时长是已观测的耗时，不是完成预测——Chord 不提供预测值。

暂停就绪回执和状态快照还携带恢复队列摘要。`/status` 展示暂停状态和待处理输入总数，再列出最多八项的所属任务、简短单行预览及附件数，其余条目单独计数。展示摘要不会启动执行，完整正文及附件仍保留在 Chord。缺少队列数据时不会显示为零。接管目标须由 Chord 确认暂停就绪并提供有效摘要，绑定才会开放。headless 契约详见[兼容性说明](./compatibility_CN.md#会话接管能力)。

`background_result` 会作为聊天消息推送到 IM，因为 Chord 侧没有其它通道：后台任务通常在 turn 进入 idle 之后才完成，之后不会有 `assistant_message` 再总结它。觉得刷屏的聊天可设置 `event_visibility.background_result: false` 关闭推送；持久化结果仍保留在 Chord 会话中。

## 可选可见事件

可选事件默认关闭，通过 `event_visibility` 启用。`background_result` 是例外：默认开启，只有显式设为 `false` 才关闭。

```yaml
event_visibility:
  background_result: true
  agent_started: false
  agent_notify: false
  context_notice: false
  info: false
  toast: false
  todos: false
```

| 字段 | 事件类型 | 典型用途 |
|---|---|---|
| `agent_started` | `agent_started` | SubAgent 委托开始通知 |
| `agent_notify` | `agent_notify` | 面向 owner 或指定委派工作流的非阻塞更新 |
| `background_result` | `background_result` | 后台任务完成结果。默认开启；设为 `false` 可关闭聊天推送。 |
| `context_notice` | `context_notice` | 持久化的上下文压力提示，作为聊天消息推送。`level` 取值为 `pressure`、`imminent` 或 `warning`。 |
| `info` | `info` | 信息类消息 |
| `toast` | `toast` | 短暂提示消息 |
| `todos` | `todos` | 完整 Todo 列表更新；每个事件都会完整转发且不去重，并会计入长时间提醒的内部事件数 |

`context_notice` 默认关闭，因为它的聊天投递属于产品取舍、而不是正确性要求。一个压力周期同时最多保留一条持久通知：`level` 是写这条记录时的级别，同一周期内的后续升级不会推送到聊天，只留在 Chord 内部（注入模型请求上下文并记入诊断）。这类通知是「发出去就不管」的：当 Chord 撤回某条通知时（例如切换模型后有效阈值变化），不会推送撤回消息，因此长期开启该开关的聊天可能保留一条过期提示，直到它被翻走；之后再次越过阈值会推送一条新通知。

Gateway 在保留 `background_result` 和 `context_notice` 正文的同时，也保留其 `session_id` 与 `message_index`，用于标识来源会话和持久记录的位置。当前会话仍由 `ready` 和 `session_switched` 更新。

状态查询会携带 gateway 尚未确认终态的题目 ID，让 Core 返回这些决策，即使它们已不在有界历史页内。完整历史通过 Core 的题目游标分页读取；历史页里没有某题，不代表该题已结束。

切会话后重建问题集合；旧题只在所属会话中补答，不能默认影响当前任务。断连保留已知题目但结果视为未知，重新同步后以 Core 快照与回执为准，不编造终态。

## 长时间提醒

当一个 turn 仍处于 busy 状态时，gateway 会每 5 分钟发送一次提醒。只要有新的用户可见输出，下一次 5 分钟提醒窗口就会重新计时。提醒会显示当前 turn 已运行多久、开始以来观察到的工具调用数与模型重试数，以及自上次用户可见输出或提醒以来的已跟踪内部事件数；当前活动有意义时还会给出粗粒度阶段（`preparing`、`receiving reply`、`running tools`、`checkpointing context`、`retrying`、`cooling down`），`connecting` 这类传输层 phase 仍然不会暴露：

```text
⏳ Still working (12m03s · running tools · 7 tools · 1 retry · 4 internal events)
```

重试数统计 Chord 在该 turn 内宣告的每次重试决策（包括回退切换），是压力信号而非精确的失败次数。摘要统计从观察到工作开始到全局 idle 的整个工作周期；同一会话内的确认、交接和取消操作不会重置开始时间或累计次数。空闲时请求取消不会启动工作计时或提醒。排队消息从 Chord 报告开始工作时计时，不包含工作周期之间的空闲时间。

本地 shell 命令单独跟踪：结果返回不会重置正在运行的 agent 进度；所有已请求的 shell 命令返回前，gateway 会保持 busy。只剩 shell 工作时，提醒显示 shell 的已运行时长和 `running local shell` 阶段。

内部事件数目前基于 gateway 已跟踪的进展事件，例如 `todos`，并在每次用户可见输出或提醒后重置。启用 `event_visibility.todos` 后，每个 `todos` 事件都会以完整的当前 todo 列表推送，不做去重，即使列表未变化或为空也会推送。

## 完成通知

`assistant_message` 是发送到 IM 的主要最终回复，会替换缓存的 assistant 正文。Gateway 不展示流式预览，也不订阅 `assistant_rollback`；最终回复不依赖先收到回滚事件。

SubAgent 的 `assistant_message` 会标注 agent 类型（缺失时使用 agent ID）和任务 ID。`agent_done` 始终订阅并发送权威完成摘要，因此也能覆盖只有工具调用、没有 assistant 文本的完成轮次。

`notification` 是用户提醒的标准事件，覆盖 blocked 错误、完全停止后的完成通知，以及本身没有提示的提醒（loop 退出决策、notify 协议纠正，Chord 统一用 `user_input_required` 作为 reason）。只有确认或 handoff 的提示已成功投递后，对应请求的已识别伴随 `user_input_required` 提醒才会被去重。即使缓存仍保留旧请求，无关告警也会照常送达。持久化问题没有伴随的重复提醒，因此未回答的问题不会屏蔽此类通知。

`idle` 和 `status_response` 的 `running_jobs` 表示仍在运行、结束时会通知当前会话的后台任务数。数量不为零时，空闲提醒和 `/status` 都会展示，gateway 也不会因空闲超时回收该进程。完成通知会唤醒 agent，随后一轮工作的 `idle` 会更新计数。服务或监听任务可能长期运行。停止时不发完成通知的任务从发起停止起就不再计入：工具触发的停止会在当前工作下一次 `idle` 时体现，`session_switched` 则会清除 gateway 缓存的上一会话计数。若 gateway 在该 `idle` 前跟踪过一个 turn，`✅ Chord: Ready for input`（或后台任务变体）会附带该 turn 的紧凑摘要：时长、主 agent 与 SubAgent 报告的工具调用数、以及观察到的重试次数；没有可量化工作的 turn 保持原有简短确认。

若 agent 空闲时还有本地 shell 命令未返回，完成通知也会显示 `local shell commands still running: N`，并继续 shell 提醒。

`idle` 表示 agent 空闲：主 agent 与所有 SubAgent 都已停止活跃工作。单个 agent 的 idle 状态变化不会作为协议 `idle` 事件暴露，因此只要仍有任何 agent 在工作，gateway 就会保持 busy，不会停止长时间提醒，也不会发送 idle 通知。

`idle` 和 `idle_timeout` 不关闭问题。问题只通过按 ID 的可靠终态事件收口；历史补答使用显式 ID，保留原终态。`question_updated` 推送计时、介入、依赖处置与历史修正，不能仅更新缓存。状态快照也能恢复遗漏的答题入口或关闭已知待答题；缓存快照不会吞掉随后到达的历史修订通知。

Chord 也可能在未做决策的情况下取消待处理 handoff，例如该请求被新的请求或会话切换取代。gateway 会消费 `handoff_cancelled` 事件，丢弃已失效的待决 handoff，并发送取消通知。之后再用 `/handoff` 或 `/handoff-deny` 会回到常见的「没有待处理 handoff」提示，而不会把回复路由到已取消的请求。`handoff_cancelled` 事件可能迟到，或携带不匹配的 request ID；gateway 只清理该事件指向的待决 handoff，不会误清更新的待决请求。

Gateway 解析 JSONL 或读取流失败时，会单独向聊天发送提示。无法解析的行会被跳过；读取流失效时会先停止进程，再发送提示，并告知尚未收到消费回执的输入数量。这些 gateway 侧错误不依赖 Chord 的 `notification` 事件。

## 日志

为便于观测，gateway 日志会包含以下路由阶段：

- `gateway event`
- `gateway routing event`
- `gateway sending notification`

日志详情见 [operations_CN.md](./operations_CN.md)。

## 输入回执

普通消息和迟到回复带有关联 ID。写入进程并不代表任务开始；Gateway 根据 `input_result` 回执和始终订阅的 `activity` 更新忙碌状态及提醒。`handled` 表示本地命令已处理，`queued` 表示输入已排队，`started` 表示已创建任务，`rejected` 表示输入未被接受。除 `started` 外，其余回执都不会清除已有任务的忙碌状态。断线时未确认的输入会显示为结果未知；请先核对会话，避免重复发送有副作用的请求。需要支持输入回执的 Chord headless。

微信公共收件的 `received`、等待回复上下文（`reply_context_missing`）和隔离数量由 `/status` 展示，不代表 Chord 已开始任务。当前进程内发送新鲜且已授权的 `/status` 可建立回复上下文并唤醒等待队列；进程重启会清空该队列，但查询不创建模型任务。`consumed` 仅表示收到消费回执；`unknown` 必须在宿主机核查，不能自动重发。已消费任务的输出可能因上下文丢失而发送失败，gateway 不提供持久输出补发队列；在 Chord 原会话查看结果。

## 钉钉文本投递

钉钉以文本呈现命令、权限确认、Question 和后台结果，首版不支持卡片与图片。Stream ACK 表示内存接纳或拒绝，不表示 Chord 已执行；机器人回调在 500 或未 ACK 后不保证重投。主动发送结果未知时不自动重发，详见[钉钉状态与恢复](./dingtalk_CN.md#状态发送与恢复)。

## 生成图片

Gateway 订阅 `image_result`，接收有界的 `image_artifact` 数据分块，并按顺序向来源微信或飞书会话转发全部图片。企业微信和 Discord 首版不支持媒体发送，请在原 Chord 会话查看图片。微信上传加密媒体；飞书通过 tenant access token 上传消息图片。飞书图片达到或超过 10 MB 时发送保留透明度的缩小 PNG 预览，原图仍保存在 Chord 会话中。

这是出站图片投递。gateway 从 IM 接收文本，不会将收到的图片或文件上传到 Chord。通过 TUI 已接纳的附件仍属于移交会话，由 Chord 恢复。

Chord 结束处理时会清理并提示未完成的传输；切换会话或关闭进程也会清理未完成的传输。损坏或不完整的数据不会上传。同一传输失败只提示一次，后续分块丢弃时不再重复通知。

多个会话的图片处理串行进行，减少瞬时内存占用；等待处理的原图保留在临时文件中。等待图片处理名额最多 40 秒，超时会提示发送失败，原图仍保留在 Chord 中。预览保留透明度，采用较快的缩放方式，精细纹理可能变模糊，原图不受影响。多个 Agent 的图片独立接收。

微信 CDN 上传遇到网络错误、5xx 或缺少上传回执时，使用相同 URL 和密文最多尝试三次；4xx 和重定向不重试。飞书上传不重试瞬时故障，仅在 HTTP 200 或普通 4xx（不含 408/429）响应明确表示 token 过期时刷新并重试一次。飞书图片处理、token 等待、上传和最终发送共用两分钟预算，adapter 断开时取消。预览处理会在解码、缩放和编码各阶段之间检查取消；已经执行中的 codec 调用会先完成，再响应取消。

重试耗尽、平台明确拒绝或发送结果未确认时，都不会重放发送或重新生图。结果未确认时请先检查聊天记录，也可在 Chord 中查看已保存的原图。图片或发送结果提示确认送达后，会重新计算长时间提醒窗口。Gateway 不直接读取 Chord 本地路径，`/status` 中也不保存图片字节。

### MCP 状态

默认订阅 `mcp_status_changed`。它更新完整 MCP 视图，并在可见状态变化时发送独立于 Agent 回复的通知；相同通知不重复发送，中间操作状态不逐条推送，其他服务器的稳定故障仍会提示；`/status` 读取同一份缓存。事件不包含登录链接或凭证，执行登录命令前须进入该会话的工作目录，浏览器授权在 Chord 宿主机完成。可见范围变化后使用新的完整视图，迟到的快照不会覆盖较新的 MCP 状态。

「全部可用」只在该进程曾报告过 MCP 故障后作为恢复确认推送。进程首次拿到的可用视图不会推送，因此会话发起时即使所有服务都可用也不会打扰聊天；`/status` 仍会显示该摘要。

认证状态与连接状态分别展示：连接成功的服务仍可能需要重新登录。凭证自动刷新会显示为正在进行的操作，不会仅因需要刷新就要求用户在宿主机重新登录。

## 持久输入队列

Gateway 订阅 `input_queue_changed`，通过现有状态事件回调发布有效变化。投影按 runtime/session/epoch 绑定，并以 `view_version` 排序；存储阻塞变化不要求持久版本递增。缺失投影表示未知，空输入列表会覆盖已有队列。`input_result: unknown` 结束发送回执等待，向聊天发送警告，并持续计入 `/status` 和断线通知的未确认输入数；不会将 agent 标记为忙碌，也不会自动重发。有待处理输入、队列阻塞或结果未确认时，不允许切换目录。重发前请在宿主机检查原会话；核实并处理不确定结果后，重启 gateway 可清除保守阻塞。聊天平台普通消息仍使用下一次请求策略，队列编辑和控制仍在本地 Chord TUI 中进行。

输入队列保护也覆盖闲置回收和进程替换，防止这些路径清除待处理输入或结果未知的证据。`/status` 显示主队列数量、暂停状态、存储阻塞及恢复输入的续跑要求。带有保护状态的进程断开后不会自动替换；请先在本机核查原会话，再重启 gateway。

`/new`、`/resume`、目录切换或闲置回收前，如果空闲进程此前报告过非空恢复输入摘要，gateway 会重新查询。必须由覆盖所有 owner 的摘要确认输入已处理；仅主队列为空不足以解除保护。缺失、迟到或查询失败的状态回执不会清除保护，结果未知的输入也不会因此解除阻断。接管目标检查同样覆盖已断线的进程，保留原会话的未解决输入证据。

所有平台共用有界内存收件、授权和投递回执。Gateway 重启后不恢复收件队列、去重记录或轮询游标；结果不确定时，先核查 Chord 原会话再决定是否重发。详见[投递与重启语义](./operations_CN.md#消息投递与重启)。

飞书卡片「已接收」或「处理中」只表示接纳，不表示执行成功。消费及卡片结果以 `input_result` 回执和明确结果事件确认。可重试接纳失败表示未接纳，提交或发送结果不明则需先核查再决定是否重试。卡片更新失败不回滚消费，也不能据此重复提交相同答案；`/status` 会展示公共等待和隔离信息，无运行中的 Chord 进程时也可核查。

## Slack 输出范围

Slack 首版将文本事件、确认与 Question 提示返回原根聊天或 thread，使用 `/chord` 或 thread @Bot 文本命令交互。图片和文件转发、按钮与 modal 延期；`image_result` 不通过 Slack 上传文件。详见 [Slack 指南](./slack_CN.md)。
