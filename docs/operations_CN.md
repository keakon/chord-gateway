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
- 微信同步游标（`<state_dir>/wechat/sync-buf.json`）
- 飞书去重存储
- session pin 存储
- 会话移交阻断（`session-transfers.json`）

gateway 会通过原子替换写入 WeChat token / sync 状态、飞书去重状态、session pin 和移交阻断，避免在崩溃或异常重启时留下截断的状态文件。

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
- `performance queue=...` – 每 5 分钟记录的队列指标，覆盖 outbound 通知分片与飞书入站分发，包含队列 `depth`、`max_depth`、`full`、`closed`、`blocked`，以及阻塞入队的等待时长（`blocked_wait_total`、`blocked_wait_total_delta`、`blocked_wait_max`、`blocked_slow`、`blocked_slow_delta`）。其中 `*_total` 和 `blocked_slow` 是进程生命周期累计值；判断近期 provider 是否停滞时应看对应的 `_delta`。`max_depth` 是采样时最深的分片深度，不是历史最大值。

等待时长只在队列容量等待结束时记录，包括成功入队和 dispatcher 关闭两种情况；不包含等待分片生产者锁的时间，也不代表端到端投递延迟。因此，仍在持续的阻塞可能对应为零的等待时长 delta，应同时检查 `depth`、`max_depth`、`blocked` 和 `processed_delta`。`blocked_wait_max` 是生命周期内已结束等待的最大时长；`blocked_slow` 统计已结束且达到一秒的等待。飞书入站队列满时不会等待，因此其等待指标保持为零。背压也可能来自流量突增，而不一定是 provider 停滞。

常用字段包括：

- 事件类型
- workspace ID
- IM 类型
- chat ID
- session ID
- busy/idle 状态
- phase
- last outcome

## 飞书异步分发和去重

飞书事件处理采用长连接模式加异步分发：

- gateway 会维护一个长期存活的飞书 websocket 连接。
- 实际路由到 `chord headless` 的操作通过进程内有界队列完成。
- 重复投递会由轻量去重存储过滤。

聊天消息的去重 key 基于：

```text
app_id + chat_id + message_id
```

去重数据持久化到：

```text
<state_dir>/dedupe.json
```

卡片回调改用 `app_id + card + event_id` 去重。同一回调的重复投递会被忽略，用户再次点击相同答案仍可重试结果未知的提交。操作重试行为见[问题交互](./usage_CN.md#问答交互)。

当前去重 TTL 为 24 小时。

## 工作区路由

微信和飞书都通过显式 `chat_bindings` 选择工作区；配对后的 owner 在每个聊天使用 `/bind "~/workspace"`。

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
- `session_transfer_preparing`
- `session_transfer_status`
- `session_transfer_saved`
- `session_transfer_released`
- `background_result`
- `image_result`

可选事件由 `event_visibility` 控制；`background_result` 默认订阅，可用 `event_visibility.background_result: false` 关闭。各事件的行为以及哪些只更新状态、不发消息，详见 [event-visibility_CN.md](./event-visibility_CN.md)。

飞书卡片发送结果未确认时，Gateway 发送提示，不再把原提示重复转为文本。请先检查聊天记录；未收到问题入口时可发送 `/status`，角色菜单可发送 `/role` 重新查看。消息请求不会跟随重定向，非预期 HTTP 状态按结果未确认处理。平台明确拒绝卡片时，仍会发送文本入口。`/status` 展示 Chord 状态并可重新打开问题入口，不会查询平台投递回执。

图片转发需要 Chord 已启用内置生图，或对话模型已配置获准的服务端生图工具。Gateway 无需额外启用生图配置。飞书机器人需要图片上传（`im:resource`）与机器人消息发送（`im:message:send_as_bot`）权限。平台上传失败或发送结果未确认时会分别提示，不自动重发或重新生图。结果未确认时请先检查聊天记录，原图仍可在 Chord 中查看。
