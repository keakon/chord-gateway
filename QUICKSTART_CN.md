# 快速开始

本文档帮助你快速运行 `chord-gateway`，并完成微信 iLink 或飞书接入。

## 1. 前置条件

- Go 1.26+
- 本机可用 `chord` 可执行文件，或通过 `chord_path` 配置其路径
- 至少准备一种 IM 平台配置：
  - 微信 iLink（个人微信二维码登录）
  - 飞书应用凭据（`app_id`、`app_secret`）

## 2. 安装

通过 Go 安装：

```bash
go install github.com/keakon/chord-gateway@latest
```

或在本地源码目录构建：

```bash
go build -o chord-gateway .
```

## 3. 创建最小配置

把 `workspaces[].path` 指向你希望 Chord 操作的项目目录。

关键路由规则：

- 微信始终路由到一个工作区。
- 飞书在多工作区模式下要求每个 workspace 配置唯一 `im_chat_id`。
- 如果同时启用微信且存在多个 workspace，需要通过 `wechat_workspace_id` 指定微信使用哪个 workspace。

### 3.1 微信

```yaml
im:
  type: wechat
  wechat:
    base_url: https://ilinkai.weixin.qq.com
    bot_type: "3"
workspaces:
  - id: default
    path: /path/to/project
chord_path: chord
idle_timeout: 30m
```

首次运行行为：

- gateway 会打印二维码登录 URL。
- 用微信扫码登录。
- token 会保存到 gateway 状态目录，后续运行可复用。

### 3.2 飞书

```yaml
im:
  type: feishu
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    verification_token: your-token
    listen: ":8080"
    webhook_path: /feishu/callback
workspaces:
  - id: default
    path: /path/to/project
chord_path: chord
idle_timeout: 30m
```

飞书配置说明：

- 把 `listen + webhook_path` 对外暴露为公网 HTTPS URL。
- 在飞书开发者后台事件订阅配置该 URL。
- 至少订阅 `im.message.receive_v1`（接收入站消息）和 `card.action.trigger`（处理确认/提问卡片交互）。
- 如果启用了飞书事件加密，还需要设置 `encrypt_key`。
- 公网部署还建议配置 `owner_open_id` 和/或 `allowed_open_ids`。
- 飞书入站消息必须是纯文本；非文本消息会被忽略。

首次发消息前的飞书检查清单：

1. `app_id` 和 `app_secret` 有效，gateway 启动时能成功获取 app access token。
2. 公网回调 URL 能正确映射到 `listen + webhook_path`，并能通过飞书 URL 校验。
3. `verification_token` 与飞书开发者后台配置一致。
4. 如果在飞书后台启用了事件加密，gateway 配置里的 `encrypt_key` 也设置为相同值。
5. 已启用事件订阅，并包含 `im.message.receive_v1` 与 `card.action.trigger`。
6. 如果使用了 `owner_open_id` 或 `allowed_open_ids`，发送者的 `open_id` 已在最终 allowlist 中。
7. 在目标飞书聊天中先发送一条纯文本消息，再发送 `/status`。

### 3.3 微信 + 飞书 + 多工作区

如果你希望“微信固定一个 workspace，飞书不同群绑定不同 workspace”，可以这样配置：

```yaml
ims:
  - type: wechat
    wechat:
      base_url: https://ilinkai.weixin.qq.com
      bot_type: "3"
  - type: feishu
    feishu:
      app_id: cli_xxx
      app_secret: your-app-secret
      verification_token: your-token
      listen: ":8080"
      webhook_path: /feishu/callback
wechat_workspace_id: project-a
workspaces:
  - id: project-a
    path: ~/work/project-a
    im_chat_id: oc_project_a
  - id: project-b
    path: ~/work/project-b
    im_chat_id: oc_project_b
chord_path: chord
idle_timeout: 30m
```

这个配置下：

- 所有微信消息都进入 `project-a`
- 飞书群 `oc_project_a` 进入 `project-a`
- 飞书群 `oc_project_b` 进入 `project-b`

## 4. 启动

```bash
chord-gateway -f config.yaml
```

启动后：

1. 在已连接的 IM 聊天中发送 `/status`
2. 确认 gateway 能正确解析到预期 workspace，并成功连接 `chord headless`
3. 再发送普通文本开始与 Chord 交互

配置文件查找优先级：

1. `--config` / `-f`
2. `CHORD_GATEWAY_CONFIG`
3. `$XDG_CONFIG_HOME/chord-gateway/config.yaml`
4. `~/.config/chord-gateway/config.yaml`

运行态状态目录优先级：

1. `CHORD_GATEWAY_STATE_DIR`
2. `$XDG_STATE_HOME/chord-gateway`
3. `~/.local/state/chord-gateway`

状态目录中会存储日志、微信 token 文件、飞书去重数据和 session pin。默认日志文件为 `<state_dir>/gateway.log`。

## 5. 常用 IM 命令

- `/status` – 查看当前 Chord 状态
- `/cancel` – 取消当前 turn
- `/allow`, `/deny` – 批准或拒绝待确认请求
- `/answer <text>` – 回答待处理问题
- `/todos` – 查看当前 todo 列表
- `/new` – 清除当前绑定的 session pin，并启动全新 session
- `/resume <id>` – 恢复并 pin 指定 session
- `/sessions` – 列出 workspace 中最近的 session
- `/current` – 查看当前绑定状态和 pin 的 session
- `/login [platform]` – 发起登录流程，例如 `/login weixin`

## 6. 后续文档

- [README](./README_CN.md) – 发布入口概览
- [使用指南](./docs/usage_CN.md) – session 行为和 IM 命令
- [配置参考](./docs/configuration_CN.md) – 全部配置字段
- [运维说明](./docs/operations_CN.md) – 日志、状态文件、清理和路由
- [权限与安全边界](./docs/permissions-and-safety_CN.md)
- [故障排查](./docs/troubleshooting_CN.md)
