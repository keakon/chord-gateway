package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/keakon/chord-gateway/internal/im"

	wecom "github.com/keakon/chord-gateway/internal/platform/wecom"
)

func TestWecomDuplicateMustNotPromptResend(t *testing.T) {
	a, _, store, r := wecomAdapterFixture(t)
	replies := make(chan string, 8)
	startWecomAdapterTestWS(t, a, func(_, body string) int { replies <- body; return 0 })
	if err := a.ReceiveFrameForTest(a.ContextForTest(), wecomAdapterCallback("bot", "first", "same-business")); err != nil {
		t.Fatal(err)
	}
	waitIngressCondition(t, func() bool { return countWecomIngressEvent(store, "received") == 1 })
	r.ingress.normalizer.receiver = testIngressReceiverFunc(func(context.Context, im.IngressEnvelope) im.ReceiveResult {
		return im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "ingress_unavailable"}
	})
	if err := a.ReceiveFrameForTest(a.ContextForTest(), wecomAdapterCallback("bot", "redelivery", "same-business")); err != nil {
		t.Fatal(err)
	}
	waitIngressCondition(t, func() bool { return countWecomIngressEvent(store, "acceptance_unconfirmed") == 1 })
	select {
	case text := <-replies:
		if strings.Contains(text, "Please send it again") || !strings.Contains(text, "could not confirm receipt") {
			t.Fatalf("already durably accepted business message was reported lost and prompted resend: %s", text)
		}
	case <-time.After(time.Second):
		t.Fatal("diagnostic did not arrive")
	}
	if countWecomIngressEvent(store, "lost_before_accept") != 0 {
		t.Fatal("duplicate attempt misreported as lost input")
	}
}

