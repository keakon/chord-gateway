package main

import (
	"sync"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

// ingressReadinessNotifier owns the dispatcher wake callback shared by every
// transport. A dedicated lock keeps it independent of platform state, and the
// callback always runs outside the lock so a transport cannot deadlock the
// dispatcher or its own state.
type ingressReadinessNotifier struct {
	mu     sync.Mutex
	notify func()
}

func (n *ingressReadinessNotifier) SetIngressReadinessNotifier(notify func()) {
	n.mu.Lock()
	n.notify = notify
	n.mu.Unlock()
}

func (n *ingressReadinessNotifier) wakeIngressReadiness() {
	n.mu.Lock()
	notify := n.notify
	n.mu.Unlock()
	if notify != nil {
		notify()
	}
}

// checkIngressDeliveryReadiness prevents arbitrary platform payloads or errors
// from becoming stored/metric reason labels. An invalid result fails closed.
func checkIngressDeliveryReadiness(provider any, id im.ChatIdentity) im.IngressDeliveryReadiness {
	p, ok := provider.(im.IngressDeliveryReadinessProvider)
	if !ok {
		return im.IngressDeliveryReadiness{Ready: true}
	}
	result := p.IngressDeliveryReadiness(id)
	if result.Ready {
		return im.IngressDeliveryReadiness{Ready: true}
	}
	switch result.Reason {
	case im.IngressWaitingReplyContext, im.IngressWaitingAccount, im.IngressWaitingPlatform:
		return result
	default:
		return im.IngressDeliveryReadiness{Reason: im.IngressWaitingCondition}
	}
}

func ingressDeliveryExpired(receivedAt, now time.Time) bool {
	return receivedAt.IsZero() || !now.Before(receivedAt.Add(24*time.Hour))
}
