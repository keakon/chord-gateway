package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const wecomWSEndpoint = "wss://openws.work.weixin.qq.com"

const wecomWSStagedMaxBytes = 4 << 20

// A single missed heartbeat ACK is treated as transient jitter; only this many
// consecutive misses close the socket, matching the official SDK's tolerance.
const wecomHeartbeatMissLimit = 2

type wecomReceivedAtContextKey struct{}

type wecomCapturedEnvelopeContextKey struct{}

type wecomWSCallback struct {
	raw []byte
	ctx context.Context
}

// WecomTransportError deliberately excludes server text and frame contents.
// Code is useful only for an explicit platform rejection; unknown delivery must
// never be retried automatically.
type WecomTransportError struct {
	Category string
	Code     int
}

func (e *WecomTransportError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("wecom: %s (code=%d)", e.Category, e.Code)
	}
	return "wecom: " + e.Category
}

// IMTextSendFailureCategory is consumed by the common output executor.
func (e *WecomTransportError) IMTextSendFailureCategory() string {
	switch e.Category {
	case "unknown_send", "platform_rejected", "backpressure", "connection_lost", "connection_unavailable", "auth_failed", "duplicate_connection", "identity_mismatch", "lost_before_accept", "ingress_prepare_failed", "invalid_message", "local_error":
		return e.Category
	default:
		return "transport_error"
	}
}

func wecomWSError(category string) error { return &WecomTransportError{Category: category} }

type wecomWSCallbacks struct {
	// OnAuthenticated runs on the supervisor while heartbeat stays active,
	// before SendMarkdown becomes ready.
	OnAuthenticated func(context.Context) error
	// OnReady publishes ingress readiness after the transport can send.
	OnReady func(context.Context) error
	// CaptureFrame freezes control authority on the reader without blocking.
	// Busy state must freeze a refusal, never defer capture until delivery.
	CaptureFrame func(context.Context, []byte) context.Context
	// OnFrame must only normalize and submit to the bounded ingress workers.
	// A non-nil error is terminal (for example a mismatched bot identity).
	OnFrame       func(context.Context, []byte) error
	OnUnavailable func(error)
	// Reports unsubmitted delivery attempts, including overflow. A prior delivery
	// of the same business ID may already be in-memory. Must not block.
	OnDroppedBeforeAccept func(count int)
}

type wecomWSTiming struct {
	dial, subscribe, send, heartbeat, heartbeatACK, write, backoff, maxBackoff time.Duration
}

type wecomWS struct {
	botID, secret string
	callbacks     wecomWSCallbacks
	endpoint      string // private test seam; production always uses wecomWSEndpoint
	dialer        *websocket.Dialer
	timing        wecomWSTiming
	stableAfter   time.Duration
	jitter        func(time.Duration) time.Duration

	mu         sync.Mutex
	current    *wecomWSSession
	generation uint64
	running    bool
	stopping   bool
	cancel     context.CancelFunc
	done       chan struct{}
	unknownACK atomic.Uint64
}

type wecomWSFrame struct {
	Cmd     string `json:"cmd,omitempty"`
	Headers struct {
		ReqID string `json:"req_id"`
	} `json:"headers"`
	Body    json.RawMessage `json:"body,omitempty"`
	ErrCode *int            `json:"errcode,omitempty"`
}

type wecomWSRequest struct {
	id     string
	kind   string
	frame  wecomWSFrame
	result chan error
}

type wecomWSFailure struct {
	err      error
	terminal bool
}

type wecomWSSession struct {
	generation uint64
	conn       *websocket.Conn
	ctx        context.Context
	cancel     context.CancelFunc
	control    chan *wecomWSRequest
	data       chan *wecomWSRequest
	failure    chan wecomWSFailure
	callbacks  chan wecomWSCallback
	workers    sync.WaitGroup
	// The following fields are protected by the transport mutex.
	pending       map[string]*wecomWSRequest
	sends         int
	ready         bool
	deliverReady  bool
	authenticated bool
	recorded      *wecomWSFailure
	dropped       int
	stagedBytes   int
}

