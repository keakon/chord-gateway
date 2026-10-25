package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	feishu "github.com/keakon/chord-gateway/internal/platform/feishu"

	larkcallback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

func migrationFeishuRouter(t *testing.T, handler http.HandlerFunc) (*NotificationRouter, *ChordProcess, im.IncomingMessage) {
	t.Helper()
	fc := &config.FeishuConfig{AppID: "app", Owner: &config.OwnerIdentity{AccountID: "app", UserID: "owner"}, ChatBindings: map[string]string{"chat": "workspace"}}
	cfg := &config.Config{IMs: []config.IMAdapterConfig{{Feishu: fc}}, Workspaces: []config.Workspace{{ID: "workspace", Path: t.TempDir()}}}
	store := &ConfigStore{}
	store.snapshot.Store(cfg)
	mgr := NewChordManager(cfg, &config.Paths{StateDir: t.TempDir()})
	mgr.store = store
	r := NewNotificationRouter(mgr)
	r.admission = NewAdmission(NewPairingService(store, false, nil))
	mgr.admission = r.admission
	if err := r.admission.pairing.PlatformReady("feishu", "app"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	originalURL := feishu.OpenBaseURL
	feishu.OpenBaseURL = server.URL
	a := &feishu.FeishuAdapter{}
	a.WireForTest(cfg.IMs[0], server.Client(), r)
	a.SetTokenForTest("token", time.Now().Add(time.Hour))
	r.SetAdapter(a)
	key := (processKey{workspaceID: "workspace", imType: "feishu", chatID: "chat"}).String()
	p := &ChordProcess{key: key, runtimeID: "runtime", sessionEpoch: 1, bindingInstanceID: "binding", stdin: &ingressTestWriter{}, state: ControlState{SessionID: "session", BindingInstanceID: "binding"}}
	p.onEvent = r.HandleChordEvent
	mgr.mu.Lock()
	mgr.procs[key] = p
	mgr.mu.Unlock()
	if err := mgr.pins.Set(pinKeyForBinding(key), "session"); err != nil {
		t.Fatal(err)
	}
	msg := im.IncomingMessage{IMType: "feishu", AccountID: "app", ChatKind: "private", ChatID: "chat", SenderID: "owner", MessageID: "callback", InternalAction: &im.InternalAction{Type: "question", Action: "answer", RequestID: "q", Value: "option", BindingInstanceID: "binding", Handle: im.InteractiveCardHandle{MessageID: "card"}}}
	ticket, ok := r.admission.Authorize(msg)
	if !ok {
		t.Fatal("owner not authorized")
	}
	msg.SetAdmissionTicket(&ticket)
	t.Cleanup(func() { r.Close(); r.admission.Close(); server.Close(); feishu.OpenBaseURL = originalURL })
	return r, p, msg
}

func flushIngressPresentation(t *testing.T, r *NotificationRouter, msg im.IncomingMessage) {
	t.Helper()
	ws, err := r.getConfig().ResolveWorkspace(msg.IMType, msg.ChatID)
	if err != nil {
		t.Fatal(err)
	}
	key := (processKey{workspaceID: ws.ID, imType: msg.IMType, chatID: msg.ChatID}).String()
	for _, queueKey := range []string{key, compositeKey(msg.IMType, msg.AccountID, msg.ChatID)} {
		done := make(chan struct{})
		if r.outbound.enqueueOrWait(queueKey, func() { close(done) }) != outboundQueued {
			t.Fatal("outbound unavailable")
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("presentation timed out")
		}
	}
}

func TestFeishuIngressDiagnosticsResolveOriginalCardWithoutDuplicateText(t *testing.T) {
	for _, state := range []im.IngressState{"rejected", "unknown"} {
		t.Run(string(state), func(t *testing.T) {
			var mu sync.Mutex
			var patches, texts int
			var content string
			r, p, msg := migrationFeishuRouter(t, func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if req.Method == http.MethodPatch {
					patches++
					var body map[string]string
					_ = json.NewDecoder(req.Body).Decode(&body)
					content = body["content"]
				} else {
					texts++
				}
				fmt.Fprint(w, `{"code":0,"data":{"message_id":"message"}}`)
			})
			r.recordCardHandle(p.key, "question", "q", &im.InteractiveCardHandle{MessageID: "card"})
			r.enqueueIngressDispatchDiagnostic(msg, DispatchResult{state, "question_outcome_unknown"})
			flushIngressPresentation(t, r, msg)
			mu.Lock()
			if patches != 1 || texts != 0 || strings.Contains(content, "Processing") || !strings.Contains(content, map[string]string{"rejected": "Request not applied", "unknown": "Outcome unconfirmed"}[string(state)]) {
				t.Fatal(patches, texts, content)
			}
			mu.Unlock()
			r.mu.Lock()
			_, retained := r.cardHandles[cardHandleKey(p.key, "question", "q")]
			r.mu.Unlock()
			if retained != (state == "unknown") {
				t.Fatal("unexpected handle retention", retained)
			}
		})
	}
}

