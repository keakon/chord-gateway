package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/internal/im"
)

// DispatchResult describes evidence observed by the gateway, never IM delivery.
type DispatchResult struct {
	State  im.IngressState
	Reason string
}

var errIngressTargetExpired = errors.New("control target expired")

// CaptureIngress is the synchronous capture path for transports that have
// already read a complete batch and can wait while freezing its controls.
func (r *NotificationRouter) CaptureIngress(msg im.IncomingMessage) (string, *im.IngressControlTarget) {
	kind, target, _ := r.captureIngress(msg, false)
	return kind, target
}

// tryCaptureIngress never waits on locks shared with stdin or persistence.
// Failure freezes a refusal; callers must not recapture newer authority later.
func (r *NotificationRouter) tryCaptureIngress(msg im.IncomingMessage) (string, *im.IngressControlTarget, bool) {
	return r.captureIngress(msg, true)
}

func (r *NotificationRouter) captureIngress(msg im.IncomingMessage, nonblocking bool) (string, *im.IngressControlTarget, bool) {
	cmd := commandForIncomingMessage(msg)
	kind := "control"
	if msg.InternalAction == nil && cmd.Type == "send" && (msg.ForceText || !strings.HasPrefix(strings.TrimSpace(msg.Text), "/")) {
		kind = "task"
	}
	if r.mgr == nil {
		return kind, nil, true
	}
	ws, err := r.getConfig().ResolveWorkspace(msg.IMType, msg.ChatID)
	if err != nil || ws == nil {
		return kind, nil, true
	}
	key := (processKey{workspaceID: ws.ID, imType: msg.IMType, chatID: msg.ChatID}).String()
	var transferID string
	if cmd.Type == "takeover" && r.mgr.transfers != nil {
		store := r.mgr.transfers
		if nonblocking {
			if !store.mu.TryLock() {
				return "control", nil, false
			}
		} else {
			store.mu.Lock()
		}
		if transfer, exists := store.entries[key]; store.loadErr == nil && exists {
			transferID = transfer.Status.TransferID
		}
		store.mu.Unlock()
	}
	if nonblocking {
		if !r.mgr.mu.TryRLock() {
			return "control", nil, false
		}
	} else {
		r.mgr.mu.RLock()
	}
	p := r.mgr.procs[key]
	r.mgr.mu.RUnlock()
	if p == nil {
		if cmd.Type == "takeover" {
			return kind, &im.IngressControlTarget{TransferID: transferID}, true
		}
		target := &im.IngressControlTarget{}
		if r.mgr.pins != nil {
			pins := r.mgr.pins
			if nonblocking {
				if !pins.mu.TryLock() {
					return "control", nil, false
				}
			} else {
				pins.mu.Lock()
			}
			target.SessionID = pins.pins[pinKeyForBinding(key)]
			pins.mu.Unlock()
		}
		if cmd.Type == "question" && cmd.RequestID != "" && msg.InternalAction == nil {
			target.RequestID, target.QuestionLookup = cmd.RequestID, true
		}
		if kind == "control" {
			return kind, target, true
		}
		return kind, nil, true
	}
	if nonblocking {
		if !p.mu.TryRLock() {
			return "control", nil, false
		}
	} else {
		p.mu.RLock()
	}
	defer p.mu.RUnlock()
	s := p.state
	target := &im.IngressControlTarget{RuntimeID: p.runtimeID, SessionEpoch: p.sessionEpoch, SessionID: s.SessionID, BindingInstanceID: p.bindingInstanceID, TransferID: transferID}
	if cmd.Type == "takeover" && target.TransferID == "" && s.Transfer != nil {
		target.TransferID = s.Transfer.TransferID
	}
	if kind == "task" && s.PendingConfirm != nil && isDoneTool(s.PendingConfirm.ToolName) {
		kind = "control"
		target.RequestID = s.PendingConfirm.RequestID
	}
	switch cmd.Type {
	case "confirm":
		if s.PendingConfirm != nil {
			target.RequestID = s.PendingConfirm.RequestID
		}
		if cmd.RequestID != "" && cmd.RequestID != target.RequestID {
			return kind, nil, true
		}
	case "question":
		target.RequestID = cmd.RequestID
		q := s.Questions[cmd.RequestID]
		if cmd.RequestID == "" {
			q = s.uniquePendingQuestion()
		}
		if q != nil {
			target.RequestID = q.RequestID
			target.Version = q.Version
		} else if cmd.RequestID != "" && msg.InternalAction == nil {
			target.QuestionLookup = true
		}
		if msg.InternalAction != nil && msg.InternalAction.Version != 0 {
			target.Version = msg.InternalAction.Version
		}
	case "handoff":
		if s.PendingHandoff != nil {
			target.RequestID = s.PendingHandoff.RequestID
		}
	case "cancel":
		if s.Busy && !s.TurnStartedAt.IsZero() {
			target.TurnID = uint64(s.TurnStartedAt.UnixNano())
		}
	}
	if kind == "task" {
		return kind, nil, true
	}
	return kind, target, true
}

