package slack

// Exported seams for the root package test suite. Root tests stay in package
// main because they drive the shared router, admission and ingress fixtures,
// so the adapter and inbound internals they reach are exposed here under
// ForTest names. Nothing in this file changes production behavior.

import (
	"context"
	"time"

	"github.com/gorilla/websocket"
	slackapi "github.com/slack-go/slack"

	"github.com/keakon/chord-gateway/internal/im"
)

// SetIdentityForTest seeds the installation identity and its verified flag.
func (a *SlackAdapter) SetIdentityForTest(identity Identity, verified bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.identity, a.identityVerified = identity, verified
}

// SetVerifyIdentityForTest replaces the identity verification hook.
func (a *SlackAdapter) SetVerifyIdentityForTest(verify func(context.Context) (Identity, error)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.verifyIdentity = verify
}

// SetSocketHooksForTest replaces the socket open and dial transport hooks.
func (a *SlackAdapter) SetSocketHooksForTest(open func(context.Context) (string, error), dial func(context.Context, string) (*websocket.Conn, error)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.openSocket, a.dialSocket = open, dial
}

// SetTimeoutsForTest overrides the hello and ping timeouts.
func (a *SlackAdapter) SetTimeoutsForTest(hello, ping time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.helloTimeout, a.pingTimeout = hello, ping
}

// SetClientForTest points the Web API client at a test-supplied client.
func (a *SlackAdapter) SetClientForTest(client *slackapi.Client) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.client = client
}

// CancelForTest cancels the adapter lifecycle context.
func (a *SlackAdapter) CancelForTest() { a.cancel() }

// ReadyForTest reports current readiness under the adapter lock.
func (a *SlackAdapter) ReadyForTest() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ready
}

// SocketStateForTest snapshots the live connection and its generation.
func (a *SlackAdapter) SocketStateForTest() (conn *websocket.Conn, generation uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conn, a.generation
}

// HostForTest returns the transport host so root tests can reach the shared
// router behind the interface.
func (a *SlackAdapter) HostForTest() im.IMSocketHost { return a.router }

// InboundForTest returns the installation-wide conversation cache, creating it
// on first use exactly like the socket supervisor does.
func (a *SlackAdapter) InboundForTest(identity Identity) *Inbound { return a.inboundFor(identity) }

// RunSocketForTest drives one socket generation without the Connect supervisor.
func (a *SlackAdapter) RunSocketForTest(ctx context.Context, identity Identity) error {
	return a.runSlackSocket(ctx, identity)
}

// ACKWriterForTest drives the ACK writer against a test connection.
func (a *SlackAdapter) ACKWriterForTest(ctx context.Context, conn *websocket.Conn, generation uint64, queue <-chan ACK, fail func(error)) {
	a.slackACKWriter(ctx, conn, generation, queue, fail)
}

// QueueACKForTest enqueues one ACK frame without a live socket.
func (a *SlackAdapter) QueueACKForTest(ctx context.Context, generation uint64, frame SocketFrame, receivedAt time.Time, hint string, queue chan<- ACK) <-chan error {
	return a.queueSlackACK(ctx, generation, frame, receivedAt, hint, queue)
}

// ReceiptACKForTest drives the receipt-to-ACK bridge without a live socket.
func (a *SlackAdapter) ReceiptACKForTest(ctx context.Context, generation uint64, receipt Receipt, acks chan<- ACK) {
	a.slackReceiptACK(ctx, generation, receipt, acks)
}

// SetClientForTest points the inbound conversation client at a test client.
func (s *Inbound) SetClientForTest(client *slackapi.Client) { s.client = client }

// SetRouterForTest replaces the transport host the inbound submits to.
func (s *Inbound) SetRouterForTest(router im.IMSocketHost) { s.router = router }

// IdentityForTest reports the installation identity the inbound is bound to.
func (s *Inbound) IdentityForTest() Identity { return s.identity }

// CacheForTest runs mutate while holding the conversation cache lock, so root
// tests can seed and inspect cache entries without racing the normalizer.
func (s *Inbound) CacheForTest(mutate func(cache map[string]CachedConversation)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mutate(s.cache)
}

// DecodeForTest exposes the pure inbound decoder.
func (s *Inbound) DecodeForTest(kind string, payload []byte, envelopeID string, receivedAt time.Time) (im.IngressEnvelope, string, bool, error) {
	return s.decode(kind, payload, envelopeID, receivedAt)
}

// SubmitForTest exposes the inbound submit path.
func (s *Inbound) SubmitForTest(ctx context.Context, kind string, payload []byte, envelopeID string, receivedAt time.Time) (<-chan im.ReceiveResult, context.CancelFunc, error) {
	return s.submit(ctx, kind, payload, envelopeID, receivedAt)
}

// ConversationForTest exposes the conversation lookup and cache.
func (s *Inbound) ConversationForTest(ctx context.Context, channel string, force bool) (slackapi.Channel, error) {
	return s.conversation(ctx, channel, force)
}

// NewCachedConversationForTest builds a cache entry with a chosen expiry.
func NewCachedConversationForTest(expires time.Time) CachedConversation {
	return CachedConversation{expires: expires}
}

// SetOpenSocketForTest replaces only the socket-open hook.
func (a *SlackAdapter) SetOpenSocketForTest(open func(context.Context) (string, error)) {
	a.openSocket = open
}

// SetHostForTest replaces the transport host.
func (a *SlackAdapter) SetHostForTest(router im.IMSocketHost) {
	a.router = router
}
