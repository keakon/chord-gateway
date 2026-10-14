package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/gorilla/websocket"
)

func testWS(t *testing.T, handler func(*websocket.Conn, int)) (string, *atomic.Int32) {
	t.Helper()
	count := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn, int(count.Add(1)))
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http"), count
}
func testHello(conn *websocket.Conn, interval int) error {
	return conn.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": interval}})
}
func testReady(conn *websocket.Conn, wsURL string) error {
	return conn.WriteJSON(map[string]any{"op": 0, "t": "READY", "s": 1, "d": map[string]any{
		"v": 10, "user": map[string]any{"id": "123", "username": "test-bot", "bot": true},
		"application": map[string]any{"id": "456"}, "session_id": "test-session", "resume_gateway_url": wsURL, "guilds": []any{},
	}})
}
func testCommand(conn *websocket.Conn) (Opcode, error) {
	for {
		var message struct {
			Op Opcode `json:"op"`
		}
		if err := conn.ReadJSON(&message); err != nil {
			return 0, err
		}
		if message.Op == OpcodeHeartbeat {
			if err := conn.WriteJSON(map[string]any{"op": 11}); err != nil {
				return 0, err
			}
			continue
		}
		return message.Op, nil
	}
}
func testDrain(conn *websocket.Conn, ack bool) {
	for {
		var message struct {
			Op Opcode `json:"op"`
		}
		if conn.ReadJSON(&message) != nil {
			return
		}
		if ack && message.Op == OpcodeHeartbeat {
			if conn.WriteJSON(map[string]any{"op": 11}) != nil {
				return
			}
		}
	}
}
func requireSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for lifecycle signal")
	}
}
func requireStopped(t *testing.T, g *gatewayImpl) {
	t.Helper()
	requireSignal(t, g.done)
	if g.Status() != StatusDisconnected {
		t.Fatalf("status = %v", g.Status())
	}
	if g.SessionID() != nil || g.LastSequenceReceived() != nil || g.ResumeURL() != nil {
		t.Fatal("permanent shutdown retained session")
	}
}

func TestHeartbeatMissingACKShutdown(t *testing.T) {
	disconnected := make(chan struct{}, 1)
	url, count := testWS(t, func(conn *websocket.Conn, _ int) {
		if testHello(conn, 25) != nil {
			return
		}
		if _, err := testCommand(conn); err != nil {
			return
		}
		_ = testReady(conn, "")
		testDrain(conn, false)
	})
	lifecycle, cancel := context.WithCancel(context.Background())
	defer cancel()
	var gateway Gateway
	var ready atomic.Bool
	gateway = New(lifecycle, "test-token", nil, WithURL(url), WithStateChangeHandler(func(state Status, _ uint64) {
		_ = gateway.Status() // Callback is outside the state lock.
		if state == StatusReady {
			ready.Store(true)
		}
		if state == StatusDisconnected && ready.Load() {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}
	}))
	g := gateway.(*gatewayImpl)
	ctx, stop := context.WithTimeout(lifecycle, 3*time.Second)
	defer stop()
	if err := g.Open(ctx); err != nil {
		t.Fatal(err)
	}
	requireSignal(t, disconnected)
	g.Close(ctx)
	g.Close(ctx)
	requireStopped(t, g)
	if count.Load() != 1 {
		t.Fatalf("shutdown allowed reconnect: %d connections", count.Load())
	}
}

