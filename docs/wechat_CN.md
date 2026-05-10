# 微信 iLink 接入指南

本文档用于指导你在 `chord-gateway` 中配置并跑通 **微信 iLink**（`ims.wechat`）。

如果你只想快速跑通最小配置，可先看：[快速开始](../QUICKSTART_CN.md)。字段级配置说明见：[配置参考](./configuration_CN.md)。

## 开始前先准备

建议先准备好：

- 一个本地项目目录，供 Chord 操作
- 本机可用的 `chord` 可执行文件，或明确的 `chord_path`
- 一个可用于扫码登录的微信账号
- 一个可以查看 gateway 日志的位置（终端 stderr 或 `<state_dir>/gateway.log`）

对新手最稳妥的首轮流程建议是：

1. 先只配 **一个 workspace**。
2. 先使用默认的微信状态文件路径，只有确实需要时再改自定义路径。
3. 先确认 `/status` 能跑通，再去调整 token 路径或增加更多 IM 平台。

## 这条接入是什么

- gateway 通过 **微信 iLink Bot API** 接入个人微信。
- 登录方式为 **二维码登录**：gateway 会打印二维码登录 URL，用微信扫码完成登录。
- 登录态会以本地 **token 文件** 的形式持久化保存。

## 最小配置

只有一个 workspace 时，可以省略 `workspace_id`：

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
workspaces:
  default:
    path: /path/to/project
```

更接近日常使用的 starter 配置示例：

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

如果你配置了多个 workspace 且启用了微信，必须明确微信使用哪个 workspace：

```yaml
ims:
  wechat:
    workspace_id: project-a
workspaces:
  project-a:
    path: ~/work/project-a
  project-b:
    path: ~/work/project-b
```

## 首次登录（扫码）

1. 启动 gateway：

   ```bash
   chord-gateway -f config.yaml
   ```

2. 首次运行（或 token 缺失/过期）时，gateway 会打印 **二维码登录 URL**。
3. 打开该链接并用微信扫码。
4. 登录完成后，在微信聊天中发送 `/status` 验证绑定与路由是否正确。

可以把 `/status` 视为第一阶段成功标志。理想的首轮结果是：

- gateway 能正常启动，没有配置错误
- 二维码登录顺利完成
- 在同一个微信聊天里发送 `/status` 后收到回复

如果没有出现二维码登录，请按下面顺序检查：

1. 当前机器是否能访问 `ims.wechat.base_url`？
2. 你查看的是不是正确的终端窗口或 `<state_dir>/gateway.log`？
3. `<state_dir>/wechat/token.json`（或自定义 `token_path`）里是否残留了已损坏的旧 token？

## 运行时状态：token 与同步游标

微信接入会在 gateway 状态目录下维护两类关键运行时状态文件：

- **Token 文件（接近凭据级别）**
  - 默认：`<state_dir>/wechat/token.json`
  - 可通过 `ims.wechat.token_path` 覆盖

- **同步游标（不是凭据，但仍属于敏感元数据）**
  - 路径：`<state_dir>/wechat/sync-buf.json`
  - 用途：保存 `get_updates_buf`，用于恢复轮询进度，避免重启后重复处理旧消息。

运维注意：

- 这两个文件都会通过“原子替换”写入，避免崩溃导致的截断文件。
- 不要把它们提交到版本控制。
- 如果你迁移部署并希望保持连续性，建议同时迁移这两个文件。
- 除非你非常清楚冲突风险，否则不要让多个 gateway 实例共用同一份微信状态文件。

## 续期 / 重新登录

### 自动行为

gateway 检测到微信 session 过期后，会：

- 清理已保存 token
- 自动启动二维码登录流程

### 从其他 IM 手动续期（多 IM 模式）

当你同时配置了另一个 IM（例如飞书）时，可以在另一个 IM 聊天里发送：

```text
/login wechat
```

gateway 会返回微信二维码登录链接。扫码后会更新 token，通常不需要重启 gateway。

### 强制重新登录

1. 停止 gateway。
2. 删除 token 文件：
   - 默认 `<state_dir>/wechat/token.json`（或你的 `ims.wechat.token_path`）
3. 重新启动 gateway。

## 限制与风险提示

- 微信始终只路由到 **一个 workspace**。
- 微信 iLink 依赖外部 iLink API，稳定性与可用性可能不如纯 Bot 平台。
  - 如果你对稳定性/风控风险敏感，不建议用主号测试。

## 故障排查

- 没有出现二维码登录：见 [故障排查 — 微信问题](./troubleshooting_CN.md#微信问题)
- Token 过期：见 [故障排查 — Token 过期](./troubleshooting_CN.md#token-过期)
