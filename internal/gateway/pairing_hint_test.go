package gateway

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

func unpairedTestRouter(t *testing.T) (*NotificationRouter, string) {
	t.Helper()
	// No owner is ever bound: the pairing code stays live but unclaimed.
	return pairedTestRouterWithConfig(t, "ims: {wechat: {}}\n")
}

func TestAuthorizeUnpairedPrivateHints(t *testing.T) {
	r, code := unpairedTestRouter(t)
	result := r.admission.AuthorizeClassified(im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "stranger", MessageID: "plain", Text: "hello"})
	if result.Disposition != im.AdmissionRejected || result.Ticket.Kind != "pairing_hint_text" || result.Reason != "pairing_hint_text" {
		t.Fatal(result)
	}
	if !r.admission.Valid(result.Ticket) {
		t.Fatal("hint ticket cannot carry a flow reply")
	}
	result = r.admission.AuthorizeClassified(im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "stranger", MessageID: "usage", Text: "/bind"})
	if result.Disposition != im.AdmissionRejected || result.Ticket.Kind != "pairing_hint_usage" {
		t.Fatal(result)
	}
	result = r.admission.AuthorizeClassified(im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "stranger", MessageID: "wrong-code", Text: fmt.Sprintf("/bind %q nope", t.TempDir())})
	if result.Disposition != im.AdmissionRejected || result.Ticket.Kind != "pairing_hint_code" {
		t.Fatal(result)
	}
	// Group chatter and internal actions stay silent: no hint kind, no reply.
	result = r.admission.AuthorizeClassified(im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "group", ChatID: "group1", SenderID: "stranger", MessageID: "grouped", Text: "hello"})
	if result.Disposition != im.AdmissionRejected || result.Ticket.Kind != "" || result.Reason != "authorization_denied" {
		t.Fatal(result)
	}
	// Hints never upgraded admission, and a valid code still pairs through.
	if _, ok := r.admission.Authorize(im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "stranger", MessageID: "bind", Text: fmt.Sprintf("/bind %q %s", t.TempDir(), code)}); !ok {
		t.Fatal("valid pairing rejected")
	}
	// Hint replies die with platform readiness like every flow reply.
	r.admission.pairing.PlatformUnavailable("wechat")
	result = r.admission.AuthorizeClassified(im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "stranger", MessageID: "again", Text: "hello"})
	if result.Ticket.Kind != "" {
		t.Fatal(result)
	}
}

// A hint ticket is issued while the platform has no owner. If the same sender
// pairs in the meantime, the stale ticket must not survive as owner authority
// (or reach the gate it never had).
func TestHintTicketExpiresWhenOwnerPairs(t *testing.T) {
	r, code := unpairedTestRouter(t)
	msg := im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "owner", MessageID: "hint", Text: "hello"}
	hint := r.admission.AuthorizeClassified(msg).Ticket
	if hint.Kind != "pairing_hint_text" {
		t.Fatalf("expected a hint ticket, got %q", hint.Kind)
	}
	msg.MessageID, msg.Text = "bind", fmt.Sprintf("/bind %q %s", t.TempDir(), code)
	if !dispatchPairingTest(r, msg) {
		t.Fatal("valid pairing rejected")
	}
	if r.admission.Valid(hint) {
		t.Fatal("a hint ticket must expire once the sender becomes the owner")
	}
}

func TestPairingHintBucketExhausts(t *testing.T) {
	r, _ := unpairedTestRouter(t)
	hints := 0
	for i := 0; i < hintBucketSize+3; i++ {
		result := r.admission.AuthorizeClassified(im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "stranger", MessageID: fmt.Sprintf("hint-%d", i), Text: "hello"})
		if result.Ticket.Kind == "pairing_hint_text" {
			hints++
		}
	}
	if hints != hintBucketSize {
		t.Fatalf("hint replies = %d, want %d", hints, hintBucketSize)
	}
}

func TestPairingHintBucketIsPerChat(t *testing.T) {
	r, _ := unpairedTestRouter(t)
	for i := 0; i < hintBucketSize+3; i++ {
		r.admission.AuthorizeClassified(im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat-a", SenderID: "stranger", MessageID: fmt.Sprintf("a-%d", i), Text: "hello"})
	}
	// Exhausting one chat must not silence hints for a different private chat.
	result := r.admission.AuthorizeClassified(im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat-b", SenderID: "stranger", MessageID: "b-0", Text: "hello"})
	if result.Ticket.Kind != "pairing_hint_text" {
		t.Fatalf("second chat did not receive a hint: %+v", result)
	}
}

func TestIngressUnpairedHintReply(t *testing.T) {
	r, _ := unpairedTestRouter(t)
	store := NewIngressStore()
	service := NewIngressService(store, r)
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		service.Close()
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	adapter := r.currentAdapter().(*pairingTestAdapter)
	msg := im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "stranger", MessageID: "hint", Text: "hello"}
	result := r.ReceiveIngress(t.Context(), im.IngressEnvelope{Message: msg})
	if result.Disposition != im.ReceiveRejected || result.Reason != "pairing_hint_text" {
		t.Fatal(result)
	}
	waitIngressCondition(t, func() bool {
		adapter.mu.Lock()
		defer adapter.mu.Unlock()
		for _, text := range adapter.messages {
			if strings.Contains(text, "no paired owner yet") {
				return true
			}
		}
		return false
	})
	// Redelivery of the same message is a committed duplicate: no second hint.
	redelivered := r.ReceiveIngress(t.Context(), im.IngressEnvelope{Message: msg})
	if redelivered.Disposition != im.ReceiveDuplicate {
		t.Fatal(redelivered)
	}
	adapter.mu.Lock()
	count := len(adapter.messages)
	adapter.mu.Unlock()
	if count != 1 {
		t.Fatalf("duplicate redelivery re-hinted: %d replies", count)
	}
}

func TestPairingHintReplyTextVariants(t *testing.T) {
	store := testConfigStore(t, "ims: {wechat: {}}\n")
	paths := &config.Paths{StateDir: t.TempDir()}
	mgr := NewChordManager(store.Snapshot(), paths)
	mgr.store = store
	r := NewNotificationRouter(mgr)
	defer r.Close()
	msg := im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat"}

	r.admission = NewAdmission(NewPairingService(store, true, nil))
	mgr.admission = r.admission
	if text := pairingHintReply(r, msg, "pairing_hint_text"); !strings.Contains(text, "no longer valid") || !strings.Contains(text, "--pair") {
		t.Fatal(text)
	}
	r.admission = NewAdmission(NewPairingService(store, false, nil))
	mgr.admission = r.admission
	if text := pairingHintReply(r, msg, "pairing_hint_text"); !strings.Contains(text, "--pair") || strings.Contains(text, "no longer valid") {
		t.Fatal(text)
	}
	if text := pairingHintReply(r, msg, "pairing_hint_usage"); !strings.Contains(text, "Invalid bind command") {
		t.Fatal(text)
	}
	if text := pairingHintReply(r, msg, "pairing_hint_code"); !strings.Contains(text, "pairing code is not valid") {
		t.Fatal(text)
	}
}
