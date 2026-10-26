package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/keakon/golog/log"
)

const headlessMaxEnvelopeBytes = 16 * 1024 * 1024

// transportFailureStopGrace is how long a failed headless stdout stream waits
// for the child to exit after SIGTERM before force-killing it. It is variable
// so tests can exercise the stop ordering without the full production grace.
var transportFailureStopGrace = 2 * time.Second

// readLoop reads stdout from the chord process, parses JSON envelopes,
// updates state, and calls onEvent for notable events.
func (p *ChordProcess) readLoop(ctx context.Context, stdout io.Reader) {
	defer p.images.reset()
	scanner := bufio.NewScanner(stdout)
	// Reserve room for LF or CRLF in addition to the JSON frame body.
	scanner.Buffer(make([]byte, 0, 64*1024), headlessMaxEnvelopeBytes+2)
	var readErr error

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := scanner.Bytes()
		if len(line) > headlessMaxEnvelopeBytes {
			readErr = fmt.Errorf("headless frame exceeds %d bytes", headlessMaxEnvelopeBytes)
			break
		}
		if len(line) == 0 {
			continue
		}

		var env HeadlessEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			p.reportTransportError(fmt.Errorf("parse headless envelope: %w", err), false)
			continue
		}

		p.processEnvelope(&env)
	}

	if readErr == nil {
		readErr = scanner.Err()
	}
	if readErr != nil && ctx.Err() == nil {
		p.mu.RLock()
		stopped := p.stoppedByGateway
		p.mu.RUnlock()
		if !stopped {
			// Stop before notifying: IM delivery may block, but a failed stdout
			// stream must not leave the child blocked on its remaining output.
			p.TerminateGroup(transportFailureStopGrace)
			p.mu.RLock()
			cmd := p.cmd
			p.mu.RUnlock()
			p.waitAndRecordExit(cmd)
			p.reportTransportError(fmt.Errorf("read headless envelope stream: %w", readErr), true)
		}
	}

	// The stream ended or failed; reap the process and publish its exit.
	p.handleExit()
}

// isRetryPhase reports whether a chord activity type announces a retry decision
// for the current turn: a provider round, a same-key stream retry, a fallback
// model switch, or a switch to the next API key. The turn summary counts each
// one and long-running reminders label them all "retrying".
func isRetryPhase(phase string) bool {
	return phase == "retrying" || phase == "retrying_key"
}

