package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/keakon/chord-gateway/config"
)

func parseTakeoverCommand(rest string) IMCommand {
	c := IMCommand{Type: "takeover"}
	fields := strings.Fields(rest)
	if len(fields) == 1 && fields[0] == "status" {
		c.Action = "status"
		return c
	}
	if len(fields) != 1 && len(fields) != 3 {
		c.Invalid = true
		return c
	}
	if len(fields) == 0 || strings.HasPrefix(fields[0], "-") || fields[0] == "status" {
		c.Invalid = true
		return c
	}
	c.SessionID = fields[0]
	c.Action = "start"
	if len(fields) == 3 {
		d, err := time.ParseDuration(fields[2])
		if fields[1] != "--timeout" || err != nil || d < 0 {
			c.Invalid = true
			return c
		}
		c.Timeout = &d
	}
	return c
}

func (r *NotificationRouter) handleTakeover(ws *config.Workspace, chat, key string, cmd IMCommand) {
	if cmd.Invalid {
		r.sendTextForKey(key, chat, "⚠️ Usage: /takeover <session-id> [--timeout <duration>] or /takeover status.")
		return
	}
	if r.mgr == nil {
		r.sendTextForKey(key, chat, "❌ Process manager not available.")
		return
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		r.sendTextForKey(key, chat, "⚠️ Session takeover requires macOS or Linux.")
		return
	}
	operation, err := r.mgr.captureChatOperation(key)
	if err != nil {
		return
	}
	if cmd.Action == "status" {
		go r.observeTakeover(ws, chat, key, false, nil, operation)
		return
	}
	m := r.mgr
	if !m.gatewayManagedSupported() {
		r.sendTextForKey(key, chat, "⚠️ The configured Chord binary does not support session takeover. Ordinary sessions remain available.")
		return
	}
	m.lifecycleGate.RLock()
	release := m.acquireKeyLock(key)
	if m.shuttingDown.Load() {
		release()
		m.lifecycleGate.RUnlock()
		r.sendTextForKey(key, chat, "⚠️ Gateway is shutting down.")
		return
	}
	if m.transferBlocked(key) {
		release()
		m.lifecycleGate.RUnlock()
		r.sendTextForKey(key, chat, "⚠️ transfer_busy. Use /takeover status for the existing transfer.")
		return
	}
	if p := m.GetProcessForKey(key); p != nil {
		if err := p.guardedInputError(); err != nil {
			release()
			m.lifecycleGate.RUnlock()
			r.sendTextForKey(key, chat, err.Error())
			return
		}
	}
	if p := m.GetProcessForKey(key); p != nil && (p.Alive() || p.cmd == nil && p.stdin != nil) {
		release()
		m.lifecycleGate.RUnlock()
		r.sendTextForKey(key, chat, "⚠️ target_not_ready: this chat already owns a Chord process. Handle that binding explicitly first.")
		return
	}
	release()
	m.lifecycleGate.RUnlock()
	go r.prepareIncomingTakeover(ws, chat, key, cmd, operation)
}

