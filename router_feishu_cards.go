package main

import (
	"fmt"
	"strings"
	"time"
)

func (r *NotificationRouter) sendFeishuConfirmCard(chatID, key string, state ControlState) bool {
	if state.PendingConfirm == nil {
		return false
	}
	feishu := r.findFeishuAdapter()
	if feishu == nil {
		return false
	}
	workspaceID, _, _ := parseProcessKey(key)
	card := buildFeishuConfirmCard(chatID, state.PendingConfirm, feishuCardContext{WorkspaceID: workspaceID, SessionID: state.SessionID, RequestedAt: state.UpdatedAt, ProcessKey: key})
	handle, err := feishu.sendCardOrFallback(chatID, card, r.formatConfirmNotification(state))
	r.recordCardHandle(key, "confirm", state.PendingConfirm.RequestID, handle)
	return err == nil
}

func (r *NotificationRouter) sendFeishuQuestionCard(chatID, key string, state ControlState) bool {
	if state.PendingQuestion == nil || !shouldSendFeishuQuestionCard(state.PendingQuestion) {
		return false
	}
	feishu := r.findFeishuAdapter()
	if feishu == nil {
		return false
	}
	workspaceID, _, _ := parseProcessKey(key)
	card := buildFeishuQuestionCard(chatID, state.PendingQuestion, feishuCardContext{WorkspaceID: workspaceID, SessionID: state.SessionID, RequestedAt: state.UpdatedAt, ProcessKey: key})
	handle, err := feishu.sendCardOrFallback(chatID, card, r.formatQuestionNotification(state))
	r.recordCardHandle(key, "question", state.PendingQuestion.RequestID, handle)
	return err == nil
}

type feishuCardContext struct {
	WorkspaceID string
	SessionID   string
	RequestedAt string
	ProcessKey  string
}

func shouldSendFeishuQuestionCard(q *QuestionPayload) bool {
	if q == nil || q.Multiple || len(q.Options) == 0 || len(q.Options) > 10 {
		return false
	}
	for i, opt := range q.Options {
		if len([]rune(strings.TrimSpace(opt))) > 24 {
			return false
		}
		if i < len(q.OptionDetails) {
			detail := strings.TrimSpace(q.OptionDetails[i])
			if detail != "" && detail != opt && len([]rune(detail)) > 120 {
				return false
			}
		}
	}
	return true
}

func buildFeishuConfirmCard(chatID string, c *ConfirmPayload, cardCtx feishuCardContext) map[string]any {
	risk := riskLevelForTool(c.ToolName)
	summary := summarizeToolArgs(c.ToolName, c.ArgsJSON)
	elements := []any{map[string]any{"tag": "markdown", "content": fmt.Sprintf("**🔧 %s**\nTool: `%s`", risk.Title, c.ToolName)}}
	var contextLines []string
	if strings.TrimSpace(cardCtx.WorkspaceID) != "" {
		contextLines = append(contextLines, "Workspace: `"+cardCtx.WorkspaceID+"`")
	}
	if strings.TrimSpace(cardCtx.SessionID) != "" {
		contextLines = append(contextLines, "Session: `"+shortID(cardCtx.SessionID)+"`")
	}
	if strings.TrimSpace(c.RequestID) != "" {
		contextLines = append(contextLines, "Request: `"+shortID(c.RequestID)+"`")
	}
	if strings.TrimSpace(cardCtx.RequestedAt) != "" {
		contextLines = append(contextLines, "Requested: `"+cardCtx.RequestedAt+"`")
	}
	if len(contextLines) > 0 {
		elements = append(elements, map[string]any{"tag": "markdown", "content": strings.Join(contextLines, "\n")})
	}
	if summary != "" {
		elements = append(elements, map[string]any{"tag": "markdown", "content": summary})
	}
	if len(c.NeedsApproval) > 0 {
		var sb strings.Builder
		sb.WriteString("**Needs approval**")
		for _, p := range c.NeedsApproval {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			sb.WriteString("\n• ")
			sb.WriteString(p)
		}
		if sb.String() != "**Needs approval**" {
			elements = append(elements, map[string]any{"tag": "markdown", "content": truncate(sb.String())})
		}
	}
	elements = append(elements, map[string]any{"tag": "markdown", "content": "Click a button, or reply `/allow` / `/deny [reason]`."})
	baseValue := map[string]any{"request_id": c.RequestID, "chat_id": chatID, "im_type": "feishu", "workspace_id": cardCtx.WorkspaceID, "session_id": cardCtx.SessionID, "process_key": cardCtx.ProcessKey, "issued_at": time.Now().Unix()}
	allowValue := cloneCardValue(baseValue)
	allowValue["type"] = "confirm"
	allowValue["action"] = "allow"
	denyValue := cloneCardValue(baseValue)
	denyValue["type"] = "confirm"
	denyValue["action"] = "deny"
	actions := []any{
		feishuCardButton("Allow", "primary", allowValue),
		feishuCardButton("Deny", "danger", denyValue),
	}
	elements = append(elements, actions...)
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true},
		"header": map[string]any{"title": map[string]any{"tag": "plain_text", "content": risk.Header}, "template": risk.Template},
		"body":   map[string]any{"elements": elements},
	}
}

