package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/keakon/chord-gateway/config"
)

const (
	maxNotificationRunes = 1000
	reminderInterval     = 5 * time.Minute
)

// NotificationRouter connects the IM adapter to the chord process manager.
// It parses incoming IM messages as commands, dispatches them to chord processes,
// and routes chord events back to IM users as notifications.
type NotificationRouter struct {
	mgr        *ChordManager
	cfg        *config.Config
	cfgMu      sync.RWMutex // protects cfg for concurrent read/write
	adapter    IMAdapter
	configFile string
	bindMu     sync.Mutex

	mu sync.Mutex

	// Per-binding chatID tracking: key -> chatID.
	lastKeyChatID  map[string]string
	reminders      map[string]*reminderState
	expiredPending map[string]expiredPendingState
	cardHandles    map[string]InteractiveCardHandle
}

type reminderState struct {
	timer *time.Timer
}

type expiredPendingState struct {
	Question  *QuestionPayload
	Confirm   *ConfirmPayload
	ExpiresAt time.Time
}

// NewNotificationRouter creates a new NotificationRouter.
func NewNotificationRouter(mgr *ChordManager, cfg *config.Config) *NotificationRouter {
	r := &NotificationRouter{
		mgr:            mgr,
		lastKeyChatID:  make(map[string]string),
		reminders:      make(map[string]*reminderState),
		expiredPending: make(map[string]expiredPendingState),
		cardHandles:    make(map[string]InteractiveCardHandle),
	}
	r.cfg = cfg
	mgr.SetOnEvent(r.HandleChordEvent)
	return r
}

// SetConfigFile sets the config path used by chat-binding commands.
func (r *NotificationRouter) SetConfigFile(path string) {
	r.configFile = strings.TrimSpace(path)
}

// SetAdapter sets the IM adapter used for sending notifications.
func (r *NotificationRouter) SetAdapter(adapter IMAdapter) {
	r.adapter = adapter
}

// currentAdapter returns the active IM adapter, or nil if not yet set.
func (r *NotificationRouter) currentAdapter() IMAdapter {
	return r.adapter
}

// getConfig returns the current config snapshot.
func (r *NotificationRouter) getConfig() *config.Config {
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	return r.cfg
}

// setConfig replaces the active config atomically.
func (r *NotificationRouter) setConfig(cfg *config.Config) {
	r.cfg = cfg
}

func (r *NotificationRouter) recordChatID(key, chatID string) {
	r.mu.Lock()
	r.lastKeyChatID[key] = chatID
	r.mu.Unlock()
}

func (r *NotificationRouter) recordExpiredPending(key string, state ControlState) {
	if state.ExpiredQuestion == nil && state.ExpiredConfirm == nil {
		return
	}
	r.mu.Lock()
	if r.expiredPending == nil {
		r.expiredPending = make(map[string]expiredPendingState)
	}
	r.expiredPending[key] = expiredPendingState{
		Question:  state.ExpiredQuestion,
		Confirm:   state.ExpiredConfirm,
		ExpiresAt: time.Now().Add(r.expiredPendingTTL()),
	}
	r.mu.Unlock()
}

func (r *NotificationRouter) clearExpiredPending(key string) {
	r.mu.Lock()
	delete(r.expiredPending, key)
	r.mu.Unlock()
}

func (r *NotificationRouter) lookupExpiredPending(key string) expiredPendingState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.expiredPending == nil {
		return expiredPendingState{}
	}
	expired := r.expiredPending[key]
	if !expired.ExpiresAt.IsZero() && time.Now().After(expired.ExpiresAt) {
		delete(r.expiredPending, key)
		return expiredPendingState{}
	}
	return expired
}

func cardHandleKey(processKey, requestType, requestID string) string {
	return processKey + "|" + requestType + "|" + requestID
}

func (r *NotificationRouter) recordCardHandle(processKey, requestType, requestID string, handle *InteractiveCardHandle) {
	if strings.TrimSpace(requestID) == "" || handle == nil || (strings.TrimSpace(handle.MessageID) == "" && strings.TrimSpace(handle.Token) == "") {
		return
	}
	r.mu.Lock()
	if r.cardHandles == nil {
		r.cardHandles = make(map[string]InteractiveCardHandle)
	}
	r.cardHandles[cardHandleKey(processKey, requestType, requestID)] = *handle
	r.mu.Unlock()
}

