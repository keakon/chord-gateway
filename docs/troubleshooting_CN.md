# 故障排查

所有 IM 平台统一使用单 owner 配对与显式聊天绑定，单工作区也需绑定。首次运行 `chord-gateway --pair`，微信/飞书/企业微信/钉钉在机器人私聊发送 `/bind "~/workspace" <pairing_code>`；详见[配对与本机重置](./pairing_CN.md)。旧部署升级步骤见[更新日志](../CHANGELOG_CN.md)。 Slack 在 App 私聊使用 `/chord bind "~/workspace" <pairing_code>`；参见 [Slack 接入指南](./slack_CN.md)。

本文档覆盖 `chord-gateway` 启动、IM 集成、路由和运行时常见问题。

## 启动失败

### `load config` 错误

先确认 gateway 使用的是预期配置文件。解析顺序：

1. `--config` / `-f`
2. `$CHORD_GATEWAY_CONFIG`
3. `$XDG_CONFIG_HOME/chord-gateway/config.yaml`
4. `~/.config/chord-gateway/config.yaml`

调试时建议显式指定配置：

```bash
chord-gateway -f ./config.yaml
```

### 找不到 `chord`

gateway 会直接执行 `chord_path`。shell alias 不会展开。

可使用以下方式之一：

- 把 Chord 二进制所在目录加入 `PATH`。
- 在 `chord_path` 中配置绝对路径。
- 使用 gateway 支持的 `~` 路径。

示例：

```yaml
chord_path: ~/go/bin/chord
```

### Workspace 校验失败

常见原因：

- `workspaces.<id>.path` 不存在。
- 所有已支持平台都通过显式 `chat_bindings` 选择工作区；配对后的 owner 在每个聊天使用 `/bind "~/workspace"`。
- 飞书聊天没有对应的 `ims.feishu.chat_bindings` 条目，单 workspace 时也须先绑定。
- 飞书 `chat_bindings` 中的某个映射指向了错误的 workspace ID。

## 微信问题

如果你还没有跑通微信接入的完整流程，建议先看：[微信 iLink 接入指南](./wechat_CN.md)

### 没有出现二维码登录

检查：

- `ims` 中存在 WeChat 条目。
- `base_url` 是否正确。
- 已保存的 token 文件是否过期或损坏。默认路径是 `<state_dir>/wechat/token.json`；如果配置了 `ims.wechat.token_path`，请检查该自定义路径。
- 状态目录下的 gateway 日志。

如果需要强制重新登录，停止 gateway 后删除微信 token 文件（默认 `<state_dir>/wechat/token.json`；如配置了 `ims.wechat.token_path`，则删除该自定义路径）。

### Token 过期

微信 token 过期时，在多 IM 模式下可以从另一个活跃渠道发送：

```text
/login wechat
```

然后打开返回的登录链接并扫码。当 gateway 自身检测到微信 token 过期时，会自动清理已保存 token 并启动二维码登录。

飞书不需要在会话中登录或手动续期；gateway 会使用配置中的应用凭证自动获取和刷新飞书 access token。如果飞书连接失效，请检查部署配置和飞书开放平台中的应用凭证、权限、事件订阅与长连接设置，不要在 IM 会话中发送或修改 `app_id` / `app_secret`。

### 微信任务等待或轮询暂停

`reply_context_missing` 表示当前进程的任务尚未交付；owner 的新 `/status` 可建立上下文。重启会清空队列。容量不足时保留当前批次，接纳后才推进内存游标。

`batch_too_large` 表示响应超过 4 MiB 或 1,024 条的客户端防护预算。微信接收循环停止且不推进游标，其他平台可继续运行；日志报告 `IM adapter exited with error type=wechat`。重启不保证解决服务端同一超大批次，`/login wechat` 也不会重启已退出循环。核查平台积压与 Chord 原会话后处理，不跳过不透明游标或盲目重发。

未知结果可能已执行，先检查 `/status` 和 Chord 原会话的队列、执行记录与结果。确认结果后再决定重发或重启；重启不会取消已经执行的工作。

## Discord 问题

