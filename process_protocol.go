package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/keakon/golog/log"
)

// readLoop reads stdout from the chord process, parses JSON envelopes,
// updates state, and calls onEvent for notable events.
func (p *ChordProcess) readLoop(ctx context.Context, stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var env HeadlessEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			log.Warnf("failed to parse headless envelope line=%v error=%v", string(line), err)
			continue
		}

		p.processEnvelope(&env)
	}

	// stdout EOF — chord process has exited.
	p.handleExit()
}

// processEnvelope updates ControlState based on the envelope type and calls onEvent.
func (p *ChordProcess) processEnvelope(env *HeadlessEnvelope) {
	p.mu.Lock()
	now := time.Now()
	updatedAt := now.Format(time.RFC3339)

	p.lastActivity = now
	p.state.UpdatedAt = updatedAt

	var eventType string
	var sessionPin string
	var deferredLog func(string, ControlState)

	switch env.Type {
	case "ready":
		var payload struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			if strings.TrimSpace(payload.SessionID) != "" {
				p.state.SessionID = payload.SessionID
				sessionPin = payload.SessionID
			}
		}
		deferredLog = func(key string, state ControlState) {
			log.Infof("[%v] gateway event event=%v raw_type=%v", processLogContext(key, state), "ready", "ready")
		}
		// No notification.
		eventType = ""

	case "activity":
		p.state.Busy = true
		if p.state.LastPushAt.IsZero() {
			p.state.LastPushAt = now
		}
		var payload struct {
			AgentID string `json:"agent_id"`
			Type    string `json:"type"`
			Detail  string `json:"detail"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.Phase = payload.Type
			p.state.PhaseDetail = payload.Detail
		}
		eventType = "activity"

	case "idle":
		p.transitionToIdle("", false)
		p.state.SuppressUserNotification = false
		var payload struct {
			LastOutcome              string `json:"last_outcome"`
			SuppressUserNotification bool   `json:"suppress_user_notification"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.LastOutcome = payload.LastOutcome
			p.state.SuppressUserNotification = payload.SuppressUserNotification
		}
		eventType = "idle"

	case "confirm_request":
		var payload ConfirmPayload
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.applyPendingConfirm(&payload)
		}
		eventType = "confirm_request"

	case "question_request":
		var payload QuestionPayload
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.applyPendingQuestion(&payload)
		}
		eventType = "question_request"

	case "handoff_request":
		var payload HandoffPayload
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.applyPendingHandoff(&payload)
		}
		eventType = "handoff_request"

	case "handoff_cancelled":
		// Chord pushes this when a pending handoff is cancelled without a
		// decision (for example superseded by a new request or session
		// switch). The event may arrive late or carry a mismatched/empty
		// request ID, so only clear when it targets the current pending
		// handoff; a mismatched event must leave a newer pending handoff
		// untouched. Clearing records the request in ExpiredHandoff so the
		// router can notify the user and later /handoff falls back to the
		// no-pending path.
		var payload struct {
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			if p.state.applyHandoffCancelled(payload.RequestID) {
				eventType = "handoff_cancelled"
			}
		}

	case "error":
		var payload struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.LastError = payload.Message
		}
		eventType = "error"
	case "notification":
		var payload NotificationPayload
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.LastNotification = &payload
		}
		eventType = "notification"

	case "done_completion":
		var payload DoneCompletionPayload
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.LastNotification = &NotificationPayload{Message: payload.Report, Reason: "done_completion", AgentID: payload.AgentID}
		}
		eventType = "done_completion"

	case "local_shell_result":
		var payload LocalShellPayload
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.LastLocalShell = &payload
			p.state.Busy = false
		}
		eventType = "local_shell_result"

	case "agent_started":
		var payload AgentStartedPayload
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.LastAgentStarted = &payload
		}
		eventType = "agent_started"

	case "agent_notify":
		var payload AgentNotifyPayload
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.LastAgentNotify = &payload
		}
		eventType = "agent_notify"

	case "agent_done":
		var payload AgentDonePayload
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.LastAgentDone = &payload
		}
		eventType = "agent_done"

	case "compaction_status":
		// Terminal compaction outcomes (started/succeeded/skipped/failed/
		// cancelled) are saved for status/diagnostic interfaces. They are
		// never pushed as chat messages; headless already filters progress
		// events out of this envelope.
		var payload CompactionStatusPayload
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			// The gateway mirrors the single-slot compaction semantics of the
			// TUI pill. A synthetic started (the chord-side synchronous
			// interval/cooldown skip) never occupies the slot: while a real
			// compaction runs, its skipped terminal must not overwrite the
			// running plan's state. A terminal from a plan that no longer owns
			// the slot (the skipped half of a synthetic pair, or a superseded
			// plan's late outcome) is dropped; an idle slot applies any
			// outcome (a lone synthetic skip is still surfaced).
			switch payload.Status {
			case "started":
				if payload.Synthetic {
					break
				}
				p.compactionPlanID = payload.PlanID
				p.state.LastCompaction = &payload
			default:
				// While a plan owns the slot, only its own terminal may resolve
				// it. An empty plan id matches nothing: it must not clear a
				// running plan's state.
				if p.compactionPlanID != "" && payload.PlanID != p.compactionPlanID {
					break
				}
				p.compactionPlanID = ""
				p.state.LastCompaction = &payload
			}
		}
		eventType = ""

	case "info":
		var payload struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.InfoMessage = payload.Message
		}
		eventType = "info"

	case "toast":
		var payload struct {
			Message string `json:"message"`
			Level   string `json:"level"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.ToastMessage = payload.Message
			p.state.ToastLevel = payload.Level
		}
		eventType = "toast"

	case "status_response":
		var resp StatusResponse
		if err := json.Unmarshal(env.Payload, &resp); err == nil {
			p.state.applyStatusResponse(&resp)
			// Wake any goroutines blocked in WaitStatus.
			p.statusWaiters.notify(p.state)
		}
		// No onEvent — solicited response.

	case "role_response":
		var resp RoleResponse
		if err := json.Unmarshal(env.Payload, &resp); err == nil {
			if resp.OK && strings.TrimSpace(resp.Role) != "" {
				p.state.CurrentRole = resp.Role
			}
			// Wake any goroutines blocked in WaitRoleList/WaitRoleSwitch.
			p.roleWaiters.notify(resp)
		}
		// No onEvent — solicited response.

	case "role_change":
		var payload struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil && strings.TrimSpace(payload.Role) != "" {
			p.state.CurrentRole = payload.Role
		}
		// No onEvent: a role change is acknowledged synchronously by the
		// role_response that preceded it; the state cache is enough for /status.

	case "session_switched":
		var payload struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			if id := strings.TrimSpace(payload.SessionID); id != "" {
				p.state.SessionID = id
				// Compaction outcomes belong to the abandoned session: keep
				// them and /status would report the old session's checkpoint,
				// while the old plan's slot owner would drop the new
				// session's terminals (or vice versa). Reset both.
				p.state.LastCompaction = nil
				p.compactionPlanID = ""
				// chord pushes this only for an in-band switch that replaced
				// the session without restarting the process (handoff plan
				// execution, /resume <id>, /new). The pin is otherwise written
				// only by the ready envelope, so without this the binding keeps
				// resuming the session the switch abandoned.
				//
				// Chord only reports a switch that actually happened, so the
				// newly active session is the one later spawns must resume:
				// the pin is re-pointed unconditionally.
				sessionPin = id
			}
		}
		// No onEvent: the command that triggered the switch already answered
		// the user, and the state cache keeps /status and later spawns on the
		// session the runtime actually runs.

	case "background_result":
		var payload BackgroundResultPayload
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.LastBackgroundResult = &payload
		}
		eventType = "background_result"

	case "context_notice":
		var payload ContextNoticePayload
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.LastContextNotice = &payload
		}
		eventType = "context_notice"

	case "subscribe_response":
		// No onEvent — ack response.

	case "assistant_message":
		var payload struct {
			Text          string `json:"text"`
			AgentID       string `json:"agent_id"`
			TaskID        string `json:"task_id"`
			AgentType     string `json:"agent_type"`
			ParentAgentID string `json:"parent_agent_id"`
			ToolCalls     int    `json:"tool_calls"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			if strings.TrimSpace(payload.Text) != "" {
				p.state.LastAssistantText = payload.Text
				eventType = "assistant_message"
			} else {
				agentID, toolCalls := payload.AgentID, payload.ToolCalls
				deferredLog = func(key string, state ControlState) {
					log.Debugf("[%v] gateway assistant_message had empty text; skipping notification agent_id=%v tool_calls=%v", processLogContext(key, state), agentID, toolCalls)
				}
			}
			p.state.LastAssistantToolCalls = payload.ToolCalls
			p.state.LastAssistantAgentID = payload.AgentID
			p.state.LastAssistantTaskID = payload.TaskID
			p.state.LastAssistantAgentType = payload.AgentType
			p.state.LastAssistantParentAgentID = payload.ParentAgentID
			p.state.InternalEventsSinceLastPush = 0
			p.state.LastPushAt = now
		}

	case "todos":
		var wrapper struct {
			Todos []TodoItem `json:"todos"`
		}
		if err := json.Unmarshal(env.Payload, &wrapper); err != nil {
			parseErr := err
			deferredLog = func(key string, state ControlState) {
				log.Warnf("[%v] failed to parse todos payload error=%v", processLogContext(key, state), parseErr)
			}
			p.state.Todos = nil
		} else {
			p.state.Todos = wrapper.Todos
		}
		if !p.state.LastPushAt.IsZero() {
			p.state.InternalEventsSinceLastPush++
		}
		eventType = "todos"

	case "assistant_rollback":
		p.state.LastAssistantText = ""
		p.state.LastAssistantAgentID = ""
		p.state.LastAssistantTaskID = ""
		p.state.LastAssistantAgentType = ""
		p.state.LastAssistantParentAgentID = ""
		eventType = "assistant_rollback"

	default:
		rawType := env.Type
		deferredLog = func(string, ControlState) { log.Debugf("unknown headless event type type=%v", rawType) }
	}

	// Capture callback params under lock, then invoke outside lock to prevent
	// deadlock: onEvent → router → proc.Alive/SendCommand → p.mu.
	var (
		onEvent = p.onEvent
		key     = p.key
		state   = p.state // copy
	)
	p.mu.Unlock()

	if deferredLog != nil {
		deferredLog(key, state)
	}

	if eventType != "" {
		format := "[%v] gateway event event=%v raw_type=%v busy=%v phase=%v last_outcome=%v assistant_text_len=%v assistant_tool_calls=%v pending_confirm=%v pending_question=%v last_error=%v"
		args := []any{processLogContext(key, state),
			eventType,
			env.Type,
			state.Busy,
			state.Phase,
			state.LastOutcome,
			len(state.LastAssistantText),
			state.LastAssistantToolCalls,
			state.PendingConfirm != nil,
			state.PendingQuestion != nil,
			state.LastError,
		}
		if p.eventLogf != nil {
			p.eventLogf(format, args...)
		} else {
			log.Infof(format, args...)
		}
	}

	if sessionPin != "" && p.mgr != nil && p.mgr.pins != nil {
		if err := p.mgr.pins.Set(key, sessionPin); err != nil {
			log.Warnf("[%v] persist session pin failed error=%v", processLogContext(key, state), err)
		}
	}

	if eventType != "" && onEvent != nil {
		onEvent(key, eventType, state)
	}
}
