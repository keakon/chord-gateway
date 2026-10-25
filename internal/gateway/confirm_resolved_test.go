package gateway

import (
	"encoding/json"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
)

func confirmResolvedPayload(requestID, outcome string) json.RawMessage {
	return json.RawMessage(`{"request_id":"` + requestID + `","tool_name":"Read","agent_id":"main","outcome":"` + outcome + `"}`)
}

// A resolution without an answer must retire the pending request and keep it
// as expired, so a late console reply remains a follow-up instead of claiming
// an approval or denial chord silently dropped.
func TestConfirmResolvedTimeoutRetiresPendingAsExpired(t *testing.T) {
	p := &ChordProcess{key: "sample"}
	p.processEnvelope(&HeadlessEnvelope{Type: "confirm_request", Seq: 1, Payload: json.RawMessage(`{"tool_name":"Read","request_id":"req-1","args_json":"{}"}`)})

	p.processEnvelope(&HeadlessEnvelope{Type: "confirm_resolved", Seq: 2, Payload: confirmResolvedPayload("req-1", ConfirmOutcomeTimeout)})

	if p.state.PendingConfirm != nil {
		t.Fatalf("pending confirm = %+v, want nil after the resolution", p.state.PendingConfirm)
	}
	if p.state.ExpiredConfirm == nil || p.state.ExpiredConfirm.RequestID != "req-1" {
		t.Fatalf("expired confirm = %+v, want req-1 kept for late console replies", p.state.ExpiredConfirm)
	}
	if p.state.LastConfirmResolved == nil || p.state.LastConfirmResolved.Outcome != ConfirmOutcomeTimeout {
		t.Fatalf("last resolution = %+v, want the timeout outcome recorded", p.state.LastConfirmResolved)
	}
}

func TestConfirmResolvedCancelledRetiresPendingAsExpired(t *testing.T) {
	p := &ChordProcess{key: "sample"}
	p.processEnvelope(&HeadlessEnvelope{Type: "confirm_request", Seq: 1, Payload: json.RawMessage(`{"tool_name":"Read","request_id":"req-1","args_json":"{}"}`)})

	p.processEnvelope(&HeadlessEnvelope{Type: "confirm_resolved", Seq: 2, Payload: confirmResolvedPayload("req-1", ConfirmOutcomeCancelled)})

	if p.state.PendingConfirm != nil {
		t.Fatalf("pending confirm = %+v, want nil after the resolution", p.state.PendingConfirm)
	}
	if p.state.ExpiredConfirm == nil || p.state.ExpiredConfirm.RequestID != "req-1" {
		t.Fatalf("expired confirm = %+v, want req-1 kept for late console replies", p.state.ExpiredConfirm)
	}
	if p.state.LastConfirmResolved == nil || p.state.LastConfirmResolved.Outcome != ConfirmOutcomeCancelled {
		t.Fatalf("last resolution = %+v, want the cancelled outcome recorded", p.state.LastConfirmResolved)
	}
}

// An answered resolution is an echo of the gateway's own action: the pending
// request goes away without becoming expired.
func TestConfirmResolvedAnswerClearsWithoutExpiry(t *testing.T) {
	p := &ChordProcess{key: "sample"}
	p.processEnvelope(&HeadlessEnvelope{Type: "confirm_request", Seq: 1, Payload: json.RawMessage(`{"tool_name":"Read","request_id":"req-1","args_json":"{}"}`)})

	p.processEnvelope(&HeadlessEnvelope{Type: "confirm_resolved", Seq: 2, Payload: confirmResolvedPayload("req-1", ConfirmOutcomeApproved)})

	if p.state.PendingConfirm != nil || p.state.ExpiredConfirm != nil {
		t.Fatalf("pending=%+v expired=%+v, want both nil after an answered echo", p.state.PendingConfirm, p.state.ExpiredConfirm)
	}
}

// A resolution for an older request must not retire a newer pending one.
func TestConfirmResolvedMismatchKeepsNewerPending(t *testing.T) {
	p := &ChordProcess{key: "sample"}
	p.processEnvelope(&HeadlessEnvelope{Type: "confirm_request", Seq: 1, Payload: json.RawMessage(`{"tool_name":"Read","request_id":"req-1","args_json":"{}"}`)})
	p.processEnvelope(&HeadlessEnvelope{Type: "confirm_request", Seq: 2, Payload: json.RawMessage(`{"tool_name":"Edit","request_id":"req-2","args_json":"{}"}`)})

	p.processEnvelope(&HeadlessEnvelope{Type: "confirm_resolved", Seq: 3, Payload: confirmResolvedPayload("req-1", ConfirmOutcomeTimeout)})

	if p.state.PendingConfirm == nil || p.state.PendingConfirm.RequestID != "req-2" {
		t.Fatalf("pending confirm = %+v, want the newer req-2 kept", p.state.PendingConfirm)
	}
	if p.state.ExpiredConfirm != nil {
		t.Fatalf("expired confirm = %+v, want nil (req-2 is still actionable)", p.state.ExpiredConfirm)
	}
}

