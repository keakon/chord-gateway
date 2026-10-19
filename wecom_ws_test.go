package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func wecomTestWS(t *testing.T, handler func(*websocket.Conn), callbacks wecomWSCallbacks, configure ...func(*wecomWS)) (*wecomWS, <-chan error) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(rw, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn)
	}))
	w := newWecomWS("test-bot", "do-not-log-this-secret", callbacks)
	w.endpoint = "ws" + strings.TrimPrefix(server.URL, "http")
	w.jitter = func(delay time.Duration) time.Duration { return delay }
	w.timing = wecomWSTiming{500 * time.Millisecond, 150 * time.Millisecond, 150 * time.Millisecond,
		time.Hour, 80 * time.Millisecond, 100 * time.Millisecond, 20 * time.Millisecond, 80 * time.Millisecond}
	for _, configure := range configure {
		configure(w)
	}
	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background()) }()
	t.Cleanup(func() { w.Stop(); server.Close() })
	return w, done
}

func wecomReadTestFrame(conn *websocket.Conn) (wecomWSFrame, error) {
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var frame wecomWSFrame
	err := conn.ReadJSON(&frame)
	return frame, err
}

func wecomTestACK(conn *websocket.Conn, id string, code *int) error {
	frame := wecomWSFrame{ErrCode: code}
	frame.Headers.ReqID = id
	return conn.WriteJSON(frame)
}

func wecomTestSubscribe(conn *websocket.Conn) error {
	frame, err := wecomReadTestFrame(conn)
	if err != nil {
		return err
	}
	if frame.Cmd != "aibot_subscribe" {
		return errors.New("expected subscribe")
	}
	code := 0
	return wecomTestACK(conn, frame.Headers.ReqID, &code)
}

func wecomWaitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for transport state")
}

func wecomWaitReady(t *testing.T, w *wecomWS) {
	t.Helper()
	wecomWaitFor(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.current != nil && w.current.ready
	})
}

func wecomRequireCategory(t *testing.T, err error, category string) {
	t.Helper()
	var transportError *WecomTransportError
	if !errors.As(err, &transportError) || transportError.Category != category {
		t.Fatalf("error = %v, want category %s", err, category)
	}
}

func TestWecomWSWireAndExactACK(t *testing.T) {
	frames := make(chan wecomWSFrame, 4)
	callback := make(chan []byte, 1)
	w, _ := wecomTestWS(t, func(conn *websocket.Conn) {
		frame, err := wecomReadTestFrame(conn)
		if err != nil {
			return
		}
		frames <- frame
		zero := 0
		wecomTestACK(conn, frame.Headers.ReqID, &zero)
		conn.WriteJSON(map[string]any{"cmd": "aibot_msg_callback", "body": wecomTestTextBody("one")})
		frame, err = wecomReadTestFrame(conn)
		if err != nil {
			return
		}
		frames <- frame
		// A req_id that shares a prefix is not the request's ACK.
		wecomTestACK(conn, frame.Headers.ReqID+"-wrong", &zero)
		wecomTestACK(conn, frame.Headers.ReqID, &zero)
		conn.ReadMessage()
	}, wecomWSCallbacks{OnFrame: func(_ context.Context, raw []byte) error { callback <- raw; return nil }})
	wecomWaitReady(t, w)
	if err := w.SendMarkdown(context.Background(), wecomSendTarget{ChatID: "target-user", ChatType: 1}, "safe markdown"); err != nil {
		t.Fatal(err)
	}
	subscribe, send := <-frames, <-frames
	if subscribe.Cmd != "aibot_subscribe" || send.Cmd != "aibot_send_msg" {
		t.Fatalf("unexpected commands: %s / %s", subscribe.Cmd, send.Cmd)
	}
	var credentials map[string]string
	json.Unmarshal(subscribe.Body, &credentials)
	if credentials["bot_id"] != w.botID || credentials["secret"] != w.secret {
		t.Fatal("subscription credentials missing")
	}
	var body struct {
		ChatID   string `json:"chatid"`
		ChatType int    `json:"chat_type"`
		MsgType  string `json:"msgtype"`
		Markdown struct {
			Content string `json:"content"`
		} `json:"markdown"`
	}
	json.Unmarshal(send.Body, &body)
	if body.ChatID != "target-user" || body.ChatType != 1 || body.MsgType != "markdown" || body.Markdown.Content != "safe markdown" {
		t.Fatalf("unexpected send fields: %#v", body)
	}
	for _, frame := range []wecomWSFrame{subscribe, send} {
		if !regexp.MustCompile(`^(subscribe|send)-1-[a-f0-9]{32}$`).MatchString(frame.Headers.ReqID) {
			t.Fatalf("request ID format = %q", frame.Headers.ReqID)
		}
	}
	wecomWaitFor(t, func() bool { return w.unknownACK.Load() == 1 })
	select {
	case raw := <-callback:
		if !strings.Contains(string(raw), "aibot_msg_callback") {
			t.Fatal("callback changed")
		}
	case <-time.After(time.Second):
		t.Fatal("callback not delivered")
	}
}

