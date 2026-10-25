package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
	"github.com/keakon/chord-gateway/internal/platform/wechat"
)

func startWechatMonitorTest(t *testing.T, a *wechat.WechatAdapter) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		err := a.MonitorLoopForTest(ctx)
		a.CloseIngressAccountForTest()
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("monitor remained active during cleanup")
		}
	})
	return cancel, done
}

func stopWechatMonitorTest(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("monitor did not stop after cancellation")
	}
}

func wechatMonitorResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func wechatMonitorRequest(t *testing.T, req *http.Request) wechat.ILinkGetUpdatesRequest {
	t.Helper()
	var body wechat.ILinkGetUpdatesRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		t.Error(err)
	}
	return body
}

type wechatMonitorRetryReceiver struct {
	service *IngressService
	ready   atomic.Bool
	blocked chan struct{}
	once    sync.Once
	mu      sync.Mutex
	ids     []string
}

func (r *wechatMonitorRetryReceiver) Accept(ctx context.Context, envelope im.IngressEnvelope) im.ReceiveResult {
	r.mu.Lock()
	r.ids = append(r.ids, envelope.Message.MessageID)
	r.mu.Unlock()
	if envelope.Message.MessageID == wechat.WechatIngressBusinessID("owner", "2") && !r.ready.Load() {
		r.once.Do(func() { close(r.blocked) })
		return im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "storage_unavailable"}
	}
	return r.service.Accept(ctx, envelope)
}

func TestWechatMonitorRetainsBatchAcrossRetryable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, service, _, _ := newWechatIngressTest(t)
		receiver := &wechatMonitorRetryReceiver{service: service, blocked: make(chan struct{})}
		service.normalizer.receiver = receiver
		var polls atomic.Int32
		a.SetTransportForTest(imageRoundTripper(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/ilink/bot/getupdates" {
				return wechatMonitorResponse(`{"ret":0}`), nil
			}
			body := wechatMonitorRequest(t, req)
			if polls.Add(1) == 1 {
				if body.GetUpdatesBuf != "" {
					t.Error("first batch did not start from committed checkpoint")
				}
				messages := []wechat.ILinkMessage{wechatTextMessage("1", "owner", "first task", "context"), wechatTextMessage("2", "owner", "second task", "context"), wechatTextMessage("3", "owner", "third task", "context")}
				encoded, _ := json.Marshal(wechat.ILinkGetUpdatesResponse{GetUpdatesBuf: "next", Msgs: messages})
				return wechatMonitorResponse(string(encoded)), nil
			}
			if body.GetUpdatesBuf != "next" {
				t.Error("next poll crossed an uncommitted checkpoint")
			}
			<-req.Context().Done()
			return nil, req.Context().Err()
		}))
		cancel, done := startWechatMonitorTest(t, a)
		select {
		case <-receiver.blocked:
		case <-time.After(time.Second):
			t.Fatal("second item did not block")
		}
		// A retryable item blocks the account's polling order; the gateway must
		// report it instead of retrying silently.
		waitIngressCondition(t, func() bool {
			for _, counter := range service.store.MetricsSnapshot().Counters {
				if counter.Platform == "wechat" && counter.Event == "acceptance_unconfirmed" && counter.Reason == "storage_unavailable" && counter.Count > 0 {
					return true
				}
			}
			return false
		})
		if cursor, err := service.store.Cursor("wechat", "bot"); err != nil || cursor.Generation != 0 {
			t.Fatal(cursor, err)
		}
		if records, err := service.store.listAll(); err != nil || len(records) != 1 || records[0].BusinessID != wechat.WechatIngressBusinessID("owner", "1") {
			t.Fatal(records, err)
		}
		receiver.mu.Lock()
		if len(receiver.ids) != 2 {
			t.Error("monitor crossed retryable item")
		}
		receiver.mu.Unlock()
		if polls.Load() != 1 {
			t.Fatal("monitor fetched another batch while retrying")
		}
		receiver.ready.Store(true)
		waitIngressCondition(t, func() bool { return polls.Load() == 2 })
		stopWechatMonitorTest(t, cancel, done)
		receiver.mu.Lock()
		defer receiver.mu.Unlock()
		if len(receiver.ids) != 4 || receiver.ids[0] != wechat.WechatIngressBusinessID("owner", "1") || receiver.ids[1] != wechat.WechatIngressBusinessID("owner", "2") || receiver.ids[2] != receiver.ids[1] || receiver.ids[3] != wechat.WechatIngressBusinessID("owner", "3") {
			t.Fatal(receiver.ids)
		}
		if records, err := service.store.listAll(); err != nil || len(records) != 3 {
			t.Fatal(records, err)
		}
	})
}

