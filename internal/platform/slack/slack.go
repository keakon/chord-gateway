package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/keakon/golog/log"
	slackapi "github.com/slack-go/slack"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

// SlackAdapter owns one Socket Mode supervisor. Web API sends share its root
// lifetime, so a socket refresh does not revoke an authorized long-running task.
type SlackAdapter struct {
	imCfg                                      config.IMAdapterConfig
	router                                     im.IMSocketHost
	client                                     *slackapi.Client
	sender                                     *Sender
	mu                                         sync.Mutex
	ctx                                        context.Context
	cancel                                     context.CancelFunc
	done                                       chan struct{}
	started, stopping, ready, identityVerified bool
	identity                                   Identity
	readySince                                 time.Time
	connectionStable                           bool
	conn                                       *websocket.Conn
	generation                                 uint64
	sends                                      sync.WaitGroup
	proxyFunc                                  func(*http.Request) (*url.URL, error)
	inbound                                    *Inbound
	im.ReadinessNotifier
	http *slackHTTPClient
	// Fixed production endpoints and budgets; hooks permit protocol fault tests.
	openSocket                func(context.Context) (string, error)
	dialSocket                func(context.Context, string) (*websocket.Conn, error)
	verifyIdentity            func(context.Context) (Identity, error)
	helloTimeout, pingTimeout time.Duration
}

func NewSlackAdapter(cfg *config.Config, imCfg config.IMAdapterConfig, _ *config.Paths, router im.IMSocketHost) (*SlackAdapter, error) {
	c := imCfg.Slack
	if c == nil || c.AppToken == "" || c.BotToken == "" || c.AppID == "" || c.TeamID == "" {
		return nil, errors.New("slack: app_token, bot_token, app_id and team_id are required")
	}
	proxyFunc, err := im.WebsocketProxyFuncFor(cfg, imCfg)
	if err != nil {
		return nil, err
	}
	httpClient := NewHTTPClient(nil, proxyFunc)
	client := slackapi.New(c.BotToken, slackapi.OptionHTTPClient(httpClient), slackapi.OptionAppLevelToken(c.AppToken), slackapi.OptionDebug(false))
	sender, err := NewSender(client)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &SlackAdapter{imCfg: imCfg, router: router, client: client, sender: sender, ctx: ctx, cancel: cancel, done: make(chan struct{}), helloTimeout: 10 * time.Second, pingTimeout: 60 * time.Second, proxyFunc: proxyFunc, http: httpClient}
	a.openSocket = func(ctx context.Context) (string, error) {
		_, address, err := client.StartSocketModeContext(ctx)
		if err != nil {
			return "", SanitizeConnectionError(err)
		}
		if !ValidSocketURL(address) {
			return "", ConnectionError{"invalid_socket_url", true}
		}
		return address, nil
	}
	a.dialSocket = func(ctx context.Context, address string) (*websocket.Conn, error) {
		return dialSlackSocketWithProxy(ctx, address, proxyFunc)
	}
	a.verifyIdentity = func(ctx context.Context) (Identity, error) {
		return VerifyIdentity(ctx, client, c.TeamID, c.AppID)
	}
	return a, nil
}

func (*SlackAdapter) Type() string                { return "slack" }
func (*SlackAdapter) SupportsLoginRenewal() bool  { return false }
func (*SlackAdapter) StartLogin() (string, error) { return "", im.ErrLoginNotSupported }

func (a *SlackAdapter) Connect() error {
	a.mu.Lock()
	if a.started || a.stopping {
		a.mu.Unlock()
		return errors.New("slack: adapter already started or stopped")
	}
	a.started = true
	a.mu.Unlock()
	var locks *im.InstanceLocks
	defer func() {
		a.mu.Lock()
		a.stopping = true
		a.mu.Unlock()
		a.cancel()
		a.setUnavailable()
		a.sends.Wait()
		if a.http != nil {
			a.http.closeIdleConnections()
		}
		if locks != nil {
			locks.Close()
		}
		close(a.done)
	}()
	if a.router == nil || !a.router.IngressAvailable() {
		return errors.New("slack: in-memory ingress and admission are required")
	}
	attempt := 0
	for a.ctx.Err() == nil {
		identity, err := a.verifyIdentity(a.ctx)
		if err == nil {
			a.mu.Lock()
			changed := a.identityVerified && !a.identity.sameInstallation(identity)
			if !changed {
				a.identity, a.identityVerified = identity, true
			}
			a.mu.Unlock()
			if changed {
				// Let the shared authority revoke old account leases and nonce.
				_ = a.router.PlatformReady("slack", identity.AccountID, im.PlatformPresentation{Platform: "slack", BotUsername: identity.BotName})
				return ConnectionError{"identity_changed", true}
			}
			if locks == nil {
				// Exclude other receivers for both the bot account and the app.
				locks, err = im.AcquireIMAccountLocks("slack", identity.AccountID, identity.AppID)
				if err != nil {
					return fmt.Errorf("slack: account/app receive lock: %w", err)
				}
			}
			err = a.runSlackSocket(a.ctx, identity)
			a.mu.Lock()
			stable := a.connectionStable
			a.mu.Unlock()
			if stable {
				attempt = 0
			}
		}
		if a.ctx.Err() != nil {
			return nil
		}
		if ConnectionFatal(err) {
			return err
		}
		if slackReconnectImmediately(err) {
			// A warning/refresh disconnect asks for a prompt reconnect; skip the
			// backoff budget instead of penalising an intentional refresh.
			if !im.SleepCtx(a.ctx, slackImmediateReconnectDelay) {
				return nil
			}
			continue
		}
		delay := im.ReconnectBackoff(attempt)
		var limited *slackapi.RateLimitedError
		if errors.As(err, &limited) {
			delay = max(delay, limited.RetryAfter)
		}
		attempt++
		log.Warnf("slack: connection unavailable category=%s retry_in=%s", ConnectionCategory(err), delay)
		if !im.SleepCtx(a.ctx, delay) {
			return nil
		}
	}
	return nil
}

