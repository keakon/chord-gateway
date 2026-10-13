package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

func TestDelayedObserverCannotRecreateClearedTransferGuard(t *testing.T) {
	m, p, _ := newTakeoverTestProcess(t)
	record := takeoverTestRecord("released")
	record.Direction = "incoming"
	record.Status.TargetKind = "gateway"
	if err := m.transfers.update(p.key, &record, ""); err != nil {
		t.Fatal(err)
	}
	// A reply captured before an explicit replacement has returned late.
	if err := m.transfers.update(p.key, nil, record.Status.TransferID); err != nil {
		t.Fatal(err)
	}
	if err := m.persistObservedTransfer(p.key, record, false); err == nil {
		t.Fatal("late reply recreated a completed guard")
	}
	if m.transferBlocked(p.key) {
		t.Fatal("late observer blocked the new binding")
	}
}

func TestObservedStatusPreservesInterruptionAndTargetRestoration(t *testing.T) {
	m, p, _ := newTakeoverTestProcess(t)
	record := takeoverTestRecord("draining")
	if err := m.transfers.update(p.key, &record, ""); err != nil {
		t.Fatal(err)
	}
	latest := record
	latest.Interrupted = true
	if err := m.transfers.update(p.key, &latest, record.Status.TransferID); err != nil {
		t.Fatal(err)
	}
	record.Status.State = "released"
	if err := m.persistObservedTransfer(p.key, record, false); err != nil {
		t.Fatal(err)
	}
	got, _, _ := m.transfers.Get(p.key)
	if !got.Interrupted {
		t.Fatal("source query erased independent termination")
	}
	got.Direction = "incoming"
	got.Status.State = "restoring"
	if err := m.transfers.update(p.key, &got, got.Status.TransferID); err != nil {
		t.Fatal(err)
	}
	if err := m.persistObservedTransfer(p.key, record, false); err != nil {
		t.Fatal(err)
	}
	got, _, _ = m.transfers.Get(p.key)
	if got.Status.State != "restoring" {
		t.Fatal("source observation undid target restoration")
	}
}

