package gateway

import (
	"strings"

	"github.com/keakon/chord-gateway/internal/im"
)

// commandForIncomingMessage keeps text and authenticated platform actions on
// the same command model. Admission still decides whether the caller is allowed.
func commandForIncomingMessage(msg im.IncomingMessage) im.IMCommand {
	if msg.ForceText {
		return im.IMCommand{Type: "send", Content: msg.Text}
	}
	if msg.InternalAction != nil {
		return commandFromInternalAction(msg.InternalAction)
	}
	return parseIMCommand(msg.Text)
}

func parseIMCommand(text string) im.IMCommand {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "!") {
		return im.IMCommand{Type: "local_shell", Content: strings.TrimSpace(strings.TrimPrefix(text, "!"))}
	}
	if strings.HasPrefix(text, "！") {
		return im.IMCommand{Type: "local_shell", Content: strings.TrimSpace(strings.TrimPrefix(text, "！"))}
	}
	if !strings.HasPrefix(text, "/") {
		return im.IMCommand{Type: "send", Content: text}
	}

	parts := strings.SplitN(text, " ", 3)
	cmd := strings.ToLower(parts[0])
	rest := strings.TrimSpace(text[len(parts[0]):])

	switch cmd {
	case "/takeover":
		return parseTakeoverCommand(rest)
	case "/status":
		return im.IMCommand{Type: "status"}
	case "/summary":
		return im.IMCommand{Type: "send", Content: "/summary"}
	case "/cancel":
		return im.IMCommand{Type: "cancel"}
	case "/allow":
		// Accepts "/allow", "/allow <request-id>", and any combination with a
		// persistence flag: "/allow --session", "/allow req-3 --project".
		scope, requestID, invalid := parseAllowArgs(rest)
		return im.IMCommand{Type: "confirm", Action: "allow", RequestID: requestID, RuleScope: scope, Invalid: invalid}
	case "/deny":
		reason := ""
		if len(parts) > 1 {
			reason = strings.TrimSpace(text[len(parts[0]):])
		}
		return im.IMCommand{Type: "confirm", Action: "deny", Reason: reason}
	case "/answer":
		id := ""
		if strings.HasPrefix(rest, "@") {
			id, rest, _ = strings.Cut(rest, " ")
			id = strings.TrimPrefix(id, "@")
		}
		return im.IMCommand{Type: "question", RequestID: id, Answers: []string{rest}}
	case "/question-interact", "/question-withdraw":
		action := map[string]string{"/question-interact": "interact", "/question-withdraw": "withdraw_requirement"}[cmd]
		id, text, _ := strings.Cut(rest, " ")
		if text == "" && action == "withdraw_requirement" {
			text = "Withdraw the requirement for question " + id
		}
		return im.IMCommand{Type: "question", RequestID: strings.TrimPrefix(id, "@"), Action: action, Content: text}
	case "/question-replace":
		pieces := strings.Fields(rest)
		if len(pieces) < 2 {
			return im.IMCommand{Type: "question", Invalid: true}
		}
		return im.IMCommand{Type: "question", RequestID: strings.TrimPrefix(pieces[0], "@"), Action: "replace_requirement", Reason: strings.TrimPrefix(pieces[1], "@"), Content: "Replace the requirement for question " + pieces[0] + " with " + pieces[1]}

	case "/handoff":
		agent, pool, ok := parseHandoffArgs(strings.TrimSpace(strings.TrimPrefix(text, parts[0])))
		return im.IMCommand{Type: "handoff", Action: "accept", Agent: agent, Pool: pool, Invalid: !ok}
	case "/handoff-deny":
		reason := ""
		if len(parts) > 1 {
			reason = strings.TrimSpace(text[len(parts[0]):])
		}
		return im.IMCommand{Type: "handoff", Action: "deny", Reason: reason}
	case "/handoff-cancel", "/handoff_cancel":
		// Withdraws the request without a rejection reason. Chord distinguishes
		// cancel from deny, and forcing a fabricated reason through /handoff-deny
		// would misreport why the handoff ended.
		return im.IMCommand{Type: "handoff", Action: "cancel"}
	case "/image":
		// Re-requests a generated image Chord still holds, by artifact reference.
		return im.IMCommand{Type: "image", Content: strings.TrimSpace(rest)}
	case "/models":
		// Model pool control is a Chord protocol command the gateway can answer;
		// the TUI /models selector is a different, local affordance.
		return im.IMCommand{Type: "models", Content: strings.TrimSpace(rest)}
	case "/queue":
		// Chord's /queue only pauses, resumes or clears from the TUI. The
		// gateway answers the whole family through the input_queue protocol
		// command, which also lists, edits and deletes single items, so the raw
		// tail is parsed by the handler that knows which action is supported.
		return im.IMCommand{Type: "queue", Content: rest}
	case "/role":
		content := ""
		if len(parts) > 1 {
			content = strings.TrimSpace(strings.Join(parts[1:], " "))
		}
		return im.IMCommand{Type: "role", Content: content}
	case "/new":
		return im.IMCommand{Type: "new"}
	case "/resume":
		sessionID := ""
		if len(parts) > 1 {
			sessionID = strings.TrimSpace(parts[1])
		}
		return im.IMCommand{Type: "resume", SessionID: sessionID}
	case "/sessions":
		return im.IMCommand{Type: "sessions"}
	case "/todos":
		return im.IMCommand{Type: "todos"}
	case "/login":
		target := ""
		if len(parts) > 1 {
			target = strings.TrimSpace(parts[1])
		}
		return im.IMCommand{Type: "login", Content: target}
	case "/bind":
		return im.IMCommand{Type: "bind"}
	default:
		return im.IMCommand{Type: "send", Content: text}
	}
}

