package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

type ingressTestWriter struct {
	mu       sync.Mutex
	messages []map[string]any
}

func (w *ingressTestWriter) Write(data []byte) (int, error) {
	var command map[string]any
	if err := json.Unmarshal(data, &command); err != nil {
		return 0, err
	}
	w.mu.Lock()
	w.messages = append(w.messages, command)
	w.mu.Unlock()
	return len(data), nil
}
func (w *ingressTestWriter) Close() error { return nil }
func (w *ingressTestWriter) snapshot() []map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]map[string]any(nil), w.messages...)
}

func ingressTestService(t *testing.T) (*IngressService, *ChordProcess, *ingressTestWriter, im.IncomingMessage) {
	t.Helper()
	r, code := pairedTestRouter(t)
	msg := im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "owner", MessageID: "bind", Text: fmt.Sprintf("/bind %q %s", t.TempDir(), code)}
	if !dispatchPairingTest(r, msg) {
		t.Fatal("bind rejected")
	}
	ws, err := r.getConfig().ResolveWorkspace(msg.IMType, msg.ChatID)
	if err != nil {
		t.Fatal(err)
	}
	key := (processKey{workspaceID: ws.ID, imType: msg.IMType, chatID: msg.ChatID}).String()
	writer := &ingressTestWriter{}
	ready := make(chan struct{})
	close(ready)
	p := &ChordProcess{key: key, runtimeID: "runtime", sessionEpoch: 1, stdin: writer, readySignal: ready, state: ControlState{SessionID: "session"}}
	r.mgr.mu.Lock()
	r.mgr.procs[key] = p
	r.mgr.mu.Unlock()
	if err = r.mgr.pins.Set(pinKeyForBinding(key), "session"); err != nil {
		t.Fatal(err)
	}
	store := NewIngressStore()
	service := NewIngressService(store, r)
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		service.Close()
		p.mu.Lock()
		for _, observer := range p.inputObservers {
			if observer.timer != nil {
				observer.timer.Stop()
			}
		}
		p.mu.Unlock()
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return service, p, writer, msg
}

