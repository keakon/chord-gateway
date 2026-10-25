package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

// wecomCallbackWarnInterval bounds how often a rejected text callback repeats
// its warning. Every rejection is counted; only the log line is rate-limited.
const wecomCallbackWarnInterval = time.Minute

// WecomAdapter uses the smart-robot long connection for both ingress and
// proactive output. Durable admission and business execution stay in Gateway.
type WecomAdapter struct {
	imCfg                           config.IMAdapterConfig
	router                          *NotificationRouter
	ws                              *wecomWS
	textSender                      *im.IMTextSender
	ctx                             context.Context
	cancel                          context.CancelFunc
	mu                              sync.Mutex
	running, stopping, ready        bool
	done                            chan struct{}
	rateMu                          sync.Mutex
	accountNext                     time.Time
	targetNext                      map[string]time.Time
	accountInterval, targetInterval time.Duration
	callbackWarnAt                  time.Time

	im.ReadinessNotifier
}

func NewWecomAdapter(cfg *config.Config, imCfg config.IMAdapterConfig, _ *config.Paths, router *NotificationRouter) (*WecomAdapter, error) {
	if imCfg.Wecom == nil || imCfg.Wecom.BotID == "" || imCfg.Wecom.Secret == "" {
		return nil, errors.New("wecom: bot_id and secret are required")
	}
	proxyFunc, err := im.WebsocketProxyFuncFor(cfg, imCfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &WecomAdapter{imCfg: imCfg, router: router, ctx: ctx, cancel: cancel, done: make(chan struct{}), targetNext: make(map[string]time.Time), accountInterval: 500 * time.Millisecond, targetInterval: 4 * time.Second}
	a.ws = newWecomWSWithProxy(imCfg.Wecom.BotID, imCfg.Wecom.Secret, wecomWSCallbacks{OnAuthenticated: a.authenticated, OnReady: a.transportReady, CaptureFrame: a.captureFrame, OnFrame: a.receiveFrame, OnUnavailable: a.unavailable, OnDroppedBeforeAccept: a.droppedBeforeAccept, OnCallbackRejected: a.callbackRejected}, proxyFunc)
	sender, err := im.NewIMTextSender(im.IMTextSendOptions{Platform: "wecom", Budget: 1024, Render: renderWecomText, Measure: func(s string) int { return len(s) }, SendSegment: a.sendSegment})
	if err != nil {
		cancel()
		return nil, err
	}
	a.textSender = sender
	return a, nil
}

func (*WecomAdapter) Type() string                { return "wecom" }
func (*WecomAdapter) SupportsLoginRenewal() bool  { return false }
func (*WecomAdapter) StartLogin() (string, error) { return "", im.ErrLoginNotSupported }

func (a *WecomAdapter) Connect() error {
	a.mu.Lock()
	if a.stopping {
		a.mu.Unlock()
		// Disconnect is permanent for this adapter. Reporting success here
		// would leave a supervisor believing a dead adapter is healthy.
		return errWecomStopped
	}
	if a.running {
		a.mu.Unlock()
		return errors.New("wecom: connection already running")
	}
	a.running = true
	a.mu.Unlock()
	defer func() {
		a.unavailable(nil)
		a.cancel()
		a.mu.Lock()
		a.stopping = true
		close(a.done)
		a.mu.Unlock()
	}()
	if a.router == nil || a.router.admission == nil || a.router.ingress == nil || !a.router.ingress.Available() {
		return errors.New("wecom: common ingress and admission are required")
	}
	// Lock the configured account before even dialing: another instance must
	// never authenticate first and kick out the current local owner.
	lock, err := im.AcquireIMAccountLock("wecom", a.imCfg.Wecom.BotID)
	if err != nil {
		return fmt.Errorf("wecom: account lock: %w", err)
	}
	defer lock.Close()
	return a.ws.Run(a.ctx)
}

func (a *WecomAdapter) Disconnect() {
	a.mu.Lock()
	a.stopping = true
	running := a.running
	a.mu.Unlock()
	a.cancel()
	a.ws.Stop()
	if running {
		<-a.done
	}
}

func (a *WecomAdapter) authenticated(ctx context.Context) error {
	account := a.imCfg.Wecom.BotID
	return a.router.PrepareIngressAccount(ctx, "wecom", account)
}

func (a *WecomAdapter) transportReady(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	account := a.imCfg.Wecom.BotID
	if err := a.router.admission.pairing.PlatformReady("wecom", account); err != nil {
		return err
	}
	a.mu.Lock()
	if a.stopping {
		a.mu.Unlock()
		a.router.admission.pairing.PlatformUnavailable("wecom")
		return context.Canceled
	}
	a.ready = true
	a.mu.Unlock()
	a.WakeIngressReadiness()
	return nil
}

func (a *WecomAdapter) unavailable(err error) {
	a.mu.Lock()
	a.ready = false
	stopping := a.stopping
	a.mu.Unlock()
	// A reconnect attempt must not be silent: without this, an ingress
	// preparation failure leaves the bot offline with an empty log. Normal
	// shutdown is not a failure.
	if err != nil && !stopping {
		var failure *WecomTransportError
		if errors.As(err, &failure) {
			log.Warnf("wecom: connection unavailable category=%v", failure.Category)
		} else {
			log.Warnf("wecom: connection unavailable")
		}
	}
	if a.router != nil && a.router.admission != nil {
		a.router.admission.pairing.PlatformUnavailable("wecom")
	}
	a.WakeIngressReadiness()
}

// callbackRejected counts a malformed user text callback that was dropped
// before intake and keeps a bounded log trace. Unsupported events and media
// take the unsupported path and never reach this method.
func (a *WecomAdapter) callbackRejected(reason string) {
	if a.router != nil {
		a.router.ObserveIngress("wecom", "callback_dropped", reason)
	}
	a.mu.Lock()
	warn := time.Since(a.callbackWarnAt) >= wecomCallbackWarnInterval
	if warn {
		a.callbackWarnAt = time.Now()
	}
	a.mu.Unlock()
	if warn {
		log.Warnf("wecom: rejected an invalid text callback reason=%v", reason)
	}
}

func (a *WecomAdapter) IngressDeliveryReadiness(id im.ChatIdentity) im.IngressDeliveryReadiness {
	a.mu.Lock()
	ready := a.ready && !a.stopping
	a.mu.Unlock()
	if id.Platform != "wecom" || id.AccountID != a.imCfg.Wecom.BotID {
		return im.IngressDeliveryReadiness{Reason: im.IngressWaitingAccount}
	}
	return im.IngressDeliveryReadiness{Ready: ready, Reason: im.IngressWaitingPlatform}
}
func (a *WecomAdapter) captureFrame(ctx context.Context, raw []byte) context.Context {
	receivedAt := time.Now()
	if first, ok := ctx.Value(wecomReceivedAtContextKey{}).(time.Time); ok {
		receivedAt = first
	}
	e, err := NormalizeWecomCallback(raw, a.imCfg.Wecom.BotID, receivedAt)
	if err != nil || a.router == nil {
		return ctx
	}
	kind, target, ok := a.router.tryCaptureIngress(e.Message)
	e.Capture = &im.IngressCapture{Kind: kind, Target: target, Unavailable: !ok}
	return context.WithValue(ctx, wecomCapturedEnvelopeContextKey{}, e)
}

func (a *WecomAdapter) receiveFrame(ctx context.Context, raw []byte) error {
	if a.router == nil {
		return nil
	}
	e, captured := ctx.Value(wecomCapturedEnvelopeContextKey{}).(im.IngressEnvelope)
	var err error
	if !captured {
		receivedAt := time.Now()
		if first, ok := ctx.Value(wecomReceivedAtContextKey{}).(time.Time); ok {
			receivedAt = first
		}
		e, err = NormalizeWecomCallback(raw, a.imCfg.Wecom.BotID, receivedAt)
	}
	if errors.Is(err, errWecomAccountMismatch) {
		a.unavailable(err)
		return err
	}
	if err != nil {
		return nil
	} // Unknown events and malformed messages grant no authority.
	// Diagnostic authorization runs on the bounded worker that observes the
	// result, never on the socket reader: the reply-only ticket is minted once
	// the attempt has an outcome. It cannot change the reader's frozen control
	// target, because the pipeline authorizes the submitted envelope
	// independently. The envelope is already normalized, so submit without a
	// normalize hook and resolve the reply identity inside the observer.
	_, err = a.router.SubmitIngressObserved(ctx, e, func(result im.ReceiveResult) {
		a.observeReceive(a.admitDiagnosticReply(e.Message), result)
	})
	if err != nil {
		a.observeReceive(e.Message, im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: im.SubmitRefusalReason(err)})
	}
	return nil
}

