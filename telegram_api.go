package main

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
	"strings"
	"time"
)

const (
	telegramAPIBaseURL       = "https://api.telegram.org"
	telegramResponseMaxBytes = 4 << 20
	telegramRequestTimeout   = 15 * time.Second
	telegramPollTimeout      = 35 * time.Second
)

type telegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}
type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
type telegramEntity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}
type telegramMessage struct {
	MessageID          int64            `json:"message_id"`
	From               *telegramUser    `json:"from,omitempty"`
	Chat               telegramChat     `json:"chat"`
	Date               int64            `json:"date"`
	Text               string           `json:"text"`
	Entities           []telegramEntity `json:"entities"`
	SenderChat         *telegramChat    `json:"sender_chat,omitempty"`
	MessageThreadID    int64            `json:"message_thread_id,omitempty"`
	IsTopicMessage     bool             `json:"is_topic_message,omitempty"`
	ReplyToMessage     *telegramMessage `json:"reply_to_message,omitempty"`
	hasMessageThreadID bool
}
type telegramUpdate struct {
	UpdateID       int64            `json:"update_id"`
	Message        *telegramMessage `json:"message,omitempty"`
	EditedMessage  *telegramMessage `json:"edited_message,omitempty"`
	ChannelPost    *telegramMessage `json:"channel_post,omitempty"`
	receivedDigest string
	captured       *telegramCapturedUpdate
}

func (u *telegramUpdate) UnmarshalJSON(data []byte) error {
	type plain telegramUpdate
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
	// hashes of pairing codes or other sensitive parameters.
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var original any
	if err := decoder.Decode(&original); err != nil {
		return err
	}
	if object, ok := original.(map[string]any); ok && u.Message != nil {
		if message, ok := object["message"].(map[string]any); ok {
			_, u.Message.hasMessageThreadID = message["message_thread_id"]
		}
	}
	canonical, err := json.Marshal(original)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	u.receivedDigest = hex.EncodeToString(digest[:])
	return nil
}

type telegramWebhookInfo struct {
	URL string `json:"url"`
}

func (w *telegramWebhookInfo) UnmarshalJSON(data []byte) error {
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

type telegramAPI interface {
	GetMe(context.Context) (telegramUser, error)
	GetWebhookInfo(context.Context) (telegramWebhookInfo, error)
	GetUpdates(context.Context, *int64, int) ([]telegramUpdate, error)
	SendMessage(context.Context, int64, string) (telegramMessage, error)
}

// No raw URL, token, API description, or transport error is retained here.
// Cause only contains local context errors or the shared unknown-outcome sentinel.
type telegramAPIError struct {
	Method       string
	Status, Code int
	Category     string
	RetryAfter   time.Duration
	Cause        error
}

func (e *telegramAPIError) Error() string {
	return fmt.Sprintf("telegram %s: %s (HTTP %d, API %d)", e.Method, e.Category, e.Status, e.Code)
}
func (e *telegramAPIError) Unwrap() error { return e.Cause }

type telegramHTTPAPI struct {
	token   string
	baseURL string
	client  *http.Client
}

func newTelegramAPI(token string) *telegramHTTPAPI {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 5 * time.Second
	return &telegramHTTPAPI{token: token, baseURL: telegramAPIBaseURL, client: messageHTTPClient(&http.Client{Transport: transport})}
}
func (a *telegramHTTPAPI) GetMe(ctx context.Context) (telegramUser, error) {
	var user telegramUser
	err := a.call(ctx, "getMe", struct{}{}, &user, false)
	if err == nil && (user.ID <= 0 || !user.IsBot || user.Username == "") {
		err = &telegramAPIError{Method: "getMe", Status: http.StatusOK, Category: "invalid_identity"}
	}
	return user, err
}
func (a *telegramHTTPAPI) GetWebhookInfo(ctx context.Context) (telegramWebhookInfo, error) {
	var result telegramWebhookInfo
	err := a.call(ctx, "getWebhookInfo", struct{}{}, &result, false)
	return result, err
}
func (a *telegramHTTPAPI) GetUpdates(ctx context.Context, offset *int64, timeout int) ([]telegramUpdate, error) {
	var result []telegramUpdate
	if timeout < 0 || timeout > 25 || (offset != nil && *offset < 0) {
		return nil, &telegramAPIError{Method: "getUpdates", Category: "invalid_request"}
	}
	payload := struct {
		Offset         *int64   `json:"offset,omitempty"`
		Timeout        int      `json:"timeout"`
		AllowedUpdates []string `json:"allowed_updates"`
	}{offset, timeout, []string{"message"}}
	err := a.call(ctx, "getUpdates", payload, &result, timeout > 0)
	return result, err
}
func (a *telegramHTTPAPI) SendMessage(ctx context.Context, chatID int64, text string) (telegramMessage, error) {
	var result telegramMessage
	if chatID == 0 || text == "" {
		return result, &telegramAPIError{Method: "sendMessage", Category: "invalid_request"}
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
		err = &telegramAPIError{Method: "sendMessage", Status: http.StatusOK, Category: "unknown_send", Cause: errDeliveryOutcomeUnknown}
	}
	return result, err
}
func (a *telegramHTTPAPI) call(parent context.Context, method string, payload, result any, longPoll bool) error {
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
			category, cause = "unknown_send", errDeliveryOutcomeUnknown
		}
		return &telegramAPIError{Method: method, Status: status, Code: code, Category: category, Cause: cause}
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
	resp, err := messageHTTPClient(a.client).Do(req)
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
		// 429 is safe to retry only with an explicit Telegram rejection and a
		// positive, representable delay. Bare HTTP 429 remains an unknown send.
		if category == "rate_limited" && (envelope.Parameters.RetryAfter <= 0 || envelope.Parameters.RetryAfter > int64((1<<63-1)/int64(time.Second))) {
			category = "api_rejected"
		}
		apiErr := &telegramAPIError{Method: method, Status: resp.StatusCode, Code: envelope.ErrorCode, Category: category}
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