// A cancellation that lands between batch items is not a stuck acceptance, but
// the retry loop reported it as one: it warned and counted an
// acceptance_unconfirmed metric while the gateway was already stopping.
func TestWechatMonitorCancellationDoesNotReportUnconfirmedAcceptance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, service, _, _ := newWechatIngressTest(t)
		ctx, cancel := context.WithCancel(t.Context())
		receiver := &wechatMonitorCancellingReceiver{service: service, cancel: cancel}
		service.normalizer.receiver = receiver
		a.SetTransportForTest(imageRoundTripper(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/ilink/bot/getupdates" {
				return wechatMonitorResponse(`{"ret":0}`), nil
			}
			messages := []wechat.ILinkMessage{wechatTextMessage("1", "owner", "first task", "context"), wechatTextMessage("2", "owner", "second task", "context")}
			encoded, _ := json.Marshal(wechat.ILinkGetUpdatesResponse{GetUpdatesBuf: "next", Msgs: messages})
			return wechatMonitorResponse(string(encoded)), nil
		}))
		done := make(chan error, 1)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			err := a.MonitorLoopForTest(ctx)
			a.CloseIngressAccountForTest()
			done <- err
		}()
		t.Cleanup(func() {
			cancel()
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Error("monitor remained active during cleanup")
			}
		})
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("monitor did not stop after the in-flight cancellation")
		}
		for _, counter := range service.store.MetricsSnapshot().Counters {
			if counter.Event == "acceptance_unconfirmed" {
				t.Fatalf("cancellation was reported as an unconfirmed acceptance: %+v", counter)
			}
		}
	})
}

type wechatMonitorCancellingReceiver struct {
	service *IngressService
	cancel  context.CancelFunc
}

func (r *wechatMonitorCancellingReceiver) Accept(ctx context.Context, envelope im.IngressEnvelope) im.ReceiveResult {
	if envelope.Message.MessageID == wechat.WechatIngressBusinessID("owner", "2") {
		// The parent cancellation lands while the batch is in flight.
		r.cancel()
		return im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "storage_unavailable"}
	}
	return r.service.Accept(ctx, envelope)
}

func TestWechatMonitorMalformedIDDoesNotBlockNextMessage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, service, _, writer := newWechatIngressTest(t)
		var calls atomic.Int32
		second := make(chan struct{})
		a.SetTransportForTest(imageRoundTripper(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/ilink/bot/getupdates" {
				return wechatMonitorResponse(`{"ret":0}`), nil
			}
			if calls.Add(1) == 1 {
				return wechatMonitorResponse(`{"get_updates_buf":"next","msgs":[{"message_id":"abc","from_user_id":"owner","message_type":1,"context_token":"invalid-context","item_list":[{"msg_id":"fallback-must-not-execute","type":1,"text_item":{"text":"invalid task"}}]},{"message_id":2,"from_user_id":"owner","message_type":1,"context_token":"context","item_list":[{"type":1,"text_item":{"text":"valid task"}}]}]}`), nil
			}
			body := wechatMonitorRequest(t, req)
			if body.GetUpdatesBuf != "next" {
				t.Error("valid batch did not advance")
			}
			close(second)
			<-req.Context().Done()
			return nil, req.Context().Err()
		}))
		cancel, done := startWechatMonitorTest(t, a)
		select {
		case <-second:
		case <-time.After(time.Second):
			t.Fatal("malformed ID blocked the valid message")
		}
		stopWechatMonitorTest(t, cancel, done)
		if records, err := service.store.listAll(); err != nil || len(records) != 1 || records[0].BusinessID != wechat.WechatIngressBusinessID("owner", "2") {
			t.Fatal(records, err)
		}
		waitIngressCondition(t, func() bool { return len(writer.snapshot()) == 1 })
	})
}

