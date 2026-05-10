# chord-gateway Docs

This documentation is for users who want to run `chord-gateway`, connect IM platforms to `chord headless`, and operate it safely.

- Chinese version: [index_CN.md](./index_CN.md)

## Getting started

- [Quickstart](../QUICKSTART.md) – build, configure, and run a minimal gateway
- [Configuration reference](./configuration.md) – all config fields, routing rules, paths, and examples

## Daily usage

- [Usage](./usage.md) – IM commands, session pinning, questions, confirmations, and multi-IM login
- [Event visibility](./event-visibility.md) – required control-plane events and optional event subscriptions

## IM integration

- [IM integration overview](./im.md) – choose WeChat vs Feishu, minimal setup paths, and routing model
- [WeChat iLink](./wechat.md) – QR login, token/sync state, renewal, and limitations
- [Feishu](./feishu.md) – app setup, long connection events, interactive cards, and multi-workspace routing

## Operations and safety

- [Operations](./operations.md) – process lifecycle, state files, logs, dedupe, and cleanup behavior
- [Permissions & Safety](./permissions-and-safety.md) – security boundaries, credential handling, and deployment precautions
- [Compatibility policy](./compatibility.md) – supported legacy config forms and current compatibility boundaries
- [Troubleshooting](./troubleshooting.md) – common startup, WeChat, Feishu, routing, and runtime issues

## Project documents

- [Contributing](../CONTRIBUTING.md)
- [Security policy](../SECURITY.md)
- [Changelog](../CHANGELOG.md)
