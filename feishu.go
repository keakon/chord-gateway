// Package main implements the Feishu (飞书) IM adapter for chord-gateway.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkcallback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/keakon/chord-gateway/config"
)

const (
	feishuMaxTextLen       = 4000
	feishuQueueSize        = 256
	feishuDedupeKeyFmt     = "%s|%s|%s"      // app_id|chat_id|message_id
	feishuCardActionFmt    = "%s|card|%s|%s" // app_id|chat_id|request_id
	feishuDefaultPing      = 2 * time.Minute
	feishuDefaultReconnect = 2 * time.Minute
	feishuFragmentTTL      = 5 * time.Second
)

var feishuOpenBaseURL = "https://open.feishu.cn"

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

// FeishuAdapter implements IMAdapter for Feishu (飞书).
type FeishuAdapter struct {
	imCfg     config.IMAdapterConfig
	imCfgMu   sync.RWMutex
	msgRouter MessageRouter // used for dispatching incoming messages (testable)
	// Current notification router (for process /status etc.)
	router *NotificationRouter // retained for SendText via parent adapter

	httpClient *http.Client

	accessToken   string
	tokenExpireAt time.Time
	mu            sync.Mutex
	cancel        context.CancelFunc

	connMu sync.Mutex
	conn   *websocket.Conn

	writeMu sync.Mutex

	fragmentsMu sync.Mutex
	fragments   map[string]feishuFragmentBuffer

	intervalMu        sync.RWMutex
	pingInterval      time.Duration
	reconnectInterval time.Duration
	runLongConn       func(context.Context, *larkdispatcher.EventDispatcher) error

	// Async message queue.
	messageQueue chan IncomingMessage
	dedupe       *DedupeStore
	wg           sync.WaitGroup
}

func (a *FeishuAdapter) sendCardOrFallback(chatID string, card map[string]any, fallback string) (*InteractiveCardHandle, error) {
	handle, err := a.SendInteractiveWithHandle(chatID, card)
	if err != nil {
		slog.Warn("feishu: send interactive card failed, falling back to text", "chat_id", chatID, "error", err)
		if strings.TrimSpace(fallback) != "" {
			if fallbackErr := a.SendText(chatID, fallback); fallbackErr != nil {
				return nil, fmt.Errorf("send interactive card: %w; fallback text: %v", err, fallbackErr)
			}
			return nil, nil
		}
		return nil, err
	}
	return handle, nil
}

