package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestIngressPendingMetadataTracksAtomicTransitions(t *testing.T) {
	s := testIngressStore(t)
	task := testIngressTask(t, s, "metadata")
	task.Text = strings.Repeat("private input ", 1000)
	record := mustAcceptIngress(t, s, task)
	assertMetadata := func(state ingressState) {
		t.Helper()
		records := s.listPendingMetadata()
		route := s.ListRoutePendingMetadata(record.Identity())
		if !reflect.DeepEqual(records, route) {
			t.Fatal("global and route metadata disagree")
		}
		if state == "consumed" {
			if len(records) != 0 {
				t.Fatal("terminal input remains scheduled")
			}
			return
		}
		want, err := s.Get(record.RecordID)
		if err != nil {
			t.Fatal(err)
		}
		if want.Text == "" {
			t.Fatal("pending body was lost")
		}
		want.Text = ""
		if len(records) != 1 || records[0].State != state || !reflect.DeepEqual(want, records[0]) {
			t.Fatal("metadata differs from in-memory input")
		}
	}
	assertMetadata("received")
	failure := errors.New("rollback")
	if err := s.UpdateRecord(t.Context(), record.RecordID, []ingressState{"received"}, func(r *IngressRecord) error { r.State = "consumed"; return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	assertMetadata("received")
	for _, state := range []ingressState{"dispatching", "unknown", "consumed"} {
		if err := s.UpdateRecord(context.Background(), record.RecordID, []ingressState{"received", "dispatching", "unknown"}, func(r *IngressRecord) error { r.State = state; return nil }); err != nil {
			t.Fatal(err)
		}
		assertMetadata(state)
	}
}

// Compare the scheduler's previous full-record read with its metadata read on
// the same in-memory queue, including realistic retained input bodies.
func BenchmarkIngressPendingScan(b *testing.B) {
	s := NewIngressStore()
	b.Cleanup(func() {
		if err := s.Close(); err != nil {
			b.Error(err)
		}
	})
	body := strings.Repeat("x", 60<<10)
	for i := range 256 {
		id := ChatIdentity{"telegram", "bot", fmt.Sprint(i / 32)}
		route, err := s.EnsureRoute(context.Background(), id, "digest", "workspace", "session")
		if err != nil {
			b.Fatal(err)
		}
		result := s.AcceptRecord(context.Background(), IngressRecord{Platform: id.Platform, AccountID: id.AccountID, ChatID: id.ChatID, SenderID: "owner", BusinessID: fmt.Sprint(i), Kind: "task", Text: body, RouteRevision: route.Revision, AuthorizationDigest: route.AuthorizationDigest, WorkspaceID: route.WorkspaceID, SessionID: route.SessionID}, nil)
		if result.Disposition != ReceiveAccepted {
			b.Fatal(result)
		}
	}
	for _, mode := range []string{"full_records", "metadata"} {
		b.Run(mode, func(b *testing.B) {
			read := s.listPending
			if mode == "metadata" {
				read = func() ([]IngressRecord, error) { return s.listPendingMetadata(), nil }
			}
			b.ReportAllocs()
			for b.Loop() {
				records, err := read()
				if err != nil || len(records) != 256 {
					b.Fatal(len(records), err)
				}
			}
		})
	}
}

func BenchmarkIngressAcceptRetainedHistory(b *testing.B) {
	for _, count := range []int{0, 1000, 10000, 50000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s := NewIngressStore()
			defer s.Close()
			now := time.Now()
			for i := 0; i < count; i++ {
				s.save(IngressRecord{RecordID: fmt.Sprint(i), Platform: "slack", AccountID: "bot", ChatID: "chat", SenderID: "owner", Kind: "task", State: "consumed", UpdatedAt: now})
			}
			record := IngressRecord{Platform: "slack", AccountID: "bot", ChatID: "chat", SenderID: "owner", Kind: "task", State: "rejected", BusinessID: "probe"}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				result := s.AcceptRecord(context.Background(), record, nil)
				if result.Disposition != ReceiveRejected {
					b.Fatal(result)
				}
				// Remove only the probe, keeping the same retained-history size each iteration.
				s.mu.Lock()
				old := s.records[result.RecordID]
				s.adjustCounts(old, -1)
				delete(s.records, result.RecordID)
				delete(s.byRoute[record.Identity()], result.RecordID)
				delete(s.business, compositeKey("slack", "bot", "probe"))
				s.history = s.history[:len(s.history)-1]
				s.mu.Unlock()
			}
		})
	}
}

