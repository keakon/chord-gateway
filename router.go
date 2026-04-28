package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/keakon/chord-gateway/config"
)

const maxNotificationLen = 1000

// NotificationRouter connects the IM adapter to the chord process manager.
// It parses incoming IM messages as commands, dispatches them to chord processes,
// and routes chord events back to IM users as notifications.
type NotificationRouter struct {
	mgr     *ChordManager
	cfg     *config.Config
	adapter IMAdapter // set after creation via SetAdapter

	mu sync.Mutex

	// Per-binding chatID tracking: key -> chatID.
	lastKeyChatID map[string]string

	// Previous todos per key, for change detection.
	lastTodos map[string][]TodoItem
}

// NewNotificationRouter creates a new NotificationRouter.
func NewNotificationRouter(mgr *ChordManager, cfg *config.Config) *NotificationRouter {
	r := &NotificationRouter{
		mgr:           mgr,
		cfg:           cfg,
		lastKeyChatID: make(map[string]string),
		lastTodos:     make(map[string][]TodoItem),
	}
	mgr.SetOnEvent(r.HandleChordEvent)
	return r
}

// SetAdapter sets the IM adapter used for sending notifications.
func (r *NotificationRouter) SetAdapter(adapter IMAdapter) {
	r.adapter = adapter
}

// HandleMessage is a backward-compatible wrapper that constructs an
// IncomingMessage and delegates to HandleIncomingMessage.
func (r *NotificationRouter) HandleMessage(imType, chatID, text string) {
	r.HandleIncomingMessage(IncomingMessage{
		IMType:   imType,
		ChatID:   chatID,
		SenderID: chatID, // fallback: wechat uses FromUserID as chatID, so this is correct
		Text:     text,
	})
}

// HandleIncomingMessage is the primary entry point for IM messages.
// It parses the message into a command and dispatches it.
func (r *NotificationRouter) HandleIncomingMessage(msg IncomingMessage) {
	imType := msg.IMType
	chatID := msg.ChatID
	text := msg.Text

	cmd := parseIMCommand(text)
	slog.Debug("im command parsed",
		"chatID", chatID,
		"senderID", msg.SenderID,
		"messageID", msg.MessageID,
		"type", cmd.Type,
		"action", cmd.Action,
	)

	ws, err := r.cfg.ResolveWorkspace(imType, chatID)
	if err != nil {
		slog.Warn("resolve workspace failed", "imType", imType, "chatID", chatID, "error", err)
		r.sendText(chatID, fmt.Sprintf("⚠️ %v", err))
		return
	}
	if ws == nil {
		slog.Warn("no workspace for chatID", "chatID", chatID)
		r.sendText(chatID, "⚠️ No workspace configured for this chat.")
		return
	}

	key := (processKey{workspaceID: ws.ID, imType: imType, chatID: chatID}).String()
	go r.silenceWatcher(key)
	r.mu.Lock()
	r.lastKeyChatID[key] = chatID
	r.mu.Unlock()

	switch cmd.Type {
	case "new":
		r.handleNew(ws, chatID, imType)
	case "resume":
		r.handleResume(ws, chatID, cmd.SessionID, imType)
	case "sessions":
		r.handleSessions(ws, chatID)
	case "current":
		r.handleCurrent(ws, chatID, imType)
	case "todos":
		r.handleTodos(ws, chatID, imType)
	case "login":
		r.handleLogin(ws, chatID, imType, cmd.Content)
	default:
		r.handleChordCommand(ws, chatID, cmd, imType)
	}
}

// handleNew stops the current process and spawns a fresh one (new session, clears pinned resume).
func (r *NotificationRouter) handleNew(ws *config.Workspace, chatID string, imType string) {
	key := (processKey{workspaceID: ws.ID, imType: imType, chatID: chatID}).String()
	r.mgr.StopProcessKey(key)
	if r.mgr != nil && r.mgr.pins != nil {
		_ = r.mgr.pins.Set(key, "")
	}
	// SpawnWithArgsForKey without --resume creates a fresh session.
	proc, err := r.mgr.SpawnWithArgsForKey(key)
	if err != nil {
		slog.Error("failed to spawn new process", "workspace", ws.ID, "error", err)
		r.sendText(chatID, "❌ Failed to start new session.")
		return
	}
	if proc == nil {
		r.sendText(chatID, "❌ Workspace not configured.")
		return
	}
	r.sendText(chatID, "🆕 New session started.")
}

// handleResume stops the current process and spawns one with --resume.
func (r *NotificationRouter) handleResume(ws *config.Workspace, chatID, sessionID string, imType string) {
	key := (processKey{workspaceID: ws.ID, imType: imType, chatID: chatID}).String()
	r.mgr.StopProcessKey(key)
	if r.mgr != nil && r.mgr.pins != nil {
		_ = r.mgr.pins.Set(key, sessionID)
	}
	// Spawn directly with --resume flag.
	proc, err := r.mgr.SpawnWithArgsForKey(key, "--resume", sessionID)
	if err != nil {
		slog.Error("failed to spawn process for resume", "workspace", ws.ID, "error", err)
		r.sendText(chatID, "❌ Failed to resume session.")
		return
	}
	if proc == nil {
		r.sendText(chatID, "❌ Workspace not configured.")
		return
	}
	r.sendText(chatID, fmt.Sprintf("🔄 Resuming session %s", sessionID))
}

