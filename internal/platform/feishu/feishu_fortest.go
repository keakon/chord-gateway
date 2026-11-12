// Test seams for the gateway package's joint regression tests. The
// production code lives in feishu.go, feishu_ws.go and feishu_token.go.
package feishu

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	larkcallback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

// WireForTest installs explicit collaborators for the gateway package's
// regression tests, which drive the adapter against a live router instead of
// through the ordinary constructor path. It must be called before Connect.
func (a *FeishuAdapter) WireForTest(imCfg config.IMAdapterConfig, httpClient *http.Client, router Host) {
	a.imCfg = imCfg
	a.httpClient = httpClient
	a.fragments = make(map[string]feishuFragmentBuffer)
	a.pingInterval = feishuDefaultPing
	a.reconnectInterval = feishuDefaultReconnect
	a.host = router
	a.notifier = router
	a.runLongConn = a.runLongConnection
}

// RetargetForTest re-points a test adapter at another gateway host.
func (a *FeishuAdapter) RetargetForTest(router Host) {
	a.host = router
	a.notifier = router
}

// SetTokenForTest seeds the access token cache.
func (a *FeishuAdapter) SetTokenForTest(token string, expireAt time.Time) {
	a.accessToken = token
	a.tokenExpireAt = expireAt
}

// SetHTTPClientForTest swaps the outbound HTTP client.
func (a *FeishuAdapter) SetHTTPClientForTest(c *http.Client) { a.httpClient = c }

// FragmentsSnapshotForTest exposes the fragment buffer keys for assertions.
func (a *FeishuAdapter) FragmentsSnapshotForTest() map[string]feishuFragmentBuffer {
	a.fragmentsMu.Lock()
	defer a.fragmentsMu.Unlock()
	return a.fragments
}

// SendMessageContextForTest exposes the raw interactive send for gateway
// package tests that pin its wire behavior.
func (a *FeishuAdapter) SendMessageContextForTest(ctx context.Context, chatID, msgType string, content any, retry bool) (*im.InteractiveCardHandle, error) {
	return a.sendMessageContext(ctx, chatID, msgType, content, retry)
}

// UpdateInteractiveMessageContextForTest exposes the raw card patch for gateway
// package tests.
func (a *FeishuAdapter) UpdateInteractiveMessageContextForTest(ctx context.Context, messageID string, card map[string]any, retry bool) error {
	return a.updateInteractiveMessageContext(ctx, messageID, card, retry)
}

// TokenStateForTest exposes the cached token and its expiry.
func (a *FeishuAdapter) TokenStateForTest() (string, time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accessToken, a.tokenExpireAt
}

// SetActiveAccountForTest overrides the activated account marker.
func (a *FeishuAdapter) SetActiveAccountForTest(account string) {
	a.mu.Lock()
	a.activeAccount = account
	a.mu.Unlock()
}

// ClearTokenForTest drops the cached token under the adapter lock.
func (a *FeishuAdapter) ClearTokenForTest() {
	a.mu.Lock()
	a.accessToken = ""
	a.mu.Unlock()
}

// HostForTest exposes the wired gateway host.
func (a *FeishuAdapter) HostForTest() Host { return a.host }

// GetAccessTokenForTest drives the token acquisition/refresh path.
func (a *FeishuAdapter) GetAccessTokenForTest(ctx context.Context) (string, error) {
	return a.getAccessToken(ctx)
}

// TokenRefreshForTest exposes the in-flight refresh state for assertions.
func (a *FeishuAdapter) TokenRefreshForTest() *feishuTokenRefresh {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tokenRefresh
}

// ActiveAccountForTest reports the account the adapter last activated.
func (a *FeishuAdapter) ActiveAccountForTest() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.activeAccount
}

// ReconnectIntervalForTest exposes the reconnect backoff for assertions.
func (a *FeishuAdapter) ReconnectIntervalForTest() time.Duration {
	a.intervalMu.RLock()
	defer a.intervalMu.RUnlock()
	return a.reconnectInterval
}

// SetReconnectIntervalForTest overrides the reconnect backoff.
func (a *FeishuAdapter) SetReconnectIntervalForTest(d time.Duration) {
	a.intervalMu.Lock()
	a.reconnectInterval = d
	a.intervalMu.Unlock()
}

// IMConfigForTest exposes the adapter's configured IM entry.
func (a *FeishuAdapter) IMConfigForTest() config.IMAdapterConfig { return a.imCfg }

// StartGenerationForTest drives the connection-generation supervisor with a
// test connection.
func (a *FeishuAdapter) StartGenerationForTest(ctx context.Context, conn *websocket.Conn) *feishuConnectionGeneration {
	return a.startGeneration(ctx, conn)
}

// GenerationContextForTest exposes a generation's lifecycle context.
func (g *feishuConnectionGeneration) GenerationContextForTest() context.Context { return g.ctx }

// CloseGenerationForTest terminates a test generation.
func (g *feishuConnectionGeneration) CloseGenerationForTest() { g.close() }

// HandleWSFrameForTest drives the websocket frame handler.
func (a *FeishuAdapter) HandleWSFrameForTest(ctx context.Context, conn *websocket.Conn, msg []byte) error {
	return a.handleWSFrame(ctx, conn, msg)
}

