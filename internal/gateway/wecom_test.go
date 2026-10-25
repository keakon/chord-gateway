package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"

	wecom "github.com/keakon/chord-gateway/internal/platform/wecom"

	"github.com/gorilla/websocket"
)

func wecomAdapterCallback(bot, delivery, business string) []byte {
	return []byte(fmt.Sprintf(`{"cmd":"aibot_msg_callback","headers":{"req_id":%q},"body":{"aibotid":%q,"msgid":%q,"chattype":"single","msgtype":"text","from":{"userid":"owner"},"text":{"content":"task"}}}`, delivery, bot, business))
}

func countWecomIngressEvent(store *IngressStore, event string) uint64 {
	var n uint64
	for _, v := range store.MetricsSnapshot().Counters {
		if v.Platform == "wecom" && v.Event == event {
			n += v.Count
		}
	}
	return n
}

// A batch drop is N lost deliveries, not one event. The counter answers how
// many messages never reached the ingress, which is what docs/wecom.md promises
// and what the Discord adapter's callback_dropped accounting already does.
func TestWecomDroppedBeforeAcceptCountsEachDelivery(t *testing.T) {
	a, _, store, _ := wecomAdapterFixture(t)
	a.DroppedBeforeAcceptForTest(3)
	a.DroppedBeforeAcceptForTest(0)
	if got := countWecomIngressEvent(store, "callback_dropped"); got != 3 {
		t.Fatalf("callback_dropped = %d, want 3", got)
	}
}

func TestWecomCallbacksUseDurableBusinessDedupe(t *testing.T) {
	a, _, store, _ := wecomAdapterFixture(t)
	if err := a.ReceiveFrameForTest(a.ContextForTest(), wecomAdapterCallback("bot", "delivery-1", "business")); err != nil {
		t.Fatal(err)
	}
	if err := a.ReceiveFrameForTest(a.ContextForTest(), wecomAdapterCallback("bot", "delivery-2", "business")); err != nil {
		t.Fatal(err)
	}
	waitIngressCondition(t, func() bool { return countWecomIngressEvent(store, "duplicate") == 1 })
	if countWecomIngressEvent(store, "received") != 1 {
		t.Fatal("callback req_id replaced business ID dedupe")
	}
	if err := a.ReceiveFrameForTest(a.ContextForTest(), wecomAdapterCallback("other", "delivery-3", "business-2")); !errors.Is(err, wecom.ErrWecomAccountMismatch) {
		t.Fatal("account mismatch not fatal")
	}
	if a.IngressDeliveryReadiness(im.ChatIdentity{Platform: "wecom", AccountID: "bot", ChatID: "chat"}).Ready {
		t.Fatal("account mismatch left adapter ready")
	}
}

func TestWecomAdmissionFailureDistinguishesLossAndUncertainty(t *testing.T) {
	a, msg, store, r := wecomAdapterFixture(t)
	r.ingress.normalizer.receiver = testIngressReceiverFunc(func(context.Context, im.IngressEnvelope) im.ReceiveResult {
		return im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "ingress_unavailable"}
	})
	if err := a.ReceiveFrameForTest(a.ContextForTest(), wecomAdapterCallback("bot", "failed-delivery", "failed-business")); err != nil {
		t.Fatal(err)
	}
	waitIngressCondition(t, func() bool { return countWecomIngressEvent(store, "acceptance_unconfirmed") == 1 })
	a.ObserveReceiveForTest(msg, im.ReceiveResult{Disposition: im.ReceiveUncertain, Reason: "commit_pending"})
	if countWecomIngressEvent(store, "lost_before_accept") != 0 {
		t.Fatal("uncertain commit recorded as lost")
	}
}

func TestWecomAdapterLifecycleAndAccountLockBeforeDial(t *testing.T) {
	a, _, _, _ := wecomAdapterFixture(t)
	lock, err := im.AcquireIMAccountLock("wecom", "bot")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	a.SetWSEndpointForTest("ws://127.0.0.1:1")
	if err = a.Connect(); !errors.Is(err, im.ErrInstanceRunning) {
		t.Fatalf("dialed before acquiring account lock: %v", err)
	}
	if a.SupportsLoginRenewal() {
		t.Fatal("interactive login incorrectly enabled")
	}
	if _, err = a.StartLogin(); !errors.Is(err, im.ErrLoginNotSupported) {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { a.Disconnect(); a.Disconnect(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idempotent stop blocked")
	}
	// Disconnect is permanent: reconnecting must report that the adapter is
	// stopped rather than succeed as if a dead adapter were healthy.
	if err := a.Connect(); !errors.Is(err, wecom.ErrWecomStopped) {
		t.Fatalf("connect after disconnect = %v, want wecom.ErrWecomStopped", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.ValidateBindingTarget(ctx, im.BindingProbeTarget{Platform: "wecom", AccountID: "bot", ChatID: "invalid"}); err == nil {
		t.Fatal("stopped adapter accepted probe")
	}
}

func TestWecomConcurrentDisconnectJoinsSubscribeAndReleasesAccountLock(t *testing.T) {
	a, _, _, _ := wecomAdapterFixture(t)
	subscribed := make(chan struct{})
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var frame wecom.WecomWSFrame
		if conn.ReadJSON(&frame) != nil {
			return
		}
		close(subscribed)
		// Keep subscribe waiting for an ACK until Disconnect closes the socket.
		_ = conn.ReadJSON(&frame)
	}))
	t.Cleanup(server.Close)
	a.SetWSEndpointForTest("ws" + strings.TrimPrefix(server.URL, "http"))
	done := make(chan error, 1)
	go func() { done <- a.Connect() }()
	select {
	case <-subscribed:
	case <-time.After(time.Second):
		t.Fatal("subscribe did not start")
	}
	if lock, err := im.AcquireIMAccountLock("wecom", "bot"); err == nil {
		lock.Close()
		t.Fatal("live connection released account lock")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(a.Disconnect)
	}
	stopped := make(chan struct{})
	go func() { wg.Wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("concurrent shutdown blocked")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Connect did not stop")
	}
	lock, err := im.AcquireIMAccountLock("wecom", "bot")
	if err != nil {
		t.Fatal("account lock retained after shutdown", err)
	}
	lock.Close()
}
