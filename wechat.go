// Package main implements the WeChat iLink Bot IM adapter for chord-gateway.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/config"
)

// iLink protocol constants
const (
	ilinkMessageTypeUser    = 1
	ilinkMessageTypeBot     = 2
	ilinkMessageStateFinish = 2
	ilinkItemTypeText       = 1

	ilinkLoginTimeout   = 5 * time.Minute
	ilinkMaxRetries     = 3
	ilinkBackoffDelay   = 30 * time.Second
	ilinkSessionExpired = -14

	ilinkMaxMessageLen  = 2048
	ilinkDefaultBaseURL = "https://ilinkai.weixin.qq.com"
	ilinkDefaultBotType = "3"
	ilinkChannelVersion = "1.0.2"
)

// Poll pacing is variable so tests can exercise the loops without real-time
// waits. Production keeps these values.
var (
	ilinkPollInterval = 1500 * time.Millisecond
	ilinkRetryDelay   = 2 * time.Second
)

// --- iLink JSON protocol types ---

// ilinkQRCodeResponse is the response from get_bot_qrcode.
type ilinkQRCodeResponse struct {
	QRCode           string `json:"qrcode"`
	QRCodeImgContent string `json:"qrcode_img_content"`
}

// ilinkQRCodeStatusResponse is the response from get_qrcode_status.
type ilinkQRCodeStatusResponse struct {
	Status      string `json:"status"`
	BotToken    string `json:"bot_token,omitempty"`
	BaseURL     string `json:"baseurl,omitempty"`
	ILinkBotID  string `json:"ilink_bot_id,omitempty"`
	ILinkUserID string `json:"ilink_user_id,omitempty"`
}

// ilinkGetUpdatesRequest is the request body for getupdates.
type ilinkGetUpdatesRequest struct {
	GetUpdatesBuf string        `json:"get_updates_buf"`
	BaseInfo      ilinkBaseInfo `json:"base_info"`
}

// ilinkBaseInfo is the base_info field.
type ilinkBaseInfo struct {
	ChannelVersion string `json:"channel_version"`
}

// ilinkGetUpdatesResponse is the response from getupdates.
type ilinkGetUpdatesResponse struct {
	Ret               int            `json:"ret,omitempty"`
	ErrCode           int            `json:"errcode,omitempty"`
	ErrMsg            string         `json:"errmsg,omitempty"`
	Msgs              []ilinkMessage `json:"msgs,omitempty"`
	GetUpdatesBuf     string         `json:"get_updates_buf,omitempty"`
	LongPollTimeoutMs int            `json:"longpolling_timeout_ms,omitempty"`
}

type ilinkGetUpdatesStatus int

const (
	ilinkGetUpdatesStatusOK ilinkGetUpdatesStatus = iota
	ilinkGetUpdatesStatusEmpty
	ilinkGetUpdatesStatusSessionExpired
	ilinkGetUpdatesStatusError
)

func (r *ilinkGetUpdatesResponse) statusKind() ilinkGetUpdatesStatus {
	if r == nil {
		return ilinkGetUpdatesStatusError
	}
	if r.ErrCode == ilinkSessionExpired || r.Ret == ilinkSessionExpired {
		return ilinkGetUpdatesStatusSessionExpired
	}
	isAPIError := (r.Ret != 0 && r.Ret != -1) || (r.ErrCode != 0 && r.ErrCode != -1)
	if isAPIError {
		return ilinkGetUpdatesStatusError
	}
	if r.Ret != 0 && r.ErrCode == 0 && len(r.Msgs) == 0 {
		return ilinkGetUpdatesStatusEmpty
	}
	return ilinkGetUpdatesStatusOK
}

// ilinkMessage represents a single WeChat iLink message.
type ilinkMessage struct {
	ingressCapture *ingressCapture `json:"-"` // control target frozen at polling receipt
	receivedAt     time.Time       `json:"-"` // first observation in its retained polling batch
	MessageID      json.Number     `json:"message_id,omitempty"`
	FromUserID     string          `json:"from_user_id,omitempty"`
	ToUserID       string          `json:"to_user_id,omitempty"`
	ClientID       string          `json:"client_id,omitempty"`
	MessageType    int             `json:"message_type,omitempty"`
	MessageState   int             `json:"message_state,omitempty"`
	ContextToken   string          `json:"context_token,omitempty"`
	ItemList       []ilinkItem     `json:"item_list,omitempty"`
}