func TestWecomWSSubscribeRejectedIsTerminal(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected", true: "missing_errcode"}[missing], func(t *testing.T) {
			var connections atomic.Int32
			w, done := wecomTestWS(t, func(conn *websocket.Conn) {
				connections.Add(1)
				frame, err := wecomReadTestFrame(conn)
				if err != nil {
					return
				}
				code := 40001
				var ackCode *int
				if !missing {
					ackCode = &code
				}
				wecomTestACK(conn, frame.Headers.ReqID, ackCode)
				conn.ReadMessage()
			}, wecomWSCallbacks{})
			select {
			case err := <-done:
				wecomRequireCategory(t, err, "auth_failed")
			case <-time.After(time.Second):
				t.Fatal("authentication failure did not terminate")
			}
			w.Stop()
			if connections.Load() != 1 {
				t.Fatal("authentication failure reconnected")
			}
		})
	}
}

func TestWecomWSDisconnectedEventStopsReconnect(t *testing.T) {
	var connections atomic.Int32
	w, done := wecomTestWS(t, func(conn *websocket.Conn) {
		connections.Add(1)
		if wecomTestSubscribe(conn) != nil {
			return
		}
		conn.WriteJSON(map[string]any{"cmd": "aibot_event_callback", "body": map[string]any{"aibotid": "test-bot", "event": map[string]string{"eventtype": "disconnected_event"}}})
		conn.ReadMessage()
	}, wecomWSCallbacks{})
	select {
	case err := <-done:
		wecomRequireCategory(t, err, "duplicate_connection")
	case <-time.After(time.Second):
		t.Fatal("duplicate connection did not terminate")
	}
	w.Stop()
	if connections.Load() != 1 {
		t.Fatal("kicked transport reconnected")
	}
}

func TestWecomWSGenerationAndUnknownDelivery(t *testing.T) {
	var connections atomic.Int32
	firstID := make(chan string, 1)
	secondStarted := make(chan struct{})
	staleSent := make(chan struct{})
	allowACK := make(chan struct{})
	w, _ := wecomTestWS(t, func(conn *websocket.Conn) {
		n := connections.Add(1)
		if wecomTestSubscribe(conn) != nil {
			return
		}
		if n == 1 {
			frame, err := wecomReadTestFrame(conn)
			if err == nil {
				firstID <- frame.Headers.ReqID
			}
			return // The first send may have reached the platform, but no ACK exists.
		}
		close(secondStarted)
		frame, err := wecomReadTestFrame(conn)
		if err != nil {
			return
		}
		zero := 0
		wecomTestACK(conn, <-firstID, &zero)
		close(staleSent)
		<-allowACK
		wecomTestACK(conn, frame.Headers.ReqID, &zero)
		conn.ReadMessage()
	}, wecomWSCallbacks{})
	wecomWaitReady(t, w)
	wecomRequireCategory(t, w.SendMarkdown(context.Background(), wecomSendTarget{ChatID: "target", ChatType: 1}, "first"), "unknown_send")
	<-secondStarted
	wecomWaitReady(t, w)
	result := make(chan error, 1)
	go func() {
		result <- w.SendMarkdown(context.Background(), wecomSendTarget{ChatID: "target", ChatType: 1}, "second")
	}()
	<-staleSent
	wecomWaitFor(t, func() bool { return w.unknownACK.Load() == 1 })
	select {
	case err := <-result:
		t.Fatalf("old generation ACK completed a new send: %v", err)
	default:
	}
	close(allowACK)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if connections.Load() != 2 {
		t.Fatal("unexpected replay/reconnection")
	}
}

