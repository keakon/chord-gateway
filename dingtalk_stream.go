package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/keakon/chord-gateway/internal/im"
)

const (
	dingtalkRobotTopic            = "/v1.0/im/bot/messages/get"
	dingtalkStreamFrameLimit      = 1 << 20
	dingtalkStreamControlCapacity = 32
	dingtalkStreamReceiptCapacity = 128

	// DingTalk's Stream gateway expects a steady ping cadence. Mirror the
	// official SDK's interval and tolerate a couple of missed pongs before
	// declaring the socket dead instead of reconnecting on the first miss.
	dingtalkStreamPingInterval  = 60 * time.Second
	dingtalkStreamPongTimeout   = 10 * time.Second
	dingtalkStreamPongMissLimit = 2
)

// Headers retain raw values because the platform sends time as either a JSON
// number or a decimal string. Data is the protocol's JSON-encoded string.
type dingtalkFrame struct {
	SpecVersion string                     `json:"specVersion"`
	Type        string                     `json:"type"`
	Headers     map[string]json.RawMessage `json:"headers"`
	Data        string                     `json:"data"`
}

func (f dingtalkFrame) headerString(name string) string {
	var value string
	if json.Unmarshal(f.Headers[name], &value) != nil && name == "time" {
		var number json.Number
		if json.Unmarshal(f.Headers[name], &number) == nil {
			return number.String()
		}
	}
	return value
}

type dingtalkStreamCallbacks struct {
	OnConnected   func(context.Context) error
	OnUnavailable func()
	// OnCallback runs on the sole socket reader. It must capture and enqueue
	// admission without blocking, then return the authoritative result channel.
	OnCallback      func(context.Context, dingtalkFrame) (<-chan im.ReceiveResult, error)
	OnDiagnostic    func(string)
	OnRetry         func(category string, attempt int, delay time.Duration)
	OnReceiveResult func(im.ReceiveResult)
}

type dingtalkStream struct {
	callbacks      dingtalkStreamCallbacks
	openConnection func(context.Context) (string, string, error)
	dialer         *websocket.Dialer
	// This switch is only used by in-process protocol tests. No configuration
	// option or production constructor enables plaintext endpoints.
	allowInsecureEndpoint                   bool
	pingInterval, pongTimeout, writeTimeout time.Duration
	backoff                                 func(int) time.Duration
	stableAfter                             time.Duration
}

func newDingtalkStreamWithProxy(api *dingtalkAPI, callbacks dingtalkStreamCallbacks, proxyFunc func(*http.Request) (*url.URL, error)) *dingtalkStream {
	proxy := im.WebsocketProxyFunc(proxyFunc)
	return &dingtalkStream{
		callbacks:      callbacks,
		openConnection: api.openConnection,
		dialer:         &websocket.Dialer{Proxy: proxy, HandshakeTimeout: 10 * time.Second},
		pingInterval:   dingtalkStreamPingInterval,
		pongTimeout:    dingtalkStreamPongTimeout,
		writeTimeout:   5 * time.Second,
		stableAfter:    60 * time.Second,
		backoff:        dingtalkStreamBackoff,
	}
}

type dingtalkStreamTerminalError struct{ category string }

func (e *dingtalkStreamTerminalError) Error() string  { return "dingtalk stream: " + e.category }
func (e *dingtalkStreamTerminalError) Terminal() bool { return true }

type dingtalkStreamError struct{ category string }

func (e *dingtalkStreamError) Error() string { return "dingtalk stream: " + e.category }

func dingtalkStreamRetryCategory(err error, stage string) string {
	var streamError *dingtalkStreamError
	if errors.As(err, &streamError) {
		return streamError.category
	}
	var apiError *dingtalkAPIError
	if errors.As(err, &apiError) && apiError.Category == "rate_limited" {
		return "rate_limited"
	}
	// Unknown errors may contain credential URLs or response bodies. Only
	// report the supervisor's fixed stage, never the underlying error string.
	return stage + "_failed"
}