func (r *NotificationRouter) takeCardHandle(processKey, requestType, requestID string) (InteractiveCardHandle, bool) {
	if strings.TrimSpace(requestID) == "" {
		return InteractiveCardHandle{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cardHandles == nil {
		return InteractiveCardHandle{}, false
	}
	key := cardHandleKey(processKey, requestType, requestID)
	handle, ok := r.cardHandles[key]
	if ok {
		delete(r.cardHandles, key)
	}
	return handle, ok
}

func mergeCardHandles(primary, fallback InteractiveCardHandle) InteractiveCardHandle {
	if strings.TrimSpace(primary.MessageID) == "" {
		primary.MessageID = fallback.MessageID
	}
	if strings.TrimSpace(primary.Token) == "" {
		primary.Token = fallback.Token
	}
	return primary
}

func (r *NotificationRouter) expiredPendingTTL() time.Duration {
	cfg := r.getConfig()
	if cfg == nil {
		return 30 * time.Minute
	}
	return cfg.IdleTimeoutDuration()
}

func (r *NotificationRouter) snapshotLastKeyChatID() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make(map[string]string, len(r.lastKeyChatID))
	for k, v := range r.lastKeyChatID {
		cp[k] = v
	}
	return cp
}

// findFeishuAdapter resolves the FeishuAdapter from the adapter chain.
func (r *NotificationRouter) findFeishuAdapter() *FeishuAdapter {
	a := r.findAdapterByType("feishu")
	if a == nil {
		return nil
	}
	if feishu, ok := a.(*FeishuAdapter); ok {
		return feishu
	}
	return nil
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
	if msg.InternalAction != nil {
		cmd = commandFromInternalAction(msg.InternalAction)
	}
	slog.Debug("im command parsed",
		"chatID", chatID,
		"senderID", msg.SenderID,
		"messageID", msg.MessageID,
		"type", cmd.Type,
		"action", cmd.Action,
	)

	if cmd.Type == "bind" {
		r.handleBind(chatID, msg, cmd)
		return
	}

	cfg := r.getConfig()
	ws, err := cfg.ResolveWorkspace(imType, chatID)
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
	r.recordChatID(key, chatID)

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
		r.handleChordCommand(ws, chatID, cmd, imType, msg)
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
	r.sendText(chatID, formatTodoList(state.Todos))
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
			r.sendText(chatID, "⚠️ No IM adapter supports login renewal.")
			return
		}
		r.sendText(chatID, "Usage: /login <platform>\nSupported platforms: "+strings.Join(options, ", "))
		return
	}

	adapter := r.findAdapterByType(target)
	if adapter == nil {
		r.sendText(chatID, fmt.Sprintf("⚠️ No %s adapter found, please check configuration.", loginCommandName(target)))
		return
	}

	qrURL, err := adapter.StartLogin()
	if err != nil {
		if errors.Is(err, ErrLoginNotSupported) {
			r.sendText(chatID, fmt.Sprintf("⚠️ %s does not support login renewal via /login.", imDisplayName(target)))
			return
		}
		r.sendText(chatID, fmt.Sprintf("❌ Failed to get %s login link: %v", imDisplayName(target), err))
		return
	}

	r.sendText(chatID, fmt.Sprintf("⚠️ Click the link to scan and renew %s login:\n%s", imDisplayName(target), qrURL))
}