func newWecomWS(botID, secret string, callbacks wecomWSCallbacks) *wecomWS {
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second
	return &wecomWS{
		botID: botID, secret: secret, callbacks: callbacks,
		endpoint: wecomWSEndpoint, dialer: &dialer, done: make(chan struct{}),
		stableAfter: time.Minute,
		jitter:      jitterBackoff,
		timing: wecomWSTiming{10 * time.Second, 10 * time.Second, 10 * time.Second,
			30 * time.Second, 10 * time.Second, 5 * time.Second, time.Second, 30 * time.Second},
	}
}

// Run owns reconnection and heartbeat. Each socket has exactly one reader and
// one data writer. A transport is single-use; Stop may be called before Run.
func (w *wecomWS) Run(ctx context.Context) error {
	w.mu.Lock()
	if w.stopping || w.running {
		w.mu.Unlock()
		return wecomWSError("connection_unavailable")
	}
	w.running = true
	ctx, w.cancel = context.WithCancel(ctx)
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.stopping = true
		w.cancel()
		close(w.done)
		w.mu.Unlock()
	}()

	backoff := w.timing.backoff
	for ctx.Err() == nil {
		dialCtx, cancel := context.WithTimeout(ctx, w.timing.dial)
		conn, response, err := w.dialer.DialContext(dialCtx, w.endpoint, http.Header{})
		cancel()
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if err == nil {
			failure, stable := w.runConnection(ctx, conn)
			err = failure.err
			if stable {
				backoff = w.timing.backoff
			}
			if failure.terminal {
				return err
			}
		} else {
			err = wecomWSError("connection_lost")
			w.unavailable(err)
		}
		if ctx.Err() != nil {
			break
		}
		timer := time.NewTimer(w.jitter(backoff))
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		backoff = min(backoff*2, w.timing.maxBackoff)
	}
	return nil
}

func (w *wecomWS) runConnection(ctx context.Context, conn *websocket.Conn) (failure wecomWSFailure, stable bool) {
	ctx, cancel := context.WithCancel(ctx)
	var readySince time.Time
	s := &wecomWSSession{conn: conn, ctx: ctx, cancel: cancel,
		control: make(chan *wecomWSRequest, 16), data: make(chan *wecomWSRequest, 64),
		failure: make(chan wecomWSFailure, 1), callbacks: make(chan wecomWSCallback, 64), pending: make(map[string]*wecomWSRequest)}
	conn.SetReadLimit(1 << 20)
	w.touchReadDeadline(s)
	conn.SetPingHandler(func(data string) error {
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(w.timing.write))
	})
	// Control frames are safe concurrently with the sole data writer.
	conn.SetCloseHandler(func(code int, text string) error {
		return conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(w.timing.write))
	})
	w.mu.Lock()
	w.generation++
	s.generation = w.generation
	w.current = s
	w.mu.Unlock()
	s.workers.Add(2)
	go w.read(s)
	go w.write(s)
	defer func() {
		stable = !readySince.IsZero() && time.Since(readySince) >= w.stableAfter
		w.mu.Lock()
		if s.recorded != nil && (s.recorded.terminal || !failure.terminal) {
			failure = *s.recorded
		}
		w.mu.Unlock()
		if failure.err == nil {
			failure.err = wecomWSError("connection_lost")
		}
		w.mu.Lock()
		s.ready = false
		if w.current == s {
			w.current = nil
		}
		for _, request := range s.pending {
			err := wecomWSError("connection_lost")
			if request.kind == "send" {
				err = wecomWSError("unknown_send")
			}
			w.completeLocked(s, request, err)
		}
		w.mu.Unlock()
		cancel()
		conn.Close()
		w.unavailable(failure.err)
		s.workers.Wait()
		w.mu.Lock()
		dropped := s.dropped + len(s.callbacks)
		w.mu.Unlock()
		if dropped != 0 && w.callbacks.OnDroppedBeforeAccept != nil {
			w.callbacks.OnDroppedBeforeAccept(dropped)
		}
	}()

	body, _ := json.Marshal(map[string]string{"bot_id": w.botID, "secret": w.secret})
	request, err := w.request(s, "subscribe", "aibot_subscribe", body)
	if err == nil {
		err = w.await(s, request, ctx, w.timing.subscribe)
	}
	if err != nil {
		var transportErr *WecomTransportError
		terminal := errors.As(err, &transportErr) && transportErr.Category == "auth_failed"
		return wecomWSFailure{err, terminal}, false
	}
	// Keep transport health independent of recovery and readiness publication.
	s.workers.Add(1)
	go w.heartbeat(s)
	if w.callbacks.OnAuthenticated != nil {
		if err := w.callbacks.OnAuthenticated(ctx); err != nil {
			if ctx.Err() != nil {
				return wecomWSFailure{wecomWSError("connection_lost"), false}, false
			}
			return wecomWSFailure{wecomWSError("ingress_prepare_failed"), false}, false
		}
	}
	w.mu.Lock()
	s.ready = ctx.Err() == nil
	w.mu.Unlock()
	if ctx.Err() != nil {
		return wecomWSFailure{wecomWSError("connection_lost"), false}, false
	}
	if w.callbacks.OnReady != nil {
		if err := w.callbacks.OnReady(ctx); err != nil {
			if ctx.Err() != nil {
				return wecomWSFailure{wecomWSError("connection_lost"), false}, false
			}
			return wecomWSFailure{wecomWSError("ingress_prepare_failed"), false}, false
		}
	}
	// While recovery runs, the reader stages only a bounded number of callbacks
	// and continues handling ACKs. Drain in order before direct reader delivery.
	for {
		w.mu.Lock()
		if ctx.Err() != nil {
			w.mu.Unlock()
			return wecomWSFailure{wecomWSError("connection_lost"), false}, false
		}
		select {
		case callback := <-s.callbacks:
			s.stagedBytes -= len(callback.raw)
			w.mu.Unlock()
			if w.callbacks.OnFrame != nil {
				if err := w.callbacks.OnFrame(callback.ctx, callback.raw); err != nil {
					return wecomWSFailure{wecomWSError("identity_mismatch"), true}, false
				}
			}
		default:
			s.deliverReady = true
			w.mu.Unlock()
			goto prepared
		}
	}
