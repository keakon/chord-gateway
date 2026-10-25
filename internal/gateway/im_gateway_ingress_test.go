package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

type ingressReadinessAdapter struct {
	im.IMAdapter
	mu    sync.Mutex
	ready bool
	wake  func()
}

func TestGatewayIngressUnboundOwnerAndBindUseFlowReplies(t *testing.T) {
	s, _, _, msg := ingressTestService(t)
	adapter := s.router.currentAdapter().(*pairingTestAdapter)
	msg.ChatID, msg.MessageID, msg.Text = "new-chat", "unbound-task", "task"
	result := s.router.ReceiveIngress(t.Context(), im.IngressEnvelope{Message: msg})
	if result.Disposition != im.ReceiveRejected || result.Reason != "chat_unbound" || result.RecordID == "" {
		t.Fatal(result)
	}
	waitIngressCondition(t, func() bool {
		adapter.mu.Lock()
		defer adapter.mu.Unlock()
		for _, text := range adapter.messages {
			if strings.Contains(text, "This chat is not bound") {
				return true
			}
		}
		return false
	})
	msg.MessageID, msg.Text = "new-binding", fmt.Sprintf("/bind %q", t.TempDir())
	bound := s.router.ReceiveIngress(t.Context(), im.IngressEnvelope{Message: msg})
	if bound.Disposition != im.ReceiveAccepted {
		t.Fatal(bound)
	}
	waitIngressCondition(t, func() bool {
		rec, err := s.store.Get(bound.RecordID)
		return err == nil && rec.State == "completed"
	})
	waitIngressCondition(t, func() bool {
		adapter.mu.Lock()
		defer adapter.mu.Unlock()
		for _, text := range adapter.messages {
			if strings.HasPrefix(text, "Bound to ") {
				return true
			}
		}
		return false
	})
	// Binding later never resurrects the rejected original message.
	duplicate := s.router.ReceiveIngress(t.Context(), im.IngressEnvelope{Message: im.IncomingMessage{IMType: msg.IMType, AccountID: msg.AccountID, ChatKind: msg.ChatKind, ChatID: msg.ChatID, SenderID: msg.SenderID, MessageID: "unbound-task", Text: "task"}})
	if duplicate.Disposition != im.ReceiveDuplicate || duplicate.State != "rejected" {
		t.Fatal(duplicate)
	}
}

func (a *ingressReadinessAdapter) Type() string { return "wechat" }
func (a *ingressReadinessAdapter) IngressDeliveryReadiness(im.ChatIdentity) im.IngressDeliveryReadiness {
	a.mu.Lock()
	defer a.mu.Unlock()
	return im.IngressDeliveryReadiness{Ready: a.ready, Reason: im.IngressWaitingReplyContext}
}
func (a *ingressReadinessAdapter) SetIngressReadinessNotifier(wake func()) {
	a.mu.Lock()
	a.wake = wake
	a.mu.Unlock()
}
func (a *ingressReadinessAdapter) setReady() {
	a.mu.Lock()
	a.ready = true
	wake := a.wake
	a.mu.Unlock()
	if wake != nil {
		wake()
	}
}

func TestGatewayIngressWaitingPreservesFIFOAndWakes(t *testing.T) {
	s, p, w, msg := ingressTestService(t)
	a := &ingressReadinessAdapter{IMAdapter: s.router.currentAdapter()}
	s.router.SetAdapter(a)
	var accepted []im.ReceiveResult
	for _, content := range []string{"first", "second"} {
		msg.MessageID, msg.Text = content, content
		result := s.router.ReceiveIngress(context.Background(), im.IngressEnvelope{Message: msg})
		if result.Disposition != im.ReceiveAccepted {
			t.Fatal(result)
		}
		accepted = append(accepted, result)
	}
	// Dispatch deterministically while the prerequisite is missing.
	for _, result := range accepted {
		rec, err := s.store.Get(result.RecordID)
		if err != nil {
			t.Fatal(err)
		}
		s.dispatchTask(rec)
		still, err := s.store.Get(result.RecordID)
		if err != nil || still.State != "received" || still.Sequence != rec.Sequence || !still.ReceivedAt.Equal(rec.ReceivedAt) {
			t.Fatal(still, err)
		}
	}
	if len(w.snapshot()) != 0 {
		t.Fatal("waiting input was delivered")
	}
	if status := s.router.ingressStatus(msg); !strings.Contains(status, "Waiting: reply_context_missing") {
		t.Fatal(status)
	}
	a.setReady()
	waitIngressCondition(t, func() bool { return len(w.snapshot()) == 1 })
	if got := w.snapshot(); got[0]["content"] != "first" {
		t.Fatal(got)
	}
	payload, _ := json.Marshal(InputResultPayload{RequestID: "im-" + accepted[0].RecordID, Status: inputQueued})
	p.mu.Lock()
	p.applyInputResult(&HeadlessEnvelope{Payload: payload, Seq: 1}, time.Now())
	p.mu.Unlock()
	waitIngressCondition(t, func() bool { return len(w.snapshot()) == 2 })
	if got := w.snapshot(); got[1]["content"] != "second" {
		t.Fatal(got)
	}
	s.Close()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.wake != nil {
		t.Fatal("notifier retained after shutdown")
	}
}

