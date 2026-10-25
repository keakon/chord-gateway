package gateway

import (
	"context"
	"crypto/rand"
	"runtime"
	"sync"
	"time"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

// routeChangeSettle makes the finish closure from beginIngressRouteChange
// idempotent and guarantees it runs exactly once. A route left in the
// "changing" phase refuses every later task with route_changing and never
// recovers on its own, so an abandoned takeover must always settle it.
//
// settle re-derives the route identity from live configuration, so settling
// after a failure republishes whatever session and binding are actually
// current rather than the state the abandoned change was heading towards.
type routeChangeSettle struct {
	mu     sync.Mutex
	finish func() error
	done   bool
	err    error
}

func newRouteChangeSettle(finish func() error) *routeChangeSettle {
	if finish == nil {
		return nil
	}
	return &routeChangeSettle{finish: finish}
}

// call settles the change once and replays the same result afterwards, so an
// explicit finish followed by the deferred safety net is not reported twice.
func (s *routeChangeSettle) call() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.done {
		s.done = true
		s.err = s.finish()
	}
	return s.err
}

// keep raises the barrier on purpose without running the settle, for a transfer
// that is genuinely still in flight. The next explicit status observes the
// outcome; an abandoned change has no such observer, so it must settle instead.
func (s *routeChangeSettle) keep() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = true
	s.finish = nil
}

// transfer moves the settle to the next step instead of running it, so a
// transfer that reached restoration keeps the barrier raised across the handoff.
func (s *routeChangeSettle) transfer() *routeChangeSettle {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	s.done = true
	next := &routeChangeSettle{finish: s.finish}
	s.finish = nil
	return next
}

// dispatchIngressTakeover is called with the admission gate held. Unlike the
// legacy observer, it returns evidence from this exact transfer operation.
func (r *NotificationRouter) dispatchIngressTakeover(ctx context.Context, msg im.IncomingMessage, key string, cmd im.IMCommand, target *im.IngressControlTarget) DispatchResult {
	reject := func(reason string) DispatchResult { return DispatchResult{"rejected", reason} }
	unknown := func(reason string) DispatchResult { return DispatchResult{"unknown", reason} }
	m := r.mgr
	if cmd.Invalid || m == nil || (runtime.GOOS != "darwin" && runtime.GOOS != "linux") {
		return reject("invalid_takeover")
	}
	ws, err := r.getConfig().ResolveWorkspace(msg.IMType, msg.ChatID)
	if err != nil || ws == nil {
		return reject("chat_unbound")
	}
	p := m.GetProcessForKey(key)
	if target != nil && target.RuntimeID != "" {
		if p == nil {
			return reject("control_target_expired")
		}
		identity := p.InputReceiptIdentity()
		if identity.RuntimeID != target.RuntimeID || identity.SessionEpoch != target.SessionEpoch {
			return reject("control_target_expired")
		}
	} else if p != nil && ingressTakeoverProcessActive(p) {
		return reject("control_target_expired")
	}
	record, exists, err := m.transfers.Get(key)
	if err != nil {
		return reject("transfer_storage_unavailable")
	}
	localPreparation := false
	if !exists && p != nil {
		state := p.State()
		if state.Transfer != nil {
			record = sessionTransfer{Status: *state.Transfer, Direction: "outgoing", BindingInstanceID: state.BindingInstanceID}
			exists = true
			localPreparation = true
		}
	}
	if cmd.Action == "status" {
		if !exists {
			if target != nil && target.TransferID != "" {
				return reject("control_target_expired")
			}
			r.enqueueIngressReply(msg, "No pending transfer for this binding.")
			return DispatchResult{"completed", ""}
		}
		if target == nil || target.TransferID == "" || target.TransferID != record.Status.TransferID {
			return reject("control_target_expired")
		}
	} else {
		if cmd.Action != "start" || cmd.SessionID == "" {
			return reject("invalid_takeover")
		}
		if exists || p != nil && ingressTakeoverProcessActive(p) {
			return reject("target_not_ready")
		}
		if !m.gatewayManagedSupported() {
			return reject("takeover_unsupported")
		}
		req := takeoverRequest{Action: transferStatePreflight, TransferID: "transfer-" + rand.Text(), SessionID: cmd.SessionID, TargetInstanceID: "binding-" + rand.Text(), Workspace: ws.Path, WorkDir: ws.Path, StateDir: config.Expand(m.Config().ChordStateDir)}
		wait, cancel := context.WithTimeout(ctx, 10*time.Second)
		status, cliErr := m.runTransferCLI(wait, req)
		cancel()
		if cliErr != nil {
			return reject("takeover_preflight_rejected")
		}
		if status.State != transferStatePreflight || status.Workspace == "" || status.SourceRuntimeID == "" || status.TransferID != req.TransferID || status.SessionID != req.SessionID || status.TargetKind != "gateway" || status.TargetInstanceID != req.TargetInstanceID {
			return reject("takeover_preflight_identity_mismatch")
		}
		finish, e := r.beginIngressRouteChange(ctx, msg.AdmissionTicket().Identity)
		if e != nil {
			return reject("undelivered_inputs")
		}
		settle := newRouteChangeSettle(finish)
		// The barrier is raised from here on, and every later return must settle
		// it: a route left in "changing" refuses every task for this chat with
		// route_changing and never recovers on its own. A transfer that is still
		// in flight keeps the barrier through keep/transfer instead.
		defer func() {
			if settle.call() != nil {
				log.Warnf("takeover left the route changing key=%s transfer=%s", key, record.Status.TransferID)
			}
		}()
		// Capture and persist the exact source identity before the mutating request.
		status.State = "waiting_source"
		record = sessionTransfer{Status: status, Direction: "incoming"}
		m.lifecycleGate.RLock()
		release := m.acquireKeyLock(key)
		current := m.GetProcessForKey(key)
		if m.shuttingDown.Load() || m.transferBlocked(key) || current != p {
			release()
			m.lifecycleGate.RUnlock()
			if settle.call() != nil {
				return unknown("route_change_incomplete")
			}
			return reject("control_target_expired")
		}
		err = m.transfers.update(key, &record, "")
		release()
		m.lifecycleGate.RUnlock()
		if err != nil {
			return unknown("transfer_guard_commit_unknown")
		}
		r.stopReminder(key)
		return r.runIngressTakeoverStep(ctx, msg, key, ws, record, false, true, settle)
	}
	// Status is a query unless it must finish a previously released restoration.
	if record.Direction == "incoming" && (record.Status.State == "released" || record.Status.State == "restoring") {
		finish, e := r.beginIngressRouteChange(ctx, msg.AdmissionTicket().Identity)
		if e != nil {
			return reject("undelivered_inputs")
		}
		return r.restoreIngressTakeover(ctx, msg, key, record, newRouteChangeSettle(finish))
	}
	return r.runIngressTakeoverStep(ctx, msg, key, ws, record, localPreparation, false, nil)
}

