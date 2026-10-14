# 兼容策略

Chord Gateway 仍处于 1.0 之前版本。本文档记录当前**刻意保留**的少量兼容路径，以及后续移除这些兼容路径时应遵守的规则。

## 所需的 `chord headless` 协议能力

使用的 Chord 构建必须支持以下全部能力：

- 带单调递增 `seq` 的状态事件，以及 `input_result` 输入消费回执。
- `status_response.questions` 持久化问题集合、稳定的 `option_ids`，以及 `question_request`、`question_updated`、`question_resolved` 事件。
- 带 `action` / `operation_id` 的问题命令、关联的 `question_receipt` 回执，以及带 `version`、`binding_id`、`supports_interaction` 的展示确认命令。
- `idle` 和 `status_response` 中的后台任务数量。
- 带 SHA-256 校验信息的 `image_result` 已保存图片摘要，以及带字节偏移和末块标记的有序 `image_artifact` 分块；全部生成图片分块输出后才发送整体 `idle` 事件。

旧的单问题协议及不支持输入回执的构建不受支持，网关不会转换这些旧协议。部署时固定已经验证的 Chord 构建，升级时对照 [Chord headless 文档](https://github.com/keakon/chord/blob/main/docs/headless_CN.md)。能够忽略未知订阅项，并不代表协议兼容。

升级网关后，发送 `/status` 刷新问题集合和答题入口，使用新的入口作答，不再使用旧网关发出的选项按钮。

## 会话接管能力

会话接管还需要 Chord 支持 `chord takeover preflight/start/status/retry-save --json`、`--gateway-managed`、`--gateway-instance`、`--gateway-transfer-id`、移交准备/保存/释放事件，以及带关联身份的 `ready_paused` 回执。managed 会话成功移交时使用退出码 `20`。源端与目标端必须位于同一 macOS 或 Linux 主机、同一 OS 用户及同一规范化工作区，并使用相同的 Chord 状态目录。

`ready_paused`（或 `state: ready_paused` 的 `ready`）及 `status_response` 必须包含 `paused_inputs` 摘要：

```json
{
  "paused": true,
  "pending_input_count": 1,
  "inputs": [
    {
      "input_id": "input-1",
      "owner_task_id": "task-1",
      "preview": "Review the draft",
      "attachment_count": 1
    }
  ]
}
```

摘要单独报告待处理输入总数，条目数须等于 `min(pending_input_count, 8)`，数量字段不得为负数。input 与 owner ID 须非空、最多 128 字节、不含空白或控制字符，input ID 不得重复。gateway 最多展示八项，每项包含所属任务、最多 160 个 Unicode 字符的单行预览及附件数，展示的 ID 也有长度限制。完整输入正文、附件数据及路径保留在 Chord 会话中，不进入 takeover CLI 状态或 gateway 的移交存储。接管目标须返回有效摘要且 `paused: true`，绑定才会开放。普通状态回执缺少摘要时，gateway 保留同一会话最后确认的摘要及其目录切换保护。只有明确的空摘要或会话切换才能清除此前已知的待处理输入。

gateway 在接管前检查二进制的 headless 标志。不支持 managed headless 的构建，只要符合上面的普通协议要求，仍可运行普通聊天绑定；`/takeover` 会明确报告缺少能力。没有另一套接管协议，也不会冒险改用其他恢复方式。详见[会话接管](./usage_CN.md#会话接管)。

接管的工作区身份由 Chord 的 `takeover preflight` 回执确认，执行目录通过 `-d` 单独传入；Gateway 不推导 Git 根目录。`ready`、`ready_paused`、`status_response` 与 `session_switched` 提供 `workspace` 字段，用于校验源端和恢复目标的规范化工作区。CLI 的 `preflight` 状态证明接管请求未被接纳；`prepare_failed` 状态证明该移交尚未冻结源端，只有源实例身份仍然一致且进程存活时，Gateway 才解除准备阶段的路由阻断。

## 当前保留的兼容路径

- 自动恢复 pin 按工作区和路由记录，因此在同一路由上更换平台账号时会延续原会话。本机 `pairing reset` 会连同 owner 和绑定一起清除该平台的自动 pin,重置后的路由从新会话开始。按配对身份隔离 pin 的版本所写入的条目，以及当前解析器不再寻址的 `v2:` 前管道符旧布局条目，在加载时被丢弃，不会改写到其他身份，详见[升级说明](../CHANGELOG_CN.md)。换 owner 重新配对会按单用户部署设计继承原会话；如需隔离请用 `/new` 或删除 pin。
- YAML 中未知字段会被忽略，但会在 gateway 日志中带行号报告，便于发现拼写错误或已移除的字段。已移除的 `event_visibility.activity` 字段不再生效，活动状态跟踪始终启用。
- 仍接受 Chord headless 对外发出的 `todos` 事件。虽然 Chord 内部也存在名为 `todos_updated` 的协议事件，但当前 headless 对外协议仍暴露 `todos`，gateway 会继续按该外部事件名处理。

## 已移除的兼容路径

以下兼容路径曾在更早版本中存在，但现在已经移除：

- `HandleMessage(imType, chatID, text)` —— 已移除。请使用带结构化 `IncomingMessage` 的 `HandleIncomingMessage`。
- YAML `ims` 和 `workspaces` 的 sequence / list 形式 —— 已移除。请使用以适配器类型 / workspace ID 为 key 的 mapping 形式；`/bind` 也只支持这种 mapping 结构。
- Chord headless `todos` 的裸数组 payload（`[...]`）—— 已移除。当前 Chord 发出的是包装对象 payload（`{"todos":[...]}`），gateway 也只接受这种形式。

## 清理规则

只有在受支持的外部配置 / 协议版本发生变更，并且在同一改动中同步更新测试与文档时，才应移除仍保留的兼容路径。

## 会话列表

会话列表需要 `chord sessions list --json --limit 10`，返回版本为 1 的结构、规范化 workspace 和包含 ID、RFC 3339 最近活动时间及锁定状态的 sessions 数组。不支持此命令时会明确提示查询失败，不会改为扫描旧目录。

## 聊天绑定

所有已支持平台统一要求 owner 配对与显式聊天绑定，单工作区也不例外。旧授权字段和旧 `/bind` 已移除，不自动迁移；详见[升级说明](../CHANGELOG_CN.md)。

## 跨仓库契约测试

完整测试会运行真实 Chord CLI；TUI 接管还需要包含 `TestTakeoverProcessRoundTrip` 的 Chord 测试二进制，两者必须来自兼容的 Chord 版本。CI 克隆 Chord 远端 `main`，因此应先把所需的协议和测试 fixture 改动推送到该分支，再运行 Gateway CI。本地提交不会更新 CI 使用的源码。缺少能力应当导致契约测试失败，不应跳过测试或模拟兼容。

本地测试指定源码目录：

```bash
CHORD_GATEWAY_CONTRACT_CHORD_REPO=../chord go test ./...
```

显式 `CHORD_GATEWAY_CONTRACT_CHORD_BIN` 优先级最高，其次是显式 `CHORD_GATEWAY_CONTRACT_CHORD_REPO`。两者都未设置时，优先使用 PATH 中的 `chord`，没有已安装二进制才构建相邻仓库。可通过 `CHORD_GATEWAY_CONTRACT_CHORD_HELPER_BIN` 指定预编译的 TUI fixture，否则从选定的源码目录构建；CLI 与 fixture 应保持版本一致。`go test -short ./...` 不运行这些跨仓库测试，不能证明协议兼容。