func TestWecomWSSendTimeoutLateACKAndRejection(t *testing.T) {
	firstID := make(chan string, 1)
	late := make(chan struct{})
	w, _ := wecomTestWS(t, func(conn *websocket.Conn) {
		if wecomTestSubscribe(conn) != nil {
			return
		}
		frame, err := wecomReadTestFrame(conn)
		if err != nil {
			return
		}
		firstID <- frame.Headers.ReqID
		<-late
		zero := 0
		wecomTestACK(conn, frame.Headers.ReqID, &zero)
		frame, err = wecomReadTestFrame(conn)
		if err != nil {
			return
		}
		code := 12345
		// Sensitive platform errmsg must never escape as a transport error.
		conn.WriteJSON(map[string]any{"headers": map[string]string{"req_id": frame.Headers.ReqID}, "errcode": code, "errmsg": "do-not-log-this-secret"})
		conn.ReadMessage()
	}, wecomWSCallbacks{})
	wecomWaitReady(t, w)
	wecomRequireCategory(t, w.SendMarkdown(context.Background(), wecomSendTarget{ChatID: "target", ChatType: 1}, "first"), "unknown_send")
	<-firstID
	close(late)
	wecomWaitFor(t, func() bool { return w.unknownACK.Load() == 1 })
	err := w.SendMarkdown(context.Background(), wecomSendTarget{ChatID: "target", ChatType: 1}, "second")
	wecomRequireCategory(t, err, "platform_rejected")
	var platformError *WecomTransportError
	errors.As(err, &platformError)
	if platformError.Code != 12345 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe rejection: %v", err)
	}
}

func TestWecomWSPendingBoundAndStop(t *testing.T) {
	w, _ := wecomTestWS(t, func(conn *websocket.Conn) {
		if wecomTestSubscribe(conn) != nil {
			return
		}
		for {
			if _, err := wecomReadTestFrame(conn); err != nil {
				return
			}
		}
	}, wecomWSCallbacks{}, func(w *wecomWS) { w.timing.send = time.Hour })
	wecomWaitReady(t, w)
	results := make(chan error, 64)
	for range 64 {
		go func() {
			results <- w.SendMarkdown(context.Background(), wecomSendTarget{ChatID: "target", ChatType: 1}, "body")
		}()
	}
	wecomWaitFor(t, func() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.current != nil && w.current.sends == 64 })
	wecomRequireCategory(t, w.SendMarkdown(context.Background(), wecomSendTarget{ChatID: "target", ChatType: 1}, "overflow"), "backpressure")
	var stops sync.WaitGroup
	for range 4 {
		stops.Add(1)
		go func() { defer stops.Done(); w.Stop() }()
	}
	stops.Wait()
	for range 64 {
		wecomRequireCategory(t, <-results, "unknown_send")
	}
}

func TestWecomWSReadyWaitsForIngressPreparation(t *testing.T) {
	preparing := make(chan struct{})
	w, _ := wecomTestWS(t, func(conn *websocket.Conn) {
		if wecomTestSubscribe(conn) != nil {
			return
		}
		conn.ReadMessage()
	}, wecomWSCallbacks{OnAuthenticated: func(ctx context.Context) error {
		close(preparing)
		<-ctx.Done()
		return ctx.Err()
	}})
	<-preparing
	wecomRequireCategory(t, w.SendMarkdown(context.Background(), wecomSendTarget{ChatID: "target", ChatType: 1}, "too soon"), "connection_unavailable")
	stopped := make(chan struct{})
	go func() { w.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel preparation")
	}
}

