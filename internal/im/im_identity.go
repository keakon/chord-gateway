package im

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

const MaxScopedIDBytes = 2048
const maxScopedJSONBytes = 1536

func validIDPrefix(prefix string) bool {
	if len(prefix) < 2 || len(prefix) > 64 || prefix[len(prefix)-1] != ':' {
		return false
	}
	for _, c := range prefix[:len(prefix)-1] {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// EncodeScopedID encodes identities without delimiter ambiguity.
func EncodeScopedID(prefix string, parts ...string) (string, error) {
	if !validIDPrefix(prefix) || len(parts) == 0 {
		return "", errors.New("invalid scoped identity")
	}
	for _, p := range parts {
		if p == "" || !utf8.ValidString(p) {
			return "", errors.New("invalid identity component")
		}
	}
	b, err := json.Marshal(parts)
	if err != nil {
		return "", err
	}
	value := prefix + base64.RawURLEncoding.EncodeToString(b)
	if len(b) > maxScopedJSONBytes || len(value) > MaxScopedIDBytes {
		return "", errors.New("scoped identity too long")
	}
	return value, nil
}

func DecodeScopedID(value, prefix string, count int) ([]string, error) {
	if count < 1 || !validIDPrefix(prefix) || !strings.HasPrefix(value, prefix) || len(value) > MaxScopedIDBytes {
		return nil, errors.New("invalid scoped identity")
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil || len(b) > maxScopedJSONBytes {
		return nil, errors.New("invalid identity encoding")
	}
	var parts []string
	if json.Unmarshal(b, &parts) != nil || len(parts) != count {
		return nil, errors.New("invalid identity components")
	}
	canonical, err := EncodeScopedID(prefix, parts...)
	if err != nil || canonical != value {
		return nil, errors.New("noncanonical scoped identity")
	}
	return parts, nil
}

// AcquireIMAccountLock excludes other local gateways regardless of state directory.
func AcquireIMAccountLock(platform, accountID string) (*InstanceLocks, error) {
	return AcquireIMAccountLocks(platform, accountID)
}

// AcquireIMAccountLocks excludes every receiver for the given platform scopes.
// Adapters that must exclude more than one scope per platform (Slack excludes
// both the bot account and the app) go through here so the key format stays in
// one place.
func AcquireIMAccountLocks(platform string, accountIDs ...string) (*InstanceLocks, error) {
	keys := make([]string, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		key, err := EncodeScopedID("im-account-v1:", platform, accountID)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return &InstanceLocks{}, nil
	}
	return AcquireInstanceLocks(keys)
}

func SupportedIMPlatform(platform string) bool {
	switch platform {
	case "wechat", "feishu", "telegram", "discord", "wecom", "dingtalk", "slack":
		return true
	}
	return false
}
