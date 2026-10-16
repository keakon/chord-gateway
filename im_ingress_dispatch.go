package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/keakon/golog/log"
)

type ingressControl struct {
	message IncomingMessage
	target  *IngressControlTarget
}

// IngressService owns bounded dispatch concurrency. IngressStore owns process-local
// ordering and receipts; controls retain transient command parameters.
type IngressService struct {
	store            *IngressStore
	router           *NotificationRouter
	mu               sync.Mutex
	prepared         map[string]bool
	controls         map[string]ingressControl
	accepting        map[ChatIdentity]int
	active           map[ChatIdentity]bool
	blocked          map[ChatIdentity]ingressBlocked
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	wake             chan struct{}
	slots            chan struct{}
	started          bool
	stopping         bool
	normalizer       *IngressNormalizer
	requests         sync.WaitGroup
	questionReceipts chan ingressQuestionReceipt
}

func NewIngressService(store *IngressStore, r *NotificationRouter) *IngressService {
	r.ingressStore = store
	service := &IngressService{store: store, router: r, prepared: make(map[string]bool), accepting: make(map[ChatIdentity]int), controls: make(map[string]ingressControl), active: make(map[ChatIdentity]bool), blocked: make(map[ChatIdentity]ingressBlocked), wake: make(chan struct{}, 1), slots: make(chan struct{}, 8)}
	r.ingress = service
	return service
}

// ingressBlockedWindow bounds how long a chat is held after a terminal outcome
// could not be recorded. The store can refuse the write when the record already
// expired, so an unbounded hold would strand every later task for that chat.
const ingressBlockedWindow = 30 * time.Second

// ingressStallDeadline bounds how long a record may stay in dispatching or
// unknown. The receipt observer reports its own timeout at 30s but never
// settles the record, so without a deadline one unconfirmed delivery holds
// its chat's barrier and its pending budget until the process restarts.
// The deadline is variable so tests do not wait out the real window.
var ingressStallDeadline = 5 * time.Minute

type ingressBlocked struct {
	since  time.Time
	reason string
}

func (s *IngressService) blockRoute(id ChatIdentity, reason string) {
	s.mu.Lock()
	s.blocked[id] = ingressBlocked{since: time.Now(), reason: reason}
	s.mu.Unlock()
	log.Warnf("ingress: chat blocked after an unrecorded outcome platform=%v chat=%v reason=%v", id.Platform, id.ChatID, reason)
	s.router.ObserveIngress(id.Platform, "blocked", reason)
}

// routeBlockedLocked reports whether a chat is held and lazily clears the hold
// once its bounded window passes. Callers must hold s.mu.
func (s *IngressService) routeBlockedLocked(id ChatIdentity) bool {
	block, ok := s.blocked[id]
	if !ok {
		return false
	}
	if time.Since(block.since) >= ingressBlockedWindow {
		delete(s.blocked, id)
		return false
	}
	return true
}
func (s *IngressService) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopping {
		return errors.New("ingress already started")
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.started = true
	s.normalizer = NewIngressNormalizer(s.ctx, s)
	s.questionReceipts = make(chan ingressQuestionReceipt, 128)
	s.wg.Add(1)
	go s.runQuestionReceipts()
	s.wg.Add(1)
	go s.run()
	return nil
}
func (s *IngressService) Close() {
	s.mu.Lock()
	s.stopping = true
	if s.cancel != nil {
		s.cancel()
	}
	normalizer := s.normalizer
	s.mu.Unlock()
	s.router.detachIngressReadinessNotifier(s.router.currentAdapter())
	if normalizer != nil {
		normalizer.Close()
	}
	s.requests.Wait()
	s.wg.Wait()
}

