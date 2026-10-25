package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

const (
	maxNotificationRunes = 1000
	reminderInterval     = 5 * time.Minute
)

// NotificationRouter connects the IM adapter to the chord process manager.
// It parses incoming IM messages as commands, dispatches them to chord processes,
// and routes chord events back to IM users as notifications.
type NotificationRouter struct {
	admission    *Admission
	ingressStore *IngressStore
	ingress      *IngressService

	mgr       *ChordManager
	adapter   im.IMAdapter // protected by adapterMu
	adapterMu sync.RWMutex

	mu                sync.RWMutex
	questionReceiptMu sync.Mutex // serializes durable Question receipt settlement

	// Per-binding chatID tracking: key -> chatID.
	lastKeyChatID         map[string]string
	lastKeyChatIDSnapshot atomic.Pointer[map[string]string]
	reminders             map[string]*time.Timer
	expiredPending        map[string]expiredPendingState
	shownQuestions        map[string]map[string]bool
	deliveredPrompts      map[string]deliveredPrompt
	questionOperations    map[string]pendingQuestionOperation
	cardHandles           map[string]cardHandleEntry
	transferNotices       map[string]transferNotice
	outbound              *outboundDispatcher
}

// cardHandleEntry is a recorded interactive card handle together with the time
// it was registered, so handles that can no longer be used can be pruned.
type cardHandleEntry struct {
	handle     im.InteractiveCardHandle
	recordedAt time.Time
	terminal   bool
}

// interactiveCardHandleTTL matches the Feishu card update window: a handle
// older than this can no longer patch its card anyway, so it is discarded to
// keep unclicked cards from growing the map without bound.
const interactiveCardHandleTTL = 14 * 24 * time.Hour

type expiredPendingState struct {
	Confirm   *ConfirmPayload
	Handoff   *HandoffPayload
	ExpiresAt time.Time
}

// NewNotificationRouter creates a new NotificationRouter.
// The router reads the active config from mgr (single source of truth).
func NewNotificationRouter(mgr *ChordManager) *NotificationRouter {
	r := &NotificationRouter{
		mgr:             mgr,
		lastKeyChatID:   make(map[string]string),
		reminders:       make(map[string]*time.Timer),
		expiredPending:  make(map[string]expiredPendingState),
		cardHandles:     make(map[string]cardHandleEntry),
		transferNotices: make(map[string]transferNotice),
		outbound:        newOutboundDispatcher(),
	}
	if mgr != nil {
		mgr.SetOnEvent(r.HandleChordEvent)
		mgr.SetOnImage(r.HandleChordImage)
	}
	return r
}

// SetAdapter sets the IM adapter used for sending notifications.
func (r *NotificationRouter) SetAdapter(adapter im.IMAdapter) {
	r.adapterMu.Lock()
	r.adapter = adapter
	r.adapterMu.Unlock()
	r.installIngressReadinessNotifier(adapter)
}

// currentAdapter returns the active IM adapter, or nil if not yet set.
func (r *NotificationRouter) currentAdapter() im.IMAdapter {
	r.adapterMu.RLock()
	defer r.adapterMu.RUnlock()
	return r.adapter
}

// Close drains queued proactive notifications. No new tasks are accepted once
// shutdown begins; callers then disconnect adapters after this returns.
func (r *NotificationRouter) Close() {
	if r == nil {
		return
	}
	r.outbound.close()
}

// getConfig returns the current config snapshot from the chord manager.
func (r *NotificationRouter) getConfig() *config.Config {
	if r.mgr == nil {
		return nil
	}
	return r.mgr.Config()
}

func (r *NotificationRouter) recordChatID(key, chatID string) {
	r.mu.Lock()
	if r.lastKeyChatID == nil {
		r.lastKeyChatID = make(map[string]string)
	}
	r.lastKeyChatID[key] = chatID
	r.publishLastKeyChatIDLocked()
	r.mu.Unlock()
}

func (r *NotificationRouter) recordExpiredPending(key string, state ControlState) {
	if state.ExpiredConfirm == nil && state.ExpiredHandoff == nil {
		return
	}
	r.mu.Lock()
	if r.expiredPending == nil {
		r.expiredPending = make(map[string]expiredPendingState)
	}
	r.expiredPending[key] = expiredPendingState{

		Confirm:   state.ExpiredConfirm,
		Handoff:   state.ExpiredHandoff,
		ExpiresAt: time.Now().Add(r.expiredPendingTTL()),
	}
	r.mu.Unlock()
}