func (r *NotificationRouter) handleBind(chatID string, msg IncomingMessage, cmd IMCommand) {
	if normalizeIMType(msg.IMType) != "feishu" {
		r.sendText(chatID, "⚠️ /bind is only supported in Feishu chats.")
		return
	}
	cfg := r.getConfig()
	if cfg == nil {
		r.sendText(chatID, "❌ Configuration not loaded.")
		return
	}
	imCfg := cfg.IMConfigByType("feishu")
	if imCfg == nil || imCfg.Feishu == nil {
		r.sendText(chatID, "❌ Feishu configuration not loaded.")
		return
	}
	// Permission check: only allowed open_ids can execute /bind.
	feishuCfg := imCfg.Feishu
	if feishuCfg != nil && !feishuCfg.IsOpenIDAllowed(msg.SenderID) {
		slog.Warn("bind: sender not allowed, ignoring", "sender_id", msg.SenderID, "chat_id", chatID)
		return
	}
	if cmd.Invalid || strings.TrimSpace(cmd.WorkspaceID) == "" || strings.TrimSpace(cmd.Path) == "" {
		r.sendText(chatID, "⚠️ Usage: /bind <workspace_id> <path>")
		return
	}
	if strings.TrimSpace(r.configFile) == "" {
		r.sendText(chatID, "❌ Cannot update config: config file path not set.")
		return
	}

	oldWorkspaceID := currentFeishuBinding(cfg, chatID)
	hadOldBinding := oldWorkspaceID != ""
	if oldWorkspaceID == "" {
		if oldWS, err := cfg.ResolveWorkspace(msg.IMType, chatID); err == nil && oldWS != nil {
			oldWorkspaceID = oldWS.ID
		}
	}

	r.bindMu.Lock()
	updatedCfg, err := upsertFeishuBindingConfigFile(r.configFile, chatID, cmd.WorkspaceID, cmd.Path)
	if err == nil {
		r.setConfig(updatedCfg)
		if feishu := r.findFeishuAdapter(); feishu != nil {
			if updatedIMCfg := updatedCfg.IMConfigByType("feishu"); updatedIMCfg != nil {
				feishu.updateIMConfig(*updatedIMCfg)
			}
		}
		if r.mgr != nil {
			r.mgr.cfg = updatedCfg
		}
	}
	r.bindMu.Unlock()
	if err != nil {
		r.sendText(chatID, fmt.Sprintf("❌ Failed to update config: %v", err))
		return
	}

	oldKey := ""
	if oldWorkspaceID != "" && oldWorkspaceID != cmd.WorkspaceID {
		oldKey = (processKey{workspaceID: oldWorkspaceID, imType: msg.IMType, chatID: chatID}).String()
	}
	newKey := (processKey{workspaceID: cmd.WorkspaceID, imType: msg.IMType, chatID: chatID}).String()

	r.mu.Lock()
	if oldKey != "" {
		delete(r.lastKeyChatID, oldKey)
	}
	r.lastKeyChatID[newKey] = chatID
	r.mu.Unlock()

	if r.mgr != nil && oldKey != "" {
		r.stopReminder(oldKey)
		r.mgr.StopProcessKey(oldKey)
		if r.mgr.pins != nil {
			_ = r.mgr.pins.Set(oldKey, "")
		}
	}

	action := "Bound"
	if hadOldBinding {
		action = "Binding updated"
	}
	r.sendText(chatID, fmt.Sprintf("✅ %s：feishu/%s → workspace %s (%s)", action, chatID, cmd.WorkspaceID, cmd.Path))
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

// findAdapterByType finds an adapter by type name.
func (r *NotificationRouter) findAdapterByType(name string) IMAdapter {
	a := r.currentAdapter()
	if a == nil {
		return nil
	}
	normalizedName := normalizeIMType(name)
	if multi, ok := a.(*MultiAdapter); ok {
		return multi.FindAdapterByType(normalizedName)
	}
	if a.Type() == normalizedName {
		return a
	}
	return nil
}

func normalizeIMType(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func loginCommandName(name string) string {
	return normalizeIMType(name)
}

// HandleChordEvent is the entry point for chord events.
// It decides whether to push a notification to the IM user.
func (r *NotificationRouter) HandleChordEvent(key, eventType string, state ControlState) {
	if state.PendingQuestion != nil || state.PendingConfirm != nil {
		r.clearExpiredPending(key)
	}
	r.recordExpiredPending(key, state)
	workspaceID, imType, chatID := parseProcessKey(key)
	if workspaceID == "" || imType == "" || chatID == "" {
		slog.Warn("gateway event has invalid process key", "event", eventType, "key", key)
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

	msg := r.formatNotification(key, workspaceID, eventType, state)
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
		if eventType == "idle" || eventType == "idle_timeout" || eventType == "exit" {
			r.stopReminder(key)
		}
		return
	}

	if imType == "feishu" {
		if eventType == "confirm_request" {
			if r.sendFeishuConfirmCard(chatID, key, state) {
				r.markVisibleOutput(key)
				return
			}
		}
		if eventType == "question_request" {
			if r.sendFeishuQuestionCard(chatID, key, state) {
				r.markVisibleOutput(key)
				return
			}
		}
	}

	r.sendText(chatID, msg)
	r.markVisibleOutput(key)
}

func buildExpiredQuestionFollowup(answers []string, q *QuestionPayload) string {
	answer := strings.TrimSpace(strings.Join(answers, " "))
	var sb strings.Builder
	sb.WriteString("The previous pending question has expired, so this cannot be submitted as a structured answer. Treat the user's response below as a follow-up message and continue from the existing context if applicable.")
	if q != nil && strings.TrimSpace(q.Question) != "" {
		sb.WriteString("\n\nExpired question:\n")
		sb.WriteString(strings.TrimSpace(q.Question))
		if len(q.Options) > 0 {
			sb.WriteString("\nOptions: ")
			sb.WriteString(strings.Join(q.Options, ", "))
		}
	}
	if answer != "" {
		sb.WriteString("\n\nUser response:\n")
		sb.WriteString(answer)
	}
	return sb.String()
}

func buildFeishuResolvedCard(title, message, template string) map[string]any {
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true},
		"header": map[string]any{"title": map[string]any{"tag": "plain_text", "content": title}, "template": template},
		"body": map[string]any{"elements": []any{
			map[string]any{"tag": "markdown", "content": message},
		}},
	}
}

