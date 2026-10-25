package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/keakon/chord-gateway/internal/im"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/config"
)

// DingtalkAdapter owns one internal application's Stream connection. Business
// input and protocol probes share the Gateway's in-memory, bounded ingress workers.
type DingtalkAdapter struct {
	cfg                                           *config.DingtalkConfig
	router                                        *NotificationRouter
	account                                       string
	ctx                                           context.Context
	cancel                                        context.CancelFunc
	api                                           *dingtalkAPI
	stream                                        *dingtalkStream
	sender                                        *im.IMTextSender
	mu                                            sync.Mutex
	started, stopping, connected, verified, ready bool
	// activated survives temporary Stream outages, but is never restored from
	// identity evidence alone: this process must first complete activation.
	activated  bool
	generation uint64
	done       chan struct{}
	acceptWG   sync.WaitGroup
	sendWG     sync.WaitGroup
	fatal      error

	im.ReadinessNotifier
}

func NewDingtalkAdapter(globalCfg *config.Config, imCfg config.IMAdapterConfig, _ *config.Paths, router *NotificationRouter) (*DingtalkAdapter, error) {
	if imCfg.Dingtalk == nil || imCfg.Dingtalk.ClientSecret == "" {
		return nil, errors.New("dingtalk: incomplete configuration")
	}
	account, err := dingtalkAccountID(imCfg.Dingtalk)
	if err != nil {
		return nil, err
	}
	cfg := *imCfg.Dingtalk
	proxyFunc, err := im.WebsocketProxyFuncFor(globalCfg, imCfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &DingtalkAdapter{cfg: &cfg, router: router, account: account, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	a.api = newDingtalkAPI(ctx, &cfg, proxyFunc)
	a.sender, err = im.NewIMTextSender(im.IMTextSendOptions{Platform: "dingtalk", Budget: 1024, Measure: func(s string) int { return len(s) }, SendSegment: func(ctx context.Context, target, text string) error {
		// Refuse here rather than before the shared sender so the definite
		// non-delivery lands in the shared category vocabulary; a bare error
		// returned before textSender.Send is recorded as delivery_failed and
		// never observed (the WeCom contract).
		if !a.IngressDeliveryReadiness(im.ChatIdentity{Platform: "dingtalk", AccountID: a.account, ChatID: target}).Ready {
			return &dingtalkAPIError{Category: "connection_unavailable"}
		}
		kind, raw, err := dingtalkDecodeTarget(cfg.CorpID, target)
		if err != nil {
			return err
		}
		return a.api.SendText(ctx, kind, raw, text)
	}})
	if err != nil {
		cancel()
		return nil, err
	}
	a.api.observeRateWait = a.sender.ObserveRateWait
	a.stream = newDingtalkStreamWithProxy(a.api, dingtalkStreamCallbacks{OnConnected: a.onConnected, OnUnavailable: a.onUnavailable, OnCallback: a.onCallback, OnReceiveResult: func(result im.ReceiveResult) {
		if result.Disposition == im.ReceiveRetryable && result.Reason == "stream_capacity" {
			a.observeReceiveResult(result)
		}
		if result.Disposition == im.ReceiveUncertain {
			log.Warnf("dingtalk: acceptance outcome uncertain; inspect in-memory ingress before retrying")
		}
	}, OnDiagnostic: func(category string) {
		if category == "connected" {
			log.Infof("dingtalk stream: connected")
		} else {
			log.Warnf("dingtalk stream: %s", category)
		}
	}, OnRetry: func(category string, attempt int, delay time.Duration) {
		log.Warnf("dingtalk stream: retry category=%s attempt=%d delay=%v", category, attempt, delay)
	}}, proxyFunc)
	return a, nil
}

func (a *DingtalkAdapter) Type() string                { return "dingtalk" }
func (a *DingtalkAdapter) SupportsLoginRenewal() bool  { return false }
func (a *DingtalkAdapter) StartLogin() (string, error) { return "", im.ErrLoginNotSupported }

func (a *DingtalkAdapter) Connect() error {
	a.mu.Lock()
	if a.started || a.stopping {
		a.mu.Unlock()
		return errors.New("dingtalk: adapter already started or stopped")
	}
	a.started = true
	a.mu.Unlock()
	var locks *im.InstanceLocks
	defer func() {
		a.mu.Lock()
		a.stopping = true
		a.cancel()
		a.mu.Unlock()
		a.onUnavailable()
		a.acceptWG.Wait()
		a.sendWG.Wait()
		a.api.Close()
		if locks != nil {
			locks.Close()
		}
		close(a.done)
	}()
	if a.router == nil || a.router.admission == nil || a.router.ingress == nil || !a.router.ingress.Available() || a.router.ingressStore == nil {
		return errors.New("dingtalk: in-memory ingress is unavailable")
	}
	locks, err := acquireDingtalkAccountLocks(a.cfg)
	if err != nil {
		return fmt.Errorf("dingtalk: acquire application locks: %w", err)
	}
	err = a.stream.Run(a.ctx)
	if err != nil {
		return err
	}
	a.mu.Lock()
	err = a.fatal
	a.mu.Unlock()
	return err
}

func (a *DingtalkAdapter) Disconnect() {
	a.mu.Lock()
	a.stopping = true
	started := a.started
	a.cancel()
	a.mu.Unlock()
	a.onUnavailable()
	if started {
		<-a.done
	} else {
		a.acceptWG.Wait()
		a.sendWG.Wait()
		if a.api != nil {
			a.api.Close()
		}
	}
}

func (a *DingtalkAdapter) onConnected(ctx context.Context) error {
	a.mu.Lock()
	if a.stopping || ctx.Err() != nil {
		a.mu.Unlock()
		return context.Canceled
	}
	a.connected = true
	a.generation++
	gen, verified := a.generation, a.verified
	a.mu.Unlock()
	if verified {
		return a.activate(ctx, gen)
	}
	p := a.router.admission.pairing
	p.mu.Lock()
	if p.terminal != nil {
		_, err := fmt.Fprintln(p.terminal, "dingtalk: Connected; waiting for same-enterprise private-chat verification. Send hello to the robot.")
		if err != nil {
			p.mu.Unlock()
			return errors.New("dingtalk: could not display identity verification instructions")
		}
	}
	p.mu.Unlock()
	log.Infof("dingtalk: connected, waiting for same-enterprise private-chat verification")
	return nil
}

func (a *DingtalkAdapter) activate(ctx context.Context, generation uint64) error {
	if err := a.router.PrepareIngressAccount(ctx, "dingtalk", a.account); err != nil {
		return err
	}
	a.mu.Lock()
	if a.stopping || !a.connected || a.generation != generation || ctx.Err() != nil {
		a.mu.Unlock()
		return context.Canceled
	}
	if err := a.router.admission.pairing.PlatformReadyWithPresentation("dingtalk", a.account, im.PlatformPresentation{Platform: "dingtalk", BotMention: "@robot"}); err != nil {
		if errors.Is(err, im.ErrPairingDenied) {
			a.fatal = &dingtalkIdentityError{Category: dingtalkCategoryTenantMismatch}
			a.cancel()
			err = a.fatal
		}
		a.mu.Unlock()
		return err
	}
	a.ready = true
	a.activated = true
	a.mu.Unlock()
	a.WakeIngressReadiness()
	return nil
}

func (a *DingtalkAdapter) onUnavailable() {
	a.mu.Lock()
	a.connected, a.ready = false, false
	a.generation++
	if a.router != nil && a.router.admission != nil {
		a.router.admission.pairing.PlatformUnavailable("dingtalk")
	}
	a.mu.Unlock()
	a.WakeIngressReadiness()
}

func dingtalkResult(result im.ReceiveResult) <-chan im.ReceiveResult {
	ch := make(chan im.ReceiveResult, 1)
	ch <- result
	close(ch)
	return ch
}

func (a *DingtalkAdapter) onCallback(ctx context.Context, frame dingtalkFrame) (<-chan im.ReceiveResult, error) {
	received := time.Now()
	callback, err := parseDingtalkCallback(frame.Data)
	if err != nil {
		return dingtalkResult(im.ReceiveResult{Disposition: im.ReceiveRejected, Reason: "invalid_input"}), nil
	}
	// Enterprise/robot mismatch is terminal; unsupported sender identity is a
	// permanent refusal and never falls back to the platform's opaque senderId.
	identityErr := validateDingtalkIdentityProbe(a.cfg, &callback)
	var identity *dingtalkIdentityError
	if errors.As(identityErr, &identity) && identity.Terminal() {
		return nil, identityErr
	}
	msg, contentErr := normalizeDingtalkCallback(a.cfg, frame.Data)
	if contentErr != nil && msg.MessageID == "" {
		reason := "invalid_input"
		var refusal *dingtalkIdentityError
		if errors.As(contentErr, &refusal) {
			reason = refusal.Category
		}
		log.Warnf("dingtalk: callback refused category=%s", reason)
		return dingtalkResult(im.ReceiveResult{Disposition: im.ReceiveRejected, Reason: reason}), nil
	}
	e := im.IngressEnvelope{Message: msg, DeliveryID: frame.headerString("messageId"), ReceivedAt: received, SourceTime: received}
	if _, exists := frame.Headers["time"]; exists {
		timestamp := frame.headerString("time")
		ms, err := strconv.ParseInt(timestamp, 10, 64)
		if err != nil {
			contentErr = &dingtalkIdentityError{Category: dingtalkCategorySourceTime}
		} else {
			e.SourceTime = time.UnixMilli(ms)
			if e.SourceTime.Before(received.Add(-24*time.Hour)) || e.SourceTime.After(received.Add(5*time.Minute)) {
				contentErr = &dingtalkIdentityError{Category: dingtalkCategorySourceTime}
				// Invalid milliseconds can exceed time.Time's JSON year range.
				// Keep a serializable receipt time in the refusal tombstone.
				e.SourceTime = received
			}
		}
	}
	a.mu.Lock()
	ready, gen := a.ready, a.generation
	available := a.connected && !a.stopping
	a.mu.Unlock()
	if !available {
		return nil, im.ErrIngressClosed
	}
	if ready && contentErr == nil {
		// Capture an existing owner's reply authority now, so a failed accept
		// cannot acquire permission from a later pairing or binding change.
		// Assign into the submitted envelope copy: e.Message was cloned from
		// msg at construction, so mutating msg alone would drop the ticket.
		imCfg := a.router.getConfig().IMConfigByType("dingtalk")
		if imCfg != nil && imCfg.OwnerIdentity() != nil {
			if ticket, allowed := a.router.admission.Authorize(msg); allowed {
				msg.SetAdmissionTicket(&ticket)
				e.Message.SetAdmissionTicket(msg.AdmissionTicket())
			}
		}
		future, err := a.router.SubmitIngress(ctx, e)
		if err != nil {
			a.observeReceiveResult(im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: im.SubmitRefusalReason(err)})
			a.notifyLostInput(msg)
		}
		return a.trackAcceptance(future, msg), err
	}
	// Use the same account/chat key and global budget as normal input. A probe
	// cannot read a nonce or reach the command dispatcher, even if it says /bind.
	s := a.router.ingress
	s.mu.Lock()
	n := s.normalizer
	active := s.started && !s.stopping
	s.mu.Unlock()
	if !active || n == nil {
		return nil, im.ErrIngressClosed
	}
	key := string(ingressKey("", "dingtalk", a.account, msg.ChatID))
	future, submitErr := n.submitProtocol(ctx, key, func(ctx context.Context) im.ReceiveResult {
		ctx, cancel := context.WithTimeout(ctx, im.IngressAcceptBudget)
		defer cancel()
		reason := "identity_unverified"
		probe := !ready && identityErr == nil && contentErr == nil
		if contentErr != nil {
			reason = "invalid_input"
			var refusal *dingtalkIdentityError
			if errors.As(contentErr, &refusal) {
				reason = refusal.Category
			}
		}
		if probe {
			reason = "identity_probe"
		}
		record := IngressRecord{Platform: "dingtalk", AccountID: a.account, ChatID: msg.ChatID, SenderID: msg.SenderID, BusinessID: msg.MessageID, DeliveryID: e.DeliveryID, SourceTime: e.SourceTime, ReceivedAt: received, Kind: "control", State: im.IngressRejected, Reason: reason}
		result := a.router.ingressStore.AcceptRecord(ctx, record, nil)
		if !probe || result.RecordID == "" || result.Reason != "identity_probe" || (result.Disposition != im.ReceiveRejected && result.Disposition != im.ReceiveDuplicate) {
			return result
		}
		a.mu.Lock()
		if a.stopping || !a.connected || a.generation != gen || ctx.Err() != nil {
			a.mu.Unlock()
			return im.ReceiveResult{Disposition: im.ReceiveUncertain, Reason: "platform_unavailable"}
		}
		a.verified = true
		a.mu.Unlock()
		if err := a.activate(ctx, gen); err != nil {
			return im.ReceiveResult{Disposition: im.ReceiveUncertain, Reason: "platform_unavailable"}
		}
		return result
	})
	if submitErr != nil {
		a.observeReceiveResult(im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: im.SubmitRefusalReason(submitErr)})
	}
	return a.trackAcceptance(future, im.IncomingMessage{}), submitErr
}

