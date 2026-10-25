package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

func pauseIngressTestRoute(t *testing.T, s *IngressService, msg im.IncomingMessage) im.ChatIdentity {
	t.Helper()
	if err := s.PrepareAccount(context.Background(), msg.IMType, msg.AccountID); err != nil {
		t.Fatal(err)
	}
	id := im.ChatIdentity{Platform: msg.IMType, AccountID: msg.AccountID, ChatID: msg.ChatID}
	s.mu.Lock()
	s.active[id] = true
	s.mu.Unlock()
	return id
}
func dispatchIngressTestRoute(s *IngressService, id im.ChatIdentity) {
	s.slots <- struct{}{}
	s.wg.Add(1)
	s.dispatchRoute(id)
}
func TestIngressQuarantineOldRevisionDoesNotBlockNewTask(t *testing.T) {
	s, _, w, msg := ingressTestService(t)
	id := pauseIngressTestRoute(t, s, msg)
	msg.MessageID, msg.Text = "old", "old task"
	old := s.Accept(context.Background(), im.IngressEnvelope{Message: msg})
	if old.Disposition != im.ReceiveAccepted {
		t.Fatal(old)
	}
	err := s.store.UpdateRecord(context.Background(), old.RecordID, []im.IngressState{"received"}, func(r *IngressRecord) error { r.State = "quarantined"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.store.GetRoute(id)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := s.store.BeginRouteChange(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.store.FinishRouteChange(context.Background(), id, changed.Revision, before.AuthorizationDigest, before.WorkspaceID, before.SessionID); err != nil {
		t.Fatal(err)
	}
	msg.MessageID, msg.Text = "new", "new task"
	fresh := s.Accept(context.Background(), im.IngressEnvelope{Message: msg})
	if fresh.Disposition != im.ReceiveAccepted {
		t.Fatal(fresh)
	}
	s.mu.Lock()
	delete(s.active, id)
	s.mu.Unlock()
	s.signal()
	waitIngressCondition(t, func() bool { return len(w.snapshot()) == 1 })
	oldRecord, err := s.store.Get(old.RecordID)
	// Isolation keeps the record and its reason, but releases the body like
	// every settled state: an isolated input is never delivered to Chord.
	if err != nil || oldRecord.State != "quarantined" {
		t.Fatal("old input lost its isolation evidence", err)
	}
	if got := w.snapshot(); got[0]["content"] != "new task" {
		t.Fatal("old input was replayed", got)
	}
}
func TestIngressPriorityControlsPreserveSideEffectFIFO(t *testing.T) {
	s, p, _, msg := ingressTestService(t)
	w := &ingressInteractionWriter{proc: p, snapshot: StatusResponse{SessionID: "session"}}
	p.mu.Lock()
	p.stdin = w
	p.mu.Unlock()
	id := pauseIngressTestRoute(t, s, msg)
	for i, text := range []string{"! echo setup", "task", "/status", "/status", "/status", "/status"} {
		msg.MessageID, msg.Text = fmt.Sprint(i), text
		result := s.Accept(context.Background(), im.IngressEnvelope{Message: msg})
		if result.Disposition != im.ReceiveAccepted {
			t.Fatal(result)
		}
	}
	dispatchIngressTestRoute(s, id)
	var sent []map[string]any
	for _, command := range w.capture.snapshot() {
		if command["type"] != "status" {
			sent = append(sent, command)
		}
	}
	if len(sent) == 0 || sent[0]["type"] != "local_shell" {
		t.Fatalf("ordinary task overtook earlier shell control: %v", sent)
	}
}
func TestIngressReplyDoesNotWaitForFullOutboundShard(t *testing.T) {
	s, p, _, msg := ingressTestService(t)
	w := &ingressInteractionWriter{proc: p, snapshot: StatusResponse{SessionID: "session"}}
	p.mu.Lock()
	p.stdin = w
	p.mu.Unlock()
	ticket, ok := s.router.admission.Authorize(msg)
	if !ok {
		t.Fatal("authorize")
	}
	msg.SetAdmissionTicket(&ticket)
	d := s.router.outbound
	key := compositeKey(msg.IMType, msg.AccountID, msg.ChatID)
	started, release := make(chan struct{}), make(chan struct{})
	d.enqueue(key, func() { close(started); <-release })
	<-started
	var producerDone chan struct{}
	defer func() {
		close(release)
		if producerDone != nil {
			<-producerDone
		}
	}()
	for range outboundQueueSize {
		if d.enqueue(key, func() {}) != outboundQueued {
			t.Fatal("fill")
		}
	}
	producerDone = make(chan struct{})
	go func() { d.enqueueOrWait(key, func() {}); close(producerDone) }()
	waitIngressCondition(t, func() bool { return d.metrics.blocked.Load() > 0 })
	replyDone := make(chan struct{})
	go func() { s.router.enqueueIngressReply(msg, "status"); close(replyDone) }()
	select {
	case <-replyDone:
		if ingressCounter(s.store.MetricsSnapshot(), "reply_dropped") != 1 {
			t.Fatal("missing reply backpressure metric")
		}
		msg.MessageID, msg.Text = "status-during-backpressure", "/status"
		accepted := s.Accept(context.Background(), im.IngressEnvelope{Message: msg})
		if accepted.Disposition != im.ReceiveAccepted {
			t.Fatal(accepted)
		}
		waitIngressCondition(t, func() bool {
			record, err := s.store.Get(accepted.RecordID)
			return err == nil && record.State == "completed"
		})
		closed := make(chan struct{})
		go func() { s.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("ingress shutdown waited for outbound capacity")
		}
	case <-time.After(time.Second):
		t.Fatal("ingress reply blocked behind outbound producer lock")
	}
}

func TestIngressBindingPersistenceOutcomes(t *testing.T) {
	for _, stage := range []string{"before_replace", "directory_sync"} {
		t.Run(stage, func(t *testing.T) {
			r, code := pairedTestRouter(t)
			store := testIngressStore(t)
			s := NewIngressService(store, r)
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			msg := im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "owner", MessageID: "bind-new", Text: fmt.Sprintf("/bind %q %s", t.TempDir(), code)}
			id := pauseIngressTestRoute(t, s, msg)
			switch stage {
			case "before_replace":
				r.mgr.store.writer.replace = func(string, string) (CommitOutcome, error) {
					return CommitOutcome{BeforeReplace}, fmt.Errorf("injected replacement failure")
				}
			case "directory_sync":
				r.mgr.store.writer.replace = func(temp, path string) (CommitOutcome, error) {
					if err := os.Rename(temp, path); err != nil {
						return CommitOutcome{BeforeReplace}, err
					}
					return CommitOutcome{Replaced}, fmt.Errorf("injected directory sync failure")
				}

			}
			accepted := s.Accept(context.Background(), im.IngressEnvelope{Message: msg})
			if accepted.Disposition != im.ReceiveAccepted {
				t.Fatal(accepted)
			}
			dispatchIngressTestRoute(s, id)
			record, err := store.Get(accepted.RecordID)
			if err != nil {
				t.Fatal(err)
			}
			disk, err := NewConfigStore(r.mgr.store.path)
			if err != nil {
				t.Fatal(err)
			}
			route, err := store.GetRoute(id)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "before_replace" {
				if record.State != "rejected" || disk.Snapshot().IMConfigByType(msg.IMType).OwnerIdentity() != nil || route.Phase != "active" {
					t.Fatal("known pre-replacement failure was not settled", record.State, route.Phase)
				}
				// A failed first bind must not poison the nonce or leave an unbound route
				// permanently changing. A new explicit bind can complete normally.
				r.mgr.store.writer = defaultConfigWriteOps()
				pauseIngressTestRoute(t, s, msg)
				msg.MessageID = "bind-retry"
				retried := s.Accept(context.Background(), im.IngressEnvelope{Message: msg})
				if retried.Disposition != im.ReceiveAccepted {
					t.Fatal(retried)
				}
				dispatchIngressTestRoute(s, id)
				settled, err := store.Get(retried.RecordID)
				if err != nil || settled.State != "completed" {
					t.Fatal("first bind could not be retried", settled.State, err)
				}
			} else {
				if disk.Snapshot().IMConfigByType(msg.IMType).OwnerIdentity() == nil {
					t.Fatal("fixture did not replace config")
				}
				if record.State != "unknown" || route.Phase != "changing" {
					t.Fatal("uncertain binding lost recovery protection", record.State, route.Phase)
				}
			}
		})
	}
}
