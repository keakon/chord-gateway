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
	case "notification":
		return r.formatHeadlessNotification(state)

	case "confirm_request":
		return r.formatConfirmNotification(state)

	case "question_request":
		return r.formatQuestionNotification(state)

	case "idle":
		return r.formatIdleNotification(state)

	case "idle_timeout":
		return r.formatExpiredPendingNotification(state)

	case "error":
		return ""

	case "agent_done":
		// assistant_message already delivers the user-visible completion content.
		// Keep agent_done as internal state only; do not push another message.
		return ""

	case "assistant_message":
		// Send the completed assistant message.
		if state.LastAssistantText == "" {
			return ""
		}
		return state.LastAssistantText

	case "info":
		return r.formatInfoNotification(state)

	case "toast":
		return r.formatToastNotification(state)

	case "activity":
		// Too noisy, don't push.
		return ""

	case "exit":
		return r.formatExitNotification(state)

	case "tool_result":
		return r.formatToolResultNotification(state)

	case "todos":
		return r.formatTodosNotification(state)

	default:
		return ""
	}
}

func (r *NotificationRouter) formatExpiredPendingNotification(state ControlState) string {
	if state.ExpiredQuestion != nil {
		return "⌛ The pending question has expired. You can still reply, and I will send it as a follow-up message instead of a structured answer."
	}
	if state.ExpiredConfirm != nil {
		return "⌛ The pending confirmation has expired. It was not approved or denied. Please retry the original request if confirmation is still needed."
	}
	return ""
}

func (r *NotificationRouter) formatIdleNotification(state ControlState) string {
	if msg := r.formatExpiredPendingNotification(state); msg != "" {
		return msg
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
	var sb strings.Builder
	sb.WriteString("🔧 Confirm required: ")
	sb.WriteString(c.ToolName)

	// Show a human-readable summary of the tool args so the user knows
	// what the tool will actually do (e.g. which command Bash will run,
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
	case "Bash", "Spawn":
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

func (r *NotificationRouter) formatToolResultNotification(state ControlState) string {
	if state.LastToolResult == nil {
		return ""
	}
	if state.LastToolResult.Status == "error" {
		return truncate(fmt.Sprintf("⚠️ Tool %s failed", state.LastToolResult.Name))
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
	if state.LastError != "" {
		sb.WriteString("\n❌ Last error: ")
		sb.WriteString(state.LastError)
	}
	if state.LastToolResult != nil {
		sb.WriteString("\n🔧 Last tool: ")
		sb.WriteString(state.LastToolResult.Name)
		sb.WriteString(" (")
		sb.WriteString(state.LastToolResult.Status)
		sb.WriteString(")")
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