// NewFeishuAdapter creates a new Feishu adapter.
func NewFeishuAdapter(cfg *config.Config, imCfg config.IMAdapterConfig, paths *config.Paths, router *NotificationRouter) (*FeishuAdapter, error) {
	if imCfg.Feishu == nil {
		return nil, fmt.Errorf("feishu: feishu config is required when im type is feishu")
	}

	a := &FeishuAdapter{
		imCfg:             imCfg,
		router:            router,
		msgRouter:         router, // NotificationRouter satisfies MessageRouter
		httpClient:        &http.Client{Timeout: 10 * time.Second},
		fragments:         make(map[string]feishuFragmentBuffer),
		pingInterval:      feishuDefaultPing,
		reconnectInterval: feishuDefaultReconnect,
		messageQueue:      make(chan IncomingMessage, feishuQueueSize),
	}
	if a.runLongConn == nil {
		a.runLongConn = a.runLongConnection
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

func (a *FeishuAdapter) getIMConfig() config.IMAdapterConfig {
	a.imCfgMu.RLock()
	defer a.imCfgMu.RUnlock()
	return a.imCfg
}

func (a *FeishuAdapter) updateIMConfig(cfg config.IMAdapterConfig) {
	if cfg.Feishu == nil {
		return
	}
	a.imCfgMu.Lock()
	a.imCfg = cfg
	a.imCfgMu.Unlock()
}

func (a *FeishuAdapter) feishuConfig() *config.FeishuConfig {
	cfg := a.getIMConfig()
	return cfg.Feishu
}

func (a *FeishuAdapter) StartLogin() (string, error) {
	return "", ErrLoginNotSupported
}

// Connect starts the adapter: validates credentials, starts the async queue
// consumer, establishes a Feishu long connection, and blocks until Disconnect
// is called or a fatal error occurs.
func (a *FeishuAdapter) Connect() error {
	if a.router == nil {
		return fmt.Errorf("feishu: router not configured")
	}

	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	defer a.closeConn()
	defer a.dedupe.Close()

	// Get initial access token to validate credentials.
	if _, err := a.getAccessToken(); err != nil {
		slog.Error("feishu: failed to get access token", "error", err)
		cancel()
		a.router.HandleSessionExpired("feishu")
		return fmt.Errorf("feishu: initial access token: %w", err)
	}

	// Start the async queue consumer goroutine.
	a.wg.Add(1)
	go a.queueConsumer(ctx)

	dispatcher := larkdispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(a.handleMessageEvent).
		OnP2CardActionTrigger(a.handleCardActionEvent)

	for {
		err := a.runLongConn(ctx, dispatcher)
		if ctx.Err() != nil {
			a.wg.Wait()
			return nil
		}
		if err != nil {
			var clientErr *larkws.ClientError
			if errors.As(err, &clientErr) {
				cancel()
				a.wg.Wait()
				return fmt.Errorf("feishu long connection: %w", err)
			}
			slog.Warn("feishu: long connection dropped, retrying", "error", err, "retry_in", a.currentReconnectInterval())
		}
		if !sleepWithContext(ctx, a.currentReconnectInterval()) {
			a.wg.Wait()
			return nil
		}
	}
}

func sleepWithContext(ctx context.Context, d time.Duration) bool {
	return sleepCtx(ctx, d)
}

func (a *FeishuAdapter) runLongConnection(ctx context.Context, dispatcher *larkdispatcher.EventDispatcher) error {
	connURL, err := a.getLongConnURL(ctx)
	if err != nil {
		return err
	}

	u, err := url.Parse(connURL)
	if err != nil {
		return fmt.Errorf("feishu long connection parse URL: %w", err)
	}
	serviceID, _ := strconv.ParseInt(u.Query().Get(larkws.ServiceID), 10, 32)

	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, connURL, nil)
	if err != nil {
		if resp != nil {
			return parseFeishuWSError(resp)
		}
		return fmt.Errorf("feishu long connection dial: %w", err)
	}
	defer conn.Close()

	connCtx, cancelPing := context.WithCancel(ctx)
	defer cancelPing()

	a.setConn(conn)
	defer a.clearConn(conn)

	slog.Info("feishu adapter: long connection established", "url", u.Redacted())

	pingErrCh := make(chan error, 1)
	go a.pingLoop(connCtx, conn, int32(serviceID), pingErrCh)

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
			slog.Warn("feishu: ignoring non-binary websocket message", "message_type", mt)
			continue
		}
		if err := a.handleWSFrame(ctx, conn, dispatcher, msg); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

func (a *FeishuAdapter) getLongConnURL(ctx context.Context) (string, error) {
	fc := a.feishuConfig()
	if fc == nil {
		return "", fmt.Errorf("feishu: config missing")
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
		return "", larkws.NewServerError(endpointResp.Code, endpointResp.Msg)
	default:
		return "", larkws.NewClientError(endpointResp.Code, endpointResp.Msg)
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
	msg := resp.Header.Get(larkws.HeaderHandshakeMsg)
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
			return
		}
	}
}

func (a *FeishuAdapter) handleWSFrame(ctx context.Context, conn *websocket.Conn, dispatcher *larkdispatcher.EventDispatcher, msg []byte) error {
	var frame larkws.Frame
	if err := frame.Unmarshal(msg); err != nil {
		return fmt.Errorf("feishu long connection unmarshal frame: %w", err)
	}

	switch larkws.FrameType(frame.Method) {
	case larkws.FrameTypeControl:
		a.handleControlFrame(frame)
		return nil
	case larkws.FrameTypeData:
		return a.handleDataFrame(ctx, conn, dispatcher, frame)
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
			slog.Warn("feishu: unmarshal client config failed", "error", err)
			return
		}
		a.applyWSClientConfig(&conf)
	}
}