func TestPreparationCanReopenOnlyAfterSourceConfirmsNoFreeze(t *testing.T) {
	m, p, _ := newTakeoverTestProcess(t)
	status := takeoverTestRecord("preparing").Status
	p.transfer = &status
	if err := m.transfers.update(p.key, &sessionTransfer{Status: status, Direction: "outgoing", BindingInstanceID: p.bindingInstanceID}, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.reopenUnfrozenSource(p.key, status); err == nil {
		t.Fatal("preparing response reopened admission")
	}
	status.State = transferStatePrepareFailed
	wrong := status
	wrong.SourceRuntimeID = "other"
	if err := m.reopenUnfrozenSource(p.key, wrong); err == nil {
		t.Fatal("wrong source reopened admission")
	}
	if err := m.reopenUnfrozenSource(p.key, status); err != nil {
		t.Fatal(err)
	}
	if m.transferBlocked(p.key) || p.transferActive() {
		t.Fatal("authoritatively rejected preparation remains blocked")
	}
}

func TestOutgoingStatusUsesSourceTargetIdentity(t *testing.T) {
	m, p, _ := newTakeoverTestProcess(t)
	record := takeoverTestRecord("draining")
	if err := m.transfers.update(p.key, &record, ""); err != nil {
		t.Fatal(err)
	}
	called := false
	m.takeoverCLI = func(_ context.Context, _ string, req takeoverRequest) (TransferStatus, error) {
		called = true
		if req.Action != "status" || req.TargetInstanceID != "" {
			t.Errorf("outgoing query used gateway target identity: %#v", req)
		}
		return record.Status, nil
	}
	r := &NotificationRouter{mgr: m, adapter: &stubIMAdapter{typ: "wechat"}}
	r.observeTakeover(&config.Workspace{ID: "test"}, "chat", p.key, false, nil, testChatOperation(t, r.mgr, p.key))
	if !called {
		t.Fatal("source transfer not queried")
	}
}

func TestOrdinaryReplacementCannotStopDrainingSource(t *testing.T) {
	m, p, _ := newTakeoverTestProcess(t)
	record := takeoverTestRecord("draining")
	p.transfer = &record.Status
	if err := m.transfers.update(p.key, &record, ""); err != nil {
		t.Fatal(err)
	}
	_, err := m.replaceOrdinarySession(p.key, "")
	if !errors.Is(err, ErrSessionTransferred) || p.stoppedByGateway {
		t.Fatalf("replacement stopped transferring source: %v", err)
	}
}

func TestZeroWaitReleasedSourceDoesNotSpawnTarget(t *testing.T) {
	m, p, _ := newTakeoverTestProcess(t)
	m.removeProcessIfCurrent(p.key, p)
	record := takeoverTestRecord("waiting_source")
	record.Direction = "incoming"
	record.Status.TargetKind = "gateway"
	if err := m.transfers.update(p.key, &record, ""); err != nil {
		t.Fatal(err)
	}
	m.takeoverCLI = func(context.Context, string, takeoverRequest) (TransferStatus, error) {
		s := record.Status
		s.State = "released"
		return s, nil
	}
	r := &NotificationRouter{mgr: m, adapter: &stubIMAdapter{typ: "wechat"}}
	zero := time.Duration(0)
	r.observeTakeover(&config.Workspace{ID: "test"}, "chat", p.key, true, &zero, testChatOperation(t, r.mgr, p.key))
	got, _, _ := m.transfers.Get(p.key)
	if got.Status.State != "released" || m.GetProcessForKey(p.key) != nil {
		t.Fatal("zero observer budget started target work")
	}
}

func TestInBandSessionSwitchRefreshesTakeoverEpoch(t *testing.T) {
	m, p, _ := newTakeoverTestProcess(t)
	p.processEnvelope(&HeadlessEnvelope{Type: "session_switched", Payload: []byte(`{"session_id":"session-new","runtime_id":"runtime-1","session_epoch":2}`)})
	status := takeoverTestRecord("preparing").Status
	status.SessionID = "session-new"
	status.SourceSessionEpoch = 2
	p.processEnvelope(takeoverEnvelope(t, "session_transfer_preparing", status))
	if !m.transferBlocked(p.key) || p.sessionEpoch != 2 {
		t.Fatal("in-band switch left stale takeover identity")
	}
}

func TestTransferStoreKeepsPublishedStartTimeAndObserverProjectionSeparate(t *testing.T) {
	store := newSessionTransferStore(t.TempDir())
	record := takeoverTestRecord("draining")
	record.Status.StartedAt = "2026-10-13T01:00:00Z"
	if err := store.update("key", &record, ""); err != nil {
		t.Fatal(err)
	}
	record.Status.StartedAt = "2026-10-13T02:00:00Z"
	record.Status.WaitExpired = true
	if err := store.update("key", &record, record.Status.TransferID); err != nil {
		t.Fatal(err)
	}
	got, _, _ := store.Get("key")
	if got.Status.StartedAt != "2026-10-13T01:00:00Z" || got.Status.WaitExpired {
		t.Fatal("observer reset published start time or stored its wait expiry as source state")
	}
}

func TestRejectedPreparationCannotReopenDisconnectedOrChangedSource(t *testing.T) {
	for _, change := range []string{"runtime", "epoch", "binding", "workspace", "session", "stdin", "exited", "stopped"} {
		t.Run(change, func(t *testing.T) {
			m, p, _ := newTakeoverTestProcess(t)
			status := takeoverTestRecord("preparing").Status
			p.transfer = &status
			if err := m.transfers.update(p.key, &sessionTransfer{Status: status, Direction: "outgoing", BindingInstanceID: p.bindingInstanceID}, ""); err != nil {
				t.Fatal(err)
			}
			reply := status
			reply.State = transferStatePrepareFailed
			switch change {
			case "runtime":
				p.runtimeID = "other"
			case "epoch":
				p.sessionEpoch++
			case "binding":
				p.bindingInstanceID = "other"
			case "workspace":
				p.workspacePath = "/other"
			case "session":
				p.state.SessionID = "other"
			case "stdin":
				p.stdin = nil
			case "exited":
				p.exited = true
			case "stopped":
				p.stoppedByGateway = true
			}
			if err := m.reopenUnfrozenSource(p.key, reply); err == nil || !m.transferBlocked(p.key) {
				t.Fatalf("changed source reopened: %v", err)
			}
		})
	}
}