检查 DM 隐私设置、配对 owner 显式 @Bot、每个 thread 独立绑定，以及频道/thread 权限。`403` 应检查频道覆盖或私有 thread 访问权限，`4014` 应检查请求的 intents。未锁定归档 thread 可以因发送恢复活跃；锁定或删除时明确失败，不改投其他聊天。详见 [Discord 故障排查](./discord_CN.md#故障排查与部署验证)。

## 飞书问题

如果你还没有跑通飞书后台配置与长连接接入，建议先看：[飞书接入指南](./feishu_CN.md)

### 飞书长连接未建立或收不到事件

检查：

- gateway 是否已启动并保持运行。
- 飞书应用是否在“事件与回调”中选择了“使用长连接接收事件”。
- 订阅的事件类型是否包含 `im.message.receive_v1`。
- 回调配置中是否添加了 `card.action.trigger` 用于卡片交互。
- 飞书应用是否已发布最新版本。
- 机器是否可以主动访问飞书公网服务。

### 长连接配置失败

检查飞书开放平台中事件订阅方式是否已切换为“使用长连接接收事件”，并确认 gateway 已启动、`app_id`/`app_secret` 正确。

### 消息被忽略或聊天未绑定

确认消息来自当前机器人账号的配对 owner，目标聊天已显式绑定，消息为文本（`text` 或 `post`）。首次配对必须运行前台 `--pair` 并在私聊使用终端配对码；普通启动不会开放配对。

绑定群聊：先完成私聊配对，再把机器人加入可信群，由 owner 在群中发送 `/bind "~/work/project-a"`。目录必须已存在。成功后可在 YAML 的 `chat_bindings` 中查看聊天 ID 和工作区映射；绑定前的消息不会重放。无法切换时检查 `/status` 中的待处理输入、后台任务、交互和会话转移；刷新失败时保留原绑定，恢复连接后重试。

账号变化或授权恢复见[配对与本机重置](./pairing_CN.md)。不要从不可信消息日志复制身份来绕过配对。

### 重复消息

飞书可能重试事件。Gateway 按 App ID、聊天与业务消息/卡片 ID 在当前进程内去重，重启后不保留去重历史。

## 钉钉问题

首次连接需同企业员工私聊 `hello`，群消息不能解锁配对。`Gateway binding check.` 失败时不要手工保存目标：按 `proactive_send_denied` 修复主动 API 权限和发布状态，按 `target_unreachable` 修复员工范围或群成员关系，再发送新的 `/bind`。Stream 注册认证或企业身份不符会停止重连；主动 API 认证失败仅终止当前发送并清理 token。发送结果未知、回调 500 都不能作为自动重放依据，详见[钉钉错误分类](./dingtalk_CN.md#范围与排障)。

## 路由问题

### 消息进入了错误 workspace

飞书多 workspace 模式下，请确认 `ims.feishu.chat_bindings` 把每个飞书 chat ID 映射到预期 workspace ID。

飞书不会为未绑定聊天选择默认 workspace；即使只有一个工作区，也须由 owner 先绑定聊天。

### Session 上下文不是预期内容

使用：

```text
/status
/sessions
```

然后用 `/new` 清除当前绑定，或用 `/resume <id>` pin 到已知 session。

微信或飞书原 pin 会话不存在、被占用或无法恢复时，已接纳输入会隔离，不自动新建会话重发。先查看 `/status` 和原 Chord 会话，必要时在本机核查收件记录。显式 `/new` 或 `/resume` 为后续任务选择目标，不重放已隔离请求。确认原请求是否执行后，再明确发送新任务。

### 会话切换被拒绝

`/new` 与 `/resume` 会在替换路由前拒绝正忙的 Chord 进程：先发送 `/cancel`，再重试。不同原因分别命名：

- `session_change_busy` —— 仍有任务在运行。
- `session_transferred` —— 转移保护生效中。
- `undelivered_inputs` —— 已恢复或排队输入尚未解决。

刚启动或升级后出现的 `control_target_expired` 拒绝属于过期控制快照：该保护已收窄，仍在启动中（尚无 runtime 身份）的进程不再触发。若持续出现，请查看 `/status`、重启并在 gateway 日志中检查被丢弃的 readiness 事件。

## 运行时问题

### Chord 没有回复

检查：

- gateway 日志中是否有进程启动或 stdout 解析错误。
- Chord 日志和 session 状态。
- 是否有待确认或待回答问题（`/status`）。
- 进程是否被取消或超时。

Headless JSONL 帧的 JSON 正文上限为 16 MiB，不含 LF 或 CRLF 换行符；Shell 输出经过 JSON 转义后增大的体积也计入此上限。遇到无法解析的 JSON 行时，gateway 会向聊天发送警告并继续读取。帧超限或读取流失败时，gateway 会先停止 Chord 进程（等待正常退出最多两秒，随后强制结束），再向聊天发送错误和未收到消费回执的输入数量。这种停止不会触发崩溃自动重启。重试前请核对会话；输入不会自动重放。

### 迟到回答与确认

Chord 已关闭的问题（无论终态，包括 `no_response`）仍可在所属会话中按 ID 补答：`/answer @<id> <answer>` 会追加一条历史修正，原终态和任务归属保持不变。普通文字始终是新指令，不会当作旧问题的回答。idle 和空闲超时回收不关闭问题；超时只通知一次，不会在后续每次 idle 时重复通知。

如果进程在结果到达前断开，gateway 会保留已知问题但结果视为未知，不会臆造终态；重试前请先核对 `/status` 和会话，再主动提交相同答案。重试缓存仍可用时，gateway 会复用结果未知的原操作；缓存期限与重启行为见[问题交互](./usage_CN.md#问答交互)。答题入口未能打开时，`/status` 会重试展示确认，完成后才显示入口。

飞书确认过期后，`/allow` 或 `/deny` 会被拒绝，不再变成模型跟进上下文。先查看 `/status`；仍需确认原操作时，请让 Chord 重新发起确认；需要新任务时明确发送任务内容。

### 任务看起来卡住

先发送 `/status`。如果应该停止任务，发送：

```text
/cancel
```

如果进程仍卡住，重启 gateway 并检查是否有孤儿 Chord 进程。

### 生成图片没有收到

只有 Chord 确实产出了图片才会转发：模型必须真的生成图片，且 Chord 构建须支持 `image_result` 摘要与 `image_artifact` 分块（见[兼容策略](./compatibility_CN.md)）。Gateway 自行订阅 `image_result`，没有开关可以启用或关闭转发。

请检查：

- Chord 配置已启用生图，或对话模型已配置获准的服务端生图工具。
- 飞书应用需要 `im:resource` 权限才能上传图片；新增权限后要发布新的应用版本。
- 飞书图片达到或超过平台限制时，会发送缩小的 PNG 预览并附一条提示；原图仍保存在 Chord 会话中。
- 聊天里是否提示 `image delivery failed` 或结果未确认，gateway 日志是否记录了失败的 artifact 引用。
- 提示失败后不会重新生图，也不会自动重发。请在 Chord 会话中查看已保存的原图。

微信图片转发已有协议测试，但尚未完成真机解密与显示验证。验证部署时，请生成一张小图并确认能在微信客户端打开；API 成功回执不能单独证明图片可正常显示。

### 重启后收件队列为空

这是预期行为：Gateway 的收件、去重和轮询游标只存内存。旧收件文件不再读取或升级。Chord 会话、配置、凭据与移交保护仍按各自规则保存；详见[投递与重启](./operations_CN.md#消息投递与重启)。

### 公共收件中的输入一直是 received

通过 `/status` 查看等待原因和原输入年龄。适配器可能需要有效回复上下文、可用账号等本地交付条件；公共队列等待时保留顺序，达到原来的 24 小时窗口后隔离，发送新消息不会刷新旧输入的年龄。微信已接入此链路：由已授权 owner 在原聊天发送新鲜的 `/status` 可建立上下文并唤醒原队列；普通新任务排在旧任务之后。飞书进程重启后收件状态为空，不导入旧文件。

### 日志和状态文件在哪里？

状态目录优先级：

1. `$CHORD_GATEWAY_STATE_DIR`
2. `$XDG_STATE_HOME/chord-gateway`
3. `~/.local/state/chord-gateway`

日志、token、去重数据和 session pin 都存储在这里。微信 token 默认位于 `<state_dir>/wechat/token.json`。

## 仍然无法解决

提交问题时请提供：

- gateway 版本或 commit
- OS 和架构
- Go 版本
- Chord 版本或 commit
- 脱敏后的配置
- 相关 gateway 日志
- 使用的 IM 平台和模式

所有平台共用有界内存收件、授权和投递回执。Gateway 重启后不恢复收件队列、去重记录或轮询游标；结果不确定时，先核查 Chord 原会话再决定是否重发。详见[投递与重启语义](./operations_CN.md#消息投递与重启)。

## 企业微信问题

开通阶段先排除四类前置问题。管理后台入口是 **智能专区 → 智能机器人**，「安全与管理 > 管理工具」不是当前入口；个人团队没有工作台也无法登录 Web 管理后台，两个入口都进不去，需改为企业团队或用「变更为企业账户」升级。创建页看不到 Bot ID 和 Secret，说明「API 模式管理」里的长连接接入成员范围未包含当前账号，通讯录超过 1000 人的企业默认为空，需管理员开通。客户端搜不到机器人或收不到 `Gateway binding check.`，先核对可见范围是否包含自己。企业微信认证不是前置条件：长连接不需要域名、备案或公网 IP，未认证企业同样可以接入。创建页的「可使用权限」是 MCP/CLI 能力通道，Gateway 不使用该通道。

认证失败先核对机器人是否为 API 长连接模式以及 bot ID/secret。绑定探测失败检查可用范围、目标、发送权限及配额，然后重新 `/bind`。`duplicate_connection` 表示另一部署取代当前连接，当前实例停止争抢；先停止其他部署再重启。`unknown_send` 不能证明平台没收到正文，先检查聊天和原会话。更换 bot ID 后需先停服，在本机控制终端执行 `chord-gateway -f config.yaml pairing reset --platform wecom`，输入 `wecom` 确认后重新配对。`endpoint` 字段会被忽略并提示警告，不能修改连接地址。详细诊断及待完成的真实账号验收见[企业微信指南](./wecom_CN.md)。

## Slack 问题

安装、`/chord` 冲突、scope 重装、身份错误、频道权限和未知发送结果见 [Slack 指南排障](./slack_CN.md#安装检查与排障)。thread 的 slash 始终作用于根目标，请改用 thread 内 @Bot 文本命令，并单独绑定该 thread。

## Telegram 问题

凭据、webhook 冲突、重复 poller、privacy、群迁移与未知发送请见[Telegram 排障](./telegram_CN.md#排障)。群绑定需 `/bind@BotUsername`；Topic 与媒体不受支持。
