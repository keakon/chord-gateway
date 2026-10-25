package main

import (
	"context"
	"errors"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

// Presentation is queued separately from execution. Its target and deadline
// are frozen before enqueue so a later binding cannot receive the response.
func (r *NotificationRouter) enqueueIngressPresentation(msg im.IncomingMessage, present func(context.Context, im.DeliveryTarget) error, expectedBindings ...string) outboundEnqueueResult {
	if r.outbound == nil {
		return outboundClosed
	}
	if msg.AdmissionTicket() != nil {
		ticketSnapshot := *msg.AdmissionTicket()
		msg.SetAdmissionTicket(&ticketSnapshot)
	}
	ws, err := r.getConfig().ResolveWorkspace(msg.IMType, msg.ChatID)
	if err != nil || ws == nil {
		return outboundClosed
	}
	target, ok := r.targetForKey((processKey{workspaceID: ws.ID, imType: msg.IMType, chatID: msg.ChatID}).String())
	if !ok {
		return outboundClosed
	}
	expectedBinding := ""
	if msg.InternalAction != nil {
		expectedBinding = msg.InternalAction.BindingInstanceID
	}
	if len(expectedBindings) > 0 {
		expectedBinding = expectedBindings[0]
	}
	if expectedBinding != "" && target.RuntimeInstanceID != expectedBinding {
		return outboundClosed
	}
	deadline := time.Now().Add(im.IMControlSendTimeout)
	recordDrop := func(reason string) {
		if r.ingressStore != nil {
			r.ingressStore.metrics.record(msg.IMType, "reply_dropped", reason)
		}
	}
	result := r.outbound.enqueue(target.ProcessKey, func() {
		if msg.AdmissionTicket() == nil || r.admission == nil || !r.admission.Valid(*msg.AdmissionTicket()) {
			recordDrop("authorization_changed")
			return
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		if ctx.Err() != nil {
			recordDrop("reply_expired")
			return
		}
		if present(ctx, target) != nil {
			recordDrop("delivery_failed")
		}
	})
	if result == outboundFull {
		recordDrop("outbound_full")
	} else if result == outboundClosed {
		recordDrop("outbound_closed")
	}
	return result
}

func (r *NotificationRouter) enqueueIngressInteractionResult(msg im.IncomingMessage, kind, request, title, text, template string, expectedBindings ...string) {
	r.enqueueIngressInteractionPresentation(msg, kind, request, title, text, template, true, expectedBindings...)
}

func (r *NotificationRouter) enqueueIngressInteractionPresentation(msg im.IncomingMessage, kind, request, title, text, template string, terminal bool, expectedBindings ...string) {
	r.enqueueIngressInteractionFeedback(msg, kind, request, title, text, template, terminal, false, expectedBindings...)
}

func (r *NotificationRouter) enqueueIngressInteractionFeedback(msg im.IncomingMessage, kind, request, title, text, template string, terminal, diagnostic bool, expectedBindings ...string) {
	r.enqueueIngressPresentation(msg, func(ctx context.Context, target im.DeliveryTarget) error {
		if msg.IMType == "feishu" {
			if kind == "confirm" && r.confirmCardIsTerminal(target.ProcessKey, request) {
				if diagnostic {
					return r.SendBound(ctx, target, text)
				}
				return nil
			}
			// A committed Question closure may precede its operation receipt.
			// Its notification owns the final card; a later generic reply must
			// not overwrite it through the callback-supplied handle.
			if kind == "question" && msg.InternalAction != nil {
				if p := r.mgr.GetProcessForKey(target.ProcessKey); p != nil {
					p.mu.RLock()
					q := p.state.Questions[request]
					resolved := p.bindingInstanceID == target.RuntimeInstanceID && q != nil && q.Outcome != "" && q.Version >= msg.InternalAction.Version
					p.mu.RUnlock()
					if resolved {
						// A question's prior closure is not a receipt for this
						// click. Keep its final card, but report a rejected or
						// unconfirmed operation separately.
						if diagnostic {
							return r.SendBound(ctx, target, text)
						}
						return nil
					}
				}
			}
			updated, err := r.patchFeishuCardForTargetContext(ctx, msg, target, kind, request, buildFeishuResolvedCard(title, text, template), terminal)
			if updated {
				return nil
			}
			if errors.Is(err, im.ErrDeliveryOutcomeUnknown) || ctx.Err() != nil {
				return err
			}
		}
		return r.SendBound(ctx, target, text)
	}, expectedBindings...)
}

func (r *NotificationRouter) confirmCardIsTerminal(processKey, requestID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cardHandles[cardHandleKey(processKey, "confirm", requestID)].terminal
}

// Role dispatch renders its own detailed errors. All other controls share one
// diagnostic, so callbacks update their original card without duplicate chat
// messages. Unknown is a checkable interim state, never an invented success.
func (r *NotificationRouter) enqueueIngressDispatchDiagnostic(msg im.IncomingMessage, result DispatchResult) {
	if result.State != im.IngressRejected && result.State != im.IngressUnknown {
		return
	}
	switch result.Reason {
	case "invalid_bind", "binding_not_changed", "role_query_unconfirmed", "role_query_rejected", "role_selection_invalid", "role_switch_unconfirmed", "role_switch_rejected":
		return
	}
	title, text, template := "Request not applied", "Request was not applied: "+result.Reason+". Check "+r.ingressCommandHint(msg, "/status")+".", "grey"
	if result.Reason == "done_reason_required" {
		text = "Rejecting Done requires a reason. Reply " + r.ingressCommandHint(msg, "/deny <what remains to do>") + "."
	}
	if result.Reason == "session_change_busy" {
		// Mirror the legacy console refusal: a busy process is mid-turn, so the
		// actionable step is cancelling first, not checking status.
		text = "The process is busy. Send " + r.ingressCommandHint(msg, "/cancel") + " before changing the session."
	}
	if result.Reason == "session_transferred" {
		text = "The session was transferred elsewhere. Check " + r.ingressCommandHint(msg, "/takeover status") + " for the transfer outcome before changing the session."
	}
	if result.Reason == "undelivered_inputs" {
		text = "Request was not applied: undelivered inputs. Check " + r.ingressCommandHint(msg, "/status") + " and resolve pending input before changing the session; do not resend blindly."
	}
	if result.State == im.IngressUnknown {
		title, text = "Outcome unconfirmed", "Execution outcome unknown. Check "+r.ingressCommandHint(msg, "/status")+" and the original session before sending another request."
	}
	if msg.InternalAction != nil {
		r.enqueueIngressInteractionFeedback(msg, msg.InternalAction.Type, msg.InternalAction.RequestID, title, text, template, result.State == im.IngressRejected, true)
		return
	}
	r.enqueueIngressReply(msg, text)
}

func (r *NotificationRouter) enqueueIngressRoleMenu(msg im.IncomingMessage, current string, roles []RoleInfo, expectedBindings ...string) {
	r.enqueueIngressPresentation(msg, func(ctx context.Context, target im.DeliveryTarget) error {
		text := buildRoleMenuText(current, roles, func(command string) string { return r.ingressCommandHint(msg, command) })
		if msg.IMType != "feishu" || r.findFeishuAdapter() == nil {
			return r.SendBound(ctx, target, text)
		}
		request := newRoleMenuRequestID()
		card := buildFeishuRoleCard(msg.ChatID, request, current, roles)
		stampCardBinding(card, target.RuntimeInstanceID)
		handle, err := r.sendCardToTargetContext(ctx, target, card, text)
		if err == nil && handle != nil {
			r.recordCardHandle(target.ProcessKey, "role", request, handle)
		}
		return err
	}, expectedBindings...)
}
