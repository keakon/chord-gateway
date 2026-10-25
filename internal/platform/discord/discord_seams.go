package discord

// Exported seams for the root package test suite. Root tests stay in package
// main because they drive the shared router, admission and ingress fixtures,
// so the adapter internals they reach are exposed here under ForTest names.
// Nothing in this file changes production behavior.

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/gorilla/websocket"

	gateway "github.com/keakon/chord-gateway/internal/discordgateway"
	"github.com/keakon/chord-gateway/internal/im"
)

// ContextForTest exposes the adapter lifecycle context.
func (a *DiscordAdapter) ContextForTest() context.Context { return a.ctx }

// RESTForTest returns the adapter's REST client.
func (a *DiscordAdapter) RESTForTest() rest.Rest { return a.rest }

// SetRESTForTest points the adapter's REST client at a test-supplied client.
func (a *DiscordAdapter) SetRESTForTest(r rest.Rest) { a.rest = r }

// StateForTest snapshots account, generation and readiness under the adapter lock.
func (a *DiscordAdapter) StateForTest() (accountID string, generation uint64, ready, stopping bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accountID, a.generation, a.ready, a.stopping
}

// GatewayForTest returns the live gateway handle for sequence assertions.
func (a *DiscordAdapter) GatewayForTest() gateway.Gateway {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gw
}

// SetSessionForTest seeds account identity and readiness without a connection.
func (a *DiscordAdapter) SetSessionForTest(accountID string, generation uint64, ready bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.accountID, a.generation, a.ready = accountID, generation, ready
}

// AccountIDForTest reports the authenticated account ID.
func (a *DiscordAdapter) AccountIDForTest() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accountID
}

// CurrentGenerationForTest exposes the resume-barrier generation guard.
func (a *DiscordAdapter) CurrentGenerationForTest(generation uint64) bool {
	return a.currentGeneration(generation)
}

// OnStateChangeForTest drives the transport state handler without a gateway.
func (a *DiscordAdapter) OnStateChangeForTest(state gateway.Status, generation uint64) {
	a.onStateChange(state, generation)
}

// ApplyChannelMetadataForTest feeds classification events without a gateway.
func (a *DiscordAdapter) ApplyChannelMetadataForTest(data gateway.EventData, generation uint64) {
	a.applyChannelMetadata(data, generation)
}

// AuthenticateForTest runs the REST identity handshake.
func (a *DiscordAdapter) AuthenticateForTest(ctx context.Context) (string, error) {
	return a.authenticate(ctx)
}

// SetInboundForTest replaces the shared channel classification cache.
func (a *DiscordAdapter) SetInboundForTest(inbound *discordInbound) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inbound = inbound
}

// GenerationGuardsForTest exposes the generation guards and metrics observer
// that the shared inbound constructor takes.
func (a *DiscordAdapter) GenerationGuardsForTest() (current, live func(uint64) bool, observe func(event, reason string)) {
	return a.currentGeneration, a.connectionLive, a.observeIngress
}

// SetSenderForTest replaces the shared text sender.
func (a *DiscordAdapter) SetSenderForTest(sender *im.IMTextSender) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sender = sender
}

// ChannelForTest reads one cached channel classification.
func (d *discordInbound) ChannelForTest(id string) (discord.ChannelType, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	metadata, exists := d.channels[id]
	return metadata.kind, exists
}

// FillChannelCacheForTest adds count cache entries so tests can observe the
// bounded eviction behavior without constructing channel objects.
func (d *discordInbound) FillChannelCacheForTest(count int, kind discord.ChannelType) {
	for index := range count {
		d.cacheChannel(strconv.Itoa(index), discordChannelMetadata{kind})
	}
}

// CacheSizeForTest reports the number of cached channel classifications.
func (d *discordInbound) CacheSizeForTest() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.channels)
}

// SetSubmitForTest replaces the shared normalizer entry point. Callers must do
// this before the inbound starts handling messages.
func (d *discordInbound) SetSubmitForTest(submit discordIngressSubmit) {
	d.submit = submit
}

// DiscordContentlessStateForTest returns the contentless diagnostic budget state.
func DiscordContentlessStateForTest() (last time.Time, reported int) {
	discordContentless.mu.Lock()
	defer discordContentless.mu.Unlock()
	return discordContentless.last, discordContentless.reported
}

// SetDiscordContentlessStateForTest overrides the contentless diagnostic
// budget state so tests can exercise interval and budget boundaries.
func SetDiscordContentlessStateForTest(last time.Time, reported int) {
	discordContentless.mu.Lock()
	defer discordContentless.mu.Unlock()
	discordContentless.last, discordContentless.reported = last, reported
}

// BucketForTest snapshots one route bucket's state under the limiter lock.
func (l *discordRateLimiter) BucketForTest(endpoint *rest.CompiledEndpoint) (reset time.Time, remaining, references int, busy bool) {
	key, err := discordRateLimitEndpointKey(endpoint)
	if err != nil {
		return time.Time{}, 0, 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		return time.Time{}, 0, 0, false
	}
	return b.reset, b.remaining, b.references, b.busy
}

// GlobalResetForTest returns the current global reset deadline.
func (l *discordRateLimiter) GlobalResetForTest() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.global
}

// BucketCountForTest reports how many route buckets the limiter tracks.
func (l *discordRateLimiter) BucketCountForTest() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// NewRateLimitTransportForTest wraps a base transport exactly like the owned
// production HTTP client does, so tests can install it over a stub transport.
func NewRateLimitTransportForTest(base http.RoundTripper) http.RoundTripper {
	return discordRateLimitTransport{base: base}
}

// SafeLogHandlerForTest builds the sanitized SDK log handler.
func SafeLogHandlerForTest() slog.Handler { return discordSafeLogHandler{} }

// ProxyDialerForTest builds the SDK dialer from a proxy function.
func ProxyDialerForTest(proxyFunc func(*http.Request) (*url.URL, error)) *websocket.Dialer {
	return discordProxyDialer(proxyFunc)
}