// handleSessions lists recent sessions by reading the .chord/sessions directory.
func (r *NotificationRouter) handleSessions(ws *config.Workspace, chatID string) {
	sessionsDir := ws.Path + "/.chord/sessions"
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		r.sendText(chatID, "📂 No sessions found.")
		return
	}

	// Show up to 10 most recent session directories (sorted by name = timestamp-based).
	var lines []string
	count := 0
	for i := len(entries) - 1; i >= 0 && count < 10; i-- {
		if !entries[i].IsDir() {
			continue
		}
		info, err := entries[i].Info()
		if err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("• %s (%s)", entries[i].Name(), info.ModTime().Format("2006-01-02 15:04")))
		count++
	}

	if len(lines) == 0 {
		r.sendText(chatID, "📂 No sessions found.")
		return
	}
	r.sendText(chatID, "📂 Recent sessions:\n"+strings.Join(lines, "\n"))
}

// handleCurrent shows info about the currently active session.
func (r *NotificationRouter) handleCurrent(ws *config.Workspace, chatID string, imType string) {
	key := (processKey{workspaceID: ws.ID, imType: imType, chatID: chatID}).String()
	proc, err := r.mgr.GetOrSpawnForKey(key)
	if err != nil {
		r.sendText(chatID, "❌ Failed to connect to chord process.")
		return
	}
	if proc == nil || !proc.Alive() {
		r.sendText(chatID, formatBindingStatus(ws, imType, chatID, ControlState{}))
		return
	}
	state := proc.State()
	r.sendText(chatID, formatBindingStatus(ws, imType, chatID, state))
}

// handleTodos shows the current todo list.
func (r *NotificationRouter) handleTodos(ws *config.Workspace, chatID string, imType string) {
	key := (processKey{workspaceID: ws.ID, imType: imType, chatID: chatID}).String()
	proc, err := r.mgr.GetOrSpawnForKey(key)
	if err != nil {
		r.sendText(chatID, "❌ Failed to connect to chord process.")
		return
	}
	if proc == nil || !proc.Alive() {
		r.sendText(chatID, "⏸️ No active session.")
		return
	}
	state := proc.State()
	if len(state.Todos) == 0 {
		r.sendText(chatID, "📋 No todos.")
		return
	}
	var lines []string
	for _, t := range state.Todos {
		emoji := "⬜"
		switch t.Status {
		case "in_progress":
			emoji = "🔄"
		case "completed":
			emoji = "✅"
		case "cancelled":
			emoji = "❌"
		}
		line := fmt.Sprintf("%s %s", emoji, t.Content)
		if t.ActiveForm != "" {
			line += fmt.Sprintf(" (%s)", t.ActiveForm)
		}
		lines = append(lines, line)
	}
	r.sendText(chatID, "📋 Todos:\n"+strings.Join(lines, "\n"))
}

// handleLogin initiates a login flow for the specified IM adapter.
// If no target is specified, lists the adapters that support login.
func (r *NotificationRouter) handleLogin(ws *config.Workspace, chatID string, imType string, target string) {
	_ = ws
	_ = imType
	target = normalizeIMType(target)
	if target == "" {
		options := r.availableLoginTargets()
		if len(options) == 0 {
			r.sendText(chatID, "⚠️ 当前没有支持登录续期的 IM 适配器。")
			return
		}
		r.sendText(chatID, "用法：/login <平台>\n支持的平台："+strings.Join(options, "、"))
		return
	}

	adapter := r.findAdapterByType(target)
	if adapter == nil {
		r.sendText(chatID, fmt.Sprintf("⚠️ 未找到 %s 适配器，请检查配置。", loginCommandName(target)))
		return
	}

	qrURL, err := adapter.StartLogin()
	if err != nil {
		if errors.Is(err, ErrLoginNotSupported) {
			r.sendText(chatID, fmt.Sprintf("⚠️ %s 不支持通过 /login 续期。", imDisplayName(target)))
			return
		}
		r.sendText(chatID, fmt.Sprintf("❌ 获取 %s 登录链接失败：%v", imDisplayName(target), err))
		return
	}

	r.sendText(chatID, fmt.Sprintf("⚠️ 请点击链接扫码续期%s登录：\n%s", imDisplayName(target), qrURL))
}

func (r *NotificationRouter) availableLoginTargets() []string {
	var targets []string
	seen := make(map[string]bool)
	for _, name := range []string{"wechat", "feishu", "console"} {
		adapter := r.findAdapterByType(name)
		if adapter == nil {
			continue
		}
		if _, err := adapter.StartLogin(); errors.Is(err, ErrLoginNotSupported) {
			continue
		}
		cmdName := loginCommandName(name)
		if !seen[cmdName] {
			seen[cmdName] = true
			targets = append(targets, cmdName)
		}
	}
	return targets
}

// findAdapterByType finds an adapter by type name (e.g. "weixin" → "wechat").
func (r *NotificationRouter) findAdapterByType(name string) IMAdapter {
	if r.adapter == nil {
		return nil
	}
	normalizedName := normalizeIMType(name)
	if multi, ok := r.adapter.(*MultiAdapter); ok {
		return multi.FindAdapterByType(normalizedName)
	}
	if r.adapter.Type() == normalizedName {
		return r.adapter
	}
	return nil
}

func normalizeIMType(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "weixin", "wx":
		return "wechat"
	case "lark":
		return "feishu"
	default:
		return strings.ToLower(strings.TrimSpace(name))
	}
}

func loginCommandName(name string) string {
	if normalizeIMType(name) == "wechat" {
		return "weixin"
	}
	return normalizeIMType(name)
}

