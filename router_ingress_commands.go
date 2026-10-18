package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (r *NotificationRouter) ingressStatus(msg IncomingMessage) string {
	if r.ingressStore == nil {
		return ""
	}
	records, err := r.ingressStore.ListRouteRecords(ChatIdentity{msg.IMType, msg.AccountID, msg.ChatID})
	if err != nil {
		return "\nIngress: storage unavailable."
	}
	counts := map[string]int{}
	var oldest time.Time
	reason := ""
	var lastReasonAt time.Time
	for _, rec := range records {
		counts[string(rec.State)]++
		if rec.State == ingressReceived && (oldest.IsZero() || rec.ReceivedAt.Before(oldest)) {
			oldest = rec.ReceivedAt
		}
		if rec.Reason != "" && (lastReasonAt.IsZero() || rec.UpdatedAt.After(lastReasonAt)) {
			lastReasonAt = rec.UpdatedAt
			reason = rec.Reason
		}
	}
	text := fmt.Sprintf("\nIngress: %d received, %d dispatching, %d unknown, %d quarantined.", counts["received"], counts["dispatching"], counts["unknown"], counts["quarantined"])
	if !oldest.IsZero() {
		text += fmt.Sprintf(" Oldest wait: %s.", time.Since(oldest).Round(time.Second))
		readiness := checkIngressDeliveryReadiness(r.findAdapterByType(msg.IMType), ChatIdentity{msg.IMType, msg.AccountID, msg.ChatID})
		if !readiness.Ready {
			text += " Waiting: " + readiness.Reason + "."
		}
	}
	if reason != "" {
		text += " Last diagnostic: " + reason
	}
	if provider, ok := r.findAdapterByType(msg.IMType).(IMTextMetricsProvider); ok {
		last := provider.TextSendTargetSnapshot(msg.ChatID)
		if !last.LastFailureAt.IsZero() {
			text += fmt.Sprintf("\nLast outbound failure: %s after %d accepted segments.", last.LastFailureCategory, last.LastAcceptedSegments)
		}
	}
	return text
}
func (r *NotificationRouter) dispatchIngressSlash(ctx context.Context, msg IncomingMessage, p *ChordProcess, cmd IMCommand, target *IngressControlTarget, id string) DispatchResult {
	switch strings.ToLower(strings.TrimSpace(cmd.Content)) {
	case "/model", "/export":
		return DispatchResult{"rejected", "command_requires_local_tui"}
	}
	if action, ok := remoteMCPCommand(cmd.Content); ok {
		text := "Manage MCP authentication in this session's working directory on the host: chord mcp login <server> or chord mcp logout <server>. Reconnect in the local TUI. These commands are not forwarded to the model."
		if action == "status" {
			text = formatMCPStatus(p.State().MCP)
			if text == "" {
				text = "No MCP status is available. Use " + r.ingressCommandHint(msg, "/status") + " to synchronize with the host."
			}
		}
		r.enqueueIngressReply(msg, text)
		return DispatchResult{"completed", ""}
	}
	p.mu.RLock()
	valid := p.ingressTargetMatchesLocked(target, cmd)
	p.mu.RUnlock()
	if !valid {
		return DispatchResult{"rejected", "control_target_expired"}
	}
	identity := p.InputReceiptIdentity()
	if identity.RuntimeID != target.RuntimeID || identity.SessionEpoch != target.SessionEpoch {
		return DispatchResult{"rejected", "control_target_expired"}
	}
	request := "im-" + id
	err := r.ingressStore.UpdateRecord(ctx, id, []ingressState{ingressReceived}, func(rec *IngressRecord) error {
		rec.State = ingressDispatching
		rec.RequestID = request
		rec.SessionID = identity.SessionID
		rec.DispatchRuntimeID = identity.RuntimeID
		rec.DispatchSessionEpoch = identity.SessionEpoch
		return nil
	})
	if err != nil {
		return DispatchResult{"unknown", "storage_unavailable"}
	}
	err = p.ObserveInputReceipt(request, func(receipt ProcessInputReceipt) error {
		if receipt.RuntimeID != identity.RuntimeID || receipt.SessionID != identity.SessionID || receipt.SessionEpoch != identity.SessionEpoch {
			return errIngressRouteChanged
		}
		state := ingressUnknown
		switch receipt.Status {
		case "handled", "queued", "started":
			state = "submitted"
		case "rejected":
			state = "rejected"
		}
		return r.ingressStore.UpdateRecord(context.Background(), id, []ingressState{ingressDispatching, ingressUnknown}, func(rec *IngressRecord) error { rec.State = state; rec.ReceiptStatus = receipt.Status; return nil })
	}, identity)
	if err != nil {
		return DispatchResult{"unknown", "observer_unavailable"}
	}
	if err = p.SendUserMessageWithRequestID(cmd.Content, request); err != nil {
		return DispatchResult{"unknown", "stdin_outcome_unknown"}
	}
	return DispatchResult{"awaiting_receipt", ""}
}
func (r *NotificationRouter) dispatchIngressRole(ctx context.Context, msg IncomingMessage, p *ChordProcess, cmd IMCommand, target *IngressControlTarget) DispatchResult {
	wait, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	p.roleMu.Lock()
	defer p.roleMu.Unlock()
	list, err := p.waitIngressRole(wait, target, map[string]any{"type": "role", "action": "list"})
	if err != nil {
		r.enqueueIngressInteractionResult(msg, "role", interactionRequestID(msg), "Role unavailable", "❌ Failed to load available roles.", "grey", target.BindingInstanceID)
		return DispatchResult{"rejected", "role_query_unconfirmed"}
	}
	if !list.OK {
		r.enqueueIngressInteractionResult(msg, "role", interactionRequestID(msg), "Role unavailable", "⚠️ "+roleResponseMessage(list.Message, "Failed to load available roles."), "grey", target.BindingInstanceID)
		return DispatchResult{"rejected", "role_query_rejected"}
	}
	current, roles := roleListState(list)
	name := strings.TrimSpace(cmd.Content)
	if name == "" {
		r.enqueueIngressRoleMenu(msg, current, roles, target.BindingInstanceID)
		return DispatchResult{"completed", ""}
	}
	name, err = selectRoleName(name, roles, msg.InternalAction != nil && msg.InternalAction.Type == "role", func(command string) string { return r.ingressCommandHint(msg, command) })
	if err != nil {
		r.enqueueIngressInteractionResult(msg, "role", interactionRequestID(msg), "Role unavailable", "⚠️ "+err.Error(), "grey", target.BindingInstanceID)
		return DispatchResult{"rejected", "role_selection_invalid"}
	}

	if name == current {
		r.enqueueIngressInteractionResult(msg, "role", interactionRequestID(msg), "Role unchanged", "ℹ️ "+name+" is already the current role.", "grey", target.BindingInstanceID)
		return DispatchResult{"completed", ""}
	}
	response, err := p.waitIngressRole(wait, target, map[string]any{"type": "role", "action": "set", "role": name})
	if err != nil {
		r.enqueueIngressInteractionResult(msg, "role", interactionRequestID(msg), "Role switch unconfirmed", "Could not confirm the role switch. Send "+r.ingressCommandHint(msg, "/status")+" to check the current role.", "grey", target.BindingInstanceID)
		return DispatchResult{"unknown", "role_switch_unconfirmed"}
	}
	if !response.OK {
		r.enqueueIngressInteractionResult(msg, "role", interactionRequestID(msg), "Role switch rejected", "⚠️ "+roleResponseMessage(response.Message, "Role switch was rejected."), "red", target.BindingInstanceID)
		return DispatchResult{"rejected", "role_switch_rejected"}
	}
	r.enqueueIngressInteractionResult(msg, "role", interactionRequestID(msg), "Role switched", "✅ Switched role: "+name, "green", target.BindingInstanceID)
	return DispatchResult{"completed", ""}
}
func (p *ChordProcess) waitIngressRole(ctx context.Context, target *IngressControlTarget, payload map[string]any) (RoleResponse, error) {
	ch := make(chan RoleResponse, 1)
	p.mu.Lock()
	p.roleWaiters.add(ch)
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.roleWaiters.remove(ch); p.mu.Unlock() }()
	if err := p.sendIngressControl(target, IMCommand{Type: "role"}, payload); err != nil {
		return RoleResponse{}, err
	}
	select {
	case response := <-ch:
		p.mu.RLock()
		valid := p.ingressTargetMatchesLocked(target, IMCommand{Type: "role"})
		p.mu.RUnlock()
		if !valid {
			return RoleResponse{}, errIngressTargetExpired
		}
		return response, nil
	case <-ctx.Done():
		return RoleResponse{}, ctx.Err()
	}
}
