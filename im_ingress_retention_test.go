package main

import (
	"context"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
)

// The receipt observer fires its own timeout once and is never heard from
// again. A delivery left in dispatching or unknown otherwise keeps its route
// barrier raised and its pending slot reserved until the process restarts.
func TestIngressStalledDeliveryIsSettled(t *testing.T) {
	for _, state := range []im.IngressState{"unknown", "dispatching"} {
		t.Run(string(state), func(t *testing.T) {
			s, _, _, msg := ingressTestService(t)
			id := pauseIngressTestRoute(t, s, msg)
			msg.MessageID, msg.Text = "stalled", "task body"
			result := s.Accept(context.Background(), im.IngressEnvelope{Message: msg})
			if result.Disposition != im.ReceiveAccepted {
				t.Fatal(result)
			}
			// Simulate a delivery whose receipt never arrived.
			if err := s.store.UpdateRecord(context.Background(), result.RecordID, []im.IngressState{"received"}, func(r *IngressRecord) error {
				r.State = state
				r.Reason = "receipt_timeout"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if got := len(s.store.ListRoutePendingMetadata(id)); got != 1 {
				t.Fatalf("stalled record left the pending index: %d", got)
			}

			ageIngressRecord(t, s.store, result.RecordID)
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
	result := s.Accept(context.Background(), im.IngressEnvelope{Message: msg})
	if result.Disposition != im.ReceiveAccepted {
		t.Fatal(result)
	}
	if err := s.store.UpdateRecord(context.Background(), result.RecordID, []im.IngressState{"received"}, func(r *IngressRecord) error {
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

// The sweep only helps if the dispatcher's route visit runs it, so drive the
// service loop instead of calling the helper: deleting the call from run()
// must fail here rather than leaving only a direct unit of the helper.
func TestIngressRouteVisitSettlesStalledDelivery(t *testing.T) {
	s, _, w, msg := ingressTestService(t)
	id := pauseIngressTestRoute(t, s, msg)
	msg.MessageID, msg.Text = "stalled", "task body"
	stalled := s.Accept(context.Background(), im.IngressEnvelope{Message: msg})
	if stalled.Disposition != im.ReceiveAccepted {
		t.Fatal(stalled)
	}
	if err := s.store.UpdateRecord(context.Background(), stalled.RecordID, []im.IngressState{"received"}, func(r *IngressRecord) error {
		r.State = "dispatching"
		r.Reason = "receipt_timeout"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ageIngressRecord(t, s.store, stalled.RecordID)

	// Leave the route eligible and wake the dispatcher; the settlement must
	// come from run()'s visit, not from this test.
	s.mu.Lock()
	delete(s.active, id)
	s.mu.Unlock()
	s.signal()
	waitIngressCondition(t, func() bool {
		got, err := s.store.Get(stalled.RecordID)
		return err == nil && got.State == im.IngressExpired && got.Terminal()
	})
	got, err := s.store.Get(stalled.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reason != "receipt_timeout" || got.Text != "" {
		t.Fatalf("the dispatcher did not settle the stalled delivery: %+v", got)
	}

	// Settling a stalled delivery must not hold back work that arrived later:
	// the dispatcher must deliver it without any direct visitRoute call.
	msg.MessageID, msg.Text = "fresh", "new task"
	fresh := s.Accept(context.Background(), im.IngressEnvelope{Message: msg})
	if fresh.Disposition != im.ReceiveAccepted {
		t.Fatal(fresh)
	}
	waitIngressCondition(t, func() bool { return len(w.snapshot()) == 1 })
	if got := w.snapshot()[0]["content"]; got != "new task" {
		t.Fatalf("fresh delivery was disturbed by the stalled one: %v", got)
	}
}
