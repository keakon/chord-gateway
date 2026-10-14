// Derived from github.com/disgoorg/disgo/gateway v0.19.6; see UPSTREAM.md.
package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/gorilla/websocket"
)

// Version defines which discord API version disgo should use to connect to discord.
const Version = 10

// URL is the default URL used to connect to the Discord gateway.
const URL = "wss://gateway.discord.gg"

const maximumConnectDelay = 60 * time.Second

// ReplayCapacity bounds useful captured dispatches before RESUMED. A single
// shard rarely buffers more than a few dozen events across a transient
// reconnect; 1024 covers minutes of outage without entering a fatal loop.
const ReplayCapacity = 1024

// ErrReplayCapacityExceeded permanently stops replay before advancing its
// resume checkpoint, so callers observe an explicit capacity failure.
var ErrReplayCapacityExceeded = errors.New("discord replay_capacity_exceeded")

// ErrSessionStartLimitFatal marks authentication, sharding or invalid quota
// responses which cannot be repaired by reconnecting.
var ErrSessionStartLimitFatal = errors.New("discord identify_quota_unavailable")

// Status is the state that the client is currently in.
type Status int

// IsConnected returns whether the Gateway is connected.
func (s Status) IsConnected() bool {
	switch s {
	case StatusWaitingForHello, StatusIdentifying, StatusWaitingForReady, StatusReady:
		return true
	default:
		return false
	}
}

// String returns the string representation of the Status.
func (s Status) String() string {
	switch s {
	case StatusUnconnected:
		return "Unconnected"
	case StatusConnecting:
		return "Connecting"
	case StatusWaitingForHello:
		return "WaitingForHello"
	case StatusIdentifying:
		return "Identifying"
	case StatusResuming:
		return "Resuming"
	case StatusWaitingForReady:
		return "WaitingForReady"
	case StatusReady:
		return "Ready"
	case StatusDisconnected:
		return "Disconnected"
	default:
		return "Unknown"
	}
}

// Indicates how far along the client is too connecting.
const (
	// StatusUnconnected is the initial state when a new Gateway is created.
	StatusUnconnected Status = iota

	// StatusConnecting is the state when the client is connecting to the Discord gateway.
	StatusConnecting

	// StatusWaitingForHello is the state when the Gateway is waiting for the first OpcodeHello packet.
	StatusWaitingForHello

	// StatusIdentifying is the state when the Gateway received its first OpcodeHello packet and now sends a OpcodeIdentify packet.
	StatusIdentifying

	// StatusResuming is the state when the Gateway received its first OpcodeHello packet and now sends a OpcodeResume packet.
	StatusResuming

	// StatusWaitingForReady is the state when the Gateway received sent a OpcodeIdentify or OpcodeResume packet and now waits for a OpcodeDispatch with EventTypeReady packet.
	StatusWaitingForReady

	// StatusReady is the state when the Gateway received a OpcodeDispatch with EventTypeReady packet.
	StatusReady

	// StatusDisconnected is the state when the Gateway is disconnected.
	// Either due to an error or because the Gateway was closed gracefully.
	StatusDisconnected
)

type (
	// EventHandlerFunc is a function that is called when an event is received.
	EventHandlerFunc func(gateway Gateway, eventType EventType, sequenceNumber int, event EventData)

	// CreateFunc is a type that is used to create a new Gateway(s).
	CreateFunc func(ctx context.Context, token string, eventHandlerFunc EventHandlerFunc, opts ...ConfigOpt) Gateway

	// CloseHandlerFunc is a function that is called when the Gateway is closed.
	CloseHandlerFunc func(gateway Gateway, err error, reconnect bool)
)