// ilinkItem represents an item in a message.
type ilinkItem struct {
	MessageID string          `json:"msg_id,omitempty"`
	ImageItem *ilinkImageItem `json:"image_item,omitempty"`
	Type      int             `json:"type,omitempty"`
	TextItem  *ilinkTextItem  `json:"text_item,omitempty"`
}

// ilinkTextItem holds the text content.
type ilinkTextItem struct {
	Text string `json:"text,omitempty"`
}

// ilinkSendMessageRequest is the request body for sendmessage.
type ilinkSendMessageRequest struct {
	Msg ilinkMessage `json:"msg"`
}

// ilinkAPIResponse is a generic API response.
type ilinkAPIResponse struct {
	Ret     *int   `json:"ret,omitempty"`
	ErrCode *int   `json:"errcode,omitempty"`
	ErrMsg  string `json:"errmsg,omitempty"`
}

// TokenData holds the persisted iLink authentication token data.
type TokenData struct {
	Token     string `json:"token"`
	BaseURL   string `json:"baseUrl"`
	AccountID string `json:"accountId"`
	UserID    string `json:"userId"`
	SavedAt   string `json:"savedAt"`
}

// WechatAdapter implements IMAdapter for WeChat iLink Bot (personal WeChat).
type WechatAdapter struct {
	replyContexts  map[string]wechatReplyContext
	ingressPending map[string]*wechatIngressPending

	ingressReadinessNotifier

	imCfg         config.IMAdapterConfig
	msgRouter     MessageRouter
	notifier      SessionLoginNotifier
	token         atomic.Pointer[TokenData]
	syncAccount   string
	syncBuf       string // monitorLoop-only: written/read from a single goroutine
	mu            sync.Mutex
	connectMu     sync.Mutex
	pollCancel    context.CancelFunc // protected by mu
	pollToken     *TokenData         // protected by mu
	stopped       bool               // protected by mu; terminal polling scope
	ctx           context.Context
	cancel        context.CancelFunc
	storageDir    string
	accountLock   *instanceLocks // owned by the single polling supervisor
	accountLockID string
	tokenFile     string
	httpClient    *http.Client
	proxyFunc     func(*http.Request) (*url.URL, error)
}

// NewWechatAdapter creates a new WeChat iLink adapter.
func NewWechatAdapter(cfg *config.Config, imCfg config.IMAdapterConfig, paths *config.Paths, router *NotificationRouter) (*WechatAdapter, error) {
	storageDir := filepath.Join(paths.StateDir, "wechat")
	tokenFile := filepath.Join(storageDir, "token.json")
	if imCfg.Wechat != nil && imCfg.Wechat.TokenPath != "" {
		tokenFile = imCfg.Wechat.TokenPath
	}
	proxyFunc, err := proxyFuncFor(cfg, imCfg)
	if err != nil {
		return nil, err
	}

	a := &WechatAdapter{
		imCfg:      imCfg,
		msgRouter:  router,
		notifier:   router,
		storageDir: storageDir,
		tokenFile:  tokenFile,
		httpClient: &http.Client{Timeout: 40 * time.Second, Transport: proxiedTransport(proxyFunc)},
		proxyFunc:  proxyFunc,
	}

	// Try to load existing token.
	if token := a.loadToken(); token != nil {
		a.publishToken(token)
		log.Infof("wechat ilink: loaded saved token account_id=%v", token.AccountID)
	}

	return a, nil
}

func (a *WechatAdapter) Type() string { return "wechat" }

func (a *WechatAdapter) SupportsLoginRenewal() bool { return true }

// StartLogin initiates a WeChat iLink QR login flow and returns the QR URL.
// The user should open this URL on their phone and scan the QR code in WeChat.
// A background goroutine polls for scan confirmation and auto-replaces the
// token on success.
func (a *WechatAdapter) StartLogin() (string, error) {
	qr, err := a.getBotQRCode()
	if err != nil {
		return "", fmt.Errorf("get QR code: %w", err)
	}
	go a.pollQRStatusForRelogin(a.adapterContext(), qr.QRCode)
	return qr.QRCodeImgContent, nil
}