func dingtalkStreamIsTerminal(err error) bool {
	var terminal interface{ Terminal() bool }
	return errors.As(err, &terminal) && terminal.Terminal()
}

// Run is the only registration and reconnect supervisor. Each connection is
// fully joined before the next one starts, so old completions cannot publish
// readiness or ACK on a newer socket.
func (s *dingtalkStream) Run(ctx context.Context) error {
	failures := 0
	lastCategory := ""
	var lastReport time.Time
	for ctx.Err() == nil {
		stage := "registration"
		registrationCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		endpoint, ticket, err := s.openConnection(registrationCtx)
		cancel()
		if err == nil && ctx.Err() == nil {
			var connectionURL string
			connectionURL, err = dingtalkStreamURL(endpoint, ticket, s.allowInsecureEndpoint)
			if err == nil {
				stage = "handshake"
				var conn *websocket.Conn
				var response *http.Response
				conn, response, err = s.dialConnection(ctx, connectionURL)
				if err != nil {
					if response != nil && response.Body != nil {
						_ = response.Body.Close()
					}
					// Never return the dial error: it may embed the credential URL.
					if response != nil && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
						err = &dingtalkStreamTerminalError{category: "auth_failed"}
					} else {
						err = &dingtalkStreamError{category: "handshake_failed"}
					}
				} else {
					stage = "connection"
					connectedAt := time.Now()
					err = s.runConnection(ctx, conn)
					if time.Since(connectedAt) >= s.stableAfter {
						failures = 0
					}
				}
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		if dingtalkStreamIsTerminal(err) {
			return err
		}
		delay := s.backoff(failures)
		category := dingtalkStreamRetryCategory(err, stage)
		if s.callbacks.OnRetry != nil && (category != lastCategory || time.Since(lastReport) >= time.Minute) {
			s.callbacks.OnRetry(category, failures+1, delay)
			lastCategory, lastReport = category, time.Now()
		}
		if !im.SleepCtx(ctx, delay) {
			return nil
		}
		failures++
	}
	return nil
}

// Gorilla applies the handshake deadline, but cancellation after TCP dialing
// does not interrupt a stalled HTTP upgrade read. Close the underlying socket
// on cancellation and join the cancellation callback before leaving the dial.
func (s *dingtalkStream) dialConnection(ctx context.Context, connectionURL string) (*websocket.Conn, *http.Response, error) {
	dialer := *s.dialer
	type dialWatcher struct {
		stop func() bool
		done chan struct{}
	}
	var watchers []dialWatcher
	watch := func(conn net.Conn, err error) (net.Conn, error) {
		if err != nil {
			return conn, err
		}
		done := make(chan struct{})
		stop := context.AfterFunc(ctx, func() { _ = conn.Close(); close(done) })
		watchers = append(watchers, dialWatcher{stop, done})
		return conn, nil
	}
	defer func() {
		for _, watcher := range watchers {
			if !watcher.stop() {
				<-watcher.done
			}
		}
	}()
	netDial := dialer.NetDialContext
	if netDial == nil {
		if dialer.NetDial != nil {
			netDial = func(_ context.Context, network, address string) (net.Conn, error) {
				return dialer.NetDial(network, address)
			}
		} else {
			netDial = (&net.Dialer{}).DialContext
		}
	}
	dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return watch(netDial(ctx, network, address))
	}
	if tlsDial := dialer.NetDialTLSContext; tlsDial != nil {
		dialer.NetDialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return watch(tlsDial(ctx, network, address))
		}
	}
	return dialer.DialContext(ctx, connectionURL, nil)
}

