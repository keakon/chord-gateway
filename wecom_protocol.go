package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	errWecomAccountMismatch = errors.New("wecom callback account mismatch")
	// errWecomUnsupportedCallback marks a callback that is deliberately not a
	// user text command (events, media, assistant/channel sources). It is
	// ignored without being counted as lost business input.
	errWecomUnsupportedCallback = errors.New("unsupported wecom callback")
	// errWecomInvalidCallback marks a callback that claimed to be user text but
	// failed validation. It is reported instead of vanishing silently.
	errWecomInvalidCallback = errors.New("invalid wecom callback")
	// errWecomSourceTimeOutOfRange is wrapped by errWecomInvalidCallback so a
	// skewed create_time is visible as its own metric reason.
	errWecomSourceTimeOutOfRange = errors.New("wecom callback create_time out of range")
	// errWecomEmptyText is wrapped by errWecomInvalidCallback so an empty or
	// blank message is reported as invalid_input, matching the other adapters.
	errWecomEmptyText = errors.New("wecom callback text is empty")
)

// wecomCallbackRejectionReason maps a validation failure to the bounded metric
// reason reported through the callback_dropped counter.
func wecomCallbackRejectionReason(err error) string {
	switch {
	case errors.Is(err, errWecomSourceTimeOutOfRange):
		return "source_time_out_of_range"
	case errors.Is(err, errWecomEmptyText):
		return "invalid_input"
	}
	return "invalid_callback"
}

// EncodeWecomTarget keeps private users and groups in distinct routing scopes.
func EncodeWecomTarget(kind, target string) (string, error) {
	var scope string
	switch kind {
	case "private":
		scope = "wecom-dm"
	case "group":
		scope = "wecom-group"
	default:
		return "", errors.New("invalid wecom target kind")
	}
	if strings.TrimSpace(target) == "" {
		return "", errors.New("invalid wecom target")
	}
	return EncodeScopedID("target-v1:", scope, target)
}

type wecomSendTarget struct {
	ChatID   string
	ChatType int // WeCom wire values: 1 = user, 2 = group.
}

// DecodeWecomTarget preserves the addressing mode for proactive sending.
func DecodeWecomTarget(chatID string) (wecomSendTarget, error) {
	parts, err := DecodeScopedID(chatID, "target-v1:", 2)
	if err != nil || (parts[0] != "wecom-dm" && parts[0] != "wecom-group") || strings.TrimSpace(parts[1]) == "" {
		return wecomSendTarget{}, errors.New("invalid wecom target")
	}
	chatType := 1
	if parts[0] == "wecom-group" {
		chatType = 2
	}
	return wecomSendTarget{ChatID: parts[1], ChatType: chatType}, nil
}