func TestWecomWSHeartbeatTimeoutReconnect(t *testing.T) {
	for _, ack := range []string{"absent", "missing_code", "rejected"} {
		t.Run(ack, func(t *testing.T) {
			var connections atomic.Int32
			pingReceived := make(chan struct{}, 1)
			w, _ := wecomTestWS(t, func(conn *websocket.Conn) {
				n := connections.Add(1)
				if wecomTestSubscribe(conn) != nil {
					return
				}
				for {
					frame, err := wecomReadTestFrame(conn)
					if err != nil {
						return
					}
					if frame.Cmd != "ping" {
						t.Error("unexpected heartbeat command")
						return
					}
					if n == 1 {
						pingReceived <- struct{}{}
						if ack == "missing_code" {
							wecomTestACK(conn, frame.Headers.ReqID, nil)
						}
						if ack == "rejected" {
							code := 7
							wecomTestACK(conn, frame.Headers.ReqID, &code)
						}
					} else {
						zero := 0
						wecomTestACK(conn, frame.Headers.ReqID, &zero)
					}
				}
			}, wecomWSCallbacks{}, func(w *wecomWS) { w.timing.heartbeat = 15 * time.Millisecond })
			select {
			case <-pingReceived:
			case <-time.After(time.Second):
				t.Fatal("no heartbeat was sent")
			}
			wecomWaitFor(t, func() bool { return connections.Load() >= 2 })
			wecomWaitReady(t, w)
			w.Stop()
		})
	}
}

func TestWecomWSStopCancelsDialAndIsIdempotent(t *testing.T) {
	w := newWecomWS("bot", "secret", wecomWSCallbacks{})
	entered := make(chan struct{})
	w.dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background()) }()
	<-entered
	w.Stop()
	w.Stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dial did not cancel")
	}
	fresh := newWecomWS("bot", "secret", wecomWSCallbacks{})
	fresh.Stop()
	wecomRequireCategory(t, fresh.Run(context.Background()), "connection_unavailable")
}

func TestWecomWSCallbackIdentityFailureIsTerminal(t *testing.T) {
	w, done := wecomTestWS(t, func(conn *websocket.Conn) {
		if wecomTestSubscribe(conn) != nil {
			return
		}
		conn.WriteJSON(map[string]any{"cmd": "aibot_msg_callback", "body": map[string]string{"aibotid": "another"}})
		conn.ReadMessage()
	}, wecomWSCallbacks{OnFrame: func(context.Context, []byte) error { return errors.New("unsafe platform payload") }})
	select {
	case err := <-done:
		wecomRequireCategory(t, err, "identity_mismatch")
	case <-time.After(time.Second):
		t.Fatal("identity mismatch did not terminate")
	}
	w.Stop()
}

func TestWecomWSRecoveryStagesCallbacksAndReadyCanSend(t *testing.T) {
	preparing := make(chan struct{})
	release := make(chan struct{})
	readySent := make(chan error, 1)
	times := make(chan time.Time, 3)
	w, _ := wecomTestWS(t, func(conn *websocket.Conn) {
		if wecomTestSubscribe(conn) != nil {
			return
		}
		for i := range 3 {
			conn.WriteJSON(map[string]any{"cmd": "aibot_msg_callback", "body": wecomTestTextBody(i)})
		}
		for {
			frame, err := wecomReadTestFrame(conn)
			if err != nil {
				return
			}
			zero := 0
			wecomTestACK(conn, frame.Headers.ReqID, &zero)
		}
	}, wecomWSCallbacks{
		OnAuthenticated: func(ctx context.Context) error {
			close(preparing)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		OnFrame: func(ctx context.Context, _ []byte) error {
			times <- ctx.Value(wecomReceivedAtContextKey{}).(time.Time)
			return nil
		},
	}, func(w *wecomWS) {
		w.callbacks.OnReady = func(ctx context.Context) error {
			// Publication of common Ready can immediately dispatch recovered output.
			err := w.SendMarkdown(ctx, wecomSendTarget{ChatID: "target", ChatType: 1}, "recovered output")
			readySent <- err
			return err
		}
	})
	<-preparing
	wecomWaitFor(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.current != nil && len(w.current.callbacks) == 3
	})
	select {
	case <-times:
		t.Fatal("callback entered ingress during preparation")
	default:
	}
	beforeRelease := time.Now()
	close(release)
	if err := <-readySent; err != nil {
		t.Fatalf("OnReady transport send failed: %v", err)
	}
	for range 3 {
		select {
		case receivedAt := <-times:
			if receivedAt.After(beforeRelease) {
				t.Fatal("staged callback lost its first receive time")
			}
		case <-time.After(time.Second):
			t.Fatal("staged callback lost")
		}
	}
	wecomWaitReady(t, w)
}

