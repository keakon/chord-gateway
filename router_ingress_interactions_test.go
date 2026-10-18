package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type ingressInteractionWriter struct {
	capture  ingressTestWriter
	proc     *ChordProcess
	snapshot StatusResponse
	receipts bool
	mu       sync.Mutex
}

func (w *ingressInteractionWriter) Close() error { return nil }
func (w *ingressInteractionWriter) Write(data []byte) (int, error) {
	n, err := w.capture.Write(data)
	if err != nil {
		return n, err
	}
	var command map[string]any
	if err = json.Unmarshal(data, &command); err != nil {
		return 0, err
	}
	w.mu.Lock()
	respond := w.receipts
	w.mu.Unlock()
	var payload any
	var typ string
	switch command["type"] {
	case "status":
		typ = "status_response"
		payload = w.snapshot
	case "question":
		if !respond {
			return n, nil
		}
		typ = "question_receipt"
		payload = QuestionReceipt{OperationID: command["operation_id"].(string), RequestID: command["request_id"].(string), Version: uint64(command["version"].(float64)), Accepted: true}
	}
	if typ != "" {
		raw, _ := json.Marshal(payload)
		go w.proc.processEnvelope(&HeadlessEnvelope{Type: typ, Payload: raw})
	}
	return n, nil
}

func TestIngressInteractionValidationAndCommandIdentity(t *testing.T) {
	valid := IncomingMessage{IMType: "feishu", Text: "/new", InternalAction: &InternalAction{Type: "question", Action: "answer", BindingInstanceID: "binding", RequestID: "question", Values: []string{"1", "3"}}}
	if err := ValidateIngressInteraction(valid); err != nil {
		t.Fatal(err)
	}
	if got := commandForIncomingMessage(valid); got.Type != "question" || got.RequestID != "question" || !reflect.DeepEqual(got.Answers, []string{"1", "3"}) {
		t.Fatal(got)
	}
	for _, change := range []func(*InternalAction){func(a *InternalAction) { a.BindingInstanceID = "" }, func(a *InternalAction) { a.Type = "send" }, func(a *InternalAction) { a.Action = "allow" }, func(a *InternalAction) { a.Value = "option" }, func(a *InternalAction) { a.Values = []string{strings.Repeat("a", 64*1024+1)} }} {
		copyMessage := valid
		copyAction := *valid.InternalAction
		copyMessage.InternalAction = &copyAction
		change(&copyAction)
		if err := ValidateIngressInteraction(copyMessage); err == nil {
			t.Fatalf("invalid action accepted: %+v", copyAction)
		}
	}
}

func TestIngressQuestionHistoricalTargetSynchronizesWithinFrozenSession(t *testing.T) {
	s, p, _, msg := ingressTestService(t)
	msg.MessageID, msg.Text = "answer", "/answer @historical Revised"
	ticket, ok := s.router.admission.Authorize(msg)
	if !ok {
		t.Fatal("authorization")
	}
	msg.ticket = &ticket
	writer := &ingressInteractionWriter{proc: p, receipts: true, snapshot: StatusResponse{SessionID: "session", Questions: []*QuestionPayload{{RequestID: "historical", Version: 3, Outcome: QuestionResolvedAnswered}}}}
	p.mu.Lock()
	p.stdin = writer
	p.mu.Unlock()
	_, target := s.router.CaptureIngress(msg)
	if target == nil || target.RequestID != "historical" || !target.QuestionLookup {
		t.Fatal(target)
	}
	result := s.router.DispatchAccepted(t.Context(), msg, target)
	if result.State != "completed" {
		t.Fatal(result)
	}
	commands := writer.capture.snapshot()
	if len(commands) != 2 || commands[0]["type"] != "status" || commands[1]["action"] != "revise" || commands[1]["request_id"] != "historical" || commands[1]["version"] != float64(3) {
		t.Fatal(commands)
	}
	// A later session may never satisfy the historical query.
	p.mu.Lock()
	p.sessionEpoch++
	p.mu.Unlock()
	result = s.router.DispatchAccepted(t.Context(), msg, target)
	if result.State != "rejected" || len(writer.capture.snapshot()) != 2 {
		t.Fatal(result)
	}
}

