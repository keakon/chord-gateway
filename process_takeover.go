package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/internal/buildinfo"
)

func resolveTakeoverWorkDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve canonical workspace: %w", err)
	}
	return resolved, nil
}
func (m *ChordManager) transferBlocked(key string) bool { return m != nil && m.transfers.Blocked(key) }
func (p *ChordProcess) inputBlockedLocked() bool {
	return p.bindingGuard || p.transfer != nil || p.restoringTransferID != ""
}
func (p *ChordProcess) commandAllowedLocked(cmd map[string]any) bool {
	if !p.inputBlockedLocked() {
		return true
	}
	switch cmd["type"] {
	case "status", "subscribe", "session_transfer_prepared", "cancel":
		return true
	case "question":
		// Only existing answer/presentation operations may close an in-flight wait.
		q := p.state.Questions[fmt.Sprint(cmd["request_id"])]
		return p.restoringTransferID == "" && q != nil && q.Outcome == "" && (cmd["action"] == "answer" || cmd["action"] == "interact" || cmd["action"] == "decline" || cmd["action"] == "presented")
	}
	return false
}
func sameTransferIdentity(a, b TransferStatus) bool {
	return a.TransferID == b.TransferID && a.SessionID == b.SessionID && a.SourceRuntimeID == b.SourceRuntimeID && a.SourceSessionEpoch == b.SourceSessionEpoch && a.Workspace == b.Workspace && a.TargetKind == b.TargetKind && a.TargetInstanceID == b.TargetInstanceID
}

// Preparation closes input on the same mutex used for every stdin command.
// Persistence and lifecycle serialization finish before sending the ordered ACK.
func (p *ChordProcess) processTransferEnvelope(env *HeadlessEnvelope) {
	var status TransferStatus
	if err := json.Unmarshal(env.Payload, &status); err != nil {
		p.reportTransportError(fmt.Errorf("decode session transfer: %w", err), false)
		return
	}
	m := p.mgr
	if m == nil {
		return
	}
	var notification *ControlState
	var notificationErr error
	defer func() {
		if notificationErr != nil {
			p.reportTransportError(notificationErr, false)
		}
		if notification != nil && p.onEvent != nil {
			p.onEvent(p.key, env.Type, *notification)
		}
	}()
	m.lifecycleGate.RLock()
	defer m.lifecycleGate.RUnlock()
	release := m.acquireKeyLock(p.key)
	defer release()
	if m.GetProcessForKey(p.key) != p || m.shuttingDown.Load() {
		return
	}
	p.mu.Lock()
	valid := status.TransferID != "" && status.SessionID == p.state.SessionID && status.SourceRuntimeID != "" && status.SourceRuntimeID == p.runtimeID && status.SourceSessionEpoch == p.sessionEpoch && status.Workspace == p.workspacePath && status.TargetKind == "tui" && status.TargetInstanceID != "" && p.bindingInstanceID != "" && status.BindingInstanceID == p.bindingInstanceID
	if !valid {
		p.mu.Unlock()
		notificationErr = errors.New("stale_source: invalid session transfer identity")
		return
	}
	if env.Type == "session_transfer_preparing" {
		if p.stoppedByGateway || p.exited || p.stdin == nil {
			p.mu.Unlock()
			notificationErr = p.ackTransferPreparation(status, "target_not_ready")
			return
		}
		if p.transfer != nil {
			p.mu.Unlock()
			return
		}
		status.State = "preparing"
		p.transfer = &status
	} else if p.transfer == nil || !sameTransferIdentity(*p.transfer, status) {
		p.mu.Unlock()
		return
	} else {
		switch env.Type {
		case "session_transfer_status":
			switch status.State {
			case "preparing", transferStatePrepareFailed, "draining", "saving", "save_failed":
			default:
				p.mu.Unlock()
				notificationErr = errors.New("invalid session transfer progress state")
				return
			}
		case "session_transfer_saved":
			status.State = "saved"
		case "session_transfer_released":
			status.State = "released"
		default:
			p.mu.Unlock()
			return
		}
		terminal := status.State == "saved" || status.State == "released"
		if terminal && len(p.pendingInputs) != 0 {
			p.mu.Unlock()
			notificationErr = errors.New("session transfer has unconfirmed inputs; saved state not accepted")
			return
		}
		if p.transfer.State == "released" || p.transfer.State == "saved" && status.State != "released" {
			p.mu.Unlock()
			return
		}
		p.transfer = &status
	}
	p.autoRestart = false
	p.state.Transfer = &status
	p.mu.Unlock()
	value := sessionTransfer{Status: status, Direction: "outgoing", BindingInstanceID: p.bindingInstanceID}
	_, exists, loadErr := m.transfers.Get(p.key)
	expected := ""
	if exists {
		expected = status.TransferID
	}
	persistErr := loadErr
	if persistErr == nil {
		persistErr = m.transfers.update(p.key, &value, expected)
	}
	if env.Type == "session_transfer_preparing" {
		errorCode := ""
		if persistErr != nil {
			errorCode = "persistence_failed"
		}
		notificationErr = p.ackTransferPreparation(status, errorCode)
	} else if (status.State == "saved" || status.State == "released") && persistErr == nil && m.pins != nil {
		if err := m.pins.ClearIf(p.sessionPinKey(), status.SessionID); err != nil {
			persistErr = err
		}
	}
	if persistErr != nil {
		notificationErr = errors.Join(notificationErr, fmt.Errorf("save transfer guard: %w", persistErr))
	}
	snapshot := p.State()
	notification = &snapshot
}

