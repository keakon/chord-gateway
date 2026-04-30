package main

import "strings"

func parseIMCommand(text string) IMCommand {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return IMCommand{Type: "send", Content: text}
	}

	parts := strings.SplitN(text, " ", 3)
	cmd := strings.ToLower(parts[0])

	switch cmd {
	case "/status":
		return IMCommand{Type: "status"}
	case "/summary":
		return IMCommand{Type: "send", Content: "/summary"}
	case "/cancel":
		return IMCommand{Type: "cancel"}
	case "/allow":
		requestID := ""
		if len(parts) > 1 {
			requestID = strings.TrimSpace(parts[1])
		}
		return IMCommand{Type: "confirm", Action: "allow", RequestID: requestID}
	case "/deny":
		reason := ""
		if len(parts) > 1 {
			reason = strings.TrimSpace(text[len(parts[0]):])
		}
		return IMCommand{Type: "confirm", Action: "deny", Reason: reason}
	case "/answer":
		answer := ""
		if len(parts) > 1 {
			answer = strings.Join(parts[1:], " ")
		}
		return IMCommand{Type: "question", Answers: []string{answer}}
	case "/new":
		return IMCommand{Type: "new"}
	case "/resume":
		sessionID := ""
		if len(parts) > 1 {
			sessionID = strings.TrimSpace(parts[1])
		}
		return IMCommand{Type: "resume", SessionID: sessionID}
	case "/sessions":
		return IMCommand{Type: "sessions"}
	case "/current":
		return IMCommand{Type: "current"}
	case "/todos":
		return IMCommand{Type: "todos"}
	case "/login":
		target := ""
		if len(parts) > 1 {
			target = strings.TrimSpace(parts[1])
		}
		return IMCommand{Type: "login", Content: target}
	case "/bind":
		workspaceID, path, ok := parseBindArgs(strings.TrimSpace(strings.TrimPrefix(text, parts[0])))
		return IMCommand{Type: "bind", WorkspaceID: workspaceID, Path: path, Invalid: !ok}
	default:
		return IMCommand{Type: "send", Content: text}
	}
}

func commandFromInternalAction(action *InternalAction) IMCommand {
	if action == nil {
		return IMCommand{Type: "send"}
	}
	switch action.Type {
	case "confirm":
		return IMCommand{Type: "confirm", Action: action.Action, RequestID: action.RequestID}
	case "question":
		return IMCommand{Type: "question", RequestID: action.RequestID, Answers: []string{action.Value}}
	default:
		return IMCommand{Type: "send"}
	}
}

func parseBindArgs(rest string) (workspaceID, path string, ok bool) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", "", true
	}
	workspaceID, remaining, ok := nextCommandArg(rest)
	if !ok {
		return "", "", false
	}
	if strings.TrimSpace(remaining) == "" {
		return workspaceID, "", true
	}
	path, remaining, ok = nextCommandArg(remaining)
	if !ok {
		return "", "", false
	}
	if strings.TrimSpace(remaining) != "" {
		return "", "", false
	}
	return workspaceID, path, true
}

func nextCommandArg(s string) (arg, rest string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	if s[0] != '"' {
		fields := strings.Fields(s)
		if len(fields) == 0 {
			return "", "", false
		}
		idx := strings.Index(s, fields[0]) + len(fields[0])
		return fields[0], strings.TrimSpace(s[idx:]), true
	}

	var b strings.Builder
	escaped := false
	for i := 1; i < len(s); i++ {
		ch := s[i]
		if escaped {
			switch ch {
			case '"', '\\':
				b.WriteByte(ch)
			default:
				b.WriteByte('\\')
				b.WriteByte(ch)
			}
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			return b.String(), strings.TrimSpace(s[i+1:]), true
		}
		b.WriteByte(ch)
	}
	if escaped {
		b.WriteByte('\\')
	}
	return "", "", false
}

// formatStatus formats a ControlState as a human-readable status message.
func formatStatus(state ControlState) string {
	return formatBindingStatus(nil, "", "", state)
}

// truncate shortens a string to maxNotificationRunes runes, appending an ellipsis
// when truncation occurs. Operates on runes so multi-byte UTF-8 sequences
// (e.g. Chinese characters or emoji) are never split.
func truncate(s string) string {
	const ellipsis = "..."
	if len(s) <= maxNotificationRunes {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxNotificationRunes {
		return s
	}
	return string(runes[:maxNotificationRunes-len(ellipsis)]) + ellipsis
}
