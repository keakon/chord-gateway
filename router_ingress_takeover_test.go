package main

import (
	"context"
	"errors"
	"testing"
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
			identity := ChatIdentity{"wechat", "bot", "chat"}
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
			msg := IncomingMessage{IMType: "wechat", ChatID: "chat", ticket: &AdmissionTicket{Identity: ChatIdentity{"wechat", "bot", "chat"}}}
			result := r.dispatchIngressTakeover(context.Background(), msg, key, IMCommand{Type: "takeover", Action: "start", SessionID: "session"}, nil)
			want := "submitted"
			if unknown {
				want = "unknown"
			}
			after, err := r.ingressStore.GetRoute(identity)
			if err != nil || after.Revision == before.Revision || after.Phase != "changing" {
				t.Fatalf("transfer did not retain changed revision: %+v %v", after, err)
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
	id := ChatIdentity{"wechat", "bot", "chat"}
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
	msg := IncomingMessage{IMType: "wechat", ChatID: "chat", ticket: &AdmissionTicket{Identity: id}}
	got := r.dispatchIngressTakeover(context.Background(), msg, key, IMCommand{Type: "takeover", Action: "start", SessionID: "session"}, nil)
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
	got := r.dispatchIngressTakeover(context.Background(), IncomingMessage{IMType: "wechat", ChatID: "chat"}, key, IMCommand{Type: "takeover", Action: "status"}, &IngressControlTarget{TransferID: "old-transfer"})
	if got.State != "rejected" || got.Reason != "control_target_expired" {
		t.Fatal(got)
	}
}