func TestWechatMonitorTokenRenewalReReadsCommittedCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, service, _, _ := newWechatIngressTest(t)
		initial, _ := wechat.NextWechatCursor(im.IngressCursor{}, "bot", "committed")
		if err := service.store.CommitCursor(t.Context(), "wechat", "bot", 0, initial); err != nil {
			t.Fatal(err)
		}
		var calls atomic.Int32
		a.SetTransportForTest(imageRoundTripper(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/ilink/bot/getupdates" {
				return wechatMonitorResponse(`{"ret":0}`), nil
			}
			body := wechatMonitorRequest(t, req)
			if body.GetUpdatesBuf != "committed" {
				t.Error("credential replacement inherited uncommitted response")
			}
			if calls.Add(1) == 1 {
				a.PublishTokenForTest(&wechat.TokenData{AccountID: "bot", Token: "renewed-token", BaseURL: "https://example.invalid"})
				messages := []wechat.ILinkMessage{wechatTextMessage("99", "owner", "stale task", "stale-context")}
				encoded, _ := json.Marshal(wechat.ILinkGetUpdatesResponse{GetUpdatesBuf: "stale-next", Msgs: messages})
				return wechatMonitorResponse(string(encoded)), nil
			}
			if req.Header.Get("Authorization") != "Bearer renewed-token" {
				t.Error("new poll reused old credentials")
			}
			return wechatMonitorResponse(`{"get_updates_buf":"renewed-next","msgs":[]}`), nil
		}))
		cancel, done := startWechatMonitorTest(t, a)
		waitIngressCondition(t, func() bool {
			cursor, _ := service.store.Cursor("wechat", "bot")
			return cursor.Wechat != nil && cursor.Wechat.Buf == "renewed-next"
		})
		stopWechatMonitorTest(t, cancel, done)
		if calls.Load() != 2 {
			t.Fatal("empty batch started a busy polling loop")
		}
		if records, err := service.store.listAll(); err != nil || len(records) != 0 {
			t.Fatal("stale credential response was accepted", records, err)
		}
		if a.ReplyContextCountForTest() != 0 {
			t.Fatal("stale response updated reply context")
		}
	})
}

func TestWechatMonitorResetDiscardsInFlightResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, service, _, _ := newWechatIngressTest(t)
		var calls atomic.Int32
		second := make(chan struct{})
		a.SetTransportForTest(imageRoundTripper(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/ilink/bot/getupdates" {
				return wechatMonitorResponse(`{"ret":0}`), nil
			}
			body := wechatMonitorRequest(t, req)
			if body.GetUpdatesBuf != "" {
				t.Error("reset response advanced checkpoint")
			}
			if calls.Add(1) == 1 {
				p := service.router.admission.pairing
				p.mu.Lock()
				p.platforms["wechat"].epoch++
				p.platforms["wechat"].ready = false
				p.mu.Unlock()
				encoded, _ := json.Marshal(wechat.ILinkGetUpdatesResponse{GetUpdatesBuf: "stale", Msgs: []wechat.ILinkMessage{wechatTextMessage("1", "owner", "stale task", "stale-context")}})
				return wechatMonitorResponse(string(encoded)), nil
			}
			close(second)
			<-req.Context().Done()
			return nil, req.Context().Err()
		}))
		cancel, done := startWechatMonitorTest(t, a)
		select {
		case <-second:
		case <-time.After(time.Second):
			t.Fatal("monitor did not discard reset batch")
		}
		stopWechatMonitorTest(t, cancel, done)
		if cursor, err := service.store.Cursor("wechat", "bot"); err != nil || cursor.Generation != 0 {
			t.Fatal(cursor, err)
		}
		if records, err := service.store.listAll(); err != nil || len(records) != 0 {
			t.Fatal(records, err)
		}
	})
}

