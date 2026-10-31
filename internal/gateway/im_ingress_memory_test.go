package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

// TestIngressRecordSizeAccountingStaysExact walks one record through the
// transitions that mutate it (accept, dispatching, terminal text drop) and
// requires the retained accounting to equal a fresh recomputation over the
// stored records. save caches each record's encoded size, so this fails if a
// mutation site ever skips the invalidation.
func TestIngressRecordSizeAccountingStaysExact(t *testing.T) {
	s := testIngressStore(t)
	r := testIngressTask(t, s, "lifecycle")
	r.Text = strings.Repeat("body ", 512)
	accepted := mustAcceptIngress(t, s, r)

	check := func(stage string) {
		t.Helper()
		total := 0
		records, _ := s.listAll()
		for _, rec := range records {
			total += recordSize(rec)
			if rec.RecordID == accepted.RecordID && int(rec.size) != recordSize(rec) {
				t.Fatalf("%s: cached size %d diverged from encoded size %d", stage, rec.size, recordSize(rec))
			}
		}
		if s.memoryBytes != total {
			t.Fatalf("%s: accounting %d != encoded sizes %d", stage, s.memoryBytes, total)
		}
	}
	check("accepted")

	if err := s.UpdateRecord(t.Context(), accepted.RecordID, []im.IngressState{"received"}, func(r *IngressRecord) error { r.State = im.IngressDispatching; return nil }); err != nil {
		t.Fatal(err)
	}
	check("dispatching")

	if err := s.UpdateRecord(t.Context(), accepted.RecordID, []im.IngressState{"dispatching"}, func(r *IngressRecord) error { r.State = im.IngressConsumed; return nil }); err != nil {
		t.Fatal(err)
	}
	check("consumed")
}

func TestIngressRestartStartsEmpty(t *testing.T) {
	old := testIngressStore(t)
	r := testIngressTask(t, old, "same-message")
	accepted := old.AcceptRecord(t.Context(), r, testTelegramIngressCursor(1, 42))
	if accepted.Disposition != im.ReceiveAccepted {
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
	if result := fresh.AcceptRecord(t.Context(), r, nil); result.Disposition != im.ReceiveAccepted {
		t.Fatal("unexpected cross-process deduplication", result)
	}
}

func TestIngressCancelledLockWaitNeverPublishes(t *testing.T) {
	s := testIngressStore(t)
	r := testIngressTask(t, s, "cancelled")
	s.mu.Lock()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan im.ReceiveResult, 1)
	go func() { done <- s.AcceptRecord(ctx, r, testTelegramIngressCursor(1, 42)) }()
	cancel()
	result := <-done
	s.mu.Unlock()
	if result.Disposition != im.ReceiveRetryable {
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
	if result := s.AcceptRecord(t.Context(), r, nil); result.Disposition != im.ReceiveAccepted {
		t.Fatal(result)
	}
	if _, err := s.Get(old.RecordID); !errors.Is(err, errIngressMissing) {
		t.Fatal("expired history retained", err)
	}
	if _, err := s.Get(pending.RecordID); err != nil {
		t.Fatal("unresolved input silently evicted", err)
	}
	if result := s.AcceptRecord(t.Context(), testIngressTask(t, s, "terminal"), nil); result.Disposition != im.ReceiveAccepted {
		t.Fatal("expired dedup key retained", result)
	}
}

func TestIngressSnapshotsOwnControlTargets(t *testing.T) {
	s := testIngressStore(t)
	r := testIngressTask(t, s, "control")
	r.Kind = "control"
	r.ControlTarget = &im.IngressControlTarget{RequestID: "original"}
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
	if result := s.AcceptRecord(t.Context(), next, nil); result.Disposition != im.ReceiveAccepted {
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
	if err := s.UpdateRecord(t.Context(), old.RecordID, []im.IngressState{"consumed"}, func(r *IngressRecord) error { r.Reason = "updated"; return nil }); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if result := s.AcceptRecord(t.Context(), testIngressTask(t, s, "old"), nil); result.Disposition != im.ReceiveAccepted {
		t.Fatal(result)
	}
}
