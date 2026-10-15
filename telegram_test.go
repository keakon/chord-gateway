package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

func TestTelegramNormalizeIdentityAndUnsupportedInputs(t *testing.T) {
	a := &TelegramAdapter{accountID: "9007199254740993", username: "ThisBot", now: realTelegramClock}
	tests := []struct {
		name   string
		mutate func(*telegramUpdate)
		reason string
	}{
		{name: "private"},
		{name: "group", mutate: func(u *telegramUpdate) { u.Message.Chat = telegramChat{ID: -9007199254740993, Type: "supergroup"} }},
		{name: "nonmessage", mutate: func(u *telegramUpdate) { u.Message = nil }, reason: "unsupported_update"},
		{name: "channel", mutate: func(u *telegramUpdate) { u.Message.Chat.Type = "channel" }, reason: "unsupported_chat"},
		{name: "topic", mutate: func(u *telegramUpdate) { u.Message.MessageThreadID = 1 }, reason: "unsupported_topic"},
		{name: "topicflag", mutate: func(u *telegramUpdate) { u.Message.IsTopicMessage = true }, reason: "unsupported_topic"},
		{name: "sender_chat", mutate: func(u *telegramUpdate) { u.Message.SenderChat = &telegramChat{ID: -1} }, reason: "unsupported_sender_chat"},
		{name: "anonymous", mutate: func(u *telegramUpdate) { u.Message.From = nil }, reason: "invalid_identity"},
		{name: "bot", mutate: func(u *telegramUpdate) { u.Message.From.IsBot = true }, reason: "invalid_identity"},
		{name: "private_wrong_from", mutate: func(u *telegramUpdate) { u.Message.From.ID = 8 }, reason: "invalid_identity"},
		{name: "wrong_group_sign", mutate: func(u *telegramUpdate) { u.Message.Chat.Type = "group" }, reason: "invalid_identity"},
		{name: "missingchat", mutate: func(u *telegramUpdate) { u.Message.Chat.ID = 0 }, reason: "invalid_identity"},
		{name: "missingmessage", mutate: func(u *telegramUpdate) { u.Message.MessageID = 0 }, reason: "invalid_identity"},
		{name: "media", mutate: func(u *telegramUpdate) { u.Message.Text = "" }, reason: "unsupported_content"},
		{name: "media_caption", mutate: func(u *telegramUpdate) { u.Message.Text = "summarize this"; u.Message.hasMedia = true }, reason: "unsupported_content"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			u := telegramTestUpdate(9007199254740993)
			if test.mutate != nil {
				test.mutate(&u)
			}
			e, reason := a.normalizeUpdate(u)
			if reason != test.reason {
				t.Fatalf("reason=%q want=%q", reason, test.reason)
			}
			if e.Message.AccountID != "9007199254740993" || e.Message.MessageID != "update:9007199254740993" {
				t.Fatalf("numeric identity changed: %+v", e)
			}
			if reason == "" && e.InputDigest == "" {
				t.Fatal("missing original-input digest")
			}
			if reason == "" {
				parts, err := DecodeScopedID(e.SemanticID, "message:", 2)
				if err != nil || parts[0] != e.Message.ChatID {
					t.Fatal(parts, err)
				}
			}
			if reason == "invalid_identity" && u.Message.Chat.ID == 0 && e.Message.ChatID == "0" {
				t.Fatal("invalid target presented as trusted zero chat")
			}
		})
	}
}

