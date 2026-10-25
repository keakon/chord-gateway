package wecom

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/keakon/chord-gateway/internal/im"

	"github.com/keakon/golog/log"
)

const BindingCheckMessage = "Gateway binding check."

// wecomProcessKey encodes the gateway's opaque process key for one workspace
// binding (v2:<base64url(JSON [workspaceID, imType, chatID])>). The canonical
// encoder lives in the gateway core, which the adapter only sees through the
// Host interface, so the wire format is reproduced here unchanged.
func wecomProcessKey(workspaceID, chatID string) string {
	payload, err := json.Marshal([]string{workspaceID, "wecom", chatID})
	if err != nil {
		return ""
	}
	return "v2:" + base64.RawURLEncoding.EncodeToString(payload)
}

// Neutralize angle-bracket mention syntax while preserving plain @ in command
// arguments (for example /answer @request 1).
func renderWecomText(source string) string {
	var out strings.Builder
	for _, r := range source {
		switch r {
		case '<':
			out.WriteRune('＜')
		case '>':
			out.WriteRune('＞')
		case '\\', '`', '*', '_', '{', '}', '[', ']', '(', ')', '#', '+', '-', '.', '!', '|', '~', '=':
			out.WriteByte('\\')
			out.WriteRune(r)
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

func (a *WecomAdapter) SendText(chatID, text string) error {
	if a.host == nil {
		return im.ErrPairingDenied
	}
	cfg := a.host.CurrentConfig()
	ws, err := cfg.ResolveWorkspace("wecom", chatID)
	if err != nil || ws == nil {
		return im.ErrPairingDenied
	}
	target, ok := a.host.TargetForKey(wecomProcessKey(ws.ID, chatID))
	if !ok {
		return im.ErrPairingDenied
	}
	return a.host.SendBound(a.ctx, target, text)
}

func (a *WecomAdapter) SendTextContext(ctx context.Context, chatID, text string) error {
	if ctx == nil || a.host == nil {
		return im.ErrPairingDenied
	}
	control := false
	switch {
	case ctx.Value(im.OutboundDeliveryContextKey{}) != nil:
		target, ok := ctx.Value(im.OutboundDeliveryContextKey{}).(im.DeliveryTarget)
		if !ok || target.Identity != (im.ChatIdentity{Platform: "wecom", AccountID: a.imCfg.Wecom.BotID, ChatID: chatID}) || !a.host.ValidTarget(target) {
			return im.ErrPairingDenied
		}
	case ctx.Value(im.OutboundReplyContextKey{}) != nil:
		permit, ok := ctx.Value(im.OutboundReplyContextKey{}).(im.ReplyPermit)
		if !ok || permit.Identity != (im.ChatIdentity{Platform: "wecom", AccountID: a.imCfg.Wecom.BotID, ChatID: chatID}) || !time.Now().Before(permit.ExpiresAt) || !a.host.ValidTicket(permit.Ticket) {
			return im.ErrPairingDenied
		}
		control = true
	default:
		return im.ErrPairingDenied
	}
	return a.textSender.Send(ctx, a.ctx, ctx, chatID, text, control)
}

// ValidateBindingTarget is the sole temporary unbound-target outlet. Its narrow
// signature cannot choose arbitrary text, and BindingService verifies the
// nonce/owner before calling it and revalidates authority before persistence.
func (a *WecomAdapter) ValidateBindingTarget(ctx context.Context, target im.BindingProbeTarget) error {
	if ctx == nil || target.Platform != "wecom" || target.AccountID != a.imCfg.Wecom.BotID || !a.IngressDeliveryReadiness(im.ChatIdentity(target)).Ready {
		return im.ErrPairingDenied
	}
	if _, err := DecodeWecomTarget(target.ChatID); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, im.IMControlSendTimeout)
	defer cancel()
	stop := context.AfterFunc(a.ctx, cancel)
	defer stop()
	if a.ctx.Err() != nil {
		cancel()
	}
	// Share normal output's target ordering slot so the fixed probe cannot
	// overtake a body segment that is still awaiting platform acceptance.
	release, err := a.textSender.AcquireTarget(ctx, target.ChatID)
	if err != nil {
		return err
	}
	defer release()
	select {
	case im.IMTextSendSlots <- struct{}{}:
		defer func() { <-im.IMTextSendSlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	return a.sendSegment(ctx, target.ChatID, BindingCheckMessage)
}

func (a *WecomAdapter) sendSegment(ctx context.Context, target, rendered string) error {
	// Refuse here rather than inside SendMarkdown so the reason is a definite
	// non-delivery, and use the shared category vocabulary so the common
	// executor records it instead of falling back to delivery_failed.
	if !a.IngressDeliveryReadiness(im.ChatIdentity{Platform: "wecom", AccountID: a.imCfg.Wecom.BotID, ChatID: target}).Ready {
		return wecomWSError("connection_unavailable")
	}
	raw, err := DecodeWecomTarget(target)
	if err != nil {
		return err
	}
	if err = a.waitSendSlot(ctx, target); err != nil {
		return err
	}
	err = a.ws.SendMarkdown(ctx, raw, rendered)
	if err != nil {
		var failure *WecomTransportError
		if errors.As(err, &failure) {
			log.Warnf("wecom text send failed: category=%v code=%v", failure.Category, failure.Code)
		}
	}
	return err
}

// Only successful acquisition updates the clock. Cancelling a throttled send
// cannot reserve capacity far into the future. Recent targets are bounded;
// expired entries may be pruned without weakening the four-second interval.
func (a *WecomAdapter) waitSendSlot(ctx context.Context, target string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.rateMu.Lock()
		now := time.Now()
		for key, next := range a.targetNext {
			if !next.After(now) {
				delete(a.targetNext, key)
			}
		}
		next := a.accountNext
		if a.targetNext[target].After(next) {
			next = a.targetNext[target]
		}
		if !next.After(now) {
			if len(a.targetNext) >= 128 {
				a.rateMu.Unlock()
				return wecomWSError("backpressure")
			}
			a.accountNext = now.Add(a.accountInterval)
			a.targetNext[target] = now.Add(a.targetInterval)
			a.rateMu.Unlock()
			return nil
		}
		delay := next.Sub(now)
		a.rateMu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

func (a *WecomAdapter) TextSendMetrics() im.IMTextMetricsSnapshot {
	return a.textSender.TextSendMetrics()
}
func (a *WecomAdapter) TextSendTargetSnapshot(target string) im.IMTextTargetSnapshot {
	return a.textSender.TextSendTargetSnapshot(target)
}
