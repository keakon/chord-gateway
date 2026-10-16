# Owner 配对与聊天绑定

每个平台登记一个可信 owner，每个聊天显式绑定到已有本地目录。已有部署升级前请阅读[不兼容变更](../CHANGELOG_CN.md)。

Slack 首次配对在已核验的 App 私聊使用 `/chord bind "~/workspace" <pairing_code>`；后续根目标使用 `/chord bind ...`，thread 内使用 `@Bot /bind ...`，不继承根绑定。详见 [Slack 指南](./slack_CN.md)。

## 首次连接

配置需要的平台，不必预先填写 owner、聊天 ID 或工作区：

```yaml
ims:
  wechat: {}
  # feishu:
  #   app_id: cli_example
  #   app_secret: your-app-secret
  # wecom:
  #   bot_id: your-bot-id
  #   secret: your-bot-secret
  # discord:
  #   bot_token: your-bot-token
  #   application_id: "123456789012345678"
```

在有控制终端的前台运行：

```sh
chord-gateway --config /path/to/config.yaml --pair
```

微信按提示扫码登录。使用 `--pair` 时，终端会立即显示一个 10 分钟有效的通用配对码，对所有已连接平台都有效，且在整个窗口内 `/bind` 始终可用，因此可以在过期前绑定多个用户和聊天。钉钉显示同样的启动码，但其绑定仍需通过同企业身份探测后才会放行。在与机器人的**私聊**中发送终端给出的命令，将目录替换为 gateway 主机上已有的目录：

```text
/bind "~/workspace" <pairing_code>
```

企业微信需先通过固定主动绑定探测，收到平台成功 ACK 后才能保存。Gateway 将 owner、工作区和当前聊天绑定保存到 YAML。下一条任务才会在该目录启动 Chord。配对码不会写入配置或日志，但终端录制和 IM 历史可能保留它，请妥善保护。

配置写回会保留 YAML 别名和合并键继承的有效值，包括顶层合并。`/bind` 和本机重置与配置加载使用相同的平台名规则：忽略大小写和首尾空白。写回时可能将别名和合并展开为显式映射。

之后可以普通启动或作为后台服务运行。普通启动不开启配对；`--pair` 不覆盖已有 owner。

连接暂时断开不会撤销同一账号、同一目录绑定下已接纳的 owner 输入或回复。发送仍依赖平台传输接口；结果未知时不会自动重试。替换 Chord 进程会丢弃其排队输出，并取消尚在进行的发送；平台已经接收的消息无法撤回。账号变化或目录切换仍会使旧授权失效。

钉钉首次连接先等待同企业员工私聊 `hello`，身份验证通过后才会放行绑定；验证消息不执行任务、不登记 owner。已授权的 `/bind` 保存配置前会主动发送固定探测，失败或未知时不保存绑定。详见[钉钉指南](./dingtalk_CN.md)。

## 后续绑定与切换

Owner 可在新聊天中绑定其他目录，无需配对码：

```text
/bind "/path/to/another workspace"
```

配对后的 owner 也可以绑定飞书、企业微信或钉钉群（钉钉群需 @机器人）或 Discord 频道/thread；其他发送者不能执行命令，但成员可以看到回复。微信仍使用现有个人聊天标识，不新增群聊能力。

未绑定聊天只收到绑定引导，输入不会执行，也不会在绑定后重放。目录按实际文件系统身份复用；新目录只登记，不创建、不克隆。多个工作区 ID 指向同一目录时，需要先在本机消除歧义。无关旧目录暂时不可访问，不阻止绑定其他目录。

路径支持绝对路径和 `~/`。包含空格时使用双引号；双引号内只有 `\"` 和 `\\` 分别转义引号和反斜杠。不执行环境变量或命令替换，不接受相对路径、裸 `~` 或 `~other-user`。

重复绑定同一目录没有副作用，任务运行期间也可以重复。在微信中，重复绑定或切换被拒绝都不会中断当前任务的后续回复。切换目录前应完成任务和待处理交互；`/cancel` 只是请求取消，需要等待实际结束。后台任务、尚未确认的输入和会话转移仍可能阻止切换。恢复后的空闲会话会先刷新待处理输入摘要；刷新失败时保留原绑定。无法确认旧进程停止时，绑定会暂停，需本机检查并重启。配置保存失败不会登记新 owner；替换结果不确定时，gateway 会关停，需检查磁盘配置。