func TestTelegramCommandEntitySuffixAndUTF16(t *testing.T) {
	tests := []struct {
		name, text   string
		entities     []telegramEntity
		want, reason string
	}{
		{"suffix", "/status@THISBOT", []telegramEntity{{Type: "bot_command", Length: 15}}, "/status", ""},
		{"args", "/bind@ThisBot \"/tmp/😃 @path\"", []telegramEntity{{Type: "bot_command", Length: 13}}, "/bind \"/tmp/😃 @path\"", ""},
		{"other", "/status@other_bot", []telegramEntity{{Type: "bot_command", Length: 17}}, "", "other_bot_command"},
		{"noentity", "/status@ThisBot", nil, "/status@ThisBot", ""},
		{"ordinary", "path /tmp/a@ThisBot 😀", nil, "path /tmp/a@ThisBot 😀", ""},
		{"nonleading", "😃 /status@ThisBot", []telegramEntity{{Type: "bot_command", Offset: 3, Length: 15}}, "😃 /status@ThisBot", ""},
		{"surrogatehalf", "/😃@ThisBot", []telegramEntity{{Type: "bot_command", Length: 2}}, "", "invalid_command_entity"},
		{"outofbounds", "/status", []telegramEntity{{Type: "bot_command", Length: 20}}, "", "invalid_command_entity"},
		{"notcommand", "hello", []telegramEntity{{Type: "bot_command", Length: 5}}, "", "invalid_command_entity"},
		{"legacy_hyphen_full_entity", "/handoff-deny@ThisBot", []telegramEntity{{Type: "bot_command", Length: 21}}, "", "invalid_command_entity"},
		{"legacy_hyphen_prefix_entity", "/handoff-deny@ThisBot", []telegramEntity{{Type: "bot_command", Length: 8}}, "", "invalid_command_entity"},
		{"alias_full_entity", "/handoff_deny@ThisBot", []telegramEntity{{Type: "bot_command", Length: 21}}, "/handoff-deny", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, reason := telegramNormalizeCommand(test.text, test.entities, "ThisBot")
			if got != test.want || reason != test.reason {
				t.Fatalf("got=(%q,%q) want=(%q,%q)", got, reason, test.want, test.reason)
			}
		})
	}
	for _, test := range []struct {
		units, byteIndex int
		ok               bool
	}{{0, 0, true}, {1, 1, true}, {2, 0, false}, {3, 5, true}, {4, 6, true}, {5, 6, false}} {
		byteIndex, ok := telegramUTF16Boundary("a😃b", test.units)
		if ok != test.ok || ok && byteIndex != test.byteIndex {
			t.Fatal(test, byteIndex, ok)
		}
	}
}

func TestTelegramNormalizeOmitsCommandDigest(t *testing.T) {
	a := &TelegramAdapter{accountID: "99", username: "ThisBot", now: realTelegramClock}
	u := telegramTestUpdate(10)
	u.Message.Text = "/status@ThisBot"
	u.Message.Entities = []telegramEntity{{Type: "bot_command", Length: 15}}
	e, reason := a.normalizeUpdate(u)
	if reason != "" || e.Message.Text != "/status" || e.InputDigest != "" {
		t.Fatal(e, reason)
	}
}

func TestTelegramAdapterConstructionAndLifecycle(t *testing.T) {
	if _, err := NewTelegramAdapter(nil, config.IMAdapterConfig{}, nil, nil); err == nil {
		t.Fatal("missing token accepted")
	}
	a, err := NewTelegramAdapter(nil, config.IMAdapterConfig{Telegram: &config.TelegramConfig{BotToken: "secret"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Type() != "telegram" || a.SupportsLoginRenewal() {
		t.Fatal("adapter identity")
	}
	if _, err := a.StartLogin(); !errors.Is(err, ErrLoginNotSupported) {
		t.Fatal(err)
	}
	if err := a.SendTextContext(t.Context(), "7", "text"); err == nil {
		t.Fatal("unready account sent")
	}
	a.running = true
	if err := a.Connect(); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatal(err)
	}
	a.Disconnect()
	if !errors.Is(a.ctx.Err(), context.Canceled) {
		t.Fatal("disconnect did not cancel lifecycle")
	}
}

func TestTelegramStartupRetryStopsOnCredentialsAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	a := &TelegramAdapter{ctx: ctx, cancel: cancel}
	count := 0
	err := a.startupRequest(func() error { count++; return &telegramAPIError{Category: "credentials"} })
	if count != 1 || err == nil {
		t.Fatal(count, err)
	}
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.startupRequest(func() error {
			close(entered)
			return &telegramAPIError{Category: "rate_limited", RetryAfter: time.Hour}
		})
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("startup retry did not stop")
	}
}

// The transport remains context-aware while allowing the test to observe
// shutdown before its final send cleanup finishes.
type telegramLifecycleAPI struct {
	telegramAPI
	getMeFn   func(context.Context) (telegramUser, error)
	updatesFn func(context.Context, *int64, int) ([]telegramUpdate, error)
	sendFn    func(context.Context, int64, string) (telegramMessage, error)
}

func (a *telegramLifecycleAPI) GetUpdates(ctx context.Context, offset *int64, timeout int) ([]telegramUpdate, error) {
	return a.updatesFn(ctx, offset, timeout)
}
func (a *telegramLifecycleAPI) SendMessage(ctx context.Context, chat int64, text string) (telegramMessage, error) {
	return a.sendFn(ctx, chat, text)
}

