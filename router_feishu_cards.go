package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/keakon/golog/log"
)

func (r *NotificationRouter) sendFeishuConfirmCard(target DeliveryTarget, state ControlState) (bool, error) {
	key, chatID := target.ProcessKey, target.Identity.ChatID
	if state.PendingConfirm == nil {
		return false, nil
	}
	feishu := r.findFeishuAdapter()
	if feishu == nil {
		return false, nil
	}
	workspaceID, _, _ := parseProcessKey(key)
	card := buildFeishuConfirmCard(chatID, state.PendingConfirm, feishuCardContext{WorkspaceID: workspaceID, SessionID: state.SessionID, RequestedAt: state.UpdatedAt, ProcessKey: key, BindingInstanceID: state.BindingInstanceID})
	handle, err := r.sendCardToTarget(target, card, r.formatConfirmNotification(state))
	r.recordCardHandle(key, "confirm", state.PendingConfirm.RequestID, handle)
	return true, err
}

func (r *NotificationRouter) sendFeishuQuestionCard(target DeliveryTarget, state ControlState) (bool, error) {
	key, chatID := target.ProcessKey, target.Identity.ChatID
	if state.questionForNotification() == nil || !shouldSendFeishuQuestionCard(state.questionForNotification()) {
		return false, nil
	}
	feishu := r.findFeishuAdapter()
	if feishu == nil {
		return false, nil
	}
	workspaceID, _, _ := parseProcessKey(key)
	card := buildFeishuQuestionCard(chatID, state.questionForNotification(), feishuCardContext{WorkspaceID: workspaceID, SessionID: state.SessionID, RequestedAt: state.UpdatedAt, ProcessKey: key, BindingInstanceID: state.BindingInstanceID})
	handle, err := r.sendCardToTarget(target, card, r.formatQuestionNotification(state))
	r.recordCardHandle(key, "question", state.questionForNotification().RequestID, handle)
	return true, err
}

// Only a diagnostic may follow an uncertain card send. The original prompt
// must not be repeated as text by this helper or its caller.
func (r *NotificationRouter) reportFeishuCardFailure(target DeliveryTarget, err error) {
	log.Warnf("feishu card delivery failed error=%v", err)
	if errors.Is(err, errDeliveryOutcomeUnknown) {
		feishu := r.findFeishuAdapter()
		if feishu != nil {
			if err := r.SendBound(context.Background(), target, "Message delivery is unconfirmed; check the chat first. If the question entry is missing, send /status to view it; send /role to view the role menu. The prompt is not resent as text automatically."); err != nil {
				log.Warnf("feishu card delivery notice failed error=%v", err)
			} else {
				r.markVisibleOutput(target.ProcessKey)
			}
		}
	}
}

// sendFeishuRoleMenu presents the role list as an interactive Feishu card.
// On card-send failure sendCardToTarget emits the text menu fallback and
// reports a nil handle, so the boolean only reports an interactive card.
func (r *NotificationRouter) sendFeishuRoleMenu(chatID, procKey, current string, roles []RoleInfo, instance ...string) bool {
	feishu := r.findFeishuAdapter()
	if feishu == nil {
		return false
	}
	reqID := newRoleMenuRequestID()
	card := buildFeishuRoleCard(chatID, reqID, current, roles)
	if p := r.mgr.GetProcessForKey(procKey); p != nil {
		binding := p.State().BindingInstanceID
		if len(instance) > 0 {
			if binding != instance[0] {
				return false
			}
			binding = instance[0]
		}
		stampCardBinding(card, binding)
	}
	target, ok := r.targetForKey(procKey)
	if !ok || len(instance) > 0 && target.RuntimeInstanceID != instance[0] {
		return false
	}
	handle, err := r.sendCardToTarget(target, card, buildRoleMenuText(current, roles))
	if err != nil {
		r.reportFeishuCardFailure(target, err)
	}
	if err == nil && handle != nil {
		r.recordCardHandle(procKey, "role", reqID, handle)
	}
	return err == nil && handle != nil
}

// newRoleMenuRequestID synthesizes a request id for a role menu card. Role
// switches carry no chord request id, so the id only correlates the menu card
// with its own button clicks for card-status updates.
func newRoleMenuRequestID() string {
	return fmt.Sprintf("role-%d", time.Now().UnixNano())
}