func TestFeishuIngressUnknownCardPatchDoesNotBlindlySendText(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			var mu sync.Mutex
			var patches, texts int
			r, p, msg := migrationFeishuRouter(t, func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if req.Method == http.MethodPatch {
					patches++
					if unknown {
						w.WriteHeader(http.StatusBadGateway)
					} else {
						w.WriteHeader(http.StatusBadRequest)
					}
					fmt.Fprint(w, `{"code":230099,"msg":"failure"}`)
				} else {
					texts++
					fmt.Fprint(w, `{"code":0,"data":{"message_id":"message"}}`)
				}
			})
			r.recordCardHandle(p.key, "question", "q", &im.InteractiveCardHandle{MessageID: "card"})
			r.enqueueIngressInteractionResult(msg, "question", "q", "Question updated", "Question updated.", "green")
			flushIngressPresentation(t, r, msg)
			mu.Lock()
			wantTexts := 1
			if unknown {
				wantTexts = 0
			}
			if patches != 1 || texts != wantTexts {
				t.Fatal(patches, texts)
			}
			mu.Unlock()
			r.mu.Lock()
			_, retained := r.cardHandles[cardHandleKey(p.key, "question", "q")]
			r.mu.Unlock()
			if !retained {
				t.Fatal("failed patch discarded original handle")
			}
		})
	}
}

func TestFeishuIngressConfirmationWriteDoesNotClaimChordAcceptance(t *testing.T) {
	var mu sync.Mutex
	var card string
	var texts int
	r, p, msg := migrationFeishuRouter(t, func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if req.Method == http.MethodPatch {
			var body map[string]string
			_ = json.NewDecoder(req.Body).Decode(&body)
			card = body["content"]
		} else {
			texts++
		}
		fmt.Fprint(w, `{"code":0,"data":{"message_id":"message"}}`)
	})
	msg.InternalAction = &im.InternalAction{Type: "confirm", Action: "allow", RequestID: "confirm", BindingInstanceID: "binding", Handle: im.InteractiveCardHandle{MessageID: "card"}}
	p.state.PendingConfirm = &ConfirmPayload{RequestID: "confirm", ToolName: "Shell"}
	r.recordCardHandle(p.key, "confirm", "confirm", &im.InteractiveCardHandle{MessageID: "card"})
	_, target := r.CaptureIngress(msg)
	result := r.DispatchAccepted(t.Context(), msg, target)
	if result.State != "submitted" {
		t.Fatal(result)
	}
	flushIngressPresentation(t, r, msg)
	mu.Lock()
	if texts != 0 || !strings.Contains(card, "Approval submitted") || !strings.Contains(card, "acceptance is unconfirmed") || strings.Contains(card, "Confirmation approved") {
		t.Fatal(card, texts)
	}
	mu.Unlock()
	r.mu.Lock()
	_, retained := r.cardHandles[cardHandleKey(p.key, "confirm", "confirm")]
	r.mu.Unlock()
	if !retained {
		t.Fatal("submitted confirmation discarded reconciliation handle")
	}
}

