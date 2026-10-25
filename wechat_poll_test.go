package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

func TestWechatPollingRetainsRetryablePosition(t *testing.T) {
	batch := wechatPollingBatch{response: &ilinkGetUpdatesResponse{Msgs: []ilinkMessage{{MessageID: "1"}, {MessageID: "2"}, {MessageID: "3"}}}}
	var called []string
	retry := true
	receive := func(_ context.Context, msg ilinkMessage, _ im.ReceiveResult) im.ReceiveResult {
		called = append(called, msg.MessageID.String())
		if msg.MessageID == "2" && retry {
			return im.ReceiveResult{Disposition: im.ReceiveRetryable}
		}
		return im.ReceiveResult{Disposition: im.ReceiveAccepted, RecordID: msg.MessageID.String()}
	}
	resolve := func(context.Context, ilinkMessage, im.ReceiveResult) im.ReceiveResult {
		t.Fatal("retryable was treated as uncertain")
		return im.ReceiveResult{}
	}
	if batch.advance(t.Context(), receive, resolve) || batch.position != 1 || !reflect.DeepEqual(called, []string{"1", "2"}) {
		t.Fatalf("position=%d calls=%v", batch.position, called)
	}
	retry = false
	if !batch.advance(t.Context(), receive, resolve) || !reflect.DeepEqual(called, []string{"1", "2", "2", "3"}) {
		t.Fatalf("position=%d calls=%v", batch.position, called)
	}
}

func TestWechatPollingUncertainQueriesBeforeContinuing(t *testing.T) {
	store := testIngressStore(t)
	batch := wechatPollingBatch{response: &ilinkGetUpdatesResponse{Msgs: []ilinkMessage{{MessageID: "1"}, {MessageID: "2"}, {MessageID: "3"}}}}
	var calls, queries int
	receive := func(_ context.Context, msg ilinkMessage, _ im.ReceiveResult) im.ReceiveResult {
		calls++
		if msg.MessageID == "2" {
			return im.ReceiveResult{Disposition: im.ReceiveUncertain}
		}
		return im.ReceiveResult{Disposition: im.ReceiveTransportIgnored}
	}
	business := wechatIngressBusinessID("sender", "2")
	resolve := func(_ context.Context, _ ilinkMessage, previous im.ReceiveResult) im.ReceiveResult {
		queries++
		result, err := store.lookupCommittedBusiness("wechat", "account", business)
		if err != nil {
			return previous
		}
		return result
	}
	if batch.advance(t.Context(), receive, resolve) || calls != 2 {
		t.Fatal("crossed uncertain item")
	}
	if batch.advance(t.Context(), receive, resolve) || calls != 2 || queries != 1 {
		t.Fatal("resubmitted uncertain item")
	}
	record := IngressRecord{Platform: "wechat", AccountID: "account", ChatID: "sender", SenderID: "sender", BusinessID: business, Kind: "task", State: "rejected", Reason: "authorization_denied", ReceivedAt: time.Now()}
	if result := store.AcceptRecord(t.Context(), record, nil); result.Disposition != im.ReceiveRejected {
		t.Fatal(result)
	}
	if !batch.advance(t.Context(), receive, resolve) || calls != 3 || queries != 2 {
		t.Fatalf("calls=%d queries=%d", calls, queries)
	}
}

func TestWechatPollingOnlyCommittedRefusalAdvances(t *testing.T) {
	for _, tc := range []struct {
		name     string
		result   im.ReceiveResult
		complete bool
	}{
		{"ignored", im.ReceiveResult{Disposition: im.ReceiveTransportIgnored}, true},
		{"duplicate", im.ReceiveResult{Disposition: im.ReceiveDuplicate, State: "consumed"}, true},
		{"rejected", im.ReceiveResult{Disposition: im.ReceiveRejected, RecordID: "durable"}, true},
		{"uncommitted_refusal", im.ReceiveResult{Disposition: im.ReceiveRejected}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			batch := wechatPollingBatch{response: &ilinkGetUpdatesResponse{Msgs: []ilinkMessage{{}}}}
			fn := func(context.Context, ilinkMessage, im.ReceiveResult) im.ReceiveResult { return tc.result }
			if complete := batch.advance(t.Context(), fn, fn); complete != tc.complete {
				t.Fatal(complete)
			}
		})
	}
}