// NormalizationSnapshot reports bounded normalizer queue use for metrics.
func (s *IngressService) NormalizationSnapshot() (IngressNormalizationSnapshot, bool) {
	s.mu.Lock()
	normalizer := s.normalizer
	s.mu.Unlock()
	if normalizer == nil {
		return IngressNormalizationSnapshot{}, false
	}
	return normalizer.Snapshot(), true
}
func (s *IngressService) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *IngressService) Accept(ctx context.Context, e IngressEnvelope) ReceiveResult {
	ctx, cancel := context.WithTimeout(ctx, ingressAcceptBudget)
	defer cancel()
	s.mu.Lock()
	ready := s.started && !s.stopping && s.ctx.Err() == nil
	if ready {
		s.requests.Add(1)
	}
	s.mu.Unlock()
	if !ready {
		return ReceiveResult{Disposition: ReceiveRetryable, Reason: "ingress_unavailable"}
	}
	defer s.requests.Done()
	e = s.router.CaptureIngressEnvelope(e)
	if e.capture.err != nil {
		return ReceiveResult{Disposition: ReceiveRetryable, Reason: "target_busy"}
	}
	msg := e.Message
	if validateIngressCursor(msg.IMType, e.Cursor) != nil {
		return ReceiveResult{Disposition: ReceiveRejected, Reason: "cursor_platform_invalid"}
	}
	if msg.AccountID == "" || msg.MessageID == "" || msg.ChatID == "" {
		return ReceiveResult{Disposition: ReceiveRejected, Reason: "invalid_identity"}
	}
	invalidInteraction := ValidateIngressInteraction(msg) != nil
	invalidInput := len(msg.Text) > 64<<10 || !utf8.ValidString(msg.Text)
	var transportTicket *AdmissionTicket
	valid := func() bool {
		return (e.preparation == nil || e.preparation.valid == nil || e.preparation.valid()) && (transportTicket == nil || s.router.admission.Valid(*transportTicket))
	}
	if !valid() {
		return ReceiveResult{Disposition: ReceiveRetryable, Reason: "transport_identity_changed"}
	}
	_, duplicateErr := s.store.lookupCommittedBusiness(msg.IMType, msg.AccountID, msg.MessageID)
	// Rechecking a duplicate must not spend a second pairing attempt.
	a := s.router.admission.authorizeClassified(msg, duplicateErr != nil)
	if a.Disposition == AdmissionUnavailable {
		return ReceiveResult{Disposition: ReceiveRetryable, Reason: a.Reason}
	}
	if a.Disposition == AdmissionAllowed {
		if err := s.PrepareAccount(ctx, msg.IMType, msg.AccountID); err != nil {
			return ReceiveResult{Disposition: ReceiveRetryable, Reason: "ingress_unavailable"}
		}
	}
	msg.ticket = &a.Ticket
	if e.preparation != nil && (a.Disposition == AdmissionAllowed || a.Ticket.Kind == "owner_unbound" || strings.HasPrefix(a.Ticket.Kind, "pairing_hint")) {
		transportTicket = &a.Ticket
	}
	prepare := func() bool {
		if !valid() {
			return false
		}
		return e.preparation == nil || e.preparation.prepare == nil || e.preparation.prepare(a)
	}
	rec := IngressRecord{Platform: msg.IMType, AccountID: msg.AccountID, ChatID: msg.ChatID, SenderID: msg.SenderID, BusinessID: msg.MessageID, SemanticID: e.SemanticID, DeliveryID: e.DeliveryID, InputDigest: e.InputDigest, SourceTime: e.SourceTime, ReceivedAt: e.ReceivedAt, Kind: "task", State: "received"}
	if rec.ReceivedAt.IsZero() {
		rec.ReceivedAt = time.Now()
	}
	if invalidInput {
		rec.State, rec.Reason = "rejected", "invalid_input"
		return s.store.acceptRecord(ctx, rec, e.Cursor, valid)
	}
	if invalidInteraction {
		// A permanently rejected callback with a verified business identity
		// needs the same atomic deduplication/progress update as other refusals.
		rec.Kind, rec.State, rec.Reason = "control", "rejected", "invalid_interaction"
		return s.store.acceptRecord(ctx, rec, e.Cursor, valid)
	}
	if a.Disposition == AdmissionRejected {
		if (a.Ticket.Kind == "owner_unbound" || strings.HasPrefix(a.Ticket.Kind, "pairing_hint")) && !prepare() {
			// The bounded refusal reply still needs the transport's inbound
			// reply context; if the snapshot expired, retry instead of dropping.
			return ReceiveResult{Disposition: ReceiveRetryable, Reason: "transport_identity_changed"}
		}
		rec.State = "rejected"
		rec.Reason = a.Reason
		result := s.store.acceptRecord(ctx, rec, e.Cursor, valid)
		if a.Ticket.Kind == "owner_unbound" && (result.Disposition == ReceiveRejected || result.Disposition == ReceiveDuplicate) {
			s.router.enqueueIngressReply(msg, "This chat is not bound. Send "+s.router.ingressCommandHint(msg, "/bind <directory>")+".")
		}
		if strings.HasPrefix(a.Ticket.Kind, "pairing_hint") && result.Disposition == ReceiveRejected {
			// Only fresh messages reach here: duplicates never re-hint.
			s.router.enqueueIngressReply(msg, pairingHintReply(s.router, msg, a.Ticket.Kind))
		}
		return result
	}
	// A socket reader that could not freeze the original state must never
	// acquire a replacement control target on a later worker. Record a refusal
	// through normal business deduplication, without retaining the body.
	if e.capture != nil && e.capture.unavailable {
		rec.Kind, rec.State, rec.Reason = "control", "rejected", "control_target_unavailable"
		return s.store.acceptRecord(ctx, rec, e.Cursor, valid)
	}
	gate := a.Ticket.gate
	if err := lockIngressMutex(ctx, &gate.opMu); err != nil {
		return ReceiveResult{Disposition: ReceiveRetryable, Reason: "route_busy"}
	}
	if !s.router.admission.Valid(a.Ticket) {
		gate.opMu.Unlock()
		return ReceiveResult{Disposition: ReceiveRetryable, Reason: "authorization_changed"}
	}
	if !prepare() {
		gate.opMu.Unlock()
		return ReceiveResult{Disposition: ReceiveRetryable, Reason: "transport_identity_changed"}
	}
	rec.Kind, rec.ControlTarget = e.capture.kind, e.capture.target
	digest, workspace, session, err := s.router.ingressRouteIdentity(rec.Identity())
	if err != nil && a.Ticket.Kind != "paired_bind" {
		gate.opMu.Unlock()
		return ReceiveResult{Disposition: ReceiveRetryable, Reason: "binding_unavailable"}
	}
	route, err := s.store.GetRoute(rec.Identity())
	if err == nil && route.Phase == "changing" {
		if rec.Kind != "control" {
			gate.opMu.Unlock()
			return ReceiveResult{Disposition: ReceiveRetryable, Reason: "route_changing"}
		}
	} else {
		route, err = s.store.EnsureRoute(ctx, rec.Identity(), digest, workspace, session)
	}
	if err != nil {
		gate.opMu.Unlock()
		return ReceiveResult{Disposition: ReceiveRetryable, Reason: "route_unavailable"}
	}
	rec.AuthorizationDigest = digest
	rec.WorkspaceID = workspace
	rec.SessionID = session
	rec.RouteRevision = route.Revision
	if rec.Kind == "task" {
		rec.Text = commandForIncomingMessage(msg).Content
	}
	// Publish acceptance-in-progress without holding the service mutex across
	// acceptance. A wake cannot expire controls until their parameters arrive.
	s.mu.Lock()
	s.accepting[rec.Identity()]++
	s.mu.Unlock()
	rec.statusQuery = rec.Kind == "control" && msg.InternalAction == nil && commandForIncomingMessage(msg).Type == "status"
	result := s.store.acceptRecord(ctx, rec, e.Cursor, valid)
	s.mu.Lock()
	s.accepting[rec.Identity()]--
	if s.accepting[rec.Identity()] == 0 {
		delete(s.accepting, rec.Identity())
	}
	if result.Disposition == ReceiveAccepted && rec.Kind == "control" {
		s.controls[result.RecordID] = ingressControl{msg, rec.ControlTarget}
	}
	s.mu.Unlock()
	gate.opMu.Unlock()
	s.signal()
	return result
}
func (s *IngressService) run() {
	defer s.wg.Done()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		case <-s.store.Changed():
		case <-tick.C:
		}
		s.sweepControls()
		slotBlocked := ""
		// Visit only routes with pending records (the store's route index) and
		// skip the ones already prepared-and-active before cloning any body.
		for _, id := range s.store.PendingRoutes() {
			s.mu.Lock()
			eligible := s.prepared[compositeKey(id.Platform, id.AccountID)] && !s.active[id]
			routeBlocked := s.routeBlockedLocked(id)
			s.mu.Unlock()
			if !eligible {
				continue
			}
			records := s.store.ListRoutePendingMetadata(id)
			s.settleStalledRecords(records)
			blocked := false
			quarantined := make(map[ingressRevision]bool)
			for _, rec := range records {
				if rec.State == "dispatching" || rec.State == "unknown" {
					blocked = true
				}
				if rec.State == "quarantined" {
					quarantined[ingressRevision{id, rec.RouteRevision}] = true
				}
			}
			ready := false
			for _, rec := range records {
				if rec.State != "received" {
					continue
				}
				if rec.Kind == "task" && (blocked || routeBlocked || quarantined[ingressRevision{id, rec.RouteRevision}]) {
					continue
				}
				ready = true
				break
			}
			if !ready {
				continue
			}
			s.mu.Lock()
			select {
			case s.slots <- struct{}{}:
				s.active[id] = true
				s.wg.Add(1)
				go s.dispatchRoute(id)
			default:
				if slotBlocked == "" {
					slotBlocked = id.Platform
				}
			}
			s.mu.Unlock()
		}
		if slotBlocked != "" {
			s.router.ObserveIngress(slotBlocked, "slot_exhausted", "")
		}
	}
}