// The relay waits for the worker's actual result even when a disconnected socket
// stops waiting for its ACK. Disconnect joins these bounded workers before return.
// Add runs under the same lock that seals shutdown, so Wait cannot run between
// the availability check and the reservation (a bare Add after Wait is a panic
// even when it is benign-looking; -race does not detect it).
func (a *DingtalkAdapter) trackAcceptance(future <-chan im.ReceiveResult, msg im.IncomingMessage) <-chan im.ReceiveResult {
	if future == nil {
		return nil
	}
	a.mu.Lock()
	if a.stopping {
		a.mu.Unlock()
		// Disconnect runs the shutdown lock before cancelling the connection,
		// so the stream's 500 for this frame can no longer reach the platform
		// and nothing may outlive Disconnect. The submitted envelope's outcome
		// is now unobserved; robot callbacks are fire-and-forget, so the loss
		// has to be counted here instead of inferred from redelivery.
		if a.router != nil {
			a.router.ObserveIngress("dingtalk", "acceptance_unconfirmed", "transport_stopped")
		}
		return nil
	}
	a.acceptWG.Add(1)
	a.mu.Unlock()
	result := make(chan im.ReceiveResult, 1)
	go func() {
		defer a.acceptWG.Done()
		value, ok := <-future
		value = ingressNormalizationResult(value, ok)
		a.observeReceiveResult(value)
		if value.Disposition == im.ReceiveRetryable {
			a.notifyLostInput(msg)
		}
		result <- value
		close(result)
	}()
	return result
}