func (a *SlackAdapter) Disconnect() {
	a.mu.Lock()
	a.stopping = true
	started, conn := a.started, a.conn
	a.mu.Unlock()
	a.cancel()
	a.setUnavailable()
	if conn != nil {
		_ = conn.Close()
	}
	a.http.closeIdleConnections()
	if started {
		<-a.done
	}
}

func (a *SlackAdapter) SendText(chatID, text string) error {
	return a.SendTextContext(context.Background(), chatID, text)
}
func (a *SlackAdapter) TextSendMetrics() im.IMTextMetricsSnapshot {
	return a.sender.text.TextSendMetrics()
}
func (a *SlackAdapter) SendTextContext(ctx context.Context, chatID, text string) error {
	if ctx == nil {
		return errors.New("slack: send requires a context")
	}
	a.mu.Lock()
	identity, verified, stopped := a.identity, a.identityVerified, a.stopping
	if verified && !stopped {
		a.sends.Add(1)
	}
	a.mu.Unlock()
	if !verified || stopped {
		return errors.New("slack: account unavailable")
	}
	defer a.sends.Done()
	if account, _ := ctx.Value(im.ReplyAccountContextKey{}).(string); account != "" && account != identity.AccountID {
		return im.ErrPairingDenied
	}
	return a.sender.Send(ctx, a.ctx, chatID, text)
}

func (a *SlackAdapter) inboundFor(identity Identity) *Inbound {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inbound == nil {
		// Connect fails fatally if the installation identity changes, so one
		// conversation cache serves every socket generation of this adapter.
		a.inbound = NewInbound(a.client, identity, a.router)
	}
	return a.inbound
}

func (a *SlackAdapter) setUnavailable() {
	a.mu.Lock()
	if a.ready && time.Since(a.readySince) >= 60*time.Second {
		a.connectionStable = true
	}
	a.ready = false
	a.mu.Unlock()
	if a.router != nil {
		a.router.PlatformUnavailable("slack")
	}
	a.WakeIngressReadiness()
}
func (a *SlackAdapter) IngressDeliveryReadiness(id im.ChatIdentity) im.IngressDeliveryReadiness {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.identityVerified || a.stopping || id.AccountID != a.identity.AccountID {
		return im.IngressDeliveryReadiness{Reason: im.IngressWaitingAccount}
	}
	if !a.ready {
		return im.IngressDeliveryReadiness{Reason: im.IngressWaitingPlatform}
	}
	return im.IngressDeliveryReadiness{Ready: true}
}

type ConnectionError struct {
	category string
	fatal    bool
}

func (e ConnectionError) Error() string { return "slack: " + e.category }
func SanitizeConnectionError(err error) error {
	var limited *slackapi.RateLimitedError
	if errors.As(err, &limited) {
		return limited
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var response slackapi.SlackErrorResponse
	if errors.As(err, &response) {
		switch response.Err {
		case "invalid_auth", "account_inactive", "token_revoked", "not_authed", "missing_scope":
			return ConnectionError{response.Err, true}
		}
	}
	return ConnectionError{"api_unavailable", false}
}
func ConnectionFatal(err error) bool {
	if errors.Is(err, ErrFatal) {
		return true
	}
	var e ConnectionError
	if errors.As(err, &e) {
		return e.fatal
	}
	// Identity helpers use a bounded diagnostic, never raw transport data.
	var fatal interface{ Fatal() bool }
	return errors.As(err, &fatal) && fatal.Fatal()
}
func ConnectionCategory(err error) string {
	var limited *slackapi.RateLimitedError
	if errors.As(err, &limited) {
		return "rate_limited"
	}
	var e ConnectionError
	if errors.As(err, &e) {
		return e.category
	}
	var category interface{ Category() string }
	if errors.As(err, &category) {
		return category.Category()
	}
	return "connection_failed"
}

// slackReconnectImmediately reports a Slack-requested refresh/warning, which
// should reconnect at once instead of consuming the backoff budget.
func slackReconnectImmediately(err error) bool {
	var e ConnectionError
	if !errors.As(err, &e) {
		return false
	}
	return e.category == "socket_warning" || e.category == "socket_refresh_requested"
}

var _ im.IMTextMetricsProvider = (*SlackAdapter)(nil)

func (a *SlackAdapter) TextSendTargetSnapshot(target string) im.IMTextTargetSnapshot {
	return a.sender.text.TextSendTargetSnapshot(target)
}