// sweepControls drops retained control parameters whose record is no longer
// pending, so a settled or pruned record cannot pin its IncomingMessage. Each
// id is re-checked against the store rather than a scheduler snapshot, so a
// control accepted while the tick is running is never dropped.
func (s *IngressService) sweepControls() {
	s.mu.Lock()
	if len(s.controls) == 0 {
		s.mu.Unlock()
		return
	}
	ids := make([]string, 0, len(s.controls))
	for id := range s.controls {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		rec, err := s.store.Get(id)
		if err == nil && !rec.Terminal() {
			continue
		}
		s.mu.Lock()
		delete(s.controls, id)
		s.mu.Unlock()
	}
}

// settleStalledRecords settles deliveries that can no longer be confirmed.
// A record left in dispatching or unknown keeps its route barrier raised and
// its pending slot reserved forever, because the receipt observer fires its
// own timeout once and is never heard from again. Settling releases both.
func (s *IngressService) settleStalledRecords(records []IngressRecord) {
	if len(records) == 0 {
		return
	}
	now := s.store.now()
	for _, rec := range records {
		if rec.State != "dispatching" && rec.State != "unknown" {
			continue
		}
		if now.Sub(rec.UpdatedAt) < ingressStallDeadline {
			continue
		}
		s.finish(rec, "expired", "receipt_timeout")
	}
}

