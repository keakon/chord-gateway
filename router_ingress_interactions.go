package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/keakon/chord-gateway/config"
)

// ValidateIngressInteraction validates the normalized callback shape. It is
// deliberately independent of admission: a valid callback grants no authority.
func ValidateIngressInteraction(msg IncomingMessage) error {
	a := msg.InternalAction
	if a == nil {
		return nil
	}
	if msg.IMType == "" || a.BindingInstanceID == "" || a.RequestID == "" {
		return errors.New("invalid interaction identity")
	}
	totalBytes := 0
	for _, value := range []string{a.BindingInstanceID, a.RequestID, a.Value, a.Handle.MessageID, a.Handle.Token} {
		totalBytes += len(value)
		if len(value) > 64*1024 {
			return errors.New("interaction field too large")
		}
	}
	if len(a.Values) > 128 {
		return errors.New("too many interaction values")
	}
	for _, value := range a.Values {
		totalBytes += len(value)
		if len(value) > 64*1024 {
			return errors.New("interaction value too large")
		}
	}
	if totalBytes > 64*1024 {
		return errors.New("interaction payload too large")
	}
	switch a.Type {
	case "confirm":
		if a.Action != "allow" && a.Action != "deny" || len(a.Values) > 0 || a.Value != "" {
			return errors.New("invalid confirmation interaction")
		}
	case "question":
		if a.Action != "answer" || a.Value == "" && len(a.Values) == 0 || a.Value != "" && len(a.Values) != 0 {
			return errors.New("invalid question interaction")
		}
	case "role":
		if a.Action != "switch" || strings.TrimSpace(a.Value) == "" || len(a.Values) > 0 {
			return errors.New("invalid role interaction")
		}
	default:
		return errors.New("unsupported interaction")
	}
	return nil
}

func (r *NotificationRouter) loginReply(target string, hints ...func(string) string) (string, DispatchResult) {
	target = config.NormalizeIMType(target)
	if target == "" {
		options := r.availableLoginTargets()
		if len(options) == 0 {
			return "⚠️ No IM adapter supports login renewal.", DispatchResult{"completed", ""}
		}
		return "Usage: " + commandHint("/login <platform>", hints) + "\nSupported platforms: " + strings.Join(options, ", "), DispatchResult{"completed", ""}
	}
	adapter := r.findAdapterByType(target)
	if adapter == nil {
		return fmt.Sprintf("⚠️ No %s adapter found, please check configuration.", target), DispatchResult{"rejected", "login_adapter_missing"}
	}
	qr, err := adapter.StartLogin()
	if errors.Is(err, ErrLoginNotSupported) {
		return fmt.Sprintf("⚠️ %s does not support login renewal via %s.", imDisplayName(target), commandHint("/login", hints)), DispatchResult{"rejected", "login_unsupported"}
	}
	if err != nil {
		return fmt.Sprintf("❌ Failed to get %s login link: %v", imDisplayName(target), err), DispatchResult{"unknown", "login_outcome_unknown"}
	}
	return fmt.Sprintf("⚠️ Click the link to scan and renew %s login:\n%s", imDisplayName(target), qr), DispatchResult{"completed", ""}
}

// restoreIngressQuery may resume only the session pin frozen at reception.
// A missing pin means an empty query, never permission to create a session.
func (r *NotificationRouter) restoreIngressQuery(ctx context.Context, key string, t *IngressControlTarget) (*ChordProcess, *IngressControlTarget, DispatchResult) {
	if t == nil || t.SessionID == "" {
		return nil, t, DispatchResult{}
	}
	// Reuse the atomic absence/pin check under the lifecycle and process-key
	// locks. A query cannot attach to a runtime introduced after capture.
	p, target, result := r.startIngressCommandProcess(ctx, key, t)
	if result.State != "" && result.Reason != "control_target_expired" {
		return nil, t, DispatchResult{"rejected", "session_restore_failed"}
	}
	return p, target, result
}

