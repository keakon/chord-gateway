package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/keakon/chord-gateway/internal/im"
)

type dingtalkTestACK struct {
	Code    int               `json:"code"`
	Message string            `json:"message"`
	Headers map[string]string `json:"headers"`
	Data    string            `json:"data"`
}

func dingtalkTestFrame(kind, topic, id, data string) DingtalkFrame {
	encode := func(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }
	return DingtalkFrame{SpecVersion: "1.0", Type: kind, Headers: map[string]json.RawMessage{
		"topic": encode(topic), "messageId": encode(id), "contentType": encode("application/json"), "time": encode(int64(1700000000000)),
	}, Data: data}
}

func dingtalkTestStream(callbacks dingtalkStreamCallbacks, endpoint string) *dingtalkStream {
	s := newDingtalkStreamWithProxy(&dingtalkAPI{}, callbacks, nil)
	s.allowInsecureEndpoint = true
	s.openConnection = func(context.Context) (string, string, error) { return endpoint, "test-ticket", nil }
	s.dialer.Proxy = nil
	s.backoff = func(int) time.Duration { return time.Millisecond }
	s.pingInterval = time.Hour
	s.writeTimeout = time.Second
	return s
}

func dingtalkReadTestACK(conn *websocket.Conn) (dingtalkTestACK, error) {
	var ack dingtalkTestACK
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return ack, err
	}
	err = json.Unmarshal(raw, &ack)
	return ack, err
}

func TestDingtalkStreamEndpointValidation(t *testing.T) {
	for _, endpoint := range []string{"", "ws://example.invalid/connect", "https://example.invalid/connect", "wss:///connect", "wss://user:secret@example.invalid/connect", "wss://example.invalid/connect#fragment", "wss://example.invalid/connect#", "wss://example.invalid/connect?invalid=%zz", "://bad"} {
		t.Run(endpoint, func(t *testing.T) {
			result, err := dingtalkStreamURL(endpoint, "ticket", false)
			if result != "" || !dingtalkStreamIsTerminal(err) {
				t.Fatalf("invalid endpoint accepted: result=%q err=%v", result, err)
			}
		})
	}
	result, err := dingtalkStreamURL("wss://example.invalid/custom?existing=value&ticket=old", "ticket +/?&", false)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(result)
	if u.Path != "/custom" || u.Query().Get("existing") != "value" || u.Query().Get("ticket") != "ticket +/?&" {
		t.Fatalf("bad encoded registration URL: %q", result)
	}
	if _, err := dingtalkStreamURL("wss://example.invalid", "", false); err == nil {
		t.Fatal("empty ticket accepted")
	}
}

