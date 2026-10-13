package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/keakon/chord-gateway/config"
)

func (r *NotificationRouter) formatNotification(eventType string, state ControlState) string {
	switch eventType {
	case "session_transfer_preparing", "session_transfer_status", "session_transfer_saved", "session_transfer_released":
		if state.Transfer != nil {
			return formatTransferStatus(*state.Transfer)
		}
		return ""
	case mcpStatusChangedEvent:
		return formatMCPNotification(state.MCP)
	case "input_result":
		if result := state.LastInputResult; result != nil {
			switch result.Status {
			case inputRejected:
				return "Input rejected: " + result.Message
			case inputUnknown:
				return "Input outcome is unconfirmed. Check the original session on the host before resending; the gateway will not retry it automatically."
			}
		}
		return ""
	case "notification", "done_completion":
		return r.formatHeadlessNotification(state)

	case "confirm_request":
		return r.formatConfirmNotification(state)

	case "question_revision":
		q := state.questionForNotification()
		if q == nil || q.RevisionResult == nil {
			return ""
		}
		return truncate("✅ Historical answer updated (" + q.RequestID + "): " + strings.Join(q.RevisionResult.Selected, ", "))
	case "question_request":
		return r.formatQuestionNotification(state)

	case "question_resolved":
		return r.formatQuestionResolvedNotification(state)
	case "question_presentation_error":
		return "⚠️ " + state.LastError

	case "handoff_request":
		return r.formatHandoffNotification(state)

	case "handoff_cancelled":
		return r.formatHandoffCancelledNotification(state)

	case "idle":
		return r.formatIdleNotification(state)

	case "idle_timeout":
		return r.formatExpiredPendingNotification(state)

	case "error":
		return ""

	case "transport_error":
		return r.formatTransportErrorNotification(state)

	case "agent_started":
		return r.formatAgentStartedNotification(state)

	case "agent_notify":
		return r.formatAgentNotifyNotification(state)

	case "agent_done":
		return r.formatAgentDoneNotification(state)

	case "background_result":
		return r.formatBackgroundResultNotification(state)

	case "context_notice":
		return r.formatContextNoticeNotification(state)

	case "assistant_message":
		if state.LastAssistantText == "" {
			return ""
		}
		if state.LastAssistantAgentID != "" {
			return truncate(fmt.Sprintf("🤖 %s\n\n%s", formatAgentLabel(state.LastAssistantAgentType, state.LastAssistantAgentID, state.LastAssistantTaskID), strings.TrimSpace(state.LastAssistantText)))
		}
		return state.LastAssistantText

	case "local_shell_result":
		return r.formatLocalShellNotification(state)

	case "info":
		return r.formatInfoNotification(state)

	case "toast":
		return r.formatToastNotification(state)

	case "activity":
		// Too noisy, don't push.
		return ""

	case "exit":
		if state.LastTransportError != nil && state.LastTransportError.Fatal {
			return "" // The transport notice already explains the disconnect.
		}
		if state.UnconfirmedInputs > 0 {
			return fmt.Sprintf("Chord disconnected with %d unconfirmed inputs. Check the session before sending them again.", state.UnconfirmedInputs)
		}
		return r.formatExitNotification(state)

	case "todos":
		return r.formatTodosNotification(state)

	default:
		return ""
	}
}

func (r *NotificationRouter) formatAgentStartedNotification(state ControlState) string {
	payload := state.LastAgentStarted
	if payload == nil {
		return ""
	}
	label := formatAgentLabel(payload.AgentType, payload.AgentID, payload.TaskID)
	description := strings.TrimSpace(payload.Description)
	if description == "" {
		return truncate("🧩 Delegated " + label)
	}
	return truncate(fmt.Sprintf("🧩 Delegated %s\n%s", label, description))
}

