package main

import (
	"context"
	"testing"
	"time"
)

// The receipt observer fires its own timeout once and is never heard from
// again. A delivery left in dispatching or unknown otherwise keeps its route
// barrier raised and its pending slot reserved until the process restarts.
func TestIngressStalledDeliveryIsSettled(t *testing.T) {
	for _, state := range []string{"unknown", "dispatching"} {
		t.Run(state, func(t *testing.T) {
			s, _, _, msg := ingressTestService(t)
			id := pauseIngressTestRoute(t, s, msg)
			msg.MessageID, msg.Text = "stalled", "task body"
			result := s.Accept(context.Background(), IngressEnvelope{Message: msg})
			if result.Disposition != ReceiveAccepted {
				t.Fatal(result)
			}
			// Simulate a delivery whose receipt never arrived.
			if err := s.store.UpdateRecord(context.Background(), result.RecordID, []string{"received"}, func(r *IngressRecord) error {
				r.State = state
				r.Reason = "receipt_timeout"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if got := len(s.store.ListRoutePendingMetadata(id)); got != 1 {
				t.Fatalf("stalled record left the pending index: %d", got)
			}

			real := s.store.now
			s.store.now = func() time.Time { return real().Add(ingressStallDeadline + time.Minute) }
			defer func() { s.store.now = real }()
			s.settleStalledRecords(s.store.ListRoutePendingMetadata(id))

			got, err := s.store.Get(result.RecordID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != "expired" || got.Reason != "receipt_timeout" {
				t.Fatalf("stalled %s delivery was not settled: %+v", state, got)
			}
			if !got.Terminal() {
				t.Fatalf("settled record is still pending: %+v", got)
			}
			if got.Text != "" {
				t.Fatal("settled record retained the body", got)
			}
		})
	}
}

// A delivery that is still awaiting its receipt must survive the sweep.
func TestIngressFreshDeliverySurvivesTheStallSweep(t *testing.T) {
	s, _, _, msg := ingressTestService(t)
	id := pauseIngressTestRoute(t, s, msg)
	msg.MessageID, msg.Text = "in-flight", "task body"
	result := s.Accept(context.Background(), IngressEnvelope{Message: msg})
	if result.Disposition != ReceiveAccepted {
		t.Fatal(result)
	}
	if err := s.store.UpdateRecord(context.Background(), result.RecordID, []string{"received"}, func(r *IngressRecord) error {
		r.State = "dispatching"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.settleStalledRecords(s.store.ListRoutePendingMetadata(id))
	got, err := s.store.Get(result.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "dispatching" {
		t.Fatalf("in-flight delivery was settled early: %+v", got)
	}
}