prepared:
	readySince = time.Now()
	select {
	case <-ctx.Done():
		return wecomWSFailure{wecomWSError("connection_lost"), false}, false
	case failure := <-s.failure:
		return failure, false
	}
}

func (w *wecomWS) heartbeat(s *wecomWSSession) {
	defer s.workers.Done()
	ticker := time.NewTicker(w.timing.heartbeat)
	defer ticker.Stop()
	misses := 0
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			w.touchReadDeadline(s)
			request, err := w.request(s, "ping", "ping", nil)
			if err == nil {
				err = w.await(s, request, s.ctx, w.timing.heartbeatACK)
			}
			if err != nil {
				misses++
				if misses >= wecomHeartbeatMissLimit {
					w.fail(s, err, false)
					return
				}
				continue
			}
			misses = 0
		}
	}
}

// touchReadDeadline is driven by the heartbeat loop. The window covers one full
// heartbeat cycle plus a missed-ACK grace period so an idle but healthy socket
// is never closed by the read deadline alone.
func (w *wecomWS) touchReadDeadline(s *wecomWSSession) {
	_ = s.conn.SetReadDeadline(time.Now().Add(2*w.timing.heartbeat + w.timing.heartbeatACK))
}

// Stop cancels dialing, backoff, ACK waits and socket IO, then joins all workers.
func (w *wecomWS) Stop() {
	w.mu.Lock()
	w.stopping = true
	if w.cancel != nil {
		w.cancel()
	}
	if w.current != nil {
		w.current.cancel()
		w.current.conn.Close()
	}
	running, done := w.running, w.done
	w.mu.Unlock()
	if running {
		<-done
	}
}

