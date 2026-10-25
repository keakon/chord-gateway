package im

import "context"

// BindingProbeTarget authorizes only the adapter's fixed diagnostic text. It
// deliberately cannot carry content, a URL, or a workspace operation.
type BindingProbeTarget struct{ Platform, AccountID, ChatID string }

// BindingTargetValidator is implemented by adapters that must confirm a
// binding directory with a bounded diagnostic send before authority changes.
type BindingTargetValidator interface {
	ValidateBindingTarget(context.Context, BindingProbeTarget) error
}