// Gateway is what is used to connect to discord.
type Gateway interface {
	// Generation identifies the current connection attempt.
	Generation() uint64

	// ShardID returns the shard ID that this Gateway is configured to use.
	ShardID() int

	// ShardCount returns the total number of shards that this Gateway is configured to use.
	ShardCount() int

	// SessionID returns the session ID that is used by this Gateway.
	// This may be nil if the Gateway was never connected to Discord, was gracefully closed with websocket.CloseNormalClosure or websocket.CloseGoingAway.
	SessionID() *string

	// LastSequenceReceived returns the last sequence number that was received by the Gateway.
	// This may be nil if the Gateway was never connected to Discord, was gracefully closed with websocket.CloseNormalClosure or websocket.CloseGoingAway.
	LastSequenceReceived() *int

	// ResumeURL returns the resume url that was received by the Gateway
	ResumeURL() *string

	// Intents returns the Intents that are used by this Gateway.
	Intents() Intents

	// Open connects this Gateway to the Discord API.
	Open(ctx context.Context) error

	// Close gracefully closes the Gateway with the websocket.CloseNormalClosure code.
	// If the context is done, the Gateway connection will be killed.
	Close(ctx context.Context)

	// Status returns the Status of the Gateway.
	Status() Status

	// Send sends a message to the Discord gateway with the opCode and data.
	// If context is deadline exceeds, the message sending will be aborted.
	Send(ctx context.Context, op Opcode, data MessageData) error

	// Latency returns the latency of the Gateway.
	// This is calculated by the time it takes to send a heartbeat and receive a heartbeat ack by discord.
	Latency() time.Duration

	// Presence returns the current presence of the Gateway.
	Presence() *MessageDataPresenceUpdate
}

// New binds every connection and worker to lifecycle. Close permanently stops it.
func New(lifecycle context.Context, token string, handler EventHandlerFunc, opts ...ConfigOpt) Gateway {
	cfg := defaultConfig()
	cfg.apply(opts)
	ctx, cancel := context.WithCancel(lifecycle)
	g := &gatewayImpl{config: cfg, eventHandlerFunc: handler, token: token, ctx: ctx, cancel: cancel,
		open: make(chan struct{}, 1), done: make(chan struct{}), stateChanged: make(chan struct{}), writeGate: make(chan struct{}, 1)}
	g.config.SessionID = copyPtr(cfg.SessionID)
	g.config.LastSequenceReceived = copyPtr(cfg.LastSequenceReceived)
	g.config.ResumeURL = copyPtr(cfg.ResumeURL)
	g.quotaReset = time.Now().Add(cfg.SessionResetAfter)
	g.wg.Add(1) // Only the constructor adds to this group; Close may now safely Wait.
	go g.supervise()
	return g
}

type gatewayImpl struct {
	config                config
	eventHandlerFunc      EventHandlerFunc
	token                 string
	ctx                   context.Context
	cancel                context.CancelFunc
	open                  chan struct{}
	done                  chan struct{}
	stateChanged          chan struct{}
	active                bool
	wg                    sync.WaitGroup
	writeGate             chan struct{}
	mu                    sync.Mutex
	conn                  transport
	socket                *websocket.Conn
	connCtx               context.Context
	status                Status
	generation            uint64
	stopping              bool
	terminalErr           error
	sessionValidated      bool
	heartbeatInterval     time.Duration
	lastHeartbeatSent     time.Time
	lastHeartbeatReceived time.Time
	lastIdentify          time.Time
	quotaReset            time.Time
	quotaLocallyRefilled  bool
}