func (r *NotificationRouter) formatAgentNotifyNotification(state ControlState) string {
	payload := state.LastAgentNotify
	if payload == nil || strings.TrimSpace(payload.Message) == "" {
		return ""
	}
	label := formatAgentLabel(payload.AgentType, payload.AgentID, payload.TaskID)
	kind := strings.TrimSpace(payload.Kind)
	sub := strings.TrimSpace(payload.Subtype)
	switch {
	case kind != "" && sub != "":
		label += " · " + kind + "/" + sub
	case kind != "":
		label += " · " + kind
	case sub != "":
		label += " · " + sub
	}
	return truncate(fmt.Sprintf("📣 %s\n%s", label, strings.TrimSpace(payload.Message)))
}

func (r *NotificationRouter) formatAgentDoneNotification(state ControlState) string {
	payload := state.LastAgentDone
	if payload == nil {
		return ""
	}
	message := "✅ " + formatAgentLabel(payload.AgentType, payload.AgentID, payload.TaskID) + " completed"
	summary := strings.TrimSpace(payload.Summary)
	if summary != "" {
		message += "\n" + summary
	}
	return truncate(message)
}

// formatBackgroundResultNotification reports a finished background job's
// durable result. chord headless has no other channel for it: the job usually
// finishes after the turn went idle, so no later assistant_message summarizes
// it, and dropping this event would lose the result from IM entirely.
func (r *NotificationRouter) formatBackgroundResultNotification(state ControlState) string {
	payload := state.LastBackgroundResult
	if payload == nil {
		return ""
	}
	content := strings.TrimSpace(payload.Content)
	if content == "" {
		return ""
	}
	message := "📥 Background job result"
	if target := strings.TrimSpace(payload.TargetAgentID); target != "" {
		message += " · " + target
	}
	return truncate(message + "\n" + content)
}

// formatContextNoticeNotification reports a durable context-pressure warning.
// Level mirrors the compaction-gate overlays (pressure, imminent, warning). A
// pressure cycle keeps at most one durable row: later escalations stay internal
// to Chord and a retracted row is never announced as a withdrawal, so the
// notice is forwarded as-is instead of being deduplicated here.
func (r *NotificationRouter) formatContextNoticeNotification(state ControlState) string {
	payload := state.LastContextNotice
	if payload == nil {
		return ""
	}
	message := strings.TrimSpace(payload.Message)
	if message == "" {
		return ""
	}
	switch strings.TrimSpace(payload.Level) {
	case "pressure":
		return truncate("📈 " + message)
	case "imminent":
		return truncate("⚠️ " + message)
	case "warning":
		return truncate("⏳ " + message)
	default:
		return truncate(message)
	}
}

func formatAgentLabel(agentType, agentID, taskID string) string {
	label := strings.TrimSpace(agentType)
	if label == "" {
		label = strings.TrimSpace(agentID)
	}
	if label == "" {
		label = "SubAgent"
	}
	if taskID = strings.TrimSpace(taskID); taskID != "" {
		label += " · " + taskID
	}
	return label
}

func (r *NotificationRouter) formatLocalShellNotification(state ControlState) string {
	if state.LastLocalShell == nil {
		return ""
	}
	payload := state.LastLocalShell
	status := "✅"
	if payload.Failed {
		status = "❌"
	}
	output := strings.TrimSpace(payload.Output)
	if output == "" {
		output = "(no output)"
	}
	msg := fmt.Sprintf("%s Local shell: %s\n%s", status, payload.Command, output)
	if payload.Failed && strings.TrimSpace(payload.Error) != "" {
		msg += "\nError: " + strings.TrimSpace(payload.Error)
	}
	return truncate(msg)
}

func (r *NotificationRouter) formatExpiredPendingNotification(state ControlState) string {
	if state.ExpiredConfirm != nil {
		return "⌛ The pending confirmation has expired. It was not approved or denied. Please retry the original request if confirmation is still needed."
	}
	if state.ExpiredHandoff != nil {
		return "⌛ The pending handoff request has expired. It was not accepted or denied. Please retry the original request if handoff is still needed."
	}
	return ""
}

// formatHandoffCancelledNotification reports a pending handoff that Chord
// cancelled without a decision, e.g. because a new request or session switch
// superseded it. It reuses the ExpiredHandoff slot so the router keeps a
// record of the cancelled request.
func (r *NotificationRouter) formatHandoffCancelledNotification(state ControlState) string {
	if state.ExpiredHandoff == nil {
		return ""
	}
	return "⌛ The pending handoff request was cancelled (superseded by a new request or session switch)."
}

