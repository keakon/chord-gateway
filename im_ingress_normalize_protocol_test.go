package main

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

func TestIngressProtocolSharesFIFOAndCapacityWithBusinessInput(t *testing.T) {
	receiver := &orderedIngressReceiver{}
	n := NewIngressNormalizer(t.Context(), receiver)
	defer n.Close()
	started, release := make(chan struct{}), make(chan struct{})
	first, err := n.submitProtocol(t.Context(), "account/chat", func(ctx context.Context) ReceiveResult {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		receiver.mu.Lock()
		receiver.ids = append(receiver.ids, "identity-probe")
		receiver.mu.Unlock()
		return ReceiveResult{Disposition: ReceiveRejected, RecordID: "probe-tombstone"}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	var results []<-chan ReceiveResult
	results = append(results, first)
	for i := 1; i < ingressNormalizationCapacity; i++ {
		future, err := n.Submit("account/chat", func(context.Context) (IngressEnvelope, error) {
			return IngressEnvelope{Message: IncomingMessage{MessageID: "business"}}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, future)
	}
	if _, err := n.submitProtocol(t.Context(), "another-chat", func(context.Context) ReceiveResult { return ReceiveResult{} }); !errors.Is(err, errIngressCapacity) {
		t.Fatalf("protocol queue bypassed shared budget: %v", err)
	}
	if _, err := n.Submit("another-chat", func(context.Context) (IngressEnvelope, error) { return IngressEnvelope{}, nil }); !errors.Is(err, errIngressCapacity) {
		t.Fatalf("business queue bypassed shared budget: %v", err)
	}
	close(release)
	for _, result := range results {
		receiveNormalization(t, result)
	}
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	if len(receiver.ids) != ingressNormalizationCapacity || receiver.ids[0] != "identity-probe" {
		t.Fatalf("protocol/business order: %v", receiver.ids)
	}
}

func TestIngressProtocolCancellationRetainsStartedTransactionResult(t *testing.T) {
	n := NewIngressNormalizer(t.Context(), &orderedIngressReceiver{})
	defer n.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	future, err := n.submitProtocol(ctx, "account/chat", func(ctx context.Context) ReceiveResult {
		close(started)
		<-ctx.Done()
		// Intake published the acceptance despite cancellation. Its outcome must
		// reach the transport and must not be rewritten as a retryable refusal.
		return ReceiveResult{Disposition: ReceiveRejected, RecordID: "memory-tombstone", Reason: "identity_probe"}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	result := receiveNormalization(t, future)
	if result.Disposition != ReceiveRejected || result.RecordID != "memory-tombstone" {
		t.Fatal(result)
	}
}

func TestIngressProtocolShutdownJoinsActiveAndDropsQueuedOperations(t *testing.T) {
	n := NewIngressNormalizer(t.Context(), &orderedIngressReceiver{})
	var mu sync.Mutex
	var executed []string
	started := make(chan struct{})
	first, err := n.submitProtocol(t.Context(), "account/chat", func(ctx context.Context) ReceiveResult {
		close(started)
		<-ctx.Done()
		mu.Lock()
		executed = append(executed, "active")
		mu.Unlock()
		return ReceiveResult{Disposition: ReceiveUncertain, Reason: "transaction_in_progress"}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	second, err := n.submitProtocol(t.Context(), "account/chat", func(context.Context) ReceiveResult {
		mu.Lock()
		executed = append(executed, "queued")
		mu.Unlock()
		return ReceiveResult{Disposition: ReceiveAccepted}
	})
	if err != nil {
		t.Fatal(err)
	}
	n.Close()
	if result := receiveNormalization(t, first); result.Disposition != ReceiveUncertain {
		t.Fatal(result)
	}
	if result := receiveNormalization(t, second); result.Disposition != ReceiveRetryable || result.Reason != "normalization_cancelled" {
		t.Fatal(result)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(executed, []string{"active"}) {
		t.Fatal(executed)
	}
}