// ageIngressRecord backdates a record past the stall deadline without replacing
// the store clock, which the dispatcher goroutine reads concurrently.
func ageIngressRecord(t *testing.T, store *IngressStore, id string) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	rec, ok := store.records[id]
	if !ok {
		t.Fatal("record missing from the store")
	}
	rec.UpdatedAt = store.now().Add(-ingressStallDeadline - time.Minute)
	store.records[id] = rec
}
func waitIngressCondition(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("ingress condition timed out")
}
func TestIngressFIFOOnlyAdvancesAfterReceipt(t *testing.T) {
	s, p, w, msg := ingressTestService(t)
	msg.MessageID = "first"
	msg.Text = "first task"
	first := s.Accept(context.Background(), im.IngressEnvelope{Message: msg})
	if first.Disposition != im.ReceiveAccepted {
		t.Fatal(first)
	}
	msg.MessageID = "second"
	msg.Text = "second task"
	second := s.Accept(context.Background(), im.IngressEnvelope{Message: msg})
	if second.Disposition != im.ReceiveAccepted {
		t.Fatal(second)
	}
	waitIngressCondition(t, func() bool { return len(w.snapshot()) == 1 })
	records, err := s.store.ListRouteRecords(im.ChatIdentity{Platform: msg.IMType, AccountID: msg.AccountID, ChatID: msg.ChatID})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].State != "dispatching" || records[1].State != "received" {
		t.Fatal(records)
	}
	payload, _ := json.Marshal(InputResultPayload{RequestID: "im-" + first.RecordID, Status: inputQueued})
	p.mu.Lock()
	p.applyInputResult(&HeadlessEnvelope{Payload: payload, Seq: 1}, time.Now())
	p.mu.Unlock()
	waitIngressCondition(t, func() bool { return len(w.snapshot()) == 2 })
	if got := w.snapshot(); got[0]["content"] != "first task" || got[1]["content"] != "second task" {
		t.Fatal(got)
	}
}
func TestIngressFrozenConfirmCannotApproveReplacement(t *testing.T) {
	s, p, w, msg := ingressTestService(t)
	msg.Text = "/allow"
	msg.MessageID = "allow"
	p.mu.Lock()
	p.state.PendingConfirm = &ConfirmPayload{RequestID: "old"}
	p.mu.Unlock()
	kind, target := s.router.CaptureIngress(msg)
	if kind != "control" || target.RequestID != "old" {
		t.Fatal(kind, target)
	}
	ticket, ok := s.router.admission.Authorize(msg)
	if !ok {
		t.Fatal("authorization")
	}
	msg.SetAdmissionTicket(&ticket)
	p.mu.Lock()
	p.state.PendingConfirm = &ConfirmPayload{RequestID: "new"}
	p.mu.Unlock()
	result := s.router.DispatchAccepted(context.Background(), msg, target)
	if result.State != "rejected" || len(w.snapshot()) != 0 {
		t.Fatal(result, w.snapshot())
	}
}
func TestIngressDoneClassificationAndCancelTurn(t *testing.T) {
	s, p, w, msg := ingressTestService(t)
	p.mu.Lock()
	p.state.PendingConfirm = &ConfirmPayload{RequestID: "done", ToolName: "Done"}
	p.mu.Unlock()
	msg.Text = "needs tests"
	kind, target := s.router.CaptureIngress(msg)
	if kind != "control" || target.RequestID != "done" {
		t.Fatal(kind, target)
	}
	msg.Text = "/summary"
	kind, _ = s.router.CaptureIngress(msg)
	if kind != "control" {
		t.Fatal("slash became recoverable")
	}
	msg.Text = "/cancel"
	p.mu.Lock()
	p.state.Busy = true
	p.state.TurnStartedAt = time.Now()
	p.mu.Unlock()
	_, target = s.router.CaptureIngress(msg)
	ticket, _ := s.router.admission.Authorize(msg)
	msg.SetAdmissionTicket(&ticket)
	p.mu.Lock()
	p.state.TurnStartedAt = p.state.TurnStartedAt.Add(time.Second)
	p.mu.Unlock()
	result := s.router.DispatchAccepted(context.Background(), msg, target)
	if result.State != "rejected" || len(w.snapshot()) != 0 {
		t.Fatal(result)
	}
}