func dingtalkStreamURL(endpoint, ticket string, allowInsecure bool) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u == nil || u.Host == "" || u.Hostname() == "" || u.User != nil || strings.Contains(endpoint, "#") || u.Opaque != "" || (u.Scheme != "wss" && !(allowInsecure && u.Scheme == "ws")) || ticket == "" {
		return "", &dingtalkStreamTerminalError{category: "invalid_endpoint"}
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", &dingtalkStreamTerminalError{category: "invalid_endpoint"}
	}
	query.Set("ticket", ticket)
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func dingtalkStreamBackoff(attempt int) time.Duration { return im.ReconnectBackoff(attempt) }

type dingtalkStreamWrite struct {
	kind    int
	data    []byte
	written chan struct{}
}

type dingtalkStreamConnection struct {
	stream            *dingtalkStream
	conn              *websocket.Conn
	ctx               context.Context
	cancel            context.CancelFunc
	control, receipts chan dingtalkStreamWrite
	admissionSlots    chan struct{}
	errors            chan error
	failOnce          sync.Once
	wg                sync.WaitGroup
	heartbeatMu       sync.Mutex
	pingToken         string
	pong              chan struct{}
}

func (s *dingtalkStream) runConnection(root context.Context, conn *websocket.Conn) error {
	ctx, cancel := context.WithCancel(root)
	c := &dingtalkStreamConnection{
		stream: s, conn: conn, ctx: ctx, cancel: cancel,
		control:        make(chan dingtalkStreamWrite, dingtalkStreamControlCapacity),
		receipts:       make(chan dingtalkStreamWrite, dingtalkStreamReceiptCapacity),
		admissionSlots: make(chan struct{}, dingtalkStreamReceiptCapacity),
		errors:         make(chan error, 1),
	}
	conn.SetReadLimit(dingtalkStreamFrameLimit)
	c.refreshReadDeadline()
	conn.SetPingHandler(func(data string) error {
		// A saturated control queue must not tear the socket down: dropping a
		// pong is recoverable, while failing the reader is not.
		c.tryEnqueue(c.control, dingtalkStreamWrite{kind: websocket.PongMessage, data: []byte(data)})
		return nil
	})
	conn.SetPongHandler(func(data string) error {
		c.heartbeatMu.Lock()
		if c.pingToken != "" && c.pingToken == data {
			c.pingToken = ""
			close(c.pong)
		}
		c.heartbeatMu.Unlock()
		return nil
	})
	// The library's default close handler writes from the reader. All writes,
	// including control frames, must instead belong to our writer.
	conn.SetCloseHandler(func(int, string) error { return nil })
	c.wg.Go(func() { <-ctx.Done(); _ = conn.Close() })
	defer func() { cancel(); _ = conn.Close(); c.wg.Wait() }()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.callbacks.OnConnected != nil {
		if err := s.callbacks.OnConnected(ctx); err != nil {
			c.fail(err)
			return err
		}
	}
	if ctx.Err() != nil {
		c.fail(ctx.Err())
		return ctx.Err()
	}
	if s.callbacks.OnDiagnostic != nil {
		s.callbacks.OnDiagnostic("connected")
	}
	c.wg.Go(c.writer)
	c.wg.Go(c.heartbeat)
	c.wg.Go(c.reader)
	select {
	case err := <-c.errors:
		return err
	case <-ctx.Done():
		c.fail(ctx.Err())
		// fail cancels before publishing its classified error. Preserve the
		// winner even when cancellation and publication become ready together.
		return <-c.errors
	}
}

func (c *dingtalkStreamConnection) fail(err error) {
	c.failOnce.Do(func() {
		if c.stream.callbacks.OnUnavailable != nil {
			c.stream.callbacks.OnUnavailable()
		}
		c.cancel()
		_ = c.conn.Close()
		c.errors <- err
	})
}

func (c *dingtalkStreamConnection) enqueue(queue chan dingtalkStreamWrite, message dingtalkStreamWrite) bool {
	if c.ctx.Err() != nil {
		return false
	}
	select {
	case queue <- message:
		return true
	default:
		if c.stream.callbacks.OnDiagnostic != nil {
			c.stream.callbacks.OnDiagnostic("backpressure")
		}
		c.fail(&dingtalkStreamError{category: "backpressure"})
		return false
	}
}