func (a *FeishuAdapter) handleDataFrame(ctx context.Context, conn *websocket.Conn, dispatcher *larkdispatcher.EventDispatcher, frame larkws.Frame) error {
	hs := larkws.Headers(frame.Headers)
	sum := hs.GetInt(larkws.HeaderSum)
	seq := hs.GetInt(larkws.HeaderSeq)
	msgID := hs.GetString(larkws.HeaderMessageID)
	traceID := hs.GetString(larkws.HeaderTraceID)
	msgType := hs.GetString(larkws.HeaderType)

	payload := frame.Payload
	if sum > 1 {
		payload = a.combinePayload(msgID, sum, seq, payload)
		if payload == nil {
			return nil
		}
	}

	var err error
	var rsp any
	start := time.Now()
	switch larkws.MessageType(msgType) {
	case larkws.MessageTypeEvent, larkws.MessageTypeCard:
		rsp, err = dispatcher.Do(ctx, payload)
		if err != nil {
			var notFoundErr *larkdispatcher.NotFoundEventHandlerErr
			if errors.As(err, &notFoundErr) {
				slog.Debug("feishu: no handler for subscribed event", "message_id", msgID, "trace_id", traceID, "error", err)
				err = nil
			}
		}
	default:
		return nil
	}
	bizRT := time.Since(start).Milliseconds()
	hs.Add(larkws.HeaderBizRt, strconv.FormatInt(bizRT, 10))

	resp := larkws.NewResponseByCode(http.StatusOK)
	if err != nil {
		resp = larkws.NewResponseByCode(http.StatusInternalServerError)
	} else if rsp != nil {
		resp.Data, err = json.Marshal(rsp)
		if err != nil {
			resp = larkws.NewResponseByCode(http.StatusInternalServerError)
		}
	}

	if err != nil {
		slog.Error("feishu: handle long connection message failed", "message_type", msgType, "message_id", msgID, "trace_id", traceID, "error", err)
	}

	respPayload, marshalErr := json.Marshal(resp)
	if marshalErr != nil {
		return fmt.Errorf("feishu long connection marshal response: %w", marshalErr)
	}
	frame.Payload = respPayload
	frame.Headers = hs
	bs, marshalFrameErr := frame.Marshal()
	if marshalFrameErr != nil {
		return fmt.Errorf("feishu long connection marshal frame: %w", marshalFrameErr)
	}
	if writeErr := a.writeBinary(conn, bs); writeErr != nil {
		return fmt.Errorf("feishu long connection write response: %w", writeErr)
	}
	return nil
}

func (a *FeishuAdapter) combinePayload(msgID string, sum, seq int, chunk []byte) []byte {
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
		buf = feishuFragmentBuffer{
			parts:     make([][]byte, sum),
			expiresAt: now.Add(feishuFragmentTTL),
		}
	}
	if seq < 0 || seq >= sum {
		delete(a.fragments, msgID)
		return nil
	}
	buf.parts[seq] = chunk
	buf.expiresAt = now.Add(feishuFragmentTTL)
	a.fragments[msgID] = buf

	total := 0
	for _, part := range buf.parts {
		if len(part) == 0 {
			return nil
		}
		total += len(part)
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
	return conn.WriteMessage(websocket.BinaryMessage, data)
}

func (a *FeishuAdapter) setConn(conn *websocket.Conn) {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	a.conn = conn
}

func (a *FeishuAdapter) clearConn(conn *websocket.Conn) {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	if conn == nil || a.conn == conn {
		a.conn = nil
	}
}

