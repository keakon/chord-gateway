package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

func assertWechatConnectionStopped(t *testing.T, a *WechatAdapter) {
	t.Helper()
	a.mu.Lock()
	stopped, contexts, pending := a.stopped, len(a.replyContexts), len(a.ingressPending)
	a.mu.Unlock()
	if !stopped || contexts != 0 || pending != 0 || a.adapterContext().Err() == nil {
		t.Fatalf("stopped=%v contexts=%d pending=%d context=%v", stopped, contexts, pending, a.adapterContext().Err())
	}
	p := a.pairingRouter().admission.pairing
	p.mu.Lock()
	ready := p.platforms["wechat"].ready
	p.mu.Unlock()
	if ready {
		t.Fatal("stopped supervisor left platform ready")
	}
	lock, err := im.AcquireIMAccountLock("wechat", a.token.Load().AccountID)
	if err != nil {
		t.Fatalf("stopped supervisor retained account lock: %v", err)
	}
	lock.Close()
}

func TestWechatFatalPollingExitPausesSharedDelivery(t *testing.T) {
	a, service, _, writer := newWechatIngressTest(t)
	result := a.receiveIncomingMessage(t.Context(), wechatTextMessage("1", "owner", "pending task", ""), a.token.Load())
	if result.Disposition != im.ReceiveAccepted {
		t.Fatal(result)
	}
	a.cachePairedContext("bot", "cached-chat", "context", true)
	a.httpClient.Transport = imageRoundTripper(func(*http.Request) (*http.Response, error) {
		return wechatMonitorResponse(strings.Repeat(" ", wechatMaxBatchBytes+1)), nil
	})
	if err := a.Connect(); !errors.Is(err, errWechatBatchTooLarge) {
		t.Fatalf("Connect error = %v", err)
	}
	assertWechatConnectionStopped(t, a)
	// Even a late context publisher must not reopen delivery without a running
	// supervisor. The shared service remains available to other platforms.
	a.cachePairedContext("bot", "owner", "late-context", true)
	epoch, _ := a.currentPollingEpoch(a.token.Load())
	a.mu.Lock()
	key := compositeKey("bot", "owner")
	reply := a.replyContexts[key]
	reply.accountEpoch = epoch
	a.replyContexts[key] = reply
	a.mu.Unlock()
	if readiness := a.IngressDeliveryReadiness(im.ChatIdentity{Platform: "wechat", AccountID: "bot", ChatID: "owner"}); readiness.Ready {
		t.Fatal("stopped adapter reopened delivery")
	}
	if !service.Available() {
		t.Fatal("stopped adapter closed the shared service")
	}
	records, err := service.store.listAll()
	if err != nil || len(records) != 1 || records[0].State != "received" {
		t.Fatalf("accepted input was revoked on transport exit: %+v, %v", records, err)
	}
	if tasks := wechatTaskWrites(writer); len(tasks) != 0 {
		t.Fatal("stopped adapter executed waiting input", tasks)
	}
}

func TestWechatDisconnectCancelsInFlightTextAndPolling(t *testing.T) {
	a, _, _, _ := newWechatIngressTest(t)
	a.cachePairedContext("bot", "owner", "context", true)
	pollStarted, sendStarted := make(chan struct{}), make(chan struct{})
	a.httpClient.Transport = imageRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/ilink/bot/getupdates" {
			close(pollStarted)
		} else {
			close(sendStarted)
		}
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	connected := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		connected <- a.Connect()
	}()
	t.Cleanup(func() {
		a.Disconnect()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("supervisor remained active during cleanup")
		}
	})
	select {
	case <-pollStarted:
	case err := <-connected:
		t.Fatalf("Connect exited before polling: %v", err)
	case <-time.After(time.Second):
		t.Fatal("polling did not start")
	}
	sent := make(chan error, 1)
	go func() { sent <- a.SendTextContext(context.Background(), "owner", "message") }()
	select {
	case <-sendStarted:
	case <-time.After(time.Second):
		t.Fatal("text request did not start")
	}
	a.Disconnect()
	select {
	case err := <-connected:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("polling did not stop")
	}
	select {
	case err := <-sent:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("in-flight send error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight text survived supervisor exit")
	}
	assertWechatConnectionStopped(t, a)
}