func (p *ChordProcess) ackTransferPreparation(status TransferStatus, errorCode string) error {
	ack := map[string]any{"type": "session_transfer_prepared", "transfer_id": status.TransferID, "session_id": status.SessionID, "source_runtime_id": status.SourceRuntimeID, "source_session_epoch": status.SourceSessionEpoch, "binding_instance_id": p.bindingInstanceID, "accepted": errorCode == ""}
	if errorCode != "" {
		ack["error_code"] = errorCode
	}
	if err := p.SendCommand(ack); err != nil {
		return fmt.Errorf("send transfer preparation ACK: %w", err)
	}
	return nil
}

// Exit code 20 is sufficient to block restarts even if stdout events were lost.
// It proves no ready target or checkpoint on its own.
func (p *ChordProcess) recordTransferExit(code int) bool {
	if p.mgr != nil {
		release := p.mgr.acquireKeyLock(p.key)
		defer release()
	}
	if p.mgr != nil && p.mgr.GetProcessForKey(p.key) != p {
		return true
	}
	p.mu.Lock()
	guarded := p.inputBlockedLocked() || code == sessionTransferExitCode
	var status TransferStatus
	if p.transfer != nil {
		status = *p.transfer
	} else {
		session := p.state.SessionID
		if session == "" && p.mgr != nil && p.mgr.pins != nil {
			session = p.mgr.pins.Get(p.sessionPinKey())
		}
		if session == "" {
			session = "unknown"
		}
		status = TransferStatus{TransferID: "exit-" + p.bindingInstanceID, SessionID: session, Workspace: p.workspacePath, State: "status_unknown"}
	}
	p.mu.Unlock()
	if !guarded {
		return false
	}
	if p.mgr != nil && p.mgr.transfers != nil {
		_, exists, err := p.mgr.transfers.Get(p.key)
		if err == nil && !exists && status.SessionID != "" {
			if err = p.mgr.transfers.update(p.key, &sessionTransfer{Status: status, Direction: "outgoing", BindingInstanceID: p.bindingInstanceID}, ""); err != nil {
				p.mgr.transfers.failClosed(err)
				p.reportTransportError(fmt.Errorf("persist transfer exit guard: %w", err), false)
				if p.mgr.pins != nil {
					_ = p.mgr.pins.ClearIf(p.sessionPinKey(), status.SessionID)
				}
			}
		}
	}
	return true
}

func (m *ChordManager) runTransferCLI(ctx context.Context, req takeoverRequest) (TransferStatus, error) {
	if m.takeoverCLI != nil {
		return m.takeoverCLI(ctx, m.Config().ChordBinary(), req)
	}
	return runTakeoverCLI(ctx, m.Config().ChordBinary(), req)
}

