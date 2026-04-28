# 安全策略

## 支持版本

安全修复优先在 `main` 分支的最新代码上处理。

## 漏洞报告方式

请不要在公开 issue 中披露漏洞细节。

推荐渠道：

- 本仓库的 GitHub 私密漏洞报告：<https://github.com/keakon/chord-gateway/security/advisories/new>

如果 GitHub 私密报告不可用，请通过维护者 profile 页面上的联系方式联系：<https://github.com/keakon>。

建议提供：

- 影响范围与受影响组件
- 复现步骤或 PoC
- 可选修复建议

## 运行安全说明

`chord-gateway` 会启动本地 `chord headless` 进程并转发 IM 消息。
请将飞书 `app_secret`、webhook 校验 token、加密 key、微信 token 文件等视为敏感信息，
并确保不进入版本控制。

关于运行时安全边界、访问控制和部署建议，参见 [docs/permissions-and-safety_CN.md](./docs/permissions-and-safety_CN.md)。