func TestIngressQueuedTaskQuarantinedWhenDoneAppears(t *testing.T) {
	s, p, w, msg := ingressTestService(t)
	msg.MessageID = "first"
	msg.Text = "first"
	first := s.Accept(context.Background(), im.IngressEnvelope{Message: msg})
	if first.Disposition != im.ReceiveAccepted {
		t.Fatal(first)
	}
	waitIngressCondition(t, func() bool { return len(w.snapshot()) == 1 })
	msg.MessageID = "second"
	msg.Text = "ordinary task"
	second := s.Accept(context.Background(), im.IngressEnvelope{Message: msg})
	if second.Disposition != im.ReceiveAccepted {
		t.Fatal(second)
	}
	p.mu.Lock()
	p.state.PendingConfirm = &ConfirmPayload{RequestID: "done", ToolName: "Done"}
	payload, _ := json.Marshal(InputResultPayload{RequestID: "im-" + first.RecordID, Status: inputStarted})
	p.applyInputResult(&HeadlessEnvelope{Payload: payload, Seq: 1}, time.Now())
	p.mu.Unlock()
	waitIngressCondition(t, func() bool { rec, err := s.store.Get(second.RecordID); return err == nil && rec.State == "quarantined" })
	if len(w.snapshot()) != 1 {
		t.Fatal("queued task was written as Done feedback")
	}
}
func TestIngressBindingBarrierAndIdempotence(t *testing.T) {
	s, _, _, msg := ingressTestService(t)
	if err := s.PrepareAccount(context.Background(), msg.IMType, msg.AccountID); err != nil {
		t.Fatal(err)
	}
	ws, _ := s.router.getConfig().ResolveWorkspace(msg.IMType, msg.ChatID)
	id := im.ChatIdentity{Platform: msg.IMType, AccountID: msg.AccountID, ChatID: msg.ChatID}
	digest, workspace, session, err := s.router.ingressRouteIdentity(id)
	if err != nil {
		t.Fatal(err)
	}
	route, err := s.store.EnsureRoute(t.Context(), id, digest, workspace, session)
	if err != nil {
		t.Fatal(err)
	}
	// Direct storage insertion leaves a received input behind the same gate.
	s.mu.Lock()
	s.active[id] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.active, id); s.mu.Unlock() }()
	accepted := s.store.AcceptRecord(context.Background(), IngressRecord{Platform: msg.IMType, AccountID: msg.AccountID, ChatID: msg.ChatID, SenderID: msg.SenderID, BusinessID: "pending", Kind: "task", Text: "pending", RouteRevision: route.Revision, AuthorizationDigest: route.AuthorizationDigest, WorkspaceID: ws.ID, SessionID: "session"}, nil)
	if accepted.Disposition != im.ReceiveAccepted {
		t.Fatal(accepted)
	}
	msg.Text = fmt.Sprintf("/bind %q", ws.Path)
	ticket, ok := s.router.admission.Authorize(msg)
	if !ok {
		t.Fatal("authorize")
	}
	msg.SetAdmissionTicket(&ticket)
	result := s.router.DispatchAccepted(context.Background(), msg, nil)
	if result.State != "completed" {
		t.Fatal("idempotent bind blocked", result)
	}
	after, _ := s.store.GetRoute(id)
	if after.Revision != route.Revision {
		t.Fatal("idempotent bind changed revision")
	}
	msg.Text = fmt.Sprintf("/bind %q", t.TempDir())
	ticket, ok = s.router.admission.Authorize(msg)
	if !ok {
		t.Fatal("authorize")
	}
	msg.SetAdmissionTicket(&ticket)
	result = s.router.DispatchAccepted(context.Background(), msg, nil)
	if result.State != "rejected" {
		t.Fatal("changed directory with undelivered input", result)
	}
	current, _ := s.router.getConfig().ResolveWorkspace(msg.IMType, msg.ChatID)
	if current.ID != ws.ID {
		t.Fatal("binding changed")
	}
}

func TestIngressSessionControlRejectsNewRuntime(t *testing.T) {
	s, p, w, msg := ingressTestService(t)
	for _, text := range []string{"/new", "/resume target-session"} {
		msg.Text = text
		_, target := s.router.CaptureIngress(msg)
		ticket, ok := s.router.admission.Authorize(msg)
		if !ok {
			t.Fatal("authorize")
		}
		msg.SetAdmissionTicket(&ticket)
		p.mu.Lock()
		p.sessionEpoch++
		p.mu.Unlock()
		result := s.router.DispatchAccepted(context.Background(), msg, target)
		if result.State != "rejected" || result.Reason != "control_target_expired" || len(w.snapshot()) != 0 {
			t.Fatal(result)
		}
	}
}

