package main

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkcallback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

func TestFeishuIngressNamespacesDeliveryAndSourceEvidence(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "app", Owner: &config.OwnerIdentity{AccountID: "app", UserID: "owner"}})
	text := makeFeishuMessageEvent("owner", "chat", "same", "task")
	source := time.Now().Add(-time.Second).Truncate(time.Millisecond)
	text.Event.Message.CreateTime = stringPtr(timeToMillis(source))
	for _, delivery := range []string{"delivery-one", "delivery-two"} {
		future, err := a.submitMessageEvent(t.Context(), text, delivery)
		if err != nil {
			t.Fatal(err)
		}
		result := im.WaitIngressNormalization(t.Context(), future)
		want := im.ReceiveAccepted
		if delivery == "delivery-two" {
			want = im.ReceiveDuplicate
		}
		if result.Disposition != want {
			t.Fatal(result)
		}
	}
	card := makeFeishuCardActionEvent("owner", "chat", "request", "allow")
	card.EventV2Base.Header.EventID = "same"
	future, immediate, err := a.submitCardActionEvent(t.Context(), card, "delivery-three")
	if err != nil || immediate != nil {
		t.Fatal(immediate, err)
	}
	if result := im.WaitIngressNormalization(t.Context(), future); result.Disposition != im.ReceiveAccepted {
		t.Fatal(result)
	}
	raw, _ := feishuTestReceivers.Load(a)
	receiver := raw.(*testFeishuReceiver)
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	if len(receiver.envelopes) != 3 {
		t.Fatal(len(receiver.envelopes))
	}
	first, second, third := receiver.envelopes[0], receiver.envelopes[1], receiver.envelopes[2]
	if first.Message.MessageID != "message:same" || second.Message.MessageID != first.Message.MessageID || third.Message.MessageID != "card:same" {
		t.Fatal(first.Message.MessageID, second.Message.MessageID, third.Message.MessageID)
	}
	if first.DeliveryID == second.DeliveryID || !first.SourceTime.Equal(source) || !third.SourceTime.IsZero() {
		t.Fatal(first, second, third)
	}
	if third.Message.ChatKind != "" || first.Message.ChatKind != "private" || first.Message.SenderID != "owner" {
		t.Fatal("invented platform identity")
	}
}

func timeToMillis(value time.Time) string { return strconv.FormatInt(value.UnixMilli(), 10) }

func TestFeishuCardMissingChatTypeKeepsAuthorizationStrict(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "app", Owner: &config.OwnerIdentity{AccountID: "app", UserID: "owner"}, ChatBindings: map[string]string{"chat": "test"}})
	r := a.msgRouter.(*NotificationRouter)
	valid := im.IncomingMessage{IMType: "feishu", AccountID: "app", ChatID: "chat", SenderID: "owner", MessageID: "card:event", InternalAction: &im.InternalAction{Type: "confirm", Action: "allow", RequestID: "request", BindingInstanceID: "binding"}}
	if result := r.admission.AuthorizeClassified(valid); result.Disposition != im.AdmissionAllowed {
		t.Fatal(result)
	}
	for _, tc := range []struct {
		name   string
		change func(*im.IncomingMessage)
	}{
		{"text", func(m *im.IncomingMessage) { m.InternalAction = nil; m.Text = "task" }},
		{"other platform", func(m *im.IncomingMessage) { m.IMType = "wechat" }},
		{"wrong owner", func(m *im.IncomingMessage) { m.SenderID = "stranger" }},
		{"missing binding instance", func(m *im.IncomingMessage) {
			action := *m.InternalAction
			action.BindingInstanceID = ""
			m.InternalAction = &action
		}},
		{"invalid action", func(m *im.IncomingMessage) {
			action := *m.InternalAction
			action.Action = "switch"
			m.InternalAction = &action
		}},
		{"unbound chat", func(m *im.IncomingMessage) { m.ChatID = "elsewhere" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := valid
			tc.change(&m)
			if result := r.admission.AuthorizeClassified(m); result.Disposition != im.AdmissionRejected {
				t.Fatal(result)
			}
		})
	}
	fc := &config.FeishuConfig{AppID: "unpaired"}
	unpaired := testFeishuAdapter(t, fc)
	valid.AccountID = "unpaired"
	valid.Text = "/bind /tmp fake"
	valid.ChatKind = "private"
	if _, ok := unpaired.msgRouter.(*NotificationRouter).admission.Authorize(valid); ok {
		t.Fatal("card paired an owner")
	}
}