func (r *NotificationRouter) clearExpiredPending(key string) {
	r.mu.Lock()
	delete(r.expiredPending, key)
	r.mu.Unlock()
}

func (r *NotificationRouter) markConfirmCardTerminal(processKey, requestID string) {
	if strings.TrimSpace(requestID) == "" {
		return
	}
	key := cardHandleKey(processKey, "confirm", requestID)
	r.mu.Lock()
	if r.cardHandles == nil {
		r.cardHandles = make(map[string]cardHandleEntry)
	}
	r.pruneCardHandlesLocked(time.Now())
	entry := r.cardHandles[key]
	if entry.recordedAt.IsZero() {
		entry.recordedAt = time.Now()
	}
	entry.terminal = true
	r.cardHandles[key] = entry
	r.mu.Unlock()
}

func (r *NotificationRouter) lookupExpiredPending(key string) expiredPendingState {
	r.mu.Lock()
	defer r.mu.Unlock()
	expired := r.expiredPending[key]
	if !expired.ExpiresAt.IsZero() && time.Now().After(expired.ExpiresAt) {
		delete(r.expiredPending, key)
		return expiredPendingState{}
	}
	return expired
}

func cardHandleKey(processKey, requestType, requestID string) string {
	return compositeKey(processKey, requestType, requestID)
}

func (r *NotificationRouter) pruneCardHandlesLocked(now time.Time) {
	for key, entry := range r.cardHandles {
		if now.Sub(entry.recordedAt) > interactiveCardHandleTTL {
			delete(r.cardHandles, key)
		}
	}
}

func (r *NotificationRouter) recordCardHandle(processKey, requestType, requestID string, handle *im.InteractiveCardHandle) {
	if strings.TrimSpace(requestID) == "" || handle == nil || (strings.TrimSpace(handle.MessageID) == "" && strings.TrimSpace(handle.Token) == "") {
		return
	}
	r.mu.Lock()
	if r.cardHandles == nil {
		r.cardHandles = make(map[string]cardHandleEntry)
	}
	now := time.Now()
	r.pruneCardHandlesLocked(now)
	key := cardHandleKey(processKey, requestType, requestID)
	// The terminal event may arrive while the original card send is in flight.
	r.cardHandles[key] = cardHandleEntry{handle: *handle, recordedAt: now, terminal: r.cardHandles[key].terminal}
	r.mu.Unlock()
}