func TestLifecycleCancellationAtEveryWait(t *testing.T) {
	for _, stage := range []string{"hello", "ready", "dial", "quota", "backoff"} {
		t.Run(stage, func(t *testing.T) {
			lifecycle, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{}, 1)
			signal := func() {
				select {
				case entered <- struct{}{}:
				default:
				}
			}
			var opts []ConfigOpt
			if stage == "dial" {
				dialer := *websocket.DefaultDialer
				dialer.NetDialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
					signal()
					<-ctx.Done()
					return nil, ctx.Err()
				}
				opts = append(opts, WithDialer(&dialer))
			} else if stage == "quota" {
				opts = append(opts, WithSessionStartLimit(100, 0, time.Hour), WithStateChangeHandler(func(state Status, _ uint64) {
					if state == StatusConnecting {
						signal()
					}
				}))
			} else {
				url, _ := testWS(t, func(conn *websocket.Conn, _ int) {
					if stage == "backoff" {
						_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4000, "retry"), time.Now().Add(time.Second))
						return
					}
					if stage == "ready" {
						if testHello(conn, 60000) != nil {
							return
						}
						if _, err := testCommand(conn); err != nil {
							return
						}
					}
					signal()
					testDrain(conn, true)
				})
				opts = append(opts, WithURL(url))
				if stage == "backoff" {
					opts = append(opts, WithStateChangeHandler(func(state Status, _ uint64) {
						if state == StatusDisconnected {
							signal()
						}
					}))
				}
			}
			g := New(lifecycle, "test-token", nil, opts...).(*gatewayImpl)
			result := make(chan error, 1)
			go func() { result <- g.Open(lifecycle) }()
			requireSignal(t, entered)
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Open: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Open ignored cancellation")
			}
			g.Close(context.Background())
			requireStopped(t, g)
		})
	}
}

func TestFatalGatewayCloseCodesStopSupervisor(t *testing.T) {
	for _, code := range []int{4004, 4010, 4011, 4012, 4013, 4014} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			url, count := testWS(t, func(conn *websocket.Conn, _ int) {
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, "fatal"), time.Now().Add(time.Second))
			})
			g := New(context.Background(), "test-token", nil, WithURL(url)).(*gatewayImpl)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := g.Open(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Open = %v", err)
			}
			requireStopped(t, g)
			g.Close(ctx)
			if count.Load() != 1 {
				t.Fatalf("fatal code retried %d times", count.Load())
			}
		})
	}
}

func TestFatalGatewayHTTPAuthentication(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var count atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { count.Add(1); w.WriteHeader(status) }))
			defer server.Close()
			g := New(context.Background(), "test-token", nil, WithURL("ws"+strings.TrimPrefix(server.URL, "http"))).(*gatewayImpl)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := g.Open(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Open = %v", err)
			}
			requireStopped(t, g)
			if count.Load() != 1 {
				t.Fatalf("authentication failure retried %d times", count.Load())
			}
		})
	}
}

func TestServerReconnectResumesWithoutIdentifyBudget(t *testing.T) {
	var refreshes atomic.Int32
	events := make(chan uint64, 4)
	commands := make(chan Opcode, 2)
	url, _ := testWS(t, func(conn *websocket.Conn, attempt int) {
		if testHello(conn, 60000) != nil {
			return
		}
		op, err := testCommand(conn)
		if err != nil {
			return
		}
		commands <- op
		if attempt == 1 {
			_ = testReady(conn, "")
			_ = conn.WriteJSON(map[string]any{"op": 7})
		} else {
			_ = conn.WriteJSON(map[string]any{"op": 0, "t": "RESUMED", "s": 2, "d": map[string]any{}})
		}
		testDrain(conn, true)
	})
	g := New(context.Background(), "test-token", nil, WithURL(url), WithEnableResumeURL(false), WithSessionStartLimit(1, 1, time.Hour), WithSessionStartLimitRefresh(func(context.Context) (discord.SessionStartLimit, error) {
		refreshes.Add(1)
		return discord.SessionStartLimit{}, ErrSessionStartLimitFatal
	}), WithEventGenerationHandler(func(event EventType, _ EventData, generation uint64) {
		if event == EventTypeReady || event == EventTypeResumed {
			events <- generation
		}
	})).(*gatewayImpl)
	defer g.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := g.Open(ctx); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []uint64{1, 2} {
		select {
		case got := <-events:
			if got != generation {
				t.Fatalf("event generation = %d, want %d", got, generation)
			}
		case <-ctx.Done():
			t.Fatal("Resume did not complete")
		}
	}
	for _, expected := range []Opcode{OpcodeIdentify, OpcodeResume} {
		select {
		case got := <-commands:
			if got != expected {
				t.Fatalf("command = %v, want %v", got, expected)
			}
		case <-ctx.Done():
			t.Fatal("missing identify/resume")
		}
	}
	if seq := g.LastSequenceReceived(); seq == nil || *seq != 2 {
		t.Fatalf("sequence = %v", seq)
	}
	if refreshes.Load() != 0 {
		t.Fatal("Resume refreshed or consumed Identify quota")
	}
}