func TestWecomWSRecoveryDropsAreCounted(t *testing.T) {
	for _, count := range []int{3, 65} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			var dropped atomic.Int64
			var delivered atomic.Int64
			preparing, release := make(chan struct{}), make(chan struct{})
			w, done := wecomTestWS(t, func(conn *websocket.Conn) {
				if wecomTestSubscribe(conn) != nil {
					return
				}
				for i := range count {
					if conn.WriteJSON(map[string]any{"cmd": "aibot_msg_callback", "body": wecomTestTextBody(i)}) != nil {
						return
					}
				}
				for {
					f, err := wecomReadTestFrame(conn)
					if err != nil {
						return
					}
					zero := 0
					wecomTestACK(conn, f.Headers.ReqID, &zero)
				}
			}, wecomWSCallbacks{
				OnAuthenticated: func(ctx context.Context) error {
					close(preparing)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
				OnDroppedBeforeAccept: func(n int) { dropped.Add(int64(n)) },
				OnFrame:               func(context.Context, []byte) error { delivered.Add(1); return nil },
			})
			<-preparing
			if count == 3 {
				wecomWaitFor(t, func() bool {
					w.mu.Lock()
					defer w.mu.Unlock()
					return w.current != nil && len(w.current.callbacks) == 3
				})
				w.Stop()
				if dropped.Load() != 3 {
					t.Fatalf("dropped=%d", dropped.Load())
				}
				return
			}
			wecomWaitFor(t, func() bool { return dropped.Load() == 1 })
			select {
			case err := <-done:
				t.Fatalf("overflow terminated connection: %v", err)
			default:
			}
			close(release)
			wecomWaitFor(t, func() bool { return delivered.Load() == 64 })
			if err := w.SendMarkdown(t.Context(), wecomSendTarget{ChatID: "target", ChatType: 1}, "after overflow"); err != nil {
				t.Fatal(err)
			}
			if dropped.Load() != 1 {
				t.Fatalf("dropped=%d", dropped.Load())
			}
		})
	}
}

func TestWecomWSConcurrentSendHeartbeatAndControlPing(t *testing.T) {
	var sends, heartbeats atomic.Int32
	w, _ := wecomTestWS(t, func(conn *websocket.Conn) {
		if wecomTestSubscribe(conn) != nil {
			return
		}
		for {
			conn.WriteControl(websocket.PingMessage, []byte("control-ping"), time.Now().Add(time.Second))
			frame, err := wecomReadTestFrame(conn)
			if err != nil {
				return
			}
			switch frame.Cmd {
			case "ping":
				heartbeats.Add(1)
			case "aibot_send_msg":
				sends.Add(1)
			default:
				t.Errorf("unexpected concurrent command %s", frame.Cmd)
				return
			}
			zero := 0
			wecomTestACK(conn, frame.Headers.ReqID, &zero)
		}
	}, wecomWSCallbacks{}, func(w *wecomWS) { w.timing.heartbeat = 5 * time.Millisecond })
	wecomWaitReady(t, w)
	wecomWaitFor(t, func() bool { return heartbeats.Load() > 0 })
	results := make(chan error, 32)
	for range 32 {
		go func() {
			results <- w.SendMarkdown(context.Background(), wecomSendTarget{ChatID: "target", ChatType: 1}, "body")
		}()
	}
	for range 32 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if sends.Load() != 32 {
		t.Fatalf("received sends = %d", sends.Load())
	}
	wecomWaitFor(t, func() bool { return heartbeats.Load() > 1 })
}