// targetMatchesLocked and the command write share p.mu, closing the final
// snapshot-to-stdin race even when stdout changes the pending interaction.
func (p *ChordProcess) ingressTargetMatchesLocked(t *im.IngressControlTarget, cmd im.IMCommand) bool {
	if t == nil || t.RuntimeID == "" || t.RuntimeID != p.runtimeID || t.SessionEpoch != p.sessionEpoch || t.SessionID != "" && t.SessionID != p.state.SessionID || t.BindingInstanceID != "" && t.BindingInstanceID != p.bindingInstanceID {
		return false
	}
	switch cmd.Type {
	case "confirm":
		return t.RequestID != "" && p.state.PendingConfirm != nil && p.state.PendingConfirm.RequestID == t.RequestID
	case "question":
		q := p.state.Questions[t.RequestID]
		return q != nil && q.Version == t.Version && !t.QuestionLookup
	case "handoff":
		return t.RequestID != "" && p.state.PendingHandoff != nil && p.state.PendingHandoff.RequestID == t.RequestID
	case "cancel":
		return t.TurnID != 0 && p.state.Busy && uint64(p.state.TurnStartedAt.UnixNano()) == t.TurnID
	}
	return true
}
func (p *ChordProcess) sendIngressControl(t *im.IngressControlTarget, cmd im.IMCommand, payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ingressTargetMatchesLocked(t, cmd) {
		return errIngressTargetExpired
	}
	if !p.commandAllowedLocked(payload) {
		return ErrSessionTransferred
	}
	if err = p.writeCommandLocked(data); err != nil {
		return err
	}
	switch cmd.Type {
	case "confirm", "handoff":
		p.state.setAgentBusy(true)
	case "local_shell":
		p.state.beginLocalShell(time.Now())
	}
	return nil
}