func TestIngressQuestionTrustedFormAndStaleVersion(t *testing.T) {
	p := &ChordProcess{runtimeID: "runtime", sessionEpoch: 1, bindingInstanceID: "binding", state: ControlState{SessionID: "session", Questions: map[string]*QuestionPayload{"q": {RequestID: "q", Version: 2, Multiple: true, Options: []string{"First", "Second", "Third"}, OptionIDs: []string{"first", "second", "third"}}}}}
	writer := &ingressInteractionWriter{proc: p, receipts: true}
	p.stdin = writer
	r := &NotificationRouter{}
	msg := IncomingMessage{IMType: "feishu", ChatID: "chat", SenderID: "owner", MessageID: "submit", InternalAction: &InternalAction{Type: "question", Action: "answer", BindingInstanceID: "binding", RequestID: "q", Values: []string{"3", "1"}}}
	target := &IngressControlTarget{RuntimeID: "runtime", SessionEpoch: 1, SessionID: "session", BindingInstanceID: "binding", RequestID: "q", Version: 2}
	result := r.dispatchIngressQuestion(t.Context(), msg, p, commandForIncomingMessage(msg), target)
	if result.State != "completed" {
		t.Fatal(result)
	}
	got := writer.capture.snapshot()[0]
	if !reflect.DeepEqual(got["answers"], []any{"third", "first"}) || got["custom"] != false {
		t.Fatal(got)
	}
	stale := *target
	stale.Version = 1
	result = r.dispatchIngressQuestion(t.Context(), msg, p, commandForIncomingMessage(msg), &stale)
	if result.State != "rejected" || len(writer.capture.snapshot()) != 1 {
		t.Fatal(result)
	}
}

func TestIngressQuestionExplicitRetryAndLateReceiptResolveOnlyRelatedRecords(t *testing.T) {
	store := testIngressStore(t)
	r := &NotificationRouter{ingressStore: store}
	p := &ChordProcess{key: "workspace|telegram|chat", runtimeID: "runtime", sessionEpoch: 1, bindingInstanceID: "binding", state: ControlState{SessionID: "session", Questions: map[string]*QuestionPayload{"q": {RequestID: "q", Version: 1}}}}
	writer := &ingressInteractionWriter{proc: p}
	p.stdin = writer
	target := &IngressControlTarget{RuntimeID: "runtime", SessionEpoch: 1, SessionID: "session", BindingInstanceID: "binding", RequestID: "q", Version: 1}
	rec := testIngressTask(t, store, "first")
	rec.Kind = "control"
	rec.Text = ""
	rec.ControlTarget = target
	first := mustAcceptIngress(t, store, rec)
	command := map[string]any{"type": "question", "request_id": "q", "version": uint64(1), "operation_id": "original", "action": "answer", "answers": []string{"Answer"}, "custom": true}
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if _, err := r.waitQuestionOperationTarget(ctx, p.key, "owner", p, command, target, []string{first.RecordID}); err == nil {
		t.Fatal("expected unknown result")
	}
	if err := store.UpdateRecord(t.Context(), first.RecordID, []ingressState{"received"}, func(rec *IngressRecord) error { rec.State = "unknown"; return nil }); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.state.Questions["q"] = &QuestionPayload{RequestID: "q", Version: 2, Outcome: QuestionResolvedAnswered}
	p.mu.Unlock()
	secondTarget := *target
	secondTarget.Version = 2
	secondRec := testIngressTask(t, store, "retry")
	secondRec.Kind = "control"
	secondRec.Text = ""
	secondRec.ControlTarget = &secondTarget
	second := mustAcceptIngress(t, store, secondRec)
	retry := map[string]any{"type": "question", "request_id": "q", "version": uint64(2), "operation_id": "new-click", "action": "revise", "answers": []string{"Answer"}, "custom": true}
	writer.mu.Lock()
	writer.receipts = true
	writer.mu.Unlock()
	if _, err := r.waitQuestionOperationTarget(t.Context(), p.key, "owner", p, retry, &secondTarget, []string{second.RecordID}); err != nil {
		t.Fatal(err)
	}
	writes := writer.capture.snapshot()
	if len(writes) != 2 || !reflect.DeepEqual(writes[0], writes[1]) {
		t.Fatal(writes)
	}
	for _, id := range []string{first.RecordID, second.RecordID} {
		got, err := store.Get(id)
		if err != nil || got.State != "completed" {
			t.Fatal(got, err)
		}
	}
	if len(r.questionOperations) != 0 {
		t.Fatal("completed operation retained")
	}
	// An unrelated receipt cannot settle a fresh unknown record.
	fresh := testIngressTask(t, store, "fresh")
	fresh.Kind = "control"
	fresh.Text = ""
	fresh.ControlTarget = &secondTarget
	third := mustAcceptIngress(t, store, fresh)
	retry["operation_id"] = "uncertain"
	writer.mu.Lock()
	writer.receipts = false
	writer.mu.Unlock()
	lateCtx, lateCancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer lateCancel()
	if _, err := r.waitQuestionOperationTarget(lateCtx, p.key, "owner", p, retry, &secondTarget, []string{third.RecordID}); err == nil {
		t.Fatal("expected unknown")
	}
	if err := r.ObserveIngressQuestionReceipt(p, QuestionReceipt{OperationID: "other", RequestID: "q", Version: 2, Accepted: true}); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(third.RecordID)
	if got.Terminal() {
		t.Fatal(got)
	}
	if err := r.ObserveIngressQuestionReceipt(p, QuestionReceipt{OperationID: "uncertain", RequestID: "q", Version: 2, Accepted: true}); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Get(third.RecordID)
	if got.State != "completed" {
		t.Fatal(got)
	}
}

