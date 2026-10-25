package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"

	"github.com/keakon/chord-gateway/internal/im"
	"github.com/keakon/chord-gateway/internal/platform/slack"
)

func TestSlackSocketDisconnectDuringIdentityPreservesFailure(t *testing.T) {
	for _, reason := range []string{"link_disabled", "refresh_requested"} {
		t.Run(reason, func(t *testing.T) {
			a, identity := slackSocketTestAdapter(t)
			entered := make(chan struct{})
			a.SetVerifyIdentityForTest(func(ctx context.Context) (slack.Identity, error) {
				close(entered)
				<-ctx.Done()
				return slack.Identity{}, ctx.Err()
			})
			conn, done, _ := startSlackSocketTest(t, a, identity)
			if err := conn.WriteJSON(map[string]any{"type": "hello", "connection_info": map[string]string{"app_id": identity.AppID}}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("identity refresh not entered")
			}
			if err := conn.WriteJSON(map[string]string{"type": "disconnect", "reason": reason}); err != nil {
				t.Fatal(err)
			}
			err := waitSlackSocketDone(t, done)
			if slack.ConnectionCategory(err) != "socket_"+reason || slack.ConnectionFatal(err) != (reason == "link_disabled") {
				t.Fatalf("disconnect failure was overwritten by API cancellation: %v", err)
			}
		})
	}
}

func TestSlackSocketControlEnvelopesWithoutEvent(t *testing.T) {
	a, identity := slackSocketTestAdapter(t)
	conn, done, cancel := startSlackSocketTest(t, a, identity)
	if err := conn.WriteJSON(map[string]any{"type": "hello", "connection_info": map[string]string{"app_id": identity.AppID}}); err != nil {
		t.Fatal(err)
	}
	waitIngressCondition(t, func() bool { return a.ReadyForTest() })
	// Neither an unknown control frame nor a malformed Events envelope carries
	// an executable event. Every actual delivery still receives its own slack.ACK.
	for _, frame := range []map[string]any{
		{"type": "interactive", "envelope_id": "control-envelope"},
		{"type": "events_api", "envelope_id": "empty-event-envelope", "payload": map[string]string{"type": "event_callback", "team_id": identity.TeamID, "api_app_id": identity.AppID, "event_id": "empty-event"}},
		{"type": "events_api", "envelope_id": "foreign-envelope", "payload": map[string]string{"type": "event_callback", "team_id": "TOTHER", "api_app_id": identity.AppID, "event_id": "foreign-event"}},
	} {
		if err := conn.WriteJSON(frame); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var ack map[string]any
		if json.Unmarshal(raw, &ack) != nil || ack["envelope_id"] != frame["envelope_id"] || len(ack) != 1 {
			t.Fatalf("unsupported envelope ACK: %s", raw)
		}
	}
	records, err := a.HostForTest().(*NotificationRouter).ingress.store.listAll()
	if err != nil || len(records) != 0 {
		t.Fatalf("non-events entered durable ingress: %d, %v", len(records), err)
	}
	cancel()
	_ = waitSlackSocketDone(t, done)
}

