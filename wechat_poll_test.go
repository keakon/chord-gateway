package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
	"github.com/keakon/chord-gateway/internal/platform/wechat"
)

func TestWechatPollingUncertainQueriesBeforeContinuing(t *testing.T) {
	store := testIngressStore(t)
	batch := wechat.WechatPollingBatch{Response: &wechat.ILinkGetUpdatesResponse{Msgs: []wechat.ILinkMessage{{MessageID: "1"}, {MessageID: "2"}, {MessageID: "3"}}}}
	var calls, queries int
	receive := func(_ context.Context, msg wechat.ILinkMessage, _ im.ReceiveResult) im.ReceiveResult {
		calls++
		if msg.MessageID == "2" {
			return im.ReceiveResult{Disposition: im.ReceiveUncertain}
		}
		return im.ReceiveResult{Disposition: im.ReceiveTransportIgnored}
	}
	business := wechat.WechatIngressBusinessID("sender", "2")
	resolve := func(_ context.Context, _ wechat.ILinkMessage, previous im.ReceiveResult) im.ReceiveResult {
		queries++
		result, err := store.lookupCommittedBusiness("wechat", "account", business)
		if err != nil {
			return previous
		}
		return result
	}
	if batch.Advance(t.Context(), receive, resolve) || calls != 2 {
		t.Fatal("crossed uncertain item")
	}
	if batch.Advance(t.Context(), receive, resolve) || calls != 2 || queries != 1 {
		t.Fatal("resubmitted uncertain item")
	}
	record := IngressRecord{Platform: "wechat", AccountID: "account", ChatID: "sender", SenderID: "sender", BusinessID: business, Kind: "task", State: "rejected", Reason: "authorization_denied", ReceivedAt: time.Now()}
	if result := store.AcceptRecord(t.Context(), record, nil); result.Disposition != im.ReceiveRejected {
		t.Fatal(result)
	}
	if !batch.Advance(t.Context(), receive, resolve) || calls != 3 || queries != 2 {
		t.Fatalf("calls=%d queries=%d", calls, queries)
	}
}

func TestWechatPollingTokenRotationCancelsSnapshot(t *testing.T) {
	a := newTestWechatAdapter(t)
	old := a.TokenForTest()
	old.BaseURL = "https://example.invalid"
	started := make(chan struct{})
	a.SetTransportForTest(imageRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "Bearer "+old.Token {
			t.Error("request used another credential")
		}
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))
	ctx, cancel := a.PollingContextForTest(t.Context(), old)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.GetUpdatesSnapshotForTest(ctx, old, "committed"); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	a.SwapPollingTokenForTest(&wechat.TokenData{AccountID: old.AccountID, Token: "replacement", BaseURL: old.BaseURL})
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
	a.SetHostForTest(service.router)
	old := a.TokenForTest()
	epoch, _ := a.CurrentPollingEpochForTest(old)
	next, _ := wechat.NextWechatCursor(im.IngressCursor{}, old.AccountID, "old-next")
	a.SwapPollingTokenForTest(&wechat.TokenData{AccountID: "replacement", Token: "replacement-token"})
	if err := a.CommitPollingCursorForTest(t.Context(), old, epoch, im.IngressCursor{}, next); !errors.Is(err, wechat.ErrWechatPollingIdentityChanged) {
		t.Fatal(err)
	}
	if actual, err := service.store.Cursor("wechat", old.AccountID); err != nil || actual.Generation != 0 {
		t.Fatal(actual, err)
	}
}

func TestWechatCursorEpochChangeStopsAdvance(t *testing.T) {
	service, _, _, _ := ingressTestService(t)
	a := newTestWechatAdapter(t)
	a.SetHostForTest(service.router)
	token := a.TokenForTest()
	epoch, _ := a.CurrentPollingEpochForTest(token)
	next, _ := wechat.NextWechatCursor(im.IngressCursor{}, token.AccountID, "next")
	p := service.router.admission.pairing
	p.mu.Lock()
	p.platforms["wechat"].epoch++
	p.mu.Unlock()
	if err := a.CommitPollingCursorForTest(t.Context(), token, epoch, im.IngressCursor{}, next); !errors.Is(err, wechat.ErrWechatPollingIdentityChanged) {
		t.Fatal(err)
	}
	if actual, err := service.store.Cursor("wechat", token.AccountID); err != nil || actual.Generation != 0 {
		t.Fatal(actual, err)
	}
}

func TestWechatUpdatesSnapshotClosesBody(t *testing.T) {
	a := newTestWechatAdapter(t)
	a.TokenForTest().BaseURL = "https://example.invalid"
	closed := false
	a.SetTransportForTest(imageRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &wechatPollTestBody{Reader: strings.NewReader(`{"msgs":[]}`), close: func() { closed = true }}}, nil
	}))
	if _, err := a.GetUpdatesForTest(); err != nil || !closed {
		t.Fatal(err, closed)
	}
}

type wechatPollTestBody struct {
	io.Reader
	close func()
}

func (b *wechatPollTestBody) Close() error { b.close(); return nil }
