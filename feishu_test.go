package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkcallback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/keakon/chord-gateway/config"
)

// testFeishuAdapter creates a FeishuAdapter suitable for unit testing.
func testFeishuAdapter(t *testing.T, fc *config.FeishuConfig) *FeishuAdapter {
	t.Helper()
	dedupeDir := t.TempDir()
	cfg := &config.Config{
		IMs: []config.IMAdapterConfig{{
			Feishu: fc,
		}},
		Workspaces: []config.Workspace{
			{ID: "test", Path: "/tmp/test"},
		},
	}
	dedupe, err := NewDedupeStore(dedupeDir)
	if err != nil {
		t.Fatal(err)
	}
	a := &FeishuAdapter{
		imCfg:             cfg.IMs[0],
		httpClient:        nil,
		messageQueue:      make(chan IncomingMessage, 16),
		dedupe:            dedupe,
		fragments:         make(map[string]feishuFragmentBuffer),
		pingInterval:      feishuDefaultPing,
		reconnectInterval: feishuDefaultReconnect,
		router: &NotificationRouter{
			cfg:           cfg,
			lastKeyChatID: make(map[string]string),
		},
	}
	a.runLongConn = a.runLongConnection
	return a
}

// testFeishuAdapterWithQueue creates a test adapter and a consumer goroutine
// that drains the queue without calling the router (to avoid nil ChordManager).
// Returns the adapter and a counter of dispatched messages.
func testFeishuAdapterWithQueue(t *testing.T, fc *config.FeishuConfig) (*FeishuAdapter, *atomic.Int32, context.CancelFunc) {
	t.Helper()
	a := testFeishuAdapter(t, fc)
	ctx, cancel := context.WithCancel(context.Background())
	var dispatched atomic.Int32

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		for {
			select {
			case <-ctx.Done():
				for {
					select {
					case <-a.messageQueue:
						dispatched.Add(1)
					default:
						return
					}
				}
			case <-a.messageQueue:
				dispatched.Add(1)
			}
		}
	}()

	return a, &dispatched, cancel
}

func makeFeishuMessageEvent(openID, chatID, messageID, text string) *larkim.P2MessageReceiveV1 {
	content := fmt.Sprintf(`{"text":"%s"}`, text)
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: stringPtr(openID)},
			},
			Message: &larkim.EventMessage{
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
				SenderId: &larkim.UserId{OpenId: stringPtr(openID)},
			},
			Message: &larkim.EventMessage{
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
		Event: &larkcallback.CardActionTriggerRequest{
			Operator: &larkcallback.Operator{OpenID: openID},
			Action: &larkcallback.CallBackAction{
				Tag: "button",
				Value: map[string]interface{}{
					"type":       "confirm",
					"action":     action,
					"request_id": requestID,
					"chat_id":    chatID,
					"im_type":    "feishu",
				},
			},
			Context: &larkcallback.Context{OpenChatID: chatID},
		},
	}
}

func stringPtr(s string) *string { return &s }