// handleChordCommand dispatches a command to the chord process.
func (r *NotificationRouter) handleChordCommand(ws *config.Workspace, chatID string, cmd IMCommand, imType string) {
	procKey := (processKey{workspaceID: ws.ID, imType: imType, chatID: chatID}).String()
	proc, err := r.mgr.GetOrSpawnForKey(procKey)
	if err != nil {
		slog.Error("failed to get or spawn process", "workspace", ws.ID, "error", err)
		r.sendText(chatID, "❌ Failed to connect to chord process.")
		return
	}
	if proc == nil {
		slog.Error("no process for workspace", "workspace", ws.ID)
		r.sendText(chatID, "❌ Workspace not configured.")
		return
	}

	switch cmd.Type {
	case "status":
		// Record the LastStatusResponseAt before sending so we can detect
		// when the actual status_response arrives (not just UpdatedAt which
		// is also written on spawn/init).
		prevStatusResponseAt := proc.State().LastStatusResponseAt
		if err := proc.SendCommand(map[string]any{"type": "status"}); err != nil {
			slog.Error("failed to send status command", "workspace", ws.ID, "error", err)
			r.sendText(chatID, "❌ Failed to get status.")
			return
		}
		// Poll for the response to be processed by readLoop.
		// Wait up to 10s for LastStatusResponseAt to advance.
		for i := 0; i < 20; i++ {
			time.Sleep(500 * time.Millisecond)
			cur := proc.State().LastStatusResponseAt
			if !cur.IsZero() && (prevStatusResponseAt.IsZero() || !cur.Equal(prevStatusResponseAt)) {
				r.sendText(chatID, formatBindingStatus(ws, imType, chatID, proc.State()))
				return
			}
		}
		state := proc.State()
		r.sendText(chatID, formatBindingStatus(ws, imType, chatID, state))

	case "cancel":
		if err := proc.SendCommand(map[string]any{"type": "cancel"}); err != nil {
			slog.Error("failed to send cancel command", "workspace", ws.ID, "error", err)
			r.sendText(chatID, "❌ Failed to cancel.")
			return
		}
		r.sendText(chatID, "🛑 Cancel requested.")

	case "confirm":
		// If no request ID given, use the current pending confirm's ID.
		requestID := cmd.RequestID
		if requestID == "" {
			if pc := proc.State().PendingConfirm; pc != nil {
				requestID = pc.RequestID
			}
		}
		if requestID == "" {
			r.sendText(chatID, "⚠️ No pending confirmation to respond to.")
			return
		}
		confirmCmd := map[string]any{
			"type":       "confirm",
			"request_id": requestID,
			"action":     cmd.Action,
		}
		if err := proc.SendCommand(confirmCmd); err != nil {
			slog.Error("failed to send confirm response", "workspace", ws.ID, "error", err)
			r.sendText(chatID, "❌ Failed to send confirmation.")
			return
		}
		actionVerb := "allowed"
		if cmd.Action == "deny" {
			actionVerb = "denied"
		}
		r.sendText(chatID, fmt.Sprintf("✅ %s", actionVerb))

	case "question":
		// If no request ID given, use the current pending question's ID.
		pq := proc.State().PendingQuestion
		requestID := cmd.RequestID
		if requestID == "" && pq != nil {
			requestID = pq.RequestID
		}
		if requestID == "" {
			r.sendText(chatID, "⚠️ No pending question to answer.")
			return
		}
		// Map numeric answers to option labels (e.g. "1" → first option).
		answers := resolveQuestionAnswers(strings.Join(cmd.Answers, " "), pq)
		questionCmd := map[string]any{
			"type":       "question",
			"request_id": requestID,
			"answers":    answers,
		}
		if err := proc.SendCommand(questionCmd); err != nil {
			slog.Error("failed to send question response", "workspace", ws.ID, "error", err)
			r.sendText(chatID, "❌ Failed to send answer.")
			return
		}
		r.sendText(chatID, fmt.Sprintf("💬 Answered: %s", strings.Join(answers, ", ")))

	case "send":
		// If a pending question exists (and no pending confirm),
		// reinterpret plain text as an answer. This allows the user
		// to simply type their response without /answer prefix.
		// Unlike /answer which supports numeric shortcuts, direct
		// replies are always sent as custom text — no comma splitting
		// or index mapping, because natural language may contain commas.
		if proc.State().PendingQuestion != nil && proc.State().PendingConfirm == nil {
			if !strings.HasPrefix(cmd.Content, "/") {
				pq := proc.State().PendingQuestion
				answers := []string{cmd.Content}
				requestID := pq.RequestID
				questionCmd := map[string]any{
					"type":       "question",
					"request_id": requestID,
					"answers":    answers,
				}
				if err := proc.SendCommand(questionCmd); err != nil {
					slog.Error("failed to send question response (auto-redirect)", "workspace", ws.ID, "error", err)
					r.sendText(chatID, "❌ Failed to send answer.")
					return
				}
				r.sendText(chatID, fmt.Sprintf("💬 Answered: %s", strings.Join(answers, ", ")))
				return
			}
		}
		// Filter slash commands that are only supported in local TUI.
		// Remote control plane must not forward them.
		switch strings.ToLower(strings.TrimSpace(cmd.Content)) {
		case "/model", "/new", "/resume", "/export":
			r.sendText(chatID, "⚠️ This command is only available in local TUI.")
			return
		}
		if err := proc.SendUserMessage(cmd.Content); err != nil {
			slog.Error("failed to send user message", "workspace", ws.ID, "error", err)
			r.sendText(chatID, "❌ Failed to send message to chord.")
			return
		}
		// Force a quick control-plane response (helps debug init failures).
		_ = proc.SendCommand(map[string]any{"type": "status"})

	default:
		slog.Warn("unknown command type", "type", cmd.Type)
		r.sendText(chatID, fmt.Sprintf("⚠️ Unknown command: %s", cmd.Type))
	}
}