func (r *NotificationRouter) takeCardHandle(processKey, requestType, requestID string) (im.InteractiveCardHandle, bool) {
	if strings.TrimSpace(requestID) == "" {
		return im.InteractiveCardHandle{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := cardHandleKey(processKey, requestType, requestID)
	entry, ok := r.cardHandles[key]
	if ok {
		delete(r.cardHandles, key)
		// A handle older than the card update window can no longer patch its
		// card; drop it instead of returning a handle that is guaranteed to
		// fail. Callers fall back to the callback-supplied handle, if any.
		if time.Since(entry.recordedAt) > interactiveCardHandleTTL {
			return im.InteractiveCardHandle{}, false
		}
	}
	return entry.handle, ok
}

func mergeCardHandles(primary, fallback im.InteractiveCardHandle) im.InteractiveCardHandle {
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
	if snapshot := r.lastKeyChatIDSnapshot.Load(); snapshot != nil {
		return *snapshot
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if snapshot := r.lastKeyChatIDSnapshot.Load(); snapshot != nil {
		return *snapshot
	}
	r.publishLastKeyChatIDLocked()
	return *r.lastKeyChatIDSnapshot.Load()
}

// publishLastKeyChatIDLocked publishes an immutable binding map for lock-free
// readers. Caller must hold r.mu and must not mutate the published clone.
func (r *NotificationRouter) publishLastKeyChatIDLocked() {
	snapshot := maps.Clone(r.lastKeyChatID)
	if snapshot == nil {
		snapshot = make(map[string]string)
	}
	r.lastKeyChatIDSnapshot.Store(&snapshot)
}

// findFeishuCardAdapter resolves the feishu adapter's interactive-card
// capabilities from the adapter chain; nil when feishu is absent or the
// attached adapter cannot render cards.
func (r *NotificationRouter) findFeishuCardAdapter() im.InteractiveCardAdapter {
	a := r.findAdapterByType("feishu")
	if a == nil {
		return nil
	}
	cards, _ := a.(im.InteractiveCardAdapter)
	return cards
}

// HandleIncomingMessage is the primary entry point for IM messages.
// It parses the message into a command and dispatches it.
func (r *NotificationRouter) HandleIncomingMessage(msg im.IncomingMessage) {
	if msg.IMType != "console" {
		if r.admission == nil {
			return
		}
		r.dispatchPaired(msg)
		return
	}
	r.dispatchMessage(msg)
}

func (r *NotificationRouter) dispatchMessage(msg im.IncomingMessage) {
	imType := msg.IMType
	chatID := msg.ChatID
	text := msg.Text

	cmd := parseIMCommand(text)
	if msg.InternalAction != nil {
		cmd = commandFromInternalAction(msg.InternalAction)
	}
	log.Debugf("im command parsed chatID=%v senderID=%v messageID=%v type=%v action=%v", chatID,
		msg.SenderID,
		msg.MessageID,
		cmd.Type,
		cmd.Action,
	)

	if cmd.Type == "bind" {
		r.sendTextForIM(msg.IMType, chatID, bindUsageMessage)
		return
	}

	cfg := r.getConfig()
	if cfg == nil {
		r.sendTextForIM(msg.IMType, chatID, "❌ Configuration not loaded.")
		return
	}
	ws, err := cfg.ResolveWorkspace(imType, chatID)
	if err != nil {
		log.Warnf("resolve workspace failed imType=%v chatID=%v error=%v", imType, chatID, err)
		r.sendTextForIM(msg.IMType, chatID, fmt.Sprintf("⚠️ %v", err))
		return
	}
	if ws == nil {
		log.Warnf("no workspace for chatID chatID=%v", chatID)
		r.sendTextForIM(msg.IMType, chatID, "⚠️ No workspace configured for this chat.")
		return
	}

	key := (processKey{workspaceID: ws.ID, imType: imType, chatID: chatID}).String()
	r.recordChatID(key, chatID)

	if r.rejectStaleCard(key, msg) {
		r.sendTextForIM(msg.IMType, chatID, "⚠️ This card belongs to a previous process. Use /status to get current entries.")
		return
	}
	if r.routeTransferGuard(ws, chatID, key, cmd, msg) {
		return
	}
	switch cmd.Type {
	case "takeover":
		r.handleTakeover(ws, chatID, key, cmd)
	case "new":
		r.handleNew(ws, chatID, imType)
	case "resume":
		r.handleResume(ws, chatID, cmd.SessionID, imType)
	case "sessions":
		r.handleSessions(ws, chatID, imType)
	case "todos":
		r.handleTodos(ws, chatID, imType)
	case "login":
		r.handleLogin(chatID, cmd.Content, imType)
	default:
		r.handleChordCommand(ws, chatID, cmd, imType, &msg)
	}
}

// handleNew asks the current chord process to start a fresh session when possible.
// If no usable process is already running, fall back to clearing the pin and
// starting a fresh headless process without trying to resume a pinned session first.
func (r *NotificationRouter) handleNew(ws *config.Workspace, chatID, imType string) {
	if r.mgr == nil {
		r.sendTextForIM(imType, chatID, "❌ Process manager not available.")
		return
	}
	key := (processKey{workspaceID: ws.ID, imType: imType, chatID: chatID}).String()
	proc := r.mgr.GetProcessForKey(key)
	if proc != nil && (proc.Alive() || proc.cmd == nil && proc.stdin != nil) {
		if err := proc.SendCommand(map[string]any{"type": "send", "content": "/new"}); err == nil {
			r.sendTextForIM(imType, chatID, "🆕 /new sent to chord process.")
			return
		} else if errors.Is(err, errInputStatusUnknown) || errors.Is(err, errUnresolvedInputs) {
			r.sendTextForKey(key, chatID, err.Error())
			return
		} else {
			log.Warnf("failed to send /new to chord process key=%v error=%v", key, err)
		}
	}
	r.respawnSession(ws, chatID, imType, "")
}

// handleResume stops the current process and spawns one with --resume.
func (r *NotificationRouter) handleResume(ws *config.Workspace, chatID, sessionID string, imType string) {
	r.respawnSession(ws, chatID, imType, sessionID)
}

func (r *NotificationRouter) respawnSession(ws *config.Workspace, chatID, imType, sessionID string) {
	if r.mgr == nil {
		r.sendTextForIM(imType, chatID, "❌ Process manager not available.")
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	isResume := sessionID != ""

	key := (processKey{workspaceID: ws.ID, imType: imType, chatID: chatID}).String()
	if proc := r.mgr.GetProcessForKey(key); proc != nil && proc.Alive() && proc.State().Busy {
		r.sendTextForIM(imType, chatID, "⚠️ The process is busy. Send /cancel before changing the session.")
		return
	}
	proc, err := r.mgr.replaceOrdinarySession(key, sessionID)
	if errors.Is(err, errUnresolvedInputs) || errors.Is(err, errInputStatusUnknown) {
		r.sendTextForKey(key, chatID, err.Error())
		return
	}
	if err != nil {
		if isResume {
			log.Errorf("failed to spawn process for resume workspace=%v error=%v", ws.ID, err)
			r.sendTextForIM(imType, chatID, "❌ Failed to resume session.")
		} else {
			log.Errorf("failed to spawn new process workspace=%v error=%v", ws.ID, err)
			r.sendTextForIM(imType, chatID, "❌ Failed to start new session.")
		}
		return
	}
	if proc == nil {
		r.sendTextForIM(imType, chatID, "❌ Workspace not configured.")
		return
	}
	if isResume {
		if !r.resumeProcessAvailable(proc, sessionID) {
			r.mgr.discardFailedResume(key, proc, sessionID)
			log.Warnf("resume session failed after spawn key=%v session_id=%v", key, sessionID)
			r.sendTextForIM(imType, chatID, fmt.Sprintf("❌ Failed to resume session %s. It may not exist or may be busy.", sessionID))
			return
		}
		r.sendTextForIM(imType, chatID, fmt.Sprintf("🔄 Resuming session %s", sessionID))
	} else {
		r.sendTextForIM(imType, chatID, "🆕 New session started.")
	}
}

func (r *NotificationRouter) resumeProcessAvailable(proc *ChordProcess, sessionID string) bool {
	deadline := time.Now().Add(2 * time.Second)
	for {
		if proc == nil || !proc.Alive() {
			return false
		}
		state := proc.State()
		if strings.TrimSpace(state.SessionID) == strings.TrimSpace(sessionID) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// handleTodos shows the current todo list.
func (r *NotificationRouter) handleTodos(ws *config.Workspace, chatID string, imType string) {
	key := (processKey{workspaceID: ws.ID, imType: imType, chatID: chatID}).String()
	proc, err := r.mgr.getOrSpawnForKeyChatLocked(key)
	if err != nil {
		r.sendTextForIM(imType, chatID, "❌ Failed to connect to chord process.")
		return
	}
	if proc == nil || !proc.Alive() {
		r.sendTextForIM(imType, chatID, "⏸️ No active session.")
		return
	}
	state := proc.State()
	r.sendTextForIM(imType, chatID, formatTodoList(state.Todos))
}

// handleLogin initiates a login flow for the specified IM adapter.
// If no target is specified, lists the adapters that support login.
func (r *NotificationRouter) handleLogin(chatID, target string, platform ...string) {
	text, _ := r.loginReply(target)
	r.sendTextForIM(r.replyPlatform(platform), chatID, text)
}

func (r *NotificationRouter) availableLoginTargets() []string {
	var targets []string
	seen := make(map[string]bool)
	for _, name := range []string{"wechat", "feishu", "console"} {
		adapter := r.findAdapterByType(name)
		if adapter == nil {
			continue
		}
		if !adapter.SupportsLoginRenewal() {
			continue
		}
		cmdName := config.NormalizeIMType(name)
		if !seen[cmdName] {
			seen[cmdName] = true
			targets = append(targets, cmdName)
		}
	}
	return targets
}

// findAdapterByType finds an adapter by type name.
func (r *NotificationRouter) findAdapterByType(name string) im.IMAdapter {
	a := r.currentAdapter()
	if a == nil {
		return nil
	}
	normalizedName := config.NormalizeIMType(name)
	if multi, ok := a.(*MultiAdapter); ok {
		return multi.FindAdapterByType(normalizedName)
	}
	if a.Type() == normalizedName {
		return a
	}
	return nil
}

// HandleChordEvent is the entry point for chord events.
// It decides whether to push a notification to the IM user.
func (r *NotificationRouter) HandleChordEvent(key, eventType string, state ControlState) {
	target, targetOK := r.targetForKey(key)
	if !targetOK {
		return
	}
	sendCtx := context.Background()
	if target.Lease != nil {
		sendCtx = target.Lease.Context()
	}
	if state.BindingInstanceID != "" {
		p := r.mgr.GetProcessForKey(key)
		if p == nil || p.BindingInstanceID() != state.BindingInstanceID {
			return
		}
	}
	if eventType == "confirm_resolved" && target.Identity.Platform == "feishu" && state.LastConfirmResolved != nil &&
		(state.LastConfirmResolved.Outcome == ConfirmOutcomeTimeout || state.LastConfirmResolved.Outcome == ConfirmOutcomeCancelled) {
		r.markConfirmCardTerminal(key, state.LastConfirmResolved.RequestID)
	}
	if strings.HasPrefix(eventType, "session_transfer_") {
		r.stopReminder(key)
	}

	if eventType == "session_switched" || eventType == "exit" {
		r.mu.Lock()
		delete(r.shownQuestions, key)
		r.mu.Unlock()
	}
	if eventType == "question_request" || eventType == "question_updated" {
		q := state.questionForNotification()
		if q == nil {
			return
		}
		if eventType == "question_updated" && q.Outcome != "" {
			if q.RevisionResult != nil {
				eventType = "question_revision"
			} else {
				return
			}
		}
		// IM does not expose the start of text entry. Disable the Core timer before
		// opening this answer channel; only the committed update permits display.
		if q.Outcome == "" && (q.Timer == "armed" || q.Timer == "awaiting_presentation" || q.Timer == "suspended") {
			if r.mgr != nil {
				if proc := r.mgr.GetProcessForKey(key); proc != nil {
					if err := proc.SendCommand(map[string]any{"type": "question", "action": "presented", "operation_id": fmt.Sprintf("gateway:presented:%s:%d", q.RequestID, q.Version), "request_id": q.RequestID, "version": q.Version, "binding_id": q.BindingID, "supports_interaction": false}); err != nil {
						log.Warnf("failed to disable IM question timer error=%v", err)
						state.LastError = "Could not open question " + q.RequestID + ". Check /status to retry."
						r.HandleChordEvent(key, "question_presentation_error", state)
					}
				}
			}
			return
		}
		if eventType == "question_updated" {
			eventType = "question_request"
		}
	}

	if state.Busy && (eventType == "activity" || eventType == "input_result" && state.LastInputResult != nil && state.LastInputResult.Status == inputStarted) {
		r.scheduleReminder(key, state.LastPushAt)
	}
	// The expired-pending mirror follows what chord still qualifies for a late
	// answer: a pending dialog supersedes the old expiry, and a state that
	// reports no expired interaction at all means chord dropped the
	// qualification (the answer arrived, a newer question retired it, the
	// session switched, or the process was replaced). Clearing on that signal
	// keeps the record from being refreshed forever by every push and from
	// outliving the process state it mirrors.
	switch {
	case state.pendingQuestionCount() > 0 || state.PendingConfirm != nil || state.PendingHandoff != nil:
		r.clearExpiredPending(key)
	case state.ExpiredConfirm == nil && state.ExpiredHandoff == nil:
		r.clearExpiredPending(key)
	default:
		r.recordExpiredPending(key, state)
	}
	workspaceID, imType, chatID := parseProcessKey(key)
	if workspaceID == "" || imType == "" || chatID == "" {
		log.Warnf("gateway event has invalid process key event=%v key=%v", eventType, key)
		return
	}

	log.Infof("[%v] gateway routing event event=%v busy=%v phase=%v last_outcome=%v assistant_text_len=%v", processLogContext(key, state),
		eventType,
		state.Busy,
		state.Phase,
		state.LastOutcome,
		len(state.LastAssistantText),
	)

	msg := r.formatNotification(eventType, state, func(command string) string {
		return r.commandHint(imType, im.CommandTargetForChat(imType, chatID), command)
	})
	willSend := msg != ""
	msgLen := len(msg)
	// A transfer republishes its status on every blocker change. Report the
	// progression instead of every event, so one transfer does not become a
	// stream of near-identical messages.
	if willSend && strings.HasPrefix(eventType, "session_transfer_") {
		willSend = r.transferEventVisible(key, state.Transfer, msg, time.Now())
		if !willSend {
			log.Debugf("[%v] transfer notification suppressed as already reported event=%v", processLogContext(key, state), eventType)
		}
	}
	log.Infof("[%v] gateway routing decision event=%v assistant_text_len=%v message_len=%v will_send=%v", processLogContext(key, state),
		eventType,
		len(state.LastAssistantText),
		msgLen,
		willSend,
	)
	if msg == "" {
		if !state.Busy && (eventType == "idle" || eventType == "idle_timeout" || eventType == "exit") {
			r.stopReminder(key)
		}
		return
	}
	if !willSend {
		return
	}

	task := func() {
		if !r.validTarget(target) {
			return
		}
		if state.BindingInstanceID != "" {
			p := r.mgr.GetProcessForKey(key)
			if p == nil || p.BindingInstanceID() != state.BindingInstanceID {
				return
			}
		}
		r.deliverChordEvent(target, eventType, state, msg)
	}
	if r.outbound != nil {
		result := r.outbound.enqueueOrWaitContext(sendCtx, key, task)
		switch result {
		case outboundQueued:
			return
		case outboundClosed:
			log.Debugf("[%v] outbound notification dispatcher closed, dropping event=%v", processLogContext(key, state), eventType)
			return
		}
	}
	task()
}

func (r *NotificationRouter) deliverChordEvent(target im.DeliveryTarget, eventType string, state ControlState, msg string) {
	key, imType := target.ProcessKey, target.Identity.Platform
	if eventType == "notification" && r.duplicatePromptNotification(key, state) {
		return
	}
	delivered := false
	questionID := ""
	defer func() {
		if delivered {
			r.recordDeliveredPrompt(key, eventType, state)
		}
		if questionID != "" && !delivered {
			r.mu.Lock()
			delete(r.shownQuestions[key], questionID)
			r.mu.Unlock()
		}
	}()
	if eventType == "question_request" {
		q := state.questionForNotification()
		if q == nil {
			return
		}
		r.mu.Lock()
		if r.shownQuestions == nil {
			r.shownQuestions = map[string]map[string]bool{}
		}
		if r.shownQuestions[key] == nil {
			r.shownQuestions[key] = map[string]bool{}
		}
		shown := r.shownQuestions[key][q.RequestID]
		if !shown {
			r.shownQuestions[key][q.RequestID] = true
		}
		r.mu.Unlock()
		if shown {
			return
		}
		questionID = q.RequestID
	}

	if imType == "feishu" {
		if eventType == "confirm_request" {
			if handled, err := r.sendFeishuConfirmCard(target, state); handled {
				delivered = err == nil
				if delivered {
					r.markVisibleOutput(key)
				} else {
					r.reportFeishuCardFailure(target, err)
				}
				return
			}
		}
		if eventType == "question_request" {
			if handled, err := r.sendFeishuQuestionCard(target, state); handled {
				delivered = err == nil
				if delivered {
					r.markVisibleOutput(key)
				} else {
					r.reportFeishuCardFailure(target, err)
				}
				return
			}
		}
		if eventType == "confirm_resolved" {
			if updated, err := r.resolveConfirmCard(target, state); updated || errors.Is(err, im.ErrDeliveryOutcomeUnknown) {
				if updated {
					r.markVisibleOutput(key)
				}
				return
			}
		}
		if eventType == "question_resolved" {
			if updated, err := r.resolveQuestionCard(target, state); updated || errors.Is(err, im.ErrDeliveryOutcomeUnknown) {
				if updated {
					r.markVisibleOutput(key)
				}
				return
			}
		}
	}

	delivered = r.SendBound(context.Background(), target, msg) == nil
	if delivered {
		r.markVisibleOutput(key)
	}
}

// resolveConfirmCard greys out a delivered confirmation card whose wait chord
// settled without an answer. Answered resolutions return without patching: the
// answering interaction already resolved the card itself. Like the question
// variant, an uncertain patch must not trigger the text fallback, and a
// text-delivered confirmation (no stored handle) falls back to the text notice.
func (r *NotificationRouter) resolveConfirmCard(target im.DeliveryTarget, state ControlState) (bool, error) {
	p := state.LastConfirmResolved
	if p == nil || (p.Outcome != ConfirmOutcomeTimeout && p.Outcome != ConfirmOutcomeCancelled) {
		return false, nil
	}
	title, message := "Confirmation expired", "⌛ The pending confirmation has expired."
	if p.Outcome == ConfirmOutcomeCancelled {
		title, message = "Confirmation cancelled", "⌛ The pending confirmation was cancelled."
	}
	message += "\nRequest: " + p.RequestID
	return r.patchFeishuCardForTargetContext(context.Background(), im.IncomingMessage{IMType: "feishu"}, target, "confirm", p.RequestID, buildFeishuResolvedCard(title, message, "grey"), true)
}

// resolveQuestionCard patches the interactive question card for a request that
// chord just closed. It reports whether a card was actually updated; when the
// question was delivered as text (no stored handle) the caller falls back to
// sending the close notification as text. An uncertain patch must not trigger
// that fallback, because the original card may already have been updated.
func (r *NotificationRouter) resolveQuestionCard(target im.DeliveryTarget, state ControlState) (bool, error) {
	p := state.LastQuestionResolved
	if p == nil {
		return false, nil
	}
	title, message, template := questionResolvedCardStatus(p.Outcome)
	if p.Outcome == QuestionResolvedDefaulted || p.Outcome == QuestionResolvedAnswered {
		message += "\n" + strings.Join(p.Selected, ", ")
	}
	message += "\nQuestion: " + p.RequestID
	return r.patchFeishuCardForTargetContext(context.Background(), im.IncomingMessage{IMType: "feishu"}, target, "question", p.RequestID, buildFeishuResolvedCard(title, message, template), true)
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
		if imCfg := cfg.IMConfigByType("feishu"); imCfg != nil && imCfg.Feishu != nil {
			for chatID, boundWorkspaceID := range imCfg.Feishu.ChatBindings {
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
	adapterType = config.NormalizeIMType(adapterType)
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
		if imCfg := cfg.IMConfigByType("feishu"); imCfg != nil && imCfg.Feishu != nil {
			if _, ok := imCfg.Feishu.ChatBindings[chatID]; ok {
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
	p := r.mgr.GetProcessForKey(key)
	return p, p != nil
}

// sendText sends a text message via the IM adapter, logging errors.
func (r *NotificationRouter) sendText(chatID, text string) bool {
	a := r.currentAdapter()
	if a == nil {
		log.Debugf("adapter not set, skipping notification chatID=%v", chatID)
		return false
	}
	log.Infof("gateway sending notification chatID=%v text_len=%v", chatID, len(text))
	if err := a.SendText(chatID, text); err != nil {
		log.Errorf("failed to send notification chatID=%v text_len=%v error=%v", chatID, len(text), err)
		return false
	}
	return true
}

// HandleSessionExpired is called when an IM adapter detects its session has
// expired (e.g. WeChat iLink errcode=-14). It broadcasts a notification to
// all OTHER adapters so the user can take action.
func (r *NotificationRouter) HandleSessionExpired(imType string) {
	msg := fmt.Sprintf("⚠️ %s session expired: send /login %s to renew", imDisplayName(imType), config.NormalizeIMType(imType))
	if config.NormalizeIMType(imType) == "feishu" {
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
		r.broadcastExcept(imType, fmt.Sprintf("❌ %s login renewal failed. Check the local gateway.", imDisplayName(imType)))
	}
}

// broadcastExcept sends a message through all adapters EXCEPT the one of the
// given type. Used for cross-IM notifications.
func (r *NotificationRouter) broadcastExcept(excludeType string, text string) {
	if cfg := r.getConfig(); cfg != nil {
		for _, imCfg := range cfg.IMs {
			if imCfg.Type() == excludeType {
				continue
			}
			for chat, ws := range imCfg.Bindings() {
				if r.admission != nil {
					r.ensureOwnedGate(imCfg.Type(), chat, ws)
				}
				r.sendTextForIM(imCfg.Type(), chat, text)
			}
		}
	}
}

// imDisplayName returns a human-friendly name for an IM type.
func imDisplayName(imType string) string {
	switch config.NormalizeIMType(imType) {
	case "wechat":
		return "WeChat"
	case "feishu":
		return "Feishu"
	case "telegram":
		return "Telegram"
	case "discord":
		return "Discord"
	case "wecom":
		return "WeCom"
	case "dingtalk":
		return "DingTalk"
	case "slack":
		return "Slack"
	default:
		return imType
	}
}

// parseIMCommand parses a user's text message into an im.IMCommand.

func (r *NotificationRouter) replyPlatform(platform []string) string {
	if len(platform) > 0 {
		return platform[0]
	}
	if a := r.currentAdapter(); a != nil {
		return a.Type()
	}
	return ""
}