func (r *NotificationRouter) formatIdleNotification(state ControlState) string {
	if msg := r.formatExpiredPendingNotification(state); msg != "" {
		return msg
	}
	if state.SuppressUserNotification {
		return ""
	}
	summary := formatTurnSummary(state.LastTurnDuration, state.LastTurnToolCalls, state.LastTurnRetries)
	msg := "✅ Chord: Ready for input"
	if state.RunningJobs > 0 || state.PendingLocalShells > 0 {
		msg = "⏳ Chord: Ready for input"
	}
	if state.RunningJobs > 0 {
		msg += fmt.Sprintf("; background jobs still running: %d", state.RunningJobs)
	}
	if state.PendingLocalShells > 0 {
		msg += fmt.Sprintf("; local shell commands still running: %d", state.PendingLocalShells)
	}
	if summary != "" {
		msg += " (" + summary + ")"
	}
	return msg
}

// formatHeadlessNotification renders the notification event. chord reports
// user_input_required as an alert that the agent is blocked on the user; the
// Delivery deduplicates a known companion alert only after its matching prompt
// was sent successfully. A pending cache entry alone cannot identify an alert.
func (r *NotificationRouter) formatHeadlessNotification(state ControlState) string {
	if state.LastNotification == nil {
		return ""
	}
	msg := strings.TrimSpace(state.LastNotification.Message)
	if msg == "" {
		return ""
	}
	if state.LastNotification.Reason != notificationReasonUserInputRequired {
		return truncate(msg)
	}
	return truncate("❓ " + msg)
}

func (r *NotificationRouter) formatConfirmNotification(state ControlState) string {
	if state.PendingConfirm == nil {
		return ""
	}
	c := state.PendingConfirm
	if isDoneTool(c.ToolName) {
		return r.formatDoneConfirmNotification(c)
	}
	var sb strings.Builder
	sb.WriteString("🔧 Confirm required: ")
	sb.WriteString(c.ToolName)

	// Show a human-readable summary of the tool args so the user knows
	// what the tool will actually do (e.g. which command Shell will run,
	// which file Write will modify).
	if summary := summarizeToolArgs(c.ToolName, c.ArgsJSON); summary != "" {
		sb.WriteString("\n")
		sb.WriteString(summary)
	}

	if len(c.NeedsApproval) > 0 {
		sb.WriteString("\n")
		for _, p := range c.NeedsApproval {
			sb.WriteString("  • ")
			sb.WriteString(p)
		}
	}
	sb.WriteString("\nReply /allow or /deny [reason]")
	return truncate(sb.String())
}

func doneConfirmReportReason(c *ConfirmPayload) (report, reason string) {
	if c == nil {
		return "", ""
	}
	report = strings.TrimSpace(c.DoneReport)
	reason = strings.TrimSpace(c.DoneReason)
	argReport, argReason := parseDoneArgs(c.ArgsJSON)
	if report == "" {
		report = argReport
	}
	if reason == "" {
		reason = argReason
	}
	return report, reason
}

func (r *NotificationRouter) formatDoneConfirmNotification(c *ConfirmPayload) string {
	if c == nil {
		return ""
	}
	report, reason := doneConfirmReportReason(c)
	var sb strings.Builder
	sb.WriteString("✅ Done requests completion")
	if reason != "" {
		sb.WriteString("\nReason: ")
		sb.WriteString(reason)
	}
	if report != "" {
		sb.WriteString("\n\n")
		sb.WriteString(report)
	}
	sb.WriteString("\n\nReply /allow to finish, or /deny <reason> to continue.")
	return truncate(sb.String())
}

func parseDoneArgs(argsJSON string) (report, reason string) {
	if strings.TrimSpace(argsJSON) == "" {
		return "", ""
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", ""
	}
	report, _ = args["report"].(string)
	reason, _ = args["reason"].(string)
	return strings.TrimSpace(report), strings.TrimSpace(reason)
}