func TestIngressRoutePendingCapacityAndCleanup(t *testing.T) {
	s := testIngressStore(t)
	task := testIngressTask(t, s, "first")
	first := mustAcceptIngress(t, s, task)
	for i := 1; i < 64; i++ {
		task.BusinessID = fmt.Sprint(i)
		mustAcceptIngress(t, s, task)
	}
	check := func(want int) {
		t.Helper()
		pending, _ := s.listRoutePending(first.Identity())
		if len(pending) != want || len(s.pendingByRoute[first.Identity()]) != want || len(s.routePending(first.Identity())) != want {
			t.Fatalf("pending=%d want=%d", len(pending), want)
		}
	}
	check(64)
	if err := s.UpdateRecord(t.Context(), first.RecordID, []ingressState{"received"}, func(r *IngressRecord) error { r.State = "quarantined"; return nil }); err != nil {
		t.Fatal(err)
	}
	// Isolation is settled input: it keeps its evidence until the history TTL
	// expires, so it must not keep holding a pending slot the chat still needs.
	check(63)
	task.BusinessID = "overflow"
	if got := s.AcceptRecord(t.Context(), task, nil); got.Disposition != ReceiveAccepted {
		t.Fatalf("isolation still held a slot: %+v", got)
	}
	check(64)
	if err := s.Discard(t.Context(), first.RecordID); err != nil {
		t.Fatal(err)
	}
	check(64)
	// Terminal metadata updates must not recreate a pending entry.
	if err := s.UpdateRecord(t.Context(), first.RecordID, []ingressState{"consumed"}, func(r *IngressRecord) error { r.Reason = "settled"; return nil }); err != nil {
		t.Fatal(err)
	}
	check(64)
	s.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	s.pruneHistory(first.Identity())
	check(64)
	if _, err := s.Get(first.RecordID); !errors.Is(err, errIngressMissing) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if len(s.pendingByRoute) != 0 {
		t.Fatal("close retained pending routes")
	}
}

func TestIngressPendingRoutesTracksIndexInIdentityOrder(t *testing.T) {
	s := testIngressStore(t)
	if routes := s.PendingRoutes(); len(routes) != 0 {
		t.Fatalf("empty store reported routes: %v", routes)
	}
	first := mustAcceptIngress(t, s, testIngressTask(t, s, "first"))
	second := testIngressTask(t, s, "second")
	second.ChatID = "chat-2"
	route, err := s.EnsureRoute(t.Context(), second.Identity(), "digest-2", "workspace", "session")
	if err != nil {
		t.Fatal(err)
	}
	second.RouteRevision, second.AuthorizationDigest, second.WorkspaceID, second.SessionID = route.Revision, route.AuthorizationDigest, route.WorkspaceID, route.SessionID
	mustAcceptIngress(t, s, second)
	routes := s.PendingRoutes()
	if len(routes) != 2 || routes[0] != first.Identity() || routes[1] != second.Identity() {
		t.Fatalf("routes not ordered by identity: %v", routes)
	}
	if err := s.Discard(t.Context(), first.RecordID); err != nil {
		t.Fatal(err)
	}
	if routes = s.PendingRoutes(); len(routes) != 1 || routes[0] != second.Identity() {
		t.Fatalf("settled route retained: %v", routes)
	}
}
