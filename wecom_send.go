package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/keakon/golog/log"
)

const wecomBindingCheck = "Gateway binding check."

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
	if a.router == nil || a.router.admission == nil {
		return errPairingDenied
	}
	cfg := a.router.getConfig()
	ws, err := cfg.ResolveWorkspace("wecom", chatID)
	if err != nil || ws == nil {
		return errPairingDenied
	}
	target, ok := a.router.targetForKey((processKey{workspaceID: ws.ID, imType: "wecom", chatID: chatID}).String())
	if !ok {
		return errPairingDenied
	}
	return a.router.SendBound(a.ctx, target, text)
}

func (a *WecomAdapter) SendTextContext(ctx context.Context, chatID, text string) error {
	if ctx == nil || a.router == nil || a.router.admission == nil {
		return errPairingDenied
	}
	if !a.IngressDeliveryReadiness(ChatIdentity{"wecom", a.imCfg.Wecom.BotID, chatID}).Ready {
		return errors.New("wecom: connection unavailable")
	}
	control := false
	switch {
	case ctx.Value(outboundDeliveryContextKey{}) != nil:
		target, ok := ctx.Value(outboundDeliveryContextKey{}).(DeliveryTarget)
		if !ok || target.Identity != (ChatIdentity{"wecom", a.imCfg.Wecom.BotID, chatID}) || !a.router.validTarget(target) {
			return errPairingDenied
		}
	case ctx.Value(outboundReplyContextKey{}) != nil:
		permit, ok := ctx.Value(outboundReplyContextKey{}).(ReplyPermit)
		if !ok || permit.Identity != (ChatIdentity{"wecom", a.imCfg.Wecom.BotID, chatID}) || !time.Now().Before(permit.ExpiresAt) || !a.router.admission.Valid(permit.ticket) {
			return errPairingDenied
		}
		control = true
	default:
		return errPairingDenied
	}
	return a.textSender.Send(ctx, a.ctx, ctx, chatID, text, control)
}

// ValidateBindingTarget is the sole temporary unbound-target outlet. Its narrow
// signature cannot choose arbitrary text, and BindingService verifies the
// nonce/owner before calling it and revalidates authority before persistence.
func (a *WecomAdapter) ValidateBindingTarget(ctx context.Context, target BindingProbeTarget) error {
	if ctx == nil || target.Platform != "wecom" || target.AccountID != a.imCfg.Wecom.BotID || !a.IngressDeliveryReadiness(ChatIdentity(target)).Ready {
		return errPairingDenied
	}
	if _, err := DecodeWecomTarget(target.ChatID); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, IMControlSendTimeout)
	defer cancel()
	stop := context.AfterFunc(a.ctx, cancel)
	defer stop()
	if a.ctx.Err() != nil {
		cancel()
	}
	// Share normal output's target ordering slot so the fixed probe cannot
	// overtake a body segment that is still awaiting platform acceptance.
	release, err := a.textSender.acquireTarget(ctx, target.ChatID)
	if err != nil {
		return err
	}
	defer release()
	select {
	case imTextSendSlots <- struct{}{}:
		defer func() { <-imTextSendSlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	return a.sendSegment(ctx, target.ChatID, wecomBindingCheck)
}

func (a *WecomAdapter) sendSegment(ctx context.Context, target, rendered string) error {
	if !a.IngressDeliveryReadiness(ChatIdentity{"wecom", a.imCfg.Wecom.BotID, target}).Ready {
		return errors.New("wecom: connection unavailable")
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
				return errors.New("wecom: local send backpressure")
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

func (a *WecomAdapter) TextSendMetrics() IMTextMetricsSnapshot { return a.textSender.TextSendMetrics() }
func (a *WecomAdapter) TextSendTargetSnapshot(target string) IMTextTargetSnapshot {
	return a.textSender.TextSendTargetSnapshot(target)
}
