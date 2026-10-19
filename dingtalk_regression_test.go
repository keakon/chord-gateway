package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A receive lock must protect the application's entire lifecycle, including a
// send already admitted before shutdown. Socket shutdown alone is insufficient.
func TestDingtalkRegressionAccountLocksCoverSendJoin(t *testing.T) {
	f := newDingtalkAdapterFixture(t)
	dingtalkAdapterVerify(t, f)
	registration := make(chan struct{})
	f.a.stream.openConnection = func(ctx context.Context) (string, string, error) {
		close(registration)
		<-ctx.Done()
		return "", "", ctx.Err()
	}
	connectDone := make(chan error, 1)
	go func() { connectDone <- f.a.Connect() }()
	dingtalkWaitTest(t, registration)
	sendEntered, sendRelease := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(sendRelease) }) }
	defer release()
	f.a.sender.options.SendSegment = func(context.Context, string, string) error {
		close(sendEntered)
		<-sendRelease
		return context.Canceled
	}
	target, _ := dingtalkEncodeTarget(f.a.cfg.CorpID, "private", "test-staff")
	sendCtx := context.WithValue(t.Context(), replyAccountContextKey{}, f.a.account)
	sendDone := make(chan error, 1)
	go func() { sendDone <- f.a.SendTextContext(sendCtx, target, "test response") }()
	dingtalkWaitTest(t, sendEntered)
	disconnectDone := make(chan struct{})
	go func() { f.a.Disconnect(); close(disconnectDone) }()
	dingtalkWaitTest(t, f.a.ctx.Done())
	// Continue checking across scheduler turns, so an early unlock in Connect's
	// defer chain cannot hide behind the initial cancellation notification.
	deadline := time.Now().Add(40 * time.Millisecond)
	for time.Now().Before(deadline) {
		locks, err := acquireDingtalkAccountLocks(f.a.cfg)
		if err == nil {
			locks.Close()
			t.Fatal("account locks released before admitted send joined")
		}
		if !errors.Is(err, errInstanceRunning) {
			t.Fatal(err)
		}
		select {
		case <-disconnectDone:
			t.Fatal("Disconnect returned while admitted send was still running")
		case <-time.After(time.Millisecond):
		}
	}
	release()
	dingtalkWaitTest(t, sendDone)
	dingtalkWaitTest(t, disconnectDone)
	if err := dingtalkWaitTest(t, connectDone); err != nil {
		t.Fatal(err)
	}
	locks, err := acquireDingtalkAccountLocks(f.a.cfg)
	if err != nil {
		t.Fatalf("account lock retained after all workers joined: %v", err)
	}
	locks.Close()
}

