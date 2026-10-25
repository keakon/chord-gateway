package im

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Ingress acceptance plumbing shared by every transport: the closed/capacity
// sentinels, the bounded acceptance budget, the normalization wait, and the
// submission refusal mapping.

var (
	// ErrIngressClosed reports that the shared ingress service is gone.
	ErrIngressClosed = errors.New("ingress is closed")
	// ErrIngressCapacity reports global ingress backpressure.
	ErrIngressCapacity = errors.New("ingress capacity reached")
	// ErrIngressCaptureBusy reports that the control target for one message is
	// already being captured elsewhere; the retry is per-target.
	ErrIngressCaptureBusy = errors.New("ingress capture is busy")
	// ErrIngressTransportChanged reports that the transport identity changed
	// before an admission or checkpoint could commit.
	ErrIngressTransportChanged = errors.New("transport identity changed before admission")
	// ErrIngressCursorConflict reports a non-sequential cursor generation.
	ErrIngressCursorConflict = errors.New("ingress cursor generation conflict")
)

// IngressAcceptBudget bounds how long a dispatch waits for an acceptance before
// reporting an uncertain outcome. It is variable so the acceptance-budget test
// does not wait out the full window; production keeps two seconds.
var IngressAcceptBudget = 2 * time.Second

// WaitIngressNormalization waits for the committed outcome. Cancellation of
// the wait cannot establish whether the writer committed: report uncertain,
// never retryable. The buffered channel still receives the authoritative result
// and transport redelivery is safe through in-memory business-ID deduplication.
func WaitIngressNormalization(ctx context.Context, result <-chan ReceiveResult) ReceiveResult {
	if result == nil {
		return ReceiveResult{Disposition: ReceiveRetryable, Reason: "normalization_unavailable"}
	}
	// Prefer an already-completed result over a simultaneous cancellation.
	select {
	case value, ok := <-result:
		return ingressNormalizationResult(value, ok)
	default:
	}
	select {
	case value, ok := <-result:
		return ingressNormalizationResult(value, ok)
	case <-ctx.Done():
		select {
		case value, ok := <-result:
			return ingressNormalizationResult(value, ok)
		default:
			return ReceiveResult{Disposition: ReceiveUncertain, Reason: "normalization_wait_cancelled"}
		}
	}
}

func ingressNormalizationResult(value ReceiveResult, ok bool) ReceiveResult {
	if !ok {
		return ReceiveResult{Disposition: ReceiveUncertain, Reason: "normalization_result_unavailable"}
	}
	return value
}

// SubmitRefusalReason maps a submission failure to its bounded metric reason.
// Capture-busy is a retry against one target, capacity is global backpressure,
// and anything else leaves the ingress unavailable.
func SubmitRefusalReason(err error) string {
	switch {
	case errors.Is(err, ErrIngressCaptureBusy):
		return "target_busy"
	case errors.Is(err, ErrIngressCapacity):
		return "capacity"
	default:
		return "ingress_unavailable"
	}
}

// SleepCtx waits for d to elapse or ctx to be cancelled. Returns true if the
// full duration elapsed, false if the context was cancelled. A non-positive d
// defaults to 1 second.
func SleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		d = time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ReadinessNotifier owns the dispatcher wake callback shared by every
// transport. A dedicated lock keeps it independent of platform state, and the
// callback always runs outside the lock so a transport cannot deadlock the
// dispatcher or its own state. Adapters embed it by value.
type ReadinessNotifier struct {
	mu     sync.Mutex
	notify func()
}

// SetIngressReadinessNotifier installs or detaches the dispatcher wake
// callback; passing nil detaches it during shutdown.
func (n *ReadinessNotifier) SetIngressReadinessNotifier(notify func()) {
	n.mu.Lock()
	n.notify = notify
	n.mu.Unlock()
}

// WakeIngressReadiness runs the dispatcher wake callback outside the lock.
func (n *ReadinessNotifier) WakeIngressReadiness() {
	n.mu.Lock()
	notify := n.notify
	n.mu.Unlock()
	if notify != nil {
		notify()
	}
}
