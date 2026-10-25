package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"

	wecom "github.com/keakon/chord-gateway/internal/platform/wecom"
)

func TestWecomRecoveryCaptureCannotApproveReplacementRuntime(t *testing.T) {
	a, msg, store, r := wecomAdapterFixture(t)
	key := (processKey{workspaceID: "project", imType: "wecom", chatID: msg.ChatID}).String()
	p := r.mgr.GetProcessForKey(key)
	p.mu.Lock()
	p.state.PendingConfirm = &ConfirmPayload{RequestID: "old-confirm"}
	p.mu.Unlock()
	raw := []byte(strings.Replace(string(wecomAdapterCallback("bot", "delayed-delivery", "freeze-control")), `"content":"task"`, `"content":"/allow"`, 1))
	firstReceived := time.Now().Add(-2 * time.Second)
	readerCtx := context.WithValue(a.ContextForTest(), wecom.ReceivedAtContextKey{}, firstReceived)
	frozenCtx := a.CaptureFrameForTest(readerCtx, raw)
	e, ok := frozenCtx.Value(wecom.CapturedEnvelopeContextKey{}).(im.IngressEnvelope)
	if !ok || e.Capture == nil || e.Capture.Target == nil || e.Capture.Target.RuntimeID != "runtime" || e.Capture.Target.RequestID != "old-confirm" {
		t.Fatalf("reader did not capture original control: %+v", e.Capture)
	}
	// Simulate the restore window changing the active runtime and confirmation
	// after socket receipt but before the staged callback reaches SubmitIngress.
	p.mu.Lock()
	p.runtimeID = "replacement-runtime"
	p.sessionEpoch++
	p.state.PendingConfirm = &ConfirmPayload{RequestID: "new-confirm"}
	writer := p.stdin.(*ingressTestWriter)
	p.mu.Unlock()
	if err := a.ReceiveFrameForTest(frozenCtx, raw); err != nil {
		t.Fatal(err)
	}
	var rec IngressRecord
	waitIngressCondition(t, func() bool {
		records, err := store.listAll()
		if err != nil || len(records) != 1 {
			return false
		}
		rec = records[0]
		return rec.Terminal()
	})
	if rec.State != "rejected" || rec.Reason != "control_target_expired" || rec.ControlTarget == nil || rec.ControlTarget.RuntimeID != "runtime" || rec.ControlTarget.RequestID != "old-confirm" {
		t.Fatalf("control reacquired replacement authority: %+v", rec)
	}
	if !rec.ReceivedAt.Equal(firstReceived) || !rec.SourceTime.Equal(firstReceived) {
		t.Fatalf("staging refreshed source/receive time: %+v", rec)
	}
	if len(writer.snapshot()) != 0 {
		t.Fatal("replacement runtime received original /allow")
	}
}
