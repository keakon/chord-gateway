# 权限与安全边界

所有 IM 平台统一使用单 owner 配对与显式聊天绑定，单工作区也需绑定。首次运行 `chord-gateway --pair`，微信/飞书/企业微信/钉钉在机器人私聊发送 `/bind "~/workspace" <pairing_code>`；详见[配对与本机重置](./pairing_CN.md)。旧部署升级步骤见[更新日志](../CHANGELOG_CN.md)。 Slack 在 App 私聊使用 `/chord bind "~/workspace" <pairing_code>`；参见 [Slack 接入指南](./slack_CN.md)。

`chord-gateway` 是本地 `chord headless` 进程的远程控制面。任何能向 gateway 发送并被接受的 IM 消息的用户，都可能影响 Chord 在配置工作区中的行为。

## 安全边界

网关不是多租户安全沙箱。

请把被允许的 IM 发送者视为可以在配置工作区中与 Chord 交互的人。根据 Chord 配置和权限，这可能包括读取文件、编辑文件、运行工具或请求模型执行操作。

工作区文件和 Chord 进程会留在网关所在主机，但聊天消息与回复仍会经过所选 IM 平台；发送给模型提供商的数据范围由 Chord 配置决定。

会话接管沿用现有 IM 发送者授权和工作区路由规则，没有独立的接管权限开关，也不要求源端用户再次审批。它只支持 macOS/Linux 下同一 OS 用户、同一规范化工作区的 TUI 与 gateway 管理的 headless 双向移交。本机端点身份检查用于防止路由到错误运行时，不能隔离以同一 OS 用户运行的恶意进程。

## 推荐部署方式

- 尽量使用专用机器、用户账号、容器或 VM 运行 gateway。
- 只把 `workspaces.<id>.path` 指向允许从 IM 控制的项目。
- 不要把整个 home 目录配置为 workspace。
- 保持 Chord 权限和工具审批策略保守。
- 监控日志中是否出现异常发送者、聊天、命令或工作区路由。

## IM 访问控制

微信、飞书、企业微信、钉钉、Discord 和 Slack 都只接受已配对 owner 的输入，且聊天必须显式绑定。首次通过前台 `--pair` 和机器人私聊登记 owner；普通启动不会开放配对。运行在本机并不意味着外部用户无法向机器人发消息。

owner 拥有与 gateway 进程相当的本机控制能力，包括已有 shell 命令；配对并不是目录沙箱。仅在可信群聊中绑定机器人，因为回复对群成员可见。账号变化会阻断旧身份，重新授权需停服务后在本机 reset。详见[配对与重置](./pairing_CN.md)。

飞书卡片回调须校验应用、用户、聊天、请求与绑定目标，普通文本不能冒充可信卡片操作。已接纳卡片的参数、表单值和 card token 只保留在内存中，不写入收件记录。网关重启后，待处理卡片控制不可直接恢复执行；先查看 `/status`，使用重新展示的交互。留存输入正文和收件备份仍属于敏感本机状态。

## 凭据处理

以下内容都应视为敏感信息：

- Discord `bot_token`
- 飞书 `app_secret`
- 企业微信 `secret`
- 钉钉 `client_secret`
- 微信 token 文件（默认 `<state_dir>/wechat/token.json`，或 `ims.wechat.token_path`）
- 微信旧同步游标和去重文件（不再导入;文件已失效,可以删除）
- gateway 状态目录
- Chord provider 凭据和 auth 文件

不要把密钥或状态目录提交到版本控制。条件允许时，应把 gateway 状态目录的读取权限限制在 gateway 运行用户内，因为 session pin 和同步状态即使不属于直接凭据，也可能暴露聊天 / 会话标识符。

微信短期 `context_token` 仅存内存，按账号和聊天隔离，不进入收件数据库、普通日志或诊断。只有当前 owner 或通过核验的首次配对流程可以准备上下文；首次配对上下文仅用于本次限时反馈，不开放普通后台发送。账号更替立即清空旧上下文，旧输出不能使用新账号发送。已接纳正文只存内存，终态清空；进程重启丢弃队列。

同账号轮询在同一 OS 用户、同一主机上互斥，即使配置和状态目录不同；跨用户或跨主机时由部署方确保单实例。登录续期不撤销已接纳输入，换账号或本机 reset 才按授权规则隔离旧输入。

## 多工作区安全

飞书多工作区模式下，请配置 `ims.feishu.chat_bindings`，把每个飞书 chat ID 映射到预期 workspace。

微信仅支持私聊，每个聊天同时绑定一个目录，可通过 `/bind` 切换。需要在不同私聊或群聊中分别控制不同目录时，可使用飞书、企业微信、钉钉或 Discord。Discord thread 需独立绑定并具备访问权限；只授予 View Channel、Send Messages、Send Messages in Threads，输出 mention 不触发通知。频道与 thread 的回复对成员可见。

## 钉钉身份边界

同企业私聊回调只验证配置中的企业与机器人身份，不授予 owner 权限；owner 仍需终端配对码。员工 ID 来自已认证企业回调，群内 @机器人不扩大权限。未绑定聊天不能任意请求主动发送，只有已授权的绑定探测可发送候选目标。Reset 撤销身份验证证据。保护应用密钥与收件数据库，详见[钉钉指南](./dingtalk_CN.md)。

## 事件响应

如果怀疑出现未授权访问：

1. 停止 gateway。
2. 吊销或轮换受影响的飞书应用密钥、企业微信机器人 secret、钉钉 client_secret 和 Discord bot token。
3. 删除或轮换微信 token 文件（默认 `<state_dir>/wechat/token.json`，或 `ims.wechat.token_path`）。
4. 检查 gateway 日志和 Chord session 历史。
5. 检查配置工作区中的变更。
6. 在本机 reset 并重新配对可信 owner，核对聊天绑定后再启动服务。

项目漏洞反馈方式见 [SECURITY_CN.md](../SECURITY_CN.md)。

所有平台共用有界内存收件、授权和投递回执。Gateway 重启后不恢复收件队列、去重记录或轮询游标；结果不确定时，先核查 Chord 原会话再决定是否重发。详见[投递与重启语义](./operations_CN.md#消息投递与重启)。

## 企业微信安全边界

企业微信 `secret` 属于敏感凭据。绑定前的临时发送许可仅允许向已校验的候选目标发送固定 `Gateway binding check.`，不会执行任务或开放普通未绑定输出。群内仅 owner 的输入可执行，回复对群成员可见。模型正文按安全文本发送，不生成提醒、链接、图片或卡片。轮换同 bot 的 secret 保留 owner，换 bot ID 需先停服，在本机控制终端执行 `chord-gateway -f config.yaml pairing reset --platform wecom` 并输入 `wecom` 确认。

## Slack

Slack app token 和 Bot token 均为敏感凭据。只授予 [Slack 指南](./slack_CN.md)列出的最小权限，不授予文件或频道全文读取权限。首版只接受 owner 输入，频道/thread 必须显式 @Bot；频道回复对成员可见。共享频道、Slack Connect 和多人私聊不支持。不要把 ACK 当作执行成功证据。
