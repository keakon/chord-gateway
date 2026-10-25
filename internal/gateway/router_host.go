package gateway

// Host-facing methods on NotificationRouter. Platform adapters depend on these
// (together with the im.IMAccountHost / IMPollingHost / IMSocketHost interface
// methods in im_host_router.go) instead of the concrete router type, so the
// adapters can live in their own packages while this file stays the single
// place that touches pairing, admission and ingress-store internals.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

// CurrentConfig returns the live configuration snapshot; nil when the store is
// absent (console mode before startup completes).
func (r *NotificationRouter) CurrentConfig() *config.Config {
	if r == nil {
		return nil
	}
	return r.getConfig()
}

// IngressReady reports whether admission, the ingress service and the durable
// ingress store are all present and accepting.
func (r *NotificationRouter) IngressReady() bool {
	return r != nil && r.admission != nil && r.ingress != nil && r.ingress.Available() && r.ingressStore != nil
}

// ValidTicket rechecks an admission ticket's authority without upgrading it.
func (r *NotificationRouter) ValidTicket(t im.AdmissionTicket) bool {
	return r != nil && r.admission != nil && r.admission.Valid(t)
}

// AuthorizeIngress admits one classified inbound message and returns its
// ticket; ok is false when the message is rejected or admission is absent.
func (r *NotificationRouter) AuthorizeIngress(msg im.IncomingMessage) (im.AdmissionTicket, bool) {
	if r == nil || r.admission == nil {
		return im.AdmissionTicket{}, false
	}
	return r.admission.Authorize(msg)
}

// TargetForKey resolves the authoritative delivery target for a process key.
func (r *NotificationRouter) TargetForKey(key string) (im.DeliveryTarget, bool) {
	if r == nil {
		return im.DeliveryTarget{}, false
	}
	return r.targetForKey(key)
}

// ValidTarget revalidates a previously captured delivery target against the
// current binding and runtime.
func (r *NotificationRouter) ValidTarget(target im.DeliveryTarget) bool {
	return r != nil && r.validTarget(target)
}

// EnqueueIngressReply queues one diagnostic reply admitted by the message's
// ticket and reports whether it was accepted for delivery.
func (r *NotificationRouter) EnqueueIngressReply(msg im.IncomingMessage, text string) bool {
	if r == nil {
		return false
	}
	return r.enqueueIngressReply(msg, text) == outboundQueued
}

// IngressCursorFor reads the durable polling cursor for one platform account.
func (r *NotificationRouter) IngressCursorFor(platform, account string) (im.IngressCursor, error) {
	if r == nil || r.ingressStore == nil {
		return im.IngressCursor{}, im.ErrIngressClosed
	}
	return r.ingressStore.Cursor(platform, account)
}

// CommitIngressCursor commits a platform checkpoint guarded by the expected
// generation and an optional transport-liveness callback.
func (r *NotificationRouter) CommitIngressCursor(ctx context.Context, platform, account string, expected uint64, next im.IngressCursor, valid func() bool) error {
	if r == nil || r.ingressStore == nil {
		return im.ErrIngressClosed
	}
	return r.ingressStore.commitCursorGuarded(ctx, platform, account, expected, next, valid)
}

// ObserveLostBeforeAccept records that a transport dropped input before the
// shared ingress could accept it.
func (r *NotificationRouter) ObserveLostBeforeAccept(platform, reason string) {
	if r != nil && r.ingressStore != nil {
		r.ingressStore.ObserveLostBeforeAccept(platform, reason)
	}
}

// PairingEpoch returns the live pairing epoch for platform+account and whether
// that account is still the paired owner with an active platform context.
func (r *NotificationRouter) PairingEpoch(platform, account string) (uint64, bool) {
	if r == nil || r.admission == nil {
		return 0, false
	}
	p := r.admission.pairing
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.platforms[platform]
	if state == nil || state.account != account || state.changed || state.ctx.Err() != nil {
		return 0, false
	}
	return state.epoch, true
}

// PlatformReadySimple marks the platform ready without a bot presentation.
func (r *NotificationRouter) PlatformReadySimple(platform, account string) error {
	if r == nil || r.admission == nil {
		return im.ErrPairingDenied
	}
	return r.admission.pairing.PlatformReady(platform, account)
}

// ReplaceTransportAccount handles a transport credential replacement under the
// config-store transaction lock: it invalidates the paired platform state and
// closes the platform's binding leases, then runs commit (the adapter's own
// credential swap) while the lock is still held, and finally stops the
// platform's paired processes asynchronously. It reports whether the account
// actually changed.
func (r *NotificationRouter) ReplaceTransportAccount(platform, account string, commit func()) bool {
	if r == nil || r.admission == nil {
		commit()
		return false
	}
	p := r.admission.pairing
	p.store.txMu.Lock()
	defer p.store.txMu.Unlock()
	p.mu.Lock()
	state := p.platforms[platform]
	changed := state != nil && account != "" && state.account != account
	if state != nil {
		state.ready = false
		if changed {
			if state.cancel != nil {
				state.cancel()
			}
			state.epoch++
			state.changed = true
		}
	}
	p.mu.Unlock()
	if changed {
		r.admission.mu.Lock()
		for id, g := range r.admission.gates {
			if id.Platform == platform {
				lease, _, _ := g.Snapshot()
				if lease != nil {
					lease.Close()
				}
			}
		}
		r.admission.mu.Unlock()
	}
	if commit != nil {
		commit()
	}
	if changed {
		go r.StopPairedPlatform(platform)
	}
	return changed
}