func copyPtr[T any](v *T) *T {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}
func (g *gatewayImpl) ShardID() int     { return g.config.ShardID }
func (g *gatewayImpl) ShardCount() int  { return g.config.ShardCount }
func (g *gatewayImpl) Intents() Intents { return g.config.Intents }
func (g *gatewayImpl) SessionID() *string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return copyPtr(g.config.SessionID)
}
func (g *gatewayImpl) ResumeURL() *string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return copyPtr(g.config.ResumeURL)
}
func (g *gatewayImpl) LastSequenceReceived() *int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return copyPtr(g.config.LastSequenceReceived)
}
func (g *gatewayImpl) Status() Status     { g.mu.Lock(); defer g.mu.Unlock(); return g.status }
func (g *gatewayImpl) Generation() uint64 { g.mu.Lock(); defer g.mu.Unlock(); return g.generation }
func (g *gatewayImpl) Latency() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastHeartbeatReceived.Sub(g.lastHeartbeatSent)
}
func (g *gatewayImpl) Presence() *MessageDataPresenceUpdate {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := copyPtr(g.config.Presence)
	if p != nil {
		p.Activities = append([]discord.Activity(nil), p.Activities...)
	}
	return p
}

func (g *gatewayImpl) transition(state Status, generation uint64) bool {
	g.mu.Lock()
	if generation != g.generation || ((g.stopping || !g.active) && state != StatusDisconnected) {
		g.mu.Unlock()
		return false
	}
	changed := g.status != state
	g.status = state
	if changed {
		close(g.stateChanged)
		g.stateChanged = make(chan struct{})
	}
	g.mu.Unlock()
	if changed && g.config.OnStateChange != nil {
		g.config.OnStateChange(state, generation)
	}
	return true
}
func (g *gatewayImpl) Open(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case g.open <- struct{}{}:
	default:
	}
	for {
		g.mu.Lock()
		stopping, state, terminal, changed := g.stopping, g.status, g.terminalErr, g.stateChanged
		g.mu.Unlock()
		if terminal != nil {
			return terminal
		}
		if stopping {
			return context.Canceled
		}
		if state == StatusReady {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-g.ctx.Done():
			g.mu.Lock()
			terminal = g.terminalErr
			g.mu.Unlock()
			if terminal != nil {
				return terminal
			}
			return g.ctx.Err()
		case <-changed:
		}
	}
}
func (g *gatewayImpl) Close(_ context.Context) {
	g.mu.Lock()
	g.stopping = true
	g.cancel()
	socket := g.socket
	generation := g.generation
	g.clearSessionLocked()
	g.mu.Unlock()
	g.transition(StatusDisconnected, generation)
	// Closing the underlying socket is concurrency safe and interrupts blocked I/O.
	if socket != nil {
		_ = socket.Close()
	}
	g.wg.Wait()
}
func (g *gatewayImpl) clearSessionLocked() {
	g.config.SessionID = nil
	g.config.LastSequenceReceived = nil
	g.config.ResumeURL = nil
	g.sessionValidated = false
}
func wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type connectionResult struct {
	err   error
	fatal bool
	delay time.Duration
}

// supervise is the only goroutine allowed to dial or begin a connection generation.
func (g *gatewayImpl) supervise() {
	defer g.wg.Done()
	defer close(g.done)
	defer func() {
		g.mu.Lock()
		g.stopping = true
		generation := g.generation
		g.clearSessionLocked()
		g.mu.Unlock()
		g.transition(StatusDisconnected, generation)
		g.cancel()
	}()
	select {
	case <-g.ctx.Done():
		return
	case <-g.open:
	}
	backoff := time.Second
	for g.ctx.Err() == nil {
		g.mu.Lock()
		g.generation++
		g.active = true
		generation := g.generation
		g.mu.Unlock()
		g.transition(StatusConnecting, generation)
		result := g.runConnection(generation)
		g.transition(StatusDisconnected, generation)
		if g.ctx.Err() != nil {
			return
		}
		if result.fatal || !g.config.AutoReconnect {
			g.mu.Lock()
			g.terminalErr = result.err
			g.mu.Unlock()
			if g.config.CloseHandler != nil {
				g.config.CloseHandler(g, result.err, !result.fatal)
			}
			return
		}
		delay := result.delay
		if delay == 0 {
			delay = backoff
		}
		if wait(g.ctx, delay) != nil {
			return
		}
		if backoff < maximumConnectDelay {
			backoff *= 2
		}
		if backoff > maximumConnectDelay {
			backoff = maximumConnectDelay
		}
	}
}

