package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/buildinfo"
	gateway "github.com/keakon/chord-gateway/internal/discordgateway"
	"github.com/keakon/chord-gateway/internal/im"
)

// DiscordAdapter owns one authenticated Bot account and its cancellable transport.
type DiscordAdapter struct {
	imCfg                    config.IMAdapterConfig
	router                   *NotificationRouter
	rest                     rest.Rest
	httpClient               *http.Client
	ctx                      context.Context
	cancel                   context.CancelFunc
	mu                       sync.Mutex
	stateMu                  sync.Mutex
	started, stopping, ready bool
	generation               uint64
	accountID                string
	gw                       gateway.Gateway
	inbound                  *discordInbound
	sender                   *im.IMTextSender
	done                     chan struct{}
	doneOnce                 sync.Once
	finishOnce               sync.Once
	accountLock              *im.InstanceLocks
	sends                    sync.WaitGroup
	fatal                    chan error
	proxyFunc                func(*http.Request) (*url.URL, error)

	ingressReadinessNotifier
}

// Discord rejects REST requests that carry no User-Agent, and the SDK default is
// empty, which makes net/http drop the header entirely.
const discordRepositoryURL = "https://github.com/keakon/chord-gateway"

func discordRESTUserAgent() string {
	return "DiscordBot (" + discordRepositoryURL + ", " + buildinfo.Current().Short() + ")"
}

// newDiscordRESTClient builds the REST client for one adapter lifecycle. The
// caller owns the HTTP pool so shutdown can release it; options apply last.
func newDiscordRESTClient(ctx context.Context, httpClient *http.Client, token string, opts ...rest.ClientConfigOpt) rest.Rest {
	return rest.New(rest.NewClient(token, append([]rest.ClientConfigOpt{
		rest.WithHTTPClient(httpClient),
		rest.WithLogger(slog.New(discordSafeLogHandler{})),
		rest.WithRateLimiter(newDiscordRateLimiter(ctx)),
		rest.WithUserAgent(discordRESTUserAgent()),
	}, opts...)...))
}

