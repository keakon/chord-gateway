# 快速开始

本指南帮助你把一个项目连接到微信或飞书。首次接入只配置一个工作区；确认聊天可用后，再增加其他工作区。

## 1. 安装 Chord 和网关

先按 [Chord 官方安装说明](https://github.com/keakon/chord/blob/main/README_CN.md#三步完成设置)安装 Chord，完成首次配置向导并验证：

```bash
chord --version
```

然后使用 Go 1.26.3 或更高版本安装网关：

```bash
go install github.com/keakon/chord-gateway@latest
chord-gateway --version
```

预编译版本、源码构建和后台服务配置见 [Cookbook](./docs/cookbook_CN.md)。

## 2. 创建一个最小配置

从下面选择一个平台，把示例保存为 `config.yaml`。将 `/path/to/project` 替换为允许 Chord 操作的项目目录。路径可以是绝对路径、以 `~` 开头，也可以使用 Windows 盘符或 UNC 前缀。

### 方案 A：微信

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
workspaces:
  default:
    path: /path/to/project
chord_path: chord
idle_timeout: 30m
```

首次启动时，用微信扫描终端中显示的二维码链接。登录凭据默认保存在 `<state_dir>/wechat/token.json`。

### 方案 B：飞书

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner_open_id: ou_xxx
workspaces:
  default:
    path: /path/to/project
chord_path: chord
idle_timeout: 30m
```

启动网关前，请在飞书后台选择长连接接收事件、订阅 `im.message.receive_v1`、把机器人加入私聊或受控测试群，并发布应用版本。需要交互式确认和提问卡片时，再添加 `card.action.trigger`。具体权限与后台操作见 [飞书接入指南](./docs/feishu_CN.md)。

生成最终配置前，可以安全获取自己的 ID：先不配置 `owner_open_id` 和 `allowed_open_ids` 启动一次，发送一条文本消息，然后从 `message from non-allowed open_id` 日志中复制 `open_id=ou_xxx`。该日志不包含消息正文，消息也不会被处理。停止网关，把该值配置为 `owner_open_id` 后再启动。即使网关运行在本机，也不代表只有本机用户能通过飞书访问机器人。

## 3. 启动并验证

```bash
chord-gateway -f config.yaml
```

启动后：

1. 完成微信扫码登录，或从飞书 owner 账号发送一条文本消息。
2. 发送 `/status`，确认显示的是预期工作区。
3. 发送普通文本请求，开始使用 Chord。

使用飞书时，还应确认 allowlist 之外账号发送的消息会被忽略。

## 4. 稳定运行后再增加高级路由

微信始终路由到一个工作区。飞书可通过 `/bind` 和 `chat_bindings` 将不同聊天映射到不同工作区。请先完成单工作区接入，再按[飞书多工作区指南](./docs/feishu_CN.md#多工作区路由与-bind)操作。

## 5. 状态位置与后续文档

网关默认使用以下位置：

- macOS / Linux 状态目录：`${XDG_STATE_HOME:-~/.local/state}/chord-gateway`
- 配置文件：`${XDG_CONFIG_HOME:-~/.config}/chord-gateway/config.yaml`

状态目录包含日志、微信 token（默认 `<state_dir>/wechat/token.json`）、飞书去重数据和会话固定记录。飞书 `app_id` / `app_secret` 保留在配置中；短期访问 token 只保存在内存中，并按需刷新。

- [配置参考](./docs/configuration_CN.md)
- [Cookbook](./docs/cookbook_CN.md)
- [IM 接入总览](./docs/im_CN.md)
- [使用指南](./docs/usage_CN.md)
- [运维说明](./docs/operations_CN.md)
- [故障排查](./docs/troubleshooting_CN.md)
