package main

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
)

func TestFeishuTextInFlightCancellationPreservesUnknownOutcome(t *testing.T) {
	entered := make(chan struct{})
	var posts atomic.Int32
	a := textSendFeishuAdapter(t, func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			t.Errorf("unexpected method %s", req.Method)
		}
		textSendRequest(t, req)
		posts.Add(1)
		close(entered)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.SendTextContext(ctx, "chat", "body") }()
	waitFeishuLifecycle(t, entered)
	cancel()
	err := waitFeishuLifecycle(t, done)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, im.ErrDeliveryOutcomeUnknown) {
		t.Fatalf("in-flight cancellation lost outcome evidence: %v", err)
	}
	var partial *im.IMTextSendError
	if !errors.As(err, &partial) || partial.AcceptedSegments != 0 || partial.Category != "unknown_send" {
		t.Fatalf("incorrect partial cancellation evidence: %v", err)
	}
	if posts.Load() != 1 {
		t.Fatalf("unknown segment replayed: %d POSTs", posts.Load())
	}
}

func TestFeishuTextConfirmedRateLimitCancellationKeepsKnownRejection(t *testing.T) {
	entered := make(chan struct{})
	var posts atomic.Int32
	a := textSendFeishuAdapter(t, func(req *http.Request) (*http.Response, error) {
		textSendRequest(t, req)
		posts.Add(1)
		response := textSendResponse(http.StatusTooManyRequests, `{"code":230020}`)
		response.Header.Set("Retry-After", "300")
		close(entered)
		return response, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.SendTextContext(ctx, "chat", "body") }()
	waitFeishuLifecycle(t, entered)
	cancel()
	err := waitFeishuLifecycle(t, done)
	if !errors.Is(err, context.Canceled) || errors.Is(err, im.ErrDeliveryOutcomeUnknown) {
		t.Fatalf("confirmed 429 rejection became an unknown delivery: %v", err)
	}
	if posts.Load() != 1 {
		t.Fatalf("cancelled rate-limit wait retried: %d POSTs", posts.Load())
	}
}
