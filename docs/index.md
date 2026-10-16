# chord-gateway Docs

All IM platforms use one paired owner and explicit chat bindings, including single-workspace deployments. Run `chord-gateway --pair`, then send `/bind "~/workspace" <pairing_code>` in a WeChat/Feishu/WeCom/DingTalk private bot chat. See [pairing and local reset](./pairing.md) and the [upgrade notes](../CHANGELOG.md). Slack uses `/chord bind "~/workspace" <pairing_code>` in the App DM; see the [Slack guide](./slack.md).

This documentation is for users who want to run `chord-gateway`, connect IM platforms to `chord headless`, and operate it safely.

- Chinese version: [index_CN.md](./index_CN.md)

## Getting started

- [Quickstart](../QUICKSTART.md) – build, configure, and run a minimal gateway
- [Cookbook](./cookbook.md) – release installs, common config recipes, systemd, and launchd
- [Configuration reference](./configuration.md) – all config fields, routing rules, paths, and examples

## Daily usage

- [Usage](./usage.md) – IM commands, session pinning, questions, confirmations, and multi-IM login
- [Event visibility](./event-visibility.md) – required control-plane events and optional event subscriptions

## IM integration

- [IM integration overview](./im.md) – choose an IM platform, minimal setup paths, the local console mode, and routing model
- [WeChat iLink](./wechat.md) – QR login, token/sync state, renewal, and limitations
- [Telegram](./telegram.md) – Bot API polling, group privacy, retention, and recovery
- [Feishu](./feishu.md) – app setup, long connection events, interactive cards, and multi-workspace routing
- [WeCom guide](./wecom.md) – intelligent bot setup, pairing, proactive replies, and acceptance
- [DingTalk](./dingtalk.md) – enterprise Stream robot, identity verification, proactive text, and pending deployment acceptance
- [Discord](./discord.md) – bot DMs, @Bot text commands, minimal permissions, and separate thread bindings
- [Slack](./slack.md) – Socket Mode installation, DM pairing and separate channel/thread bindings

## Operations and safety

- [Operations](./operations.md) – process lifecycle, state files, logs, dedupe, and cleanup behavior
- [Permissions & Safety](./permissions-and-safety.md) – security boundaries, credential handling, and deployment precautions
- [Compatibility policy](./compatibility.md) – kept and removed compatibility paths and cleanup rules
- [Troubleshooting](./troubleshooting.md) – common startup, WeChat, Feishu, routing, and runtime issues

## Project documents

- [Contributing](../CONTRIBUTING.md)
- [Security policy](../SECURITY.md)
- [Changelog](../CHANGELOG.md)

- [Adapter development](./adapter-development.md) — boundaries, testing and integration cost
