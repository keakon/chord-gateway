package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAdmittedWorkSurvivesTemporaryDisconnect(t *testing.T) {
	r, code := pairedTestRouter(t)
	msg, key := bindTestOwner(t, r, code, t.TempDir())
	adapter := &pairingTestAdapter{}
	r.SetAdapter(adapter)
	writer := &captureWriteCloser{}
	r.mgr.procs[key] = &ChordProcess{stdin: writer, bindingInstanceID: "runtime"}
	msg.Text, msg.MessageID = "accepted input", "accepted-before-disconnect"
	admitted, ok := admittedMessage(r, msg)
	if !ok {
		t.Fatal("owner input denied")
	}
	target, ok := r.targetForKey(key)
	if !ok {
		t.Fatal("missing target")
	}
	r.admission.pairing.PlatformUnavailable("wechat")
	if _, ok := admittedMessage(r, msg); ok {
		t.Fatal("new input admitted while disconnected")
	}
	if !r.admission.Valid(*admitted.ticket) {
		t.Fatal("accepted input lost on disconnect")
	}
	r.dispatchPaired(admitted)
	r.HandleChordEvent(key, "assistant_message", ControlState{LastAssistantText: "final result"})
	r.Close()
	adapter.mu.Lock()
	messages := strings.Join(adapter.messages, "\n")
	adapter.mu.Unlock()
	if !strings.Contains(writer.String(), "accepted input") || !strings.Contains(messages, "final result") {
		t.Fatalf("accepted work or final reply lost: %s", messages)
	}
	if err := r.admission.pairing.PlatformReady("wechat", "bot"); err != nil {
		t.Fatal(err)
	}
	if !r.admission.Valid(*admitted.ticket) || !r.validTarget(target) {
		t.Fatal("reconnect revoked original identity")
	}
	if err := r.admission.pairing.PlatformReady("wechat", "replacement-bot"); err == nil {
		t.Fatal("account replacement allowed")
	}
	if r.admission.Valid(*admitted.ticket) || r.validTarget(target) {
		t.Fatal("old work survived account change")
	}
}

func TestUnpairedTicketRequiresLivePairingWindow(t *testing.T) {
	r, code := pairedTestRouter(t)
	msg := IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "owner", MessageID: "pair", Text: "/bind /tmp/project " + code}
	ticket, ok := r.admission.Authorize(msg)
	if !ok {
		t.Fatal("pairing denied")
	}
	r.admission.pairing.PlatformUnavailable("wechat")
	if r.admission.Valid(ticket) {
		t.Fatal("disconnected first pairing allowed")
	}
}

func TestOldEventCannotAcquireReplacementRuntime(t *testing.T) {
	for _, event := range []string{"notification", "confirm_request", "question_request", "question_resolved"} {
		t.Run(event, func(t *testing.T) {
			r, code := pairedTestRouter(t)
			_, key := bindTestOwner(t, r, code, t.TempDir())
			adapter := &pairingTestAdapter{}
			r.SetAdapter(adapter)
			old := &ChordProcess{bindingInstanceID: "old"}
			r.mgr.mu.Lock()
			r.mgr.procs[key] = old
			r.mgr.mu.Unlock()
			target, ok := r.targetForKey(key)
			if !ok || !r.validTarget(target) {
				t.Fatal("old runtime not admitted")
			}
			// Replace after event admission, before the actual text/card I/O.
			r.mgr.removeProcessIfCurrent(key, old)
			r.mgr.mu.Lock()
			r.mgr.procs[key] = &ChordProcess{bindingInstanceID: "new"}
			r.mgr.mu.Unlock()
			state := ControlState{BindingInstanceID: "old", PendingConfirm: &ConfirmPayload{RequestID: "confirm-old"}, Questions: map[string]*QuestionPayload{"question-old": {RequestID: "question-old", Question: "Choose", Options: []string{"A"}}}}
			r.deliverChordEvent(target, event, state, "old prompt")
			adapter.mu.Lock()
			defer adapter.mu.Unlock()
			if len(adapter.messages) != 0 {
				t.Fatalf("stale event sent: %v", adapter.messages)
			}
		})
	}
}

type cancelDeliveryAdapter struct {
	pairingTestAdapter
	entered chan struct{}
}

func (a *cancelDeliveryAdapter) SendTextContext(ctx context.Context, _, _ string) error {
	close(a.entered)
	<-ctx.Done()
	return ctx.Err()
}

func TestRuntimeRetirementCancelsAdmittedSend(t *testing.T) {
	for _, retire := range []string{"process", "account", "binding"} {
		t.Run(retire, func(t *testing.T) {
			r, code := pairedTestRouter(t)
			_, key := bindTestOwner(t, r, code, t.TempDir())
			old := &ChordProcess{bindingInstanceID: "old"}
			r.mgr.mu.Lock()
			r.mgr.procs[key] = old
			r.mgr.mu.Unlock()
			target, _ := r.targetForKey(key)
			adapter := &cancelDeliveryAdapter{entered: make(chan struct{})}
			r.SetAdapter(adapter)
			done := make(chan error, 1)
			go func() { done <- r.SendBound(t.Context(), target, "old result") }()
			select {
			case <-adapter.entered:
			case <-time.After(time.Second):
				t.Fatal("send not started")
			}
			switch retire {
			case "process":
				r.mgr.removeProcessIfCurrent(key, old)
			case "account":
				_ = r.admission.pairing.PlatformReady("wechat", "replacement-bot")
			case "binding":
				target.lease.Close()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("send result: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("retired send was not cancelled")
			}
		})
	}
}