type ingressRevision struct {
	identity ChatIdentity
	revision string
}

func (s *IngressService) dispatchRoute(id ChatIdentity) {
	defer func() { s.mu.Lock(); delete(s.active, id); s.mu.Unlock(); <-s.slots; s.wg.Done() }()
	records := s.store.ListRoutePendingMetadata(id)
	s.mu.Lock()
	blocked := s.routeBlockedLocked(id)
	s.mu.Unlock()
	unknown := blocked
	quarantined := make(map[string]bool)
	for _, rec := range records {
		if rec.State == "unknown" {
			unknown = true
		}
		if rec.State == "unknown" || rec.State == "dispatching" {
			blocked = true
		}
		if rec.State == "quarantined" {
			quarantined[rec.RouteRevision] = true
		}
	}
	records = s.prioritizeControls(records)
	for _, rec := range records {
		if rec.State != "received" {
			continue
		}
		if rec.Kind == "control" {
			s.mu.Lock()
			control, ok := s.controls[rec.RecordID]
			accepting := s.accepting[id] > 0
			delete(s.controls, rec.RecordID)
			s.mu.Unlock()
			if !ok {
				if accepting {
					return
				}
				s.finish(rec, "expired", "control_parameters_unavailable")
				continue
			}
			cmd := commandForIncomingMessage(control.message)
			query := cmd.Type == "status" || cmd.Type == "todos" || cmd.Type == "sessions" || cmd.Type == "role" && strings.TrimSpace(cmd.Content) == "" || !control.message.ForceText && strings.TrimSpace(control.message.Text) == "/help" && control.message.InternalAction == nil
			if unknown && !query && cmd.Type != "cancel" && cmd.Type != "bind" && !s.canRetryIngressQuestion(control, records) {
				s.finish(rec, "rejected", "route_outcome_unknown")
				s.router.enqueueIngressDispatchDiagnostic(control.message, DispatchResult{"rejected", "route_outcome_unknown"})
				continue
			}
			result := s.router.DispatchAccepted(s.ctx, control.message, control.target, rec.RecordID)
			s.router.enqueueIngressDispatchDiagnostic(control.message, result)
			if result.State != "awaiting_receipt" {
				s.finish(rec, result.State, result.Reason)
			}
			if result.State == "awaiting_receipt" {
				blocked = true
			}
			blocked, unknown = s.currentIngressBlockers(id)
			continue
		}
		if blocked || quarantined[rec.RouteRevision] {
			continue
		}
		s.dispatchTask(rec)
		return
	}
}
func (s *IngressService) finish(rec IngressRecord, state, reason string) {
	s.dropControl(rec.RecordID)
	err := s.store.UpdateRecord(context.Background(), rec.RecordID, []string{"received", "dispatching", "unknown"}, func(r *IngressRecord) error { r.State = state; r.Reason = reason; return nil })
	if err != nil {
		if current, getErr := s.store.Get(rec.RecordID); getErr == nil && current.Terminal() {
			return
		}
		s.blockRoute(rec.Identity(), reason)
	}
}
func (s *IngressService) quarantine(ctx context.Context, rec IngressRecord, reason string) error {
	s.dropControl(rec.RecordID)
	return s.store.UpdateRecord(ctx, rec.RecordID, []string{"received"}, func(r *IngressRecord) error { r.State = "quarantined"; r.Reason = reason; return nil })
}

