package main

import (
	"strings"

	"github.com/keakon/golog/log"
)

// buildExpiredQuestionFollowup formats a follow-up message for the chord
// process when a user replies to an already-expired question. The text makes
// it explicit that the response should not be parsed as a structured answer.
func buildExpiredQuestionFollowup(answers []string, q *QuestionPayload) string {
	answer := strings.TrimSpace(strings.Join(answers, " "))
	var sb strings.Builder
	sb.WriteString("The previous pending question has expired, so this cannot be submitted as a structured answer. Treat the user's response below as a follow-up message and continue from the existing context if applicable.")
	if q != nil && strings.TrimSpace(q.Question) != "" {
		sb.WriteString("\n\nExpired question:\n")
		sb.WriteString(strings.TrimSpace(q.Question))
		if len(q.Options) > 0 {
			sb.WriteString("\nOptions: ")
			sb.WriteString(strings.Join(q.Options, ", "))
		}
	}
	if answer != "" {
		sb.WriteString("\n\nUser response:\n")
		sb.WriteString(answer)
	}
	return sb.String()
}

// buildExpiredConfirmFollowup formats a follow-up message for the chord
// process when a user replies to an already-expired confirmation request.
func buildExpiredConfirmFollowup(cmd IMCommand, c *ConfirmPayload) string {
	var sb strings.Builder
	sb.WriteString("The previous pending confirmation has expired, so this must not be treated as an approval or denial. Treat this only as follow-up context and ask the user to retry the original request if confirmation is still required.")
	if c != nil {
		if strings.TrimSpace(c.ToolName) != "" {
			sb.WriteString("\n\nExpired confirmation tool: ")
			sb.WriteString(strings.TrimSpace(c.ToolName))
		}
		if strings.TrimSpace(c.ArgsJSON) != "" {
			sb.WriteString("\nExpired confirmation arguments: ")
			sb.WriteString(strings.TrimSpace(c.ArgsJSON))
		}
	}
	if strings.TrimSpace(cmd.Action) != "" {
		sb.WriteString("\n\nExpired confirmation response: ")
		sb.WriteString(strings.TrimSpace(cmd.Action))
	}
	if strings.TrimSpace(cmd.Reason) != "" {
		sb.WriteString("\nReason: ")
		sb.WriteString(strings.TrimSpace(cmd.Reason))
	}
	return sb.String()
}

// buildFeishuResolvedCard builds a minimal Feishu card body used to replace an
// already-sent interactive card after it has been resolved (approved/denied/
// answered/expired).
func buildFeishuResolvedCard(title, message, template string) map[string]any {
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true},
		"header": map[string]any{"title": map[string]any{"tag": "plain_text", "content": title}, "template": template},
		"body": map[string]any{"elements": []any{
			map[string]any{"tag": "markdown", "content": message},
		}},
	}
}

// displaySender returns a short, human-readable name for the sender of an
// inbound IM message. Falls back to a truncated id, then the literal "user".
func displaySender(msg IncomingMessage) string {
	if strings.TrimSpace(msg.SenderName) != "" {
		return strings.TrimSpace(msg.SenderName)
	}
	if strings.TrimSpace(msg.SenderID) != "" {
		return shortID(msg.SenderID)
	}
	return "user"
}

// updateFeishuCardStatus resolves a previously-sent interactive Feishu card
// (referenced by processKey/requestType/requestID) by patching it with the
// supplied card body. No-op for non-Feishu IMs or when no handle is available.
func (r *NotificationRouter) updateFeishuCardStatus(msg IncomingMessage, processKey, requestType, requestID string, card map[string]any) {
	if msg.IMType != "feishu" {
		return
	}
	feishu := r.findFeishuAdapter()
	if feishu == nil {
		return
	}
	stored, ok := r.takeCardHandle(processKey, requestType, requestID)
	handle := stored
	if msg.InternalAction != nil {
		handle = mergeCardHandles(stored, msg.InternalAction.Handle)
	}
	if !ok && (strings.TrimSpace(handle.MessageID) == "" && strings.TrimSpace(handle.Token) == "") {
		return
	}
	if err := feishu.UpdateInteractiveCard(handle, card); err != nil {
		log.Warnf("feishu: failed to update interactive card request_id=%v request_type=%v message_id=%v error=%v", requestID, requestType, handle.MessageID, err)
	}
}

// resolveFeishuCard patches the previously-sent interactive Feishu card with a
// title/message/template tuple, sparing callers from constructing the card body.
func (r *NotificationRouter) resolveFeishuCard(msg IncomingMessage, processKey, requestType, requestID, title, message, template string) {
	r.updateFeishuCardStatus(msg, processKey, requestType, requestID, buildFeishuResolvedCard(title, message, template))
}