func TestDingtalkStreamProtocolAndPersistenceACK(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan im.ReceiveResult, 1)
	received := make(chan DingtalkFrame, 1)
	serverResult := make(chan error, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverResult <- err
			return
		}
		defer conn.Close()
		callback := dingtalkTestFrame("CALLBACK", DingtalkRobotTopic, "delivery-1", `{"msgId":"message-1"}`)
		if err = conn.WriteJSON(callback); err != nil {
			serverResult <- err
			return
		}
		// Callback persistence is blocked; the same reader must still reply to
		// SYSTEM pings and refuse unsupported protocol messages immediately.
		frames := []DingtalkFrame{
			dingtalkTestFrame("SYSTEM", "ping", "ping-1", `{"opaque":"opaque-value", "extra":true}`),
			dingtalkTestFrame("EVENT", "*", "event-1", `{}`),
			dingtalkTestFrame("CALLBACK", "/unsupported", "unsupported-1", `{}`),
			dingtalkTestFrame("CALLBACK", DingtalkRobotTopic, "invalid-version", `{}`),
			dingtalkTestFrame("CALLBACK", DingtalkRobotTopic, "invalid-content", `{}`),
		}
		frames[0].SpecVersion = "" // SYSTEM samples legitimately omit this.
		frames[3].SpecVersion = "2.0"
		frames[4].Headers["contentType"] = json.RawMessage(`42`)
		for i, frame := range frames {
			if err = conn.WriteJSON(frame); err != nil {
				serverResult <- err
				return
			}
			ack, readErr := dingtalkReadTestACK(conn)
			expected := 200
			if i == 1 || i == 2 {
				expected = 404
			}
			if readErr != nil || ack.Code != expected || ack.Headers["messageId"] != frame.headerString("messageId") || ack.Headers["contentType"] != "application/json" {
				serverResult <- fmt.Errorf("protocol ACK %d: %+v error=%v", i, ack, readErr)
				return
			}
			if i == 0 && ack.Data != frame.Data {
				serverResult <- errors.New("SYSTEM ping data was modified")
				return
			}
		}
		result <- im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "sensitive-body-should-not-be-echoed"}
		ack, err := dingtalkReadTestACK(conn)
		if err != nil || ack.Code != 500 || ack.Message != "internal_error" || ack.Data != `{"response":null}` || ack.Headers["messageId"] != "delivery-1" {
			serverResult <- fmt.Errorf("persistence ACK: %+v error=%v", ack, err)
			return
		}
		serverResult <- nil
		<-ctx.Done()
	}))
	defer server.Close()
	s := dingtalkTestStream(dingtalkStreamCallbacks{OnCallback: func(_ context.Context, frame DingtalkFrame) (<-chan im.ReceiveResult, error) {
		received <- frame
		return result, nil
	}}, "ws"+strings.TrimPrefix(server.URL, "http"))
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	frame := WaitTestForTest(t, received)
	if frame.headerString("time") != "1700000000000" {
		t.Fatalf("numeric timestamp lost: %q", frame.headerString("time"))
	}
	if err := WaitTestForTest(t, serverResult); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := WaitTestForTest(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestDingtalkStreamAdmissionResultCodes(t *testing.T) {
	for _, test := range []struct {
		disposition im.ReceiveDisposition
		code        int
	}{{im.ReceiveAccepted, 200}, {im.ReceiveDuplicate, 200}, {im.ReceiveRejected, 200}, {im.ReceiveRetryable, 500}, {im.ReceiveUncertain, 500}, {"", 500}} {
		t.Run(string(test.disposition), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			outcome := make(chan dingtalkTestACK, 1)
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				frame := dingtalkTestFrame("CALLBACK", DingtalkRobotTopic, "delivery", `{}`)
				delete(frame.Headers, "contentType")
				_ = conn.WriteJSON(frame)
				ack, err := dingtalkReadTestACK(conn)
				if err != nil {
					t.Error(err)
					return
				}
				outcome <- ack
				<-ctx.Done()
			}))
			defer server.Close()
			s := dingtalkTestStream(dingtalkStreamCallbacks{OnCallback: func(context.Context, DingtalkFrame) (<-chan im.ReceiveResult, error) {
				result := make(chan im.ReceiveResult, 1)
				result <- im.ReceiveResult{Disposition: test.disposition}
				return result, nil
			}}, "ws"+strings.TrimPrefix(server.URL, "http"))
			done := make(chan error, 1)
			go func() { done <- s.Run(ctx) }()
			if ack := WaitTestForTest(t, outcome); ack.Code != test.code {
				t.Fatalf("got %+v", ack)
			}
			cancel()
			WaitTestForTest(t, done)
		})
	}
}

func TestDingtalkStreamHandshakeReregistersTicket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var registrations atomic.Int32
	tickets := make(chan string, 2)
	connected := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ticket := r.URL.Query().Get("ticket")
		tickets <- ticket
		if ticket == "ticket-1" {
			http.Error(w, "handshake rejected", http.StatusBadGateway)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		<-ctx.Done()
	}))
	defer server.Close()
	s := dingtalkTestStream(dingtalkStreamCallbacks{OnConnected: func(context.Context) error { connected <- struct{}{}; return nil }}, "")
	s.openConnection = func(context.Context) (string, string, error) {
		count := registrations.Add(1)
		return "ws" + strings.TrimPrefix(server.URL, "http"), fmt.Sprintf("ticket-%d", count), nil
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	WaitTestForTest(t, connected)
	if first, second := WaitTestForTest(t, tickets), WaitTestForTest(t, tickets); first != "ticket-1" || second != "ticket-2" {
		t.Fatalf("ticket reused: %q %q", first, second)
	}
	cancel()
	WaitTestForTest(t, done)
	if registrations.Load() != 2 {
		t.Fatalf("unexpected registration count: %d", registrations.Load())
	}
}

