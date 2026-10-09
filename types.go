// Package main defines the gateway ControlState and event types.
package main

import (
	"encoding/json"
	"maps"
	"time"
)

// ControlState is the aggregated state of a chord headless process,
// maintained by the gateway from stdout event stream.
type ControlState struct {
	SessionID   string `json:"session_id"`
	Busy        bool   `json:"busy"`
	RunningJobs int    `json:"running_jobs"`
	Phase       string `json:"phase"`
	PhaseDetail string `json:"phase_detail"`
	CurrentRole string `json:"current_role,omitempty"`
	// SuppressUserNotification applies only to the most recent idle event. It
	// does not change the idle state transition; the router uses it to skip a
	// user-facing completion reminder for configuration-only quiescence.
	SuppressUserNotification bool                   `json:"-"`
	LastError                string                 `json:"last_error"`
	LastTransportError       *TransportErrorPayload `json:"-"`
	LastOutcome              string                 `json:"last_outcome"` // "completed" / "cancelled" / "error" / ""
	UpdatedAt                string                 `json:"updated_at"`
	// LastEnvelopeSeq is the highest seq observed on state-carrying envelopes
	// (pushes and status_response). chord versions pushed state in seq order,
	// but a status_response snapshot is copied on its command path and can
	// arrive after a newer push; a snapshot older than this marker is dropped
	// instead of rolling the aggregated state back.
	LastEnvelopeSeq   uint64              `json:"-"`
	LastInputResult   *InputResultPayload `json:"-"`
	UnconfirmedInputs int                 `json:"-"`

	// Pending interactions
	PendingConfirm *ConfirmPayload             `json:"pending_confirm,omitempty"`
	Questions      map[string]*QuestionPayload `json:"questions,omitempty"`
	PendingHandoff *HandoffPayload             `json:"pending_handoff,omitempty"`
	ExpiredConfirm *ConfirmPayload             `json:"-"`
	ExpiredHandoff *HandoffPayload             `json:"-"`

	LastQuestion *QuestionPayload `json:"-"`

	// Todos from chord process
	Todos []TodoItem `json:"todos,omitempty"`

	LastAssistantText          string               `json:"-"` // last completed assistant message
	LastAssistantToolCalls     int                  `json:"-"` // tool calls executed in the last turn
	LastAssistantAgentID       string               `json:"-"`
	LastAssistantTaskID        string               `json:"-"`
	LastAssistantAgentType     string               `json:"-"`
	LastAssistantParentAgentID string               `json:"-"`
	LastAgentStarted           *AgentStartedPayload `json:"-"`
	LastAgentNotify            *AgentNotifyPayload  `json:"-"`
	LastAgentDone              *AgentDonePayload    `json:"-"`
	LastLocalShell             *LocalShellPayload   `json:"-"`
	// LastBackgroundResult carries a finished background job's durable result.
	// chord headless has no other channel for it (by the time the job finishes
	// the turn is usually idle, so no later assistant_message summarizes it),
	// so the gateway pushes it as a chat message instead of keeping it
	// state-only.
	LastBackgroundResult *BackgroundResultPayload `json:"-"`
	// LastContextNotice carries a durable context-pressure warning, which also
	// has no other headless channel, so it is pushed as a chat message.
	LastContextNotice *ContextNoticePayload `json:"-"`

	// For long-running reminders.
	InternalEventsSinceLastPush int       `json:"-"`
	LastPushAt                  time.Time `json:"-"`
	InfoMessage                 string    `json:"-"` // from info event
	ToastMessage                string    `json:"-"` // from toast event
	ToastLevel                  string    `json:"-"` // from toast event
	// Last notification emitted by chord headless for guaranteed user-facing alerts.
	LastNotification *NotificationPayload `json:"-"`
	// LastQuestionResolved is the most recent question_resolved event, used to
	// format the close notification without re-deriving the reason.
	LastQuestionResolved *QuestionPayload `json:"-"`
	// LastCompaction is the most recent compaction_status terminal outcome
	// forwarded by chord headless. Progress events are filtered headless-side;
	// the gateway only ever sees started and terminal outcomes, and never
	// turns them into chat messages.
	LastCompaction *CompactionStatusPayload `json:"-"`
}

// CompactionStatusPayload is the compaction_status envelope payload.
// Status is one of started|succeeded|skipped|failed|cancelled; trigger is
// manual|usage_driven|length_recovery|oversize_driven|model_driven|model_downshift.
// PlanID correlates a terminal outcome with the started that produced it.
// Synthetic marks lifecycle events of a plan that never occupied the
// compaction slot (the chord-side synchronous interval/cooldown skip): a
// single-slot consumer must not let a synthetic pair overwrite the state of a
// compaction that is still running.
type CompactionStatusPayload struct {
	Status    string `json:"status"`
	Trigger   string `json:"trigger"`
	Reason    string `json:"reason,omitempty"`
	PlanID    string `json:"plan_id,omitempty"`
	Synthetic bool   `json:"synthetic,omitempty"`
}