func TestWecomHeartbeatDuringRecovery(t *testing.T) {
	preparing, release := make(chan struct{}), make(chan struct{})
	pings := make(chan struct{}, 8)
	w, _ := wecom.StartWSForTest(t, func(c *websocket.Conn) {
		if wecom.SubscribeWSForTest(c) != nil {
			return
		}
		for {
			f, err := wecom.ReadWSFrameForTest(c)
			if err != nil {
				return
			}
			if f.Cmd == "ping" {
				pings <- struct{}{}
			}
			code := 0
			_ = wecom.WriteWSACKForTest(c, f.Headers.ReqID, &code)
		}
	}, wecom.WecomWSCallbacks{OnAuthenticated: func(ctx context.Context) error {
		close(preparing)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}, func(w *wecom.WecomWS) { w.SetHeartbeatIntervalForTest(20 * time.Millisecond) })
	defer w.Stop()
	defer close(release)
	<-preparing
	select {
	case <-pings:
	case <-time.After(time.Second):
		t.Fatal("authenticated connection sent no heartbeat during recovery")
	}
}

func TestWecomCaptureLockMustNotBlockACKOrStop(t *testing.T) {
	a, msg, _, r := wecomAdapterFixture(t)
	key := (processKey{workspaceID: "project", imType: "wecom", chatID: msg.ChatID}).String()
	p := r.mgr.GetProcessForKey(key)
	trigger, captured := make(chan struct{}), make(chan struct{})
	w, _ := wecom.StartWSForTest(t, func(c *websocket.Conn) {
		if wecom.SubscribeWSForTest(c) != nil {
			return
		}
		<-trigger
		_ = c.WriteMessage(websocket.TextMessage, wecomAdapterCallback("bot", "callback", "business"))
		for {
			f, err := wecom.ReadWSFrameForTest(c)
			if err != nil {
				return
			}
			code := 0
			_ = wecom.WriteWSACKForTest(c, f.Headers.ReqID, &code)
		}
	}, wecom.WecomWSCallbacks{CaptureFrame: func(ctx context.Context, raw []byte) context.Context {
		close(captured)
		return a.CaptureFrameForTest(ctx, raw)
	}}, func(w *wecom.WecomWS) { w.SetBotIDForTest("bot") })
	wecom.WaitWSReadyForTest(t, w)
	p.mu.Lock()
	close(trigger)
	<-captured
	err := w.SendMarkdown(context.Background(), wecom.WecomSendTarget{ChatID: "owner", ChatType: 1}, "output")
	if err != nil {
		t.Errorf("server ACK was blocked behind business state lock: %v", err)
	}
	stopped := make(chan struct{})
	go func() { w.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Error("Stop cannot join reader until unrelated process lock is released")
	}
	p.mu.Unlock()
	<-stopped
}

func TestWecomBusyCaptureRemainsRejectedAfterStateChanges(t *testing.T) {
	for _, text := range []string{"/allow", "ordinary input"} {
		t.Run(text, func(t *testing.T) {
			a, msg, store, r := wecomAdapterFixture(t)
			key := (processKey{workspaceID: "project", imType: "wecom", chatID: msg.ChatID}).String()
			p := r.mgr.GetProcessForKey(key)
			raw := []byte(strings.Replace(string(wecomAdapterCallback("bot", "busy-delivery", "busy-business")), `"content":"task"`, `"content":"`+text+`"`, 1))
			p.mu.Lock()
			ctx := a.CaptureFrameForTest(a.ContextForTest(), raw)
			p.state.PendingConfirm = &ConfirmPayload{RequestID: "replacement-confirm", ToolName: "Done"}
			writer := p.stdin.(*ingressTestWriter)
			p.mu.Unlock()
			e := ctx.Value(wecom.CapturedEnvelopeContextKey{}).(im.IngressEnvelope)
			if e.Capture == nil || !e.Capture.Unavailable {
				t.Fatal("busy state was not frozen as unavailable")
			}
			if err := a.ReceiveFrameForTest(ctx, raw); err != nil {
				t.Fatal(err)
			}
			waitIngressCondition(t, func() bool {
				records, err := store.listAll()
				return err == nil && len(records) == 1 && records[0].State == "rejected" && records[0].Reason == "control_target_unavailable" && records[0].Text == ""
			})
			if len(writer.snapshot()) != 0 {
				t.Fatal("busy callback executed under replacement state")
			}
			// A fresh capture on redelivery must not revive the rejected business ID.
			if err := a.ReceiveFrameForTest(a.CaptureFrameForTest(a.ContextForTest(), raw), raw); err != nil {
				t.Fatal(err)
			}
			waitIngressCondition(t, func() bool { return countWecomIngressEvent(store, "duplicate") == 1 })
			if len(writer.snapshot()) != 0 {
				t.Fatal("duplicate revived busy callback")
			}
		})
	}
}

func TestWecomCaptureDoesNotWaitForManagerTransferOrPins(t *testing.T) {
	for _, resource := range []string{"manager", "transfer", "pins"} {
		t.Run(resource, func(t *testing.T) {
			_, msg, _, r := wecomAdapterFixture(t)
			mgr := r.mgr
			key := (processKey{workspaceID: "project", imType: "wecom", chatID: msg.ChatID}).String()
			msg.Text = "/status"
			var unlock func()
			switch resource {
			case "manager":
				mgr.mu.Lock()
				unlock = mgr.mu.Unlock
			case "transfer":
				msg.Text = "/takeover status"
				mgr.transfers.mu.Lock()
				unlock = mgr.transfers.mu.Unlock
			case "pins":
				mgr.mu.Lock()
				delete(mgr.procs, key)
				mgr.mu.Unlock()
				mgr.pins.mu.Lock()
				unlock = mgr.pins.mu.Unlock
			}
			done := make(chan bool, 1)
			go func() { _, _, ok := r.tryCaptureIngress(msg); done <- ok }()
			select {
			case ok := <-done:
				if ok {
					t.Error("busy state granted capture")
				}
			case <-time.After(time.Second):
				t.Error("capture blocked on " + resource)
			}
			unlock()
		})
	}
}

func TestWecomHeartbeatTimeoutCancelsRecovery(t *testing.T) {
	preparing, cancelled := make(chan struct{}, 1), make(chan struct{}, 1)
	w, _ := wecom.StartWSForTest(t, func(c *websocket.Conn) {
		if wecom.SubscribeWSForTest(c) != nil {
			return
		}
		for {
			if _, err := wecom.ReadWSFrameForTest(c); err != nil {
				return
			}
		}
	}, wecom.WecomWSCallbacks{OnAuthenticated: func(ctx context.Context) error {
		preparing <- struct{}{}
		<-ctx.Done()
		cancelled <- struct{}{}
		return ctx.Err()
	}}, func(w *wecom.WecomWS) {
		w.SetHeartbeatIntervalForTest(10 * time.Millisecond)
		w.SetHeartbeatACKTimeoutForTest(20 * time.Millisecond)
		w.SetHeartbeatBackoffForTest(time.Hour)
	})
	<-preparing
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("missing heartbeat ACK did not cancel recovery")
	}
	w.Stop()
}

func TestWecomReconnectBackoffResetsOnlyAfterStability(t *testing.T) {
	ready := make(chan struct{}, 1)
	delays := make(chan time.Duration, 8)
	var attempts atomic.Int32
	w, _ := wecom.StartWSForTest(t, func(c *websocket.Conn) {
		if wecom.SubscribeWSForTest(c) != nil {
			return
		}
		<-ready
		if attempts.Add(1) == 3 {
			time.Sleep(80 * time.Millisecond)
		} else {
			time.Sleep(5 * time.Millisecond)
		}
	}, wecom.WecomWSCallbacks{OnReady: func(context.Context) error { ready <- struct{}{}; return nil }}, func(w *wecom.WecomWS) {
		w.SetStableAfterForTest(50 * time.Millisecond)
		w.SetJitterForTest(func(delay time.Duration) time.Duration { delays <- delay; return time.Millisecond })
	})
	defer w.Stop()
	for _, want := range []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 20 * time.Millisecond} {
		select {
		case got := <-delays:
			if got != want {
				t.Fatalf("backoff=%s want %s", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("missing reconnect")
		}
	}
}

func TestWecomReconnectJitterBounds(t *testing.T) {
	w := wecom.NewWSForTest("bot", "secret", wecom.WecomWSCallbacks{})
	seen := make(map[time.Duration]bool)
	for range 100 {
		delay := w.JitterForTest(time.Second)
		if delay < 800*time.Millisecond || delay > 1200*time.Millisecond {
			t.Fatalf("jitter outside bounds: %s", delay)
		}
		seen[delay] = true
	}
	if len(seen) == 1 {
		t.Fatal("backoff has no jitter")
	}
}