// A late receipt for an ingress record already expired from the 24h store must
// not fail the 14d question operation window: it is skipped instead of marking
// the process input blocked.
func TestIngressQuestionLateReceiptForExpiredRecordIsSkipped(t *testing.T) {
	store := testIngressStore(t)
	r := &NotificationRouter{ingressStore: store, questionOperations: map[string]pendingQuestionOperation{}}
	p := &ChordProcess{key: "workspace|telegram|chat", runtimeID: "runtime", sessionEpoch: 1, bindingInstanceID: "binding", state: ControlState{SessionID: "session"}}
	target := &IngressControlTarget{RuntimeID: "runtime", SessionEpoch: 1, SessionID: "session", BindingInstanceID: "binding", RequestID: "q", Version: 1}
	r.questionOperations["late"] = pendingQuestionOperation{
		command:   map[string]any{"operation_id": "late-op"},
		recordIDs: []string{"gc-missing-record"},
		target:    target,
		expiresAt: time.Now().Add(time.Minute),
	}
	if err := r.ObserveIngressQuestionReceipt(p, QuestionReceipt{OperationID: "late-op", RequestID: "q", Version: 1, Accepted: true}); err != nil {
		t.Fatal(err)
	}
	if len(r.questionOperations) != 0 {
		t.Fatal("expired receipt retained its operation")
	}
}

func TestIngressLoginReusesExistingRenewalService(t *testing.T) {
	calls := 0
	adapter := &stubIMAdapter{typ: "wechat", supportsLoginRenewal: true, startLoginFunc: func() (string, error) { calls++; return "https://example.invalid/login", nil }}
	r := &NotificationRouter{adapter: adapter}
	text, result := r.loginReply("wechat")
	if calls != 1 || result.State != "completed" || !strings.Contains(text, "https://example.invalid/login") {
		t.Fatal(calls, text, result)
	}
	_, result = r.loginReply("feishu")
	if result.State != "rejected" || calls != 1 {
		t.Fatal(result, calls)
	}
}

