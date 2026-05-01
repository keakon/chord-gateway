# 权限与安全边界

`chord-gateway` 是本地 `chord headless` 进程的远程控制面。任何能向 gateway 发送并被接受的 IM 消息的用户，都可能影响 Chord 在配置工作区中的行为。

## 安全边界

gateway 不是多租户安全沙箱。

请把被允许的 IM 发送者视为可以在配置工作区中与 Chord 交互的人。根据 Chord 配置和权限，这可能包括读取文件、编辑文件、运行工具或请求模型执行操作。

## 推荐部署方式

- 尽量使用专用机器、用户账号、容器或 VM 运行 gateway。
- 只把 `workspaces.<id>.path` 指向允许从 IM 控制的项目。
- 不要把整个 home 目录配置为 workspace。
- 保持 Chord 权限和工具审批策略保守。
- 监控日志中是否出现异常发送者、聊天、命令或工作区路由。

## 飞书访问控制

如果不是本地测试，建议为飞书配置 `owner_open_id` 和/或 `allowed_open_ids`。

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner_open_id: ou_owner_xxx
    allowed_open_ids:
      - ou_teammate_xxx
```

行为：

- 如果两个字段都未设置，则允许所有用户。
- 如果任一字段已设置，则只允许列表中的 `open_id`。
- `owner_open_id` 会自动进入有效 allowlist。
- 被拒绝的消息会被静默忽略。

## 凭据处理

以下内容都应视为敏感信息：

- 飞书 `app_secret`
- 微信 token 文件（默认 `<state_dir>/wechat/token.json`，或 `ims.wechat.token_path`）
- 微信同步游标（`<state_dir>/wechat/sync-buf.json`）
- gateway 状态目录
- Chord provider 凭据和 auth 文件

不要把密钥或状态目录提交到版本控制。条件允许时，应把 gateway 状态目录的读取权限限制在 gateway 运行用户内，因为 session pin 和同步状态即使不属于直接凭据，也可能暴露聊天 / 会话标识符。

## 多工作区安全

飞书多工作区模式下，请配置 `ims.feishu.chat_bindings`，把每个飞书 chat ID 映射到预期 workspace。

微信只支持一个 workspace。如果需要聊天到工作区的独立绑定，请使用飞书。

## 事件响应

如果怀疑出现未授权访问：

1. 停止 gateway。
2. 吊销或轮换飞书应用密钥。
3. 删除或轮换微信 token 文件（默认 `<state_dir>/wechat/token.json`，或 `ims.wechat.token_path`）。
4. 检查 gateway 日志和 Chord session 历史。
5. 检查配置工作区中的变更。
6. 使用更严格的 allowlist 和 workspace 范围重新启动。

项目漏洞反馈方式见 [SECURITY_CN.md](../SECURITY_CN.md)。