func TestDingtalkStreamAuthenticationStopsRetry(t *testing.T) {
	var registrations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "credential-body", http.StatusUnauthorized)
	}))
	defer server.Close()
	s := dingtalkTestStream(dingtalkStreamCallbacks{}, "")
	s.openConnection = func(context.Context) (string, string, error) {
		registrations.Add(1)
		return "ws" + strings.TrimPrefix(server.URL, "http"), "secret-ticket", nil
	}
	err := s.Run(context.Background())
	if !dingtalkStreamIsTerminal(err) || registrations.Load() != 1 || strings.Contains(err.Error(), "secret-ticket") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("auth retry/leak: count=%d err=%v", registrations.Load(), err)
	}
}

func TestDingtalkStreamDisconnectNoACKAndGenerationJoin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var connections atomic.Int32
	var unavailable atomic.Int32
	second := make(chan struct{}, 1)
	oldClosed := make(chan error, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if connections.Add(1) == 1 {
			_ = conn.WriteJSON(dingtalkTestFrame("SYSTEM", "disconnect", "", `{"reason":"expired"}`))
			_, _, err = conn.ReadMessage()
			oldClosed <- err
			return
		}
		<-ctx.Done()
	}))
	defer server.Close()
	var ready atomic.Int32
	s := dingtalkTestStream(dingtalkStreamCallbacks{OnUnavailable: func() { unavailable.Add(1) }, OnConnected: func(context.Context) error {
		if ready.Add(1) == 2 {
			if unavailable.Load() != 1 {
				t.Error("new connection became ready before old unavailable")
			}
			second <- struct{}{}
		}
		return nil
	}}, "ws"+strings.TrimPrefix(server.URL, "http"))
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	WaitTestForTest(t, second)
	if err := WaitTestForTest(t, oldClosed); err == nil {
		t.Fatal("disconnect received an ACK")
	}
	cancel()
	WaitTestForTest(t, done)
	if unavailable.Load() != 2 {
		t.Fatalf("unavailable count=%d", unavailable.Load())
	}
}

func TestDingtalkStreamHeartbeatRequiresMatchingPong(t *testing.T) {
	for _, matching := range []bool{true, false} {
		t.Run(fmt.Sprint(matching), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var connections atomic.Int32
			pings := make(chan string, 16)
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				connections.Add(1)
				conn.SetPingHandler(func(payload string) error {
					pings <- payload
					if !matching {
						payload = "wrong-pong"
					}
					return conn.WriteControl(websocket.PongMessage, []byte(payload), time.Now().Add(time.Second))
				})
				for {
					if _, _, err = conn.ReadMessage(); err != nil {
						return
					}
				}
			}))
			defer server.Close()
			second := make(chan struct{}, 1)
			var ready atomic.Int32
			s := dingtalkTestStream(dingtalkStreamCallbacks{OnConnected: func(context.Context) error {
				if ready.Add(1) == 2 {
					second <- struct{}{}
				}
				return nil
			}}, "ws"+strings.TrimPrefix(server.URL, "http"))
			s.pingInterval = 15 * time.Millisecond
			s.pongTimeout = 30 * time.Millisecond
			done := make(chan error, 1)
			go func() { done <- s.Run(ctx) }()
			first := WaitTestForTest(t, pings)
			if len(first) != 32 {
				t.Fatalf("ping token=%q", first)
			}
			if matching {
				for range 2 {
					if next := WaitTestForTest(t, pings); next == first {
						t.Fatal("ping token was reused")
					}
				}
				if connections.Load() != 1 {
					t.Fatal("matching pong reconnected")
				}
			} else {
				WaitTestForTest(t, second)
			}
			cancel()
			WaitTestForTest(t, done)
		})
	}
}