// NormalizeWecomCallback accepts the text callback contract from the pinned
// official SDK. In particular, chattype and chatid are top-level body fields;
// only msgtype "text" is a user command, so voice transcripts and mixed
// messages are not.
func NormalizeWecomCallback(raw []byte, account string, receivedAt time.Time) (IngressEnvelope, error) {
	if len(raw) > 1<<20 || !utf8.Valid(raw) || strings.TrimSpace(account) == "" || receivedAt.IsZero() {
		return IngressEnvelope{}, errWecomInvalidCallback
	}
	var frame struct {
		Command string `json:"cmd"`
		Headers struct {
			RequestID string `json:"req_id"`
		} `json:"headers"`
		Body struct {
			BotID      string          `json:"aibotid"`
			MessageID  string          `json:"msgid"`
			ChatType   string          `json:"chattype"`
			ChatID     string          `json:"chatid"`
			Type       string          `json:"msgtype"`
			Created    json.RawMessage `json:"create_time"`
			CompatInfo json.RawMessage `json:"compat_info"`
			From       struct {
				UserID string `json:"userid"`
			} `json:"from"`
			Text *struct {
				Content *string `json:"content"`
			} `json:"text"`
		} `json:"body"`
	}
	if json.Unmarshal(raw, &frame) != nil {
		return IngressEnvelope{}, errWecomInvalidCallback
	}
	if frame.Command != "aibot_msg_callback" {
		return IngressEnvelope{}, errWecomUnsupportedCallback
	}
	b := frame.Body
	if strings.TrimSpace(b.BotID) == "" {
		return IngressEnvelope{}, errWecomInvalidCallback
	}
	// Check the account before filtering message type: an identity mismatch is
	// fatal even when the callback is for a currently unsupported media type.
	if b.BotID != account {
		return IngressEnvelope{}, errWecomAccountMismatch
	}
	// compat_info is present -- even as JSON null -- whenever the callback is
	// not an ordinary text message from a real user. Detect key presence, not
	// the payload content.
	if b.CompatInfo != nil {
		return IngressEnvelope{}, errWecomUnsupportedCallback
	}
	// Type comes before per-field validation: a voice or mixed callback is
	// deliberately unsupported even when its other fields are incomplete, so it
	// must not be reported as malformed user text.
	if b.Type != "text" {
		return IngressEnvelope{}, errWecomUnsupportedCallback
	}
	for _, id := range []string{b.BotID, b.MessageID, b.From.UserID} {
		if strings.TrimSpace(id) == "" || len(id) > maxScopedIDBytes {
			return IngressEnvelope{}, errWecomInvalidCallback
		}
	}
	if len("message:")+len(b.MessageID) > maxScopedIDBytes || len(frame.Headers.RequestID) > maxScopedIDBytes || b.Text == nil || b.Text.Content == nil || len(*b.Text.Content) > 64<<10 {
		return IngressEnvelope{}, errWecomInvalidCallback
	}
	// An empty or blank message is not a task; rejecting it here keeps the
	// shared dispatch from sending empty input to Chord (Telegram already
	// refuses empty text the same way).
	if strings.TrimSpace(*b.Text.Content) == "" {
		return IngressEnvelope{}, fmt.Errorf("%w: %w", errWecomInvalidCallback, errWecomEmptyText)
	}
	kind, target := "", ""
	switch b.ChatType {
	case "single":
		kind, target = "private", b.From.UserID
	case "group":
		kind, target = "group", b.ChatID
	default:
		return IngressEnvelope{}, errWecomInvalidCallback
	}
	chatID, err := EncodeWecomTarget(kind, target)
	if err != nil {
		return IngressEnvelope{}, errWecomInvalidCallback
	}
	sourceTime, err := wecomSourceTime(b.Created, receivedAt)
	if err != nil {
		return IngressEnvelope{}, err
	}
	return IngressEnvelope{
		Message:    IncomingMessage{IMType: "wecom", AccountID: account, ChatKind: kind, ChatID: chatID, SenderID: b.From.UserID, MessageID: "message:" + b.MessageID, Text: *b.Text.Content},
		DeliveryID: frame.Headers.RequestID,
		SourceTime: sourceTime,
		ReceivedAt: receivedAt,
	}, nil
}

func wecomSourceTime(raw json.RawMessage, receivedAt time.Time) (time.Time, error) {
	if len(raw) == 0 {
		return receivedAt, nil
	}
	value := strings.TrimSpace(string(raw))
	if len(value) == 0 || (len(value) > 10 && len(value) != 13) {
		return time.Time{}, errWecomInvalidCallback
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return time.Time{}, errWecomInvalidCallback
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, errWecomInvalidCallback
	}
	if len(value) == 13 {
		n /= 1000
	}
	source := time.Unix(n, 0)
	// Reject out-of-range platform timestamps like every other adapter and
	// the shared store (-24h/+5m window) instead of clamping. This callback
	// never reaches the store, so the rejection is reported directly as
	// source_time_out_of_range instead of a store tombstone.
	if source.Before(receivedAt.Add(-24*time.Hour)) || source.After(receivedAt.Add(5*time.Minute)) {
		return time.Time{}, fmt.Errorf("%w: %w", errWecomInvalidCallback, errWecomSourceTimeOutOfRange)
	}
	return source, nil
}