func TestRejectedReadyIdentityNeverPublishesReady(t *testing.T) {
	expected := errors.New("identity mismatch")
	var ready atomic.Bool
	url, _ := testWS(t, func(conn *websocket.Conn, _ int) {
		_ = testHello(conn, 60000)
		_, _ = testCommand(conn)
		_ = testReady(conn, "")
		testDrain(conn, true)
	})
	g := New(context.Background(), "test-token", nil, WithURL(url), WithReadyValidator(func(EventReady) error { return expected }), WithStateChangeHandler(func(state Status, _ uint64) {
		if state == StatusReady {
			ready.Store(true)
		}
	})).(*gatewayImpl)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := g.Open(ctx); !errors.Is(err, expected) {
		t.Fatalf("Open = %v", err)
	}
	requireStopped(t, g)
	if ready.Load() {
		t.Fatal("unverified identity became ready")
	}
}

type testTransport struct{ message *Message }

func (t *testTransport) ReceiveMessage() (*Message, error) { return t.message, nil }
func (*testTransport) WriteMessage(Message) error          { return nil }
func (*testTransport) WriteClose(int, string) error        { return nil }
func (*testTransport) Close() error                        { return nil }

func TestInvalidSessionResumeFlagAndRandomWait(t *testing.T) {
	for _, canResume := range []bool{false, true} {
		t.Run(fmt.Sprint(canResume), func(t *testing.T) {
			g := New(context.Background(), "test-token", nil, WithSessionID("test-session"), WithSequence(42)).(*gatewayImpl)
			defer g.Close(context.Background())
			g.mu.Lock()
			g.generation = 1
			g.active = true
			g.mu.Unlock()
			var result connectionResult
			g.listen(g.ctx, &testTransport{message: &Message{Op: OpcodeInvalidSession, D: MessageDataInvalidSession(canResume)}}, 1, make(chan time.Duration, 1), func(r connectionResult) { result = r })
			if result.delay < time.Second || result.delay > 5*time.Second {
				t.Fatalf("InvalidSession wait = %v", result.delay)
			}
			if (g.SessionID() != nil) != canResume {
				t.Fatalf("resume state retained = %v, want %v", g.SessionID() != nil, canResume)
			}
		})
	}
}

func TestOldGenerationCannotPublishStateOrEvents(t *testing.T) {
	var events atomic.Int32
	g := New(context.Background(), "test-token", nil, WithEventGenerationHandler(func(EventType, EventData, uint64) { events.Add(1) })).(*gatewayImpl)
	defer g.Close(context.Background())
	g.mu.Lock()
	g.generation = 2
	g.active = true
	g.status = StatusReady
	g.mu.Unlock()
	if g.transition(StatusDisconnected, 1) {
		t.Fatal("old generation changed current state")
	}
	if g.transition(StatusReady, 1) {
		t.Fatal("old READY accepted")
	}
	g.dispatch(1, EventTypeReady, 1, EventReady{})
	if g.Status() != StatusReady || events.Load() != 0 {
		t.Fatal("old socket event escaped generation guard")
	}
}

func TestConcurrentSendGettersAndClose(t *testing.T) {
	url, _ := testWS(t, func(conn *websocket.Conn, _ int) {
		_ = testHello(conn, 60000)
		_, _ = testCommand(conn)
		_ = testReady(conn, "")
		testDrain(conn, true)
	})
	g := New(context.Background(), "test-token", nil, WithURL(url)).(*gatewayImpl)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := g.Open(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot := g.SessionID()
	*snapshot = "mutated"
	if *g.SessionID() == "mutated" {
		t.Fatal("session getter leaked mutable state")
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 20 {
				_ = g.SessionID()
				_ = g.ResumeURL()
				_ = g.LastSequenceReceived()
				_ = g.Latency()
				_ = g.Presence()
				_ = g.Send(ctx, OpcodePresenceUpdate, MessageDataPresenceUpdate{})
			}
		}()
	}
	workers.Add(2)
	for range 2 {
		go func() { defer workers.Done(); g.Close(ctx) }()
	}
	workers.Wait()
	requireStopped(t, g)
}

