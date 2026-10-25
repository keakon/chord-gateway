package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

func TestWechatFullWaitingQueueAcceptsStatusAndWakesFIFO(t *testing.T) {
	a, service, process, writer := newWechatIngressTest(t)
	var first im.ReceiveResult
	for i := range 64 {
		result := a.receiveIncomingMessage(t.Context(), wechatTextMessage(fmt.Sprint(i+1), "owner", fmt.Sprintf("waiting task %d", i), ""), a.token.Load())
		if result.Disposition != im.ReceiveAccepted {
			t.Fatalf("input %d: %+v", i, result)
		}
		if i == 0 {
			first = result
		}
	}
	if writes := wechatTaskWrites(writer); len(writes) != 0 {
		t.Fatal("waiting inputs executed without reply context", writes)
	}
	status := a.receiveIncomingMessage(t.Context(), wechatTextMessage("65", "owner", "/status", "fresh-context"), a.token.Load())
	if status.Disposition != im.ReceiveAccepted {
		t.Fatalf("full queue prevented context recovery: %+v", status)
	}
	waitIngressCondition(t, func() bool { return len(wechatTaskWrites(writer)) == 1 })
	if writes := wechatTaskWrites(writer); writes[0]["content"] != "waiting task 0" {
		t.Fatal("status changed task order", writes)
	}
	payload, _ := json.Marshal(InputResultPayload{RequestID: "im-" + first.RecordID, Status: inputQueued})
	process.mu.Lock()
	process.applyInputResult(&HeadlessEnvelope{Payload: payload, Seq: 1}, time.Now())
	process.mu.Unlock()
	waitIngressCondition(t, func() bool { return len(wechatTaskWrites(writer)) == 2 })
	if writes := wechatTaskWrites(writer); writes[1]["content"] != "waiting task 1" {
		t.Fatal("status created a task or overtook waiting input", writes)
	}
	if record, err := service.store.Get(status.RecordID); err != nil || record.Kind != "control" {
		t.Fatalf("status was not a control: %+v %v", record, err)
	}
}

func TestIngressStatusCountReserveIsBounded(t *testing.T) {
	for _, scope := range []string{"chat", "account", "global"} {
		t.Run(scope, func(t *testing.T) {
			s := testIngressStore(t)
			r := testIngressTask(t, s, "ordinary-overflow")
			if scope == "chat" {
				for i := range 64 {
					r.BusinessID = fmt.Sprint(i)
					mustAcceptIngress(t, s, r)
				}
			} else {
				if scope == "global" {
					s.total.Pending = 4096
				} else {
					s.counts[compositeKey(r.Platform, r.AccountID)] = ingressCounts{Pending: 1024}
				}

			}
			r.BusinessID = "ordinary-overflow"
			if got := s.AcceptRecord(t.Context(), r, nil); got.Disposition != im.ReceiveRetryable || got.Reason != "capacity" {
				t.Fatalf("ordinary input used reserve: %+v", got)
			}
			r.Kind, r.BusinessID = "control", "other-control"
			if got := s.AcceptRecord(t.Context(), r, nil); got.Disposition != im.ReceiveRetryable || got.Reason != "capacity" {
				t.Fatalf("unprivileged control used reserve: %+v", got)
			}
			r.statusQuery, r.BusinessID = true, "status-query"
			accepted := mustAcceptIngress(t, s, r)
			r.BusinessID = "second-status-query"
			if got := s.AcceptRecord(t.Context(), r, nil); got.Disposition != im.ReceiveRetryable || got.Reason != "capacity" {
				t.Fatalf("second status exceeded bounded reserve: %+v", got)
			}
			if err := s.Discard(t.Context(), accepted.RecordID); err != nil {
				t.Fatal(err)
			}
			mustAcceptIngress(t, s, r)
		})
	}
}

func TestIngressStatusReservePreservesStorageLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		global bool
		counts ingressCounts
	}{
		{"account-bytes", false, ingressCounts{Bytes: 16 << 20}},
		{"global-bytes", true, ingressCounts{Bytes: 64 << 20}},
		{"tombstones", false, ingressCounts{Tombstones: 100000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testIngressStore(t)
			r := testIngressTask(t, s, "status-query")
			r.Kind, r.statusQuery = "control", true
			if tc.global {
				s.total = tc.counts
			} else {
				s.counts[compositeKey(r.Platform, r.AccountID)] = tc.counts
			}
			result := s.AcceptRecord(context.Background(), r, nil)
			if result.Disposition != im.ReceiveRetryable || result.Reason != "capacity" {
				t.Fatalf("status bypassed storage limit: %+v", result)
			}
		})
	}
}
