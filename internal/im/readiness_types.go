package im

// IngressDeliveryReadiness describes a transport prerequisite, never permission
// to execute. Providers must only inspect local state: no network or waiting.
// A waiting record keeps its original received time, sequence and route binding.
type IngressDeliveryReadiness struct {
	Ready  bool
	Reason string
}

// IngressDeliveryReadinessProvider is optional. Platforms with no extra local
// prerequisite remain ready under the ordinary admission/session checks.
type IngressDeliveryReadinessProvider interface {
	IngressDeliveryReadiness(ChatIdentity) IngressDeliveryReadiness
}

// IngressDeliveryReadinessNotifier lets a provider wake the common dispatcher
// when local state changes. Passing nil detaches the previous callback during
// shutdown. The callback must be called outside provider locks; it must not
// perform delivery itself or create a platform-specific recovery queue.
type IngressDeliveryReadinessNotifier interface {
	SetIngressReadinessNotifier(func())
}

const (
	IngressWaitingReplyContext = "reply_context_missing"
	IngressWaitingAccount      = "account_unavailable"
	IngressWaitingPlatform     = "platform_not_ready"
	IngressWaitingCondition    = "delivery_condition_unavailable"
)
