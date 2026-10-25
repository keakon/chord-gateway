package wechat

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
)

func TestWechatPollingRetainsRetryablePosition(t *testing.T) {
	batch := WechatPollingBatch{Response: &ILinkGetUpdatesResponse{Msgs: []ILinkMessage{{MessageID: "1"}, {MessageID: "2"}, {MessageID: "3"}}}}
	var called []string
	retry := true
	receive := func(_ context.Context, msg ILinkMessage, _ im.ReceiveResult) im.ReceiveResult {
		called = append(called, msg.MessageID.String())
		if msg.MessageID == "2" && retry {
			return im.ReceiveResult{Disposition: im.ReceiveRetryable}
		}
		return im.ReceiveResult{Disposition: im.ReceiveAccepted, RecordID: msg.MessageID.String()}
	}
	resolve := func(context.Context, ILinkMessage, im.ReceiveResult) im.ReceiveResult {
		t.Fatal("retryable was treated as uncertain")
		return im.ReceiveResult{}
	}
	if batch.Advance(t.Context(), receive, resolve) || batch.Position != 1 || !reflect.DeepEqual(called, []string{"1", "2"}) {
		t.Fatalf("position=%d calls=%v", batch.Position, called)
	}
	retry = false
	if !batch.Advance(t.Context(), receive, resolve) || !reflect.DeepEqual(called, []string{"1", "2", "2", "3"}) {
		t.Fatalf("position=%d calls=%v", batch.Position, called)
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
			batch := WechatPollingBatch{Response: &ILinkGetUpdatesResponse{Msgs: []ILinkMessage{{}}}}
			fn := func(context.Context, ILinkMessage, im.ReceiveResult) im.ReceiveResult { return tc.result }
			if complete := batch.Advance(t.Context(), fn, fn); complete != tc.complete {
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
		{"body_limit", strings.Repeat(" ", WechatMaxBatchBytes+1), true},
		{"message_limit", `{"msgs":[` + strings.Repeat(`{},`, WechatMaxBatchMessages) + `{}]}`, true},
		{"wrong_list", `{"msgs":{}}`, false},
		{"wrong_object", `null`, false},
		{"truncated", `{"msgs":[{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeWechatUpdates(strings.NewReader(tc.body))
			if err == nil || errors.Is(err, ErrWechatBatchTooLarge) != tc.tooLarge {
				t.Fatal(err)
			}
		})
	}
	valid := `{"get_updates_buf":"opaque","msgs":[` + strings.Repeat(`{},`, WechatMaxBatchMessages-1) + `{"message_id":18446744073709551615}]}`
	response, err := decodeWechatUpdates(strings.NewReader(valid))
	if err != nil || len(response.Msgs) != WechatMaxBatchMessages || response.Msgs[len(response.Msgs)-1].MessageID.String() != "18446744073709551615" {
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
		next, err := NextWechatCursor(previous, "account", buf)
		if err != nil || next.Generation != 5 || next.Wechat.BatchVersion != 9 {
			t.Fatal(next, err)
		}
		if buf == "" && next.Wechat.Buf != previous.Wechat.Buf {
			t.Fatal("empty next cleared checkpoint")
		}
	}
}