// DispatchAccepted executes only in-memory control parameters. It intentionally
// does not call the legacy void dispatcher or its fresh-session resend path.
func (r *NotificationRouter) DispatchAccepted(ctx context.Context, msg im.IncomingMessage, target *im.IngressControlTarget, recordIDs ...string) DispatchResult {
	reject := func(reason string) DispatchResult { return DispatchResult{"rejected", reason} }
	if msg.AdmissionTicket() == nil || r.admission == nil || !r.admission.Valid(*msg.AdmissionTicket()) {
		return reject("authorization_changed")
	}
	cmd := commandForIncomingMessage(msg)
	if cmd.Invalid {
		return reject("invalid_command")
	}
	help := !msg.ForceText && msg.InternalAction == nil && strings.TrimSpace(msg.Text) == "/help"
	if msg.AdmissionTicket().Kind == "owner_unbound" {
		text := "This chat is not bound. Send " + r.ingressCommandHint(msg, "/bind <directory>") + "."
		if help {
			text += " Use " + r.ingressCommandHint(msg, "/status") + " to check the binding."
		}
		r.enqueueIngressReply(msg, text)
		return DispatchResult{"completed", ""}
	}
	if cmd.Type == "bind" {
		args, err := parsePairedBindArgs(msg.Text)
		if err != nil {
			r.enqueueIngressReply(msg, bindUsageMessage)
			return reject("invalid_bind")
		}
		binding, err := (&BindingService{router: r}).Bind(ctx, msg, *msg.AdmissionTicket(), args)
		if err != nil {
			if binding.ConfigOutcome.Phase >= Replaced || errors.Is(err, errConfigFaulted) || errors.Is(err, errBindOutcomeUnknown) {
				return DispatchResult{"unknown", "binding_outcome_unknown"}
			}
			if text := bindingReply(binding, err); text != "" {
				r.enqueueIngressReply(msg, text)
			}
			return reject("binding_not_changed")
		}
		r.enqueueIngressReply(msg, bindingReply(binding, nil))
		return DispatchResult{"completed", ""}
	}
	gate := msg.AdmissionTicket().Gate
	gate.OpMu.Lock()
	opLocked := true
	unlockOp := func() {
		if opLocked {
			gate.OpMu.Unlock()
			opLocked = false
		}
	}
	// Long waits below must not hold the chat operation lock: it is the same
	// lock a fresh inbound acceptance takes with a two-second budget, so holding
	// it across a restore, spawn or session change would refuse the chat's own
	// next message. Release it for the wait and re-acquire before continuing.
	relockOp := func() {
		if !opLocked {
			gate.OpMu.Lock()
			opLocked = true
		}
	}
	defer func() {
		if opLocked {
			gate.OpMu.Unlock()
		}
	}()
	if !r.admission.Valid(*msg.AdmissionTicket()) {
		return reject("authorization_changed")
	}
	ws, err := r.getConfig().ResolveWorkspace(msg.IMType, msg.ChatID)
	if err != nil || ws == nil {
		return reject("chat_unbound")
	}
	key := (processKey{workspaceID: ws.ID, imType: msg.IMType, chatID: msg.ChatID}).String()
	p := r.mgr.GetProcessForKey(key)
	if msg.InternalAction != nil {
		if err := ValidateIngressInteraction(msg); err != nil || p == nil || target == nil || msg.InternalAction.BindingInstanceID != p.BindingInstanceID() || target.BindingInstanceID != msg.InternalAction.BindingInstanceID {
			return reject("interaction_target_expired")
		}
	}
	if cmd.Type == "login" {
		text, result := r.loginReply(cmd.Content, func(command string) string { return r.ingressCommandHint(msg, command) })
		r.enqueueIngressReply(msg, text)
		return result
	}
	if p == nil && (cmd.Type == "status" || cmd.Type == "todos" || cmd.Type == "role" && strings.TrimSpace(cmd.Content) == "" || cmd.Type == "question" && target != nil && target.QuestionLookup) {
		var result DispatchResult
		unlockOp()
		p, target, result = r.restoreIngressQuery(ctx, key, target)
		relockOp()
		if !r.admission.Valid(*msg.AdmissionTicket()) {
			return reject("authorization_changed")
		}
		if result.State != "" {
			return result
		}
	}
	if cmd.Type == "status" || help || cmd.Type == "todos" {
		text := "No active session."
		if help {
			commands := []string{"/status", "/todos", "/sessions", "/cancel", "/allow [request-id]", "/deny <reason>", "/answer @<question-id> <answer>", "/question-interact <id>", "/question-withdraw <id>", "/question-replace <old-id> <new-id>", "/role [name or number]", "/login [platform]", "/handoff [agent] [pool]", "/handoff-deny [reason]", "! <shell command>", "/summary", "/mcp", "/bind <directory>", "/new", "/resume <session-id>", "/takeover status"}
			for i, command := range commands {
				commands[i] = r.ingressCommandHint(msg, command)
			}
			text = strings.Join(commands, "\n")
		} else if p != nil {
			if cmd.Type == "todos" {
				text = formatTodoList(p.State().Todos)
			} else {
				state := p.State()
				unlockOp()
				wait, cancel := context.WithTimeout(ctx, 10*time.Second)
				if synchronized, syncErr := p.WaitStatus(wait); syncErr == nil {
					state = synchronized
				}
				cancel()
				relockOp()
				if !r.admission.Valid(*msg.AdmissionTicket()) {
					return reject("authorization_changed")
				}
				text = formatBindingStatus(ws, msg.IMType, msg.ChatID, state)
			}
		}
		// The shared outbound dispatcher owns network waits; completion means query generated.
		if cmd.Type == "status" {
			text += r.ingressStatus(msg)
		}
		r.enqueueIngressReply(msg, text)
		return DispatchResult{"completed", ""}
	}

	if cmd.Type == "sessions" {
		unlockOp()
		wait, cancel := context.WithTimeout(ctx, 10*time.Second)
		sessions, err := querySessionList(wait, r.getConfig(), ws.Path)
		cancel()
		relockOp()
		if !r.admission.Valid(*msg.AdmissionTicket()) {
			return reject("authorization_changed")
		}
		if err != nil {
			return reject("session_query_failed")
		}
		r.enqueueIngressReply(msg, formatSessionList(sessions))
		return DispatchResult{"completed", ""}
	}
	if cmd.Type == "takeover" {
		unlockOp()
		result := r.dispatchIngressTakeover(ctx, msg, key, cmd, target)
		relockOp()
		return result
	}
	if cmd.Type == "new" || cmd.Type == "resume" {
		unlockOp()
		result := r.dispatchIngressSession(ctx, msg, key, cmd, target)
		relockOp()
		return result
	}
	if p == nil && cmd.Type == "role" && strings.TrimSpace(cmd.Content) == "" {
		r.enqueueIngressReply(msg, "No active session. Send a task or "+r.ingressCommandHint(msg, "/new")+" to start one, then use "+r.ingressCommandHint(msg, "/role")+".")
		return DispatchResult{"completed", ""}
	}
	if p == nil && (cmd.Type == "local_shell" || cmd.Type == "role" || cmd.Type == "send") {
		var result DispatchResult
		unlockOp()
		p, target, result = r.startIngressCommandProcess(ctx, key, target)
		relockOp()
		if !r.admission.Valid(*msg.AdmissionTicket()) {
			return reject("authorization_changed")
		}
		if result.State != "" {
			return result
		}
	}
	if p == nil {
		return reject("control_target_missing")
	}
	if msg.InternalAction == nil && cmd.Type == "send" && (msg.ForceText || !strings.HasPrefix(strings.TrimSpace(msg.Text), "/")) {
		cmd = im.IMCommand{Type: "confirm", Action: "deny", Reason: msg.Text}
	}
	payload := map[string]any{"type": cmd.Type}
	switch cmd.Type {
	case "confirm":
		payload["request_id"] = targetRequestID(target)
		payload["action"] = cmd.Action
		if cmd.Action == "deny" {
			payload["deny_reason"] = cmd.Reason
			pending := p.State().PendingConfirm
			if strings.TrimSpace(cmd.Reason) == "" && pending != nil && isDoneTool(pending.ToolName) {
				return reject("done_reason_required")
			}
		}
	case "handoff":
		payload["request_id"] = targetRequestID(target)
		payload["action"] = cmd.Action
		payload["agent"] = cmd.Agent
		payload["pool"] = cmd.Pool
		payload["deny_reason"] = cmd.Reason
	case "cancel":
	case "local_shell":
		if cmd.Content == "" {
			return reject("empty_command")
		}
		payload["command"] = cmd.Content
	case "question":
		return r.dispatchIngressQuestion(ctx, msg, p, cmd, target, recordIDs...)
	case "role":
		return r.dispatchIngressRole(ctx, msg, p, cmd, target)
	case "send":
		if len(recordIDs) == 0 {
			return reject("ingress_record_required")
		}
		return r.dispatchIngressSlash(ctx, msg, p, cmd, target, recordIDs[0])
	default:
		return reject("control_requires_local_session")
	}
	if cmd.Invalid {
		return reject("invalid_command")
	}
	err = p.sendIngressControl(target, cmd, payload)
	if errors.Is(err, errIngressTargetExpired) || errors.Is(err, ErrSessionTransferred) {
		return reject("control_target_expired")
	}
	if err != nil {
		return DispatchResult{"unknown", "write_outcome_unknown"}
	}
	if cmd.Type == "confirm" {
		title, text, template := "Approval submitted", "Approval submitted by "+displaySender(msg)+". Chord acceptance is unconfirmed; check /status.", "grey"
		if cmd.Action == "deny" {
			title, text, template = "Denial submitted", "Denial submitted by "+displaySender(msg)+". Chord acceptance is unconfirmed; check /status.", "grey"
		}
		r.enqueueIngressInteractionPresentation(msg, "confirm", targetRequestID(target), title, text, template, false, target.BindingInstanceID)
	} else {
		r.enqueueIngressReply(msg, "Request submitted.")
	}
	return DispatchResult{"submitted", ""}
}
func targetRequestID(t *im.IngressControlTarget) string {
	if t == nil {
		return ""
	}
	return t.RequestID
}