func TestConfirmResolvedInvalidPayloadDoesNotReplayOrClearPending(t *testing.T) {
	var events []ControlState
	p := &ChordProcess{
		key: "sample",
		onEvent: func(_ string, eventType string, state ControlState) {
			if eventType == "confirm_resolved" {
				events = append(events, state)
			}
		},
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "confirm_request", Seq: 1, Payload: json.RawMessage(`{"tool_name":"Read","request_id":"req-1","args_json":"{}"}`)})
	p.processEnvelope(&HeadlessEnvelope{Type: "confirm_resolved", Seq: 2, Payload: confirmResolvedPayload("req-1", ConfirmOutcomeTimeout)})
	for _, payload := range []json.RawMessage{
		json.RawMessage(`{"request_id":9,"outcome":"timeout"}`),
		json.RawMessage(`{"request_id":"","outcome":"timeout"}`),
		json.RawMessage(`{"request_id":"req-1","outcome":"unknown"}`),
	} {
		p.processEnvelope(&HeadlessEnvelope{Type: "confirm_resolved", Seq: 3, Payload: payload})
	}
	if len(events) != 1 {
		t.Fatalf("confirm_resolved events = %d, want 1 (malformed payload must not replay prior state)", len(events))
	}
	if p.state.PendingConfirm != nil || p.state.ExpiredConfirm == nil || p.state.ExpiredConfirm.RequestID != "req-1" {
		t.Fatalf("pending=%+v expired=%+v, want original timeout state preserved", p.state.PendingConfirm, p.state.ExpiredConfirm)
	}

	q := &ChordProcess{key: "sample"}
	q.processEnvelope(&HeadlessEnvelope{Type: "confirm_request", Seq: 1, Payload: json.RawMessage(`{"tool_name":"Read","request_id":"req-1","args_json":"{}"}`)})
	q.processEnvelope(&HeadlessEnvelope{Type: "confirm_resolved", Seq: 2, Payload: json.RawMessage(`{"request_id":"req-1","outcome":"unknown"}`)})
	if q.state.PendingConfirm == nil || q.state.PendingConfirm.RequestID != "req-1" || q.state.ExpiredConfirm != nil {
		t.Fatalf("unknown outcome changed confirmation state: pending=%+v expired=%+v", q.state.PendingConfirm, q.state.ExpiredConfirm)
	}
}

func TestFormatConfirmResolvedNotification(t *testing.T) {
	r := &NotificationRouter{}
	timeout := r.formatNotification("confirm_resolved", ControlState{LastConfirmResolved: &ConfirmResolutionPayload{RequestID: "req-1", ToolName: "Read", Outcome: ConfirmOutcomeTimeout}})
	if timeout == "" {
		t.Fatal("a timeout resolution must produce a user-visible notice")
	}
	cancelled := r.formatNotification("confirm_resolved", ControlState{LastConfirmResolved: &ConfirmResolutionPayload{RequestID: "req-1", ToolName: "Read", Outcome: ConfirmOutcomeCancelled}})
	if cancelled == "" {
		t.Fatal("a cancelled resolution must produce a user-visible notice")
	}
	for _, outcome := range []string{ConfirmOutcomeApproved, ConfirmOutcomeDenied} {
		if got := r.formatNotification("confirm_resolved", ControlState{LastConfirmResolved: &ConfirmResolutionPayload{RequestID: "req-1", Outcome: outcome}}); got != "" {
			t.Fatalf("answered outcome %q formatted %q, want silence (the answer already replied)", outcome, got)
		}
	}
	if got := r.formatNotification("confirm_resolved", ControlState{}); got != "" {
		t.Fatalf("resolution without state formatted %q, want silence", got)
	}
}

// resolveConfirmCard must ignore answered echoes: the card was already
// resolved by the answering interaction, and patching it again would overwrite
// that outcome.
func TestResolveConfirmCardIgnoresAnsweredEcho(t *testing.T) {
	r := &NotificationRouter{}
	updated, err := r.resolveConfirmCard(im.DeliveryTarget{}, ControlState{LastConfirmResolved: &ConfirmResolutionPayload{RequestID: "req-1", Outcome: ConfirmOutcomeApproved}})
	if updated || err != nil {
		t.Fatalf("updated=%v err=%v, want no card patch for an answered echo", updated, err)
	}
}
