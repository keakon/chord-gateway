package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

// testFeishuAdapter creates a FeishuAdapter suitable for unit testing.
func testFeishuAdapter(t *testing.T, fc *config.FeishuConfig) *FeishuAdapter {
	t.Helper()
	dedupeDir := t.TempDir()
	cfg := &config.Config{
		IM: config.IMConfig{
			Type:   "feishu",
			Feishu: fc,
		},
		Workspaces: []config.Workspace{
			{ID: "test", Path: "/tmp/test"},
		},
	}
	dedupe, err := NewDedupeStore(dedupeDir)
	if err != nil {
		t.Fatal(err)
	}
	// Create a minimal NotificationRouter with no ChordManager.
	// dispatchMessage will call HandleIncomingMessage, which will
	// parse the command and try to resolve workspace — since there is
	// no ChordManager, it may call sendText which will log but not panic.
	a := &FeishuAdapter{
		cfg:          cfg,
		httpClient:   &http.Client{Timeout: 5 * time.Second},
		messageQueue: make(chan IncomingMessage, 16),
		dedupe:       dedupe,
		router: &NotificationRouter{
			cfg:           cfg,
			lastKeyChatID: make(map[string]string),
			lastTodos:     make(map[string][]TodoItem),
		},
	}
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

	// Custom consumer that counts messages without calling router.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		for {
			select {
			case <-ctx.Done():
				// Drain.
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

// makeFeishuCallbackBody builds a JSON body for an im.message.receive_v1 callback.
func makeFeishuCallbackBody(token, openID, chatID, messageID, text string) []byte {
	content := fmt.Sprintf(`{"text":"%s"}`, text)
	event := FeishuMessageEvent{
		Sender: FeishuEventSender{
			SenderID: FeishuSenderID{OpenID: openID},
		},
		Message: FeishuEventMessage{
			ChatID:      chatID,
			MessageID:   messageID,
			MessageType: "text",
			Content:     content,
		},
	}
	eventJSON, _ := json.Marshal(event)
	callback := FeishuEventCallback{
		Schema: "2.0",
		Header: FeishuEventHeader{
			EventType: "im.message.receive_v1",
			Token:     token,
		},
		Event: eventJSON,
	}
	body, _ := json.Marshal(callback)
	return body
}

func makeFeishuCardActionBody(token, openID, chatID, requestID, command string) []byte {
	event := FeishuCardActionEvent{}
	event.Operator.OpenID = openID
	event.Action.Tag = "button"
	event.Action.Value = map[string]any{
		"request_id": requestID,
		"command":    command,
		"chat_id":    chatID,
		"im_type":    "feishu",
	}
	event.Context.OpenChatID = chatID
	eventJSON, _ := json.Marshal(event)
	callback := FeishuEventCallback{
		Schema: "2.0",
		Header: FeishuEventHeader{EventType: "card.action.trigger", Token: token},
		Event:  eventJSON,
	}
	body, _ := json.Marshal(callback)
	return body
}

func TestFeishuHandler_FastACK(t *testing.T) {
	fc := &config.FeishuConfig{
		AppID:     "cli_test",
		AppSecret: "secret",
	}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer a.dedupe.Close()
	defer cancel()

	body := makeFeishuCallbackBody("", "ou_owner", "oc_chat1", "msg_001", "hello")
	req := httptest.NewRequest(http.MethodPost, "/feishu/callback", bytes.NewReader(body))
	w := httptest.NewRecorder()

	a.handleCallback(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 1 {
		t.Fatalf("expected 1 dispatch, got %d", dispatched.Load())
	}
}

func TestFeishuHandler_DuplicateDedup(t *testing.T) {
	fc := &config.FeishuConfig{
		AppID:     "cli_test",
		AppSecret: "secret",
	}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer a.dedupe.Close()
	defer cancel()

	body := makeFeishuCallbackBody("", "ou_owner", "oc_chat1", "msg_dup", "hello")

	// First call should enqueue.
	req1 := httptest.NewRequest(http.MethodPost, "/feishu/callback", bytes.NewReader(body))
	w1 := httptest.NewRecorder()
	a.handleCallback(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("first call: expected 200, got %d", w1.Code)
	}

	// Wait for consumer to process (which commits dedupe).
	time.Sleep(100 * time.Millisecond)

	// Second call with same message_id should be deduped.
	req2 := httptest.NewRequest(http.MethodPost, "/feishu/callback", bytes.NewReader(body))
	w2 := httptest.NewRecorder()
	a.handleCallback(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("second call: expected 200, got %d", w2.Code)
	}

	time.Sleep(50 * time.Millisecond)

	if count := dispatched.Load(); count != 1 {
		t.Fatalf("expected 1 dispatch, got %d", count)
	}
}

func TestFeishuHandler_OwnerFilter(t *testing.T) {
	fc := &config.FeishuConfig{
		AppID:       "cli_test",
		AppSecret:   "secret",
		OwnerOpenID: "ou_owner",
	}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer a.dedupe.Close()
	defer cancel()

	// Non-owner message should be silently rejected (200 but not enqueued).
	body := makeFeishuCallbackBody("", "ou_not_owner", "oc_chat1", "msg_001", "hello")
	req := httptest.NewRequest(http.MethodPost, "/feishu/callback", bytes.NewReader(body))
	w := httptest.NewRecorder()
	a.handleCallback(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 0 {
		t.Fatal("non-owner message should not be dispatched")
	}

	// Owner message should be accepted.
	body2 := makeFeishuCallbackBody("", "ou_owner", "oc_chat1", "msg_002", "hello")
	req2 := httptest.NewRequest(http.MethodPost, "/feishu/callback", bytes.NewReader(body2))
	w2 := httptest.NewRecorder()
	a.handleCallback(w2, req2)

	time.Sleep(100 * time.Millisecond)
	if dispatched.Load() != 1 {
		t.Fatalf("owner message should be dispatched, got %d", dispatched.Load())
	}
}

func TestFeishuHandler_AllowlistFilter(t *testing.T) {
	fc := &config.FeishuConfig{
		AppID:          "cli_test",
		AppSecret:      "secret",
		AllowedOpenIDs: []string{"ou_allowed1", "ou_allowed2"},
	}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer a.dedupe.Close()
	defer cancel()

	// Allowed user should pass.
	body := makeFeishuCallbackBody("", "ou_allowed1", "oc_chat1", "msg_001", "hello")
	req := httptest.NewRequest(http.MethodPost, "/feishu/callback", bytes.NewReader(body))
	w := httptest.NewRecorder()
	a.handleCallback(w, req)

	time.Sleep(100 * time.Millisecond)
	if dispatched.Load() != 1 {
		t.Fatalf("allowed user message should be dispatched, got %d", dispatched.Load())
	}

	// Not-allowed user should be rejected.
	body2 := makeFeishuCallbackBody("", "ou_not_allowed", "oc_chat1", "msg_002", "hello")
	req2 := httptest.NewRequest(http.MethodPost, "/feishu/callback", bytes.NewReader(body2))
	w2 := httptest.NewRecorder()
	a.handleCallback(w2, req2)

	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 1 {
		t.Fatalf("not-allowed user should not be dispatched, got %d", dispatched.Load())
	}
}

func TestFeishuHandler_NoFilterWhenNotConfigured(t *testing.T) {
	fc := &config.FeishuConfig{
		AppID:     "cli_test",
		AppSecret: "secret",
	}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer a.dedupe.Close()
	defer cancel()

	// Without owner/allowlist, any user should pass.
	body := makeFeishuCallbackBody("", "ou_anyone", "oc_chat1", "msg_001", "hello")
	req := httptest.NewRequest(http.MethodPost, "/feishu/callback", bytes.NewReader(body))
	w := httptest.NewRecorder()
	a.handleCallback(w, req)

	time.Sleep(100 * time.Millisecond)
	if dispatched.Load() != 1 {
		t.Fatalf("any user should pass when no filter configured, got %d", dispatched.Load())
	}
}

func TestFeishuHandler_QueueFullRelease(t *testing.T) {
	fc := &config.FeishuConfig{
		AppID:     "cli_test",
		AppSecret: "secret",
	}
	// Use a tiny queue (size 1) that we don't drain, so it fills up.
	a := testFeishuAdapter(t, fc)
	a.messageQueue = make(chan IncomingMessage, 1) // override to size 1
	defer a.dedupe.Close()

	// Fill the queue with a blocking call.
	body := makeFeishuCallbackBody("", "ou_user", "oc_chat1", "msg_001", "first")
	req := httptest.NewRequest(http.MethodPost, "/feishu/callback", bytes.NewReader(body))
	w := httptest.NewRecorder()
	a.handleCallback(w, req)
	if w.Code != http.StatusOK {
		t.Fatal("expected 200")
	}

	// Now the queue is full. The second message with a different message_id
	// should be rejected (queue full → release dedupe), still 200.
	body2 := makeFeishuCallbackBody("", "ou_user", "oc_chat1", "msg_002", "second")
	req2 := httptest.NewRequest(http.MethodPost, "/feishu/callback", bytes.NewReader(body2))
	w2 := httptest.NewRecorder()
	a.handleCallback(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatal("queue full should still return 200")
	}

	// Verify dedupe was released — TryBegin should succeed for msg_002 now.
	dedupeKey := fmt.Sprintf("%s|%s|%s", fc.AppID, "oc_chat1", "msg_002")
	if !a.dedupe.TryBegin(dedupeKey) {
		t.Fatal("dedupe should be released after queue full, TryBegin should succeed")
	}
}

func TestFeishuHandler_IgnoresNonTextMessages(t *testing.T) {
	fc := &config.FeishuConfig{
		AppID:     "cli_test",
		AppSecret: "secret",
	}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer a.dedupe.Close()
	defer cancel()

	// Build a non-text message callback.
	event := FeishuMessageEvent{
		Sender: FeishuEventSender{
			SenderID: FeishuSenderID{OpenID: "ou_user"},
		},
		Message: FeishuEventMessage{
			ChatID:      "oc_chat1",
			MessageID:   "msg_001",
			MessageType: "image",
			Content:     "{}",
		},
	}
	eventJSON, _ := json.Marshal(event)
	callback := FeishuEventCallback{
		Schema: "2.0",
		Header: FeishuEventHeader{
			EventType: "im.message.receive_v1",
		},
		Event: eventJSON,
	}
	body, _ := json.Marshal(callback)
	req := httptest.NewRequest(http.MethodPost, "/feishu/callback", bytes.NewReader(body))
	w := httptest.NewRecorder()
	a.handleCallback(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 0 {
		t.Fatal("non-text message should not be dispatched")
	}
}

func TestFeishuHandler_CardActionTriggerDispatchesOnce(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", OwnerOpenID: "ou_owner"}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer a.dedupe.Close()
	defer cancel()

	body := makeFeishuCardActionBody("", "ou_owner", "oc_chat1", "req_1", "/allow req_1")
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/feishu/callback", bytes.NewReader(body))
		w := httptest.NewRecorder()
		a.handleCallback(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if dispatched.Load() != 1 {
		t.Fatalf("expected 1 dispatched card action, got %d", dispatched.Load())
	}
}

func TestFeishuHandler_CardActionWrongContextIgnored(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", OwnerOpenID: "ou_owner"}
	a, dispatched, cancel := testFeishuAdapterWithQueue(t, fc)
	defer a.dedupe.Close()
	defer cancel()

	body := makeFeishuCardActionBody("", "ou_owner", "oc_chat1", "req_2", "/allow req_2")
	body = []byte(strings.ReplaceAll(string(body), `"chat_id":"oc_chat1"`, `"chat_id":"oc_other"`))
	req := httptest.NewRequest(http.MethodPost, "/feishu/callback", bytes.NewReader(body))
	w := httptest.NewRecorder()
	a.handleCallback(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if dispatched.Load() != 0 {
		t.Fatal("wrong-context card action should not be dispatched")
	}
}
