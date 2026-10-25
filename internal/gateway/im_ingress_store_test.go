package gateway

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

func testIngressStore(t *testing.T) *IngressStore {
	t.Helper()
	s := NewIngressStore()
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func testIngressTask(t *testing.T, s *IngressStore, business string) IngressRecord {
	t.Helper()
	id := im.ChatIdentity{Platform: "telegram", AccountID: "bot", ChatID: "chat"}
	route, err := s.EnsureRoute(context.Background(), id, "digest", "workspace", "session")
	if err != nil {
		t.Fatal(err)
	}
	return IngressRecord{Platform: id.Platform, AccountID: id.AccountID, ChatID: id.ChatID, SenderID: "owner", BusinessID: business, Kind: "task", Text: "task body", RouteRevision: route.Revision, AuthorizationDigest: route.AuthorizationDigest, WorkspaceID: route.WorkspaceID, SessionID: route.SessionID}
}
func mustAcceptIngress(t *testing.T, s *IngressStore, r IngressRecord) IngressRecord {
	t.Helper()
	result := s.AcceptRecord(context.Background(), r, nil)
	if result.Disposition != im.ReceiveAccepted {
		t.Fatalf("accept: %+v", result)
	}
	got, err := s.Get(result.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestIngressStoreConcurrentBusinessAndSemanticDedupe(t *testing.T) {
	s := testIngressStore(t)
	input := testIngressTask(t, s, "business")
	input.SemanticID = "original-message"
	var wg sync.WaitGroup
	results := make(chan im.ReceiveResult, 20)
	for range 20 {
		wg.Go(func() { results <- s.AcceptRecord(context.Background(), input, nil) })
	}
	wg.Wait()
	close(results)
	accepted := 0
	id := ""
	for result := range results {
		if result.Disposition == im.ReceiveAccepted {
			accepted++
		} else if result.Disposition != im.ReceiveDuplicate {
			t.Fatalf("unexpected result: %+v", result)
		}
		if id == "" {
			id = result.RecordID
		}
		if result.RecordID != id {
			t.Fatal("duplicate created another record")
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted=%d", accepted)
	}
	before, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	input.BusinessID = "second-delivery"
	input.Text = "replacement body"
	input.ChatID = "different-chat"
	duplicate := s.AcceptRecord(context.Background(), input, nil)
	// Telegram retains the new delivery identity even without a body digest;
	// it must not overwrite or execute the original semantic input.
	if duplicate.Disposition != im.ReceiveDuplicate || duplicate.RecordID == id || duplicate.Reason != "semantic_duplicate" {
		t.Fatalf("semantic duplicate: %+v", duplicate)
	}
	after, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("duplicate rewrote first acceptance")
	}
	records, err := s.listAll()
	if err != nil || len(records) != 2 {
		t.Fatalf("records=%v err=%v", records, err)
	}
}

func TestIngressStoreSourceTimeAndRejectedDoNotRevive(t *testing.T) {
	for _, age := range []time.Duration{-25 * time.Hour, 6 * time.Minute} {
		t.Run(age.String(), func(t *testing.T) {
			s := testIngressStore(t)
			r := testIngressTask(t, s, "old")
			r.SourceTime = time.Now().Add(age)
			result := s.AcceptRecord(context.Background(), r, nil)
			if result.Disposition != im.ReceiveRejected || result.State != "rejected" || result.RecordID == "" {
				t.Fatalf("missing rejection tombstone: %+v", result)
			}
			original, err := s.Get(result.RecordID)
			if err != nil {
				t.Fatal(err)
			}
			if original.Text != "" {
				t.Fatal("rejection retained body")
			}
			r.SourceTime = time.Time{}
			r.Text = "now authorized"
			duplicate := s.AcceptRecord(context.Background(), r, nil)
			if duplicate.Disposition != im.ReceiveDuplicate || duplicate.State != "rejected" || duplicate.Reason != original.Reason {
				t.Fatalf("rejection revived: %+v", duplicate)
			}
		})
	}
	s := testIngressStore(t)
	r := testIngressTask(t, s, "unbound")
	r.State = "rejected"
	r.Reason = "chat_unbound"
	result := s.AcceptRecord(context.Background(), r, nil)
	if result.Disposition != im.ReceiveRejected {
		t.Fatal(result)
	}
	if _, err := s.EnsureRoute(context.Background(), r.Identity(), "new-digest", "new-workspace", "new-session"); err != nil {
		t.Fatal(err)
	}
	r.State = "received"
	r.Reason = ""
	duplicate := s.AcceptRecord(context.Background(), r, nil)
	if duplicate.State != "rejected" || duplicate.Reason != "chat_unbound" {
		t.Fatalf("bind revived rejection: %+v", duplicate)
	}
}

func TestIngressStoreCursorAtomicRollbackAndGeneration(t *testing.T) {
	s := testIngressStore(t)
	r := testIngressTask(t, s, "first")
	result := s.AcceptRecord(context.Background(), r, testTelegramIngressCursor(1, 10))
	if result.Disposition != im.ReceiveAccepted {
		t.Fatal(result)
	}
	if result = s.AcceptRecord(context.Background(), r, testTelegramIngressCursor(2, 9)); result.Disposition != im.ReceiveDuplicate {
		t.Fatal(result)
	}
	cursor, err := s.Cursor(r.Platform, r.AccountID)
	if err != nil || *cursor.Telegram.PendingOffset != 9 {
		t.Fatalf("cursor regressed: %+v %v", cursor, err)
	}
	r.BusinessID = "rolled-back"
	result = s.acceptRecord(context.Background(), r, testTelegramIngressCursor(3, 11), func() bool { return false })
	if result.Disposition != im.ReceiveRetryable && result.Disposition != im.ReceiveUncertain {
		t.Fatalf("failed transaction accepted: %+v", result)
	}
	cursor, err = s.Cursor(r.Platform, r.AccountID)
	if err != nil || *cursor.Telegram.PendingOffset != 9 {
		t.Fatalf("cursor committed without record: %+v %v", cursor, err)
	}
	records, err := s.listAll()
	if err != nil || len(records) != 1 {
		t.Fatalf("rollback retained record: %d %v", len(records), err)
	}
}

// Isolation is settled input, so it must be reclaimed by the same history TTL
// as a delivered message. Holding it as pending instead strands the chat: once
// enough inputs are isolated, every later message is refused as over capacity
// until the process restarts.
func TestIngressStoreIsolationIsSettledAndReclaimed(t *testing.T) {
	s := testIngressStore(t)
	r := testIngressTask(t, s, "0")
	var first IngressRecord
	for i := range 64 {
		r.BusinessID = fmt.Sprint(i)
		got := mustAcceptIngress(t, s, r)
		if i == 0 {
			first = got
		}
	}
	if err := s.UpdateRecord(context.Background(), first.RecordID, []im.IngressState{"received"}, func(r *IngressRecord) error { r.State = "quarantined"; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := len(s.pendingByRoute[first.Identity()]); got != 63 {
		t.Fatalf("isolation held a pending slot: %d", got)
	}
	// It still costs retention and its own evidence until the TTL expires.
	if len(s.records) != 64 || s.memoryBytes == 0 {
		t.Fatalf("isolation skipped retention accounting: %d %d", len(s.records), s.memoryBytes)
	}
	if got, err := s.Get(first.RecordID); err != nil || got.State != "quarantined" {
		t.Fatalf("isolation lost its evidence: %+v %v", got, err)
	}
	s.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	s.pruneHistory(first.Identity())
	if _, err := s.Get(first.RecordID); !errors.Is(err, errIngressMissing) {
		t.Fatal("isolation was never reclaimed", err)
	}
	// The freed slot is usable again, and the reclaimed input consumed no
	// sequence of its own.
	r.BusinessID = "after-reclaim"
	accepted := mustAcceptIngress(t, s, r)
	if accepted.Sequence != 65 {
		t.Fatalf("isolation consumed a sequence: %d", accepted.Sequence)
	}
}

func TestIngressStoreTerminalClearsBodyAndControlNeverRetainsSecrets(t *testing.T) {
	for _, state := range []im.IngressState{"consumed", "rejected"} {
		t.Run(string(state), func(t *testing.T) {
			s := testIngressStore(t)
			r := mustAcceptIngress(t, s, testIngressTask(t, s, "body"))
			if err := s.UpdateRecord(context.Background(), r.RecordID, []im.IngressState{"received"}, func(r *IngressRecord) error { r.State = state; return nil }); err != nil {
				t.Fatal(err)
			}
			got, err := s.Get(r.RecordID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Text != "" {
				t.Fatalf("terminal body policy: %+v", got)
			}
		})
	}
	s := testIngressStore(t)
	r := testIngressTask(t, s, "pairing")
	r.Kind = "control"
	r.Text = "/bind secret-pairing-code"
	got := mustAcceptIngress(t, s, r)
	if got.Text != "" {
		t.Fatal("control retained sensitive content")
	}
}

func TestIngressStoreRouteRevisionNeverRevives(t *testing.T) {
	s := testIngressStore(t)
	r := testIngressTask(t, s, "old")
	first := mustAcceptIngress(t, s, r)
	same, err := s.EnsureRoute(context.Background(), r.Identity(), "digest", "workspace", "session")
	if err != nil || same.Revision != r.RouteRevision {
		t.Fatalf("idempotent route changed: %+v %v", same, err)
	}
	if _, err = s.BeginRouteChange(context.Background(), r.Identity()); !errors.Is(err, errIngressBlocked) {
		t.Fatalf("pending task bypassed barrier: %v", err)
	}
	changed, err := s.EnsureRoute(context.Background(), r.Identity(), "other-digest", "other-workspace", "other-session")
	if err != nil {
		t.Fatal(err)
	}
	returned, err := s.EnsureRoute(context.Background(), r.Identity(), "digest", "workspace", "session")
	if err != nil {
		t.Fatal(err)
	}
	if returned.Revision == r.RouteRevision || returned.Revision == changed.Revision {
		t.Fatal("A-B-A revived revision")
	}
	got, err := s.Get(first.RecordID)
	if err != nil || got.State != "quarantined" {
		t.Fatalf("old task not isolated: %+v %v", got, err)
	}
	stale := s.AcceptRecord(context.Background(), r, nil)
	if stale.Disposition != im.ReceiveDuplicate {
		t.Fatal(stale)
	}
	r.BusinessID = "stale-ticket"
	if result := s.AcceptRecord(context.Background(), r, nil); result.Disposition != im.ReceiveRetryable || result.Reason != "route_changed" {
		t.Fatalf("stale revision accepted: %+v", result)
	}
	barrier, err := s.BeginRouteChange(context.Background(), r.Identity())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishRouteChange(context.Background(), r.Identity(), returned.Revision, "digest", "workspace", "session"); !errors.Is(err, errIngressBlocked) {
		t.Fatalf("stale barrier finished: %v", err)
	}
	if err = s.FinishRouteChange(context.Background(), r.Identity(), barrier.Revision, "digest", "workspace", "session"); err != nil {
		t.Fatal(err)
	}
}

func TestIngressStoreCancelledBeforeTransactionDoesNotCommit(t *testing.T) {
	s := testIngressStore(t)
	r := testIngressTask(t, s, "cancelled")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := s.AcceptRecord(ctx, r, nil)
	if result.Disposition != im.ReceiveRetryable {
		t.Fatalf("cancelled admission: %+v", result)
	}
	records, err := s.listAll()
	if err != nil || len(records) != 0 {
		t.Fatalf("cancelled input committed: %d %v", len(records), err)
	}
}

func TestIngressPendingIndexesTrackTransitions(t *testing.T) {
	s := testIngressStore(t)
	r := mustAcceptIngress(t, s, testIngressTask(t, s, "index"))
	all, err := s.listPending()
	if err != nil || len(all) != 1 {
		t.Fatalf("%v %v", all, err)
	}
	route, err := s.listRoutePending(r.Identity())
	if err != nil || len(route) != 1 {
		t.Fatalf("%v %v", route, err)
	}
	if err = s.UpdateRecord(context.Background(), r.RecordID, []im.IngressState{"received"}, func(r *IngressRecord) error { r.State = "unknown"; return nil }); err != nil {
		t.Fatal(err)
	}
	all, err = s.listPending()
	if err != nil || len(all) != 1 || all[0].State != "unknown" {
		t.Fatalf("%v %v", all, err)
	}
	if err = s.Discard(context.Background(), r.RecordID); err != nil {
		t.Fatal(err)
	}
	all, err = s.listPending()
	if err != nil || len(all) != 0 {
		t.Fatalf("terminal in scheduler: %v %v", all, err)
	}
	route, err = s.listRoutePending(r.Identity())
	if err != nil || len(route) != 0 {
		t.Fatalf("terminal in route: %v %v", route, err)
	}
	history, err := s.ListRouteRecords(r.Identity())
	if err != nil || len(history) != 1 {
		t.Fatal("lost dedupe history")
	}
}

func testTelegramIngressCursor(generation uint64, offset int64) *im.IngressCursor {
	return &im.IngressCursor{Kind: "telegram", Version: 1, Generation: generation, Telegram: &im.TelegramIngressCursor{PendingOffset: &offset, ObservedAt: time.Now(), State: "pending"}}
}

type testIngressReceiverFunc func(context.Context, im.IngressEnvelope) im.ReceiveResult

func (f testIngressReceiverFunc) Accept(ctx context.Context, e im.IngressEnvelope) im.ReceiveResult {
	return f(ctx, e)
}

// Discard is a test-only shortcut for releasing a pending fixture.
func (s *IngressStore) Discard(ctx context.Context, id string) error {
	return s.UpdateRecord(ctx, id, []im.IngressState{"received", "dispatching", "unknown", "quarantined"}, func(r *IngressRecord) error { r.State = "consumed"; r.Reason = "local_discard"; return nil })
}
