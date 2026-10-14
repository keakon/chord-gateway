package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIngressRestartStartsEmpty(t *testing.T) {
	old := testIngressStore(t)
	r := testIngressTask(t, old, "same-message")
	accepted := old.AcceptRecord(t.Context(), r, testTelegramIngressCursor(1, 42))
	if accepted.Disposition != ReceiveAccepted {
		t.Fatal(accepted)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	fresh := testIngressStore(t)
	if records, _ := fresh.listAll(); len(records) != 0 {
		t.Fatal("replayed a previous process queue")
	}
	if cursor, _ := fresh.Cursor("telegram", "bot"); cursor.Generation != 0 {
		t.Fatal("restored a polling cursor")
	}
	r = testIngressTask(t, fresh, "same-message")
	if result := fresh.AcceptRecord(t.Context(), r, nil); result.Disposition != ReceiveAccepted {
		t.Fatal("unexpected cross-process deduplication", result)
	}
}

func TestIngressCancelledLockWaitNeverPublishes(t *testing.T) {
	s := testIngressStore(t)
	r := testIngressTask(t, s, "cancelled")
	s.mu.Lock()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan ReceiveResult, 1)
	go func() { done <- s.AcceptRecord(ctx, r, testTelegramIngressCursor(1, 42)) }()
	cancel()
	result := <-done
	s.mu.Unlock()
	if result.Disposition != ReceiveRetryable {
		t.Fatal(result)
	}
	if records, _ := s.listAll(); len(records) != 0 {
		t.Fatal("cancelled input retained")
	}
	if cursor, _ := s.Cursor("telegram", "bot"); cursor.Generation != 0 {
		t.Fatal("cancelled input advanced cursor")
	}
}

func TestIngressHistoryExpiryAndPendingRetention(t *testing.T) {
	s := testIngressStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	old := mustAcceptIngress(t, s, testIngressTask(t, s, "terminal"))
	if err := s.Discard(t.Context(), old.RecordID); err != nil {
		t.Fatal(err)
	}
	pending := mustAcceptIngress(t, s, testIngressTask(t, s, "pending"))
	now = now.Add(ingressHistoryTTL + time.Second)
	r := testIngressTask(t, s, "next")
	if result := s.AcceptRecord(t.Context(), r, nil); result.Disposition != ReceiveAccepted {
		t.Fatal(result)
	}
	if _, err := s.Get(old.RecordID); !errors.Is(err, errIngressMissing) {
		t.Fatal("expired history retained", err)
	}
	if _, err := s.Get(pending.RecordID); err != nil {
		t.Fatal("unresolved input silently evicted", err)
	}
	if result := s.AcceptRecord(t.Context(), testIngressTask(t, s, "terminal"), nil); result.Disposition != ReceiveAccepted {
		t.Fatal("expired dedup key retained", result)
	}
}

func TestIngressSnapshotsOwnControlTargets(t *testing.T) {
	s := testIngressStore(t)
	r := testIngressTask(t, s, "control")
	r.Kind = "control"
	r.ControlTarget = &IngressControlTarget{RequestID: "original"}
	accepted := mustAcceptIngress(t, s, r)
	r.ControlTarget.RequestID = "changed"
	accepted.ControlTarget.RequestID = "changed-again"
	stored, err := s.Get(accepted.RecordID)
	if err != nil || stored.ControlTarget.RequestID != "original" {
		t.Fatal(stored, err)
	}
}

func TestIngressExpiryKeepsPreparedRoute(t *testing.T) {
	s := testIngressStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	old := mustAcceptIngress(t, s, testIngressTask(t, s, "old"))
	if err := s.Discard(t.Context(), old.RecordID); err != nil {
		t.Fatal(err)
	}
	// The only old entry expires between preparing and publishing the next input.
	next := testIngressTask(t, s, "new")
	now = now.Add(ingressHistoryTTL + time.Second)
	if result := s.AcceptRecord(t.Context(), next, nil); result.Disposition != ReceiveAccepted {
		t.Fatal(result)
	}
}

func TestIngressTerminalUpdatesDoNotExtendDeduplication(t *testing.T) {
	s := testIngressStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	old := mustAcceptIngress(t, s, testIngressTask(t, s, "old"))
	if err := s.Discard(t.Context(), old.RecordID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(ingressHistoryTTL - time.Second)
	if err := s.UpdateRecord(t.Context(), old.RecordID, []string{"consumed"}, func(r *IngressRecord) error { r.Reason = "updated"; return nil }); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if result := s.AcceptRecord(t.Context(), testIngressTask(t, s, "old"), nil); result.Disposition != ReceiveAccepted {
		t.Fatal(result)
	}
}