// HandleChordEvent is the entry point for chord events.
// It decides whether to push a notification to the IM user.
func (r *NotificationRouter) HandleChordEvent(key, eventType string, state ControlState) {
	workspaceID, imType, chatID := parseProcessKey(key)
	if chatID == "" {
		// Fallback to legacy routing: broadcast to workspace.
		msg := r.formatNotification(workspaceID, eventType, state)
		if msg != "" {
			r.sendTextAll(workspaceID, msg)
		}
		return
	}

	slog.Info("gateway routing event",
		"event", eventType,
		"key", key,
		"workspace", workspaceID,
		"im", imType,
		"chat_id", chatID,
		"channel", imType,
		"session_id", state.SessionID,
		"busy", state.Busy,
		"phase", state.Phase,
		"last_outcome", state.LastOutcome,
		"assistant_text_len", len(state.LastAssistantText),
	)

	msg := r.formatNotification(workspaceID, eventType, state)
	willSend := msg != ""
	msgLen := len(msg)
	slog.Info("gateway routing decision",
		"event", eventType,
		"key", key,
		"chat_id", chatID,
		"assistant_text_len", len(state.LastAssistantText),
		"message_len", msgLen,
		"will_send", willSend,
	)
	if msg == "" {
		return
	}

	// Special-case todos: keyed diff tracking.
	if eventType == "todos" {
		msg = r.formatTodosNotification(key, state)
		if msg == "" {
			return
		}
	}

	if imType == "feishu" {
		if eventType == "confirm_request" {
			if r.sendFeishuConfirmCard(chatID, state) {
				return
			}
		}
		if eventType == "question_request" {
			if r.sendFeishuQuestionCard(chatID, state) {
				return
			}
		}
	}

	r.sendText(chatID, msg)
}

// formatNotification returns the notification text for the given event,
// or empty string if the event should not trigger a notification.
func (r *NotificationRouter) formatNotification(workspaceID, eventType string, state ControlState) string {
	switch eventType {
	case "notification":
		return r.formatHeadlessNotification(state)

	case "confirm_request":
		return r.formatConfirmNotification(state)

	case "question_request":
		return r.formatQuestionNotification(state)

	case "idle":
		return ""

	case "error":
		return ""

	case "agent_done":
		// assistant_message already delivers the user-visible completion content.
		// Keep agent_done as internal state only; do not push another message.
		return ""

	case "assistant_message":
		// Send the completed assistant message.
		if state.LastAssistantText == "" {
			return ""
		}
		return state.LastAssistantText

	case "info":
		return r.formatInfoNotification(state)

	case "toast":
		return r.formatToastNotification(state)

	case "activity":
		// Too noisy, don't push.
		return ""

	case "exit":
		return r.formatExitNotification(workspaceID, state)

	case "tool_result":
		return r.formatToolResultNotification(state)

	case "todos":
		// Todos notifications are keyed by binding; formatTodosNotification is called
		// from HandleChordEvent where key is available.
		return ""

	case "long_running":
		return r.formatLongRunningNotification(state)

	default:
		return ""
	}
}

func (r *NotificationRouter) formatHeadlessNotification(state ControlState) string {
	if state.LastNotification == nil {
		return ""
	}
	msg := strings.TrimSpace(state.LastNotification.Message)
	if msg == "" {
		return ""
	}
	switch state.LastNotification.Reason {
	case "confirm_request":
		return truncate("🔧 " + msg)
	case "question_request":
		return truncate("❓ " + msg)
	case "error", "cancelled":
		return truncate("⚠️ " + msg)
	case "idle":
		return truncate("✅ " + msg)
	default:
		return truncate(msg)
	}
}

func (r *NotificationRouter) formatConfirmNotification(state ControlState) string {
	if state.PendingConfirm == nil {
		return ""
	}
	c := state.PendingConfirm
	var sb strings.Builder
	sb.WriteString("🔧 Confirm required: ")
	sb.WriteString(c.ToolName)

	// Show a human-readable summary of the tool args so the user knows
	// what the tool will actually do (e.g. which command Bash will run,
	// which file Write will modify).
	if summary := summarizeToolArgs(c.ToolName, c.ArgsJSON); summary != "" {
		sb.WriteString("\n")
		sb.WriteString(summary)
	}

	if len(c.NeedsApproval) > 0 {
		sb.WriteString("\n")
		for _, p := range c.NeedsApproval {
			sb.WriteString("  • ")
			sb.WriteString(p)
		}
	}
	sb.WriteString("\nReply /allow or /deny")
	return truncate(sb.String())
}