func TestTelegramAdapterShutdownJoinsPollAndSendBeforeReleasingAccount(t *testing.T) {
	f := newTelegramGatewayFixture(t, 840001)
	a, base := f.adapter(t, f.botID)
	pollEntered := make(chan struct{})
	sendEntered := make(chan struct{})
	sendCanceled := make(chan struct{})
	allowSendExit := make(chan struct{})
	var pollOnce, sendOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(allowSendExit) }) }
	t.Cleanup(release)
	api := &telegramLifecycleAPI{telegramAPI: base}
	api.updatesFn = func(ctx context.Context, offset *int64, timeout int) ([]telegramUpdate, error) {
		if timeout == 0 {
			return base.GetUpdates(ctx, offset, timeout)
		}
		pollOnce.Do(func() { close(pollEntered) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	api.sendFn = func(ctx context.Context, _ int64, _ string) (telegramMessage, error) {
		sendOnce.Do(func() { close(sendEntered) })
		<-ctx.Done()
		close(sendCanceled)
		<-allowSendExit
		return telegramMessage{}, ctx.Err()
	}
	a.api = api
	var err error
	a.sender, err = newTelegramTextSender(api)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Connect(); close(done) }()
	t.Cleanup(func() {
		a.Disconnect()
		release()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("Connect did not stop")
		}
	})
	<-pollEntered
	sendDone := make(chan error, 1)
	go func() { sendDone <- a.SendTextContext(t.Context(), "123456", "shutdown test") }()
	<-sendEntered
	a.Disconnect()
	select {
	case <-sendCanceled:
	case <-time.After(time.Second):
		t.Fatal("send context did not cancel")
	}
	// The poll has exited, but the transport still owns an unfinished send.
	// Account exclusion must remain until that send returns.
	select {
	case err := <-done:
		t.Fatal("Connect returned before send cleanup", err)
	default:
	}
	lock, err := AcquireIMAccountLock("telegram", strconv.FormatInt(f.botID, 10))
	if err == nil {
		lock.Close()
		t.Fatal("account lock released before send cleanup")
	}
	release()
	select {
	case err := <-sendDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("send did not stop")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("poller did not stop")
	}
	f.router.admission.pairing.mu.Lock()
	ready := f.router.admission.pairing.platforms["telegram"].ready
	f.router.admission.pairing.mu.Unlock()
	if ready {
		t.Fatal("pairing remained ready after shutdown")
	}
	if a.IngressDeliveryReadiness(ChatIdentity{Platform: "telegram", AccountID: "840001", ChatID: "123456"}).Ready {
		t.Fatal("delivery remained ready")
	}
	lock, err = AcquireIMAccountLock("telegram", strconv.FormatInt(f.botID, 10))
	if err != nil {
		t.Fatal("shutdown retained account lock", err)
	}
	lock.Close()
}

func TestTelegramAdapterAccountLockAcrossStateDirectories(t *testing.T) {
	f1 := newTelegramGatewayFixture(t, 840002)
	f2 := newTelegramGatewayFixture(t, 840002)
	if f1.paths.StateDir == f2.paths.StateDir {
		t.Fatal("fixtures reused state directory")
	}
	a1, _ := f1.adapter(t, f1.botID)
	done := make(chan error, 1)
	go func() { done <- a1.Connect(); close(done) }()
	t.Cleanup(func() {
		a1.Disconnect()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("first poller did not stop")
		}
	})
	waitIngressCondition(t, func() bool { a1.mu.Lock(); defer a1.mu.Unlock(); return a1.ready })
	a2, api2 := f2.adapter(t, f2.botID)
	if err := a2.Connect(); err == nil {
		t.Fatal("same account acquired lock from separate state directory")
	}
	api2.mu.Lock()
	probed := api2.probed
	api2.mu.Unlock()
	if probed {
		t.Fatal("conflicting poller contacted getUpdates")
	}
	a1.Disconnect()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poller did not stop")
	}
	lock, err := AcquireIMAccountLock("telegram", "840002")
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}

