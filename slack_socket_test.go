package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

func slackSocketTestAdapter(t *testing.T) (*SlackAdapter, slackIdentity) {
	t.Helper()
	store := testConfigStore(t, "ims: {slack: {app_token: sample-app-token, bot_token: sample-bot-token, app_id: ATEST, team_id: TTEST}}\n")
	mgr := NewChordManager(store.Snapshot(), testPaths(t))
	mgr.store = store
	router := NewNotificationRouter(mgr)
	router.admission = NewAdmission(NewPairingService(store, false, nil))
	mgr.admission = router.admission
	db := NewIngressStore()
	ingress := NewIngressService(db, router)
	if err := ingress.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	adapter, err := NewSlackAdapter(store.Snapshot(), store.Snapshot().IMs[0], testPaths(t), router)
	if err != nil {
		t.Fatal(err)
	}
	account, _ := im.EncodeScopedID("account-v1:", "TTEST", "UBOT")
	identity := slackIdentity{TeamID: "TTEST", AppID: "ATEST", BotUserID: "UBOT", BotID: "BBOT", AccountID: account}
	adapter.identity, adapter.identityVerified = identity, true
	adapter.verifyIdentity = func(context.Context) (slackIdentity, error) { return identity, nil }
	router.SetAdapter(adapter)
	t.Cleanup(func() { adapter.cancel(); ingress.Close(); router.admission.Close(); router.Close(); _ = db.Close() })
	return adapter, identity
}

func startSlackSocketTest(t *testing.T, adapter *SlackAdapter, identity slackIdentity) (*websocket.Conn, <-chan error, context.CancelFunc) {
	t.Helper()
	connections := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			connections <- conn
		}
	}))
	ctx, cancel := context.WithCancel(t.Context())
	adapter.openSocket = func(context.Context) (string, error) { return "ws" + strings.TrimPrefix(server.URL, "http"), nil }
	adapter.dialSocket = func(ctx context.Context, address string) (*websocket.Conn, error) {
		conn, _, err := websocket.DefaultDialer.DialContext(ctx, address, nil)
		return conn, err
	}
	done := make(chan error, 1)
	go func() { done <- adapter.runSlackSocket(ctx, identity) }()
	var conn *websocket.Conn
	select {
	case conn = <-connections:
	case <-time.After(time.Second):
		t.Fatal("socket not dialed")
	}
	t.Cleanup(func() { cancel(); _ = conn.Close(); server.Close() })
	return conn, done, cancel
}

func waitSlackSocketDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("socket workers did not exit")
		return nil
	}
}

func TestSlackSocketHelloAndDisconnect(t *testing.T) {
	for _, reason := range []string{"refresh_requested", "warning", "link_disabled"} {
		t.Run(reason, func(t *testing.T) {
			a, id := slackSocketTestAdapter(t)
			conn, done, _ := startSlackSocketTest(t, a, id)
			if err := conn.WriteJSON(map[string]any{"type": "hello", "connection_info": map[string]string{"app_id": id.AppID}}); err != nil {
				t.Fatal(err)
			}
			waitIngressCondition(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.ready })
			if err := conn.WriteJSON(map[string]string{"type": "disconnect", "reason": reason}); err != nil {
				t.Fatal(err)
			}
			err := waitSlackSocketDone(t, done)
			if slackConnectionFatal(err) != (reason == "link_disabled") {
				t.Fatal(err)
			}
			a.mu.Lock()
			ready := a.ready
			a.mu.Unlock()
			if ready {
				t.Fatal("disconnected generation stayed ready")
			}
		})
	}
}

func TestSlackSocketHelloIdentityAndTimeout(t *testing.T) {
	for _, app := range []string{"", "AOTHER", "timeout"} {
		t.Run(app, func(t *testing.T) {
			a, id := slackSocketTestAdapter(t)
			a.helloTimeout = 25 * time.Millisecond
			conn, done, _ := startSlackSocketTest(t, a, id)
			if app != "timeout" {
				_ = conn.WriteJSON(map[string]any{"type": "hello", "connection_info": map[string]string{"app_id": app}})
			}
			err := waitSlackSocketDone(t, done)
			if slackConnectionFatal(err) != (app != "timeout") {
				t.Fatal(err)
			}
			if p := a.router.(*NotificationRouter).admission.pairing; func() bool {
				p.mu.Lock()
				defer p.mu.Unlock()
				return p.platforms["slack"] != nil && p.platforms["slack"].ready
			}() {
				t.Fatal("invalid hello became ready")
			}
		})
	}
}

func TestSlackSocketPingAndIgnoredACK(t *testing.T) {
	a, id := slackSocketTestAdapter(t)
	conn, done, cancel := startSlackSocketTest(t, a, id)
	_ = conn.WriteJSON(map[string]any{"type": "hello", "connection_info": map[string]string{"app_id": id.AppID}})
	waitIngressCondition(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.ready })
	pong := make(chan string, 1)
	conn.SetPongHandler(func(payload string) error { pong <- payload; return nil })
	_ = conn.WriteControl(websocket.PingMessage, []byte("heartbeat"), time.Now().Add(time.Second))
	_ = conn.WriteJSON(map[string]string{"type": "interactive", "envelope_id": "ignored"})
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if json.Unmarshal(raw, &ack) != nil || ack["envelope_id"] != "ignored" || ack["payload"] != nil {
		t.Fatal(string(raw))
	}
	select {
	case payload := <-pong:
		if payload != "heartbeat" {
			t.Fatal(payload)
		}
	case <-time.After(time.Second):
		t.Fatal("ping not echoed")
	}
	cancel()
	_ = waitSlackSocketDone(t, done)
}

