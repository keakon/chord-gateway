# chord-gateway

[![CI](https://github.com/keakon/chord-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/keakon/chord-gateway/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/keakon/chord-gateway?display_name=release)](https://github.com/keakon/chord-gateway/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/keakon/chord-gateway)](https://go.dev/)
[![License](https://img.shields.io/github/license/keakon/chord-gateway)](./LICENSE)

`chord-gateway` 用于把微信、飞书等 IM 平台连接到本地 `chord headless` 进程。你可以在聊天窗口里远程控制 Chord，同时让 agent 进程、工作区访问、凭据和状态继续保留在自己的机器或服务器上。

- English: [README.md](./README.md)
- 完整文档: [docs/index_CN.md](./docs/index_CN.md)
- 运行要求: Go 1.26+，并且本机可用 `chord` 可执行文件

## 功能特性

- 微信 iLink：扫码登录与 token 持久化（默认 `<state_dir>/wechat/token.json`）
- 飞书机器人：长连接模式、owner allowlist、事件去重
- 支持同时运行多个 IM adapter
- 按聊天会话和工作区隔离 Chord session
- 会话 pin 与恢复命令（`/new`、`/resume`、`/sessions`、`/current`）
- 某个 IM 过期时，可通过其他 IM 发送登录通知（例如提示重新登录）
- 本地 `chord headless` 子进程生命周期管理与清理
- 可配置可选控制面事件的可见性

## 安装

通过 Go 安装：

```bash
go install github.com/keakon/chord-gateway@latest
```

或在本地源码目录构建：

```bash
go build -o chord-gateway .
```

请单独安装 Chord，并确保 `chord` 在 `PATH` 中可执行；也可以在 gateway 配置中设置 `chord_path`。由于 gateway 直接执行二进制文件，`chord_path` 不支持 shell alias。

验证安装：

```bash
chord-gateway --version
```

## 快速开始

创建一个最小配置，并把 `workspaces.default.path` 指向你希望 Chord 操作的项目目录。workspace 路径必须以 `/`、`~`、Windows 盘符前缀或 UNC 前缀开头。以下是微信示例：

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

启动 gateway：

```bash
chord-gateway -f config.yaml
```

仅支持 YAML 配置文件（`.yaml` 或 `.yml`）。

启动后，在已连接的 IM 聊天中发送 `/status` 确认路由正常，再发送普通文本开始与 Chord 交互。

提示：微信始终路由到单个 workspace；当前飞书入站处理只接受文本消息。详见“支持范围 / 已知限制”。

飞书配置和多工作区路由请参见 [QUICKSTART_CN.md](./QUICKSTART_CN.md)。

## 支持范围 / 已知限制

- 源码构建需要 Go 1.26+，并且需要单独安装 `chord` 二进制；`chord-gateway` 不会内置 Chord。
- 当前 CI 会构建这些目标：`darwin/amd64`、`darwin/arm64`、`linux/amd64`、`linux/arm64`、`windows/amd64`。
- 微信始终路由到一个 workspace。存在多个 workspace 时，必须通过 `ims.wechat.workspace_id` 指定微信使用哪个 workspace。
- 飞书多 workspace 路由通过 `ims.feishu.chat_bindings` 把 chat ID 映射到 workspace ID。当前飞书入站处理只接受文本消息，非文本消息会被忽略。
- `/login [platform]` 仅适用于支持交互式登录续期的适配器；目前该流程适用于微信，不适用于飞书。

## 常用 IM 命令

| 命令 | 说明 |
|---|---|
| `/status` | 查看当前 Chord 状态 |
| `/cancel` | 取消当前 turn |
| `/allow` / `/deny [reason]` | 批准或拒绝待确认请求 |
| `/answer <text>` | 回答待处理问题；支持数字快捷选择 |
| `/todos` | 查看当前 todo 列表 |
| `/new` | 为当前绑定启动新 session |
| `/resume <id>` | 恢复并 pin 指定 session |
| `/sessions` | 列出最近 session |
| `/current` | 查看当前绑定状态和 pin 的 session |
| `/login [platform]` | 启动交互式登录流程 |

完整命令行为和会话语义见 [docs/usage_CN.md](./docs/usage_CN.md)。

## 文档

- [快速开始](./QUICKSTART_CN.md)
- [使用指南](./docs/usage_CN.md)
- [配置参考](./docs/configuration_CN.md)
- [运维说明](./docs/operations_CN.md)
- [事件可见性](./docs/event-visibility_CN.md)
- [权限与安全边界](./docs/permissions-and-safety_CN.md)
- [故障排查](./docs/troubleshooting_CN.md)
- [变更日志](./CHANGELOG_CN.md)
- English docs: [docs/index.md](./docs/index.md)

## 安全提示

`chord-gateway` 会把 IM 消息路由到本地 `chord headless` 进程，而该进程可以与配置的工作区交互。请把 IM 访问视为对该工作区的控制面访问。

公网飞书部署建议配置 allowlist（`owner_open_id` 和/或 `allowed_open_ids`）。请不要把飞书密钥、微信 token 文件（默认 `<state_dir>/wechat/token.json`，除非设置了 `ims.wechat.token_path`）或 gateway 状态目录提交到版本控制。详见 [docs/permissions-and-safety_CN.md](./docs/permissions-and-safety_CN.md) 和 [SECURITY_CN.md](./SECURITY_CN.md)。

## License

MIT License. See [LICENSE](./LICENSE).
