# 变更日志

本文档记录 `chord-gateway` 的重要变更。

本项目使用简单的人类可读变更日志格式。日期格式为 `YYYY-MM-DD`。

- English: [CHANGELOG.md](./CHANGELOG.md)

## 未发布

### 不兼容变更

- Gateway 入站队列、去重和轮询游标改为仅存内存，重启不恢复，不保证跨重启不丢不重；旧去重与游标文件不再导入。配置、登录凭据、session pin 和移交保护仍保留，Chord 会话由 Chord 保存。停服前核查未决输入，详见[投递语义](./docs/operations_CN.md#消息投递与重启)。

- 飞书与微信统一使用单 owner 配对和显式聊天绑定，单工作区也不例外。删除 legacy 模式和 `access_mode`、`owner_open_id`、`allowed_open_ids`、微信 `workspace_id` 字段；包含这些字段的配置会忽略它们并在启动时告警。删除旧 `/bind <workspace_id> <path>`，改用 `/bind "<已有目录>"`；首次私聊配对需额外提供终端配对码。旧授权不会自动迁移：以 `--pair` 启动并完成私聊配对后，已有 `chat_bindings` 即生效；未配对且不带 `--pair` 启动时会报错并给出指引。
- 升级已有部署：先停止 gateway，完成或处理未结束的会话转移，备份 YAML 与状态目录。配置文件可以保持原样——旧字段会忽略并告警，`chat_bindings` 在 owner 配对完成后自动生效。前台运行 `--pair`，在终端验证的私聊中完成配对，然后重新绑定各聊天。不要删除尚未处理的 transfer 状态文件。
- 本地标准输入/输出改用显式 `--console`，要求配置恰好一个工作区，不能与 `--pair` 同用。缺少微信 token 会触发扫码登录，不再隐式切换 console。
- 删除 `/question-decline`（Telegram 为 `/question_decline`），与 Chord 保持一致：问题只能回答或取消。取消问题请用 `/question-withdraw <id>`；不再显示 `declined` 结果。

### 新功能

- gateway 现在会在启动时以 `outbound proxy ...` 记录每个已配置出站代理(已去除凭据),方便确认实际解析到的值。
- 新增全局与各 IM 可选出站代理 `proxy_url`（`ims.<platform>.proxy_url`，优先级各 IM > 全局）。为空表示直连；环境代理变量会被忽略。支持 `http`、`https`、`socks5`、`socks5h`。HTTP 客户端四种都支持;WebSocket 长连接(Discord、Slack、飞书、企业微信、钉钉)支持 `http`、`socks5`、`socks5h`,并在启动时拒绝 `https`,同时把 `socks5h` 规范化成 `socks5`。单独配置的 `ims.<platform>.proxy_url` 不会被不兼容的全局默认值阻断:只有 WebSocket 平台实际解析到 `https` 代理时才会在启动时拒绝。
- `--pair` 启动后立即显示一个配对码，无需等待各平台连接完成。该配对码对所有已连接平台都有效，并在 10 分钟窗口内始终可用，可绑定多个聊天和平台；绑定成功不再使其失效。钉钉显示同样的启动码，但其绑定仍需通过同企业身份探测后才会放行。详见[配对说明](./docs/pairing_CN.md)。

- 新增 Telegram Bot API 长轮询：私聊/群纯文本、owner 配对、控制命令、分段输出与限流；已有 webhook 会阻止启动，不自动删除。详见 [Telegram 指南](./docs/telegram_CN.md)。

- 新增企业微信智能机器人长连接：私聊/群文本、配对探测、后台回复和账号互斥；网络故障退避重连，被其他连接替换时停止争抢。
- 新增单 workspace Slack Socket Mode：owner 私聊/频道/thread 输入，根聊天 `/chord` 命令、thread @Bot 控制，独立绑定、文本转义与限流；ACK 为发送留出时间，未确认接纳有诊断。详见 [Slack 指南](./docs/slack_CN.md)。

- 新增钉钉内部应用 Stream 文本与主动回复：每次启动先由 owner 私聊验证同企业身份，原有绑定保留；token 与验证结果只存内存。临时重连保留当前进程内已接纳工作，未知发送不重试。详见[钉钉指南](./docs/dingtalk_CN.md)。

- 新增 Discord Bot 接入：向外建立 Gateway 连接，私聊 owner 配对，服务器频道与既有公开/私有 thread 中显式 @Bot 文本输入，以及进程内消息 ID 去重。每个 thread 独立绑定，最小权限无需 Message Content privileged intent；输出不触发 mention 通知或链接 embed。未锁定归档 thread 可因发送恢复活跃。原生 interaction、按钮、附件、预览和自动创建 thread 延期；真实账号部署检查见 [Discord 指南](./docs/discord_CN.md)。


- 所有平台共用有界内存接纳、进程内去重、授权复核、聊天顺序与 Chord 消费回执；结果未知时不自动重发，`/status` 显示等待与隔离原因。

- 新增 owner 配对（`--pair`）、按目录 `/bind` 和本机 `pairing reset`。切换目录会保留已有配置，并要求先处理完待执行工作和会话转移。临时断线保留已接纳的 owner 输入与回复；账号、目录或进程变化后，旧输出不能借用新身份发送。文本和图片发送区分明确拒绝与结果未知，不自动重发结果不确定的消息。

- `/status` 和 `/mcp status` 展示 MCP 可用状态，对可见状态变化去重推送，并提示在宿主机的会话工作目录中登录。远程认证命令由 gateway 返回操作说明，不会转发给模型。

- 新增 macOS/Linux 下本机 TUI 与 IM 会话接管：`/takeover <session-id> [--timeout <duration>]` 等待既有工作自然结束，不取消执行，保留已接纳的排队输入和 TUI 附件，新端恢复后保持暂停。`/takeover status` 查询进度，展示有界阻塞项摘要和恢复指引；等待超时不停止源端。明确继续前，可用 `/status` 核对恢复队列的总数、所属任务、有界预览及附件数。移交阻断在 gateway 重启后仍然生效，普通消息、旧卡和提醒不能拉回已经移交出去的会话，只有显式 `/new` 或 `/resume` 成功后才解除原绑定的阻断。接管进来的目标确认暂停就绪、有效队列摘要并保存 pin 后才开放输入。需要 Chord 支持接管；可用 `chord_state_dir` 指定双方共用的自定义 Chord 状态目录。

- 新增 `event_visibility.background_result` 开关，可关闭后台任务完成结果到聊天窗口的推送。该推送默认开启；关闭后持久化结果仍保留在 Chord 会话中。

### 变更

- 微信在当前进程内逐批接纳后推进内存游标；同账号续期保留进度，临时回复上下文失效时等待新 owner 消息。超大批次有明确诊断且停止该平台接收，不跳过游标。

- 飞书 text/post 与可信卡片使用公共内存接纳和消费回执，话题群按群授权；非 Question 卡片等待命令终态更新，不把平台接纳误报为执行完成。保留 Question 表单、角色卡片与图片输出。

- 飞书文本发送现共用公共并发预算与 5 分钟总期限，每条输出最多 64 KiB / 32 段，截断时给出查看原会话的提示。明确 HTTP 429 拒绝只重试被拒绝段，等待可取消；部分失败保留已确认段数，未知结果不重发。


- 完成通知现在附带刚结束 turn 的紧凑摘要（时长、工具调用数、重试次数），`⏳ Chord: Ready for input; background jobs still running: N` 变体同样适用。长时间提醒现在显示已运行的 turn 时长、`running tools` / `retrying` 等粗粒度阶段，以及当前工作周期内观察到的工具与重试次数。同一会话内的确认、交接和取消操作不会清零累计进度。本地 shell 工作单独跟踪，不会重置 agent 进度；完成通知会显示仍在运行的 shell 命令数量。`connecting` 这类传输层 phase 仍然不会暴露。

- 配置中不支持或已移除的 YAML 字段现在会在 gateway 日志中带行号报告并被忽略，不再静默失效：拼写错误或 `event_visibility.activity` 这类已移除选项在启动时即可发现，文件其余部分仍正常加载。

- 配置文件中的重复 YAML key 不再阻止启动。保留首次出现的值，后续重复项以 `duplicate key ignored` 告警带行号报告，下一次配置变更时会将文件重写为去重后的内容。

- gateway 未配对时，私聊发送者现在会收到一条限量提示而不是被静默丢弃：如何配对、`/bind` 格式缺了什么，或配对码不正确。配对未激活时提示会要求以 `--pair` 重启。群聊、内部动作与重复重投保持静默，提示使用独立于配对尝试的限流预算。

- 企业微信开通步骤改为可照做的完整流程：管理后台入口更正为 `智能专区 → 智能机器人`（此前「安全与管理 > 管理工具」不是当前入口）；补充个人团队没有工作台与 Web 管理后台、两个创建入口均不可用，需改为企业团队或用「变更为企业账户」升级；补充「智能机器人创建权限」与「API 模式管理」两层权限及通讯录超过 1000 人时长连接范围默认为空导致看不到 Bot ID 和 Secret；明确企业认证不是前置条件、创建页「可使用权限」是 Gateway 不使用的 MCP/CLI 能力通道；补充可见范围需包含 owner、使用方式选择，以及客户端查找机器人的路径。排障文档同步新增这四类开通前阻塞的诊断。

- 启动配对提示与快速开始、配对、IM 指南统一改用 `/bind "<directory>" <pairing_code>`，不再使用 `~/workspace`——该目录在新主机上通常不存在，照抄示例的首次 `/bind` 会失败（「目录不存在或不可访问」）。占位符与运行时提示一致，仍要求加引号并绑定已存在的目录；Cookbook 与平台指南中的具体示例保持不变。

### 修复

- 控制台模式不再因休眠的聊天绑定而要求 `--pair`:其配对补救方式与 `--console` 互斥,因此在该模式下跳过检查。
- 配对提示回复改为按私聊单独计数,而非按平台共享,单个聊天的刷屏者不再能压掉同平台其他未配对用户的提示。
- `pairing reset` 现在会在重写配置前打印即将被丢弃的重复 key 与不支持字段告警,不再静默去重。
- Discord 在 @机器人 但不含可用内容时,改为回复有界、限流的诊断提示,不再静默丢弃。
- 修正 Slack 指南中的重连描述,使其与实际重投行为一致:接纳饱和时该帧不发送 ACK,而非本地退避。
- 补充说明企业微信单聊 callback 的 `from.userid` 仅在机器人创建者为超级管理员时才为明文,并把心跳 ACK 描述更正为连续两次丢失才关闭连接。
- 自动恢复 pin 重新按工作区和路由记录，不再按配对身份隔离。重新绑定同一聊天或更换平台账号时会延续原会话,不再静默开启新会话。本机 `pairing reset` 是明确的例外:它会连同 owner 和绑定一起清除该平台的自动 pin,重置后的路由从新会话开始。按身份隔离布局写入的 pin，以及当前 key 解析器不再寻址的 `v2:` 前管道符旧布局 pin，在加载时被丢弃，不会改写到其他身份。如升级后需要历史会话，先用 `/sessions` 查看，再用 `/resume <id>` 显式重新固定。注意：换 owner 重新配对会按单用户部署设计继承原会话；如需隔离请用 `/new` 或删除 pin。
- `/new` 现在回复 `Session created`，`/resume` 回复 `Session resumed`。此前两者都回复 `Session selected`，既没有表达实际结果，在 `/new` 时看起来还像失败。
- `/new` 与 `/resume` 在 Chord 进程忙碌时于路由变更屏障之前直接拒绝，并提示先 `/cancel`，不再进入替换后才失败。会话切换失败现在区分忙碌、移交中、输入未决等原因，不再只给出笼统的 `session_change_failed`。
- 会话接管不再为每个内部事件发送一条完整状态。Chord 的阻塞项集合每次变化都会重复发布状态，此前一次接管会在聊天中刷出一串几乎相同的消息；现在只汇报三件事：移交已开始、已等待多久以及在等什么、会话已保存。未完成的进度最快每分钟一条，内容未变化的报告直接丢弃，失败仍完整送达并保留恢复动作。等待时长为已观测耗时，不是完成预测。
- 会话转移或会话切换在路由变更屏障升起后失败时，现在会结算该路由。此前持久化移交保护失败会让该聊天一直以 `route_changing` 拒绝后续所有任务，直到重启 gateway；微信还会因此停在那条消息上不再推进批次。
- 工作区的 Chord 进程仍在启动时，`/new` 与 `/resume <id>` 不再被当作 control target 过期而拒绝。启动或升级网关后曾直接报 `control_target_expired` 而无法开会话的场景现已可以正常启动；已捕获到运行时身份、之后又发生变化的目标仍然拒绝。会话切换失败与被丢弃的 ready 事件现在会记录具体原因，不再只给出笼统的拒绝。
- Telegram 入站现在会记录每次拉取到的批次、每条 update 的捕获结果与派发处置，网关从未接纳的消息与 Telegram 从未投递的消息可以区分。持续无法进入终态的 update 会带上`update_id` 先告警一次、之后最多每分钟一次，并记录 `acceptance_unconfirmed` 指标，不再静默无限重试。本地重启后紧接着的 409 会短暂重试而不是直接退出；只有持续冲突仍为致命错误。
- 企业微信回答提示保留 `@request`，可直接复制回复。Slack 会话查询、身份核验及 Socket 重连统一遵守各 API 方法的 `Retry-After`。
- 部分发送诊断保留投递结果未知状态，包括发出请求后取消的情况；钉钉区分 token 获取、节流等待失败与消息投递未知。
- 接收容量检查改用按聊天维护的待处理索引，避免接纳消息时扫描保留的已完成历史。
- 超出有限响应队列的突发不再断开钉钉 Stream 与飞书长连接：溢出的单帧返回 500 等待重投，连接保持以排空工作队列。钉钉发送节流表增加上限，企业微信越界平台时间戳改为明确拒绝。
- 入站记录已过期（24 小时）后的迟到 Question 回执不再把进程输入标脏，而是跳过，等待 14 天问题操作窗口自然排空。

- 企业微信主动回复及绑定探测明确区分单聊和群聊寻址；智能助理和频道回调不再进入用户命令处理。
- Slack 聊天查询较慢时，ACK 等待结束后仍可在独立的有限期限内完成接纳，同进程重投继续去重。Slack ACK 提示和钉钉提及占位符统一遵循英文命令回复惯例。
- Telegram 在启动或轮询期间正常停止时成功退出，仍保留真实错误。

- 启动配置告警在取得资源锁后同时输出到标准错误和配置的 gateway 日志，避免未知字段提示丢失。

- 会话列表改为查询 Chord 配置的存储位置及仓库范围，支持自定义状态目录和 worktree，不再扫描旧工作区目录；查询失败会明确提示，需要 `chord sessions list --json`。

- 存在持久待处理输入、队列阻塞或回执结果不确定时，不再允许切换目录、闲置回收或替换会话，接管到已断线的绑定也遵循同一保护。`/new`、`/resume`、目录切换和闲置回收前会刷新此前观察到的恢复输入摘要，避免已完成的工作留下过期阻断。断线后仍可通过 `/status` 查看队列阻塞原因；不会自动重放请求。

- 空闲时请求取消不再把进程标记为 busy 或反复发送工作提醒。尚未返回的本地 shell 命令会在 agent 空闲事件和状态快照到达后继续保持 busy，也不会因空闲超时而被回收。

- Slack 现在立即 ACK `events_api` 消息，不再等待会话查询完成：慢查询不会错过 3 秒 ACK 期限并触发平台重投。仅当 `/chord` slash 命令声明可带响应载荷时才短暂等待以附带提示，普通接纳保持裸 ACK。Socket 存活判定也改为任何入站帧都有效，不再依赖 WebSocket 未承诺的 ping 间隔。

- Discord 的 Gateway Resume 缓存从 128 提升到 1024 个有效事件，较长的瞬时断线不会再变成致命重连循环。

- 企业微信对越界的 `create_time` 采用与其他平台一致的拒绝处理，不再静默改写为接收时间。


## v0.3.3 – 2026-10-12

### 新功能

- 向来源微信和飞书会话按顺序转发全部生成图片。飞书图片达到或超过上传上限时发送缩小预览，Chord 保留原图；图片处理减少瞬时内存占用，传输失败不会逐块刷屏或再次生图。微信 CDN 上传遇到瞬时故障时最多尝试三次；飞书上传仅在明确的 token 过期拒绝后刷新并重试一次；图片发送失败或结果未确认都不会重新生图。需要支持图片摘要和原图分块的 Chord headless。

- 按 ID 管理多道待答题，支持新题存在时修正历史答案；普通聊天保留问题，答题等待 Chord 可靠回执。IM 回答入口开放前停用题目计时，系统默认不会标为用户同意。

- 空闲提醒和 `/status` 会展示仍在运行的后台任务；报告有后台任务的进程不会因空闲超时被回收。

- 新增 IM `/role` 命令，通过 Chord headless 切换主 agent 角色（例如 `builder` → `planner`）：`/role` 列出当前角色与可切换角色（飞书为按钮卡片，文本平台为编号列表），`/role <编号>` / `/role <角色名>` 选择目标角色。gateway 现在始终订阅 Chord 的 `role_change` 事件，并在 `/status` 中跟踪当前主角色。需要支持 headless `role` 控制命令的 Chord 版本。切换请求失败或响应超时时，已点击的飞书卡片会更新为结果未确认，并提示用户在重试前用 `/status` 核实当前角色。飞书卡片点击只在原卡片上更新结果，不再额外发一条聊天消息。
- gateway 现在始终订阅 Chord 的 `compaction_status` 事件，并在 `/status` 中展示最近一次上下文检查点的结果（例如 `succeeded`、带原因的 `skipped`、`failed`）；这些结果不会作为聊天消息推送。未占用压缩槽位的跳过事件不会覆盖仍在运行中的压缩结果。
- Gateway 现在会消费 Chord 的 `handoff_cancelled` 事件：当待决 handoff 在未做决策的情况下被取消（例如被新的请求或会话切换取代）时，gateway 会丢弃已失效的待决请求，并向聊天发送取消通知。之后再用 `/handoff` 或 `/handoff-deny` 会回到常见的「没有待处理 handoff」提示，而不再把回复路由到已取消的请求。
- gateway 现在始终订阅 Chord 的 `background_result` 事件，并把后台任务完成后的持久化结果推送到聊天；Chord 侧没有其它投递通道，因此它此前在 IM 中完全不可见。持久化的上下文压力提示（`context_notice`）同样会被消费，但把它推到聊天属于产品取舍、而不是正确性要求，因此改为通过新增的 `event_visibility.context_notice` 开关选择启用（默认 `false`）。
- 多选问题现在也使用飞书交互卡片：选项变成表单里的勾选项，配合一个 `Submit` 按钮提交，勾选的选项会作为答案发出。未勾选任何选项就提交时，卡片给出提示，不会发出空答案。 无法识别的勾选值会被拒绝，不会被猜测为已选中。

### 变更

- 普通输入的忙碌状态与提醒改为跟随 Chord 消费回执及活动事件，避免本地命令导致错误的忙碌状态。activity 始终订阅，请从配置移除 `event_visibility.activity`。断线会提示未确认输入，不自动重放。需要支持输入回执的 Chord headless。

- 将 Go toolchain 要求更新到 1.27.0，并刷新第三方 Go 依赖（`golog` v0.4.1、飞书 SDK v3.10.0）。
- SubAgent 的 `agent_notify` 通知现在会带上告警子类型（例如 `blocked/stall_resolved`），便于区分已解除的阻塞与仍在持续的阻塞。
- 周期性的 `performance queue=...` 指标日志现在还会输出当前最深分片（`max_depth`）及已结束的队列容量等待时长（`blocked_wait_total`、`blocked_wait_max`、`blocked_slow`），帮助诊断通知背压。时长指标不包含仍在持续的等待或分片生产者锁等待。
- 飞书问题卡片不再因为选项文本过长而整题退回文本：任一选项超出按钮长度预算时，卡片整体改用序号按钮（`1`、`2`…），选项全文仍然列在卡片正文里；选项详情也不再参与是否使用卡片的判定。只有自由回答和超过 10 个选项的问题仍走文本形式。

- 消息发送现在须收到平台明确的成功回执，飞书还须返回消息 ID。缺少回执、传输失败或服务端错误时提示结果未确认，不再误报成功，也不会自动重发；非 JSON 响应不再当作已送达。此前这类发送可能被误报为已送达。

### 修复

- 消息请求不再跟随可能重放发送的重定向；非预期 HTTP 状态会提示结果未确认。平台明确拒绝（4xx，408/429 除外）会提示确定失败而非结果未确认，因此被拒绝的飞书卡片仍会回退到文本入口，而结果未确认的卡片不会重复发送原提示的文本版本。

- 飞书请求改用 tenant access token，不再使用 app access token；token 仍由配置的应用凭证自动刷新，包括普通 4xx 响应中明确的 token 过期拒绝。

- 状态快照提前清空 pending 后，问题卡片仍能正常收口。超时提示不再随后续 idle 重复发送，飞书提交答案后会保留原卡片直到结果到达。
- 飞书问题按钮使用稳定的选项 ID，重复或数字标签都能提交实际点击的选项。失败或结果未知的提交可以主动重试，回调按事件 ID 去重；卡片和文本都会把默认选择标为系统决定。答题入口展示失败后，可通过 `/status` 重试。回答本地缓存缺失的 ID 时会自动同步恢复的题目；终态通知会标明题目，已回答卡片保留所选答案。

- Chord 进程重启后输入 ID 仍保持唯一，避免与恢复的问题事务冲突。接受历史补答不再误报忙碌；等待输入期间提醒定时器不会连续空转，未回答的异步问题也不会抑制其他工作中的提醒。

- Headless JSONL 帧支持最大 16 MiB 的 JSON 正文，包括 JSON 转义后体积增大的输出。无法解析的行会在聊天中提示后继续读取；帧超限或读取流失败时，会在有界等待后停止 Chord 进程并向聊天报告错误，不会重放输入或触发崩溃自动重启。

- 确认或 handoff 提示成功投递后，只有对应请求的已识别伴随 `notification` 提醒会被去重。旧的待处理缓存不再遮蔽无关告警；提示投递失败时仍保留伴随提醒。本身没有专用提示的提醒（如 loop 退出决策、notify 协议纠正）仍会照常送达。
- 对于仅由配置操作（例如手动切换 model pool）导致的静默状态，gateway 现在仍会正确收口 headless idle 状态，但不再发送通用的“可输入”提示。待确认、问题和 handoff 的过期通知仍会正常发送。
- 按飞书开放平台当前行为修正了飞书接入指南：事件页面是 **事件订阅**（不是“事件与回调”），`im.message.receive_v1` 在控制台显示为 **接收消息 v2.0**；接收消息权限是**按场景分别生效**的——私聊和群聊都用，必须同时开私聊权限和群聊权限，而不是任选其一。指南同时补充了 **批量导入** JSON 的方式，并说明 `im:message:update` 不是必需项，因为 `im:message` / `im:message:send_as_bot` 已覆盖卡片更新调用。
- gateway 现在会跟随 Chord 的 `session_switched` 事件。此前 session pin 只在 Chord 进程启动时写入，因此进程内切换会话（handoff plan 执行、`/resume <id>`、`/new`）之后，已 pin 的绑定仍指向被放弃的那个会话，之后重新拉起进程会 resume 错误的会话。
- gateway 现在会丢掉被更新推送超车的 headless `status_response` 旧快照：Chord 给携带状态的 envelope 按单调 `seq` 编号，而 status 快照与推送分属不同 goroutine 发出，先拷贝的快照可能晚到，因此不能让它把 SessionID、busy 和待决交互回退。需要发送 `seq` 的 Chord headless 版本。
- gateway 现在会在关闭开始时取消待执行的崩溃自动重启，并停止空闲检查循环。此前，若 Chord 进程恰好在关闭前崩溃，会留下一个睡满整个重启延迟的 goroutine，并打印一条误导性的 `auto-restart failed ... chord manager is shutting down` 错误日志。

### 不兼容变更

- 需要支持输入回执和持久化问题集合的 Chord headless，不再支持旧的单问题协议。普通聊天不再自动回答待决问题，请使用 `/answer @<id> <answer>` 或当前飞书答题入口。
- 移除 `event_visibility.activity`，请从配置中删除。活动状态跟踪始终启用，YAML 中未知字段会被忽略。

- 移除 IM `/current` 命令。`/status` 已展示活动 session、当前角色、最近一次检查点结果和待处理交互。

## v0.3.2

### Added

- 新增 SubAgent 事件端到端转发：完成摘要现在始终订阅并发送到 IM，可选 `agent_started` / `agent_notify` 事件用于展示委托进展，SubAgent assistant 文本也会标注 agent 与 task 来源。
- 新增 Chord headless `handoff_request` 支持：gateway 现在默认订阅该事件，会展示完整 handoff plan 以及可选 agent / model pool，并支持 `/handoff <agent> [model_pool]` 或 `/handoff-deny <reason>` 响应。
- 新增 IM `!` / `！` 本地 shell 快捷入口，由 Chord headless `local_shell` 命令执行。Gateway 现在默认订阅 `local_shell_result`，并把 stdout/stderr 结果回显到 IM 会话。

### 不兼容变更

- 移除 gateway 对 Chord headless `tool_result` 的配置、订阅、状态和 IM 渲染支持。gateway 现在通过 Chord 的 `done_completion` 事件接收非 loop Done 报告；loop 模式的 Done 退出申请仍使用 `confirm_request`，并携带 `done_report` / `done_reason` 字段。

### Changed

- 精简中英文快速开始，以单工作区首次接入为主线；补充 Chord 官方安装入口；从配置参考中移除重复的飞书绑定教程；并明确 IM / 模型数据边界以及飞书允许名单要求。
- 文档站构建栈升级到 Astro 7 与 Starlight 0.41，声明的 Node.js 基线与 CI 保持一致，并刷新传递依赖，使生产依赖审计不再报告已知漏洞。
- 将 Go toolchain 要求更新到 1.26.3，并刷新第三方 Go 依赖。
- Done 确认渲染现在优先使用显式的 `done_report` / `done_reason` 字段；当存在待处理 Done 确认时，普通文本会作为拒绝理由处理。

### Fixed

- Gateway 管理的状态目录现在使用 `0700`，敏感持久化状态与轮转日志使用 `0600`；此前以较宽权限创建的已有文件也会在后续写入时被收紧。
- 修复 pinned Chord session 过期或不可用时的 IM 会话恢复行为：`/new` 会优先交给已有可用进程处理，必要时不再带过期 resume 启动新会话；普通消息在旧 session 已清理或被占用导致发送失败时会自动以新 session 重试一次；`/resume <id>` 现在会提示 session 不存在或被占用，而不是误报成功；已退出的 Chord 进程也不会再被复用。

## v0.3.1 – 2026-05-11

### Changed

- 将 Chord 集成中旧的 `Bash` 工具名更新为不兼容变更后的 `Shell` 工具名，并让命令执行确认的风险分级与摘要渲染匹配当前 Chord 事件。
- 改进跨仓库 headless 契约测试，使其符合当前 Chord 配置要求，并在 ready 超时时输出更有用的诊断。
- 收紧 CI 质量检查：保持覆盖率门槛为 70.0%，对齐 staticcheck 执行方式，并新增 gopls 校验任务。

### Added

- 在 `chord-gateway --version` 中加入紧凑的 gateway 构建身份输出（版本号、短 commit，以及在适用时显示 dirty 标记）。启动日志现在会记录 `gateway_version`、`gateway_commit`、`gateway_build_time`、`gateway_vcs_time`、`gateway_dirty` 和 `go_version`。启动子 `chord headless` 的日志现在也会记录配置的 `chord_binary` 路径和 mtime，便于排查版本来源。

## v0.3.0 – 2026-05-05

### Breaking changes

- 移除 `HandleMessage(imType, chatID, text)` router 入口；改用 `HandleIncomingMessage` 配合结构化的 `IncomingMessage`。
- 移除 `config.yaml` 中 `ims` 与 `workspaces` 的 sequence（列表）写法，两者必须为按 adapter 类型 / workspace id 索引的 mapping。
- 移除协议/状态模型中未被消费的字段：`ConfirmPayload.TimeoutMS`、`ConfirmPayload.AlreadyAllowed`、`QuestionPayload.TimeoutMS`、`IncomingMessage.ConversationID`、`ControlState.LastStatusResponseAt`。下游若依赖这些字段，请改用等价方式（例如使用 `WaitStatus` 等待 `status_response`，而非轮询 `LastStatusResponseAt`）。
- 将原本在 `main` 包中私有的 `normalizeIMType` 统一到 `config.NormalizeIMType`，作为跨包唯一入口；旧的 `main.normalizeIMType` 已删除，下游代码请改用 `config.NormalizeIMType`。

### Added

- 新增兼容策略文档，记录余下的兼容面（Chord headless `todos` 事件名）以及清理规则。
- 新增 session pin 回归测试，覆盖写盘失败和并发更新场景。
- 新增 idle 事件渲染和普通 idle 清理待确认状态的回归测试。
- 恢复 WeChat 关键回归测试覆盖，包括持久化 token / sync 状态加载、过期 token 自动重新登录、自定义 token 路径、`splitText` 以及响应 context 取消的 sleep 行为。
- 新增 `SessionLoginNotifier` 接口，IM adapter 现在仅依赖该窄接口完成跨 IM 通知，不再持有完整的 `*NotificationRouter`。
- 新增 `ChordProcess.BeginTurn` / `MarkVisibleOutput` 方法，router 不再越过封装直接修改 `ChordProcess.state`。
- 新增 `ControlState.applyPendingConfirm` / `applyPendingQuestion` / `applyStatusResponse` 辅助方法，"接收到的待交互"处理在一个地方完成，避免分支遗漏。

### Changed

- 在不改变文档化行为的前提下，按主题拆分 router 与 process 实现文件（`router_commands`、`router_format`、`router_feishu_cards`、`router_reminders`、`router_parse`、`process_protocol`、`process_lifecycle`、`process_env`）。
- 将所有 rune 截断辅助函数（`truncate`、`truncateLine`、`truncateButtonLabel`、`shortID`、`truncateStderrTail`）合并到统一的 `text.go`，所有 IM 输出共享一份实现。
- 将飞书相关辅助（`buildFeishuResolvedCard`、`displaySender`、`updateFeishuCardStatus`、`buildExpired*Followup`）集中到 `router_feishu_helpers.go`，并新增 `resolveFeishuCard(...)` 包装方法，消除 router 命令路径中 7 处重复的 `updateFeishuCardStatus + buildFeishuResolvedCard` 写法。
- 抽出 `submitQuestionAnswer` 共用方法，让 `/answer` 与“普通文本回退到回答”两条路径共享同一份分发实现。
- 抽出 `compositeKey` 工具，process key、Feishu 去重 key 与 Feishu 卡片 handle key 共用同一种字符串拼接形式。
- 启用 `todos` 事件可见性后，现在每次事件都会直接转发完整的当前任务列表，而不再只提示当前进行中的单项任务。
- `/deny` 现在接受可选的人类可读拒绝理由文本，而非平台内部的 request_id；gateway 会自动匹配当前待确认请求。
- `/new` 现在通过 stdin 发送命令给 chord，不再直接杀死进程，让 chord 优雅地管理会话生命周期。
- `/bind` 现在在进程正在执行时拒绝绑定变更；请先 `/cancel` 取消当前任务。
- 飞书确认/提问卡片不再在按钮里嵌入用户可见的文本命令，而是通过结构化的内部动作回调，保持 IM 协议干净。
- 飞书 post 富文本消息现在可以像纯文本一样被解析，允许在富文本编辑器中发送命令。
- 将 router 的配置读取统一到 `ChordManager`，作为单一配置真理源，避免 `/bind` 更新时还要手动保持 router 与 manager 两份配置副本同步。
- session pin 与 dedupe 持久化现在使用原子替换写入；同时修复 session pin 更新逻辑，确保写盘失败不污染内存状态，并避免并发更新丢失 pin。
- 明确飞书续期行为：用户文档和跨 IM 通知现在说明飞书 access token 会基于已配置的应用凭证自动刷新，`/login feishu` 不受支持，且不应在 IM 会话中发送或修改应用凭证。
- `/status` 不再轮询 `LastStatusResponseAt`，改为通过带缓冲的 channel 等待下一次 `status_response`，IM 消息消费 goroutine 不再被阻塞最多 10 秒。
- `truncate` 与 `splitText` 改为按 rune 截断/分块，含中文或 emoji 的通知不会再在多字节字符中间被切断。
- 通过 `atomic.Pointer` 加固 `ChordManager.cfg` 与 `WechatAdapter.token` 的并发访问，`/bind` 引发的配置更新与 WeChat token 刷新不再与读路径竞争。
- Feishu 发送/更新接口抽出统一的 `doFeishuJSONRequest`，access token 过期重试逻辑只在一个地方维护。
- 飞书交互式确认/问题卡片现在会携带更完整的上下文，并在批准或回答后尽力把原卡片更新为最终状态；如果卡片发送或更新失败，仍会回退到现有文本通知。
- 对飞书待回答问题直接发送普通文本时，gateway 现在会在可能时更新原始问题卡片；同时卡片更新会优先使用发送时记录的消息 ID，而不是回调元数据，避免更新到错误消息。
- gateway 现在直接使用 `github.com/keakon/golog/log` 记录日志，并使用 `github.com/keakon/golog` 进行文件轮转；轮转后的日志不再 gzip 压缩。
- Chord `idle` envelope 现在由 gateway 渲染为用户可见的 ready 通知，不再依赖额外的 headless `notification` envelope。
- 将 `github.com/keakon/golog` 更新到 v0.3.0。
- 将非中文文档和 IM 响应中剩余的运行时/用户可见文案统一为英文。
- `handleChordCommand` 改为接收显式的 `*IncomingMessage` 参数，取代原先用 variadic 模拟的“可选 1 个”，让“是否携带原始消息”语义清晰可见。

### Removed

- 删除未被使用的辅助方法和死字段：`MultiAdapter.BroadcastText`、`MultiAdapter.Adapters`、`FeishuAdapter.SendInteractive`、`WechatAdapter.sessionExpired` 标志，以及 `ControlState.StreamText`、`LastThinkingText` 字段。
- 移除 Chord headless `todos` raw array 负载（`[...]`）支持；gateway 现在只接受当前 wrapper 负载格式（`{"todos":[...]}`）。
- 删除冗余的 `WechatAdapter.sleep` 包装、`NewFeishuAdapter` 中的 `runLongConn` 自赋值、`cloneStringMap` 工具（用 `maps.Clone` 替代），以及不再被使用的 `feishuDedupeKeyFmt` / `feishuCardActionFmt` 格式常量。

### Fixed

- `pins.Set` 失败（如 `/new`、`/resume`、`/bind`）现在以 warn 级别日志输出，不再被静默吞掉。
- 补回 router 配置与进程管理器访问路径上的 nil 防护：当 router 在没有 manager 或没有活动配置的情况下被构造时，`HandleIncomingMessage` 和 session 重启路径现在会返回用户可见错误，而不是 panic。
- 移除每次普通 `send` 后都附带的调试用 `status` 命令，减少冗余的 stdin 调用。
- `truncateLine` 现在也改为按 rune 截断，工具参数摘要不再把中文或 emoji 截断在多字节字符中间；并补充回归测试以确保输出保持有效 UTF-8。
- 修正 `Config.UnmarshalYAML` 注释，使其与当前行为一致：只支持 mapping 形式的 `ims` / `workspaces`。
- 修复 dedupe 持久化：通过查询路径移除过期项时现在会正确标记为待写盘，且成功提交后不会再触发冗余 cleanup 重写。
- 当 Chord 正在等待确认或问题回答时，不再发送长时间运行的 `⏳ Still working` 提醒。
- 修复普通 Chord `idle` 处理：陈旧的待确认状态会被清空，但不会被报告为已过期；过期提示现在只用于 gateway idle timeout 终止进程的场景。
- 修复 `/bind` 和 `/resume` 的 busy 检查：现在只检查已有进程，不会意外启动新的 Chord 进程。
- 修复 dedupe cleanup 持久化：写盘失败时会保留 dirty 状态，下一次 cleanup tick 可以继续重试。

## v0.2.0 – 2026-04-30

### Added

- 新增飞书 `/bind` 绑定命令，仅支持立即更新内存/YAML 中的飞书 `chat_bindings` 和 `workspaces`。

### Changed

- 飞书现在只通过长连接模式接收事件。
- 长时间处于 busy 的 turn 现在会在任务仍在进行时每 5 分钟持续发送简短提醒。
- 启用 `todos` 事件可见性后，现在每次事件都会直接转发完整的当前任务列表，而不再只提示当前进行中的单项任务。
- `/bind` 现在会更严格地拒绝格式错误输入：未闭合引号、多余参数、不支持的路径前缀、不可访问路径和非目录路径都会失败，且不会修改配置文件。
- 飞书相关的配置、运维、安全、排障与使用文档已统一更新为长连接模式，并补充 `/bind` 路由说明。
- 待回答问题/待确认请求过期时现在会通知用户；迟到的 `/answer`、`/allow` 和 `/deny` 会作为后续上下文转发，而不会被当作结构化响应处理。

### Removed

- 移除飞书 webhook 模式及其相关配置字段（`verification_token`、`encrypt_key`、`listen`、`webhook_path`）。

## v0.1.0 – 2026-04-29

### Added

- 微信 iLink 模式，支持二维码登录和 token 持久化。
- 飞书机器人模式。
- 多 IM 模式，可同时运行微信和飞书。
- IM 会话过期时的跨 IM 登录通知。
- 按聊天和工作区隔离的 session pin 机制。
- IM session 命令：`/new`、`/resume`、`/sessions`、`/current`。
- 飞书 owner allowlist，支持 `owner_open_id` 和 `allowed_open_ids`。
- 飞书事件去重，并持久化到 gateway 状态目录。
- 可配置的可选控制面事件可见性。
- 为启动的 `chord headless` 进程提供进程组清理。
- 补充使用、运维、安全、排障和配置文档。

### Changed

- README 调整为简洁发布入口页，详细行为迁移到 `docs/`。
- 最终 assistant 消息会实时推送；`/summary` 不再属于文档化命令集。
- 补充了 router、多适配器、WeChat 辅助逻辑、adapter factory 与配置路径的单元测试；共享 Go 质量检查现已在 pre-commit 与 CI 中统一执行，并要求覆盖率 >= 60.0%、通过 `go vet` 和 `staticcheck`。
- 新增 GitHub Actions 发布 workflow，可在标签推送时构建多平台二进制归档并生成校验和。
- 通过平台特定实现隔离 Unix 进程组 syscall，修复 Windows 构建兼容性。
- contract blackbox 测试构建临时 `chord` 二进制时禁用 VCS stamping，提升 hook 与 CI 执行稳定性。

### Security

- 新增 IM 访问控制、飞书凭据处理和 workspace 范围控制的安全说明。
