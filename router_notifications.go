package main

import "strings"

type deliveredPrompt struct {
	requestID string
	sessionID string
}

func (r *NotificationRouter) recordDeliveredPrompt(key, kind string, state ControlState) {
	var id string
	switch kind {
	case "confirm_request":
		if state.PendingConfirm != nil {
			id = state.PendingConfirm.RequestID
		}
	case "handoff_request":
		if state.PendingHandoff != nil {
			id = state.PendingHandoff.RequestID
		}
	}
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deliveredPrompts == nil {
		r.deliveredPrompts = make(map[string]deliveredPrompt)
	}
	// Keep only the latest prompt of each kind per binding, not a request history.
	r.deliveredPrompts[compositeKey(key, kind)] = deliveredPrompt{id, state.SessionID}
}

func (r *NotificationRouter) duplicatePromptNotification(key string, state ControlState) bool {
	n := state.LastNotification
	if n == nil || n.Reason != notificationReasonUserInputRequired {
		return false
	}
	var kind, id string
	// The current headless notification has no request ID or typed origin.
	// Only these exact companion messages identify a dedicated prompt. Unknown
	// messages must be delivered, even when the pending cache is stale.
	switch strings.TrimSpace(n.Message) {
	case "Chord: Permission confirmation required":
		kind = "confirm_request"
		if state.PendingConfirm != nil {
			id = state.PendingConfirm.RequestID
		}
	case "Chord: Handoff requires your decision":
		kind = "handoff_request"
		if state.PendingHandoff != nil {
			id = state.PendingHandoff.RequestID
		}
	default:
		return false
	}
	if id == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	prompt, ok := r.deliveredPrompts[compositeKey(key, kind)]
	return ok && prompt.requestID == id && prompt.sessionID == state.SessionID
}