func isDoneTool(toolName string) bool {
	return strings.EqualFold(strings.TrimSpace(toolName), "Done")
}

// summarizeToolArgs extracts a human-readable summary from the tool's JSON args.
func summarizeToolArgs(toolName, argsJSON string) string {
	if strings.TrimSpace(argsJSON) == "" {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		// Not valid JSON — show raw (truncated).
		return truncateLine(argsJSON, 200)
	}

	switch toolName {
	case "Done":
		report, reason := parseDoneArgs(argsJSON)
		if report != "" {
			return report
		}
		if reason != "" {
			return reason
		}
	case "Shell", "Spawn":
		if cmd, _ := args["command"].(string); cmd != "" {
			return "$ " + truncateLine(cmd, 300)
		}
	case "Write", "Edit":
		if path, _ := args["path"].(string); path != "" {
			return "📝 " + truncateLine(path, 160)
		}
	case "Delete":
		if paths, _ := args["paths"].([]any); len(paths) > 0 {
			if s, ok := paths[0].(string); ok {
				return "🗑️ " + truncateLine(s, 160)
			}
		}
	case "Read":
		if path, _ := args["path"].(string); path != "" {
			return "📖 " + truncateLine(path, 160)
		}
	case "Grep", "Glob":
		if pat, _ := args["pattern"].(string); pat != "" {
			return "🔍 " + truncateLine(pat, 160)
		}
	case "WebFetch":
		if url, _ := args["url"].(string); url != "" {
			return "🌐 " + truncateLine(url, 160)
		}
	case "Lsp":
		// Show operation + path if available
		op, _ := args["operation"].(string)
		path, _ := args["path"].(string)
		summary := strings.TrimSpace(op + " " + path)
		if summary != "" {
			return "🔎 " + truncateLine(summary, 180)
		}
	}

	// Generic fallback: show key=value pairs for known fields, or raw JSON.
	var parts []string
	for _, key := range []string{"command", "path", "url", "pattern", "content", "description"} {
		if v, ok := args[key]; ok {
			s := fmt.Sprintf("%v", v)
			parts = append(parts, key+"="+truncateLine(s, 120))
		}
		if len(parts) >= 3 {
			break
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	// Last resort: raw JSON (truncated).
	return truncateLine(argsJSON, 200)
}

func (r *NotificationRouter) formatHandoffNotification(state ControlState) string {
	if state.PendingHandoff == nil {
		return ""
	}
	h := state.PendingHandoff
	var sb strings.Builder
	sb.WriteString("🤝 Handoff requested")
	if strings.TrimSpace(h.PlanPath) != "" {
		sb.WriteString("\n📄 Plan: ")
		sb.WriteString(h.PlanPath)
	}
	if strings.TrimSpace(h.PlanError) != "" {
		sb.WriteString("\n⚠️ Failed to read full plan: ")
		sb.WriteString(h.PlanError)
	}
	if strings.TrimSpace(h.PlanText) != "" {
		sb.WriteString("\n\n")
		sb.WriteString(h.PlanText)
	}
	if len(h.Agents) > 0 {
		sb.WriteString("\n\nAgents / model pools:")
		for _, agent := range h.Agents {
			if strings.TrimSpace(agent.Name) == "" {
				continue
			}
			sb.WriteString("\n- ")
			sb.WriteString(agent.Name)
			if agent.Default {
				sb.WriteString(" (default)")
			}
			if strings.TrimSpace(agent.CurrentModelPool) != "" {
				sb.WriteString(" current=")
				sb.WriteString(agent.CurrentModelPool)
			}
			if len(agent.ModelPools) > 0 {
				sb.WriteString(" pools=")
				sb.WriteString(strings.Join(agent.ModelPools, ", "))
			}
		}
	}
	sb.WriteString("\n\nReply /handoff <agent> [model_pool] to execute, /handoff to use the default, or /handoff-deny <reason> to reject.")
	return sb.String()
}

func (r *NotificationRouter) formatQuestionNotification(state ControlState) string {
	if state.questionForNotification() == nil {
		return ""
	}
	q := state.questionForNotification()
	var sb strings.Builder
	sb.WriteString("❓ ")
	if q.Header != "" {
		sb.WriteString(q.Header)
		sb.WriteString(": ")
	}
	sb.WriteString(q.Question)
	if len(q.Options) > 0 {
		for i, opt := range q.Options {
			sb.WriteString("\n  ")
			sb.WriteString(strconv.Itoa(i + 1))
			sb.WriteString(". ")
			sb.WriteString(opt)
			if i < len(q.OptionDetails) && q.OptionDetails[i] != "" && q.OptionDetails[i] != opt {
				sb.WriteString(" — ")
				sb.WriteString(q.OptionDetails[i])
			}
		}
	}
	if q.Multiple {
		sb.WriteString(" (multi-select)")
	}
	if len(q.Options) == 0 {
		sb.WriteString("\nReply /answer @" + q.RequestID + " <answer>")
	} else if q.Multiple {
		sb.WriteString("\nReply /answer @" + q.RequestID + " 1,2")
	} else {
		sb.WriteString("\nReply /answer @" + q.RequestID + " 1")
	}
	return truncate(sb.String())
}

// formatQuestionResolvedNotification reports a pending question that chord
// closed. Only answered means the submitted answer was accepted; the rest are
// terminal states where no answer was recorded.
func (r *NotificationRouter) formatQuestionResolvedNotification(state ControlState) string {
	p := state.LastQuestionResolved
	if p == nil {
		return ""
	}
	_, message, _ := questionResolvedCardStatus(p.Outcome)
	switch p.Outcome {
	case QuestionResolvedDefaulted:
		message = "⌛ No answer received. Core adopted: " + strings.Join(p.Selected, ", ") + " (system decision, not user consent)."
	case QuestionResolvedAnswered:
		message = "✅ The answer was accepted: " + strings.Join(p.Selected, ", ")
	case QuestionResolvedNoResponse:
		message = "⌛ No answer before the deadline. You can still revise this question by ID, for example /answer @" + p.RequestID + " <answer>; the original outcome stays."
	}
	return truncate("Question: " + p.RequestID + "\n" + message)
}

// questionResolvedCardStatus maps a close reason to the card title, status
// text, and template used when patching the question card.
func questionResolvedCardStatus(reason string) (title, message, template string) {
	switch reason {
	case QuestionResolvedDefaulted:
		return "Question defaulted", "⌛ Core adopted the default (system decision, not user consent).", "grey"
	case QuestionResolvedAnswered:
		return "Question answered", "✅ Answer accepted by chord.", "green"
	case QuestionResolvedDeclined:
		return "Question declined", "🚫 Declined; no answer was recorded.", "grey"
	case QuestionResolvedNoResponse:
		return "Question expired", "⌛ No answer before the deadline.", "grey"
	case QuestionResolvedSuperseded:
		return "Question superseded", "🔁 Superseded by a newer message.", "grey"
	case QuestionResolvedCancelled:
		return "Question cancelled", "⚠️ Cancelled before an answer was recorded.", "grey"
	default:
		return "Question closed", "⚠️ Closed after an execution error.", "red"
	}
}

func (r *NotificationRouter) formatInfoNotification(state ControlState) string {
	if state.InfoMessage == "" {
		return ""
	}
	return truncate("ℹ️ " + state.InfoMessage)
}

func (r *NotificationRouter) formatToastNotification(state ControlState) string {
	if state.ToastMessage == "" {
		return ""
	}
	// Only push warn/error level toasts.
	switch state.ToastLevel {
	case "warn", "error":
		return truncate("🔔 " + state.ToastMessage)
	default:
		return ""
	}
}

func (r *NotificationRouter) formatTransportErrorNotification(state ControlState) string {
	failure := state.LastTransportError
	if failure == nil {
		return ""
	}
	if !failure.Fatal {
		return "⚠️ Invalid Chord message skipped; reading continues. " + failure.Message
	}
	message := "⚠️ Chord connection failed and the process was stopped. " + failure.Message
	if state.UnconfirmedInputs > 0 {
		message += fmt.Sprintf("\n%d inputs have unconfirmed outcomes.", state.UnconfirmedInputs)
	}
	return message + "\nCheck the session before retrying. Inputs were not replayed."
}

func (r *NotificationRouter) formatExitNotification(state ControlState) string {
	if state.Busy {
		return "🔌 Chord process exited unexpectedly."
	}
	return ""
}

func formatTodoList(todos []TodoItem) string {
	if len(todos) == 0 {
		return "📋 No todos."
	}

	var lines []string
	for _, t := range todos {
		prefix := "⬜"
		switch t.Status {
		case "in_progress":
			prefix = "▶"
		case "completed":
			prefix = "✅"
		case "cancelled":
			prefix = "❌"
		}
		line := fmt.Sprintf("%s %s", prefix, t.Content)
		if t.ActiveForm != "" {
			line += fmt.Sprintf(" (%s)", t.ActiveForm)
		}
		lines = append(lines, line)
	}
	return truncate("📋 Todos:\n" + strings.Join(lines, "\n"))
}

func (r *NotificationRouter) formatTodosNotification(state ControlState) string {
	return formatTodoList(state.Todos)
}

// formatTurnSummary renders the compact progress summary for a completed turn.
// Empty parts are omitted, so a turn that did no measurable work keeps the
// plain completion message.
func formatTurnSummary(duration time.Duration, toolCalls, retries int) string {
	var parts []string
	if duration >= time.Second {
		parts = append(parts, formatTurnDuration(duration))
	}
	if toolCalls > 0 {
		parts = append(parts, formatCount(toolCalls, "tool", "tools"))
	}
	if retries > 0 {
		parts = append(parts, formatCount(retries, "retry", "retries"))
	}
	return strings.Join(parts, " · ")
}

// formatTurnDuration renders a duration for a chat line: seconds under a
// minute, minutes and seconds under an hour, hours and minutes above.
func formatTurnDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		if s := int(d.Seconds()) % 60; s > 0 {
			return fmt.Sprintf("%dm%02ds", int(d.Minutes()), s)
		}
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func formatCount(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// formatTurnStage maps a chord activity type to a coarse, user-facing stage for
// long-running reminders. Transport-level phases (connecting, waiting for
// headers or tokens) stay hidden: they are brief, internal, and the earlier
// product decision keeps them out of IM updates.
func formatTurnStage(phase string) string {
	if isRetryPhase(phase) {
		return "retrying"
	}
	switch phase {
	case "preparing":
		return "preparing"
	case "streaming":
		return "receiving reply"
	case "executing":
		return "running tools"
	case "compacting":
		return "checkpointing context"
	case "cooling":
		return "cooling down"
	default:
		return ""
	}
}

func (r *NotificationRouter) formatLongRunningNotification(state ControlState, now time.Time) string {
	waitingForInput := state.PendingConfirm != nil || state.Phase == "waiting_input" && state.pendingQuestionCount() > 0
	if state.PendingLocalShells == 0 && waitingForInput {
		return ""
	}
	var parts []string
	started := state.TurnStartedAt
	stage := formatTurnStage(state.Phase)
	showShell := state.PendingLocalShells > 0 && (started.IsZero() || waitingForInput)
	if showShell {
		started = state.LocalShellStartedAt
		stage = "running local shell"
	}
	if !started.IsZero() {
		if elapsed := now.Sub(started); elapsed >= time.Second {
			parts = append(parts, formatTurnDuration(elapsed))
		}
	}
	if stage != "" {
		parts = append(parts, stage)
	}
	if !showShell && state.TurnToolCalls > 0 {
		parts = append(parts, formatCount(state.TurnToolCalls, "tool", "tools"))
	}
	if !showShell && state.TurnRetries > 0 {
		parts = append(parts, formatCount(state.TurnRetries, "retry", "retries"))
	}
	if state.InternalEventsSinceLastPush > 0 {
		parts = append(parts, fmt.Sprintf("%d internal events", state.InternalEventsSinceLastPush))
	}
	msg := "⏳ Still working"
	if len(parts) > 0 {
		msg += " (" + strings.Join(parts, " · ") + ")"
	}
	return truncate(msg)
}

func workspaceDisplayName(ws *config.Workspace) string {
	if ws == nil {
		return "(unknown)"
	}
	base := filepath.Base(strings.TrimRight(ws.Path, string(os.PathSeparator)))
	if base == "." || base == string(os.PathSeparator) || base == "" {
		base = ws.Path
	}
	return base
}

func formatBindingStatus(ws *config.Workspace, imType, chatID string, state ControlState) string {
	var sb strings.Builder
	if state.PausedInputs != nil && state.PausedInputs.Paused {
		sb.WriteString("⏸️ Paused")
	} else if state.Busy {
		sb.WriteString("🔄 Busy")
	} else {
		sb.WriteString("⏸️ Idle")
	}
	if status := formatMCPStatus(state.MCP); status != "" {
		sb.WriteString("\n" + status)
	}
	if status := formatInputQueueStatus(state.InputQueue); status != "" {
		sb.WriteString("\n" + status)
	}
	if state.UnconfirmedInputs > 0 {
		fmt.Fprintf(&sb, "\n⚠️ Unconfirmed inputs: %d. Check the original session on the host before resending or changing directories.", state.UnconfirmedInputs)
	}
	if state.RunningJobs > 0 {
		fmt.Fprintf(&sb, "\n⏳ Background jobs: %d", state.RunningJobs)
	}
	if ws != nil {
		sb.WriteString("\n🗂️ Workspace: ")
		sb.WriteString(ws.ID)
		sb.WriteString(" (")
		sb.WriteString(workspaceDisplayName(ws))
		sb.WriteString(")")
	}
	if imType != "" || chatID != "" {
		sb.WriteString("\n💬 Binding: ")
		sb.WriteString(imType)
		if chatID != "" {
			sb.WriteString("/")
			sb.WriteString(chatID)
		}
	}
	if state.SessionID != "" {
		sb.WriteString("\n🧵 Session: ")
		sb.WriteString(state.SessionID)
	} else {
		sb.WriteString("\n🧵 Session: (none)")
	}
	if state.CurrentRole != "" {
		sb.WriteString("\n🎭 Role: ")
		sb.WriteString(state.CurrentRole)
	}
	if state.Phase != "" {
		sb.WriteString("\n📍 Phase: ")
		sb.WriteString(state.Phase)
		if state.PhaseDetail != "" {
			sb.WriteString(" — ")
			sb.WriteString(state.PhaseDetail)
		}
	}
	if state.PendingConfirm != nil {
		sb.WriteString("\n🔧 Pending confirm: ")
		sb.WriteString(state.PendingConfirm.ToolName)
	}
	if state.pendingQuestionCount() > 0 {
		sb.WriteString(fmt.Sprintf("\n❓ Pending questions: %d", state.pendingQuestionCount()))
		ids := make([]string, 0, len(state.Questions))
		for id, q := range state.Questions {
			if q.Outcome == "" {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		for _, id := range ids {
			sb.WriteString("\n  " + id + ": " + state.Questions[id].Question)
		}
	}
	if state.LastOutcome != "" {
		sb.WriteString("\n📋 Last outcome: ")
		sb.WriteString(state.LastOutcome)
	}
	if state.LastCompaction != nil && state.LastCompaction.Status != "" && state.LastCompaction.Status != "started" {
		sb.WriteString("\n🧠 Last context checkpoint: ")
		sb.WriteString(state.LastCompaction.Status)
		if state.LastCompaction.Trigger == "model_driven" {
			sb.WriteString(" (model-driven)")
		}
		if state.LastCompaction.Reason != "" {
			sb.WriteString(" — ")
			sb.WriteString(state.LastCompaction.Reason)
		}
	}
	if state.LastError != "" {
		sb.WriteString("\n❌ Last error: ")
		sb.WriteString(state.LastError)
	}
	if len(state.Todos) > 0 {
		completed := 0
		for _, t := range state.Todos {
			if t.Status == "completed" {
				completed++
			}
		}
		fmt.Fprintf(&sb, "\n📋 Todos: %d/%d completed", completed, len(state.Todos))
	}
	return appendPausedInputProjection(sb.String(), formatPausedInputs(state.PausedInputs))
}