func TestFeishuConfirmResolutionWinsOverLateSubmissionFeedback(t *testing.T) {
	for _, tc := range []struct {
		name           string
		terminalFirst  bool
		cardSendQueued bool
		outcome        string
	}{
		{name: "timeout delivered first", terminalFirst: true, outcome: ConfirmOutcomeTimeout},
		{name: "feedback before timeout", outcome: ConfirmOutcomeTimeout},
		{name: "cancellation delivered first", terminalFirst: true, outcome: ConfirmOutcomeCancelled},
		{name: "feedback before cancellation", outcome: ConfirmOutcomeCancelled},
		{name: "confirmation card still sending", terminalFirst: true, cardSendQueued: true, outcome: ConfirmOutcomeTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var cards []string
			var texts int
			r, p, msg := migrationFeishuRouter(t, func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if req.Method == http.MethodPatch {
					var body map[string]string
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					cards = append(cards, body["content"])
				} else {
					texts++
				}
				fmt.Fprint(w, `{"code":0,"data":{"message_id":"message"}}`)
			})
			msg.InternalAction = &im.InternalAction{Type: "confirm", Action: "allow", RequestID: "req-1", BindingInstanceID: "binding", Handle: im.InteractiveCardHandle{MessageID: "card"}}
			if tc.cardSendQueued {
				release := make(chan struct{})
				released := false
				defer func() {
					if !released {
						close(release)
					}
				}()
				r.outbound.enqueueOrWait(p.key, func() { <-release })
				p.processEnvelope(&HeadlessEnvelope{Type: "confirm_request", Seq: 1, Payload: json.RawMessage(`{"request_id":"req-1","tool_name":"Read"}`)})
				p.processEnvelope(&HeadlessEnvelope{Type: "confirm_resolved", Seq: 2, Payload: confirmResolvedPayload("req-1", tc.outcome)})
				r.enqueueIngressInteractionPresentation(msg, "confirm", "req-1", "Approval submitted", "Chord acceptance is unconfirmed; check /status.", "grey", false, "binding")
				close(release)
				released = true
			} else {
				p.mu.Lock()
				p.state.PendingConfirm = &ConfirmPayload{RequestID: "req-1", ToolName: "Read"}
				p.mu.Unlock()
				r.recordCardHandle(p.key, "confirm", "req-1", &im.InteractiveCardHandle{MessageID: "card"})

				queueFeedback := func() {
					r.enqueueIngressInteractionPresentation(msg, "confirm", "req-1", "Approval submitted", "Chord acceptance is unconfirmed; check /status.", "grey", false, "binding")
				}
				resolve := func() {
					p.processEnvelope(&HeadlessEnvelope{Type: "confirm_resolved", Seq: 1, Payload: confirmResolvedPayload("req-1", tc.outcome)})
				}
				if tc.terminalFirst {
					resolve()
					flushIngressPresentation(t, r, msg)
					queueFeedback()
				} else {
					queueFeedback()
					flushIngressPresentation(t, r, msg)
					resolve()
				}
			}
			flushIngressPresentation(t, r, msg)

			mu.Lock()
			defer mu.Unlock()
			wantTexts := 0
			if tc.cardSendQueued {
				wantTexts = 1
			}
			wantTitle := "Confirmation expired"
			if tc.outcome == ConfirmOutcomeCancelled {
				wantTitle = "Confirmation cancelled"
			}
			if len(cards) == 0 || texts != wantTexts || !strings.Contains(cards[len(cards)-1], wantTitle) {
				t.Fatalf("cards=%v texts=%d, want terminal confirmation card last", cards, texts)
			}
			for _, card := range cards {
				if tc.terminalFirst && strings.Contains(card, "Approval submitted") {
					t.Fatalf("stale submission overwrote final card: %v", cards)
				}
			}
			r.mu.RLock()
			entry := r.cardHandles[cardHandleKey(p.key, "confirm", "req-1")]
			r.mu.RUnlock()
			if !entry.terminal || entry.handle != (im.InteractiveCardHandle{}) {
				t.Fatalf("terminal card guard not retained after patch: %+v", entry)
			}
		})
	}
}

