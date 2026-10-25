package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
	"github.com/keakon/chord-gateway/internal/platform/wechat"
)

func TestWechatAccountCredentialRotationKeepsCursor(t *testing.T) {
	a, service, _, _ := newWechatIngressTest(t)
	t.Cleanup(a.CloseIngressAccountForTest)
	old := a.TokenForTest()
	cursor, err := a.PrepareIngressAccountForTest(t.Context(), old)
	if err != nil {
		t.Fatal(err)
	}
	next, err := wechat.NextWechatCursor(cursor, "bot", "committed")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.store.CommitCursor(t.Context(), "wechat", "bot", cursor.Generation, next); err != nil {
		t.Fatal(err)
	}
	lock := a.AccountLockForTest()
	request, cancel := a.PollingContextForTest(t.Context(), old)
	defer cancel()
	renewed := &wechat.TokenData{AccountID: "bot", Token: "renewed-token"}
	a.SwapPollingTokenForTest(renewed)
	if request.Err() == nil {
		t.Fatal("token rotation left old credential request active")
	}
	if _, err := a.PrepareIngressAccountForTest(t.Context(), old); err == nil {
		t.Fatal("stale credential snapshot prepared an account")
	}
	actual, err := a.PrepareIngressAccountForTest(t.Context(), renewed)
	if err != nil || !wechat.EqualWechatCursor(actual, next) || a.AccountLockForTest() != lock {
		t.Fatalf("same-account renewal lost committed progress or lock: %+v, %v", actual, err)
	}
}

func TestWechatAccountAccountLockAcrossDatabases(t *testing.T) {
	first, _, _, _ := newWechatIngressTest(t)
	second, _, _, _ := newWechatIngressTest(t)
	t.Cleanup(first.CloseIngressAccountForTest)
	t.Cleanup(second.CloseIngressAccountForTest)
	if _, err := first.PrepareIngressAccountForTest(t.Context(), first.TokenForTest()); err != nil {
		t.Fatal(err)
	}
	if _, err := second.PrepareIngressAccountForTest(t.Context(), second.TokenForTest()); !errors.Is(err, im.ErrInstanceRunning) {
		t.Fatalf("distinct databases bypassed account lock: %v", err)
	}
	first.CloseIngressAccountForTest()
	if _, err := second.PrepareIngressAccountForTest(t.Context(), second.TokenForTest()); err != nil {
		t.Fatalf("released account lock prevented second deployment: %v", err)
	}
}

func TestWechatAccountOversizedTextDoesNotBlockBatchProgress(t *testing.T) {
	a, service, _, writer := newWechatIngressTest(t)
	t.Cleanup(a.CloseIngressAccountForTest)
	token := a.TokenForTest()
	cursor, err := a.PrepareIngressAccountForTest(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	batch := wechat.WechatPollingBatch{Response: &wechat.ILinkGetUpdatesResponse{
		GetUpdatesBuf: "after-rejected-message",
		Msgs: []wechat.ILinkMessage{
			wechatTextMessage("200", "owner", strings.Repeat("x", (64<<10)+1), ""),
			wechatTextMessage("201", "owner", "next task", ""),
		},
	}}
	var results []im.ReceiveResult
	receive := func(ctx context.Context, msg wechat.ILinkMessage, _ im.ReceiveResult) im.ReceiveResult {
		result := a.ReceiveIncomingMessageForTest(ctx, msg, token)
		results = append(results, result)
		return result
	}
	resolve := func(ctx context.Context, msg wechat.ILinkMessage, result im.ReceiveResult) im.ReceiveResult {
		return a.ResolveIncomingMessageForTest(ctx, msg, token, result)
	}
	if !batch.Advance(t.Context(), receive, resolve) || batch.Position != 2 || len(results) != 2 {
		t.Fatalf("permanent size rejection blocked the batch: position=%d results=%+v", batch.Position, results)
	}
	if results[0].Disposition != im.ReceiveRejected || results[0].RecordID == "" || results[1].Disposition != im.ReceiveAccepted {
		t.Fatalf("unexpected batch results: %+v", results)
	}
	rejected, err := service.store.Get(results[0].RecordID)
	if err != nil || rejected.State != "rejected" || rejected.Text != "" {
		t.Fatalf("permanent rejection did not preserve a private tombstone: %+v, %v", rejected, err)
	}
	next, err := wechat.NextWechatCursor(cursor, token.AccountID, batch.Response.GetUpdatesBuf)
	if err != nil {
		t.Fatal(err)
	}
	epoch, ok := a.CurrentPollingEpochForTest(token)
	if !ok {
		t.Fatal("account epoch unavailable")
	}
	if err := a.CommitPollingCursorForTest(t.Context(), token, epoch, cursor, next); err != nil {
		t.Fatal(err)
	}
	actual, err := service.store.Cursor("wechat", token.AccountID)
	if err != nil || !wechat.EqualWechatCursor(actual, next) {
		t.Fatalf("determinate batch did not advance checkpoint: %+v, %v", actual, err)
	}
	if len(wechatTaskWrites(writer)) != 0 {
		t.Fatal("batch acceptance bypassed missing reply context")
	}
}
