package wecom

// Test-only seams for the gateway joint tests, which live in the root package
// and cannot reach unexported members across the package boundary. Production
// code must not use anything in this file.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/keakon/chord-gateway/internal/im"
)

// NewWSForTest builds a transport with production timing; the joint tests use
// it to probe jitter bounds without a server.
func NewWSForTest(botID, secret string, callbacks WecomWSCallbacks) *WecomWS {
	return newWecomWS(botID, secret, callbacks)
}

// StartWSForTest runs a transport against a plaintext WebSocket test server.
// The default test timing keeps reconnects fast; configure runs before Run.
func StartWSForTest(t *testing.T, handler func(*websocket.Conn), callbacks WecomWSCallbacks, configure ...func(*WecomWS)) (*WecomWS, <-chan error) {
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

// ReadWSFrameForTest reads one protocol frame with a bounded deadline.
func ReadWSFrameForTest(conn *websocket.Conn) (WecomWSFrame, error) {
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var frame WecomWSFrame
	err := conn.ReadJSON(&frame)
	return frame, err
}

// WriteWSACKForTest answers one request with the supplied result code.
func WriteWSACKForTest(conn *websocket.Conn, id string, code *int) error {
	frame := WecomWSFrame{ErrCode: code}
	frame.Headers.ReqID = id
	return conn.WriteJSON(frame)
}

// SubscribeWSForTest completes the subscribe handshake as the platform would.
func SubscribeWSForTest(conn *websocket.Conn) error {
	frame, err := ReadWSFrameForTest(conn)
	if err != nil {
		return err
	}
	if frame.Cmd != "aibot_subscribe" {
		return errors.New("expected subscribe")
	}
	code := 0
	return WriteWSACKForTest(conn, frame.Headers.ReqID, &code)
}

// WaitWSReadyForTest blocks until the transport has a ready session.
func WaitWSReadyForTest(t *testing.T, w *WecomWS) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		ready := w.current != nil && w.current.ready
		w.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for transport readiness")
}

// JitterForTest exposes the reconnect jitter function.
func (w *WecomWS) JitterForTest(delay time.Duration) time.Duration { return w.jitter(delay) }

// SetJitterForTest replaces the reconnect jitter function.
func (w *WecomWS) SetJitterForTest(jitter func(time.Duration) time.Duration) { w.jitter = jitter }

// SetStableAfterForTest shortens the connection-stability window.
func (w *WecomWS) SetStableAfterForTest(stableAfter time.Duration) { w.stableAfter = stableAfter }

// SetBotIDForTest overrides the subscribed bot identity.
func (w *WecomWS) SetBotIDForTest(botID string) { w.botID = botID }

// SetHeartbeatIntervalForTest overrides the heartbeat cadence.
func (w *WecomWS) SetHeartbeatIntervalForTest(heartbeat time.Duration) {
	w.timing.heartbeat = heartbeat
}

// SetHeartbeatACKTimeoutForTest overrides the heartbeat ACK window.
func (w *WecomWS) SetHeartbeatACKTimeoutForTest(heartbeatACK time.Duration) {
	w.timing.heartbeatACK = heartbeatACK
}

// SetHeartbeatBackoffForTest overrides the reconnect base backoff.
func (w *WecomWS) SetHeartbeatBackoffForTest(backoff time.Duration) { w.timing.backoff = backoff }

// ContextForTest exposes the adapter lifecycle context.
func (a *WecomAdapter) ContextForTest() context.Context { return a.ctx }

// TextSenderForTest returns the shared text sender for segment assertions.
func (a *WecomAdapter) TextSenderForTest() *im.IMTextSender { return a.textSender }

// AuthenticatedForTest drives the OnAuthenticated callback.
func (a *WecomAdapter) AuthenticatedForTest(ctx context.Context) error { return a.authenticated(ctx) }

// TransportReadyForTest drives the OnReady callback.
func (a *WecomAdapter) TransportReadyForTest(ctx context.Context) error { return a.transportReady(ctx) }

// UnavailableForTest drives the OnUnavailable callback.
func (a *WecomAdapter) UnavailableForTest(err error) { a.unavailable(err) }