type InteractiveCardHandle struct {
	MessageID string
	Token     string
}

// ConfirmPayload is the confirm_request event payload.
type ConfirmPayload struct {
	ToolName      string   `json:"tool_name"`
	ArgsJSON      string   `json:"args_json"`
	RequestID     string   `json:"request_id"`
	NeedsApproval []string `json:"needs_approval,omitempty"`
	DoneReport    string   `json:"done_report,omitempty"`
	DoneReason    string   `json:"done_reason,omitempty"`
}

// QuestionPayload is the question_request event payload.
type QuestionPayload struct {
	BindingID       string   `json:"binding_id"`
	OptionIDs       []string `json:"option_ids"`
	Version         uint64   `json:"version"`
	TaskID          string   `json:"task_id"`
	ScopeID         string   `json:"request_scope_id"`
	ResponsePolicy  string   `json:"response_policy"`
	DefaultOptionID string   `json:"default_option_id,omitempty"`
	Timer           string   `json:"timer"`
	Outcome         string   `json:"reason,omitempty"`
	ResultID        string   `json:"result_id,omitempty"`
	Selected        []string `json:"selected,omitempty"`
	SelectedIDs     []string `json:"selected_ids,omitempty"`

	ToolName       string            `json:"tool_name"`
	Header         string            `json:"header,omitempty"`
	Question       string            `json:"question"`
	Options        []string          `json:"options"`
	OptionDetails  []string          `json:"option_details,omitempty"`
	Multiple       bool              `json:"multiple"`
	RequestID      string            `json:"request_id"`
	Deadline       *time.Time        `json:"deadline,omitempty"`
	RevisionResult *QuestionRevision `json:"revision_result,omitempty"`
}

// Question resolved reasons mirror the Chord headless terminal outcomes.
// Historical answers append revisions without changing these outcomes.
const (
	QuestionResolvedDefaulted  = "defaulted"
	QuestionResolvedAnswered   = "answered"
	QuestionResolvedDeclined   = "declined"
	QuestionResolvedNoResponse = "no_response"
	QuestionResolvedSuperseded = "superseded"
	QuestionResolvedCancelled  = "cancelled"
	QuestionResolvedError      = "error"
)

// HandoffPayload is the handoff_request event payload.
type HandoffPayload struct {
	RequestID string               `json:"request_id"`
	PlanPath  string               `json:"plan_path"`
	PlanText  string               `json:"plan_text,omitempty"`
	PlanError string               `json:"plan_error,omitempty"`
	Agents    []HandoffAgentOption `json:"agents"`
}

// HandoffAgentOption describes one selectable execution agent and its model pools.
type HandoffAgentOption struct {
	Name             string   `json:"name"`
	Default          bool     `json:"default"`
	ModelPools       []string `json:"model_pools,omitempty"`
	CurrentModelPool string   `json:"current_model_pool,omitempty"`
}

// notificationReasonUserInputRequired is the reason chord reports when the
// agent is blocked on the user; it is the only reason chord emits today.
const notificationReasonUserInputRequired = "user_input_required"