// summarizeToolArgs extracts a human-readable summary from the tool's JSON args.
func summarizeToolArgs(toolName, argsJSON string) string {
	if strings.TrimSpace(argsJSON) == "" {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		// Not valid JSON — show raw (truncated).
		return truncateLine(argsJSON, 200)
	}

	switch toolName {
	case "Bash", "Spawn":
		if cmd, _ := args["command"].(string); cmd != "" {
			return "$ " + truncateLine(cmd, 300)
		}
	case "Write", "Edit":
		if path, _ := args["path"].(string); path != "" {
			return "📝 " + path
		}
	case "Delete":
		if paths, _ := args["paths"].([]any); len(paths) > 0 {
			if s, ok := paths[0].(string); ok {
				return "🗑️ " + s
			}
		}
	case "Read":
		if path, _ := args["path"].(string); path != "" {
			return "📖 " + path
		}
	case "Grep", "Glob":
		if pat, _ := args["pattern"].(string); pat != "" {
			return "🔍 " + pat
		}
	case "WebFetch":
		if url, _ := args["url"].(string); url != "" {
			return "🌐 " + url
		}
	case "Lsp":
		// Show operation + path if available
		op, _ := args["operation"].(string)
		path, _ := args["path"].(string)
		summary := strings.TrimSpace(op + " " + path)
		if summary != "" {
			return "🔎 " + summary
		}
	}

	// Generic fallback: show key=value pairs for known fields, or raw JSON.
	var parts []string
	for _, key := range []string{"command", "path", "url", "pattern", "content", "description"} {
		if v, ok := args[key]; ok {
			s := fmt.Sprintf("%v", v)
			parts = append(parts, key+"="+truncateLine(s, 120))
		}
		if len(parts) >= 3 {
			break
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	// Last resort: raw JSON (truncated).
	return truncateLine(argsJSON, 200)
}

// truncateLine truncates a single line to maxRunes, appending "…" if needed.
func truncateLine(s string, maxRunes int) string {
	// Replace newlines so the summary stays on one line.
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) <= maxRunes {
		return s
	}
	return s[:maxRunes-1] + "…"
}

func (r *NotificationRouter) formatQuestionNotification(state ControlState) string {
	if state.PendingQuestion == nil {
		return ""
	}
	q := state.PendingQuestion
	var sb strings.Builder
	sb.WriteString("❓ ")
	if q.Header != "" {
		sb.WriteString(q.Header)
		sb.WriteString(": ")
	}
	sb.WriteString(q.Question)
	if len(q.Options) > 0 {
		for i, opt := range q.Options {
			sb.WriteString("\n  ")
			sb.WriteString(strconv.Itoa(i + 1))
			sb.WriteString(". ")
			sb.WriteString(opt)
			if i < len(q.OptionDetails) && q.OptionDetails[i] != "" && q.OptionDetails[i] != opt {
				sb.WriteString(" — ")
				sb.WriteString(q.OptionDetails[i])
			}
		}
	}
	if q.DefaultAnswer != "" {
		sb.WriteString("\nDefault: ")
		sb.WriteString(q.DefaultAnswer)
	}
	if q.Multiple {
		sb.WriteString(" (multi-select)")
	}
	sb.WriteString("\nReply /answer 1 / 1,2 / or type your answer")
	return truncate(sb.String())
}

func (r *NotificationRouter) sendFeishuConfirmCard(chatID string, state ControlState) bool {
	if state.PendingConfirm == nil {
		return false
	}
	adapter, ok := r.adapter.(*MultiAdapter)
	if ok {
		if fa := adapter.FindAdapterByType("feishu"); fa != nil {
			if feishu, ok := fa.(*FeishuAdapter); ok {
				card := buildFeishuConfirmCard(chatID, state.PendingConfirm)
				feishu.sendCardOrFallback(chatID, card, r.formatConfirmNotification(state))
				return true
			}
		}
	}
	if feishu, ok := r.adapter.(*FeishuAdapter); ok {
		card := buildFeishuConfirmCard(chatID, state.PendingConfirm)
		feishu.sendCardOrFallback(chatID, card, r.formatConfirmNotification(state))
		return true
	}
	return false
}

func (r *NotificationRouter) sendFeishuQuestionCard(chatID string, state ControlState) bool {
	if state.PendingQuestion == nil || state.PendingQuestion.Multiple || len(state.PendingQuestion.Options) == 0 || len(state.PendingQuestion.Options) > 5 {
		return false
	}
	adapter, ok := r.adapter.(*MultiAdapter)
	if ok {
		if fa := adapter.FindAdapterByType("feishu"); fa != nil {
			if feishu, ok := fa.(*FeishuAdapter); ok {
				card := buildFeishuQuestionCard(chatID, state.PendingQuestion)
				feishu.sendCardOrFallback(chatID, card, r.formatQuestionNotification(state))
				return true
			}
		}
	}
	if feishu, ok := r.adapter.(*FeishuAdapter); ok {
		card := buildFeishuQuestionCard(chatID, state.PendingQuestion)
		feishu.sendCardOrFallback(chatID, card, r.formatQuestionNotification(state))
		return true
	}
	return false
}

func buildFeishuConfirmCard(chatID string, c *ConfirmPayload) map[string]any {
	summary := summarizeToolArgs(c.ToolName, c.ArgsJSON)
	elements := []any{map[string]any{"tag": "markdown", "content": fmt.Sprintf("**🔧 Confirm required**\n%s", c.ToolName)}}
	if summary != "" {
		elements = append(elements, map[string]any{"tag": "markdown", "content": summary})
	}
	actions := []any{
		feishuCardButton("Allow", "primary", map[string]any{"command": "/allow " + c.RequestID, "request_id": c.RequestID, "chat_id": chatID, "im_type": "feishu"}),
		feishuCardButton("Deny", "danger", map[string]any{"command": "/deny " + c.RequestID, "request_id": c.RequestID, "chat_id": chatID, "im_type": "feishu"}),
	}
	elements = append(elements, map[string]any{"tag": "action", "actions": actions})
	return map[string]any{
		"schema": "2.0",
		"header": map[string]any{"title": map[string]any{"tag": "plain_text", "content": "Confirm required"}, "template": "orange"},
		"body":   map[string]any{"elements": elements},
	}
}

func buildFeishuQuestionCard(chatID string, q *QuestionPayload) map[string]any {
	elements := []any{map[string]any{"tag": "markdown", "content": fmt.Sprintf("**❓ %s**", q.Question)}}
	actions := make([]any, 0, len(q.Options))
	for _, opt := range q.Options {
		actions = append(actions, feishuCardButton(opt, "default", map[string]any{"command": "/answer " + opt, "request_id": q.RequestID, "chat_id": chatID, "im_type": "feishu"}))
	}
	elements = append(elements, map[string]any{"tag": "action", "actions": actions})
	return map[string]any{
		"schema": "2.0",
		"header": map[string]any{"title": map[string]any{"tag": "plain_text", "content": "Question"}, "template": "blue"},
		"body":   map[string]any{"elements": elements},
	}
}

func feishuCardButton(label, style string, value map[string]any) map[string]any {
	return map[string]any{
		"tag":       "button",
		"type":      style,
		"text":      map[string]any{"tag": "plain_text", "content": label},
		"behaviors": []any{map[string]any{"type": "callback", "value": value}},
	}
}

// resolveQuestionAnswers interprets the /answer input against the question's
// options. If the input is purely comma-separated numeric indices within range
// (e.g. "1" or "1,3"), they are mapped to option labels. Otherwise the entire
// input is returned as a single custom-text answer for the model. Single-select
// questions with multiple indices also fall back to custom text.
func resolveQuestionAnswers(input string, q *QuestionPayload) []string {
	if q == nil || len(q.Options) == 0 {
		return []string{input}
	}
	parts := strings.Split(input, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > len(q.Options) {
			// Non-numeric or out of range: treat entire input as custom text.
			return []string{input}
		}
		result = append(result, q.Options[n-1])
	}
	// Single-select with multiple indices is ambiguous; treat as custom text.
	if !q.Multiple && len(result) > 1 {
		return []string{input}
	}
	return result
}

func (r *NotificationRouter) formatIdleNotification(workspaceID string, state ControlState) string {
	switch state.LastOutcome {
	case "completed":
		return ""

	case "error":
		msg := "❌ Error"
		if state.LastError != "" {
			msg += ": " + state.LastError
		}
		return truncate(msg)

	case "cancelled":
		return ""

	default:
		return ""
	}
}

func (r *NotificationRouter) formatErrorNotification(workspaceID string, state ControlState) string {
	msg := "⚠️ Error"
	if state.LastError != "" {
		msg += ": " + state.LastError
	}
	return truncate(msg)
}

func (r *NotificationRouter) formatInfoNotification(state ControlState) string {
	if state.InfoMessage == "" {
		return ""
	}
	return truncate("ℹ️ " + state.InfoMessage)
}

func (r *NotificationRouter) formatToastNotification(state ControlState) string {
	if state.ToastMessage == "" {
		return ""
	}
	// Only push warn/error level toasts.
	switch state.ToastLevel {
	case "warn", "error":
		return truncate("🔔 " + state.ToastMessage)
	default:
		return ""
	}
}

func (r *NotificationRouter) formatExitNotification(workspaceID string, state ControlState) string {
	if state.Busy {
		return "🔌 Chord process exited unexpectedly."
	}
	return ""
}

func (r *NotificationRouter) formatToolResultNotification(state ControlState) string {
	if state.LastToolResult == nil {
		return ""
	}
	if state.LastToolResult.Status == "error" {
		return truncate(fmt.Sprintf("⚠️ Tool %s failed", state.LastToolResult.Name))
	}
	return ""
}

func (r *NotificationRouter) formatTodosNotification(key string, state ControlState) string {
	prev := r.lastTodos[key]
	r.lastTodos[key] = state.Todos

	// Build a map of previous todos by ID for quick lookup.
	prevMap := make(map[string]TodoItem, len(prev))
	for _, t := range prev {
		prevMap[t.ID] = t
	}

	// Check for new todos or status changes to in_progress.
	for _, t := range state.Todos {
		if t.Status == "in_progress" {
			old, exists := prevMap[t.ID]
			if !exists || old.Status != "in_progress" {
				return truncate(fmt.Sprintf("🔄 %s", t.Content))
			}
		}
	}
	return ""
}

func (r *NotificationRouter) formatLongRunningNotification(state ControlState) string {
	msg := "⏳ Still working"
	if state.Phase != "" {
		msg += " — " + state.Phase
		if state.PhaseDetail != "" {
			msg += ": " + state.PhaseDetail
		}
	}
	if state.ToolCallsSinceLastPush > 0 {
		msg += fmt.Sprintf(" (%d tool updates)", state.ToolCallsSinceLastPush)
	}
	return truncate(msg)
}

func workspaceDisplayName(ws *config.Workspace) string {
	if ws == nil {
		return "(unknown)"
	}
	base := filepath.Base(strings.TrimRight(ws.Path, string(os.PathSeparator)))
	if base == "." || base == string(os.PathSeparator) || base == "" {
		base = ws.Path
	}
	return base
}

func formatBindingStatus(ws *config.Workspace, imType, chatID string, state ControlState) string {
	var sb strings.Builder
	if state.Busy {
		sb.WriteString("🔄 Busy")
	} else {
		sb.WriteString("⏸️ Idle")
	}
	if ws != nil {
		sb.WriteString("\n🗂️ Workspace: ")
		sb.WriteString(ws.ID)
		sb.WriteString(" (")
		sb.WriteString(workspaceDisplayName(ws))
		sb.WriteString(")")
	}
	if imType != "" || chatID != "" {
		sb.WriteString("\n💬 Binding: ")
		sb.WriteString(imType)
		if chatID != "" {
			sb.WriteString("/")
			sb.WriteString(chatID)
		}
	}
	if state.SessionID != "" {
		sb.WriteString("\n🧵 Session: ")
		sb.WriteString(state.SessionID)
	} else {
		sb.WriteString("\n🧵 Session: (none)")
	}
	if state.Phase != "" {
		sb.WriteString("\n📍 Phase: ")
		sb.WriteString(state.Phase)
		if state.PhaseDetail != "" {
			sb.WriteString(" — ")
			sb.WriteString(state.PhaseDetail)
		}
	}
	if state.PendingConfirm != nil {
		sb.WriteString("\n🔧 Pending confirm: ")
		sb.WriteString(state.PendingConfirm.ToolName)
	}
	if state.PendingQuestion != nil {
		sb.WriteString("\n❓ Pending question: ")
		sb.WriteString(state.PendingQuestion.Question)
	}
	if state.LastOutcome != "" {
		sb.WriteString("\n📋 Last outcome: ")
		sb.WriteString(state.LastOutcome)
	}
	if state.LastError != "" {
		sb.WriteString("\n❌ Last error: ")
		sb.WriteString(state.LastError)
	}
	if state.LastToolResult != nil {
		sb.WriteString("\n🔧 Last tool: ")
		sb.WriteString(state.LastToolResult.Name)
		sb.WriteString(" (")
		sb.WriteString(state.LastToolResult.Status)
		sb.WriteString(")")
	}
	if len(state.Todos) > 0 {
		completed := 0
		for _, t := range state.Todos {
			if t.Status == "completed" {
				completed++
			}
		}
		sb.WriteString(fmt.Sprintf("\n📋 Todos: %d/%d completed", completed, len(state.Todos)))
	}
	return truncate(sb.String())
}

// chatIDsForWorkspace returns imType→chatID for all adapters that have a
// known chatID for this workspace. Used for multi-IM notification routing.
func (r *NotificationRouter) chatIDsForWorkspace(workspaceID string) map[string]string {
	result := make(map[string]string)
	for k, chatID := range r.lastKeyChatID {
		w, imType, _ := parseProcessKey(k)
		if w != workspaceID {
			continue
		}
		if chatID != "" {
			result[imType] = chatID
		}
	}
	// Fill in missing from workspace config's IMChatID (typically for feishu).
	for i := range r.cfg.Workspaces {
		if r.cfg.Workspaces[i].ID == workspaceID && r.cfg.Workspaces[i].IMChatID != "" {
			if _, has := result["feishu"]; !has {
				result["feishu"] = r.cfg.Workspaces[i].IMChatID
			}
		}
	}
	return result
}

// chatIDForAdapter returns the most suitable chatID for a specific adapter type.
func (r *NotificationRouter) chatIDForAdapter(adapterType string) string {
	adapterType = normalizeIMType(adapterType)
	for i := range r.cfg.Workspaces {
		chatIDs := r.chatIDsForWorkspace(r.cfg.Workspaces[i].ID)
		if chatID := chatIDs[adapterType]; chatID != "" {
			return chatID
		}
	}
	return ""
}

// adapterTypeForChatID returns the adapter type associated with a known chatID.
func (r *NotificationRouter) adapterTypeForChatID(chatID string) string {
	for k, knownChatID := range r.lastKeyChatID {
		if knownChatID != chatID {
			continue
		}
		_, imType, _ := parseProcessKey(k)
		if imType != "" {
			return imType
		}
	}
	for i := range r.cfg.Workspaces {
		if r.cfg.Workspaces[i].IMChatID == chatID {
			return "feishu"
		}
	}
	return ""
}

func (r *NotificationRouter) silenceWatcher(key string) {
	const (
		minQuiet  = 3 * time.Minute
		interval  = 30 * time.Second
		maxAlerts = 3
	)

	alerts := 0
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		proc, err := r.mgr.GetOrSpawnForKey(key)
		if err != nil {
			return
		}
		if proc == nil || !proc.Alive() {
			return
		}
		state := proc.State()
		if !state.Busy {
			return
		}
		if alerts >= maxAlerts {
			return
		}
		if state.LastPushAt.IsZero() || time.Since(state.LastPushAt) < minQuiet {
			continue
		}
		if state.ToolCallsSinceLastPush <= 0 && state.Phase == "" {
			continue
		}

		_, _, chatID := parseProcessKey(key)
		if chatID == "" {
			return
		}
		msg := r.formatLongRunningNotification(state)
		if msg == "" {
			continue
		}
		r.sendText(chatID, msg)
		alerts++

		// Reset counters under process lock.
		proc.mu.Lock()
		proc.state.ToolCallsSinceLastPush = 0
		proc.state.LastPushAt = time.Now()
		proc.mu.Unlock()
	}
}

