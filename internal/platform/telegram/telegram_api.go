package telegram

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

const (
	telegramAPIBaseURL       = "https://api.telegram.org"
	telegramResponseMaxBytes = 4 << 20
	telegramRequestTimeout   = 15 * time.Second
	// Telegram holds a long poll open until its own server-side timeout, so the
	// client read is bounded by the poll length plus a network allowance rather
	// than by trusting the server to answer. Without it a stalled connection
	// would hold the poll loop open indefinitely.
	telegramLongPollAllowance = 10 * time.Second
)

// telegramPollTimeout is a var so a test can shrink the bound instead of
// waiting out the real allowance.
var telegramPollTimeout = telegramLongPollSeconds*time.Second + telegramLongPollAllowance

type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
type Entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}
type Message struct {
	MessageID          int64    `json:"message_id"`
	From               *User    `json:"from,omitempty"`
	Chat               Chat     `json:"chat"`
	Date               int64    `json:"date"`
	Text               string   `json:"text"`
	Entities           []Entity `json:"entities"`
	SenderChat         *Chat    `json:"sender_chat,omitempty"`
	MessageThreadID    int64    `json:"message_thread_id,omitempty"`
	IsTopicMessage     bool     `json:"is_topic_message,omitempty"`
	ReplyToMessage     *Message `json:"reply_to_message,omitempty"`
	hasMessageThreadID bool
	hasMedia           bool
}
type Update struct {
	UpdateID       int64    `json:"update_id"`
	Message        *Message `json:"message,omitempty"`
	EditedMessage  *Message `json:"edited_message,omitempty"`
	ChannelPost    *Message `json:"channel_post,omitempty"`
	receivedDigest string
	captured       *telegramCapturedUpdate
}

func (u *Update) UnmarshalJSON(data []byte) error {
	type plain Update
	var wire struct {
		*plain
		UpdateID *int64 `json:"update_id"`
	}
	wire.plain = (*plain)(u)
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.UpdateID == nil || *wire.UpdateID < 0 {
		return errors.New("missing or invalid update identity")
	}
	u.UpdateID = *wire.UpdateID
	// This fingerprint stays in memory, including for controls and unsupported
	// updates. It preserves a frozen capture on redelivery without persisting
	// hashes of pairing codes or other sensitive parameters. Telegram serializes
	// one update identically on every redelivery, so the received bytes are the
	// identity; re-encoding them into canonical form would only add two more
	// full passes over the update.
	digest := sha256.Sum256(data)
	u.receivedDigest = hex.EncodeToString(digest[:])
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	if u.Message != nil {
		if message, ok := object["message"].(map[string]any); ok {
			_, u.Message.hasMessageThreadID = message["message_thread_id"]
			u.Message.hasMedia = telegramHasMediaPayload(message)
		}
	}
	return nil
}

type WebhookInfo struct {
	URL string `json:"url"`
}

// telegramMediaFields are the Update message shapes whose payload is not text.
// Telegram also copies their caption into `text`, so presence alone decides:
// any of these fields makes the update a media update and never a text task.
var telegramMediaFields = []string{
	"photo", "document", "video", "audio", "voice", "animation", "sticker",
	"video_note", "contact", "location", "venue", "poll", "dice", "game",
	"invoice", "successful_payment", "story", "passport_data", "web_app_data",
}

func telegramHasMediaPayload(message map[string]any) bool {
	for _, field := range telegramMediaFields {
		if _, present := message[field]; present {
			return true
		}
	}
	return false
}

