package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
)

type classifiedTextFailure struct{ category string }

func (e classifiedTextFailure) Error() string                     { return "sanitized transport failure" }
func (e classifiedTextFailure) IMTextSendFailureCategory() string { return e.category }

func TestTextSenderRetainsUnknownOutcomeAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sender, err := im.NewIMTextSender(im.IMTextSendOptions{Budget: 128, SendSegment: func(context.Context, string, string) error {
		cancel()
		return classifiedTextFailure{"unknown_send"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Send(ctx, nil, nil, "target", "output", false)
	var partial *im.IMTextSendError
	if !errors.As(err, &partial) || partial.Category != "unknown_send" || partial.AcceptedSegments != 0 {
		t.Fatalf("send=%v", err)
	}
}

func TestTextSenderLimitsTransportDiagnosticLabels(t *testing.T) {
	for _, category := range []string{"platform_rejected", "unknown_send", "backpressure", "connection_lost", "arbitrary-label"} {
		t.Run(category, func(t *testing.T) {
			sender, err := im.NewIMTextSender(im.IMTextSendOptions{Budget: 128, SendSegment: func(context.Context, string, string) error { return classifiedTextFailure{category} }})
			if err != nil {
				t.Fatal(err)
			}
			var partial *im.IMTextSendError
			err = sender.Send(t.Context(), nil, nil, "target", "output", false)
			want := category
			if category == "arbitrary-label" {
				want = "delivery_failed"
			}
			if !errors.As(err, &partial) || partial.Category != want {
				t.Fatalf("send=%v", err)
			}
		})
	}
}

func TestTextSenderPreservesWrappedUnknownOutcome(t *testing.T) {
	for _, cancelSend := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		sender, err := im.NewIMTextSender(im.IMTextSendOptions{Budget: 128, SendSegment: func(context.Context, string, string) error {
			calls++
			if calls == 1 {
				return nil
			}
			if cancelSend {
				cancel()
			}
			return errors.Join(im.ErrDeliveryOutcomeUnknown, context.Canceled)
		}})
		if err != nil {
			t.Fatal(err)
		}
		err = sender.Send(ctx, nil, nil, "target", strings.Repeat("x", 256), false)
		cancel()
		var partial *im.IMTextSendError
		last := sender.TextSendTargetSnapshot("target")
		if !errors.As(err, &partial) || partial.Category != "unknown_send" || partial.AcceptedSegments != 1 || last.LastFailureCategory != "unknown_send" || calls != 2 {
			t.Fatalf("send=%v snapshot=%+v calls=%d", err, last, calls)
		}
	}
}
