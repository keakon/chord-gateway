package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	feishu "github.com/keakon/chord-gateway/internal/platform/feishu"

	"gopkg.in/yaml.v3"

	"github.com/gorilla/websocket"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkcallback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

// findFeishuAdapterForTest resolves the package adapter from the router chain.
func findFeishuAdapterForTest(r *NotificationRouter) *feishu.FeishuAdapter {
	a, _ := r.findAdapterByType("feishu").(*feishu.FeishuAdapter)
	return a
}

// testFeishuReceiver observes the adapter contract at the common receiver.
// Durable deduplication itself is verified by the ingress store integration tests.
type testFeishuReceiver struct {
	router     *NotificationRouter
	queue      chan im.IncomingMessage
	dispatched atomic.Int32
	mu         sync.Mutex
	seen       map[string]bool
	envelopes  []im.IngressEnvelope
}

func (r *testFeishuReceiver) Accept(_ context.Context, e im.IngressEnvelope) im.ReceiveResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.envelopes = append(r.envelopes, e)
	if _, ok := r.router.admission.Authorize(e.Message); !ok {
		return im.ReceiveResult{Disposition: im.ReceiveRejected, Reason: "authorization_denied", State: "rejected"}
	}
	if ValidateIngressInteraction(e.Message) != nil {
		return im.ReceiveResult{Disposition: im.ReceiveRejected, Reason: "invalid_interaction", State: "rejected"}
	}
	if r.seen[e.Message.MessageID] {
		return im.ReceiveResult{Disposition: im.ReceiveDuplicate, State: "received"}
	}
	r.seen[e.Message.MessageID] = true
	r.queue <- e.Message
	r.dispatched.Add(1)
	return im.ReceiveResult{Disposition: im.ReceiveAccepted, State: "received"}
}

var feishuTestReceivers sync.Map

func installFeishuTestIngress(t *testing.T, a *feishu.FeishuAdapter, r *NotificationRouter) *testFeishuReceiver {
	t.Helper()
	receiver := &testFeishuReceiver{router: r, queue: make(chan im.IncomingMessage, 128), seen: make(map[string]bool)}
	service := NewIngressService(testIngressStore(t), r)
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	service.normalizer.Close()
	service.normalizer = NewIngressNormalizer(context.Background(), receiver)
	initialNormalizer := service.normalizer
	feishuTestReceivers.Store(a, receiver)
	t.Cleanup(func() { initialNormalizer.Close(); service.Close(); feishuTestReceivers.Delete(a) })
	return receiver
}
func testFeishuAdapter(t *testing.T, fc *config.FeishuConfig) *feishu.FeishuAdapter {
	t.Helper()
	if fc != nil && fc.AppID == "" {
		fc.AppID = "cli_test"
	}
	cfg := &config.Config{IMs: []config.IMAdapterConfig{{Feishu: fc}}, Workspaces: []config.Workspace{{ID: "test", Path: "/tmp/test"}}}
	router := &NotificationRouter{mgr: newTestChordManager(cfg), lastKeyChatID: make(map[string]string)}
	a := &feishu.FeishuAdapter{}
	a.WireForTest(cfg.IMs[0], &http.Client{Timeout: 10 * time.Second}, router)
	if fc != nil {
		store := &ConfigStore{}
		store.snapshot.Store(cfg)
		router.admission = NewAdmission(NewPairingService(store, false, nil))
		router.mgr.store = store
		if err := router.admission.pairing.PlatformReady("feishu", fc.AppID); err != nil {
			t.Fatal(err)
		}
	}
	installFeishuTestIngress(t, a, router)
	return a
}
func testFeishuAdapterWithQueue(t *testing.T, fc *config.FeishuConfig) (*feishu.FeishuAdapter, *atomic.Int32, context.CancelFunc) {
	t.Helper()
	a := testFeishuAdapter(t, fc)
	raw, _ := feishuTestReceivers.Load(a)
	receiver := raw.(*testFeishuReceiver)
	return a, &receiver.dispatched, func() {}
}
func feishuTestQueue(a *feishu.FeishuAdapter, _ im.IncomingMessage) chan im.IncomingMessage {
	raw, _ := feishuTestReceivers.Load(a)
	return raw.(*testFeishuReceiver).queue
}

