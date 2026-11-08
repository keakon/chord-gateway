package wechat

import (
	"context"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
)

// resolveIncomingMessage re-resolves a message whose earlier admission is
// pending: a settled future supersedes the uncertainty, an open future keeps
// the pending entry, and unsupported messages pass the previous result through.
func TestWechatResolveIncomingMessage(t *testing.T) {
	token := &TokenData{AccountID: "acct"}

	msg := ILinkMessage{
		MessageType: ILinkMessageTypeUser,
		FromUserID:  "u1",
		ItemList:    []ILinkItem{{Type: ILinkItemTypeText, MessageID: "77", TextItem: &ILinkTextItem{Text: "hello"}}},
	}
	key := compositeKey(token.AccountID, WechatIngressBusinessID(msg.FromUserID, "77"))

	t.Run("unsupported message passes previous through", func(t *testing.T) {
		a := NewAdapterForTest(t)
		previous := im.ReceiveResult{Disposition: im.ReceiveAccepted, State: im.IngressReceived}
		if got := a.ResolveIncomingMessageForTest(context.Background(), ILinkMessage{MessageType: ILinkMessageTypeBot}, token, previous); got != previous {
			t.Fatalf("unsupported message changed the result: %#v", got)
		}
	})

	t.Run("settled future supersedes and cleans up", func(t *testing.T) {
		a := NewAdapterForTest(t)
		a.ingressPending = map[string]*WechatIngressPending{}
		future := make(chan im.ReceiveResult, 1)
		accepted := im.ReceiveResult{Disposition: im.ReceiveAccepted, State: im.IngressReceived, RecordID: "record-1"}
		future <- accepted
		a.ingressPending[key] = &WechatIngressPending{Future: future}

		got := a.ResolveIncomingMessageForTest(context.Background(), msg, token, im.ReceiveResult{})
		if got != accepted {
			t.Fatalf("resolved result = %#v, want the settled acceptance", got)
		}
		if _, still := a.ingressPending[key]; still {
			t.Fatal("pending entry survived a settled resolution")
		}
	})

	t.Run("open future keeps the pending entry uncertain", func(t *testing.T) {
		a := NewAdapterForTest(t)
		a.ingressPending = map[string]*WechatIngressPending{}
		future := make(chan im.ReceiveResult, 1)
		// A live pending entry always carries the uncertain outcome the first
		// receive recorded; only settled entries are deleted there.
		a.ingressPending[key] = &WechatIngressPending{Future: future, Result: im.ReceiveResult{Disposition: im.ReceiveUncertain}}

		got := a.ResolveIncomingMessageForTest(context.Background(), msg, token, im.ReceiveResult{})
		if got.Disposition != im.ReceiveUncertain {
			t.Fatalf("unresolved result = %#v, want uncertain", got)
		}
		if a.ingressPending[key] == nil {
			t.Fatal("pending entry was dropped while the future is still open")
		}
	})

	t.Run("rejected result without a record commits as retryable", func(t *testing.T) {
		a := NewAdapterForTest(t)
		a.ingressPending = map[string]*WechatIngressPending{}
		future := make(chan im.ReceiveResult, 1)
		future <- im.ReceiveResult{Disposition: im.ReceiveRejected, State: im.IngressRejected}
		a.ingressPending[key] = &WechatIngressPending{Future: future}

		got := a.ResolveIncomingMessageForTest(context.Background(), msg, token, im.ReceiveResult{})
		if got.Disposition != im.ReceiveRetryable || got.Reason != "rejection_not_committed" {
			t.Fatalf("committed result = %#v, want rejection_not_committed retryable", got)
		}
	})
}
