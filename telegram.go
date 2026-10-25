package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

// TelegramAdapter owns exactly one long poller and one account-wide sender.
// Its lifecycle context also cancels in-flight sends and throttle waits.
type TelegramAdapter struct {
	api       telegramAPI
	sender    *im.IMTextSender
	router    im.IMPollingHost
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	sends     sync.WaitGroup
	running   bool
	accountID string
	username  string
	ready     bool
	now       telegramClock
	receive   func(context.Context, im.IngressEnvelope) im.ReceiveResult
	reject    func(context.Context, im.IngressEnvelope, string) im.ReceiveResult
	store     telegramCursorStore
	retry     telegramBackoff // Owned by the single startup/poll worker.
	retryWarn time.Time
	captured  map[int64]telegramCapturedUpdate

	im.ReadinessNotifier
}

// errTelegramAlreadyRunning is returned when Connect is called while another
// poller is still running on the same adapter.
var errTelegramAlreadyRunning = errors.New("telegram poller already running")

func NewTelegramAdapter(cfg *config.Config, imCfg config.IMAdapterConfig, _ *config.Paths, router *NotificationRouter) (*TelegramAdapter, error) {
	if imCfg.Telegram == nil || imCfg.Telegram.BotToken == "" {
		return nil, errors.New("telegram bot_token is required")
	}
	proxyFunc, err := im.ProxyFuncFor(cfg, imCfg)
	if err != nil {
		return nil, err
	}
	api := newTelegramAPI(imCfg.Telegram.BotToken, proxyFunc)
	sender, err := newTelegramTextSender(api)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &TelegramAdapter{api: api, sender: sender, router: router, ctx: ctx, cancel: cancel, now: realTelegramClock}
	if router != nil {
		a.receive = router.ReceiveIngress
		a.reject = router.RejectIngressEnvelope
		a.store = router.ingressStore
	}
	return a, nil
}

func (a *TelegramAdapter) Type() string                { return "telegram" }
func (a *TelegramAdapter) SupportsLoginRenewal() bool  { return false }
func (a *TelegramAdapter) StartLogin() (string, error) { return "", im.ErrLoginNotSupported }
func (a *TelegramAdapter) Disconnect() {
	if a.cancel != nil {
		a.cancel()
	}
}
func (a *TelegramAdapter) SendText(chat, text string) error {
	return a.SendTextContext(context.Background(), chat, text)
}
func (a *TelegramAdapter) SendTextContext(ctx context.Context, chat, text string) error {
	a.mu.Lock()
	ready, account := a.ready, a.accountID
	if ready {
		a.sends.Add(1)
	}
	a.mu.Unlock()
	if !ready {
		return errors.New("telegram account unavailable")
	}
	defer a.sends.Done()
	if expected, ok := ctx.Value(im.ReplyAccountContextKey{}).(string); ok && account != expected {
		return errors.New("telegram account changed")
	}
	return a.sender.Send(ctx, a.ctx, nil, chat, text, false)
}
func (a *TelegramAdapter) TextSendMetrics() im.IMTextMetricsSnapshot {
	return a.sender.TextSendMetrics()
}
func (a *TelegramAdapter) TextSendTargetSnapshot(target string) im.IMTextTargetSnapshot {
	return a.sender.TextSendTargetSnapshot(target)
}
func (a *TelegramAdapter) IngressDeliveryReadiness(id im.ChatIdentity) im.IngressDeliveryReadiness {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.ready {
		return im.IngressDeliveryReadiness{Reason: im.IngressWaitingPlatform}
	}
	if id.AccountID != a.accountID {
		return im.IngressDeliveryReadiness{Reason: im.IngressWaitingAccount}
	}
	return im.IngressDeliveryReadiness{Ready: true}
}

func (a *TelegramAdapter) Connect() (result error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return errTelegramAlreadyRunning
	}
	a.running = true
	a.mu.Unlock()
	var accountLock *im.InstanceLocks
	defer func() {
		// Only an explicit shutdown is success; preserve unrelated failures.
		if errors.Is(result, context.Canceled) && a.ctx.Err() != nil {
			result = nil
		}

		a.captured = nil
		a.mu.Lock()
		a.ready = false
		a.mu.Unlock()
		if a.router != nil {
			a.router.PlatformUnavailable("telegram")
		}
		a.WakeIngressReadiness()
		a.cancel()
		a.sends.Wait()
		if accountLock != nil {
			accountLock.Close()
		}
		a.mu.Lock()
		a.running = false
		a.mu.Unlock()
	}()
	if a.router == nil || !a.router.IngressAvailable() || a.store == nil {
		return errors.New("telegram ingress unavailable")
	}
	var me telegramUser
	err := a.startupRequest(func() error { var err error; me, err = a.api.GetMe(a.ctx); return err })
	if err != nil {
		return err
	}
	if me.ID <= 0 || !me.IsBot || me.Username == "" {
		return errors.New("telegram invalid bot identity")
	}
	account := strconv.FormatInt(me.ID, 10)
	accountLock, err = im.AcquireIMAccountLock("telegram", account)
	if err != nil {
		return err
	}
	var webhook telegramWebhookInfo
	err = a.startupRequest(func() error { var err error; webhook, err = a.api.GetWebhookInfo(a.ctx); return err })
	if err != nil {
		return err
	}
	if webhook.URL != "" {
		return errors.New("telegram webhook_conflict")
	}
	a.mu.Lock()
	a.accountID = account
	a.username = me.Username
	a.mu.Unlock()
	if err := a.router.PrepareIngressAccount(a.ctx, "telegram", account); err != nil {
		return fmt.Errorf("telegram ingress recovery: %w", err)
	}
	cursor, err := a.loadCursor(a.ctx)
	if err != nil {
		return err
	}
	// The successful probe can contain input. Keep it until readiness and route
	// recovery are established, then admit the exact response below.
	batch, err := a.pollRequest(a.ctx, &cursor, true)
	if err != nil {
		return err
	}
	if err := a.router.PlatformReady("telegram", account, im.PlatformPresentation{Platform: "telegram", BotUsername: me.Username}); err != nil {
		return err
	}
	a.mu.Lock()
	a.ready = true
	a.mu.Unlock()
	a.WakeIngressReadiness()
	return a.poll(a.ctx, cursor, batch, true)
}

func (a *TelegramAdapter) startupRequest(request func() error) error {
	for {
		if a.ctx.Err() != nil {
			return a.ctx.Err()
		}
		err := request()
		if err == nil {
			a.retry.succeeded(time.Now())
			a.retryWarn = time.Time{}
			return nil
		}
		delay, retry := a.retry.failed(err)
		if !retry {
			return err
		}
		a.warnRetry(err, delay)
		if !telegramWait(a.ctx, delay) {
			return a.ctx.Err()
		}
	}
}