func (r *NotificationRouter) dispatchIngressSession(ctx context.Context, msg im.IncomingMessage, key string, cmd im.IMCommand, target *im.IngressControlTarget) DispatchResult {
	p := r.mgr.GetProcessForKey(key)
	// /new and /resume replace the active session, so a stale or missing
	// control target must not block them. The only staleness that still applies
	// is a captured target whose RuntimeID is set but no longer matches the live
	// process (a session change between capture and dispatch): sending a
	// session change to a process that has already moved on is meaningless.
	// The spawn window — RuntimeID empty because chord has not emitted ready
	// yet — is the normal post-startup / post-upgrade state and must be allowed
	// through; replaceOrdinarySession re-spawns as needed, and the real safety
	// (transfer guard, blocked/guarded inputs, busy process) is enforced there.
	if target != nil && target.RuntimeID != "" {
		if p == nil {
			return DispatchResult{"rejected", "control_target_expired"}
		}
		identity := p.InputReceiptIdentity()
		if identity.RuntimeID != target.RuntimeID || identity.SessionEpoch != target.SessionEpoch {
			return DispatchResult{"rejected", "control_target_expired"}
		}
	}
	if cmd.Type == "resume" && strings.TrimSpace(cmd.SessionID) == "" {
		return DispatchResult{"rejected", "session_id_required"}
	}
	// A busy process is mid-turn, so replacing its session would discard work
	// in flight. Refuse before the route-change barrier on the same fast path
	// the legacy console dispatch takes; replaceOrdinarySession re-checks under
	// the key lock and reports errProcessBusy for the remaining race.
	if p != nil && p.Alive() && p.State().Busy {
		return DispatchResult{"rejected", "session_change_busy"}
	}
	finish, err := r.beginIngressRouteChange(ctx, msg.AdmissionTicket().Identity)
	if err != nil {
		return DispatchResult{"rejected", "route_change_unavailable"}
	}
	// A route left in "changing" refuses every later task with route_changing
	// and never recovers, so settle it on every exit from here.
	settle := newRouteChangeSettle(finish)
	defer func() {
		if settle.call() != nil {
			log.Warnf("session change left the route changing key=%s cmd=%s", key, cmd.Type)
		}
	}()
	p, err = r.mgr.replaceOrdinarySession(key, cmd.SessionID)
	if err != nil || p == nil {
		if settle.call() != nil {
			return DispatchResult{"unknown", "route_change_incomplete"}
		}
		if err != nil {
			log.Warnf("session change failed key=%s cmd=%s error=%v", key, cmd.Type, err)
			return DispatchResult{"rejected", sessionChangeRejectReason(err)}
		}
		return DispatchResult{"rejected", "session_change_failed"}
	}
	wait, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	identity, err := r.waitIngressReadyPin(wait, key, p, cmd.SessionID)
	if err != nil {
		return DispatchResult{"unknown", "session_change_unconfirmed"}
	}
	if err = settle.call(); err != nil {
		return DispatchResult{"unknown", "route_change_incomplete"}
	}
	verb := "Session resumed"
	if cmd.Type == "new" {
		verb = "Session created"
	}
	r.enqueueIngressReply(msg, fmt.Sprintf("%s: %s", verb, identity.SessionID))
	return DispatchResult{"completed", ""}
}

