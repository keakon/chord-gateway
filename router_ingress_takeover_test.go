package main

import (
	"context"
	"errors"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
)

func TestIngressTakeoverStartUsesAuthoritativeResult(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "submitted", true: "unknown"}[unknown], func(t *testing.T) {
			cfg := testConfig()
			cfg.Workspaces[0].Path = t.TempDir()
			m := newTestChordManager(cfg)
			m.transfers = newSessionTransferStore(t.TempDir())
			r := NewNotificationRouter(m)
			t.Cleanup(r.Close)
			r.ingressStore = testIngressStore(t)
			identity := im.ChatIdentity{Platform: "wechat", AccountID: "bot", ChatID: "chat"}
			before, err := r.ingressStore.EnsureRoute(context.Background(), identity, "digest", "test", "")
			if err != nil {
				t.Fatal(err)
			}
			key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
			calls := 0
			var original TransferStatus
			m.takeoverCLI = func(_ context.Context, _ string, req takeoverRequest) (TransferStatus, error) {
				calls++
				if req.Action == transferStatePreflight {
					original = TransferStatus{TransferID: req.TransferID, SessionID: req.SessionID, SourceRuntimeID: "source", SourceSessionEpoch: 3, Workspace: cfg.Workspaces[0].Path, TargetKind: "gateway", TargetInstanceID: req.TargetInstanceID, State: transferStatePreflight}
					return original, nil
				}
				guard, ok, err := m.transfers.Get(key)
				if err != nil || !ok || !sameTransferIdentity(guard.Status, original) {
					t.Fatal("mutating start lacked frozen durable source guard")
				}
				if unknown {
					return TransferStatus{}, errors.New("disconnected")
				}
				status := original
				status.State = "draining"
				return status, nil
			}
			msg := im.IncomingMessage{IMType: "wechat", ChatID: "chat"}
			msg.SetAdmissionTicket(&im.AdmissionTicket{Identity: im.ChatIdentity{Platform: "wechat", AccountID: "bot", ChatID: "chat"}})
			result := r.dispatchIngressTakeover(context.Background(), msg, key, im.IMCommand{Type: "takeover", Action: "start", SessionID: "session"}, nil)
			want := im.IngressSubmitted
			if unknown {
				want = im.IngressUnknown
			}
			after, err := r.ingressStore.GetRoute(identity)
			if err != nil {
				t.Fatal(err)
			}
			if after.Revision == before.Revision {
				t.Fatalf("transfer did not retain changed revision: %+v", after)
			}
			if !unknown && after.Phase != "changing" {
				t.Fatalf("an in-flight transfer must keep the barrier raised: %+v", after)
			}
			// An abandoned change has no observer left to settle it, and a route
			// stuck in "changing" refuses every later task with route_changing.
			if unknown && after.Phase != "active" {
				t.Fatalf("an abandoned change must settle the route: %+v", after)
			}
			if result.State != want || calls != 2 || !m.transferBlocked(key) {
				t.Fatalf("result=%+v calls=%d", result, calls)
			}
		})
	}
}

