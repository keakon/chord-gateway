package main

import (
	"context"
	"errors"
	"time"
)

type DeliveryTarget struct {
	Identity                        ChatIdentity
	AccountEpoch, BindingGeneration uint64
	ProcessKey, RuntimeInstanceID   string
	lease                           *BindingLease
	paired                          bool
}

type ReplyPermit struct {
	Identity        ChatIdentity
	SenderID        string
	AccountEpoch    uint64
	RequestID, Kind string
	ExpiresAt       time.Time
	ticket          AdmissionTicket
}

type contextTextSender interface {
	SendTextContext(context.Context, string, string) error
}

func (r *NotificationRouter) targetForKey(key string) (DeliveryTarget, bool) {
	ws, platform, chat := parseProcessKey(key)
	target := DeliveryTarget{Identity: ChatIdentity{Platform: platform, ChatID: chat}, ProcessKey: key}
	if r.mgr != nil {
		if p := r.mgr.GetProcessForKey(key); p != nil {
			target.RuntimeInstanceID = p.bindingInstanceID
		}
	}
	cfg := r.getConfig()
	if cfg == nil || r.admission == nil {
		return target, r.admission == nil
	}
	if platform == "console" {
		if r.admission != nil {
			r.admission.mu.Lock()
			gate := r.admission.gates[ChatIdentity{Platform: platform, ChatID: chat}]
			r.admission.mu.Unlock()
			if gate != nil {
				lease, gen, phase := gate.snapshot()
				if phase != "active" || lease == nil || lease.WorkspaceID != ws || lease.ctx.Err() != nil {
					return target, false
				}
				target.lease = lease
				target.BindingGeneration = gen
			}
		}
		return target, true
	}
	target.paired = true
	if r.admission == nil {
		return target, false
	}
	im := cfg.IMConfigByType(platform)
	if im == nil {
		return target, false
	}
	owner := im.OwnerIdentity()
	if owner == nil || im.Bindings()[chat] != ws {
		return target, false
	}
	target.Identity.AccountID = owner.AccountID
	r.admission.mu.Lock()
	gate := r.admission.gates[target.Identity]
	closed := r.admission.closed
	r.admission.mu.Unlock()
	if gate == nil || closed {
		return target, false
	}
	lease, generation, phase := gate.snapshot()
	if lease == nil || phase != "active" || lease.ctx.Err() != nil {
		return target, false
	}
	target.lease = lease
	target.BindingGeneration = generation
	target.AccountEpoch = lease.AccountEpoch
	return target, true
}

func (r *NotificationRouter) validTarget(target DeliveryTarget) bool {
	current, ok := r.targetForKey(target.ProcessKey)
	if !ok || current.RuntimeInstanceID != target.RuntimeInstanceID {
		return false
	}
	if target.lease == nil {
		return current.lease == nil
	}
	if current.lease != target.lease {
		return false
	}
	if !target.paired {
		return true
	}
	p := r.admission.pairing
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.platforms[target.Identity.Platform]
	return s != nil && !s.changed && s.epoch == target.AccountEpoch && s.account == target.Identity.AccountID
}

func (r *NotificationRouter) SendBound(ctx context.Context, target DeliveryTarget, text string) error {
	ctx, done, err := r.deliveryContext(ctx, target)
	if err != nil {
		return err
	}
	defer done()
	if target.Identity.Platform == "console" && r.findAdapterByType("console") == nil {
		return sendConsoleText(target.Identity.ChatID, text)
	}
	adapter := r.deliveryAdapter(target.Identity.Platform)
	if adapter == nil {
		return errors.New("adapter unavailable")
	}
	if sender, ok := adapter.(contextTextSender); ok {
		return sender.SendTextContext(ctx, target.Identity.ChatID, text)
	}
	if target.paired {
		return errors.New("adapter does not support cancellable delivery")
	}
	return adapter.SendText(target.Identity.ChatID, text)
}

func (r *NotificationRouter) SendFlowReply(ctx context.Context, permit ReplyPermit, text string) error {
	if !time.Now().Before(permit.ExpiresAt) || !r.admission.Valid(permit.ticket) {
		return errPairingDenied
	}
	p := r.admission.pairing
	p.mu.Lock()
	state := p.platforms[permit.Identity.Platform]
	if state == nil || !state.ready || state.changed || state.epoch != permit.AccountEpoch || state.account != permit.Identity.AccountID {
		p.mu.Unlock()
		return errPairingDenied
	}
	parent := state.ctx
	p.mu.Unlock()
	ctx = context.WithValue(ctx, replyAccountContextKey{}, permit.Identity.AccountID)
	ctx = context.WithValue(ctx, outboundDeliveryContextKey{}, nil)
	ctx = context.WithValue(ctx, outboundReplyContextKey{}, permit)
	ctx, cancel := context.WithDeadline(ctx, permit.ExpiresAt)
	defer cancel()
	if parent != nil {
		stop := context.AfterFunc(parent, cancel)
		defer stop()
		if parent.Err() != nil {
			cancel()
		}
	}
	adapter := r.findAdapterByType(permit.Identity.Platform)
	sender, ok := adapter.(contextTextSender)
	if !ok {
		return errors.New("adapter does not support flow replies")
	}
	return sender.SendTextContext(ctx, permit.Identity.ChatID, text)
}