// sendText sends a text message via the IM adapter, logging errors.
func (r *NotificationRouter) sendText(chatID, text string) {
	if r.adapter == nil {
		slog.Debug("adapter not set, skipping notification", "chatID", chatID)
		return
	}
	slog.Info("gateway sending notification", "chatID", chatID, "text_len", len(text))
	if err := r.adapter.SendText(chatID, text); err != nil {
		slog.Error("failed to send notification", "chatID", chatID, "text_len", len(text), "error", err)
	}
}

// sendTextAll sends a notification to all active channels for a workspace.
// In multi-IM mode, each adapter receives the message at its tracked chatID.
// In single-IM mode, falls back to sendText with the first available chatID.
func (r *NotificationRouter) sendTextAll(workspaceID, text string) {
	if r.adapter == nil {
		return
	}
	chatIDs := r.chatIDsForWorkspace(workspaceID)
	if multi, ok := r.adapter.(*MultiAdapter); ok && len(chatIDs) > 0 {
		for imType, chatID := range chatIDs {
			a := multi.FindAdapterByType(imType)
			if a == nil {
				continue
			}
			if err := a.SendText(chatID, text); err != nil {
				slog.Error("failed to send notification", "type", imType, "chatID", chatID, "error", err)
			}
		}
		return
	}
	// Single-IM mode: use the first chatID.
	for _, chatID := range chatIDs {
		r.sendText(chatID, text)
		return
	}
}

