package dingtalk

// Test-only seams for the gateway joint tests, which live in the root package
// and cannot reach unexported members across the package boundary. Production
// code must not use anything in this file.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

// WaitTestForTest waits for one channel value with a bounded deadline so a
// hung test reports a failure instead of timing out the whole run.
func WaitTestForTest[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stream lifecycle")
		var zero T
		return zero
	}
}

// IdentityTestConfigForTest returns the neutral enterprise fixture config.
func IdentityTestConfigForTest() *config.DingtalkConfig {
	return &config.DingtalkConfig{ClientID: "test-client", ClientSecret: "test-secret", CorpID: "test-corp", RobotCode: "test-robot"}
}

// IdentityTestCallbackForTest returns a well-formed private text callback that
// carries a bind command; tests mutate its fields per scenario.
func IdentityTestCallbackForTest() DingtalkCallback {
	callback := DingtalkCallback{MsgID: "test-message", ConversationType: "1", ConversationID: "test-conversation",
		SenderStaffID: "test-staff", SenderCorpID: "test-corp", ChatbotCorpID: "test-corp", RobotCode: "test-robot", MsgType: "text"}
	callback.Text.Content = " \t\n/bind \"/project/path @literal\" nonce\n"
	return callback
}

// IdentityTestPayloadForTest serializes a callback into the Stream wire format.
func IdentityTestPayloadForTest(t *testing.T, callback DingtalkCallback) string {
	t.Helper()
	data, err := json.Marshal(callback)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// FrameForTest builds a CALLBACK frame for one callback and delivery ID,
// mirroring the Stream wire format the robot topic uses.
func FrameForTest(t *testing.T, callback DingtalkCallback, delivery string) DingtalkFrame {
	t.Helper()
	return DingtalkFrame{SpecVersion: "1.0", Type: "CALLBACK", Headers: map[string]json.RawMessage{
		"topic": json.RawMessage(fmt.Sprintf("%q", DingtalkRobotTopic)), "messageId": json.RawMessage(fmt.Sprintf("%q", delivery)),
		"time": json.RawMessage(fmt.Sprintf("%d", time.Now().UnixMilli())),
	}, Data: IdentityTestPayloadForTest(t, callback)}
}

// AcquireAccountLocksForTest re-acquires the account locks after shutdown.
func AcquireAccountLocksForTest(cfg *config.DingtalkConfig) (*im.InstanceLocks, error) {
	return acquireDingtalkAccountLocks(cfg)
}

// EncodeTargetForTest encodes one private or group delivery target.
func EncodeTargetForTest(corp, kind, raw string) (string, error) {
	return dingtalkEncodeTarget(corp, kind, raw)
}

// NormalizeCallbackForTest runs the pure callback normalizer on a raw payload.
func NormalizeCallbackForTest(cfg *config.DingtalkConfig, payload string) (im.IncomingMessage, error) {
	return normalizeDingtalkCallback(cfg, payload)
}

// AssertIdentityCategoryForTest asserts that err is an identity refusal with
// the expected category and terminal classification.
func AssertIdentityCategoryForTest(t *testing.T, err error, category string, terminal bool) {
	t.Helper()
	var identityErr *dingtalkIdentityError
	if !errors.As(err, &identityErr) || identityErr.Category != category || identityErr.Terminal() != terminal || err.Error() != category {
		t.Fatalf("error = %v, want %s (terminal %v)", err, category, terminal)
	}
}

// WriteTokenForTest answers a token request with a canned access token.
func WriteTokenForTest(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"accessToken":"test-token","expireIn":7200}`))
}

// AccountForTest reports the authenticated account identity.
func (a *DingtalkAdapter) AccountForTest() string { return a.account }

// ConfigForTest exposes the adapter's enterprise configuration.
func (a *DingtalkAdapter) ConfigForTest() *config.DingtalkConfig { return a.cfg }

// ContextForTest exposes the adapter lifecycle context.
func (a *DingtalkAdapter) ContextForTest() context.Context { return a.ctx }

// SenderForTest returns the shared text sender for segment interception.
func (a *DingtalkAdapter) SenderForTest() *im.IMTextSender { return a.sender }

// VerifiedForTest reports identity verification under the adapter lock.
func (a *DingtalkAdapter) VerifiedForTest() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.verified
}

// SetVerifiedForTest seeds identity verification evidence.
func (a *DingtalkAdapter) SetVerifiedForTest(verified bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.verified = verified
}

// GenerationForTest reports the connection generation under the adapter lock.
func (a *DingtalkAdapter) GenerationForTest() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.generation
}

// AdvanceGenerationForTest models a new handshake generation and returns it.
func (a *DingtalkAdapter) AdvanceGenerationForTest() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.generation++
	return a.generation
}

// SetConnectedForTest marks the transport connected without a socket.
func (a *DingtalkAdapter) SetConnectedForTest(connected bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.connected = connected
}

// ReadyForTest reports published readiness under the adapter lock.
func (a *DingtalkAdapter) ReadyForTest() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ready
}

// SetStoppingForTest seals the adapter against new admitted work.
func (a *DingtalkAdapter) SetStoppingForTest(stopping bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopping = stopping
}

// OnConnectedForTest drives the OnConnected callback.
func (a *DingtalkAdapter) OnConnectedForTest(ctx context.Context) error { return a.onConnected(ctx) }

// OnCallbackForTest drives the OnCallback callback with one protocol frame.
func (a *DingtalkAdapter) OnCallbackForTest(ctx context.Context, frame DingtalkFrame) (<-chan im.ReceiveResult, error) {
	return a.onCallback(ctx, frame)
}

// OnUnavailableForTest drives the OnUnavailable callback.
func (a *DingtalkAdapter) OnUnavailableForTest() { a.onUnavailable() }

// ActivateForTest drives activation for one connection generation.
func (a *DingtalkAdapter) ActivateForTest(ctx context.Context, generation uint64) error {
	return a.activate(ctx, generation)
}

// TrackAcceptanceForTest drives the acceptance tracking relay directly.
func (a *DingtalkAdapter) TrackAcceptanceForTest(future <-chan im.ReceiveResult, msg im.IncomingMessage) <-chan im.ReceiveResult {
	return a.trackAcceptance(future, msg)
}

// SetStreamOpenConnectionForTest replaces the registration fetch.
func (a *DingtalkAdapter) SetStreamOpenConnectionForTest(openConnection func(context.Context) (string, string, error)) {
	a.stream.openConnection = openConnection
}

// SetStreamBackoffForTest replaces the reconnect backoff curve.
func (a *DingtalkAdapter) SetStreamBackoffForTest(backoff func(int) time.Duration) {
	a.stream.backoff = backoff
}

// SetStreamTLSConfigForTest points the Stream dialer at a test TLS trust pool.
func (a *DingtalkAdapter) SetStreamTLSConfigForTest(tlsConfig *tls.Config) {
	a.stream.dialer.TLSClientConfig = tlsConfig
}

// SetAPIServerForTest points the REST client at a test server.
func (a *DingtalkAdapter) SetAPIServerForTest(baseURL string, client *http.Client) {
	a.api.baseURL, a.api.client = baseURL, client
}

// SetAPIPacingForTest overrides the account and target send pacing gates.
func (a *DingtalkAdapter) SetAPIPacingForTest(accountInterval, targetInterval time.Duration) {
	a.api.accountInterval, a.api.targetInterval = accountInterval, targetInterval
}

// SetAPIRetryBaseForTest overrides the 429 retry base delay.
func (a *DingtalkAdapter) SetAPIRetryBaseForTest(retryBase time.Duration) {
	a.api.retryBase = retryBase
}

// HoldTargetSlotForTest reserves a target's next send slot until the deadline.
func (a *DingtalkAdapter) HoldTargetSlotForTest(target string, until time.Time) {
	a.api.rateMu.Lock()
	a.api.targetNext[target] = until
	a.api.rateMu.Unlock()
}

// HoldAccountSlotForTest reserves the account-wide send slot until the deadline.
func (a *DingtalkAdapter) HoldAccountSlotForTest(until time.Time) {
	a.api.rateMu.Lock()
	a.api.accountNext = until
	a.api.rateMu.Unlock()
}

// WaitRateLimitForTest drives the rate-limit wait directly.
func (a *DingtalkAdapter) WaitRateLimitForTest(ctx context.Context, delay time.Duration) error {
	return a.api.waitRateLimit(ctx, delay)
}