func TestDingtalkRegressionCurrentProbeActivatesVerifiedButUnreadyConnection(t *testing.T) {
	f := newDingtalkAdapterFixture(t)
	if err := f.a.onConnected(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Evidence can survive an interrupted activation. It proves enterprise
	// identity but cannot itself publish readiness for a different generation.
	f.a.verified = true
	f.a.mu.Lock()
	f.a.verified = true
	oldGeneration := f.a.generation
	f.a.mu.Unlock()
	f.a.onUnavailable()
	// Model a current handshake that has not yet published PlatformReady.
	f.a.mu.Lock()
	f.a.connected = true
	f.a.generation++
	f.a.mu.Unlock()
	if err := f.a.activate(t.Context(), oldGeneration); !errors.Is(err, context.Canceled) {
		t.Fatalf("old generation activated: %v", err)
	}
	if f.a.IngressDeliveryReadiness(ChatIdentity{"dingtalk", f.a.account, "chat"}).Ready {
		t.Fatal("old evidence activated current connection")
	}
	callback := dingtalkIdentityTestCallback()
	callback.MsgID, callback.Text.Content = "current-generation-probe", "hello"
	result := dingtalkAdapterReceive(t, f.a, callback, "current-generation-delivery")
	if result.Disposition != ReceiveRejected || result.Reason != "identity_probe" {
		t.Fatalf("current private probe refused: %+v", result)
	}
	if !f.a.IngressDeliveryReadiness(ChatIdentity{"dingtalk", f.a.account, "chat"}).Ready {
		t.Fatal("verified but unready connection could not recover from a current private probe")
	}
	if f.store.Snapshot().IMConfigByType("dingtalk").OwnerIdentity() != nil {
		t.Fatal("recovery probe assigned owner")
	}
}

func TestDingtalkRegressionAcceptedWorkSurvivesStreamOutage(t *testing.T) {
	f := newDingtalkAdapterFixture(t)
	dingtalkAdapterVerify(t, f)
	var sends atomic.Int32
	f.a.sender.options.SendSegment = func(ctx context.Context, _ string, text string) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("send has no deadline")
		}
		if text == "background result during reconnect" && time.Until(deadline) < 4*time.Minute {
			t.Error("account scope incorrectly shortened background output deadline")
		}
		if text == "flow response" && time.Until(deadline) > IMControlSendTimeout {
			t.Error("flow response exceeded control deadline")
		}
		sends.Add(1)
		return nil
	}
	msg, err := normalizeDingtalkCallback(f.a.cfg, dingtalkIdentityTestPayload(t, dingtalkIdentityTestCallback()))
	if err != nil {
		t.Fatal(err)
	}
	code, dir := dingtalkAdapterNonce(f), t.TempDir()
	msg.Text = fmt.Sprintf("/bind %q %s", dir, code)
	ticket, ok := f.r.admission.Authorize(msg)
	if !ok {
		t.Fatal("binding not authorized")
	}
	if _, err := (&BindingService{router: f.r}).Bind(t.Context(), msg, ticket, []string{dir, code}); err != nil {
		t.Fatal(err)
	}
	permit := ReplyPermit{Identity: ticket.Identity, AccountEpoch: ticket.AccountEpoch, ExpiresAt: time.Now().Add(time.Minute), ticket: ticket}
	if err := f.r.SendFlowReply(t.Context(), permit, "flow response"); err != nil {
		t.Fatal(err)
	}
	ws, err := f.r.getConfig().ResolveWorkspace(msg.IMType, msg.ChatID)
	if err != nil {
		t.Fatal(err)
	}
	key := (processKey{workspaceID: ws.ID, imType: msg.IMType, chatID: msg.ChatID}).String()
	writer := &ingressTestWriter{}
	ready := make(chan struct{})
	close(ready)
	p := &ChordProcess{key: key, runtimeID: "runtime", sessionEpoch: 1, stdin: writer, readySignal: ready, state: ControlState{SessionID: "session"}}
	f.r.mgr.mu.Lock()
	f.r.mgr.procs[key] = p
	f.r.mgr.mu.Unlock()
	t.Cleanup(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, observer := range p.inputObservers {
			if observer.timer != nil {
				observer.timer.Stop()
			}
		}
	})
	if err := f.r.mgr.pins.Set(pinKeyForBinding(key), "session"); err != nil {
		t.Fatal(err)
	}
	id := ticket.Identity
	f.s.mu.Lock()
	f.s.blocked[id] = ingressBlocked{since: time.Now(), reason: "test"}
	f.s.mu.Unlock()
	callback := dingtalkIdentityTestCallback()
	callback.MsgID, callback.Text.Content = "accepted-before-outage", "run accepted task"
	if result := dingtalkAdapterReceive(t, f.a, callback, "delivery-before-outage"); result.Disposition != ReceiveAccepted {
		t.Fatal(result)
	}
	f.a.onUnavailable()
	if !f.r.admission.Valid(ticket) || !f.a.IngressDeliveryReadiness(id).Ready {
		t.Fatal("temporary outage revoked accepted work")
	}
	if f.a.IngressDeliveryReadiness(ChatIdentity{"dingtalk", "other-account", msg.ChatID}).Ready {
		t.Fatal("other account became available")
	}
	callback.MsgID = "new-input-during-outage"
	if _, err := f.a.onCallback(t.Context(), dingtalkAdapterFrame(t, callback, "outage-delivery")); !errors.Is(err, errIngressClosed) {
		t.Fatalf("outage admitted new input: %v", err)
	}
	f.s.mu.Lock()
	delete(f.s.blocked, id)
	f.s.mu.Unlock()
	f.s.signal()
	waitIngressCondition(t, func() bool { return len(writer.snapshot()) == 1 })
	target, ok := f.r.targetForKey(key)
	if !ok {
		t.Fatal("bound target unavailable")
	}
	ctx := context.WithValue(t.Context(), replyAccountContextKey{}, f.a.account)
	if err := f.r.SendBound(ctx, target, "background result during reconnect"); err != nil {
		t.Fatal(err)
	}
	if sends.Load() != 3 { // Binding probe, flow reply, and background result.
		t.Fatalf("unexpected sends: %d", sends.Load())
	}
	f.a.Disconnect()
	if f.a.IngressDeliveryReadiness(id).Ready {
		t.Fatal("shutdown left delivery enabled")
	}
	if err := f.r.SendBound(t.Context(), target, "must not send after shutdown"); err == nil || sends.Load() != 3 {
		t.Fatalf("shutdown permitted HTTP output: %v", err)
	}
}

