package im

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