func (a *DingtalkAdapter) notifyLostInput(msg im.IncomingMessage) {
	if msg.AdmissionTicket() != nil {
		// Robot callbacks are fire-and-forget: the platform does not redeliver
		// unacknowledged or rejected frames, so the reply must tell the user to
		// resend instead of waiting for a retry that never comes.
		a.router.enqueueIngressReply(msg, "This input was not accepted, and DingTalk does not redeliver robot callbacks, so please send it again. If the bot keeps dropping input, check Gateway ingress health first.")
	}
}

func (a *DingtalkAdapter) observeReceiveResult(result im.ReceiveResult) {
	if a.router == nil || a.router.ingressStore == nil {
		return
	}
	if result.Disposition == im.ReceiveRetryable {
		reason := result.Reason
		if reason == "stream_capacity" {
			reason = "capacity"
		}
		a.router.ingressStore.ObserveLostBeforeAccept("dingtalk", reason)
	}
	if result.Disposition == im.ReceiveUncertain {
		log.Warnf("dingtalk: acceptance outcome uncertain; inspect in-memory ingress before retrying")
	}
}

func (a *DingtalkAdapter) SendText(chat, text string) error {
	return a.SendTextContext(context.Background(), chat, text)
}
func (a *DingtalkAdapter) SendTextContext(ctx context.Context, chat, text string) error {
	if ctx == nil {
		return errors.New("dingtalk: send requires context")
	}
	a.mu.Lock()
	ready := a.activated && !a.stopping && a.ctx.Err() == nil
	if ready {
		a.sendWG.Add(1)
	}
	a.mu.Unlock()
	if ready {
		defer a.sendWG.Done()
	}
	// A not-ready adapter still enters the shared sender: its SendSegment
	// produces a categorized, definite non-delivery instead of an unobserved
	// bare error. Only ready calls are joined by sendWG, because not-ready
	// segment refusal never touches the API.
	if a.router == nil {
		err := &dingtalkAPIError{Category: "connection_unavailable"}
		a.reportSendFailure(err)
		return err
	}
	imCfg := a.router.getConfig().IMConfigByType("dingtalk")
	expectedAccount, scoped := ctx.Value(im.ReplyAccountContextKey{}).(string)
	if scoped {
		if expectedAccount != a.account {
			return im.ErrPairingDenied
		}
	} else if imCfg == nil || imCfg.OwnerIdentity() == nil || imCfg.OwnerIdentity().AccountID != a.account || imCfg.Bindings()[chat] == "" {
		return im.ErrPairingDenied
	}
	_, control := ctx.Value(im.OutboundReplyContextKey{}).(im.ReplyPermit)
	err := a.sender.Send(ctx, a.ctx, nil, chat, text, control)
	a.reportSendFailure(err)
	return err
}
func (a *DingtalkAdapter) ValidateBindingTarget(ctx context.Context, target im.BindingProbeTarget) error {
	a.mu.Lock()
	ready := a.ready && !a.stopping
	if ready {
		a.sendWG.Add(1)
	}
	a.mu.Unlock()
	if !ready {
		return im.ErrPairingDenied
	}
	defer a.sendWG.Done()
	if target.Platform != "dingtalk" || target.AccountID != a.account {
		return im.ErrPairingDenied
	}
	if _, _, err := dingtalkDecodeTarget(a.cfg.CorpID, target.ChatID); err != nil {
		return err
	}
	err := a.sender.Send(ctx, a.ctx, nil, target.ChatID, "Gateway binding check.", true)
	a.reportSendFailure(err)
	return err
}

func (a *DingtalkAdapter) reportSendFailure(err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	category, accepted := "unknown_send", 0
	var apiError *dingtalkAPIError
	if errors.As(err, &apiError) {
		category = apiError.Category
	}
	var partial *im.IMTextSendError
	if errors.As(err, &partial) {
		accepted = partial.AcceptedSegments
	}
	log.Warnf("dingtalk: proactive send failed category=%s accepted_segments=%d", category, accepted)
}
func (a *DingtalkAdapter) IngressDeliveryReadiness(id im.ChatIdentity) im.IngressDeliveryReadiness {
	a.mu.Lock()
	defer a.mu.Unlock()
	return im.IngressDeliveryReadiness{Ready: a.activated && !a.stopping && a.ctx.Err() == nil && id.Platform == "dingtalk" && id.AccountID == a.account, Reason: im.IngressWaitingAccount}
}
func (a *DingtalkAdapter) TextSendMetrics() im.IMTextMetricsSnapshot {
	return a.sender.TextSendMetrics()
}
func (a *DingtalkAdapter) TextSendTargetSnapshot(target string) im.IMTextTargetSnapshot {
	return a.sender.TextSendTargetSnapshot(target)
}
