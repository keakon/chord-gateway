package main

import (
	"context"
	"time"
)

type ReceiveDisposition string

const (
	ReceiveAccepted  ReceiveDisposition = "accepted"
	ReceiveDuplicate ReceiveDisposition = "duplicate"
	ReceiveRejected  ReceiveDisposition = "rejected"
	ReceiveRetryable ReceiveDisposition = "retryable"
	ReceiveUncertain ReceiveDisposition = "uncertain"
)

// ingressState is the lifecycle state of an in-memory ingress record; it is
// never persisted. The constants document the bounded vocabulary and keep the
// switch arms and allowed-state sets in agreement, but an untyped string
// literal is still assignable, so new states must be added here rather than
// written inline.
type ingressState string

const (
	ingressReceived    ingressState = "received"
	ingressDispatching ingressState = "dispatching"
	ingressUnknown     ingressState = "unknown"
	ingressConsumed    ingressState = "consumed"
	ingressRejected    ingressState = "rejected"
	ingressQuarantined ingressState = "quarantined"
	ingressExpired     ingressState = "expired"
	ingressSubmitted   ingressState = "submitted"
	ingressCompleted   ingressState = "completed"
)

type ReceiveResult struct {
	Disposition ReceiveDisposition
	RecordID    string
	State       ingressState
	Reason      string
}
type IngressEnvelope struct {
	Cursor                 *IngressCursor
	capture                *ingressCapture
	preparation            *ingressPreparation
	Message                IncomingMessage
	DeliveryID, SemanticID string
	InputDigest            string // Original ordinary-text digest; never control parameters.
	SourceTime, ReceivedAt time.Time
}

// ingressPreparation keeps transport credential checks in memory. It cannot
// grant authorization; the common admission service authorizes before prepare.
type ingressPreparation struct {
	prepare func(AdmissionResult) bool
	valid   func() bool
}

type IngressReceiver interface {
	Accept(context.Context, IngressEnvelope) ReceiveResult
}

type IngressControlTarget struct {
	BindingInstanceID string `json:"binding_instance_id,omitempty"`
	SessionID         string `json:"session_id,omitempty"`
	QuestionLookup    bool   `json:"question_lookup,omitempty"`
	TransferID        string `json:"transfer_id,omitempty"`
	RuntimeID         string `json:"runtime_id,omitempty"`
	SessionEpoch      uint64 `json:"session_epoch,omitempty"`
	RequestID         string `json:"request_id,omitempty"`
	Version           uint64 `json:"version,omitempty"`
	TurnID            uint64 `json:"turn_id,omitempty"` // Gateway-observed window, not a conditional Chord cancellation ID.
}
type IngressRecord struct {
	// statusQuery grants one bounded count reserve to an authorized /status.
	// It is an admission-only capability, never granted by caller metadata.
	statusQuery          bool
	RecordID             string                `json:"record_id"`
	Platform             string                `json:"platform"`
	AccountID            string                `json:"account_id"`
	ChatID               string                `json:"chat_id"`
	SenderID             string                `json:"sender_id"`
	BusinessID           string                `json:"business_id"`
	SemanticID           string                `json:"semantic_id,omitempty"`
	DeliveryID           string                `json:"delivery_id,omitempty"`
	SourceTime           time.Time             `json:"source_time"`
	ReceivedAt           time.Time             `json:"received_at"`
	Sequence             uint64                `json:"sequence"`
	Kind                 string                `json:"kind"`
	State                ingressState          `json:"state"`
	Reason               string                `json:"reason,omitempty"`
	Text                 string                `json:"text,omitempty"`
	AuthorizationDigest  string                `json:"authorization_digest"`
	RouteRevision        string                `json:"route_revision"`
	WorkspaceID          string                `json:"workspace_id"`
	SessionID            string                `json:"session_id"`
	RequestID            string                `json:"request_id"`
	DispatchRuntimeID    string                `json:"dispatch_runtime_id"`
	DispatchSessionEpoch uint64                `json:"dispatch_session_epoch"`
	ReceiptStatus        string                `json:"receipt_status"`
	UpdatedAt            time.Time             `json:"updated_at"`
	InputDigest          string                `json:"input_digest,omitempty"`
	ControlTarget        *IngressControlTarget `json:"control_target,omitempty"`
}

