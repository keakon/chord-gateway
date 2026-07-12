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
		var payload struct {
			LastOutcome string `json:"last_outcome"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.LastOutcome = payload.LastOutcome
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

	case "agent_done":
		eventType = "agent_done"

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
			p.notifyStatusWaiters(p.state)
		}
		// No onEvent — solicited response.

	case "subscribe_response":
		// No onEvent — ack response.

	case "assistant_message":
		var payload struct {
			Text      string `json:"text"`
			AgentID   string `json:"agent_id"`
			ToolCalls int    `json:"tool_calls"`
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
