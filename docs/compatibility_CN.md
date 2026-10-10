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

## 当前保留的兼容路径

- 读取已有状态目录时，会话 pin 仍接受早期的 `workspaceID|imType|chatID` 存储键；新键使用编码形式。
- YAML 中未知字段会被忽略。因此已移除的 `event_visibility.activity` 字段不再生效，活动状态跟踪始终启用。
- 仍接受 Chord headless 对外发出的 `todos` 事件。虽然 Chord 内部也存在名为 `todos_updated` 的协议事件，但当前 headless 对外协议仍暴露 `todos`，gateway 会继续按该外部事件名处理。

## 已移除的兼容路径

以下兼容路径曾在更早版本中存在，但现在已经移除：

- `HandleMessage(imType, chatID, text)` —— 已移除。请使用带结构化 `IncomingMessage` 的 `HandleIncomingMessage`。
- YAML `ims` 和 `workspaces` 的 sequence / list 形式 —— 已移除。请使用以适配器类型 / workspace ID 为 key 的 mapping 形式；`/bind` 也只支持这种 mapping 结构。
- Chord headless `todos` 的裸数组 payload（`[...]`）—— 已移除。当前 Chord 发出的是包装对象 payload（`{"todos":[...]}`），gateway 也只接受这种形式。

## 清理规则

只有在受支持的外部配置 / 协议版本发生变更，并且在同一改动中同步更新测试与文档时，才应移除仍保留的兼容路径。