// Restore uses ordinary resume only. No failed restore is retried as fresh.
func (m *ChordManager) restoreTransferredSession(ctx context.Context, key string, record sessionTransfer) (*ChordProcess, error) {
	m.lifecycleGate.RLock()
	defer m.lifecycleGate.RUnlock()
	release := m.acquireKeyLock(key)
	defer release()
	if m.shuttingDown.Load() {
		return nil, ErrManagerShuttingDown
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current, ok, err := m.transfers.Get(key)
	if err != nil {
		return nil, err
	}
	if !ok || current.Status.TransferID != record.Status.TransferID {
		return nil, errors.New("stale transfer")
	}
	if current.Direction != "incoming" || (current.Status.State != "released" && current.Status.State != "restoring") {
		return nil, errors.New("source has not released session")
	}
	record = current
	p := m.GetProcessForKey(key)
	if p != nil && p.Alive() {
		p.mu.RLock()
		matches := p.restoringTransferID == record.Status.TransferID && p.bindingInstanceID == record.Status.TargetInstanceID
		p.mu.RUnlock()
		if !matches {
			return nil, errors.New("target_not_ready: binding already owns another process")
		}
	} else {
		if p != nil && p.restoringTransferID != record.Status.TransferID {
			if err := p.guardedInputError(); err != nil {
				return nil, err
			}
		}
		record.Status.State = "restoring"
		record.BindingInstanceID = record.Status.TargetInstanceID
		if err = m.transfers.update(key, &record, record.Status.TransferID); err != nil {
			return nil, err
		}
		p, err = m.spawnWithArgsForKeyLocked(key, "--resume", record.Status.SessionID, "--gateway-instance", record.Status.TargetInstanceID, "--gateway-transfer-id", record.Status.TransferID)
		if err != nil {
			return nil, err
		}
	}
	// The read loop sets ready only after validating all three target identities.
	select {
	case <-p.readySignal:
	case <-m.shutdownSignal():
		return nil, ErrManagerShuttingDown
	case <-ctx.Done():
		return nil, fmt.Errorf("restore readiness unknown; check /takeover status: %w", ctx.Err())
	}
	// Readiness and cancellation can become observable together. Cancellation
	// must not publish a session or clear its durable transfer guard.
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = m.publishGuardedBinding(p, record, record.Status.SessionID, true); err != nil {
		return nil, err
	}
	return p, nil
}

// The caller holds the binding lifecycle lock. Keep the stdin mutex through
// publication so EOF or termination cannot invalidate readiness between checking
// the target and clearing its durable guard.
func (m *ChordManager) publishGuardedBinding(p *ChordProcess, record sessionTransfer, session string, paused bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m.shuttingDown.Load() {
		return ErrManagerShuttingDown
	}
	if p.stdin == nil || p.exited || p.stoppedByGateway || p.state.SessionID == "" || session != "" && p.state.SessionID != session {
		return errors.New("session readiness not confirmed: target disconnected")
	}
	if paused && (p.workspacePath != record.Status.Workspace || !p.readyPaused || p.state.PausedInputs == nil || !p.state.PausedInputs.Paused || p.restoringTransferID != record.Status.TransferID || p.bindingInstanceID != record.Status.TargetInstanceID) {
		return errors.New("restore_failed: target did not report ready_paused")
	}
	if m.pins == nil {
		return errors.New("session pin store unavailable")
	}
	if err := m.pins.Set(p.sessionPinKey(), p.state.SessionID); err != nil {
		return fmt.Errorf("pin restored session: %w", err)
	}
	if m.shuttingDown.Load() {
		return ErrManagerShuttingDown
	}
	if err := m.transfers.update(p.key, nil, record.Status.TransferID); err != nil {
		return err
	}
	p.restoringTransferID = ""
	p.bindingGuard = false
	p.autoRestart = true
	return nil
}

func transferCommandBlocked(cmd IMCommand) bool {
	switch cmd.Type {
	case "status", "takeover", "new", "resume", "cancel", "question":
		return false
	}
	return true
}

func (p *ChordProcess) transferActive() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.inputBlockedLocked()
}

