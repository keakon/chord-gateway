package im

import "time"

// InteractiveCardHandle identifies one interactive card message on a platform.
type InteractiveCardHandle struct {
	MessageID string
	Token     string
}

// IMCommand is a parsed command from IM user.
type IMCommand struct {
	Timeout   *time.Duration
	Type      string   // "local_shell", "send", "status", "cancel", "confirm", "question", "handoff", "role", "new", "resume", "sessions", "todos", "login", "bind"
	Content   string   // for send/login target
	RequestID string   // for confirm/question/handoff
	Action    string   // for confirm/handoff: "allow"/"deny"/"accept"
	Reason    string   // for deny: human-readable reason text
	Agent     string   // for handoff
	Pool      string   // for handoff model pool
	Answers   []string // for question
	SessionID string   // for resume
	Invalid   bool     // command-specific parse failure
}

// IncomingMessage is the structured inbound message model.
// It replaces the raw (imType, chatID, text) tuple as the primary
// entry point for the router.
type IncomingMessage struct {
	AccountID string `json:"-"`
	ChatKind  string `json:"-"`
	ticket    *AdmissionTicket

	IMType     string `json:"im_type"`               // e.g. "wechat", "feishu", "console"
	ChatID     string `json:"chat_id"`               // chat/group identifier for routing & replies
	SenderID   string `json:"sender_id"`             // user identifier (open_id, from_user_id, "console")
	SenderName string `json:"sender_name,omitempty"` // display name (optional)
	MessageID  string `json:"message_id,omitempty"`  // platform message ID for deduplication
	Text       string `json:"text"`                  // message text content
	// ForceText preserves text-only addressing semantics supplied by the transport.
	ForceText bool   `json:"-"`
	AppID     string `json:"app_id,omitempty"` // feishu app_id for multi-account dedupe

	// InternalAction carries trusted structured commands from platform callbacks.
	// User-authored text must not set this field; parseIMCommand handles that path.
	InternalAction *InternalAction `json:"-"`
}

func (m *IncomingMessage) SetAdmissionTicket(t *AdmissionTicket) { m.ticket = t }
func (m *IncomingMessage) AdmissionTicket() *AdmissionTicket     { return m.ticket }

type InternalAction struct {
	BindingInstanceID string
	Version           uint64
	Type              string
	Action            string
	RequestID         string
	Value             string
	// Values carries the answers of a multi-select question card form: the
	// checked option numbers, in option order. Value stays empty on that path,
	// and vice versa for a single-select button click.
	Values []string
	Handle InteractiveCardHandle
}

// MessageRouter is the interface for routing incoming messages.
// NotificationRouter implements this interface.
// Defined so that tests can use a stub router.
type MessageRouter interface {
	HandleIncomingMessage(msg IncomingMessage)
}

// QuestionFormSubmit reports whether the action came from a multi-select
// question card's form: it carries the checked option numbers instead of a
// stable option ID.
func (a *InternalAction) QuestionFormSubmit() bool {
	return a != nil && a.Type == "question" && len(a.Values) > 0
}
