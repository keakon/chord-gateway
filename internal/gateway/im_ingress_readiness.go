package gateway

import (
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

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
