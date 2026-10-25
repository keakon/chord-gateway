package im

import (
	"context"
	"strings"
	"time"
)

type ReceiveDisposition string

const (
	ReceiveAccepted  ReceiveDisposition = "accepted"
	ReceiveDuplicate ReceiveDisposition = "duplicate"
	ReceiveRejected  ReceiveDisposition = "rejected"
	ReceiveRetryable ReceiveDisposition = "retryable"
	ReceiveUncertain ReceiveDisposition = "uncertain"

	// ReceiveTransportIgnored is local to the WeChat transport. It provides no
	// in-memory refusal and is never an admission result for a business message.
	ReceiveTransportIgnored ReceiveDisposition = "transport_ignored"
)

// IngressState is the lifecycle state of an in-memory ingress record; it is
// never persisted. The constants document the bounded vocabulary and keep the
// switch arms and allowed-state sets in agreement, but an untyped string
// literal is still assignable, so new states must be added here rather than
// written inline.
type IngressState string

const (
	IngressReceived    IngressState = "received"
	IngressDispatching IngressState = "dispatching"
	IngressUnknown     IngressState = "unknown"
	IngressConsumed    IngressState = "consumed"
	IngressRejected    IngressState = "rejected"
	IngressQuarantined IngressState = "quarantined"
	IngressExpired     IngressState = "expired"
	IngressSubmitted   IngressState = "submitted"
	IngressCompleted   IngressState = "completed"
)

type ReceiveResult struct {
	Disposition ReceiveDisposition
	RecordID    string
	State       IngressState
	Reason      string
}

// IngressCapture freezes transport-side controls on the reader before slow
// identity lookups.
type IngressCapture struct {
	Unavailable bool
	Kind        string
	Target      *IngressControlTarget
	Err         error
}

// IngressPreparation keeps transport credential checks in memory. It cannot
// grant authorization; the common admission service authorizes before prepare.
type IngressPreparation struct {
	Prepare func(AdmissionResult) bool
	Valid   func() bool
}

type IngressEnvelope struct {
	Cursor                 *IngressCursor
	Capture                *IngressCapture
	Preparation            *IngressPreparation
	Message                IncomingMessage
	DeliveryID, SemanticID string
	InputDigest            string // Original ordinary-text digest; never control parameters.
	SourceTime, ReceivedAt time.Time
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

// AdmissionTicket carries the authority granted by the gateway's admission
// service for one inbound message or operation.
type AdmissionTicket struct {
	Kind                            string
	Identity                        ChatIdentity
	SenderID                        string
	AccountEpoch, BindingGeneration uint64
	PairingNonceID                  string
	ReceivedAt                      time.Time
	Gate                            *ChatGate
}

// IsPairingHint reports whether the ticket authorizes only a bounded pairing
// hint reply. Every hint kind shares the "pairing_hint" prefix, and callers that
// decide authority, reply permits and dispatch all have to agree on that set, so
// the distinction lives with the ticket instead of at each call site.
func (t AdmissionTicket) IsPairingHint() bool {
	return strings.HasPrefix(t.Kind, "pairing_hint")
}