func TestWechatUpdatesDefensiveBounds(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		tooLarge   bool
	}{
		{"body_limit", strings.Repeat(" ", wechatMaxBatchBytes+1), true},
		{"message_limit", `{"msgs":[` + strings.Repeat(`{},`, wechatMaxBatchMessages) + `{}]}`, true},
		{"wrong_list", `{"msgs":{}}`, false},
		{"wrong_object", `null`, false},
		{"truncated", `{"msgs":[{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeWechatUpdates(strings.NewReader(tc.body))
			if err == nil || errors.Is(err, errWechatBatchTooLarge) != tc.tooLarge {
				t.Fatal(err)
			}
		})
	}
	valid := `{"get_updates_buf":"opaque","msgs":[` + strings.Repeat(`{},`, wechatMaxBatchMessages-1) + `{"message_id":18446744073709551615}]}`
	response, err := decodeWechatUpdates(strings.NewReader(valid))
	if err != nil || len(response.Msgs) != wechatMaxBatchMessages || response.Msgs[len(response.Msgs)-1].MessageID.String() != "18446744073709551615" {
		t.Fatalf("response=%v error=%v", response != nil, err)
	}
	if response.Msgs[0].receivedAt.IsZero() || response.Msgs[0].receivedAt != response.Msgs[len(response.Msgs)-1].receivedAt {
		t.Fatal("batch lost first observation time")
	}
}

func TestWechatMalformedTopLevelIDNeverUsesItemFallback(t *testing.T) {
	for _, id := range []string{`""`, `null`, `"abc"`, `{}`, `[]`, `true`, `0`, `-1`, `1.5`, `18446744073709551616`} {
		t.Run(id, func(t *testing.T) {
			body := `{"msgs":[{"message_id":` + id + `,"item_list":[{"msg_id":"fallback"}]},{"message_id":"18446744073709551615"}]}`
			response, err := decodeWechatUpdates(strings.NewReader(body))
			if err != nil || len(response.Msgs) != 2 {
				t.Fatal(response, err)
			}
			if response.Msgs[0].inboundMessageID() != "" || response.Msgs[1].inboundMessageID() != "18446744073709551615" {
				t.Fatal("invalid top-level ID fell back or blocked the next message")
			}
		})
	}
	response, err := decodeWechatUpdates(strings.NewReader(`{"msgs":[{"item_list":[{"msg_id":"fallback"}]}]}`))
	if err != nil || response.Msgs[0].inboundMessageID() != "fallback" {
		t.Fatal("missing top-level ID did not use item fallback", err)
	}
}

func TestWechatCursorMissingBufPreservesCheckpoint(t *testing.T) {
	previous := im.IngressCursor{Kind: "wechat", Version: 1, Generation: 4, Wechat: &im.WechatIngressCursor{AccountID: "account", Buf: "opaque-prev", BatchVersion: 8}}
	for _, buf := range []string{"", "opaque-prev", "opaque-next"} {
		next, err := nextWechatCursor(previous, "account", buf)
		if err != nil || next.Generation != 5 || next.Wechat.BatchVersion != 9 {
			t.Fatal(next, err)
		}
		if buf == "" && next.Wechat.Buf != previous.Wechat.Buf {
			t.Fatal("empty next cleared checkpoint")
		}
	}
}

func TestWechatPollingTokenRotationCancelsSnapshot(t *testing.T) {
	a := newTestWechatAdapter(t)
	old := a.token.Load()
	old.BaseURL = "https://example.invalid"
	started := make(chan struct{})
	a.httpClient.Transport = imageRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "Bearer "+old.Token {
			t.Error("request used another credential")
		}
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := a.pollingContext(t.Context(), old)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.getUpdatesSnapshot(ctx, old, "committed"); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	a.swapPollingToken(&TokenData{AccountID: old.AccountID, Token: "replacement", BaseURL: old.BaseURL})
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("old request survived token replacement")
	}
}

func TestWechatCursorRejectsOldAccountSnapshot(t *testing.T) {
	service, _, _, _ := ingressTestService(t)
	a := newTestWechatAdapter(t)
	a.msgRouter = service.router
	old := a.token.Load()
	epoch, _ := a.currentPollingEpoch(old)
	next, _ := nextWechatCursor(im.IngressCursor{}, old.AccountID, "old-next")
	a.swapPollingToken(&TokenData{AccountID: "replacement", Token: "replacement-token"})
	if err := a.commitPollingCursor(t.Context(), old, epoch, im.IngressCursor{}, next); !errors.Is(err, errWechatPollingIdentityChanged) {
		t.Fatal(err)
	}
	if actual, err := service.store.Cursor("wechat", old.AccountID); err != nil || actual.Generation != 0 {
		t.Fatal(actual, err)
	}
}

func TestWechatCursorEpochChangeStopsAdvance(t *testing.T) {
	service, _, _, _ := ingressTestService(t)
	a := newTestWechatAdapter(t)
	a.msgRouter = service.router
	token := a.token.Load()
	epoch, _ := a.currentPollingEpoch(token)
	next, _ := nextWechatCursor(im.IngressCursor{}, token.AccountID, "next")
	p := service.router.admission.pairing
	p.mu.Lock()
	p.platforms["wechat"].epoch++
	p.mu.Unlock()
	if err := a.commitPollingCursor(t.Context(), token, epoch, im.IngressCursor{}, next); !errors.Is(err, errWechatPollingIdentityChanged) {
		t.Fatal(err)
	}
	if actual, err := service.store.Cursor("wechat", token.AccountID); err != nil || actual.Generation != 0 {
		t.Fatal(actual, err)
	}
}

func TestWechatUpdatesSnapshotClosesBody(t *testing.T) {
	a := newTestWechatAdapter(t)
	a.token.Load().BaseURL = "https://example.invalid"
	closed := false
	a.httpClient.Transport = imageRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &wechatPollTestBody{Reader: strings.NewReader(`{"msgs":[]}`), close: func() { closed = true }}}, nil
	})
	if _, err := a.getUpdates(); err != nil || !closed {
		t.Fatal(err, closed)
	}
}

type wechatPollTestBody struct {
	io.Reader
	close func()
}

func (b *wechatPollTestBody) Close() error { b.close(); return nil }