// Workspace identity comes from Chord. Resolve it without holding lifecycle
// locks, then recheck the binding before persisting the guard and contacting
// the source with a mutating start request.
func (r *NotificationRouter) prepareIncomingTakeover(ws *config.Workspace, chat, key string, cmd IMCommand, operation *chatOperation) {
	m := r.mgr
	req := takeoverRequest{Action: transferStatePreflight, TransferID: "transfer-" + rand.Text(), SessionID: cmd.SessionID, TargetInstanceID: "binding-" + rand.Text(), Workspace: ws.Path, WorkDir: ws.Path, StateDir: config.Expand(m.Config().ChordStateDir)}
	ctx, cancel := context.WithTimeout(operation.context(), 10*time.Second)
	done := make(chan struct{})
	go func() {
		select {
		case <-m.shutdownSignal():
			cancel()
		case <-done:
		}
	}()
	status, err := m.runTransferCLI(ctx, req)
	close(done)
	cancel()
	releaseChat, gateErr := operation.acquire()
	if gateErr != nil {
		return
	}
	defer releaseChat()
	if err != nil {
		r.sendTextForKey(key, chat, "⚠️ Source preflight was not accepted; no transfer was started. "+status.ErrorCode)
		return
	}
	if status.State != transferStatePreflight || status.Workspace == "" || status.SourceRuntimeID == "" || status.TransferID != req.TransferID || status.SessionID != req.SessionID || status.TargetKind != "gateway" || status.TargetInstanceID != req.TargetInstanceID {
		r.sendTextForKey(key, chat, "⚠️ Source preflight identity was not confirmed; no transfer was started.")
		return
	}
	m.lifecycleGate.RLock()
	release := m.acquireKeyLock(key)
	releaseBinding := func() { release(); m.lifecycleGate.RUnlock() }
	if m.shuttingDown.Load() {
		releaseBinding()
		r.sendTextForKey(key, chat, "⚠️ Gateway is shutting down.")
		return
	}
	if m.transferBlocked(key) {
		releaseBinding()
		r.sendTextForKey(key, chat, "⚠️ transfer_busy. Use /takeover status for the existing transfer.")
		return
	}
	if p := m.GetProcessForKey(key); p != nil {
		if err := p.guardedInputError(); err != nil {
			releaseBinding()
			r.sendTextForKey(key, chat, err.Error())
			return
		}
	}
	if p := m.GetProcessForKey(key); p != nil && (p.Alive() || p.cmd == nil && p.stdin != nil) {
		releaseBinding()
		r.sendTextForKey(key, chat, "⚠️ target_not_ready: this chat already owns a Chord process. Handle that binding explicitly first.")
		return
	}
	status.State = "waiting_source"
	status.SourceRuntimeID = ""
	status.SourceSessionEpoch = 0
	record := sessionTransfer{Status: status, Direction: "incoming"}
	err = m.transfers.update(key, &record, "")
	releaseBinding()
	if err != nil {
		r.sendTextForKey(key, chat, "❌ Could not persist the transfer guard; no transfer was started.")
		return
	}
	r.stopReminder(key)
	go r.observeTakeover(ws, chat, key, true, cmd.Timeout, operation)
	r.sendTextForKey(key, chat, "⏳ Transfer recorded: "+record.Status.TransferID+". Use /takeover status to inspect progress.")
}

func formatTransferStatus(status TransferStatus) string {
	var b strings.Builder
	// Bound individual fields so even unusually long metadata cannot displace
	// the error, wait outcome or recovery action from the notification.
	fmt.Fprintf(&b, "Session transfer %s\nSession: %s\nState: %s", truncateLine(status.TransferID, 128), truncateLine(status.SessionID, 128), truncateLine(status.State, 32))
	if status.StartedAt != "" {
		fmt.Fprintf(&b, "\nStarted: %s", truncateLine(status.StartedAt, 32))
	}
	fmt.Fprintf(&b, "\nPending inputs: %d", status.PendingInputs)
	if status.ErrorCode != "" {
		fmt.Fprintf(&b, "\n%s: %s", truncateLine(status.ErrorCode, 64), truncateLine(status.Message, 160))
	}
	if status.WaitExpired {
		b.WriteString("\nWait budget expired. Source work is not cancelled; use /takeover status.")
	}
	if status.State == "released" || status.State == "saved" {
		b.WriteString("\nSession saved. Resume explicitly if target restoration fails.")
	}
	if status.State == "ready_paused" {
		b.WriteString("\nRestored and paused. Send an explicit continuation or a new request to continue.")
	}
	if status.RecoverableAction != "" {
		fmt.Fprintf(&b, "\nNext: %s", truncateLine(status.RecoverableAction, 128))
	}
	total := max(status.BlockerCount, len(status.Blockers))
	if total > 0 {
		fmt.Fprintf(&b, "\nBlockers: %d", total)
		footer := func(remaining int) string {
			if remaining == 0 {
				return ""
			}
			return fmt.Sprintf("\n… %d more blockers; inspect full status on the host.", remaining)
		}
		used := utf8.RuneCountInString(b.String())
		shown := 0
		for _, blocker := range status.Blockers[:min(3, len(status.Blockers))] {
			row := "\nWaiting: " + truncateLine(blocker.OwnerTaskID, 32) + " " + truncateLine(blocker.Type, 32) + " — " + truncateLine(blocker.Description, 96)
			n := utf8.RuneCountInString(row)
			if used+n+utf8.RuneCountInString(footer(total-shown-1)) > maxNotificationRunes {
				break
			}
			b.WriteString(row)
			used += n
			shown++
		}
		b.WriteString(footer(total - shown))
	}
	return b.String()
}

