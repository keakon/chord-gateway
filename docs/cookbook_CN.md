# Cookbook

所有 IM 平台统一使用单 owner 配对与显式聊天绑定，单工作区也需绑定。首次运行 `chord-gateway --pair`，微信/飞书/企业微信/钉钉在机器人私聊发送 `/bind "~/workspace" <pairing_code>`；详见[配对与本机重置](./pairing_CN.md)。旧部署升级步骤见[更新日志](../CHANGELOG_CN.md)。 Slack 在 App 私聊使用 `/chord bind "~/workspace" <pairing_code>`；参见 [Slack 接入指南](./slack_CN.md)。

本文档收集常见真实部署场景下可直接复制的安装、配置和运行示例。

- English version: [cookbook.md](./cookbook.md)

## 选择安装方式

### 方式 A：从 GitHub Releases 安装

适用于希望直接使用预构建二进制、不需要本地编译源码的场景。

1. 在 [GitHub Releases](https://github.com/keakon/chord-gateway/releases) 页面选择 release tag 和目标平台。

   当前 release 产物命名类似：

   ```text
   chord-gateway-${VERSION}-darwin-amd64.tar.gz
   chord-gateway-${VERSION}-darwin-arm64.tar.gz
   chord-gateway-${VERSION}-linux-amd64.tar.gz
   chord-gateway-${VERSION}-linux-arm64.tar.gz
   chord-gateway-${VERSION}-windows-amd64.zip
   SHA256SUMS
   ```

2. 下载并安装对应产物。以下是 Linux amd64 示例：

   ```bash
   VERSION=v0.3.1  # 替换成你要安装的 release
   OS=linux
   ARCH=amd64
   ASSET="chord-gateway-${VERSION}-${OS}-${ARCH}.tar.gz"

   curl -LO "https://github.com/keakon/chord-gateway/releases/download/${VERSION}/${ASSET}"
   curl -LO "https://github.com/keakon/chord-gateway/releases/download/${VERSION}/SHA256SUMS"
   grep "  ${ASSET}$" SHA256SUMS | sha256sum -c -

   tar -xzf "${ASSET}"
   sudo install -m 0755 "chord-gateway-${VERSION}-${OS}-${ARCH}/chord-gateway" /usr/local/bin/chord-gateway
   chord-gateway --version
   ```

   如果你在 macOS 上执行这段命令，请把 `sha256sum -c -` 换成 `shasum -a 256 -c -`。请根据实际平台替换 `VERSION`、`OS` 和 `ARCH`。Windows 用户下载 `.zip` 产物，并把 `chord-gateway.exe` 放到 `PATH` 中即可。

### 方式 B：通过 Go 安装

适用于本机已有目标 Go 工具链的场景：

```bash
go install github.com/keakon/chord-gateway@latest
```

二进制通常会安装到 `$(go env GOPATH)/bin`。请确保该目录已加入 `PATH`，或用绝对路径调用。

### 方式 C：从源码构建

```bash
git clone https://github.com/keakon/chord-gateway.git
cd chord-gateway
go build -o chord-gateway .
./chord-gateway --version
```

## 准备配置和状态目录

固定目录结构能简化 service 配置和排障：

```bash
mkdir -p ~/.config/chord-gateway ~/.local/state/chord-gateway
# 如果你在源码 checkout 中，可以从示例文件开始：
# cp config.example.yaml ~/.config/chord-gateway/config.yaml
# 否则请根据下方某个场景模板创建 config.yaml。
touch ~/.config/chord-gateway/config.yaml
chmod 700 ~/.config/chord-gateway ~/.local/state/chord-gateway
chmod 600 ~/.config/chord-gateway/config.yaml
```

启动 gateway 前，请先编辑 `~/.config/chord-gateway/config.yaml`。

注意事项：

- 如果通过 systemd 或 launchd 运行，建议把 `chord_path` 配成绝对路径。服务环境不会加载 shell alias，`PATH` 也可能很小。
- 不要把 Discord `bot_token`、飞书 `app_secret`、微信 token 文件或整个状态目录提交到版本控制。
- gateway 状态默认位于 `${XDG_STATE_HOME:-~/.local/state}/chord-gateway`。

## 场景：只用微信，一个 workspace

适用于一个微信登录控制一个 workspace 的场景。

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
workspaces:
  default:
    path: ~/work/project-a
chord_path: /usr/local/bin/chord
idle_timeout: 30m
```

启动：

```bash
chord-gateway -f ~/.config/chord-gateway/config.yaml --pair
```

首次运行时，gateway 会打印二维码 URL。扫码登录后，默认会把可复用的微信 token 保存到 `<state_dir>/wechat/token.json`；如需单独管理该密钥，可设置 `ims.wechat.token_path`。

先在机器人私聊发送 `/bind "~/work/project-a" <pairing_code>` 完成配对，再发送任务。

## 场景：只用飞书，一个 workspace

适用于已显式绑定的飞书聊天进入同一个 workspace 的场景。授权用户仍须先绑定聊天。

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
workspaces:
  default:
    path: ~/work/project-a
chord_path: /usr/local/bin/chord
idle_timeout: 30m
```

飞书应用配置检查清单：

1. 在飞书开发者后台使用长连接接收事件。
2. 订阅 `im.message.receive_v1`。
3. 在回调配置中添加 `card.action.trigger`，用于交互卡片。
4. 修改能力、权限或事件订阅后，发布新的应用版本。
5. 把机器人加入目标聊天。
6. 前台运行 `--pair`，在机器人私聊发送 `/bind "~/work/project-a" <pairing_code>`，成功后再发送 `/status`。

首次接入时省略 `owner` 和绑定，运行 `chord-gateway --pair`，按[配对指南](./pairing_CN.md)完成私聊配对。下方身份配置代表成功保存后的结果。

## 场景：飞书多个群，对应多个 workspace

适用于不同飞书群控制不同仓库的场景。

推荐上线流程：

1. 先按上面的单工作区流程完成飞书私聊配对。
2. 创建每个目标飞书群，并把机器人加入群里。
3. 在群里发送一条文本消息（`text` 或 `post`）。
4. 在同一个聊天里执行 `/bind "<path>"`。
5. 重新打开 `config.yaml`，确认 `ims.feishu.chat_bindings` 已按预期写入。

示例最终配置：

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner: {account_id: cli_xxx, user_id: ou_owner_xxx}
    chat_bindings:
      oc_project_a: project-a
      oc_project_b: project-b
workspaces:
  project-a:
    path: ~/work/project-a
  project-b:
    path: ~/work/project-b
chord_path: /usr/local/bin/chord
idle_timeout: 30m
```

`/bind` 只会更新飞书聊天绑定和 workspace 定义。手工修改其他配置字段后，仍需要重启 gateway 才会生效。

## 场景：Discord 私聊、频道与 thread

```yaml
ims:
  discord:
    bot_token: your-bot-token
    application_id: "123456789012345678"
chord_path: chord
```

安装 Bot 时授予 View Channel、Send Messages、Send Messages in Threads，保持 privileged intents 关闭。运行 `chord-gateway -f config.yaml --pair`，再在 Bot DM 发送 `/bind "~/work/project-a" <pairing_code>`。Owner 使用 `@Bot /bind "~/work/project-a"` 分别绑定服务器频道与可访问的既有 thread，后续命令也需 @Bot。每个 thread 独立路由和维护会话。私有 thread 访问、归档及部署验收见 [Discord 指南](./discord_CN.md)。

## 场景：微信 + 飞书

适用于微信固定一个 workspace，而飞书群独立路由的场景。

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
    owner: {account_id: bot_example, user_id: user_example}
    chat_bindings:
      user_example: project-a
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner: {account_id: cli_xxx, user_id: ou_owner_xxx}
    chat_bindings:
      oc_project_a: project-a
      oc_project_b: project-b
workspaces:
  project-a:
    path: ~/work/project-a
  project-b:
    path: ~/work/project-b
chord_path: /usr/local/bin/chord
idle_timeout: 30m
```

路由行为：

- 已绑定微信聊天的消息进入 `project-a`。
- 飞书群 `oc_project_a` 进入 `project-a`。
- 飞书群 `oc_project_b` 进入 `project-b`。

## 作为 systemd user service 运行

适用于 Linux 上希望 gateway 失败后自动重启的场景。

1. 确保配置和状态目录已存在：

   ```bash
   mkdir -p ~/.config/chord-gateway ~/.local/state/chord-gateway ~/.config/systemd/user
   ```

2. 创建 `~/.config/systemd/user/chord-gateway.service`：

   ```ini
   [Unit]
   Description=chord-gateway
   After=default.target

   [Service]
   Type=simple
   ExecStart=/usr/local/bin/chord-gateway -f %h/.config/chord-gateway/config.yaml
   Restart=on-failure
   RestartSec=5s
   Environment=CHORD_GATEWAY_STATE_DIR=%h/.local/state/chord-gateway

   [Install]
   WantedBy=default.target
   ```

   如果你通过 `go install` 安装，请把 `ExecStart` 改成 `%h/go/bin/chord-gateway ...` 或其他实际绝对路径。

3. 启用并启动：

   ```bash
   systemctl --user daemon-reload
   systemctl --user enable --now chord-gateway
   journalctl --user -u chord-gateway -f
   ```

4. 如果希望用户退出登录后服务仍继续运行，并且你的系统需要显式开启 lingering：

   ```bash
   sudo loginctl enable-linger "$USER"
   ```

Gateway 自身日志仍会写入 `<state_dir>/gateway.log`；`journalctl` 主要用于查看 service 生命周期日志。

## 在 macOS 上用 launchd 运行

适用于希望 gateway 在登录后自动启动的场景。

1. 准备目录：

   ```bash
   mkdir -p ~/.config/chord-gateway ~/.local/state/chord-gateway ~/Library/LaunchAgents
   ```

2. 创建 `~/Library/LaunchAgents/io.github.keakon.chord-gateway.plist`。

   请把 `/Users/YOUR_USER` 和二进制路径替换成真实绝对路径。`launchd` 不会在 plist 字符串里展开 `~`。

   ```xml
   <?xml version="1.0" encoding="UTF-8"?>
   <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
   <plist version="1.0">
   <dict>
     <key>Label</key>
     <string>io.github.keakon.chord-gateway</string>

     <key>ProgramArguments</key>
     <array>
       <string>/usr/local/bin/chord-gateway</string>
       <string>-f</string>
       <string>/Users/YOUR_USER/.config/chord-gateway/config.yaml</string>
     </array>

     <key>EnvironmentVariables</key>
     <dict>
       <key>CHORD_GATEWAY_STATE_DIR</key>
       <string>/Users/YOUR_USER/.local/state/chord-gateway</string>
     </dict>

     <key>RunAtLoad</key>
     <true/>
     <key>KeepAlive</key>
     <true/>

     <key>StandardOutPath</key>
     <string>/Users/YOUR_USER/.local/state/chord-gateway/launchd.out.log</string>
     <key>StandardErrorPath</key>
     <string>/Users/YOUR_USER/.local/state/chord-gateway/launchd.err.log</string>
   </dict>
   </plist>
   ```

3. 加载服务：

   ```bash
   launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/io.github.keakon.chord-gateway.plist
   launchctl enable "gui/$(id -u)/io.github.keakon.chord-gateway"
   launchctl kickstart -k "gui/$(id -u)/io.github.keakon.chord-gateway"
   ```

4. 查看日志：

   ```bash
   tail -f ~/.local/state/chord-gateway/gateway.log
   tail -f ~/.local/state/chord-gateway/launchd.err.log
   ```

## 无人值守前的运维检查清单

在长期运行 gateway 前，建议确认：

- 从每个预期使用的 IM 聊天里执行 `/status`。
- `chord_path` 在 service 环境下指向预期 Chord 二进制。
- gateway 日志路径和状态目录对 service 用户可写。
- 所有已支持平台都需完成 owner 配对，聊天必须显式绑定；本机运行并不限制外部用户向机器人发消息。
- 如果希望迁移机器后保留登录状态，请备份 `config.yaml`，以及包含微信 token 的状态目录。
- 知道如何停止服务：
  - systemd user service：`systemctl --user stop chord-gateway`
  - launchd：`launchctl bootout "gui/$(id -u)" ~/Library/LaunchAgents/io.github.keakon.chord-gateway.plist`