func TestDingtalkStreamCancelJoinsAdmissionWaiters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	captured := make(chan context.Context, 1)
	observed := make(chan im.ReceiveResult, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(dingtalkTestFrame("CALLBACK", DingtalkRobotTopic, "delivery", `{}`))
		<-ctx.Done()
	}))
	defer server.Close()
	s := dingtalkTestStream(dingtalkStreamCallbacks{OnReceiveResult: func(result im.ReceiveResult) { observed <- result }, OnCallback: func(ctx context.Context, _ DingtalkFrame) (<-chan im.ReceiveResult, error) {
		captured <- ctx
		return make(chan im.ReceiveResult), nil
	}}, "ws"+strings.TrimPrefix(server.URL, "http"))
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	workerCtx := WaitTestForTest(t, captured)
	cancel()
	WaitTestForTest(t, done)
	if workerCtx.Err() == nil {
		t.Fatal("admission waiter context survives disconnect")
	}
	if outcome := WaitTestForTest(t, observed); outcome.Disposition != im.ReceiveUncertain {
		t.Fatalf("cancellation fabricated a known rejection: %+v", outcome)
	}
}

func TestDingtalkStreamCancelStalledHandshake(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { request <- struct{}{}; <-ctx.Done() }))
	defer server.Close()
	s := dingtalkTestStream(dingtalkStreamCallbacks{}, "ws"+strings.TrimPrefix(server.URL, "http"))
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	WaitTestForTest(t, request)
	started := time.Now()
	cancel()
	WaitTestForTest(t, done)
	if time.Since(started) > time.Second {
		t.Fatal("cancel waited for handshake deadline")
	}
}

func TestDingtalkStreamCancelRegistrationAndBackoff(t *testing.T) {
	for _, duringRegistration := range []bool{true, false} {
		t.Run(fmt.Sprint(duringRegistration), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			registration := make(chan struct{}, 1)
			var calls atomic.Int32
			s := dingtalkTestStream(dingtalkStreamCallbacks{}, "")
			s.openConnection = func(ctx context.Context) (string, string, error) {
				calls.Add(1)
				registration <- struct{}{}
				if duringRegistration {
					<-ctx.Done()
				}
				return "", "", errors.New("registration_failed")
			}
			s.backoff = func(int) time.Duration { return time.Hour }
			done := make(chan error, 1)
			go func() { done <- s.Run(ctx) }()
			WaitTestForTest(t, registration)
			cancel()
			WaitTestForTest(t, done)
			if calls.Load() != 1 {
				t.Fatalf("registration after stop: %d", calls.Load())
			}
		})
	}
}

func TestDingtalkStreamTerminalCallbackStopsRetry(t *testing.T) {
	var callbacks atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(dingtalkTestFrame("CALLBACK", DingtalkRobotTopic, "delivery", `{}`))
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	s := dingtalkTestStream(dingtalkStreamCallbacks{OnCallback: func(context.Context, DingtalkFrame) (<-chan im.ReceiveResult, error) {
		callbacks.Add(1)
		return nil, &dingtalkStreamTerminalError{category: "tenant_mismatch"}
	}}, "ws"+strings.TrimPrefix(server.URL, "http"))
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	err := WaitTestForTest(t, done)
	if !dingtalkStreamIsTerminal(err) || !strings.Contains(err.Error(), "tenant_mismatch") || callbacks.Load() != 1 {
		t.Fatalf("terminal callback retried: calls=%d err=%v", callbacks.Load(), err)
	}
}