// dropControl releases retained control parameters once their record can no
// longer be dispatched, so a settled or pruned record cannot pin its message.
func (s *IngressService) dropControl(recordID string) {
	s.mu.Lock()
	delete(s.controls, recordID)
	s.mu.Unlock()
}

// quarantineTask settles a received task as quarantined and reports an
// unexpected failure. An already-settled, pruned, or shutting-down record is
// benign and stays silent.
func (s *IngressService) quarantineTask(rec IngressRecord, reason string) {
	if err := s.quarantine(s.ctx, rec, reason); err != nil && !s.quarantineBenign(err) {
		log.Warnf("ingress: quarantine failed platform=%v chat=%v reason=%v err=%v", imMetricPlatform(rec.Platform), rec.ChatID, reason, err)
	}
}
func (s *IngressService) quarantineBenign(err error) bool {
	return errors.Is(err, errIngressMissing) || errors.Is(err, errIngressBlocked) || errors.Is(err, errIngressClosed) || s.ctx.Err() != nil
}
func (s *IngressService) dispatchTask(rec IngressRecord) {
	if !s.Available() {
		return
	}
	// Scheduling only reads metadata. Load the body for this one delivery and
	// recheck state in case cleanup or a route barrier changed it meanwhile.
	rec, err := s.store.Get(rec.RecordID)
	if err != nil || rec.Kind != "task" || rec.State != "received" {
		return
	}
	// Current authorization is reacquired; old tickets are never reused.
	a := s.reauthorizeRecord(rec)
	if a.Disposition != AdmissionAllowed {
		// A permanent refusal can never become deliverable, so settle the
		// record instead of re-dispatching it every tick. A transient refusal
		// keeps retrying until the delivery window closes.
		if a.Disposition == AdmissionRejected {
			s.quarantineTask(rec, "authorization_changed")
		} else if ingressDeliveryExpired(rec.ReceivedAt, time.Now()) {
			s.quarantineTask(rec, "received_expired")
		}
		return
	}
	gate := a.Ticket.gate
	gate.opMu.Lock()
	locked := true
	defer func() {
		if locked {
			gate.opMu.Unlock()
		}
	}()
	if !s.router.admission.Valid(a.Ticket) {
		return
	}
	digest, workspace, session, err := s.router.ingressRouteIdentity(rec.Identity())
	route, routeErr := s.store.GetRoute(rec.Identity())
	if err != nil || routeErr != nil || route.Phase != "active" || route.Revision != rec.RouteRevision || digest != rec.AuthorizationDigest || workspace != rec.WorkspaceID || (rec.SessionID != "" && session != rec.SessionID) {
		s.quarantineTask(rec, "route_identity_changed")
		return
	}
	if ingressDeliveryExpired(rec.ReceivedAt, time.Now()) {
		s.quarantineTask(rec, "received_expired")
		return
	}
	if readiness := checkIngressDeliveryReadiness(s.router.findAdapterByType(rec.Platform), rec.Identity()); !readiness.Ready {
		return
	}
	key := (processKey{workspaceID: workspace, imType: rec.Platform, chatID: rec.ChatID}).String()
	if s.router.mgr.transferBlocked(key) {
		s.quarantineTask(rec, "transfer_guard")
		return
	}
	p, err := s.router.mgr.getOrSpawnForKeyChatLocked(key)
	if err != nil || p == nil {
		s.quarantineTask(rec, "session_restore_failed")
		return
	}
	gate.opMu.Unlock()
	locked = false
	wait, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	identity, err := s.router.waitIngressReadyPin(wait, key, p, rec.SessionID)
	if err != nil {
		s.quarantineTask(rec, "session_ready_pin_unconfirmed")
		return
	}
	gate.opMu.Lock()
	locked = true
	current, readErr := s.store.Get(rec.RecordID)
	currentRoute, routeErr := s.store.GetRoute(rec.Identity())
	currentDigest, currentWorkspace, currentSession, identityErr := s.router.ingressRouteIdentity(rec.Identity())
	if readErr != nil || current.State != "received" || routeErr != nil || currentRoute.Phase != "active" || currentRoute.Revision != rec.RouteRevision || identityErr != nil || currentDigest != digest || currentWorkspace != workspace || currentSession != identity.SessionID || s.router.mgr.GetProcessForKey(key) != p || s.router.mgr.transferBlocked(key) {
		s.quarantineTask(rec, "route_identity_changed")
		return
	}
	if ingressDeliveryExpired(rec.ReceivedAt, time.Now()) {
		s.quarantineTask(rec, "received_expired")
		return
	}
	// Restoring a process can wait for tens of seconds. Recheck the live
	// transport and authorization before publishing or sending this delivery.
	if !s.router.admission.Valid(a.Ticket) || !checkIngressDeliveryReadiness(s.router.findAdapterByType(rec.Platform), rec.Identity()).Ready {
		return
	}
	if rec.SessionID == "" {
		if err = s.store.BindRouteSession(s.ctx, rec.Identity(), rec.RouteRevision, identity.SessionID); err != nil {
			s.quarantineTask(rec, "session_pin_persistence_failed")
			return
		}
	}
	state := p.State()
	if state.PendingConfirm != nil && isDoneTool(state.PendingConfirm.ToolName) {
		s.quarantineTask(rec, "new_done_confirmation")
		return
	}
	requestID := "im-" + rec.RecordID
	err = s.store.UpdateRecord(s.ctx, rec.RecordID, []string{"received"}, func(r *IngressRecord) error {
		if !s.router.admission.Valid(a.Ticket) || !checkIngressDeliveryReadiness(s.router.findAdapterByType(rec.Platform), rec.Identity()).Ready {
			return errIngressTransportChanged
		}
		r.State = "dispatching"
		r.SessionID = identity.SessionID
		r.RequestID = requestID
		r.DispatchRuntimeID = identity.RuntimeID
		r.DispatchSessionEpoch = identity.SessionEpoch
		return nil
	})
	if err != nil {
		return
	}
	if !s.router.admission.Valid(a.Ticket) || !checkIngressDeliveryReadiness(s.router.findAdapterByType(rec.Platform), rec.Identity()).Ready {
		// No observer or stdin write exists yet, so this delivery is known not
		// to have started. If restoring received fails, leave the conservative
		// dispatching evidence for the existing unknown-outcome handling.
		_ = s.store.UpdateRecord(s.ctx, rec.RecordID, []string{"dispatching"}, func(r *IngressRecord) error {
			r.State = "received"
			return nil
		})
		return
	}
	err = p.ObserveInputReceipt(requestID, func(receipt ProcessInputReceipt) error {
		if receipt.RuntimeID != identity.RuntimeID || receipt.SessionID != identity.SessionID || receipt.SessionEpoch != identity.SessionEpoch {
			return errIngressRouteChanged
		}
		state := "unknown"
		switch receipt.Status {
		case "handled", "queued", "started":
			state = "consumed"
		case "rejected":
			state = "rejected"
		}
		err := s.store.UpdateRecord(context.Background(), rec.RecordID, []string{"dispatching", "unknown"}, func(r *IngressRecord) error { r.State = state; r.ReceiptStatus = receipt.Status; return nil })
		if err == nil {
			s.signal()
		}
		return err
	}, identity)
	if err != nil {
		s.finish(rec, "unknown", "observer_registration_failed")
		return
	}
	if err = p.SendUserMessageWithRequestID(rec.Text, requestID); err != nil {
		s.finish(rec, "unknown", "stdin_outcome_unknown")
	}
}