func TestFeishuConfirmResolutionDelivery(t *testing.T) {
	for _, name := range []string{"success", "no handle", "rejected patch", "unknown patch"} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var patches, texts int
			r, p, msg := migrationFeishuRouter(t, func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case req.Method == http.MethodPatch && name == "rejected patch":
					patches++
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"code":230099,"msg":"failure"}`)
					return
				case req.Method == http.MethodPatch && name == "unknown patch":
					patches++
					w.WriteHeader(http.StatusBadGateway)
					fmt.Fprint(w, `{"code":230099,"msg":"failure"}`)
					return
				case req.Method == http.MethodPatch:
					patches++
				default:
					texts++
				}
				fmt.Fprint(w, `{"code":0,"data":{"message_id":"message"}}`)
			})
			if name != "no handle" {
				r.recordCardHandle(p.key, "confirm", "req-1", &im.InteractiveCardHandle{MessageID: "card"})
			}
			p.processEnvelope(&HeadlessEnvelope{Type: "confirm_resolved", Seq: 1, Payload: confirmResolvedPayload("req-1", ConfirmOutcomeTimeout)})
			flushIngressPresentation(t, r, msg)
			mu.Lock()
			wantPatches, wantTexts := 1, 0
			switch name {
			case "no handle":
				wantPatches, wantTexts = 0, 1
			case "rejected patch":
				wantTexts = 1
			}
			gotPatches, gotTexts := patches, texts
			mu.Unlock()
			if gotPatches != wantPatches || gotTexts != wantTexts {
				t.Fatalf("patches=%d texts=%d, want %d and %d", gotPatches, gotTexts, wantPatches, wantTexts)
			}
			r.mu.RLock()
			entry := r.cardHandles[cardHandleKey(p.key, "confirm", "req-1")]
			r.mu.RUnlock()
			wantHandle := ""
			if name == "rejected patch" || name == "unknown patch" {
				wantHandle = "card"
			}
			if !entry.terminal || entry.handle.MessageID != wantHandle {
				t.Fatalf("unexpected terminal handle state: %+v", entry)
			}
			msg.InternalAction = &im.InternalAction{Type: "confirm", RequestID: "req-1", BindingInstanceID: "binding", Handle: im.InteractiveCardHandle{MessageID: "card"}}
			r.enqueueIngressInteractionFeedback(msg, "confirm", "req-1", "Request not applied", "Request was rejected.", "red", true, true, "binding")
			flushIngressPresentation(t, r, msg)
			mu.Lock()
			defer mu.Unlock()
			if patches != wantPatches || texts != wantTexts+1 {
				t.Fatalf("diagnostic overwrote terminal card or disappeared: patches=%d texts=%d", patches, texts)
			}
		})
	}
}

func TestFeishuIngressRoleFailureDoesNotRepeatChatMessage(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	r, _, msg := migrationFeishuRouter(t, func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		fmt.Fprint(w, `{"code":0,"data":{"message_id":"message"}}`)
	})
	msg.InternalAction.Type = "role"
	msg.InternalAction.Action = "switch"
	msg.InternalAction.RequestID = "menu"
	r.enqueueIngressInteractionResult(msg, "role", "menu", "Role switch rejected", "Role switch was rejected.", "red")
	r.enqueueIngressDispatchDiagnostic(msg, DispatchResult{"rejected", "role_switch_rejected"})
	flushIngressPresentation(t, r, msg)
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Fatal("duplicated role failure", requests)
	}
}

func TestFeishuQuestionCardCarriesFrozenVersion(t *testing.T) {
	q := &QuestionPayload{RequestID: "q", Version: 7, Options: []string{"Choice"}, OptionIDs: []string{"option"}}
	for _, multiple := range []bool{false, true} {
		q.Multiple = multiple
		data, _ := json.Marshal(buildFeishuQuestionCard("chat", q, feishuCardContext{SessionID: "session", BindingInstanceID: "binding"}))
		if !strings.Contains(string(data), `"version":7`) || !strings.Contains(string(data), `"binding_instance_id":"binding"`) || !strings.Contains(string(data), `"session_id":"session"`) {
			t.Fatal(string(data))
		}
	}
}

func TestFeishuIngressStatusRestoresQuestionPresentationOnce(t *testing.T) {
	var mu sync.Mutex
	cards := 0
	r, p, msg := migrationFeishuRouter(t, func(w http.ResponseWriter, req *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(req.Body).Decode(&body)
		mu.Lock()
		if body["msg_type"] == "interactive" {
			cards++
		}
		mu.Unlock()
		fmt.Fprint(w, `{"code":0,"data":{"message_id":"question-card"}}`)
	})
	p.stdin = &ingressInteractionWriter{proc: p, snapshot: StatusResponse{SessionID: "session", Questions: []*QuestionPayload{{RequestID: "restored", Version: 2, Question: "Choose", Options: []string{"Choice"}, OptionIDs: []string{"option"}, Timer: "disabled"}}}}
	msg.InternalAction = nil
	msg.Text = "/status"
	for i := 0; i < 2; i++ {
		_, target := r.CaptureIngress(msg)
		if result := r.DispatchAccepted(context.Background(), msg, target); result.State != "completed" {
			t.Fatal(result)
		}
		flushIngressPresentation(t, r, msg)
	}
	mu.Lock()
	defer mu.Unlock()
	if cards != 1 {
		t.Fatal("status failed to restore or duplicated question", cards)
	}
}

func migrationFeishuIngress(t *testing.T, r *NotificationRouter, msg im.IncomingMessage) (*IngressService, im.ChatIdentity) {
	t.Helper()
	store := NewIngressStore()
	service := NewIngressService(store, r)
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	id := pauseIngressTestRoute(t, service, msg)
	t.Cleanup(func() {
		service.Close()
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return service, id
}

func TestFeishuRealIngressNamespacesVolatileAnswersAndReceipt(t *testing.T) {
	r, p, msg := migrationFeishuRouter(t, func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{"message_id":"message"}}`)
	})
	service, id := migrationFeishuIngress(t, r, msg)
	writer := &ingressInteractionWriter{proc: p, receipts: true}
	p.mu.Lock()
	p.stdin = writer
	p.state.Questions = map[string]*QuestionPayload{"q": {RequestID: "q", Version: 7, Options: []string{"Choice"}, OptionIDs: []string{"sensitive-option"}}}
	p.mu.Unlock()
	adapter := findFeishuAdapterForTest(r)
	var message larkim.P2MessageReceiveV1
	if err := json.Unmarshal([]byte(`{"schema":"2.0","header":{"app_id":"app","event_id":"text-event"},"event":{"sender":{"sender_type":"user","sender_id":{"open_id":"owner"}},"message":{"message_id":"same","chat_id":"chat","chat_type":"p2p","message_type":"text","content":"{\"text\":\"/help\"}"}}}`), &message); err != nil {
		t.Fatal(err)
	}
	future, err := adapter.SubmitMessageEvent(t.Context(), &message, "delivery-1")
	if err != nil || future == nil {
		t.Fatal(err)
	}
	first := im.WaitIngressNormalization(t.Context(), future)
	if first.Disposition != im.ReceiveAccepted {
		t.Fatal(first)
	}
	future, err = adapter.SubmitMessageEvent(t.Context(), &message, "delivery-2")
	if err != nil {
		t.Fatal(err)
	}
	duplicate := im.WaitIngressNormalization(t.Context(), future)
	if duplicate.Disposition != im.ReceiveDuplicate || duplicate.RecordID != first.RecordID {
		t.Fatal(duplicate)
	}
	var card larkcallback.CardActionTriggerEvent
	if err := json.Unmarshal([]byte(`{"schema":"2.0","header":{"app_id":"app","event_id":"same"},"event":{"operator":{"open_id":"owner"},"token":"sensitive-token","action":{"tag":"button","value":{"type":"question","action":"answer","request_id":"q","version":7,"binding_instance_id":"binding","chat_id":"chat","im_type":"feishu","value":"sensitive-option"}},"context":{"open_chat_id":"chat","open_message_id":"card"}}}`), &card); err != nil {
		t.Fatal(err)
	}
	future, immediate, err := adapter.SubmitCardActionEvent(t.Context(), &card, "card-delivery-1")
	if err != nil || future == nil || immediate != nil {
		t.Fatal(immediate, err)
	}
	second := im.WaitIngressNormalization(t.Context(), future)
	if second.Disposition != im.ReceiveAccepted || second.RecordID == first.RecordID {
		t.Fatal(second)
	}
	future, _, err = adapter.SubmitCardActionEvent(t.Context(), &card, "card-delivery-2")
	if err != nil {
		t.Fatal(err)
	}
	duplicate = im.WaitIngressNormalization(t.Context(), future)
	if duplicate.Disposition != im.ReceiveDuplicate || duplicate.RecordID != second.RecordID {
		t.Fatal(duplicate)
	}
	records, err := service.store.ListRouteRecords(id)
	if err != nil || len(records) != 2 || records[0].BusinessID != "message:same" || records[1].BusinessID != "card:same" {
		t.Fatal(records, err)
	}
	data, _ := json.Marshal(records[1])
	if strings.Contains(string(data), "sensitive-option") || strings.Contains(string(data), "sensitive-token") || records[1].ControlTarget == nil || records[1].ControlTarget.Version != 7 {
		t.Fatal(string(data))
	}
	dispatchIngressTestRoute(service, id)
	waitIngressCondition(t, func() bool { rec, _ := service.store.Get(second.RecordID); return rec.State == "completed" })
	writes := writer.capture.snapshot()
	if len(writes) != 1 || writes[0]["type"] != "question" || writes[0]["operation_id"] == "" {
		t.Fatal(writes)
	}
}

