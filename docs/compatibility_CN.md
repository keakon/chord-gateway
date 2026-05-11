# 兼容策略

Chord Gateway 仍处于 1.0 之前版本。本文档记录当前**刻意保留**的少量兼容路径，以及后续移除这些兼容路径时应遵守的规则。

## 当前保留的兼容路径

- 仍接受 Chord headless 对外发出的 `todos` 事件。虽然 Chord 内部也存在名为 `todos_updated` 的协议事件，但当前 headless 对外协议仍暴露 `todos`，gateway 会继续按该外部事件名处理。

## 已移除的兼容路径

以下兼容路径曾在更早版本中存在，但现在已经移除：

- `HandleMessage(imType, chatID, text)` —— 已移除。请使用带结构化 `IncomingMessage` 的 `HandleIncomingMessage`。
- YAML `ims` 和 `workspaces` 的 sequence / list 形式 —— 已移除。请使用以适配器类型 / workspace ID 为 key 的 mapping 形式；`/bind` 也只支持这种 mapping 结构。
- Chord headless `todos` 的裸数组 payload（`[...]`）—— 已移除。当前 Chord 发出的是包装对象 payload（`{"todos":[...]}`），gateway 也只接受这种形式。

## 清理规则

只有在受支持的外部配置 / 协议版本发生变更，并且在同一改动中同步更新测试与文档时，才应移除仍保留的兼容路径。