func TestIdentifySpacingAndQuotaWaitAreCancellable(t *testing.T) {
	for _, mode := range []string{"spacing", "quota"} {
		t.Run(mode, func(t *testing.T) {
			var opts []ConfigOpt
			if mode == "quota" {
				opts = append(opts, WithSessionStartLimit(100, 0, time.Hour))
			}
			g := New(context.Background(), "test-token", nil, opts...).(*gatewayImpl)
			defer g.Close(context.Background())
			if mode == "spacing" {
				g.lastIdentify = time.Now()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if err := g.waitIdentify(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Identify %s wait = %v", mode, err)
			}
		})
	}
}

type waitingSendLimiter struct{ entered chan struct{} }

func (l *waitingSendLimiter) Wait(ctx context.Context, kind RateLimiterCommandType) error {
	if kind == InternalCommandType {
		return nil
	}
	close(l.entered)
	<-ctx.Done()
	return ctx.Err()
}
func (*waitingSendLimiter) Reset()                {}
func (*waitingSendLimiter) Close(context.Context) {}
func (*waitingSendLimiter) Unlock()               {}

func TestCloseCancelsSendRateLimitWait(t *testing.T) {
	url, _ := testWS(t, func(conn *websocket.Conn, _ int) {
		_ = testHello(conn, 60000)
		_, _ = testCommand(conn)
		_ = testReady(conn, "")
		testDrain(conn, true)
	})
	limiter := &waitingSendLimiter{entered: make(chan struct{})}
	g := New(context.Background(), "test-token", nil, WithURL(url), WithRateLimiter(limiter)).(*gatewayImpl)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := g.Open(ctx); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- g.Send(ctx, OpcodePresenceUpdate, MessageDataPresenceUpdate{}) }()
	requireSignal(t, limiter.entered)
	g.Close(ctx)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("send wait = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Close did not cancel Send wait")
	}
	requireStopped(t, g)
}

type deadlineConn struct {
	net.Conn
	deadlines chan time.Duration
}

func (c deadlineConn) SetWriteDeadline(deadline time.Time) error {
	select {
	case c.deadlines <- time.Until(deadline):
	default:
	}
	return c.Conn.SetWriteDeadline(deadline)
}

func TestGatewayDataWriteHasFiveSecondDeadline(t *testing.T) {
	url, _ := testWS(t, func(conn *websocket.Conn, _ int) {
		_ = testHello(conn, 60000)
		_, _ = testCommand(conn)
		_ = testReady(conn, "")
		testDrain(conn, true)
	})
	deadlines := make(chan time.Duration, 4)
	dialer := *websocket.DefaultDialer
	dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return deadlineConn{Conn: conn, deadlines: deadlines}, nil
	}
	g := New(context.Background(), "test-token", nil, WithURL(url), WithDialer(&dialer)).(*gatewayImpl)
	defer g.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := g.Open(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case deadline := <-deadlines:
		if deadline < 4900*time.Millisecond || deadline > 5*time.Second {
			t.Fatalf("write deadline = %v", deadline)
		}
	case <-ctx.Done():
		t.Fatal("write had no deadline")
	}
}

func TestResumeReplayCapturedAtArrivalAndDeliveredAfterResumed(t *testing.T) {
	reconnect, finishReplay := make(chan struct{}), make(chan struct{})
	captured, delivered := make(chan string, 1), make(chan string, 1)
	var target atomic.Value
	target.Store("original-target")
	url, _ := testWS(t, func(conn *websocket.Conn, attempt int) {
		_ = testHello(conn, 60000)
		_, _ = testCommand(conn)
		if attempt == 1 {
			_ = testReady(conn, "")
			<-reconnect
			_ = conn.WriteJSON(map[string]any{"op": 7})
		} else {
			_ = conn.WriteJSON(map[string]any{"op": 0, "t": "MESSAGE_CREATE", "s": 2, "d": map[string]any{"id": "789", "channel_id": "500", "type": 0}})
			<-finishReplay
			_ = conn.WriteJSON(map[string]any{"op": 0, "t": "RESUMED", "s": 3, "d": map[string]any{}})
		}
		testDrain(conn, true)
	})
	g := New(context.Background(), "test-token", nil, WithURL(url), WithEventCaptureHandler(func(event EventType, _ EventData, _ uint64) func() {
		if event != EventTypeMessageCreate {
			return nil
		}
		frozen := target.Load().(string)
		captured <- frozen
		return func() { delivered <- frozen }
	})).(*gatewayImpl)
	defer g.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := g.Open(ctx); err != nil {
		t.Fatal(err)
	}
	close(reconnect)
	select {
	case value := <-captured:
		if value != "original-target" {
			t.Fatal(value)
		}
	case <-ctx.Done():
		t.Fatal("resume dispatch not captured")
	}
	if g.Status() == StatusReady {
		t.Fatal("ready before RESUMED")
	}
	select {
	case <-delivered:
		t.Fatal("replay delivered before RESUMED")
	default:
	}
	waitCtx, stopWait := context.WithTimeout(ctx, 20*time.Millisecond)
	if err := g.Open(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Open did not wait for RESUMED: %v", err)
	}
	stopWait()
	target.Store("replacement-target")
	close(finishReplay)
	if err := g.Open(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-delivered:
		if value != "original-target" {
			t.Fatalf("capture was deferred until flush: %s", value)
		}
	case <-ctx.Done():
		t.Fatal("replay was lost")
	}
}

