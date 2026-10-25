package main

import (
	"context"
	"errors"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

// SubmitIngress captures the operation before asynchronous normalization and
// uses the Gateway's single bounded queue. Adapters map its result to their
// protocol acknowledgment; submitting a future is not in-memory acceptance.
func (r *NotificationRouter) SubmitIngress(ctx context.Context, envelope im.IngressEnvelope) (<-chan im.ReceiveResult, error) {
	return r.SubmitIngressNormalization(ctx, envelope.Message.ChatID, envelope, nil)
}

// SubmitIngressObserved keeps socket acceptance observation on the Gateway's
// bounded workers. observe must only record diagnostics or enqueue a reply;
// it must never wait for network I/O. No observer runs when submission fails.
func (r *NotificationRouter) SubmitIngressObserved(ctx context.Context, envelope im.IngressEnvelope, observe func(im.ReceiveResult)) (<-chan im.ReceiveResult, error) {
	return r.submitIngressNormalization(ctx, envelope.Message.ChatID, envelope, nil, observe)
}

// SubmitIngressNormalization lets a transport resolve identities without
// recapturing a newer control target. The key is account scoped and supplied in
// transport receive order, before any network lookup. Normalization may enrich
// missing identity fields and display metadata, but never replace the received
// operation, its known identity, or its platform checkpoint.
func (r *NotificationRouter) SubmitIngressNormalization(ctx context.Context, key string, envelope im.IngressEnvelope, normalize func(context.Context, im.IngressEnvelope) (im.IngressEnvelope, error)) (<-chan im.ReceiveResult, error) {
	return r.submitIngressNormalization(ctx, key, envelope, normalize, nil)
}

func (r *NotificationRouter) submitIngressNormalization(ctx context.Context, key string, envelope im.IngressEnvelope, normalize func(context.Context, im.IngressEnvelope) (im.IngressEnvelope, error), observe func(im.ReceiveResult)) (<-chan im.ReceiveResult, error) {
	if r.ingress == nil {
		return nil, im.ErrIngressClosed
	}
	if key == "" || envelope.Message.IMType == "" || envelope.Message.AccountID == "" {
		return nil, errors.New("invalid normalization scope")
	}
	key = string(ingressKey("", envelope.Message.IMType, envelope.Message.AccountID, key))
	if envelope.ReceivedAt.IsZero() {
		envelope.ReceivedAt = time.Now()
	}
	envelope = r.CaptureIngressEnvelope(envelope)
	if envelope.Capture.Err != nil {
		return nil, envelope.Capture.Err
	}
	r.ingress.mu.Lock()
	normalizer := r.ingress.normalizer
	available := r.ingress.started && !r.ingress.stopping
	r.ingress.mu.Unlock()
	if !available || normalizer == nil {
		return nil, im.ErrIngressClosed
	}
	return normalizer.submitObserved(ctx, key, func(ctx context.Context) (im.IngressEnvelope, error) {
		if normalize == nil {
			return envelope, nil
		}
		result, err := normalize(ctx, r.CaptureIngressEnvelope(envelope))
		if err != nil {
			return im.IngressEnvelope{}, err
		}
		before, after := envelope.Message, result.Message
		for _, pair := range [][2]string{{before.IMType, after.IMType}, {before.AccountID, after.AccountID}, {before.ChatID, after.ChatID}, {before.SenderID, after.SenderID}, {before.MessageID, after.MessageID}, {before.ChatKind, after.ChatKind}, {before.AppID, after.AppID}} {
			if pair[0] != "" && pair[0] != pair[1] {
				return im.IngressEnvelope{}, errors.New("normalization changed received identity")
			}
		}
		result.Message.Text = envelope.Message.Text
		result.Message.ForceText = envelope.Message.ForceText
		result.Message.InternalAction = envelope.Message.InternalAction
		result.Cursor = envelope.Cursor
		result.DeliveryID, result.SemanticID = envelope.DeliveryID, envelope.SemanticID
		result.InputDigest = envelope.InputDigest
		result.SourceTime = envelope.SourceTime
		result.Capture = envelope.Capture
		result.Preparation = envelope.Preparation
		result.ReceivedAt = envelope.ReceivedAt
		return result, err
	}, observe)
}

// RejectIngressEnvelope persists a transport refusal and its checkpoint together.
// It grants no authority and never dispatches the rejected input.
func (r *NotificationRouter) RejectIngressEnvelope(ctx context.Context, e im.IngressEnvelope, reason string) im.ReceiveResult {
	if r.ingress == nil {
		return im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "ingress_unavailable"}
	}
	s := r.ingress
	s.mu.Lock()
	ready := s.started && !s.stopping && s.ctx.Err() == nil
	if ready {
		s.requests.Add(1)
	}
	s.mu.Unlock()
	if !ready {
		return im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "ingress_unavailable"}
	}
	defer s.requests.Done()
	m := e.Message
	return s.store.AcceptRecord(ctx, IngressRecord{
		Platform: m.IMType, AccountID: m.AccountID, ChatID: m.ChatID,
		SenderID: m.SenderID, BusinessID: m.MessageID, SemanticID: e.SemanticID,
		DeliveryID: e.DeliveryID, InputDigest: e.InputDigest, SourceTime: e.SourceTime,
		ReceivedAt: e.ReceivedAt, Kind: "task", State: im.IngressRejected, Reason: reason,
	}, e.Cursor)
}

// ReceiveIngress is the result-returning common entry point for transports that
// can wait. Socket readers should use SubmitIngress and handle its future away
// from heartbeats. Cancellation cannot certify that an in-flight commit failed.
func (r *NotificationRouter) ReceiveIngress(ctx context.Context, envelope im.IngressEnvelope) im.ReceiveResult {
	ctx, cancel := context.WithTimeout(ctx, im.IngressAcceptBudget)
	defer cancel()
	future, err := r.SubmitIngress(ctx, envelope)
	if err != nil {
		return im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: im.SubmitRefusalReason(err)}
	}
	return im.WaitIngressNormalization(ctx, future)
}

// PrepareIngressAccount is called after transport identity verification, before
// PlatformReady. Pending work is released only after current admission is ready.
func (r *NotificationRouter) PrepareIngressAccount(ctx context.Context, platform, account string) error {
	if r.ingress == nil {
		return im.ErrIngressClosed
	}
	s := r.ingress
	s.mu.Lock()
	ready := s.started && !s.stopping && s.ctx != nil && s.ctx.Err() == nil
	s.mu.Unlock()
	if !ready || s.store.closing.Load() {
		return im.ErrIngressClosed
	}
	return s.PrepareAccount(ctx, platform, account)
}

func (r *NotificationRouter) installIngressReadinessNotifier(adapter im.IMAdapter) {
	if r.ingress == nil || adapter == nil {
		return
	}
	if notifier, ok := adapter.(im.IngressDeliveryReadinessNotifier); ok {
		notifier.SetIngressReadinessNotifier(r.ingress.signal)
	}
	if multi, ok := adapter.(*MultiAdapter); ok {
		for _, child := range multi.adapters {
			r.installIngressReadinessNotifier(child)
		}
	}
}

func (r *NotificationRouter) detachIngressReadinessNotifier(adapter im.IMAdapter) {
	if notifier, ok := adapter.(im.IngressDeliveryReadinessNotifier); ok {
		notifier.SetIngressReadinessNotifier(nil)
	}
	if multi, ok := adapter.(*MultiAdapter); ok {
		for _, child := range multi.adapters {
			r.detachIngressReadinessNotifier(child)
		}
	}
}
