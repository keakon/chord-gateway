package main

import (
	"context"
	"strings"
	"time"

	"github.com/keakon/golog/log"
)

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
func (r *NotificationRouter) updateFeishuCardStatus(msg IncomingMessage, processKey, requestType, requestID string, card map[string]any) bool {
	target, ok := r.targetForKey(processKey)
	if !ok {
		return false
	}
	return r.updateFeishuCardForTarget(msg, target, requestType, requestID, card)
}

func (r *NotificationRouter) updateFeishuCardForTarget(msg IncomingMessage, target DeliveryTarget, requestType, requestID string, card map[string]any) bool {
	return r.updateFeishuCardForTargetContext(context.Background(), msg, target, requestType, requestID, card)
}

func (r *NotificationRouter) updateFeishuCardForTargetContext(ctx context.Context, msg IncomingMessage, target DeliveryTarget, requestType, requestID string, card map[string]any) bool {
	updated, _ := r.patchFeishuCardForTargetContext(ctx, msg, target, requestType, requestID, card, true)
	return updated
}

// Preserve handles until a confirmed terminal patch. A failed or uncertain
// patch can be reconciled by a later trusted result instead of stranding the
// original card, and uncertain writes must not trigger the same text fallback.
func (r *NotificationRouter) patchFeishuCardForTargetContext(ctx context.Context, msg IncomingMessage, target DeliveryTarget, requestType, requestID string, card map[string]any, terminal bool) (bool, error) {
	ctx, done, err := r.deliveryContext(ctx, target)
	if err != nil {
		return false, err
	}
	defer done()
	if msg.IMType != "feishu" {
		return false, nil
	}
	feishu := r.findFeishuAdapter()
	if feishu == nil {
		return false, nil
	}
	key := cardHandleKey(target.ProcessKey, requestType, requestID)
	r.mu.Lock()
	entry, ok := r.cardHandles[key]
	if ok && time.Since(entry.recordedAt) > interactiveCardHandleTTL {
		delete(r.cardHandles, key)
		ok = false
	}
	r.mu.Unlock()
	handle := InteractiveCardHandle{}
	if ok {
		handle = entry.handle
	}
	if msg.InternalAction != nil {
		handle = mergeCardHandles(handle, msg.InternalAction.Handle)
	}
	if strings.TrimSpace(handle.MessageID) == "" {
		return false, nil
	}
	if err := feishu.updateInteractiveMessageContext(ctx, handle.MessageID, card, false); err != nil {
		log.Warnf("feishu: failed to update interactive card request_id=%v request_type=%v message_id=%v error=%v", requestID, requestType, handle.MessageID, err)
		return false, err
	}
	if terminal && ok {
		r.mu.Lock()
		if current, exists := r.cardHandles[key]; exists && current.recordedAt == entry.recordedAt {
			delete(r.cardHandles, key)
		}
		r.mu.Unlock()
	}
	return true, nil
}

// resolveFeishuCard patches the previously-sent interactive Feishu card with a
// title/message/template tuple, sparing callers from constructing the card body.
// It reports whether the card was actually patched.
func (r *NotificationRouter) resolveFeishuCard(msg IncomingMessage, processKey, requestType, requestID, title, message, template string) bool {
	return r.updateFeishuCardStatus(msg, processKey, requestType, requestID, buildFeishuResolvedCard(title, message, template))
}