func TestGatewayIngressWaitingExpiresWithoutDelivery(t *testing.T) {
	s, _, w, msg := ingressTestService(t)
	s.router.SetAdapter(&ingressReadinessAdapter{IMAdapter: s.router.currentAdapter()})
	msg.MessageID, msg.Text = "expired", "expired task"
	result := s.router.ReceiveIngress(context.Background(), im.IngressEnvelope{Message: msg, ReceivedAt: time.Now().Add(-24 * time.Hour)})
	if result.Disposition != im.ReceiveAccepted {
		t.Fatal(result)
	}
	waitIngressCondition(t, func() bool {
		rec, err := s.store.Get(result.RecordID)
		return err == nil && rec.State == "quarantined" && rec.Reason == "received_expired"
	})
	if len(w.snapshot()) != 0 {
		t.Fatal("expired input delivered")
	}
}

func TestGatewayIngressNormalizationCannotReplaceOperation(t *testing.T) {
	s, _, _, msg := ingressTestService(t)
	pauseIngressTestRoute(t, s, msg)
	msg.MessageID, msg.Text = "original", "original task"
	future, err := s.router.SubmitIngressNormalization(context.Background(), "account/chat", im.IngressEnvelope{Message: msg}, func(_ context.Context, e im.IngressEnvelope) (im.IngressEnvelope, error) {
		e.Message.Text = "/allow"
		return e, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result := im.WaitIngressNormalization(context.Background(), future)
	if result.Disposition != im.ReceiveAccepted {
		t.Fatal(result)
	}
	rec, err := s.store.Get(result.RecordID)
	if err != nil || rec.Kind != "task" || rec.Text != "original task" {
		t.Fatal(rec, err)
	}
	msg.MessageID = "identity-change"
	future, err = s.router.SubmitIngressNormalization(context.Background(), "account/chat", im.IngressEnvelope{Message: msg}, func(_ context.Context, e im.IngressEnvelope) (im.IngressEnvelope, error) {
		e.Message.ChatID = "replacement"
		return e, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result = im.WaitIngressNormalization(context.Background(), future); result.Disposition != im.ReceiveRetryable || result.Reason != "normalization_failed" {
		t.Fatal(result)
	}
}

func TestGatewayIngressUnavailableNeverExecutes(t *testing.T) {
	s, _, w, msg := ingressTestService(t)
	s.Close()
	msg.MessageID, msg.Text = "closed", "task"
	if result := s.router.ReceiveIngress(context.Background(), im.IngressEnvelope{Message: msg}); result.Disposition != im.ReceiveRetryable {
		t.Fatal(result)
	}
	if len(w.snapshot()) != 0 {
		t.Fatal("closed ingress executed input")
	}
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("restarted closed service")
	}
	// A router without the common receiver cannot silently use the legacy path.
	s.router.ingress = nil
	if result := s.router.ReceiveIngress(context.Background(), im.IngressEnvelope{Message: msg}); result.Disposition != im.ReceiveRetryable {
		t.Fatal(result)
	}
}

func TestGatewayIngressInvalidInteractionKeepsDurableRejection(t *testing.T) {
	s, _, w, msg := ingressTestService(t)
	msg.MessageID = "invalid-callback"
	msg.Text = ""
	msg.InternalAction = &im.InternalAction{Type: "unsupported", RequestID: "request", BindingInstanceID: "binding", Value: "private-argument"}
	first := s.router.ReceiveIngress(t.Context(), im.IngressEnvelope{Message: msg})
	if first.Disposition != im.ReceiveRejected || first.RecordID == "" || first.Reason != "invalid_interaction" {
		t.Fatal(first)
	}
	second := s.router.ReceiveIngress(t.Context(), im.IngressEnvelope{Message: msg})
	if second.Disposition != im.ReceiveDuplicate || second.RecordID != first.RecordID || second.State != "rejected" {
		t.Fatal(second)
	}
	rec, err := s.store.Get(first.RecordID)
	if err != nil || rec.Kind != "control" || rec.Text != "" || rec.ControlTarget != nil || len(w.snapshot()) != 0 {
		t.Fatal(rec, err, w.snapshot())
	}
}

func TestGatewayIngressUnknownStillAllowsReadOnlyQueries(t *testing.T) {
	s, p, w, msg := ingressTestService(t)
	id := pauseIngressTestRoute(t, s, msg)
	msg.MessageID, msg.Text = "unresolved", "task"
	first := s.Accept(t.Context(), im.IngressEnvelope{Message: msg})
	if first.Disposition != im.ReceiveAccepted {
		t.Fatal(first)
	}
	if err := s.store.UpdateRecord(t.Context(), first.RecordID, []im.IngressState{"received"}, func(rec *IngressRecord) error { rec.State = "unknown"; return nil }); err != nil {
		t.Fatal(err)
	}
	queryWriter := &ingressInteractionWriter{proc: p, snapshot: StatusResponse{SessionID: "session"}}
	p.mu.Lock()
	p.stdin = queryWriter
	p.mu.Unlock()
	var controls []im.ReceiveResult
	for _, command := range []string{"/help", "/status", "/todos"} {
		msg.MessageID, msg.Text = command, command
		result := s.Accept(t.Context(), im.IngressEnvelope{Message: msg})
		if result.Disposition != im.ReceiveAccepted {
			t.Fatal(result)
		}
		controls = append(controls, result)
	}
	dispatchIngressTestRoute(s, id)
	for _, result := range controls {
		rec, err := s.store.Get(result.RecordID)
		if err != nil || rec.State != "completed" {
			t.Fatal(rec, err)
		}
	}
	rec, err := s.store.Get(first.RecordID)
	if err != nil || rec.State != "unknown" || len(w.snapshot()) != 0 || s.router.mgr.GetProcessForKey(p.key) != p {
		t.Fatal(rec, err, w.snapshot())
	}
	if commands := queryWriter.capture.snapshot(); len(commands) != 1 || commands[0]["type"] != "status" {
		t.Fatal(commands)
	}
}
