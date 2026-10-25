package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

type orderedIngressReceiver struct {
	mu  sync.Mutex
	ids []string
}

func (r *orderedIngressReceiver) Accept(_ context.Context, e im.IngressEnvelope) im.ReceiveResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, e.Message.MessageID)
	return im.ReceiveResult{Disposition: im.ReceiveAccepted}
}
func TestIngressNormalizationPreservesTargetOrder(t *testing.T) {
	receiver := &orderedIngressReceiver{}
	n := NewIngressNormalizer(context.Background(), receiver)
	defer n.Close()
	started, release := make(chan struct{}), make(chan struct{})
	first, err := n.Submit("channel", func(ctx context.Context) (im.IngressEnvelope, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return im.IngressEnvelope{Message: im.IncomingMessage{MessageID: "first"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	second, err := n.Submit("channel", func(context.Context) (im.IngressEnvelope, error) {
		return im.IngressEnvelope{Message: im.IncomingMessage{MessageID: "second"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	other, err := n.Submit("another-channel", func(context.Context) (im.IngressEnvelope, error) {
		return im.IngressEnvelope{Message: im.IncomingMessage{MessageID: "other"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result := <-other; result.Disposition != im.ReceiveAccepted {
		t.Fatal(result)
	}
	close(release)
	<-first
	<-second
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	if len(receiver.ids) != 3 || receiver.ids[1] != "first" || receiver.ids[2] != "second" {
		t.Fatal(receiver.ids)
	}
}
func TestIngressNormalizationCapacityAndShutdown(t *testing.T) {
	n := NewIngressNormalizer(context.Background(), &orderedIngressReceiver{})
	var results []<-chan im.ReceiveResult
	for range 128 {
		result, err := n.Submit("channel", func(ctx context.Context) (im.IngressEnvelope, error) {
			<-ctx.Done()
			return im.IngressEnvelope{}, ctx.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}
	if _, err := n.Submit("channel", func(context.Context) (im.IngressEnvelope, error) { return im.IngressEnvelope{}, nil }); err == nil {
		t.Fatal("queue exceeded")
	}
	n.Close()
	n.Close()
	for _, result := range results {
		if r := <-result; r.Disposition != im.ReceiveRetryable {
			t.Fatal(r)
		}
	}
}

func receiveNormalization(t *testing.T, result <-chan im.ReceiveResult) im.ReceiveResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return WaitIngressNormalization(ctx, result)
}

func TestIngressNormalizationParentCancellationDrainsWithoutClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	n := NewIngressNormalizer(ctx, &orderedIngressReceiver{})
	defer n.Close()
	var results []<-chan im.ReceiveResult
	for range 16 {
		result, err := n.Submit("channel", func(ctx context.Context) (im.IngressEnvelope, error) {
			<-ctx.Done()
			return im.IngressEnvelope{}, ctx.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}
	cancel()
	for _, result := range results {
		if got := receiveNormalization(t, result); got.Disposition != im.ReceiveRetryable || got.Reason != "normalization_cancelled" {
			t.Fatal(got)
		}
		if _, ok := <-result; ok {
			t.Fatal("result channel was not closed")
		}
	}
	if _, err := n.Submit("channel", func(context.Context) (im.IngressEnvelope, error) { return im.IngressEnvelope{}, nil }); !errors.Is(err, errIngressClosed) {
		t.Fatalf("submit after cancellation: %v", err)
	}
}

func TestIngressNormalizationIndividualCancellationPreservesFIFO(t *testing.T) {
	receiver := &orderedIngressReceiver{}
	n := NewIngressNormalizer(context.Background(), receiver)
	defer n.Close()
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	first, err := n.SubmitContext(ctx, "channel", func(ctx context.Context) (im.IngressEnvelope, error) {
		close(started)
		<-ctx.Done()
		// A lookup ignoring cancellation must never proceed to admission.
		return im.IngressEnvelope{Message: im.IncomingMessage{MessageID: "cancelled"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	second, err := n.Submit("channel", func(context.Context) (im.IngressEnvelope, error) {
		return im.IngressEnvelope{Message: im.IncomingMessage{MessageID: "next"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if got := receiveNormalization(t, first); got.Disposition != im.ReceiveRetryable {
		t.Fatal(got)
	}
	if got := receiveNormalization(t, second); got.Disposition != im.ReceiveAccepted {
		t.Fatal(got)
	}
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	if !reflect.DeepEqual(receiver.ids, []string{"next"}) {
		t.Fatal(receiver.ids)
	}
}

type cancellingIngressReceiver struct {
	cancel context.CancelFunc
}

func (r cancellingIngressReceiver) Accept(context.Context, im.IngressEnvelope) im.ReceiveResult {
	r.cancel()
	return im.ReceiveResult{Disposition: im.ReceiveAccepted, RecordID: "accepted"}
}

func TestIngressNormalizationPreservesAdmissionResultAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := NewIngressNormalizer(context.Background(), cancellingIngressReceiver{cancel})
	defer n.Close()
	result, err := n.SubmitContext(ctx, "channel", func(context.Context) (im.IngressEnvelope, error) { return im.IngressEnvelope{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if got := receiveNormalization(t, result); got.Disposition != im.ReceiveAccepted || got.RecordID != "accepted" {
		t.Fatal(got)
	}
}

func TestIngressNormalizationWaitCancellationIsUncertain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := make(chan im.ReceiveResult, 1)
	if got := WaitIngressNormalization(ctx, result); got.Disposition != im.ReceiveUncertain {
		t.Fatal(got)
	}
	result <- im.ReceiveResult{Disposition: im.ReceiveAccepted}
	if got := WaitIngressNormalization(ctx, result); got.Disposition != im.ReceiveAccepted {
		t.Fatal(got)
	}
	close(result)
	if got := WaitIngressNormalization(ctx, result); got.Disposition != im.ReceiveUncertain {
		t.Fatal(got)
	}
}

func TestIngressNormalizationGlobalWorkerBound(t *testing.T) {
	n := NewIngressNormalizer(context.Background(), &orderedIngressReceiver{})
	defer n.Close()
	started := make(chan struct{}, 16)
	for i := range 16 {
		_, err := n.Submit(fmt.Sprintf("channel-%d", i), func(ctx context.Context) (im.IngressEnvelope, error) {
			started <- struct{}{}
			<-ctx.Done()
			return im.IngressEnvelope{}, ctx.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for range ingressNormalizationWorkers {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("normalization workers did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("normalizer exceeded the shared worker bound")
	default:
	}
	if got := n.Snapshot(); got.Pending != 16 || got.Keys != 16 || got.Closed {
		t.Fatal(got)
	}
	n.Close()
	if got := n.Snapshot(); got.Pending != 0 || got.Keys != 0 || !got.Closed {
		t.Fatal(got)
	}
}

func TestIngressNormalizationKeyFairness(t *testing.T) {
	n := NewIngressNormalizer(context.Background(), &orderedIngressReceiver{})
	defer n.Close()
	blocked := make(chan struct{}, ingressNormalizationWorkers-1)
	for i := range ingressNormalizationWorkers - 1 {
		_, err := n.Submit(fmt.Sprintf("blocker-%d", i), func(ctx context.Context) (im.IngressEnvelope, error) {
			blocked <- struct{}{}
			<-ctx.Done()
			return im.IngressEnvelope{}, ctx.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for range ingressNormalizationWorkers - 1 {
		<-blocked
	}
	started, release := make(chan struct{}), make(chan struct{})
	_, err := n.Submit("hot", func(context.Context) (im.IngressEnvelope, error) {
		close(started)
		<-release
		return im.IngressEnvelope{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	order := make(chan string, 2)
	for _, key := range []string{"hot", "cold"} {
		_, err := n.Submit(key, func(context.Context) (im.IngressEnvelope, error) {
			order <- key
			return im.IngressEnvelope{}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	for _, want := range []string{"cold", "hot"} {
		select {
		case got := <-order:
			if got != want {
				t.Fatalf("order got %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("normalization stalled")
		}
	}
}

func TestCaptureIngressEnvelopeFreezesActionAndIsIdempotent(t *testing.T) {
	// An existing capture must never be replaced by a later runtime observation.
	r := &NotificationRouter{}
	action := &im.InternalAction{Type: "question", Values: []string{"1", "2"}}
	offset := int64(7)
	cursor := &im.IngressCursor{Kind: "telegram", Version: 1, Telegram: &im.TelegramIngressCursor{PendingOffset: &offset}}
	envelope := im.IngressEnvelope{Cursor: cursor, Message: im.IncomingMessage{InternalAction: action}, Capture: &im.IngressCapture{Kind: "control", Target: &im.IngressControlTarget{RuntimeID: "original"}}}
	frozen := r.CaptureIngressEnvelope(envelope)
	offset = 100
	if *frozen.Cursor.Telegram.PendingOffset != 7 {
		t.Fatal("progress mutation changed frozen cursor")
	}
	action.Type = "role"
	action.Values[0] = "changed"
	if frozen.Message.InternalAction.Type != "question" || frozen.Message.InternalAction.Values[0] != "1" {
		t.Fatal("callback mutation changed frozen action")
	}
	if frozen.Capture != envelope.Capture || frozen.Capture.Target.RuntimeID != "original" {
		t.Fatal("existing target was recaptured")
	}
}

func TestIngressNormalizationConcurrentCloseDeliversEachResultOnce(t *testing.T) {
	n := NewIngressNormalizer(context.Background(), &orderedIngressReceiver{})
	var results []<-chan im.ReceiveResult
	for range ingressNormalizationCapacity {
		result, err := n.Submit("channel", func(ctx context.Context) (im.IngressEnvelope, error) {
			<-ctx.Done()
			return im.IngressEnvelope{}, ctx.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}
	var closers sync.WaitGroup
	for range 16 {
		closers.Go(n.Close)
	}
	closers.Wait()
	for _, result := range results {
		if got := receiveNormalization(t, result); got.Disposition != im.ReceiveRetryable {
			t.Fatal(got)
		}
		if _, ok := <-result; ok {
			t.Fatal("normalization returned more than one result")
		}
	}
}