func (r *NotificationRouter) runIngressTakeoverStep(ctx context.Context, msg im.IncomingMessage, key string, ws *config.Workspace, record sessionTransfer, localPreparation, start bool, settle *routeChangeSettle) (result DispatchResult) {
	// An abandoned change must never leave the route in "changing": the chat
	// would refuse every later task with route_changing and never recover.
	defer func() {
		if settle.call() != nil && result.Reason != "route_change_incomplete" {
			result = DispatchResult{"unknown", "route_change_incomplete"}
		}
	}()
	action := "status"
	if start {
		action = "start"
	}
	req := takeoverRequest{Action: action, TransferID: record.Status.TransferID, SessionID: record.Status.SessionID, TargetInstanceID: gatewayTargetInstance(record), Workspace: record.Status.Workspace, WorkDir: ws.Path, StateDir: config.Expand(r.mgr.Config().ChordStateDir)}
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	status, err := r.mgr.runTransferCLI(wait, req)
	cancel()
	if status.TransferID == "" || !sameTransferIdentity(record.Status, status) {
		return DispatchResult{"unknown", "takeover_receipt_identity_mismatch"}
	}
	record.Status = status
	if e := r.mgr.persistObservedTransfer(key, record, localPreparation); e != nil {
		return DispatchResult{"unknown", "transfer_status_commit_unknown"}
	}
	r.enqueueIngressReply(msg, formatTransferStatus(status))
	if status.State == transferStatePrepareFailed {
		if record.Direction == "incoming" {
			if e := r.mgr.transfers.update(key, nil, status.TransferID); e != nil {
				return DispatchResult{"unknown", "transfer_guard_clear_failed"}
			}
		} else if e := r.mgr.reopenUnfrozenSource(key, status); e != nil {
			return DispatchResult{"unknown", "source_reopen_unconfirmed"}
		}
		if settle.call() != nil {
			return DispatchResult{"unknown", "route_change_incomplete"}
		}
		return DispatchResult{"rejected", "takeover_not_accepted"}
	}
	if err != nil {
		return DispatchResult{"unknown", "takeover_outcome_unknown"}
	}
	if record.Direction == "incoming" && status.State == "released" {
		next := settle.transfer()
		if next == nil {
			finish, e := r.beginIngressRouteChange(ctx, msg.AdmissionTicket().Identity)
			if e != nil {
				return DispatchResult{"rejected", "undelivered_inputs"}
			}
			next = newRouteChangeSettle(finish)
		}
		return r.restoreIngressTakeover(ctx, msg, key, record, next)
	}
	// A nonterminal transfer keeps the route changing until an explicit status
	// observes restoration, preventing input from entering the previous pin.
	if start {
		settle.keep()
		return DispatchResult{"submitted", "transfer_pending"}
	}
	return DispatchResult{"completed", ""}
}

func (r *NotificationRouter) restoreIngressTakeover(ctx context.Context, msg im.IncomingMessage, key string, record sessionTransfer, settle *routeChangeSettle) (result DispatchResult) {
	// Restoration failing must still settle the route, or the chat keeps
	// refusing every later task with route_changing.
	defer func() {
		if settle.call() != nil && result.Reason != "route_change_incomplete" {
			result = DispatchResult{"unknown", "route_change_incomplete"}
		}
	}()
	wait, cancel := context.WithTimeout(ctx, transferReadyTimeout)
	defer cancel()
	p, err := r.mgr.restoreTransferredSession(wait, key, record)
	if err != nil {
		return DispatchResult{"unknown", "target_restore_unconfirmed"}
	}
	if _, err = r.waitIngressReadyPin(wait, key, p, record.Status.SessionID); err != nil {
		return DispatchResult{"unknown", "target_pin_unconfirmed"}
	}
	if err = settle.call(); err != nil {
		return DispatchResult{"unknown", "route_change_incomplete"}
	}
	r.clearTransferCards(key)
	record.Status.State = "ready_paused"
	r.enqueueIngressReply(msg, formatPausedTargetStatus(record.Status, p.State()))
	return DispatchResult{"completed", ""}
}

func ingressTakeoverProcessActive(p *ChordProcess) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.stdin != nil || p.cmd != nil && p.cmd.Process != nil && !p.exited
}
