package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/keakon/chord-gateway/config"
)

// WecomAdapter uses the smart-robot long connection for both ingress and
// proactive output. Durable admission and business execution stay in Gateway.
type WecomAdapter struct {
	imCfg                           config.IMAdapterConfig
	router                          *NotificationRouter
	ws                              *wecomWS
	textSender                      *IMTextSender
	ctx                             context.Context
	cancel                          context.CancelFunc
	mu                              sync.Mutex
	running, stopping, ready        bool
	done                            chan struct{}
	rateMu                          sync.Mutex
	accountNext                     time.Time
	targetNext                      map[string]time.Time
	accountInterval, targetInterval time.Duration

	ingressReadinessNotifier
}

func NewWecomAdapter(cfg *config.Config, imCfg config.IMAdapterConfig, _ *config.Paths, router *NotificationRouter) (*WecomAdapter, error) {
	if imCfg.Wecom == nil || imCfg.Wecom.BotID == "" || imCfg.Wecom.Secret == "" {
		return nil, errors.New("wecom: bot_id and secret are required")
	}
	proxyFunc, err := websocketProxyFuncFor(cfg, imCfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &WecomAdapter{imCfg: imCfg, router: router, ctx: ctx, cancel: cancel, done: make(chan struct{}), targetNext: make(map[string]time.Time), accountInterval: 500 * time.Millisecond, targetInterval: 4 * time.Second}
	a.ws = newWecomWSWithProxy(imCfg.Wecom.BotID, imCfg.Wecom.Secret, wecomWSCallbacks{OnAuthenticated: a.authenticated, OnReady: a.transportReady, CaptureFrame: a.captureFrame, OnFrame: a.receiveFrame, OnUnavailable: a.unavailable, OnDroppedBeforeAccept: a.droppedBeforeAccept}, proxyFunc)
	sender, err := NewIMTextSender(IMTextSendOptions{Platform: "wecom", Budget: 1024, Render: renderWecomText, Measure: func(s string) int { return len(s) }, SendSegment: a.sendSegment})
	if err != nil {
		cancel()
		return nil, err
	}
	a.textSender = sender
	return a, nil
}

func (*WecomAdapter) Type() string                { return "wecom" }
func (*WecomAdapter) SupportsLoginRenewal() bool  { return false }
func (*WecomAdapter) StartLogin() (string, error) { return "", ErrLoginNotSupported }

func (a *WecomAdapter) Connect() error {
	a.mu.Lock()
	if a.stopping {
		a.mu.Unlock()
		return nil
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
	lock, err := AcquireIMAccountLock("wecom", a.imCfg.Wecom.BotID)
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
	a.wakeIngressReadiness()
	return nil
}

func (a *WecomAdapter) unavailable(_ error) {
	a.mu.Lock()
	a.ready = false
	a.mu.Unlock()
	if a.router != nil && a.router.admission != nil {
		a.router.admission.pairing.PlatformUnavailable("wecom")
	}
	a.wakeIngressReadiness()
}

func (a *WecomAdapter) IngressDeliveryReadiness(id ChatIdentity) IngressDeliveryReadiness {
	a.mu.Lock()
	ready := a.ready && !a.stopping
	a.mu.Unlock()
	if id.Platform != "wecom" || id.AccountID != a.imCfg.Wecom.BotID {
		return IngressDeliveryReadiness{Reason: IngressWaitingAccount}
	}
	return IngressDeliveryReadiness{Ready: ready, Reason: IngressWaitingPlatform}
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
	e.capture = &ingressCapture{kind: kind, target: target, unavailable: !ok}
	return context.WithValue(ctx, wecomCapturedEnvelopeContextKey{}, e)
}

func (a *WecomAdapter) receiveFrame(ctx context.Context, raw []byte) error {
	e, captured := ctx.Value(wecomCapturedEnvelopeContextKey{}).(IngressEnvelope)
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
	// Diagnostic authorization runs on an existing bounded worker, never on
	// the socket reader. It cannot change the reader's frozen control target:
	// the admitted ticket only addresses this attempt's diagnostic reply, while
	// the pipeline authorizes the submitted envelope independently.
	msg := e.Message
	_, err = a.router.submitIngressNormalization(ctx, msg.ChatID, e, func(_ context.Context, envelope IngressEnvelope) (IngressEnvelope, error) {
		if a.router.admission != nil {
			im := a.router.getConfig().IMConfigByType("wecom")
			if im != nil && im.OwnerIdentity() != nil && im.OwnerIdentity().AccountID == msg.AccountID && im.OwnerIdentity().UserID == msg.SenderID && msg.ChatKind == "private" {
				msg, _ = admittedMessage(a.router, msg)
			}
		}
		return envelope, nil
	}, func(result ReceiveResult) { a.observeReceive(msg, result) })
	if err != nil {
		a.observeReceive(e.Message, ReceiveResult{Disposition: ReceiveRetryable, Reason: ingressSubmitRefusalReason(err)})
	}
	return nil
}

func (a *WecomAdapter) observeReceive(msg IncomingMessage, result ReceiveResult) {
	if a.router == nil {
		return
	}
	// The ticket here is diagnostic-only, minted on a bounded worker for this
	// attempt's reply. It never grants control authority: the pipeline
	// authorizes the frozen capture independently. Replies that need pipeline
	// authority are sent by the pipeline itself from its own ticket.
	switch result.Disposition {
	case ReceiveRetryable:
		// Retryable describes this attempt, not the business ID's history.
		// A previous delivery may already have committed or still be in flight.
		if a.router.ingressStore != nil {
			a.router.ingressStore.metrics.record("wecom", "acceptance_unconfirmed", result.Reason)
		}
		if msg.ticket != nil {
			a.router.enqueueIngressReply(msg, "Gateway could not confirm receipt of this message. Check the original Chord session before sending it again.")
		}
	case ReceiveRejected:
		if result.Reason == "control_target_unavailable" && msg.ticket != nil {
			a.router.enqueueIngressReply(msg, "Request was not applied: control_target_unavailable. Check /status before trying again.")
		}
	case ReceiveUncertain:
		// The common store records the authoritative uncertain outcome. Do not
		// label it lost or encourage resending an operation that may have committed.
		if msg.ticket != nil {
			a.router.enqueueIngressReply(msg, "Gateway could not confirm receipt of this message. Check the original Chord session before sending it again.")
		}
	}
}

func (a *WecomAdapter) droppedBeforeAccept(count int) {
	if a.router == nil || a.router.ingressStore == nil {
		return
	}
	for range count {
		a.router.ingressStore.metrics.record("wecom", "callback_dropped", "")
	}
}
