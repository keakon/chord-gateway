# IM 接入总览

本文档是 `chord-gateway` 的“任务型入口页”，用于指导你把 IM 平台接入 gateway。

如果你需要字段级配置参考，请直接看：

- [配置参考](./configuration_CN.md)

## 当前支持哪些 IM 平台？

`chord-gateway` 当前支持：

- **微信 iLink**（`ims.wechat`）— 通过 iLink Bot API 做个人微信扫码登录。
- **飞书**（`ims.feishu`）— 通过飞书应用凭据 + 长连接接收事件。

## 该选微信还是飞书？

### 适合选微信 iLink 的场景

- 你明确需要 **个人微信** 作为聊天入口。
- 你接受 **单工作区路由**（所有微信消息必须进入同一个 workspace）。

从这里开始：[微信 iLink 指南](./wechat_CN.md)

### 适合选飞书的场景

- 你需要 **多工作区路由**（不同飞书群聊进入不同 workspace）。
- 你希望确认/提问交互尽可能使用更友好的交互卡片体验。

从这里开始：[飞书接入指南](./feishu_CN.md)

## 两条快速路径

### A) 只用微信（最小闭环）

1. 配置 `ims.wechat` 和一个 workspace。
2. 启动 gateway。
3. 扫描 gateway 打印的二维码登录 URL。
4. 在微信聊天中发送 `/status`。

最小配置示例：

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
workspaces:
  default:
    path: /path/to/project
```

第一阶段成功标志：

- 二维码登录完成
- 在同一个微信聊天里 `/status` 有回复

详细步骤见：[微信 iLink 指南](./wechat_CN.md)

### B) 只用飞书（最小闭环）

1. 在飞书开放平台创建应用并获取 `app_id` / `app_secret`。
2. 设置为 **长连接接收事件**，并订阅 `im.message.receive_v1`。
3. 启动 gateway，然后给机器人发送一条纯文本消息。

最小配置示例：

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
workspaces:
  default:
    path: /path/to/project
```

第一阶段成功标志：

- gateway 日志出现 `feishu: received message`
- 你能从这条日志里拿到 `chat_id` 和 `open_id`

详细步骤见：[飞书接入指南](./feishu_CN.md)

## 路由模型（重要）

gateway 的路由由 IM 平台决定：

- **微信** 通过 `ims.wechat.workspace_id` 选择唯一 workspace。
- **飞书** 通过 `ims.feishu.chat_bindings` 做 chat → workspace 映射。

如果只有一个 workspace，则微信 `workspace_id` 与飞书 `chat_bindings` 都可以省略。

## 用一句话解释 `/bind`

在某个飞书聊天中执行 `/bind <workspace_id> <path>` 会**只**更新：

- `ims.feishu.chat_bindings`
- `workspaces`

并把这两段写回 YAML 配置文件；它**不会**重载 allowlist 等其他字段。手工修改配置文件仍需要重启 gateway。

## 最低限度安全提示

- 把 IM 发送者视为对 workspace 的控制面访问者。
- 飞书公网部署请配置 `owner_open_id` 和/或 `allowed_open_ids`。
- 不要把 gateway 状态目录或密钥提交到版本控制。详见：[权限与安全边界](./permissions-and-safety_CN.md)

## 故障排查入口

- 微信问题：[故障排查](./troubleshooting_CN.md#微信问题)
- 飞书问题：[故障排查](./troubleshooting_CN.md#飞书问题)