func TestIngressRoleCardNumericNameIsLiteral(t *testing.T) {
	roles := []RoleInfo{{Name: "2"}, {Name: "builder"}}
	if got, err := selectRoleName("2", roles, true); err != nil || got != "2" {
		t.Fatal(got, err)
	}
	if got, err := selectRoleName("2", roles, false); err != nil || got != "builder" {
		t.Fatal(got, err)
	}
	if _, err := selectRoleName("missing", roles, true); err == nil {
		t.Fatal("missing role accepted")
	}
}

func TestIngressQueryWithoutPinDoesNotSpawn(t *testing.T) {
	r := newTestRouter()
	p, target, result := r.restoreIngressQuery(t.Context(), "test|wechat|chat", &IngressControlTarget{})
	if p != nil || target == nil || result.State != "" || len(r.mgr.procs) != 0 {
		t.Fatal(p, target, result)
	}
}

func TestIngressTrustedInteractionParametersRemainVolatile(t *testing.T) {
	s, p, _, msg := ingressTestService(t)
	pauseIngressTestRoute(t, s, msg)
	p.mu.Lock()
	p.bindingInstanceID = "binding"
	p.state.Questions = map[string]*QuestionPayload{"q": {RequestID: "q", Version: 7, Options: []string{"Choice"}, OptionIDs: []string{"sensitive-choice"}}}
	p.mu.Unlock()
	msg.MessageID = "callback"
	msg.Text = ""
	msg.InternalAction = &InternalAction{BindingInstanceID: "binding", Type: "question", Action: "answer", RequestID: "q", Version: 7, Value: "sensitive-choice", Handle: InteractiveCardHandle{Token: "sensitive-card-token"}}
	result := s.Accept(t.Context(), IngressEnvelope{Message: msg})
	if result.Disposition != ReceiveAccepted {
		t.Fatal(result)
	}
	rec, err := s.store.Get(result.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Kind != "control" || rec.Text != "" || strings.Contains(string(data), "sensitive-choice") || strings.Contains(string(data), "sensitive-card-token") {
		t.Fatal(string(data))
	}
	if rec.ControlTarget == nil || rec.ControlTarget.RequestID != "q" || rec.ControlTarget.Version != 7 {
		t.Fatal(rec)
	}
}

func TestIngressReadOnlyCommandsWithoutActiveRuntime(t *testing.T) {
	s, p, _, msg := ingressTestService(t)
	s.router.mgr.mu.Lock()
	delete(s.router.mgr.procs, p.key)
	s.router.mgr.mu.Unlock()
	if err := s.router.mgr.pins.Set(pinKeyForBinding(p.key), ""); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"/help", "/status", "/todos", "/role", "/login"} {
		msg.MessageID, msg.Text = text, text
		ticket, ok := s.router.admission.Authorize(msg)
		if !ok {
			t.Fatal("authorization")
		}
		msg.ticket = &ticket
		_, target := s.router.CaptureIngress(msg)
		result := s.router.DispatchAccepted(t.Context(), msg, target)
		if result.State != "completed" || s.router.mgr.GetProcessForKey(p.key) != nil {
			t.Fatal(text, result)
		}
	}
}

func TestIngressExplicitCommandStartsOnlyFrozenFirstRuntime(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(map[bool]string{false: "first session", true: "original pin"}[pinned], func(t *testing.T) {
			cfg := testConfig()
			cfg.Workspaces[0].Path = t.TempDir()
			binary := filepath.Join(t.TempDir(), "chord")
			script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"ready\",\"payload\":{\"session_id\":\"session\",\"runtime_id\":\"runtime\",\"session_epoch\":1}}'\nwhile IFS= read -r line; do :; done\n"
			if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			cfg.ChordPath = binary
			mgr := newTestChordManager(cfg)
			mgr.pins = newSessionPinStore(t.TempDir())
			defer mgr.StopAll(time.Second)
			r := &NotificationRouter{mgr: mgr}
			key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
			target := &IngressControlTarget{}
			if pinned {
				target.SessionID = "session"
				if err := mgr.pins.Set(pinKeyForBinding(key), "session"); err != nil {
					t.Fatal(err)
				}
			}
			p, frozen, result := r.startIngressCommandProcess(t.Context(), key, target)
			if result.State != "" || p == nil || frozen.RuntimeID != "runtime" || frozen.SessionID != "session" {
				t.Fatal(p, frozen, result)
			}
			if err := p.sendIngressControl(frozen, IMCommand{Type: "local_shell"}, map[string]any{"type": "local_shell", "command": "echo sample"}); err != nil {
				t.Fatal(err)
			}
			// The original absence cannot be reused after a process has appeared.
			if _, _, result := r.startIngressCommandProcess(t.Context(), key, target); result.State != "rejected" || mgr.GetProcessForKey(key) != p {
				t.Fatal(result)
			}
		})
	}
}

