package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestIngressObservedReportsAuthoritativeResultOnce(t *testing.T) {
	receiver := &orderedIngressReceiver{}
	n := NewIngressNormalizer(context.Background(), receiver)
	defer n.Close()
	observed := make(chan ReceiveResult, 1)
	result, err := n.submitObserved(context.Background(), "chat", func(context.Context) (IngressEnvelope, error) {
		return IngressEnvelope{Message: IncomingMessage{MessageID: "message"}}, nil
	}, func(result ReceiveResult) { observed <- result })
	if err != nil {
		t.Fatal(err)
	}
	want := <-result
	select {
	case got := <-observed:
		if got != want || got.Disposition != ReceiveAccepted {
			t.Fatalf("observer=%+v result=%+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("observer did not complete")
	}
	n.Close()
	select {
	case <-observed:
		t.Fatal("observer completed twice")
	default:
	}
}

func TestIngressObservedShutdownDrainsWithoutHoldingQueueLock(t *testing.T) {
	n := NewIngressNormalizer(context.Background(), &orderedIngressReceiver{})
	started := make(chan struct{})
	var mu sync.Mutex
	count := 0
	observe := func(r ReceiveResult) {
		// Observers may inspect queue state or enqueue a bounded diagnostic.
		_ = n.Snapshot()
		if r.Disposition != ReceiveRetryable {
			t.Errorf("unexpected result: %+v", r)
		}
		mu.Lock()
		count++
		mu.Unlock()
	}
	_, err := n.submitObserved(context.Background(), "chat", func(ctx context.Context) (IngressEnvelope, error) {
		close(started)
		<-ctx.Done()
		return IngressEnvelope{}, ctx.Err()
	}, observe)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	for range 12 {
		if _, err = n.submitObserved(context.Background(), "chat", func(context.Context) (IngressEnvelope, error) {
			t.Error("queued input executed during shutdown")
			return IngressEnvelope{}, nil
		}, observe); err != nil {
			t.Fatal(err)
		}
	}
	n.Close()
	mu.Lock()
	defer mu.Unlock()
	if count != 13 {
		t.Fatalf("observations=%d, want 13", count)
	}
	if _, err = n.submitObserved(context.Background(), "chat", func(context.Context) (IngressEnvelope, error) { return IngressEnvelope{}, nil }, observe); err == nil {
		t.Fatal("closed queue accepted input")
	}
}