func (g *gatewayImpl) waitIdentify(ctx context.Context) error {
	// Only the supervisor and its current listener touch quota bookkeeping.
	for g.config.SessionLimitSet && g.config.SessionRemaining == 0 {
		if err := wait(ctx, time.Until(g.quotaReset)); err != nil {
			return err
		}
		if g.config.SessionStartLimitRefresh == nil {
			// Compatibility for standalone SDK users: one initial expiry may refill,
			// but the old reset_after is never reused as a new window duration.
			if g.quotaLocallyRefilled {
				return fmt.Errorf("%w: Identify quota refresh required", ErrSessionStartLimitFatal)
			}
			g.quotaLocallyRefilled = true
			g.config.SessionRemaining = g.config.SessionTotal
			break
		}
		limit, err := g.config.SessionStartLimitRefresh(ctx)
		if err != nil {
			return err
		}
		if limit.Total <= 0 || limit.Remaining < 0 || limit.Remaining > limit.Total || limit.ResetAfter < 0 ||
			uint64(limit.ResetAfter) > uint64((1<<63-1)/time.Millisecond) || (limit.Remaining == 0 && limit.ResetAfter == 0) {
			return fmt.Errorf("%w: invalid refreshed session start limit", ErrSessionStartLimitFatal)
		}
		g.config.SessionTotal = limit.Total
		g.config.SessionRemaining = limit.Remaining
		g.config.SessionResetAfter = time.Duration(limit.ResetAfter) * time.Millisecond
		g.quotaReset = time.Now().Add(g.config.SessionResetAfter)
	}
	if !g.lastIdentify.IsZero() {
		if err := wait(ctx, time.Until(g.lastIdentify.Add(5*time.Second))); err != nil {
			return err
		}
	}
	return nil
}

func (g *gatewayImpl) runConnection(generation uint64) connectionResult {
	ctx, cancel := context.WithCancel(g.ctx)
	defer cancel()
	if g.config.SessionLimitSet && (g.config.SessionTotal <= 0 || g.config.SessionRemaining < 0 || g.config.SessionRemaining > g.config.SessionTotal || g.config.SessionResetAfter < 0) {
		return connectionResult{err: fmt.Errorf("%w: invalid session start limit", ErrSessionStartLimitFatal), fatal: true}
	}
	g.mu.Lock()
	wsURL := g.config.URL
	if g.config.ResumeURL != nil && *g.config.ResumeURL != "" && g.config.EnableResumeURL {
		wsURL = *g.config.ResumeURL
	}
	needsIdentify := g.config.SessionID == nil || g.config.LastSequenceReceived == nil
	g.mu.Unlock()
	if needsIdentify {
		if err := g.waitIdentify(ctx); err != nil {
			return connectionResult{err: err, fatal: errors.Is(err, ErrSessionStartLimitFatal)}
		}
	}
	u, err := url.Parse(wsURL)
	if err != nil {
		return connectionResult{err: fmt.Errorf("invalid gateway URL: %w", err), fatal: true}
	}
	values := u.Query()
	values.Set("v", strconv.Itoa(Version))
	values.Set("encoding", "json")
	u.RawQuery = values.Encode()
	socket, response, err := g.config.Dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		fatal := response != nil && (response.StatusCode == 401 || response.StatusCode == 403)
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return connectionResult{err: fmt.Errorf("gateway dial failed: %w", err), fatal: fatal}
	}
	socket.SetReadLimit(ReadLimit)
	socket.SetCloseHandler(func(int, string) error { return nil })
	conn := &jsonTransport{conn: socket}
	g.mu.Lock()
	if g.stopping {
		g.mu.Unlock()
		_ = conn.Close()
		return connectionResult{err: ctx.Err()}
	}
	g.conn, g.socket, g.connCtx = conn, socket, ctx
	g.lastHeartbeatSent, g.lastHeartbeatReceived = time.Time{}, time.Now()
	g.mu.Unlock()
	g.config.RateLimiter.Reset()
	g.transition(StatusWaitingForHello, generation)
	// Workers can only be created here, before the matching local Wait.
	var workers sync.WaitGroup
	results := make(chan connectionResult, 1)
	var resultOnce sync.Once
	report := func(result connectionResult) {
		resultOnce.Do(func() {
			g.mu.Lock()
			g.active = false
			g.mu.Unlock()
			g.transition(StatusDisconnected, generation)
			results <- result
		})
	}
	hello := make(chan time.Duration, 1)
	workers.Add(2)
	go func() { defer workers.Done(); g.listen(ctx, conn, generation, hello, report) }()
	go func() { defer workers.Done(); g.heartbeat(ctx, conn, generation, hello, report) }()
	var result connectionResult
	select {
	case <-ctx.Done():
		result.err = ctx.Err()
	case result = <-results:
	}
	cancel()
	_ = socket.Close()
	workers.Wait()
	_ = conn.Close() // Compression resources are only released after the reader exited.
	g.mu.Lock()
	if g.generation == generation {
		g.conn = nil
		g.socket = nil
		g.connCtx = nil
	}
	g.mu.Unlock()
	return result
}

