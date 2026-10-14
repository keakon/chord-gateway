# 运维说明

所有 IM 平台统一使用单 owner 配对与显式聊天绑定，单工作区也需绑定。首次运行 `chord-gateway --pair`，在机器人私聊发送 `/bind "~/workspace" <pairing_code>`；详见[配对与本机重置](./pairing_CN.md)。旧部署升级步骤见[更新日志](../CHANGELOG_CN.md)。

本文档说明运行和运维 `chord-gateway` 时需要关注的行为。

## 进程生命周期

gateway 会按需为每个活跃绑定启动 `chord headless` 子进程。

每个 Chord 子进程会被放入独立进程组。gateway 正常关闭时，会尝试终止整个进程组：

1. 关闭 stdin，请求进程优雅退出。
2. 向进程组发送 `SIGTERM`。
3. 等待一小段宽限时间后，向进程组发送 `SIGKILL`。

这样可以避免孤儿 Chord 进程或其子进程继续持有 session lock 或工作区资源。

如果 gateway 自身被 `SIGKILL`（`kill -9`）杀死，则无法执行清理 handler。此时依赖 Chord 的 parent-death watcher 检测父进程变化并尽快退出。

如果 Chord 进程崩溃（gateway 并未请求其停止却退出），gateway 会在短暂延迟后自动重新拉起；若此时 gateway 正在关闭，则取消这次待执行的重启。会话移交中的进程也不会自动重启；成功移交使用退出码 `20`。即使最终事件丢失或进程异常退出，移交阻断仍然生效。

## 会话移交恢复

在受影响的 IM 聊天中使用 `/takeover status`；绑定被阻断时也可用 `/status`。查询不会新建移交或改用全新会话。对于已有的接管到当前聊天的移交，查询可以在源端释放后完成暂停目标的恢复。在 gateway 所在主机上，以源端相同的工作区和 Chord 状态目录查询：

```sh
chord takeover status -d /path/to/workspace --session <session-id> --transfer-id <transfer-id> --json
```

IM 通知只展示简短的阻塞项预览。宿主机命令返回一页有界结果及总数 `blocker_count`；若包含 `next_offset`，可追加 `--blocker-offset <next_offset>` 再次查询下一页。

如果 Chord 使用自定义状态目录，追加 `--state-dir /path/to/chord-state`，并在 gateway 的 `chord_state_dir` 配置相同路径。它与 gateway 自己的状态目录相互独立。

- 等待超时只结束观察，源端继续自然收口并持有会话锁。检查阻塞项，等待既有工作结束。长期任务可能一直阻塞；停止该任务属于用户单独发起的操作。
- 保存失败时源端继续冻结并持有锁。处理报告的存储问题，再按原移交 ID 重试保存：

  ```sh
  chord takeover retry-save -d /path/to/workspace --session <session-id> --transfer-id <transfer-id> --json
  ```

- 源端已经保存、目标恢复失败时，先核对工作区、角色配置与会话锁状态，再显式发送 `/resume <session-id>`。暂停检查点恢复后不会调用模型，直到用户明确继续或发送新输入。继续之前，用 `/status` 核对恢复队列的数量、所属任务、预览及附件数。
- 观察中断时查询原移交。没有回执不代表源端恢复了执行，也不代表目标已经启动；协调进程不可用时，不保证目标会无人值守地自动启动。
- 在自然收口期间重启或停止 gateway 属于外部中断，不是成功自然移交；阻断记录会保留供恢复处理。

`<state_dir>/session-transfers.json` 在源端确认准备之前记录绑定阻断，重启后优先于 session pin。普通消息、提醒、旧卡和崩溃恢复不能隐式拉回已经移交的会话。接管到当前聊天的会话确认 `ready_paused` 且 pin 保存成功后，目标绑定会开放输入，等待用户明确继续。已经把会话移交出去的绑定继续阻断，直到显式 `/new` 或 `/resume` 成功；失败时保留阻断。清除阻断时使用私有的 `session-transfers.json.pending` 日志，确保磁盘同步失败后重启不会误开放绑定。若存储故障留下该日志，先处理故障再显式重试；源端所有权不确定时，不要删除任一阻断文件来强行触发自动重启。

会话接管通知通过有序发送队列投递。慢速 IM 发送不会占用进程生命周期锁；队列满时，有界队列的背压仍可能延缓协议读取。

## 空闲超时

`idle_timeout` 控制空闲 Chord 进程可保留多久。默认值为 `30m`。即使进程正在等待待回答问题、待确认请求或待处理 handoff 请求，也使用同一个 timeout。报告有活跃后台任务的进程会继续保留，直到这些任务不再计入运行数量；可通过 `/status` 查看。本地 shell 命令的结果尚未返回时，也会继续保留进程。移交准备和自然收口期间同样不会执行空闲回收。

