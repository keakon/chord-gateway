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

const (
	maxNotificationLen = 1000
	reminderInterval   = 5 * time.Minute
)

// NotificationRouter connects the IM adapter to the chord process manager.
// It parses incoming IM messages as commands, dispatches them to chord processes,
// and routes chord events back to IM users as notifications.
type NotificationRouter struct {
	mgr        *ChordManager
	cfg        *config.Config
	cfgMu      sync.RWMutex // protects cfg for concurrent read/write
	adapter    IMAdapter    // set after creation via SetAdapter
	configFile string
	bindMu     sync.Mutex

	mu sync.Mutex

	// Per-binding chatID tracking: key -> chatID.
	lastKeyChatID  map[string]string
	reminders      map[string]*reminderState
	expiredPending map[string]expiredPendingState
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
		cfg:            cfg,
		lastKeyChatID:  make(map[string]string),
		reminders:      make(map[string]*reminderState),
		expiredPending: make(map[string]expiredPendingState),
	}
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

// getConfig returns the current config snapshot under read lock.
func (r *NotificationRouter) getConfig() *config.Config {
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	return r.cfg
}

// setConfig replaces the config under write lock.
func (r *NotificationRouter) setConfig(cfg *config.Config) {
	r.cfgMu.Lock()
	r.cfg = cfg
	r.cfgMu.Unlock()
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
	if r.adapter == nil {
		return nil
	}
	if multi, ok := r.adapter.(*MultiAdapter); ok {
		if fa := multi.FindAdapterByType("feishu"); fa != nil {
			if feishu, ok := fa.(*FeishuAdapter); ok {
				return feishu
			}
		}
	}
	if feishu, ok := r.adapter.(*FeishuAdapter); ok {
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
			r.mgr.mu.Lock()
			r.mgr.cfg = updatedCfg
			r.mgr.mu.Unlock()
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
	return strings.ToLower(strings.TrimSpace(name))
}

func loginCommandName(name string) string {
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
		r.beginTurn(procKey)
		r.sendText(chatID, "🛑 Cancel requested.")

	case "confirm":
		state := proc.State()
		pc := state.PendingConfirm
		if pc == nil || strings.TrimSpace(pc.RequestID) == "" {
			expired := r.lookupExpiredPending(procKey)
			content := buildExpiredConfirmFollowup(cmd, expired.Confirm)
			if err := proc.SendUserMessage(content); err != nil {
				slog.Error("failed to send expired confirmation follow-up", "workspace", ws.ID, "error", err)
				r.sendText(chatID, "❌ Failed to send follow-up message to chord.")
				return
			}
			r.beginTurn(procKey)
			r.sendText(chatID, "⚠️ The pending confirmation has expired. Your response was sent as a follow-up message, not as an approval or denial.")
			return
		}
		requestID := pc.RequestID
		if cmd.RequestID != "" {
			if cmd.RequestID != pc.RequestID {
				r.sendText(chatID, "⚠️ No matching pending confirmation to respond to.")
				return
			}
			requestID = cmd.RequestID
		}
		confirmCmd := map[string]any{
			"type":       "confirm",
			"request_id": requestID,
			"action":     cmd.Action,
		}
		if cmd.Action == "deny" && strings.TrimSpace(cmd.Reason) != "" {
			confirmCmd["deny_reason"] = strings.TrimSpace(cmd.Reason)
		}
		if err := proc.SendCommand(confirmCmd); err != nil {
			slog.Error("failed to send confirm response", "workspace", ws.ID, "error", err)
			r.sendText(chatID, "❌ Failed to send confirmation.")
			return
		}
		r.beginTurn(procKey)
		if cmd.Action == "deny" {
			msg := "✅ denied"
			if strings.TrimSpace(cmd.Reason) != "" {
				msg += ": " + strings.TrimSpace(cmd.Reason)
			}
			r.sendText(chatID, msg)
			return
		}
		r.sendText(chatID, "✅ allowed")

	case "question":
		// If no request ID given, use the current pending question's ID.
		state := proc.State()
		pq := state.PendingQuestion
		requestID := cmd.RequestID
		if requestID == "" && pq != nil {
			requestID = pq.RequestID
		}
		if requestID == "" {
			expired := r.lookupExpiredPending(procKey)
			q := state.ExpiredQuestion
			if q == nil {
				q = expired.Question
			}
			content := buildExpiredQuestionFollowup(cmd.Answers, q)
			if err := proc.SendUserMessage(content); err != nil {
				slog.Error("failed to send expired question follow-up", "workspace", ws.ID, "error", err)
				r.sendText(chatID, "❌ Failed to send follow-up message to chord.")
				return
			}
			r.beginTurn(procKey)
			r.sendText(chatID, "⚠️ The pending question has expired. Your response was sent as a follow-up message, not as a structured answer.")
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
		r.beginTurn(procKey)
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
				r.beginTurn(procKey)
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
		r.beginTurn(procKey)
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
			if r.sendFeishuConfirmCard(chatID, state) {
				r.markVisibleOutput(key)
				return
			}
		}
		if eventType == "question_request" {
			if r.sendFeishuQuestionCard(chatID, state) {
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
func (r *NotificationRouter) formatNotification(key, workspaceID, eventType string, state ControlState) string {
	switch eventType {
	case "notification":
		return r.formatHeadlessNotification(state)

	case "confirm_request":
		return r.formatConfirmNotification(state)

	case "question_request":
		return r.formatQuestionNotification(state)

	case "idle", "idle_timeout":
		return r.formatExpiredPendingNotification(state)

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
		return r.formatTodosNotification(state)

	case "long_running":
		return r.formatLongRunningNotification(state)

	default:
		return ""
	}
}

func (r *NotificationRouter) formatExpiredPendingNotification(state ControlState) string {
	if state.ExpiredQuestion != nil {
		return "⌛ The pending question has expired. You can still reply, and I will send it as a follow-up message instead of a structured answer."
	}
	if state.ExpiredConfirm != nil {
		return "⌛ The pending confirmation has expired. It was not approved or denied. Please retry the original request if confirmation is still needed."
	}
	return ""
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
	sb.WriteString("\nReply /allow or /deny [reason]")
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
	feishu := r.findFeishuAdapter()
	if feishu == nil {
		return false
	}
	card := buildFeishuConfirmCard(chatID, state.PendingConfirm)
	feishu.sendCardOrFallback(chatID, card, r.formatConfirmNotification(state))
	return true
}

func (r *NotificationRouter) sendFeishuQuestionCard(chatID string, state ControlState) bool {
	if state.PendingQuestion == nil || state.PendingQuestion.Multiple || len(state.PendingQuestion.Options) == 0 || len(state.PendingQuestion.Options) > 5 {
		return false
	}
	feishu := r.findFeishuAdapter()
	if feishu == nil {
		return false
	}
	card := buildFeishuQuestionCard(chatID, state.PendingQuestion)
	feishu.sendCardOrFallback(chatID, card, r.formatQuestionNotification(state))
	return true
}

func buildFeishuConfirmCard(chatID string, c *ConfirmPayload) map[string]any {
	summary := summarizeToolArgs(c.ToolName, c.ArgsJSON)
	elements := []any{map[string]any{"tag": "markdown", "content": fmt.Sprintf("**🔧 Confirm required**\n%s", c.ToolName)}}
	if summary != "" {
		elements = append(elements, map[string]any{"tag": "markdown", "content": summary})
	}
	if len(c.NeedsApproval) > 0 {
		var sb strings.Builder
		sb.WriteString("**Needs approval**")
		for _, p := range c.NeedsApproval {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			sb.WriteString("\n• ")
			sb.WriteString(p)
		}
		if sb.String() != "**Needs approval**" {
			elements = append(elements, map[string]any{"tag": "markdown", "content": truncate(sb.String())})
		}
	}
	elements = append(elements, map[string]any{"tag": "markdown", "content": "Click a button, or reply `/allow` / `/deny [reason]`."})
	actions := []any{
		feishuCardButton("Allow", "primary", map[string]any{"type": "confirm", "action": "allow", "request_id": c.RequestID, "chat_id": chatID, "im_type": "feishu"}),
		feishuCardButton("Deny", "danger", map[string]any{"type": "confirm", "action": "deny", "request_id": c.RequestID, "chat_id": chatID, "im_type": "feishu"}),
	}
	elements = append(elements, actions...)
	return map[string]any{
		"schema": "2.0",
		"header": map[string]any{"title": map[string]any{"tag": "plain_text", "content": "Confirm required"}, "template": "orange"},
		"body":   map[string]any{"elements": elements},
	}
}

func buildFeishuQuestionCard(chatID string, q *QuestionPayload) map[string]any {
	question := strings.TrimSpace(q.Question)
	if q.Header != "" {
		question = strings.TrimSpace(q.Header) + ": " + question
	}
	elements := []any{map[string]any{"tag": "markdown", "content": fmt.Sprintf("**❓ %s**", question)}}
	for i, opt := range q.Options {
		if i < len(q.OptionDetails) && strings.TrimSpace(q.OptionDetails[i]) != "" && q.OptionDetails[i] != opt {
			elements = append(elements, map[string]any{"tag": "markdown", "content": fmt.Sprintf("**%d. %s**\n%s", i+1, opt, q.OptionDetails[i])})
		}
	}
	if q.DefaultAnswer != "" {
		elements = append(elements, map[string]any{"tag": "markdown", "content": "Default: " + q.DefaultAnswer})
	}
	elements = append(elements, map[string]any{"tag": "markdown", "content": "Click an option, or reply with the option number/text. `/answer 1` also works."})
	actions := make([]any, 0, len(q.Options))
	for _, opt := range q.Options {
		actions = append(actions, feishuCardButton(opt, "default", map[string]any{"type": "question", "value": opt, "request_id": q.RequestID, "chat_id": chatID, "im_type": "feishu"}))
	}
	elements = append(elements, actions...)
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

func formatTodoList(todos []TodoItem) string {
	if len(todos) == 0 {
		return "📋 No todos."
	}

	var lines []string
	for _, t := range todos {
		prefix := "⬜"
		switch t.Status {
		case "in_progress":
			prefix = "▶"
		case "completed":
			prefix = "✅"
		case "cancelled":
			prefix = "❌"
		}
		line := fmt.Sprintf("%s %s", prefix, t.Content)
		if t.ActiveForm != "" {
			line += fmt.Sprintf(" (%s)", t.ActiveForm)
		}
		lines = append(lines, line)
	}
	return truncate("📋 Todos:\n" + strings.Join(lines, "\n"))
}

func (r *NotificationRouter) formatTodosNotification(state ControlState) string {
	return formatTodoList(state.Todos)
}

func (r *NotificationRouter) formatLongRunningNotification(state ControlState) string {
	msg := "⏳ Still working"
	if state.InternalEventsSinceLastPush > 0 {
		msg += fmt.Sprintf(" (%d internal events)", state.InternalEventsSinceLastPush)
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

func (r *NotificationRouter) ensureReminderState(key string) *reminderState {
	if r.reminders == nil {
		r.reminders = make(map[string]*reminderState)
	}
	st := r.reminders[key]
	if st == nil {
		st = &reminderState{}
		r.reminders[key] = st
	}
	return st
}

// resetReminderTimer arms or resets the reminder timer for the given key.
// Must be called with r.mu held.
func (r *NotificationRouter) resetReminderTimer(key string, st *reminderState, d time.Duration) {
	if d <= 0 {
		d = time.Nanosecond
	}
	if st.timer == nil {
		st.timer = time.AfterFunc(d, func() { r.fireReminder(key) })
	} else {
		st.timer.Reset(d)
	}
}

func reminderDelay(lastPush time.Time, now time.Time) time.Duration {
	if lastPush.IsZero() {
		return reminderInterval
	}
	elapsed := now.Sub(lastPush)
	if elapsed >= reminderInterval {
		return time.Nanosecond
	}
	return reminderInterval - elapsed
}

func (r *NotificationRouter) scheduleReminder(key string, lastPush time.Time) {
	now := time.Now()
	r.mu.Lock()
	st := r.ensureReminderState(key)
	r.resetReminderTimer(key, st, reminderDelay(lastPush, now))
	r.mu.Unlock()
}

func (r *NotificationRouter) beginTurn(key string) {
	now := time.Now()

	if proc, ok := r.lookupProcessByKey(key); ok {
		proc.mu.Lock()
		proc.state.Busy = true
		proc.state.LastPushAt = now
		proc.state.InternalEventsSinceLastPush = 0
		proc.mu.Unlock()
	}

	r.scheduleReminder(key, now)
}

func (r *NotificationRouter) markVisibleOutput(key string) {
	now := time.Now()
	if proc, ok := r.lookupProcessByKey(key); ok {
		proc.mu.Lock()
		proc.state.LastPushAt = now
		proc.state.InternalEventsSinceLastPush = 0
		proc.mu.Unlock()
	}

	r.mu.Lock()
	if st := r.reminders[key]; st != nil {
		r.resetReminderTimer(key, st, reminderInterval)
	}
	r.mu.Unlock()
}

func (r *NotificationRouter) stopReminder(key string) {
	r.mu.Lock()
	if st := r.reminders[key]; st != nil {
		if st.timer != nil {
			st.timer.Stop()
		}
		delete(r.reminders, key)
	}
	r.mu.Unlock()
}

func (r *NotificationRouter) fireReminder(key string) {
	proc, ok := r.lookupProcessByKey(key)
	if !ok || proc == nil || !proc.Alive() {
		r.stopReminder(key)
		return
	}
	state := proc.State()
	if !state.Busy {
		r.stopReminder(key)
		return
	}
	_, _, chatID := parseProcessKey(key)
	if chatID == "" {
		r.stopReminder(key)
		return
	}
	msg := r.formatLongRunningNotification(state)
	if msg != "" {
		r.sendText(chatID, msg)
		r.markVisibleOutput(key)
		return
	}
	r.scheduleReminder(key, state.LastPushAt)
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
	msg := fmt.Sprintf("⚠️ %s session expired: send /login %s to renew", imDisplayName(imType), loginCommandName(imType))
	if normalizeIMType(imType) == "feishu" {
		msg = "⚠️ Feishu session expired: please check configuration"
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
	if r.adapter == nil {
		slog.Warn("no adapter set, cannot broadcast notification")
		return
	}
	if multi, ok := r.adapter.(*MultiAdapter); ok {
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
		reason := ""
		if len(parts) > 1 {
			reason = strings.TrimSpace(text[len(parts[0]):])
		}
		return IMCommand{Type: "confirm", Action: "deny", Reason: reason}
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
	case "/bind":
		workspaceID, path, ok := parseBindArgs(strings.TrimSpace(strings.TrimPrefix(text, parts[0])))
		return IMCommand{Type: "bind", WorkspaceID: workspaceID, Path: path, Invalid: !ok}
	default:
		return IMCommand{Type: "send", Content: text}
	}
}

func commandFromInternalAction(action *InternalAction) IMCommand {
	if action == nil {
		return IMCommand{Type: "send"}
	}
	switch action.Type {
	case "confirm":
		return IMCommand{Type: "confirm", Action: action.Action, RequestID: action.RequestID}
	case "question":
		return IMCommand{Type: "question", RequestID: action.RequestID, Answers: []string{action.Value}}
	default:
		return IMCommand{Type: "send"}
	}
}

func parseBindArgs(rest string) (workspaceID, path string, ok bool) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", "", true
	}
	workspaceID, remaining, ok := nextCommandArg(rest)
	if !ok {
		return "", "", false
	}
	if strings.TrimSpace(remaining) == "" {
		return workspaceID, "", true
	}
	path, remaining, ok = nextCommandArg(remaining)
	if !ok {
		return "", "", false
	}
	if strings.TrimSpace(remaining) != "" {
		return "", "", false
	}
	return workspaceID, path, true
}

func nextCommandArg(s string) (arg, rest string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	if s[0] != '"' {
		fields := strings.Fields(s)
		if len(fields) == 0 {
			return "", "", false
		}
		idx := strings.Index(s, fields[0]) + len(fields[0])
		return fields[0], strings.TrimSpace(s[idx:]), true
	}

	var b strings.Builder
	escaped := false
	for i := 1; i < len(s); i++ {
		ch := s[i]
		if escaped {
			switch ch {
			case '"', '\\':
				b.WriteByte(ch)
			default:
				b.WriteByte('\\')
				b.WriteByte(ch)
			}
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			return b.String(), strings.TrimSpace(s[i+1:]), true
		}
		b.WriteByte(ch)
	}
	if escaped {
		b.WriteByte('\\')
	}
	return "", "", false
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