func TestResumeReplayCapacityStopsWithoutSkippingCheckpoint(t *testing.T) {
	reconnect := make(chan struct{})
	result := make(chan error, 1)
	var delivered atomic.Int32
	var checkpoint atomic.Int32
	url, count := testWS(t, func(conn *websocket.Conn, attempt int) {
		_ = testHello(conn, 60000)
		_, _ = testCommand(conn)
		if attempt == 1 {
			_ = testReady(conn, "")
			<-reconnect
			_ = conn.WriteJSON(map[string]any{"op": 7})
		} else {
			for index := range ReplayCapacity + 1 {
				if conn.WriteJSON(map[string]any{"op": 0, "t": "MESSAGE_CREATE", "s": index + 2, "d": map[string]any{"id": fmt.Sprint(index + 1000), "channel_id": "500", "type": 0}}) != nil {
					return
				}
			}
		}
		testDrain(conn, true)
	})
	g := New(context.Background(), "test-token", nil, WithURL(url), WithEventCaptureHandler(func(event EventType, _ EventData, _ uint64) func() {
		if event != EventTypeMessageCreate {
			return nil
		}
		return func() { delivered.Add(1) }
	}), WithCloseHandler(func(gateway Gateway, err error, _ bool) {
		if sequence := gateway.LastSequenceReceived(); sequence != nil {
			checkpoint.Store(int32(*sequence))
		}
		result <- err
	})).(*gatewayImpl)
	defer g.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := g.Open(ctx); err != nil {
		t.Fatal(err)
	}
	close(reconnect)
	select {
	case err := <-result:
		if !errors.Is(err, ErrReplayCapacityExceeded) {
			t.Fatalf("overflow = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("replay overflow did not stop")
	}
	requireStopped(t, g)
	if checkpoint.Load() != 1 {
		t.Fatalf("undelivered replay advanced checkpoint to %d", checkpoint.Load())
	}
	if delivered.Load() != 0 || count.Load() != 2 {
		t.Fatalf("overflow delivered/retried: delivered=%d connections=%d", delivered.Load(), count.Load())
	}
}

func TestMalformedGatewayFrameErrorOmitsOriginalPayload(t *testing.T) {
	const marker = "sample-private-body"
	_, err := decodeGatewayMessage([]byte(`{"op":0,"t":"MESSAGE_CREATE","d":{"content":"sample-private-body","type":"invalid"}}`))
	if err == nil {
		t.Fatal("malformed message accepted")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("decode error retained message body: %v", err)
	}
}

func TestIdentifyQuotaRefreshUsesCurrentWindowAndCanWaitAgain(t *testing.T) {
	var calls atomic.Int32
	g := New(context.Background(), "test-token", nil, WithSessionStartLimit(100, 0, 0), WithSessionStartLimitRefresh(func(context.Context) (discord.SessionStartLimit, error) {
		if calls.Add(1) == 1 {
			return discord.SessionStartLimit{Total: 100, Remaining: 0, ResetAfter: 30}, nil
		}
		return discord.SessionStartLimit{Total: 100, Remaining: 5, ResetAfter: 1000}, nil
	})).(*gatewayImpl)
	defer g.Close(context.Background())
	start := time.Now()
	if err := g.waitIdentify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || time.Since(start) < 30*time.Millisecond || g.config.SessionRemaining != 5 {
		t.Fatal("refreshed empty quota was not waited", calls.Load())
	}
	// Exhaust the new budget; its new one-second reset must replace the original
	// zero/30ms countdown instead of becoming a repeating short window.
	g.config.SessionRemaining = 0
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := g.waitIdentify(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("new quota window = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatal("refreshed quota window was reused as an immediate refill")
	}
}

func TestIdentifyQuotaRefreshValidationAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit discord.SessionStartLimit
	}{
		{"zero_total", discord.SessionStartLimit{}},
		{"negative_remaining", discord.SessionStartLimit{Total: 100, Remaining: -1, ResetAfter: 100}},
		{"remaining_over_total", discord.SessionStartLimit{Total: 100, Remaining: 101, ResetAfter: 100}},
		{"negative_reset", discord.SessionStartLimit{Total: 100, Remaining: 1, ResetAfter: -1}},
		{"empty_zero_reset", discord.SessionStartLimit{Total: 100, Remaining: 0, ResetAfter: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := New(context.Background(), "test-token", nil, WithSessionStartLimit(100, 0, 0), WithSessionStartLimitRefresh(func(context.Context) (discord.SessionStartLimit, error) { return tc.limit, nil })).(*gatewayImpl)
			defer g.Close(context.Background())
			if err := g.waitIdentify(context.Background()); !errors.Is(err, ErrSessionStartLimitFatal) {
				t.Fatalf("invalid refreshed quota = %v", err)
			}
		})
	}
	g := New(context.Background(), "test-token", nil, WithSessionStartLimit(100, 0, 0), WithSessionStartLimitRefresh(func(ctx context.Context) (discord.SessionStartLimit, error) {
		<-ctx.Done()
		return discord.SessionStartLimit{}, ctx.Err()
	})).(*gatewayImpl)
	defer g.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := g.waitIdentify(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("refresh ignored context: %v", err)
	}
}

func TestIdentifyQuotaWithoutProviderCannotRepeatInitialWindow(t *testing.T) {
	g := New(context.Background(), "test-token", nil, WithSessionStartLimit(1, 0, 0)).(*gatewayImpl)
	defer g.Close(context.Background())
	if err := g.waitIdentify(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.config.SessionRemaining = 0
	if err := g.waitIdentify(context.Background()); !errors.Is(err, ErrSessionStartLimitFatal) {
		t.Fatalf("exhausted quota without provider was silently refilled: %v", err)
	}
}

func TestGatewayReadLimitAllowsLargeReady(t *testing.T) {
	url, _ := testWS(t, func(conn *websocket.Conn, _ int) {
		if testHello(conn, 1000) != nil {
			return
		}
		if _, err := testCommand(conn); err != nil {
			return
		}
		// READY can legitimately exceed a generic one-MiB socket limit.
		guilds := make([]map[string]any, 40000)
		for i := range guilds {
			guilds[i] = map[string]any{"id": fmt.Sprint(i + 1), "unavailable": true}
		}
		if conn.WriteJSON(map[string]any{"op": 0, "t": "READY", "s": 1, "d": map[string]any{
			"v": 10, "user": map[string]any{"id": "123", "bot": true}, "application": map[string]any{"id": "456"}, "session_id": "large", "guilds": guilds,
		}}) != nil {
			return
		}
		testDrain(conn, true)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	g := New(ctx, "test-token", nil, WithURL(url))
	defer g.Close(context.Background())
	if err := g.Open(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayReadLimitRejectsOversizedMessage(t *testing.T) {
	url, _ := testWS(t, func(conn *websocket.Conn, _ int) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(strings.Repeat(" ", ReadLimit+1)))
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	g := New(ctx, "test-token", nil, WithURL(url)).(*gatewayImpl)
	defer g.Close(context.Background())
	// Exercise the actual connection setup, including its configured limit.
	result := g.runConnection(1)
	if !errors.Is(result.err, websocket.ErrReadLimit) {
		t.Fatalf("oversized frame result: %v", result.err)
	}
}