恢复输入摘要可能过期时，闲置回收会先刷新再判断。成功的状态刷新仍会重新计算闲置时间；摘要确认没有待处理输入后，无需手动 `/status` 即可恢复正常回收。刷新失败则保留进程。

清理空闲进程会关闭该 Core 并暂停题目计时，不编造问答终态。重新连接并恢复会话后按原 ID 回答；先检查 `/status`，以 Chord 回执确定结果。权限与 Handoff 请求继续按各自的取消规则处理。

使用 Go duration 语法，例如：

```yaml
idle_timeout: 30m
```

## 状态目录

运行态状态按以下优先级存储：

1. `$CHORD_GATEWAY_STATE_DIR`
2. `$XDG_STATE_HOME/chord-gateway`
3. `~/.local/state/chord-gateway`

对新手来说，这意味着：

- 如果你没有额外设置，状态通常会落在 `~/.local/state/chord-gateway`
- 如果你找不到日志或微信 token 文件，优先检查这个目录
- 如果你运行多个 gateway 实例，除非你明确需要共享状态，否则请给它们分别使用独立状态目录

状态数据包括：

- 日志
- 微信 token 文件（默认 `<state_dir>/wechat/token.json`，或 `ims.wechat.token_path`）
- session pin 存储
- 会话移交阻断（`session-transfers.json`）

Gateway 原子保存微信 token、session pin 和移交保护；入站队列、去重与轮询游标只存内存。

## 升级与停止服务

停服前通过 `/status` 检查待处理输入，并在 Chord 原会话核对未知结果。重启会清空 Gateway 内存队列，不能用重启代替核查。升级前备份配置、凭据、session pin 和移交保护；Chord 会话由 Chord 自己保存。

旧版本的 `dedupe.json`、微信 `sync-buf.json` 和开发版本的 `im-ingress.db` 不再读取、导入或更新，也不会自动删除。首次启动即使用空收件状态；旧消息若由平台再次送达，可能重复执行。回退旧版前先核对会话结果，不要依赖陈旧游标防重。

同主机、同 OS 用户的同账号只能运行一个实例；跨用户或主机部署需要自行保证单实例。微信同账号续期在当前进程内保留游标；切换账号需本机 reset 后重新配对。

## 配置文件解析

gateway 按以下优先级加载配置：

1. `--config` / `-f`
2. `$CHORD_GATEWAY_CONFIG`
3. `$XDG_CONFIG_HOME/chord-gateway/config.yaml`
4. `~/.config/chord-gateway/config.yaml`

## 日志

gateway 会把 golog 格式化的 key-value 日志写入 stderr，并写入状态目录下的滚动日志文件。
默认日志文件为 `<state_dir>/gateway.log`；可通过 `CHORD_GATEWAY_LOG_FILE` 覆盖。
日志文件达到 10 MiB 后轮转，并保留 3 个备份。轮转后的日志不会 gzip 压缩。

gateway 会记录关键路由阶段：

- `chord-gateway starting` – 启动元数据，包括 `gateway_version`、`gateway_commit`、`gateway_build_time`、`gateway_vcs_time`、`gateway_dirty` 和 `go_version`
- `chord process spawned` – 子 Chord 进程元数据，包括 `chord_binary` 和 `chord_binary_mtime`
- `gateway event` – 从 `chord headless` stdout 解析出的原始事件
- `gateway routing event` – router 对某个绑定的处理决策
- `gateway sending notification` – 尝试向 IM 发送通知
- `performance queue=...` – 每 5 分钟记录的队列指标，覆盖 outbound 通知分片，包含队列 `depth`、`max_depth`、`full`、`closed`、`blocked`，以及阻塞入队的等待时长（`blocked_wait_total`、`blocked_wait_total_delta`、`blocked_wait_max`、`blocked_slow`、`blocked_slow_delta`）。其中 `*_total` 和 `blocked_slow` 是进程生命周期累计值；判断近期 provider 是否停滞时应看对应的 `_delta`。`max_depth` 是采样时最深的分片深度，不是历史最大值。

等待时长只在队列容量等待结束时记录，包括成功入队和 dispatcher 关闭两种情况；不包含等待分片生产者锁的时间，也不代表端到端投递延迟。因此，仍在持续的阻塞可能对应为零的等待时长 delta，应同时检查 `depth`、`max_depth`、`blocked` 和 `processed_delta`。`blocked_wait_max` 是生命周期内已结束等待的最大时长；`blocked_slow` 统计已结束且达到一秒的等待。背压也可能来自流量突增，而不一定是 provider 停滞。