func TestTelegramAdapterPollerConflictTerminatesAndRevokesReadiness(t *testing.T) {
	f := newTelegramGatewayFixture(t, 840003)
	a, base := f.adapter(t, f.botID)
	api := &telegramLifecycleAPI{telegramAPI: base}
	api.updatesFn = func(ctx context.Context, offset *int64, timeout int) ([]telegramUpdate, error) {
		if timeout == 0 {
			return base.GetUpdates(ctx, offset, timeout)
		}
		return nil, &telegramAPIError{Method: "getUpdates", Status: 409, Code: 409, Category: "poller_conflict"}
	}
	a.api = api
	err := a.Connect()
	var apiErr *telegramAPIError
	if !errors.As(err, &apiErr) || apiErr.Category != "poller_conflict" {
		t.Fatal(err)
	}
	f.router.admission.pairing.mu.Lock()
	ready := f.router.admission.pairing.platforms["telegram"].ready
	f.router.admission.pairing.mu.Unlock()
	if ready {
		t.Fatal("409 retained pairing readiness")
	}
	if err := a.SendTextContext(t.Context(), "123456", "result"); err == nil {
		t.Fatal("409 allowed outbound send")
	}
	lock, err := AcquireIMAccountLock("telegram", "840003")
	if err != nil {
		t.Fatal("409 retained account lock", err)
	}
	lock.Close()
}

func TestTelegramAdapterWebhookConflictStopsBeforePolling(t *testing.T) {
	f := newTelegramGatewayFixture(t, time.Now().UnixNano())
	a, _ := f.adapter(t, f.botID)
	var mu sync.Mutex
	methods := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		method := parts[len(parts)-1]
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "getMe":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"id": f.botID, "is_bot": true, "username": "GatewayBot"}})
		case "getWebhookInfo":
			_, _ = w.Write([]byte(`{"ok":true,"result":{"url":"https://example.invalid/telegram-hook"}}`))
		default:
			t.Errorf("webhook conflict unexpectedly invoked %s", method)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400}`))
		}
	}))
	defer server.Close()
	api := newTelegramAPI("webhook-test-token")
	api.baseURL = server.URL
	api.client = server.Client()
	a.api = api
	err := a.Connect()
	if err == nil || !strings.Contains(err.Error(), "webhook_conflict") {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]string(nil), methods...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []string{"getMe", "getWebhookInfo"}) {
		t.Fatalf("startup methods=%v", got)
	}
	if a.IngressDeliveryReadiness(ChatIdentity{Platform: "telegram", AccountID: strconv.FormatInt(f.botID, 10)}).Ready {
		t.Fatal("webhook conflict marked adapter ready")
	}
	cursor, err := f.service.store.Cursor("telegram", strconv.FormatInt(f.botID, 10))
	if err != nil || cursor.Generation != 0 {
		t.Fatal("webhook conflict committed cursor", cursor, err)
	}
	lock, err := AcquireIMAccountLock("telegram", strconv.FormatInt(f.botID, 10))
	if err != nil {
		t.Fatal("webhook conflict retained account lock", err)
	}
	lock.Close()
}

func (a *telegramLifecycleAPI) GetMe(ctx context.Context) (telegramUser, error) {
	if a.getMeFn != nil {
		return a.getMeFn(ctx)
	}
	return a.telegramAPI.GetMe(ctx)
}

func TestTelegramConnectShutdownDuringStartup(t *testing.T) {
	for _, category := range []string{"cancelled", "credentials"} {
		t.Run(category, func(t *testing.T) {
			f := newTelegramGatewayFixture(t, 840011)
			a, base := f.adapter(t, f.botID)
			entered := make(chan struct{})
			a.api = &telegramLifecycleAPI{telegramAPI: base, getMeFn: func(ctx context.Context) (telegramUser, error) {
				close(entered)
				<-ctx.Done()
				if category == "credentials" {
					return telegramUser{}, &telegramAPIError{Category: "credentials"}
				}
				return telegramUser{}, ctx.Err()
			}}
			done := make(chan error, 1)
			go func() { done <- a.Connect() }()
			<-entered
			a.Disconnect()
			select {
			case err := <-done:
				if category == "cancelled" && err != nil {
					t.Fatal(err)
				}
				if category == "credentials" {
					var failure *telegramAPIError
					if !errors.As(err, &failure) || failure.Category != category {
						t.Fatal("shutdown swallowed fatal error", err)
					}
				}
			case <-time.After(time.Second):
				t.Fatal("startup did not stop")
			}
		})
	}
}