func TestFeishuRealIngressRejectsAcceptedCardBehindUnknownOutcome(t *testing.T) {
	var mu sync.Mutex
	var content string
	var patches, texts int
	r, p, msg := migrationFeishuRouter(t, func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if req.Method == http.MethodPatch {
			patches++
			var body map[string]string
			_ = json.NewDecoder(req.Body).Decode(&body)
			content = body["content"]
		} else {
			texts++
		}
		fmt.Fprint(w, `{"code":0,"data":{"message_id":"message"}}`)
	})
	service, id := migrationFeishuIngress(t, r, msg)
	task := msg
	task.InternalAction = nil
	task.Text, task.MessageID = "uncertain task", "message:old"
	previous := service.Accept(t.Context(), im.IngressEnvelope{Message: task})
	if previous.Disposition != im.ReceiveAccepted {
		t.Fatal(previous)
	}
	if err := service.store.UpdateRecord(t.Context(), previous.RecordID, []im.IngressState{"received"}, func(rec *IngressRecord) error { rec.State = "unknown"; return nil }); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.state.PendingConfirm = &ConfirmPayload{RequestID: "confirm", ToolName: "Shell"}
	p.mu.Unlock()
	msg.MessageID = "card:new"
	msg.InternalAction = &im.InternalAction{Type: "confirm", Action: "allow", RequestID: "confirm", BindingInstanceID: "binding", Handle: im.InteractiveCardHandle{MessageID: "card"}}
	accepted := service.Accept(t.Context(), im.IngressEnvelope{Message: msg})
	if accepted.Disposition != im.ReceiveAccepted {
		t.Fatal(accepted)
	}
	dispatchIngressTestRoute(service, id)
	flushIngressPresentation(t, r, msg)
	after, err := service.store.Get(accepted.RecordID)
	if err != nil || after.State != "rejected" || after.Reason != "route_outcome_unknown" {
		t.Fatal(after, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if patches != 1 || texts != 0 || !strings.Contains(content, "route_outcome_unknown") {
		t.Fatal(patches, texts, content)
	}
	writer := p.stdin.(*ingressTestWriter)
	if len(writer.snapshot()) != 0 {
		t.Fatal(writer.snapshot())
	}
}

func TestFeishuRealIngressQueuedCardCannotAffectReplacementRequest(t *testing.T) {
	r, p, msg := migrationFeishuRouter(t, func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{"message_id":"message"}}`)
	})
	service, id := migrationFeishuIngress(t, r, msg)
	p.mu.Lock()
	p.state.PendingConfirm = &ConfirmPayload{RequestID: "original", ToolName: "Shell"}
	p.mu.Unlock()
	msg.InternalAction = &im.InternalAction{Type: "confirm", Action: "allow", RequestID: "original", BindingInstanceID: "binding", Handle: im.InteractiveCardHandle{MessageID: "old-card"}}
	msg.MessageID = "card:approval"
	accepted := service.Accept(t.Context(), im.IngressEnvelope{Message: msg})
	if accepted.Disposition != im.ReceiveAccepted {
		t.Fatal(accepted)
	}
	p.mu.Lock()
	p.state.PendingConfirm = &ConfirmPayload{RequestID: "replacement", ToolName: "Shell"}
	p.mu.Unlock()
	dispatchIngressTestRoute(service, id)
	after, err := service.store.Get(accepted.RecordID)
	if err != nil || after.State != "rejected" || after.Reason != "control_target_expired" {
		t.Fatal(after, err)
	}
	if writes := p.stdin.(*ingressTestWriter).snapshot(); len(writes) != 0 {
		t.Fatal(writes)
	}
}

type ingressResolutionBeforeReceiptWriter struct {
	proc    *ChordProcess
	patched <-chan struct{}
}

func (w ingressResolutionBeforeReceiptWriter) Close() error { return nil }
func (w ingressResolutionBeforeReceiptWriter) Write(data []byte) (int, error) {
	var cmd struct {
		OperationID string `json:"operation_id"`
		RequestID   string `json:"request_id"`
		Version     uint64 `json:"version"`
	}
	if err := json.Unmarshal(data, &cmd); err != nil {
		return 0, err
	}
	go func() {
		resolved, _ := json.Marshal(QuestionPayload{RequestID: cmd.RequestID, Version: cmd.Version + 1, Outcome: QuestionResolvedAnswered, Selected: []string{"Choice"}})
		w.proc.processEnvelope(&HeadlessEnvelope{Type: "question_resolved", Payload: resolved})
		<-w.patched
		receipt, _ := json.Marshal(QuestionReceipt{OperationID: cmd.OperationID, RequestID: cmd.RequestID, Version: cmd.Version + 1, Accepted: true})
		w.proc.processEnvelope(&HeadlessEnvelope{Type: "question_receipt", Payload: receipt})
	}()
	return len(data), nil
}

func TestFeishuIngressQuestionClosureBeforeReceiptKeepsAuthoritativeCard(t *testing.T) {
	var mu sync.Mutex
	var cards []string
	var texts int
	patched := make(chan struct{})
	var once sync.Once
	r, p, msg := migrationFeishuRouter(t, func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		if req.Method == http.MethodPatch {
			var body map[string]string
			_ = json.NewDecoder(req.Body).Decode(&body)
			cards = append(cards, body["content"])
		} else {
			texts++
		}
		mu.Unlock()
		fmt.Fprint(w, `{"code":0,"data":{"message_id":"message"}}`)
		once.Do(func() { close(patched) })
	})
	p.mu.Lock()
	p.state.Questions = map[string]*QuestionPayload{"q": {RequestID: "q", Version: 7, Options: []string{"Choice"}, OptionIDs: []string{"option"}}}
	p.stdin = ingressResolutionBeforeReceiptWriter{proc: p, patched: patched}
	p.mu.Unlock()
	msg.InternalAction.Version = 7
	r.recordCardHandle(p.key, "question", "q", &im.InteractiveCardHandle{MessageID: "card"})
	_, target := r.CaptureIngress(msg)
	result := r.DispatchAccepted(t.Context(), msg, target)
	if result.State != "completed" {
		t.Fatal(result)
	}
	// A closure is not proof that a separate unconfirmed operation succeeded.
	// Keep the authoritative card and deliver that diagnostic separately.
	r.enqueueIngressDispatchDiagnostic(msg, DispatchResult{"unknown", "question_outcome_unknown"})
	flushIngressPresentation(t, r, msg)
	mu.Lock()
	defer mu.Unlock()
	if len(cards) != 1 || texts != 1 || !strings.Contains(cards[0], "Choice") || strings.Contains(cards[0], "Question updated") || strings.Contains(cards[0], "Outcome unconfirmed") {
		t.Fatal(cards, texts)
	}
}

func TestFeishuIngressPresentationDoesNotAdoptReplacementRuntime(t *testing.T) {
	requests := 0
	var mu sync.Mutex
	r, p, msg := migrationFeishuRouter(t, func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		fmt.Fprint(w, `{"code":0,"data":{"message_id":"message"}}`)
	})
	// The original text control finished writing just before the process was
	// replaced. Its output must not take the newer runtime's same-ID handle.
	msg.InternalAction = nil
	p.mu.Lock()
	p.bindingInstanceID = "replacement"
	p.state.BindingInstanceID = "replacement"
	p.mu.Unlock()
	r.recordCardHandle(p.key, "confirm", "confirm", &im.InteractiveCardHandle{MessageID: "new-runtime-card"})
	r.enqueueIngressInteractionPresentation(msg, "confirm", "confirm", "Approval submitted", "Old runtime submission.", "grey", false, "binding")
	flushIngressPresentation(t, r, msg)
	mu.Lock()
	defer mu.Unlock()
	if requests != 0 {
		t.Fatal("old result used a newer runtime", requests)
	}
	r.mu.Lock()
	entry, ok := r.cardHandles[cardHandleKey(p.key, "confirm", "confirm")]
	r.mu.Unlock()
	if !ok || entry.handle.MessageID != "new-runtime-card" {
		t.Fatal(entry, ok)
	}
}

func TestFeishuIngressQuestionReceiptBeforeClosureRetainsOriginalHandle(t *testing.T) {
	var mu sync.Mutex
	var cards []string
	var texts int
	r, p, msg := migrationFeishuRouter(t, func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if req.Method == http.MethodPatch {
			var body map[string]string
			_ = json.NewDecoder(req.Body).Decode(&body)
			cards = append(cards, body["content"])
		} else {
			texts++
		}
		fmt.Fprint(w, `{"code":0,"data":{"message_id":"message"}}`)
	})
	writer := &ingressInteractionWriter{proc: p, receipts: true}
	p.mu.Lock()
	p.state.Questions = map[string]*QuestionPayload{"q": {RequestID: "q", Version: 7, Options: []string{"Choice"}, OptionIDs: []string{"option"}}}
	p.stdin = writer
	p.mu.Unlock()
	msg.InternalAction.Version = 7
	r.recordCardHandle(p.key, "question", "q", &im.InteractiveCardHandle{MessageID: "card"})
	_, target := r.CaptureIngress(msg)
	if result := r.DispatchAccepted(t.Context(), msg, target); result.State != "completed" {
		t.Fatal(result)
	}
	flushIngressPresentation(t, r, msg)
	resolved, _ := json.Marshal(QuestionPayload{RequestID: "q", Version: 8, Outcome: QuestionResolvedAnswered, Selected: []string{"Choice"}})
	p.processEnvelope(&HeadlessEnvelope{Type: "question_resolved", Payload: resolved})
	flushIngressPresentation(t, r, msg)
	mu.Lock()
	defer mu.Unlock()
	if len(cards) != 2 || texts != 0 || !strings.Contains(cards[0], "Question updated") || !strings.Contains(cards[1], "Choice") {
		t.Fatal(cards, texts)
	}
}

func TestFeishuIngressTrustedClosureUnknownPatchDoesNotDuplicateText(t *testing.T) {
	var mu sync.Mutex
	var patches, texts int
	r, p, msg := migrationFeishuRouter(t, func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if req.Method == http.MethodPatch {
			patches++
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"code":230099,"msg":"unconfirmed"}`)
		} else {
			texts++
			fmt.Fprint(w, `{"code":0,"data":{"message_id":"message"}}`)
		}
	})
	r.recordCardHandle(p.key, "question", "q", &im.InteractiveCardHandle{MessageID: "card"})
	state := p.State()
	state.LastQuestionResolved = &QuestionPayload{RequestID: "q", Version: 8, Outcome: QuestionResolvedAnswered, Selected: []string{"Choice"}}
	r.HandleChordEvent(p.key, "question_resolved", state)
	flushIngressPresentation(t, r, msg)
	mu.Lock()
	defer mu.Unlock()
	if patches != 1 || texts != 0 {
		t.Fatal(patches, texts)
	}
	r.mu.Lock()
	_, retained := r.cardHandles[cardHandleKey(p.key, "question", "q")]
	r.mu.Unlock()
	if !retained {
		t.Fatal("unknown trusted result discarded reconciliation handle")
	}
}
