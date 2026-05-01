// Package main implements the WeChat iLink Bot IM adapter for chord-gateway.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/keakon/chord-gateway/config"
)

// iLink protocol constants
const (
	ilinkMessageTypeUser    = 1
	ilinkMessageTypeBot     = 2
	ilinkMessageStateNew    = 0
	ilinkMessageStateFinish = 2
	ilinkItemTypeText       = 1

	ilinkLongPollTimeout = 35 * time.Second
	ilinkPollInterval    = 1500 * time.Millisecond
	ilinkLoginTimeout    = 5 * time.Minute
	ilinkMaxRetries      = 3
	ilinkBackoffDelay    = 30 * time.Second
	ilinkRetryDelay      = 2 * time.Second
	ilinkSessionExpired  = -14

	ilinkMaxMessageLen  = 2048
	ilinkDefaultBaseURL = "https://ilinkai.weixin.qq.com"
	ilinkDefaultBotType = "3"
	ilinkChannelVersion = "1.0.2"
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

// ilinkMessage represents a single WeChat iLink message.
type ilinkMessage struct {
	FromUserID   string      `json:"from_user_id,omitempty"`
	ToUserID     string      `json:"to_user_id,omitempty"`
	ClientID     string      `json:"client_id,omitempty"`
	MessageType  int         `json:"message_type,omitempty"`
	MessageState int         `json:"message_state,omitempty"`
	ContextToken string      `json:"context_token,omitempty"`
	ItemList     []ilinkItem `json:"item_list,omitempty"`
}

// ilinkItem represents an item in a message.
type ilinkItem struct {
	Type     int            `json:"type,omitempty"`
	TextItem *ilinkTextItem `json:"text_item,omitempty"`
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
	Ret     int    `json:"ret,omitempty"`
	ErrCode int    `json:"errcode,omitempty"`
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
	cfg        *config.Config
	imCfg      config.IMAdapterConfig
	router     *NotificationRouter
	token      atomic.Pointer[TokenData]
	syncBuf    string // monitorLoop-only: written/read from a single goroutine
	mu         sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
	storageDir string
	tokenFile  string
	httpClient *http.Client

	// contextToken per user — needed to send replies.
	contextTokens map[string]string
}

// NewWechatAdapter creates a new WeChat iLink adapter.
func NewWechatAdapter(cfg *config.Config, imCfg config.IMAdapterConfig, paths *config.Paths, router *NotificationRouter) (*WechatAdapter, error) {
	storageDir := filepath.Join(paths.StateDir, "wechat")
	tokenFile := filepath.Join(storageDir, "token.json")
	if imCfg.Wechat != nil && imCfg.Wechat.TokenPath != "" {
		tokenFile = imCfg.Wechat.TokenPath
	}

	a := &WechatAdapter{
		cfg:           cfg,
		imCfg:         imCfg,
		router:        router,
		storageDir:    storageDir,
		tokenFile:     tokenFile,
		httpClient:    &http.Client{Timeout: 40 * time.Second},
		contextTokens: make(map[string]string),
	}

	// Try to load existing token.
	if token := a.loadToken(); token != nil {
		a.token.Store(token)
		slog.Info("wechat ilink: loaded saved token", "account_id", token.AccountID)
	}

	// Try to load saved sync buf.
	a.syncBuf = a.loadSyncBuf()

	return a, nil
}

func (a *WechatAdapter) Type() string { return "wechat" }

// StartLogin initiates a WeChat iLink QR login flow and returns the QR URL.
// The user should open this URL on their phone and scan the QR code in WeChat.
// A background goroutine polls for scan confirmation and auto-replaces the
// token on success.
func (a *WechatAdapter) StartLogin() (string, error) {
	qr, err := a.getBotQRCode()
	if err != nil {
		return "", fmt.Errorf("get QR code: %w", err)
	}
	go a.pollQRStatusForRelogin(a.ctx, qr.QRCode)
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
				a.sleep(ctx, ilinkPollInterval)
			} else {
				time.Sleep(ilinkPollInterval)
			}
			continue
		}
		switch resp.Status {
		case "confirmed":
			a.token.Store(&TokenData{
				Token:     resp.BotToken,
				BaseURL:   resp.BaseURL,
				AccountID: resp.ILinkBotID,
				UserID:    resp.ILinkUserID,
				SavedAt:   time.Now().Format(time.RFC3339),
			})
			a.saveToken()
			slog.Info("wechat ilink: re-login successful via QR scan")
			if a.router != nil {
				a.router.HandleLoginResult("wechat", true, "")
			}
			return
		case "expired":
			slog.Warn("wechat ilink: QR code expired during re-login")
			if a.router != nil {
				a.router.HandleLoginResult("wechat", false, "QR code expired")
			}
			return
		}
		if ctx != nil {
			a.sleep(ctx, ilinkPollInterval)
		} else {
			time.Sleep(ilinkPollInterval)
		}
	}
	slog.Warn("wechat ilink: re-login polling timed out")
	if a.router != nil {
		a.router.HandleLoginResult("wechat", false, "login timeout")
	}
}