func makeFeishuMessageEvent(openID, chatID, messageID, text string) *larkim.P2MessageReceiveV1 {
	content := fmt.Sprintf(`{"text":"%s"}`, text)
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderType: stringPtr("user"),
				SenderId:   &larkim.UserId{OpenId: stringPtr(openID)},
			},
			Message: &larkim.EventMessage{
				ChatType:    stringPtr("p2p"),
				ChatId:      stringPtr(chatID),
				MessageId:   stringPtr(messageID),
				MessageType: stringPtr("text"),
				Content:     stringPtr(content),
			},
		},
	}
}

func makeFeishuNonTextMessageEvent(openID, chatID, messageID string) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderType: stringPtr("user"),
				SenderId:   &larkim.UserId{OpenId: stringPtr(openID)},
			},
			Message: &larkim.EventMessage{
				ChatType:    stringPtr("p2p"),
				ChatId:      stringPtr(chatID),
				MessageId:   stringPtr(messageID),
				MessageType: stringPtr("image"),
				Content:     stringPtr("{}"),
			},
		},
	}
}

func makeFeishuCardActionEvent(openID, chatID, requestID, action string) *larkcallback.CardActionTriggerEvent {
	return &larkcallback.CardActionTriggerEvent{
		EventV2Base: &larkevent.EventV2Base{Header: &larkevent.EventHeader{EventID: "event-" + rand.Text()}},
		Event: &larkcallback.CardActionTriggerRequest{
			Operator: &larkcallback.Operator{OpenID: openID},
			Action: &larkcallback.CallBackAction{
				Tag: "button",
				Value: map[string]interface{}{
					"type":                "confirm",
					"action":              action,
					"request_id":          requestID,
					"chat_id":             chatID,
					"im_type":             "feishu",
					"binding_instance_id": "binding-test",
				},
			},
			Context: &larkcallback.Context{OpenChatID: chatID, OpenMessageID: "om_" + requestID},
			Token:   "token_" + requestID,
		},
	}
}

func stringPtr(s string) *string { return &s }

