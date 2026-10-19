package main

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/keakon/chord-gateway/config"
)

// dingtalkIdentityError contains only a fixed, safe classification. Callback
// data must never appear in diagnostics, including rejected input.
type dingtalkIdentityError struct{ Category string }

func (e *dingtalkIdentityError) Error() string { return e.Category }

// Identity refusal categories compared across files; keep them as constants so
// the agreement between the producer and the reason mapping stays checkable.
const (
	dingtalkCategoryTenantMismatch = "tenant_mismatch"
	dingtalkCategorySourceTime     = "source_time_out_of_range"
)

func (e *dingtalkIdentityError) Terminal() bool { return e.Category == dingtalkCategoryTenantMismatch }

func dingtalkIdentityFailure(category string) error {
	return &dingtalkIdentityError{Category: category}
}

func dingtalkAccountID(cfg *config.DingtalkConfig) (string, error) {
	if cfg == nil {
		return "", dingtalkIdentityFailure("invalid_configuration")
	}
	account, err := EncodeScopedID("account-v1:", cfg.CorpID, cfg.ClientID, cfg.RobotCode)
	if err != nil {
		return "", dingtalkIdentityFailure("invalid_configuration")
	}
	return account, nil
}

func dingtalkEncodeTarget(corp, kind, raw string) (string, error) {
	var scope string
	switch kind {
	case "private":
		scope = "dingtalk-dm"
	case "group":
		scope = "dingtalk-group"
	default:
		return "", dingtalkIdentityFailure("invalid_target")
	}
	target, err := EncodeScopedID("target-v1:", scope, corp, raw)
	if err != nil {
		return "", dingtalkIdentityFailure("invalid_target")
	}
	return target, nil
}

func dingtalkDecodeTarget(corp, target string) (kind, raw string, err error) {
	parts, err := DecodeScopedID(target, "target-v1:", 3)
	if err != nil || parts[1] != corp {
		return "", "", dingtalkIdentityFailure("invalid_target")
	}
	switch parts[0] {
	case "dingtalk-dm":
		kind = "private"
	case "dingtalk-group":
		kind = "group"
	default:
		return "", "", dingtalkIdentityFailure("invalid_target")
	}
	return kind, parts[2], nil
}

// Both locks are acquired together in stable order. The client resource excludes
// even processes configured with a different (incorrect) enterprise or robot.
func dingtalkLockResources(cfg *config.DingtalkConfig) ([]string, error) {
	account, err := dingtalkAccountID(cfg)
	if err != nil {
		return nil, err
	}
	client, err := EncodeScopedID("dingtalk-client-v1:", cfg.ClientID)
	if err != nil {
		return nil, dingtalkIdentityFailure("invalid_configuration")
	}
	accountLock, err := EncodeScopedID("im-account-v1:", "dingtalk", account)
	if err != nil {
		return nil, dingtalkIdentityFailure("invalid_configuration")
	}
	resources := []string{client, accountLock}
	sort.Strings(resources)
	return resources, nil
}

func acquireDingtalkAccountLocks(cfg *config.DingtalkConfig) (*instanceLocks, error) {
	resources, err := dingtalkLockResources(cfg)
	if err != nil {
		return nil, err
	}
	return acquireInstanceLocks(resources)
}

type dingtalkCallback struct {
	MsgID            string `json:"msgId"`
	ConversationType string `json:"conversationType"`
	ConversationID   string `json:"conversationId"`
	SenderStaffID    string `json:"senderStaffId"`
	SenderCorpID     string `json:"senderCorpId"`
	ChatbotCorpID    string `json:"chatbotCorpId"`
	RobotCode        string `json:"robotCode"`
	MsgType          string `json:"msgtype"`
	IsInAtList       bool   `json:"isInAtList"`
	Text             struct {
		Content string `json:"content"`
	} `json:"text"`
}

func parseDingtalkCallback(payload string) (dingtalkCallback, error) {
	var callback dingtalkCallback
	object := strings.TrimSpace(payload)
	if len(object) == 0 || object[0] != '{' || !utf8.ValidString(payload) || json.Unmarshal([]byte(payload), &callback) != nil {
		return callback, dingtalkIdentityFailure("invalid_callback")
	}
	return callback, nil
}

func validateDingtalkCallbackIdentity(cfg *config.DingtalkConfig, callback *dingtalkCallback) error {
	if _, err := dingtalkAccountID(cfg); err != nil {
		return err
	}
	if callback == nil {
		return dingtalkIdentityFailure("invalid_callback")
	}
	if callback.ChatbotCorpID != cfg.CorpID || callback.RobotCode != cfg.RobotCode {
		return dingtalkIdentityFailure(dingtalkCategoryTenantMismatch)
	}
	if callback.SenderStaffID == "" || callback.SenderCorpID != cfg.CorpID {
		return dingtalkIdentityFailure("sender_identity_unsupported")
	}
	return nil
}

func validateDingtalkIdentityProbe(cfg *config.DingtalkConfig, callback *dingtalkCallback) error {
	if err := validateDingtalkCallbackIdentity(cfg, callback); err != nil {
		return err
	}
	if callback.ConversationType != "1" {
		return dingtalkIdentityFailure("identity_probe_requires_private")
	}
	return nil
}

func normalizeDingtalkCallback(cfg *config.DingtalkConfig, payload string) (IncomingMessage, error) {
	callback, err := parseDingtalkCallback(payload)
	if err != nil {
		return IncomingMessage{}, err
	}
	if err = validateDingtalkCallbackIdentity(cfg, &callback); err != nil {
		return IncomingMessage{}, err
	}
	if callback.MsgID == "" || len(callback.MsgID) > maxScopedIDBytes || !utf8.ValidString(callback.MsgID) {
		return IncomingMessage{}, dingtalkIdentityFailure("invalid_message_id")
	}
	var kind, raw string
	switch callback.ConversationType {
	case "1":
		kind, raw = "private", callback.SenderStaffID
	case "2":
		kind, raw = "group", callback.ConversationID
	default:
		return IncomingMessage{}, dingtalkIdentityFailure("unsupported_conversation")
	}
	target, err := dingtalkEncodeTarget(cfg.CorpID, kind, raw)
	if err != nil {
		return IncomingMessage{}, err
	}
	account, err := dingtalkAccountID(cfg)
	if err != nil {
		return IncomingMessage{}, err
	}
	msg := IncomingMessage{IMType: "dingtalk", AccountID: account, ChatKind: kind,
		ChatID: target, SenderID: callback.SenderStaffID, MessageID: "message:" + callback.MsgID,
	}
	// A permanent content rejection retains only authenticated business identity
	// so the shared ingress can commit a tombstone without persisting its body.
	if callback.MsgType != "text" {
		return msg, dingtalkIdentityFailure("unsupported_message")
	}
	if len(callback.Text.Content) > 64*1024 {
		return msg, dingtalkIdentityFailure("message_too_large")
	}
	if kind == "group" && !callback.IsInAtList {
		return msg, dingtalkIdentityFailure("group_mention_required")
	}
	// An empty or blank message is not a task; rejecting it here keeps the
	// shared dispatch from sending empty input to Chord (Telegram already
	// refuses empty text the same way).
	if strings.TrimSpace(callback.Text.Content) == "" {
		return msg, dingtalkIdentityFailure("invalid_input")
	}
	msg.Text = strings.TrimLeftFunc(callback.Text.Content, unicode.IsSpace)
	return msg, nil
}