func (r *NotificationRouter) sendTextForKey(key, chat, text string) bool {
	target, ok := r.targetForKey(key)
	if !ok {
		return false
	}
	return r.SendBound(context.Background(), target, text) == nil
}
func (r *NotificationRouter) sendTextForIM(platform, chat, text string) bool {
	if platform == "console" && r.findAdapterByType("console") == nil {
		return sendConsoleText(chat, text) == nil
	}
	if r.admission == nil && platform == "multi" {
		return r.sendText(chat, text)
	}
	cfg := r.getConfig()
	if cfg != nil && r.admission != nil && platform != "console" {
		ws, err := cfg.ResolveWorkspace(platform, chat)
		if err != nil {
			return false
		}
		return r.sendTextForKey((processKey{workspaceID: ws.ID, imType: platform, chatID: chat}).String(), chat, text)
	}
	adapter := r.deliveryAdapter(platform)
	if adapter == nil {
		return false
	}
	return adapter.SendText(chat, text) == nil
}

// deliveryContext admits a send for the captured binding and runtime. Runtime
// retirement cancels admitted I/O; it never upgrades old work to a new process.
func (r *NotificationRouter) deliveryContext(parent context.Context, target DeliveryTarget) (context.Context, func(), error) {
	if !r.validTarget(target) {
		return nil, nil, context.Canceled
	}
	if target.paired {
		parent = context.WithValue(parent, replyAccountContextKey{}, target.Identity.AccountID)
	}
	parent = context.WithValue(parent, outboundReplyContextKey{}, nil)
	ctx, cancel := context.WithCancel(context.WithValue(parent, outboundDeliveryContextKey{}, target))
	var stops []func() bool
	watch := func(source context.Context) {
		stops = append(stops, context.AfterFunc(source, cancel))
		if source.Err() != nil {
			cancel()
		}
	}
	if target.paired {
		p := r.admission.pairing
		p.mu.Lock()
		state := p.platforms[target.Identity.Platform]
		if state == nil || state.changed || state.epoch != target.AccountEpoch || state.account != target.Identity.AccountID {
			p.mu.Unlock()
			cancel()
			return nil, nil, context.Canceled
		}
		if state.ctx != nil {
			watch(state.ctx)
		}
		p.mu.Unlock()
	}
	if r.mgr != nil {
		r.mgr.mu.Lock()
		p := r.mgr.procs[target.ProcessKey]
		if p == nil && target.RuntimeInstanceID != "" || p != nil && p.bindingInstanceID != target.RuntimeInstanceID {
			r.mgr.mu.Unlock()
			for _, stop := range stops {
				stop()
			}
			cancel()
			return nil, nil, context.Canceled
		}
		if p != nil {
			if p.deliveryCtx == nil {
				p.deliveryCtx, p.deliveryCancel = context.WithCancel(context.Background())
			}
			watch(p.deliveryCtx)
		}
		r.mgr.mu.Unlock()
	}
	done := func() {
		for _, stop := range stops {
			stop()
		}
		cancel()
	}
	if target.lease != nil {
		if !target.lease.BeginSend() {
			done()
			return nil, nil, context.Canceled
		}
		watch(target.lease.ctx)
		cleanup := done
		done = func() { cleanup(); target.lease.EndSend() }
	}
	return ctx, done, nil
}

func (r *NotificationRouter) sendCardToTarget(target DeliveryTarget, card map[string]any, fallback string) (*InteractiveCardHandle, error) {
	return r.sendCardToTargetContext(context.Background(), target, card, fallback)
}

func (r *NotificationRouter) sendCardToTargetContext(ctx context.Context, target DeliveryTarget, card map[string]any, fallback string) (*InteractiveCardHandle, error) {
	ctx, done, err := r.deliveryContext(ctx, target)
	if err != nil {
		return nil, err
	}
	defer done()
	chat := target.Identity.ChatID
	adapter := r.findFeishuAdapter()
	if adapter == nil {
		return nil, errors.New("adapter unavailable")
	}
	handle, err := adapter.sendMessageContext(ctx, chat, "interactive", card, false)
	if err != nil && !errors.Is(err, errDeliveryOutcomeUnknown) && fallback != "" {
		err = adapter.SendTextContext(ctx, chat, fallback)
	}
	return handle, err
}

func (r *NotificationRouter) deliveryAdapter(platform string) IMAdapter {
	adapter := r.findAdapterByType(platform)
	if adapter == nil && r.admission == nil {
		if current := r.currentAdapter(); current != nil {
			if _, multi := current.(*MultiAdapter); !multi {
				return current
			}
		}
	}
	return adapter
}

func (r *NotificationRouter) ensureOwnedGate(platform, chat, workspace string) {
	p := r.admission.pairing
	p.mu.Lock()
	state := p.platforms[platform]
	if state == nil || !state.ready || state.changed {
		p.mu.Unlock()
		return
	}
	account, epoch := state.account, state.epoch
	p.mu.Unlock()
	im := r.getConfig().IMConfigByType(platform)
	if im == nil || im.OwnerIdentity() == nil || im.OwnerIdentity().AccountID != account {
		return
	}
	r.admission.gate(ChatIdentity{platform, account, chat}, workspace, epoch)
}

type replyAccountContextKey struct{}

// Captured authority lets transports validate their exact destination without
// upgrading a queued output to the current binding or an unrelated flow.
type outboundDeliveryContextKey struct{}
type outboundReplyContextKey struct{}
