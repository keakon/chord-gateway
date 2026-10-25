package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

func TestIngressQuestionReceiptRejectsAfterParentCancellation(t *testing.T) {
	s, p, _, _ := ingressTestService(t)
	// Cancelling Start's parent lifecycle can precede the owner's explicit Close.
	// Join dispatch/receipt consumers first, then verify no false acceptance into
	// an orphaned channel. The cleanup still calls Close normally.
	s.cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ingress consumers did not exit after cancellation")
	}
	if s.router.enqueueIngressQuestionReceipt(p, QuestionReceipt{OperationID: "operation", RequestID: "question", Version: 1, Accepted: true}) {
		t.Fatal("question receipt accepted after its receipt consumer exited")
	}
}

func TestIngressNormalizationCannotReplaceChatEvidence(t *testing.T) {
	s, _, _, msg := ingressTestService(t)
	pauseIngressTestRoute(t, s, msg)
	msg.MessageID, msg.Text = "chat-evidence", "task"
	msg.ChatKind = "private"
	future, err := s.router.SubmitIngressNormalization(context.Background(), "chat", im.IngressEnvelope{Message: msg}, func(_ context.Context, e im.IngressEnvelope) (im.IngressEnvelope, error) {
		e.Message.ChatKind = "group"
		return e, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := receiveNormalization(t, future); got.Disposition != im.ReceiveRetryable || got.Reason != "normalization_failed" {
		t.Fatalf("normalization changed trusted chat evidence: %v", got)
	}
}

func TestIngressQueryCannotAttachToRuntimeIntroducedAfterCapture(t *testing.T) {
	s, p, w, msg := ingressTestService(t)
	s.router.mgr.mu.Lock()
	delete(s.router.mgr.procs, p.key)
	s.router.mgr.mu.Unlock()
	msg.Text = "/status"
	_, target := s.router.CaptureIngress(msg)
	if target == nil || target.RuntimeID != "" || target.SessionID != "session" {
		t.Fatal(target)
	}
	s.router.mgr.mu.Lock()
	s.router.mgr.procs[p.key] = p
	s.router.mgr.mu.Unlock()
	proc, _, result := s.router.restoreIngressQuery(t.Context(), p.key, target)
	if proc != nil || result.State != "rejected" || result.Reason != "control_target_expired" || len(w.snapshot()) != 0 {
		t.Fatal(proc, result, w.snapshot())
	}
}

func TestIngressNormalizationRawKeyIsAccountScoped(t *testing.T) {
	s, _, _, msg := ingressTestService(t)
	pauseIngressTestRoute(t, s, msg)
	msg.MessageID, msg.Text = "blocked-account", "task"
	started, release := make(chan struct{}), make(chan struct{})
	future, err := s.router.SubmitIngressNormalization(context.Background(), "shared-raw-chat", im.IngressEnvelope{Message: msg}, func(ctx context.Context, e im.IngressEnvelope) (im.IngressEnvelope, error) {
		close(started)
		select {
		case <-release:
			return e, nil
		case <-ctx.Done():
			return im.IngressEnvelope{}, ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	other := msg
	other.AccountID, other.MessageID = "other-account", "independent-account"
	otherFuture, err := s.router.SubmitIngressNormalization(context.Background(), "shared-raw-chat", im.IngressEnvelope{Message: other}, func(_ context.Context, e im.IngressEnvelope) (im.IngressEnvelope, error) { return e, nil })
	if err != nil {
		t.Fatal(err)
	}
	// The other account is not the owner, so admission rejects it. It must
	// nevertheless complete without waiting for this account's slow lookup.
	if got := receiveNormalization(t, otherFuture); got.Disposition != im.ReceiveRejected {
		t.Fatalf("another account was serialized behind the blocked lookup: %v", got)
	}
	close(release)
	if got := receiveNormalization(t, future); got.Disposition != im.ReceiveAccepted {
		t.Fatal(got)
	}
}

func TestIngressQuestionReceiptQueueIsBoundedAndNonblocking(t *testing.T) {
	service := &IngressService{started: true, ctx: context.Background(), questionReceipts: make(chan ingressQuestionReceipt, 128)}
	router := &NotificationRouter{ingress: service}
	process := &ChordProcess{}
	for range cap(service.questionReceipts) {
		if !router.enqueueIngressQuestionReceipt(process, QuestionReceipt{}) {
			t.Fatal("receipt refused before queue capacity")
		}
	}
	if router.enqueueIngressQuestionReceipt(process, QuestionReceipt{}) {
		t.Fatal("receipt accepted beyond queue capacity")
	}
	if got := len(service.questionReceipts); got != 128 {
		t.Fatalf("receipt queue size: %d", got)
	}
}

func TestIngressQuestionReceiptUpdateFailureKeepsControlUnresolved(t *testing.T) {
	s, process, _, _ := ingressTestService(t)
	target := &im.IngressControlTarget{RuntimeID: process.runtimeID, SessionEpoch: process.sessionEpoch, SessionID: "session", RequestID: "question", Version: 1}
	record := testIngressTask(t, s.store, "receipt-storage-failure")
	record.Kind, record.Text, record.ControlTarget = "control", "", target
	accepted := mustAcceptIngress(t, s.store, record)
	if err := s.store.UpdateRecord(t.Context(), accepted.RecordID, []im.IngressState{"received"}, func(record *IngressRecord) error { record.State = "unknown"; return nil }); err != nil {
		t.Fatal(err)
	}
	s.router.mu.Lock()
	s.router.questionOperations = map[string]pendingQuestionOperation{
		"retained": {command: map[string]any{"operation_id": "operation", "version": uint64(1)}, target: target, recordIDs: []string{accepted.RecordID}, expiresAt: time.Now().Add(time.Hour)},
	}
	s.router.mu.Unlock()
	// Keep unresolved evidence while simulating ingress shutdown.
	s.store.closing.Store(true)
	defer s.store.closing.Store(false)
	if !s.router.enqueueIngressQuestionReceipt(process, QuestionReceipt{OperationID: "operation", RequestID: "question", Version: 1, Accepted: true}) {
		t.Fatal("receipt enqueue failed")
	}
	waitIngressCondition(t, func() bool {
		process.mu.RLock()
		blocked, diagnostic := process.inputReceiptBlocked, process.state.LastError
		process.mu.RUnlock()
		if !blocked || !strings.Contains(diagnostic, "ingress_unavailable") {
			return false
		}
		for _, counter := range s.store.MetricsSnapshot().Counters {
			if counter.Event == "receipt_update_failed" && counter.Reason == "ingress_unavailable" && counter.Count > 0 {
				return true
			}
		}
		return false
	})
	current, err := s.store.Get(accepted.RecordID)
	if err != nil || current.State != "unknown" {
		t.Fatalf("failed receipt application fabricated a terminal outcome: %v %v", current, err)
	}
	s.router.mu.Lock()
	_, retained := s.router.questionOperations["retained"]
	s.router.mu.Unlock()
	if !retained {
		t.Fatal("failed receipt application discarded the operation correlation")
	}
}

func TestIngressQuestionReceiptCallbackFailureBlocksWithDiagnostic(t *testing.T) {
	process := &ChordProcess{
		key: "workspace|wechat|chat",
		onQuestionReceipt: func(*ChordProcess, QuestionReceipt) bool {
			return false
		},
	}
	payload, err := json.Marshal(QuestionReceipt{OperationID: "operation", RequestID: "question", Accepted: true, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	process.processEnvelope(&HeadlessEnvelope{Type: "question_receipt", Payload: payload, Seq: 1})
	process.mu.RLock()
	defer process.mu.RUnlock()
	if !process.inputReceiptBlocked || !strings.Contains(process.state.LastError, "input_receipt_backpressure") {
		t.Fatalf("receipt refusal was not observable: blocked=%t error=%q", process.inputReceiptBlocked, process.state.LastError)
	}
}
