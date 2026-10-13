# 快速开始

所有 IM 平台统一使用单 owner 配对与显式聊天绑定，单工作区也需绑定。首次运行 `chord-gateway --pair`，在机器人私聊发送 `/bind "~/workspace" <pairing_code>`；详见[配对与本机重置](./docs/pairing_CN.md)。旧部署升级步骤见[更新日志](./CHANGELOG_CN.md)。

本指南帮助你把一个项目连接到微信或飞书。配对后即可在聊天中登记目录。

## 1. 安装 Chord 和网关

先按 [Chord 官方安装说明](https://github.com/keakon/chord/blob/main/README_CN.md#三步完成设置)安装 Chord，完成首次配置向导并验证：

```bash
chord --version
```

然后使用 Go 1.27.0 或更高版本安装网关：

```bash
go install github.com/keakon/chord-gateway@latest
chord-gateway --version
```

预编译版本、源码构建和后台服务配置见 [Cookbook](./docs/cookbook_CN.md)。

## 2. 选择平台

将下面一个最小配置保存为 `config.yaml`。

微信：

```yaml
ims:
  wechat: {}
```

飞书：

```yaml
ims:
  feishu:
    app_id: cli_example
    app_secret: your-app-secret
```

飞书应用需要启用长连接、订阅 `im.message.receive_v1` 并发布版本；使用交互卡片时添加 `card.action.trigger`。权限和控制台步骤见[飞书指南](./docs/feishu_CN.md)。

## 3. 在私聊中配对

在前台终端运行：

```bash
chord-gateway -f config.yaml --pair
```

微信按提示扫码登录。平台连接成功后，终端会显示配对码。在与机器人的私聊中发送：

```text
/bind "~/workspace" <pairing_code>
```

使用 gateway 主机上已有的目录，配对码有效期为 10 分钟。Gateway 保存 owner 和聊天绑定，下一条任务才启动 Chord；其他发送者的消息会被忽略。配对后可使用 `chord-gateway -f config.yaml` 普通启动，也可作为后台服务运行。

## 4. 绑定新聊天或切换目录

Owner 可在新聊天或空闲的已有聊天中发送 `/bind "/path/to/project"`，无需配对码。Paired 模式下两个平台均使用显式聊天路由。Owner 可以绑定飞书群，但群成员可以看到回复。切换目录前应完成任务和待处理交互。

保存失败恢复、账号变化和本机重置见[配对指南](./docs/pairing_CN.md)。

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