func (g *gatewayImpl) Send(ctx context.Context, op Opcode, data MessageData) error {
	g.mu.Lock()
	conn, generation, state := g.conn, g.generation, g.status
	g.mu.Unlock()
	if state != StatusReady {
		return discord.ErrShardNotReady
	}
	return g.send(ctx, conn, generation, NormalCommandType, op, data)
}
func (g *gatewayImpl) send(ctx context.Context, conn transport, generation uint64, kind RateLimiterCommandType, op Opcode, data MessageData) error {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(g.ctx, cancel)
	defer func() { stop(); cancel() }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case g.writeGate <- struct{}{}:
	}
	defer func() { <-g.writeGate }()
	g.mu.Lock()
	valid := !g.stopping && g.active && generation == g.generation && conn != nil && conn == g.conn
	connectionCtx := g.connCtx
	g.mu.Unlock()
	if !valid {
		return discord.ErrShardNotConnected
	}
	stopConnection := context.AfterFunc(connectionCtx, cancel)
	defer stopConnection()
	if err := g.config.RateLimiter.Wait(ctx, kind); err != nil {
		return err
	}
	defer g.config.RateLimiter.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return conn.WriteMessage(Message{Op: op, D: data})
}

func (g *gatewayImpl) heartbeat(ctx context.Context, conn transport, generation uint64, hello <-chan time.Duration, report func(connectionResult)) {
	var interval time.Duration
	select {
	case <-ctx.Done():
		return
	case interval = <-hello:
	}
	// Discord requires a randomized initial heartbeat, then the HELLO interval.
	timer := time.NewTimer(time.Duration(rand.Float64() * float64(interval)))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		g.mu.Lock()
		missed := g.lastHeartbeatSent.After(g.lastHeartbeatReceived)
		g.mu.Unlock()
		if missed {
			report(connectionResult{err: errors.New("heartbeat ACK not received")})
			return
		}
		if err := g.sendHeartbeat(ctx, conn, generation); err != nil {
			report(connectionResult{err: err})
			return
		}
		timer.Reset(interval)
	}
}
func (g *gatewayImpl) sendHeartbeat(ctx context.Context, conn transport, generation uint64) error {
	g.mu.Lock()
	seq := copyPtr(g.config.LastSequenceReceived)
	g.mu.Unlock()
	sequence := 0
	if seq != nil {
		sequence = *seq
	}
	g.mu.Lock()
	g.lastHeartbeatSent = time.Now()
	g.mu.Unlock()
	return g.send(ctx, conn, generation, InternalCommandType, OpcodeHeartbeat, MessageDataHeartbeat(sequence))
}