func TestDingtalkNotReadySendIsCategorized(t *testing.T) {
	f := newDingtalkAdapterFixture(t)
	dingtalkAdapterVerify(t, f)
	target, err := dingtalkEncodeTarget(f.a.cfg.CorpID, "private", "test-staff")
	if err != nil {
		t.Fatal(err)
	}
	sendCtx := context.WithValue(t.Context(), replyAccountContextKey{}, f.a.account)
	f.a.mu.Lock()
	f.a.stopping = true
	f.a.mu.Unlock()
	// A definite non-delivery must use the shared category vocabulary instead
	// of an unobserved bare error, so operators can see it in text metrics.
	err = f.a.SendTextContext(sendCtx, target, "output")
	var failure *dingtalkAPIError
	if !errors.As(err, &failure) || failure.Category != "connection_unavailable" {
		t.Fatalf("not-ready send error: %v", err)
	}
	if category := failure.IMTextSendFailureCategory(); category != "connection_unavailable" {
		t.Fatalf("not-ready send category: %q", category)
	}
}

func TestDingtalkCaptureBusyRefusalStaysRetryable(t *testing.T) {
	f := newDingtalkAdapterFixture(t)
	dingtalkAdapterVerify(t, f)
	// Bind the callback's chat: capture only freezes state for a bound target.
	f.a.sender.options.SendSegment = func(context.Context, string, string) error { return nil }
	binding := dingtalkIdentityTestCallback()
	binding.MsgID = "busy-bind"
	msg, err := normalizeDingtalkCallback(f.a.cfg, dingtalkIdentityTestPayload(t, binding))
	if err != nil {
		t.Fatal(err)
	}
	code, dir := dingtalkAdapterNonce(f), t.TempDir()
	msg.Text = fmt.Sprintf("/bind %q %s", dir, code)
	ticket, ok := f.r.admission.Authorize(msg)
	if !ok {
		t.Fatal("binding not authorized")
	}
	if _, err := (&BindingService{router: f.r}).Bind(t.Context(), msg, ticket, []string{dir, code}); err != nil {
		t.Fatal(err)
	}
	// A takeover command captures through the transfer store without waiting;
	// holding it models a delivery racing the target's state write.
	callback := dingtalkIdentityTestCallback()
	callback.MsgID, callback.Text.Content = "busy-delivery", "/takeover status"
	frame := dingtalkAdapterFrame(t, callback, "busy-delivery")
	transfers := f.r.mgr.transfers
	if transfers == nil {
		t.Fatal("transfer store unavailable")
	}
	transfers.mu.Lock()
	future, err := f.a.onCallback(t.Context(), frame)
	transfers.mu.Unlock()
	// A delivery that races the target's state write must be refused as
	// target_busy with the loss visible in the metrics; robot callbacks are
	// fire-and-forget, so the refusal reply tells the user to resend.
	if future != nil || !errors.Is(err, errIngressCaptureBusy) {
		t.Fatalf("busy capture was not refused: future=%v err=%v", future, err)
	}
	var count uint64
	for _, counter := range f.r.ingressStore.MetricsSnapshot().Counters {
		if counter.Platform == "dingtalk" && counter.Event == "lost_before_accept" && counter.Reason == "target_busy" {
			count = counter.Count
		}
	}
	if count != 1 {
		t.Fatalf("capture-busy loss metric count=%d", count)
	}
}

func TestDingtalkTrackAcceptanceRefusesAfterStopping(t *testing.T) {
	f := newDingtalkAdapterFixture(t)
	future := make(chan ReceiveResult, 1)
	f.a.mu.Lock()
	f.a.stopping = true
	f.a.mu.Unlock()
	// The reservation and the shutdown seal share one lock, so Wait can never
	// race a late Add. The stream's 500 for this frame can no longer reach the
	// platform, so the unobserved outcome is counted here instead.
	if got := f.a.trackAcceptance(future, IncomingMessage{}); got != nil {
		t.Fatal("accepted work after the shutdown seal")
	}
	var count uint64
	for _, counter := range f.r.ingressStore.MetricsSnapshot().Counters {
		if counter.Platform == "dingtalk" && counter.Event == "acceptance_unconfirmed" && counter.Reason == "transport_stopped" {
			count = counter.Count
		}
	}
	if count != 1 {
		t.Fatalf("shutdown-race loss metric count=%d", count)
	}
}