// pollQRStatusForRelogin polls the QR code status until the user scans it.
// On success it replaces the current token and notifies the router.
func (a *WechatAdapter) pollQRStatusForRelogin(ctx context.Context, qrcodeID string) {
	deadline := time.Now().Add(ilinkLoginTimeout)
	for time.Now().Before(deadline) {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
		resp, err := a.getQRCodeStatus(qrcodeID)
		if err != nil {
			if ctx != nil {
				sleepCtx(ctx, ilinkPollInterval)
			} else {
				time.Sleep(ilinkPollInterval)
			}
			continue
		}
		switch resp.Status {
		case "confirmed":
			a.publishToken(&TokenData{
				Token:     resp.BotToken,
				BaseURL:   resp.BaseURL,
				AccountID: resp.ILinkBotID,
				UserID:    resp.ILinkUserID,
				SavedAt:   time.Now().Format(time.RFC3339),
			})
			a.saveToken()
			log.Infof("wechat ilink: re-login successful via QR scan")
			if a.notifier != nil {
				a.notifier.HandleLoginResult("wechat", true, "")
			}
			return
		case "expired":
			log.Warnf("wechat ilink: QR code expired during re-login")
			if a.notifier != nil {
				a.notifier.HandleLoginResult("wechat", false, "QR code expired")
			}
			return
		}
		if ctx != nil {
			sleepCtx(ctx, ilinkPollInterval)
		} else {
			time.Sleep(ilinkPollInterval)
		}
	}
	log.Warnf("wechat ilink: re-login polling timed out")
	if a.notifier != nil {
		a.notifier.HandleLoginResult("wechat", false, "login timeout")
	}
}

// Connect starts the adapter and blocks until Disconnect is called or a fatal
// error occurs. Missing credentials start QR login.
func (a *WechatAdapter) Connect() error {
	if !a.connectMu.TryLock() {
		return fmt.Errorf("wechat polling supervisor is already running")
	}
	defer a.connectMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		// Stop delivery before releasing the cross-instance account lock. The
		// shared dispatcher can remain alive for other IM adapters after this
		// supervisor exits, including on a fatal polling/checkpoint error.
		a.mu.Lock()
		a.stopped = true
		a.replyContexts = nil
		a.ingressPending = nil
		if a.pollCancel != nil {
			a.pollCancel()
			a.pollCancel, a.pollToken = nil, nil
		}
		a.mu.Unlock()
		cancel()
		if r := a.pairingRouter(); r != nil && r.admission != nil {
			r.admission.pairing.PlatformUnavailable("wechat")
		}
		a.closeIngressAccount()
	}()
	a.mu.Lock()
	a.stopped = false
	a.ctx = ctx
	a.cancel = cancel
	a.mu.Unlock()

	return a.connectILink(ctx)
}

// connectILink performs QR login (if needed) and starts the long-poll monitor loop.
func (a *WechatAdapter) connectILink(ctx context.Context) error {
	if a.token.Load() == nil {
		if err := a.login(ctx); err != nil {
			return fmt.Errorf("wechat ilink login: %w", err)
		}
	}

	return a.monitorLoop(ctx)
}

// SendText sends a plain text message to the specified chat (user ID).
// It posts to the iLink sendmessage API.
func (a *WechatAdapter) SendText(chatID, text string) error {
	return a.SendTextContext(context.Background(), chatID, text)
}

// Disconnect signals the adapter to stop.
func (a *WechatAdapter) Disconnect() {
	a.mu.Lock()
	cancel := a.cancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *WechatAdapter) adapterContext() context.Context {
	a.mu.Lock()
	ctx := a.ctx
	a.mu.Unlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// --- Helpers ---

func (a *WechatAdapter) baseURL() string {
	if a.imCfg.Wechat != nil && a.imCfg.Wechat.BaseURL != "" {
		return strings.TrimRight(a.imCfg.Wechat.BaseURL, "/")
	}
	if tok := a.token.Load(); tok != nil && tok.BaseURL != "" {
		return strings.TrimRight(tok.BaseURL, "/")
	}

	return ilinkDefaultBaseURL
}

func (a *WechatAdapter) botType() string {
	// iLink API: get_bot_qrcode requires bot_type=3.
	// Public protocol references describe it as a fixed constant, so we do not expose it in config.
	return ilinkDefaultBotType
}

func (a *WechatAdapter) tokenString() string {
	tok := a.token.Load()
	if tok == nil {
		return ""
	}
	return tok.Token
}

// randomWechatUIN generates a random X-WECHAT-UIN header value.
// Mirrors the iLink fingerprint format used by the official WeChat client:
// 4 random bytes -> uint32 -> decimal string -> base64. Do not change without
// confirming compatibility with the iLink server-side validation.
func randomWechatUIN() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		log.Warnf("wechat ilink: failed to generate random uin bytes, using time-based fallback error=%v", err)
		return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	}
	val := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%d", val)))
}

