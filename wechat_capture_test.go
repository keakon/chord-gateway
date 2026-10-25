package main

import (
	"context"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
	"github.com/keakon/chord-gateway/internal/platform/wechat"
)

func TestWechatRetainedBatchKeepsOriginalControlTarget(t *testing.T) {
	for _, blockedPosition := range []int{0, 1} {
		t.Run([]string{"predecessor-retry", "control-retry"}[blockedPosition], func(t *testing.T) {
			a, service, process, writer := newWechatIngressTest(t)
			token := a.TokenForTest()
			process.mu.Lock()
			process.state.PendingConfirm = &ConfirmPayload{RequestID: "original"}
			process.mu.Unlock()
			response := &wechat.ILinkGetUpdatesResponse{Msgs: []wechat.ILinkMessage{
				wechatTextMessage("801", "owner", "earlier message", ""),
				wechatTextMessage("802", "owner", "/allow", "reply-context"),
			}}
			a.CapturePollingMessagesForTest(response, token)
			batch := wechat.WechatPollingBatch{Response: response}
			blocked := true
			var accepted im.ReceiveResult
			receive := func(ctx context.Context, msg wechat.ILinkMessage, _ im.ReceiveResult) im.ReceiveResult {
				if blocked && batch.Position == blockedPosition {
					return im.ReceiveResult{Disposition: im.ReceiveRetryable}
				}
				if msg.MessageID == "801" {
					return im.ReceiveResult{Disposition: im.ReceiveTransportIgnored}
				}
				accepted = a.ReceiveIncomingMessageForTest(ctx, msg, token)
				return accepted
			}
			if batch.Advance(t.Context(), receive, receive) || batch.Position != blockedPosition {
				t.Fatal("retry did not retain the expected position")
			}
			process.mu.Lock()
			process.state.PendingConfirm = &ConfirmPayload{RequestID: "replacement"}
			process.mu.Unlock()
			blocked = false
			if !batch.Advance(t.Context(), receive, receive) || accepted.Disposition != im.ReceiveAccepted {
				t.Fatalf("retained control was not admitted: %+v", accepted)
			}
			waitIngressCondition(t, func() bool {
				record, err := service.store.Get(accepted.RecordID)
				return err == nil && record.Terminal()
			})
			record, err := service.store.Get(accepted.RecordID)
			if err != nil || record.State != "rejected" || record.ControlTarget == nil || record.ControlTarget.RequestID != "original" {
				t.Fatalf("stale control changed its target: %+v, %v", record, err)
			}
			for _, command := range writer.snapshot() {
				if command["type"] == "confirm" {
					t.Fatal("stale approval reached the replacement request")
				}
			}
		})
	}
}
