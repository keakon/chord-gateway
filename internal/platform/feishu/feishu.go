// Package main implements the Feishu IM adapter for chord-gateway.
package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/keakon/golog/log"

	"github.com/gorilla/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

const (
	feishuMaxTextLen       = 4000
	feishuDefaultPing      = 2 * time.Minute
	feishuDefaultReconnect = 2 * time.Minute
	feishuFragmentTTL      = 5 * time.Second
)

var OpenBaseURL = "https://open.feishu.cn"

// --- Feishu API types ---

// FeishuTokenResponse is the response from the Feishu tenant_access_token API.
type FeishuTokenResponse struct {
	Code              int    `json:"code"`
	Msg               string `json:"msg"`
	TenantAccessToken string `json:"tenant_access_token"`
	Expire            int    `json:"expire"`
}

// FeishuSendResponse is the response from the Feishu message send API.
type FeishuSendResponse struct {
	Code *int   `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		MessageID string `json:"message_id"`
	} `json:"data"`
}

// FeishuMessageContent is the parsed content of a text or post message.
type FeishuMessageContent struct {
	Text    string             `json:"text,omitempty"`
	Content [][]FeishuPostItem `json:"content,omitempty"`
}

type FeishuPostItem struct {
	Tag      string `json:"tag"`
	Text     string `json:"text"`
	UserName string `json:"user_name"`
}

type feishuFragmentBuffer struct {
	parts     [][]byte
	expiresAt time.Time
}

// --- Adapter ---

// Host is the gateway capability set the feishu transport depends on.
// *NotificationRouter satisfies it.
type Host interface {
	im.SessionLoginNotifier
	CurrentConfig() *config.Config
	IngressAvailable() bool
	PrepareIngressAccount(ctx context.Context, platform, account string) error
	PlatformReadySimple(platform, account string) error
	PlatformReady(platform, account string, presentation im.PlatformPresentation) error
	PlatformUnavailable(platform string)
	ObserveIngress(platform, event, reason string)
	SubmitIngress(ctx context.Context, envelope im.IngressEnvelope) (<-chan im.ReceiveResult, error)
	CaptureIngressEnvelope(envelope im.IngressEnvelope) im.IngressEnvelope
}

// FeishuAdapter implements im.IMAdapter for Feishu.
type FeishuAdapter struct {
	host     Host
	imCfg    config.IMAdapterConfig
	notifier im.SessionLoginNotifier // delivers async session/login notices

	// Outbound HTTP keeps receipt validation, error classification and media
	// deadlines under the adapter's control. Inbound events use shared ingress;
	// an unconfirmed outbound message is never replayed.
	httpClient *http.Client
	proxyFunc  func(*http.Request) (*url.URL, error)

	accessToken   string
	tokenExpireAt time.Time
	mu            sync.Mutex
	tokenRefresh  *feishuTokenRefresh
	textSender    *im.IMTextSender
	ctx           context.Context
	cancel        context.CancelFunc

	connMu     sync.Mutex
	conn       *websocket.Conn
	generation *feishuConnectionGeneration

	writeMu sync.Mutex

	fragmentsMu sync.Mutex
	fragments   map[string]feishuFragmentBuffer

	intervalMu        sync.RWMutex
	pingInterval      time.Duration
	reconnectInterval time.Duration
	runLongConn       func(context.Context) error

	// The gateway owns in-memory acceptance and dispatch. This adapter only owns
	// transport generations and bounded response workers.
	connectDone   chan struct{}
	stopped       bool
	activeAccount string
}

type feishuTokenRefresh struct {
	done     chan struct{}
	token    string
	expireAt time.Time
	err      error
}

// NewFeishuAdapter creates a new Feishu adapter.
func NewFeishuAdapter(cfg *config.Config, imCfg config.IMAdapterConfig, paths *config.Paths, router Host) (*FeishuAdapter, error) {
	if imCfg.Feishu == nil {
		return nil, fmt.Errorf("feishu: feishu config is required when im type is feishu")
	}
	proxyFunc, err := im.WebsocketProxyFuncFor(cfg, imCfg)
	if err != nil {
		return nil, err
	}

	a := &FeishuAdapter{
		host:              router,
		imCfg:             imCfg,
		notifier:          router, // Host satisfies im.SessionLoginNotifier
		httpClient:        &http.Client{Timeout: 10 * time.Second, Transport: im.ProxiedTransport(proxyFunc)},
		fragments:         make(map[string]feishuFragmentBuffer),
		pingInterval:      feishuDefaultPing,
		reconnectInterval: feishuDefaultReconnect,
		proxyFunc:         proxyFunc,
	}
	a.runLongConn = a.runLongConnection

	return a, nil
}

func (a *FeishuAdapter) Type() string { return "feishu" }

func (a *FeishuAdapter) SupportsLoginRenewal() bool { return false }

func (a *FeishuAdapter) feishuConfig() *config.FeishuConfig {
	if a.host != nil {
		if cfg := a.host.CurrentConfig(); cfg != nil {
			if imCfg := cfg.IMConfigByType("feishu"); imCfg != nil {
				return imCfg.Feishu
			}
		}
	}
	return a.imCfg.Feishu
}

func (a *FeishuAdapter) StartLogin() (string, error) {
	return "", im.ErrLoginNotSupported
}

// Connect validates credentials, locks and prepares the account, and supervises
// its long connection until Disconnect or a fatal error.
func (a *FeishuAdapter) Connect() error {
	if a.host == nil || !a.host.IngressAvailable() {
		return fmt.Errorf("feishu: in-memory ingress is required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	if a.stopped || a.connectDone != nil {
		a.mu.Unlock()
		cancel()
		return fmt.Errorf("feishu: adapter already started or stopped")
	}
	a.ctx, a.cancel = ctx, cancel
	done := make(chan struct{})
	a.connectDone = done
	a.mu.Unlock()
	var accountLocks *im.InstanceLocks
	defer close(done)
	defer func() {
		cancel()
		a.closeConn()
		a.mu.Lock()
		refresh := a.tokenRefresh
		a.mu.Unlock()
		if refresh != nil {
			<-refresh.done
		}
		if accountLocks != nil {
			accountLocks.Close()
		}
	}()
	fc := a.feishuConfig()
	if fc == nil || fc.AppID == "" {
		return fmt.Errorf("feishu: missing account identity")
	}
	account := fc.AppID
	a.mu.Lock()
	a.activeAccount = account
	a.mu.Unlock()
	if _, err := a.getAccessToken(ctx); err != nil {
		if a.notifier != nil {
			a.notifier.HandleSessionExpired("feishu")
		}
		return fmt.Errorf("feishu: initial access token: %w", err)
	}
	var err error
	accountLocks, err = im.AcquireIMAccountLock("feishu", account)
	if err != nil {
		return fmt.Errorf("feishu: account lock: %w", err)
	}
	if err := a.prepareIngressAccount(ctx, account); err != nil {
		return fmt.Errorf("feishu: prepare in-memory ingress: %w", err)
	}
	for {
		if fc := a.feishuConfig(); fc == nil || fc.AppID != account {
			return fmt.Errorf("feishu: account identity changed; restart required")
		}
		err := a.runLongConn(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			var clientErr *larkws.ClientError
			if errors.As(err, &clientErr) {
				return fmt.Errorf("feishu long connection: %w", err)
			}
			log.Warnf("feishu: long connection dropped, retrying error=%v retry_in=%v", err, a.currentReconnectInterval())
		}
		if !im.SleepCtx(ctx, a.currentReconnectInterval()) {
			return nil
		}
	}
}

// SendText sends a plain text message to the specified chat.
// Long messages are split at line boundaries to stay within Feishu's ~4000 char limit.
func (a *FeishuAdapter) SendText(chatID, text string) error {
	return a.SendTextContext(a.connectionContext(), chatID, text)
}
func (a *FeishuAdapter) SendTextContext(ctx context.Context, chatID, text string) error {
	sender, err := a.feishuTextSender()
	if err != nil {
		return err
	}
	// A flow reply carries a reply permit; it gets the control-send budget
	// like the other adapters, instead of the long proactive-output budget.
	control := ctx.Value(im.OutboundReplyContextKey{}) != nil
	return sender.Send(ctx, a.connectionContext(), nil, chatID, text, control)
}

// Disconnect closes acceptance and joins transport/response workers. The shared
// ingress database remains owned by the gateway.
func (a *FeishuAdapter) Disconnect() {
	a.mu.Lock()
	a.stopped = true
	cancel, done := a.cancel, a.connectDone
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	a.closeConn()
	if done != nil {
		<-done
	}
}

// --- Sending messages ---

// doFeishuJSONRequest sends a JSON request to a Feishu OpenAPI endpoint and
// decodes the JSON envelope. On 99991663 (access token expired) it clears the
// cached token and retries once.
func (a *FeishuAdapter) doFeishuJSONRequest(ctx context.Context, method, url string, reqBody any, result *FeishuSendResponse, retry bool) error {
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := im.MessageHTTPClient(a.httpClient).Do(req)
	if err != nil {
		return fmt.Errorf("%w: HTTP request: %w", im.ErrDeliveryOutcomeUnknown, err)
	}
	defer resp.Body.Close()

	// Only the shared text sender retries explicit rate-limit rejections. Card
	// and image outcomes keep their existing conservative classification.
	if resp.StatusCode == http.StatusTooManyRequests && isFeishuTextRequest(reqBody) {
		return &im.IMRateLimitError{RetryAfter: feishuTextRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	}
	if im.IsAmbiguousDeliveryStatus(resp.StatusCode) {
		return fmt.Errorf("%w: feishu HTTP %d", im.ErrDeliveryOutcomeUnknown, resp.StatusCode)
	}
	*result = FeishuSendResponse{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(result); err != nil {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("feishu HTTP %d: request rejected: %w", resp.StatusCode, err)
		}
		return fmt.Errorf("%w: decode response: %w", im.ErrDeliveryOutcomeUnknown, err)
	}
	// Ordinary 4xx responses can carry the same explicit expired-token code
	// as HTTP 200. Only a definite rejection permits replaying a message.
	if result.Code != nil && *result.Code == 99991663 && !retry {
		log.Warnf("feishu: access token expired, refreshing")
		a.invalidateAccessToken()
		resp.Body.Close()
		return a.doFeishuJSONRequest(ctx, method, url, reqBody, result, true)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("feishu HTTP %d: request rejected", resp.StatusCode)
	}

	if result.Code == nil {
		return fmt.Errorf("%w: feishu response is missing code", im.ErrDeliveryOutcomeUnknown)
	}
	if *result.Code != 0 {
		return fmt.Errorf("feishu API error: code=%d", *result.Code)
	}
	return nil
}

func (a *FeishuAdapter) sendMessageContext(ctx context.Context, chatID, msgType string, content any, retry bool) (*im.InteractiveCardHandle, error) {
	contentJSON, err := json.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("marshal message content: %w", err)
	}
	reqBody := map[string]string{
		"receive_id": chatID,
		"msg_type":   msgType,
		"content":    string(contentJSON),
	}
	url := OpenBaseURL + "/open-apis/im/v1/messages?receive_id_type=chat_id"
	var result FeishuSendResponse
	if err := a.doFeishuJSONRequest(ctx, http.MethodPost, url, reqBody, &result, retry); err != nil {
		return nil, err
	}
	if strings.TrimSpace(result.Data.MessageID) == "" {
		return nil, fmt.Errorf("%w: feishu send response is missing message_id", im.ErrDeliveryOutcomeUnknown)
	}
	return &im.InteractiveCardHandle{MessageID: result.Data.MessageID}, nil
}

func (a *FeishuAdapter) updateInteractiveMessageContext(ctx context.Context, messageID string, card map[string]any, retry bool) error {
	if strings.TrimSpace(messageID) == "" {
		return errors.New("feishu card update requires a message_id")
	}
	contentJSON, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("marshal interactive card content: %w", err)
	}
	reqBody := map[string]string{"content": string(contentJSON)}
	requestURL := OpenBaseURL + "/open-apis/im/v1/messages/" + url.PathEscape(messageID)
	var result FeishuSendResponse
	return a.doFeishuJSONRequest(ctx, http.MethodPatch, requestURL, reqBody, &result, retry)
}

var _ im.IMTextMetricsProvider = (*FeishuAdapter)(nil)

// SendInteractiveCard posts one interactive card and returns the created
// message handle (im.InteractiveCardAdapter).
func (a *FeishuAdapter) SendInteractiveCard(ctx context.Context, chatID string, card map[string]any) (*im.InteractiveCardHandle, error) {
	return a.sendMessageContext(ctx, chatID, "interactive", card, false)
}

// UpdateInteractiveCard patches an existing card message in place
// (im.InteractiveCardAdapter).
func (a *FeishuAdapter) UpdateInteractiveCard(ctx context.Context, messageID string, card map[string]any) error {
	return a.updateInteractiveMessageContext(ctx, messageID, card, false)
}

var _ im.InteractiveCardAdapter = (*FeishuAdapter)(nil)