// ReceiveFrameForTest feeds one raw callback frame through the frame path.
func (a *WecomAdapter) ReceiveFrameForTest(ctx context.Context, raw []byte) error {
	return a.receiveFrame(ctx, raw)
}

// CaptureFrameForTest feeds one raw callback frame through the capture path.
func (a *WecomAdapter) CaptureFrameForTest(ctx context.Context, raw []byte) context.Context {
	return a.captureFrame(ctx, raw)
}

// ObserveReceiveForTest drives the bounded-worker result observer.
func (a *WecomAdapter) ObserveReceiveForTest(msg im.IncomingMessage, result im.ReceiveResult) {
	a.observeReceive(msg, result)
}

// DroppedBeforeAcceptForTest reports dropped deliveries as the transport would.
func (a *WecomAdapter) DroppedBeforeAcceptForTest(count int) { a.droppedBeforeAccept(count) }

// CallbackRejectedForTest reports a rejected text callback as the transport would.
func (a *WecomAdapter) CallbackRejectedForTest(reason string) { a.callbackRejected(reason) }

// WaitSendSlotForTest drives the send pacing gate directly.
func (a *WecomAdapter) WaitSendSlotForTest(ctx context.Context, target string) error {
	return a.waitSendSlot(ctx, target)
}

// AdapterReadyForTest snapshots adapter readiness under the adapter lock.
func (a *WecomAdapter) AdapterReadyForTest() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ready && !a.stopping
}

// SetRatePacingForTest speeds up the account and target send pacing gates.
func (a *WecomAdapter) SetRatePacingForTest(accountInterval, targetInterval time.Duration) {
	a.accountInterval, a.targetInterval = accountInterval, targetInterval
}

// SetWSEndpointForTest points the transport at a test server.
func (a *WecomAdapter) SetWSEndpointForTest(endpoint string) { a.ws.endpoint = endpoint }

// WSRunForTest starts the transport loop; the caller owns the goroutine.
func (a *WecomAdapter) WSRunForTest(ctx context.Context) error { return a.ws.Run(ctx) }

// WSTransportReadyForTest reports whether the current session can deliver.
func (a *WecomAdapter) WSTransportReadyForTest() bool {
	a.ws.mu.Lock()
	defer a.ws.mu.Unlock()
	return a.ws.current != nil && a.ws.current.ready && a.ws.current.deliverReady
}

// WSPendingSendsForTest counts requests awaiting an ACK in the current session.
func (a *WecomAdapter) WSPendingSendsForTest() int {
	a.ws.mu.Lock()
	defer a.ws.mu.Unlock()
	if a.ws.current == nil {
		return 0
	}
	return a.ws.current.sends
}

// SetWSSendTimeoutForTest shortens the per-send ACK window.
func (a *WecomAdapter) SetWSSendTimeoutForTest(send time.Duration) { a.ws.timing.send = send }

// RenderTextForTest exposes the markdown escaper to the root rendering tests.
func RenderTextForTest(source string) string { return renderWecomText(source) }

// SetStoppingForTest seals the adapter as if shutdown had begun.
func (a *WecomAdapter) SetStoppingForTest(stopping bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopping = stopping
}

// RatePacingForTest reports the current account and target send intervals.
func (a *WecomAdapter) RatePacingForTest() (account, target time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accountInterval, a.targetInterval
}

// WecomCallbackRejectionReasonForTest exposes the reason classifier.
func WecomCallbackRejectionReasonForTest(err error) string {
	return wecomCallbackRejectionReason(err)
}

// Test-only sentinels for the gateway metric vocabulary test.
var (
	ErrWecomInvalidCallbackForTest      = ErrWecomInvalidCallback
	ErrWecomSourceTimeOutOfRangeForTest = ErrWecomSourceTimeOutOfRange
	ErrWecomEmptyTextForTest            = ErrWecomEmptyText
)

// RunWSForTest drives the websocket transport for joint gateway tests.
func (a *WecomAdapter) RunWSForTest(ctx context.Context) error { return a.ws.Run(ctx) }