func TestFeishuMessageEvent_EnqueuesAndDispatches(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", Owner: &config.OwnerIdentity{AccountID: "cli_test", UserID: "ou_owner"}}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer cancel()

	err := handleMessageEvent(context.Background(), a, makeFeishuMessageEvent("ou_owner", "oc_chat1", "msg_001", "hello"))
	if err != nil {
		t.Fatalf("handleMessageEvent() error = %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 1 {
		t.Fatalf("expected 1 dispatch, got %d", dispatched.Load())
	}
}

func TestFeishuMessageEvent_DuplicateDedup(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", Owner: &config.OwnerIdentity{AccountID: "cli_test", UserID: "ou_owner"}}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer cancel()

	event := makeFeishuMessageEvent("ou_owner", "oc_chat1", "msg_dup", "hello")
	if err := handleMessageEvent(context.Background(), a, event); err != nil {
		t.Fatalf("first handleMessageEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := handleMessageEvent(context.Background(), a, event); err != nil {
		t.Fatalf("second handleMessageEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	if count := dispatched.Load(); count != 1 {
		t.Fatalf("expected 1 dispatch, got %d", count)
	}
}

func TestFeishuMessageEvent_OwnerFilter(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", Owner: &config.OwnerIdentity{AccountID: "cli_test", UserID: "ou_owner"}}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer cancel()

	if err := handleMessageEvent(context.Background(), a, makeFeishuMessageEvent("ou_not_owner", "oc_chat1", "msg_001", "hello")); err != nil {
		t.Fatalf("non-owner handleMessageEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 0 {
		t.Fatal("non-owner message should not be dispatched")
	}

	if err := handleMessageEvent(context.Background(), a, makeFeishuMessageEvent("ou_owner", "oc_chat1", "msg_002", "hello")); err != nil {
		t.Fatalf("owner handleMessageEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 1 {
		t.Fatalf("owner message should be dispatched, got %d", dispatched.Load())
	}
}

func TestFeishuMessageEvent_DeniesWhenNotConfigured(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret"}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer cancel()

	if err := handleMessageEvent(context.Background(), a, makeFeishuMessageEvent("ou_anyone", "oc_chat1", "msg_001", "hello")); err != nil {
		t.Fatalf("handleMessageEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 0 {
		t.Fatalf("empty allowlist should deny all users, got %d", dispatched.Load())
	}
}

func TestFeishuMessageEvent_IgnoresNonTextMessages(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret"}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer cancel()

	if err := handleMessageEvent(context.Background(), a, makeFeishuNonTextMessageEvent("ou_user", "oc_chat1", "msg_001")); err != nil {
		t.Fatalf("handleMessageEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 0 {
		t.Fatal("non-text message should not be dispatched")
	}
}

func TestFeishuCardActionEvent_DispatchesOnce(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", Owner: &config.OwnerIdentity{AccountID: "cli_test", UserID: "ou_owner"}}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer cancel()

	event := makeFeishuCardActionEvent("ou_owner", "oc_chat1", "req_1", "allow")
	for i := 0; i < 2; i++ {
		if _, err := handleCardActionEvent(context.Background(), a, event); err != nil {
			t.Fatalf("handleCardActionEvent() error = %v", err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 1 {
		t.Fatalf("expected 1 dispatched card action, got %d", dispatched.Load())
	}
}

func TestFeishuCardActionEvent_WrongContextIgnored(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", Owner: &config.OwnerIdentity{AccountID: "cli_test", UserID: "ou_owner"}}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer cancel()

	event := makeFeishuCardActionEvent("ou_owner", "oc_chat1", "req_2", "allow")
	event.Event.Action.Value["chat_id"] = "oc_other"
	if _, err := handleCardActionEvent(context.Background(), a, event); err != nil {
		t.Fatalf("handleCardActionEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 0 {
		t.Fatal("wrong-context card action should not be dispatched")
	}
}

func TestFeishuCardActionEvent_InvalidActionIgnored(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", Owner: &config.OwnerIdentity{AccountID: "cli_test", UserID: "ou_owner"}}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer cancel()

	tests := []struct {
		name   string
		mutate func(*larkcallback.CardActionTriggerEvent)
	}{
		{name: "unknown confirm action", mutate: func(e *larkcallback.CardActionTriggerEvent) { e.Event.Action.Value["action"] = "approve_forever" }},
		{name: "unknown type", mutate: func(e *larkcallback.CardActionTriggerEvent) { e.Event.Action.Value["type"] = "admin" }},
		{name: "question wrong action", mutate: func(e *larkcallback.CardActionTriggerEvent) {
			e.Event.Action.Value["type"] = "question"
			e.Event.Action.Value["action"] = "allow"
			e.Event.Action.Value["value"] = "yes"
		}},
		{name: "question empty value", mutate: func(e *larkcallback.CardActionTriggerEvent) {
			e.Event.Action.Value["type"] = "question"
			e.Event.Action.Value["action"] = "answer"
			e.Event.Action.Value["value"] = ""
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := makeFeishuCardActionEvent("ou_owner", "oc_chat1", "req_invalid_"+strings.ReplaceAll(tt.name, " ", "_"), "allow")
			tt.mutate(event)
			if resp, err := handleCardActionEvent(context.Background(), a, event); err != nil || resp != nil {
				t.Fatalf("handleCardActionEvent() resp=%#v err=%v", resp, err)
			}
		})
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 0 {
		t.Fatalf("invalid card actions should not be dispatched, got %d", dispatched.Load())
	}
}
func makeFeishuQuestionFormEvent(openID, chatID, requestID string, formValue map[string]interface{}) *larkcallback.CardActionTriggerEvent {
	event := makeFeishuCardActionEvent(openID, chatID, requestID, "answer")
	event.Event.Action.Value["type"] = "question"
	event.Event.Action.Name = feishu.FeishuQuestionSubmitName
	event.Event.Action.FormValue = formValue
	return event
}

func TestFeishuCardActionEvent_FormSubmitCarriesCheckedOptions(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", Owner: &config.OwnerIdentity{AccountID: "cli_test", UserID: "ou_owner"}}
	a := testFeishuAdapter(t, fc)

	event := makeFeishuQuestionFormEvent("ou_owner", "oc_chat1", "req_q", map[string]interface{}{
		"option_2": true,
		"option_3": false,
		"option_1": true,
	})
	if _, err := handleCardActionEvent(context.Background(), a, event); err != nil {
		t.Fatalf("handleCardActionEvent() error = %v", err)
	}
	queued := im.IncomingMessage{AppID: fc.AppID, ChatID: "oc_chat1"}
	select {
	case msg := <-feishuTestQueue(a, queued):
		action := msg.InternalAction
		if action == nil || action.Type != "question" || action.Action != "answer" || action.RequestID != "req_q" {
			t.Fatalf("im.InternalAction = %#v", action)
		}
		if got := strings.Join(action.Values, ","); got != "1,2" {
			t.Fatalf("im.InternalAction.Values = %v, want option order 1,2", action.Values)
		}
		if msg.MessageID != "card:"+event.EventV2Base.Header.EventID {
			t.Fatalf("MessageID = %q, want callback event ID", msg.MessageID)
		}
	default:
		t.Fatal("expected the form submission to be enqueued")
	}
}

func TestFeishuCardActionEvent_DistinctFormSelectionsAreNotDuplicates(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", Owner: &config.OwnerIdentity{AccountID: "cli_test", UserID: "ou_owner"}}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer cancel()

	first := makeFeishuQuestionFormEvent("ou_owner", "oc_chat1", "req_q", map[string]interface{}{"option_1": true})
	if _, err := handleCardActionEvent(context.Background(), a, first); err != nil {
		t.Fatalf("handleCardActionEvent() error = %v", err)
	}
	// Answering the same request with a different selection is a new answer,
	// not a retry of the first callback.
	second := makeFeishuQuestionFormEvent("ou_owner", "oc_chat1", "req_q", map[string]interface{}{"option_2": true})
	if _, err := handleCardActionEvent(context.Background(), a, second); err != nil {
		t.Fatalf("handleCardActionEvent() error = %v", err)
	}
	// Retrying the first submission is still deduped.
	if _, err := handleCardActionEvent(context.Background(), a, first); err != nil {
		t.Fatalf("handleCardActionEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := dispatched.Load(); got != 2 {
		t.Fatalf("dispatched = %d, want the two distinct selections", got)
	}
}

func TestFeishuCardActionEvent_SameAnswerNewClickCanRetry(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", Owner: &config.OwnerIdentity{AccountID: "cli_test", UserID: "ou_owner"}}
	a := testFeishuAdapter(t, fc)
	queue := feishuTestQueue(a, im.IncomingMessage{})
	first := makeFeishuQuestionFormEvent("ou_owner", "oc_chat1", "q-1", map[string]interface{}{"option_1": true})
	second := makeFeishuQuestionFormEvent("ou_owner", "oc_chat1", "q-1", map[string]interface{}{"option_1": true})
	for _, event := range []*larkcallback.CardActionTriggerEvent{first, first, second} {
		if _, err := handleCardActionEvent(context.Background(), a, event); err != nil {
			t.Fatal(err)
		}
	}
	if len(queue) != 2 {
		t.Fatalf("queued %d events, want two deliberate clicks", len(queue))
	}
	if a, b := <-queue, <-queue; a.MessageID == b.MessageID || strings.Join(a.InternalAction.Values, ",") != strings.Join(b.InternalAction.Values, ",") {
		t.Fatal("identical answers did not keep distinct event identities")
	}
}

func TestFeishuCardActionEvent_MissingEventIDCannotSubmit(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", Owner: &config.OwnerIdentity{AccountID: "cli_test", UserID: "ou_owner"}}
	a := testFeishuAdapter(t, fc)
	event := makeFeishuCardActionEvent("ou_owner", "oc_chat1", "q-1", "answer")
	event.Event.Action.Value["type"] = "question"
	event.Event.Action.Value["value"] = "first"
	event.EventV2Base.Header.EventID = ""
	response, err := handleCardActionEvent(context.Background(), a, event)
	if err != nil || response == nil || response.Toast == nil || response.Toast.Type != "error" {
		t.Fatalf("missing ID did not report an error: response=%+v err=%v", response, err)
	}
	for _, queue := range []chan im.IncomingMessage{feishuTestQueue(a, im.IncomingMessage{})} {
		if len(queue) != 0 {
			t.Fatal("unidentified callback was submitted")
		}
	}
}

func TestFeishuCardActionEvent_EmptyFormSubmitToasts(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", Owner: &config.OwnerIdentity{AccountID: "cli_test", UserID: "ou_owner"}}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer cancel()

	resp, err := handleCardActionEvent(context.Background(), a, makeFeishuQuestionFormEvent("ou_owner", "oc_chat1", "req_q", nil))
	if err != nil {
		t.Fatalf("handleCardActionEvent() error = %v", err)
	}
	if resp == nil || resp.Toast == nil || !strings.Contains(resp.Toast.Content, "Select at least one option") {
		t.Fatalf("response = %#v, want a selection hint toast", resp)
	}
	time.Sleep(50 * time.Millisecond)
	if got := dispatched.Load(); got != 0 {
		t.Fatalf("an empty selection must not be submitted, dispatched = %d", got)
	}
}

func TestFeishuMessageEvent_InvalidJSONIgnored(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret"}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer cancel()

	event := makeFeishuMessageEvent("ou_user", "oc_chat1", "msg_001", "hello")
	event.Event.Message.Content = stringPtr("{invalid")
	if err := handleMessageEvent(context.Background(), a, event); err != nil {
		t.Fatalf("handleMessageEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 0 {
		t.Fatal("invalid JSON content should not be dispatched")
	}
}

func TestFeishuCardActionEvent_UsesEventIDAsMessageID(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", Owner: &config.OwnerIdentity{AccountID: "cli_test", UserID: "ou_owner"}}
	a := testFeishuAdapter(t, fc)

	event := makeFeishuCardActionEvent("ou_owner", "oc_chat1", "req_9", "allow")
	if _, err := handleCardActionEvent(context.Background(), a, event); err != nil {
		t.Fatalf("handleCardActionEvent() error = %v", err)
	}
	queued := im.IncomingMessage{AppID: fc.AppID, ChatID: "oc_chat1"}
	select {
	case msg := <-feishuTestQueue(a, queued):
		if msg.MessageID != "card:"+event.EventV2Base.Header.EventID {
			t.Fatalf("MessageID = %q", msg.MessageID)
		}
		if msg.InternalAction == nil || msg.InternalAction.Type != "confirm" || msg.InternalAction.Action != "allow" || msg.InternalAction.RequestID != "req_9" {
			t.Fatalf("im.InternalAction = %#v", msg.InternalAction)
		}
		if msg.InternalAction.Handle.MessageID != "om_req_9" || msg.InternalAction.Handle.Token != "token_req_9" {
			t.Fatalf("im.InternalAction.Handle = %#v", msg.InternalAction.Handle)
		}
	default:
		t.Fatal("expected card action to be enqueued")
	}
}

func TestFeishuMessageEvent_ContentMatchesText(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", Owner: &config.OwnerIdentity{AccountID: "cli_test", UserID: "ou_user"}}
	a := testFeishuAdapter(t, fc)

	event := makeFeishuMessageEvent("ou_user", "oc_chat1", "msg_001", "hello world")
	if err := handleMessageEvent(context.Background(), a, event); err != nil {
		t.Fatalf("handleMessageEvent() error = %v", err)
	}
	queued := im.IncomingMessage{AppID: fc.AppID, ChatID: "oc_chat1"}
	select {
	case msg := <-feishuTestQueue(a, queued):
		if msg.Text != "hello world" {
			t.Fatalf("Text = %q", msg.Text)
		}
	default:
		t.Fatal("expected message to be enqueued")
	}
}

func TestFeishuMessageEvent_PostContentDispatchesPlainText(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", Owner: &config.OwnerIdentity{AccountID: "cli_test", UserID: "ou_user"}}
	a := testFeishuAdapter(t, fc)

	event := makeFeishuMessageEvent("ou_user", "oc_chat1", "msg_post", "")
	*event.Event.Message.MessageType = "post"
	*event.Event.Message.Content = `{"content":[[{"tag":"text","text":"/deny "},{"tag":"text","text":"not safe"}]]}`
	if err := handleMessageEvent(context.Background(), a, event); err != nil {
		t.Fatalf("handleMessageEvent() error = %v", err)
	}
	queued := im.IncomingMessage{AppID: fc.AppID, ChatID: "oc_chat1"}
	select {
	case msg := <-feishuTestQueue(a, queued):
		if msg.Text != "/deny not safe" {
			t.Fatalf("Text = %q", msg.Text)
		}
	default:
		t.Fatal("expected post message to be enqueued")
	}
}

func TestFeishuMessageEvent_MessageContentEncodingMatchesFeishu(t *testing.T) {
	content := feishu.FeishuMessageContent{Text: "hello"}
	bs, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if string(bs) != `{"text":"hello"}` {
		t.Fatalf("content JSON = %s", string(bs))
	}
}

func TestFeishuSendInteractiveMessageParsesMessageID(t *testing.T) {
	var sawSend bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"token","expire":7200}`))
		case "/open-apis/im/v1/messages":
			sawSend = true
			if r.URL.Query().Get("receive_id_type") != "chat_id" {
				t.Fatalf("receive_id_type = %q", r.URL.Query().Get("receive_id_type"))
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_msg_1"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "app", AppSecret: "secret"})
	a.SetHTTPClientForTest(server.Client())
	oldBaseURL := feishu.OpenBaseURL
	feishu.OpenBaseURL = server.URL
	defer func() { feishu.OpenBaseURL = oldBaseURL }()

	handle, err := a.SendMessageContextForTest(t.Context(), "sample-chat", "interactive", map[string]any{"schema": "2.0"}, false)
	if err != nil {
		t.Fatalf("sendMessageContext() error = %v", err)
	}
	if !sawSend {
		t.Fatal("send endpoint was not called")
	}
	if handle == nil || handle.MessageID != "om_msg_1" {
		t.Fatalf("handle = %#v", handle)
	}
}

func TestFeishuUpdateInteractiveMessagePatchesCardContent(t *testing.T) {
	var sawPatch bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"token","expire":7200}`))
		case "/open-apis/im/v1/messages/om_msg_1":
			sawPatch = true
			if r.Method != http.MethodPatch {
				t.Fatalf("method = %s", r.Method)
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			if _, ok := body["msg_type"]; ok {
				t.Fatalf("message card patch body must not include msg_type: %#v", body)
			}
			if !strings.Contains(body["content"], `"update_multi":true`) || !strings.Contains(body["content"], `"schema":"2.0"`) {
				t.Fatalf("unexpected card content: %s", body["content"])
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "app", AppSecret: "secret"})
	a.SetHTTPClientForTest(server.Client())
	oldBaseURL := feishu.OpenBaseURL
	feishu.OpenBaseURL = server.URL
	defer func() { feishu.OpenBaseURL = oldBaseURL }()

	if err := a.UpdateInteractiveMessageContextForTest(t.Context(), "", map[string]any{}, false); err == nil {
		t.Fatal("card update without message_id was accepted")
	}
	if err := a.UpdateInteractiveMessageContextForTest(t.Context(), "om_msg_1", buildFeishuResolvedCard("Done", "Done", "green"), false); err != nil {
		t.Fatalf("updateInteractiveMessageContext() error = %v", err)
	}
	if !sawPatch {
		t.Fatal("patch endpoint was not called")
	}
}

func TestUpdateFeishuCardStatusPrefersStoredMessageID(t *testing.T) {
	cfg := &config.Config{IMs: []config.IMAdapterConfig{{Feishu: &config.FeishuConfig{AppID: "app", AppSecret: "secret", ChatBindings: map[string]string{"chat-1": "ws1"}}}}, Workspaces: []config.Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}
	paths := &config.Paths{StateDir: t.TempDir()}
	mgr := NewChordManager(cfg, paths)
	fa := testFeishuAdapter(t, &config.FeishuConfig{AppID: "app", AppSecret: "secret", ChatBindings: map[string]string{"chat-1": "ws1"}})
	var patchedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"token","expire":7200}`))
		case "/open-apis/im/v1/messages/om_sent_1":
			patchedPath = r.URL.Path
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
		case "/open-apis/im/v1/messages/om_callback_1":
			t.Fatalf("should not patch using callback open_message_id")
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	fa.SetHTTPClientForTest(server.Client())
	oldBaseURL := feishu.OpenBaseURL
	feishu.OpenBaseURL = server.URL
	defer func() { feishu.OpenBaseURL = oldBaseURL }()
	r := &NotificationRouter{mgr: mgr, adapter: fa, lastKeyChatID: make(map[string]string), expiredPending: make(map[string]expiredPendingState), cardHandles: make(map[string]cardHandleEntry)}
	key := (processKey{workspaceID: "ws1", imType: "feishu", chatID: "chat-1"}).String()
	r.recordCardHandle(key, "confirm", "req-1", &im.InteractiveCardHandle{MessageID: "om_sent_1"})
	msg := im.IncomingMessage{IMType: "feishu", ChatID: "chat-1", SenderID: "ou_owner", InternalAction: &im.InternalAction{Handle: im.InteractiveCardHandle{MessageID: "om_callback_1", Token: "token_1"}}}
	r.updateFeishuCardStatus(msg, key, "confirm", "req-1", buildFeishuResolvedCard("Done", "✅ Done", "green"))
	if patchedPath != "/open-apis/im/v1/messages/om_sent_1" {
		t.Fatalf("patched path = %q", patchedPath)
	}
}

func TestFeishuCardActionFrameTypeCard_Dispatches(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", Owner: &config.OwnerIdentity{AccountID: "cli_test", UserID: "ou_owner"}}
	a := testFeishuAdapter(t, fc)

	payload := []byte(`{"schema":"2.0","header":{"event_type":"card.action.trigger","event_id":"evt_1"},"event":{"operator":{"open_id":"ou_owner"},"action":{"tag":"button","value":{"type":"confirm","action":"allow","request_id":"req_card","chat_id":"oc_chat1","im_type":"feishu","binding_instance_id":"binding-test"}},"context":{"open_chat_id":"oc_chat1"}}}`)
	frame := larkws.Frame{
		Method: int32(larkws.FrameTypeData),
		Headers: larkws.Headers{
			{Key: larkws.HeaderType, Value: string(larkws.MessageTypeCard)},
			{Key: larkws.HeaderMessageID, Value: "frame_1"},
			{Key: larkws.HeaderSum, Value: "1"},
			{Key: larkws.HeaderSeq, Value: "0"},
		},
		Payload: payload,
	}
	serverConn := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("websocket upgrade: %v", err)
			return
		}
		serverConn <- conn
	}))
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial test websocket: %v", err)
	}
	defer clientConn.Close()
	conn := <-serverConn
	defer conn.Close()

	if err := a.HandleDataFrameForTest(context.Background(), conn, frame); err != nil {
		t.Fatalf("handleDataFrame() error = %v", err)
	}
	if _, _, err := clientConn.ReadMessage(); err != nil {
		t.Fatalf("read response frame: %v", err)
	}
	queued := im.IncomingMessage{AppID: fc.AppID, ChatID: "oc_chat1"}
	select {
	case msg := <-feishuTestQueue(a, queued):
		if msg.InternalAction == nil || msg.InternalAction.Type != "confirm" || msg.InternalAction.Action != "allow" || msg.InternalAction.RequestID != "req_card" {
			t.Fatalf("message internal action = %#v", msg.InternalAction)
		}
	default:
		t.Fatal("expected card action frame to enqueue command")
	}
}

func TestFeishuAdapterConfigSnapshotAffectsOwner(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", Owner: &config.OwnerIdentity{AccountID: "cli_test", UserID: "ou_old"}})
	store := testConfigStore(t, "ims: {feishu: {app_id: cli_test, app_secret: secret, owner: {account_id: cli_test, user_id: ou_old}}}\nworkspaces: {main: {path: /tmp/project}}")
	mgr := NewChordManager(store.Snapshot(), &config.Paths{StateDir: t.TempDir()})
	mgr.store = store
	r := NewNotificationRouter(mgr)
	defer r.Close()
	r.admission = NewAdmission(NewPairingService(store, false, nil))
	if err := r.admission.pairing.PlatformReady("feishu", "cli_test"); err != nil {
		t.Fatal(err)
	}
	a.RetargetForTest(r)
	installFeishuTestIngress(t, a, r)
	_, err := store.Commit(context.Background(), ConfigMutation{Apply: func(doc *yaml.Node, _ *config.Config) error {
		ims, err := independentMapping(doc.Content[0], "ims")
		if err != nil {
			return err
		}
		platform, err := independentMapping(ims, "feishu")
		if err != nil {
			return err
		}
		setMappingValue(platform, "owner", mappingNode(scalarNode("account_id", 0), scalarNode("cli_test", 0), scalarNode("user_id", 0), scalarNode("ou_new", 0)))
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}

	if err := handleMessageEvent(context.Background(), a, makeFeishuMessageEvent("ou_old", "oc_chat1", "msg_old", "old")); err != nil {
		t.Fatalf("old owner handleMessageEvent() error = %v", err)
	}
	queued := im.IncomingMessage{AppID: "cli_test", ChatID: "oc_chat1"}
	select {
	case msg := <-feishuTestQueue(a, queued):
		t.Fatalf("old owner should be rejected after config update, got %#v", msg)
	default:
	}
	if err := handleMessageEvent(context.Background(), a, makeFeishuMessageEvent("ou_new", "oc_chat1", "msg_new", "new")); err != nil {
		t.Fatalf("new owner handleMessageEvent() error = %v", err)
	}
	select {
	case msg := <-feishuTestQueue(a, queued):
		if msg.SenderID != "ou_new" {
			t.Fatalf("message = %#v", msg)
		}
	default:
		t.Fatal("new owner should be accepted after config update")
	}
}

func TestFeishuConnect_ClientErrorReturnsInsteadOfHanging(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret"}
	a := testFeishuAdapter(t, fc)
	a.SetTokenForTest("token", time.Now().Add(time.Hour))
	a.SetLongConnForTest(func(ctx context.Context) error {
		return larkws.NewClientError(larkws.AuthFailed, "auth failed")
	})

	done := make(chan error, 1)
	go func() {
		done <- a.Connect()
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "feishu long connection") {
			t.Fatalf("Connect() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Connect() hung on client error")
	}
}

func TestFeishuGetAccessTokenCoalescesConcurrentRefreshes(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"shared-token","expire":3600}`))
	}))
	defer server.Close()

	oldBaseURL := feishu.OpenBaseURL
	feishu.OpenBaseURL = server.URL
	defer func() { feishu.OpenBaseURL = oldBaseURL }()
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "app", AppSecret: "secret"})
	a.SetHTTPClientForTest(server.Client())
	a.SetTokenForTest("", time.Time{})

	const callers = 16
	start := make(chan struct{})
	results := make(chan string, callers)
	errs := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			token, err := a.GetAccessTokenForTest(t.Context())
			results <- token
			errs <- err
		}()
	}
	close(start)
	for range callers {
		if err := <-errs; err != nil {
			t.Fatalf("getAccessToken() error = %v", err)
		}
		if token := <-results; token != "shared-token" {
			t.Fatalf("token = %q, want shared-token", token)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("token refresh requests = %d, want 1", got)
	}
}

func TestFeishuGetAccessTokenSharesRefreshFailure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(`{"code":1,"msg":"denied"}`))
	}))
	defer server.Close()

	oldBaseURL := feishu.OpenBaseURL
	feishu.OpenBaseURL = server.URL
	defer func() { feishu.OpenBaseURL = oldBaseURL }()
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "app", AppSecret: "secret"})
	a.SetHTTPClientForTest(server.Client())
	a.SetTokenForTest("", time.Time{})

	const callers = 16
	start := make(chan struct{})
	errs := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			_, err := a.GetAccessTokenForTest(t.Context())
			errs <- err
		}()
	}
	close(start)
	for range callers {
		if err := <-errs; err == nil || !strings.Contains(err.Error(), "code=1") || strings.Contains(err.Error(), "denied") {
			t.Fatalf("getAccessToken() error = %v, want sanitized status code", err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("failed token refresh requests = %d, want 1", got)
	}
}