// Connect starts the adapter and blocks until Disconnect is called or a fatal
// error occurs. If base_url is empty and no token file exists, it falls back
// to console mode (stdin/stdout).
func (a *WechatAdapter) Connect() error {
	ctx, cancel := context.WithCancel(context.Background())
	a.ctx = ctx
	a.cancel = cancel

	baseURL := a.baseURL()

	// Console mode fallback: no base_url configured and no saved token.
	if baseURL == "" && a.token.Load() == nil {
		return a.connectConsole(ctx)
	}

	return a.connectILink(ctx)
}

// connectConsole runs in console mode (stdin/stdout) for testing without WeChat.
func (a *WechatAdapter) connectConsole(ctx context.Context) error {
	slog.Info("wechat adapter: console mode (no base_url configured and no saved token), reading from stdin")

	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			a.router.HandleIncomingMessage(IncomingMessage{
				IMType:     "console",
				ChatID:     "console",
				SenderID:   "console",
				SenderName: "console",
				Text:       line,
			})
		}
		if err := scanner.Err(); err != nil {
			slog.Error("wechat: stdin read error", "error", err)
		}
		a.cancel()
	}()

	<-ctx.Done()
	return nil
}

// connectILink performs QR login (if needed) and starts the long-poll monitor loop.
func (a *WechatAdapter) connectILink(ctx context.Context) error {
	if a.token.Load() == nil {
		if err := a.login(ctx); err != nil {
			return fmt.Errorf("wechat ilink login: %w", err)
		}
	}

	// Startup probe: do one lightweight getUpdates call to detect expired tokens early.
	resp, err := a.getUpdates()
	if err != nil {
		slog.Warn("wechat ilink: startup probe failed", "error", err)
	} else if resp.ErrCode == ilinkSessionExpired || resp.Ret == ilinkSessionExpired {
		slog.Warn("wechat ilink: saved token is expired, clearing it and re-login")
		a.clearToken()
		if err := a.login(ctx); err != nil {
			return fmt.Errorf("wechat ilink re-login: %w", err)
		}
	}

	tok := a.token.Load()
	slog.Info("wechat ilink: connected, starting monitor loop",
		"account_id", tok.AccountID,
		"base_url", tok.BaseURL,
	)

	a.monitorLoop(ctx)
	return nil
}

// SendText sends a plain text message to the specified chat (user ID).
// In console mode it prints to stdout. For iLink, it posts to the sendmessage API.
func (a *WechatAdapter) SendText(chatID, text string) error {
	if a.baseURL() == "" && a.token.Load() == nil {
		fmt.Printf("[%s] %s\n", chatID, text)
		return nil
	}
	return a.sendILinkText(chatID, text)
}

// Disconnect signals the adapter to stop.
func (a *WechatAdapter) Disconnect() {
	if a.cancel != nil {
		a.cancel()
	}
}

// --- Helpers ---