func (a *FeishuAdapter) closeConn() {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	if a.conn != nil {
		_ = a.conn.Close()
		a.conn = nil
	}
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

func (a *FeishuAdapter) SendInteractiveWithHandle(chatID string, card map[string]any) (*InteractiveCardHandle, error) {
	return a.sendMessage(chatID, "interactive", card, false)
}

func (a *FeishuAdapter) UpdateInteractiveCard(handle InteractiveCardHandle, card map[string]any) error {
	if strings.TrimSpace(handle.MessageID) == "" {
		if strings.TrimSpace(handle.Token) != "" {
			return fmt.Errorf("feishu card callback token can only be used in the immediate callback response")
		}
		return fmt.Errorf("feishu card update handle has no message_id")
	}
	return a.updateInteractiveMessage(handle.MessageID, card, false)
}

// Disconnect signals the adapter to stop and shuts down the long connection.
// dedupe is closed by Connect's deferred cleanup; we only need to cancel here.
func (a *FeishuAdapter) Disconnect() {
	if a.cancel != nil {
		a.cancel()
	}
	a.closeConn()
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

func (a *FeishuAdapter) handleMessageEvent(_ context.Context, event *larkim.P2MessageReceiveV1) error {
	if event == nil || event.Event == nil || event.Event.Message == nil || event.Event.Sender == nil || event.Event.Sender.SenderId == nil {
		return nil
	}

	message := event.Event.Message
	messageType := derefString(message.MessageType)
	contentRaw := derefString(message.Content)
	contentText, err := parseFeishuMessageText(messageType, contentRaw)
	if err != nil {
		slog.Error("feishu: failed to parse message content", "error", err, "msg_type", messageType, "content", contentRaw)
		return nil
	}
	if strings.TrimSpace(contentText) == "" {
		slog.Debug("feishu: ignoring empty message", "msg_type", messageType)
		return nil
	}

	fc := a.feishuConfig()
	if fc == nil {
		return nil
	}
	appID := fc.AppID
	senderOpenID := derefString(event.Event.Sender.SenderId.OpenId)
	chatID := derefString(message.ChatId)
	messageID := derefString(message.MessageId)
	conversationID := chatID
	if threadID := derefString(message.ThreadId); threadID != "" {
		conversationID = threadID
	}

	if !fc.IsOpenIDAllowed(senderOpenID) {
		slog.Debug("feishu: message from non-allowed open_id, ignoring", "open_id", senderOpenID, "chat_id", chatID)
		return nil
	}

	dedupeKey := fmt.Sprintf(feishuDedupeKeyFmt, appID, chatID, messageID)
	if !a.dedupe.TryBegin(dedupeKey) {
		slog.Debug("feishu: duplicate message, skipping", "message_id", messageID, "chat_id", chatID)
		return nil
	}

	slog.Info("feishu: received message", "chat_id", chatID, "open_id", senderOpenID, "message_id", messageID, "content", contentText)
	msg := IncomingMessage{IMType: "feishu", ChatID: chatID, SenderID: senderOpenID, MessageID: messageID, ConversationID: conversationID, Text: contentText, AppID: appID}
	if a.enqueueIncomingMessage(msg) {
		a.dedupe.Commit(dedupeKey)
	} else {
		a.dedupe.Release(dedupeKey)
	}
	return nil
}

func parseFeishuMessageText(messageType, contentRaw string) (string, error) {
	var content FeishuMessageContent
	if err := json.Unmarshal([]byte(contentRaw), &content); err != nil {
		return "", err
	}
	switch messageType {
	case "text":
		return content.Text, nil
	case "post":
		var lines []string
		for _, line := range content.Content {
			var b strings.Builder
			for _, item := range line {
				switch item.Tag {
				case "text", "a":
					b.WriteString(item.Text)
				case "at":
					if item.UserName != "" {
						b.WriteString("@")
						b.WriteString(item.UserName)
					}
				}
			}
			lines = append(lines, b.String())
		}
		return strings.TrimSpace(strings.Join(lines, "\n")), nil
	default:
		return "", nil
	}
}

func (a *FeishuAdapter) handleCardActionEvent(_ context.Context, event *larkcallback.CardActionTriggerEvent) (*larkcallback.CardActionTriggerResponse, error) {
	if event == nil || event.Event == nil || event.Event.Action == nil || event.Event.Context == nil {
		return nil, nil
	}

	chatID := event.Event.Context.OpenChatID
	fc := a.feishuConfig()
	if fc == nil {
		return nil, nil
	}
	senderOpenID := ""
	if event.Event.Operator != nil {
		senderOpenID = event.Event.Operator.OpenID
	}
	if !fc.IsOpenIDAllowed(senderOpenID) {
		slog.Debug("feishu: card action from non-allowed open_id, ignoring", "open_id", senderOpenID, "chat_id", chatID)
		return nil, nil
	}

	requestID, _ := event.Event.Action.Value["request_id"].(string)
	actionType, _ := event.Event.Action.Value["type"].(string)
	action, _ := event.Event.Action.Value["action"].(string)
	value, _ := event.Event.Action.Value["value"].(string)
	contextIMType, _ := event.Event.Action.Value["im_type"].(string)
	contextChatID, _ := event.Event.Action.Value["chat_id"].(string)
	if requestID == "" || actionType == "" || contextChatID == "" || contextIMType != "feishu" || contextChatID != chatID {
		slog.Warn("feishu: stale/wrong-context card action ignored", "request_id", requestID, "chat_id", chatID, "context_chat_id", contextChatID, "action_type", actionType)
		return nil, nil
	}
	if !isValidFeishuCardAction(actionType, action, value) {
		slog.Warn("feishu: invalid card action ignored", "request_id", requestID, "chat_id", chatID, "action_type", actionType, "action", action)
		return nil, nil
	}

	dedupeKey := fmt.Sprintf(feishuCardActionFmt, fc.AppID, chatID, requestID+"|"+actionType+"|"+action+"|"+value)
	if !a.dedupe.TryBegin(dedupeKey) {
		slog.Debug("feishu: duplicate card action, skipping", "request_id", requestID, "action_type", actionType, "chat_id", chatID)
		return nil, nil
	}

	msg := IncomingMessage{IMType: "feishu", ChatID: chatID, SenderID: senderOpenID, MessageID: requestID + ":" + actionType + ":" + action + ":" + value, ConversationID: chatID, AppID: fc.AppID, InternalAction: &InternalAction{Type: actionType, Action: action, RequestID: requestID, Value: value, Handle: InteractiveCardHandle{MessageID: event.Event.Context.OpenMessageID, Token: event.Event.Token}}}
	if a.enqueueIncomingMessage(msg) {
		a.dedupe.Commit(dedupeKey)
	} else {
		a.dedupe.Release(dedupeKey)
	}
	return &larkcallback.CardActionTriggerResponse{Toast: &larkcallback.Toast{Type: "info", Content: "已收到，正在处理..."}, Card: &larkcallback.Card{Type: "raw", Data: buildFeishuResolvedCard("Processing", "⌛ Your response was received and is being processed.", "blue")}}, nil
}

func isValidFeishuCardAction(actionType, action, value string) bool {
	switch actionType {
	case "confirm":
		return action == "allow" || action == "deny"
	case "question":
		return action == "answer" && strings.TrimSpace(value) != ""
	default:
		return false
	}
}

func (a *FeishuAdapter) enqueueIncomingMessage(msg IncomingMessage) bool {
	select {
	case a.messageQueue <- msg:
		return true
	default:
		slog.Error("feishu: message queue full, dropping message", "chat_id", msg.ChatID, "message_id", msg.MessageID)
		return false
	}
}

// --- Access token management ---

func (a *FeishuAdapter) getAccessToken() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.accessToken != "" && time.Now().Before(a.tokenExpireAt) {
		return a.accessToken, nil
	}

	fc := a.feishuConfig()
	if fc == nil {
		return "", fmt.Errorf("feishu: config missing")
	}
	body, err := json.Marshal(map[string]string{
		"app_id":     fc.AppID,
		"app_secret": fc.AppSecret,
	})
	if err != nil {
		return "", fmt.Errorf("marshal feishu token request: %w", err)
	}
	resp, err := a.httpClient.Post(
		feishuOpenBaseURL+"/open-apis/auth/v3/app_access_token/internal",
		"application/json",
		bytes.NewReader(body),
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
	_, err := a.sendMessage(chatID, "text", map[string]string{"text": text}, retry)
	return err
}

// doFeishuJSONRequest sends a JSON request to a Feishu OpenAPI endpoint and
// decodes the JSON envelope. On 99991663 (access token expired) it clears the
// cached token and retries once.
func (a *FeishuAdapter) doFeishuJSONRequest(method, url string, reqBody any, result *FeishuSendResponse, retry bool) error {
	token, err := a.getAccessToken()
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	if result.Code != 0 {
		if result.Code == 99991663 && !retry {
			slog.Warn("feishu: access token expired, refreshing")
			a.mu.Lock()
			a.accessToken = ""
			a.tokenExpireAt = time.Time{}
			a.mu.Unlock()
			return a.doFeishuJSONRequest(method, url, reqBody, result, true)
		}
		return fmt.Errorf("feishu API error: code=%d msg=%s", result.Code, result.Msg)
	}
	return nil
}

func (a *FeishuAdapter) sendMessage(chatID, msgType string, content any, retry bool) (*InteractiveCardHandle, error) {
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
	if err := a.doFeishuJSONRequest(http.MethodPost, url, reqBody, &result, retry); err != nil {
		return nil, err
	}
	return &InteractiveCardHandle{MessageID: result.Data.MessageID}, nil
}

func (a *FeishuAdapter) updateInteractiveMessage(messageID string, card map[string]any, retry bool) error {
	contentJSON, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("marshal interactive card content: %w", err)
	}
	reqBody := map[string]string{"content": string(contentJSON)}
	requestURL := feishuOpenBaseURL + "/open-apis/im/v1/messages/" + url.PathEscape(messageID)
	var result FeishuSendResponse
	return a.doFeishuJSONRequest(http.MethodPatch, requestURL, reqBody, &result, retry)
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
