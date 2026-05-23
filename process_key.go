package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

const encodedProcessKeyPrefix = "v2:"

// processKey identifies an isolated control-plane session.
//
// A single workspace may be bound to multiple IM channels (DM/group/chat). Each
// binding gets its own chord headless process and its own pinned session ID.
//
// Encoded format: v2:<base64url(JSON [workspaceID, imType, chatID])>.
// Legacy workspaceID|imType|chatID keys are still accepted when parsing.
//
// Note: chatID is treated as an opaque identifier from the IM adapter.
type processKey struct {
	workspaceID string
	imType      string
	chatID      string
}

func (k processKey) String() string {
	payload, err := json.Marshal([]string{k.workspaceID, k.imType, k.chatID})
	if err != nil {
		return ""
	}
	return encodedProcessKeyPrefix + base64.RawURLEncoding.EncodeToString(payload)
}

func legacyProcessKeyString(workspaceID, imType, chatID string) string {
	return strings.Join([]string{workspaceID, imType, chatID}, "|")
}

func parseProcessKey(s string) (workspaceID, imType, chatID string) {
	if strings.HasPrefix(s, encodedProcessKeyPrefix) {
		payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, encodedProcessKeyPrefix))
		if err != nil {
			return "", "", ""
		}
		var parts []string
		if err := json.Unmarshal(payload, &parts); err != nil || len(parts) != 3 {
			return "", "", ""
		}
		return parts[0], parts[1], parts[2]
	}
	parts := strings.SplitN(s, "|", 3)
	if len(parts) != 3 {
		return "", "", ""
	}
	return parts[0], parts[1], parts[2]
}

func processLogContext(key string, state ControlState) string {
	workspaceID, imType, chatID := parseProcessKey(key)
	if workspaceID == "" && imType == "" && chatID == "" {
		return "key=" + key + " sid=" + state.SessionID
	}
	return "wid=" + workspaceID + " im=" + imType + " chat_id=" + chatID + " sid=" + state.SessionID
}

// compositeKey joins parts with the canonical "|" separator used across the
// gateway for cache keys that are not parsed back into opaque parts (card handle
// keys, dedupe keys, …).
func compositeKey(parts ...string) string {
	return strings.Join(parts, "|")
}