func displaySender(msg IncomingMessage) string {
	if strings.TrimSpace(msg.SenderName) != "" {
		return strings.TrimSpace(msg.SenderName)
	}
	if strings.TrimSpace(msg.SenderID) != "" {
		return shortID(msg.SenderID)
	}
	return "user"
}

func (r *NotificationRouter) updateFeishuCardStatus(msg IncomingMessage, processKey, requestType, requestID string, card map[string]any) {
	if msg.IMType != "feishu" {
		return
	}
	feishu := r.findFeishuAdapter()
	if feishu == nil {
		return
	}
	stored, ok := r.takeCardHandle(processKey, requestType, requestID)
	handle := stored
	if msg.InternalAction != nil {
		handle = mergeCardHandles(msg.InternalAction.Handle, stored)
	}
	if !ok && (strings.TrimSpace(handle.MessageID) == "" && strings.TrimSpace(handle.Token) == "") {
		return
	}
	if err := feishu.UpdateInteractiveCard(handle, card); err != nil {
		slog.Warn("feishu: failed to update interactive card", "request_id", requestID, "request_type", requestType, "message_id", handle.MessageID, "error", err)
	}
}

func buildExpiredConfirmFollowup(cmd IMCommand, c *ConfirmPayload) string {
	var sb strings.Builder
	sb.WriteString("The previous pending confirmation has expired, so this must not be treated as an approval or denial. Treat this only as follow-up context and ask the user to retry the original request if confirmation is still required.")
	if c != nil {
		if strings.TrimSpace(c.ToolName) != "" {
			sb.WriteString("\n\nExpired confirmation tool: ")
			sb.WriteString(strings.TrimSpace(c.ToolName))
		}
		if strings.TrimSpace(c.ArgsJSON) != "" {
			sb.WriteString("\nExpired confirmation arguments: ")
			sb.WriteString(strings.TrimSpace(c.ArgsJSON))
		}
	}
	if strings.TrimSpace(cmd.Action) != "" {
		sb.WriteString("\n\nExpired confirmation response: ")
		sb.WriteString(strings.TrimSpace(cmd.Action))
	}
	if strings.TrimSpace(cmd.Reason) != "" {
		sb.WriteString("\nReason: ")
		sb.WriteString(strings.TrimSpace(cmd.Reason))
	}
	return sb.String()
}

// formatNotification returns the notification text for the given event,
// or empty string if the event should not trigger a notification.
// chatIDsForWorkspace returns imType→chatID for all adapters that have a
// known chatID for this workspace. Used for multi-IM notification routing.
func (r *NotificationRouter) chatIDsForWorkspace(workspaceID string) map[string]string {
	result := make(map[string]string)
	for k, chatID := range r.snapshotLastKeyChatID() {
		w, imType, _ := parseProcessKey(k)
		if w != workspaceID {
			continue
		}
		if chatID != "" {
			result[imType] = chatID
		}
	}
	// Fill in missing IDs from Feishu chat bindings.
	cfg := r.getConfig()
	if cfg != nil {
		if im := cfg.IMConfigByType("feishu"); im != nil && im.Feishu != nil {
			for chatID, boundWorkspaceID := range im.Feishu.ChatBindings {
				if boundWorkspaceID == workspaceID {
					if _, has := result["feishu"]; !has {
						result["feishu"] = chatID
					}
				}
			}
		}
	}
	return result
}

