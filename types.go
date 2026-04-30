// Package main defines the gateway ControlState and event types.
package main

import (
	"encoding/json"
	"time"
)

// ControlState is the aggregated state of a chord headless process,
// maintained by the gateway from stdout event stream.
type ControlState struct {
	SessionID   string `json:"session_id"`
	Busy        bool   `json:"busy"`
	Phase       string `json:"phase"`
	PhaseDetail string `json:"phase_detail"`
	LastError   string `json:"last_error"`
	LastOutcome string `json:"last_outcome"` // "completed" / "cancelled" / "error" / ""
	UpdatedAt   string `json:"updated_at"`

	// Pending interactions
	PendingConfirm  *ConfirmPayload  `json:"pending_confirm,omitempty"`
	PendingQuestion *QuestionPayload `json:"pending_question,omitempty"`
	ExpiredConfirm  *ConfirmPayload  `json:"-"`
	ExpiredQuestion *QuestionPayload `json:"-"`

	// Todos from chord process
	Todos []TodoItem `json:"todos,omitempty"`

	// Last tool result (not persisted)
	LastToolResult *ToolResultInfo `json:"-"` // from tool_result event

	// Stream assistant text as it arrives.
	StreamText             string `json:"-"` // accumulated assistant streamed text (tail only)
	LastAssistantText      string `json:"-"` // last completed assistant message
	LastThinkingText       string `json:"-"` // last completed thinking block(s)
	LastAssistantToolCalls int    `json:"-"` // tool calls executed in the last turn

	// For long-running reminders.
	InternalEventsSinceLastPush int       `json:"-"`
	LastPushAt                  time.Time `json:"-"`
	InfoMessage                 string    `json:"-"` // from info event
	ToastMessage                string    `json:"-"` // from toast event
	ToastLevel                  string    `json:"-"` // from toast event
	// LastStatusResponseAt is set only when a status_response envelope is received.
	// Used for /status freshness polling instead of UpdatedAt (which is also updated on spawn).
	LastStatusResponseAt time.Time `json:"-"`
	// Last notification emitted by chord headless for guaranteed user-facing alerts.
	LastNotification *NotificationPayload `json:"-"`
}

type InteractiveCardHandle struct {
	MessageID string
	Token     string
}

// ConfirmPayload is the confirm_request event payload.
type ConfirmPayload struct {
	ToolName       string   `json:"tool_name"`
	ArgsJSON       string   `json:"args_json"`
	RequestID      string   `json:"request_id"`
	TimeoutMS      int64    `json:"timeout_ms"`
	NeedsApproval  []string `json:"needs_approval,omitempty"`
	AlreadyAllowed []string `json:"already_allowed,omitempty"`
}

// QuestionPayload is the question_request event payload.
type QuestionPayload struct {
	ToolName      string   `json:"tool_name"`
	Header        string   `json:"header,omitempty"`
	Question      string   `json:"question"`
	Options       []string `json:"options"`
	OptionDetails []string `json:"option_details,omitempty"`
	DefaultAnswer string   `json:"default_answer"`
	Multiple      bool     `json:"multiple"`
	RequestID     string   `json:"request_id"`
	TimeoutMS     int64    `json:"timeout_ms"`
}

// NotificationPayload is the notification event payload.
type NotificationPayload struct {
	Message string `json:"message"`
	Reason  string `json:"reason,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
}

// HeadlessEnvelope is the JSON envelope from chord headless stdout.
type HeadlessEnvelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// StatusResponse is the payload for type="status_response".
type StatusResponse struct {
	SessionID       string           `json:"session_id"`
	Busy            bool             `json:"busy"`
	Phase           string           `json:"phase"`
	PhaseDetail     string           `json:"phase_detail"`
	PendingConfirm  *ConfirmPayload  `json:"pending_confirm,omitempty"`
	PendingQuestion *QuestionPayload `json:"pending_question,omitempty"`
	LastError       string           `json:"last_error"`
	LastOutcome     string           `json:"last_outcome"`
	UpdatedAt       string           `json:"updated_at"`
}

// IMCommand is a parsed command from IM user.
type IMCommand struct {
	Type        string   // "status", "send", "confirm", "question", "cancel", "new", "resume", "sessions", "current", "todos", "login", "bind"
	Content     string   // for send/login target
	RequestID   string   // for confirm/question
	Action      string   // for confirm: "allow" or "deny"
	Reason      string   // for deny: human-readable reason text
	Answers     []string // for question
	SessionID   string   // for resume
	WorkspaceID string   // for bind
	Path        string   // for bind workspace path
	Invalid     bool     // command-specific parse failure (currently used by /bind)
}

// TodoItem represents a todo item from the chord process.
type TodoItem struct {
	ID         string `json:"id"`
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"active_form,omitempty"`
}

// ToolResultInfo represents the result of a tool execution.
type ToolResultInfo struct {
	CallID  string `json:"call_id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	AgentID string `json:"agent_id"`
}

// IncomingMessage is the structured inbound message model.
// It replaces the raw (imType, chatID, text) tuple as the primary
// entry point for the router.
type IncomingMessage struct {
	IMType         string `json:"im_type"`                   // e.g. "wechat", "feishu", "console"
	ChatID         string `json:"chat_id"`                   // chat/group identifier for routing & replies
	SenderID       string `json:"sender_id"`                 // user identifier (open_id, from_user_id, "console")
	SenderName     string `json:"sender_name,omitempty"`     // display name (optional)
	MessageID      string `json:"message_id,omitempty"`      // platform message ID for deduplication
	ConversationID string `json:"conversation_id,omitempty"` // optional conversation thread ID
	Text           string `json:"text"`                      // message text content
	AppID          string `json:"app_id,omitempty"`          // feishu app_id for multi-account dedupe

	// InternalAction carries trusted structured commands from platform callbacks.
	// User-authored text must not set this field; parseIMCommand handles that path.
	InternalAction *InternalAction `json:"-"`
}

type InternalAction struct {
	Type      string
	Action    string
	RequestID string
	Value     string
	Handle    InteractiveCardHandle
}

// MessageRouter is the interface for routing incoming messages.
// NotificationRouter implements this interface.
// Defined so that tests can use a stub router.
type MessageRouter interface {
	HandleIncomingMessage(msg IncomingMessage)
}
