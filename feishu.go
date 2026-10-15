// Package main implements the Feishu IM adapter for chord-gateway.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/keakon/golog/log"

	"github.com/gorilla/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/keakon/chord-gateway/config"
)

const (
	feishuMaxTextLen       = 4000
	feishuDefaultPing      = 2 * time.Minute
	feishuDefaultReconnect = 2 * time.Minute
	feishuFragmentTTL      = 5 * time.Second
)

var feishuOpenBaseURL = "https://open.feishu.cn"

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

// FeishuAdapter implements IMAdapter for Feishu.
type FeishuAdapter struct {
	imCfg     config.IMAdapterConfig
	msgRouter MessageRouter        // dispatches incoming messages to the router
	notifier  SessionLoginNotifier // delivers async session/login notices

	// Outbound HTTP keeps receipt validation, error classification and media
	// deadlines under the adapter's control. Inbound events use shared ingress;
	// an unconfirmed outbound message is never replayed.
	httpClient *http.Client
	proxyFunc  func(*http.Request) (*url.URL, error)

	accessToken   string
	tokenExpireAt time.Time
	mu            sync.Mutex
	tokenRefresh  *feishuTokenRefresh
	textSender    *IMTextSender
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
func NewFeishuAdapter(cfg *config.Config, imCfg config.IMAdapterConfig, paths *config.Paths, router *NotificationRouter) (*FeishuAdapter, error) {
	if imCfg.Feishu == nil {
		return nil, fmt.Errorf("feishu: feishu config is required when im type is feishu")
	}
	proxyFunc, err := websocketProxyFuncFor(cfg, imCfg)
	if err != nil {
		return nil, err
	}

	a := &FeishuAdapter{
		imCfg:             imCfg,
		msgRouter:         router, // NotificationRouter satisfies MessageRouter
		notifier:          router, // ... and SessionLoginNotifier
		httpClient:        &http.Client{Timeout: 10 * time.Second, Transport: proxiedTransport(proxyFunc)},
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
	if r, ok := a.msgRouter.(*NotificationRouter); ok && r.mgr != nil && r.mgr.store != nil {
		if im := r.getConfig().IMConfigByType("feishu"); im != nil {
			return im.Feishu
		}
	}
	return a.imCfg.Feishu
}

func (a *FeishuAdapter) StartLogin() (string, error) {
	return "", ErrLoginNotSupported
}

// Connect validates credentials, locks and prepares the account, and supervises
// its long connection until Disconnect or a fatal error.
func (a *FeishuAdapter) Connect() error {
	r, ok := a.msgRouter.(*NotificationRouter)
	if !ok || r.ingress == nil || !r.ingress.Available() {
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
	var accountLocks *instanceLocks
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
	accountLocks, err = AcquireIMAccountLock("feishu", account)
	if err != nil {
		return fmt.Errorf("feishu: account lock: %w", err)
	}
	if err := a.prepareIngressAccount(ctx, r, account); err != nil {
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
		if !sleepCtx(ctx, a.currentReconnectInterval()) {
			return nil
		}
	}
}

func (a *FeishuAdapter) runLongConnection(ctx context.Context) error {
	connURL, err := a.getLongConnURL(ctx)
	if err != nil {
		return err
	}

	u, err := url.Parse(connURL)
	if err != nil {
		return fmt.Errorf("feishu long connection parse URL: %w", err)
	}
	serviceID, _ := strconv.ParseInt(u.Query().Get(larkws.ServiceID), 10, 32)

	dialer := *websocket.DefaultDialer
	dialer.Proxy = websocketProxyFunc(a.proxyFunc)
	conn, resp, err := dialer.DialContext(ctx, connURL, nil)
	if err != nil {
		if resp != nil {
			return parseFeishuWSError(resp)
		}
		return fmt.Errorf("feishu long connection dial failed")
	}
	defer conn.Close()
	conn.SetReadLimit(1 << 20)

	generation := a.startGeneration(ctx, conn)
	defer a.clearConn(conn)
	defer generation.close()
	if generation.ctx.Err() != nil {
		return nil
	}

	log.Infof("feishu adapter: long connection established")
	if r, ok := a.msgRouter.(*NotificationRouter); ok && r.admission != nil {
		fc := a.feishuConfig()
		if fc == nil || !a.matchesActiveAccount(fc.AppID) {
			return fmt.Errorf("feishu: account identity changed during connection")
		}
		if err := r.admission.pairing.PlatformReady("feishu", fc.AppID); err != nil {
			return err
		}
		defer r.admission.pairing.PlatformUnavailable("feishu")
	}

	pingErrCh := make(chan error, 1)
	generation.wg.Go(func() { a.pingLoop(generation.ctx, conn, int32(serviceID), pingErrCh) })

	for {
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			select {
			case pingErr := <-pingErrCh:
				if pingErr != nil {
					return pingErr
				}
			default:
			}
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("feishu long connection read: %w", err)
		}
		if mt != websocket.BinaryMessage {
			log.Warnf("feishu: ignoring non-binary websocket message message_type=%v", mt)
			continue
		}
		if err := a.handleWSFrame(generation.ctx, conn, msg); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

func (a *FeishuAdapter) getLongConnURL(ctx context.Context) (string, error) {
	fc := a.feishuConfig()
	if fc == nil || !a.matchesActiveAccount(fc.AppID) {
		return "", fmt.Errorf("feishu: account config missing or changed")
	}
	reqBody := map[string]string{
		"AppID":     fc.AppID,
		"AppSecret": fc.AppSecret,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("feishu long connection marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, feishuOpenBaseURL+larkws.GenEndpointUri, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("feishu long connection create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("locale", "zh")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("feishu long connection HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", larkws.NewServerError(resp.StatusCode, "system busy")
	}

	var endpointResp larkws.EndpointResp
	if err := json.NewDecoder(resp.Body).Decode(&endpointResp); err != nil {
		return "", fmt.Errorf("feishu long connection decode endpoint response: %w", err)
	}

	switch endpointResp.Code {
	case larkws.OK:
	case larkws.SystemBusy:
		return "", larkws.NewServerError(endpointResp.Code, "system busy")
	case larkws.InternalError:
		return "", larkws.NewServerError(endpointResp.Code, "endpoint request failed")
	default:
		return "", larkws.NewClientError(endpointResp.Code, "endpoint request rejected")
	}

	if endpointResp.Data == nil || endpointResp.Data.Url == "" {
		return "", larkws.NewServerError(http.StatusInternalServerError, "endpoint is null")
	}
	if endpointResp.Data.ClientConfig != nil {
		a.applyWSClientConfig(endpointResp.Data.ClientConfig)
	}
	return endpointResp.Data.Url, nil
}

func (a *FeishuAdapter) applyWSClientConfig(conf *larkws.ClientConfig) {
	if conf == nil {
		return
	}
	a.intervalMu.Lock()
	defer a.intervalMu.Unlock()
	if conf.PingInterval > 0 {
		a.pingInterval = time.Duration(conf.PingInterval) * time.Second
	}
	if conf.ReconnectInterval > 0 {
		a.reconnectInterval = time.Duration(conf.ReconnectInterval) * time.Second
	}
}

func (a *FeishuAdapter) currentPingInterval() time.Duration {
	a.intervalMu.RLock()
	defer a.intervalMu.RUnlock()
	return a.pingInterval
}

func (a *FeishuAdapter) currentReconnectInterval() time.Duration {
	a.intervalMu.RLock()
	defer a.intervalMu.RUnlock()
	return a.reconnectInterval
}

func parseFeishuWSError(resp *http.Response) error {
	code, _ := strconv.Atoi(resp.Header.Get(larkws.HeaderHandshakeStatus))
	msg := "connection handshake rejected"
	switch code {
	case larkws.AuthFailed:
		authCode, _ := strconv.Atoi(resp.Header.Get(larkws.HeaderHandshakeAuthErrCode))
		if authCode == larkws.ExceedConnLimit {
			return larkws.NewClientError(code, msg)
		}
		return larkws.NewServerError(code, msg)
	case larkws.Forbidden:
		return larkws.NewClientError(code, msg)
	default:
		if code != 0 {
			return larkws.NewServerError(code, msg)
		}
		return larkws.NewServerError(resp.StatusCode, resp.Status)
	}
}

func (a *FeishuAdapter) pingLoop(ctx context.Context, conn *websocket.Conn, serviceID int32, errCh chan<- error) {
	for {
		interval := a.currentPingInterval()
		if interval <= 0 {
			interval = feishuDefaultPing
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		frame := larkws.NewPingFrame(serviceID)
		bs, err := frame.Marshal()
		if err != nil {
			continue
		}
		if err := a.writeBinary(conn, bs); err != nil {
			select {
			case errCh <- fmt.Errorf("feishu long connection ping: %w", err):
			default:
			}
			_ = conn.Close()
			return
		}
	}
}

func (a *FeishuAdapter) handleWSFrame(ctx context.Context, conn *websocket.Conn, msg []byte) error {
	var frame larkws.Frame
	if err := frame.Unmarshal(msg); err != nil {
		return fmt.Errorf("feishu long connection unmarshal frame: %w", err)
	}

	switch larkws.FrameType(frame.Method) {
	case larkws.FrameTypeControl:
		a.handleControlFrame(frame)
		return nil
	case larkws.FrameTypeData:
		return a.handleDataFrame(ctx, conn, frame)
	default:
		return nil
	}
}

func (a *FeishuAdapter) handleControlFrame(frame larkws.Frame) {
	hs := larkws.Headers(frame.Headers)
	switch larkws.MessageType(hs.GetString(larkws.HeaderType)) {
	case larkws.MessageTypePong:
		if len(frame.Payload) == 0 {
			return
		}
		var conf larkws.ClientConfig
		if err := json.Unmarshal(frame.Payload, &conf); err != nil {
			log.Warnf("feishu: unmarshal client config failed error=%v", err)
			return
		}
		a.applyWSClientConfig(&conf)
	}
}

func (a *FeishuAdapter) handleDataFrame(ctx context.Context, conn *websocket.Conn, frame larkws.Frame) error {
	hs := larkws.Headers(frame.Headers)
	sum := hs.GetInt(larkws.HeaderSum)
	seq := hs.GetInt(larkws.HeaderSeq)
	msgID := hs.GetString(larkws.HeaderMessageID)
	_ = hs.GetString(larkws.HeaderTraceID)
	msgType := hs.GetString(larkws.HeaderType)

	payload := frame.Payload
	if sum > 1 {
		payload = a.combinePayload(msgID, sum, seq, payload)
		if payload == nil {
			return nil
		}
	}

	switch larkws.MessageType(msgType) {
	case larkws.MessageTypeEvent, larkws.MessageTypeCard:
		return a.submitDataFrame(ctx, conn, frame, payload)
	default:
		return nil
	}
}

func (a *FeishuAdapter) combinePayload(msgID string, sum, seq int, chunk []byte) []byte {
	if msgID == "" || sum < 1 || sum > 128 || seq < 0 || seq >= sum || len(chunk) > 1<<20 {
		return nil
	}
	now := time.Now()
	a.fragmentsMu.Lock()
	defer a.fragmentsMu.Unlock()

	for key, buf := range a.fragments {
		if now.After(buf.expiresAt) {
			delete(a.fragments, key)
		}
	}

	buf, ok := a.fragments[msgID]
	if !ok || len(buf.parts) != sum {
		if !ok && len(a.fragments) >= 128 {
			return nil
		}
		buf = feishuFragmentBuffer{
			parts:     make([][]byte, sum),
			expiresAt: now.Add(feishuFragmentTTL),
		}
	}
	// Count incomplete buffers too: waiting for a missing part must not let
	// retained chunks exceed the complete-message budget.
	total := len(chunk)
	for index, part := range buf.parts {
		if index != seq {
			total += len(part)
		}
	}
	if total > 1<<20 {
		delete(a.fragments, msgID)
		return nil
	}
	buf.parts[seq] = chunk
	buf.expiresAt = now.Add(feishuFragmentTTL)
	a.fragments[msgID] = buf

	for _, part := range buf.parts {
		if len(part) == 0 {
			return nil
		}
	}

	payload := make([]byte, 0, total)
	for _, part := range buf.parts {
		payload = append(payload, part...)
	}
	delete(a.fragments, msgID)
	return payload
}

func (a *FeishuAdapter) writeBinary(conn *websocket.Conn, data []byte) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	a.connMu.Lock()
	stale := a.conn != nil && a.conn != conn
	a.connMu.Unlock()
	if stale {
		return fmt.Errorf("feishu: stale connection generation")
	}
	if err := conn.SetWriteDeadline(time.Now().Add(feishuWriteTimeout)); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.BinaryMessage, data)
}

func (a *FeishuAdapter) clearConn(conn *websocket.Conn) {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	if conn == nil || a.conn == conn {
		a.conn = nil
		a.generation = nil
	}
}

func (a *FeishuAdapter) closeConn() {
	a.connMu.Lock()
	conn, generation := a.conn, a.generation
	a.connMu.Unlock()
	if generation != nil {
		generation.cancel()
	}
	if conn != nil {
		_ = conn.Close()
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
	return sender.Send(ctx, a.connectionContext(), nil, chatID, text, false)
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

// --- Access token management ---

func (a *FeishuAdapter) invalidateAccessToken() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.accessToken = ""
	a.tokenExpireAt = time.Time{}
}

func (a *FeishuAdapter) connectionContext() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

func (a *FeishuAdapter) matchesActiveAccount(account string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.stopped && (a.activeAccount == "" || a.activeAccount == account)
}

func (a *FeishuAdapter) getAccessToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fc := a.feishuConfig()
	if fc == nil || !a.matchesActiveAccount(fc.AppID) {
		return "", fmt.Errorf("feishu: account config missing or changed")
	}
	a.mu.Lock()
	if a.accessToken != "" && time.Now().Before(a.tokenExpireAt) {
		token := a.accessToken
		a.mu.Unlock()
		return token, nil
	}
	refresh := a.tokenRefresh
	if refresh == nil {
		refresh = &feishuTokenRefresh{done: make(chan struct{})}
		a.tokenRefresh = refresh
		parent := a.ctx
		if parent == nil {
			parent = context.Background()
		}
		// A caller can stop waiting without cancelling the refresh shared by
		// other callers. Shutdown and a short deadline bound the refresh itself.
		go func() {
			refreshCtx, cancel := context.WithTimeout(parent, 10*time.Second)
			defer cancel()
			refresh.token, refresh.expireAt, refresh.err = a.fetchAccessToken(refreshCtx)
			a.mu.Lock()
			if refresh.err == nil {
				a.accessToken = refresh.token
				a.tokenExpireAt = refresh.expireAt
			}
			close(refresh.done)
			a.tokenRefresh = nil
			a.mu.Unlock()
		}()
	}
	a.mu.Unlock()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-refresh.done:
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return refresh.token, refresh.err
	}
}

func (a *FeishuAdapter) fetchAccessToken(ctx context.Context) (string, time.Time, error) {
	fc := a.feishuConfig()
	if fc == nil || !a.matchesActiveAccount(fc.AppID) {
		return "", time.Time{}, fmt.Errorf("feishu: account config missing or changed")
	}
	body, err := json.Marshal(map[string]string{
		"app_id":     fc.AppID,
		"app_secret": fc.AppSecret,
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("marshal feishu token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		feishuOpenBaseURL+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create feishu token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("feishu token HTTP request: %w", err)
	}
	defer resp.Body.Close()

	var result FeishuTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", time.Time{}, fmt.Errorf("feishu decode token response: %w", err)
	}

	if result.Code != 0 {
		return "", time.Time{}, fmt.Errorf("feishu token error: code=%d", result.Code)
	}

	// Refresh 5 minutes before actual expiry.
	expiry := result.Expire - 300
	if expiry <= 0 {
		expiry = result.Expire
	}
	expireAt := time.Now().Add(time.Duration(expiry) * time.Second)

	log.Infof("feishu: access token refreshed expire=%v", result.Expire)
	return result.TenantAccessToken, expireAt, nil
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

	resp, err := messageHTTPClient(a.httpClient).Do(req)
	if err != nil {
		return fmt.Errorf("%w: HTTP request: %w", errDeliveryOutcomeUnknown, err)
	}
	defer resp.Body.Close()

	// Only the shared text sender retries explicit rate-limit rejections. Card
	// and image outcomes keep their existing conservative classification.
	if resp.StatusCode == http.StatusTooManyRequests && isFeishuTextRequest(reqBody) {
		return &IMRateLimitError{RetryAfter: feishuTextRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	}
	if isAmbiguousDeliveryStatus(resp.StatusCode) {
		return fmt.Errorf("%w: feishu HTTP %d", errDeliveryOutcomeUnknown, resp.StatusCode)
	}
	*result = FeishuSendResponse{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(result); err != nil {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("feishu HTTP %d: request rejected: %w", resp.StatusCode, err)
		}
		return fmt.Errorf("%w: decode response: %w", errDeliveryOutcomeUnknown, err)
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
		return fmt.Errorf("%w: feishu response is missing code", errDeliveryOutcomeUnknown)
	}
	if *result.Code != 0 {
		return fmt.Errorf("feishu API error: code=%d", *result.Code)
	}
	return nil
}

func (a *FeishuAdapter) sendMessageContext(ctx context.Context, chatID, msgType string, content any, retry bool) (*InteractiveCardHandle, error) {
	contentJSON, err := json.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("marshal message content: %w", err)
	}
	reqBody := map[string]string{
		"receive_id": chatID,
		"msg_type":   msgType,
		"content":    string(contentJSON),
	}
	url := feishuOpenBaseURL + "/open-apis/im/v1/messages?receive_id_type=chat_id"
	var result FeishuSendResponse
	if err := a.doFeishuJSONRequest(ctx, http.MethodPost, url, reqBody, &result, retry); err != nil {
		return nil, err
	}
	if strings.TrimSpace(result.Data.MessageID) == "" {
		return nil, fmt.Errorf("%w: feishu send response is missing message_id", errDeliveryOutcomeUnknown)
	}
	return &InteractiveCardHandle{MessageID: result.Data.MessageID}, nil
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
	requestURL := feishuOpenBaseURL + "/open-apis/im/v1/messages/" + url.PathEscape(messageID)
	var result FeishuSendResponse
	return a.doFeishuJSONRequest(ctx, http.MethodPatch, requestURL, reqBody, &result, retry)
}

var _ IMTextMetricsProvider = (*FeishuAdapter)(nil)
