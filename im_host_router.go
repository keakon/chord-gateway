package main

import (
	"github.com/keakon/chord-gateway/internal/im"
)

func (r *NotificationRouter) IngressAvailable() bool {
	return r != nil && r.admission != nil && r.ingress != nil && r.ingress.Available()
}
func (r *NotificationRouter) PlatformReady(platform, account string, presentation im.PlatformPresentation) error {
	if r == nil || r.admission == nil {
		return im.ErrIngressClosed
	}
	return r.admission.pairing.PlatformReadyWithPresentation(platform, account, presentation)
}
func (r *NotificationRouter) PlatformUnavailable(platform string) {
	if r != nil && r.admission != nil {
		r.admission.pairing.PlatformUnavailable(platform)
	}
}
func (r *NotificationRouter) ObserveIngress(platform, event, reason string) {
	if r != nil && r.ingressStore != nil {
		r.ingressStore.metrics.record(platform, event, reason)
	}
}
func (r *NotificationRouter) TryCaptureIngress(msg im.IncomingMessage) (string, *im.IngressControlTarget, bool) {
	return r.tryCaptureIngress(msg)
}