// processEnvelope updates ControlState based on the envelope type and calls onEvent.
func (p *ChordProcess) processEnvelope(env *HeadlessEnvelope) {
	if env.Type == "session_transfer_preparing" || env.Type == "session_transfer_status" || env.Type == "session_transfer_saved" || env.Type == "session_transfer_released" {
		p.processTransferEnvelope(env)
		return
	}
	if env.Type == imageResultEvent || env.Type == imageArtifactEvent {
		p.processImageEnvelope(env)
		return
	}
	if env.Type == "idle" {
		// Chord emits the aggregate idle after all image result chunks.
		if err := p.images.finish(); err != nil {
			p.reportTransportError(err, false)
		}
	}
	if env.Type == "session_switched" {
		p.images.reset()
	}
	if env.Type == "error" {
		var payload struct {
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(env.Payload, &payload) == nil {
			p.images.cancelRefetch(payload.RequestID)
		}
	}
	p.mu.Lock()
	now := time.Now()
	updatedAt := now.Format(time.RFC3339)

	p.lastActivity = now
	p.state.UpdatedAt = updatedAt

	var eventType string
	var sessionPin string
	var deferredLog func(string, ControlState)
	var questionEvents []questionEvent
	var ingressQuestionReceipt *QuestionReceipt

	switch env.Type {
	case "input_queue_changed":
		var queue InputQueue
		if env.Seq >= p.state.LastEnvelopeSeq && json.Unmarshal(env.Payload, &queue) == nil && p.applyInputQueue(&queue) {
			eventType = "input_queue_changed"
		}
	case "input_result":
		if p.applyInputResult(env, now) {
			eventType = "input_result"
		}
	case "ready", "ready_paused":
		var payload struct {
			SessionID         string        `json:"session_id"`
			RuntimeID         string        `json:"runtime_id"`
			SessionEpoch      *uint64       `json:"session_epoch"`
			Workspace         string        `json:"workspace"`
			TransferID        string        `json:"transfer_id"`
			BindingInstanceID string        `json:"binding_instance_id"`
			State             string        `json:"state"`
			PausedInputs      *PausedInputs `json:"paused_inputs,omitempty"`
			InputQueue        *InputQueue   `json:"input_queue,omitempty"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			inputs, err := normalizePausedInputs(payload.PausedInputs)
			if err != nil {
				deferredLog = func(string, ControlState) { log.Warnf("invalid ready paused input projection: %v", err) }
				break
			}
			if strings.TrimSpace(payload.SessionID) == "" {
				deferredLog = func(key string, _ ControlState) { log.Warnf("drop ready event: empty session id key=%s", key) }
				break
			}
			if p.expectedSessionID != "" && payload.SessionID != p.expectedSessionID {
				sessionID, expected := payload.SessionID, p.expectedSessionID
				deferredLog = func(key string, _ ControlState) {
					log.Warnf("drop ready event: session %q != expected %q key=%s", sessionID, expected, key)
				}
				break
			}
			if p.restoringTransferID != "" && (payload.TransferID != p.restoringTransferID || payload.BindingInstanceID != p.bindingInstanceID || (env.Type != "ready_paused" && payload.State != "ready_paused")) {
				transferID, expected := payload.TransferID, p.restoringTransferID
				deferredLog = func(key string, _ ControlState) {
					log.Warnf("drop ready_paused event: transfer identity mismatch key=%s transfer=%q expected=%q", key, transferID, expected)
				}
				break
			}
			if p.restoringTransferID != "" && (payload.RuntimeID == "" || payload.SessionEpoch == nil || inputs == nil || !inputs.Paused) {
				deferredLog = func(key string, _ ControlState) {
					log.Warnf("drop ready_paused event: incomplete paused projection key=%s", key)
				}
				break
			}
			p.runtimeID = payload.RuntimeID
			if payload.Workspace != "" {
				p.workspacePath = payload.Workspace
			}
			p.sessionEpoch = 0
			if payload.SessionEpoch != nil {
				p.sessionEpoch = *payload.SessionEpoch
			}
			p.state.BindingInstanceID = p.bindingInstanceID
			p.state.PausedInputs = inputs
			p.readyPaused = env.Type == "ready_paused" || payload.State == "ready_paused"
			if p.readySignal != nil {
				p.readyOnce.Do(func() { close(p.readySignal) })
			}
			if strings.TrimSpace(payload.SessionID) != "" {
				p.state.SessionID = payload.SessionID
				p.state.InputQueue = nil
				p.applyInputQueue(payload.InputQueue)
				if p.restoringTransferID == "" && !p.inputBlockedLocked() {
					sessionPin = payload.SessionID
				}
			}
		}
		deferredLog = func(key string, state ControlState) {
			log.Infof("[%v] gateway event event=%v raw_type=%v", processLogContext(key, state), "ready", "ready")
		}
		// No notification.
		eventType = ""

	case "activity":
		if env.Seq < p.state.LastEnvelopeSeq {
			break
		}
		if !p.state.Busy {
			p.state.LastPushAt = now
			p.state.InternalEventsSinceLastPush = 0
		}
		p.state.ensureTurnWindow(now)
		p.state.setAgentBusy(true)
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
			// Each retry activity is one retry decision Chord announced for
			// this turn. It is a pressure signal for reminders, not an exact
			// failure count.
			if isRetryPhase(payload.Type) {
				p.state.TurnRetries++
			}
		}
		eventType = "activity"

	case "idle":
		p.transitionToIdle("", false)
		p.state.SuppressUserNotification = false
		var payload struct {
			RunningJobs              int    `json:"running_jobs"`
			LastOutcome              string `json:"last_outcome"`
			SuppressUserNotification bool   `json:"suppress_user_notification"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.RunningJobs = payload.RunningJobs
			p.state.LastOutcome = payload.LastOutcome
			p.state.SuppressUserNotification = payload.SuppressUserNotification
		}
		p.state.finishTurnWindow(now)
		eventType = "idle"

	case "confirm_request":
		var payload ConfirmPayload
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.applyPendingConfirm(&payload)
		}
		eventType = "confirm_request"

	case "confirm_resolved":
		var payload ConfirmResolutionPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil || payload.RequestID == "" || !validConfirmOutcome(payload.Outcome) {
			break
		}
		if pc := p.state.PendingConfirm; pc != nil && pc.RequestID == payload.RequestID {
			switch payload.Outcome {
			case ConfirmOutcomeTimeout, ConfirmOutcomeCancelled:
				p.state.ExpiredConfirm = pc
			case ConfirmOutcomeApproved, ConfirmOutcomeDenied:
				p.state.ExpiredConfirm = nil
			}
			p.state.PendingConfirm = nil
		}
		p.state.LastConfirmResolved = &payload
		eventType = "confirm_resolved"

	case "question_request", "question_updated", "question_resolved":
		var payload QuestionPayload
		if json.Unmarshal(env.Payload, &payload) == nil && payload.RequestID != "" {
			old := p.state.Questions[payload.RequestID]
			if old == nil && env.Seq < p.state.LastEnvelopeSeq || old != nil && old.BindingID != payload.BindingID {
				break
			}
			// A newer snapshot must not swallow committed terminal/revision
			// events, but older open states must never reopen the question.
			if old != nil && old.Version > payload.Version && (old.Outcome == "" || payload.Outcome == "") {
				break
			}
			p.state.applyQuestionState(&payload)
			if p.markQuestionEvent(env.Type, &payload) {
				questionEvents = append(questionEvents, p.questionEvent(env.Type, &payload))
			}
		}
	case "question_receipt":
		var receipt QuestionReceipt
		if json.Unmarshal(env.Payload, &receipt) == nil {
			ingressQuestionReceipt = &receipt
			if ch := p.questionWaiters[receipt.OperationID]; ch != nil {
				select {
				case ch <- receipt:
				default:
				}
			} else if !receipt.Accepted && strings.HasPrefix(receipt.OperationID, "gateway:presented:") {
				p.state.LastError = "Could not open question " + receipt.RequestID + ": " + receipt.Error + ". Check /status to retry."
				eventType = "question_presentation_error"
			}
		}
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
			p.state.finishLocalShell()
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

	case mcpStatusChangedEvent:
		if env.Seq < p.state.LastEnvelopeSeq {
			break
		}
		var snapshot MCPStatusSnapshot
		if json.Unmarshal(env.Payload, &snapshot) == nil && p.state.applyMCPStatus(&snapshot) {
			text := formatMCPNotification(p.state.MCP)
			identity := snapshot.BindingID + "\n" + text
			if text != "" && identity != p.lastMCPNotification {
				p.lastMCPNotification = identity
				// A fresh process starting a session reports its first MCP
				// view as fully available. That summary reports nothing that
				// changed for the chat, so it is not pushed; unavailable
				// servers and later recoveries still notify once each.
				if text != mcpAllAvailableNotification || p.mcpNotified {
					p.mcpNotified = true
					eventType = mcpStatusChangedEvent
				}
			}
			if len(snapshot.Servers) == 0 {
				p.lastMCPNotification = ""
			}
		}

	case "status_response":
		var resp StatusResponse
		if err := json.Unmarshal(env.Payload, &resp); err == nil {
			inputs, err := normalizePausedInputs(resp.PausedInputs)
			if err != nil {
				deferredLog = func(string, ControlState) { log.Warnf("invalid status paused input projection: %v", err) }
				break
			}
			resp.PausedInputs = inputs
			if env.Seq < p.state.LastEnvelopeSeq {
				// Stale snapshot: a newer push already moved the aggregated
				// state past what this copy holds (the status copy and the
				// push emit run on different chord goroutines with no shared
				// order). Merging it would roll SessionID, busy, and pending
				// interactions back, so skip the merge. Waiters below still get
				// the current aggregated state.
				deferredLog = func(key string, state ControlState) {
					log.Debugf("[%v] gateway dropped stale status_response seq=%v last_seq=%v", processLogContext(key, state), env.Seq, state.LastEnvelopeSeq)
				}
			} else {
				previous := p.state.Questions
				if resp.SessionID != p.state.SessionID {
					previous = nil
					p.questionEventVersions = nil
					p.state.resetTurnWindow()
				}
				if resp.SessionID != p.state.SessionID {
					p.state.InputQueue = nil
				}
				p.state.applyStatusResponse(&resp)
				if p.applyInputQueue(resp.InputQueue) {
					eventType = "input_queue_changed"
				}
				if resp.Workspace != "" {
					p.workspacePath = resp.Workspace
				}
				if resp.Busy {
					p.state.ensureTurnWindow(now)
				}
				questionEvents = p.reconcileQuestionEvents(previous)
			}
			// Wake any goroutines blocked in WaitStatus.
			p.statusWaiters.notify(p.state)
		}
		// The solicited response also reconciles question presentation/closure.

	case "role_response":
		var resp RoleResponse
		if err := json.Unmarshal(env.Payload, &resp); err == nil {
			if resp.OK && strings.TrimSpace(resp.Role) != "" {
				if p.state.CurrentRole != resp.Role {
					p.state.MCP = nil
				}
				p.state.CurrentRole = resp.Role
			}
			// Wake any goroutines blocked in WaitRoleList/WaitRoleSwitch.
			p.roleWaiters.notify(resp)
		}
		// No onEvent — solicited response.

	case "workdir_changed":
		var payload struct {
			Path       string `json:"path"`
			WorktreeID string `json:"worktree_id,omitempty"`
			Generation int    `json:"generation,omitempty"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil && strings.TrimSpace(payload.Path) != "" {
			p.state.Workdir = WorkdirSnapshot{Path: payload.Path, WorktreeID: payload.WorktreeID, Generation: payload.Generation}
		}
		// No onEvent: the change is reported through the next /status read,
		// which is where the operator sees paths.

	case "models_response":
		var resp ModelsResponse
		if err := json.Unmarshal(env.Payload, &resp); err == nil {
			p.modelsWaiters.notify(resp)
		}
		// No onEvent — solicited response.

	case "input_queue_result":
		var result InputQueueResult
		if err := json.Unmarshal(env.Payload, &result); err == nil && result.OperationID != "" {
			p.notifyInputQueueResult(result)
		}
		// No onEvent — solicited response. The queue state it changes arrives
		// separately as input_queue_changed, which already drives the cache.
	case "confirm_rule_intent_result":
		var result ConfirmRuleIntentResult
		if err := json.Unmarshal(env.Payload, &result); err == nil && result.RequestID != "" {
			p.notifyConfirmRuleIntentResult(result)
		}
		// No onEvent — this is a correlated persistence receipt.

	case "role_change":
		var payload struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil && strings.TrimSpace(payload.Role) != "" {
			if p.state.CurrentRole != payload.Role {
				p.state.MCP = nil
			}
			p.state.CurrentRole = payload.Role
		}
		// No onEvent: a role change is acknowledged synchronously by the
		// role_response that preceded it; the state cache is enough for /status.

	case "session_switched":
		var payload struct {
			SessionID    string      `json:"session_id"`
			RuntimeID    string      `json:"runtime_id"`
			SessionEpoch uint64      `json:"session_epoch"`
			InputQueue   *InputQueue `json:"input_queue,omitempty"`
			Workspace    string      `json:"workspace"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			if id := strings.TrimSpace(payload.SessionID); id != "" {
				if p.inputBlockedLocked() {
					break
				}
				p.expectedSessionID = ""
				if payload.RuntimeID != "" {
					p.runtimeID = payload.RuntimeID
				}
				p.sessionEpoch = payload.SessionEpoch
				p.state.MCP = nil
				if payload.Workspace != "" {
					p.workspacePath = payload.Workspace
				}
				p.state.resetTurnWindow()
				p.state.RunningJobs = 0
				p.state.SessionID = id
				p.state.PausedInputs = nil
				p.state.InputQueue = nil
				p.applyInputQueue(payload.InputQueue)
				p.state.Questions = nil
				p.state.LastQuestion = nil
				p.state.LastQuestionResolved = nil
				p.questionEventVersions = nil
				// Compaction outcomes belong to the abandoned session: keep
				// them and /status would report the old session's checkpoint,
				// while the old plan's slot owner would drop the new
				// session's terminals (or vice versa). Reset both.
				p.state.LastCompaction = nil
				p.compactionPlanID = ""
				// A late answer qualifies only for the session that asked the
				// question, so the switch drops that record along with the
				// abandoned session's compaction outcome.
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
		// The switch carries no user-visible message — the command that
		// triggered it already answered the user, and the state cache keeps
		// /status and later spawns on the session the runtime actually runs.
		// It is still pushed so the router re-syncs the state it mirrors from
		// this one, including the late-answer qualification dropped above.
		eventType = "session_switched"

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
			// Sum every agent's rounds so the turn summary counts the whole
			// turn, not just its last message.
			p.state.TurnToolCalls += payload.ToolCalls
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

	// Track the newest state version seen. chord numbers state-carrying
	// envelopes monotonically and pushes arrive in seq order, so max() here is
	// the high-water mark the status_response case guards against. A stale seq
	// can never advance it, which keeps this update unconditionally safe.
	if env.Seq > p.state.LastEnvelopeSeq {
		p.state.LastEnvelopeSeq = env.Seq
	}

	// Capture callback params under lock, then invoke outside lock to prevent
	// deadlock: onEvent → router → proc.Alive/SendCommand → p.mu.
	var (
		onEvent           = p.onEvent
		key               = p.key
		state             = p.state // copy
		onQuestionReceipt = p.onQuestionReceipt
	)
	// Queue projections are replaced, never mutated internally. Copy only
	// when publishing to a caller that may mutate its own snapshot.
	p.mu.Unlock()
	if ingressQuestionReceipt != nil && onQuestionReceipt != nil && !onQuestionReceipt(p, *ingressQuestionReceipt) {
		p.mu.Lock()
		p.inputReceiptBlocked = true
		p.state.LastError = "input_receipt_backpressure: question receipt application unavailable; check ingress and the original session"
		p.mu.Unlock()
	}

	if deferredLog != nil {
		deferredLog(key, state)
	}

	logEvent := func(eventType string, state ControlState) {
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
			state.pendingQuestionCount() > 0,
			state.LastError,
		}
		if p.eventLogf != nil {
			p.eventLogf(format, args...)
		} else {
			log.Infof(format, args...)
		}
	}
	if eventType != "" {
		logEvent(eventType, state)
	}
	for _, event := range questionEvents {
		logEvent(event.kind, event.state)
	}

	if sessionPin != "" && p.mgr != nil && p.mgr.pins != nil && !p.mgr.transferBlocked(key) {
		if err := p.mgr.publishSessionPin(p, sessionPin); err != nil {
			log.Warnf("[%v] persist session pin failed error=%v", processLogContext(key, state), err)
		}
	}

	if eventType != "" && onEvent != nil {
		state.InputQueue = state.InputQueue.clone()
		onEvent(key, eventType, state)
	}
	if onEvent != nil {
		for _, event := range questionEvents {
			event.state.InputQueue = event.state.InputQueue.clone()
			onEvent(key, event.kind, event.state)
		}
	}
}