// sessionChangeRejectReason names why a session replacement was refused, so a
// busy process, a transfer in progress, unresolved inputs and a failed spawn
// stay distinguishable in diagnostics and records instead of sharing one bare
// reason. Everything unrecognized keeps the generic reason.
func sessionChangeRejectReason(err error) string {
	switch {
	case errors.Is(err, errProcessBusy):
		return "session_change_busy"
	case errors.Is(err, ErrSessionTransferred):
		return "session_transferred"
	case errors.Is(err, errUnresolvedInputs), errors.Is(err, errInputStatusUnknown):
		return "undelivered_inputs"
	default:
		return "session_change_failed"
	}
}
func (r *NotificationRouter) waitIngressReadyPin(ctx context.Context, key string, p *ChordProcess, expected string) (ProcessInputReceipt, error) {
	if p.readySignal != nil {
		select {
		case <-p.readySignal:
		case <-ctx.Done():
			return ProcessInputReceipt{}, ctx.Err()
		}
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		identity := p.InputReceiptIdentity()
		if identity.SessionID != "" && identity.RuntimeID != "" && (expected == "" || identity.SessionID == expected) && r.mgr.pins != nil && r.mgr.pins.Get(pinKeyForBinding(key)) == identity.SessionID {
			return identity, nil
		}
		select {
		case <-ctx.Done():
			return identity, ctx.Err()
		case <-tick.C:
		}
	}
}

func (r *NotificationRouter) ingressCommandHint(msg im.IncomingMessage, command string) string {
	target := im.CommandTargetForChat(msg.IMType, msg.ChatID)
	target.ChatKind = msg.ChatKind
	return r.commandHint(msg.IMType, target, command)
}