func transferRestoreArgs(args []string) (id, instance string) {
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--gateway-transfer-id":
			id = args[i+1]
		case "--gateway-instance":
			instance = args[i+1]
		}
	}
	return
}

func (m *ChordManager) replaceBlockedBinding(ctx context.Context, key, session string) (retErr error) {
	m.lifecycleGate.RLock()
	defer m.lifecycleGate.RUnlock()
	release := m.acquireKeyLock(key)
	defer release()
	if m.shuttingDown.Load() {
		return ErrManagerShuttingDown
	}
	record, ok, err := m.transfers.Get(key)
	if err != nil {
		return err
	}
	if !ok {
		return ErrSessionTransferred
	}
	if p := m.GetProcessForKey(key); p != nil {
		// A persisted incoming guard cannot authorize discarding an unrelated
		// disconnected session's unresolved input evidence.
		if record.Direction == "incoming" && p.restoringTransferID != record.Status.TransferID {
			if err := p.guardedInputError(); err != nil {
				return err
			}
		}
		if p.Alive() {
			return errors.New("source or target is still active; finish the transfer before changing the session")
		}
	}
	args := []string{}
	if strings.TrimSpace(session) != "" {
		args = []string{"--resume", session}
	}
	p, err := m.spawnWithArgsForKeyLocked(key, args...)
	if err != nil {
		return err
	}
	// This is an explicit replacement, not an observer of source transfer work.
	// Failed replacements must release their own target so another explicit
	// /new or /resume can retry without deleting the transfer guard.
	defer func() {
		if retErr != nil {
			m.removeProcessIfCurrent(key, p)
			p.TerminateGroup(2 * time.Second)
		}
	}()
	select {
	case <-p.readySignal:
	case <-m.shutdownSignal():
		return ErrManagerShuttingDown
	case <-ctx.Done():
		return ctx.Err()
	}
	return m.publishGuardedBinding(p, record, session, false)
}

var transferReadyTimeout = 10 * time.Second