// HandleSessionExpired is called when an IM adapter detects its session has
// expired (e.g. WeChat iLink errcode=-14). It broadcasts a notification to
// all OTHER adapters so the user can take action.
func (r *NotificationRouter) HandleSessionExpired(imType string) {
	msg := fmt.Sprintf("⚠️ %s 会话已失效：请发送 /login %s 续期", imDisplayName(imType), loginCommandName(imType))
	if normalizeIMType(imType) == "feishu" {
		msg = "⚠️ 飞书会话已失效：请检查配置"
	}
	r.broadcastExcept(imType, msg)
}

// HandleLoginResult is called when a background login poll completes.
// It broadcasts the result to all adapters except the one that just logged in
// (since that adapter may not be able to send yet).
func (r *NotificationRouter) HandleLoginResult(imType string, success bool, errMsg string) {
	if success {
		r.broadcastExcept(imType, fmt.Sprintf("✅ %s 登录已续期", imDisplayName(imType)))
	} else {
		r.broadcastExcept(imType, fmt.Sprintf("❌ %s 登录续期失败：%s", imDisplayName(imType), errMsg))
	}
}

// broadcastExcept sends a message through all adapters EXCEPT the one of the
// given type. Used for cross-IM notifications.
func (r *NotificationRouter) broadcastExcept(excludeType string, text string) {
	if r.adapter == nil {
		slog.Warn("no adapter set, cannot broadcast notification")
		return
	}
	if multi, ok := r.adapter.(*MultiAdapter); ok {
		for i := range r.cfg.Workspaces {
			chatIDs := r.chatIDsForWorkspace(r.cfg.Workspaces[i].ID)
			if len(chatIDs) == 0 {
				continue
			}
			multi.BroadcastTextExcept(excludeType, chatIDs, text)
		}
		return
	}
	// Single adapter mode — can't cross-notify.
	slog.Warn("session issue but no multi-IM for cross-notification", "exclude", excludeType)
}