// ExpireTransportAccount clears a transport credential under the config-store
// transaction lock and withdraws the platform's readiness afterwards.
func (r *NotificationRouter) ExpireTransportAccount(platform string, clear func()) {
	if r != nil && r.admission != nil {
		p := r.admission.pairing
		p.store.txMu.Lock()
		defer p.store.txMu.Unlock()
	}
	if clear != nil {
		clear()
	}
	if r != nil && r.admission != nil {
		p := r.admission.pairing
		p.mu.Lock()
		if state := p.platforms[platform]; state != nil {
			state.ready = false
		}
		p.mu.Unlock()
	}
}

// StopPairedPlatform stops every process and reminder bound to the platform's
// current gates and marks the gates blocked. Used when an account is replaced.
func (r *NotificationRouter) StopPairedPlatform(platform string) {
	if r == nil || r.admission == nil {
		return
	}
	r.admission.mu.Lock()
	var gates []*im.ChatGate
	for id, g := range r.admission.gates {
		if id.Platform == platform {
			gates = append(gates, g)
		}
	}
	r.admission.mu.Unlock()
	for _, g := range gates {
		g.OpMu.Lock()
		lease, _, _ := g.Snapshot()
		if lease != nil {
			key := (processKey{workspaceID: lease.WorkspaceID, imType: platform, chatID: lease.Identity.ChatID}).String()
			r.stopReminder(key)
			if p := r.mgr.GetProcessForKey(key); p != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = p.StopAndWait(ctx, 2*time.Second)
				cancel()
			}
		}
		g.Block()
		g.OpMu.Unlock()
	}
}

// NotifyPairingVerificationWait writes the platform's identity-verification
// instructions to the pairing terminal, if one is attached.
func (r *NotificationRouter) NotifyPairingVerificationWait(platform, instructions string) error {
	if r == nil || r.admission == nil {
		return nil
	}
	p := r.admission.pairing
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.terminal == nil {
		return nil
	}
	if _, err := fmt.Fprintln(p.terminal, instructions); err != nil {
		return errors.New(platform + ": could not display identity verification instructions")
	}
	return nil
}

// SubmitIngressProtocol routes a transport-defined protocol probe (an
// unverified caller that must never reach the command dispatcher) through the
// shared normalizer under the platform account/chat key and the global
// acceptance budget.
func (r *NotificationRouter) SubmitIngressProtocol(ctx context.Context, platform, account, chatID string, protocol func(ctx context.Context) im.ReceiveResult) (<-chan im.ReceiveResult, error) {
	if r == nil || r.ingress == nil {
		return nil, im.ErrIngressClosed
	}
	s := r.ingress
	s.mu.Lock()
	n := s.normalizer
	active := s.started && !s.stopping
	s.mu.Unlock()
	if !active || n == nil {
		return nil, im.ErrIngressClosed
	}
	key := string(ingressKey("", platform, account, chatID))
	return n.submitProtocol(ctx, key, protocol)
}

// AcceptIdentityProbeRecord commits the durable rejection tombstone for one
// identity probe and returns the acceptance decision.
func (r *NotificationRouter) AcceptIdentityProbeRecord(ctx context.Context, platform, account, chatID, senderID, businessID, deliveryID string, sourceTime, receivedAt time.Time, reason string) im.ReceiveResult {
	if r == nil || r.ingressStore == nil {
		return im.ReceiveResult{Disposition: im.ReceiveUncertain, Reason: "ingress_unavailable"}
	}
	record := IngressRecord{Platform: platform, AccountID: account, ChatID: chatID, SenderID: senderID, BusinessID: businessID, DeliveryID: deliveryID, SourceTime: sourceTime, ReceivedAt: receivedAt, Kind: "control", State: im.IngressRejected, Reason: reason}
	return r.ingressStore.AcceptRecord(ctx, record, nil)
}

// PublishPlatformReadinessUnderConfigTx marks the platform ready while holding
// the config-store transaction lock, as polling transports must do when they
// publish readiness alongside a checkpoint write.
func (r *NotificationRouter) PublishPlatformReadinessUnderConfigTx(ctx context.Context, platform, account string) error {
	if r == nil || r.admission == nil {
		return im.ErrPairingDenied
	}
	p := r.admission.pairing
	if err := lockIngressMutex(ctx, &p.store.txMu); err != nil {
		return err
	}
	defer p.store.txMu.Unlock()
	return p.PlatformReady(platform, account)
}