如果 `/takeover` 尚在检查源会话时目录已经切换，该接管请求会被丢弃；转移一旦登记，则必须先处理完毕才能切换目录。目录切换开始后，即使仍在等待旧回复发送结束，也会拒绝从旧进程转出会话的新请求。

Discord 服务器频道和 thread 使用 `@Bot /bind "/path/to/project"`，每个 thread 都需独立绑定；首次配对仍必须在 Bot DM 完成。

## 配置与路由

`owner` 包含 `account_id` 和 `user_id`；所有已实现平台都通过 `chat_bindings` 将聊天 ID 映射到工作区 ID。首次配对前可省略 owner、工作区和绑定。空的工作区与绑定可以省略，也可写成 `{}` 或 `null`。未配对平台只能在前台 `--pair` 开启的窗口中接收有效配对命令。私聊发送者若未携带有效配对命令，会收到一条限量的提示而不是被静默丢弃——如何配对、`/bind` 格式缺了什么，或配对码不正确；配对未激活时提示会要求以 `--pair` 重启。群聊、内部动作与重复重投保持静默，提示使用独立于配对尝试的限流预算。未绑定聊天不能执行任务，也不自动选择唯一工作区。

旧授权字段与模式开关已移除，不做自动迁移，见[升级说明](../CHANGELOG_CN.md)。本地标准输入/输出使用显式 `--console`，要求恰好一个工作区；该模式不连接 IM，不能与 `--pair` 同时使用。

## 本机重置与恢复

先停止 gateway，再对目标平台执行：

```sh
chord-gateway pairing reset --platform feishu --config /path/to/config.yaml
chord-gateway pairing reset --platform wechat --config /path/to/config.yaml
chord-gateway pairing reset --platform wecom --config /path/to/config.yaml
chord-gateway pairing reset --platform dingtalk --config /path/to/config.yaml
chord-gateway pairing reset --platform discord --config /path/to/config.yaml
chord-gateway pairing reset --platform slack --config /path/to/config.yaml
chord-gateway pairing reset --platform telegram --config /path/to/config.yaml
```

只运行需要重置的平台对应命令，在控制终端输入平台名确认。没有 IM 远程重置，也没有非交互 `--yes`。

Reset 清除该平台 owner、聊天绑定和自动恢复 pin，保留工作区、Chord 会话文件及其他平台配置。存在会话转移 guard，或无法读取 pin/transfer 状态时，重置会被拒绝。若 pin 清理成功而配置替换失败，原身份仍有效，但自动恢复 pin 可能已经清除；处理问题后可重试。替换结果不确定时不会宣告成功。

钉钉 reset 还会撤销本机企业身份验证证据；下一次连接需先私聊 `hello`。相同身份正常重启会在新握手通过后复用证据。

随后通过 `--pair` 重新登记。即使是同一 owner，也不会自动继承已清除的 pin。这不隔离本机历史文件：owner 是可信本机控制面用户，具有 gateway 进程可访问目录的操作能力，现有 Shell 命令能力仍保留。

机器人账号变化不会自动授权新账号；旧 paired 活动会被取消并阻断。应在本机核对部署后显式重置。手工修改 owner 不能替代 reset。

编辑身份或路由前先停服务。运行期事务检测到文件字节改变会拒绝写入，不在线合并外部编辑。资源锁覆盖配置、状态目录、pin 和自定义微信 token 路径，锁目录位于当前 OS 用户缓存目录，不随 `CHORD_GATEWAY_STATE_DIR` 改变。不要与不支持该锁协议的旧 gateway 二进制同时运行。配置变更需要文件系统支持锁和持久化替换。

旧绑定输出不能重新获得新绑定的发送许可，但平台已经接收的消息无法撤回，可能延迟显示。跨平台登录通知只包含连接状态，paired 接收范围仅来自当前显式聊天绑定。

如果目录切换因待处理输入、存储故障或结果未知而被阻止，请通过 `/status` 查看原因，并在本机核查原会话。仅取消正在执行的任务不能解决这些状态。闲置回收和进程替换也会保留相同的输入保护。

所有平台共用有界内存收件、授权和投递回执。Gateway 重启后不恢复收件队列、去重记录或轮询游标；结果不确定时，先核查 Chord 原会话再决定是否重发。详见[投递与重启语义](./operations_CN.md#消息投递与重启)。
