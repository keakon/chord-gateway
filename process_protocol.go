package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"time"
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
			slog.Warn("failed to parse headless envelope", "line", string(line), "error", err)
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

	p.lastActivity = time.Now()
	p.state.UpdatedAt = time.Now().Format(time.RFC3339)

	var eventType string

	switch env.Type {
	case "ready":
		var payload struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			if strings.TrimSpace(payload.SessionID) != "" {
				p.state.SessionID = payload.SessionID
				if p.mgr != nil && p.mgr.pins != nil {
					if perr := p.mgr.pins.Set(p.key, payload.SessionID); perr != nil {
						slog.Warn("persist session pin failed", "key", p.key, "session_id", payload.SessionID, "error", perr)
					}
				}
			}
		}
		_, imType, _ := parseProcessKey(p.key)
		slog.Info("gateway event", "event", "ready", "raw_type", "ready", "key", p.key, "workspace", p.workspaceID, "im", imType, "session_id", p.state.SessionID)
		// No notification.
		eventType = ""

	case "activity":
		p.state.Busy = true
		p.lastActivity = time.Now()
		if p.state.LastPushAt.IsZero() {
			p.state.LastPushAt = time.Now()
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
			p.state.PendingConfirm = &payload
			p.state.ExpiredConfirm = nil
		}
		eventType = "confirm_request"

	case "question_request":
		var payload QuestionPayload
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.PendingQuestion = &payload
			p.state.ExpiredQuestion = nil
		}
		eventType = "question_request"

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

	case "agent_done":
		p.lastActivity = time.Now()
		eventType = "agent_done"

	case "info":
		p.lastActivity = time.Now()
		var payload struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.InfoMessage = payload.Message
		}
		eventType = "info"

	case "toast":
		p.lastActivity = time.Now()
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
			p.state.SessionID = resp.SessionID
			p.state.Busy = resp.Busy
			p.state.Phase = resp.Phase
			p.state.PhaseDetail = resp.PhaseDetail
			p.state.PendingConfirm = resp.PendingConfirm
			p.state.PendingQuestion = resp.PendingQuestion
			if resp.PendingConfirm != nil || resp.PendingQuestion != nil {
				p.state.ExpiredConfirm = nil
				p.state.ExpiredQuestion = nil
			}
			p.state.LastError = resp.LastError
			p.state.UpdatedAt = resp.UpdatedAt
			p.state.LastOutcome = resp.LastOutcome
			p.state.LastStatusResponseAt = time.Now()
			// Wake any goroutines blocked in WaitStatus.
			p.notifyStatusWaiters(p.state)
		}
		// No onEvent — solicited response.

	case "subscribe_response":
		// No onEvent — ack response.

	case "tool_result":
		var payload struct {
			CallID  string `json:"call_id"`
			Name    string `json:"name"`
			Status  string `json:"status"`
			AgentID string `json:"agent_id"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.LastToolResult = &ToolResultInfo{
				CallID:  payload.CallID,
				Name:    payload.Name,
				Status:  payload.Status,
				AgentID: payload.AgentID,
			}
			p.state.InternalEventsSinceLastPush++
			p.state.UpdatedAt = time.Now().Format(time.RFC3339)
			p.lastActivity = time.Now()
		}
		eventType = "tool_result"

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
				slog.Debug("gateway assistant_message had empty text; skipping notification",
					"key", p.key,
					"workspace", p.workspaceID,
					"agent_id", payload.AgentID,
					"tool_calls", payload.ToolCalls,
				)
			}
			p.state.LastAssistantToolCalls = payload.ToolCalls
			p.state.InternalEventsSinceLastPush = 0
			p.state.LastPushAt = time.Now()
		}

	case "todos":
		var wrapper struct {
			Todos []TodoItem `json:"todos"`
		}
		if err := json.Unmarshal(env.Payload, &wrapper); err != nil {
			slog.Warn("failed to parse todos payload", "key", p.key, "error", err)
			p.state.Todos = nil
		} else {
			p.state.Todos = wrapper.Todos
			p.lastActivity = time.Now()
		}
		if !p.state.LastPushAt.IsZero() {
			p.state.InternalEventsSinceLastPush++
		}
		eventType = "todos"

	case "assistant_rollback":
		p.state.LastAssistantText = ""
		eventType = "assistant_rollback"

	default:
		slog.Debug("unknown headless event type", "type", env.Type)
	}

	if eventType != "" {
		_, imType, chatID := parseProcessKey(p.key)
		slog.Info("gateway event",
			"event", eventType,
			"raw_type", env.Type,
			"key", p.key,
			"workspace", p.workspaceID,
			"im", imType,
			"chat_id", chatID,
			"session_id", p.state.SessionID,
			"busy", p.state.Busy,
			"phase", p.state.Phase,
			"last_outcome", p.state.LastOutcome,
			"assistant_text_len", len(p.state.LastAssistantText),
			"assistant_tool_calls", p.state.LastAssistantToolCalls,
			"pending_confirm", p.state.PendingConfirm != nil,
			"pending_question", p.state.PendingQuestion != nil,
			"last_error", p.state.LastError,
		)
	}

	// Capture callback params under lock, then invoke outside lock to prevent
	// deadlock: onEvent → router → proc.Alive/SendCommand → p.mu.
	var (
		onEvent = p.onEvent
		key     = p.key
		state   = p.state // copy
	)
	p.mu.Unlock()

	if eventType != "" && onEvent != nil {
		onEvent(key, eventType, state)
	}
}