// A wait budget owns the observer only. Each CLI subprocess is short-lived;
// ending observation never sends cancel/stop to the source or clears guards.
func (r *NotificationRouter) observeTakeover(ws *config.Workspace, chat, key string, start bool, budget *time.Duration, operation *chatOperation) {
	m := r.mgr
	began := time.Now()
	last := ""
	observe := start
	// Each bounded step owns the original gate through CLI completion and
	// persistence/restoration. Release it before sleeping between observations.
	step := func(start bool) bool {
		releaseChat, err := operation.acquire()
		if err != nil {
			return false
		}
		defer releaseChat()

		record, ok, err := m.transfers.Get(key)
		localPreparation := false
		if err != nil {
			r.sendTextForKey(key, chat, "❌ Transfer storage could not be loaded; automatic input remains blocked.")
			return false
		}
		if !ok {
			if p := m.GetProcessForKey(key); p != nil {
				state := p.State()
				if state.Transfer != nil {
					record = sessionTransfer{Status: *state.Transfer, Direction: "outgoing", BindingInstanceID: state.BindingInstanceID}
					ok = true
					localPreparation = true
				}
			}
			if !ok {
				r.sendTextForKey(key, chat, "ℹ️ No pending transfer for this binding.")
				return false
			}
		}
		// A previously started target may become ready after the caller stopped waiting.
		if record.Direction == "incoming" && record.Status.State == "restoring" {
			restoreWait := transferReadyTimeout
			if observe && budget != nil {
				restoreWait = min(restoreWait, *budget-time.Since(began))
			}
			ctx, cancel := context.WithTimeout(operation.context(), restoreWait)
			var target *ChordProcess
			target, err = m.restoreTransferredSession(ctx, key, record)
			cancel()
			if err != nil {
				r.sendTextForKey(key, chat, "⚠️ Target restore remains unconfirmed; binding is blocked.")
				return false
			}
			record.Status.State = "ready_paused"
			r.clearTransferCards(key)
			r.sendTextForKey(key, chat, formatPausedTargetStatus(record.Status, target.State()))
			return false
		}
		wait := 10 * time.Second
		if observe && budget != nil && !start {
			wait = min(wait, *budget-time.Since(began))
			if wait <= 0 {
				record.Status.WaitExpired = true
				r.sendTextForKey(key, chat, formatTransferStatus(record.Status))
				return false
			}
		}
		ctx, cancel := context.WithTimeout(operation.context(), wait)
		done := make(chan struct{})
		go func() {
			select {
			case <-m.shutdownSignal():
				cancel()
			case <-done:
			}
		}()
		action := "status"
		if start {
			action = "start"
		}
		req := takeoverRequest{Action: action, TransferID: record.Status.TransferID, SessionID: record.Status.SessionID, TargetInstanceID: gatewayTargetInstance(record), Workspace: record.Status.Workspace, WorkDir: ws.Path, StateDir: config.Expand(m.Config().ChordStateDir)}
		status, cliErr := m.runTransferCLI(ctx, req)
		close(done)
		cancel()
		if !operation.validLocked() {
			return false
		}
		if status.TransferID != "" {
			if status.TransferID != record.Status.TransferID || status.SessionID != record.Status.SessionID || status.Workspace != record.Status.Workspace || record.Status.TargetKind != status.TargetKind || record.Status.TargetInstanceID != status.TargetInstanceID || record.Status.SourceRuntimeID != "" && !sameTransferIdentity(record.Status, status) {
				r.sendTextForKey(key, chat, "⚠️ stale_source: transfer identity changed. Binding remains blocked.")
				return false
			}
			record.Status = status
			if record.Direction == "outgoing" && status.State == transferStatePrepareFailed {
				if err = m.reopenUnfrozenSource(key, status); err != nil {
					r.sendTextForKey(key, chat, "⚠️ Could not confirm open source admission. Binding remains blocked.")
				} else {
					r.sendTextForKey(key, chat, "ℹ️ Source confirmed transfer was not accepted; input routing reopened.")
				}
				return false
			}
			if record.Direction == "incoming" && (status.State == transferStatePrepareFailed || start && status.State == transferStatePreflight && cliErr != nil) {
				if err = m.transfers.update(key, nil, record.Status.TransferID); err != nil {
					r.sendTextForKey(key, chat, "❌ Source did not accept transfer; clearing the guard failed.")
				} else {
					r.sendTextForKey(key, chat, formatTransferStatus(status)+"\nℹ️ Transfer was not accepted; ordinary chat remains available.")
				}
				return false
			}
			if err = m.persistObservedTransfer(key, record, localPreparation); err != nil {
				r.sendTextForKey(key, chat, "⚠️ Transfer binding changed or status could not be persisted. The current binding was preserved.")
				return false
			}

		}
		if cliErr != nil {
			if observe && budget != nil && time.Since(began) >= *budget {
				record.Status.WaitExpired = true
				r.sendTextForKey(key, chat, formatTransferStatus(record.Status))
				return false
			}
			r.sendTextForKey(key, chat, formatTransferStatus(record.Status)+"\n⚠️ Outcome unconfirmed or rejected. Use /takeover status; do not resend start.")
			return false
		}
		if observe && budget != nil && time.Since(began) >= *budget {
			status.WaitExpired = true
			r.sendTextForKey(key, chat, formatTransferStatus(status))
			return false
		}
		if record.Direction == "incoming" && status.State == "released" {
			restoreWait := transferReadyTimeout
			if observe && budget != nil {
				restoreWait = min(restoreWait, *budget-time.Since(began))
			}
			ctx, cancel := context.WithTimeout(operation.context(), restoreWait)
			var target *ChordProcess
			target, err = m.restoreTransferredSession(ctx, key, record)
			cancel()
			if err != nil {
				r.sendTextForKey(key, chat, formatTransferStatus(status)+"\n⚠️ Target restore unconfirmed. Use /takeover status or explicitly /resume after checking the saved session.")
				return false
			}
			status.State = "ready_paused"
			r.clearTransferCards(key)
			r.sendTextForKey(key, chat, formatPausedTargetStatus(status, target.State()))
			return false
		}
		text := formatTransferStatus(status)
		if record.Interrupted {
			text += "\nTransfer interrupted by independent process termination. Binding remains blocked; verify the saved session before resuming."
		}
		if text != last {
			r.sendTextForKey(key, chat, text)
			last = text
		}
		if !observe || status.State == "save_failed" || status.State == "restore_failed" || status.State == "released" {
			return false
		}
		if budget != nil && time.Since(began) >= *budget {
			status.WaitExpired = true
			r.sendTextForKey(key, chat, formatTransferStatus(status))
			return false
		}
		return true
	}
	for step(start) {
		delay := time.Second
		if budget != nil {
			delay = min(delay, *budget-time.Since(began))
		}
		timer := time.NewTimer(max(delay, 0))
		select {
		case <-timer.C:
		case <-operation.context().Done():
			timer.Stop()
			return
		case <-m.shutdownSignal():
			timer.Stop()
			return
		}
		start = false
	}
}