func (a *WechatAdapter) buildHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("AuthorizationType", "ilink_bot_token")
	h.Set("X-WECHAT-UIN", randomWechatUIN())
	if tok := a.tokenString(); tok != "" {
		h.Set("Authorization", "Bearer "+tok)
	}
	return h
}

// --- iLink HTTP API ---

func (a *WechatAdapter) apiGet(path string) (*http.Response, error) {
	baseURL := a.baseURL()
	if baseURL == "" {
		return nil, fmt.Errorf("no base URL configured")
	}
	url := baseURL + "/" + path
	req, err := http.NewRequestWithContext(a.adapterContext(), http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header = a.buildHeaders()
	return a.httpClient.Do(req)
}

func (a *WechatAdapter) apiPost(path string, body any) (*http.Response, error) {
	return a.apiPostSnapshot(a.adapterContext(), a.token.Load(), path, body)
}

// --- QR Login ---

func (a *WechatAdapter) login(ctx context.Context) error {
	baseURL := ""
	if a.imCfg.Wechat != nil {
		baseURL = a.imCfg.Wechat.BaseURL
	}
	if baseURL == "" {
		baseURL = ilinkDefaultBaseURL
	}

	log.Infof("wechat ilink: starting QR login flow")

	// Step 1: Get QR code.
	qrResp, err := a.getBotQRCode()
	if err != nil {
		return fmt.Errorf("get bot qrcode: %w", err)
	}

	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("  WeChat iLink Bot Login")
	fmt.Println("========================================")
	fmt.Println("Please scan this QR code with WeChat:")
	fmt.Printf("  %s\n", qrResp.QRCodeImgContent)
	fmt.Println("========================================")
	fmt.Println()

	// Step 2: Poll QR code status.
	deadline := time.Now().Add(ilinkLoginTimeout)
	currentQRCode := qrResp.QRCode
	refreshCount := 0

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		statusResp, err := a.getQRCodeStatus(currentQRCode)
		if err != nil {
			log.Warnf("wechat ilink: polling QR status failed error=%v", err)
			time.Sleep(ilinkPollInterval)
			continue
		}

		switch statusResp.Status {
		case "wait":
			// Still waiting, poll again.
		case "scaned":
			log.Infof("wechat ilink: QR code scanned, please confirm in WeChat...")
		case "expired":
			refreshCount++
			if refreshCount > 3 {
				return fmt.Errorf("QR code expired multiple times, please retry")
			}
			log.Warnf("wechat ilink: QR expired, refreshing attempt=%v", refreshCount)
			newQR, err := a.getBotQRCode()
			if err != nil {
				return fmt.Errorf("refresh QR code: %w", err)
			}
			currentQRCode = newQR.QRCode
			fmt.Println("New QR code (scan again):")
			fmt.Printf("  %s\n", newQR.QRCodeImgContent)
		case "confirmed":
			baseURL, err := a.trustedLoginBaseURL(statusResp.BaseURL)
			if err != nil {
				return err
			}
			log.Infof("wechat ilink: login successful!")
			tok := &TokenData{
				Token:     statusResp.BotToken,
				BaseURL:   baseURL,
				AccountID: statusResp.ILinkBotID,
				UserID:    statusResp.ILinkUserID,
				SavedAt:   time.Now().Format(time.RFC3339),
			}
			a.publishToken(tok)
			a.saveToken()
			log.Infof("wechat ilink: token saved account_id=%v", tok.AccountID)
			return nil
		}

		time.Sleep(ilinkPollInterval)
	}

	return fmt.Errorf("login timeout (%v)", ilinkLoginTimeout)
}

func (a *WechatAdapter) trustedLoginBaseURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("wechat ilink: invalid login base URL")
	}
	configured := ""
	if a.imCfg.Wechat != nil {
		configured = strings.TrimRight(strings.TrimSpace(a.imCfg.Wechat.BaseURL), "/")
	}
	if configured != "" {
		cu, parseErr := url.Parse(configured)
		if parseErr != nil || !strings.EqualFold(cu.Scheme, u.Scheme) || !strings.EqualFold(cu.Host, u.Host) {
			return "", fmt.Errorf("wechat ilink: login base URL does not match configured endpoint")
		}
		return raw, nil
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("wechat ilink: login base URL must use https")
	}
	return raw, nil
}