func (r *NotificationRouter) synchronizeIngressQuestion(ctx context.Context, p *ChordProcess, cmd IMCommand, t *IngressControlTarget) (*IngressControlTarget, error) {
	if t == nil {
		return nil, errIngressTargetExpired
	}
	if !t.QuestionLookup {
		return t, nil
	}
	if cmd.RequestID == "" || t.RequestID != cmd.RequestID {
		return nil, errIngressTargetExpired
	}
	p.mu.RLock()
	valid := p.ingressTargetMatchesLocked(t, IMCommand{Type: "status"})
	p.mu.RUnlock()
	if !valid {
		return nil, errIngressTargetExpired
	}
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := p.WaitStatus(wait, t.RequestID); err != nil {
		return nil, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.ingressTargetMatchesLocked(t, IMCommand{Type: "status"}) {
		return nil, errIngressTargetExpired
	}
	q := p.state.Questions[t.RequestID]
	if q == nil {
		return nil, errIngressTargetExpired
	}
	copyTarget := *t
	copyTarget.Version = q.Version
	copyTarget.QuestionLookup = false
	return &copyTarget, nil
}

func questionOperationCommand(msg IncomingMessage, q *QuestionPayload, cmd IMCommand, t *IngressControlTarget) (map[string]any, error) {
	if cmd.Invalid || q == nil || t == nil {
		return nil, errIngressTargetExpired
	}
	payload := map[string]any{"type": "question", "request_id": t.RequestID, "version": t.Version}
	if cmd.Action == "" {
		answer := parseQuestionAnswer(cmd.Answers, q, msg.InternalAction)
		if len(answer.values) == 0 {
			return nil, errors.New("invalid question answer")
		}
		payload["answers"], payload["custom"], payload["action"] = answer.values, answer.custom, "answer"
		if q.Outcome != "" {
			payload["action"] = "revise"
		}
	} else {
		switch cmd.Action {
		case "interact", "withdraw_requirement", "replace_requirement":
		default:
			return nil, errors.New("invalid question action")
		}
		payload["action"], payload["user_text"], payload["replacement_id"] = cmd.Action, cmd.Content, cmd.Reason
	}
	payload["operation_id"] = questionUserOperationID(msg, t.RequestID, payload["action"].(string))
	return payload, nil
}

func (r *NotificationRouter) dispatchIngressQuestion(ctx context.Context, msg IncomingMessage, p *ChordProcess, cmd IMCommand, t *IngressControlTarget, recordIDs ...string) DispatchResult {
	t, err := r.synchronizeIngressQuestion(ctx, p, cmd, t)
	if err != nil {
		return DispatchResult{"rejected", "question_sync_failed"}
	}
	p.mu.RLock()
	valid := p.ingressTargetMatchesLocked(t, cmd)
	var q *QuestionPayload
	if valid {
		q = p.state.Questions[t.RequestID]
	}
	payload, err := questionOperationCommand(msg, q, cmd, t)
	p.mu.RUnlock()
	if !valid {
		return DispatchResult{"rejected", "control_target_expired"}
	}
	if err != nil {
		return DispatchResult{"rejected", "invalid_answer"}
	}
	for _, recordID := range recordIDs {
		if r.ingressStore == nil {
			return DispatchResult{"unknown", "storage_unavailable"}
		}
		if err := r.ingressStore.UpdateRecord(ctx, recordID, []string{"received"}, func(rec *IngressRecord) error {
			if rec.ControlTarget == nil || rec.ControlTarget.RequestID != t.RequestID || rec.ControlTarget.SessionID != "" && rec.ControlTarget.SessionID != t.SessionID || rec.ControlTarget.RuntimeID != "" && rec.ControlTarget.RuntimeID != t.RuntimeID {
				return errIngressTargetExpired
			}
			copyTarget := *t
			rec.ControlTarget = &copyTarget
			return nil
		}); err != nil {
			return DispatchResult{"unknown", "question_target_commit_unknown"}
		}
	}
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	receipt, err := r.waitQuestionOperationTarget(wait, p.key, msg.SenderID, p, payload, t, recordIDs)
	if errors.Is(err, errIngressTargetExpired) || errors.Is(err, ErrSessionTransferred) {
		return DispatchResult{"rejected", "control_target_expired"}
	}
	if err != nil {
		return DispatchResult{"unknown", "question_outcome_unknown"}
	}
	if !receipt.Accepted {
		return DispatchResult{"rejected", "question_rejected"}
	}
	r.enqueueIngressInteractionPresentation(msg, "question", t.RequestID, "Question updated", "✅ Question updated.", "green", false, t.BindingInstanceID)
	return DispatchResult{"completed", ""}
}

// waitIngressQuestion registers and writes while holding the same state lock
// used by stdout projections. The frozen target cannot be replaced mid-write.
func (p *ChordProcess) waitIngressQuestion(ctx context.Context, command map[string]any, t *IngressControlTarget) (QuestionReceipt, error) {
	id, _ := command["operation_id"].(string)
	ch := make(chan QuestionReceipt, 1)
	p.mu.Lock()
	if !p.ingressTargetMatchesLocked(t, IMCommand{Type: "question"}) {
		p.mu.Unlock()
		return QuestionReceipt{}, errIngressTargetExpired
	}
	if p.questionWaiters == nil {
		p.questionWaiters = make(map[string]chan QuestionReceipt)
	}
	if p.questionWaiters[id] != nil {
		p.mu.Unlock()
		return QuestionReceipt{}, errors.New("question operation already waiting")
	}
	p.questionWaiters[id] = ch
	data, err := json.Marshal(command)
	if err == nil {
		if !p.commandAllowedLocked(command) {
			err = ErrSessionTransferred
		} else {
			err = p.writeCommandLocked(data)
		}
	}
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.questionWaiters, id); p.mu.Unlock() }()
	if err != nil {
		return QuestionReceipt{}, err
	}
	select {
	case receipt := <-ch:
		p.mu.RLock()
		valid := p.ingressTargetMatchesLocked(t, IMCommand{Type: "status"})
		p.mu.RUnlock()
		version, _ := command["version"].(uint64)
		if !valid || receipt.OperationID != id || receipt.RequestID != t.RequestID || receipt.Accepted && receipt.Version < version {
			return QuestionReceipt{}, errors.New("question receipt target mismatch")
		}
		return receipt, nil
	case <-ctx.Done():
		return QuestionReceipt{}, ctx.Err()
	}
}

