package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

type contextProbeAdapter struct {
	pairingTestAdapter
	send func(context.Context) error
}

func (a *contextProbeAdapter) SendTextContext(ctx context.Context, _, _ string) error {
	return a.send(ctx)
}

func TestDeliveryPreservesCallerContext(t *testing.T) {
	for _, flow := range []bool{false, true} {
		for _, source := range []string{"caller", "binding", "deadline"} {
			name := source
			if flow {
				name = "flow/" + name
			} else {
				name = "bound/" + name
			}
			t.Run(name, func(t *testing.T) {
				r, code := pairedTestRouter(t)
				msg, key := bindTestOwner(t, r, code, t.TempDir())
				target, ok := r.targetForKey(key)
				if !ok {
					t.Fatal("missing target")
				}
				msg.Text = "task"
				ticket, ok := r.admission.Authorize(msg)
				if !ok {
					t.Fatal("missing ticket")
				}
				permit := im.ReplyPermit{Identity: ticket.Identity, AccountEpoch: ticket.AccountEpoch, ExpiresAt: time.Now().Add(time.Minute), Ticket: ticket}
				type valueKey struct{}
				deadline := time.Now().Add(10 * time.Second)
				if source == "deadline" {
					deadline = time.Now().Add(30 * time.Millisecond)
				}
				ctx, cancel := context.WithDeadline(context.WithValue(context.Background(), valueKey{}, "request"), deadline)
				defer cancel()
				entered := make(chan struct{})
				probe := &contextProbeAdapter{send: func(got context.Context) error {
					if got.Value(im.ReplyAccountContextKey{}) != "bot" {
						t.Error("original account identity lost")
					}
					if flow {
						captured, ok := got.Value(im.OutboundReplyContextKey{}).(im.ReplyPermit)
						if !ok || captured.Ticket.Identity != ticket.Identity || got.Value(im.OutboundDeliveryContextKey{}) != nil {
							t.Error("flow authority was not captured exclusively")
						}
					} else {
						captured, ok := got.Value(im.OutboundDeliveryContextKey{}).(im.DeliveryTarget)
						if !ok || captured != target || got.Value(im.OutboundReplyContextKey{}) != nil {
							t.Error("binding authority was not captured exclusively")
						}
					}
					if got.Value(valueKey{}) != "request" {
						t.Error("caller values lost")
					}
					if d, ok := got.Deadline(); !ok || !d.Equal(deadline) {
						t.Error("caller deadline lost")
					}
					close(entered)
					<-got.Done()
					return got.Err()
				}}
				r.SetAdapter(probe)
				done := make(chan error, 1)
				go func() {
					if flow {
						done <- r.SendFlowReply(ctx, permit, "reply")
					} else {
						done <- r.SendBound(ctx, target, "reply")
					}
				}()
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("send did not start")
				}
				switch source {
				case "caller":
					cancel()
				case "binding":
					if flow {
						r.admission.Close()
					} else {
						target.Lease.Close()
					}
				}
				select {
				case err := <-done:
					want := context.Canceled
					if source == "deadline" {
						want = context.DeadlineExceeded
					}
					if !errors.Is(err, want) {
						t.Fatalf("send = %v, want %v", err, want)
					}
				case <-time.After(time.Second):
					t.Fatal("send ignored cancellation")
				}
			})
		}
	}
}