// Accepted records already passed transport chat-kind evidence. Rebuilding an
// owner ticket here does not make an assertion that an opaque chat ID is a DM.
func (s *IngressService) reauthorizeRecord(rec IngressRecord) AdmissionResult {
	a := s.router.admission
	cfg := s.router.getConfig()
	im := cfg.IMConfigByType(rec.Platform)
	if im == nil || im.OwnerIdentity() == nil {
		return AdmissionResult{Disposition: AdmissionRejected}
	}
	owner := im.OwnerIdentity()
	if owner.AccountID != rec.AccountID || owner.UserID != rec.SenderID || im.Bindings()[rec.ChatID] != rec.WorkspaceID {
		return AdmissionResult{Disposition: AdmissionRejected}
	}
	a.pairing.mu.Lock()
	platform := a.pairing.platforms[rec.Platform]
	if platform == nil || platform.changed || platform.account != rec.AccountID {
		a.pairing.mu.Unlock()
		return AdmissionResult{Disposition: AdmissionUnavailable}
	}
	epoch := platform.epoch
	a.pairing.mu.Unlock()
	gate := a.gate(rec.Identity(), rec.WorkspaceID, epoch)
	if gate == nil {
		return AdmissionResult{Disposition: AdmissionUnavailable}
	}
	_, generation, _ := gate.snapshot()
	ticket := AdmissionTicket{Kind: "paired_task", Identity: rec.Identity(), SenderID: rec.SenderID, AccountEpoch: epoch, BindingGeneration: generation, ReceivedAt: rec.ReceivedAt, gate: gate}
	if !a.Valid(ticket) {
		return AdmissionResult{Disposition: AdmissionRejected}
	}
	return AdmissionResult{Ticket: ticket, Disposition: AdmissionAllowed}
}