func (a *WechatAdapter) baseURL() string {
	if a.imCfg.Wechat != nil && a.imCfg.Wechat.BaseURL != "" {
		return strings.TrimRight(a.imCfg.Wechat.BaseURL, "/")
	}
	if tok := a.token.Load(); tok != nil && tok.BaseURL != "" {
		return strings.TrimRight(tok.BaseURL, "/")
	}
	return ""
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
	_, _ = rand.Read(buf[:])
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
	req, err := http.NewRequestWithContext(a.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header = a.buildHeaders()
	return a.httpClient.Do(req)
}

func (a *WechatAdapter) apiPost(path string, body any) (*http.Response, error) {
	baseURL := a.baseURL()
	if baseURL == "" {
		return nil, fmt.Errorf("no base URL configured")
	}
	url := baseURL + "/" + path

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(a.ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header = a.buildHeaders()
	return a.httpClient.Do(req)
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
	// Temporarily set baseURL for API calls during login.
	a.token.Store(&TokenData{BaseURL: strings.TrimRight(baseURL, "/")})

	slog.Info("wechat ilink: starting QR login flow")

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
			slog.Warn("wechat ilink: polling QR status failed", "error", err)
			time.Sleep(ilinkPollInterval)
			continue
		}

		switch statusResp.Status {
		case "wait":
			// Still waiting, poll again.
		case "scaned":
			slog.Info("wechat ilink: QR code scanned, please confirm in WeChat...")
		case "expired":
			refreshCount++
			if refreshCount > 3 {
				return fmt.Errorf("QR code expired multiple times, please retry")
			}
			slog.Warn("wechat ilink: QR expired, refreshing", "attempt", refreshCount)
			newQR, err := a.getBotQRCode()
			if err != nil {
				return fmt.Errorf("refresh QR code: %w", err)
			}
			currentQRCode = newQR.QRCode
			fmt.Println("New QR code (scan again):")
			fmt.Printf("  %s\n", newQR.QRCodeImgContent)
		case "confirmed":
			slog.Info("wechat ilink: login successful!")
			tok := &TokenData{
				Token:     statusResp.BotToken,
				BaseURL:   statusResp.BaseURL,
				AccountID: statusResp.ILinkBotID,
				UserID:    statusResp.ILinkUserID,
				SavedAt:   time.Now().Format(time.RFC3339),
			}
			a.token.Store(tok)
			a.saveToken()
			slog.Info("wechat ilink: token saved", "account_id", tok.AccountID)
			return nil
		}

		time.Sleep(ilinkPollInterval)
	}

	return fmt.Errorf("login timeout (%v)", ilinkLoginTimeout)
}

func (a *WechatAdapter) getBotQRCode() (*ilinkQRCodeResponse, error) {
	path := "ilink/bot/get_bot_qrcode?bot_type=" + a.botType()
	resp, err := a.apiGet(path)
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
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
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result ilinkQRCodeStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

// --- Long-poll Monitor ---

func (a *WechatAdapter) monitorLoop(ctx context.Context) {
	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		resp, err := a.getUpdates()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			consecutiveFailures++
			slog.Error("wechat ilink: getupdates error",
				"error", err,
				"consecutive_failures", consecutiveFailures,
				"max", ilinkMaxRetries,
			)
			if consecutiveFailures >= ilinkMaxRetries {
				slog.Warn("wechat ilink: backing off after consecutive failures", "delay", ilinkBackoffDelay)
				consecutiveFailures = 0
				a.sleep(ctx, ilinkBackoffDelay)
			} else {
				a.sleep(ctx, ilinkRetryDelay)
			}
			continue
		}

		// Check for API-level errors.
		isAPIError := (resp.Ret != 0 && resp.Ret != -1) || (resp.ErrCode != 0 && resp.ErrCode != -1)
		// Note: ret=-1 with errcode=0 often means "no new messages" — treat as success.
		// The API returns ret=0 on success with messages.
		if resp.Ret != 0 && resp.ErrCode == 0 && len(resp.Msgs) == 0 {
			// No new messages, not an error.
			isAPIError = false
		}

		if isAPIError {
			isSessionExpired := resp.ErrCode == ilinkSessionExpired || resp.Ret == ilinkSessionExpired
			if isSessionExpired {
				slog.Warn("wechat ilink: session expired (errcode=-14), clearing token and re-login")
				a.clearToken()
				if err := a.login(ctx); err != nil {
					slog.Error("wechat ilink: re-login failed", "error", err)
					a.notifySessionExpired()
					a.sleep(ctx, ilinkBackoffDelay)
				}
				continue
			}

			consecutiveFailures++
			slog.Warn("wechat ilink: getupdates API error",
				"ret", resp.Ret,
				"errcode", resp.ErrCode,
				"errmsg", resp.ErrMsg,
				"consecutive_failures", consecutiveFailures,
			)
			if consecutiveFailures >= ilinkMaxRetries {
				slog.Warn("wechat ilink: backing off after consecutive failures", "delay", ilinkBackoffDelay)
				consecutiveFailures = 0
				a.sleep(ctx, ilinkBackoffDelay)
			} else {
				a.sleep(ctx, ilinkRetryDelay)
			}
			continue
		}

		consecutiveFailures = 0

		// Update sync buf.
		if resp.GetUpdatesBuf != "" {
			a.syncBuf = resp.GetUpdatesBuf
			a.saveSyncBuf()
		}

		// Process messages.
		for _, msg := range resp.Msgs {
			a.handleIncomingMessage(msg)
		}
	}
}

