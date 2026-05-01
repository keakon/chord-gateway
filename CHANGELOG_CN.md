# 变更日志

本文档记录 `chord-gateway` 的重要变更。

本项目使用简单的人类可读变更日志格式。日期格式为 `YYYY-MM-DD`。

- English: [CHANGELOG.md](./CHANGELOG.md)

## Unreleased

### Added

- 新增兼容策略文档，记录当前仍支持的 legacy 配置形态、向后兼容的 router 入口，以及仍在生效的 headless `todos` 事件协议。
- 新增 session pin 回归测试，覆盖写盘失败和并发更新场景。

### Changed

- 在不改变文档化行为的前提下，按主题拆分 router 与 process 实现文件（`router_commands`、`router_format`、`router_feishu_cards`、`router_reminders`、`router_parse`、`process_protocol`、`process_lifecycle`、`process_env`）。
- session pin 与 dedupe 持久化现在使用原子替换写入；同时修复 session pin 更新逻辑，确保写盘失败不污染内存状态，并避免并发更新丢失 pin。
- 明确飞书续期行为：用户文档和跨 IM 通知现在说明飞书 access token 会基于已配置的应用凭证自动刷新，`/login feishu` 不受支持，且不应在 IM 会话中发送或修改应用凭证。
- 飞书交互式确认/问题卡片现在会携带更完整的上下文，并在批准或回答后尽力把原卡片更新为最终状态；如果卡片发送或更新失败，仍会回退到现有文本通知。
- 对飞书待回答问题直接发送普通文本时，gateway 现在会在可能时更新原始问题卡片；同时卡片更新会优先使用发送时记录的消息 ID，而不是回调元数据，避免更新到错误消息。

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
