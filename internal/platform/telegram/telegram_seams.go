package telegram

// Exported seams for the root package test suite. Root tests stay in package
// main because they drive the shared router, admission and ingress fixtures,
// so the poller internals they reach are exposed here under ForTest names.
// Nothing in this file changes production behavior.

import (
	"context"
	"net/http"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

// NewTelegramAdapterForTest builds a poller fixture with the internal fields
// root tests drive directly. nil now, ctx or cancel fall back to the
// production clock and a fresh background lifecycle.
func NewTelegramAdapterForTest(accountID, username string, now func() time.Time, ctx context.Context, cancel context.CancelFunc) *TelegramAdapter {
	if now == nil {
		now = realTelegramClock
	}
	if ctx == nil {
		ctx, cancel = context.WithCancel(context.Background())
	}
	return &TelegramAdapter{accountID: accountID, username: username, now: now, ctx: ctx, cancel: cancel}
}

// SetAPIForTest replaces the transport API.
func (a *TelegramAdapter) SetAPIForTest(api API) { a.api = api }

// SetSenderForTest replaces the account-wide text sender.
func (a *TelegramAdapter) SetSenderForTest(sender *im.IMTextSender) { a.sender = sender }

// SetClockForTest replaces the poller clock.
func (a *TelegramAdapter) SetClockForTest(now func() time.Time) { a.now = now }

// SetCursorStoreForTest replaces the durable cursor store.
func (a *TelegramAdapter) SetCursorStoreForTest(store telegramCursorStore) { a.store = store }

// SetIngressHooksForTest replaces the receive and reject entry points.
func (a *TelegramAdapter) SetIngressHooksForTest(receive func(context.Context, im.IngressEnvelope) im.ReceiveResult, reject func(context.Context, im.IngressEnvelope, string) im.ReceiveResult) {
	a.receive, a.reject = receive, reject
}

// IngressHooksForTest returns the current receive and reject entry points so
// tests can wrap them.
func (a *TelegramAdapter) IngressHooksForTest() (receive func(context.Context, im.IngressEnvelope) im.ReceiveResult, reject func(context.Context, im.IngressEnvelope, string) im.ReceiveResult) {
	return a.receive, a.reject
}

// SetAccountForTest seeds the authenticated account identity.
func (a *TelegramAdapter) SetAccountForTest(accountID, username string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.accountID, a.username = accountID, username
}

// SetRunningForTest marks the poller running so Connect refuses a second start.
func (a *TelegramAdapter) SetRunningForTest(running bool) { a.running = running }

// ContextForTest exposes the lifecycle context.
func (a *TelegramAdapter) ContextForTest() context.Context { return a.ctx }

// ReadyForTest reports current readiness under the adapter lock.
func (a *TelegramAdapter) ReadyForTest() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ready
}

// AccountIDForTest reports the authenticated account ID.
func (a *TelegramAdapter) AccountIDForTest() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accountID
}

// StartupRequestForTest drives the startup retry loop.
func (a *TelegramAdapter) StartupRequestForTest(request func() error) error {
	return a.startupRequest(request)
}

// LoadCursorForTest loads and repairs the durable cursor.
func (a *TelegramAdapter) LoadCursorForTest(ctx context.Context) (im.IngressCursor, error) {
	return a.loadCursor(ctx)
}

// PollRequestForTest issues one poll request with retry pacing.
func (a *TelegramAdapter) PollRequestForTest(ctx context.Context, cursor *im.IngressCursor, probe bool) ([]Update, error) {
	return a.pollRequest(ctx, cursor, probe)
}

// PollForTest drives the acknowledge loop for one batch.
func (a *TelegramAdapter) PollForTest(ctx context.Context, cursor im.IngressCursor, batch []Update, hasResponse bool) error {
	return a.poll(ctx, cursor, batch, hasResponse)
}

// CaptureBatchForTest freezes one batch before readiness or admission.
func (a *TelegramAdapter) CaptureBatchForTest(batch []Update) { a.captureBatch(batch) }

// NormalizeUpdateForTest normalizes one update without capturing it.
func (a *TelegramAdapter) NormalizeUpdateForTest(update Update) (im.IngressEnvelope, string) {
	return a.normalizeUpdate(update)
}

// ConfigureAPIForTest points a real API at a test server without changing the
// redirect policy NewAPI installs.
func ConfigureAPIForTest(api *httpAPI, baseURL string, client *http.Client) {
	api.baseURL, api.client = baseURL, client
}

// RealClockForTest returns the production clock.
func RealClockForTest() func() time.Time { return realTelegramClock }

// LongPollSecondsForTest returns the production long-poll timeout.
func LongPollSecondsForTest() int { return telegramLongPollSeconds }

// SetReceiveForTest replaces only the ingress receive hook.
func (a *TelegramAdapter) SetReceiveForTest(receive func(context.Context, im.IngressEnvelope) im.ReceiveResult) {
	a.receive = receive
}

// ReceiveForTest returns the ingress receive hook.
func (a *TelegramAdapter) ReceiveForTest() func(context.Context, im.IngressEnvelope) im.ReceiveResult {
	return a.receive
}

// CapturedForTest reports which updates in a batch were frozen.
func (a *TelegramAdapter) CapturedForTest(batch []Update) []bool {
	out := make([]bool, len(batch))
	for i := range batch {
		out[i] = batch[i].captured != nil
	}
	return out
}

// SetRejectForTest replaces only the ingress reject hook.
func (a *TelegramAdapter) SetRejectForTest(reject func(context.Context, im.IngressEnvelope, string) im.ReceiveResult) {
	a.reject = reject
}

// HasMediaForTest reports whether the update carries a media attachment.
func (u Update) HasMediaForTest() bool {
	return u.Message != nil && u.Message.hasMedia
}

// SetHasMediaForTest marks the message as carrying media.
func (u *Update) SetHasMediaForTest(v bool) {
	if u.Message != nil {
		u.Message.hasMedia = v
	}
}

// CapturedEnvelopesForTest returns the frozen envelopes for a batch.
func (a *TelegramAdapter) CapturedEnvelopesForTest(batch []Update) []im.IngressEnvelope {
	out := make([]im.IngressEnvelope, 0, len(batch))
	for i := range batch {
		if batch[i].captured != nil {
			out = append(out, batch[i].captured.envelope)
		}
	}
	return out
}
