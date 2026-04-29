// Package main implements the Feishu (飞书) IM adapter for chord-gateway.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/keakon/chord-gateway/config"
)

const (
	feishuMaxTextLen    = 4000
	feishuQueueSize     = 256
	feishuDedupeKeyFmt  = "%s|%s|%s"      // app_id|chat_id|message_id
	feishuCardActionFmt = "%s|card|%s|%s" // app_id|chat_id|request_id
)

// --- Feishu API types ---

// FeishuTokenResponse is the response from the Feishu app_access_token API.
type FeishuTokenResponse struct {
	Code           int    `json:"code"`
	Msg            string `json:"msg"`
	AppAccessToken string `json:"app_access_token"`
	Expire         int    `json:"expire"`
}

// FeishuSendResponse is the response from the Feishu message send API.
type FeishuSendResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// FeishuURLVerification is the URL verification challenge request.
type FeishuURLVerification struct {
	Challenge string `json:"challenge"`
	Token     string `json:"token"`
	Type      string `json:"type"`
}

// FeishuEventCallback is the event callback envelope from Feishu.
type FeishuEventCallback struct {
	Schema string            `json:"schema"`
	Header FeishuEventHeader `json:"header"`
	Event  json.RawMessage   `json:"event"`
}

// FeishuEventHeader is the header of a Feishu event callback.
type FeishuEventHeader struct {
	EventID   string `json:"event_id"`
	EventType string `json:"event_type"`
	Token     string `json:"token"`
}

// FeishuMessageEvent is the event payload for im.message.receive_v1.
type FeishuMessageEvent struct {
	Sender  FeishuEventSender  `json:"sender"`
	Message FeishuEventMessage `json:"message"`
}

// FeishuEventSender contains the sender information.
type FeishuEventSender struct {
	SenderID FeishuSenderID `json:"sender_id"`
}

// FeishuSenderID contains various IDs for the sender.
type FeishuSenderID struct {
	UnionID string `json:"union_id"`
	UserID  string `json:"user_id"`
	OpenID  string `json:"open_id"`
}

// FeishuEventMessage contains the message information.
type FeishuEventMessage struct {
	ChatID      string `json:"chat_id"`
	MessageID   string `json:"message_id"`
	MessageType string `json:"message_type"`
	Content     string `json:"content"`
}

// FeishuCardActionEvent is the callback payload for interactive card actions.
type FeishuCardActionEvent struct {
	Operator struct {
		OpenID string `json:"open_id"`
	} `json:"operator"`
	Action struct {
		Tag    string         `json:"tag"`
		Value  map[string]any `json:"value"`
		Option string         `json:"option,omitempty"`
		Name   string         `json:"name,omitempty"`
	} `json:"action"`
	Context struct {
		OpenChatID string `json:"open_chat_id"`
		ChatID     string `json:"chat_id"`
	} `json:"context"`
}

// FeishuMessageContent is the parsed content of a text message.
type FeishuMessageContent struct {
	Text string `json:"text"`
}

// --- Adapter ---

// FeishuAdapter implements IMAdapter for Feishu (飞书).
type FeishuAdapter struct {
	cfg       *config.Config
	imCfg     config.IMAdapterConfig
	msgRouter MessageRouter // used for dispatching incoming messages (testable)
	// Current notification router (for process /status etc.)
	router        *NotificationRouter // retained for SendText via parent adapter
	server        *http.Server
	httpClient    *http.Client
	accessToken   string
	tokenExpireAt time.Time
	mu            sync.Mutex
	cancel        context.CancelFunc

	// Async message queue.
	messageQueue chan IncomingMessage
	dedupe       *DedupeStore
	wg           sync.WaitGroup
}

func (a *FeishuAdapter) sendCardOrFallback(chatID string, card map[string]any, fallback string) {
	if err := a.SendInteractive(chatID, card); err != nil {
		slog.Warn("feishu: send interactive card failed, falling back to text", "chat_id", chatID, "error", err)
		if strings.TrimSpace(fallback) != "" {
			_ = a.SendText(chatID, fallback)
		}
	}
}

