package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
	"github.com/keakon/chord-gateway/internal/platform/wechat"
)

func TestWechatIngressFailedAcceptancePreservesCommittedReplyContext(t *testing.T) {
	a, service, _, writer := newWechatIngressTest(t)
	id := im.ChatIdentity{Platform: "wechat", AccountID: "bot", ChatID: "owner"}
	service.mu.Lock()
	service.blocked[id] = ingressBlocked{since: time.Now(), reason: "test"}
	service.mu.Unlock()
	for i := range 64 {
		reply := ""
		if i == 0 {
			reply = "confirmed-context"
		}
		msg := wechatTextMessage(fmt.Sprint(100+i), "owner", "queued task", reply)
		if result := a.ReceiveIncomingMessageForTest(t.Context(), msg, a.TokenForTest()); result.Disposition != im.ReceiveAccepted {
			t.Fatal(result)
		}
	}
	failed := a.ReceiveIncomingMessageForTest(t.Context(), wechatTextMessage("200", "owner", "overflow task", "unaccepted-context"), a.TokenForTest())
	if failed.Disposition != im.ReceiveRetryable || failed.Reason != "capacity" {
		t.Fatal(failed)
	}
	reply, _ := a.ReplyContextForTest(compositeKey("bot", "owner"))
	if reply.Token != "confirmed-context" || !reply.Committed || !a.IngressDeliveryReadiness(id).Ready {
		t.Fatal("failed acceptance replaced the context of previously accepted work")
	}
	service.mu.Lock()
	delete(service.blocked, id)
	service.mu.Unlock()
	service.signal()
	waitIngressCondition(t, func() bool { return len(wechatTaskWrites(writer)) == 1 })
}

type wechatIngressReadinessProbe struct {
	*wechat.WechatAdapter
	readyChecked chan struct{}
	once         sync.Once
}

func (a *wechatIngressReadinessProbe) IngressDeliveryReadiness(id im.ChatIdentity) im.IngressDeliveryReadiness {
	readiness := a.WechatAdapter.IngressDeliveryReadiness(id)
	if readiness.Ready {
		a.once.Do(func() { close(a.readyChecked) })
	}
	return readiness
}

func TestWechatIngressStopDuringSessionReadyWaitPreservesReceived(t *testing.T) {
	a, service, process, writer := newWechatIngressTest(t)
	ready := make(chan struct{})
	process.readySignal = ready
	probe := &wechatIngressReadinessProbe{WechatAdapter: a, readyChecked: make(chan struct{})}
	service.router.SetAdapter(probe)
	result := a.ReceiveIncomingMessageForTest(t.Context(), wechatTextMessage("301", "owner", "queued task", "confirmed-context"), a.TokenForTest())
	if result.Disposition != im.ReceiveAccepted {
		close(ready)
		t.Fatal(result)
	}
	select {
	case <-probe.readyChecked:
	case <-time.After(3 * time.Second):
		close(ready)
		t.Fatal("dispatcher did not check readiness")
	}
	a.SetStoppedForTest(true)
	close(ready)
	waitIngressCondition(t, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()
		return !service.active[im.ChatIdentity{Platform: "wechat", AccountID: "bot", ChatID: "owner"}]
	})
	record, err := service.store.Get(result.RecordID)
	if err != nil || record.State != "received" || len(wechatTaskWrites(writer)) != 0 {
		t.Fatalf("stopped transport delivered after session became ready: %+v, %v", record, err)
	}
}

type wechatIngressAcceptanceBarrier struct {
	im.IngressReceiver
	started chan struct{}
	release chan struct{}
}

func (b *wechatIngressAcceptanceBarrier) Accept(ctx context.Context, envelope im.IngressEnvelope) im.ReceiveResult {
	close(b.started)
	<-b.release
	return b.IngressReceiver.Accept(ctx, envelope)
}

func TestWechatIngressReconnectRejectsPreviousConnectionCallbacks(t *testing.T) {
	for _, beforePrepare := range []bool{true} {
		name := "after_prepare"
		if beforePrepare {
			name = "before_prepare"
		}
		t.Run(name, func(t *testing.T) {
			a, service, _, _ := newWechatIngressTest(t)
			if err := service.PrepareAccount(t.Context(), "wechat", "bot"); err != nil {
				t.Fatal(err)
			}
			oldConnection, cancelOld := context.WithCancel(t.Context())
			defer cancelOld()
			a.SetConnectionForTest(oldConnection, nil)
			started, release := make(chan struct{}), make(chan struct{})
			if beforePrepare {
				service.normalizer.receiver = &wechatIngressAcceptanceBarrier{service, started, release}
			}

			msg := wechatTextMessage("501", "owner", "old connection task", "old-context")
			future := make(chan im.ReceiveResult, 1)
			go func() { future <- a.ReceiveIncomingMessageForTest(t.Context(), msg, a.TokenForTest()) }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				close(release)
				t.Fatal("acceptance did not reach its barrier")
			}
			cancelOld()
			newConnection, cancelNew := context.WithCancel(t.Context())
			defer cancelNew()
			newPending := &wechat.WechatIngressPending{Connection: newConnection}
			key := compositeKey("bot", wechat.WechatIngressBusinessID("owner", "501"))
			// Recreate the Connect teardown/start state without changing token
			// or account epoch: only the connection scope distinguishes it.
			a.SetConnectionForTest(newConnection, nil)
			a.SetStoppedForTest(false)
			a.ReplaceReplyContextsForTest(nil)
			a.ReplaceIngressPendingForTest(map[string]*wechat.WechatIngressPending{key: newPending})
			close(release)
			select {
			case result := <-future:
				if result.Disposition != im.ReceiveRetryable || result.Reason != "transport_identity_changed" {
					t.Fatal(result)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("old acceptance did not finish")
			}
			contexts := a.ReplyContextCountForTest()
			preserved := a.IngressPendingForTest(key) == newPending
			if contexts != 0 || !preserved {
				t.Fatal("old connection modified replacement context or pending receipt")
			}
			if _, err := service.store.lookupCommittedBusiness("wechat", "bot", wechat.WechatIngressBusinessID("owner", "501")); !errors.Is(err, errIngressMissing) {
				t.Fatal("old connection committed its input", err)
			}
		})
	}
}

func TestWechatIngressReconnectRejectsPreviousConnectionConfirmation(t *testing.T) {
	a, service, _, _ := newWechatIngressTest(t)
	oldConnection, cancelOld := context.WithCancel(t.Context())
	defer cancelOld()
	newConnection, cancelNew := context.WithCancel(t.Context())
	defer cancelNew()
	pairing := service.router.admission.pairing
	pairing.mu.Lock()
	epoch := pairing.platforms["wechat"].epoch
	pairing.mu.Unlock()
	pending := &wechat.WechatIngressPending{Connection: oldConnection, Prepared: true, ReplyToken: "reused-context", Bound: true, AccountEpoch: epoch}
	a.SetConnectionForTest(newConnection, nil)
	a.ReplaceReplyContextsForTest(map[string]wechat.WechatReplyContext{
		compositeKey("bot", "owner"): {Token: "reused-context", Bound: true, AccountEpoch: epoch},
	})
	cancelOld()
	a.ConfirmIncomingContextForTest(wechatTextMessage("502", "owner", "old task", "reused-context"), a.TokenForTest(), im.ReceiveResult{Disposition: im.ReceiveAccepted}, pending)
	committed, ok := a.ReplyContextForTest(compositeKey("bot", "owner"))
	if !ok || committed.Committed {
		t.Fatal("late confirmation reopened the replacement connection")
	}
}