func (g *gatewayImpl) listen(ctx context.Context, conn transport, generation uint64, hello chan<- time.Duration, report func(connectionResult)) {
	helloReceived := false
	ready := false
	resuming := false
	replayDelivered := false
	var resumeCheckpoint *int
	var replay []func()
	defer func() {
		// Events buffered before RESUMED have not been handed to the common ingress.
		// Restore the previous checkpoint if replay did not complete.
		if resuming && !replayDelivered {
			g.mu.Lock()
			if g.generation == generation && g.sessionValidated && g.config.SessionID != nil {
				g.config.LastSequenceReceived = copyPtr(resumeCheckpoint)
			}
			g.mu.Unlock()
		}
	}()
	for {
		message, err := conn.ReceiveMessage()
		if err != nil {
			result := connectionResult{err: err}
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				result.fatal = !CloseEventCodeByCode(closeErr.Code).Reconnect
				if closeErr.Code == 4007 || closeErr.Code == 4009 {
					g.mu.Lock()
					if g.generation == generation {
						g.clearSessionLocked()
					}
					g.mu.Unlock()
				}
			}
			report(result)
			return
		}
		g.mu.Lock()
		current := !g.stopping && g.active && g.generation == generation
		g.mu.Unlock()
		if !current || ctx.Err() != nil {
			return
		}
		if message == nil {
			continue
		}
		switch message.Op {
		case OpcodeHello:
			if helloReceived {
				report(connectionResult{err: errors.New("duplicate HELLO")})
				return
			}
			helloReceived = true
			interval := time.Duration(message.D.(MessageDataHello).HeartbeatInterval) * time.Millisecond
			if interval <= 0 {
				report(connectionResult{err: errors.New("invalid heartbeat interval"), fatal: true})
				return
			}
			g.mu.Lock()
			g.heartbeatInterval = interval
			session, seq := copyPtr(g.config.SessionID), copyPtr(g.config.LastSequenceReceived)
			g.mu.Unlock()
			hello <- interval
			var data MessageData
			op := OpcodeResume
			if session != nil && seq != nil {
				resuming = true
				resumeCheckpoint = copyPtr(seq)
				g.transition(StatusResuming, generation)
				data = MessageDataResume{Token: g.token, SessionID: *session, Seq: *seq}
			} else {
				g.transition(StatusIdentifying, generation)
				op = OpcodeIdentify
				data = MessageDataIdentify{Token: g.token, Properties: IdentifyCommandDataProperties{OS: g.config.OS, Browser: g.config.Browser, Device: g.config.Device},
					LargeThreshold: g.config.LargeThreshold, Intents: g.config.Intents, Presence: g.Presence(), Shard: &[2]int{g.ShardID(), g.ShardCount()}}
			}
			if op == OpcodeIdentify {
				g.lastIdentify = time.Now()
				if g.config.SessionLimitSet && g.config.SessionRemaining > 0 {
					g.config.SessionRemaining--
				}
			}
			if err := g.send(ctx, conn, generation, InternalCommandType, op, data); err != nil {
				report(connectionResult{err: err})
				return
			}
			g.transition(StatusWaitingForReady, generation)
		case OpcodeDispatch:
			if !helloReceived {
				report(connectionResult{err: errors.New("dispatch before HELLO")})
				return
			}
			event, ok := message.D.(EventData)
			if !ok {
				continue
			}
			if e, ok := event.(EventReady); ok {
				if ready || resuming {
					report(connectionResult{err: errors.New("unexpected READY"), fatal: true})
					return
				}
				if g.config.ReadyValidator != nil {
					if err := g.config.ReadyValidator(e); err != nil {
						report(connectionResult{err: err, fatal: true})
						return
					}
				}
				g.mu.Lock()
				if g.stopping || !g.active || g.generation != generation {
					g.mu.Unlock()
					return
				}
				g.sessionValidated = true
				g.config.SessionID = copyPtr(&e.SessionID)
				g.config.ResumeURL = copyPtr(&e.ResumeGatewayURL)
				g.mu.Unlock()
				ready = true
			} else if _, ok := event.(EventResumed); ok {
				g.mu.Lock()
				validSession := g.sessionValidated && g.config.SessionID != nil
				g.mu.Unlock()
				if ready || !resuming || !validSession {
					report(connectionResult{err: errors.New("unexpected RESUMED"), fatal: true})
					return
				}
				ready = true
			}
			g.mu.Lock()
			if g.stopping || !g.active || g.generation != generation {
				g.mu.Unlock()
				return
			}
			g.config.LastSequenceReceived = copyPtr(&message.S)
			g.mu.Unlock()

			if resuming && !ready {
				if delivery := g.capture(generation, message.T, message.S, event); delivery != nil {
					if len(replay) == ReplayCapacity {
						report(connectionResult{err: ErrReplayCapacityExceeded, fatal: true})
						return
					}
					replay = append(replay, delivery)
				}
				continue
			}
			if ready && (message.T == EventTypeReady || message.T == EventTypeResumed) {
				if !g.transition(StatusReady, generation) {
					return
				}
			}
			if ready && message.T == EventTypeResumed {
				for _, delivery := range replay {
					if g.ctx.Err() != nil {
						return
					}
					delivery()
				}
				replay = nil
				replayDelivered = true
			}
			if ready && ctx.Err() == nil {
				if g.config.EnableRawEvents {
					g.dispatch(generation, EventTypeRaw, message.S, EventRaw{EventType: message.T, Payload: bytes.NewReader(message.RawD)})
				}
				if _, unknown := event.(EventUnknown); !unknown {
					g.dispatch(generation, message.T, message.S, event)
				}
			}
		case OpcodeHeartbeat:
			if err := g.sendHeartbeat(ctx, conn, generation); err != nil {
				report(connectionResult{err: err})
				return
			}
		case OpcodeHeartbeatACK:
			g.mu.Lock()
			previous := g.lastHeartbeatReceived
			now := time.Now()
			g.lastHeartbeatReceived = now
			g.mu.Unlock()
			g.dispatch(generation, EventTypeHeartbeatAck, message.S, EventHeartbeatAck{LastHeartbeat: previous, NewHeartbeat: now})
		case OpcodeReconnect:
			report(connectionResult{err: errors.New("server requested reconnect")})
			return
		case OpcodeInvalidSession:
			if !bool(message.D.(MessageDataInvalidSession)) {
				g.mu.Lock()
				g.clearSessionLocked()
				g.mu.Unlock()
			}
			report(connectionResult{err: errors.New("invalid session"), delay: time.Second + time.Duration(rand.Int64N(int64(4*time.Second)))})
			return
		}
	}
}

// capture runs at socket receive time, before replay delivery can be delayed.
func (g *gatewayImpl) capture(generation uint64, eventType EventType, sequence int, data EventData) func() {
	g.mu.Lock()
	valid := !g.stopping && g.active && g.generation == generation
	g.mu.Unlock()
	if !valid {
		return nil
	}
	if g.config.EventCaptureHandler != nil {
		return g.config.EventCaptureHandler(eventType, data, generation)
	}
	return func() {
		if g.config.EventGenerationHandler != nil {
			g.config.EventGenerationHandler(eventType, data, generation)
		}
		if g.eventHandlerFunc != nil {
			g.eventHandlerFunc(g, eventType, sequence, data)
		}
	}
}
func (g *gatewayImpl) dispatch(generation uint64, eventType EventType, sequence int, data EventData) {
	if delivery := g.capture(generation, eventType, sequence, data); delivery != nil {
		delivery()
	}
}
