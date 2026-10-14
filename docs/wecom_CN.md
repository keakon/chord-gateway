# 企业微信接入指南

企业微信智能机器人通过 API 长连接接入 Gateway，支持私聊、群文本、现有控制命令及后台文本结果。无需公网回调地址；任务完成后主动发送结果，不依赖触发任务的 callback 有效期。首版不支持卡片、流式回复或媒体。

## 开通机器人

在企业微信创建智能机器人，选择 **API 模式 → 长连接**，取得 bot ID 和 secret，并设置可用范围，让 owner 能私聊机器人并将其加入目标群。若控制台没有该入口，请由管理员开通对应能力，按 `product_mode_unavailable` 处理部署失败。[企业微信官方入口](https://open.work.weixin.qq.com/help?doc_id=21657)

本接入只适用于智能机器人 API 长连接。企业自建应用、普通群机器人 webhook 和 HTTP/XML 回调需使用各自的产品接口。

## 配置与首次配对

```yaml
ims:
  wecom:
    bot_id: your-bot-id
    secret: your-bot-secret
workspaces: {}
```

连接地址固定为 `wss://openws.work.weixin.qq.com`。YAML 中的 `endpoint` 为未知字段，会被忽略并提示警告，不会覆盖连接地址。请保护 YAML 中的 secret，并确保目标目录已存在。

```bash
chord-gateway -f config.yaml --pair
```

认证及收件准备完成后，前台终端显示配对码。在机器人私聊发送：

```text
/bind "/path/to/project" <pairing_code>
```

Gateway 会先向该聊天主动发送固定文本 `Gateway binding check.`。只有平台返回成功 ACK 才保存 owner 和绑定；探测失败时绑定不变，检查机器人可用范围、主动发送权限和当前配额后重新发送 `/bind`。

配对完成后发送 `/status`，再发送普通任务。owner 可以在机器人所在群发送 `/bind "/path/to/project"`；探测及后续回复都发送到该群。其他成员不能执行命令或批准请求，但群内所有成员都能看到回复。每个聊天显式绑定一个工作区，Gateway 自动写入编码后的 `chat_bindings`，无需手工构造聊天 ID。控制命令见[使用指南](./usage_CN.md)，配对与重置见[配对指南](./pairing_CN.md)。

bot ID 不变时，轮换 secret 保留原 owner。换 bot ID 会阻断旧配对，需停服并在本机执行 `chord-gateway -f config.yaml pairing reset --platform wecom`，在本机控制终端输入 `wecom` 确认后重新配对。reset 会清除 owner、绑定和自动 session pin，保留工作区和会话文件。一个 bot 同时只能由一个本机实例使用；账号锁在拨号前取得，跨状态目录仍互斥。

## 回复和故障处理

回复通过 `aibot_send_msg` 主动发送，并明确指定单聊或群聊，避免平台按默认群聊方式解析私聊目标。Gateway 将正文作为安全文本显示，转义 Markdown 控制字符并消除提醒语义；不生成链接、图片或卡片。分段按最终内容的 1024 UTF-8 字节预算计算，保持中文和 emoji 边界，每份输出最多 32 段，超长内容附截断提示。完整内容保留在原 Chord 会话中。

Gateway 对每个聊天至少间隔 4 秒发送，账号最多每秒 2 条，burst 为 1；下一段等待上一段的成功 ACK。每份输出总预算为 5 分钟。上述为项目保守预算和节流策略，平台实际权限及额度需在目标企业实测。

| 诊断 | 处理 |
| --- | --- |
| `auth_failed` | 核对 bot ID、secret 和机器人产品模式；认证拒绝后不自动重试。 |
| 绑定探测未接受 | 检查目标、可用范围、发送权限及配额，解决后重新发送 `/bind`。 |
| `platform_rejected` | 保留数字错误码核查；Gateway 不猜测限流错误码，也不盲重试。 |
| `unknown_send` | 网络中断或 ACK 超时，结果未确认；先查看聊天和原会话，不自动重发正文。部分发送失败会记录已接受段数。 |
| `identity_mismatch` | callback 的 bot ID 与配置账号不符，连接停止；先核对凭据和部署再重启。 |
| `ingress_prepare_failed` | 核查收件服务状态和配对账号；换 bot ID 后按上述步骤本机 reset，再重新配对。 |
| `duplicate_connection` | 服务端通知另一连接取代本实例，本实例停止自动重连；关闭其他部署后重启。 |
| `acceptance_unconfirmed` | 本次投递未能确认接纳；同一业务消息的早先投递可能已提交。先核查原 Chord 会话，不直接重发。 |
| `callback_dropped` | 连接准备缓存溢出或断线时有投递未提交给公共入口；不代表该业务消息从未接纳，平台也不保证重投。核查原会话后决定后续操作。 |
| `control_target_unavailable` | 接收时业务状态正被占用，无法冻结原控制目标；本条消息在当前进程内被拒绝，不会稍后对新状态执行。先查看 `/status`。 |

普通网络错误按 1/2/4/8/16/30 秒基准退避重连，并加入 ±20% 抖动；连接持续 ready 60 秒后才清零退避。订阅认证成功后即每 30 秒发送 JSON 心跳，收件准备期间也持续保活，10 秒内无成功 ACK 即关闭旧连接。连接准备缓存最多 64 条、原始帧合计 4 MiB，超限丢弃新到的投递并计数，保留已缓存消息及连接；不自动重放丢弃消息。断线期间不能发送，恢复后收到的旧 ACK 无效，旧 send 不会重发。

仅普通用户文本可作为命令或任务。带有 `compat_info` 的智能助理、频道或未知来源消息均不进入命令处理。普通消息无需 `create_time`；缺省时使用接收时间，同时兼容[官方 SDK 的可选时间字段](https://github.com/WecomTeam/aibot-node-sdk/blob/main/src/types/message.ts)。参见[官方长连接协议](https://developer.work.weixin.qq.com/document/path/101463)。

企业微信按业务 msgid 在当前进程内去重，重启后清空。未知结果不自动重发，先核查 `/status` 与 Chord 原会话。见[投递语义](./operations_CN.md#消息投递与重启)。

## 部署验收

自动测试覆盖模拟 WebSocket 认证、ACK 关联、关闭、命令授权、绑定探测、业务消息内存去重和文本边界。这些测试不等于真实企业账号验收，需在实际部署中验证。目标企业需依次验证：认证；私聊探测、配对和 `/status`；owner 群绑定和其他成员拒绝；1024 字节含中文/emoji 的样本和三段正文的显示、顺序及无提醒；超过 10 分钟的任务及后台文本结果回传；另一连接踢下线后停止争抢。任何权限或预算拒绝均保留错误码，不能记作验收通过。未开通长连接产品能力时，部署验收归类为 `product_mode_unavailable`；主动绑定探测被拒绝时，先检查目标类型和平台错误码；只有明确的权限拒绝才归类为 `proactive_send_denied`，不要将寻址错误、限流或网络超时归为权限问题。这些是部署验收分类，与上表中的运行期诊断分别记录。
