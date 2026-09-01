package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/keakon/chord-gateway/config"
)

func (r *NotificationRouter) formatNotification(eventType string, state ControlState) string {
	switch eventType {
	case "notification", "done_completion":
		return r.formatHeadlessNotification(state)

	case "confirm_request":
		return r.formatConfirmNotification(state)

	case "question_request":
		return r.formatQuestionNotification(state)

	case "handoff_request":
		return r.formatHandoffNotification(state)

	case "idle":
		return r.formatIdleNotification(state)

	case "idle_timeout":
		return r.formatExpiredPendingNotification(state)

	case "error":
		return ""

	case "agent_started":
		return r.formatAgentStartedNotification(state)

	case "agent_notify":
		return r.formatAgentNotifyNotification(state)

	case "agent_done":
		return r.formatAgentDoneNotification(state)

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
	if kind != "" {
		label += " · " + kind
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
	if state.ExpiredQuestion != nil {
		return "⌛ The pending question has expired. You can still reply, and I will send it as a follow-up message instead of a structured answer."
	}
	if state.ExpiredConfirm != nil {
		return "⌛ The pending confirmation has expired. It was not approved or denied. Please retry the original request if confirmation is still needed."
	}
	if state.ExpiredHandoff != nil {
		return "⌛ The pending handoff request has expired. It was not accepted or denied. Please retry the original request if handoff is still needed."
	}
	return ""
}

func (r *NotificationRouter) formatIdleNotification(state ControlState) string {
	if msg := r.formatExpiredPendingNotification(state); msg != "" {
		return msg
	}
	if state.SuppressUserNotification {
		return ""
	}
	return "✅ Chord: Ready for input"
}

func (r *NotificationRouter) formatHeadlessNotification(state ControlState) string {
	if state.LastNotification == nil {
		return ""
	}
	msg := strings.TrimSpace(state.LastNotification.Message)
	if msg == "" {
		return ""
	}
	switch state.LastNotification.Reason {
	case "confirm_request":
		return truncate("🔧 " + msg)
	case "question_request":
		return truncate("❓ " + msg)
	case "error", "cancelled":
		return truncate("⚠️ " + msg)
	case "idle":
		return truncate("✅ " + msg)
	default:
		return truncate(msg)
	}
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
	if state.PendingQuestion == nil {
		return ""
	}
	q := state.PendingQuestion
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
	if q.DefaultAnswer != "" {
		sb.WriteString("\nDefault: ")
		sb.WriteString(q.DefaultAnswer)
	}
	if q.Multiple {
		sb.WriteString(" (multi-select)")
	}
	sb.WriteString("\nReply /answer 1 / 1,2 / or type your answer")
	return truncate(sb.String())
}

// resolveQuestionAnswers interprets the /answer input against the question's
// options. If the input is purely comma-separated numeric indices within range
// (e.g. "1" or "1,3"), they are mapped to option labels. Otherwise the entire
// input is returned as a single custom-text answer for the model. Single-select
// questions with multiple indices also fall back to custom text.
func resolveQuestionAnswers(input string, q *QuestionPayload) []string {
	if q == nil || len(q.Options) == 0 {
		return []string{input}
	}
	parts := strings.Split(input, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > len(q.Options) {
			// Non-numeric or out of range: treat entire input as custom text.
			return []string{input}
		}
		result = append(result, q.Options[n-1])
	}
	// Single-select with multiple indices is ambiguous; treat as custom text.
	if !q.Multiple && len(result) > 1 {
		return []string{input}
	}
	return result
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

func (r *NotificationRouter) formatLongRunningNotification(state ControlState) string {
	if state.PendingConfirm != nil || state.PendingQuestion != nil {
		return ""
	}
	msg := "⏳ Still working"
	if state.InternalEventsSinceLastPush > 0 {
		msg += fmt.Sprintf(" (%d internal events)", state.InternalEventsSinceLastPush)
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
	if state.Busy {
		sb.WriteString("🔄 Busy")
	} else {
		sb.WriteString("⏸️ Idle")
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
	if state.PendingQuestion != nil {
		sb.WriteString("\n❓ Pending question: ")
		sb.WriteString(state.PendingQuestion.Question)
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
	return truncate(sb.String())
}
