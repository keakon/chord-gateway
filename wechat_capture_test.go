package main

import (
	"context"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
)

func TestWechatRetainedBatchKeepsOriginalControlTarget(t *testing.T) {
	for _, blockedPosition := range []int{0, 1} {
		t.Run([]string{"predecessor-retry", "control-retry"}[blockedPosition], func(t *testing.T) {
			a, service, process, writer := newWechatIngressTest(t)
			token := a.token.Load()
			process.mu.Lock()
			process.state.PendingConfirm = &ConfirmPayload{RequestID: "original"}
			process.mu.Unlock()
			response := &ilinkGetUpdatesResponse{Msgs: []ilinkMessage{
				wechatTextMessage("801", "owner", "earlier message", ""),
				wechatTextMessage("802", "owner", "/allow", "reply-context"),
			}}
			a.capturePollingMessages(response, token)
			batch := wechatPollingBatch{response: response}
			blocked := true
			var accepted im.ReceiveResult
			receive := func(ctx context.Context, msg ilinkMessage, _ im.ReceiveResult) im.ReceiveResult {
				if blocked && batch.position == blockedPosition {
					return im.ReceiveResult{Disposition: im.ReceiveRetryable}
				}
				if msg.MessageID == "801" {
					return im.ReceiveResult{Disposition: im.ReceiveTransportIgnored}
				}
				accepted = a.receiveIncomingMessage(ctx, msg, token)
				return accepted
			}
			if batch.advance(t.Context(), receive, receive) || batch.position != blockedPosition {
				t.Fatal("retry did not retain the expected position")
			}
			process.mu.Lock()
			process.state.PendingConfirm = &ConfirmPayload{RequestID: "replacement"}
			process.mu.Unlock()
			blocked = false
			if !batch.advance(t.Context(), receive, receive) || accepted.Disposition != im.ReceiveAccepted {
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