常用字段包括：

- 事件类型
- workspace ID
- IM 类型
- chat ID
- session ID
- busy/idle 状态
- phase
- last outcome

## 飞书接纳与去重

text/post 和可信卡片回调共用内存收件。`message:<message_id>` 与 `card:<event_id>` 按 App ID 隔离，当前进程内去重。平台成功响应表示接纳或明确拒绝，不证明 Chord 执行完成；卡片更新失败不撤销已经提交的操作。

文本发送共用最多 8 个并发、5 分钟总期限和每条 64 KiB / 32 段限制；控制回复期限为 15 秒。明确的 HTTP 429 只重试被拒绝段；未知结果停止发送，`/status` 显示部分失败。见[飞书指南](./feishu_CN.md)。

## 工作区路由

所有平台都通过显式 `chat_bindings` 选择工作区；owner 使用对应平台命令入口分别绑定各目标。

飞书支持多个工作区，可通过 `ims.feishu.chat_bindings` 把飞书 `chat_id` 映射到 workspace ID。

每个飞书聊天都需要 `chat_bindings` 条目，单工作区也不例外。未绑定的新群只收到提示，请先执行 `/bind` 再发送任务。

## 事件协议

gateway 运行 `chord headless` 并从 stdout 读取 JSONL 事件。默认订阅以下控制面事件：

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

可选事件由 `event_visibility` 控制；`background_result` 默认订阅，可用 `event_visibility.background_result: false` 关闭。各事件的行为以及哪些只更新状态、不发消息，详见 [event-visibility_CN.md](./event-visibility_CN.md)。

飞书卡片发送结果未确认时，Gateway 发送提示，不再把原提示重复转为文本。请先检查聊天记录；未收到问题入口时可发送 `/status`，角色菜单可发送 `/role` 重新查看。消息请求不会跟随重定向，非预期 HTTP 状态按结果未确认处理。平台明确拒绝卡片时，仍会发送文本入口。`/status` 展示 Chord 状态并可重新打开问题入口，不会查询平台投递回执。

微信和飞书的图片转发需要 Chord 已启用内置生图，或对话模型已配置获准的服务端生图工具。Gateway 无需额外启用生图配置。飞书机器人需要图片上传（`im:resource`）与机器人消息发送（`im:message:send_as_bot`）权限。平台上传失败或发送结果未确认时会分别提示，不自动重发或重新生图。结果未确认时请先检查聊天记录，原图仍可在 Chord 中查看。

## 消息投递与重启

Gateway 是 IM 转发与控制面集成层。入站队列、去重、平台游标、临时回复上下文和投递回执只保存在内存；Chord 负责会话和任务状态。Gateway 不提供跨重启收件恢复，也不保证跨重启不丢消息或不重复执行。

平台 ACK 只确认 Gateway 已在内存接纳或明确拒绝；`consumed` 表示 Chord 接纳输入，不表示任务成功。ACK 后、Chord 接纳前退出可能丢输入；Chord 接纳后、平台确认前断开可能产生重投。各平台重投能力不同，不能依靠“不 ACK”保证重投，也不能把 Chord request ID 当作跨重启幂等键。

运行期同聊天保持顺序、冻结控制目标并重新检查授权。`unknown` 表示任务可能已经执行，不自动重发；先用 `/status` 并核对 Chord 原会话。当前路由的隔离输入阻止继续投递；显式切换不会把旧输入授权给新会话。缺少 IM 回复不代表操作未执行。

默认每聊天最多 64 条待处理输入，每账号 1,024 条 / 16 MiB，全局 4,096 条 / 64 MiB；未知与隔离记录计入上限。授权 `/status` 有一条数量预留，但仍受字节上限约束。轮询不能越过前面未接纳的输入；等待容量释放。全部记录另受 100,000 条及约 64 MiB 序列化大小预算限制，实际堆内存含索引等开销。终态清空正文，去重记录保留 24 小时并在后续接纳时回收；未决记录不自动淘汰。输入超过原始 24 小时投递窗口会隔离。

`/status` 展示等待、隔离与发送失败；运行指标记录接纳结果和回执。控制回复在排队和发送间共享 15 秒预算，拥塞可能丢掉回复，但不撤销操作。没有离线 `ingress` 数据库维护命令。停止前核查未决输入；结果确认后，重启可清空内存阻断，但不能取消或撤销已经交给 Chord 的任务。

配置、登录凭据、session pin 和移交保护仍按各自用途保存。绑定配置写入结果未知时，停服并核对 YAML 与 Chord 会话后再操作；不要删除移交保护来解除阻断。