// confirmRuleScopes maps the flags an IM user can append to /allow onto the
// rule_scope Chord accepts. The long flag is documented; the short forms stay
// available because typing "--project" on a phone keypad is unpleasant.
var confirmRuleScopes = map[string]string{
	"--session": "session",
	"--project": "project",
	"--user":    "user_global",
	"-s":        "session",
	"-p":        "project",
	"-u":        "user_global",
}

// parseAllowArgs splits the tail of "/allow" into a rule scope and a request id.
// Two unconsumed positional fields are a syntax error rather than a silent merge,
// because a mistyped scope would otherwise be remembered as a wrong request.
func parseAllowArgs(rest string) (scope, requestID string, invalid bool) {
	fields := strings.Fields(rest)
	once := false
	for _, field := range fields {
		switch {
		case field == "once":
			// Explicit and optional: bare /allow already approves once.
			if scope != "" {
				return "", "", true
			}
			once = true
			continue
		case confirmRuleScopes[field] != "":
			if scope != "" || once {
				return "", "", true
			}
			scope = confirmRuleScopes[field]
			continue
		case strings.HasPrefix(field, "-"):
			return "", "", true
		default:
			if requestID != "" {
				return "", "", true
			}
			requestID = field
		}
	}
	return scope, strings.TrimPrefix(requestID, "@"), false
}

func commandFromInternalAction(action *im.InternalAction) im.IMCommand {
	if action == nil {
		return im.IMCommand{Type: "send"}
	}
	switch action.Type {
	case "confirm":
		return im.IMCommand{Type: "confirm", Action: action.Action, RequestID: action.RequestID}
	case "question":
		if action.QuestionFormSubmit() {
			return im.IMCommand{Type: "question", RequestID: action.RequestID, Answers: action.Values}
		}
		return im.IMCommand{Type: "question", RequestID: action.RequestID, Answers: []string{action.Value}}
	case "role":
		return im.IMCommand{Type: "role", Content: action.Value}
	default:
		return im.IMCommand{Type: "send"}
	}
}

func parseHandoffArgs(rest string) (agentName, pool string, ok bool) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", "", true
	}
	agentName, remaining, ok := nextCommandArg(rest)
	if !ok {
		return "", "", false
	}
	if strings.TrimSpace(remaining) == "" {
		return agentName, "", true
	}
	pool, remaining, ok = nextCommandArg(remaining)
	if !ok {
		return "", "", false
	}
	if strings.TrimSpace(remaining) != "" {
		return "", "", false
	}
	return agentName, pool, true
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
