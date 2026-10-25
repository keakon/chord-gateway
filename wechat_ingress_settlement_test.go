package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
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
		if result := a.receiveIncomingMessage(t.Context(), msg, a.token.Load()); result.Disposition != im.ReceiveAccepted {
			t.Fatal(result)
		}
	}
	failed := a.receiveIncomingMessage(t.Context(), wechatTextMessage("200", "owner", "overflow task", "unaccepted-context"), a.token.Load())
	if failed.Disposition != im.ReceiveRetryable || failed.Reason != "capacity" {
		t.Fatal(failed)
	}
	a.mu.Lock()
	reply := a.replyContexts[compositeKey("bot", "owner")]
	a.mu.Unlock()
	if reply.token != "confirmed-context" || !reply.committed || !a.IngressDeliveryReadiness(id).Ready {
		t.Fatal("failed acceptance replaced the context of previously accepted work")
	}
	service.mu.Lock()
	delete(service.blocked, id)
	service.mu.Unlock()
	service.signal()
	waitIngressCondition(t, func() bool { return len(wechatTaskWrites(writer)) == 1 })
}

type wechatIngressReadinessProbe struct {
	*WechatAdapter
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
	result := a.receiveIncomingMessage(t.Context(), wechatTextMessage("301", "owner", "queued task", "confirmed-context"), a.token.Load())
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
	a.mu.Lock()
	a.stopped = true
	a.mu.Unlock()
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
			a.mu.Lock()
			a.ctx = oldConnection
			a.mu.Unlock()
			started, release := make(chan struct{}), make(chan struct{})
			if beforePrepare {
				service.normalizer.receiver = &wechatIngressAcceptanceBarrier{service, started, release}
			}

			msg := wechatTextMessage("501", "owner", "old connection task", "old-context")
			future := make(chan im.ReceiveResult, 1)
			go func() { future <- a.receiveIncomingMessage(t.Context(), msg, a.token.Load()) }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				close(release)
				t.Fatal("acceptance did not reach its barrier")
			}
			cancelOld()
			newConnection, cancelNew := context.WithCancel(t.Context())
			defer cancelNew()
			newPending := &wechatIngressPending{connection: newConnection}
			key := compositeKey("bot", wechatIngressBusinessID("owner", "501"))
			a.mu.Lock()
			// Recreate the Connect teardown/start state without changing token
			// or account epoch: only the connection scope distinguishes it.
			a.ctx = newConnection
			a.stopped = false
			a.replyContexts = nil
			a.ingressPending = map[string]*wechatIngressPending{key: newPending}
			a.mu.Unlock()
			close(release)
			select {
			case result := <-future:
				if result.Disposition != im.ReceiveRetryable || result.Reason != "transport_identity_changed" {
					t.Fatal(result)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("old acceptance did not finish")
			}
			a.mu.Lock()
			contexts := len(a.replyContexts)
			preserved := a.ingressPending[key] == newPending
			a.mu.Unlock()
			if contexts != 0 || !preserved {
				t.Fatal("old connection modified replacement context or pending receipt")
			}
			if _, err := service.store.lookupCommittedBusiness("wechat", "bot", wechatIngressBusinessID("owner", "501")); !errors.Is(err, errIngressMissing) {
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
	pending := &wechatIngressPending{connection: oldConnection, prepared: true, replyToken: "reused-context", bound: true, accountEpoch: epoch}
	a.mu.Lock()
	a.ctx = newConnection
	a.replyContexts = map[string]wechatReplyContext{
		compositeKey("bot", "owner"): {token: "reused-context", bound: true, accountEpoch: epoch},
	}
	a.mu.Unlock()
	cancelOld()
	a.confirmIncomingContext(wechatTextMessage("502", "owner", "old task", "reused-context"), a.token.Load(), im.ReceiveResult{Disposition: im.ReceiveAccepted}, pending)
	a.mu.Lock()
	committed := a.replyContexts[compositeKey("bot", "owner")].committed
	a.mu.Unlock()
	if committed {
		t.Fatal("late confirmation reopened the replacement connection")
	}
}
