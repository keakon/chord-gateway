package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestWechatAccountCredentialRotationKeepsCursor(t *testing.T) {
	a, service, _, _ := newWechatIngressTest(t)
	t.Cleanup(a.closeIngressAccount)
	old := a.token.Load()
	cursor, err := a.prepareIngressAccount(t.Context(), old)
	if err != nil {
		t.Fatal(err)
	}
	next, err := nextWechatCursor(cursor, "bot", "committed")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.store.CommitCursor(t.Context(), "wechat", "bot", cursor.Generation, next); err != nil {
		t.Fatal(err)
	}
	lock := a.accountLock
	request, cancel := a.pollingContext(t.Context(), old)
	defer cancel()
	renewed := &TokenData{AccountID: "bot", Token: "renewed-token"}
	a.swapPollingToken(renewed)
	if request.Err() == nil {
		t.Fatal("token rotation left old credential request active")
	}
	if _, err := a.prepareIngressAccount(t.Context(), old); err == nil {
		t.Fatal("stale credential snapshot prepared an account")
	}
	actual, err := a.prepareIngressAccount(t.Context(), renewed)
	if err != nil || !equalWechatCursor(actual, next) || a.accountLock != lock {
		t.Fatalf("same-account renewal lost committed progress or lock: %+v, %v", actual, err)
	}
}

func TestWechatAccountAccountLockAcrossDatabases(t *testing.T) {
	first, _, _, _ := newWechatIngressTest(t)
	second, _, _, _ := newWechatIngressTest(t)
	t.Cleanup(first.closeIngressAccount)
	t.Cleanup(second.closeIngressAccount)
	if _, err := first.prepareIngressAccount(t.Context(), first.token.Load()); err != nil {
		t.Fatal(err)
	}
	if _, err := second.prepareIngressAccount(t.Context(), second.token.Load()); !errors.Is(err, errInstanceRunning) {
		t.Fatalf("distinct databases bypassed account lock: %v", err)
	}
	first.closeIngressAccount()
	if _, err := second.prepareIngressAccount(t.Context(), second.token.Load()); err != nil {
		t.Fatalf("released account lock prevented second deployment: %v", err)
	}
}

func TestWechatAccountOversizedTextDoesNotBlockBatchProgress(t *testing.T) {
	a, service, _, writer := newWechatIngressTest(t)
	t.Cleanup(a.closeIngressAccount)
	token := a.token.Load()
	cursor, err := a.prepareIngressAccount(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	batch := wechatPollingBatch{response: &ilinkGetUpdatesResponse{
		GetUpdatesBuf: "after-rejected-message",
		Msgs: []ilinkMessage{
			wechatTextMessage("200", "owner", strings.Repeat("x", (64<<10)+1), ""),
			wechatTextMessage("201", "owner", "next task", ""),
		},
	}}
	var results []ReceiveResult
	receive := func(ctx context.Context, msg ilinkMessage, _ ReceiveResult) ReceiveResult {
		result := a.receiveIncomingMessage(ctx, msg, token)
		results = append(results, result)
		return result
	}
	resolve := func(ctx context.Context, msg ilinkMessage, result ReceiveResult) ReceiveResult {
		return a.resolveIncomingMessage(ctx, msg, token, result)
	}
	if !batch.advance(t.Context(), receive, resolve) || batch.position != 2 || len(results) != 2 {
		t.Fatalf("permanent size rejection blocked the batch: position=%d results=%+v", batch.position, results)
	}
	if results[0].Disposition != ReceiveRejected || results[0].RecordID == "" || results[1].Disposition != ReceiveAccepted {
		t.Fatalf("unexpected batch results: %+v", results)
	}
	rejected, err := service.store.Get(results[0].RecordID)
	if err != nil || rejected.State != "rejected" || rejected.Text != "" {
		t.Fatalf("permanent rejection did not preserve a private tombstone: %+v, %v", rejected, err)
	}
	next, err := nextWechatCursor(cursor, token.AccountID, batch.response.GetUpdatesBuf)
	if err != nil {
		t.Fatal(err)
	}
	epoch, ok := a.currentPollingEpoch(token)
	if !ok {
		t.Fatal("account epoch unavailable")
	}
	if err := a.commitPollingCursor(t.Context(), token, epoch, cursor, next); err != nil {
		t.Fatal(err)
	}
	actual, err := service.store.Cursor("wechat", token.AccountID)
	if err != nil || !equalWechatCursor(actual, next) {
		t.Fatalf("determinate batch did not advance checkpoint: %+v, %v", actual, err)
	}
	if len(wechatTaskWrites(writer)) != 0 {
		t.Fatal("batch acceptance bypassed missing reply context")
	}
}