func TestIngressTakeoverPendingBlocksMutatingStart(t *testing.T) {
	cfg := testConfig()
	cfg.Workspaces[0].Path = t.TempDir()
	m := newTestChordManager(cfg)
	m.transfers = newSessionTransferStore(t.TempDir())
	r := NewNotificationRouter(m)
	t.Cleanup(r.Close)
	store := testIngressStore(t)
	r.ingressStore = store
	id := im.ChatIdentity{Platform: "wechat", AccountID: "bot", ChatID: "chat"}
	route, err := store.EnsureRoute(context.Background(), id, "digest", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	mustAcceptIngress(t, store, IngressRecord{Platform: "wechat", AccountID: "bot", ChatID: "chat", SenderID: "owner", BusinessID: "input", Kind: "task", Text: "pending", RouteRevision: route.Revision, AuthorizationDigest: "digest", WorkspaceID: "test"})
	calls := 0
	m.takeoverCLI = func(_ context.Context, _ string, req takeoverRequest) (TransferStatus, error) {
		calls++
		if req.Action != transferStatePreflight {
			t.Fatal("mutating start passed pending barrier")
		}
		return TransferStatus{TransferID: req.TransferID, SessionID: req.SessionID, SourceRuntimeID: "source", SourceSessionEpoch: 3, Workspace: cfg.Workspaces[0].Path, TargetKind: "gateway", TargetInstanceID: req.TargetInstanceID, State: transferStatePreflight}, nil
	}
	key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
	msg := im.IncomingMessage{IMType: "wechat", ChatID: "chat"}
	msg.SetAdmissionTicket(&im.AdmissionTicket{Identity: id})
	got := r.dispatchIngressTakeover(context.Background(), msg, key, im.IMCommand{Type: "takeover", Action: "start", SessionID: "session"}, nil)
	if got.State != "rejected" || got.Reason != "undelivered_inputs" || calls != 1 || m.transferBlocked(key) {
		t.Fatalf("result=%+v calls=%d", got, calls)
	}
}

func TestIngressTakeoverStatusRejectsReplacementTransfer(t *testing.T) {
	m := newTestChordManager(testConfig())
	m.transfers = newSessionTransferStore(t.TempDir())
	r := NewNotificationRouter(m)
	t.Cleanup(r.Close)
	key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
	record := takeoverTestRecord("draining")
	if err := m.transfers.update(key, &record, ""); err != nil {
		t.Fatal(err)
	}
	m.takeoverCLI = func(context.Context, string, takeoverRequest) (TransferStatus, error) {
		t.Fatal("stale status queried replacement transfer")
		return TransferStatus{}, nil
	}
	got := r.dispatchIngressTakeover(context.Background(), im.IncomingMessage{IMType: "wechat", ChatID: "chat"}, key, im.IMCommand{Type: "takeover", Action: "status"}, &im.IngressControlTarget{TransferID: "old-transfer"})
	if got.State != "rejected" || got.Reason != "control_target_expired" {
		t.Fatal(got)
	}
}

// A takeover step that fails after the barrier is raised must settle the route.
// Otherwise the chat refuses every later task with route_changing forever, and
// WeChat never advances its polling batch past that message.
func TestIngressTakeoverAbandonedChangeSettlesRoute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status TransferStatus
		err    error
	}{
		{name: "receipt identity mismatch", status: TransferStatus{}, err: nil},
		{name: "cli failure", status: TransferStatus{}, err: errors.New("disconnected")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Workspaces[0].Path = t.TempDir()
			m := newTestChordManager(cfg)
			m.transfers = newSessionTransferStore(t.TempDir())
			r := NewNotificationRouter(m)
			t.Cleanup(r.Close)
			r.ingressStore = testIngressStore(t)
			identity := im.ChatIdentity{Platform: "wechat", AccountID: "bot", ChatID: "chat"}
			if _, err := r.ingressStore.EnsureRoute(context.Background(), identity, "digest", "test", ""); err != nil {
				t.Fatal(err)
			}
			key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
			var original TransferStatus
			m.takeoverCLI = func(_ context.Context, _ string, req takeoverRequest) (TransferStatus, error) {
				if req.Action == transferStatePreflight {
					original = TransferStatus{TransferID: req.TransferID, SessionID: req.SessionID, SourceRuntimeID: "source", SourceSessionEpoch: 3, Workspace: cfg.Workspaces[0].Path, TargetKind: "gateway", TargetInstanceID: req.TargetInstanceID, State: transferStatePreflight}
					return original, nil
				}
				return tc.status, tc.err
			}
			msg := im.IncomingMessage{IMType: "wechat", ChatID: "chat"}
			msg.SetAdmissionTicket(&im.AdmissionTicket{Identity: identity})
			if result := r.dispatchIngressTakeover(context.Background(), msg, key, im.IMCommand{Type: "takeover", Action: "start", SessionID: "session"}, nil); result.State != "unknown" {
				t.Fatalf("expected an unknown outcome, got %+v", result)
			}
			route, err := r.ingressStore.GetRoute(identity)
			if err != nil {
				t.Fatal(err)
			}
			if route.Phase != "active" {
				t.Fatalf("abandoned change left the route %q; it would refuse every later task", route.Phase)
			}
		})
	}
}

// Failing to persist the transfer guard happens after the barrier is raised, so
// the route must be settled on that exit too: the failure is reported as an
// unknown outcome and the chat must not keep refusing later tasks.
func TestIngressTakeoverGuardCommitFailureSettlesRoute(t *testing.T) {
	cfg := testConfig()
	cfg.Workspaces[0].Path = t.TempDir()
	m := newTestChordManager(cfg)
	m.transfers = newSessionTransferStore(t.TempDir())
	r := NewNotificationRouter(m)
	t.Cleanup(r.Close)
	r.ingressStore = testIngressStore(t)
	identity := im.ChatIdentity{Platform: "wechat", AccountID: "bot", ChatID: "chat"}
	if _, err := r.ingressStore.EnsureRoute(context.Background(), identity, "digest", "test", ""); err != nil {
		t.Fatal(err)
	}
	key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
	m.takeoverCLI = func(_ context.Context, _ string, req takeoverRequest) (TransferStatus, error) {
		return TransferStatus{TransferID: req.TransferID, SessionID: req.SessionID, SourceRuntimeID: "source", SourceSessionEpoch: 3, Workspace: cfg.Workspaces[0].Path, TargetKind: "gateway", TargetInstanceID: req.TargetInstanceID, State: transferStatePreflight}, nil
	}
	m.transfers.writer = func(string, []byte) error { return errors.New("disk full") }
	msg := im.IncomingMessage{IMType: "wechat", ChatID: "chat"}
	msg.SetAdmissionTicket(&im.AdmissionTicket{Identity: identity})
	result := r.dispatchIngressTakeover(context.Background(), msg, key, im.IMCommand{Type: "takeover", Action: "start", SessionID: "session"}, nil)
	if result.State != "unknown" || result.Reason != "transfer_guard_commit_unknown" {
		t.Fatalf("expected the guard commit failure to be reported, got %+v", result)
	}
	route, err := r.ingressStore.GetRoute(identity)
	if err != nil {
		t.Fatal(err)
	}
	if route.Phase != "active" {
		t.Fatalf("guard commit failure left the route %q; it would refuse every later task", route.Phase)
	}
}