// notifySessionExpired logs the session expiry and notifies the router so
// other IM adapters can inform the user. The WeChat session is expired so we
// cannot notify through WeChat itself.
func (a *WechatAdapter) notifySessionExpired() {
	slog.Warn("wechat ilink: session expired — notifying via other channels")
	if a.router != nil {
		a.router.HandleSessionExpired("wechat")
	}
}

func (a *WechatAdapter) getUpdates() (*ilinkGetUpdatesResponse, error) {
	reqBody := ilinkGetUpdatesRequest{
		GetUpdatesBuf: a.syncBuf,
		BaseInfo: ilinkBaseInfo{
			ChannelVersion: ilinkChannelVersion,
		},
	}

	resp, err := a.apiPost("ilink/bot/getupdates", reqBody)
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result ilinkGetUpdatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

func (a *WechatAdapter) handleIncomingMessage(msg ilinkMessage) {
	// Only handle user messages (not our own bot messages).
	if msg.MessageType != ilinkMessageTypeUser {
		return
	}

	// Save context_token for this user (needed to reply).
	if msg.ContextToken != "" && msg.FromUserID != "" {
		a.mu.Lock()
		a.contextTokens[msg.FromUserID] = msg.ContextToken
		a.mu.Unlock()
	}

	// Extract text from item_list.
	var textParts []string
	for _, item := range msg.ItemList {
		if item.Type == ilinkItemTypeText && item.TextItem != nil && item.TextItem.Text != "" {
			textParts = append(textParts, item.TextItem.Text)
		}
	}
	if len(textParts) == 0 {
		return
	}

	text := strings.Join(textParts, "\n")
	slog.Info("wechat ilink: received message", "from", msg.FromUserID, "text_len", len(text))

	a.router.HandleIncomingMessage(IncomingMessage{
		IMType:    "wechat",
		ChatID:    msg.FromUserID,
		SenderID:  msg.FromUserID,
		MessageID: msg.ClientID,
		Text:      text,
	})
}

// --- Sending Messages ---

func (a *WechatAdapter) sendILinkText(chatID, text string) error {
	a.mu.Lock()
	ctxToken, ok := a.contextTokens[chatID]
	a.mu.Unlock()

	if !ok || ctxToken == "" {
		// iLink usually provides context_token on inbound messages, which is used to
		// thread bot replies. Some message types/clients omit it; in that case, try
		// sending without a context_token instead of dropping the reply.
		slog.Warn("wechat ilink: missing context_token, attempting send without it", "chatID", chatID)
		ctxToken = ""
	}

	// Split into segments if too long.
	segments := splitText(text, ilinkMaxMessageLen)

	baseID := fmt.Sprintf("chord-gateway-%d", time.Now().UnixNano())
	for i, seg := range segments {
		clientID := fmt.Sprintf("%s-%d", baseID, i)
		reqBody := ilinkSendMessageRequest{
			Msg: ilinkMessage{
				FromUserID:   "",
				ToUserID:     chatID,
				ClientID:     clientID,
				MessageType:  ilinkMessageTypeBot,
				MessageState: ilinkMessageStateFinish,
				ContextToken: ctxToken,
				ItemList: []ilinkItem{
					{
						Type:     ilinkItemTypeText,
						TextItem: &ilinkTextItem{Text: seg},
					},
				},
			},
		}

		if ctxToken == "" {
			// Context token is optional in sendmessage; omit it if missing.
			reqBody.Msg.ContextToken = ""
		}

		if err := a.sendMessage(reqBody); err != nil {
			return fmt.Errorf("send message segment %d: %w", i, err)
		}
	}

	return nil
}

func (a *WechatAdapter) sendMessage(body ilinkSendMessageRequest) error {
	resp, err := a.apiPost("ilink/bot/sendmessage", body)
	if err != nil {
		return fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result ilinkAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		// Non-JSON response — treat as success if HTTP 200.
		return nil
	}

	if result.ErrCode != 0 {
		if result.ErrCode == ilinkSessionExpired {
			slog.Warn("wechat ilink: send failed with session expired")
		}
		return fmt.Errorf("send error: errcode=%d errmsg=%s", result.ErrCode, result.ErrMsg)
	}

	return nil
}

// --- Token Persistence ---

func (a *WechatAdapter) tokenPath() string {
	return a.tokenFile
}

func (a *WechatAdapter) loadToken() *TokenData {
	path := a.tokenPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var token TokenData
	if err := json.Unmarshal(data, &token); err != nil {
		slog.Warn("wechat ilink: failed to parse token file", "path", path, "error", err)
		return nil
	}
	return &token
}

func (a *WechatAdapter) clearToken() {
	a.token.Store(nil)
	if err := os.Remove(a.tokenPath()); err != nil && !os.IsNotExist(err) {
		slog.Warn("wechat ilink: failed to remove token file", "error", err)
	}
}

func (a *WechatAdapter) saveToken() {
	tok := a.token.Load()
	if tok == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(a.tokenPath()), 0700); err != nil {
		slog.Error("wechat ilink: failed to create token dir", "error", err)
		return
	}
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		slog.Error("wechat ilink: failed to marshal token", "error", err)
		return
	}
	if err := os.WriteFile(a.tokenPath(), data, 0600); err != nil {
		slog.Error("wechat ilink: failed to save token", "error", err)
	}
}