func NewDiscordAdapter(cfg *config.Config, imCfg config.IMAdapterConfig, _ *config.Paths, router *NotificationRouter) (*DiscordAdapter, error) {
	if imCfg.Discord == nil || strings.TrimSpace(imCfg.Discord.BotToken) == "" || strings.TrimSpace(imCfg.Discord.ApplicationID) == "" {
		return nil, errors.New("discord requires bot_token and application_id")
	}
	proxyFunc, err := im.WebsocketProxyFuncFor(cfg, imCfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	httpClient := newDiscordHTTPClient(proxyFunc)
	a := &DiscordAdapter{imCfg: imCfg, router: router, rest: newDiscordRESTClient(ctx, httpClient, imCfg.Discord.BotToken), httpClient: httpClient, ctx: ctx, cancel: cancel, done: make(chan struct{}), fatal: make(chan error, 1), proxyFunc: proxyFunc}
	a.sender, err = newDiscordTextSender(a.sendSegment)
	if err != nil {
		cancel()
		return nil, err
	}
	return a, nil
}

func (*DiscordAdapter) Type() string                { return "discord" }
func (*DiscordAdapter) SupportsLoginRenewal() bool  { return false }
func (*DiscordAdapter) StartLogin() (string, error) { return "", im.ErrLoginNotSupported }

func (a *DiscordAdapter) Connect() error {
	a.mu.Lock()
	if a.stopping {
		a.mu.Unlock()
		return nil
	}
	if a.started {
		a.mu.Unlock()
		return errors.New("discord adapter already started")
	}
	a.started = true
	a.mu.Unlock()
	defer a.finish()
	if a.router == nil || a.router.admission == nil || a.router.ingress == nil {
		return errors.New("discord requires the common ingress service")
	}
	account, err := a.authenticate(a.ctx)
	if err != nil {
		return a.connectError(err)
	}
	lock, err := im.AcquireIMAccountLock("discord", account)
	if err != nil {
		return fmt.Errorf("discord account lock: %w", err)
	}
	a.mu.Lock()
	a.accountLock = lock
	a.accountID = account
	a.mu.Unlock()
	info, err := a.rest.GetGatewayBot(rest.WithCtx(a.ctx))
	if err != nil {
		return a.connectError(discordRESTError(err))
	}
	if err := discordGatewayInfoError(info); err != nil {
		return err
	}
	if err := a.router.PrepareIngressAccount(a.ctx, "discord", account); err != nil {
		return a.connectError(fmt.Errorf("discord ingress recovery: %w", err))
	}
	a.inbound = newDiscordInbound(a.ctx, account, a.imCfg.Discord.ApplicationID, a.router.SubmitIngressNormalization, a.resolveChannel, a.currentGeneration, a.connectionLive, a.observeIngress)
	limit := info.SessionStartLimit
	dialer := discordProxyDialer(a.proxyFunc)
	g := gateway.New(a.ctx, a.imCfg.Discord.BotToken, nil,
		gateway.WithURL(info.URL), gateway.WithIntents(gateway.IntentGuilds, gateway.IntentGuildMessages, gateway.IntentDirectMessages),
		gateway.WithLogger(slog.New(discordSafeLogHandler{})),
		gateway.WithDialer(dialer),
		gateway.WithSessionStartLimit(limit.Total, limit.Remaining, time.Duration(limit.ResetAfter)*time.Millisecond),
		gateway.WithSessionStartLimitRefresh(a.refreshSessionStartLimit),
		gateway.WithReadyValidator(func(e gateway.EventReady) error {
			if !e.User.Bot || e.User.ID.String() != account || e.Application.ID.String() != a.imCfg.Discord.ApplicationID {
				return errors.New("discord ready_identity_mismatch")
			}
			return nil
		}),
		gateway.WithStateChangeHandler(a.onStateChange), gateway.WithEventCaptureHandler(a.captureEvent),
		gateway.WithCloseHandler(func(_ gateway.Gateway, err error, reconnect bool) {
			if !reconnect {
				a.reportFatal(discordGatewayError(err))
			}
		}),
	)
	a.mu.Lock()
	a.gw = g
	stopped := a.stopping
	a.mu.Unlock()
	if stopped {
		return nil
	}
	if err = g.Open(a.ctx); err != nil {
		return a.connectError(discordGatewayError(err))
	}
	select {
	case <-a.ctx.Done():
		return nil
	case err := <-a.fatal:
		return err
	}
}

func (a *DiscordAdapter) connectError(err error) error {
	if a.ctx.Err() != nil {
		return nil
	}
	return err
}

func (a *DiscordAdapter) authenticate(ctx context.Context) (string, error) {
	var user discord.User
	if err := a.rest.Do(rest.GetCurrentUser.Compile(nil), nil, &user, rest.WithCtx(ctx)); err != nil {
		return "", discordRESTError(err)
	}
	if !user.Bot || user.ID == 0 {
		return "", errors.New("discord bot_account_required")
	}
	application, err := a.rest.GetBotApplicationInfo(rest.WithCtx(ctx))
	if err != nil {
		return "", discordRESTError(err)
	}
	// Both endpoints authenticate with the same Bot token. The application
	// response may omit its optional partial Bot object; when present its ID
	// must agree with the separately authenticated user.
	if application == nil || application.ID.String() != a.imCfg.Discord.ApplicationID || application.Bot != nil && application.Bot.ID != user.ID {
		return "", errors.New("discord application_identity_mismatch")
	}
	return user.ID.String(), nil
}

func discordGatewayInfoError(info *discord.GatewayBot) error {
	if info == nil || info.URL == "" {
		return errors.New("discord invalid_gateway")
	}
	if info.Shards != 1 {
		return errors.New("discord sharding_not_supported")
	}
	limit := info.SessionStartLimit
	if limit.Total <= 0 || limit.Remaining < 0 || limit.Remaining > limit.Total || limit.ResetAfter < 0 ||
		uint64(limit.ResetAfter) > uint64((1<<63-1)/time.Millisecond) || limit.MaxConcurrency < 1 {
		return errors.New("discord invalid_session_start_limit")
	}
	return nil
}

// refreshSessionStartLimit reads a new quota window instead of reusing the
// initial reset_after countdown as though it were a fixed period.
func (a *DiscordAdapter) refreshSessionStartLimit(ctx context.Context) (discord.SessionStartLimit, error) {
	info, err := a.rest.GetGatewayBot(rest.WithCtx(ctx))
	if err != nil {
		safe := discordRESTError(err)
		var response *rest.Error
		if errors.As(err, &response) && response.Response != nil && (response.Response.StatusCode == http.StatusUnauthorized || response.Response.StatusCode == http.StatusForbidden) {
			return discord.SessionStartLimit{}, fmt.Errorf("%w: %w", gateway.ErrSessionStartLimitFatal, safe)
		}
		return discord.SessionStartLimit{}, safe
	}
	if err := discordGatewayInfoError(info); err != nil {
		return discord.SessionStartLimit{}, fmt.Errorf("%w: %w", gateway.ErrSessionStartLimitFatal, err)
	}
	return info.SessionStartLimit, nil
}

func (a *DiscordAdapter) onStateChange(state gateway.Status, generation uint64) {
	a.stateMu.Lock()
	a.mu.Lock()
	if generation < a.generation || a.stopping {
		a.mu.Unlock()
		a.stateMu.Unlock()
		return
	}
	g := a.gw
	if g != nil && (g.Generation() != generation || g.Status() != state) {
		a.mu.Unlock()
		a.stateMu.Unlock()
		return
	}
	wasReady := a.ready
	a.generation = generation
	a.ready = false
	account := a.accountID
	a.mu.Unlock()
	if state == gateway.StatusReady {
		if err := a.router.admission.pairing.PlatformReadyWithPresentation("discord", account, im.PlatformPresentation{Platform: "discord", BotMention: "<@" + account + ">"}); err != nil {
			a.reportFatal(errors.New("discord pairing_unavailable"))
		} else {
			a.mu.Lock()
			a.ready = true
			a.mu.Unlock()
		}
	} else {
		// Channel classification outlives a connection: Discord does not change a
		// channel type because the socket was lost, and Resume replays the missed
		// window into the same cache. Only a fresh Identify after a ready session
		// starts from a different guild state, so that alone may discard it.
		if wasReady && state == gateway.StatusIdentifying && a.inbound != nil {
			a.inbound.ClearChannels()
		}
		a.router.admission.pairing.PlatformUnavailable("discord")
	}
	a.stateMu.Unlock()
	a.wakeIngressReadiness()
}

func (a *DiscordAdapter) currentGeneration(generation uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ready && !a.stopping && a.ctx.Err() == nil && generation == a.generation
}

// connectionLive reports whether a generation still owns this adapter's
// transport. Unlike currentGeneration it ignores the resume barrier, so
// connection-independent classification keeps its cache across a reconnect.
func (a *DiscordAdapter) connectionLive(generation uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.stopping && a.ctx.Err() == nil && generation == a.generation
}

// observeIngress forwards bounded ingress diagnostics to the shared metrics.
func (a *DiscordAdapter) observeIngress(event, reason string) {
	if a.router != nil {
		a.router.ObserveIngress("discord", event, reason)
	}
}

// captureEvent runs synchronously at transport arrival, including Resume replay.
// Only message closures occupy the SDK's bounded replay buffer.
func (a *DiscordAdapter) captureEvent(_ gateway.EventType, data gateway.EventData, generation uint64) func() {
	a.mu.Lock()
	valid := !a.stopping && a.ctx.Err() == nil && generation == a.generation
	a.mu.Unlock()
	if !valid {
		return nil
	}
	if event, ok := data.(gateway.EventMessageCreate); ok {
		delivery, err := a.inbound.CaptureMessage(generation, event.Message, a.captureEnvelope)
		if err != nil {
			discordLogCategory("ingress_rejected", "invalid_input")
			return nil
		}
		if delivery == nil {
			return nil
		}
		return func() {
			if err := delivery(); err != nil {
				if a.router.ingressStore != nil {
					reason := "admission_unavailable"
					if errors.Is(err, errIngressCapacity) {
						reason = "capacity"
					}
					if errors.Is(err, context.Canceled) {
						reason = "cancelled"
					}
					a.router.ingressStore.ObserveLostBeforeAccept("discord", reason)
				}
				discordLogCategory("ingress_rejected", "ingress_unavailable")
			}
		}
	}
	// Classification is not authorization and never needs a replay slot: apply it
	// on arrival, resume window included, so a reconnecting generation does not
	// rediscover channel types through a rate-limited REST lookup.
	a.applyChannelMetadata(data, generation)
	return nil
}

// captureEnvelope must return on the Gateway reader even if stdin or session
// persistence holds a process lock. A failed snapshot stays a frozen refusal;
// the common ingress persists it instead of capturing a newer control target.
func (a *DiscordAdapter) captureEnvelope(e im.IngressEnvelope) im.IngressEnvelope {
	if e.Capture == nil {
		kind, target, available := a.router.tryCaptureIngress(e.Message)
		e.Capture = &im.IngressCapture{Kind: kind, Target: target, Unavailable: !available}
	}
	return e
}

// applyChannelMetadata caches channel and thread classification. It never runs
// for MESSAGE_CREATE: a message is captured and delivered through the
// resume-aware closure, and classification cannot start a turn.
func (a *DiscordAdapter) applyChannelMetadata(data gateway.EventData, generation uint64) {
	if a.inbound == nil {
		return
	}
	switch event := data.(type) {
	case gateway.EventChannelCreate:
		a.inbound.CacheChannel(generation, event.GuildChannel)
	case gateway.EventChannelUpdate:
		a.inbound.CacheChannel(generation, event.GuildChannel)
	case gateway.EventChannelDelete:
		if event.GuildChannel != nil {
			a.inbound.DeleteChannel(generation, event.ID().String())
		}
	case gateway.EventThreadCreate:
		a.inbound.CacheChannel(generation, event.GuildThread)
	case gateway.EventThreadUpdate:
		a.inbound.CacheChannel(generation, event.GuildThread)
	case gateway.EventThreadDelete:
		a.inbound.DeleteChannel(generation, event.ID.String())
	case gateway.EventThreadListSync:
		for _, thread := range event.Threads {
			a.inbound.CacheChannel(generation, thread)
		}
	}
}

func (a *DiscordAdapter) reportFatal(err error) {
	select {
	case a.fatal <- err:
	default:
	}
}

func (a *DiscordAdapter) stop() {
	a.stateMu.Lock()
	a.mu.Lock()
	a.stopping = true
	a.ready = false
	a.cancel()
	a.mu.Unlock()
	if a.router != nil && a.router.admission != nil {
		a.router.admission.pairing.PlatformUnavailable("discord")
	}
	a.stateMu.Unlock()
	a.wakeIngressReadiness()
}
func (a *DiscordAdapter) finish() {
	a.finishOnce.Do(func() {
		a.stop()
		a.mu.Lock()
		g := a.gw
		lock := a.accountLock
		a.mu.Unlock()
		if g != nil {
			// Close joins workers; the socket close interrupts blocked IO so
			// the join is bounded. No timeout is applied because abandoning
			// the join would leak workers holding the account lock.
			g.Close(context.Background())
		}
		a.sends.Wait()
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		a.rest.Close(closeCtx)
		cancel()
		// The REST pool belongs to this adapter, so release its idle sockets here
		// instead of relying on the SDK client to do it while closing.
		if a.httpClient != nil {
			a.httpClient.CloseIdleConnections()
		}
		// Keep account ownership until every transport and outbound worker has exited.
		if lock != nil {
			lock.Close()
		}
		a.doneOnce.Do(func() { close(a.done) })
	})
}
func (a *DiscordAdapter) Disconnect() {
	a.stop()
	a.mu.Lock()
	started := a.started
	g := a.gw
	a.mu.Unlock()
	if !started {
		a.finish()
		return
	}
	if g != nil {
		// See finish(): Close joins bounded workers, no timeout applied.
		g.Close(context.Background())
	}
	<-a.done
}
func (a *DiscordAdapter) IngressDeliveryReadiness(id im.ChatIdentity) im.IngressDeliveryReadiness {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id.Platform != "discord" || id.AccountID != a.accountID {
		return im.IngressDeliveryReadiness{Reason: im.IngressWaitingAccount}
	}
	if !a.ready || a.stopping {
		return im.IngressDeliveryReadiness{Reason: im.IngressWaitingPlatform}
	}
	return im.IngressDeliveryReadiness{Ready: true}
}
func (a *DiscordAdapter) resolveChannel(ctx context.Context, id string) (discord.Channel, error) {
	channelID, err := snowflake.Parse(id)
	if err != nil || channelID == 0 {
		return nil, errors.New("discord invalid_channel")
	}
	channel, err := a.rest.GetChannel(channelID, rest.WithCtx(ctx))
	if err != nil {
		return nil, discordRESTError(err)
	}
	return channel, nil
}