func (r *NotificationRouter) clearTransferCards(key string) {
	r.stopReminder(key)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.expiredPending, key)
	delete(r.shownQuestions, key)
	delete(r.deliveredPrompts, key)
	delete(r.transferNotices, key)
	for operationKey := range r.questionOperations {
		if strings.HasPrefix(operationKey, key+"|") {
			delete(r.questionOperations, operationKey)
		}
	}
	for cardKey := range r.cardHandles {
		if strings.HasPrefix(cardKey, key+"|") {
			delete(r.cardHandles, cardKey)
		}
	}
}

func (r *NotificationRouter) routeTransferGuard(ws *config.Workspace, chat, key string, cmd IMCommand, msg IncomingMessage) bool {
	p := r.mgr.GetProcessForKey(key)
	if !r.mgr.transferBlocked(key) && (p == nil || !p.transferActive()) {
		return false
	}
	if cmd.Type == "takeover" {
		return false
	}
	if cmd.Type == "status" {
		r.handleTakeover(ws, chat, key, IMCommand{Type: "takeover", Action: "status"})
		return true
	}
	if cmd.Type == "new" || cmd.Type == "resume" {
		if cmd.Type == "resume" && cmd.SessionID == "" {
			r.sendTextForKey(key, chat, "⚠️ Use /resume <session-id>.")
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), transferReadyTimeout)
		defer cancel()
		finish := func() error { return nil }
		if r.ingressStore != nil && msg.ticket != nil {
			var err error
			finish, err = r.beginIngressRouteChange(ctx, msg.ticket.Identity)
			if err != nil {
				r.sendTextForKey(key, chat, "⚠️ Undelivered inputs block this session change. Inspect the local inbox first.")
				return true
			}
		}
		if err := r.mgr.replaceBlockedBinding(ctx, key, cmd.SessionID); err != nil {
			r.sendTextForKey(key, chat, "❌ Session change was not confirmed. The transfer guard remains in place.")
		} else if finish() != nil {
			r.sendTextForKey(key, chat, "⚠️ Session changed but the route commit is unconfirmed; inspect the local inbox.")
		} else {
			r.clearTransferCards(key)
			r.sendTextForKey(key, chat, "✅ Session loaded; binding reopened.")
		}
		return true
	}
	// A source's existing interaction can still finish naturally. Old callbacks
	// never spawn, and cannot become a late ordinary input during a transfer.
	if !transferCommandBlocked(cmd) {
		if p := r.mgr.GetProcessForKey(key); p != nil && p.transferActive() && p.Alive() {
			if cmd.Type == "cancel" {
				r.handleCancelCommand(ws, chat, key, p)
				return true
			}
			if cmd.Type == "question" {
				state := p.State()
				q := state.Questions[cmd.RequestID]
				if cmd.RequestID == "" {
					q = state.uniquePendingQuestion()
				}
				if q != nil && q.Outcome == "" {
					r.handleQuestionCommand(ws, chat, cmd, msg, key, p)
					return true
				}
			}
		}
	}
	r.sendTextForKey(key, chat, "⚠️ "+ErrSessionTransferred.Error())
	return true
}

// Cards are scoped to a runtime binding; stale approvals and late answers
// cannot turn into instructions in a replacement process.
func (r *NotificationRouter) rejectStaleCard(key string, msg IncomingMessage) bool {
	if msg.InternalAction == nil {
		return false
	}
	p := r.mgr.GetProcessForKey(key)
	if p == nil {
		return true
	}
	instance := p.BindingInstanceID()
	return instance != "" && msg.InternalAction.BindingInstanceID != instance
}
func stampCardBinding(value any, instance string) {
	switch v := value.(type) {
	case map[string]any:
		if _, ok := v["request_id"]; ok {
			v["binding_instance_id"] = instance
		}
		for _, child := range v {
			stampCardBinding(child, instance)
		}
	case []any:
		for _, child := range v {
			stampCardBinding(child, instance)
		}
	}
}

func gatewayTargetInstance(record sessionTransfer) string {
	if record.Direction == "incoming" {
		return record.Status.TargetInstanceID
	}
	return ""
}