func (w *WebhookInfo) UnmarshalJSON(data []byte) error {
	var wire struct {
		URL *string `json:"url"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.URL == nil {
		return errors.New("missing webhook URL")
	}
	w.URL = *wire.URL
	return nil
}

type API interface {
	GetMe(context.Context) (User, error)
	GetWebhookInfo(context.Context) (WebhookInfo, error)
	GetUpdates(context.Context, *int64, int) ([]Update, error)
	SendMessage(context.Context, int64, string) (Message, error)
}

// ingressMethods are the read-only methods whose answer describes the
// poll loop rather than a delivery, so their rejection is never transient.
var ingressMethods = map[string]bool{"getMe": true, "getWebhookInfo": true, "getUpdates": true}

// APIError retains no raw URL, token, API description, or transport error.
// Cause only contains local context errors or the shared unknown-outcome
// sentinel.
type APIError struct {
	Method       string
	Status, Code int
	Category     string
	RetryAfter   time.Duration
	Cause        error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s: %s (HTTP %d, API %d)", e.Method, e.Category, e.Status, e.Code)
}
func (e *APIError) Unwrap() error { return e.Cause }

type httpAPI struct {
	token   string
	baseURL string
	client  *http.Client
}

func NewAPI(token string, proxyFunc func(*http.Request) (*url.URL, error)) *httpAPI {
	transport := im.ProxiedTransport(proxyFunc)
	transport.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 5 * time.Second
	return &httpAPI{token: token, baseURL: telegramAPIBaseURL, client: im.MessageHTTPClient(&http.Client{Transport: transport})}
}
func (a *httpAPI) GetMe(ctx context.Context) (User, error) {
	var user User
	err := a.call(ctx, "getMe", struct{}{}, &user, false)
	if err == nil && (user.ID <= 0 || !user.IsBot || user.Username == "") {
		err = &APIError{Method: "getMe", Status: http.StatusOK, Category: "invalid_identity"}
	}
	return user, err
}
func (a *httpAPI) GetWebhookInfo(ctx context.Context) (WebhookInfo, error) {
	var result WebhookInfo
	err := a.call(ctx, "getWebhookInfo", struct{}{}, &result, false)
	return result, err
}
func (a *httpAPI) GetUpdates(ctx context.Context, offset *int64, timeout int) ([]Update, error) {
	var result []Update
	if timeout < 0 || timeout > 25 || (offset != nil && *offset < 0) {
		return nil, &APIError{Method: "getUpdates", Category: "invalid_request"}
	}
	payload := struct {
		Offset         *int64   `json:"offset,omitempty"`
		Timeout        int      `json:"timeout"`
		Limit          int      `json:"limit"`
		AllowedUpdates []string `json:"allowed_updates"`
	}{offset, timeout, telegramPollLimit, []string{telegramAllowedUpdates}}
	err := a.call(ctx, "getUpdates", payload, &result, timeout > 0)
	return result, err
}
func (a *httpAPI) SendMessage(ctx context.Context, chatID int64, text string) (Message, error) {
	var result Message
	if chatID == 0 || text == "" {
		return result, &APIError{Method: "sendMessage", Category: "invalid_request"}
	}
	payload := struct {
		ChatID             int64  `json:"chat_id"`
		Text               string `json:"text"`
		LinkPreviewOptions struct {
			IsDisabled bool `json:"is_disabled"`
		} `json:"link_preview_options"`
	}{ChatID: chatID, Text: text}
	payload.LinkPreviewOptions.IsDisabled = true
	err := a.call(ctx, "sendMessage", payload, &result, false)
	if err == nil && (result.MessageID <= 0 || result.Chat.ID != chatID) {
		err = &APIError{Method: "sendMessage", Status: http.StatusOK, Category: "unknown_send", Cause: im.ErrDeliveryOutcomeUnknown}
	}
	return result, err
}
func (a *httpAPI) call(parent context.Context, method string, payload, result any, longPoll bool) error {
	sending := method == "sendMessage"
	fail := func(status, code int, category string, cause error) error {
		switch status {
		case http.StatusNotFound:
			if method == "getMe" {
				category = "credentials"
			}
		case http.StatusUnauthorized:
			category = "credentials"
		case http.StatusForbidden:
			category = "forbidden"
		case http.StatusConflict:
			category = "poller_conflict"
		}
		if sending && (category == "network" || category == "invalid_response") {
			category, cause = "unknown_send", im.ErrDeliveryOutcomeUnknown
		}
		return &APIError{Method: method, Status: status, Code: code, Category: category, Cause: cause}
	}
	if parent == nil {
		return fail(0, 0, "invalid_request", nil)
	}
	if err := parent.Err(); err != nil {
		return fail(0, 0, "cancelled", err)
	}
	timeout := telegramRequestTimeout
	if longPoll {
		timeout = telegramPollTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	data, err := json.Marshal(payload)
	if err != nil {
		return fail(0, 0, "invalid_request", nil)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.baseURL, "/")+"/bot"+a.token+"/"+method, bytes.NewReader(data))
	if err != nil {
		return fail(0, 0, "invalid_request", nil)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return fail(0, 0, "network", ctx.Err())
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, telegramResponseMaxBytes+1))
	if err != nil || len(data) > telegramResponseMaxBytes {
		return fail(resp.StatusCode, 0, "invalid_response", nil)
	}
	var envelope struct {
		OK         *bool           `json:"ok"`
		Result     json.RawMessage `json:"result"`
		ErrorCode  int             `json:"error_code"`
		Parameters struct {
			RetryAfter      int64 `json:"retry_after"`
			MigrateToChatID int64 `json:"migrate_to_chat_id"`
		} `json:"parameters"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.OK == nil {
		return fail(resp.StatusCode, 0, "invalid_response", nil)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusConflict || (method == "getMe" && resp.StatusCode == http.StatusNotFound) {
		return fail(resp.StatusCode, envelope.ErrorCode, "api_rejected", nil)
	}
	if !*envelope.OK {
		category := "api_rejected"
		switch envelope.ErrorCode {
		case 404:
			if method == "getMe" {
				category = "credentials"
			}
		case 401:
			category = "credentials"
		case 403:
			category = "forbidden"
		case 409:
			category = "poller_conflict"
		case 429:
			category = "rate_limited"
		}
		if envelope.Parameters.MigrateToChatID != 0 {
			category = "chat_migrated"
		}
		// A send is safe to retry a 429 only with an explicit Telegram rejection
		// and a positive, representable delay; a bare 429 leaves the send
		// outcome unknown. Read-only methods never change state, so they keep
		// rate_limited and back off instead of turning into the fatal
		// method_rejected the next block derives.
		if category == "rate_limited" && !ingressMethods[method] &&
			(envelope.Parameters.RetryAfter <= 0 || envelope.Parameters.RetryAfter > int64((1<<63-1)/int64(time.Second))) {
			category = "api_rejected"
		}
		// getMe, getWebhookInfo and getUpdates only describe the poll loop
		// itself. A non-429 ok:false from them is a method-level refusal that
		// never self-heals -- a stale proxy answering 200 with an HTML or JSON
		// error body looks exactly like this -- so it must be fatal rather than
		// retried at the backoff cap forever.
		if category == "api_rejected" && ingressMethods[method] {
			category = "method_rejected"
		}
		apiErr := &APIError{Method: method, Status: resp.StatusCode, Code: envelope.ErrorCode, Category: category}
		if category == "rate_limited" {
			apiErr.RetryAfter = time.Duration(envelope.Parameters.RetryAfter) * time.Second
		}
		return apiErr
	}
	if resp.StatusCode != http.StatusOK || len(envelope.Result) == 0 || bytes.Equal(envelope.Result, []byte("null")) {
		return fail(resp.StatusCode, envelope.ErrorCode, "invalid_response", nil)
	}
	if json.Unmarshal(envelope.Result, result) != nil {
		return fail(resp.StatusCode, 0, "invalid_response", nil)
	}
	return nil
}