func TestIngressControlCommandsPreserveUserOperations(t *testing.T) {
	for _, tc := range []struct{ name, text, typ, action string }{
		{"approval", "/allow", "confirm", "allow"},
		{"denial", "/deny Needs more work", "confirm", "deny"},
		{"Done feedback", "Needs more work", "confirm", "deny"},
		{"handoff", "/handoff builder sample-pool", "handoff", "accept"},
		{"handoff denial", "/handoff-deny Keep current agent", "handoff", "deny"},
		{"cancel", "/cancel", "cancel", ""},
		{"local shell", "! echo sample", "local_shell", ""},
		{"Chord command", "/summary", "send", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, p, w, msg := ingressTestService(t)
			pauseIngressTestRoute(t, s, msg)
			p.mu.Lock()
			p.state.PendingConfirm = &ConfirmPayload{RequestID: "confirm", ToolName: "Shell"}
			if tc.name == "Done feedback" {
				p.state.PendingConfirm.ToolName = "Done"
			}
			p.state.PendingHandoff = &HandoffPayload{RequestID: "handoff"}
			p.state.Busy = true
			p.state.TurnStartedAt = time.Now()
			p.mu.Unlock()
			msg.MessageID, msg.Text = "operation", tc.text
			ticket, ok := s.router.admission.Authorize(msg)
			if !ok {
				t.Fatal("authorization")
			}
			msg.ticket = &ticket
			kind, target := s.router.CaptureIngress(msg)
			if kind != "control" {
				t.Fatal(kind, target)
			}
			accepted := s.Accept(t.Context(), IngressEnvelope{Message: msg})
			if accepted.Disposition != ReceiveAccepted {
				t.Fatal(accepted)
			}
			result := s.router.DispatchAccepted(t.Context(), msg, target, accepted.RecordID)
			if result.State != "submitted" && !(tc.typ == "send" && result.State == "awaiting_receipt") {
				t.Fatal(result)
			}
			writes := w.snapshot()
			if len(writes) != 1 || writes[0]["type"] != tc.typ {
				t.Fatal(writes)
			}
			sent := writes[0]
			if tc.action != "" && sent["action"] != tc.action {
				t.Fatal(sent)
			}
			if tc.typ == "confirm" && sent["request_id"] != "confirm" {
				t.Fatal(sent)
			}
			if tc.name == "Done feedback" && sent["deny_reason"] != tc.text {
				t.Fatal(sent)
			}
			if tc.name == "handoff" && (sent["agent"] != "builder" || sent["pool"] != "sample-pool") {
				t.Fatal(sent)
			}
			if tc.typ == "send" && (sent["content"] != "/summary" || sent["request_id"] == "") {
				t.Fatal(sent)
			}
		})
	}
}