func (w *wecomWS) SendMarkdown(ctx context.Context, target wecomSendTarget, rendered string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if target.ChatID == "" || (target.ChatType != 1 && target.ChatType != 2) {
		return wecomWSError("invalid_message")
	}
	body, err := json.Marshal(map[string]any{"chatid": target.ChatID, "chat_type": target.ChatType, "msgtype": "markdown", "markdown": map[string]string{"content": rendered}})
	if err != nil {
		return wecomWSError("invalid_message")
	}
	w.mu.Lock()
	s := w.current
	ready := s != nil && s.ready && !w.stopping
	w.mu.Unlock()
	if !ready {
		return wecomWSError("connection_unavailable")
	}
	request, err := w.request(s, "send", "aibot_send_msg", body)
	if err != nil {
		return err
	}
	return w.await(s, request, ctx, w.timing.send)
}

func (w *wecomWS) request(s *wecomWSSession, kind, cmd string, body json.RawMessage) (*wecomWSRequest, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, wecomWSError("local_error")
	}
	request := &wecomWSRequest{id: fmt.Sprintf("%s-%d-%s", kind, s.generation, hex.EncodeToString(random[:])),
		kind: kind, result: make(chan error, 1), frame: wecomWSFrame{Cmd: cmd, Body: body}}
	request.frame.Headers.ReqID = request.id
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.current != s || w.stopping || s.ctx.Err() != nil || (kind == "send" && !s.ready) {
		return nil, wecomWSError("connection_unavailable")
	}
	if kind == "send" && s.sends >= 64 {
		return nil, wecomWSError("backpressure")
	}
	s.pending[request.id] = request
	queue := s.control
	if kind == "send" {
		s.sends++
		queue = s.data
	}
	select {
	case queue <- request:
		return request, nil
	default:
		w.completeLocked(s, request, wecomWSError("backpressure"))
		return nil, wecomWSError("backpressure")
	}
}

func (w *wecomWS) await(s *wecomWSSession, request *wecomWSRequest, ctx context.Context, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var err error
	select {
	case err := <-request.result:
		return err
	case <-ctx.Done():
		err = ctx.Err()
	case <-s.ctx.Done():
		err = wecomWSError("connection_lost")
	case <-timer.C:
		err = wecomWSError("connection_lost")
	}
	if request.kind == "send" {
		err = wecomWSError("unknown_send")
	}
	w.mu.Lock()
	w.completeLocked(s, request, err)
	w.mu.Unlock()
	return <-request.result
}

func (w *wecomWS) completeLocked(s *wecomWSSession, request *wecomWSRequest, err error) {
	if s.pending[request.id] != request {
		return
	}
	delete(s.pending, request.id)
	if request.kind == "send" {
		s.sends--
	}
	request.result <- err
}

func (w *wecomWS) fail(s *wecomWSSession, err error, terminal bool) {
	w.mu.Lock()
	if s.recorded == nil || (terminal && !s.recorded.terminal) {
		failure := wecomWSFailure{err, terminal}
		s.recorded = &failure
		select {
		case s.failure <- failure:
		default:
		}
	}
	w.mu.Unlock()
	// Close immediately to interrupt blocked IO, even if the supervisor is preparing ingress.
	s.cancel()
	s.conn.Close()
}