// IngressQuestionRetryRecords proves that a new user input is an explicit retry
// of a retained uncertain operation. It does not replay any stored parameters.
func (r *NotificationRouter) IngressQuestionRetryRecords(msg IncomingMessage, t *IngressControlTarget) []string {
	if r.mgr == nil || t == nil || t.QuestionLookup || msg.ticket == nil || r.admission == nil || !r.admission.Valid(*msg.ticket) {
		return nil
	}
	cmd := commandForIncomingMessage(msg)
	if cmd.Type != "question" {
		return nil
	}
	ws, err := r.getConfig().ResolveWorkspace(msg.IMType, msg.ChatID)
	if err != nil || ws == nil {
		return nil
	}
	key := (processKey{workspaceID: ws.ID, imType: msg.IMType, chatID: msg.ChatID}).String()
	p := r.mgr.GetProcessForKey(key)
	if p == nil {
		return nil
	}
	p.mu.RLock()
	if !p.ingressTargetMatchesLocked(t, cmd) {
		p.mu.RUnlock()
		return nil
	}
	payload, err := questionOperationCommand(msg, p.state.Questions[t.RequestID], cmd, t)
	p.mu.RUnlock()
	if err != nil {
		return nil
	}
	operationKey, err := questionOperationIdentity(key, msg.SenderID, payload, t)
	if err != nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	op, ok := r.questionOperations[operationKey]
	if !ok || !time.Now().Before(op.expiresAt) {
		return nil
	}
	return append([]string(nil), op.recordIDs...)
}