// tryEnqueue drops a single receipt when the writer is behind instead of
// tearing down the whole connection. A transient burst must not stop the bot;
// the caller reports 500 for this frame and keeps draining ACKs.
func (c *dingtalkStreamConnection) tryEnqueue(queue chan dingtalkStreamWrite, message dingtalkStreamWrite) bool {
	if c.ctx.Err() != nil {
		return false
	}
	select {
	case queue <- message:
		return true
	default:
		if c.stream.callbacks.OnDiagnostic != nil {
			c.stream.callbacks.OnDiagnostic("backpressure")
		}
		return false
	}
}

func (c *dingtalkStreamConnection) writer() {
	var pending *dingtalkStreamWrite
	for c.ctx.Err() == nil {
		var message dingtalkStreamWrite
		select {
		case message = <-c.control:
		default:
			if pending != nil {
				message = *pending
				pending = nil
			} else {
				select {
				case <-c.ctx.Done():
					return
				case message = <-c.control:
				case receipt := <-c.receipts:
					// A control packet queued alongside a receipt wins even if
					// select happened to choose the receipt first.
					select {
					case message = <-c.control:
						pending = &receipt
					default:
						message = receipt
					}
				}
			}
		}
		deadline := time.Now().Add(c.stream.writeTimeout)
		var err error
		if message.kind == websocket.TextMessage {
			err = c.conn.SetWriteDeadline(deadline)
			if err == nil {
				err = c.conn.WriteMessage(message.kind, message.data)
			}
		} else {
			err = c.conn.WriteControl(message.kind, message.data, deadline)
		}
		if err != nil {
			c.fail(&dingtalkStreamError{category: "write_failed"})
			return
		}
		if message.written != nil {
			close(message.written)
		}
	}
}

func (c *dingtalkStreamConnection) heartbeat() {
	ticker := time.NewTicker(c.stream.pingInterval)
	defer ticker.Stop()
	misses := 0
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
		}
		c.refreshReadDeadline()
		var entropy [16]byte
		if _, err := rand.Read(entropy[:]); err != nil {
			c.fail(&dingtalkStreamError{category: "heartbeat_failed"})
			return
		}
		token := hex.EncodeToString(entropy[:])
		pong := make(chan struct{})
		c.heartbeatMu.Lock()
		c.pingToken, c.pong = token, pong
		c.heartbeatMu.Unlock()
		written := make(chan struct{})
		if !c.enqueue(c.control, dingtalkStreamWrite{kind: websocket.PingMessage, data: []byte(token), written: written}) {
			return
		}
		select {
		case <-c.ctx.Done():
			return
		case <-written:
		}
		timer := time.NewTimer(c.stream.pongTimeout)
		select {
		case <-c.ctx.Done():
			timer.Stop()
			return
		case <-pong:
			timer.Stop()
			misses = 0
		case <-timer.C:
			misses++
			if misses >= dingtalkStreamPongMissLimit {
				c.fail(&dingtalkStreamError{category: "heartbeat_timeout"})
				return
			}
		}
	}
}

// refreshReadDeadline is driven by the heartbeat loop so a half-open socket
// cannot leave the reader blocked forever. It fires only after two ping
// intervals elapse without any inbound frame.
func (c *dingtalkStreamConnection) refreshReadDeadline() {
	_ = c.conn.SetReadDeadline(time.Now().Add(2 * c.stream.pingInterval))
}

func (c *dingtalkStreamConnection) reader() {
	for c.ctx.Err() == nil {
		kind, data, err := c.conn.ReadMessage()
		if err != nil {
			c.fail(&dingtalkStreamError{category: "read_failed"})
			return
		}
		if kind != websocket.TextMessage {
			c.fail(&dingtalkStreamError{category: "invalid_frame"})
			return
		}
		var frame dingtalkFrame
		if json.Unmarshal(data, &frame) != nil {
			c.fail(&dingtalkStreamError{category: "invalid_frame"})
			return
		}
		if !c.handleFrame(frame) {
			return
		}
	}
}