func buildFeishuRoleCard(chatID, reqID, current string, roles []RoleInfo) map[string]any {
	var lines []string
	if strings.TrimSpace(current) != "" {
		lines = append(lines, "Current role: "+strings.TrimSpace(current))
	}
	for i, r := range roles {
		name := strings.TrimSpace(r.Name)
		if name == "" {
			continue
		}
		line := fmt.Sprintf("%d. %s", i+1, name)
		if name == current {
			line += " (current)"
		}
		lines = append(lines, line)
	}
	elements := []any{map[string]any{"tag": "markdown", "content": "**🎭 Switch role**\n" + strings.Join(lines, "\n")}}
	baseValue := map[string]any{
		"type": "role", "action": "switch", "request_id": reqID,
		"chat_id": chatID, "im_type": "feishu", "issued_at": time.Now().Unix(),
	}
	for _, r := range roles {
		name := strings.TrimSpace(r.Name)
		if name == "" || name == current {
			continue
		}
		value := cloneCardValue(baseValue)
		value["value"] = name
		elements = append(elements, feishuCardButton(truncateButtonLabel(name), "default", value))
	}
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true},
		"header": map[string]any{"title": map[string]any{"tag": "plain_text", "content": "Switch role"}, "template": "blue"},
		"body":   map[string]any{"elements": elements},
	}
}

type feishuCardContext struct {
	WorkspaceID       string
	SessionID         string
	RequestedAt       string
	BindingInstanceID string
	ProcessKey        string
}

// maxFeishuQuestionOptions caps how many options a question card carries.
// Beyond that the chat text form is easier to answer and to read.
const maxFeishuQuestionOptions = 10

// shouldSendFeishuQuestionCard reports whether a question becomes an
// interactive card. Only free-answer questions and questions with too many
// options fall back to chat text: option length does not decide the form
// anymore, because a long label switches the card to numbered buttons instead
// of dropping the interaction.
func shouldSendFeishuQuestionCard(q *QuestionPayload) bool {
	return q != nil && len(q.Options) > 0 && len(q.Options) <= maxFeishuQuestionOptions
}

func buildFeishuConfirmCard(chatID string, c *ConfirmPayload, cardCtx feishuCardContext) map[string]any {
	if isDoneTool(c.ToolName) {
		return buildFeishuDoneConfirmCard(chatID, c, cardCtx)
	}
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
	baseValue := map[string]any{"request_id": c.RequestID, "chat_id": chatID, "im_type": "feishu", "workspace_id": cardCtx.WorkspaceID, "session_id": cardCtx.SessionID, "process_key": cardCtx.ProcessKey, "binding_instance_id": cardCtx.BindingInstanceID, "issued_at": time.Now().Unix()}
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

func buildFeishuDoneConfirmCard(chatID string, c *ConfirmPayload, cardCtx feishuCardContext) map[string]any {
	report, reason := doneConfirmReportReason(c)
	elements := []any{map[string]any{"tag": "markdown", "content": "**✅ Done requests completion**"}}
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
	if reason != "" {
		elements = append(elements, map[string]any{"tag": "markdown", "content": "**Reason**\n" + reason})
	}
	if report != "" {
		elements = append(elements, map[string]any{"tag": "markdown", "content": "**Completion report**\n" + report})
	}
	elements = append(elements, map[string]any{"tag": "markdown", "content": "Click Finish to end the loop, or reply `/deny <reason>` to continue. Done rejection requires a reason."})
	baseValue := map[string]any{"request_id": c.RequestID, "chat_id": chatID, "im_type": "feishu", "workspace_id": cardCtx.WorkspaceID, "session_id": cardCtx.SessionID, "process_key": cardCtx.ProcessKey, "binding_instance_id": cardCtx.BindingInstanceID, "issued_at": time.Now().Unix()}
	allowValue := cloneCardValue(baseValue)
	allowValue["type"] = "confirm"
	allowValue["action"] = "allow"
	elements = append(elements, feishuCardButton("Finish", "primary", allowValue))
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true},
		"header": map[string]any{"title": map[string]any{"tag": "plain_text", "content": "Done completion confirmation"}, "template": "green"},
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
	baseValue := map[string]any{"type": "question", "action": "answer", "request_id": q.RequestID, "chat_id": chatID, "im_type": "feishu", "workspace_id": cardCtx.WorkspaceID, "session_id": cardCtx.SessionID, "process_key": cardCtx.ProcessKey, "binding_instance_id": cardCtx.BindingInstanceID, "issued_at": time.Now().Unix()}
	if q.Multiple {
		elements = append(elements, buildFeishuQuestionForm(q, baseValue))
		elements = append(elements, map[string]any{"tag": "markdown", "content": "Check one or more options, then click Submit. You can also reply /answer @" + q.RequestID + " 1,3."})
	} else {
		for i, opt := range q.Options {
			text := feishuQuestionOptionText(i+1, opt, questionOptionDetail(q, i))
			if text != "" {
				elements = append(elements, map[string]any{"tag": "markdown", "content": text})
			}
		}
		elements = append(elements, map[string]any{"tag": "markdown", "content": "Click an option, or reply /answer @" + q.RequestID + " 1."})
		elements = append(elements, feishuQuestionOptionButtons(q, baseValue)...)
	}
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true},
		"header": map[string]any{"title": map[string]any{"tag": "plain_text", "content": "Question"}, "template": "blue"},
		"body":   map[string]any{"elements": elements},
	}
}