func TestFeishuMessageEvent_EnqueuesAndDispatches(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret"}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer a.dedupe.Close()
	defer cancel()

	err := a.handleMessageEvent(context.Background(), makeFeishuMessageEvent("ou_owner", "oc_chat1", "msg_001", "hello"))
	if err != nil {
		t.Fatalf("handleMessageEvent() error = %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 1 {
		t.Fatalf("expected 1 dispatch, got %d", dispatched.Load())
	}
}

func TestFeishuMessageEvent_DuplicateDedup(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret"}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer a.dedupe.Close()
	defer cancel()

	event := makeFeishuMessageEvent("ou_owner", "oc_chat1", "msg_dup", "hello")
	if err := a.handleMessageEvent(context.Background(), event); err != nil {
		t.Fatalf("first handleMessageEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := a.handleMessageEvent(context.Background(), event); err != nil {
		t.Fatalf("second handleMessageEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	if count := dispatched.Load(); count != 1 {
		t.Fatalf("expected 1 dispatch, got %d", count)
	}
}

func TestFeishuMessageEvent_OwnerFilter(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", OwnerOpenID: "ou_owner"}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer a.dedupe.Close()
	defer cancel()

	if err := a.handleMessageEvent(context.Background(), makeFeishuMessageEvent("ou_not_owner", "oc_chat1", "msg_001", "hello")); err != nil {
		t.Fatalf("non-owner handleMessageEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 0 {
		t.Fatal("non-owner message should not be dispatched")
	}

	if err := a.handleMessageEvent(context.Background(), makeFeishuMessageEvent("ou_owner", "oc_chat1", "msg_002", "hello")); err != nil {
		t.Fatalf("owner handleMessageEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 1 {
		t.Fatalf("owner message should be dispatched, got %d", dispatched.Load())
	}
}

func TestFeishuMessageEvent_AllowlistFilter(t *testing.T) {
	fc := &config.FeishuConfig{
		AppID:          "cli_test",
		AppSecret:      "secret",
		AllowedOpenIDs: []string{"ou_allowed1", "ou_allowed2"},
	}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer a.dedupe.Close()
	defer cancel()

	if err := a.handleMessageEvent(context.Background(), makeFeishuMessageEvent("ou_allowed1", "oc_chat1", "msg_001", "hello")); err != nil {
		t.Fatalf("allowed handleMessageEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 1 {
		t.Fatalf("allowed user message should be dispatched, got %d", dispatched.Load())
	}

	if err := a.handleMessageEvent(context.Background(), makeFeishuMessageEvent("ou_not_allowed", "oc_chat1", "msg_002", "hello")); err != nil {
		t.Fatalf("not-allowed handleMessageEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 1 {
		t.Fatalf("not-allowed user should not be dispatched, got %d", dispatched.Load())
	}
}

func TestFeishuMessageEvent_NoFilterWhenNotConfigured(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret"}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer a.dedupe.Close()
	defer cancel()

	if err := a.handleMessageEvent(context.Background(), makeFeishuMessageEvent("ou_anyone", "oc_chat1", "msg_001", "hello")); err != nil {
		t.Fatalf("handleMessageEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 1 {
		t.Fatalf("any user should pass when no filter configured, got %d", dispatched.Load())
	}
}

func TestFeishuMessageEvent_QueueFullRelease(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret"}
	a := testFeishuAdapter(t, fc)
	a.messageQueue = make(chan IncomingMessage, 1)
	defer a.dedupe.Close()

	if err := a.handleMessageEvent(context.Background(), makeFeishuMessageEvent("ou_user", "oc_chat1", "msg_001", "first")); err != nil {
		t.Fatalf("first handleMessageEvent() error = %v", err)
	}
	if err := a.handleMessageEvent(context.Background(), makeFeishuMessageEvent("ou_user", "oc_chat1", "msg_002", "second")); err != nil {
		t.Fatalf("second handleMessageEvent() error = %v", err)
	}

	dedupeKey := fmt.Sprintf("%s|%s|%s", fc.AppID, "oc_chat1", "msg_002")
	if !a.dedupe.TryBegin(dedupeKey) {
		t.Fatal("dedupe should be released after queue full, TryBegin should succeed")
	}
}

func TestFeishuMessageEvent_IgnoresNonTextMessages(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret"}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer a.dedupe.Close()
	defer cancel()

	if err := a.handleMessageEvent(context.Background(), makeFeishuNonTextMessageEvent("ou_user", "oc_chat1", "msg_001")); err != nil {
		t.Fatalf("handleMessageEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 0 {
		t.Fatal("non-text message should not be dispatched")
	}
}

func TestFeishuCardActionEvent_DispatchesOnce(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", OwnerOpenID: "ou_owner"}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer a.dedupe.Close()
	defer cancel()

	event := makeFeishuCardActionEvent("ou_owner", "oc_chat1", "req_1", "allow")
	for i := 0; i < 2; i++ {
		if _, err := a.handleCardActionEvent(context.Background(), event); err != nil {
			t.Fatalf("handleCardActionEvent() error = %v", err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 1 {
		t.Fatalf("expected 1 dispatched card action, got %d", dispatched.Load())
	}
}

func TestFeishuCardActionEvent_WrongContextIgnored(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", OwnerOpenID: "ou_owner"}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer a.dedupe.Close()
	defer cancel()

	event := makeFeishuCardActionEvent("ou_owner", "oc_chat1", "req_2", "allow")
	event.Event.Action.Value["chat_id"] = "oc_other"
	if _, err := a.handleCardActionEvent(context.Background(), event); err != nil {
		t.Fatalf("handleCardActionEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 0 {
		t.Fatal("wrong-context card action should not be dispatched")
	}
}

func TestFeishuCardActionEvent_QueueFullRelease(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", OwnerOpenID: "ou_owner"}
	a := testFeishuAdapter(t, fc)
	a.messageQueue = make(chan IncomingMessage, 1)
	defer a.dedupe.Close()

	if _, err := a.handleCardActionEvent(context.Background(), makeFeishuCardActionEvent("ou_owner", "oc_chat1", "req_1", "allow")); err != nil {
		t.Fatalf("first handleCardActionEvent() error = %v", err)
	}
	if _, err := a.handleCardActionEvent(context.Background(), makeFeishuCardActionEvent("ou_owner", "oc_chat1", "req_2", "allow")); err != nil {
		t.Fatalf("second handleCardActionEvent() error = %v", err)
	}

	dedupeKey := fmt.Sprintf("%s|card|%s|%s", fc.AppID, "oc_chat1", "req_2|confirm|allow|")
	if !a.dedupe.TryBegin(dedupeKey) {
		t.Fatal("card-action dedupe should be released after queue full")
	}
}

func TestFeishuMessageEvent_InvalidJSONIgnored(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret"}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer a.dedupe.Close()
	defer cancel()

	event := makeFeishuMessageEvent("ou_user", "oc_chat1", "msg_001", "hello")
	event.Event.Message.Content = stringPtr("{invalid")
	if err := a.handleMessageEvent(context.Background(), event); err != nil {
		t.Fatalf("handleMessageEvent() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 0 {
		t.Fatal("invalid JSON content should not be dispatched")
	}
}

func TestFeishuMessageEvent_ThreadIDBecomesConversationID(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret"}
	a := testFeishuAdapter(t, fc)
	defer a.dedupe.Close()

	var got IncomingMessage
	a.msgRouter = messageRouterFunc(func(msg IncomingMessage) { got = msg })
	a.dispatchMessage(IncomingMessage{})

	event := makeFeishuMessageEvent("ou_user", "oc_chat1", "msg_001", "hello")
	event.Event.Message.ThreadId = stringPtr("omt_thread_1")
	if err := a.handleMessageEvent(context.Background(), event); err != nil {
		t.Fatalf("handleMessageEvent() error = %v", err)
	}
	select {
	case msg := <-a.messageQueue:
		got = msg
	default:
		t.Fatal("expected message to be enqueued")
	}
	if got.ConversationID != "omt_thread_1" {
		t.Fatalf("ConversationID = %q, want %q", got.ConversationID, "omt_thread_1")
	}
}

type messageRouterFunc func(IncomingMessage)

func (f messageRouterFunc) HandleIncomingMessage(msg IncomingMessage) { f(msg) }

func TestFeishuCardActionEvent_UsesRequestIDAndInternalActionAsMessageID(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", OwnerOpenID: "ou_owner"}
	a := testFeishuAdapter(t, fc)
	defer a.dedupe.Close()

	event := makeFeishuCardActionEvent("ou_owner", "oc_chat1", "req_9", "allow")
	if _, err := a.handleCardActionEvent(context.Background(), event); err != nil {
		t.Fatalf("handleCardActionEvent() error = %v", err)
	}
	select {
	case msg := <-a.messageQueue:
		if msg.MessageID != "req_9:confirm:allow:" {
			t.Fatalf("MessageID = %q", msg.MessageID)
		}
		if msg.InternalAction == nil || msg.InternalAction.Type != "confirm" || msg.InternalAction.Action != "allow" || msg.InternalAction.RequestID != "req_9" {
			t.Fatalf("InternalAction = %#v", msg.InternalAction)
		}
	default:
		t.Fatal("expected card action to be enqueued")
	}
}

func TestFeishuMessageEvent_ContentMatchesText(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret"}
	a := testFeishuAdapter(t, fc)
	defer a.dedupe.Close()

	event := makeFeishuMessageEvent("ou_user", "oc_chat1", "msg_001", "hello world")
	if err := a.handleMessageEvent(context.Background(), event); err != nil {
		t.Fatalf("handleMessageEvent() error = %v", err)
	}
	select {
	case msg := <-a.messageQueue:
		if msg.Text != "hello world" {
			t.Fatalf("Text = %q", msg.Text)
		}
	default:
		t.Fatal("expected message to be enqueued")
	}
}

func TestFeishuMessageEvent_PostContentDispatchesPlainText(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret"}
	a := testFeishuAdapter(t, fc)
	defer a.dedupe.Close()

	event := makeFeishuMessageEvent("ou_user", "oc_chat1", "msg_post", "")
	*event.Event.Message.MessageType = "post"
	*event.Event.Message.Content = `{"content":[[{"tag":"text","text":"/deny "},{"tag":"text","text":"not safe"}]]}`
	if err := a.handleMessageEvent(context.Background(), event); err != nil {
		t.Fatalf("handleMessageEvent() error = %v", err)
	}
	select {
	case msg := <-a.messageQueue:
		if msg.Text != "/deny not safe" {
			t.Fatalf("Text = %q", msg.Text)
		}
	default:
		t.Fatal("expected post message to be enqueued")
	}
}

func TestFeishuMessageEvent_MessageContentEncodingMatchesFeishu(t *testing.T) {
	content := FeishuMessageContent{Text: "hello"}
	bs, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if string(bs) != `{"text":"hello"}` {
		t.Fatalf("content JSON = %s", string(bs))
	}
}

func TestFeishuCardActionFrameTypeCard_Dispatches(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", OwnerOpenID: "ou_owner"}
	a := testFeishuAdapter(t, fc)
	defer a.dedupe.Close()

	dispatcher := larkdispatcher.NewEventDispatcher("", "").OnP2CardActionTrigger(a.handleCardActionEvent)
	payload := []byte(`{"schema":"2.0","header":{"event_type":"card.action.trigger","event_id":"evt_1"},"event":{"operator":{"open_id":"ou_owner"},"action":{"tag":"button","value":{"type":"confirm","action":"allow","request_id":"req_card","chat_id":"oc_chat1","im_type":"feishu"}},"context":{"open_chat_id":"oc_chat1"}}}`)
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

	if err := a.handleDataFrame(context.Background(), conn, dispatcher, frame); err != nil {
		t.Fatalf("handleDataFrame() error = %v", err)
	}
	if _, _, err := clientConn.ReadMessage(); err != nil {
		t.Fatalf("read response frame: %v", err)
	}
	select {
	case msg := <-a.messageQueue:
		if msg.InternalAction == nil || msg.InternalAction.Type != "confirm" || msg.InternalAction.Action != "allow" || msg.InternalAction.RequestID != "req_card" {
			t.Fatalf("message internal action = %#v", msg.InternalAction)
		}
	default:
		t.Fatal("expected card action frame to enqueue command")
	}
}

func TestFeishuAdapterUpdateIMConfigAffectsAllowlist(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", OwnerOpenID: "ou_old"})
	defer a.dedupe.Close()
	a.updateIMConfig(config.IMAdapterConfig{Feishu: &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", OwnerOpenID: "ou_new"}})

	if err := a.handleMessageEvent(context.Background(), makeFeishuMessageEvent("ou_old", "oc_chat1", "msg_old", "old")); err != nil {
		t.Fatalf("old owner handleMessageEvent() error = %v", err)
	}
	select {
	case msg := <-a.messageQueue:
		t.Fatalf("old owner should be rejected after config update, got %#v", msg)
	default:
	}
	if err := a.handleMessageEvent(context.Background(), makeFeishuMessageEvent("ou_new", "oc_chat1", "msg_new", "new")); err != nil {
		t.Fatalf("new owner handleMessageEvent() error = %v", err)
	}
	select {
	case msg := <-a.messageQueue:
		if msg.SenderID != "ou_new" {
			t.Fatalf("message = %#v", msg)
		}
	default:
		t.Fatal("new owner should be accepted after config update")
	}
}

func TestQueueConsumerDrainsOnCancel(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret"}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer a.dedupe.Close()

	a.messageQueue <- IncomingMessage{ChatID: "chat-1", MessageID: "m1"}
	a.messageQueue <- IncomingMessage{ChatID: "chat-1", MessageID: "m2"}

	cancel()
	a.wg.Wait()

	if got := dispatched.Load(); got != 2 {
		t.Fatalf("queueConsumer should drain all queued messages on cancel, got %d", got)
	}
}

func TestFeishuConnect_ClientErrorReturnsInsteadOfHanging(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret"}
	a := testFeishuAdapter(t, fc)
	defer a.dedupe.Close()
	a.accessToken = "token"
	a.tokenExpireAt = time.Now().Add(time.Hour)
	a.runLongConn = func(ctx context.Context, _ *larkdispatcher.EventDispatcher) error {
		return larkws.NewClientError(larkws.AuthFailed, "auth failed")
	}

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
