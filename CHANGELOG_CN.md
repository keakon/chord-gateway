# 变更日志

本文档记录 `chord-gateway` 的重要变更。

本项目使用简单的人类可读变更日志格式。日期格式为 `YYYY-MM-DD`。

- English: [CHANGELOG.md](./CHANGELOG.md)

## 0.3.1 – 2026-05-11

### Changed

- 将 Chord 集成中旧的 `Bash` 工具名更新为不兼容变更后的 `Shell` 工具名，并让命令执行确认的风险分级与摘要渲染匹配当前 Chord 事件。
- 改进跨仓库 headless 契约测试，使其符合当前 Chord 配置要求，并在 ready 超时时输出更有用的诊断。
- 收紧 CI 质量检查：保持覆盖率门槛为 70.0%，对齐 staticcheck 执行方式，并新增 gopls 校验任务。

### Added

- 在 `chord-gateway --version` 中加入紧凑的 gateway 构建身份输出（版本号、短 commit，以及在适用时显示 dirty 标记）。启动日志现在会记录 `gateway_version`、`gateway_commit`、`gateway_build_time`、`gateway_vcs_time`、`gateway_dirty` 和 `go_version`。启动子 `chord headless` 的日志现在也会记录配置的 `chord_binary` 路径和 mtime，便于排查版本来源。

## 0.3.0 – 2026-05-05

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

## 0.2.0 – 2026-04-30

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

## 0.1.0 – 2026-04-29

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