// PrepareAccount enables this live account; no input is restored from disk.
func (s *IngressService) PrepareAccount(ctx context.Context, platform, account string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.prepared[compositeKey(platform, account)] = true
	s.mu.Unlock()
	s.signal()
	return nil
}

// Urgent controls can overtake received tasks, while generic side effects retain
// their original position. Four controls yield a turn to the ordinary queue.
func (s *IngressService) prioritizeControls(records []IngressRecord) []IngressRecord {
	priority := make([]IngressRecord, 0, 4)
	rest := make([]IngressRecord, 0, len(records))
	var deferredPriority []IngressRecord
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range records {
		urgent := false
		if control, ok := s.controls[rec.RecordID]; ok && rec.State == "received" && rec.Kind == "control" {
			cmd := commandForIncomingMessage(control.message)
			switch cmd.Type {
			case "status", "cancel", "confirm", "question", "handoff", "sessions", "todos":
				urgent = true
			}
			urgent = urgent || !control.message.ForceText && control.message.InternalAction == nil && strings.TrimSpace(control.message.Text) == "/help" || cmd.Type == "takeover" && cmd.Action == "status"
		}
		if urgent {
			if len(priority) < 4 {
				priority = append(priority, rec)
			} else {
				deferredPriority = append(deferredPriority, rec)
			}
		} else {
			rest = append(rest, rec)
		}
	}
	// Yield to the ordinary FIFO, including its earlier side effects. Selecting
	// the first task alone would let it overtake shell/model/role commands.
	return append(append(priority, rest...), deferredPriority...)
}