func TestDingtalkStreamAdmissionBackpressureIsBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var captured atomic.Int32
	var lost atomic.Int32
	diagnostic := make(chan string, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for i := range dingtalkStreamReceiptCapacity + 1 {
			if conn.WriteJSON(dingtalkTestFrame("CALLBACK", DingtalkRobotTopic, fmt.Sprintf("delivery-%d", i), `{}`)) != nil {
				return
			}
		}
		<-ctx.Done()
	}))
	defer server.Close()
	s := dingtalkTestStream(dingtalkStreamCallbacks{OnReceiveResult: func(result im.ReceiveResult) {
		if result.Disposition == im.ReceiveRetryable && result.Reason == "stream_capacity" {
			lost.Add(1)
		}
	}, OnDiagnostic: func(category string) {
		if category != "connected" {
			diagnostic <- category
		}
	}, OnCallback: func(context.Context, DingtalkFrame) (<-chan im.ReceiveResult, error) {
		captured.Add(1)
		return make(chan im.ReceiveResult), nil
	}}, "ws"+strings.TrimPrefix(server.URL, "http"))
	// Avoid a second admission batch while the test examines the first one.
	s.backoff = func(int) time.Duration { return time.Hour }
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	if category := WaitTestForTest(t, diagnostic); category != "backpressure" {
		t.Fatal(category)
	}
	if captured.Load() != dingtalkStreamReceiptCapacity {
		t.Fatalf("unbounded admission capture: %d", captured.Load())
	}
	cancel()
	WaitTestForTest(t, done)
	if lost.Load() != 1 {
		t.Fatalf("lost before submit observations=%d", lost.Load())
	}
}

func TestDingtalkStreamControlPriorityAndQueueOverflow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverSocket := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			serverSocket <- conn
		}
		<-ctx.Done()
	}))
	defer server.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	conn := WaitTestForTest(t, serverSocket)
	defer conn.Close()
	s := dingtalkTestStream(dingtalkStreamCallbacks{}, "")
	c := &dingtalkStreamConnection{stream: s, conn: conn, ctx: ctx, cancel: cancel, control: make(chan dingtalkStreamWrite, dingtalkStreamControlCapacity), receipts: make(chan dingtalkStreamWrite, dingtalkStreamReceiptCapacity), errors: make(chan error, 1)}
	c.receipts <- dingtalkStreamWrite{kind: websocket.TextMessage, data: []byte(`{"code":500}`)}
	c.control <- dingtalkStreamWrite{kind: websocket.TextMessage, data: []byte(`{"code":200}`)}
	var writer sync.WaitGroup
	writer.Go(c.writer)
	first, err := dingtalkReadTestACK(client)
	if err != nil || first.Code != 200 {
		t.Fatalf("control did not win: %+v err=%v", first, err)
	}
	second, err := dingtalkReadTestACK(client)
	if err != nil || second.Code != 500 {
		t.Fatalf("receipt lost: %+v err=%v", second, err)
	}
	cancel()
	writer.Wait()
	// Both queue capacities are fixed; a saturated queue closes rather than
	// blocking the reader. This branch runs without a concurrent writer.
	for _, capacity := range []int{dingtalkStreamControlCapacity, dingtalkStreamReceiptCapacity} {
		queueCtx, queueCancel := context.WithCancel(context.Background())
		queue := make(chan dingtalkStreamWrite, capacity)
		overflow := &dingtalkStreamConnection{stream: s, conn: conn, ctx: queueCtx, cancel: queueCancel, errors: make(chan error, 1)}
		for range capacity {
			if !overflow.enqueue(queue, dingtalkStreamWrite{}) {
				t.Fatal("queue filled too early")
			}
		}
		if overflow.enqueue(queue, dingtalkStreamWrite{}) || queueCtx.Err() == nil {
			t.Fatal("saturated queue was not closed")
		}
		queueCancel()
	}
}

func TestDingtalkStreamOversizedFrameReconnects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var connections atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if connections.Add(1) == 1 {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", dingtalkStreamFrameLimit+1)))
		}
		<-ctx.Done()
	}))
	defer server.Close()
	second := make(chan struct{}, 1)
	var ready atomic.Int32
	s := dingtalkTestStream(dingtalkStreamCallbacks{OnConnected: func(context.Context) error {
		if ready.Add(1) == 2 {
			second <- struct{}{}
		}
		return nil
	}}, "ws"+strings.TrimPrefix(server.URL, "http"))
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	WaitTestForTest(t, second)
	cancel()
	WaitTestForTest(t, done)
}