// ObserveIngressQuestionReceipt also handles late receipts after a timeout.
// The retained in-memory operation is the only authority to correlate controls.
// The caller must not hold p.mu; no callback parameters survive a restart.
func (r *NotificationRouter) ObserveIngressQuestionReceipt(p *ChordProcess, receipt QuestionReceipt) error {
	r.questionReceiptMu.Lock()
	defer r.questionReceiptMu.Unlock()
	if receipt.OperationID == "" || receipt.RequestID == "" {
		return nil
	}
	p.mu.RLock()
	runtime, epoch, session, binding := p.runtimeID, p.sessionEpoch, p.state.SessionID, p.bindingInstanceID
	p.mu.RUnlock()
	r.mu.Lock()
	var key string
	var found pendingQuestionOperation
	for operationKey, op := range r.questionOperations {
		if op.target == nil || op.command["operation_id"] != receipt.OperationID || op.target.RequestID != receipt.RequestID || !time.Now().Before(op.expiresAt) {
			continue
		}
		if op.target.RuntimeID != runtime || op.target.SessionEpoch != epoch || op.target.SessionID != "" && op.target.SessionID != session || op.target.BindingInstanceID != "" && op.target.BindingInstanceID != binding {
			continue
		}
		if version, ok := op.command["version"].(uint64); ok && receipt.Version < version {
			continue
		}
		key, found = operationKey, op
		found.recordIDs = append([]string(nil), op.recordIDs...)
		break
	}
	r.mu.Unlock()
	if key == "" {
		return nil
	}
	state := "completed"
	reason := ""
	if !receipt.Accepted {
		state, reason = "rejected", "question_rejected"
	}
	for _, id := range found.recordIDs {
		if r.ingressStore == nil {
			return errors.New("question receipt store unavailable")
		}
		rec, err := r.ingressStore.Get(id)
		if err != nil {
			// Question operations live 14d but ingress records expire after 24h.
			// A late receipt for an already-GC'd record is not a storage fault:
			// skip it instead of dirtying the process input.
			if errors.Is(err, errIngressMissing) {
				continue
			}
			return fmt.Errorf("read correlated question control: %w", err)
		}
		if rec.Terminal() {
			continue
		}
		if rec.Kind != "control" || rec.ControlTarget == nil || rec.ControlTarget.RuntimeID != runtime || rec.ControlTarget.RequestID != receipt.RequestID {
			return fmt.Errorf("question control identity mismatch")
		}
		if err = r.ingressStore.UpdateRecord(context.Background(), id, []string{"received", "dispatching", "unknown"}, func(rec *IngressRecord) error { rec.State, rec.Reason = state, reason; return nil }); err != nil {
			return fmt.Errorf("persist question receipt: %w", err)
		}
	}
	r.mu.Lock()
	if current, ok := r.questionOperations[key]; ok && current.command["operation_id"] == receipt.OperationID && len(current.recordIDs) == len(found.recordIDs) {
		delete(r.questionOperations, key)
	}
	r.mu.Unlock()
	return nil
}

// Explicit shell, role-switch and Chord commands may start their first runtime.
// The lifecycle lock verifies the reception-time absence before any spawn; a
// disappeared captured runtime is never silently replaced.
func (r *NotificationRouter) startIngressCommandProcess(ctx context.Context, key string, target *IngressControlTarget) (*ChordProcess, *IngressControlTarget, DispatchResult) {
	if target == nil || target.RuntimeID != "" {
		return nil, target, DispatchResult{"rejected", "control_target_expired"}
	}
	m := r.mgr
	m.lifecycleGate.RLock()
	release := m.acquireKeyLock(key)
	if m.shuttingDown.Load() || m.transferBlocked(key) || m.GetProcessForKey(key) != nil || m.pins == nil || m.pins.Get(pinKeyForBinding(key)) != target.SessionID {
		release()
		m.lifecycleGate.RUnlock()
		return nil, target, DispatchResult{"rejected", "control_target_expired"}
	}
	var args []string
	if target.SessionID != "" {
		args = []string{"--resume", target.SessionID}
	}
	p, err := m.spawnWithArgsForKeyLocked(key, args...)
	release()
	m.lifecycleGate.RUnlock()
	if err != nil || p == nil {
		return nil, target, DispatchResult{"rejected", "session_start_failed"}
	}
	wait, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	identity, err := r.waitIngressReadyPin(wait, key, p, target.SessionID)
	if err != nil {
		return nil, target, DispatchResult{"unknown", "session_start_unconfirmed"}
	}
	copyTarget := *target
	copyTarget.RuntimeID, copyTarget.SessionEpoch, copyTarget.SessionID, copyTarget.BindingInstanceID = identity.RuntimeID, identity.SessionEpoch, identity.SessionID, p.BindingInstanceID()
	return p, &copyTarget, DispatchResult{}
}