func TestWechatMonitorCommittedRecordReReadDoesNotExecuteAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, service, _, writer := newWechatIngressTest(t)
		message := wechatTextMessage("1", "owner", "only once", "context")
		if result := a.ReceiveIncomingMessageForTest(t.Context(), message, a.TokenForTest()); result.Disposition != im.ReceiveAccepted {
			t.Fatal(result)
		}
		waitIngressCondition(t, func() bool { return len(writer.snapshot()) == 1 })
		// A connection interruption can occur after intake but before the
		// next poll publishes its checkpoint in this process.
		if cursor, err := service.store.Cursor("wechat", "bot"); err != nil || cursor.Generation != 0 {
			t.Fatal(cursor, err)
		}
		var polls atomic.Int32
		second := make(chan struct{})
		a.SetTransportForTest(imageRoundTripper(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/ilink/bot/getupdates" {
				return wechatMonitorResponse(`{"ret":0}`), nil
			}
			body := wechatMonitorRequest(t, req)
			if polls.Add(1) == 1 {
				if body.GetUpdatesBuf != "" {
					t.Error("re-read did not begin at original checkpoint")
				}
				encoded, _ := json.Marshal(wechat.ILinkGetUpdatesResponse{GetUpdatesBuf: "next", Msgs: []wechat.ILinkMessage{message}})
				return wechatMonitorResponse(string(encoded)), nil
			}
			if body.GetUpdatesBuf != "next" {
				t.Error("re-read checkpoint was not committed")
			}
			close(second)
			<-req.Context().Done()
			return nil, req.Context().Err()
		}))
		cancel, done := startWechatMonitorTest(t, a)
		select {
		case <-second:
		case <-time.After(time.Second):
			t.Fatal("duplicate batch did not advance checkpoint")
		}
		stopWechatMonitorTest(t, cancel, done)
		if records, err := service.store.listAll(); err != nil || len(records) != 1 {
			t.Fatal(records, err)
		}
		if got := writer.snapshot(); len(got) != 1 {
			t.Fatal("re-read wrote another task to Chord", got)
		}
	})
}

func TestWechatMonitorCancellationKeepsSharedStoreAvailable(t *testing.T) {
	for _, mode := range []string{"long_poll", "network_backoff", "empty_same_buffer"} {
		t.Run(mode, func(t *testing.T) {
			a, service, _, _ := newWechatIngressTest(t)
			started := make(chan struct{})
			var calls atomic.Int32
			a.SetTransportForTest(imageRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/ilink/bot/getupdates" {
					return wechatMonitorResponse(`{"ret":0}`), nil
				}
				if calls.Add(1) == 1 {
					close(started)
				}
				switch mode {
				case "long_poll":
					<-req.Context().Done()
					return nil, req.Context().Err()
				case "network_backoff":
					return nil, errors.New("temporary transport failure")
				default:
					return wechatMonitorResponse(`{"msgs":[]}`), nil
				}
			}))
			cancel, done := startWechatMonitorTest(t, a)
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("poll did not start")
			}
			if mode == "empty_same_buffer" {
				waitIngressCondition(t, func() bool { c, _ := service.store.Cursor("wechat", "bot"); return c.Generation == 1 })
				select {
				case <-time.After(100 * time.Millisecond):
				case err := <-done:
					t.Fatal("monitor exited during polling wait", err)
				}
				if calls.Load() != 1 {
					t.Fatal("same-buffer empty response caused a busy loop")
				}
			}
			stopWechatMonitorTest(t, cancel, done)
			result := service.store.AcceptRecord(t.Context(), IngressRecord{Platform: "feishu", AccountID: "other-bot", ChatID: "chat", SenderID: "sender", BusinessID: "message", Kind: "task", State: "rejected", Reason: "authorization_denied", ReceivedAt: time.Now()}, nil)
			if result.Disposition != im.ReceiveRejected || !service.Available() {
				t.Fatal("WeChat shutdown closed shared ingress", result)
			}
		})
	}
}