func TestWecomWSEventAccountMismatchIsTerminal(t *testing.T) {
	_, done := wecomTestWS(t, func(conn *websocket.Conn) {
		if wecomTestSubscribe(conn) != nil {
			return
		}
		conn.WriteJSON(map[string]any{"cmd": "aibot_event_callback", "body": map[string]any{"aibotid": "wrong-bot", "event": map[string]string{"eventtype": "enter_chat"}}})
		conn.ReadMessage()
	}, wecomWSCallbacks{})
	select {
	case err := <-done:
		wecomRequireCategory(t, err, "identity_mismatch")
	case <-time.After(time.Second):
		t.Fatal("event identity mismatch was ignored")
	}
}

func TestWecomWSUnknownSubscribeACKDoesNotAuthenticate(t *testing.T) {
	seen := make(chan struct{}, 1)
	w, _ := wecomTestWS(t, func(conn *websocket.Conn) {
		frame, err := wecomReadTestFrame(conn)
		if err != nil {
			return
		}
		zero := 0
		wecomTestACK(conn, frame.Headers.ReqID+"extra", &zero)
		select {
		case seen <- struct{}{}:
		default:
		}
		conn.ReadMessage()
	}, wecomWSCallbacks{})
	<-seen
	wecomWaitFor(t, func() bool { return w.unknownACK.Load() > 0 })
	wecomRequireCategory(t, w.SendMarkdown(context.Background(), wecomSendTarget{ChatID: "target", ChatType: 1}, "body"), "connection_unavailable")
	wecomWaitFor(t, func() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.generation > 1 })
}

type wecomTestBlockingConn struct {
	net.Conn
	enabled   atomic.Bool
	entered   chan struct{}
	closed    chan struct{}
	writeOnce sync.Once
	closeOnce sync.Once
}

func (c *wecomTestBlockingConn) Write(data []byte) (int, error) {
	if c.enabled.Load() {
		c.writeOnce.Do(func() { close(c.entered) })
		<-c.closed
		return 0, net.ErrClosed
	}
	return c.Conn.Write(data)
}

func (c *wecomTestBlockingConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func TestWecomWSStopInterruptsBlockedSocketWrite(t *testing.T) {
	connection := make(chan *wecomTestBlockingConn, 1)
	w, _ := wecomTestWS(t, func(conn *websocket.Conn) {
		if wecomTestSubscribe(conn) != nil {
			return
		}
		conn.ReadMessage()
	}, wecomWSCallbacks{}, func(w *wecomWS) {
		w.dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			wrapped := &wecomTestBlockingConn{Conn: conn, entered: make(chan struct{}), closed: make(chan struct{})}
			connection <- wrapped
			return wrapped, nil
		}
	})
	wecomWaitReady(t, w)
	conn := <-connection
	conn.enabled.Store(true)
	result := make(chan error, 1)
	go func() {
		result <- w.SendMarkdown(context.Background(), wecomSendTarget{ChatID: "target", ChatType: 1}, "body")
	}()
	<-conn.entered
	stopped := make(chan struct{})
	go func() { w.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop hung on blocked socket Write")
	}
	wecomRequireCategory(t, <-result, "unknown_send")
}

func TestWecomWSStopCancelsBackoff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { rw.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	unavailable := make(chan struct{}, 1)
	w := newWecomWS("bot", "secret", wecomWSCallbacks{OnUnavailable: func(error) {
		select {
		case unavailable <- struct{}{}:
		default:
		}
	}})
	w.endpoint = "ws" + strings.TrimPrefix(server.URL, "http")
	w.timing.backoff = time.Hour
	w.timing.maxBackoff = time.Hour
	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background()) }()
	<-unavailable
	w.Stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop waited for backoff")
	}
}