// imDisplayName returns a human-friendly name for an IM type.
func imDisplayName(imType string) string {
	switch normalizeIMType(imType) {
	case "wechat":
		return "微信"
	case "feishu":
		return "飞书"
	default:
		return imType
	}
}

// parseIMCommand parses a user's text message into an IMCommand.
func parseIMCommand(text string) IMCommand {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return IMCommand{Type: "send", Content: text}
	}

	parts := strings.SplitN(text, " ", 3)
	cmd := strings.ToLower(parts[0])

	switch cmd {
	case "/status":
		return IMCommand{Type: "status"}
	case "/summary":
		return IMCommand{Type: "send", Content: "/summary"}
	case "/cancel":
		return IMCommand{Type: "cancel"}
	case "/allow":
		requestID := ""
		if len(parts) > 1 {
			requestID = strings.TrimSpace(parts[1])
		}
		return IMCommand{Type: "confirm", Action: "allow", RequestID: requestID}
	case "/deny":
		requestID := ""
		if len(parts) > 1 {
			requestID = strings.TrimSpace(parts[1])
		}
		return IMCommand{Type: "confirm", Action: "deny", RequestID: requestID}
	case "/answer":
		answer := ""
		if len(parts) > 1 {
			answer = strings.Join(parts[1:], " ")
		}
		return IMCommand{Type: "question", Answers: []string{answer}}
	case "/new":
		return IMCommand{Type: "new"}
	case "/resume":
		sessionID := ""
		if len(parts) > 1 {
			sessionID = strings.TrimSpace(parts[1])
		}
		return IMCommand{Type: "resume", SessionID: sessionID}
	case "/sessions":
		return IMCommand{Type: "sessions"}
	case "/current":
		return IMCommand{Type: "current"}
	case "/todos":
		return IMCommand{Type: "todos"}
	case "/login":
		target := ""
		if len(parts) > 1 {
			target = strings.TrimSpace(parts[1])
		}
		return IMCommand{Type: "login", Content: target}
	default:
		return IMCommand{Type: "send", Content: text}
	}
}

// formatStatus formats a ControlState as a human-readable status message.
func formatStatus(state ControlState) string {
	return formatBindingStatus(nil, "", "", state)
}

// truncate shortens a string to maxNotificationLen with ellipsis if needed.
func truncate(s string) string {
	if len(s) <= maxNotificationLen {
		return s
	}
	return s[:maxNotificationLen-3] + "..."
}