func TestFeishuCardCapturePreservesOriginalQuestionVersion(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "app", Owner: &config.OwnerIdentity{AccountID: "app", UserID: "owner"}, ChatBindings: map[string]string{"chat": "test"}})
	r := a.msgRouter.(*NotificationRouter)
	key := (processKey{workspaceID: "test", imType: "feishu", chatID: "chat"}).String()
	p := &ChordProcess{runtimeID: "runtime-one", sessionEpoch: 1, bindingInstanceID: "binding", state: ControlState{SessionID: "session", Questions: map[string]*QuestionPayload{"q": {RequestID: "q", Version: 3}}}}
	r.mgr.mu.Lock()
	r.mgr.procs[key] = p
	r.mgr.mu.Unlock()
	for _, version := range []any{nil, float64(2), "3"} {
		card := makeFeishuQuestionFormEvent("owner", "chat", "q", map[string]interface{}{"option_1": true})
		card.Event.Action.Value["version"] = version
		future, _, err := a.submitCardActionEvent(context.Background(), card, "delivery")
		if err != nil {
			t.Fatal(err)
		}
		_ = im.WaitIngressNormalization(t.Context(), future)
	}
	raw, _ := feishuTestReceivers.Load(a)
	receiver := raw.(*testFeishuReceiver)
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	for i, want := range []uint64{0, 2, 3} {
		target := receiver.envelopes[i].Capture.Target
		if target == nil || target.Version != want || target.RuntimeID != "runtime-one" {
			t.Fatalf("target=%+v want version %d", target, want)
		}
	}
}

func TestFeishuCardAcceptanceNeverOverwritesTerminalCard(t *testing.T) {
	event := makeFeishuCardActionEvent("owner", "chat", "request", "allow")
	for _, result := range []im.ReceiveResult{{Disposition: im.ReceiveAccepted}, {Disposition: im.ReceiveDuplicate, State: "submitted"}, {Disposition: im.ReceiveDuplicate, State: "unknown"}, {Disposition: im.ReceiveDuplicate, State: "rejected"}, {Disposition: im.ReceiveRetryable}, {Disposition: im.ReceiveUncertain}} {
		response := feishuCardIngressResponse(event, result)
		if response == nil || response.Toast == nil || response.Card != nil {
			t.Fatalf("result=%+v response=%+v", result, response)
		}
		if (result.Disposition == im.ReceiveUncertain || result.Disposition == im.ReceiveRetryable) && feishuMessageIngressError(result) == nil {
			t.Fatal("uncommitted result acknowledged")
		}
		if result.Disposition == im.ReceiveUncertain && strings.Contains(response.Toast.Content, "try again") {
			t.Fatal("uncertain interaction encouraged repeat")
		}
	}
}

func TestFeishuSourceTimeAndProtocolIgnore(t *testing.T) {
	for _, raw := range []string{"", "broken", "0", "-1"} {
		if !feishuSourceTime(raw).IsZero() {
			t.Fatal("invented time", raw)
		}
	}
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "app", Owner: &config.OwnerIdentity{AccountID: "app", UserID: "owner"}})
	for _, kind := range []string{"app", "bot"} {
		event := makeFeishuMessageEvent("owner", "chat", "id", "task")
		event.Event.Sender.SenderType = stringPtr(kind)
		future, err := a.submitMessageEvent(t.Context(), event, "delivery")
		if future != nil || err != nil {
			t.Fatal("robot event accepted")
		}
	}
	event := makeFeishuMessageEvent("owner", "chat", "id", "task")
	event.EventV2Base = &larkevent.EventV2Base{Header: &larkevent.EventHeader{AppID: "wrong-app"}}
	if future, err := a.submitMessageEvent(t.Context(), event, "delivery"); future != nil || err != nil {
		t.Fatal("wrong app accepted")
	}
}

func TestFeishuIngressRejectsChangedActiveAccount(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "app", Owner: &config.OwnerIdentity{AccountID: "app", UserID: "owner"}})
	a.mu.Lock()
	a.activeAccount = "original-account"
	a.mu.Unlock()
	text := makeFeishuMessageEvent("owner", "chat", "id", "task")
	if future, err := a.submitMessageEvent(t.Context(), text, "delivery"); future != nil || err != nil {
		t.Fatal("changed account text accepted")
	}
	card := makeFeishuCardActionEvent("owner", "chat", "request", "allow")
	if future, response, err := a.submitCardActionEvent(t.Context(), card, "delivery"); future != nil || response != nil || err != nil {
		t.Fatal("changed account card accepted")
	}
}

func TestFeishuIngressStopsAcceptingAfterDisconnect(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "app", Owner: &config.OwnerIdentity{AccountID: "app", UserID: "owner"}})
	a.Disconnect()
	if future, err := a.submitMessageEvent(t.Context(), makeFeishuMessageEvent("owner", "chat", "id", "task"), "delivery"); future != nil || err != nil {
		t.Fatal("disconnected adapter submitted text")
	}
	if future, response, err := a.submitCardActionEvent(t.Context(), makeFeishuCardActionEvent("owner", "chat", "req", "allow"), "delivery"); future != nil || response != nil || err != nil {
		t.Fatal("disconnected adapter submitted card")
	}
	raw, _ := feishuTestReceivers.Load(a)
	receiver := raw.(*testFeishuReceiver)
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	if len(receiver.envelopes) != 0 {
		t.Fatal("disconnected adapter reached public receiver")
	}
}