// HandleDataFrameForTest drives the data-frame handler.
func (a *FeishuAdapter) HandleDataFrameForTest(ctx context.Context, conn *websocket.Conn, frame larkws.Frame) error {
	return a.handleDataFrame(ctx, conn, frame)
}

// SubmitDataFrameForTest drives the control-frame submit path.
func (a *FeishuAdapter) SubmitDataFrameForTest(ctx context.Context, conn *websocket.Conn, frame larkws.Frame, payload []byte) error {
	return a.submitDataFrame(ctx, conn, frame, payload)
}

// CombinePayloadForTest exposes the fragment reassembly helper.
func (a *FeishuAdapter) CombinePayloadForTest(msgID string, sum, seq int, chunk []byte) []byte {
	return a.combinePayload(msgID, sum, seq, chunk)
}

// CurrentPingIntervalForTest exposes the effective ping interval.
func (a *FeishuAdapter) CurrentPingIntervalForTest() time.Duration { return a.currentPingInterval() }

// ResponsesForTest exposes a generation's response queue.
func (g *feishuConnectionGeneration) ResponsesForTest() chan feishuResponseJob { return g.responses }

// ResponseWorkersForTest is the test-facing alias of the response worker count.
const ResponseWorkersForTest = feishuResponseWorkers

// NewResponseJobForTest builds a response job for gateway package tests.
func NewResponseJobForTest(ctx context.Context, cancel context.CancelFunc, resolve func(context.Context) (any, error)) feishuResponseJob {
	return feishuResponseJob{ctx: ctx, cancel: cancel, resolve: resolve}
}

// ResponseCapacityForTest is the test-facing alias of the response capacity.
const ResponseCapacityForTest = feishuResponseCapacity

// WriteBinaryForTest drives the raw websocket write path.
func (a *FeishuAdapter) WriteBinaryForTest(conn *websocket.Conn, data []byte) error {
	return a.writeBinary(conn, data)
}

// CardIngressResponseForTest builds the card action response envelope.
func (a *FeishuAdapter) CardIngressResponseForTest(event *larkcallback.CardActionTriggerEvent, result im.ReceiveResult) *larkcallback.CardActionTriggerResponse {
	return a.feishuCardIngressResponse(event, result)
}

// CardToastForTest builds a card toast response for gateway package tests.
func CardToastForTest(level, content string) *larkcallback.CardActionTriggerResponse {
	return feishuCardToast(level, content)
}

// Test-facing aliases of package constants and helpers used by the gateway
// package's regression tests.
const (
	MediaUploadTimeoutForTest = feishuMediaUploadTimeout
	MaxTextLenForTest         = feishuMaxTextLen
	QuestionSubmitNameForTest = FeishuQuestionSubmitName
)

// TextRetryAfterForTest is the test-facing alias of the retry-after policy.
var TextRetryAfterForTest = feishuTextRetryAfter

// SetLongConnForTest overrides the long-connection runner.
func (a *FeishuAdapter) SetLongConnForTest(run func(context.Context) error) { a.runLongConn = run }

// CardIngressResponseForTest builds the card response envelope without an
// adapter (the builder is receiver-independent).
func CardIngressResponseForTest(event *larkcallback.CardActionTriggerEvent, result im.ReceiveResult) *larkcallback.CardActionTriggerResponse {
	a := &FeishuAdapter{}
	return a.feishuCardIngressResponse(event, result)
}

// MessageIngressErrorForTest is the test-facing alias of the ingress error
// classifier.
var MessageIngressErrorForTest = feishuMessageIngressError

// Test-facing aliases of pure protocol helpers used by gateway package tests.
var (
	SourceTimeForTest       = feishuSourceTime
	HeaderTimeForTest       = feishuHeaderTime
	ChatKindForTest         = feishuChatKind
	CardVersionForTest      = feishuCardVersion
	ParseMessageTextForTest = parseFeishuMessageText
)

// TokenRefreshDoneForTest exposes a refresh's completion channel.
func (r *feishuTokenRefresh) TokenRefreshDoneForTest() <-chan struct{} { return r.done }

// SetLifecycleForTest installs a cancellable connection lifecycle.
func (a *FeishuAdapter) SetLifecycleForTest(ctx context.Context, cancel context.CancelFunc) {
	a.mu.Lock()
	a.ctx, a.cancel = ctx, cancel
	a.mu.Unlock()
}

// UploadImageForTest exposes the raw media upload for joint image tests.
func (a *FeishuAdapter) UploadImageForTest(ctx context.Context, source io.ReadSeeker, size int64, name, mimeType string, retried bool) (string, error) {
	return a.uploadImage(ctx, source, size, name, mimeType, retried)
}

// ImageUploadLimitForTest reports the platform's image upload byte limit.
func ImageUploadLimitForTest() int64 { return feishuImageUploadLimit }

// QuestionFormAnswersForTest exposes card option parsing for gateway tests.
func QuestionFormAnswersForTest(values map[string]any) ([]string, error) {
	return feishuQuestionFormAnswers(values)
}
