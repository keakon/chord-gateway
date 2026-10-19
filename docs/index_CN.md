# chord-gateway 文档

所有 IM 平台统一使用单 owner 配对与显式聊天绑定，单工作区也需绑定。首次运行 `chord-gateway --pair`，微信/飞书/Telegram/企业微信/钉钉/Discord 在机器人私聊发送 `/bind "<directory>" <pairing_code>`；详见[配对与本机重置](./pairing_CN.md)。旧部署升级步骤见[更新日志](../CHANGELOG_CN.md)。 Slack 在 App 私聊使用 `/chord bind "<directory>" <pairing_code>`；参见 [Slack 接入指南](./slack_CN.md)。

本文档面向希望运行 `chord-gateway`、把 IM 平台连接到 `chord headless` 并安全运维的用户。

- English: [index.md](./index.md)

## 快速上手

- [快速开始](../QUICKSTART_CN.md) – 构建、配置并运行最小 gateway
- [Cookbook](./cookbook_CN.md) – release 安装、常见配置模板、systemd 和 launchd
- [配置参考](./configuration_CN.md) – 所有配置字段、路由规则、路径和示例

## 日常使用

- [使用指南](./usage_CN.md) – IM 命令、会话 pin、问题、确认和多 IM 登录
- [事件可见性](./event-visibility_CN.md) – 必达控制面事件和可选事件订阅

## IM 接入

- [IM 接入总览](./im_CN.md) – 选择 IM 平台、最小配置路径、本地 console 模式与路由模型
- [微信 iLink](./wechat_CN.md) – 扫码登录、token/同步游标、续期与限制
- [Telegram](./telegram_CN.md) – Bot API 长轮询、群 privacy、保留期与恢复
- [飞书](./feishu_CN.md) – 应用配置、长连接事件、交互卡片与多工作区路由
- [企业微信指南](./wecom_CN.md) – 智能机器人开通、配对、主动发送与部署验收
- [钉钉](./dingtalk_CN.md) – 企业 Stream 机器人、身份验证、主动文本与待完成部署验收
- [Discord](./discord_CN.md) – Bot DM、@Bot 文本命令、最小权限与独立 thread 绑定
- [Slack](./slack_CN.md) – Socket Mode 安装、私聊配对与频道/thread 独立绑定

## 运维与安全

- [运维说明](./operations_CN.md) – 进程生命周期、状态文件、日志、去重和清理行为
- [权限与安全边界](./permissions-and-safety_CN.md) – 安全边界、凭据处理和部署注意事项
- [兼容策略](./compatibility_CN.md) – 当前仍保留的兼容路径与兼容边界
- [故障排查](./troubleshooting_CN.md) – 启动、微信、飞书、路由和运行时常见问题

## 项目文档

- [贡献指南](../CONTRIBUTING_CN.md)
- [安全策略](../SECURITY_CN.md)
- [变更日志](../CHANGELOG_CN.md)

- [适配器开发](./adapter-development_CN.md) — 职责边界、测试与接入成本
