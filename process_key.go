package main

import "strings"

// processKey identifies an isolated control-plane session.
//
// A single workspace may be bound to multiple IM channels (DM/group/chat). Each
// binding gets its own chord headless process and its own pinned session ID.
//
// Format: workspaceID|imType|chatID
//
// Note: chatID is treated as an opaque identifier from the IM adapter.
type processKey struct {
	workspaceID string
	imType      string
	chatID      string
}

func (k processKey) String() string {
	return compositeKey(k.workspaceID, k.imType, k.chatID)
}

func parseProcessKey(s string) (workspaceID, imType, chatID string) {
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
// gateway for cache keys (process keys, card handle keys, dedupe keys, …).
// Inputs are expected to be opaque IDs that don't contain "|"; the joined form
// is intended only as an in-memory map key, not for round-tripping.
func compositeKey(parts ...string) string {
	return strings.Join(parts, "|")
}