// feishuQuestionOptionButtons renders one button per option. The button value
// carries the stable option ID; when any label exceeds the button budget the
// whole card switches to numbered buttons so the numbers match the option list
// in the card body and the /answer numbers.
func feishuQuestionOptionButtons(q *QuestionPayload, baseValue map[string]any) []any {
	numbered := feishuQuestionLabelsTooLong(q.Options)
	actions := make([]any, 0, len(q.Options))
	for i, opt := range q.Options {
		if i >= len(q.OptionIDs) {
			continue
		}
		opt = strings.TrimSpace(opt)
		value := cloneCardValue(baseValue)
		value["value"] = q.OptionIDs[i]
		label := opt
		if numbered {
			label = strconv.Itoa(i + 1)
		}
		actions = append(actions, feishuCardButton(label, "default", value))
	}
	return actions
}

// buildFeishuQuestionForm renders the multi-select form: one checker per option
// plus a submit button. Submitting the form is the only card callback that
// carries several answers at once; the checked option numbers are mapped back
// to labels in the router, where the question payload is available.
func buildFeishuQuestionForm(q *QuestionPayload, baseValue map[string]any) map[string]any {
	formElements := make([]any, 0, len(q.Options)+1)
	for i, opt := range q.Options {
		text := feishuQuestionOptionText(i+1, opt, questionOptionDetail(q, i))
		if text == "" {
			continue
		}
		formElements = append(formElements, map[string]any{
			"tag":     "checker",
			"name":    feishuQuestionOptionName(i + 1),
			"checked": false,
			"text":    map[string]any{"tag": "lark_md", "content": text},
		})
	}
	submit := feishuCardButton("Submit", "primary", baseValue)
	submit["name"] = feishuQuestionSubmitName
	submit["form_action_type"] = "submit"
	formElements = append(formElements, submit)
	return map[string]any{
		"tag":      "form",
		"name":     feishuQuestionFormName,
		"elements": formElements,
	}
}

// feishuQuestionLabelsTooLong reports whether any option label exceeds the
// button label budget, which is when the card uses numbered buttons.
func feishuQuestionLabelsTooLong(options []string) bool {
	for _, opt := range options {
		if len([]rune(strings.TrimSpace(opt))) > maxFeishuButtonLabelRunes {
			return true
		}
	}
	return false
}

// feishuQuestionOptionText renders one option as markdown. The option number
// keeps the card aligned with the chat text form and with /answer numbers, and
// a detail that adds information goes on its own line.
func feishuQuestionOptionText(number int, option, detail string) string {
	option = strings.TrimSpace(option)
	detail = strings.TrimSpace(detail)
	switch {
	case option == "":
		return ""
	case detail != "" && detail != option:
		return fmt.Sprintf("**%d. %s**\n%s", number, option, detail)
	default:
		return fmt.Sprintf("%d. %s", number, option)
	}
}

// questionOptionDetail returns the detail for an option index, or "" when the
// payload carries none.
func questionOptionDetail(q *QuestionPayload, index int) string {
	if q == nil || index < 0 || index >= len(q.OptionDetails) {
		return ""
	}
	return q.OptionDetails[index]
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
	case "Delete", "Shell", "Spawn":
		return feishuRiskLevel{Header: "High risk confirmation required", Title: "High risk · destructive or command execution", Template: "red"}
	case "Edit", "Write":
		return feishuRiskLevel{Header: "Medium risk confirmation required", Title: "Medium risk · file modification", Template: "orange"}
	case "WebFetch":
		return feishuRiskLevel{Header: "Medium risk confirmation required", Title: "Medium risk · external network access", Template: "orange"}
	default:
		return feishuRiskLevel{Header: "Confirmation required", Title: "Medium risk · confirmation required", Template: "orange"}
	}
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

func feishuCardButton(label, style string, value map[string]any) map[string]any {
	return map[string]any{
		"tag":       "button",
		"type":      style,
		"text":      map[string]any{"tag": "plain_text", "content": label},
		"behaviors": []any{map[string]any{"type": "callback", "value": value}},
	}
}