func (a *WecomAdapter) observeReceive(msg im.IncomingMessage, result im.ReceiveResult) {
	if a.router == nil {
		return
	}
	// The ticket here is diagnostic-only, minted on a bounded worker for this
	// attempt's reply. It never grants control authority: the pipeline
	// authorizes the frozen capture independently. Replies that need pipeline
	// authority are sent by the pipeline itself from its own ticket.
	switch result.Disposition {
	case im.ReceiveRetryable:
		// Retryable describes this attempt, not the business ID's history.
		// A previous delivery may already have committed or still be in flight.
		if a.router.ingressStore != nil {
			a.router.ingressStore.metrics.record("wecom", "acceptance_unconfirmed", result.Reason)
		}
		if msg.AdmissionTicket() != nil {
			a.router.enqueueIngressReply(msg, "Gateway could not confirm receipt of this message. Check the original Chord session before sending it again.")
		}
	case im.ReceiveRejected:
		if result.Reason == "control_target_unavailable" && msg.AdmissionTicket() != nil {
			a.router.enqueueIngressReply(msg, "Request was not applied: control_target_unavailable. Check /status before trying again.")
		}
	}
}

// admitDiagnosticReply mints the reply-only ticket used by this attempt's
// diagnostics. Admission reads pairing and binding state under its own locks,
// so callers must run it on a bounded worker rather than a socket reader.
func (a *WecomAdapter) admitDiagnosticReply(msg im.IncomingMessage) im.IncomingMessage {
	if a.router.admission == nil {
		return msg
	}
	imCfg := a.router.getConfig().IMConfigByType("wecom")
	if imCfg == nil {
		return msg
	}
	owner := imCfg.OwnerIdentity()
	if owner == nil || owner.AccountID != msg.AccountID || owner.UserID != msg.SenderID || msg.ChatKind != "private" {
		return msg
	}
	msg, _ = admittedMessage(a.router, msg)
	return msg
}

func (a *WecomAdapter) droppedBeforeAccept(count int) {
	if a.router == nil || a.router.ingressStore == nil {
		return
	}
	// One record per dropped delivery, not per batch: the counter answers how
	// many messages never reached the ingress, and a batch of N is N of them.
	// The Discord adapter records the same event the same way.
	for range count {
		a.router.ingressStore.metrics.record("wecom", "callback_dropped", "")
	}
}