func TestDingtalkStreamBackoffBounds(t *testing.T) {
	for attempt, base := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second} {
		for range 20 {
			delay := dingtalkStreamBackoff(attempt)
			if delay < base*8/10 || delay > base*12/10 {
				t.Fatalf("attempt %d delay %v", attempt, delay)
			}
		}
	}
}

// This connection preserves the real HTTP upgrade, then models a saturated TCP
// send buffer. The test controls deadline expiry and Close independently, so it
// verifies both exit paths without relying on host socket buffer sizes or sleeps.
type dingtalkTestBlockedWriteConn struct {
	net.Conn
	blocked         atomic.Bool
	mu              sync.Mutex
	deadline        time.Time
	entered         chan time.Time
	expired, closed chan struct{}
	closeOnce       sync.Once
}

func (c *dingtalkTestBlockedWriteConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.deadline = deadline
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(deadline)
}

func (c *dingtalkTestBlockedWriteConn) Write(data []byte) (int, error) {
	if !c.blocked.Load() {
		return c.Conn.Write(data)
	}
	c.mu.Lock()
	deadline := c.deadline
	c.mu.Unlock()
	c.entered <- deadline
	select {
	case <-c.expired:
		return 0, os.ErrDeadlineExceeded
	case <-c.closed:
		return 0, net.ErrClosed
	}
}

func (c *dingtalkTestBlockedWriteConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func TestDingtalkStreamBlockedWriteDeadlineAndShutdown(t *testing.T) {
	for _, scenario := range []string{"deadline", "shutdown"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.WriteJSON(dingtalkTestFrame("SYSTEM", "ping", "system-ping", `{"opaque":"test"}`))
				<-ctx.Done()
			}))
			defer func() { cancel(); server.Close() }()
			transport := make(chan *dingtalkTestBlockedWriteConn, 1)
			readySocket := make(chan *dingtalkTestBlockedWriteConn, 1)
			retrying := make(chan int, 1)
			unavailable := make(chan struct{}, 1)
			s := dingtalkTestStream(dingtalkStreamCallbacks{OnConnected: func(context.Context) error {
				socket := <-transport
				socket.blocked.Store(true)
				readySocket <- socket
				return nil
			}, OnUnavailable: func() { unavailable <- struct{}{} }}, "ws"+strings.TrimPrefix(server.URL, "http"))
			s.writeTimeout = 5 * time.Second
			s.dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				wrapped := &dingtalkTestBlockedWriteConn{Conn: conn, entered: make(chan time.Time, 1), expired: make(chan struct{}), closed: make(chan struct{})}
				transport <- wrapped
				return wrapped, nil
			}
			s.backoff = func(attempt int) time.Duration { retrying <- attempt; return time.Hour }
			done := make(chan error, 1)
			go func() { done <- s.Run(ctx) }()
			active := WaitTestForTest(t, readySocket)
			deadline := WaitTestForTest(t, active.entered)
			if deadline.IsZero() || deadline.After(time.Now().Add(s.writeTimeout+time.Second)) {
				t.Fatalf("write missing finite configured deadline: %v", deadline)
			}
			if scenario == "deadline" {
				close(active.expired)
				WaitTestForTest(t, unavailable)
				if attempt := WaitTestForTest(t, retrying); attempt != 0 {
					t.Fatalf("deadline did not enter reconnect supervisor: %d", attempt)
				}
				cancel()
			} else {
				cancel()
				WaitTestForTest(t, unavailable)
			}
			if err := WaitTestForTest(t, done); err != nil {
				t.Fatal(err)
			}
			select {
			case <-active.closed:
			default:
				t.Fatal("blocked writer's socket was not closed")
			}
			if scenario == "shutdown" {
				select {
				case <-retrying:
					t.Fatal("shutdown scheduled another dial")
				default:
				}
			}
		})
	}
}