// NewFeishuAdapter creates a new Feishu adapter.
func NewFeishuAdapter(cfg *config.Config, imCfg config.IMAdapterConfig, paths *config.Paths, router *NotificationRouter) (*FeishuAdapter, error) {
	if imCfg.Feishu == nil {
		return nil, fmt.Errorf("feishu: feishu config is required when im type is feishu")
	}

	a := &FeishuAdapter{
		cfg:          cfg,
		imCfg:        imCfg,
		router:       router,
		msgRouter:    router, // NotificationRouter satisfies MessageRouter
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		messageQueue: make(chan IncomingMessage, feishuQueueSize),
	}

	// Initialize dedupe store.
	dedupe, err := NewDedupeStore(paths.DedupeDir)
	if err != nil {
		return nil, fmt.Errorf("feishu: init dedupe store: %w", err)
	}
	a.dedupe = dedupe

	return a, nil
}

func (a *FeishuAdapter) Type() string { return "feishu" }

func (a *FeishuAdapter) StartLogin() (string, error) {
	return "", ErrLoginNotSupported
}

// Connect starts the adapter: obtains an access token, starts the HTTP
// callback server, starts the async queue consumer, and blocks until
// Disconnect is called or a fatal error occurs.
func (a *FeishuAdapter) Connect() error {
	if a.router == nil {
		return fmt.Errorf("feishu: router not set, call SetRouter first")
	}

	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel

	// Get initial access token to validate credentials.
	if _, err := a.getAccessToken(); err != nil {
		slog.Error("feishu: failed to get access token", "error", err)
		cancel()
		a.dedupe.Close()
		a.router.HandleSessionExpired("feishu")
		return fmt.Errorf("feishu: initial access token: %w", err)
	}

	// Start the async queue consumer goroutine.
	a.wg.Add(1)
	go a.queueConsumer(ctx)

	listen := a.imCfg.Feishu.Listen
	if listen == "" {
		listen = ":8080"
	}
	path := a.imCfg.Feishu.WebhookPath
	if path == "" {
		path = "/feishu/callback"
	}

	mux := http.NewServeMux()
	mux.HandleFunc(path, a.handleCallback)

	a.server = &http.Server{
		Addr:    listen,
		Handler: mux,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("feishu adapter: HTTP server starting", "listen", listen, "path", path)
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("feishu HTTP server error: %w", err)
	case <-ctx.Done():
		// Wait for consumer to drain.
		a.wg.Wait()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := a.server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("feishu HTTP server shutdown: %w", err)
		}
	}

	return nil
}

// SendText sends a plain text message to the specified chat.
// Long messages are split at line boundaries to stay within Feishu's ~4000 char limit.
func (a *FeishuAdapter) SendText(chatID, text string) error {
	chunks := splitText(text, feishuMaxTextLen)
	for i, chunk := range chunks {
		if err := a.sendTextChunk(chatID, chunk, false); err != nil {
			return fmt.Errorf("send chunk %d/%d: %w", i+1, len(chunks), err)
		}
	}
	return nil
}

func (a *FeishuAdapter) SendInteractive(chatID string, card map[string]any) error {
	return a.sendMessage(chatID, "interactive", card, false)
}

// Disconnect signals the adapter to stop and shuts down the HTTP server.
func (a *FeishuAdapter) Disconnect() {
	if a.cancel != nil {
		a.cancel()
	}
	if a.dedupe != nil {
		a.dedupe.Close()
	}
}

// queueConsumer reads messages from the queue and dispatches them.
func (a *FeishuAdapter) queueConsumer(ctx context.Context) {
	defer a.wg.Done()
	for {
		select {
		case <-ctx.Done():
			// Drain remaining messages from the queue.
			for {
				select {
				case msg := <-a.messageQueue:
					a.dispatchMessage(msg)
				default:
					return
				}
			}
		case msg := <-a.messageQueue:
			a.dispatchMessage(msg)
		}
	}
}

// dispatchMessage routes a queued message through the router.
func (a *FeishuAdapter) dispatchMessage(msg IncomingMessage) {
	slog.Debug("feishu: dispatching queued message",
		"chat_id", msg.ChatID,
		"sender_id", msg.SenderID,
		"message_id", msg.MessageID,
	)
	if a.msgRouter != nil {
		a.msgRouter.HandleIncomingMessage(msg)
	}
}