// A freshly started or upgraded gateway spawns chord before it emits ready, so the
// captured control target carries no runtime identity. /new and /resume replace
// the session, so the spawn window must not be mistaken for a stale control and
// rejected; otherwise a first /new right after startup always fails.
func TestIngressSessionControlAllowedDuringSpawnWindow(t *testing.T) {
	s, p, _, msg := ingressTestService(t)
	// A missing binary makes the post-guard session replacement fail fast here,
	// so the test observes the guard's decision rather than real process startup.
	s.router.getConfig().ChordPath = filepath.Join(t.TempDir(), "missing-chord")
	key := p.key
	for _, text := range []string{"/new", "/resume target-session"} {
		// Re-register a fresh process in the spawn window: it exists, but chord
		// has not emitted ready, so its runtime identity is still unknown.
		ready := make(chan struct{})
		close(ready)
		proc := &ChordProcess{key: key, sessionEpoch: 1, stdin: &ingressTestWriter{}, readySignal: ready, state: ControlState{SessionID: "session"}}
		s.router.mgr.mu.Lock()
		s.router.mgr.procs[key] = proc
		s.router.mgr.mu.Unlock()

		msg.Text = text
		_, target := s.router.CaptureIngress(msg)
		if target == nil || target.RuntimeID != "" {
			t.Fatalf("spawn window target should carry no runtime id: %+v", target)
		}
		ticket, ok := s.router.admission.Authorize(msg)
		if !ok {
			t.Fatal("authorize")
		}
		msg.SetAdmissionTicket(&ticket)
		result := s.router.DispatchAccepted(context.Background(), msg, target)
		if result.Reason == "control_target_expired" || result.Reason == "session_id_required" {
			t.Fatalf("%q rejected during spawn window: %+v", text, result)
		}
	}
}
func TestIngressAdmissionBudgetIncludesBusyGate(t *testing.T) {
	s, _, _, msg := ingressTestService(t)
	msg.Text = "task"
	msg.MessageID = "busy-gate"
	if err := s.PrepareAccount(context.Background(), msg.IMType, msg.AccountID); err != nil {
		t.Fatal(err)
	}
	ticket, ok := s.router.admission.Authorize(msg)
	if !ok {
		t.Fatal("authorize")
	}
	ticket.Gate.OpMu.Lock()
	defer ticket.Gate.OpMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	result := s.Accept(ctx, im.IngressEnvelope{Message: msg})
	if result.Disposition != im.ReceiveRetryable || result.Reason != "route_busy" {
		t.Fatal(result)
	}
	records, err := s.store.listAll()
	if err != nil || len(records) != 0 {
		t.Fatal(records, err)
	}
}

func TestIngressTelegramRejectionCommitsCursor(t *testing.T) {
	r, _ := pairedTestRouterWithConfig(t, "ims: {wechat: {}, telegram: {bot_token: placeholder, owner: {account_id: telegram-bot, user_id: owner}}}\n")
	if err := r.admission.pairing.PlatformReady("telegram", "telegram-bot"); err != nil {
		t.Fatal(err)
	}
	store := NewIngressStore()
	s := NewIngressService(store, r)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		s.Close()
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	msg := im.IncomingMessage{IMType: "telegram", AccountID: "telegram-bot", ChatID: "42", ChatKind: "private", SenderID: "owner", MessageID: "100", Text: "not bound"}
	result := s.Accept(context.Background(), im.IngressEnvelope{Message: msg, Cursor: testTelegramIngressCursor(1, 101)})
	if result.Disposition != im.ReceiveRejected || result.RecordID == "" {
		t.Fatal(result)
	}
	cursor, err := store.Cursor("telegram", "telegram-bot")
	if err != nil || cursor.Telegram == nil || *cursor.Telegram.PendingOffset != 101 {
		t.Fatal(cursor, err)
	}
	record, err := store.Get(result.RecordID)
	if err != nil || record.State != "rejected" || record.Text != "" {
		t.Fatal(record, err)
	}
}

func TestIngressReadyWaitReleasesGateAndRechecksRoute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, process, writer, msg := ingressTestService(t)
		ready := make(chan struct{})
		process.readySignal = ready
		msg.MessageID, msg.Text = "waiting-for-pin", "Run task"
		result := s.Accept(t.Context(), im.IngressEnvelope{Message: msg})
		if result.Disposition != im.ReceiveAccepted {
			t.Fatal(result)
		}
		synctest.Wait()
		ticket, ok := s.router.admission.Authorize(msg)
		if !ok {
			t.Fatal("authorization unavailable")
		}
		if !ticket.Gate.OpMu.TryLock() {
			t.Fatal("readiness wait held the admission gate")
		}
		_, err := s.store.EnsureRoute(t.Context(), ticket.Identity, "changed", "workspace", "new-session")
		ticket.Gate.OpMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		close(ready)
		synctest.Wait()
		if len(writer.snapshot()) != 0 {
			t.Fatal("sent using a stale route after readiness wait")
		}
		record, err := s.store.Get(result.RecordID)
		if err != nil || record.State != "quarantined" {
			t.Fatal(record, err)
		}
	})
}