func TestFeishuUnauthorizedDuplicateCallbackDoesNotExposeStoredOutcome(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "app", Owner: &config.OwnerIdentity{AccountID: "app", UserID: "owner"}, ChatBindings: map[string]string{"chat": "test"}})
	r := a.msgRouter.(*NotificationRouter)
	store := r.ingressStore
	original := store.AcceptRecord(t.Context(), IngressRecord{Platform: "feishu", AccountID: "app", ChatID: "chat", SenderID: "owner", BusinessID: "card:shared-event", Kind: "control", State: "received"}, nil)
	if original.Disposition != im.ReceiveAccepted {
		t.Fatal(original)
	}
	if err := store.UpdateRecord(t.Context(), original.RecordID, []im.IngressState{"received"}, func(record *IngressRecord) error { record.State = "consumed"; return nil }); err != nil {
		t.Fatal(err)
	}
	// The common duplicate lookup precedes the rejected admission transaction.
	// A stranger's verified callback therefore retrieves the original record.
	message := im.IncomingMessage{IMType: "feishu", AccountID: "app", ChatID: "chat", SenderID: "stranger", MessageID: "card:shared-event", InternalAction: &im.InternalAction{Type: "confirm", Action: "allow", RequestID: "request", BindingInstanceID: "binding"}}
	result := r.ingress.Accept(t.Context(), im.IngressEnvelope{Message: message})
	if result.Disposition != im.ReceiveDuplicate || result.State != "consumed" {
		t.Fatal("duplicate lookup behavior changed", result)
	}
	event := makeFeishuCardActionEvent("stranger", "chat", "request", "allow")
	event.EventV2Base.Header.EventID = "shared-event"
	response := a.feishuCardIngressResponse(event, result)
	if response.Toast.Content != "This interaction could not be accepted." || response.Card != nil {
		t.Fatal("stored consumption state exposed", response)
	}
	// Exercise the complete SDK callback wrapper against the common receiver.
	r.ingress.normalizer.Close()
	r.ingress.normalizer = NewIngressNormalizer(t.Context(), r.ingress)
	response, err := a.handleCardActionEvent(t.Context(), event)
	if err != nil || response == nil || response.Toast.Content != "This interaction could not be accepted." {
		t.Fatal(response, err)
	}
	stored, err := store.Get(original.RecordID)
	if err != nil || stored.State != "consumed" {
		t.Fatal("unauthorized callback altered original operation", stored, err)
	}
}

func TestFeishuCardOutcomePresentationChecksCurrentOwnerAppAndChat(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "app", Owner: &config.OwnerIdentity{AccountID: "app", UserID: "owner"}, ChatBindings: map[string]string{"chat": "test"}})
	for _, result := range []im.ReceiveResult{{Disposition: im.ReceiveDuplicate, State: "consumed"}, {Disposition: im.ReceiveDuplicate, State: "unknown"}} {
		owner := makeFeishuCardActionEvent("owner", "chat", "request", "allow")
		if content := a.feishuCardIngressResponse(owner, result).Toast.Content; content == "This interaction could not be accepted." {
			t.Fatal("current owner cannot inspect result")
		}
		for _, change := range []func(){func() { owner.Event.Operator.OpenID = "stranger" }, func() { owner.Event.Context.OpenChatID = "unbound-chat" }, func() { owner.EventV2Base.Header.AppID = "wrong-app" }} {
			owner = makeFeishuCardActionEvent("owner", "chat", "request", "allow")
			change()
			if response := a.feishuCardIngressResponse(owner, result); response.Toast.Content != "This interaction could not be accepted." || response.Card != nil {
				t.Fatal("unauthorized presentation exposed stored state", response)
			}
		}
	}
}

// Synchronous test helpers exercise the production submitters.
func (a *FeishuAdapter) handleMessageEvent(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	ctx, cancel := context.WithTimeout(ctx, im.IngressAcceptBudget)
	defer cancel()
	future, err := a.submitMessageEvent(ctx, event, "")
	if err != nil || future == nil {
		return err
	}
	return feishuMessageIngressError(im.WaitIngressNormalization(ctx, future))
}

func (a *FeishuAdapter) handleCardActionEvent(ctx context.Context, event *larkcallback.CardActionTriggerEvent) (*larkcallback.CardActionTriggerResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, im.IngressAcceptBudget)
	defer cancel()
	future, immediate, err := a.submitCardActionEvent(ctx, event, "")
	if err != nil {
		return feishuCardToast("error", "Not received. Please try again later."), err
	}
	if future == nil {
		return immediate, nil
	}
	result := im.WaitIngressNormalization(ctx, future)
	return a.feishuCardIngressResponse(event, result), feishuMessageIngressError(result)
}