// --- HTTP handler ---

func (a *FeishuAdapter) handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("feishu: failed to read callback body", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Try URL verification challenge first.
	var urlVerif FeishuURLVerification
	if err := json.Unmarshal(body, &urlVerif); err == nil && urlVerif.Type == "url_verification" {
		if a.imCfg.Feishu.VerificationToken != "" && urlVerif.Token != a.imCfg.Feishu.VerificationToken {
			slog.Warn("feishu: URL verification token mismatch")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"challenge": urlVerif.Challenge})
		return
	}

	// Parse event callback.
	var callback FeishuEventCallback
	if err := json.Unmarshal(body, &callback); err != nil {
		slog.Error("feishu: failed to parse event callback", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Verify token.
	if a.imCfg.Feishu.VerificationToken != "" && callback.Header.Token != a.imCfg.Feishu.VerificationToken {
		slog.Warn("feishu: event callback token mismatch",
			"expected", a.imCfg.Feishu.VerificationToken,
			"got", callback.Header.Token,
		)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Handle message receive event.
	if callback.Header.EventType == "im.message.receive_v1" {
		var event FeishuMessageEvent
		if err := json.Unmarshal(callback.Event, &event); err != nil {
			slog.Error("feishu: failed to parse message event", "error", err)
			w.WriteHeader(http.StatusOK)
			return
		}

		if event.Message.MessageType != "text" {
			slog.Debug("feishu: ignoring non-text message", "msg_type", event.Message.MessageType)
			w.WriteHeader(http.StatusOK)
			return
		}

		var content FeishuMessageContent
		if err := json.Unmarshal([]byte(event.Message.Content), &content); err != nil {
			slog.Error("feishu: failed to parse message content", "error", err, "content", event.Message.Content)
			w.WriteHeader(http.StatusOK)
			return
		}

		appID := a.imCfg.Feishu.AppID
		senderOpenID := event.Sender.SenderID.OpenID
		chatID := event.Message.ChatID
		messageID := event.Message.MessageID

		if !a.imCfg.Feishu.IsOpenIDAllowed(senderOpenID) {
			slog.Debug("feishu: message from non-allowed open_id, ignoring", "open_id", senderOpenID, "chat_id", chatID)
			w.WriteHeader(http.StatusOK)
			return
		}

		dedupeKey := fmt.Sprintf(feishuDedupeKeyFmt, appID, chatID, messageID)
		if !a.dedupe.TryBegin(dedupeKey) {
			slog.Debug("feishu: duplicate message, skipping", "message_id", messageID, "chat_id", chatID)
			w.WriteHeader(http.StatusOK)
			return
		}

		slog.Info("feishu: received message", "chat_id", chatID, "open_id", senderOpenID, "message_id", messageID, "content", content.Text)
		msg := IncomingMessage{IMType: "feishu", ChatID: chatID, SenderID: senderOpenID, MessageID: messageID, ConversationID: event.Message.ChatID, Text: content.Text, AppID: appID}
		select {
		case a.messageQueue <- msg:
			a.dedupe.Commit(dedupeKey)
		default:
			slog.Error("feishu: message queue full, dropping message", "chat_id", chatID, "message_id", messageID)
			a.dedupe.Release(dedupeKey)
		}
	} else if callback.Header.EventType == "card.action.trigger" {
		var event FeishuCardActionEvent
		if err := json.Unmarshal(callback.Event, &event); err != nil {
			slog.Error("feishu: failed to parse card action event", "error", err)
			w.WriteHeader(http.StatusOK)
			return
		}
		chatID := event.Context.ChatID
		if chatID == "" {
			chatID = event.Context.OpenChatID
		}
		senderOpenID := event.Operator.OpenID
		if !a.imCfg.Feishu.IsOpenIDAllowed(senderOpenID) {
			slog.Debug("feishu: card action from non-allowed open_id, ignoring", "open_id", senderOpenID, "chat_id", chatID)
			w.WriteHeader(http.StatusOK)
			return
		}
		requestID, _ := event.Action.Value["request_id"].(string)
		command, _ := event.Action.Value["command"].(string)
		contextIMType, _ := event.Action.Value["im_type"].(string)
		contextChatID, _ := event.Action.Value["chat_id"].(string)
		if requestID == "" || command == "" || contextChatID == "" || contextIMType != "feishu" || contextChatID != chatID {
			slog.Warn("feishu: stale/wrong-context card action ignored", "request_id", requestID, "chat_id", chatID, "context_chat_id", contextChatID, "command", command)
			w.WriteHeader(http.StatusOK)
			return
		}
		dedupeKey := fmt.Sprintf(feishuCardActionFmt, a.imCfg.Feishu.AppID, chatID, requestID+"|"+command)
		if !a.dedupe.TryBegin(dedupeKey) {
			slog.Debug("feishu: duplicate card action, skipping", "request_id", requestID, "command", command, "chat_id", chatID)
			w.WriteHeader(http.StatusOK)
			return
		}
		msg := IncomingMessage{IMType: "feishu", ChatID: chatID, SenderID: senderOpenID, MessageID: requestID + ":" + command, ConversationID: chatID, Text: command, AppID: a.imCfg.Feishu.AppID}
		select {
		case a.messageQueue <- msg:
			a.dedupe.Commit(dedupeKey)
		default:
			slog.Error("feishu: message queue full, dropping card action", "chat_id", chatID, "request_id", requestID, "command", command)
			a.dedupe.Release(dedupeKey)
		}
	}

	// Fast ACK — always return 200 immediately.
	w.WriteHeader(http.StatusOK)
}

// --- Access token management ---

func (a *FeishuAdapter) getAccessToken() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.accessToken != "" && time.Now().Before(a.tokenExpireAt) {
		return a.accessToken, nil
	}

	reqBody := fmt.Sprintf(`{"app_id":"%s","app_secret":"%s"}`, a.imCfg.Feishu.AppID, a.imCfg.Feishu.AppSecret)
	resp, err := a.httpClient.Post(
		"https://open.feishu.cn/open-apis/auth/v3/app_access_token/internal",
		"application/json",
		strings.NewReader(reqBody),
	)
	if err != nil {
		return "", fmt.Errorf("feishu token HTTP request: %w", err)
	}
	defer resp.Body.Close()

	var result FeishuTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("feishu decode token response: %w", err)
	}

	if result.Code != 0 {
		return "", fmt.Errorf("feishu token error: code=%d msg=%s", result.Code, result.Msg)
	}

	a.accessToken = result.AppAccessToken
	// Refresh 5 minutes before actual expiry.
	expiry := result.Expire - 300
	if expiry <= 0 {
		expiry = result.Expire
	}
	a.tokenExpireAt = time.Now().Add(time.Duration(expiry) * time.Second)

	slog.Info("feishu: access token refreshed", "expire", result.Expire)
	return a.accessToken, nil
}

// --- Sending messages ---

func (a *FeishuAdapter) sendTextChunk(chatID, text string, retry bool) error {
	return a.sendMessage(chatID, "text", map[string]string{"text": text}, retry)
}

func (a *FeishuAdapter) sendMessage(chatID, msgType string, content any, retry bool) error {
	token, err := a.getAccessToken()
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	contentJSON, err := json.Marshal(content)
	if err != nil {
		return fmt.Errorf("marshal message content: %w", err)
	}

	reqBody := map[string]string{
		"receive_id": chatID,
		"msg_type":   msgType,
		"content":    string(contentJSON),
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal send request: %w", err)
	}

	url := "https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type=chat_id"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create send request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send message HTTP request: %w", err)
	}
	defer resp.Body.Close()

	var result FeishuSendResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode send response: %w", err)
	}

	if result.Code != 0 {
		if result.Code == 99991663 && !retry {
			slog.Warn("feishu: access token expired, refreshing")
			a.mu.Lock()
			a.accessToken = ""
			a.tokenExpireAt = time.Time{}
			a.mu.Unlock()
			return a.sendMessage(chatID, msgType, content, true)
		}
		return fmt.Errorf("feishu send error: code=%d msg=%s", result.Code, result.Msg)
	}

	return nil
}