// --- Sync Buf Persistence ---

func (a *WechatAdapter) syncBufPath() string {
	return filepath.Join(a.storageDir, "sync-buf.json")
}

func (a *WechatAdapter) loadSyncBuf() string {
	path := a.syncBufPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var obj struct {
		GetUpdatesBuf string `json:"get_updates_buf"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return ""
	}
	return obj.GetUpdatesBuf
}

func (a *WechatAdapter) saveSyncBuf() {
	if err := os.MkdirAll(a.storageDir, 0700); err != nil {
		slog.Error("wechat ilink: failed to create storage dir", "error", err)
		return
	}
	obj := struct {
		GetUpdatesBuf string `json:"get_updates_buf"`
	}{GetUpdatesBuf: a.syncBuf}
	data, err := json.Marshal(obj)
	if err != nil {
		slog.Error("wechat ilink: failed to marshal sync buf", "error", err)
		return
	}
	if err := os.WriteFile(a.syncBufPath(), data, 0600); err != nil {
		slog.Error("wechat ilink: failed to save sync buf", "error", err)
	}
}

// --- Utilities ---

// sleep waits for the given duration or until the context is cancelled.
func (a *WechatAdapter) sleep(ctx context.Context, d time.Duration) {
	sleepCtx(ctx, d)
}

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