func (r IngressRecord) Identity() ChatIdentity {
	return ChatIdentity{r.Platform, r.AccountID, r.ChatID}
}
func (r IngressRecord) Terminal() bool {
	switch r.State {
	case ingressConsumed, ingressRejected, ingressCompleted, ingressSubmitted, ingressExpired, ingressQuarantined:
		return true
	}
	return false
}

type IngressRoute struct {
	Identity            ChatIdentity `json:"identity"`
	Revision            string       `json:"revision"`
	AuthorizationDigest string       `json:"authorization_digest"`
	WorkspaceID         string       `json:"workspace_id"`
	SessionID           string       `json:"session_id"`
	Phase               string       `json:"phase"`
	Sequence            uint64       `json:"sequence"`
}

// IngressCursor contains only a platform-specific checkpoint, never a raw response.
// Generation is the next process-local revision; a write succeeds only from Generation-1.
type IngressCursor struct {
	Kind       string                 `json:"kind"`
	Version    int                    `json:"version"`
	Generation uint64                 `json:"generation"`
	Telegram   *TelegramIngressCursor `json:"telegram,omitempty"`
	Wechat     *WechatIngressCursor   `json:"wechat,omitempty"`
}
type TelegramIngressCursor struct {
	BotID                string    `json:"bot_id,omitempty"`
	PendingOffset        *int64    `json:"pending_offset,omitempty"`
	PendingCreatedAt     time.Time `json:"pending_created_at,omitempty"`
	LastObservedUpdateID *int64    `json:"last_observed_update_id,omitempty"`
	LastObservedAt       time.Time `json:"last_observed_at,omitempty"`
	LastUpdateSourceTime time.Time `json:"last_update_source_time,omitempty"`
	ObservedAt           time.Time `json:"observed_at"`
	State                string    `json:"state"`
}
type WechatIngressCursor struct {
	AccountID    string `json:"account_id"`
	Buf          string `json:"buf"`
	BatchVersion uint64 `json:"batch_version"`
}

type AdmissionDisposition string

const (
	AdmissionAllowed     AdmissionDisposition = "allowed"
	AdmissionRejected    AdmissionDisposition = "rejected"
	AdmissionUnavailable AdmissionDisposition = "unavailable"
)

type AdmissionResult struct {
	Ticket      AdmissionTicket
	Disposition AdmissionDisposition
	Reason      string
}

// AuthorizeClassified keeps the legacy ticket authority and distinguishes
// temporary readiness from permanent identity or binding refusal.
func (a *Admission) AuthorizeClassified(msg IncomingMessage) AdmissionResult {
	return a.authorizeClassified(msg, true)
}

func (a *Admission) authorizeClassified(msg IncomingMessage, chargePairing bool) AdmissionResult {
	if msg.AccountID == "" || msg.ChatID == "" || msg.MessageID == "" || msg.SenderID == "" || !isValidIngressChatKind(msg) {
		return AdmissionResult{Disposition: AdmissionRejected, Reason: "invalid_identity"}
	}
	t, ok := a.authorize(msg, chargePairing)
	if ok {
		if t.Kind == "owner_unbound" {
			return AdmissionResult{t, AdmissionRejected, "chat_unbound"}
		}
		return AdmissionResult{t, AdmissionAllowed, ""}
	}
	if t.IsPairingHint() {
		return AdmissionResult{t, AdmissionRejected, t.Kind}
	}
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed || a.pairing.store.faulted.Load() {
		return AdmissionResult{t, AdmissionUnavailable, "admission_unavailable"}
	}
	a.pairing.mu.Lock()
	state := a.pairing.platforms[msg.IMType]
	unavailable := state == nil || (!state.ready && !state.changed && state.account == msg.AccountID)
	a.pairing.mu.Unlock()
	if unavailable {
		return AdmissionResult{t, AdmissionUnavailable, "platform_unavailable"}
	}
	return AdmissionResult{t, AdmissionRejected, "authorization_denied"}
}

// Feishu card callbacks carry no chat-type field. A validated internal action
// can omit it because card authorization never pairs an owner or creates a
// binding. Text and every other transport still require platform type evidence.
func isValidIngressChatKind(msg IncomingMessage) bool {
	if msg.ChatKind == "private" || msg.ChatKind == "group" {
		return true
	}
	return msg.ChatKind == "" && msg.IMType == "feishu" && msg.InternalAction != nil && ValidateIngressInteraction(msg) == nil
}