// chatIDForAdapter returns the most suitable chatID for a specific adapter type.
func (r *NotificationRouter) chatIDForAdapter(adapterType string) string {
	adapterType = normalizeIMType(adapterType)
	cfg := r.getConfig()
	if cfg == nil {
		return ""
	}
	for i := range cfg.Workspaces {
		chatIDs := r.chatIDsForWorkspace(cfg.Workspaces[i].ID)
		if chatID := chatIDs[adapterType]; chatID != "" {
			return chatID
		}
	}
	return ""
}

// adapterTypeForChatID returns the adapter type associated with a known chatID.
func (r *NotificationRouter) adapterTypeForChatID(chatID string) string {
	for k, knownChatID := range r.snapshotLastKeyChatID() {
		if knownChatID != chatID {
			continue
		}
		_, imType, _ := parseProcessKey(k)
		if imType != "" {
			return imType
		}
	}
	cfg := r.getConfig()
	if cfg != nil {
		if im := cfg.IMConfigByType("feishu"); im != nil && im.Feishu != nil {
			if _, ok := im.Feishu.ChatBindings[chatID]; ok {
				return "feishu"
			}
		}
	}
	return ""
}

func (r *NotificationRouter) lookupProcessByKey(key string) (*ChordProcess, bool) {
	if r.mgr == nil {
		return nil, false
	}
	r.mgr.mu.Lock()
	defer r.mgr.mu.Unlock()
	p, ok := r.mgr.procs[key]
	return p, ok
}

// sendText sends a text message via the IM adapter, logging errors.
func (r *NotificationRouter) sendText(chatID, text string) {
	a := r.currentAdapter()
	if a == nil {
		slog.Debug("adapter not set, skipping notification", "chatID", chatID)
		return
	}
	slog.Info("gateway sending notification", "chatID", chatID, "text_len", len(text))
	if err := a.SendText(chatID, text); err != nil {
		slog.Error("failed to send notification", "chatID", chatID, "text_len", len(text), "error", err)
	}
}

// sendTextAll sends a notification to all active channels for a workspace.
// In multi-IM mode, each adapter receives the message at its tracked chatID.
// In single-IM mode, falls back to sendText with the first available chatID.
func (r *NotificationRouter) sendTextAll(workspaceID, text string) {
	a := r.currentAdapter()
	if a == nil {
		return
	}
	chatIDs := r.chatIDsForWorkspace(workspaceID)
	if multi, ok := a.(*MultiAdapter); ok && len(chatIDs) > 0 {
		for imType, chatID := range chatIDs {
			adapter := multi.FindAdapterByType(imType)
			if adapter == nil {
				continue
			}
			if err := adapter.SendText(chatID, text); err != nil {
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
	msg := fmt.Sprintf("⚠️ %s session expired: send /login %s to renew", imDisplayName(imType), loginCommandName(imType))
	if normalizeIMType(imType) == "feishu" {
		msg = "⚠️ Feishu connection invalid: Feishu tokens are refreshed automatically from configured app credentials; check deployment configuration and Feishu app event settings. /login feishu is not supported."
	}
	r.broadcastExcept(imType, msg)
}

// HandleLoginResult is called when a background login poll completes.
// It broadcasts the result to all adapters except the one that just logged in
// (since that adapter may not be able to send yet).
func (r *NotificationRouter) HandleLoginResult(imType string, success bool, errMsg string) {
	if success {
		r.broadcastExcept(imType, fmt.Sprintf("✅ %s login renewed", imDisplayName(imType)))
	} else {
		r.broadcastExcept(imType, fmt.Sprintf("❌ %s login renewal failed: %s", imDisplayName(imType), errMsg))
	}
}

// broadcastExcept sends a message through all adapters EXCEPT the one of the
// given type. Used for cross-IM notifications.
func (r *NotificationRouter) broadcastExcept(excludeType string, text string) {
	a := r.currentAdapter()
	if a == nil {
		slog.Warn("no adapter set, cannot broadcast notification")
		return
	}
	if multi, ok := a.(*MultiAdapter); ok {
		cfg := r.getConfig()
		if cfg == nil {
			slog.Warn("no config set, cannot broadcast notification")
			return
		}
		for i := range cfg.Workspaces {
			chatIDs := r.chatIDsForWorkspace(cfg.Workspaces[i].ID)
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
		return "WeChat"
	case "feishu":
		return "Feishu"
	default:
		return imType
	}
}

// parseIMCommand parses a user's text message into an IMCommand.