func (c *dingtalkStreamConnection) handleFrame(frame dingtalkFrame) bool {
	if c.ctx.Err() != nil {
		return false
	}
	topic := frame.headerString("topic")
	if frame.Type == "SYSTEM" {
		switch topic {
		case "ping":
			return c.respond(frame, 200, frame.Data, true)
		case "disconnect":
			c.fail(&dingtalkStreamError{category: "server_disconnect"})
			return false
		default:
			return c.respond(frame, 404, `{"response":null}`, true)
		}
	}
	if frame.Type != "CALLBACK" || topic != dingtalkRobotTopic {
		return c.respond(frame, 404, `{"response":null}`, false)
	}
	contentType := frame.headerString("contentType")
	_, hasContentType := frame.Headers["contentType"]
	if frame.SpecVersion != "1.0" || (hasContentType && contentType != "application/json") || frame.headerString("messageId") == "" {
		return c.respond(frame, 200, `{"response":null}`, false)
	}
	if c.stream.callbacks.OnCallback == nil {
		return c.respond(frame, 500, `{"response":null}`, false)
	}
	select {
	case c.admissionSlots <- struct{}{}:
	default:
		if c.stream.callbacks.OnReceiveResult != nil {
			c.stream.callbacks.OnReceiveResult(im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "stream_capacity"})
		}
		if c.stream.callbacks.OnDiagnostic != nil {
			c.stream.callbacks.OnDiagnostic("backpressure")
		}
		// Keep the connection: a burst above 128 in-flight admissions is a
		// single dropped delivery attempt, not proof the socket is unhealthy.
		_ = c.respond(frame, 500, `{"response":null}`, false)
		return true
	}
	// Preserve socket receive order at the capture/Submit boundary. Only waiting
	// for persistence moves to a bounded asynchronous worker.
	result, err := c.stream.callbacks.OnCallback(c.ctx, frame)
	if err != nil || result == nil {
		<-c.admissionSlots
		if dingtalkStreamIsTerminal(err) {
			c.fail(err)
			return false
		}
		return c.respond(frame, 500, `{"response":null}`, false)
	}
	c.wg.Go(func() {
		defer func() { <-c.admissionSlots }()
		outcome := im.WaitIngressNormalization(c.ctx, result)
		if c.stream.callbacks.OnReceiveResult != nil {
			c.stream.callbacks.OnReceiveResult(outcome)
		}
		if c.ctx.Err() != nil {
			return
		}
		code := 500
		switch outcome.Disposition {
		case im.ReceiveAccepted, im.ReceiveDuplicate, im.ReceiveRejected:
			code = 200
		}
		c.respond(frame, code, `{"response":null}`, false)
	})
	return true
}

func (c *dingtalkStreamConnection) respond(frame dingtalkFrame, code int, data string, control bool) bool {
	message := "OK"
	if code == 404 {
		message = "unsupported_topic"
	} else if code == 500 {
		message = "internal_error"
	}
	response := struct {
		Code    int               `json:"code"`
		Message string            `json:"message"`
		Headers map[string]string `json:"headers"`
		Data    string            `json:"data"`
	}{code, message, map[string]string{"messageId": frame.headerString("messageId"), "contentType": "application/json"}, data}
	encoded, _ := json.Marshal(response)
	if control {
		// Liveness frames (ping ACKs, unsupported-topic replies) are dropped
		// when the writer is behind rather than closing a healthy socket.
		c.tryEnqueue(c.control, dingtalkStreamWrite{kind: websocket.TextMessage, data: encoded})
		return true
	}
	// A full receipt queue drops this frame's 500 without stopping the reader.
	c.tryEnqueue(c.receipts, dingtalkStreamWrite{kind: websocket.TextMessage, data: encoded})
	return true
}