func lockIngressMutex(ctx context.Context, mu *sync.Mutex) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if mu.TryLock() {
		return nil
	}
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if mu.TryLock() {
				return nil
			}
		}
	}
}

// Available reports whether this process can accept ingress work.
func (s *IngressService) Available() bool {
	s.mu.Lock()
	ready := s.started && s.ctx != nil && s.ctx.Err() == nil
	s.mu.Unlock()
	return ready && !s.store.closing.Load()
}

// pairingHintReply explains an unpaired refusal to a private sender. Hints
// never dispatch work; they only tell the sender how pairing is meant to start.
func pairingHintReply(r *NotificationRouter, msg IncomingMessage, kind string) string {
	bind := r.ingressCommandHint(msg, `/bind "<directory>" <code>`)
	pairing := r.admission.pairing
	switch kind {
	case "pairing_hint_usage":
		return "Invalid bind command. Send " + bind + " using the code shown on the gateway terminal."
	case "pairing_hint_code":
		return "That pairing code is not valid. Check the code shown on the gateway terminal and resend " + bind + "."
	default:
		if pairing.codeLiveFor(msg.IMType) {
			return "This gateway has no paired owner yet. Send " + bind + " using the code shown on the gateway terminal."
		}
		if pairing.enabled {
			return "This gateway has no paired owner yet and its pairing code is no longer valid. Restart the gateway with --pair to display a new code, then send " + bind + "."
		}
		return "This gateway has no paired owner yet. Restart the gateway with --pair to display a pairing code, then send " + bind + "."
	}
}
