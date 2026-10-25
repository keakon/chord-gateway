package main

import (
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

// literal is still assignable, so new states must be added here rather than
// written inline.

type IngressRecord struct {
	// statusQuery grants one bounded count reserve to an authorized /status.
	// It is an admission-only capability, never granted by caller metadata.
	statusQuery          bool
	RecordID             string                   `json:"record_id"`
	Platform             string                   `json:"platform"`
	AccountID            string                   `json:"account_id"`
	ChatID               string                   `json:"chat_id"`
	SenderID             string                   `json:"sender_id"`
	BusinessID           string                   `json:"business_id"`
	SemanticID           string                   `json:"semantic_id,omitempty"`
	DeliveryID           string                   `json:"delivery_id,omitempty"`
	SourceTime           time.Time                `json:"source_time"`
	ReceivedAt           time.Time                `json:"received_at"`
	Sequence             uint64                   `json:"sequence"`
	Kind                 string                   `json:"kind"`
	State                im.IngressState          `json:"state"`
	Reason               string                   `json:"reason,omitempty"`
	Text                 string                   `json:"text,omitempty"`
	AuthorizationDigest  string                   `json:"authorization_digest"`
	RouteRevision        string                   `json:"route_revision"`
	WorkspaceID          string                   `json:"workspace_id"`
	SessionID            string                   `json:"session_id"`
	RequestID            string                   `json:"request_id"`
	DispatchRuntimeID    string                   `json:"dispatch_runtime_id"`
	DispatchSessionEpoch uint64                   `json:"dispatch_session_epoch"`
	ReceiptStatus        string                   `json:"receipt_status"`
	UpdatedAt            time.Time                `json:"updated_at"`
	InputDigest          string                   `json:"input_digest,omitempty"`
	ControlTarget        *im.IngressControlTarget `json:"control_target,omitempty"`
}

func (r IngressRecord) Identity() im.ChatIdentity {
	return im.ChatIdentity{Platform: r.Platform, AccountID: r.AccountID, ChatID: r.ChatID}
}
func (r IngressRecord) Terminal() bool {
	switch r.State {
	case im.IngressConsumed, im.IngressRejected, im.IngressCompleted, im.IngressSubmitted, im.IngressExpired, im.IngressQuarantined:
		return true
	}
	return false
}

type IngressRoute struct {
	Identity            im.ChatIdentity `json:"identity"`
	Revision            string          `json:"revision"`
	AuthorizationDigest string          `json:"authorization_digest"`
	WorkspaceID         string          `json:"workspace_id"`
	SessionID           string          `json:"session_id"`
	Phase               string          `json:"phase"`
	Sequence            uint64          `json:"sequence"`
}

// AuthorizeClassified keeps the legacy ticket authority and distinguishes
// temporary readiness from permanent identity or binding refusal.
func (a *Admission) AuthorizeClassified(msg im.IncomingMessage) im.AdmissionResult {
	return a.authorizeClassified(msg, true)
}

func (a *Admission) authorizeClassified(msg im.IncomingMessage, chargePairing bool) im.AdmissionResult {
	if msg.AccountID == "" || msg.ChatID == "" || msg.MessageID == "" || msg.SenderID == "" || !isValidIngressChatKind(msg) {
		return im.AdmissionResult{Disposition: im.AdmissionRejected, Reason: "invalid_identity"}
	}
	t, ok := a.authorize(msg, chargePairing)
	if ok {
		if t.Kind == "owner_unbound" {
			return im.AdmissionResult{Ticket: t, Disposition: im.AdmissionRejected, Reason: "chat_unbound"}
		}
		return im.AdmissionResult{Ticket: t, Disposition: im.AdmissionAllowed, Reason: ""}
	}
	if t.IsPairingHint() {
		return im.AdmissionResult{Ticket: t, Disposition: im.AdmissionRejected, Reason: t.Kind}
	}
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed || a.pairing.store.faulted.Load() {
		return im.AdmissionResult{Ticket: t, Disposition: im.AdmissionUnavailable, Reason: "admission_unavailable"}
	}
	a.pairing.mu.Lock()
	state := a.pairing.platforms[msg.IMType]
	unavailable := state == nil || (!state.ready && !state.changed && state.account == msg.AccountID)
	a.pairing.mu.Unlock()
	if unavailable {
		return im.AdmissionResult{Ticket: t, Disposition: im.AdmissionUnavailable, Reason: "platform_unavailable"}
	}
	return im.AdmissionResult{Ticket: t, Disposition: im.AdmissionRejected, Reason: "authorization_denied"}
}

// Feishu card callbacks carry no chat-type field. A validated internal action
// can omit it because card authorization never pairs an owner or creates a
// binding. Text and every other transport still require platform type evidence.
func isValidIngressChatKind(msg im.IncomingMessage) bool {
	if msg.ChatKind == "private" || msg.ChatKind == "group" {
		return true
	}
	return msg.ChatKind == "" && msg.IMType == "feishu" && msg.InternalAction != nil && ValidateIngressInteraction(msg) == nil
}
