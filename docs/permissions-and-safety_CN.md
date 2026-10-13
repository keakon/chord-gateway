# 权限与安全边界

所有 IM 平台统一使用单 owner 配对与显式聊天绑定，单工作区也需绑定。首次运行 `chord-gateway --pair`，在机器人私聊发送 `/bind "~/workspace" <pairing_code>`；详见[配对与本机重置](./pairing_CN.md)。旧部署升级步骤见[更新日志](../CHANGELOG_CN.md)。

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

飞书和微信都只接受已配对 owner 的输入，且聊天必须显式绑定。首次通过前台 `--pair` 和机器人私聊登记 owner；普通启动不会开放配对。运行在本机并不意味着外部用户无法向机器人发消息。

owner 拥有与 gateway 进程相当的本机控制能力，包括已有 shell 命令；配对并不是目录沙箱。仅在可信群聊中绑定机器人，因为回复对群成员可见。账号变化会阻断旧身份，重新授权需停服务后在本机 reset。详见[配对与重置](./pairing_CN.md)。

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

微信仅支持私聊，每个聊天同时绑定一个目录，可通过 `/bind` 切换。需要在不同私聊或群聊中分别控制不同目录时，请使用飞书。

## 事件响应

如果怀疑出现未授权访问：

1. 停止 gateway。
2. 吊销或轮换飞书应用密钥。
3. 删除或轮换微信 token 文件（默认 `<state_dir>/wechat/token.json`，或 `ims.wechat.token_path`）。
4. 检查 gateway 日志和 Chord session 历史。
5. 检查配置工作区中的变更。
6. 在本机 reset 并重新配对可信 owner，核对聊天绑定后再启动服务。

项目漏洞反馈方式见 [SECURITY_CN.md](../SECURITY_CN.md)。