func (m *ChordManager) gatewayManagedSupported() bool {
	if m.takeoverCLI != nil {
		return true
	}
	binary := m.Config().ChordBinary()
	meta := buildinfo.MetadataForPath(binary)
	identity := meta.Path + meta.MTime
	m.capabilityMu.Lock()
	defer m.capabilityMu.Unlock()
	if supported, ok := m.managedCapabilities[identity]; ok {
		return supported
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	supported := probeGatewayManaged(ctx, binary)
	if m.managedCapabilities == nil {
		m.managedCapabilities = make(map[string]bool)
	}
	m.managedCapabilities[identity] = supported
	return supported
}

// Independent termination is never reported as a natural transfer completion.
func (p *ChordProcess) markTransferInterrupted() {
	if p.mgr == nil || p.mgr.transfers == nil {
		return
	}
	record, ok, err := p.mgr.transfers.Get(p.key)
	if err != nil || !ok || record.BindingInstanceID != p.bindingInstanceID {
		return
	}
	record.Interrupted = true
	if err = p.mgr.transfers.update(p.key, &record, record.Status.TransferID); err != nil {
		p.mgr.transfers.failClosed(err)
	}
}

// Replace ordinary sessions under the same lifecycle boundary as preparation.
// An implicit reconnect cannot stop a source that has begun transferring.
func (m *ChordManager) replaceOrdinarySession(key, session string) (*ChordProcess, error) {
	if p := m.GetProcessForKey(key); p != nil {
		if err := p.refreshPendingInputStatus(context.Background()); err != nil {
			return nil, err
		}
	}
	m.lifecycleGate.RLock()
	defer m.lifecycleGate.RUnlock()
	release := m.acquireKeyLock(key)
	defer release()
	if m.shuttingDown.Load() {
		return nil, ErrManagerShuttingDown
	}
	if m.transferBlocked(key) {
		return nil, ErrSessionTransferred
	}
	if p := m.GetProcessForKey(key); p != nil {
		p.mu.RLock()
		blocked := p.inputBlockedLocked()
		busy := p.state.Busy
		guarded := p.hasGuardedInputsLocked()
		p.mu.RUnlock()
		if blocked {
			return nil, ErrSessionTransferred
		}
		if guarded {
			return nil, errUnresolvedInputs
		}
		if busy {
			return nil, errors.New("process is busy")
		}
	}
	if session != "" {
		if m.pins != nil {
			if err := m.pins.Set(pinKeyForBinding(m.Config(), key), session); err != nil {
				return nil, err
			}
		}
		return m.spawnWithArgsForKeyLocked(key, "--resume", session)
	}
	if err := m.clearSessionPinForKey(key); err != nil {
		return nil, err
	}
	return m.spawnWithArgsForKeyLocked(key)
}
func (m *ChordManager) publishSessionPin(p *ChordProcess, session string) error {
	release := m.acquireKeyLock(p.key)
	defer release()
	if m.GetProcessForKey(p.key) != p || m.transferBlocked(p.key) || p.transferActive() {
		return nil
	}
	return m.pins.Set(p.sessionPinKey(), session)
}

// Failure cleanup applies only to the exact process that failed the resume.
func (m *ChordManager) discardFailedResume(key string, p *ChordProcess, session string) {
	m.lifecycleGate.RLock()
	defer m.lifecycleGate.RUnlock()
	release := m.acquireKeyLock(key)
	defer release()
	if m.GetProcessForKey(key) != p || m.transferBlocked(key) || p.transferActive() {
		return
	}
	m.removeProcessIfCurrent(key, p)
	p.TerminateGroup(2 * time.Second)
	if m.pins != nil {
		if err := m.pins.ClearIf(pinKeyForBinding(m.Config(), key), session); err != nil {
			log.Warnf("clear failed resume pin: %v", err)
		}
	}
}

// Only an authoritative source reply that confirms admission is still open
// can undo a failed preparation. Observer timeouts never take this path.
func (m *ChordManager) reopenUnfrozenSource(key string, status TransferStatus) error {
	release := m.acquireKeyLock(key)
	defer release()
	p := m.GetProcessForKey(key)
	if p == nil {
		return errors.New("source no longer available")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	matches := p.transfer != nil && sameTransferIdentity(*p.transfer, status) && p.transfer.BindingInstanceID == p.bindingInstanceID && p.runtimeID == status.SourceRuntimeID && p.sessionEpoch == status.SourceSessionEpoch && p.workspacePath == status.Workspace && p.state.SessionID == status.SessionID && p.stdin != nil && !p.exited && !p.stoppedByGateway
	if !matches || status.State != transferStatePrepareFailed {
		return errors.New("source has not confirmed open admission")
	}
	_, ok, err := m.transfers.Get(key)
	if err != nil {
		return err
	}
	if ok {
		if err = m.transfers.update(key, nil, status.TransferID); err != nil {
			return err
		}
	}
	p.transfer = nil
	p.state.Transfer = nil
	p.autoRestart = true
	return nil
}

// Observer replies cannot recreate guards cleared by a successful replacement.
func (m *ChordManager) persistObservedTransfer(key string, record sessionTransfer, localPreparation bool) error {
	release := m.acquireKeyLock(key)
	defer release()
	current, exists, err := m.transfers.Get(key)
	if err != nil {
		return err
	}
	expected := record.Status.TransferID
	if exists {
		if current.Status.TransferID != expected {
			return errors.New("stale transfer")
		}
		// A source observer cannot undo target restoration already in progress.
		if current.Direction == "incoming" && current.Status.State == "restoring" {
			return nil
		}
		current.Status = record.Status
		record = current
	} else if localPreparation {
		p := m.GetProcessForKey(key)
		if p == nil {
			return errors.New("stale source binding")
		}
		p.mu.RLock()
		matches := p.bindingInstanceID == record.BindingInstanceID && p.transfer != nil && sameTransferIdentity(*p.transfer, record.Status)
		p.mu.RUnlock()
		if !matches {
			return errors.New("stale source preparation")
		}
		expected = ""
	} else {
		return errors.New("transfer no longer owns this binding")
	}
	return m.transfers.update(key, &record, expected)
}