func TestSlackSocketPingTimeoutAndIdentityRefresh(t *testing.T) {
	t.Run("ping", func(t *testing.T) {
		a, id := slackSocketTestAdapter(t)
		a.pingTimeout = 30 * time.Millisecond
		conn, done, _ := startSlackSocketTest(t, a, id)
		_ = conn.WriteJSON(map[string]any{"type": "hello", "connection_info": map[string]string{"app_id": id.AppID}})
		err := waitSlackSocketDone(t, done)
		if slackConnectionCategory(err) != "ping_timeout" {
			t.Fatal(err)
		}
	})
	t.Run("identity", func(t *testing.T) {
		a, id := slackSocketTestAdapter(t)
		a.verifyIdentity = func(context.Context) (slackIdentity, error) {
			changed := id
			changed.BotUserID = "UCHANGED"
			changed.AccountID = "other-account"
			return changed, nil
		}
		conn, done, _ := startSlackSocketTest(t, a, id)
		_ = conn.WriteJSON(map[string]any{"type": "hello", "connection_info": map[string]string{"app_id": id.AppID}})
		if err := waitSlackSocketDone(t, done); !slackConnectionFatal(err) {
			t.Fatal(err)
		}
	})
}

func TestSlackACKWriterRejectsExpiredAndOldGeneration(t *testing.T) {
	a, id := slackSocketTestAdapter(t)
	conn, done, cancel := startSlackSocketTest(t, a, id)
	_ = conn.WriteJSON(map[string]any{"type": "hello", "connection_info": map[string]string{"app_id": id.AppID}})
	waitIngressCondition(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.ready })
	a.mu.Lock()
	client, generation := a.conn, a.generation
	a.mu.Unlock()
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	queue := make(chan slackACK, 2)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		a.slackACKWriter(ctx, client, generation, queue, func(error) { t.Error("unexpected write") })
	}()
	for _, test := range []slackACK{{Generation: generation - 1, Deadline: time.Now().Add(time.Second), Data: []byte(`{"envelope_id":"old"}`), Done: make(chan error, 1)}, {Generation: generation, Deadline: time.Now().Add(-time.Second), Data: []byte(`{"envelope_id":"late"}`), Done: make(chan error, 1)}} {
		queue <- test
		select {
		case err := <-test.Done:
			if err == nil {
				t.Fatal("invalid ACK written")
			}
		case <-time.After(time.Second):
			t.Fatal("no writer completion")
		}
	}
	stop()
	<-writerDone
	cancel()
	_ = waitSlackSocketDone(t, done)
}

func TestSlackSocketURLAndFatalAuthentication(t *testing.T) {
	for _, address := range []string{"wss://wss.slack.com/link/?ticket=sample", "wss://wss-primary.slack.com/link", "wss://wss-backup.slack.com/link"} {
		if !validSlackSocketURL(address) {
			t.Fatal(address)
		}
	}
	for _, address := range []string{"ws://wss.slack.com/link", "wss://wss.slack.com.evil.test/link", "wss://user@wss.slack.com/link", "wss://wss.slack.com/link#fragment", "wss://wss.slack.com:8080/link"} {
		if validSlackSocketURL(address) {
			t.Fatal(address)
		}
	}
	if !slackConnectionFatal(errSlackFatal) || slackConnectionFatal(errors.New("network")) {
		t.Fatal("fatal classification")
	}
	for i := 0; i < 10; i++ {
		d := im.ReconnectBackoff(i)
		if d < 800*time.Millisecond || d > 36*time.Second {
			t.Fatal(d)
		}
	}
}

func TestSlackAdapterRequiresIngressAndStopsBeforeConnect(t *testing.T) {
	cfg := config.IMAdapterConfig{Slack: &config.SlackConfig{AppToken: "sample-app-token", BotToken: "sample-bot-token", AppID: "ATEST", TeamID: "TTEST"}}
	a, err := NewSlackAdapter(nil, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Connect(); err == nil {
		t.Fatal("started without durable ingress")
	}
	a.Disconnect()
	b, err := NewSlackAdapter(nil, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.Disconnect()
	if err = b.Connect(); err == nil {
		t.Fatal("restarted stopped adapter")
	}
}

func TestSlackAdapterReusesOneConversationCache(t *testing.T) {
	a, identity := slackSocketTestAdapter(t)
	first := a.inboundFor(identity)
	if first == nil {
		t.Fatal("adapter did not create an inbound cache")
	}
	if first != a.inboundFor(identity) {
		t.Fatal("adapter rebuilt the conversation cache between generations")
	}
	if first.identity != identity {
		t.Fatal("cache bound to the wrong installation identity")
	}
}

func TestSlackOversizedACKHintFallsBackToBareACK(t *testing.T) {
	queue := make(chan slackACK, 1)
	frame := slackSocketFrame{Type: "slash_commands", EnvelopeID: "big-hint", AcceptsResponsePayload: true}
	done := (&SlackAdapter{}).queueSlackACK(t.Context(), 1, frame, time.Now(), strings.Repeat("x", 3000), queue)
	if done == nil {
		t.Fatal("oversized hint dropped the ACK")
	}
	select {
	case ack := <-queue:
		var body map[string]json.RawMessage
		if json.Unmarshal(ack.Data, &body) != nil {
			t.Fatal(string(ack.Data))
		}
		if _, ok := body["payload"]; ok {
			t.Fatalf("oversized hint kept its payload: %s", ack.Data)
		}
		if string(body["envelope_id"]) != `"big-hint"` {
			t.Fatal(string(ack.Data))
		}
	case <-time.After(time.Second):
		t.Fatal("no ACK queued")
	}
}