func (a *WechatAdapter) getBotQRCode() (*ilinkQRCodeResponse, error) {
	path := "ilink/bot/get_bot_qrcode?bot_type=" + a.botType()
	resp, err := a.apiGet(path)
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var result ilinkQRCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

func (a *WechatAdapter) getQRCodeStatus(qrcode string) (*ilinkQRCodeStatusResponse, error) {
	path := "ilink/bot/get_qrcode_status?qrcode=" + qrcode
	resp, err := a.apiGet(path)
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var result ilinkQRCodeStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

// --- Long-poll Monitor ---

// notifySessionExpired logs the session expiry and notifies the router so
// other IM adapters can inform the user. The WeChat session is expired so we
// cannot notify through WeChat itself.
func (a *WechatAdapter) notifySessionExpired() {
	log.Warnf("wechat ilink: session expired — notifying via other channels")
	if a.notifier != nil {
		a.notifier.HandleSessionExpired("wechat")
	}
}

// --- Sending Messages ---

func (a *WechatAdapter) sendMessage(ctx context.Context, account *TokenData, body ilinkSendMessageRequest) error {
	resp, err := a.apiPostSnapshot(ctx, account, "ilink/bot/sendmessage", body)
	return checkILinkSendResponse(resp, err)
}

// Text and image sends share the same outcome rules.
// Only an explicit rejection proves the platform did not accept the message.
func checkILinkSendResponse(resp *http.Response, err error) error {
	if err != nil {
		return fmt.Errorf("%w: HTTP request: %w", errDeliveryOutcomeUnknown, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if !isAmbiguousDeliveryStatus(resp.StatusCode) {
			return fmt.Errorf("wechat ilink send rejected: HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("%w: HTTP %d", errDeliveryOutcomeUnknown, resp.StatusCode)
	}

	var result ilinkAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("%w: decode send response: %w", errDeliveryOutcomeUnknown, err)
	}

	if result.ErrCode == nil && result.Ret == nil {
		return fmt.Errorf("%w: send response is missing status", errDeliveryOutcomeUnknown)
	}
	ret := 0
	if result.Ret != nil {
		ret = *result.Ret
	}
	code := 0
	if result.ErrCode != nil {
		code = *result.ErrCode
	}
	if code != 0 || ret != 0 {
		if code == ilinkSessionExpired || ret == ilinkSessionExpired {
			log.Warnf("wechat ilink: send failed with session expired")
		}
		return fmt.Errorf("send error: ret=%d errcode=%d", ret, code)
	}

	return nil
}

// --- Token Persistence ---

func (a *WechatAdapter) loadToken() *TokenData {
	data, err := os.ReadFile(a.tokenFile)
	if err != nil {
		return nil
	}
	var token TokenData
	if err := json.Unmarshal(data, &token); err != nil {
		log.Warnf("wechat ilink: failed to parse token file path=%v error=%v", a.tokenFile, err)
		return nil
	}
	return &token
}

func (a *WechatAdapter) clearToken() {
	a.publishToken(nil)
	if err := os.Remove(a.tokenFile); err != nil && !os.IsNotExist(err) {
		log.Warnf("wechat ilink: failed to remove token file error=%v", err)
	}
}

func (a *WechatAdapter) saveToken() {
	tok := a.token.Load()
	if tok == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(a.tokenFile), 0o700); err != nil {
		log.Errorf("wechat ilink: failed to create token dir error=%v", err)
		return
	}
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		log.Errorf("wechat ilink: failed to marshal token error=%v", err)
		return
	}
	if err := writePrivateFileAtomically(a.tokenFile, data); err != nil {
		log.Errorf("wechat ilink: failed to save token error=%v", err)
	}
}

// --- Utilities ---

// splitText splits text into segments of at most maxLen runes, preferring to
// break at newlines. Operates on runes so multi-byte UTF-8 sequences (Chinese
// characters, emoji) are never split mid-character.
func splitText(text string, maxLen int) []string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		return []string{text}
	}

	var segments []string
	start := 0
	for start < len(runes) {
		end := start + maxLen
		if end >= len(runes) {
			segments = append(segments, string(runes[start:]))
			break
		}
		// Prefer to break at the last newline within [start, end).
		breakAt := end
		for i := end - 1; i > start; i-- {
			if runes[i] == '\n' {
				breakAt = i
				break
			}
		}
		segments = append(segments, string(runes[start:breakAt]))
		start = breakAt
		// Skip the newline we broke at.
		if start < len(runes) && runes[start] == '\n' {
			start++
		}
	}

	return segments
}