// NotificationPayload is the notification event payload.
type NotificationPayload struct {
	Message string `json:"message"`
	Reason  string `json:"reason,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
}

// DoneCompletionPayload is the done_completion event payload.
type DoneCompletionPayload struct {
	CallID  string `json:"call_id,omitempty"`
	Report  string `json:"report"`
	Reason  string `json:"reason,omitempty"`
	Status  string `json:"status,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

// AgentStartedPayload is emitted when a delegated SubAgent starts running.
type AgentStartedPayload struct {
	AgentID       string `json:"agent_id"`
	TaskID        string `json:"task_id"`
	AgentType     string `json:"agent_type,omitempty"`
	Description   string `json:"description,omitempty"`
	ParentAgentID string `json:"parent_agent_id,omitempty"`
	ParentTaskID  string `json:"parent_task_id,omitempty"`
}

// AgentNotifyPayload is a non-blocking update from a delegated SubAgent.
type AgentNotifyPayload struct {
	AgentID       string `json:"agent_id"`
	TaskID        string `json:"task_id"`
	AgentType     string `json:"agent_type,omitempty"`
	ParentAgentID string `json:"parent_agent_id,omitempty"`
	ParentTaskID  string `json:"parent_task_id,omitempty"`
	TargetAgentID string `json:"target_agent_id,omitempty"`
	TargetTaskID  string `json:"target_task_id,omitempty"`
	Kind          string `json:"kind,omitempty"`
	Subtype       string `json:"subtype,omitempty"`
	Message       string `json:"message"`
}

// AgentDonePayload is the terminal completion summary from a delegated SubAgent.
type AgentDonePayload struct {
	AgentID       string `json:"agent_id"`
	TaskID        string `json:"task_id"`
	AgentType     string `json:"agent_type,omitempty"`
	ParentAgentID string `json:"parent_agent_id,omitempty"`
	ParentTaskID  string `json:"parent_task_id,omitempty"`
	Summary       string `json:"summary"`
}

// LocalShellPayload is the local_shell_result event payload.
type LocalShellPayload struct {
	Command string `json:"command"`
	Output  string `json:"output"`
	Failed  bool   `json:"failed"`
	Error   string `json:"error,omitempty"`
}

// BackgroundResultPayload is the background_result event payload: the durable
// result of a finished background job, which is also the only delivery channel
// for the JOB RESULT card.
// SessionID and MessageIndex identify the source durable transcript row.
// They do not change the gateway's active session.
type BackgroundResultPayload struct {
	SessionID     string `json:"session_id,omitempty"`
	MessageIndex  int    `json:"message_index"`
	TargetAgentID string `json:"target_agent_id,omitempty"`
	Content       string `json:"content"`
}

// ContextNoticePayload is the context_notice event payload: a durable
// context-pressure warning. Level mirrors the three compaction-gate overlays
// (pressure, imminent, warning).
type ContextNoticePayload struct {
	SessionID    string `json:"session_id,omitempty"`
	MessageIndex int    `json:"message_index"`
	Level        string `json:"level"`
	Message      string `json:"message"`
}

// HeadlessEnvelope is the JSON envelope from chord headless stdout.
type HeadlessEnvelope struct {
	Type    string          `json:"type"`
	Seq     uint64          `json:"seq,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// StatusResponse is the payload for type="status_response".
type StatusResponse struct {
	SessionID      string             `json:"session_id"`
	Busy           bool               `json:"busy"`
	RunningJobs    int                `json:"running_jobs"`
	Phase          string             `json:"phase"`
	PhaseDetail    string             `json:"phase_detail"`
	CurrentRole    string             `json:"current_role,omitempty"`
	PendingConfirm *ConfirmPayload    `json:"pending_confirm,omitempty"`
	Questions      []*QuestionPayload `json:"questions"`
	PendingHandoff *HandoffPayload    `json:"pending_handoff,omitempty"`
	LastError      string             `json:"last_error"`
	LastOutcome    string             `json:"last_outcome"`
	UpdatedAt      string             `json:"updated_at"`
}

// RoleInfo describes one switchable main-agent role in a role list.
type RoleInfo struct {
	Name    string `json:"name"`
	Current bool   `json:"current,omitempty"`
}

// RoleResponse is the payload for type="role_response". Roles is ordered
// (builder first, planner second when configured, then custom roles
// alphabetically) and lists only main-mode roles.
type RoleResponse struct {
	OK      bool       `json:"ok"`
	Message string     `json:"message,omitempty"`
	Role    string     `json:"role,omitempty"`
	Roles   []RoleInfo `json:"roles,omitempty"`
}

// IMCommand is a parsed command from IM user.
type IMCommand struct {
	Type        string   // "local_shell", "send", "status", "cancel", "confirm", "question", "handoff", "role", "new", "resume", "sessions", "todos", "login", "bind"
	Content     string   // for send/login target
	RequestID   string   // for confirm/question/handoff
	Action      string   // for confirm/handoff: "allow"/"deny"/"accept"
	Reason      string   // for deny: human-readable reason text
	Agent       string   // for handoff
	Pool        string   // for handoff model pool
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

// IncomingMessage is the structured inbound message model.
// It replaces the raw (imType, chatID, text) tuple as the primary
// entry point for the router.
type IncomingMessage struct {
	IMType     string `json:"im_type"`               // e.g. "wechat", "feishu", "console"
	ChatID     string `json:"chat_id"`               // chat/group identifier for routing & replies
	SenderID   string `json:"sender_id"`             // user identifier (open_id, from_user_id, "console")
	SenderName string `json:"sender_name,omitempty"` // display name (optional)
	MessageID  string `json:"message_id,omitempty"`  // platform message ID for deduplication
	Text       string `json:"text"`                  // message text content
	AppID      string `json:"app_id,omitempty"`      // feishu app_id for multi-account dedupe

	// InternalAction carries trusted structured commands from platform callbacks.
	// User-authored text must not set this field; parseIMCommand handles that path.
	InternalAction *InternalAction `json:"-"`
}

type InternalAction struct {
	Type      string
	Action    string
	RequestID string
	Value     string
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

// applyPendingConfirm records a freshly-received confirm_request payload and
// clears any prior expired-confirm marker so the new request supersedes it.
func (s *ControlState) applyPendingConfirm(c *ConfirmPayload) {
	s.PendingConfirm = c
	s.ExpiredConfirm = nil
}

// applyQuestionState uses copy-on-write so outbound event snapshots remain
// immutable while later answers update the process state.
func (s *ControlState) applyQuestionState(q *QuestionPayload) bool {
	if q == nil || q.RequestID == "" {
		return false
	}
	if !newerQuestionState(q, s.Questions[q.RequestID]) {
		return false
	}
	next := make(map[string]*QuestionPayload, len(s.Questions)+1)
	for id, old := range s.Questions {
		next[id] = old
	}
	next[q.RequestID] = q
	s.Questions = next
	s.LastQuestion = q
	if q.Outcome != "" {
		s.LastQuestionResolved = q
	}
	return true
}

func newerQuestionState(q, old *QuestionPayload) bool {
	return old == nil || old.Version < q.Version ||
		(old.Version == q.Version && old.RevisionResult == nil && q.RevisionResult != nil)
}

func (s ControlState) uniquePendingQuestion() *QuestionPayload {
	var result *QuestionPayload
	for _, q := range s.Questions {
		if q.Outcome != "" {
			continue
		}
		if result != nil {
			return nil
		}
		result = q
	}
	return result
}
func (s ControlState) pendingQuestionCount() int {
	n := 0
	for _, q := range s.Questions {
		if q.Outcome == "" {
			n++
		}
	}
	return n
}
func (s ControlState) questionForNotification() *QuestionPayload {
	if s.LastQuestion != nil {
		return s.LastQuestion
	}
	return s.uniquePendingQuestion()
}

func (s *ControlState) applyPendingHandoff(h *HandoffPayload) {
	s.PendingHandoff = h
	s.ExpiredHandoff = nil
}

// applyHandoffCancelled clears the pending handoff when a cancelled event
// targets it: an empty request ID matches any pending handoff, while a
// non-empty one must equal the pending request ID so a late event cannot clear
// a newer pending handoff. It records the cleared request in ExpiredHandoff so
// the router notifies the user and resolves late replies, and reports whether
// a pending handoff was actually cleared.
func (s *ControlState) applyHandoffCancelled(requestID string) bool {
	if s.PendingHandoff == nil {
		return false
	}
	if requestID != "" && requestID != s.PendingHandoff.RequestID {
		return false
	}
	s.ExpiredHandoff = s.PendingHandoff
	s.PendingHandoff = nil
	return true
}

// applyStatusResponse merges a chord-headless status_response envelope into
// the aggregated state, clearing expired-pending markers when the response
// reports any active pending interaction.
func (s *ControlState) applyStatusResponse(resp *StatusResponse) {
	if resp == nil {
		return
	}
	if s.SessionID != resp.SessionID {
		s.Questions = nil
		s.LastQuestion = nil
		s.LastQuestionResolved = nil
	}
	s.SessionID = resp.SessionID
	s.Busy = resp.Busy
	s.RunningJobs = resp.RunningJobs
	s.Phase = resp.Phase
	s.PhaseDetail = resp.PhaseDetail
	if resp.CurrentRole != "" {
		s.CurrentRole = resp.CurrentRole
	}
	s.PendingConfirm = resp.PendingConfirm
	questions := resp.Questions
	// A full snapshot merges the history in one copy instead of cloning it
	// once for each question, which would make restoration quadratic.
	if len(questions) > 0 {
		next := s.Questions
		copied := false
		for _, q := range questions {
			if q == nil || q.RequestID == "" || !newerQuestionState(q, next[q.RequestID]) {
				continue
			}
			if !copied {
				next = maps.Clone(s.Questions)
				if next == nil {
					next = make(map[string]*QuestionPayload, len(questions))
				}
				copied = true
			}
			next[q.RequestID] = q
			s.LastQuestion = q
			if q.Outcome != "" {
				s.LastQuestionResolved = q
			}
		}
		s.Questions = next
	}
	s.PendingHandoff = resp.PendingHandoff
	if resp.PendingConfirm != nil || s.pendingQuestionCount() > 0 || resp.PendingHandoff != nil {
		s.ExpiredConfirm = nil
		s.ExpiredHandoff = nil
	}
	s.LastError = resp.LastError
	s.UpdatedAt = resp.UpdatedAt
	s.LastOutcome = resp.LastOutcome
}

type QuestionRevision struct {
	QuestionID string   `json:"question_id"`
	ResultID   string   `json:"result_id"`
	Selected   []string `json:"selected"`
	Outcome    string   `json:"outcome"`
}