func TestIngressQuestionActionsPreserveOriginalRequests(t *testing.T) {
	for _, tc := range []struct{ text, action string }{
		{"/answer @q Answer", "answer"},
		{"/question-interact q", "interact"},
		{"/question-withdraw q", "withdraw_requirement"},
		{"/question-replace q replacement", "replace_requirement"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			s, p, _, msg := ingressTestService(t)
			writer := &ingressInteractionWriter{proc: p, receipts: true}
			p.mu.Lock()
			p.stdin = writer
			p.state.Questions = map[string]*QuestionPayload{"q": {RequestID: "q", Version: 1}}
			p.mu.Unlock()
			msg.MessageID, msg.Text = "question-operation", tc.text
			ticket, ok := s.router.admission.Authorize(msg)
			if !ok {
				t.Fatal("authorization")
			}
			msg.ticket = &ticket
			_, target := s.router.CaptureIngress(msg)
			result := s.router.DispatchAccepted(t.Context(), msg, target)
			if result.State != "completed" {
				t.Fatal(result)
			}
			sent := writer.capture.snapshot()
			if len(sent) != 1 || sent[0]["type"] != "question" || sent[0]["request_id"] != "q" || sent[0]["action"] != tc.action {
				t.Fatal(sent)
			}
			if tc.action == "replace_requirement" && sent[0]["replacement_id"] != "replacement" {
				t.Fatal(sent)
			}
		})
	}
}

func TestIngressQueuedTrustedInteractionCannotApproveReplacement(t *testing.T) {
	s, p, w, msg := ingressTestService(t)
	pauseIngressTestRoute(t, s, msg)
	p.mu.Lock()
	p.bindingInstanceID = "binding"
	p.state.PendingConfirm = &ConfirmPayload{RequestID: "original"}
	p.mu.Unlock()
	msg.MessageID, msg.Text = "callback", ""
	msg.InternalAction = &InternalAction{BindingInstanceID: "binding", Type: "confirm", Action: "allow", RequestID: "original"}
	accepted := s.Accept(t.Context(), IngressEnvelope{Message: msg})
	if accepted.Disposition != ReceiveAccepted {
		t.Fatal(accepted)
	}
	s.mu.Lock()
	control := s.controls[accepted.RecordID]
	s.mu.Unlock()
	p.mu.Lock()
	p.state.PendingConfirm = &ConfirmPayload{RequestID: "replacement"}
	p.mu.Unlock()
	result := s.router.DispatchAccepted(t.Context(), control.message, control.target, accepted.RecordID)
	if result.State != "rejected" || len(w.snapshot()) != 0 {
		t.Fatal(result, w.snapshot())
	}
}