func (w *wecomWS) read(s *wecomWSSession) {
	defer s.workers.Done()
	for {
		_, raw, err := s.conn.ReadMessage()
		if err != nil {
			w.fail(s, wecomWSError("connection_lost"), false)
			return
		}
		receivedAt := time.Now()
		var frame wecomWSFrame
		if json.Unmarshal(raw, &frame) != nil {
			continue
		}
		if frame.Cmd == "" {
			w.mu.Lock()
			request := s.pending[frame.Headers.ReqID]
			if w.current != s || request == nil {
				w.unknownACK.Add(1)
				w.mu.Unlock()
				continue
			}
			var ackError error
			if frame.ErrCode == nil || *frame.ErrCode != 0 {
				category := "connection_lost"
				if request.kind == "subscribe" {
					category = "auth_failed"
				} else if request.kind == "send" {
					category = "unknown_send"
					if frame.ErrCode != nil {
						category = "platform_rejected"
					}
				}
				ackError = &WecomTransportError{Category: category}
				if frame.ErrCode != nil {
					ackError.(*WecomTransportError).Code = *frame.ErrCode
				}
			}
			if request.kind == "subscribe" && ackError == nil {
				s.authenticated = true
			}
			w.completeLocked(s, request, ackError)
			w.mu.Unlock()
			if request.kind == "subscribe" && ackError != nil {
				w.fail(s, ackError, true)
				return
			}
			continue
		}
		w.mu.Lock()
		acceptCallback := w.current == s && s.authenticated && s.ctx.Err() == nil
		w.mu.Unlock()
		if frame.Cmd != "aibot_msg_callback" && frame.Cmd != "aibot_event_callback" {
			continue
		}
		var identity struct {
			BotID string `json:"aibotid"`
		}
		if json.Unmarshal(frame.Body, &identity) != nil || identity.BotID == "" {
			continue
		}
		if identity.BotID != w.botID {
			w.fail(s, wecomWSError("identity_mismatch"), true)
			return
		}
		if frame.Cmd == "aibot_event_callback" {
			var event struct {
				Event struct {
					Type string `json:"eventtype"`
				} `json:"event"`
			}
			if json.Unmarshal(frame.Body, &event) == nil && event.Event.Type == "disconnected_event" {
				w.fail(s, wecomWSError("duplicate_connection"), true)
				return
			}
		}
		if !acceptCallback {
			continue
		}
		// The shared pure normalizer defines supported business callbacks. Events,
		// media and malformed messages never occupy recovery slots or count as lost
		// business input; identity was checked first, including unsupported types.
		if _, err := NormalizeWecomCallback(raw, w.botID, receivedAt); err != nil {
			continue
		}
		callbackCtx := context.WithValue(s.ctx, wecomReceivedAtContextKey{}, receivedAt)
		if w.callbacks.CaptureFrame != nil {
			callbackCtx = w.callbacks.CaptureFrame(callbackCtx, raw)
		}
		w.mu.Lock()
		if w.current != s || s.ctx.Err() != nil {
			s.dropped++
			w.mu.Unlock()
			continue
		}
		ready := s.deliverReady
		queued := false
		if !ready {
			if s.stagedBytes+len(raw) <= wecomWSStagedMaxBytes {
				select {
				case s.callbacks <- wecomWSCallback{raw: raw, ctx: callbackCtx}:
					s.stagedBytes += len(raw)
					queued = true
				default:
				}
			}
		}
		w.mu.Unlock()
		if !ready {
			if !queued {
				// Overflow is a dropped delivery attempt, not proof that its
				// business ID was never accepted. Keep draining ACKs and retain
				// earlier callbacks; a transient burst must not stop the bot.
				if w.callbacks.OnDroppedBeforeAccept != nil {
					w.callbacks.OnDroppedBeforeAccept(1)
				}
			}
			continue
		}
		if w.callbacks.OnFrame != nil {
			if err := w.callbacks.OnFrame(callbackCtx, raw); err != nil {
				w.fail(s, wecomWSError("identity_mismatch"), true)
				return
			}
		}
	}
}

func (w *wecomWS) write(s *wecomWSSession) {
	defer s.workers.Done()
	for {
		if s.ctx.Err() != nil {
			return
		}
		var request *wecomWSRequest
		select {
		case request = <-s.control:
		default:
			select {
			case <-s.ctx.Done():
				return
			case request = <-s.control:
			case request = <-s.data:
			}
		}
		w.mu.Lock()
		active := w.current == s && s.pending[request.id] == request
		w.mu.Unlock()
		if !active {
			continue
		}
		if err := s.conn.SetWriteDeadline(time.Now().Add(w.timing.write)); err != nil {
			w.fail(s, wecomWSError("connection_lost"), false)
			return
		}
		if err := s.conn.WriteJSON(request.frame); err != nil {
			w.fail(s, wecomWSError("connection_lost"), false)
			return
		}
	}
}

func (w *wecomWS) unavailable(err error) {
	if w.callbacks.OnUnavailable != nil {
		w.callbacks.OnUnavailable(err)
	}
}