func buildFeishuQuestionCard(chatID string, q *QuestionPayload, cardCtx feishuCardContext) map[string]any {
	question := strings.TrimSpace(q.Question)
	if q.Header != "" {
		question = strings.TrimSpace(q.Header) + ": " + question
	}
	elements := []any{map[string]any{"tag": "markdown", "content": fmt.Sprintf("**❓ %s**", question)}}
	var contextLines []string
	if q.Multiple {
		contextLines = append(contextLines, "Multiple answers allowed")
	}
	if strings.TrimSpace(cardCtx.WorkspaceID) != "" {
		contextLines = append(contextLines, "Workspace: `"+cardCtx.WorkspaceID+"`")
	}
	if strings.TrimSpace(cardCtx.SessionID) != "" {
		contextLines = append(contextLines, "Session: `"+shortID(cardCtx.SessionID)+"`")
	}
	if strings.TrimSpace(q.RequestID) != "" {
		contextLines = append(contextLines, "Request: `"+shortID(q.RequestID)+"`")
	}
	if len(contextLines) > 0 {
		elements = append(elements, map[string]any{"tag": "markdown", "content": strings.Join(contextLines, "\n")})
	}
	for i, opt := range q.Options {
		opt = strings.TrimSpace(opt)
		detail := ""
		if i < len(q.OptionDetails) {
			detail = strings.TrimSpace(q.OptionDetails[i])
		}
		switch {
		case detail != "" && detail != opt:
			elements = append(elements, map[string]any{"tag": "markdown", "content": fmt.Sprintf("**%d. %s**\n%s", i+1, opt, detail)})
		case opt != "":
			elements = append(elements, map[string]any{"tag": "markdown", "content": fmt.Sprintf("%d. %s", i+1, opt)})
		}
	}
	if q.DefaultAnswer != "" {
		elements = append(elements, map[string]any{"tag": "markdown", "content": "Default: " + q.DefaultAnswer})
	}
	elements = append(elements, map[string]any{"tag": "markdown", "content": "Click an option, or reply with the option number/text. `/answer 1` also works."})
	actions := make([]any, 0, len(q.Options))
	baseValue := map[string]any{"type": "question", "action": "answer", "request_id": q.RequestID, "chat_id": chatID, "im_type": "feishu", "workspace_id": cardCtx.WorkspaceID, "session_id": cardCtx.SessionID, "process_key": cardCtx.ProcessKey, "issued_at": time.Now().Unix()}
	for _, opt := range q.Options {
		value := cloneCardValue(baseValue)
		value["value"] = opt
		actions = append(actions, feishuCardButton(truncateButtonLabel(opt), "default", value))
	}
	elements = append(elements, actions...)
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true},
		"header": map[string]any{"title": map[string]any{"tag": "plain_text", "content": "Question"}, "template": "blue"},
		"body":   map[string]any{"elements": elements},
	}
}

type feishuRiskLevel struct {
	Header   string
	Title    string
	Template string
}

func riskLevelForTool(tool string) feishuRiskLevel {
	switch strings.TrimSpace(tool) {
	case "Read", "Glob", "Grep", "Lsp":
		return feishuRiskLevel{Header: "Low risk confirmation required", Title: "Low risk · read-only operation", Template: "blue"}
	case "Delete", "Bash", "Spawn":
		return feishuRiskLevel{Header: "High risk confirmation required", Title: "High risk · destructive or command execution", Template: "red"}
	case "Edit", "Write":
		return feishuRiskLevel{Header: "Medium risk confirmation required", Title: "Medium risk · file modification", Template: "orange"}
	case "WebFetch":
		return feishuRiskLevel{Header: "Medium risk confirmation required", Title: "Medium risk · external network access", Template: "orange"}
	default:
		return feishuRiskLevel{Header: "Confirmation required", Title: "Medium risk · confirmation required", Template: "orange"}
	}
}

func shortID(s string) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= 12 {
		return s
	}
	r := []rune(s)
	return string(r[:8]) + "…" + string(r[len(r)-4:])
}

func cloneCardValue(value map[string]any) map[string]any {
	out := make(map[string]any, len(value)+2)
	for k, v := range value {
		if str, ok := v.(string); ok && str == "" {
			continue
		}
		out[k] = v
	}
	return out
}

func truncateButtonLabel(label string) string {
	label = strings.TrimSpace(label)
	r := []rune(label)
	if len(r) <= 24 {
		return label
	}
	return string(r[:23]) + "…"
}

func feishuCardButton(label, style string, value map[string]any) map[string]any {
	return map[string]any{
		"tag":       "button",
		"type":      style,
		"text":      map[string]any{"tag": "plain_text", "content": label},
		"behaviors": []any{map[string]any{"type": "callback", "value": value}},
	}
}