func TestIngressDispatcherAllowsOnlyRelatedExplicitQuestionRetry(t *testing.T) {
	for _, foreignUnknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "only related unknown", true: "unrelated unknown task"}[foreignUnknown], func(t *testing.T) {
			s, p, _, msg := ingressTestService(t)
			id := pauseIngressTestRoute(t, s, msg)
			writer := &ingressInteractionWriter{proc: p}
			p.mu.Lock()
			p.stdin = writer
			p.state.Questions = map[string]*QuestionPayload{"q": {RequestID: "q", Version: 1}}
			p.mu.Unlock()
			msg.MessageID, msg.Text = "original-question", "/answer @q Answer"
			first := s.Accept(t.Context(), IngressEnvelope{Message: msg})
			if first.Disposition != ReceiveAccepted {
				t.Fatal(first)
			}
			s.mu.Lock()
			original := s.controls[first.RecordID]
			delete(s.controls, first.RecordID)
			s.mu.Unlock()
			payload, err := questionOperationCommand(original.message, p.State().Questions["q"], commandForIncomingMessage(original.message), original.target)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
			_, err = s.router.waitQuestionOperationTarget(ctx, p.key, msg.SenderID, p, payload, original.target, []string{first.RecordID})
			cancel()
			if err == nil {
				t.Fatal("expected unknown")
			}
			if err := s.store.UpdateRecord(t.Context(), first.RecordID, []ingressState{"received"}, func(rec *IngressRecord) error { rec.State = "unknown"; return nil }); err != nil {
				t.Fatal(err)
			}
			msg.MessageID = "explicit-retry"
			retry := s.Accept(t.Context(), IngressEnvelope{Message: msg})
			if retry.Disposition != ReceiveAccepted {
				t.Fatal(retry)
			}
			s.mu.Lock()
			control := s.controls[retry.RecordID]
			s.mu.Unlock()
			records := s.store.ListRoutePendingMetadata(id)
			if !s.canRetryIngressQuestion(control, records) {
				t.Fatal("matching explicit retry not recognized")
			}
			changed := control
			changed.message.Text = "/answer @q Different"
			if s.canRetryIngressQuestion(changed, records) {
				t.Fatal("different parameters matched")
			}
			changed = control
			changed.message.SenderID = "other"
			if s.canRetryIngressQuestion(changed, records) {
				t.Fatal("different sender matched")
			}
			if foreignUnknown {
				taskMsg := msg
				taskMsg.MessageID, taskMsg.Text = "unrelated", "Unrelated task"
				task := s.Accept(t.Context(), IngressEnvelope{Message: taskMsg})
				if task.Disposition != ReceiveAccepted {
					t.Fatal(task)
				}
				if err := s.store.UpdateRecord(t.Context(), task.RecordID, []ingressState{"received"}, func(rec *IngressRecord) error { rec.State = "unknown"; return nil }); err != nil {
					t.Fatal(err)
				}
				records = s.store.ListRoutePendingMetadata(id)
				if s.canRetryIngressQuestion(control, records) {
					t.Fatal("retry bypassed unrelated unknown")
				}
			}
			writer.mu.Lock()
			writer.receipts = true
			writer.mu.Unlock()
			dispatchIngressTestRoute(s, id)
			after, err := s.store.Get(retry.RecordID)
			if err != nil {
				t.Fatal(err)
			}
			originalAfter, err := s.store.Get(first.RecordID)
			if err != nil {
				t.Fatal(err)
			}
			if foreignUnknown {
				if after.State != "rejected" || originalAfter.State != "unknown" || len(writer.capture.snapshot()) != 1 {
					t.Fatal(after, originalAfter, writer.capture.snapshot())
				}
			} else {
				writes := writer.capture.snapshot()
				if after.State != "completed" || originalAfter.State != "completed" || len(writes) != 2 || !reflect.DeepEqual(writes[0], writes[1]) {
					t.Fatal(after, originalAfter, writes)
				}
			}
		})
	}
}

type ingressStaleQuestionReceiptWriter struct{ proc *ChordProcess }

func (w ingressStaleQuestionReceiptWriter) Close() error { return nil }
func (w ingressStaleQuestionReceiptWriter) Write(data []byte) (int, error) {
	var cmd struct {
		OperationID string `json:"operation_id"`
		RequestID   string `json:"request_id"`
		Version     uint64 `json:"version"`
	}
	if err := json.Unmarshal(data, &cmd); err != nil {
		return 0, err
	}
	raw, _ := json.Marshal(QuestionReceipt{OperationID: cmd.OperationID, RequestID: cmd.RequestID, Version: cmd.Version - 1, Accepted: true})
	go w.proc.processEnvelope(&HeadlessEnvelope{Type: "question_receipt", Payload: raw})
	return len(data), nil
}
func TestIngressQuestionStaleReceiptDoesNotConfirmAcceptance(t *testing.T) {
	p := &ChordProcess{runtimeID: "runtime", sessionEpoch: 1, state: ControlState{SessionID: "session", Questions: map[string]*QuestionPayload{"q": {RequestID: "q", Version: 5}}}}
	p.stdin = ingressStaleQuestionReceiptWriter{proc: p}
	target := &IngressControlTarget{RuntimeID: "runtime", SessionEpoch: 1, SessionID: "session", RequestID: "q", Version: 5}

	r := &NotificationRouter{}
	if result := r.dispatchIngressQuestion(t.Context(), IncomingMessage{MessageID: "answer", Text: "/answer @q custom"}, p, IMCommand{Type: "question", RequestID: "q", Answers: []string{"custom"}}, target); result.State != "unknown" {
		t.Fatal(result)
	}
	p.mu.RLock()
	waiters := len(p.questionWaiters)
	p.mu.RUnlock()
	if waiters != 0 || len(r.questionOperations) != 1 {
		t.Fatal(waiters, len(r.questionOperations))
	}
}