func TestSlackSocketHTTPQueryDoesNotBlockPingOrShutdown(t *testing.T) {
	a, identity := slackSocketTestAdapter(t)
	apiEntered, apiExited := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	a.SetClientForTest(testSlackAPI(t, func(_ http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		close(apiEntered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
		close(apiExited)
	}))
	conn, done, cancel := startSlackSocketTest(t, a, identity)
	if err := conn.WriteJSON(map[string]any{"type": "hello", "connection_info": map[string]string{"app_id": identity.AppID}}); err != nil {
		t.Fatal(err)
	}
	waitIngressCondition(t, func() bool { return a.ReadyForTest() })
	now := time.Now()
	ts := fmt.Sprintf("%d.%06d", now.Unix(), now.Nanosecond()/1000)
	if err := conn.WriteJSON(map[string]any{
		"type": "events_api", "envelope_id": "slow-query",
		"payload": map[string]any{"type": "event_callback", "team_id": identity.TeamID, "api_app_id": identity.AppID, "event_id": "slow-query-event", "event": map[string]string{"type": "message", "channel_type": "im", "channel": "DTEST", "user": "UOWNER", "text": "sample task", "ts": ts}},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-apiEntered:
	case <-time.After(time.Second):
		t.Fatal("conversation lookup not entered")
	}
	pong := make(chan string, 1)
	conn.SetPongHandler(func(payload string) error { pong <- payload; return nil })
	if err := conn.WriteControl(websocket.PingMessage, []byte("during-query"), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(map[string]string{"type": "interactive", "envelope_id": "reader-still-active"}); err != nil {
		t.Fatal(err)
	}
	// events_api is ACKed immediately, so its slack.ACK frame precedes the pong on
	// the wire. Read it first, then read again so the ping handler runs in a
	// later read while the conversation lookup is still blocked.
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-pong:
		t.Fatalf("unexpected early pong %q", payload)
	default:
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-pong:
		if payload != "during-query" {
			t.Fatal(payload)
		}
	default:
		t.Fatal("HTTP query blocked the reader's ping response")
	}
	cancel()
	_ = waitSlackSocketDone(t, done)
	select {
	case <-apiExited:
	case <-time.After(time.Second):
		t.Fatal("socket shutdown did not cancel conversation HTTP request")
	}
}

func TestSlackReceiptDoesNotACKUncertainOrRetryable(t *testing.T) {
	for _, disposition := range []im.ReceiveDisposition{im.ReceiveUncertain, im.ReceiveRetryable} {
		t.Run(string(disposition), func(t *testing.T) {
			future := make(chan im.ReceiveResult, 1)
			future <- im.ReceiveResult{Disposition: disposition}
			queue := make(chan slack.ACK, 1)
			receipt := slack.Receipt{Frame: slack.SocketFrame{Type: "slash_commands", EnvelopeID: "uncommitted", AcceptsResponsePayload: true}, ReceivedAt: time.Now(), Future: future}
			(&slack.SlackAdapter{}).ReceiptACKForTest(t.Context(), 1, receipt, queue)
			if len(queue) != 0 {
				t.Fatal("uncommitted result was acknowledged")
			}
		})
	}
}

func TestSlackReceiptAcceptanceHintMatchesIntakeDisposition(t *testing.T) {
	for _, tc := range []struct {
		name     string
		result   im.ReceiveResult
		canReply bool
		wantHint bool
	}{
		{"accepted with response", im.ReceiveResult{Disposition: im.ReceiveAccepted}, true, false},
		{"accepted without response", im.ReceiveResult{Disposition: im.ReceiveAccepted}, false, false},
		{"accepted duplicate", im.ReceiveResult{Disposition: im.ReceiveDuplicate, State: "received"}, true, false},
		{"rejected duplicate", im.ReceiveResult{Disposition: im.ReceiveDuplicate, State: "rejected"}, true, false},
		{"rejected", im.ReceiveResult{Disposition: im.ReceiveRejected, State: "rejected"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			future := make(chan im.ReceiveResult, 1)
			future <- tc.result
			queue := make(chan slack.ACK, 1)
			done := make(chan struct{})
			receipt := slack.Receipt{Frame: slack.SocketFrame{Type: "slash_commands", EnvelopeID: "disposition-envelope", AcceptsResponsePayload: tc.canReply}, ReceivedAt: time.Now(), Future: future}
			go func() { defer close(done); (&slack.SlackAdapter{}).ReceiptACKForTest(t.Context(), 1, receipt, queue) }()
			select {
			case ack := <-queue:
				var body map[string]json.RawMessage
				if json.Unmarshal(ack.Data, &body) != nil {
					t.Fatal(string(ack.Data))
				}
				_, hint := body["payload"]
				if hint != tc.wantHint {
					t.Errorf("acceptance hint=%v, want %v: %s", hint, tc.wantHint, ack.Data)
				}
				ack.Done <- nil
			case <-time.After(time.Second):
				t.Fatal("confirmed result did not enqueue ACK")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("ACK writer completion did not release receipt waiter")
			}
		})
	}
}

func TestSlackReceiptShutdownInterruptsACKCompletionWait(t *testing.T) {
	future := make(chan im.ReceiveResult, 1)
	future <- im.ReceiveResult{Disposition: im.ReceiveAccepted}
	queue := make(chan slack.ACK, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	receipt := slack.Receipt{Frame: slack.SocketFrame{Type: "slash_commands", EnvelopeID: "accepted", AcceptsResponsePayload: true}, ReceivedAt: time.Now(), Future: future}
	go func() { defer close(done); (&slack.SlackAdapter{}).ReceiptACKForTest(ctx, 1, receipt, queue) }()
	select {
	case ack := <-queue:
		var payload struct {
			EnvelopeID string            `json:"envelope_id"`
			Payload    map[string]string `json:"payload"`
		}
		if json.Unmarshal(ack.Data, &payload) != nil || payload.EnvelopeID != "accepted" || len(payload.Payload) != 0 {
			t.Fatal(string(ack.Data))
		}
		select {
		case <-done:
			t.Fatal("ACK enqueue was treated as completed write")
		default:
		}
	case <-time.After(time.Second):
		t.Fatal("accepted operation did not enqueue ACK")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not interrupt ACK completion wait")
	}
}

func TestSlackReceiptDeadlineDoesNotInventRetryableOutcome(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	future := make(chan im.ReceiveResult, 1)
	queue := make(chan slack.ACK, 1)
	receipt := slack.Receipt{Frame: slack.SocketFrame{Type: "events_api", EnvelopeID: "late-commit"}, ReceivedAt: time.Now(), Future: future}
	(&slack.SlackAdapter{}).ReceiptACKForTest(ctx, 1, receipt, queue)
	if len(queue) != 0 || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("unconfirmed future received an ACK")
	}
	// Completion is still buffered for durable reconciliation after the waiter exits.
	future <- im.ReceiveResult{Disposition: im.ReceiveAccepted, RecordID: "late-record"}
	if result := im.WaitIngressNormalization(t.Context(), future); result.Disposition != im.ReceiveAccepted {
		t.Fatal(result)
	}
}

// A submit refusal from the shared normalizer - for example a saturated queue -
// used to leave the envelope unacknowledged with no metric or log at all. The
// reader must count it like every other discard before Slack redelivers it.
func TestSlackSocketSubmitFailureRecordsLostIntake(t *testing.T) {
	a, identity := slackSocketTestAdapter(t)
	router := a.HostForTest().(*NotificationRouter)
	gate := make(chan struct{})
	defer close(gate)
	for i := range ingressNormalizationCapacity {
		if _, err := router.ingress.normalizer.Submit(fmt.Sprintf("key-%d", i), func(context.Context) (im.IngressEnvelope, error) {
			<-gate
			return im.IngressEnvelope{}, errors.New("sample normalization failure")
		}); err != nil {
			t.Fatal(err)
		}
	}
	conn, done, cancel := startSlackSocketTest(t, a, identity)
	if err := conn.WriteJSON(map[string]any{"type": "hello", "connection_info": map[string]string{"app_id": identity.AppID}}); err != nil {
		t.Fatal(err)
	}
	waitIngressCondition(t, func() bool { return a.ReadyForTest() })
	now := time.Now()
	ts := fmt.Sprintf("%d.%06d", now.Unix(), now.Nanosecond()/1000)
	if err := conn.WriteJSON(map[string]any{
		"type": "events_api", "envelope_id": "capacity-envelope",
		"payload": map[string]any{"type": "event_callback", "team_id": identity.TeamID, "api_app_id": identity.AppID, "event_id": "capacity-event", "event": map[string]string{"type": "message", "channel_type": "im", "channel": "DTEST", "user": "UOWNER", "text": "sample task", "ts": ts}},
	}); err != nil {
		t.Fatal(err)
	}
	waitIngressCondition(t, func() bool {
		for _, counter := range router.ingressStore.MetricsSnapshot().Counters {
			if counter.Platform == "slack" && counter.Event == "lost_before_accept" && counter.Reason == "capacity" && counter.Count > 0 {
				return true
			}
		}
		return false
	})
	cancel()
	_ = waitSlackSocketDone(t, done)
}

func TestSlackReceiptTimeoutIsObservableBeforeACKDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := NewIngressStore()
		adapter := &slack.SlackAdapter{}
		adapter.SetHostForTest(&NotificationRouter{ingressStore: store})
		acks := make(chan slack.ACK, 1)
		start := time.Now()
		adapter.ReceiptACKForTest(t.Context(), 1, slack.Receipt{
			Frame: slack.SocketFrame{EnvelopeID: "slow-normalization"}, ReceivedAt: start,
			Future: make(chan im.ReceiveResult),
		}, acks)
		if elapsed := time.Since(start); elapsed != 2*time.Second {
			t.Fatalf("intake wait: %v", elapsed)
		}
		if len(acks) != 0 {
			t.Fatal("unconfirmed intake acknowledged")
		}
		metrics := store.MetricsSnapshot()
		if len(metrics.Counters) != 1 || metrics.Counters[0].Event != "acceptance_unconfirmed" {
			t.Fatal(metrics)
		}
	})
}