func wecomTestTextBody(id any) map[string]any {
	return map[string]any{"aibotid": "test-bot", "msgid": fmt.Sprint(id), "msgtype": "text", "chattype": "single", "from": map[string]string{"userid": "owner"}, "text": map[string]string{"content": "input"}}
}

func TestWecomWSInvalidCallbacksDoNotStopOrEnterRecovery(t *testing.T) {
	preparing := make(chan struct{})
	dropped := make(chan int, 1)
	rejected := make(chan string, 4)
	w, _ := wecomTestWS(t, func(conn *websocket.Conn) {
		if wecomTestSubscribe(conn) != nil {
			return
		}
		// A missing bot identity grants no authority to a disconnected event.
		conn.WriteJSON(map[string]any{"cmd": "aibot_event_callback", "body": map[string]any{"event": map[string]string{"eventtype": "disconnected_event"}}})
		// Unknown commands are ignored even when they contain another bot's ID.
		conn.WriteJSON(map[string]any{"cmd": "unknown", "body": map[string]string{"aibotid": "wrong"}})
		// Supported event identities are validated, but events are not lost input.
		conn.WriteJSON(map[string]any{"cmd": "aibot_event_callback", "body": map[string]any{"aibotid": "test-bot", "event": map[string]string{"eventtype": "enter_chat"}}})
		conn.WriteJSON(map[string]any{"cmd": "aibot_msg_callback", "body": map[string]string{"aibotid": "test-bot", "msgtype": "voice"}})
		// Malformed text callbacks are reported instead of vanishing silently.
		conn.WriteJSON(map[string]any{"cmd": "aibot_msg_callback", "body": map[string]string{"aibotid": "test-bot", "msgtype": "text"}})
		skewed := wecomTestTextBody("skewed")
		skewed["create_time"] = 1
		conn.WriteJSON(map[string]any{"cmd": "aibot_msg_callback", "body": skewed})
		conn.WriteJSON(map[string]any{"cmd": "aibot_msg_callback", "body": wecomTestTextBody("valid")})
		conn.ReadMessage()
	}, wecomWSCallbacks{
		OnAuthenticated:       func(ctx context.Context) error { close(preparing); <-ctx.Done(); return ctx.Err() },
		OnDroppedBeforeAccept: func(count int) { dropped <- count },
		OnCallbackRejected:    func(reason string) { rejected <- reason },
	})
	<-preparing
	wecomWaitFor(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.current != nil && len(w.current.callbacks) == 1
	})
	reasons := map[string]bool{}
	for range 2 {
		select {
		case reason := <-rejected:
			reasons[reason] = true
		case <-time.After(time.Second):
			t.Fatalf("malformed text callbacks were not reported: %v", reasons)
		}
	}
	if !reasons["invalid_callback"] || !reasons["source_time_out_of_range"] || len(reasons) != 2 {
		t.Fatalf("reported reasons: %v", reasons)
	}
	w.Stop()
	if count := <-dropped; count != 1 {
		t.Fatalf("non-business callbacks counted as lost: %d", count)
	}
	select {
	case reason := <-rejected:
		t.Fatalf("unsupported callbacks must not be reported: %q", reason)
	default:
	}
}

func TestWecomWSMissingSendErrcodeIsUnknown(t *testing.T) {
	w, _ := wecomTestWS(t, func(conn *websocket.Conn) {
		if wecomTestSubscribe(conn) != nil {
			return
		}
		frame, err := wecomReadTestFrame(conn)
		if err != nil {
			return
		}
		wecomTestACK(conn, frame.Headers.ReqID, nil)
		conn.ReadMessage()
	}, wecomWSCallbacks{})
	wecomWaitReady(t, w)
	wecomRequireCategory(t, w.SendMarkdown(context.Background(), wecomSendTarget{ChatID: "target", ChatType: 1}, "body"), "unknown_send")
}