func TestIngressSweepControlsDropsStaleEntries(t *testing.T) {
	store := testIngressStore(t)
	pending := mustAcceptIngress(t, store, testIngressTask(t, store, "pending"))
	service := &IngressService{store: store, controls: map[string]ingressControl{}}
	service.controls[pending.RecordID] = ingressControl{}
	service.controls["missing"] = ingressControl{}
	service.sweepControls()
	if _, ok := service.controls[pending.RecordID]; !ok {
		t.Fatal("dropped parameters for a pending record")
	}
	if _, ok := service.controls["missing"]; ok {
		t.Fatal("retained parameters for a pruned record")
	}
	if err := store.UpdateRecord(t.Context(), pending.RecordID, []im.IngressState{"received"}, func(r *IngressRecord) error { r.State = "consumed"; return nil }); err != nil {
		t.Fatal(err)
	}
	service.sweepControls()
	if _, ok := service.controls[pending.RecordID]; ok {
		t.Fatal("retained parameters for a settled record")
	}
}

// A delivery that outlives the stall deadline is settled while its receipt is
// still in flight, so the receipt arrives for a record that no longer accepts
// transitions. Refusing it would latch receipt backpressure for the rest of the
// process lifetime and leave the chat unable to send anything until restart.
// Tasks and control commands register separate receipt callbacks, so both paths
// must drop the stale receipt instead of returning its storage error.
func assertLateReceiptAfterSettlementKeepsInputUsable(t *testing.T, text string) {
	t.Helper()
	s, p, w, msg := ingressTestService(t)
	msg.MessageID, msg.Text = "stalled", text
	accepted := s.Accept(context.Background(), im.IngressEnvelope{Message: msg})
	if accepted.Disposition != im.ReceiveAccepted {
		t.Fatal(accepted)
	}
	waitIngressCondition(t, func() bool { return len(w.snapshot()) == 1 })
	if got := w.snapshot()[0]; got["content"] != text {
		t.Fatalf("delivery recorded %v, want %q", got, text)
	}
	requestID := "im-" + accepted.RecordID
	p.mu.RLock()
	_, observed := p.inputObservers[requestID]
	p.mu.RUnlock()
	if !observed {
		t.Fatal("delivery was not dispatched, so no receipt can arrive")
	}

	identity := im.ChatIdentity{Platform: msg.IMType, AccountID: msg.AccountID, ChatID: msg.ChatID}
	ageIngressRecord(t, s.store, accepted.RecordID)
	s.settleStalledRecords(s.store.ListRoutePendingMetadata(identity))
	stalled, err := s.store.Get(accepted.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	if stalled.State != "expired" || !stalled.Terminal() {
		t.Fatalf("delivery was not settled before its receipt arrived: %+v", stalled)
	}

	payload, err := json.Marshal(InputResultPayload{RequestID: requestID, Status: inputQueued})
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.applyInputResult(&HeadlessEnvelope{Payload: payload, Seq: 1}, time.Now())
	p.mu.Unlock()

	waitIngressCondition(t, func() bool {
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.inputReceiptBlocked || len(p.inputReceiptQueue) == 0
	})
	p.mu.RLock()
	blocked, lastError := p.inputReceiptBlocked, p.state.LastError
	p.mu.RUnlock()
	if blocked {
		t.Fatalf("a late receipt latched receipt backpressure: %q", lastError)
	}
	if err := p.SendUserMessageWithRequestID("next", "next-input"); err != nil {
		t.Fatalf("input was refused after a late receipt: %v", err)
	}
	got, err := s.store.Get(accepted.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "expired" || got.ReceiptStatus != "" {
		t.Fatalf("a late receipt rewrote a settled delivery: %+v", got)
	}
}

func TestIngressLateReceiptAfterSettlementKeepsInputUsable(t *testing.T) {
	assertLateReceiptAfterSettlementKeepsInputUsable(t, "task body")
}

func TestIngressControlLateReceiptAfterSettlementKeepsInputUsable(t *testing.T) {
	assertLateReceiptAfterSettlementKeepsInputUsable(t, "/summary")
}
