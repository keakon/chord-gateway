# 变更日志

本文档记录 `chord-gateway` 的重要变更。

本项目使用简单的人类可读变更日志格式。日期格式为 `YYYY-MM-DD`。

- English: [CHANGELOG.md](./CHANGELOG.md)

## 未发布

### 新功能

- 新增 IM `/role` 命令，通过 Chord headless 切换主 agent 角色（例如 `builder` → `planner`）：`/role` 列出当前角色与可切换角色（飞书为按钮卡片，文本平台为编号列表），`/role <编号>` / `/role <角色名>` 选择目标角色。gateway 现在始终订阅 Chord 的 `role_change` 事件，并在 `/status` 中跟踪当前主角色。需要支持 headless `role` 控制命令的 Chord 版本。切换请求失败或响应超时时，已点击的飞书卡片会更新为结果未确认，并提示用户在重试前用 `/status` 核实当前角色。飞书卡片点击只在原卡片上更新结果，不再额外发一条聊天消息。
- gateway 现在始终订阅 Chord 的 `compaction_status` 事件，并在 `/status` 中展示最近一次上下文检查点的结果（例如 `succeeded`、带原因的 `skipped`、`failed`）；这些结果不会作为聊天消息推送。未占用压缩槽位的跳过事件不会覆盖仍在运行中的压缩结果。
- Gateway 现在会消费 Chord 的 `handoff_cancelled` 事件：当待决 handoff 在未做决策的情况下被取消（例如被新的请求或会话切换取代）时，gateway 会丢弃已失效的待决请求，并向聊天发送取消通知。之后再用 `/handoff` 或 `/handoff-deny` 会回到常见的「没有待处理 handoff」提示，而不再把回复路由到已取消的请求。
- gateway 现在始终订阅 Chord 的 `background_result` 事件，并把后台任务完成后的持久化结果推送到聊天；Chord 侧没有其它投递通道，因此它此前在 IM 中完全不可见。持久化的上下文压力提示（`context_notice`）同样会被消费，但把它推到聊天属于产品取舍、而不是正确性要求，因此改为通过新增的 `event_visibility.context_notice` 开关选择启用（默认 `false`）。

### 变更

- 将 Go toolchain 要求更新到 1.27.0，并刷新第三方 Go 依赖（`golog` v0.4.1、飞书 SDK v3.10.0）。
- SubAgent 的 `agent_notify` 通知现在会带上告警子类型（例如 `blocked/stall_resolved`），便于区分已解除的阻塞与仍在持续的阻塞。
- 周期性的 `performance queue=...` 指标日志现在还会输出当前最深分片（`max_depth`）及已结束的队列容量等待时长（`blocked_wait_total`、`blocked_wait_max`、`blocked_slow`），帮助诊断通知背压。时长指标不包含仍在持续的等待或分片生产者锁等待。

### 修复

- 对于仅由配置操作（例如手动切换 model pool）导致的静默状态，gateway 现在仍会正确收口 headless idle 状态，但不再发送通用的“可输入”提示。待确认、问题和 handoff 的过期通知仍会正常发送。
- 按飞书开放平台当前行为修正了飞书接入指南：事件页面是 **事件订阅**（不是“事件与回调”），`im.message.receive_v1` 在控制台显示为 **接收消息 v2.0**；接收消息权限是**按场景分别生效**的——私聊和群聊都用，必须同时开私聊权限和群聊权限，而不是任选其一。指南同时补充了 **批量导入** JSON 的方式，并说明 `im:message:update` 不是必需项，因为 `im:message` / `im:message:send_as_bot` 已覆盖卡片更新调用。
- gateway 现在会跟随 Chord 的 `session_switched` 事件。此前 session pin 只在 Chord 进程启动时写入，因此进程内切换会话（handoff plan 执行、`/resume <id>`、`/new`）之后，已 pin 的绑定仍指向被放弃的那个会话，之后重新拉起进程会 resume 错误的会话。
- gateway 现在会丢掉被更新推送超车的 headless `status_response` 旧快照：Chord 给携带状态的 envelope 按单调 `seq` 编号，而 status 快照与推送分属不同 goroutine 发出，先拷贝的快照可能晚到，因此不能让它把 SessionID、busy 和待决交互回退。需要发送 `seq` 的 Chord headless 版本。
- gateway 现在会在关闭开始时取消待执行的崩溃自动重启，并停止空闲检查循环。此前，若 Chord 进程恰好在关闭前崩溃，会留下一个睡满整个重启延迟的 goroutine，并打印一条误导性的 `auto-restart failed ... chord manager is shutting down` 错误日志。

### 不兼容变更

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