func TestDingtalkStreamStableConnectionResetsBackoff(t *testing.T) {
	for _, stable := range []bool{false, true} {
		t.Run(fmt.Sprint(stable), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.WriteJSON(dingtalkTestFrame("SYSTEM", "disconnect", "", `{}`))
				_, _, _ = conn.ReadMessage()
			}))
			defer server.Close()
			s := dingtalkTestStream(dingtalkStreamCallbacks{}, "")
			var registrations atomic.Int32
			s.openConnection = func(context.Context) (string, string, error) {
				if registrations.Add(1) <= 2 {
					return "", "", errors.New("network_failure")
				}
				return "ws" + strings.TrimPrefix(server.URL, "http"), "test-ticket", nil
			}
			if stable {
				s.stableAfter = time.Nanosecond
			} else {
				s.stableAfter = time.Hour
			}
			attempts := make(chan int, 3)
			var waits atomic.Int32
			s.backoff = func(attempt int) time.Duration {
				attempts <- attempt
				if waits.Add(1) == 3 {
					return time.Hour
				}
				// A zero delay is floored to one second by im.SleepCtx; use a real
				// minimum so the retry chain runs without that floor.
				return time.Millisecond
			}
			done := make(chan error, 1)
			go func() { done <- s.Run(ctx) }()
			want := []int{0, 1, 2}
			if stable {
				want[2] = 0
			}
			for _, expected := range want {
				if actual := WaitTestForTest(t, attempts); actual != expected {
					t.Fatalf("stable=%v backoff=%d want=%d", stable, actual, expected)
				}
			}
			cancel()
			WaitTestForTest(t, done)
			if registrations.Load() != 3 {
				t.Fatalf("unexpected registrations=%d", registrations.Load())
			}
		})
	}
}

func TestDingtalkStreamRetryDiagnosticsAndRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(dingtalkTestFrame("SYSTEM", "disconnect", "", `{}`))
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	type retry struct {
		category string
		attempt  int
		delay    time.Duration
	}
	var retries []retry
	var connected int
	s := dingtalkTestStream(dingtalkStreamCallbacks{
		OnDiagnostic: func(category string) {
			if category == "connected" {
				connected++
			}
		},
		OnRetry: func(category string, attempt int, delay time.Duration) {
			retries = append(retries, retry{category, attempt, delay})
			if category == "server_disconnect" {
				cancel()
			}
		},
	}, "")
	attempts := 0
	s.openConnection = func(context.Context) (string, string, error) {
		attempts++
		switch attempts {
		case 1, 2:
			return "", "", errors.New("https://example.invalid/?ticket=test-secret response-body")
		case 3:
			return "", "", &DingtalkAPIError{Category: "rate_limited"}
		default:
			return "ws" + strings.TrimPrefix(server.URL, "http"), "test-ticket", nil
		}
	}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	want := []retry{{"registration_failed", 1, time.Millisecond}, {"rate_limited", 3, time.Millisecond}, {"server_disconnect", 4, time.Millisecond}}
	if !reflect.DeepEqual(retries, want) || connected != 1 {
		t.Fatalf("retries=%+v connected=%d; want=%+v", retries, connected, want)
	}
}

func TestDingtalkStreamRetryCategoriesAreSanitized(t *testing.T) {
	for _, category := range []string{"handshake_failed", "read_failed", "write_failed", "heartbeat_timeout", "backpressure"} {
		err := fmt.Errorf("private detail: %w", &dingtalkStreamError{category: category})
		if got := dingtalkStreamRetryCategory(err, "connection"); got != category {
			t.Fatalf("category=%q want=%q", got, category)
		}
	}
	if got := dingtalkStreamRetryCategory(errors.New("private detail"), "connection"); got != "connection_failed" {
		t.Fatalf("unsafe category: %q", got)
	}
}