func TestWecomWSRecoveryByteBound(t *testing.T) {
	var dropped, delivered atomic.Int64
	release := make(chan struct{})
	w, done := wecomTestWS(t, func(conn *websocket.Conn) {
		if wecomTestSubscribe(conn) != nil {
			return
		}
		body := wecomTestTextBody("padded")
		body["unused_padding"] = strings.Repeat("x", 900000)
		for range 5 {
			if conn.WriteJSON(map[string]any{"cmd": "aibot_msg_callback", "body": body}) != nil {
				return
			}
		}
		conn.ReadMessage()
	}, wecomWSCallbacks{
		OnAuthenticated: func(ctx context.Context) error {
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		OnDroppedBeforeAccept: func(n int) { dropped.Add(int64(n)) },
		OnFrame:               func(context.Context, []byte) error { delivered.Add(1); return nil },
	})
	wecomWaitFor(t, func() bool { return dropped.Load() == 1 })
	w.mu.Lock()
	bytes := w.current.stagedBytes
	w.mu.Unlock()
	if bytes > wecomWSStagedMaxBytes {
		t.Fatalf("staged bytes=%d", bytes)
	}
	select {
	case err := <-done:
		t.Fatalf("byte overflow terminated connection: %v", err)
	default:
	}
	close(release)
	wecomWaitFor(t, func() bool { return delivered.Load() == 4 })
	w.Stop()
	if dropped.Load() != 1 {
		t.Fatalf("dropped=%d", dropped.Load())
	}
}

func TestWecomWSRecoveryPreservesReaderCapturedContext(t *testing.T) {
	type capturedKey struct{}
	var runtime atomic.Int64
	runtime.Store(1)
	captured := make(chan struct{})
	release := make(chan struct{})
	delivered := make(chan int64, 1)
	_, _ = wecomTestWS(t, func(conn *websocket.Conn) {
		if wecomTestSubscribe(conn) != nil {
			return
		}
		body := wecomTestTextBody("control")
		body["text"] = map[string]string{"content": "/approve"}
		conn.WriteJSON(map[string]any{"cmd": "aibot_msg_callback", "body": body})
		conn.ReadMessage()
	}, wecomWSCallbacks{
		OnAuthenticated: func(ctx context.Context) error {
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		CaptureFrame: func(ctx context.Context, _ []byte) context.Context {
			ctx = context.WithValue(ctx, capturedKey{}, runtime.Load())
			close(captured)
			return ctx
		},
		OnFrame: func(ctx context.Context, _ []byte) error { delivered <- ctx.Value(capturedKey{}).(int64); return nil },
	})
	select {
	case <-captured:
	case <-time.After(time.Second):
		t.Fatal("reader did not capture before recovery")
	}
	runtime.Store(2)
	close(release)
	select {
	case state := <-delivered:
		if state != 1 {
			t.Fatalf("control captured later runtime %d", state)
		}
	case <-time.After(time.Second):
		t.Fatal("captured callback not delivered")
	}
}

func TestWecomProactiveTargetAddressing(t *testing.T) {
	for _, tc := range []struct {
		kind string
		wire int
	}{{"private", 1}, {"group", 2}} {
		t.Run(tc.kind, func(t *testing.T) {
			bodies := make(chan map[string]json.RawMessage, 1)
			w, _ := wecomTestWS(t, func(conn *websocket.Conn) {
				if wecomTestSubscribe(conn) != nil {
					return
				}
				frame, err := wecomReadTestFrame(conn)
				if err != nil {
					return
				}
				var body map[string]json.RawMessage
				if json.Unmarshal(frame.Body, &body) != nil {
					return
				}
				bodies <- body
				zero := 0
				_ = wecomTestACK(conn, frame.Headers.ReqID, &zero)
				_, _, _ = conn.ReadMessage()
			}, wecomWSCallbacks{})
			wecomWaitReady(t, w)
			encoded, err := EncodeWecomTarget(tc.kind, "same-id")
			if err != nil {
				t.Fatal(err)
			}
			target, err := DecodeWecomTarget(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.SendMarkdown(t.Context(), target, "result"); err != nil {
				t.Fatal(err)
			}
			body := <-bodies
			if string(body["chat_type"]) != fmt.Sprint(tc.wire) || string(body["chatid"]) != `"same-id"` {
				t.Fatalf("incorrect addressing: %v", body)
			}
		})
	}
}
