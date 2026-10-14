package main

import "context"

// IMAccountHost is the lifecycle boundary shared by polling and socket adapters.
// Adapters publish verified transport facts; the host owns pairing and admission.
type IMAccountHost interface {
	IngressAvailable() bool
	PrepareIngressAccount(context.Context, string, string) error
	PlatformReady(string, string, PlatformPresentation) error
	PlatformUnavailable(string)
	ObserveIngress(string, string, string)
}

type IMPollingHost interface {
	IMAccountHost
	ReceiveIngress(context.Context, IngressEnvelope) ReceiveResult
	RejectIngressEnvelope(context.Context, IngressEnvelope, string) ReceiveResult
	CaptureIngress(IncomingMessage) (string, *IngressControlTarget)
}

type IMSocketHost interface {
	IMAccountHost
	TryCaptureIngress(IncomingMessage) (string, *IngressControlTarget, bool)
	SubmitIngressNormalization(context.Context, string, IngressEnvelope, func(context.Context, IngressEnvelope) (IngressEnvelope, error)) (<-chan ReceiveResult, error)
}

func (r *NotificationRouter) IngressAvailable() bool {
	return r != nil && r.admission != nil && r.ingress != nil && r.ingress.Available()
}
func (r *NotificationRouter) PlatformReady(platform, account string, presentation PlatformPresentation) error {
	if r == nil || r.admission == nil {
		return errIngressClosed
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
func (r *NotificationRouter) TryCaptureIngress(msg IncomingMessage) (string, *IngressControlTarget, bool) {
	return r.tryCaptureIngress(msg)
}
